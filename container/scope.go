package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/alpha-omega-security/harness"
	"github.com/alpha-omega-security/harness/egress"
)

// Command is one auxiliary command run with a Scope's container settings.
// WorkDir defaults to WorkMount. Args contains the executable and its argv.
type Command struct {
	Args    []string
	WorkDir string
}

// Scope owns the resources shared by a sequence of container invocations.
// Close must run after the final command so hardened networks and proxy
// sidecars are removed and their egress events are emitted.
type Scope struct {
	runner     Runner
	harness    harness.Harness
	processEnv []string
	hardened   hardenedRun
	closed     atomic.Bool
}

// Open resolves Runner configuration and creates its hardened network and
// proxy sidecar when Hardened is set. Run and RunCommand reuse those resources
// until Close is called.
func (r Runner) Open(ctx context.Context, h harness.Harness) (*Scope, error) {
	if h == nil {
		return nil, fmt.Errorf("container: backend is required")
	}
	if r.StateDir != "" {
		abs, err := filepath.Abs(r.StateDir)
		if err != nil {
			return nil, fmt.Errorf("container: state dir: %w", err)
		}
		r.StateDir = abs
	}
	processEnv, err := overlayProcessEnv(os.Environ(), r.ProcessEnv)
	if err != nil {
		return nil, err
	}
	scope := &Scope{runner: r, harness: h, processEnv: processEnv}
	if !r.Hardened {
		scope.setProxyProcessEnv()
		return scope, nil
	}
	if r.Network != "" {
		return nil, fmt.Errorf("container: Hardened and Network cannot both be set")
	}
	hardened, err := r.setupHardened(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("container: hardened network: %w", err)
	}
	scope.hardened = hardened
	scope.runner.Hardened = false
	scope.runner.Network = hardened.network
	scope.runner.HostGatewayIP = hardened.gatewayIP
	scope.runner.ReadOnly = true
	switch {
	case hardened.proxyEndpoint != "":
		scope.runner.ProxyURL = egress.EndpointURL(hardened.token, hardened.proxyEndpoint)
	case r.Runtime.Bin == runtimeApple:
		scope.runner.ProxyURL = proxyURLWithHost(r.ProxyURL, hardened.gatewayIP)
	}
	scope.setProxyProcessEnv()
	return scope, nil
}

func (s *Scope) setProxyProcessEnv() {
	if s.runner.ProxyURL == "" {
		return
	}
	s.processEnv, _ = overlayProcessEnv(s.processEnv, []string{
		"HTTPS_PROXY=" + s.runner.ProxyURL,
		"HTTP_PROXY=" + s.runner.ProxyURL,
		"ALL_PROXY=" + s.runner.ProxyURL,
		"NO_PROXY=",
	})
}

// Run starts the scope's backend inside an ephemeral container and streams its
// parsed events to emit.
func (s *Scope) Run(ctx context.Context, j harness.Job, emit func(harness.Event)) error {
	if s == nil {
		return fmt.Errorf("container: scope is required")
	}
	if s.closed.Load() {
		return fmt.Errorf("container: scope is closed")
	}
	if err := validateJob(s.harness, j); err != nil {
		return err
	}
	if err := harness.WriteSystemPrompt(s.harness, j); err != nil {
		return err
	}
	return s.run(ctx, j, emit)
}

func (s *Scope) run(ctx context.Context, j harness.Job, emit func(harness.Event)) error {
	if s.closed.Load() {
		return fmt.Errorf("container: scope is closed")
	}
	absWork, err := filepath.Abs(j.Workspace)
	if err != nil {
		return fmt.Errorf("container: workspace: %w", err)
	}
	cj := j
	cj.Workspace = WorkMount
	args := s.runner.args(s.harness, cj, absWork)
	args = append(args, s.harness.Binary())
	args = append(args, s.harness.Args(cj)...)

	cmd := exec.CommandContext(ctx, s.runner.Runtime.bin(), args...)
	cmd.Env = s.processEnv
	stderr, err := harness.StreamCmd(cmd, s.harness, emit)
	if err == nil {
		return nil
	}
	var accountErr *harness.AccountError
	if errors.As(err, &accountErr) {
		return err
	}
	wrapped := fmt.Errorf("container: %s %s: %w", s.runner.Runtime.bin(), s.harness.Binary(), err)
	if tail := tailStderr(stderr); tail != "" {
		return fmt.Errorf("%w: %s", wrapped, tail)
	}
	return wrapped
}

// RunCommand runs an auxiliary command in an ephemeral container with the
// scope's mounts, environment, network, proxy, and hardening settings. Its
// combined output is returned without backend stream parsing.
func (s *Scope) RunCommand(ctx context.Context, j harness.Job, command Command) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("container: scope is required")
	}
	if s.closed.Load() {
		return nil, fmt.Errorf("container: scope is closed")
	}
	if err := validateJob(s.harness, j); err != nil {
		return nil, err
	}
	if len(command.Args) == 0 {
		return nil, fmt.Errorf("container: command args are required")
	}
	absWork, err := filepath.Abs(j.Workspace)
	if err != nil {
		return nil, fmt.Errorf("container: workspace: %w", err)
	}
	workDir := command.WorkDir
	if workDir == "" {
		workDir = WorkMount
	}
	cj := j
	cj.Workspace = WorkMount
	args := s.runner.argsAt(s.harness, cj, absWork, workDir, command.Args[0])
	args = append(args, command.Args[1:]...)

	cmd := exec.CommandContext(ctx, s.runner.Runtime.bin(), args...)
	cmd.Env = s.processEnv
	var output bytes.Buffer
	stderr, err := harness.StreamCmd(cmd, commandHarness{Harness: s.harness, output: &output}, nil)
	if err == nil {
		return output.Bytes(), nil
	}
	wrapped := fmt.Errorf("container: %s %s: %w", s.runner.Runtime.bin(), command.Args[0], err)
	if tail := tailStderr(stderr); tail != "" {
		wrapped = fmt.Errorf("%w: %s", wrapped, tail)
	}
	return output.Bytes(), wrapped
}

// Close emits proxy decisions and removes the scope's sidecar and network.
// Repeated calls have no effect.
func (s *Scope) Close(emit func(harness.Event)) {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.hardened.cleanup(s.runner.Runtime, emit)
}

func validateJob(h harness.Harness, j harness.Job) error {
	if h == nil {
		return fmt.Errorf("container: backend is required")
	}
	if j.Workspace == "" {
		return fmt.Errorf("container: workspace is required")
	}
	return nil
}

func overlayProcessEnv(base, overrides []string) ([]string, error) {
	out := append([]string(nil), base...)
	positions := make(map[string]int, len(out)+len(overrides))
	for i, entry := range out {
		key, _, _ := strings.Cut(entry, "=")
		positions[key] = i
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("container: ProcessEnv entry %q must have KEY=value form", entry)
		}
		if i, exists := positions[key]; exists {
			out[i] = entry
			continue
		}
		positions[key] = len(out)
		out = append(out, entry)
	}
	return out, nil
}

type commandHarness struct {
	harness.Harness
	output io.Writer
}

func (h commandHarness) ParseStream(r io.Reader, _ func(harness.Event)) {
	_, _ = io.Copy(h.output, r)
}

func (commandHarness) AccountErrorText(string) string { return "" }
