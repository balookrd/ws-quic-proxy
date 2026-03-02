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

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Debug {
			connHadRequest.Store(r.RemoteAddr, true)
			log.Printf("[debug] incoming http request: method=%s proto=%s host=%s path=%s remote=%s", r.Method, r.Proto, r.Host, r.URL.String(), r.RemoteAddr)
		}

		if strings.ToUpper(r.Method) == http.MethodConnect {
			p.HandleH3WebSocket(w, r)
			return
		}

		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}

		http.NotFound(w, r)
	})

	quicCfg := defaultQUICConfig(cfg.Debug, &connHadRequest, &connRemoteAddr)
	tlsCfg, err := loadServerTLSConfig(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load TLS config: %w", err)
	}

	server := http3.Server{
		Addr:       cfg.ListenAddr,
		Handler:    mux,
		TLSConfig:  tlsCfg,
		QUICConfig: quicCfg,
	}

	if cfg.Debug {
		server.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		server.ConnContext = func(ctx context.Context, c quic.Connection) context.Context {
			log.Printf("[debug] http3 conn context: conn_id=%v local=%s remote=%s", c.Context().Value(quic.ConnectionTracingKey), c.LocalAddr(), c.RemoteAddr())
			return ctx
		}
		server.StreamHijacker = func(ft http3.FrameType, connID quic.ConnectionTracingID, _ quic.Stream, err error) (bool, error) {
			if err != nil {
				log.Printf("[debug] http3 stream parse issue: conn_id=%v err=%v", connID, err)
				return false, nil
			}
			log.Printf("[debug] http3 stream first frame: conn_id=%v frame_type=%d", connID, ft)
			return false, nil
		}
		server.UniStreamHijacker = func(st http3.StreamType, connID quic.ConnectionTracingID, _ quic.ReceiveStream, err error) bool {
			if err != nil {
				log.Printf("[debug] http3 unistream parse issue: conn_id=%v err=%v", connID, err)
				return false
			}
			log.Printf("[debug] http3 unistream: conn_id=%v stream_type=%d", connID, st)
			return false
		}
	}

	if cfg.Debug {
		log.Printf("[debug] quic config: max_idle=%s keepalive=%s allow_0rtt=%v incoming_streams=%d incoming_uni_streams=%d stream_recv_window=%d conn_recv_window=%d", quicCfg.MaxIdleTimeout, quicCfg.KeepAlivePeriod, quicCfg.Allow0RTT, quicCfg.MaxIncomingStreams, quicCfg.MaxIncomingUniStreams, quicCfg.MaxStreamReceiveWindow, quicCfg.MaxConnectionReceiveWindow)
	}

	log.Printf("HTTP/3 WS proxy listening on udp %s, path=%s, backend=%s, debug=%v", cfg.ListenAddr, cfg.PathPattern, backendURL.String(), cfg.Debug)
	if err := server.ListenAndServe(); err != nil {
		return fmt.Errorf("ListenAndServe: %w", err)
	}
	return nil
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
		MaxIncomingStreams:             10000,
		MaxIncomingUniStreams:          1000,
		InitialStreamReceiveWindow:     2 << 20,
		MaxStreamReceiveWindow:         8 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
		Allow0RTT:                      false,
	}

	if debug {
		quicCfg.Tracer = func(_ context.Context, _ logging.Perspective, connID quic.ConnectionID) *logging.ConnectionTracer {
			const packetLogLimit = int64(12)
			var rxPackets int64
			var txPackets int64
			var droppedPackets int64
			var rxClientBidiStreamFrames int64
			var rxClientUniStreamFrames int64
			var rxServerBidiStreamFrames int64
			var rxServerUniStreamFrames int64
			var connStartedAt time.Time
			var firstClientUniStreamID int64 = -1

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
				SentLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, frames []logging.Frame) {
					n := atomic.AddInt64(&txPackets, 1)
					if n <= packetLogLimit {
						log.Printf("[debug] quic packet sent: conn_id=%s kind=long type=%s pn=%d bytes=%d frames=%s", connID, hdr.Type, hdr.PacketNumber, size, summarizeQUICFrames(frames))
					}
				},
				SentShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, _ logging.ECN, _ *logging.AckFrame, frames []logging.Frame) {
					n := atomic.AddInt64(&txPackets, 1)
					if n <= packetLogLimit {
						log.Printf("[debug] quic packet sent: conn_id=%s kind=short pn=%d bytes=%d key_phase=%d frames=%s", connID, hdr.PacketNumber, size, hdr.KeyPhase, summarizeQUICFrames(frames))
					}
				},
				ReceivedLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, _ logging.ECN, frames []logging.Frame) {
					observeRxStreamFrames(frames)
					n := atomic.AddInt64(&rxPackets, 1)
					if n <= packetLogLimit {
						log.Printf("[debug] quic packet recv: conn_id=%s kind=long type=%s pn=%d bytes=%d frames=%s", connID, hdr.Type, hdr.PacketNumber, size, summarizeQUICFrames(frames))
					}
				},
				ReceivedShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, _ logging.ECN, frames []logging.Frame) {
					observeRxStreamFrames(frames)
					n := atomic.AddInt64(&rxPackets, 1)
					if n <= packetLogLimit {
						log.Printf("[debug] quic packet recv: conn_id=%s kind=short pn=%d bytes=%d key_phase=%d frames=%s", connID, hdr.PacketNumber, size, hdr.KeyPhase, summarizeQUICFrames(frames))
					}
				},
				DroppedPacket: func(pt logging.PacketType, pn logging.PacketNumber, size logging.ByteCount, reason logging.PacketDropReason) {
					atomic.AddInt64(&droppedPackets, 1)
					if isExpectedDroppedPacket(pt, reason) {
						return
					}
					log.Printf("[debug] quic packet dropped: conn_id=%s packet_type=%s pn=%d bytes=%d reason=%s", connID, packetTypeName(pt), pn, size, packetDropReasonName(reason))
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
							diagnoseMissingRequestStream(connID, clientBidi, clientUni, serverBidi, serverUni, firstClientUniStreamID)
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
						diagnoseMissingRequestStream(connID, clientBidi, clientUni, serverBidi, serverUni, firstClientUniStreamID)
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

func summarizeQUICFrames(frames []logging.Frame) string {
	if len(frames) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(frames))
	for _, f := range frames {
		switch ff := f.(type) {
		case *logging.StreamFrame:
			parts = append(parts, fmt.Sprintf("stream(id=%d off=%d len=%d fin=%v)", ff.StreamID, ff.Offset, ff.Length, ff.Fin))
		case *logging.CryptoFrame:
			parts = append(parts, fmt.Sprintf("crypto(off=%d len=%d)", ff.Offset, ff.Length))
		case *logging.ConnectionCloseFrame:
			parts = append(parts, fmt.Sprintf("conn_close(code=%d reason=%q app=%v)", ff.ErrorCode, ff.ReasonPhrase, ff.IsApplicationError))
		case *logging.AckFrame:
			parts = append(parts, fmt.Sprintf("ack(ranges=%d delay=%s)", len(ff.AckRanges), ff.DelayTime))
		default:
			parts = append(parts, fmt.Sprintf("%T", f))
		}
	}
	return strings.Join(parts, ",")
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

func diagnoseMissingRequestStream(connID quic.ConnectionID, clientBidi, clientUni, serverBidi, serverUni, firstClientUniStreamID int64) {
	if clientBidi > 0 {
		return
	}
	log.Printf("[debug] quic conn request-stream hint: conn_id=%s no client-initiated bidi stream frames observed (expected stream id 0/4/8... for CONNECT requests)", connID)
	switch {
	case clientUni > 0 && serverBidi == 0 && serverUni == 0:
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s only client unidirectional stream traffic observed (typically control stream / SETTINGS), no request stream created", connID)
		if firstClientUniStreamID >= 0 {
			log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s first_client_uni_stream_id=%d (expected control stream id=2 on first h3 unidirectional stream)", connID, firstClientUniStreamID)
		}
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s peer likely closed before opening request stream; verify client actually sends CONNECT on client-initiated bidirectional stream", connID)
	case clientUni == 0 && serverBidi == 0 && serverUni == 0:
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s handshake completed but no HTTP/3 stream activity followed", connID)
	default:
		log.Printf("[debug] quic conn request-stream diagnosis: conn_id=%s stream activity observed without client request stream (client_uni=%d server_bidi=%d server_uni=%d)", connID, clientUni, serverBidi, serverUni)
	}
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
