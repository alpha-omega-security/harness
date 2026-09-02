package container

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alpha-omega-security/harness"
)

type accountStubHarness struct{ stubHarness }

func (accountStubHarness) ParseStream(r io.Reader, _ func(harness.Event)) {
	_, _ = io.Copy(io.Discard, r)
}

func (accountStubHarness) AccountErrorText(text string) string {
	if strings.Contains(text, "credit balance") {
		return text
	}
	return ""
}

func TestScopeReusesHardenedResources(t *testing.T) {
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
  case "$*" in
    *"--entrypoint grep"*) printf '%s\n' '192.0.2.1 hgw'; exit 0 ;;
    *"http://1.1.1.1"*) printf '%s\n' 'BLOCKED'; exit 0 ;;
    *"http://10.89.1.2:3128/"*) printf '%s\n' 'REACHED'; exit 0 ;;
    *"provider-readiness"*) printf '%s\n' 'ready'; printf '%s\n' 'warning' >&2; exit 0 ;;
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

	runner := Runner{
		Runtime:  Runtime{Bin: "podman", Rootless: true},
		Image:    "img:latest",
		Hardened: true,
		Sidecar: SidecarConfig{
			Token:     "tok",
			APIPort:   "8080",
			GatewayIP: "192.0.2.9",
		},
	}
	scope, err := runner.Open(t.Context(), hardenedStubHarness{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { scope.Close(nil) })
	job := harness.Job{Workspace: t.TempDir()}
	out, err := scope.RunCommand(t.Context(), job, Command{
		Args:    []string{"sh", "-c", "provider-readiness"},
		WorkDir: "/tmp",
	})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if got := string(out); !strings.Contains(got, "ready") || !strings.Contains(got, "warning") {
		t.Errorf("RunCommand output = %q", got)
	}
	for i := 0; i < 2; i++ {
		if err := scope.Run(t.Context(), job, nil); err != nil {
			t.Fatalf("Run %d: %v", i+1, err)
		}
	}
	var events []harness.Event
	scope.Close(func(event harness.Event) {
		events = append(events, event)
	})
	scope.Close(nil)

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
	for label, got := range map[string]int{
		"network create": strings.Count(log, "network create --internal --disable-dns -- "+network),
		"sidecar create": strings.Count(log, "run -d --name "),
		"readiness":      strings.Count(log, "provider-readiness"),
		"backend run":    strings.Count(log, "stub --headless"),
		"network remove": strings.Count(log, "network rm -- "+network),
	} {
		want := 1
		if label == "backend run" {
			want = 2
		}
		if got != want {
			t.Errorf("%s count = %d, want %d\n%s", label, got, want, log)
		}
	}
	if !strings.Contains(log, "-w /tmp") || strings.Count(log, "--network "+network) < 3 {
		t.Errorf("scope commands did not share network and readiness workdir:\n%s", log)
	}
	if !strings.Contains(log, "--entrypoint sh -- img:latest -c provider-readiness") {
		t.Errorf("readiness command did not override the image entrypoint:\n%s", log)
	}
	if len(events) != 1 || events[0].Kind != harness.KindEgress || !strings.Contains(events[0].Text, "blocked.test") {
		t.Errorf("egress events = %+v", events)
	}
	if err := scope.Run(t.Context(), job, nil); err == nil || !strings.Contains(err.Error(), "scope is closed") {
		t.Errorf("Run after Close error = %v", err)
	}
	if _, err := scope.RunCommand(t.Context(), job, Command{Args: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "scope is closed") {
		t.Errorf("RunCommand after Close error = %v", err)
	}
}

func TestRunnerProcessEnvKeepsSecretOutOfArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "runtime")
	envPath := filepath.Join(dir, "env.log")
	argsPath := filepath.Join(dir, "args.log")
	script := `#!/bin/sh
printf '%s\n%s' "$PROVIDER_TOKEN" "$HTTPS_PROXY" > "$HARNESS_ENV_LOG"
printf '%s' "$*" > "$HARNESS_ARGS_LOG"
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROVIDER_TOKEN", "host-value")
	t.Setenv("HARNESS_ENV_LOG", envPath)
	t.Setenv("HARNESS_ARGS_LOG", argsPath)
	runner := Runner{
		Runtime:    Runtime{Bin: runtimePath},
		Image:      "img:latest",
		Env:        []string{"PROVIDER_TOKEN"},
		ProcessEnv: []string{"PROVIDER_TOKEN=scope-secret"},
		ProxyURL:   "http://harness:proxy-secret@proxy.test:3128",
	}
	if err := runner.Run(t.Context(), stubHarness{}, harness.Job{Workspace: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	envBytes, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(envBytes) != "scope-secret\nhttp://harness:proxy-secret@proxy.test:3128" {
		t.Errorf("runtime process env = %q", envBytes)
	}
	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if args := string(argsBytes); !strings.Contains(args, "-e PROVIDER_TOKEN") || !strings.Contains(args, "-e HTTPS_PROXY") || strings.Contains(args, "scope-secret") || strings.Contains(args, "proxy-secret") {
		t.Errorf("runtime args expose or omit provider token: %s", args)
	}
}

func TestRunnerRejectsInvalidProcessEnv(t *testing.T) {
	_, err := (Runner{ProcessEnv: []string{"MISSING_VALUE"}}).Open(context.Background(), stubHarness{})
	if err == nil || !strings.Contains(err.Error(), "KEY=value") {
		t.Errorf("Open invalid ProcessEnv error = %v", err)
	}
}

func TestScopeCommandReturnsRawFailureAndRunClassifiesAccountError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	runtimePath := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nprintf 'partial output'\nprintf 'credit balance is too low' >&2\nexit 7\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	scope, err := (Runner{Runtime: Runtime{Bin: runtimePath}}).Open(t.Context(), accountStubHarness{})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close(nil)
	job := harness.Job{Workspace: t.TempDir()}
	out, err := scope.RunCommand(t.Context(), job, Command{Args: []string{"provider-readiness"}})
	if err == nil {
		t.Fatal("RunCommand error = nil")
	}
	var accountErr *harness.AccountError
	if errors.As(err, &accountErr) {
		t.Fatalf("RunCommand classified account error: %v", err)
	}
	if message := err.Error(); !strings.Contains(message, runtimePath+" provider-readiness") || !strings.Contains(message, "credit balance is too low") {
		t.Errorf("RunCommand error lacks runtime context: %v", err)
	}
	if got := string(out); !strings.Contains(got, "partial output") || !strings.Contains(got, "credit balance") {
		t.Errorf("RunCommand output = %q", got)
	}
	err = scope.Run(t.Context(), job, nil)
	if !errors.As(err, &accountErr) {
		t.Fatalf("Run error = %v, want *harness.AccountError", err)
	}
}
