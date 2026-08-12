//go:build unix

package container

import (
	"fmt"
	"os"
	"testing"

	"github.com/alpha-omega-security/harness"
)

func TestRunnerArgsHostUserUnix(t *testing.T) {
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if got := hostUser(); got != want {
		t.Fatalf("hostUser() = %q, want %q", got, want)
	}
	args := Runner{}.args(stubHarness{}, harness.Job{Workspace: WorkMount}, "/abs/work")
	if !hasAdjacent(args, "--user", want) {
		t.Errorf("expected --user %s in %v", want, args)
	}
}
