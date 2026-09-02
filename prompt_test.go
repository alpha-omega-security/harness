package harness

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPromptBytes(t *testing.T) {
	t.Parallel()

	const validation = ` Validate ./report.json against ./schema.json before finishing.`
	job := Job{SkillName: "audit", OutputFile: "report.json"}
	tests := []struct {
		name string
		h    Harness
		want string
	}{
		{
			name: "claude",
			h:    ClaudeHarness{},
			want: `Use the "audit" skill on the repository cloned at ./src. Write your structured output to ./report.json as the skill specifies.` + validation,
		},
		{
			name: "codex",
			h:    CodexHarness{},
			want: `Follow the instructions in ./skills/audit/SKILL.md against the repository cloned at ./src. Write your structured output to ./report.json as the skill specifies.` + validation,
		},
		{
			name: "copilot",
			h:    CopilotHarness{},
			want: `Follow the instructions in ./.github/skills/audit/SKILL.md against the repository cloned at ./src. Write your structured output to ./report.json as the skill specifies.` + validation,
		},
		{
			name: "opencode",
			h:    OpencodeHarness{},
			want: `Follow the instructions in ./.opencode/skill/audit/SKILL.md against the repository cloned at ./src. Write your structured output to ./report.json as the skill specifies.` + validation,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.h.Prompt(job); got != test.want {
				t.Errorf("Prompt() = %q, want %q", got, test.want)
			}
		})
	}

	// A caller-supplied ValidationHint replaces the generic default so a
	// caller with its own validation endpoint can steer the agent there.
	custom := job
	custom.ValidationHint = "POST it to http://host/validate; don't install a schema validator."
	got := ClaudeHarness{}.Prompt(custom)
	if !strings.HasSuffix(got, " "+custom.ValidationHint) {
		t.Errorf("Prompt() with ValidationHint = %q, want suffix %q", got, custom.ValidationHint)
	}
	if strings.Contains(got, "before finishing") {
		t.Errorf("Prompt() with ValidationHint still contains the default: %q", got)
	}
}

func TestPromptSourceDirectory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		h    Harness
		job  Job
		want string
	}{
		{
			name: "workspace root",
			h:    ClaudeHarness{},
			job:  Job{SkillName: "audit", SrcDir: "."},
			want: `Use the "audit" skill on the repository cloned at the workspace root.`,
		},
		{
			name: "nested checkout",
			h:    CodexHarness{},
			job:  Job{SkillName: "audit", SrcDir: "checkouts/repo"},
			want: `Follow the instructions in ./skills/audit/SKILL.md against the repository cloned at ./checkouts/repo.`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.h.Prompt(test.job); got != test.want {
				t.Errorf("Prompt() = %q, want %q", got, test.want)
			}
		})
	}
}

type promptTransportHarness struct {
	viaArgs bool
	binary  string
	guide   string
}

func (h promptTransportHarness) Binary() string                   { return h.binary }
func (promptTransportHarness) Args(Job) []string                  { return nil }
func (promptTransportHarness) Prompt(Job) string                  { return "" }
func (promptTransportHarness) ParseStream(io.Reader, func(Event)) {}
func (promptTransportHarness) SkillDir(string, string) string     { return "" }
func (h promptTransportHarness) GuideFilename() string            { return h.guide }
func (h promptTransportHarness) SystemPromptViaArgs() bool        { return h.viaArgs }
func (promptTransportHarness) EgressHosts() []string              { return nil }
func (promptTransportHarness) Env(string) []string                { return nil }
func (promptTransportHarness) StateEnv(string) []string           { return nil }
func (promptTransportHarness) AccountErrorText(string) string     { return "" }
func (promptTransportHarness) DefaultModels() []ModelDefault      { return nil }

func TestWriteSystemPromptUsesBackendCapability(t *testing.T) {
	t.Parallel()

	t.Run("binary named claude can use a guide", func(t *testing.T) {
		t.Parallel()
		workspace := t.TempDir()
		h := promptTransportHarness{binary: "claude", guide: "CUSTOM.md"}
		if err := os.WriteFile(filepath.Join(workspace, "CUSTOM.md"), []byte("old content that must be truncated"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(workspace, "CUSTOM.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "Use the guide.\n" {
			t.Errorf("guide = %q", content)
		}
	})

	t.Run("unregistered variant can pass prompt in args", func(t *testing.T) {
		t.Parallel()
		workspace := t.TempDir()
		h := promptTransportHarness{
			viaArgs: true,
			binary:  "claude-variant",
			guide:   "SHOULD-NOT-EXIST.md",
		}
		if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use argv."}); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(filepath.Join(workspace, "SHOULD-NOT-EXIST.md"))
		if !os.IsNotExist(err) {
			t.Fatalf("guide stat error = %v, want not found", err)
		}
	})
}

func TestWriteSystemPromptPreservesGuidePermissions(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	guide := filepath.Join(workspace, "CUSTOM.md")
	if err := os.WriteFile(guide, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(guide)
	if err != nil {
		t.Fatal(err)
	}
	h := promptTransportHarness{guide: "CUSTOM.md"}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(guide)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("guide permissions = %v, want %v", after.Mode().Perm(), before.Mode().Perm())
	}
}

func TestWriteSystemPromptCreatesMissingGuideDirectory(t *testing.T) {
	t.Parallel()

	workspace := filepath.Join(t.TempDir(), "workspace")
	guide := filepath.Join(".github", "copilot-instructions.md")
	h := promptTransportHarness{guide: guide}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, guide))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Use the guide.\n" {
		t.Errorf("guide = %q", content)
	}
}

func TestWriteSystemPromptReplacesGuideSymlinkWithinWorkspace(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	target := filepath.Join(workspace, "shared-guide.md")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(target), filepath.Join(workspace, "CUSTOM.md")); err != nil {
		t.Fatal(err)
	}
	h := promptTransportHarness{guide: "CUSTOM.md"}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old" {
		t.Errorf("guide target = %q, want unchanged", content)
	}
	content, err = os.ReadFile(filepath.Join(workspace, "CUSTOM.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Use the guide.\n" {
		t.Errorf("guide = %q", content)
	}
}

func TestWriteSystemPromptReplacesExternalGuideSymlink(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	guide := filepath.Join(workspace, "CUSTOM.md")
	if err := os.Symlink(victim, guide); err != nil {
		t.Fatal(err)
	}
	h := promptTransportHarness{guide: "CUSTOM.md"}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Errorf("outside victim = %q, want unchanged", content)
	}
	info, err := os.Lstat(guide)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("guide mode = %v, want regular file", info.Mode())
	}
	content, err = os.ReadFile(guide)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Use the guide.\n" {
		t.Errorf("guide = %q", content)
	}
}

func TestWriteSystemPromptRejectsExternalGuideDirectorySymlink(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "copilot-instructions.md")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".github")); err != nil {
		t.Fatal(err)
	}
	h := promptTransportHarness{guide: filepath.Join(".github", "copilot-instructions.md")}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err == nil {
		t.Fatal("WriteSystemPrompt followed a guide directory symlink outside the workspace")
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Errorf("outside victim = %q, want unchanged", content)
	}
}

func TestWriteSystemPromptReplacesExternalGuideHardLink(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(parent, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	guide := filepath.Join(workspace, "CUSTOM.md")
	if err := os.Link(victim, guide); err != nil {
		t.Fatal(err)
	}
	h := promptTransportHarness{guide: "CUSTOM.md"}
	if err := WriteSystemPrompt(h, Job{Workspace: workspace, SystemPrompt: "Use the guide."}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Errorf("outside victim = %q, want unchanged", content)
	}
	content, err = os.ReadFile(guide)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "Use the guide.\n" {
		t.Errorf("guide = %q", content)
	}
}
