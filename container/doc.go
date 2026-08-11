// Package container runs a harness backend inside an ephemeral OCI container.
// It owns runtime detection (docker, podman, Apple's container CLI), the
// hardening flags shared across engines, and the bind-mount layout that puts
// the caller's workspace at /work and an optional persistent state directory
// at /harness-state. Run mirrors [harness.Run]'s signature so a caller can
// switch between host and container execution by swapping the function.
package container
