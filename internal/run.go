package app

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/proxy"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
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

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Debug {
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

	quicCfg := defaultQUICConfig()
	server := http3.Server{
		Addr:       cfg.ListenAddr,
		Handler:    mux,
		TLSConfig:  config.DefaultTLSConfig(),
		QUICConfig: quicCfg,
	}

	if cfg.Debug {
		log.Printf("[debug] quic config: max_idle=%s keepalive=%s incoming_streams=%d incoming_uni_streams=%d stream_recv_window=%d conn_recv_window=%d", quicCfg.MaxIdleTimeout, quicCfg.KeepAlivePeriod, quicCfg.MaxIncomingStreams, quicCfg.MaxIncomingUniStreams, quicCfg.MaxStreamReceiveWindow, quicCfg.MaxConnectionReceiveWindow)
	}

	log.Printf("HTTP/3 WS proxy listening on udp %s, path=%s, backend=%s, debug=%v", cfg.ListenAddr, cfg.PathPattern, backendURL.String(), cfg.Debug)
	if err := server.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil {
		return fmt.Errorf("ListenAndServeTLS: %w", err)
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

func defaultQUICConfig() *quic.Config {
	return &quic.Config{
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
}
