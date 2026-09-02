package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alpha-omega-security/harness"
	"github.com/alpha-omega-security/harness/egress"
)

type hardenedStubHarness struct{ stubHarness }

func (hardenedStubHarness) EgressHosts() []string { return []string{"api.example.test"} }

func TestRunnerRunHardenedSidecarLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	runtimePath := filepath.Join(binDir, "podman")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HARNESS_RUNTIME_LOG"
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  exit 1
fi
if [ "$1" = "run" ]; then
	if [ "$2" = "-d" ]; then
		printf 'sidecar-token=%s\n' "$HARNESS_PROXY_TOKEN" >> "$HARNESS_RUNTIME_LOG"
	fi
  case "$*" in
    *"--entrypoint grep"*) printf '%s\n' '192.0.2.1 hgw'; exit 0 ;;
    *"http://1.1.1.1"*) printf '%s\n' 'BLOCKED'; exit 0 ;;
    *"http://10.89.1.2:3128/"*) printf '%s\n' 'REACHED'; exit 0 ;;
  esac
fi
if [ "$1" = "inspect" ]; then
  printf '%s\n' '10.89.1.2'
  exit 0
fi
if [ "$1" = "logs" ]; then
  printf '%s\n' 'time=t level=INFO msg="ready"'
  printf '%s\n' 'time=t level=WARN msg="egress denied" host=blocked.test'
fi
exit 0
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HARNESS_RUNTIME_LOG", logPath)

	var events []harness.Event
	runner := Runner{
		Runtime:  Runtime{Bin: "podman", Rootless: true},
		Image:    "img:latest",
		Hardened: true,
		Sidecar: SidecarConfig{
			Token:     "tok",
			Allow:     []string{"packages.example.test"},
			APIPort:   "8080",
			GatewayIP: "192.0.2.9",
		},
	}
	err := runner.Run(t.Context(), hardenedStubHarness{}, harness.Job{Workspace: t.TempDir()}, func(event harness.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	var network string
	for line := range strings.SplitSeq(log, "\n") {
		if strings.HasPrefix(line, "network create --internal --disable-dns -- ") {
			network = strings.TrimPrefix(line, "network create --internal --disable-dns -- ")
			break
		}
	}
	if !strings.HasPrefix(network, hardenedNetworkPrefix) {
		t.Fatalf("network create missing from runtime log:\n%s", log)
	}
	for _, want := range []string{
		"run -d --name " + strings.Replace(network, hardenedNetworkPrefix, proxySidecarPrefix, 1) + " --network " + network,
		ProxyAllowEnv + "=" + egress.HostGatewayAlias + ",api.example.test,packages.example.test",
		"-- img:latest harness-proxy",
		"network connect -- podman",
		"--network " + network,
		"-e HTTPS_PROXY",
		"sidecar-token=tok",
		"--read-only",
		"stub --headless",
		"network rm -- " + network,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("runtime log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "HTTPS_PROXY=http://harness:tok") || strings.Contains(log, ProxyTokenEnv+"=tok") {
		t.Errorf("proxy token exposed in runtime args:\n%s", log)
	}
	if len(events) != 1 || events[0].Kind != harness.KindEgress || !strings.Contains(events[0].Text, "blocked.test") {
		t.Errorf("egress events = %+v", events)
	}
}

func TestRunnerRunHardenedValidation(t *testing.T) {
	job := harness.Job{Workspace: t.TempDir()}
	runtime := Runtime{Bin: "podman"}
	if err := (Runner{Runtime: runtime, Hardened: true, Network: "existing"}).Run(t.Context(), stubHarness{}, job, nil); err == nil || !strings.Contains(err.Error(), "cannot both") {
		t.Errorf("Hardened plus Network error = %v", err)
	}
	if err := (Runner{Runtime: runtime, Hardened: true}).Run(t.Context(), stubHarness{}, job, nil); err == nil || !strings.Contains(err.Error(), "ProxyURL is required") {
		t.Errorf("missing ProxyURL error = %v", err)
	}
	const proxyToken = "secret-proxy-token"
	err := (Runner{Runtime: runtime, Hardened: true, ProxyURL: "http://harness:" + proxyToken + "@proxy"}).Run(t.Context(), stubHarness{}, job, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid ProxyURL") {
		t.Errorf("invalid ProxyURL error = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), proxyToken) {
		t.Errorf("invalid ProxyURL error exposed proxy token: %v", err)
	}
}

func TestRunnerOpenPreservesCancellationDuringSidecarReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	binDir := t.TempDir()
	reachStarted := filepath.Join(t.TempDir(), "reach-started")
	runtimePath := filepath.Join(binDir, "podman")
	script := `#!/bin/sh
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  exit 1
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"--entrypoint grep"*) printf '%s\n' '192.0.2.1 hgw'; exit 0 ;;
    *"http://1.1.1.1"*) printf '%s\n' 'BLOCKED'; exit 0 ;;
    *"http://10.89.1.2:3128/"*) : > "$HARNESS_REACH_STARTED"; while :; do :; done ;;
  esac
fi
if [ "$1" = "inspect" ]; then
  printf '%s\n' '10.89.1.2'
fi
exit 0
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HARNESS_REACH_STARTED", reachStarted)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		scope, err := (Runner{
			Runtime:  Runtime{Bin: "podman", Rootless: true},
			Image:    "img:latest",
			Hardened: true,
			Sidecar:  SidecarConfig{Token: "tok"},
		}).Open(ctx, hardenedStubHarness{})
		if scope != nil {
			scope.Close(nil)
		}
		errCh <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(reachStarted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sidecar reachability probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Errorf("Open error = %v, want context.Canceled", err)
	}
}

func TestResolveSidecarConfigAddsHarnessHosts(t *testing.T) {
	runner := Runner{
		Runtime: Runtime{Bin: "podman", Rootless: true},
		Sidecar: SidecarConfig{
			Token:     "tok",
			Allow:     []string{"API.EXAMPLE.TEST", "packages.example.test"},
			APIPort:   "8080",
			GatewayIP: "192.0.2.9",
		},
	}
	got, err := runner.resolveSidecarConfig(context.Background(), hardenedStubHarness{}, "runner:latest")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{egress.HostGatewayAlias, "api.example.test", "packages.example.test"}
	if !slices.Equal(got.Allow, want) {
		t.Errorf("Allow = %v, want %v", got.Allow, want)
	}
	if got.Image != "runner:latest" {
		t.Errorf("Image = %q", got.Image)
	}
}

func TestSidecarRunArgs(t *testing.T) {
	cfg := SidecarConfig{
		Image:     "proxy:latest",
		Token:     "tok",
		Allow:     []string{"api.example.test"},
		APIPort:   "8080",
		HostPorts: []string{"11434"},
		GatewayIP: "192.0.2.9",
	}
	args := sidecarRunArgs(cfg, "harness-proxy-a", "harness-hardened-a", 1234)
	for _, pair := range [][2]string{
		{"--name", "harness-proxy-a"},
		{"--network", "harness-hardened-a"},
		{"--label", proxyOwnerPIDLabel + "=1234"},
		{"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"},
		{"--add-host", egress.HostGatewayAlias + ":192.0.2.9"},
		{"-e", ProxyListenEnv + "=" + egress.ListenFirstIface + ":3128"},
		{"-e", ProxyHostPortsEnv + "=11434"},
		{"-e", ProxyTokenEnv},
	} {
		if !hasAdjacent(args, pair[0], pair[1]) {
			t.Errorf("missing %q %q in %v", pair[0], pair[1], args)
		}
	}
	if !slices.Contains(args, "--read-only") {
		t.Errorf("missing --read-only in %v", args)
	}
	if tail := args[len(args)-3:]; !slices.Equal(tail, []string{"--", "proxy:latest", "harness-proxy"}) {
		t.Errorf("sidecar tail = %v", tail)
	}
	if strings.Contains(strings.Join(args, " "), ProxyTokenEnv+"=tok") {
		t.Errorf("sidecar args expose proxy token: %v", args)
	}
}

func TestSetupHardenedCleansNetworkAfterCreateFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "runtime.log")
	runtimePath := filepath.Join(dir, "runtime")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HARNESS_RUNTIME_LOG"
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then exit 1; fi
if [ "$1" = "network" ] && [ "$2" = "create" ]; then exit 7; fi
exit 0
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_RUNTIME_LOG", logPath)
	runner := Runner{Runtime: Runtime{Bin: runtimePath}, Hardened: true, ProxyURL: "http://proxy.test:3128"}
	if _, err := runner.Open(t.Context(), stubHarness{}); err == nil {
		t.Fatal("Open error = nil")
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if log := string(logBytes); !strings.Contains(log, "network rm -- "+hardenedNetworkPrefix) {
		t.Errorf("network cleanup missing after create failure:\n%s", log)
	}
}

func TestHardenedProbeArgs(t *testing.T) {
	rt := Runtime{Bin: "podman", Rootless: true}
	block := rt.hardenedEgressBlockArgs("harness-hardened-a", "img:latest")
	if !hasAdjacent(block, "--network", "harness-hardened-a") || !strings.Contains(strings.Join(block, " "), "1.1.1.1") {
		t.Errorf("block args = %v", block)
	}
	if strings.Contains(strings.Join(block, " "), "HTTPS_PROXY") {
		t.Errorf("block probe contains proxy env: %v", block)
	}
	reach := sidecarReachArgs("harness-hardened-a", "10.89.1.2:3128", "img:latest")
	if !hasAdjacent(reach, "--network", "harness-hardened-a") || !strings.Contains(strings.Join(reach, " "), "http://10.89.1.2:3128/") {
		t.Errorf("sidecar reach args = %v", reach)
	}
}

func TestHardenedNetworkCreateArgs(t *testing.T) {
	docker := hardenedNetworkCreateArgs(Runtime{Bin: "docker"}, "harness-hardened-a")
	if slices.Contains(docker, "--disable-dns") {
		t.Errorf("docker args contain unsupported --disable-dns: %v", docker)
	}
	podman := hardenedNetworkCreateArgs(Runtime{Bin: "podman", Rootless: true}, "harness-hardened-a")
	if !slices.Contains(podman, "--disable-dns") {
		t.Errorf("rootless podman args missing --disable-dns: %v", podman)
	}
	for _, args := range [][]string{docker, podman} {
		if tail := args[len(args)-2:]; !slices.Equal(tail, []string{"--", "harness-hardened-a"}) {
			t.Errorf("network name is not protected by --: %v", args)
		}
	}
}

func TestEmitProxyLogLines(t *testing.T) {
	var events []harness.Event
	emitProxyLogLines([]byte("time=t level=INFO msg=ready\ntime=t level=INFO msg=\"egress allowed\" host=api.test\ntime=t level=WARN msg=denied\ntime=t level=ERROR msg=failed\n"), func(event harness.Event) {
		events = append(events, event)
	})
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	for _, event := range events {
		if event.Kind != harness.KindEgress || !strings.HasPrefix(event.Text, "egress-proxy: ") {
			t.Errorf("event = %+v", event)
		}
	}
}

func TestHardenedHelpers(t *testing.T) {
	if got := routeHexIPv4("0102A8C0"); got != "192.168.2.1" {
		t.Errorf("routeHexIPv4 = %q", got)
	}
	if got := proxyURLWithHost("http://harness:tok@host.docker.internal:3128", "192.0.2.1"); got != "http://harness:tok@192.0.2.1:3128" {
		t.Errorf("proxyURLWithHost = %q", got)
	}
	if port, err := proxyPortFromURL("http://harness:tok@host:3128"); err != nil || port != "3128" {
		t.Errorf("proxyPortFromURL = %q, %v", port, err)
	}
	for _, raw := range []string{"http://host", "http://host:70000", "http://host:bad"} {
		if _, err := proxyPortFromURL(raw); err == nil {
			t.Errorf("proxyPortFromURL(%q) returned nil error", raw)
		}
	}
	got := prefixedNames([]byte("harness-hardened-a\nmy-harness-hardened-b\nharness-hardened-c\n"), hardenedNetworkPrefix)
	if !slices.Equal(got, []string{"harness-hardened-a", "harness-hardened-c"}) {
		t.Errorf("prefixedNames = %v", got)
	}
	key, err := hardenedKey()
	if err != nil {
		t.Fatal(err)
	}
	if pid, ok := hardenedNetworkOwnerPID(hardenedNetworkPrefix + key); !ok || pid != os.Getpid() {
		t.Errorf("hardened network owner = %d, %v", pid, ok)
	}
	for _, name := range []string{
		"harness-hardened-a",
		"harness-hardened-1234-short",
		"harness-hardened-1234-zzzzzzzzzzzzzzzz",
	} {
		if pid, ok := hardenedNetworkOwnerPID(name); ok {
			t.Errorf("hardenedNetworkOwnerPID(%q) = %d, true", name, pid)
		}
	}
}

func TestSweepHardened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HARNESS_RUNTIME_LOG"
if [ "$1" = "ps" ]; then
	printf '%s\n' 'harness-proxy-a' 'my-harness-proxy-b' 'harness-proxy-c' 'harness-proxy-unknown'
fi
if [ "$1" = "inspect" ]; then
	case "$*" in
		*"harness-proxy-a"*) printf '%s\n' "$PPID" ;;
		*"harness-proxy-c"*) printf '%s\n' '999999' ;;
	esac
fi
if [ "$1" = "network" ] && [ "$2" = "ls" ]; then
  printf 'harness-hardened-%s-0123456789abcdef\n' "$PPID"
  printf '%s\n' 'my-harness-hardened-b' 'harness-hardened-99999999-fedcba9876543210' 'harness-hardened-legacy'
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HARNESS_RUNTIME_LOG", logPath)

	result, err := SweepHardened(t.Context(), Runtime{Bin: "podman", Rootless: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProxySidecars != 1 || result.Networks != 1 {
		t.Errorf("result = %+v", result)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	for _, want := range []string{
		"rm -f -- harness-proxy-c",
		"network rm -- harness-hardened-99999999-fedcba9876543210",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("runtime log missing %q: %s", want, log)
		}
	}
	if strings.Contains(log, "rm -f -- harness-proxy-a") {
		t.Errorf("sweep removed a live process's sidecar: %s", log)
	}
	if strings.Contains(log, "rm -f -- harness-proxy-unknown") {
		t.Errorf("sweep removed a sidecar without an owner label: %s", log)
	}
	if strings.Contains(log, "network rm -- harness-hardened-legacy") || strings.Contains(log, "network rm -- harness-hardened-"+strconv.Itoa(os.Getpid())+"-") {
		t.Errorf("sweep removed a network without a dead owner: %s", log)
	}
	if strings.Contains(log, "rm -f -- my-") || strings.Contains(log, "network rm -- my-") {
		t.Errorf("sweep removed a substring-only match: %s", log)
	}
}

func TestSweepHardenedKeepsActiveScopeNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "runtime.log")
	namePath := filepath.Join(dir, "network-name")
	runtimePath := filepath.Join(dir, "runtime")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HARNESS_RUNTIME_LOG"
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then
  exit 1
fi
if [ "$1" = "network" ] && [ "$2" = "create" ]; then
  for arg do name="$arg"; done
  printf '%s\n' "$name" > "$HARNESS_NETWORK_NAME"
fi
if [ "$1" = "network" ] && [ "$2" = "ls" ]; then
  cat "$HARNESS_NETWORK_NAME"
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"--entrypoint grep"*) printf '%s\n' '192.0.2.1 hgw' ;;
    *) printf '%s\n' 'ready' ;;
  esac
fi
exit 0
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_RUNTIME_LOG", logPath)
	t.Setenv("HARNESS_NETWORK_NAME", namePath)

	runner := Runner{
		Runtime:  Runtime{Bin: runtimePath},
		Image:    "img:latest",
		Hardened: true,
		ProxyURL: "http://proxy.test:3128",
	}
	scope, err := runner.Open(t.Context(), stubHarness{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scope.Close(nil) })
	result, err := SweepHardened(t.Context(), runner.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if result.Networks != 0 {
		t.Fatalf("sweep result = %+v", result)
	}
	if _, err := scope.RunCommand(t.Context(), harness.Job{Workspace: t.TempDir()}, Command{Args: []string{"true"}}); err != nil {
		t.Fatalf("RunCommand after sweep: %v", err)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logBytes), "network rm --") {
		t.Errorf("sweep removed the active scope network:\n%s", logBytes)
	}
}
