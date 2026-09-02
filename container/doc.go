// Package container runs a harness backend inside an ephemeral OCI container.
// It owns runtime detection (docker, podman, Apple's container CLI), the
// hardening flags shared across engines, and the bind-mount layout that puts
// the caller's workspace at /work and an optional persistent state directory
// at /harness-state. Hardened scopes use a private internal network and, when
// the host proxy is unreachable, a sidecar restricted to backend egress hosts.
// Run owns those resources for one backend invocation. Open returns a Scope
// that can share them across auxiliary commands, backend runs, and retries.
package container
