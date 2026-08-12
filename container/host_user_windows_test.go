//go:build windows

package container

import (
	"slices"
	"testing"

	"github.com/alpha-omega-security/harness"
)

func TestRunnerArgsOmitsHostUserOnWindows(t *testing.T) {
	if got := hostUser(); got != "" {
		t.Fatalf("hostUser() = %q, want empty on Windows", got)
	}
	args := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if slices.Contains(args, "--user") {
		t.Errorf("unexpected --user on Windows: %v", args)
	}
}
