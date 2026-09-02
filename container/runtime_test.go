package container

import (
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestRuntimeBin(t *testing.T) {
	appleBinary := "container"
	tests := []struct {
		rt   Runtime
		want string
	}{
		{Runtime{}, "docker"},
		{Runtime{Bin: "docker"}, "docker"},
		{Runtime{Bin: "podman"}, "podman"},
		{Runtime{Bin: "podman", Rootless: true}, "podman"},
		{Runtime{Bin: "apple"}, appleBinary},
	}
	for _, tc := range tests {
		if got := tc.rt.bin(); got != tc.want {
			t.Errorf("%+v.bin() = %q, want %q", tc.rt, got, tc.want)
		}
	}
}

func TestRuntimeNeedsKeepID(t *testing.T) {
	// keep-id is the bind-mount ownership fix and must fire for rootless
	// podman ONLY: docker and rootful podman already run as the host uid, so
	// remapping there would break mounts.
	tests := []struct {
		rt   Runtime
		want bool
	}{
		{Runtime{}, false},                              // docker (zero value)
		{Runtime{Bin: "docker"}, false},                 // docker explicit
		{Runtime{Bin: "podman"}, false},                 // rootful podman
		{Runtime{Bin: "podman", Rootless: true}, true},  // rootless podman
		{Runtime{Bin: "docker", Rootless: true}, false}, // rootless flag ignored for docker
		{Runtime{Bin: "apple"}, false},                  // Apple has no podman subuid remap
	}
	for _, tc := range tests {
		if got := tc.rt.NeedsKeepID(); got != tc.want {
			t.Errorf("%+v.NeedsKeepID() = %v, want %v", tc.rt, got, tc.want)
		}
	}
}

func TestRuntimeNeedsHardenedNetVerify(t *testing.T) {
	wantUnknownDocker := runtime.GOOS != "linux"
	tests := []struct {
		rt   Runtime
		want bool
	}{
		{Runtime{}, wantUnknownDocker},                                         // docker (zero value)
		{Runtime{Bin: "docker", Version: "24.0.7"}, false},                     // docker engine
		{Runtime{Bin: "docker", Version: "24.0.7", DockerDesktop: true}, true}, // Docker Desktop
		{Runtime{Bin: "podman"}, false},                                        // rootful podman -> trusted like docker
		{Runtime{Bin: "podman", Rootless: true}, true},                         // rootless podman -> verified
		{Runtime{Bin: "apple"}, true},                                          // apple --internal -> proven per run
	}
	for _, tc := range tests {
		if got := tc.rt.NeedsHardenedNetVerify(); got != tc.want {
			t.Errorf("%+v.NeedsHardenedNetVerify() = %v, want %v", tc.rt, got, tc.want)
		}
	}
}

func TestRuntimeHostGatewayProbeNetwork(t *testing.T) {
	for _, tc := range []struct {
		rt   Runtime
		want string
	}{
		{Runtime{Bin: "docker", Version: "24.0.7"}, "hardened"},
		{Runtime{Bin: "docker", Version: "24.0.7", DockerDesktop: true}, ""},
		{Runtime{Bin: "podman"}, "hardened"},
		{Runtime{Bin: "apple"}, "hardened"},
	} {
		if got := tc.rt.hostGatewayProbeNetwork("hardened"); got != tc.want {
			t.Errorf("%+v.hostGatewayProbeNetwork() = %q, want %q", tc.rt, got, tc.want)
		}
	}
}

func TestRuntimeNeedsEgressSidecar(t *testing.T) {
	wantUnknownDocker := runtime.GOOS != "linux"
	tests := []struct {
		rt   Runtime
		want bool
	}{
		{Runtime{}, wantUnknownDocker},                                         // docker (zero value)
		{Runtime{Bin: "docker", Version: "24.0.7"}, false},                     // docker engine
		{Runtime{Bin: "docker", Version: "24.0.7", DockerDesktop: true}, true}, // Docker Desktop
		{Runtime{Bin: "podman"}, false},                                        // rootful podman -> host proxy
		{Runtime{Bin: "podman", Rootless: true}, true},                         // rootless podman -> sidecar
		{Runtime{Bin: "apple"}, false},                                         // apple -> host proxy, NOT a sidecar
	}
	for _, tc := range tests {
		if got := tc.rt.NeedsEgressSidecar(); got != tc.want {
			t.Errorf("%+v.NeedsEgressSidecar() = %v, want %v", tc.rt, got, tc.want)
		}
	}
	if got := (Runtime{Bin: "docker"}).sidecarEgressNetwork(); got != "bridge" {
		t.Errorf("docker sidecar network = %q", got)
	}
	if got := (Runtime{Bin: "podman", Rootless: true}).sidecarEgressNetwork(); got != "podman" {
		t.Errorf("podman sidecar network = %q", got)
	}
}

// TestRuntimeCapabilityFlags is the run-flag parity matrix: for each runtime
// it pins exactly which Docker/Podman flags apply and how `run` starts. docker
// and podman are identical; apple diverges only where its CLI lacks the flag
// (--add-host, --pull never, --security-opt) and adds --progress none.
func TestRuntimeCapabilityFlags(t *testing.T) {
	tests := []struct {
		name                string
		rt                  Runtime
		wantHostGatewayAdd  bool
		wantPullNever       bool
		wantNoNewPrivileges bool
		wantRunArgs         []string
	}{
		{"docker zero value", Runtime{}, true, true, true, []string{"run", "--rm"}},
		{"docker explicit", Runtime{Bin: "docker"}, true, true, true, []string{"run", "--rm"}},
		{"podman", Runtime{Bin: "podman"}, true, true, true, []string{"run", "--rm"}},
		{"apple", Runtime{Bin: "apple"}, false, false, false, []string{"run", "--progress", "none", "--rm"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rt.supportsHostGatewayAddHost(); got != tc.wantHostGatewayAdd {
				t.Errorf("supportsHostGatewayAddHost = %v, want %v", got, tc.wantHostGatewayAdd)
			}
			if got := tc.rt.supportsPullNever(); got != tc.wantPullNever {
				t.Errorf("supportsPullNever = %v, want %v", got, tc.wantPullNever)
			}
			if got := tc.rt.supportsNoNewPrivileges(); got != tc.wantNoNewPrivileges {
				t.Errorf("supportsNoNewPrivileges = %v, want %v", got, tc.wantNoNewPrivileges)
			}
			if got := tc.rt.runArgs("--rm"); !slices.Equal(got, tc.wantRunArgs) {
				t.Errorf("runArgs = %v, want %v", got, tc.wantRunArgs)
			}
		})
	}
}

func TestDetectRuntime(t *testing.T) {
	probeErr := errors.New("not installed")
	appleBinary := "container"
	type call struct {
		name string
		args []string
	}
	tests := []struct {
		name     string
		prefer   string
		probeOut []byte
		probeErr error
		want     Runtime
		wantOK   bool
	}{
		{"docker engine", "docker", []byte("24.0.7|Ubuntu 24.04 LTS\n"), nil, Runtime{Bin: "docker", Version: "24.0.7"}, true},
		{"docker desktop", "docker", []byte("24.0.7|Docker Desktop\n"), nil, Runtime{Bin: "docker", DockerDesktop: true, Version: "24.0.7"}, true},
		{"empty defaults to docker", "", []byte("24.0.7|Ubuntu 24.04 LTS\n"), nil, Runtime{Bin: "docker", Version: "24.0.7"}, true},
		{"podman rootless", "podman", []byte("4.9.4|true\n"), nil, Runtime{Bin: "podman", Rootless: true, Version: "4.9.4"}, true},
		{"podman rootful", "podman", []byte("4.9.4|false\n"), nil, Runtime{Bin: "podman", Rootless: false, Version: "4.9.4"}, true},
		{"apple", "apple", []byte("FIELD VALUE\nstatus running\napiserver.version container-apiserver version 1.0.0 (build: release)\n"), nil, Runtime{Bin: "apple", Version: "1.0.0"}, true},
		// No fallback: a podman probe failure stays unavailable; the docker
		// default on a podman-only host likewise fails (explicit opt-in).
		{"podman unreachable", "podman", nil, probeErr, Runtime{}, false},
		{"docker unreachable", "docker", nil, probeErr, Runtime{}, false},
		{"apple unreachable", "apple", nil, probeErr, Runtime{}, false},
		{"podman malformed", "podman", []byte("nopipe\n"), nil, Runtime{}, false},
		{"docker malformed", "docker", []byte("24.0.7\n"), nil, Runtime{}, false},
		{"docker empty output", "docker", []byte("  \n"), nil, Runtime{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []call
			probe := func(name string, args ...string) ([]byte, error) {
				calls = append(calls, call{name, append([]string(nil), args...)})
				return tc.probeOut, tc.probeErr
			}
			got, ok := detectRuntime(tc.prefer, probe)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("detectRuntime(%q) = %+v,%v; want %+v,%v", tc.prefer, got, ok, tc.want, tc.wantOK)
			}
			// docker's {{.ServerVersion}} errors against podman's schema and
			// podman's fields error against docker's; assert each engine only
			// ever sees its own template (guards the availability-flip risk).
			for _, c := range calls {
				joined := strings.Join(c.args, " ")
				if c.name == "podman" && strings.Contains(joined, "ServerVersion") {
					t.Errorf("podman probed with docker template: %v", c.args)
				}
				if c.name == "docker" && strings.Contains(joined, "Host.Security.Rootless") {
					t.Errorf("docker probed with podman template: %v", c.args)
				}
				if c.name == appleBinary && strings.Contains(joined, "--format") {
					t.Errorf("apple runtime probed with docker/podman format template: %v", c.args)
				}
			}
		})
	}

	t.Run("bogus prefer never probes", func(t *testing.T) {
		called := false
		probe := func(string, ...string) ([]byte, error) { called = true; return nil, nil }
		if got, ok := detectRuntime("containerd", probe); ok || got != (Runtime{}) {
			t.Errorf("detectRuntime(bogus) = %+v,%v; want zero,false", got, ok)
		}
		if called {
			t.Error("bogus runtime should not shell out")
		}
	})
}

func TestParseDockerInfo(t *testing.T) {
	tests := []struct {
		in          string
		wantVersion string
		wantDesktop bool
		wantOK      bool
	}{
		{"24.0.7|Ubuntu 24.04 LTS\n", "24.0.7", false, true},
		{"29.7.2|Docker Desktop", "29.7.2", true, true},
		{"29.7.2|docker desktop for linux", "29.7.2", true, true},
		{"29.7.2", "", false, false},
		{"|Docker Desktop", "", false, false},
	}
	for _, tc := range tests {
		version, desktop, ok := parseDockerInfo([]byte(tc.in))
		if version != tc.wantVersion || desktop != tc.wantDesktop || ok != tc.wantOK {
			t.Errorf("parseDockerInfo(%q) = %q,%v,%v; want %q,%v,%v", tc.in, version, desktop, ok, tc.wantVersion, tc.wantDesktop, tc.wantOK)
		}
	}
}

func TestParsePodmanInfo(t *testing.T) {
	tests := []struct {
		in       string
		wantVer  string
		wantRoot bool
		wantOK   bool
	}{
		{"4.9.4|true\n", "4.9.4", true, true},
		{"4.9.4|false", "4.9.4", false, true},
		{" 5.0.1 | true ", "5.0.1", true, true},
		{"nopipe", "", false, false},
		{"4.9.4|maybe", "", false, false},
		{"", "", false, false},
	}
	for _, tc := range tests {
		ver, root, ok := parsePodmanInfo([]byte(tc.in))
		if ver != tc.wantVer || root != tc.wantRoot || ok != tc.wantOK {
			t.Errorf("parsePodmanInfo(%q) = %q,%v,%v; want %q,%v,%v", tc.in, ver, root, ok, tc.wantVer, tc.wantRoot, tc.wantOK)
		}
	}
}

func TestParseAppleStatus(t *testing.T) {
	in := []byte("FIELD VALUE\nstatus running\napiserver.version container-apiserver version 1.0.0 (build: release)\n")
	if got := parseAppleStatus(in); got != "1.0.0" {
		t.Errorf("parseAppleStatus = %q, want 1.0.0", got)
	}
	if got := parseAppleStatus([]byte("container CLI version 1.2.3")); got != "1.2.3" {
		t.Errorf("fallback parseAppleStatus = %q, want 1.2.3", got)
	}
}

func TestPodmanHostGatewaySupported(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"4.7.0", true},
		{"4.7", true},
		{"4.9.4", true},
		{"5.0.1", true},
		{"4.6.9", false},
		{"3.4.0", false},
		{"", true},        // unparseable: don't warn
		{"garbage", true}, // unparseable: don't warn
		{"4", true},       // no minor: don't warn
	}
	for _, tc := range tests {
		if got := podmanHostGatewaySupported(tc.version); got != tc.want {
			t.Errorf("podmanHostGatewaySupported(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestPodmanPastaDefault(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"5.0.0", true},
		{"5.0", true},
		{"5.4.1", true},
		{"6.0.0", true},
		{"4.9.4", false},
		{"4.7.0", false},
		{"3.4.0", false},
		{"", true},        // unparseable: don't warn
		{"garbage", true}, // unparseable: don't warn
		{"5", true},       // no minor: don't warn
	}
	for _, tc := range tests {
		if got := podmanPastaDefault(tc.version); got != tc.want {
			t.Errorf("podmanPastaDefault(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestHostLoopbackBackendLikely(t *testing.T) {
	tests := []struct {
		rt   Runtime
		want bool
	}{
		{Runtime{Bin: "docker"}, true},                   // non-podman: always true
		{Runtime{}, true},                                // zero value = docker
		{Runtime{Bin: "podman", Version: "5.0.0"}, true}, // pasta default
		{Runtime{Bin: "podman", Version: "6.1.0"}, true},
		{Runtime{Bin: "podman", Version: "4.9.4"}, false}, // pre-5.0: warn
		{Runtime{Bin: "podman", Version: ""}, true},       // unparseable: don't warn
	}
	for _, tc := range tests {
		if got := tc.rt.HostLoopbackBackendLikely(); got != tc.want {
			t.Errorf("HostLoopbackBackendLikely(%+v) = %v, want %v", tc.rt, got, tc.want)
		}
	}
}
