package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/metrics"
	"h3ws2h1ws-proxy/internal/ws"

	"github.com/gorilla/websocket"
)

func pumpH3ToBackend(ctx context.Context, s io.ReadWriter, bws *websocket.Conn, lim config.Limits) error {
	br := bufio.NewReaderSize(s, 64<<10)

	var (
		assembling   bool
		assemOpcode  byte
		assemPayload []byte
	)

	flushMessage := func(op byte, msg []byte) error {
		if err := bws.SetWriteDeadline(time.Now().Add(lim.WriteTimeout)); err != nil {
			return err
		}
		switch op {
		case ws.OpText:
			metrics.Messages.WithLabelValues("h3_to_h1", "text").Inc()
			metrics.Bytes.WithLabelValues("h3_to_h1").Add(float64(len(msg)))
			return bws.WriteMessage(websocket.TextMessage, msg)
		case ws.OpBinary:
			metrics.Messages.WithLabelValues("h3_to_h1", "binary").Inc()
			metrics.Bytes.WithLabelValues("h3_to_h1").Add(float64(len(msg)))
			return bws.WriteMessage(websocket.BinaryMessage, msg)
		default:
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		f, err := ws.ReadFrame(br, lim.MaxFrameSize)
		if err != nil {
			return err
		}

		switch f.Opcode {
		case ws.OpText, ws.OpBinary:
			if assembling {
				return errors.New("protocol error: new data frame while assembling")
			}
			if f.Fin {
				if int64(len(f.Payload)) > lim.MaxMessageSize {
					metrics.OversizeDrops.WithLabelValues("message").Inc()
					_ = ws.WriteCloseFrame(s, 1009, "message too big")
					return errors.New("message too big")
				}
				if err := flushMessage(f.Opcode, f.Payload); err != nil {
					return err
				}
				continue
			}
			assembling = true
			assemOpcode = f.Opcode
			assemPayload = append(assemPayload[:0], f.Payload...)
			if int64(len(assemPayload)) > lim.MaxMessageSize {
				metrics.OversizeDrops.WithLabelValues("message").Inc()
				_ = ws.WriteCloseFrame(s, 1009, "message too big")
				return errors.New("message too big")
			}

		case ws.OpCont:
			if !assembling {
				return errors.New("protocol error: continuation without start")
			}
			assemPayload = append(assemPayload, f.Payload...)
			if int64(len(assemPayload)) > lim.MaxMessageSize {
				metrics.OversizeDrops.WithLabelValues("message").Inc()
				_ = ws.WriteCloseFrame(s, 1009, "message too big")
				return errors.New("message too big")
			}
			if f.Fin {
				msg := make([]byte, len(assemPayload))
				copy(msg, assemPayload)
				assembling = false
				assemPayload = assemPayload[:0]
				if err := flushMessage(assemOpcode, msg); err != nil {
					return err
				}
			}

		case ws.OpPing:
			metrics.Ctrl.WithLabelValues("ping").Inc()
			if err := ws.WriteControlFrame(s, ws.OpPong, f.Payload); err != nil {
				return err
			}
			_ = bws.WriteControl(websocket.PingMessage, f.Payload, time.Now().Add(5*time.Second))

		case ws.OpPong:
			metrics.Ctrl.WithLabelValues("pong").Inc()
			_ = bws.WriteControl(websocket.PongMessage, f.Payload, time.Now().Add(5*time.Second))

		case ws.OpClose:
			metrics.Ctrl.WithLabelValues("close").Inc()
			code, reason := ws.ParseClosePayload(f.Payload)
			_ = bws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second))
			_ = ws.WriteCloseFrame(s, uint16(code), reason)
			return io.EOF
		}
	}
}

func pumpBackendToH3(ctx context.Context, bws *websocket.Conn, s io.Writer, lim config.Limits) error {
	bws.SetPingHandler(func(appData string) error {
		metrics.Ctrl.WithLabelValues("ping").Inc()
		_ = ws.WriteControlFrame(s, ws.OpPing, []byte(appData))
		return bws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})
	bws.SetPongHandler(func(appData string) error {
		metrics.Ctrl.WithLabelValues("pong").Inc()
		_ = ws.WriteControlFrame(s, ws.OpPong, []byte(appData))
		return nil
	})
	bws.SetCloseHandler(func(code int, text string) error {
		metrics.Ctrl.WithLabelValues("close").Inc()
		_ = ws.WriteCloseFrame(s, uint16(code), text)
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := bws.SetReadDeadline(time.Now().Add(lim.ReadTimeout)); err != nil {
			return err
		}
		mt, data, err := bws.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				_ = ws.WriteCloseFrame(s, uint16(ce.Code), ce.Text)
			} else {
				_ = ws.WriteCloseFrame(s, 1011, "backend read error")
			}
			return err
		}

		if int64(len(data)) > lim.MaxMessageSize {
			metrics.OversizeDrops.WithLabelValues("message").Inc()
			_ = ws.WriteCloseFrame(s, 1009, "message too big")
			return errors.New("backend message too big")
		}

		switch mt {
		case websocket.TextMessage:
			metrics.Messages.WithLabelValues("h1_to_h3", "text").Inc()
			metrics.Bytes.WithLabelValues("h1_to_h3").Add(float64(len(data)))
			if err := ws.WriteDataFrame(s, ws.OpText, data, false, lim.MaxFrameSize); err != nil {
				return err
			}
		case websocket.BinaryMessage:
			metrics.Messages.WithLabelValues("h1_to_h3", "binary").Inc()
			metrics.Bytes.WithLabelValues("h1_to_h3").Add(float64(len(data)))
			if err := ws.WriteDataFrame(s, ws.OpBinary, data, false, lim.MaxFrameSize); err != nil {
				return err
			}
		}
	}
}
