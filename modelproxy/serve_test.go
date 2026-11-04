package modelproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/alpha-omega-security/harness/egress"
)

func serve(t *testing.T, useTLS bool) *Server {
	t.Helper()
	p, err := New(OpenAI, "real-key", "allowed", Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Serve(p, useTLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// client reaches s on the loopback address while naming egress.HostGatewayAlias, as a container would.
func client(s *Server) (*http.Client, string) {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(s.Origin, "https://"), "http://"))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
	}}
	if s.CA != nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(s.CA)
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport}, port
}

func TestServeOverHTTP(t *testing.T) {
	s := serve(t, false)
	c, port := client(s)
	if s.Origin != "http://"+egress.HostGatewayAlias+":"+port || s.APIPort != port || s.HostPorts != nil || s.CA != nil {
		t.Fatalf("server = %+v", s)
	}
	resp, err := c.Post(s.Origin+PathPrefix+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a request without a token got %d", resp.StatusCode)
	}
}

func TestServeOverHTTPS(t *testing.T) {
	s := serve(t, true)
	c, port := client(s)
	if s.Origin != "https://"+egress.HostGatewayAlias+":"+port || s.APIPort != "" || len(s.HostPorts) != 1 || s.HostPorts[0] != port || s.CA == nil {
		t.Fatalf("server = %+v", s)
	}
	resp, err := c.Post(s.Origin+PathPrefix+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a request without a token got %d", resp.StatusCode)
	}

	plain, _ := client(&Server{Origin: s.Origin})
	if resp, err := plain.Get(s.Origin + PathPrefix); err == nil {
		_ = resp.Body.Close()
		t.Error("a client that does not trust the CA reached the server")
	}
}

func TestServeStopsOnClose(t *testing.T) {
	s := serve(t, false)
	c, _ := client(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get(s.Origin + PathPrefix); err == nil {
		_ = resp.Body.Close()
		t.Error("a closed server answered")
	}
}
