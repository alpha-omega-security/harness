package egress

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEgressProxy_DeniesAPIConnect(t *testing.T) {
	for _, target := range []struct {
		name, requestTarget, host, apiPort string
		apiHosts                           []string
	}{
		{"authority", HostGatewayAlias + ":8080", "localhost:8080", "8080", nil},
		{"path", "/api/ping", HostGatewayAlias + ":8080", "8080", nil},
		{"case", "HOST.DOCKER.INTERNAL:8080", "localhost:8080", "8080", nil},
		{"custom", "192.0.2.1:8080", "localhost:8080", "8080", []string{"192.0.2.1"}},
		{"ipv6", "[::1]:8080", "localhost:8080", "8080", []string{"::1"}},
		{"default-port", HostGatewayAlias, HostGatewayAlias, "443", nil},
	} {
		t.Run(target.name, func(t *testing.T) {
			for _, auth := range []struct {
				name, header string
				status       int
			}{
				{"valid", "Basic " + base64.StdEncoding.EncodeToString([]byte("any-user:token")), http.StatusForbidden},
				{"missing", "", http.StatusProxyAuthRequired},
				{"wrong", "Basic " + base64.StdEncoding.EncodeToString([]byte("any-user:wrong")), http.StatusProxyAuthRequired},
				{"malformed", "Basic %%%", http.StatusProxyAuthRequired},
			} {
				t.Run(auth.name, func(t *testing.T) {
					p := &Proxy{Token: "token", Allow: []string{HostGatewayAlias, "192.0.2.1", "::1"},
						APIPort: target.apiPort, APIHosts: target.apiHosts, HostPorts: []string{target.apiPort}}
					raw := "CONNECT " + target.requestTarget + " HTTP/1.1\r\nHost: " + target.host +
						"\r\nProxy-Authorization: " + auth.header + "\r\n\r\n"
					r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = r.Body.Close() }()
					w := httptest.NewRecorder()
					p.ServeHTTP(w, r)
					if w.Code != auth.status {
						t.Fatalf("status = %d, want %d: %s", w.Code, auth.status, w.Body)
					}
					if auth.status == http.StatusProxyAuthRequired && w.Header().Get("Proxy-Authenticate") != `Basic realm="harness"` {
						t.Fatalf("missing proxy challenge: %v", w.Header())
					}
				})
			}
		})
	}
}

func TestAPIHostGateDeniesEquivalentPorts(t *testing.T) {
	for _, ports := range [][2]string{
		{"8080", "08080"}, {"08080", "8080"}, {"+8080", "8080"},
		{"8080", "+8080"}, {"http", "80"}, {"80", "http"},
	} {
		t.Run(ports[0]+"/"+ports[1], func(t *testing.T) {
			p := &Proxy{APIPort: ports[0], HostPorts: []string{ports[1]}, Log: quietLog()}
			w := httptest.NewRecorder()
			if p.apiHostGate(w, http.MethodConnect, HostGatewayAlias, ports[1]) || w.Code != http.StatusForbidden {
				t.Fatalf("equivalent HostPorts grant overrides API denial: %d", w.Code)
			}
		})
	}
}

func TestEgressProxy_ForwardPreservesAPIIdentity(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	authority := net.JoinHostPort(HostGatewayAlias, port)
	p := &Proxy{Token: "token", Allow: []string{HostGatewayAlias}, APIPort: port, Log: quietLog()}
	raw := "GET http://" + authority + "/api/ping HTTP/1.1\r\nHost: localhost:" + port +
		"\r\nAuthorization: Bearer api-token\r\nProxy-Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte("harness:token")) + "\r\n\r\n"
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("forward status = %d: %s", w.Code, w.Body)
	}
	got := <-seen
	if got.Host != authority || got.Header.Get("Authorization") != "Bearer api-token" || got.Header.Get("Proxy-Authorization") != "" {
		t.Fatalf("forwarded Host=%q headers=%v", got.Host, got.Header)
	}
}

func TestEgressListenersDenyAPIConnect(t *testing.T) {
	for _, starter := range []string{"Start", "Serve"} {
		t.Run(starter, func(t *testing.T) {
			p := &Proxy{Token: "token", Allow: []string{HostGatewayAlias}, APIPort: "8080"}
			var addr string
			if starter == "Start" {
				port, err := Start(p)
				if err != nil {
					t.Fatal(err)
				}
				addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			} else {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr = ln.Addr().String()
				_ = ln.Close()
				go func() { _ = Serve(p, addr) }()
			}
			// These process-lifetime starters have no close hook; their listeners
			// exit with the test binary, as in TestServeEgressProxy.
			for _, target := range []string{HostGatewayAlias + ":8080", "/api/ping"} {
				checkAPIConnectDenied(t, addr, target)
			}
		})
	}
}

func checkAPIConnectDenied(t *testing.T, addr, target string) {
	t.Helper()
	var conn net.Conn
	var err error
	deadline := time.Now().Add(time.Second)
	for {
		conn, err = net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("harness:token"))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s:8080\r\nProxy-Authorization: Basic %s\r\n\r\n", target, HostGatewayAlias, auth); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT %s status = %d, want 403", target, resp.StatusCode)
	}
}
