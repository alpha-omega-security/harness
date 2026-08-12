//go:build windows

package container

// Windows has no Unix uid or gid to pass to a Linux container. Docker Desktop
// handles bind-mount permissions, so the image's default user is used.
func hostUser() string {
	return ""
}
