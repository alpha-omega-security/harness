package container

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/alpha-omega-security/harness"
	"github.com/alpha-omega-security/harness/egress"
)

// stubHarness is a minimal Harness whose Env and StateEnv return predictable
// values so args() can be asserted on without depending on any real backend.
type stubHarness struct{}

func (stubHarness) Binary() string                             { return "stub" }
func (stubHarness) Args(harness.Job) []string                  { return []string{"--headless"} }
func (stubHarness) Prompt(harness.Job) string                  { return "" }
func (stubHarness) ParseStream(io.Reader, func(harness.Event)) {}
func (stubHarness) SkillDir(string, string) string             { return "" }
func (stubHarness) GuideFilename() string                      { return "AGENTS.md" }
func (stubHarness) SystemPromptViaArgs() bool                  { return true }
func (stubHarness) EgressHosts() []string                      { return nil }
func (stubHarness) Env(baseURL string) []string {
	if baseURL != "" {
		return []string{"STUB_TELEMETRY=off", "STUB_BASE_URL=" + baseURL}
	}
	return []string{"STUB_TELEMETRY=off"}
}
func (stubHarness) StateEnv(dir string) []string          { return []string{"STUB_STATE=" + dir} }
func (stubHarness) AccountErrorText(string) string        { return "" }
func (stubHarness) DefaultModels() []harness.ModelDefault { return nil }

var _ harness.Harness = stubHarness{}

// hasAdjacent reports whether args contains flag immediately followed by val,
// matching how a container `run` takes `-v host:container` / `-e KEY=VAL`
// pairs.
func hasAdjacent(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func TestRunnerArgs_Baseline(t *testing.T) {
	// The baseline flags are present in every mode across every runtime: capped
	// caps, HOME tmpfs, workspace bind mount, working dir, the
	// backend's telemetry suppressor, and the -- image terminator.
	for _, r := range []Runner{
		{},
		{Runtime: Runtime{Bin: "podman", Rootless: true}},
		{Runtime: Runtime{Bin: "apple"}},
		{ReadOnly: true},
		{ProxyURL: "http://p:8080"},
		{Network: "hardened-1", ProxyURL: "http://p:8080", ReadOnly: true},
	} {
		got := r.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
		for _, pair := range [][2]string{
			{"--cap-drop", "ALL"},
			{"-e", "HOME=/tmp"},
			{"--tmpfs", tmpfsSpec},
			{"-v", "/abs/work:" + WorkMount},
			{"-w", WorkMount},
			{"-e", "STUB_TELEMETRY=off"},
		} {
			if !hasAdjacent(got, pair[0], pair[1]) {
				t.Errorf("%+v: missing %s %s in %v", r, pair[0], pair[1], got)
			}
		}
		if got[0] != "run" || !slices.Contains(got, "--rm") {
			t.Errorf("%+v: not a run --rm invocation: %v", r, got)
		}
		if got[len(got)-2] != "--" {
			t.Errorf("%+v: missing -- terminator before image: %v", r, got)
		}
	}
}

func TestTmpfsSpecAllowsToolchainExecutables(t *testing.T) {
	opts := strings.Split(tmpfsSpec, ",")
	if strings.Contains(tmpfsSpec, "noexec") || !slices.Contains(opts, "exec") {
		t.Errorf("tmpfsSpec %q prevents build tools from running temporary binaries", tmpfsSpec)
	}
}

func TestRunnerArgs_Image(t *testing.T) {
	def := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if def[len(def)-1] != DefaultImage {
		t.Errorf("empty Image: got %q, want DefaultImage", def[len(def)-1])
	}
	custom := Runner{Image: "example.com/runner:v1"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if custom[len(custom)-1] != "example.com/runner:v1" {
		t.Errorf("custom Image not passed: %v", custom)
	}
}

func TestRunnerArgs_RuntimeVariants(t *testing.T) {
	// docker and podman get --add-host, apple does not; rootless podman alone
	// gets --userns=keep-id; apple alone gets --progress none.
	tests := []struct {
		rt          Runtime
		addHost     bool
		keepID      bool
		hasProgress bool
	}{
		{Runtime{}, true, false, false},
		{Runtime{Bin: "podman"}, true, false, false},
		{Runtime{Bin: "podman", Rootless: true}, true, true, false},
		{Runtime{Bin: "apple"}, false, false, true},
	}
	for _, tc := range tests {
		args := Runner{Runtime: tc.rt}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
		if got := hasAdjacent(args, "--add-host", egress.HostGatewayAlias+":host-gateway"); got != tc.addHost {
			t.Errorf("%+v: --add-host presence = %v, want %v: %v", tc.rt, got, tc.addHost, args)
		}
		if got := slices.Contains(args, "--userns=keep-id"); got != tc.keepID {
			t.Errorf("%+v: --userns=keep-id presence = %v, want %v: %v", tc.rt, got, tc.keepID, args)
		}
		if got := hasAdjacent(args, "--progress", "none"); got != tc.hasProgress {
			t.Errorf("%+v: --progress none presence = %v, want %v: %v", tc.rt, got, tc.hasProgress, args)
		}
	}
}

func TestRunnerArgs_HostGatewayIP(t *testing.T) {
	got := Runner{HostGatewayIP: "10.0.2.2"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(got, "--add-host", egress.HostGatewayAlias+":10.0.2.2") {
		t.Errorf("expected --add-host with resolved gateway IP, got %v", got)
	}
}

func TestRunnerArgs_ReadOnly(t *testing.T) {
	on := Runner{ReadOnly: true}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !slices.Contains(on, "--read-only") || !hasAdjacent(on, "--security-opt", "no-new-privileges") {
		t.Errorf("ReadOnly: expected --read-only + no-new-privileges in %v", on)
	}
	// Apple lacks --security-opt no-new-privileges, so ReadOnly there is
	// --read-only only.
	apple := Runner{Runtime: Runtime{Bin: "apple"}, ReadOnly: true}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !slices.Contains(apple, "--read-only") {
		t.Errorf("apple ReadOnly: expected --read-only in %v", apple)
	}
	if hasAdjacent(apple, "--security-opt", "no-new-privileges") {
		t.Errorf("apple ReadOnly: must not set --security-opt no-new-privileges: %v", apple)
	}
	off := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if slices.Contains(off, "--read-only") || hasAdjacent(off, "--security-opt", "no-new-privileges") {
		t.Errorf("default: must not set --read-only / no-new-privileges: %v", off)
	}
}

func TestRunnerArgs_StateDir(t *testing.T) {
	got := Runner{StateDir: "/data/state"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(got, "-v", "/data/state:"+StateMount) {
		t.Errorf("expected state mount at %s: %v", StateMount, got)
	}
	if !hasAdjacent(got, "-e", "STUB_STATE="+StateMount) {
		t.Errorf("expected StateEnv pointing at %s: %v", StateMount, got)
	}
	none := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if slices.ContainsFunc(none, func(a string) bool { return strings.HasPrefix(a, "STUB_STATE=") }) {
		t.Errorf("no StateDir: must not set StateEnv: %v", none)
	}
}

func TestRunnerArgs_ExtraMountsAndEnv(t *testing.T) {
	r := Runner{
		Mounts: []Mount{
			{Host: "/host/src", Container: "/work/target", ReadOnly: true},
			{Host: "/host/cache", Container: "/cache"},
		},
		Env: []string{"EXTRA=1"},
	}
	got := r.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(got, "-v", "/host/src:/work/target:ro") {
		t.Errorf("expected read-only extra mount: %v", got)
	}
	if !hasAdjacent(got, "-v", "/host/cache:/cache") {
		t.Errorf("expected read-write extra mount: %v", got)
	}
	if !hasAdjacent(got, "-e", "EXTRA=1") {
		t.Errorf("expected extra env: %v", got)
	}
}

func TestRunnerArgs_EnvPreservesBareKeys(t *testing.T) {
	t.Setenv("STUB_TOKEN", "sk-test")
	got := Runner{Env: []string{"STUB_TOKEN"}}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(got, "-e", "STUB_TOKEN") {
		t.Errorf("bare env key not preserved for runtime passthrough: %v", got)
	}
	if hasAdjacent(got, "-e", "STUB_TOKEN=sk-test") {
		t.Errorf("secret value exposed in runtime argv: %v", got)
	}
}

func TestRunnerArgs_Network(t *testing.T) {
	// No proxy, no network -> --network none (fail closed).
	closed := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(closed, "--network", "none") {
		t.Errorf("no proxy, no network: expected --network none: %v", closed)
	}
	// Proxy set -> proxy env, default bridge (no --network).
	proxied := Runner{ProxyURL: "http://p:8080"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
		if !hasAdjacent(proxied, "-e", key+"=http://p:8080") {
			t.Errorf("proxy: expected -e %s: %v", key, proxied)
		}
	}
	if !hasAdjacent(proxied, "-e", "NO_PROXY=") {
		t.Errorf("proxy: expected -e NO_PROXY=: %v", proxied)
	}
	if hasAdjacent(proxied, "--network", "none") {
		t.Errorf("proxy: must not set --network none: %v", proxied)
	}
	// Named network -> --network <name>, no --network none. Proxy env applies
	// on top when both are set (host proxy on a hardened --internal network).
	both := Runner{Network: "hardened-1", ProxyURL: "http://p:8080"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(both, "--network", "hardened-1") {
		t.Errorf("network: expected --network hardened-1: %v", both)
	}
	if !hasAdjacent(both, "-e", "HTTPS_PROXY=http://p:8080") {
		t.Errorf("network+proxy: expected proxy env: %v", both)
	}
	// Named network without a proxy -> --network <name> only (caller owns
	// egress), no --network none and no empty proxy env.
	netOnly := Runner{Network: "hardened-1"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(netOnly, "--network", "hardened-1") || hasAdjacent(netOnly, "--network", "none") {
		t.Errorf("network only: expected --network hardened-1 and no none: %v", netOnly)
	}
	if hasAdjacent(netOnly, "-e", "HTTPS_PROXY=") {
		t.Errorf("network only: must not set empty proxy env: %v", netOnly)
	}
}

func TestRunnerArgs_SELinuxRelabel(t *testing.T) {
	on := Runner{
		SELinuxRelabel: true,
		StateDir:       "/data/state",
		Mounts:         []Mount{{Host: "/host/src", Container: "/src", ReadOnly: true}},
	}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(on, "-v", "/abs/work:"+WorkMount+":z") {
		t.Errorf("relabel on: expected /work mount with :z: %v", on)
	}
	if !hasAdjacent(on, "-v", "/data/state:"+StateMount+":z") {
		t.Errorf("relabel on: expected state mount with :z: %v", on)
	}
	if !hasAdjacent(on, "-v", "/host/src:/src:ro,z") {
		t.Errorf("relabel on: expected extra mount with ro,z: %v", on)
	}
	// Off (the default) is byte-for-byte unchanged: no :z anywhere.
	off := Runner{StateDir: "/data/state"}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	for _, a := range off {
		if strings.HasSuffix(a, ":z") || strings.HasSuffix(a, ",z") {
			t.Errorf("relabel off: unexpected :z in %q: %v", a, off)
		}
	}
}

func TestRunnerArgs_BaseURL(t *testing.T) {
	got := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount, BaseURL: "http://gw:8081"}, "/abs/work")
	if !hasAdjacent(got, "-e", "STUB_BASE_URL=http://gw:8081") {
		t.Errorf("expected Env(BaseURL) in %v", got)
	}
}

func TestTailStderr(t *testing.T) {
	short := "one line\ntwo lines"
	if got := tailStderr("  " + short + "  "); got != short {
		t.Errorf("tailStderr(short) = %q, want trimmed input", got)
	}
	body := strings.Repeat("noise noise noise\n", 400)
	long := body + "penultimate line\nactionable message"
	got := tailStderr(long)
	if len(got) > stderrTailLimit+len("[...] ") {
		t.Errorf("tailStderr(long) length = %d, want <= %d", len(got), stderrTailLimit+len("[...] "))
	}
	if !strings.HasPrefix(got, "[...] ") || !strings.HasSuffix(got, "actionable message") {
		t.Errorf("tailStderr(long) = %q, want [...] prefix and last line intact", got)
	}
	if strings.HasPrefix(strings.TrimPrefix(got, "[...] "), "oise") {
		t.Errorf("tailStderr(long) = %q, want cut at a line boundary", got)
	}
}

func TestRunNilBackend(t *testing.T) {
	if err := (Runner{}).Run(context.Background(), nil, harness.Job{Workspace: t.TempDir()}, nil); err == nil {
		t.Error("Run(nil harness) = nil, want error")
	}
	if err := (Runner{}).Run(context.Background(), stubHarness{}, harness.Job{}, nil); err == nil {
		t.Error("Run(empty workspace) = nil, want error")
	}
}
