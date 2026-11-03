package modelproxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/alpha-omega-security/harness/egress"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = time.Minute
)

// Server serves a Proxy to the containers of this host, which reach it at egress.HostGatewayAlias.
type Server struct {
	// Origin is the URL the containers reach the proxy at, without PathPrefix.
	Origin string
	// CA is the PEM certificate a container trusts for an HTTPS server, nil over HTTP.
	CA []byte
	// APIPort and HostPorts go in the egress sidecar configuration. The egress proxy refuses CONNECT to its
	// API port, so an HTTPS server is a separate host port.
	APIPort   string
	HostPorts []string

	server *http.Server
}

// Serve serves p on a free port of every interface, since a container reaches the host through its gateway
// address. With useTLS, it serves HTTPS under a throwaway certificate, which Codex needs to send a ChatGPT
// access token. Apple's container runtime reaches the host at a gateway IP rather than
// egress.HostGatewayAlias, so its containers cannot use Origin.
func Serve(p *Proxy, useTLS bool) (*Server, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	s := &Server{
		Origin:  "http://" + egress.HostGatewayAlias + ":" + port,
		APIPort: port,
		server:  &http.Server{Handler: p, ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout},
	}
	if useTLS {
		ca, cert, err := NewCertificate(egress.HostGatewayAlias)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		s.Origin = "https://" + egress.HostGatewayAlias + ":" + port
		s.CA, s.APIPort, s.HostPorts = ca, "", []string{port}
	}
	go func() { _ = s.server.Serve(ln) }()
	return s, nil
}

// Close stops serving.
func (s *Server) Close() error {
	return s.server.Close()
}
