package app

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/proxy"
)

func TestNewProxyHandlerHealthEndpoints(t *testing.T) {
	t.Parallel()

	cfg := config.Config{PathRegexp: regexp.MustCompile(`^/ws$`)}
	h := newProxyHandler(cfg, &proxy.Proxy{}, nil)

	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		status  int
		body    string
		assert  func(t *testing.T, rr *httptest.ResponseRecorder)
	}{
		{name: "root", method: http.MethodGet, path: "/", status: http.StatusOK, body: "ok\n"},
		{name: "health tcp get", method: http.MethodGet, path: "/health/tcp", status: http.StatusOK, body: "ok\n"},
		{name: "health udp get", method: http.MethodGet, path: "/health/udp", status: http.StatusOK, body: "ok\n"},
		{name: "health tcp connect", method: http.MethodConnect, path: "/health/tcp", status: http.StatusOK, body: ""},
		{name: "not found", method: http.MethodGet, path: "/health", status: http.StatusNotFound, body: "404 page not found\n"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()

			h.ServeHTTP(rr, req)

			if rr.Code != tc.status {
				t.Fatalf("status: got %d, want %d", rr.Code, tc.status)
			}
			if rr.Body.String() != tc.body {
				t.Fatalf("body: got %q, want %q", rr.Body.String(), tc.body)
			}
			if tc.assert != nil {
				tc.assert(t, rr)
			}
		})
	}
}

func TestShouldSuppressQuicDebugRecord(t *testing.T) {
	t.Parallel()

	mkRecord := func(level slog.Level, msg, errText string) slog.Record {
		r := slog.NewRecord(time.Now(), level, msg, 0)
		if errText != "" {
			r.AddAttrs(slog.String("error", errText))
		}
		return r
	}

	if !shouldSuppressQuicDebugRecord(mkRecord(slog.LevelDebug, "accepting unidirectional stream failed", "NO_ERROR (remote)")) {
		t.Fatal("expected suppress for accepting unidirectional stream failed + NO_ERROR")
	}
	if !shouldSuppressQuicDebugRecord(mkRecord(slog.LevelDebug, "handling connection failed", "accepting stream failed: NO_ERROR (remote)")) {
		t.Fatal("expected suppress for handling connection failed + NO_ERROR")
	}
	if shouldSuppressQuicDebugRecord(mkRecord(slog.LevelInfo, "handling connection failed", "accepting stream failed: NO_ERROR (remote)")) {
		t.Fatal("did not expect suppress for non-debug level")
	}
	if shouldSuppressQuicDebugRecord(mkRecord(slog.LevelDebug, "handling connection failed", "context deadline exceeded")) {
		t.Fatal("did not expect suppress for real error")
	}
}

func TestRequestPath(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodConnect, "https://senko.beerloga.su/health/udp", nil)
	if got, want := requestPath(req), "/health/udp"; got != want {
		t.Fatalf("requestPath absolute: got %q, want %q", got, want)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/health/tcp", nil)
	if got, want := requestPath(req2), "/health/tcp"; got != want {
		t.Fatalf("requestPath origin: got %q, want %q", got, want)
	}
}
