//go:build unix

package harness

import (
	"os"
	"syscall"
)

func openRepairOutput(root *os.Root, name string) (*os.File, error) {
	// A concurrent writer can replace a regular file with a FIFO before open.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
