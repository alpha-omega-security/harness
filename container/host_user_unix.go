//go:build unix

package container

import (
	"fmt"
	"os"
)

// hostUser returns the "uid:gid" string for --user so files written to bind
// mounts stay owned by the invoking host user.
func hostUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
