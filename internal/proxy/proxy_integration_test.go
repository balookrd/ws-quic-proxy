package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"

	"h3ws2h1ws-proxy/internal/config"
	"h3ws2h1ws-proxy/internal/ws"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func TestRealTrafficClientQUICBackendRoundTrip(t *testing.T) {
	backendURL, closeBackend := startEchoBackend(t)
	defer closeBackend()

	backendParsed, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	proxy := &Proxy{
		Backend:    backendParsed,
		PathRegexp: regexp.MustCompile(`^/ws$`),
		Debug:      true,
		Limits: config.Limits{
			MaxFrameSize:   1 << 20,
			MaxMessageSize: 1 << 20,
			MaxConns:       100,
			WriteTimeout:   5 * time.Second,
		},
	}

	tlsCert := mustMakeTLSCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer pc.Close()

	h3Server := &http3.Server{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			NextProtos:   []string{http3.NextProtoH3},
		},
		Handler: http.HandlerFunc(proxy.HandleH3WebSocket),
	}
	defer h3Server.Close()

	go func() { _ = h3Server.Serve(pc) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, pc.LocalAddr().String(), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{http3.NextProtoH3},
	}, nil)
	if err != nil {
		t.Fatalf("dial quic: %v", err)
	}
	defer func() { _ = conn.CloseWithError(0, "") }()

	rt := &http3.SingleDestinationRoundTripper{Connection: conn}

	stream, err := rt.OpenRequestStream(ctx)
	if err != nil {
		t.Fatalf("open h3 request stream: %v", err)
	}
	defer stream.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+pc.LocalAddr().String()+"/ws", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Proto = "websocket"
	req.ProtoMajor = 3
	req.ProtoMinor = 0
	req.Header.Set("protocol", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")

	if err := stream.SendRequestHeader(req); err != nil {
		t.Fatalf("send request headers: %v", err)
	}
	resp, err := stream.ReadResponse()
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected CONNECT status: got %d", resp.StatusCode)
	}

	payload := []byte("real-traffic-client-quic-backend-roundtrip")
	if err := ws.WriteDataFrame(stream, ws.OpBinary, payload, true, 1<<20); err != nil {
		t.Fatalf("write client->proxy frame: %v", err)
	}

	frame, err := ws.ReadFrame(bufio.NewReader(stream), 1<<20)
	if err != nil {
		t.Fatalf("read proxy->client frame: %v", err)
	}
	if frame.Opcode != ws.OpBinary {
		t.Fatalf("unexpected opcode: got %d want %d", frame.Opcode, ws.OpBinary)
	}
	if string(frame.Payload) != string(payload) {
		t.Fatalf("payload mismatch: got %q want %q", string(frame.Payload), string(payload))
	}
}

func mustMakeTLSCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse keypair: %v", err)
	}
	return cert
}
