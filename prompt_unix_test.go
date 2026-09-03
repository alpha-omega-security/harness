//go:build unix

package harness

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteSystemPromptPreservesGuidePermissionsAcrossUmask(t *testing.T) {
	existingWorkspace := t.TempDir()
	existingGuide := filepath.Join(existingWorkspace, "CUSTOM.md")
	if err := os.WriteFile(existingGuide, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existingGuide, 0o644); err != nil {
		t.Fatal(err)
	}
	newWorkspace := t.TempDir()
	h := promptTransportHarness{guide: "CUSTOM.md"}

	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)
	if err := WriteSystemPrompt(h, Job{Workspace: existingWorkspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSystemPrompt(h, Job{Workspace: newWorkspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}

	existingInfo, err := os.Stat(existingGuide)
	if err != nil {
		t.Fatal(err)
	}
	if existingInfo.Mode().Perm() != 0o644 {
		t.Errorf("existing guide permissions = %v, want %v", existingInfo.Mode().Perm(), os.FileMode(0o644))
	}
	newInfo, err := os.Stat(filepath.Join(newWorkspace, "CUSTOM.md"))
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.Mode().Perm() != 0o600 {
		t.Errorf("new guide permissions = %v, want %v", newInfo.Mode().Perm(), os.FileMode(0o600))
	}
}

func TestWriteSystemPromptReplacesGuideFIFO(t *testing.T) {
	workspace := t.TempDir()
	guide := filepath.Join(workspace, "CUSTOM.md")
	if err := syscall.Mkfifo(guide, 0o600); err != nil {
		t.Fatal(err)
	}
	h := promptTransportHarness{guide: "CUSTOM.md"}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(guide)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("guide mode = %v, want regular file", info.Mode())
	}
	content, err := os.ReadFile(guide)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Use the guide.\n" {
		t.Errorf("guide = %q", content)
	}
}
