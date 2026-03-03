package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/metrics"
	"h3ws2h1ws-proxy/internal/ws"

	"github.com/gorilla/websocket"
)

type sessionTrafficStats struct {
	h3ToH1Bytes uint64
	h1ToH3Bytes uint64
}

func debugf(enabled bool, format string, args ...any) {
	if enabled {
		log.Printf("[debug] "+format, args...)
	}
}

func pumpH3ToBackend(ctx context.Context, s io.ReadWriter, bws *websocket.Conn, lim config.Limits, st *sessionTrafficStats, debug bool) error {
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
			metrics.MessageSize.WithLabelValues("h3_to_h1", "text").Observe(float64(len(msg)))
			metrics.Bytes.WithLabelValues("h3_to_h1").Add(float64(len(msg)))
			atomic.AddUint64(&st.h3ToH1Bytes, uint64(len(msg)))
			err := bws.WriteMessage(websocket.TextMessage, msg)
			if err == nil {
				debugf(debug, "h3->h1 text message forwarded bytes=%d", len(msg))
			}
			return err
		case ws.OpBinary:
			metrics.Messages.WithLabelValues("h3_to_h1", "binary").Inc()
			metrics.MessageSize.WithLabelValues("h3_to_h1", "binary").Observe(float64(len(msg)))
			metrics.Bytes.WithLabelValues("h3_to_h1").Add(float64(len(msg)))
			atomic.AddUint64(&st.h3ToH1Bytes, uint64(len(msg)))
			err := bws.WriteMessage(websocket.BinaryMessage, msg)
			if err == nil {
				debugf(debug, "h3->h1 binary message forwarded bytes=%d", len(msg))
			}
			return err
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
			if errors.Is(err, io.EOF) || ws.IsNetClose(err) {
				debugf(debug, "h3->h1 input half-closed: %v", err)
				return nil
			}
			debugf(debug, "h3->h1 read frame error: %v", err)
			return err
		}
		debugf(debug, "h3->h1 frame opcode=%d fin=%v payload=%d", f.Opcode, f.Fin, len(f.Payload))

		switch f.Opcode {
		case ws.OpText, ws.OpBinary:
			if f.Opcode == ws.OpText {
				metrics.Frames.WithLabelValues("h3_to_h1", "text").Inc()
			} else {
				metrics.Frames.WithLabelValues("h3_to_h1", "binary").Inc()
			}
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
					debugf(debug, "h3->h1 write message error: %v", err)
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
			metrics.Frames.WithLabelValues("h3_to_h1", "cont").Inc()
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
					debugf(debug, "h3->h1 write reassembled message error: %v", err)
					return err
				}
			}

		case ws.OpPing:
			metrics.Frames.WithLabelValues("h3_to_h1", "ping").Inc()
			metrics.Ctrl.WithLabelValues("ping").Inc()
			if err := ws.WriteControlFrame(s, ws.OpPong, f.Payload); err != nil {
				debugf(debug, "h3->h1 pong write error: %v", err)
				return err
			}
			if err := bws.WriteControl(websocket.PingMessage, f.Payload, time.Now().Add(5*time.Second)); err == nil {
				debugf(debug, "h3->h1 ping forwarded payload=%d", len(f.Payload))
			}

		case ws.OpPong:
			metrics.Frames.WithLabelValues("h3_to_h1", "pong").Inc()
			metrics.Ctrl.WithLabelValues("pong").Inc()
			if err := bws.WriteControl(websocket.PongMessage, f.Payload, time.Now().Add(5*time.Second)); err == nil {
				debugf(debug, "h3->h1 pong forwarded payload=%d", len(f.Payload))
			}

		case ws.OpClose:
			metrics.Frames.WithLabelValues("h3_to_h1", "close").Inc()
			metrics.Ctrl.WithLabelValues("close").Inc()
			code, reason := ws.ParseClosePayload(f.Payload)
			if err := bws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(5*time.Second)); err == nil {
				debugf(debug, "h3->h1 close forwarded code=%d reason=%q", code, reason)
			}
			_ = ws.WriteCloseFrame(s, uint16(code), reason)
			return io.EOF
		}
	}
}

func pumpBackendToH3(ctx context.Context, bws *websocket.Conn, s io.Writer, lim config.Limits, st *sessionTrafficStats, debug bool) error {
	bws.SetPingHandler(func(appData string) error {
		metrics.Frames.WithLabelValues("h1_to_h3", "ping").Inc()
		metrics.Ctrl.WithLabelValues("ping").Inc()
		if err := ws.WriteControlFrame(s, ws.OpPing, []byte(appData)); err == nil {
			debugf(debug, "h1->h3 ping forwarded payload=%d", len(appData))
		}
		return bws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})
	bws.SetPongHandler(func(appData string) error {
		metrics.Frames.WithLabelValues("h1_to_h3", "pong").Inc()
		metrics.Ctrl.WithLabelValues("pong").Inc()
		if err := ws.WriteControlFrame(s, ws.OpPong, []byte(appData)); err == nil {
			debugf(debug, "h1->h3 pong forwarded payload=%d", len(appData))
		}
		return nil
	})
	bws.SetCloseHandler(func(code int, text string) error {
		metrics.Frames.WithLabelValues("h1_to_h3", "close").Inc()
		metrics.Ctrl.WithLabelValues("close").Inc()
		if err := ws.WriteCloseFrame(s, uint16(code), text); err == nil {
			debugf(debug, "h1->h3 close forwarded code=%d reason=%q", code, text)
		}
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if lim.ReadTimeout > 0 {
			if err := bws.SetReadDeadline(time.Now().Add(lim.ReadTimeout)); err != nil {
				return err
			}
		} else {
			if err := bws.SetReadDeadline(time.Time{}); err != nil {
				return err
			}
		}
		mt, data, err := bws.ReadMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				debugf(debug, "h1->h3 backend read timeout: %v (continuing)", err)
				continue
			}
			if ws.IsNetClose(err) {
				debugf(debug, "h1->h3 backend input half-closed: %v", err)
				return nil
			}
			if ce, ok := err.(*websocket.CloseError); ok {
				switch ce.Code {
				case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived:
					debugf(debug, "h1->h3 backend input half-closed: code=%d reason=%q", ce.Code, ce.Text)
					_ = ws.WriteCloseFrame(s, uint16(ce.Code), ce.Text)
					return nil
				}
			}
			debugf(debug, "h1->h3 backend read error: %v", err)
			if ce, ok := err.(*websocket.CloseError); ok {
				_ = ws.WriteCloseFrame(s, uint16(ce.Code), ce.Text)
			} else {
				_ = ws.WriteCloseFrame(s, 1011, "backend read error")
			}
			return err
		}
		debugf(debug, "h1->h3 message type=%d payload=%d", mt, len(data))

		if int64(len(data)) > lim.MaxMessageSize {
			metrics.OversizeDrops.WithLabelValues("message").Inc()
			_ = ws.WriteCloseFrame(s, 1009, "message too big")
			return errors.New("backend message too big")
		}

		switch mt {
		case websocket.TextMessage:
			metrics.Frames.WithLabelValues("h1_to_h3", "text").Inc()
			metrics.Messages.WithLabelValues("h1_to_h3", "text").Inc()
			metrics.MessageSize.WithLabelValues("h1_to_h3", "text").Observe(float64(len(data)))
			metrics.Bytes.WithLabelValues("h1_to_h3").Add(float64(len(data)))
			atomic.AddUint64(&st.h1ToH3Bytes, uint64(len(data)))
			if err := ws.WriteDataFrame(s, ws.OpText, data, false, lim.MaxFrameSize); err != nil {
				debugf(debug, "h1->h3 write text frame error: %v", err)
				return err
			}
			debugf(debug, "h1->h3 text message forwarded bytes=%d", len(data))
		case websocket.BinaryMessage:
			metrics.Frames.WithLabelValues("h1_to_h3", "binary").Inc()
			metrics.Messages.WithLabelValues("h1_to_h3", "binary").Inc()
			metrics.MessageSize.WithLabelValues("h1_to_h3", "binary").Observe(float64(len(data)))
			metrics.Bytes.WithLabelValues("h1_to_h3").Add(float64(len(data)))
			atomic.AddUint64(&st.h1ToH3Bytes, uint64(len(data)))
			if err := ws.WriteDataFrame(s, ws.OpBinary, data, false, lim.MaxFrameSize); err != nil {
				debugf(debug, "h1->h3 write binary frame error: %v", err)
				return err
			}
			debugf(debug, "h1->h3 binary message forwarded bytes=%d", len(data))
		}
	}
}
