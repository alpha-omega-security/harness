package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/alpha-omega-security/harness/container"
	"github.com/alpha-omega-security/harness/egress"
)

const (
	readinessTimeout = 20 * time.Second
	readinessPoll    = 250 * time.Millisecond
)

type proxyConfig struct {
	listen    string
	token     string
	apiHost   string
	apiPort   string
	hostPorts []string
	allow     []string
}

func main() {
	if err := runProxy(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseProxyConfig(args []string, getenv func(string) string) (proxyConfig, error) {
	fset := flag.NewFlagSet("harness-proxy", flag.ContinueOnError)
	var listen, token, apiHost, apiPort, hostPorts, allow string
	fset.StringVar(&listen, "listen", envOr(getenv, container.ProxyListenEnv, ":3128"), "listen address")
	fset.StringVar(&token, "token", getenv(container.ProxyTokenEnv), "Proxy-Authorization token")
	fset.StringVar(&apiHost, "api-host", getenv(container.ProxyAPIHostEnv), "host-gateway IPv4")
	fset.StringVar(&apiPort, "api-port", getenv(container.ProxyAPIPortEnv), "host API port")
	fset.StringVar(&hostPorts, "host-ports", getenv(container.ProxyHostPortsEnv), "comma-separated host ports")
	fset.StringVar(&allow, "allow", getenv(container.ProxyAllowEnv), "comma-separated host allowlist")
	if err := fset.Parse(args); err != nil {
		return proxyConfig{}, err
	}
	cfg := proxyConfig{
		listen:    listen,
		token:     token,
		apiHost:   apiHost,
		apiPort:   apiPort,
		hostPorts: splitCSV(hostPorts),
		allow:     splitCSV(allow),
	}
	if cfg.token == "" {
		return proxyConfig{}, errors.New("proxy: empty token")
	}
	if len(cfg.allow) == 0 {
		return proxyConfig{}, errors.New("proxy: empty allowlist")
	}
	if cfg.apiPort != "" && cfg.apiHost == "" {
		return proxyConfig{}, errors.New("proxy: api-port requires api-host")
	}
	return cfg, nil
}

func splitCSV(value string) []string {
	var values []string
	for part := range strings.SplitSeq(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

func envOr(getenv func(string) string, key, fallback string) string {
	if value := getenv(key); value != "" {
		return value
	}
	return fallback
}

func resolveListen(listen string, firstIfaceIPv4 func() (string, error)) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host != egress.ListenFirstIface {
		return listen, nil
	}
	ip, err := firstIfaceIPv4()
	if err != nil {
		return "", fmt.Errorf("resolve %s listen address: %w", egress.ListenFirstIface, err)
	}
	return net.JoinHostPort(ip, port), nil
}

func runProxy(args []string) error {
	cfg, err := parseProxyConfig(args, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.listen, err = resolveListen(cfg.listen, egress.FirstIfaceIPv4); err != nil {
		return fmt.Errorf("egress proxy refusing to start: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), readinessTimeout)
	defer cancel()
	if cfg.apiHost != "" && cfg.apiPort != "" {
		if err := egress.WaitHostAPIReachable(ctx, cfg.apiHost, cfg.apiPort); err != nil {
			return fmt.Errorf("egress proxy refusing to start: %w", err)
		}
	}
	if err := waitUpstreamDNS(ctx, cfg.allow, egress.VerifyUpstreamDNS, readinessPoll); err != nil {
		return fmt.Errorf("egress proxy refusing to start: %w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	proxy := &egress.Proxy{
		Allow:           cfg.allow,
		Token:           cfg.token,
		APIPort:         cfg.apiPort,
		HostPorts:       cfg.hostPorts,
		GatewayDialHost: cfg.apiHost,
		Log:             log,
	}
	log.Info("egress proxy listening", "addr", cfg.listen, "allow", len(cfg.allow))
	return egress.Serve(proxy, cfg.listen)
}

func waitUpstreamDNS(ctx context.Context, allow []string, verify func(context.Context, []string) error, poll time.Duration) error {
	var lastErr error
	for {
		if err := verify(ctx, allow); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %v", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}
