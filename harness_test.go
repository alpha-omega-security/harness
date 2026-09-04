package harness

import (
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSafePromptAndSessionID(t *testing.T) {
	if got := safePrompt("--dangerously-skip-permissions"); got[0] != ' ' {
		t.Errorf("safePrompt on flag-shaped prompt = %q, want leading space", got)
	}
	if got := safePrompt("hello"); got != "hello" {
		t.Errorf("safePrompt(plain) = %q", got)
	}
	if got := safeSessionID("--flag"); got != "" {
		t.Errorf("safeSessionID on flag-shaped id = %q, want empty", got)
	}
	if got := safeSessionID("abc-123"); got != "abc-123" {
		t.Errorf("safeSessionID(uuid-like) = %q", got)
	}
}

// TestArgsGuardsTrailingPrompt pins that a Job.Prompt beginning with `-`
// cannot become a flag on any backend that appends the prompt as a trailing
// positional. Copilot passes it as -p <value> so is not affected either way.
func TestArgsGuardsTrailingPrompt(t *testing.T) {
	j := Job{Workspace: "/w", Prompt: "--dangerously-skip-permissions"}
	for _, name := range []string{"claude", "codex", "opencode"} {
		h, _ := ByName(name)
		args := h.Args(j)
		last := args[len(args)-1]
		if strings.HasPrefix(last, "-") {
			t.Errorf("%s: trailing arg %q is flag-shaped", name, last)
		}
		// The value that reaches the CLI must still contain the user's text.
		if !strings.Contains(last, "dangerously-skip-permissions") {
			t.Errorf("%s: prompt content lost: %q", name, last)
		}
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	if Names() != "claude, codex, copilot, opencode" {
		t.Fatalf("Names() = %q", Names())
	}
	for _, name := range strings.Split(Names(), ", ") {
		h, err := ByName(name)
		if err != nil {
			t.Fatalf("ByName(%q): %v", name, err)
		}
		if Name(h) != name {
			t.Errorf("Name(ByName(%q)) = %q", name, Name(h))
		}
	}
	h, err := ByName("")
	if err != nil {
		t.Fatal(err)
	}
	if Name(h) != "claude" {
		t.Errorf("empty backend selected %q", Name(h))
	}
	if _, err := ByName("missing"); err == nil {
		t.Fatal("ByName accepted an unknown backend")
	}
}

func TestBackendPathsAndState(t *testing.T) {
	t.Parallel()

	workspace := filepath.Join("tmp", "work")
	tests := []struct {
		harness Harness
		skill   string
		guide   string
		state   []string
	}{
		{
			harness: ClaudeHarness{},
			skill:   filepath.Join(workspace, ".claude", "skills", "audit"),
			guide:   "CLAUDE.md",
			state:   []string{"CLAUDE_CONFIG_DIR=/state"},
		},
		{
			harness: CodexHarness{},
			skill:   filepath.Join(workspace, "skills", "audit"),
			guide:   "AGENTS.md",
			state:   []string{"CODEX_HOME=/state"},
		},
		{
			harness: OpencodeHarness{},
			skill:   filepath.Join(workspace, ".opencode", "skill", "audit"),
			guide:   "AGENTS.md",
			state: []string{
				"OPENCODE_CONFIG_DIR=/state",
				"OPENCODE_DB=/state/opencode.db",
			},
		},
		{
			harness: CopilotHarness{},
			skill:   filepath.Join(workspace, ".github", "skills", "audit"),
			guide:   filepath.Join(".github", "copilot-instructions.md"),
			state:   []string{"COPILOT_HOME=/state"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(Name(test.harness), func(t *testing.T) {
			t.Parallel()
			if got := test.harness.SkillDir(workspace, "audit"); got != test.skill {
				t.Errorf("SkillDir() = %q, want %q", got, test.skill)
			}
			if got := test.harness.GuideFilename(); got != test.guide {
				t.Errorf("GuideFilename() = %q, want %q", got, test.guide)
			}
			if got := test.harness.StateEnv("/state"); !reflect.DeepEqual(got, test.state) {
				t.Errorf("StateEnv() = %v, want %v", got, test.state)
			}
		})
	}
}

func TestClaudeArgs(t *testing.T) {
	t.Parallel()

	job := Job{
		SkillName:       "audit",
		SystemPrompt:    "Be exact.",
		Model:           "claude-sonnet-4-6",
		Effort:          "high",
		MaxTurns:        12,
		OutputFile:      "report.json",
		AllowedTools:    "Read,Grep",
		ResumeSessionID: "session-1",
		ResumePrompt:    "Repair the report.",
	}
	args := ClaudeHarness{}.Args(job)
	for _, want := range []string{
		"--model", "claude-sonnet-4-6",
		"--permission-mode", "acceptEdits",
		"--allowedTools", "Read,Grep,Skill",
		"--system-prompt", "Be exact.",
		"--effort", "high",
		"--resume", "session-1",
		"--max-turns", "12",
		"Repair the report.",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("Args() missing %q: %v", want, args)
		}
	}
}

func TestPrompts(t *testing.T) {
	t.Parallel()

	job := Job{SkillName: "audit", OutputFile: "report.json"}
	for _, h := range []Harness{ClaudeHarness{}, CodexHarness{}, OpencodeHarness{}, CopilotHarness{}} {
		prompt := h.Prompt(job)
		if !strings.Contains(prompt, "audit") ||
			!strings.Contains(prompt, "report.json") ||
			!strings.Contains(prompt, "schema.json") {
			t.Errorf("%s prompt missing job details: %q", Name(h), prompt)
		}
	}
}

func TestDefaultModelsArePriced(t *testing.T) {
	t.Parallel()

	for _, h := range []Harness{ClaudeHarness{}, CodexHarness{}, OpencodeHarness{}, CopilotHarness{}} {
		for _, model := range h.DefaultModels() {
			usage := Usage{InputTokens: 1, OutputTokens: 1}
			if CostFromUsage(model.ID, usage) == 0 {
				t.Errorf("%s default model %q has no price", Name(h), model.ID)
			}
		}
	}
}

type unregisteredHarness struct{}

func (unregisteredHarness) Binary() string                     { return "custom" }
func (unregisteredHarness) Args(Job) []string                  { return nil }
func (unregisteredHarness) Prompt(Job) string                  { return "" }
func (unregisteredHarness) ParseStream(io.Reader, func(Event)) {}
func (unregisteredHarness) SkillDir(string, string) string     { return "" }
func (unregisteredHarness) GuideFilename() string              { return "" }
func (unregisteredHarness) SystemPromptViaArgs() bool          { return false }
func (unregisteredHarness) EgressHosts() []string              { return nil }
func (unregisteredHarness) Env(string) []string                { return nil }
func (unregisteredHarness) StateEnv(string) []string           { return nil }
func (unregisteredHarness) AccountErrorText(string) string     { return "" }
func (unregisteredHarness) DefaultModels() []ModelDefault      { return nil }

func TestNameFallsBackToBinary(t *testing.T) {
	t.Parallel()
	if got := Name(unregisteredHarness{}); got != "custom" {
		t.Errorf("Name() = %q", got)
	}
}
