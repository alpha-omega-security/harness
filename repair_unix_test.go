//go:build unix && !aix && !solaris

package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenRepairOutputDoesNotBlockOnFIFO(t *testing.T) {
	workspace := t.TempDir()
	name := filepath.Join(workspace, "report.json")
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	done := make(chan error, 1)
	go func() {
		f, err := openRepairOutput(root, "report.json")
		if err == nil {
			err = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		fd, err := syscall.Open(name, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Close(fd) }()
		if err := <-done; err != nil {
			t.Errorf("opening the FIFO after connecting a writer: %v", err)
		}
		t.Fatal("opening a FIFO blocked waiting for a writer")
	}
}

func TestRunRepairedRejectsFIFO(t *testing.T) {
	workspace := t.TempDir()
	run := func(_ context.Context, j Job, _ func(Event)) error {
		return syscall.Mkfifo(filepath.Join(j.Workspace, j.OutputFile), 0o600)
	}
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{}, nil)
	var invalid *ValidationError
	if output != nil || !errors.As(err, &invalid) || invalid.Detail != "the file is not a regular file" {
		t.Fatalf("output = %s, err = %v", output, err)
	}
}
