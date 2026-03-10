package app

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/metrics"
	"h3ws2h1ws-proxy/internal/proxy"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/logging"
)

func Run() error {
	cfg := parseConfig()

	backendURL, err := url.Parse(cfg.BackendWS)
	if err != nil {
		return fmt.Errorf("bad -backend: %w", err)
	}
	if backendURL.Scheme != "ws" && backendURL.Scheme != "wss" {
		return fmt.Errorf("backend scheme must be ws or wss, got %q", backendURL.Scheme)
	}
	backendURL.Path = ""
	backendURL.RawPath = ""
	backendURL.RawQuery = ""
	backendURL.Fragment = ""

	if cfg.MetricsAddr != "" {
		startMetricsServer(cfg.MetricsAddr)
	} else {
		log.Printf("metrics disabled (use -metrics to enable)")
	}

	p := &proxy.Proxy{
		Backend:    backendURL,
		PathRegexp: cfg.PathRegexp,
		Debug:      cfg.Debug,
		Limits: config.Limits{
			MaxFrameSize:   cfg.MaxFrame,
			MaxMessageSize: cfg.MaxMessage,
			MaxConns:       cfg.MaxConns,
			ReadTimeout:    cfg.ReadTimeout,
			WriteTimeout:   cfg.WriteTimeout,
		},
	}

	var connHadRequest sync.Map
	var connRemoteAddr sync.Map

	mux := newProxyHandler(cfg, p, &connHadRequest)

	quicCfg := defaultQUICConfig(cfg.Debug, &connHadRequest, &connRemoteAddr)
	tlsCfg, err := loadServerTLSConfig(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load TLS config: %w", err)
	}

	server := http3.Server{
		Addr:            cfg.ListenAddr,
		Handler:         mux,
		TLSConfig:       tlsCfg,
		QUICConfig:      quicCfg,
		EnableDatagrams: false,
	}

	if cfg.Debug {
		server.Logger = slog.New(newQuicDebugLogFilter(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		server.ConnContext = func(ctx context.Context, c quic.Connection) context.Context {
			log.Printf("[debug] http3 conn context: conn_id=%v local=%s remote=%s", c.Context().Value(quic.ConnectionTracingKey), c.LocalAddr(), c.RemoteAddr())
			return ctx
		}
		// Keep debug hooks passive: avoid StreamHijacker / UniStreamHijacker overrides,
		// as they may interfere with stream dispatch on some client + quic-go combinations.
	}

	if cfg.Debug {
		log.Printf("[debug] quic config: max_idle=%s keepalive=%s datagrams=%v allow_0rtt=%v incoming_streams=%d incoming_uni_streams=%d stream_recv_window=%d conn_recv_window=%d", quicCfg.MaxIdleTimeout, quicCfg.KeepAlivePeriod, quicCfg.EnableDatagrams, quicCfg.Allow0RTT, quicCfg.MaxIncomingStreams, quicCfg.MaxIncomingUniStreams, quicCfg.MaxStreamReceiveWindow, quicCfg.MaxConnectionReceiveWindow)
	}

	log.Printf("HTTP/3 WS proxy listening on udp %s, path=%s, backend=%s, debug=%v", cfg.ListenAddr, cfg.PathPattern, backendURL.String(), cfg.Debug)
	if err := server.ListenAndServe(); err != nil {
		return fmt.Errorf("ListenAndServe: %w", err)
	}
	return nil
}

func newProxyHandler(cfg config.Config, p *proxy.Proxy, connHadRequest *sync.Map) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Debug {
			log.Printf("[debug] incoming http request: method=%s proto=%s host=%s path=%s remote=%s", r.Method, r.Proto, r.Host, r.URL.String(), r.RemoteAddr)
		}

		if connHadRequest != nil {
			connHadRequest.Store(r.RemoteAddr, true)
		}

		path := requestPath(r)
		if path != r.URL.Path {
			r.URL.Path = path
			r.URL.RawPath = ""
		}
		if isHealthPath(path) {
			handleHealthRequest(w, r)
			return
		}

		if r.Method != http.MethodConnect {
			if path == "/" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok\n"))
				return
			}
			http.NotFound(w, r)
			return
		}

		p.HandleH3WebSocket(w, r)
	})
	return mux
}

func isHealthPath(path string) bool {
	return path == "/health/tcp" || path == "/health/udp"
}

func requestPath(r *http.Request) string {
	if p := normalizeRequestPath(r.URL.Path); p != "" {
		return p
	}
	if u, err := url.Parse(r.URL.String()); err == nil {
		if p := normalizeRequestPath(u.Path); p != "" {
			return p
		}
	}
	return "/"
}

func normalizeRequestPath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "//") {
		if i := strings.Index(p[2:], "/"); i >= 0 {
			return p[i+2:]
		}
		return "/"
	}
	return p
}

func handleHealthRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	// For CONNECT health probes return a neutral 200 response only.
	// No websocket-specific response headers or frames are emitted here.
	w.WriteHeader(http.StatusOK)
}
func parseConfig() config.Config {
	var cfg config.Config

	flag.StringVar(&cfg.ListenAddr, "listen", ":443", "UDP listen addr for HTTP/3 (e.g. :443, :8443)")
	flag.StringVar(&cfg.CertFile, "cert", "cert.pem", "TLS cert PEM")
	flag.StringVar(&cfg.KeyFile, "key", "key.pem", "TLS key PEM")

	flag.StringVar(&cfg.BackendWS, "backend", "ws://127.0.0.1:8080", "backend ws:// or wss:// URL (HTTP/1.1 WebSocket), without path")
	flag.StringVar(&cfg.PathPattern, "path", "^/ws$", "regexp pattern for RFC9220 websocket CONNECT path")

	flag.StringVar(&cfg.MetricsAddr, "metrics", "", "TCP addr for Prometheus /metrics (empty disables metrics server)")
	flag.Int64Var(&cfg.MaxFrame, "max-frame", 1<<20, "max ws frame payload bytes (H3 side)")
	flag.Int64Var(&cfg.MaxMessage, "max-message", 8<<20, "max reassembled message bytes (H3 side)")
	flag.Int64Var(&cfg.MaxConns, "max-conns", 2000, "max concurrent sessions")
	flag.DurationVar(&cfg.ReadTimeout, "read-timeout", 120*time.Second, "read timeout")
	flag.DurationVar(&cfg.WriteTimeout, "write-timeout", 15*time.Second, "write timeout")
	flag.BoolVar(&cfg.Debug, "debug", false, "enable verbose debug logs for QUIC/HTTP3 and proxy flow")
	flag.Parse()

	pathRegexp, err := regexp.Compile(cfg.PathPattern)
	if err != nil {
		log.Fatalf("bad -path regexp: %v", err)
	}
	cfg.PathRegexp = pathRegexp

	return cfg
}

func startMetricsServer(addr string) {
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		log.Printf("metrics listening on http://%s/metrics", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server error: %v", err)
		}
	}()
}

func defaultQUICConfig(debug bool, connHadRequest, connRemoteAddr *sync.Map) *quic.Config {
	quicCfg := &quic.Config{
		EnableDatagrams:                false,
		MaxIdleTimeout:                 60 * time.Second,
		KeepAlivePeriod:                20 * time.Second,
		MaxIncomingStreams:             100,
		MaxIncomingUniStreams:          100,
		InitialStreamReceiveWindow:     2 << 20,
		MaxStreamReceiveWindow:         8 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
		Allow0RTT:                      false,
	}

	if debug {
		quicCfg.Tracer = func(_ context.Context, _ logging.Perspective, connID quic.ConnectionID) *logging.ConnectionTracer {
			var rxPackets int64
			var txPackets int64
			var droppedPackets int64
			var rxClientBidiStreamFrames int64
			var rxClientUniStreamFrames int64
			var rxServerBidiStreamFrames int64
			var rxServerUniStreamFrames int64
			var connStartedAt time.Time
			var firstClientUniStreamID int64 = -1
			var loggedClientUniFinHint bool

			log.Printf("[debug] quic connection tracer attached: conn_id=%s", connID)
			observeRxStreamFrames := func(frames []logging.Frame) {
				for _, f := range frames {
					sf, ok := f.(*logging.StreamFrame)
					if !ok {
						continue
					}
					sid := uint64(sf.StreamID)
					switch sid % 4 {
					case 0:
						atomic.AddInt64(&rxClientBidiStreamFrames, 1)
					case 1:
						atomic.AddInt64(&rxServerBidiStreamFrames, 1)
					case 2:
						atomic.AddInt64(&rxClientUniStreamFrames, 1)
						if firstClientUniStreamID < 0 {
							firstClientUniStreamID = int64(sf.StreamID)
						}
						if !loggedClientUniFinHint && sf.StreamID == 2 && sf.Offset == 0 && sf.Fin && sf.Length <= 16 {
							loggedClientUniFinHint = true
							log.Printf("[debug] quic conn hint: conn_id=%s first client uni stream closed immediately (id=2 off=%d len=%d fin=%v); this often indicates peer closed HTTP/3 control stream before opening request stream", connID, sf.Offset, sf.Length, sf.Fin)
						}
					case 3:
						atomic.AddInt64(&rxServerUniStreamFrames, 1)
					}
				}
			}
			return &logging.ConnectionTracer{
				StartedConnection: func(local, remote net.Addr, srcConnID, destConnID logging.ConnectionID) {
					connStartedAt = time.Now()
					if connRemoteAddr != nil {
						connRemoteAddr.Store(connID, remote.String())
					}
					log.Printf("[debug] quic conn started: local=%s remote=%s src_conn_id=%s dest_conn_id=%s", local, remote, srcConnID, destConnID)
				},
				SentTransportParameters: func(tp *logging.TransportParameters) {
					log.Printf("[debug] quic transport params sent: conn_id=%s max_idle=%s max_udp_payload=%d initial_max_streams_bidi=%d initial_max_streams_uni=%d", connID, tp.MaxIdleTimeout, tp.MaxUDPPayloadSize, tp.MaxBidiStreamNum, tp.MaxUniStreamNum)
				},
				ReceivedTransportParameters: func(tp *logging.TransportParameters) {
					log.Printf("[debug] quic transport params recv: conn_id=%s max_idle=%s max_udp_payload=%d initial_max_streams_bidi=%d initial_max_streams_uni=%d", connID, tp.MaxIdleTimeout, tp.MaxUDPPayloadSize, tp.MaxBidiStreamNum, tp.MaxUniStreamNum)
				},
				ReceivedLongHeaderPacket: func(_ *logging.ExtendedHeader, _ logging.ByteCount, _ logging.ECN, frames []logging.Frame) {
					observeRxStreamFrames(frames)
					atomic.AddInt64(&rxPackets, 1)
				},
				ReceivedShortHeaderPacket: func(_ *logging.ShortHeader, _ logging.ByteCount, _ logging.ECN, frames []logging.Frame) {
					observeRxStreamFrames(frames)
					atomic.AddInt64(&rxPackets, 1)
				},
				SentLongHeaderPacket: func(_ *logging.ExtendedHeader, _ logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, _ []logging.Frame) {
					atomic.AddInt64(&txPackets, 1)
				},
				SentShortHeaderPacket: func(_ *logging.ShortHeader, _ logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, _ []logging.Frame) {
					atomic.AddInt64(&txPackets, 1)
				},
				DroppedPacket: func(pt logging.PacketType, _ logging.PacketNumber, _ logging.ByteCount, reason logging.PacketDropReason) {
					atomic.AddInt64(&droppedPackets, 1)
					if !isExpectedDroppedPacket(pt, reason) {
						log.Printf("[debug] quic packet dropped: conn_id=%s packet_type=%s reason=%s", connID, packetTypeName(pt), packetDropReasonName(reason))
					}
				},
				ChoseALPN: func(protocol string) {
					log.Printf("[debug] quic conn alpn negotiated: conn_id=%s alpn=%q", connID, protocol)
				},
				ClosedConnection: func(err error) {
					hadReq := false
					if connHadRequest != nil && connRemoteAddr != nil {
						if remote, ok := connRemoteAddr.Load(connID); ok {
							connRemoteAddr.Delete(connID)
							if remoteAddr, ok := remote.(string); ok {
								if v, ok := connHadRequest.Load(remoteAddr); ok {
									hadReq, _ = v.(bool)
									connHadRequest.Delete(remoteAddr)
								}
							}
						}
					}
					if err != nil {
						if !hadReq {
							clientBidi := atomic.LoadInt64(&rxClientBidiStreamFrames)
							clientUni := atomic.LoadInt64(&rxClientUniStreamFrames)
							serverBidi := atomic.LoadInt64(&rxServerBidiStreamFrames)
							serverUni := atomic.LoadInt64(&rxServerUniStreamFrames)
							lifetime := time.Since(connStartedAt)
							log.Printf("[debug] quic conn closed before any request: conn_id=%s err=%v lifetime=%s rx_packets=%d tx_packets=%d dropped_packets=%d rx_stream_frames(client_bidi=%d client_uni=%d server_bidi=%d server_uni=%d)", connID, err, lifetime, atomic.LoadInt64(&rxPackets), atomic.LoadInt64(&txPackets), atomic.LoadInt64(&droppedPackets), clientBidi, clientUni, serverBidi, serverUni)
							diagnoseMissingRequestStream(connID, err, clientBidi, clientUni, serverBidi, serverUni, firstClientUniStreamID)
							return
						}
						log.Printf("[debug] quic conn closed: conn_id=%s err=%v rx_packets=%d tx_packets=%d dropped_packets=%d", connID, err, atomic.LoadInt64(&rxPackets), atomic.LoadInt64(&txPackets), atomic.LoadInt64(&droppedPackets))
						return
					}
					if !hadReq {
						clientBidi := atomic.LoadInt64(&rxClientBidiStreamFrames)
						clientUni := atomic.LoadInt64(&rxClientUniStreamFrames)
						serverBidi := atomic.LoadInt64(&rxServerBidiStreamFrames)
						serverUni := atomic.LoadInt64(&rxServerUniStreamFrames)
						lifetime := time.Since(connStartedAt)
						log.Printf("[debug] quic conn closed cleanly before any request: conn_id=%s lifetime=%s rx_packets=%d tx_packets=%d dropped_packets=%d rx_stream_frames(client_bidi=%d client_uni=%d server_bidi=%d server_uni=%d)", connID, lifetime, atomic.LoadInt64(&rxPackets), atomic.LoadInt64(&txPackets), atomic.LoadInt64(&droppedPackets), clientBidi, clientUni, serverBidi, serverUni)
						diagnoseMissingRequestStream(connID, err, clientBidi, clientUni, serverBidi, serverUni, firstClientUniStreamID)
						return
					}
					log.Printf("[debug] quic conn closed cleanly: conn_id=%s rx_packets=%d tx_packets=%d dropped_packets=%d", connID, atomic.LoadInt64(&rxPackets), atomic.LoadInt64(&txPackets), atomic.LoadInt64(&droppedPackets))
				},
				Debug: func(name, msg string) {
					log.Printf("[debug] quic conn event: conn_id=%s name=%s msg=%s", connID, name, msg)
				},
			}
		}
	}

	return quicCfg
}

func packetTypeName(pt logging.PacketType) string {
	switch pt {
	case logging.PacketTypeInitial:
		return "initial"
	case logging.PacketTypeHandshake:
		return "handshake"
	case logging.PacketTypeRetry:
		return "retry"
	case logging.PacketType0RTT:
		return "0rtt"
	case logging.PacketTypeVersionNegotiation:
		return "version_negotiation"
	case logging.PacketType1RTT:
		return "1rtt"
	case logging.PacketTypeStatelessReset:
		return "stateless_reset"
	case logging.PacketTypeNotDetermined:
		return "not_determined"
	default:
		return fmt.Sprintf("unknown(%d)", pt)
	}
}

func packetDropReasonName(reason logging.PacketDropReason) string {
	switch reason {
	case logging.PacketDropKeyUnavailable:
		return "key_unavailable"
	case logging.PacketDropUnknownConnectionID:
		return "unknown_connection_id"
	case logging.PacketDropHeaderParseError:
		return "header_parse_error"
	case logging.PacketDropPayloadDecryptError:
		return "payload_decrypt_error"
	case logging.PacketDropProtocolViolation:
		return "protocol_violation"
	case logging.PacketDropDOSPrevention:
		return "dos_prevention"
	case logging.PacketDropUnsupportedVersion:
		return "unsupported_version"
	case logging.PacketDropUnexpectedPacket:
		return "unexpected_packet"
	case logging.PacketDropUnexpectedSourceConnectionID:
		return "unexpected_source_connection_id"
	case logging.PacketDropUnexpectedVersion:
		return "unexpected_version"
	case logging.PacketDropDuplicate:
		return "duplicate"
	default:
		return fmt.Sprintf("unknown(%d)", reason)
	}
}
func isExpectedDroppedPacket(pt logging.PacketType, reason logging.PacketDropReason) bool {
	return pt == logging.PacketTypeNotDetermined && reason == logging.PacketDropUnknownConnectionID
}

func diagnoseMissingRequestStream(connID quic.ConnectionID, closeErr error, clientBidi, clientUni, serverBidi, serverUni, firstClientUniStreamID int64) {
	if clientBidi > 0 {
		if closeErr != nil && strings.Contains(closeErr.Error(), "expected first frame to be a HEADERS frame") {
			metrics.PreRequestClose.WithLabelValues("request_stream_invalid_first_frame_or_headers").Inc()
			metrics.Errors.WithLabelValues("h3_framing").Inc()
			log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s request stream failed before handler with reason=%q", connID, closeErr.Error())
			log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s in quic-go this reason can mean either non-HEADERS first frame OR invalid HEADERS/QPACK block", connID)
			log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s remediation: verify first request-stream frame is HEADERS (RFC9114) and header block is valid QPACK for Extended CONNECT (RFC9220)", connID)
		}
		return
	}
	log.Printf("[debug] quic conn request-stream hint: conn_id=%s no client-initiated bidi stream frames observed (expected stream id 0/4/8... for CONNECT requests)", connID)
	switch {
	case clientUni > 0 && serverBidi == 0 && serverUni == 0:
		metrics.PreRequestClose.WithLabelValues("no_bidi_request_stream").Inc()
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s only client unidirectional stream traffic observed (typically control stream / SETTINGS), no request stream created", connID)
		if firstClientUniStreamID >= 0 {
			log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s first_client_uni_stream_id=%d (expected control stream id=2 on first h3 unidirectional stream)", connID, firstClientUniStreamID)
		}
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s peer likely closed before opening request stream; verify client actually sends CONNECT on client-initiated bidirectional stream", connID)
	case clientUni == 0 && serverBidi == 0 && serverUni == 0:
		metrics.PreRequestClose.WithLabelValues("no_h3_stream_activity").Inc()
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s handshake completed but no HTTP/3 stream activity followed", connID)
	default:
		metrics.PreRequestClose.WithLabelValues("stream_activity_without_request").Inc()
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s stream activity observed without client request stream (client_uni=%d server_bidi=%d server_uni=%d)", connID, clientUni, serverBidi, serverUni)
	}
}

type quicDebugLogFilter struct {
	next slog.Handler
}

func newQuicDebugLogFilter(next slog.Handler) slog.Handler {
	return &quicDebugLogFilter{next: next}
}

func (h *quicDebugLogFilter) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *quicDebugLogFilter) Handle(ctx context.Context, record slog.Record) error {
	if shouldSuppressQuicDebugRecord(record) {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h *quicDebugLogFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &quicDebugLogFilter{next: h.next.WithAttrs(attrs)}
}

func (h *quicDebugLogFilter) WithGroup(name string) slog.Handler {
	return &quicDebugLogFilter{next: h.next.WithGroup(name)}
}

func shouldSuppressQuicDebugRecord(record slog.Record) bool {
	if record.Level != slog.LevelDebug {
		return false
	}
	if record.Message != "accepting unidirectional stream failed" && record.Message != "handling connection failed" {
		return false
	}
	errText := ""
	record.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			errText = a.Value.String()
			return false
		}
		return true
	})
	return strings.Contains(errText, "NO_ERROR (remote)")
}

func loadServerTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	tlsCfg := config.DefaultTLSConfig()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	tlsCfg.Certificates = []tls.Certificate{cert}
	return tlsCfg, nil
}
