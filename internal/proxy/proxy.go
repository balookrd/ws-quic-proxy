package proxy

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/metrics"
	"h3ws2h1ws-proxy/internal/ws"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go/http3"
)

type Proxy struct {
	Backend    *url.URL
	PathRegexp *regexp.Regexp
	Debug      bool
	Limits     config.Limits
	active     int64
}

func (p *Proxy) debugf(format string, args ...any) {
	if p.Debug {
		log.Printf("[debug] "+format, args...)
	}
}

func (p *Proxy) backendURLForRequest(r *http.Request) *url.URL {
	target := *p.Backend
	target.Path = r.URL.Path
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	target.Fragment = ""
	return &target
}

func (p *Proxy) HandleH3WebSocket(w http.ResponseWriter, r *http.Request) {
	p.debugf("incoming request: method=%s proto=%s path=%s remote=%s", r.Method, r.Proto, r.URL.String(), r.RemoteAddr)

	if atomic.AddInt64(&p.active, 1) > p.Limits.MaxConns {
		atomic.AddInt64(&p.active, -1)
		metrics.Rejected.WithLabelValues("max_conns").Inc()
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer atomic.AddInt64(&p.active, -1)

	if r.Method != http.MethodConnect {
		metrics.Rejected.WithLabelValues("method").Inc()
		http.Error(w, "expected CONNECT", http.StatusMethodNotAllowed)
		return
	}
	if p.PathRegexp != nil && !p.PathRegexp.MatchString(r.URL.Path) {
		metrics.Rejected.WithLabelValues("path").Inc()
		http.Error(w, "path not allowed", http.StatusNotFound)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	ver := r.Header.Get("Sec-WebSocket-Version")
	if key == "" || ver != "13" {
		metrics.Rejected.WithLabelValues("bad_headers").Inc()
		http.Error(w, "missing/invalid websocket headers", http.StatusBadRequest)
		return
	}

	hs, ok := w.(http3.HTTPStreamer)
	if !ok {
		metrics.Errors.WithLabelValues("no_stream_takeover").Inc()
		http.Error(w, "http3 stream takeover not supported", http.StatusInternalServerError)
		return
	}
	stream := hs.HTTPStream()
	defer func() { _ = stream.Close() }()
	p.debugf("http3 stream takeover success: path=%s", r.URL.Path)

	w.Header().Set("Sec-WebSocket-Accept", ws.ComputeAccept(key))
	subp := r.Header.Get("Sec-WebSocket-Protocol")
	if subp != "" {
		w.Header().Set("Sec-WebSocket-Protocol", ws.PickFirstToken(subp))
	}
	w.WriteHeader(http.StatusOK)

	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		ReadBufferSize:    64 << 10,
		WriteBufferSize:   64 << 10,
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: false,
	}
	backendHeader := http.Header{}
	if subp != "" {
		backendHeader.Set("Sec-WebSocket-Protocol", ws.PickFirstToken(subp))
	}
	backendURL := p.backendURLForRequest(r)
	p.debugf("dial backend websocket: %s", backendURL.String())
	bws, resp, err := dialer.Dial(backendURL.String(), backendHeader)
	if err != nil {
		metrics.Errors.WithLabelValues("backend_dial").Inc()
		if resp != nil {
			log.Printf("backend dial failed to %s: %v (status=%s)", backendURL.String(), err, resp.Status)
		} else {
			log.Printf("backend dial failed to %s: %v", backendURL.String(), err)
		}
		_ = ws.WriteCloseFrame(stream, 1011, "backend dial failed")
		return
	}
	defer func() { _ = bws.Close() }()
	p.debugf("backend websocket connected: %s", backendURL.String())

	metrics.Accepted.Inc()
	metrics.ActiveSessions.Inc()
	defer metrics.ActiveSessions.Dec()

	sessionStarted := time.Now()
	st := &sessionTrafficStats{}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bws.SetReadLimit(p.Limits.MaxMessageSize)

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- pumpH3ToBackend(ctx, stream, bws, p.Limits, st, p.Debug)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- pumpBackendToH3(ctx, bws, stream, p.Limits, st, p.Debug)
	}()

	err1 := <-errCh
	cancel()
	_ = stream.Close()
	_ = bws.Close()
	wg.Wait()

	metrics.SessionDuration.Observe(time.Since(sessionStarted).Seconds())
	metrics.SessionTrafficBytes.WithLabelValues("h3_to_h1").Observe(float64(atomic.LoadUint64(&st.h3ToH1Bytes)))
	metrics.SessionTrafficBytes.WithLabelValues("h1_to_h3").Observe(float64(atomic.LoadUint64(&st.h1ToH3Bytes)))
	p.debugf("session finished: path=%s dur=%s h3_to_h1_bytes=%d h1_to_h3_bytes=%d err=%v", r.URL.Path, time.Since(sessionStarted), atomic.LoadUint64(&st.h3ToH1Bytes), atomic.LoadUint64(&st.h1ToH3Bytes), err1)

	if err1 != nil && !errors.Is(err1, context.Canceled) && !ws.IsNetClose(err1) {
		metrics.Errors.WithLabelValues("session").Inc()
		log.Printf("session ended: %v", err1)
	}
}
