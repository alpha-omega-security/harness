//go:build windows

package harness

import "os"

func openRepairOutput(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
