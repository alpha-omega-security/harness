package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alpha-omega-security/harness"
	"github.com/alpha-omega-security/harness/egress"
)

// DefaultImage bundles the harness backends (claude, codex, copilot,
// opencode), git, and node/python/go toolchains.
const DefaultImage = "ghcr.io/alpha-omega-security/scrutineer-runner:latest"

// tmpfsSpec is the /tmp tmpfs mount spec. HOME points here so backend session
// files land in the container, not the host user's dotfiles. The mount stays
// executable because build tools such as Go run temporary binaries from it.
const tmpfsSpec = "/tmp:rw,exec,nosuid,size=256m"

// Fixed in-container mount points. The caller's Job.Workspace is mounted at
// WorkMount and Runner.StateDir at StateMount; both stay writable under
// --read-only.
const (
	WorkMount  = "/work"
	StateMount = "/harness-state"
)

// Mount is one host path bind-mounted into the container in addition to the
// workspace and state directory.
type Mount struct {
	Host      string
	Container string
	ReadOnly  bool
}

// Runner runs a harness backend inside an ephemeral container. The zero value
// shells out to docker with DefaultImage and no network.
type Runner struct {
	// Runtime is the OCI engine; the zero value means docker.
	Runtime Runtime
	// Image is the runner image; empty uses DefaultImage.
	Image string
	// StateDir is a host path mounted at StateMount and pointed at by
	// h.StateEnv so a later run can resume the backend's session. Empty means
	// no state mount, so session state lands in the /tmp tmpfs and dies with
	// the container.
	StateDir string
	// Mounts are additional bind mounts beyond the workspace and StateDir.
	Mounts []Mount
	// Env are additional -e entries beyond h.Env(j.BaseURL). Bare keys pass
	// the host's value through, matching [harness.Run].
	Env []string
	// ProxyURL is set as HTTPS_PROXY/HTTP_PROXY/ALL_PROXY inside the
	// container. When empty and Network is empty, --network none is used so a
	// misconfigured caller fails closed rather than granting open egress.
	ProxyURL string
	// HostGatewayIP is the IPv4 host.docker.internal is aliased to. Empty
	// uses the runtime's own "host-gateway" resolution.
	HostGatewayIP string
	// Network is a pre-created network name attached with --network. When
	// set, the caller owns egress enforcement (typically a --internal network
	// paired with a proxy sidecar). Empty defers to ProxyURL.
	Network string
	// ReadOnly enables --read-only rootfs and (where supported)
	// --security-opt no-new-privileges. WorkMount, StateMount, and /tmp stay
	// writable.
	ReadOnly bool
	// SELinuxRelabel appends the ":z" relabel option to every bind mount.
	// Use ResolveSELinuxRelabel to derive it from a user-facing mode string.
	SELinuxRelabel bool
}

// Run starts h inside an ephemeral container with j.Workspace bind-mounted at
// WorkMount and streams parsed events to emit. It writes j.SystemPrompt to the
// backend's guide file on the host before starting the container, matching
// [harness.Run].
func (r Runner) Run(ctx context.Context, h harness.Harness, j harness.Job, emit func(harness.Event)) error {
	if h == nil {
		return fmt.Errorf("container: backend is required")
	}
	if j.Workspace == "" {
		return fmt.Errorf("container: workspace is required")
	}
	if emit == nil {
		emit = func(harness.Event) {}
	}
	if err := harness.WriteSystemPrompt(h, j); err != nil {
		return err
	}

	absWork, err := filepath.Abs(j.Workspace)
	if err != nil {
		return fmt.Errorf("container: workspace: %w", err)
	}
	if r.StateDir != "" {
		if r.StateDir, err = filepath.Abs(r.StateDir); err != nil {
			return fmt.Errorf("container: state dir: %w", err)
		}
	}

	// The backend runs against the fixed in-container paths; every other Job
	// field is workspace-relative and passes through unchanged.
	cj := j
	cj.Workspace = WorkMount

	args := r.args(h, cj, absWork)
	args = append(args, h.Binary())
	args = append(args, h.Args(cj)...)

	cmd := exec.CommandContext(ctx, r.Runtime.bin(), args...)
	// The runtime client inherits the full host environment: bare -e KEY
	// entries pick up their values from it, and DOCKER_HOST / CONTAINER_HOST
	// / XDG_RUNTIME_DIR select a non-default engine socket.
	cmd.Env = os.Environ()
	prepareProcessGroup(cmd)
	cmd.Cancel = func() error {
		return terminateProcessGroup(cmd.Process)
	}

	reader, writer := io.Pipe()
	var stderr strings.Builder
	cmd.Stdout = writer
	cmd.Stderr = io.MultiWriter(writer, &stderr)

	parseDone := make(chan struct{})
	go func() {
		h.ParseStream(reader, emit)
		close(parseDone)
	}()

	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		<-parseDone
		return fmt.Errorf("container: start %s: %w", r.Runtime.bin(), err)
	}
	runErr := cmd.Wait()
	_ = writer.Close()
	<-parseDone
	if runErr == nil {
		return nil
	}
	if detail := h.AccountErrorText(stderr.String()); detail != "" {
		return &harness.AccountError{Detail: detail}
	}
	// Unlike harness.Run, the tail of stderr is included: a container-runtime
	// failure (image pull, mount permission, cgroup) puts the actionable
	// message there and runErr alone is just "exit status 125".
	return fmt.Errorf("container: %s %s: %w: %s", r.Runtime.bin(), h.Binary(), runErr,
		tailStderr(stderr.String()))
}

// stderrTailLimit caps how much stderr is appended to a returned error so a
// runaway backend cannot balloon the error string a caller logs.
const stderrTailLimit = 4096

func tailStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= stderrTailLimit {
		return s
	}
	tail := s[len(s)-stderrTailLimit:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < len(tail)-1 {
		tail = tail[nl+1:]
	}
	return "[...] " + tail
}

// args builds the container-run argv up to and including the image name. j is
// the in-container view of the job (j.Workspace == WorkMount); absWork is the
// host workspace path bound there.
func (r Runner) args(h harness.Harness, j harness.Job, absWork string) []string {
	args := r.Runtime.runArgs(
		"--rm",
		"--cap-drop", "ALL",
	)
	args = appendHostUser(args)
	args = append(args,
		"-e", "HOME=/tmp",
		"--tmpfs", tmpfsSpec,
		"-v", bindMount(absWork, WorkMount, r.SELinuxRelabel),
		"-w", WorkMount,
	)
	// Backend env: model-API credential, base URL, and the backend's own
	// telemetry / autoupdate suppressors. Bare keys pass the host's value
	// through without putting secrets in the runtime process's argv. All
	// supported runtimes inherit bare keys from the client environment.
	for _, e := range append(h.Env(j.BaseURL), r.Env...) {
		args = append(args, "-e", e)
	}
	if r.Runtime.supportsHostGatewayAddHost() {
		gwTarget := "host-gateway"
		if r.HostGatewayIP != "" {
			gwTarget = r.HostGatewayIP
		}
		args = append(args, "--add-host", egress.HostGatewayAlias+":"+gwTarget)
	}
	if r.Runtime.NeedsKeepID() {
		// Rootless podman remaps --user uid:gid through /etc/subuid, so
		// writes to the bind mounts would land owned by a subordinate uid.
		// keep-id maps the container user back to the invoking host uid so
		// output stays host-owned.
		args = append(args, "--userns=keep-id")
	}
	if r.StateDir != "" {
		// Persist the backend's resumable session store outside the
		// container. Without this it lands in the /tmp tmpfs and dies with
		// the container, so a retry could not resume the agent loop. The
		// bind mount stays writable even under --read-only. The mountpoint
		// is fixed; each backend points its own state env var(s) at it via
		// StateEnv.
		args = append(args, "-v", bindMount(r.StateDir, StateMount, r.SELinuxRelabel))
		for _, e := range h.StateEnv(StateMount) {
			args = append(args, "-e", e)
		}
	}
	for _, m := range r.Mounts {
		var opts []string
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		args = append(args, "-v", bindMount(m.Host, m.Container, r.SELinuxRelabel, opts...))
	}
	if r.ReadOnly {
		// Read-only rootfs + no-new-privileges close the residual paths a
		// hostile workspace could use to escalate inside the container.
		// WorkMount stays writable and /tmp is the tmpfs declared above.
		// --cap-drop ALL is already set in every mode. Unix hosts also run
		// with the invoking uid and gid.
		args = append(args, "--read-only")
		if r.Runtime.supportsNoNewPrivileges() {
			args = append(args, "--security-opt", "no-new-privileges")
		}
	}
	if r.Network != "" {
		args = append(args, "--network", r.Network)
	}
	if r.ProxyURL != "" {
		args = append(args,
			"-e", "HTTPS_PROXY="+r.ProxyURL,
			"-e", "HTTP_PROXY="+r.ProxyURL,
			"-e", "ALL_PROXY="+r.ProxyURL,
			"-e", "NO_PROXY=",
		)
	} else if r.Network == "" {
		args = append(args, "--network", "none")
	}
	image := r.Image
	if image == "" {
		image = DefaultImage
	}
	return append(args, "--", image)
}
