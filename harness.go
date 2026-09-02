package harness

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
)

const (
	DefaultMaxTurns = 30

	modelClaudeOpus5ID   = "claude-opus-5"
	modelClaudeSonnet5ID = "claude-sonnet-5"
	modelGPT53CodexID    = "gpt-5.3-codex"
	modelGPT54MiniID     = "gpt-5.4-mini"
	modelGPT54ID         = "gpt-5.4"
	modelGPT55ID         = "gpt-5.5"

	modelTierMid  = "mid"
	modelTierHigh = "high"
	modelTierMax  = "max"
)

// Job contains the resolved inputs for one CLI invocation.
type Job struct {
	// Workspace is the command's working directory. Paths passed to a CLI are
	// relative to it.
	Workspace string

	// SrcDir is the workspace-relative repository directory used in generated
	// prompts. It defaults to "src"; use "." when Workspace is the repository
	// root.
	SrcDir string

	// SkillName selects a staged SKILL.md directory. An empty value means no
	// staged skill.
	SkillName string

	// Prompt is the user turn for a fresh run. When it is empty and SkillName
	// is set, the harness builds an activation prompt.
	Prompt string

	// SystemPrompt supplies additional instructions. Claude receives it via
	// --system-prompt. Run writes it to the project guide file used by the
	// other backends.
	SystemPrompt string

	Model string
	// Effort is the backend-native reasoning effort accepted by Claude and
	// Copilot. An empty value leaves the backend default unchanged.
	Effort   string
	MaxTurns int

	// OutputFile is a workspace-relative path the skill should write. It is
	// empty for free-form runs.
	OutputFile string

	// ValidationHint is appended to generated prompts after the OutputFile
	// clause when OutputFile ends in .json. It lets a caller tell the agent
	// how to validate its output (an API endpoint, a staged validator) in
	// terms of the caller's own context. When empty, a generic instruction
	// to check against ./schema.json is used.
	ValidationHint string

	// AllowedTools is Claude's comma-separated tool allowlist. Other backends
	// leave tool restrictions to their caller's sandbox.
	AllowedTools string

	// BaseURL overrides the model API endpoint where the backend supports it.
	BaseURL string

	// ResumeSessionID continues a prior conversation. ResumePrompt is the
	// corrective turn used for the resumed invocation.
	ResumeSessionID string
	ResumePrompt    string
}

// Harness describes the CLI-specific parts of an agent invocation. It owns
// the binary, arguments, stream format, project guide, skill discovery,
// provider environment, and default models. Process placement and isolation
// remain the caller's responsibility.
type Harness interface {
	// Binary is the executable name expected on PATH.
	Binary() string
	// Args returns argv without the binary for one job.
	Args(Job) []string
	// Prompt returns the final user prompt passed by Args.
	Prompt(Job) string
	// ParseStream maps the backend's combined output onto Event values.
	ParseStream(io.Reader, func(Event))
	// SkillDir returns the directory where the backend discovers a staged
	// SKILL.md and its sibling files.
	SkillDir(workspace, name string) string
	// GuideFilename is the workspace-relative project instruction file loaded
	// by the backend.
	GuideFilename() string
	// SystemPromptViaArgs reports whether Args passes Job.SystemPrompt itself.
	// When false, WriteSystemPrompt writes it to GuideFilename instead.
	SystemPromptViaArgs() bool
	// EgressHosts lists the model and authentication hosts needed by the
	// backend, using "*.example.com" for wildcard suffixes.
	EgressHosts() []string
	// Env returns backend environment entries. A bare key asks a process
	// runner to pass through the caller's value.
	Env(baseURL string) []string
	// StateEnv points the backend at a caller-owned persistent state directory
	// so a later process can resume the same session.
	StateEnv(dir string) []string
	// AccountErrorText returns the matching provider account error, or an empty
	// string. Callers should consult it only after a non-zero process exit so
	// ordinary model text cannot pause retries.
	AccountErrorText(string) string
	// DefaultModels returns the built-in model picker entries. The first entry
	// is the backend default.
	DefaultModels() []ModelDefault
}

// ModelDefault is one model offered by a backend. Tier is "mid", "high",
// "max", or empty when the model is selectable but not a tier default.
type ModelDefault struct {
	Name string
	ID   string
	Tier string
}

// harnesses is the backend registry. The empty alias preserves Claude as the
// default when callers have no explicit selection.
var harnesses = map[string]Harness{
	"":         ClaudeHarness{},
	"claude":   ClaudeHarness{},
	"codex":    CodexHarness{},
	"copilot":  CopilotHarness{},
	"opencode": OpencodeHarness{},
}

// ByName resolves a backend name. The empty name selects Claude.
func ByName(name string) (Harness, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if h, ok := harnesses[name]; ok {
		return h, nil
	}
	return nil, fmt.Errorf("harness: unknown backend %q, must be one of %s", name, Names())
}

// Name returns the registered name of h. Comparing concrete types avoids an
// interface equality panic if a future implementation contains a slice or
// map. An unregistered implementation falls back to its binary name.
func Name(h Harness) string {
	if h == nil {
		return ""
	}
	typ := reflect.TypeOf(h)
	for name, registered := range harnesses {
		if name != "" && reflect.TypeOf(registered) == typ {
			return name
		}
	}
	return h.Binary()
}

// Names returns the registered backend names in lexical order, excluding the
// empty default alias.
func Names() string {
	names := make([]string, 0, len(harnesses)-1)
	for name := range harnesses {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// passthroughEnv returns each host environment key that is set. Bare entries
// preserve the process-runner convention of passing a secret through without
// embedding its value in argv.
// safePrompt returns p unchanged unless it begins with `-`, in which case a
// single leading space is prefixed so the backend CLI cannot parse it as a
// flag. Job.Prompt and Job.ResumePrompt land in positional argv on three of
// four backends; without this a prompt of `--dangerously-skip-permissions`
// (Claude) or `--full-auto` (Codex) would override the permission mode Args
// set. The space is whitespace the model ignores.
func safePrompt(p string) string {
	if strings.HasPrefix(p, "-") {
		return " " + p
	}
	return p
}

// safeSessionID returns id unless it begins with `-`, in which case it returns
// "" so the caller falls back to a fresh run instead of passing a flag-shaped
// value in positional argv. Session ids captured from a KindSession event are
// UUIDs, so this only trips on a corrupted or hostile store.
func safeSessionID(id string) string {
	if strings.HasPrefix(id, "-") {
		return ""
	}
	return id
}

func passthroughEnv(keys ...string) []string {
	var entries []string
	for _, key := range keys {
		if os.Getenv(key) != "" {
			entries = append(entries, key)
		}
	}
	return entries
}
