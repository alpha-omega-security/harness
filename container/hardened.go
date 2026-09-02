package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alpha-omega-security/harness"
	"github.com/alpha-omega-security/harness/egress"
)

const (
	hardenedNetworkPrefix = "harness-hardened-"
	proxySidecarPrefix    = "harness-proxy-"
	proxyOwnerPIDLabel    = "org.alpha-omega-security.harness.owner-pid"
	proxySidecarPort      = "3128"
	proxyReadyTimeout     = 30 * time.Second
	proxyReadyPoll        = time.Second
)

// Environment variables read by cmd/harness-proxy.
const (
	ProxyTokenEnv     = "HARNESS_PROXY_TOKEN"
	ProxyAllowEnv     = "HARNESS_PROXY_ALLOW"
	ProxyAPIHostEnv   = "HARNESS_PROXY_API_HOST"
	ProxyAPIPortEnv   = "HARNESS_PROXY_API_PORT"
	ProxyHostPortsEnv = "HARNESS_PROXY_HOST_PORTS"
	ProxyListenEnv    = "HARNESS_PROXY_LISTEN"
)

// SidecarConfig supplies the caller-specific parts of a hardened run's egress
// proxy. Runner adds egress.HardenedAllow and Harness.EgressHosts to Allow.
// Empty Token and GatewayIP values are generated and resolved per run.
type SidecarConfig struct {
	Image string // empty uses Runner.Image
	Token string // empty generates a per-run token
	Allow []string
	// APIPort is the host API port allowed through HostGatewayAlias. When set,
	// the proxy proves that API is reachable before accepting traffic.
	APIPort string
	// HostPorts adds host services to the same gateway port gate.
	HostPorts []string
	// GatewayIP is the default-network host gateway. Empty resolves it per run
	// when APIPort or HostPorts requires host access.
	GatewayIP string
}

type hardenedRun struct {
	network       string
	gatewayIP     string
	proxyEndpoint string
	proxyName     string
	token         string
}

func (r Runner) setupHardened(ctx context.Context, h harness.Harness) (hardenedRun, error) {
	if !r.Runtime.NeedsEgressSidecar() {
		if r.ProxyURL == "" {
			return hardenedRun{}, errors.New("ProxyURL is required when the runtime does not use an egress sidecar")
		}
		if _, err := proxyPortFromURL(r.ProxyURL); err != nil {
			return hardenedRun{}, fmt.Errorf("invalid ProxyURL: %w", err)
		}
	}
	key, err := hardenedKey()
	if err != nil {
		return hardenedRun{}, fmt.Errorf("generate isolation key: %w", err)
	}
	network := hardenedNetworkPrefix + key
	hn := hardenedRun{network: network}
	failed := true
	defer func() {
		if failed {
			hn.cleanup(r.Runtime, nil)
		}
	}()
	if err := ensureHardenedNetwork(ctx, r.Runtime, network); err != nil {
		return hardenedRun{}, err
	}

	image := r.image()
	hn.gatewayIP = ResolveHostGatewayIPv4(ctx, r.Runtime, image, r.Runtime.hostGatewayProbeNetwork(network))
	if r.Runtime.Bin == runtimeApple && hn.gatewayIP == "" {
		return hardenedRun{}, fmt.Errorf("could not resolve the Apple network gateway for %q", network)
	}

	if r.Runtime.NeedsEgressSidecar() {
		cfg, err := r.resolveSidecarConfig(ctx, h, image)
		if err != nil {
			return hardenedRun{}, err
		}
		hn.token = cfg.Token
		hn.proxyName = proxySidecarPrefix + key
		hn.proxyEndpoint, err = r.startProxySidecar(ctx, cfg, hn.proxyName, network)
		if err != nil {
			return hardenedRun{}, err
		}
	}

	if r.Runtime.NeedsHardenedNetVerify() {
		if err := r.verifyHardenedNetwork(ctx, hn, image); err != nil {
			return hardenedRun{}, fmt.Errorf("verify %q: %w", network, err)
		}
	}
	failed = false
	return hn, nil
}

func (r Runner) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultImage
}

func hardenedKey() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(b[:]), nil
}

func ensureHardenedNetwork(ctx context.Context, rt Runtime, name string) error {
	if out, err := exec.CommandContext(ctx, rt.bin(), runtimeCommandNetwork, "inspect", "--", name).Output(); err == nil && len(out) > 0 {
		return nil
	}
	args := hardenedNetworkCreateArgs(rt, name)
	if out, err := exec.CommandContext(ctx, rt.bin(), args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s network create --internal %s: %w: %s", rt.bin(), name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func hardenedNetworkCreateArgs(rt Runtime, name string) []string {
	args := []string{runtimeCommandNetwork, "create", "--internal"}
	if rt.needsInternalDNSDisabled() {
		// Keep the internal resolver from shadowing DNS on the sidecar's
		// default-network egress leg.
		args = append(args, "--disable-dns")
	}
	return append(args, "--", name)
}

func (r Runner) resolveSidecarConfig(ctx context.Context, h harness.Harness, runnerImage string) (SidecarConfig, error) {
	cfg := r.Sidecar
	if cfg.Image == "" {
		cfg.Image = runnerImage
	}
	if cfg.Token == "" {
		cfg.Token = egress.NewToken()
	}
	cfg.Allow = appendUniqueFold(nil, egress.HardenedAllow...)
	cfg.Allow = appendUniqueFold(cfg.Allow, h.EgressHosts()...)
	cfg.Allow = appendUniqueFold(cfg.Allow, r.Sidecar.Allow...)
	if err := validateSidecarPorts(cfg); err != nil {
		return SidecarConfig{}, err
	}
	if cfg.GatewayIP == "" && (cfg.APIPort != "" || len(cfg.HostPorts) > 0) {
		cfg.GatewayIP = ResolveHostGatewayIPv4(ctx, r.Runtime, cfg.Image, "")
	}
	if cfg.GatewayIP != "" {
		ip := net.ParseIP(cfg.GatewayIP)
		if ip == nil || ip.To4() == nil {
			return SidecarConfig{}, fmt.Errorf("invalid sidecar gateway IPv4 %q", cfg.GatewayIP)
		}
	}
	if (cfg.APIPort != "" || len(cfg.HostPorts) > 0) && cfg.GatewayIP == "" {
		return SidecarConfig{}, errors.New("could not resolve the host gateway required by the egress sidecar")
	}
	return cfg, nil
}

func appendUniqueFold(dst []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		found := false
		for _, current := range dst {
			if strings.EqualFold(current, value) {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, value)
		}
	}
	return dst
}

func validateSidecarPorts(cfg SidecarConfig) error {
	for _, port := range append([]string{cfg.APIPort}, cfg.HostPorts...) {
		if port == "" {
			continue
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid sidecar host port %q", port)
		}
	}
	return nil
}

func (r Runner) startProxySidecar(ctx context.Context, cfg SidecarConfig, name, network string) (string, error) {
	removeContainer(r.Runtime, name)
	cmd := exec.CommandContext(ctx, r.Runtime.bin(), sidecarRunArgs(cfg, name, network, os.Getpid())...)
	cmd.Env, _ = overlayProcessEnv(os.Environ(), []string{ProxyTokenEnv + "=" + cfg.Token})
	if out, err := cmd.CombinedOutput(); err != nil {
		removeContainer(r.Runtime, name)
		return "", fmt.Errorf("%s run proxy sidecar: %w: %s", r.Runtime.bin(), err, strings.TrimSpace(string(out)))
	}
	egressNetwork := r.Runtime.sidecarEgressNetwork()
	if out, err := exec.CommandContext(ctx, r.Runtime.bin(), runtimeCommandNetwork, "connect", "--", egressNetwork, name).CombinedOutput(); err != nil {
		removeContainer(r.Runtime, name)
		return "", fmt.Errorf("%s network connect %s: %w: %s", r.Runtime.bin(), egressNetwork, err, strings.TrimSpace(string(out)))
	}
	format := fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, network)
	out, err := exec.CommandContext(ctx, r.Runtime.bin(), "inspect", "--format", format, "--", name).Output()
	if err != nil {
		removeContainer(r.Runtime, name)
		return "", fmt.Errorf("%s inspect proxy sidecar: %w", r.Runtime.bin(), err)
	}
	ip := strings.TrimSpace(string(out))
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		removeContainer(r.Runtime, name)
		return "", fmt.Errorf("proxy sidecar %q has invalid network IPv4 %q", name, ip)
	}
	return net.JoinHostPort(ip, proxySidecarPort), nil
}

func sidecarRunArgs(cfg SidecarConfig, name, network string, ownerPID int) []string {
	args := []string{
		runtimeCommandRun, "-d",
		"--name", name,
		"--network", network,
		"--label", proxyOwnerPIDLabel + "=" + strconv.Itoa(ownerPID),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
	}
	if cfg.GatewayIP != "" {
		args = append(args, "--add-host", egress.HostGatewayAlias+":"+cfg.GatewayIP)
	}
	args = append(args, "-e", ProxyTokenEnv)
	for _, env := range SidecarEnv(cfg, egress.ListenFirstIface+":"+proxySidecarPort)[1:] {
		args = append(args, "-e", env)
	}
	return append(args, "--", cfg.Image, "harness-proxy")
}

// SidecarEnv returns the environment contract shared with cmd/harness-proxy.
func SidecarEnv(cfg SidecarConfig, listen string) []string {
	return []string{
		ProxyTokenEnv + "=" + cfg.Token,
		ProxyAllowEnv + "=" + strings.Join(cfg.Allow, ","),
		ProxyAPIHostEnv + "=" + cfg.GatewayIP,
		ProxyAPIPortEnv + "=" + cfg.APIPort,
		ProxyHostPortsEnv + "=" + strings.Join(cfg.HostPorts, ","),
		ProxyListenEnv + "=" + listen,
	}
}

func (r Runner) verifyHardenedNetwork(ctx context.Context, hn hardenedRun, image string) error {
	out, err := exec.CommandContext(ctx, r.Runtime.bin(), r.Runtime.hardenedEgressBlockArgs(hn.network, image)...).CombinedOutput()
	probe := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("egress-block probe could not run: %w: %s", err, probe)
	}
	if strings.Contains(probe, "NOCURL") {
		return fmt.Errorf("runner image %q lacks curl", image)
	}
	if !strings.Contains(probe, "BLOCKED") {
		return fmt.Errorf("external egress was not blocked (probe output: %q)", probe)
	}
	if hn.proxyEndpoint != "" {
		return r.verifySidecarReachable(ctx, hn, image)
	}
	return r.verifyHostProxyReachable(ctx, hn, image)
}

func (r Runner) verifyHostProxyReachable(ctx context.Context, hn hardenedRun, image string) error {
	port, err := proxyPortFromURL(r.ProxyURL)
	if err != nil {
		return fmt.Errorf("parse ProxyURL: %w", err)
	}
	gateway := "host-gateway"
	if hn.gatewayIP != "" {
		gateway = hn.gatewayIP
	}
	args := r.Runtime.hardenedProxyReachArgs(hn.network, gateway, port, image)
	out, err := exec.CommandContext(ctx, r.Runtime.bin(), args...).CombinedOutput()
	probe := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("proxy-reach probe could not run: %w: %s", err, probe)
	}
	if !strings.Contains(probe, "REACHED") {
		return fmt.Errorf("host proxy at %s:%s was unreachable (probe output: %q)", gateway, port, probe)
	}
	return nil
}

func (r Runner) verifySidecarReachable(ctx context.Context, hn hardenedRun, image string) error {
	deadline := time.NewTimer(proxyReadyTimeout)
	defer deadline.Stop()
	var last string
	for {
		out, err := exec.CommandContext(ctx, r.Runtime.bin(), sidecarReachArgs(hn.network, hn.proxyEndpoint, image)...).CombinedOutput()
		last = strings.TrimSpace(string(out))
		if err == nil && strings.Contains(last, "REACHED") {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !sidecarRunning(ctx, r.Runtime, hn.proxyName) {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("proxy sidecar %q exited before becoming reachable; logs: %s", hn.proxyName, sidecarLogTail(r.Runtime, hn.proxyName))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("proxy sidecar at %s was unreachable (probe output: %q); logs: %s", hn.proxyEndpoint, last, sidecarLogTail(r.Runtime, hn.proxyName))
		case <-time.After(proxyReadyPoll):
		}
	}
}

func (rt Runtime) hardenedEgressBlockArgs(network, image string) []string {
	const script = `command -v curl >/dev/null 2>&1 || { echo NOCURL; exit 0; }
curl -s -m 5 -o /dev/null http://1.1.1.1 && echo REACHED || echo BLOCKED`
	return rt.runArgs("--rm", "--cap-drop", "ALL", "--network", network,
		"--entrypoint", "sh", "--", image, "-c", script)
}

func (rt Runtime) hardenedProxyReachArgs(network, gateway, port, image string) []string {
	args := rt.runArgs("--rm", "--cap-drop", "ALL", "--network", network)
	var target string
	if rt.supportsHostGatewayAddHost() {
		args = append(args, "--add-host", egress.HostGatewayAlias+":"+gateway)
		target = "http://" + net.JoinHostPort(egress.HostGatewayAlias, port) + "/"
	} else {
		target = "http://" + net.JoinHostPort(gateway, port) + "/"
	}
	script := "curl -s -m 5 -o /dev/null " + target + " && echo REACHED || echo UNREACHABLE"
	return append(args, "--entrypoint", "sh", "--", image, "-c", script)
}

func sidecarReachArgs(network, endpoint, image string) []string {
	target := "http://" + endpoint + "/"
	script := "curl -s -m 5 -o /dev/null " + target + " && echo REACHED || echo UNREACHABLE"
	return []string{runtimeCommandRun, "--rm", "--cap-drop", "ALL", "--network", network,
		"--entrypoint", "sh", "--", image, "-c", script}
}

func proxyPortFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid proxy URL")
	}
	port := u.Port()
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("proxy URL must include a port between 1 and 65535")
	}
	return port, nil
}

func proxyURLWithHost(raw, host string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Host = net.JoinHostPort(host, u.Port())
	return u.String()
}

func sidecarRunning(ctx context.Context, rt Runtime, name string) bool {
	out, err := exec.CommandContext(ctx, rt.bin(), "inspect", "--format", "{{.State.Running}}", "--", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func sidecarLogTail(rt Runtime, name string) string {
	out, _ := exec.Command(rt.bin(), "logs", "--tail", "20", "--", name).CombinedOutput()
	if log := strings.TrimSpace(string(out)); log != "" {
		return log
	}
	return "(no logs)"
}

func (hn hardenedRun) cleanup(rt Runtime, emit func(harness.Event)) {
	if hn.proxyName != "" {
		if out, err := exec.Command(rt.bin(), "logs", "--", hn.proxyName).CombinedOutput(); err == nil {
			emitProxyLogLines(out, emit)
		}
		removeContainer(rt, hn.proxyName)
	}
	if hn.network != "" {
		_ = exec.Command(rt.bin(), runtimeCommandNetwork, "rm", "--", hn.network).Run()
	}
}

func removeContainer(rt Runtime, name string) {
	_ = exec.Command(rt.bin(), "rm", "-f", "--", name).Run()
}

func emitProxyLogLines(out []byte, emit func(harness.Event)) {
	if emit == nil {
		return
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		allowed := strings.Contains(line, `msg="egress allowed"`)
		if allowed || strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR") {
			emit(harness.Event{Kind: harness.KindEgress, Text: "egress-proxy: " + line})
		}
	}
}

// ResolveHostGatewayIPv4 returns the runtime's host-gateway IPv4 on network.
// An empty network probes the default network.
func ResolveHostGatewayIPv4(ctx context.Context, rt Runtime, image, network string) string {
	if rt.Bin == runtimeApple {
		return resolveAppleHostGatewayIPv4(ctx, rt, image, network)
	}
	args := rt.runArgs("--rm", "--add-host", "hgw:host-gateway")
	if network != "" {
		args = append(args, "--network", network)
	}
	args = append(args, "--entrypoint", "grep", "--", image, "hgw", "/etc/hosts")
	out, err := exec.CommandContext(ctx, rt.bin(), args...).Output()
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if ip := net.ParseIP(fields[0]); ip != nil && ip.To4() != nil {
				return fields[0]
			}
		}
	}
	return ""
}

func resolveAppleHostGatewayIPv4(ctx context.Context, rt Runtime, image, network string) string {
	if image == "" {
		return ""
	}
	const script = `awk '$2 == "00000000" { print $3; exit }' /proc/net/route`
	args := rt.runArgs("--rm")
	if network != "" {
		args = append(args, "--network", network)
	}
	args = append(args, "--entrypoint", "sh", "--", image, "-c", script)
	out, err := exec.CommandContext(ctx, rt.bin(), args...).Output()
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if ip := routeHexIPv4(strings.TrimSpace(line)); ip != "" {
			return ip
		}
	}
	return ""
}

func routeHexIPv4(field string) string {
	const ipv4HexLen = 8
	if len(field) != ipv4HexLen {
		return ""
	}
	n, err := strconv.ParseUint(field, 16, 32)
	if err != nil || n == 0 {
		return ""
	}
	return net.IPv4(byte(n), byte(n>>8), byte(n>>16), byte(n>>24)).String() //nolint:mnd
}

// VerifyProxyBinary checks that a locally cached sidecar image contains
// harness-proxy. Missing local images are left for the first run to pull.
func VerifyProxyBinary(ctx context.Context, rt Runtime, image string) error {
	if !rt.NeedsEgressSidecar() || image == "" || !imageExistsLocally(ctx, rt, image) {
		return nil
	}
	out, err := exec.CommandContext(ctx, rt.bin(), runtimeCommandRun, "--rm", "--pull", "never",
		"--", image, "harness-proxy", "-h").CombinedOutput()
	if err != nil {
		return fmt.Errorf("runner image %q is missing harness-proxy: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SweepResult reports stale hardened resources removed at startup.
type SweepResult struct {
	ProxySidecars int
	Networks      int
}

// SweepHardened removes stale sidecars first, then hardened networks whose
// owning process is no longer running.
func SweepHardened(ctx context.Context, rt Runtime) (SweepResult, error) {
	var result SweepResult
	var sidecarErr error
	if rt.NeedsEgressSidecar() {
		result.ProxySidecars, sidecarErr = sweepProxySidecars(ctx, rt)
	}
	var err error
	result.Networks, err = sweepNetworks(ctx, rt)
	return result, errors.Join(sidecarErr, err)
}

func sweepProxySidecars(ctx context.Context, rt Runtime) (int, error) {
	out, err := exec.CommandContext(ctx, rt.bin(), "ps", "-a", "--filter", "name="+proxySidecarPrefix,
		"--format", "{{.Names}}").Output()
	if err != nil {
		return 0, fmt.Errorf("%s ps: %w", rt.bin(), err)
	}
	removed := 0
	for _, name := range prefixedNames(out, proxySidecarPrefix) {
		ownerPID, ok := sidecarOwnerPID(ctx, rt, name)
		if !ok || processRunning(ownerPID) {
			continue
		}
		if exec.CommandContext(ctx, rt.bin(), "rm", "-f", "--", name).Run() == nil {
			removed++
		}
	}
	return removed, nil
}

func sidecarOwnerPID(ctx context.Context, rt Runtime, name string) (int, bool) {
	format := fmt.Sprintf(`{{index .Config.Labels %q}}`, proxyOwnerPIDLabel)
	out, err := exec.CommandContext(ctx, rt.bin(), "inspect", "--format", format, "--", name).Output()
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid, err == nil && pid > 0
}

func sweepNetworks(ctx context.Context, rt Runtime) (int, error) {
	args := []string{runtimeCommandNetwork, "ls", "--filter", "name=" + hardenedNetworkPrefix, "--format", "{{.Name}}"}
	if rt.Bin == runtimeApple {
		args = []string{runtimeCommandNetwork, "list", "--quiet"}
	}
	out, err := exec.CommandContext(ctx, rt.bin(), args...).Output()
	if err != nil {
		return 0, fmt.Errorf("%s network list: %w", rt.bin(), err)
	}
	removed := 0
	for _, name := range prefixedNames(out, hardenedNetworkPrefix) {
		ownerPID, ok := hardenedNetworkOwnerPID(name)
		if !ok || processRunning(ownerPID) {
			continue
		}
		if exec.CommandContext(ctx, rt.bin(), runtimeCommandNetwork, "rm", "--", name).Run() == nil {
			removed++
		}
	}
	return removed, nil
}

func hardenedNetworkOwnerPID(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, hardenedNetworkPrefix)
	if !ok {
		return 0, false
	}
	pidText, key, ok := strings.Cut(rest, "-")
	if !ok || len(key) != 16 {
		return 0, false
	}
	if _, err := hex.DecodeString(key); err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(pidText)
	return pid, err == nil && pid > 0
}

func prefixedNames(out []byte, prefix string) []string {
	var names []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names
}
