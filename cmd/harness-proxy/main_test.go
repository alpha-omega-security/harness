package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alpha-omega-security/harness/container"
	"github.com/alpha-omega-security/harness/egress"
)

func TestParseProxyConfigFromSidecarEnv(t *testing.T) {
	env := map[string]string{}
	cfg := container.SidecarConfig{
		Token:     "tok",
		Allow:     []string{"api.example.test", "*.example.test"},
		APIPort:   "8080",
		HostPorts: []string{"11434", "1234"},
		GatewayIP: "192.0.2.9",
	}
	for _, assignment := range container.SidecarEnv(cfg, egress.ListenFirstIface+":3128") {
		key, value, _ := strings.Cut(assignment, "=")
		env[key] = value
	}
	got, err := parseProxyConfig(nil, func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if got.token != cfg.Token || got.apiHost != cfg.GatewayIP || got.apiPort != cfg.APIPort {
		t.Errorf("config = %+v", got)
	}
	if !slices.Equal(got.allow, cfg.Allow) || !slices.Equal(got.hostPorts, cfg.HostPorts) {
		t.Errorf("config = %+v", got)
	}
	if got.listen != egress.ListenFirstIface+":3128" {
		t.Errorf("listen = %q", got.listen)
	}
}

func TestParseProxyConfigValidation(t *testing.T) {
	getenv := func(string) string { return "" }
	if _, err := parseProxyConfig(nil, getenv); err == nil {
		t.Fatal("empty config returned nil error")
	}
	if _, err := parseProxyConfig([]string{"-token", "tok", "-allow", "api.example.test", "-api-port", "8080"}, getenv); err == nil {
		t.Fatal("api-port without api-host returned nil error")
	}
}

func TestResolveListen(t *testing.T) {
	got, err := resolveListen(egress.ListenFirstIface+":3128", func() (string, error) { return "10.89.1.2", nil })
	if err != nil || got != "10.89.1.2:3128" {
		t.Errorf("resolveListen = %q, %v", got, err)
	}
	if _, err := resolveListen(egress.ListenFirstIface+":3128", func() (string, error) { return "", errors.New("no interface") }); err == nil {
		t.Fatal("resolution failure returned nil error")
	}
	got, err = resolveListen("127.0.0.1:3128", func() (string, error) { return "", errors.New("must not run") })
	if err != nil || got != "127.0.0.1:3128" {
		t.Errorf("explicit listen = %q, %v", got, err)
	}
}

func TestWaitUpstreamDNSRetries(t *testing.T) {
	attempts := 0
	err := waitUpstreamDNS(t.Context(), []string{"api.example.test"}, func(context.Context, []string) error {
		attempts++
		if attempts == 1 {
			return errors.New("egress network is not attached")
		}
		return nil
	}, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestWaitUpstreamDNSPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	err := waitUpstreamDNS(ctx, []string{"api.example.test"}, func(context.Context, []string) error {
		cancel()
		return errors.New("resolver unavailable")
	}, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
