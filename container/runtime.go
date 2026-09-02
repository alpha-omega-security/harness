package container

// Container runtime selection. The runner shells out to an OCI engine (docker,
// podman, or Apple's container) to run each job in an ephemeral container.
// This file owns the engine choice and the small set of traits that changes
// the generated `run` flags so the rest of the package stays runtime-neutral.

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// runtimeApple is the Runtime.Bin value for Apple's container runtime. Hoisted
// to a constant because the identifier is checked throughout the package.
const runtimeApple = "apple"

// runtimePodman is the Runtime.Bin value for podman (rootful or rootless).
// Hoisted to a constant for the same reason as runtimeApple.
const runtimePodman = "podman"

// Runtime identifies the OCI engine the runner shells out to and the main
// trait that changes the generated `run` flags: rootless podman maps
// --user uid:gid through /etc/subuid, so files written to bind mounts land as
// the wrong host uid unless --userns=keep-id is set. On Unix, docker and
// rootful podman both run the container process as the host uid directly.
// Apple's per-container VMs do not use podman's subuid remap, so they need no
// remap.
// The zero value is the docker runtime, so a bare Runner{} keeps shelling out
// to "docker".
type Runtime struct {
	Bin           string // "docker", "podman", or "apple"; "" means docker
	Rootless      bool   // true only for rootless podman
	DockerDesktop bool   // true when the detected docker daemon is Docker Desktop
	// Version is the engine version captured at detection (e.g. "4.9.4").
	// Best-effort and only used for the startup host-gateway check; "" when
	// unknown.
	Version string
}

// bin returns the executable name, defaulting to docker so the zero value
// stays valid.
func (rt Runtime) bin() string {
	switch rt.Bin {
	case "":
		return "docker"
	case runtimeApple:
		return "container"
	default:
		return rt.Bin
	}
}

// NeedsKeepID reports whether `run` invocations must add --userns=keep-id to
// keep bind-mount writes owned by the invoking host user. True only for
// rootless podman: docker and rootful podman already run the container process
// as the host uid, so remapping there would be wrong. The first such run
// remaps the whole runner image into the subuid range and can take ~a minute,
// so callers may want to log a notice before [VerifyKeepID].
func (rt Runtime) NeedsKeepID() bool {
	return rt.Bin == runtimePodman && rt.Rootless
}

// NeedsHardenedNetVerify reports whether a hardened run must prove its
// per-run --internal network fail-closed before running. True for runtimes
// that use a sidecar and for Apple's container runtime. Rootless podman's
// pasta/slirp4netns host path is what varies across backends and what
// --internal can sever. Apple's vmnet host-only network has the right
// semantics (egress blocked, host reachable) but the implementation has known
// rough edges, and this is a security boundary, so it is proven per run
// rather than assumed. docker and rootful podman both run a bridge in the
// host netns (gateway on the host), so Linux docker and rootful podman keep the
// trusted path and pay no probe cost.
func (rt Runtime) NeedsHardenedNetVerify() bool {
	return rt.Bin == runtimeApple || rt.NeedsEgressSidecar()
}

// NeedsEgressSidecar reports whether hardened egress runs through a per-run
// proxy sidecar rather than an in-process host proxy. Rootless podman and
// Docker Desktop cannot reach a host proxy across the --internal boundary.
// Apple keeps the host proxy, reached through the per-run gateway.
func (rt Runtime) NeedsEgressSidecar() bool {
	return (rt.Bin == runtimePodman && rt.Rootless) || rt.isDockerDesktop()
}

func (rt Runtime) isDockerDesktop() bool {
	if rt.bin() != "docker" {
		return false
	}
	if rt.Version != "" {
		return rt.DockerDesktop
	}
	return rt.DockerDesktop || runtime.GOOS != "linux"
}

func (rt Runtime) sidecarEgressNetwork() string {
	if rt.bin() == "docker" {
		return "bridge"
	}
	return "podman"
}

func (rt Runtime) needsInternalDNSDisabled() bool {
	return rt.Bin == runtimePodman && rt.Rootless
}

// supportsHostGatewayAddHost reports whether the runtime accepts Docker's
// `--add-host name:host-gateway` marker. Apple's container CLI does not
// expose that flag; it reaches host services through the default gateway
// address instead.
func (rt Runtime) supportsHostGatewayAddHost() bool {
	return rt.Bin != runtimeApple
}

func (rt Runtime) hostGatewayProbeNetwork(network string) string {
	if rt.isDockerDesktop() {
		return ""
	}
	return network
}

// supportsPullNever reports whether `run --pull never` is supported. Apple's
// container CLI does not expose a pull policy flag, so callers that need a
// no-pull probe must check the local image cache before running.
func (rt Runtime) supportsPullNever() bool {
	return rt.Bin != runtimeApple
}

// supportsNoNewPrivileges reports whether the runtime accepts Docker/Podman's
// `--security-opt no-new-privileges` hardening flag.
func (rt Runtime) supportsNoNewPrivileges() bool {
	return rt.Bin != runtimeApple
}

// runArgs starts a runtime `run` command, adding runtime-specific flags that
// must precede the common options. Apple's container CLI writes lifecycle
// progress to stdout by default; suppress it so probe parsers and the
// backend's stream reader only see the container payload.
func (rt Runtime) runArgs(args ...string) []string {
	out := []string{"run"}
	if rt.Bin == runtimeApple {
		out = append(out, "--progress", "none")
	}
	return append(out, args...)
}

// runtimeProber runs a runtime command and returns its stdout. The production
// prober shells out; tests inject a stub so DetectRuntime's selection logic is
// exercised without a live daemon.
type runtimeProber func(name string, args ...string) ([]byte, error)

func execProber(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// DetectRuntime resolves the caller's runtime choice into a Runtime, verifying
// the engine is actually reachable. prefer is "docker" (or "" defaulting to
// docker), "podman", or "apple". There is no auto-detection or fallback: a
// podman-only host left at the docker default still reports unavailable, by
// design (explicit opt-in). For podman it also probes rootless-ness so the run
// path can decide on --userns=keep-id.
//
// Returns (zero, false) when the chosen engine is not installed or its daemon
// is unreachable, so the caller emits the same hard error it emits for a
// missing docker.
func DetectRuntime(prefer string) (Runtime, bool) {
	return detectRuntime(prefer, execProber)
}

func detectRuntime(prefer string, probe runtimeProber) (Runtime, bool) {
	switch prefer {
	case "", "docker":
		// Both fields exist in docker's info schema. OperatingSystem identifies
		// Docker Desktop even when its client runs on Linux.
		out, err := probe("docker", "info", "--format", "{{.ServerVersion}}|{{.OperatingSystem}}")
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return Runtime{}, false
		}
		version, desktop, ok := parseDockerInfo(out)
		if !ok {
			return Runtime{}, false
		}
		return Runtime{Bin: "docker", DockerDesktop: desktop, Version: version}, true
	case runtimePodman:
		// podman's info has no .ServerVersion (a docker-only field that would
		// error the Go template); .Version.Version is the engine version and
		// .Host.Security.Rootless is the rootless flag. One call confirms
		// reachability AND rootless-ness without ever feeding podman the
		// docker template.
		out, err := probe(runtimePodman, "info", "--format", "{{.Version.Version}}|{{.Host.Security.Rootless}}")
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return Runtime{}, false
		}
		version, rootless, ok := parsePodmanInfo(out)
		if !ok {
			return Runtime{}, false
		}
		return Runtime{Bin: runtimePodman, Rootless: rootless, Version: version}, true
	case runtimeApple:
		// Apple's container CLI has no docker/podman-compatible `info`
		// template. `system status` verifies the background service is
		// running; parse the apiserver version best-effort for logs.
		out, err := probe("container", "system", "status")
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return Runtime{}, false
		}
		return Runtime{Bin: runtimeApple, Version: parseAppleStatus(out)}, true
	default:
		return Runtime{}, false
	}
}

func parseDockerInfo(out []byte) (version string, desktop, ok bool) {
	version, operatingSystem, found := strings.Cut(strings.TrimSpace(string(out)), "|")
	version = strings.TrimSpace(version)
	if !found || version == "" {
		return "", false, false
	}
	return version, strings.Contains(strings.ToLower(operatingSystem), "docker desktop"), true
}

// parsePodmanInfo splits the "<version>|<rootless>" line emitted by the podman
// info probe. ok is false when the line is malformed or the rootless field is
// not a bool, so DetectRuntime treats an unparseable probe as unavailable
// rather than guessing the uid-remap behaviour (which would silently break
// bind-mount ownership).
func parsePodmanInfo(out []byte) (version string, rootless, ok bool) {
	v, r, found := strings.Cut(strings.TrimSpace(string(out)), "|")
	if !found {
		return "", false, false
	}
	b, err := strconv.ParseBool(strings.TrimSpace(r))
	if err != nil {
		return "", false, false
	}
	return strings.TrimSpace(v), b, true
}

// parseAppleStatus extracts the apiserver version from `container system
// status` output.
func parseAppleStatus(out []byte) string {
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "apiserver.version" {
			continue
		}
		if v := firstDottedVersion(strings.Join(fields[1:], " ")); v != "" {
			return v
		}
	}
	return firstDottedVersion(string(out))
}

func firstDottedVersion(s string) string {
	for field := range strings.FieldsSeq(s) {
		field = strings.Trim(field, "(),")
		if _, _, ok := parseMajorMinor(field); ok {
			return field
		}
	}
	return ""
}

// podman gained `--add-host name:host-gateway` in 4.7; below that the egress
// path cannot resolve the host alias.
const (
	podmanHostGatewayMajor = 4
	podmanHostGatewayMinor = 7
)

// podman 5.0 makes pasta the default rootless network backend; pasta forwards
// host-gateway to the host loopback (--map-host-loopback), which an egress
// proxy sidecar needs to reach a loopback-bound host API. Below 5.0 the
// default is slirp4netns, which also reaches host loopback when host-loopback
// is enabled -- so this is a soft signal, not a gate.
const podmanPastaDefaultMajor = 5

// podmanHostGatewaySupported reports whether the podman version is recent
// enough to honour `--add-host host.docker.internal:host-gateway`, which the
// egress path depends on. An unparseable version returns true so a probe quirk
// never produces a spurious startup warning.
func podmanHostGatewaySupported(version string) bool {
	major, minor, ok := parseMajorMinor(version)
	if !ok {
		return true
	}
	return major > podmanHostGatewayMajor || (major == podmanHostGatewayMajor && minor >= podmanHostGatewayMinor)
}

// podmanPastaDefault reports whether the podman version defaults to the pasta
// backend (>= 5.0), the most reliable host-gateway -> host-loopback path for
// an egress proxy sidecar. An unparseable version returns true so a probe
// quirk never produces a spurious warning.
func podmanPastaDefault(version string) bool {
	major, _, ok := parseMajorMinor(version)
	if !ok {
		return true
	}
	return major >= podmanPastaDefaultMajor
}

// parseMajorMinor pulls the leading major and minor integers out of a dotted
// version string ("4.9.4" -> 4, 9). ok is false when either is absent or
// non-numeric.
func parseMajorMinor(version string) (major, minor int, ok bool) {
	majStr, rest, found := strings.Cut(strings.TrimSpace(version), ".")
	if !found {
		return 0, 0, false
	}
	minStr, _, _ := strings.Cut(rest, ".")
	maj, err1 := strconv.Atoi(majStr)
	min, err2 := strconv.Atoi(minStr)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// HostGatewaySupported reports whether the detected runtime is known to honour
// `--add-host host.docker.internal:host-gateway`, which the egress path needs.
// Always true for docker; for podman it checks the detected version against
// 4.7. Used for a soft startup warning, not a hard gate (an unparseable
// version returns true so a probe quirk never blocks startup).
func (rt Runtime) HostGatewaySupported() bool {
	if rt.Bin != runtimePodman {
		return true
	}
	return podmanHostGatewaySupported(rt.Version)
}

// HostLoopbackBackendLikely reports whether the detected podman is recent
// enough to default to a backend (pasta >= 5.0) that forwards host-gateway to
// the host loopback, which an egress proxy sidecar needs to reach a
// loopback-bound host API. Used for a soft startup warning on the rootless
// hardened sidecar path, not a hard gate: older podman with a
// host-loopback-enabled slirp4netns also works, and the sidecar verifies
// reachability fail-closed regardless. Always true for non-podman; an
// unparseable version returns true.
func (rt Runtime) HostLoopbackBackendLikely() bool {
	if rt.Bin != runtimePodman {
		return true
	}
	return podmanPastaDefault(rt.Version)
}

// imageExistsLocally reports whether tag is in the runtime's local image
// cache. Apple's container CLI has no `image inspect`; the callers
// (VerifyKeepID, VerifySELinuxMount) are podman- and Linux-only respectively,
// so this is never reached on Apple and would fail safe (skip the smoke test)
// if it were.
func imageExistsLocally(ctx context.Context, rt Runtime, tag string) bool {
	return exec.CommandContext(ctx, rt.bin(), "image", "inspect", tag).Run() == nil
}

// VerifyKeepID smoke-tests `--userns=keep-id` for rootless podman so a missing
// or too-small /etc/subuid range fails once at startup with an actionable
// message instead of silently breaking every run's bind-mount ownership. It is
// a no-op for docker and rootful podman. It is also skipped (returns nil) when
// the runner image is not yet present locally: the check needs an image to
// run, and the first job will pull it -- and surface any sub-id problem --
// then, so startup never eagerly pulls.
func VerifyKeepID(ctx context.Context, rt Runtime, image string) error {
	if !rt.NeedsKeepID() {
		return nil
	}
	if image == "" || !imageExistsLocally(ctx, rt, image) {
		return nil
	}
	out, err := exec.CommandContext(ctx, rt.bin(), "run", "--rm", "--pull", "never",
		"--userns=keep-id", "--entrypoint", "sh", "--", image, "-c", "exit 0").CombinedOutput()
	if err != nil {
		return fmt.Errorf("rootless podman --userns=keep-id smoke test failed "+
			"(ensure /etc/subuid and /etc/subgid grant your user a sub-id range; "+
			"see `podman system migrate`): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
