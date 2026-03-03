package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/ws"

	"github.com/gorilla/websocket"
)

func TestQUICToBackendAndBackWithoutLoss(t *testing.T) {
	backendURL, closeBackend := startEchoBackend(t)
	defer closeBackend()

	backendConn, _, err := websocket.DefaultDialer.Dial(backendURL, nil)
	if err != nil {
		t.Fatalf("dial backend websocket: %v", err)
	}
	defer backendConn.Close()

	quicSide, proxySide := net.Pipe()
	defer quicSide.Close()
	defer proxySide.Close()

	limits := config.Limits{
		MaxFrameSize:   1024,
		MaxMessageSize: 1024,
		ReadTimeout:    5 * time.Second,
		WriteTimeout:   5 * time.Second,
	}
	stats := &sessionTrafficStats{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errCh <- pumpH3ToBackend(ctx, proxySide, backendConn, limits, stats, false, "test-upstream", "h3")
	}()
	go func() {
		defer wg.Done()
		errCh <- pumpBackendToH3(ctx, backendConn, proxySide, limits, stats, false, "test-upstream", "h3")
	}()

	original := bytes.Repeat([]byte("quic-payload-"), 10)
	if err := quicSide.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline on quic side: %v", err)
	}
	if err := ws.WriteDataFrame(quicSide, ws.OpBinary, original, true, limits.MaxFrameSize); err != nil {
		t.Fatalf("write quic->proxy frame: %v", err)
	}

	opcode, echoed, err := readWSMessage(bufio.NewReader(quicSide), limits.MaxFrameSize)
	if err != nil {
		t.Fatalf("read echoed message from proxy: %v", err)
	}
	if opcode != ws.OpBinary {
		t.Fatalf("unexpected opcode: got %d want %d", opcode, ws.OpBinary)
	}
	if !bytes.Equal(echoed, original) {
		t.Fatalf("echoed payload mismatch: got %d bytes want %d", len(echoed), len(original))
	}

	cancel()
	_ = quicSide.Close()
	_ = proxySide.Close()
	_ = backendConn.Close()
	wg.Wait()
	close(errCh)

	for pumpErr := range errCh {
		if pumpErr == nil || errors.Is(pumpErr, context.Canceled) || errors.Is(pumpErr, io.EOF) || ws.IsNetClose(pumpErr) {
			continue
		}
		t.Fatalf("unexpected pump error: %v", pumpErr)
	}
}

func readWSMessage(br *bufio.Reader, maxFrame int64) (byte, []byte, error) {
	first, err := ws.ReadFrame(br, maxFrame)
	if err != nil {
		return 0, nil, err
	}
	if first.Opcode != ws.OpText && first.Opcode != ws.OpBinary {
		return 0, nil, errors.New("expected data frame")
	}
	payload := append([]byte(nil), first.Payload...)
	if first.Fin {
		return first.Opcode, payload, nil
	}
	for {
		next, err := ws.ReadFrame(br, maxFrame)
		if err != nil {
			return 0, nil, err
		}
		if next.Opcode != ws.OpCont {
			return 0, nil, errors.New("expected continuation frame")
		}
		payload = append(payload, next.Payload...)
		if next.Fin {
			return first.Opcode, payload, nil
		}
	}
}

func startEchoBackend(t *testing.T) (string, func()) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wsURL, srv.Close
}
