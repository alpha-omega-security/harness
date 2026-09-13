package container

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alpha-omega-security/harness"
	"github.com/alpha-omega-security/harness/egress"
)

func TestVerifyProxyBinaryRequiresCapabilityBeforeHelp(t *testing.T) {
	for _, image := range []string{"current:latest", "stale:latest", "missing-binary:latest", "uncached:latest"} {
		t.Run(image, func(t *testing.T) {
			logPath := capabilityRuntime(t, `
if [ "$1" = "image" ]; then
  case "$*" in *"uncached:latest"*) exit 1 ;; esac
  exit 0
fi
case "$*" in
  *"-- current:latest harness-proxy --require-capability=deny-api-connect-v1 -h") exit 0 ;;
  *"-- stale:latest harness-proxy --require-capability=deny-api-connect-v1 -h")
    printf '%s\n' 'flag provided but not defined: -require-capability'; exit 2 ;;
  *"missing-binary:latest"*) printf '%s\n' 'harness-proxy: not found'; exit 127 ;;
esac
exit 99
`)
			err := VerifyProxyBinary(t.Context(), Runtime{Bin: "podman", Rootless: true}, image)
			wantError := image == "stale:latest" || image == "missing-binary:latest"
			if (err != nil) != wantError {
				t.Fatalf("VerifyProxyBinary error = %v, wantError=%v", err, wantError)
			}
			if err != nil && !strings.Contains(err.Error(), "update or rebuild") {
				t.Fatalf("missing remediation in error: %v", err)
			}
			log := readCapabilityLog(t, logPath)
			want := "run --rm --pull never -- " + image + " harness-proxy --require-capability=" + egress.CapabilityDenyAPIConnect + " -h"
			if image == "uncached:latest" {
				if strings.Contains(log, "run ") {
					t.Fatalf("preflight attempted to run an uncached image: %s", log)
				}
			} else if !strings.Contains(log, want) {
				t.Fatalf("missing capability before help in runtime log: %s", log)
			}
		})
	}
}

func TestRunnerRejectsExitedCustomProxy(t *testing.T) {
	logPath := capabilityRuntime(t, `
if [ "$1" = "network" ] && [ "$2" = "inspect" ]; then exit 1; fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"--entrypoint grep"*) printf '%s\n' '192.0.2.1 hgw'; exit 0 ;;
    *"http://1.1.1.1"*) printf '%s\n' 'BLOCKED'; exit 0 ;;
    *"http://10.89.1.2:3128/"*) printf '%s\n' 'UNREACHABLE'; exit 0 ;;
  esac
fi
if [ "$1" = "inspect" ]; then
  case "$*" in
    *".State.Running"*) printf '%s\n' 'false' ;;
    *) printf '%s\n' '10.89.1.2' ;;
  esac
fi
if [ "$1" = "logs" ]; then
  printf '%s\n' 'flag provided but not defined: -require-capability'
fi
exit 0
`)
	runner := Runner{
		Runtime: Runtime{Bin: "podman", Rootless: true}, Image: "current:latest", Hardened: true,
		Sidecar: SidecarConfig{Image: "custom:stale", Token: "tok"},
	}
	err := runner.Run(t.Context(), hardenedStubHarness{}, harness.Job{Workspace: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "exited before becoming reachable") || !strings.Contains(err.Error(), "require-capability") {
		t.Fatalf("Run error = %v", err)
	}
	log := readCapabilityLog(t, logPath)
	for _, want := range []string{
		"-- custom:stale harness-proxy --require-capability=" + egress.CapabilityDenyAPIConnect,
		"network rm -- " + hardenedNetworkPrefix,
		"rm -f -- " + proxySidecarPrefix,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("runtime log missing %q: %s", want, log)
		}
	}
	if strings.Contains(log, "stub --headless") {
		t.Fatalf("workload launched after the proxy exited: %s", log)
	}
}

func capabilityRuntime(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test runtime is a POSIX shell script")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "runtime.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HARNESS_RUNTIME_LOG\"\n" + body
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HARNESS_RUNTIME_LOG", logPath)
	return logPath
}

func readCapabilityLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
