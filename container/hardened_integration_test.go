//go:build integration

package container

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/alpha-omega-security/harness"
)

type integrationHarness struct{}

func (integrationHarness) Binary() string                             { return "true" }
func (integrationHarness) Args(harness.Job) []string                  { return nil }
func (integrationHarness) Prompt(harness.Job) string                  { return "" }
func (integrationHarness) ParseStream(io.Reader, func(harness.Event)) {}
func (integrationHarness) SkillDir(string, string) string             { return "" }
func (integrationHarness) GuideFilename() string                      { return "AGENTS.md" }
func (integrationHarness) SystemPromptViaArgs() bool                  { return true }
func (integrationHarness) EgressHosts() []string                      { return nil }
func (integrationHarness) Env(string) []string                        { return nil }
func (integrationHarness) StateEnv(string) []string                   { return nil }
func (integrationHarness) AccountErrorText(string) string             { return "" }
func (integrationHarness) DefaultModels() []harness.ModelDefault      { return nil }

func integrationHardenedRunner(t *testing.T, rt Runtime, image, proxyURL string) Runner {
	t.Helper()
	runner := Runner{Runtime: rt, Image: image, Hardened: true, ProxyURL: proxyURL}
	if sidecarImage := os.Getenv("HARNESS_TEST_SIDECAR_IMAGE"); sidecarImage != "" {
		runner.Sidecar.Image = sidecarImage
	}
	if rt.NeedsEgressSidecar() {
		probeImage := runner.Sidecar.Image
		if probeImage == "" {
			probeImage = image
		}
		if err := VerifyProxyBinary(t.Context(), rt, probeImage); err != nil {
			t.Skipf("sidecar image is unavailable: %v", err)
		}
	}
	return runner
}

func TestIntegrationHardenedRunner(t *testing.T) {
	image := os.Getenv("HARNESS_TEST_RUNNER_IMAGE")
	if image == "" {
		t.Skip("set HARNESS_TEST_RUNNER_IMAGE to a cached image containing true and grep")
	}
	rt, ok := DetectRuntime("docker")
	if !ok {
		t.Skip("docker is unavailable")
	}
	if !imageExistsLocally(t.Context(), rt, image) {
		t.Skipf("image %q is not cached", image)
	}
	runner := integrationHardenedRunner(t, rt, image, "http://harness:token@host.docker.internal:3128")
	if err := runner.Run(t.Context(), integrationHarness{}, harness.Job{Workspace: t.TempDir()}, func(harness.Event) {}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationHardenedScope(t *testing.T) {
	image := os.Getenv("HARNESS_TEST_RUNNER_IMAGE")
	if image == "" {
		t.Skip("set HARNESS_TEST_RUNNER_IMAGE to a cached image containing sh, true, and grep")
	}
	rt, ok := DetectRuntime("docker")
	if !ok {
		t.Skip("docker is unavailable")
	}
	if !imageExistsLocally(t.Context(), rt, image) {
		t.Skipf("image %q is not cached", image)
	}
	runner := integrationHardenedRunner(t, rt, image, "http://harness:token@host.docker.internal:3128")
	runner.Env = []string{"SCOPE_TOKEN"}
	runner.ProcessEnv = []string{"SCOPE_TOKEN=secret"}
	scope, err := runner.Open(t.Context(), integrationHarness{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scope.Close(nil) })
	job := harness.Job{Workspace: t.TempDir()}
	out, err := scope.RunCommand(t.Context(), job, Command{
		Args:    []string{"sh", "-c", `test "$SCOPE_TOKEN" = secret && printf ready`},
		WorkDir: "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "ready" {
		t.Fatalf("RunCommand output = %q", out)
	}
	for i := 0; i < 2; i++ {
		if err := scope.Run(t.Context(), job, nil); err != nil {
			t.Fatalf("Run %d: %v", i+1, err)
		}
	}
	network := scope.hardened.network
	if err := exec.CommandContext(t.Context(), rt.bin(), "network", "inspect", "--", network).Run(); err != nil {
		t.Fatalf("network %q was removed before Close: %v", network, err)
	}
	scope.Close(nil)
	if err := exec.CommandContext(t.Context(), rt.bin(), "network", "inspect", "--", network).Run(); err == nil {
		t.Fatalf("network %q remains after Close", network)
	}
}

func TestIntegrationHardenedDockerProxy(t *testing.T) {
	image := os.Getenv("HARNESS_TEST_RUNNER_IMAGE")
	if image == "" {
		t.Skip("set HARNESS_TEST_RUNNER_IMAGE to a cached image containing curl and grep")
	}
	rt, ok := DetectRuntime("docker")
	if !ok {
		t.Skip("docker is unavailable")
	}
	if !imageExistsLocally(t.Context(), rt, image) {
		t.Skipf("image %q is not cached", image)
	}
	if err := exec.CommandContext(t.Context(), rt.bin(), "run", "--rm", "--entrypoint", "curl", "--", image, "--version").Run(); err != nil {
		t.Skipf("image %q does not contain curl", image)
	}

	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "proxied")
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	proxyURL := fmt.Sprintf("http://host.docker.internal:%d", port)
	runner := integrationHardenedRunner(t, rt, image, proxyURL)
	if rt.NeedsEgressSidecar() {
		runner.ProxyURL = ""
		runner.Sidecar.APIPort = strconv.Itoa(port)
	}
	scope, err := runner.Open(t.Context(), integrationHarness{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scope.Close(nil) })
	out, err := scope.RunCommand(t.Context(), harness.Job{Workspace: t.TempDir()}, Command{
		Args: []string{"curl", "-fsS", "http://host.docker.internal:" + strconv.Itoa(port) + "/probe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "proxied" {
		t.Fatalf("proxied output = %q", out)
	}
}
