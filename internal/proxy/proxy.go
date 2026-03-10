package proxy

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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

type websocketBufferPool struct {
	pool sync.Pool
}

func newWebsocketBufferPool(bufSize int) *websocketBufferPool {
	return &websocketBufferPool{pool: sync.Pool{New: func() any {
		b := make([]byte, bufSize)
		return &b
	}}}
}

func (p *websocketBufferPool) Get() interface{} {
	return p.pool.Get()
}

func (p *websocketBufferPool) Put(x interface{}) {
	switch b := x.(type) {
	case *[]byte:
		p.pool.Put(b)
	case []byte:
		bb := b
		p.pool.Put(&bb)
	}
}

var backendWriteBufferPool = newWebsocketBufferPool(16 << 10)

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

	// Compatibility note:
	// Some clients / gateways still omit RFC8441 `:protocol` and
	// Sec-WebSocket-Version over H3 Extended CONNECT.
	// We reject only explicitly invalid values, but tolerate absence.
	if proto := firstNonEmpty(
		r.Header.Get(":protocol"),
		r.Header.Get("protocol"),
		r.Header.Get("Protocol"),
	); proto != "" && proto != "websocket" {
		metrics.Rejected.WithLabelValues("bad_headers").Inc()
		http.Error(w, "missing/invalid :protocol websocket", http.StatusBadRequest)
		return
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	ver := r.Header.Get("Sec-WebSocket-Version")
	if ver != "" && ver != "13" {
		metrics.Rejected.WithLabelValues("bad_headers").Inc()
		http.Error(w, "missing/invalid websocket headers", http.StatusBadRequest)
		return
	}

	rc := http.NewResponseController(w)
	fullDuplexEnabled := false
	if err := rc.EnableFullDuplex(); err == nil {
		fullDuplexEnabled = true
	} else if !errors.Is(err, http.ErrNotSupported) {
		p.debugf("enable full duplex failed: %v", err)
	}

	hs, ok := w.(http3.HTTPStreamer)
	if !ok {
		metrics.Errors.WithLabelValues("no_stream_takeover").Inc()
		http.Error(w, "http3 stream takeover not supported", http.StatusInternalServerError)
		return
	}

	if key != "" {
		w.Header().Set("Sec-WebSocket-Accept", ws.ComputeAccept(key))
	}

	subp := r.Header.Get("Sec-WebSocket-Protocol")
	if subp != "" {
		w.Header().Set("Sec-WebSocket-Protocol", ws.PickFirstToken(subp))
	}
	w.WriteHeader(http.StatusOK)
	p.debugf("rfc9220 handshake response sent: status=200 path=%s", r.URL.Path)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	stream := hs.HTTPStream()
	defer func() { _ = stream.Close() }()
	if !fullDuplexEnabled {
		// HTTP/3 handlers may not implement ResponseController full-duplex hook,
		// but stream takeover gives us bidirectional access to the request stream.
		fullDuplexEnabled = true
	}
	p.debugf("full duplex mode: enabled=%v", fullDuplexEnabled)
	p.debugf("http3 stream takeover success: path=%s", r.URL.Path)

	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		ReadBufferSize:    16 << 10,
		WriteBufferSize:   16 << 10,
		WriteBufferPool:   backendWriteBufferPool,
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: false,
	}
	backendHeader := http.Header{}
	backendHeader["connection"] = []string{"Upgrade"}
	backendHeader["upgrade"] = []string{"websocket"}
	if subp != "" {
		backendHeader.Set("Sec-WebSocket-Protocol", ws.PickFirstToken(subp))
	}
	backendURL := p.backendURLForRequest(r)
	p.debugf("dial backend websocket: %s", backendURL.String())
	bws, resp, err := dialer.Dial(backendURL.String(), backendHeader)
	if err != nil {
		metrics.Errors.WithLabelValues("backend_dial").Inc()
		if resp != nil {
			p.debugf("backend dial failed to %s: %v (status=%s)", backendURL.String(), err, resp.Status)
		} else {
			p.debugf("backend dial failed to %s: %v", backendURL.String(), err)
		}
		_ = ws.WriteCloseFrame(stream, 1011, "backend dial failed")
		return
	}
	defer func() { _ = bws.Close() }()

	backendStatus := ""
	backendUpgrade := ""
	backendConnection := ""
	backendProto := ""
	if resp != nil {
		backendStatus = resp.Status
		backendUpgrade = resp.Header.Get("Upgrade")
		backendConnection = resp.Header.Get("Connection")
		backendProto = resp.Header.Get("Sec-WebSocket-Protocol")
		if resp.StatusCode != http.StatusSwitchingProtocols {
			metrics.Errors.WithLabelValues("backend_dial").Inc()
			p.debugf("backend websocket handshake unexpected status: backend=%s status=%s", backendURL.String(), resp.Status)
			_ = ws.WriteCloseFrame(stream, 1011, "backend handshake failed")
			return
		}
	}
	p.debugf("backend dial ok: remote=%s path=%s backend=%s status=%s upgrade=%q connection=%q subprotocol=%q", r.RemoteAddr, r.URL.Path, backendURL.String(), backendStatus, backendUpgrade, backendConnection, backendProto)
	p.debugf("backend websocket connected: %s (status=%s upgrade=%q connection=%q subprotocol=%q)", backendURL.String(), backendStatus, backendUpgrade, backendConnection, backendProto)

	metrics.Accepted.Inc()
	metrics.ActiveSessions.Inc()
	defer metrics.ActiveSessions.Dec()

	sessionStarted := time.Now()
	st := &sessionTrafficStats{}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bws.SetReadLimit(p.Limits.MaxMessageSize)

	upstream, proto := logContextFields(r)

	type pumpResult struct {
		dir string
		err error
	}

	var wg sync.WaitGroup
	errCh := make(chan pumpResult, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- pumpResult{dir: "h3_to_h1", err: pumpH3ToBackend(ctx, stream, bws, p.Limits, st, p.Debug, upstream, proto)}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- pumpResult{dir: "h1_to_h3", err: pumpBackendToH3(ctx, bws, stream, p.Limits, st, p.Debug, upstream, proto)}
	}()

	first := <-errCh
	p.debugf("pump finished: dir=%s err=%v", first.dir, first.err)
	err1 := first.err
	if first.dir == "h3_to_h1" && (first.err == nil || errors.Is(first.err, io.EOF) || ws.IsNetClose(first.err)) {
		p.debugf("h3_to_h1 finished first with graceful close; waiting for backend->client pump to finish")
		second := <-errCh
		p.debugf("pump finished: dir=%s err=%v", second.dir, second.err)
		err1 = second.err
	} else {
		cancel()
		_ = stream.Close()
		_ = bws.Close()
		second := <-errCh
		p.debugf("pump finished after cancel: dir=%s err=%v", second.dir, second.err)
	}
	cancel()
	_ = stream.Close()
	_ = bws.Close()
	wg.Wait()

	dur := time.Since(sessionStarted)
	h3ToH1Bytes := atomic.LoadUint64(&st.h3ToH1Bytes)
	h1ToH3Bytes := atomic.LoadUint64(&st.h1ToH3Bytes)
	h3ToH1Messages := atomic.LoadUint64(&st.h3ToH1Messages)
	h1ToH3Messages := atomic.LoadUint64(&st.h1ToH3Messages)
	metrics.SessionDuration.Observe(dur.Seconds())
	metrics.SessionTrafficBytes.WithLabelValues("h3_to_h1").Observe(float64(h3ToH1Bytes))
	metrics.SessionTrafficBytes.WithLabelValues("h1_to_h3").Observe(float64(h1ToH3Bytes))
	p.debugf("session finished: path=%s dur=%s h3_to_h1_bytes=%d h1_to_h3_bytes=%d h3_to_h1_msgs=%d h1_to_h3_msgs=%d err=%v", r.URL.Path, dur, h3ToH1Bytes, h1ToH3Bytes, h3ToH1Messages, h1ToH3Messages, err1)
	p.debugf("backend session summary: remote=%s path=%s dur=%s h3_to_h1_bytes=%d h1_to_h3_bytes=%d h3_to_h1_msgs=%d h1_to_h3_msgs=%d err=%v", r.RemoteAddr, r.URL.Path, dur, h3ToH1Bytes, h1ToH3Bytes, h3ToH1Messages, h1ToH3Messages, err1)
	if h1ToH3Messages == 0 {
		p.debugf("backend diagnostic: no backend->client messages observed for remote=%s path=%s (backend=%s)", r.RemoteAddr, r.URL.Path, backendURL.String())
	}

	if err1 != nil && !errors.Is(err1, context.Canceled) && !ws.IsNetClose(err1) {
		metrics.Errors.WithLabelValues("session").Inc()
		log.Printf("session ended: %v", err1)
	}
}

func logContextFields(r *http.Request) (string, string) {
	host := r.Host
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSpace(host)
	upstream := host
	if dot := strings.Index(upstream, "."); dot > 0 {
		upstream = upstream[:dot]
	}
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		upstream = "unknown"
	}

	proto := strings.Trim(strings.ToLower(strings.TrimSpace(r.URL.Path)), "/")
	if slash := strings.LastIndex(proto, "/"); slash >= 0 {
		proto = proto[slash+1:]
	}
	if proto == "" {
		proto = "unknown"
	}

	return upstream, proto
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
