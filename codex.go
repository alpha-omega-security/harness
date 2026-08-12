package harness

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
)

// CodexHarness drives Codex in headless exec mode. Codex reads AGENTS.md as
// project guidance and stores resumable threads under CODEX_HOME.
type CodexHarness struct{}

func (CodexHarness) Binary() string { return "codex" }

// Args builds codex exec argv. Headless Codex has no slash-style skill
// invocation, so Prompt names the staged SKILL.md. Resume inserts the thread
// id after "exec resume". Codex has no per-turn cap, so Job.MaxTurns is
// intentionally ignored.
func (CodexHarness) Args(j Job) []string {
	var args []string
	if j.BaseURL != "" {
		args = append(args, "-c", "openai_base_url="+codexConfigValue(j.BaseURL))
	}
	args = append(args,
		"exec",
		"--json",
		// Codex's Linux sandbox uses bubblewrap, which does not work in
		// restricted container runners without unprivileged user namespaces.
		// danger-full-access avoids that nested layer. The caller must provide
		// the filesystem, process, and network isolation described in README.
		"--sandbox", "danger-full-access",
		"--skip-git-repo-check",
	)
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}
	if id := safeSessionID(j.ResumeSessionID); id != "" {
		args = append(args, "resume", id)
	}
	return append(args, "--", safePrompt(CodexHarness{}.Prompt(j)))
}

// codexConfigValue quotes s for a codex -c key=<value> TOML fragment. A URL
// should never contain characters outside TOML's basic-string subset; if it
// does, an empty value is safer than one that could break out of the string
// and set a second key.
func codexConfigValue(s string) string {
	for _, r := range s {
		if r < 0x20 || r == '"' || r == '\\' || r == 0x7f {
			return `""`
		}
	}
	return `"` + s + `"`
}

func (CodexHarness) Prompt(j Job) string {
	return explicitSkillPrompt(j, "./skills/"+j.SkillName)
}

// ParseStream maps codex exec --json output onto backend-neutral events.
// Session announcements enable resume, item completions carry text and tools,
// and unknown or non-JSON lines pass through as text. Codex has no max-turns
// event because its exec command has no turn cap.
func (CodexHarness) ParseStream(r io.Reader, emit func(Event)) {
	scanJSONL(r, emit, parseCodexLine)
}

// codexLine contains the fields used from codex exec --json. These shapes were
// verified against Codex 0.147.0. Unknown event types remain visible as text
// instead of being silently dropped.
type codexLine struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	ThreadID  string          `json:"thread_id"`
	Text      string          `json:"text"`
	Message   string          `json:"message"`
	Tool      string          `json:"tool"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Error     string          `json:"error"`
	Item      *codexItem      `json:"item"`
	Usage     *codexUsage     `json:"usage"`
}

type codexItem struct {
	Type    string            `json:"type"`
	Text    string            `json:"text"`
	Message string            `json:"message"`
	Command string            `json:"command"`
	Tool    string            `json:"tool"`
	Name    string            `json:"name"`
	Input   json.RawMessage   `json:"input"`
	Query   string            `json:"query"`
	Changes []codexFileChange `json:"changes"`
}

type codexFileChange struct {
	Path string `json:"path"`
}

// codexUsage is the turn.completed token breakdown.
type codexUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

func parseCodexLine(raw []byte, emit func(Event)) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return
	}
	var event codexLine
	if err := json.Unmarshal(raw, &event); err != nil {
		// Codex writes this line to stderr on every headless run even when
		// stdin is closed. Combined-output runners would otherwise show it in
		// every log.
		if strings.HasPrefix(line, "Reading additional input from stdin") {
			return
		}
		emit(Event{Kind: KindText, Text: line})
		return
	}
	switch {
	case isCodexSessionEvent(event) && (event.SessionID != "" || event.ThreadID != ""):
		id := event.SessionID
		if id == "" {
			id = event.ThreadID
		}
		emit(Event{Kind: KindSession, SessionID: id})
	case event.Type == "turn.started":
		// This marker has no payload; completed items carry the content.
	case event.Type == "turn.completed":
		var usage Usage
		if event.Usage != nil {
			usage = Usage{
				InputTokens:     event.Usage.InputTokens,
				OutputTokens:    event.Usage.OutputTokens,
				CacheReadTokens: event.Usage.CachedInputTokens,
			}
		}
		// Codex emits one turn.completed event for the prompt sent by this
		// invocation. It supplies tokens but no price, so callers can use
		// CostFromUsage when they need a list-price estimate.
		emit(Event{Kind: KindResult, Usage: usage, Turns: 1})
	case event.Type == "item.started":
		// item.completed repeats the identifying fields and adds the result.
		// Emitting both would display each command twice.
	case event.Item != nil && event.Item.Type == "todo_list":
		// Codex emits todo-list snapshots on item.updated and item.completed.
		// They are internal progress state rather than agent output, and each
		// update repeats the full list, so do not surface them as text.
	case event.Item != nil && event.Item.Type == "error":
		emit(Event{Kind: KindError, Text: event.Item.Message})
	case event.Item != nil && event.Item.Text != "":
		emit(Event{Kind: KindText, Text: event.Item.Text})
	case event.Item != nil && isCodexToolItem(event.Item.Type):
		name := codexToolName(event.Item)
		emit(Event{Kind: KindTool, Tool: name, Text: codexToolText(event.Item)})
	case event.Type == "tool" || event.Tool != "":
		name := event.Tool
		if name == "" {
			name = event.Name
		}
		emit(Event{Kind: KindTool, Tool: name, Text: summariseInput(name, event.Input)})
	case event.Error != "":
		emit(Event{Kind: KindError, Text: event.Error})
	case event.Text != "":
		emit(Event{Kind: KindText, Text: event.Text})
	case event.Message != "":
		emit(Event{Kind: KindText, Text: event.Message})
	default:
		emit(Event{Kind: KindText, Text: line})
	}
}

func isCodexSessionEvent(event codexLine) bool {
	switch event.Type {
	case "thread.started", "session.created", "init":
		return true
	default:
		return false
	}
}

func isCodexToolItem(itemType string) bool {
	return strings.Contains(itemType, "command") || strings.Contains(itemType, "tool") ||
		itemType == "web_search" || itemType == "file_change"
}

func codexToolName(item *codexItem) string {
	for _, name := range []string{item.Tool, item.Name} {
		if name != "" {
			return name
		}
	}
	if strings.Contains(item.Type, "command") {
		return "command"
	}
	return item.Type
}

func codexToolText(item *codexItem) string {
	if item.Command != "" {
		return item.Command
	}
	if item.Query != "" {
		return item.Query
	}
	if len(item.Changes) > 0 {
		paths := make([]string, 0, len(item.Changes))
		for _, change := range item.Changes {
			paths = append(paths, change.Path)
		}
		return strings.Join(paths, ", ")
	}
	return summariseInput(codexToolName(item), item.Input)
}

func (CodexHarness) SkillDir(workspace, name string) string {
	return filepath.Join(workspace, "skills", name)
}

func (CodexHarness) GuideFilename() string { return "AGENTS.md" }

func (CodexHarness) SystemPromptViaArgs() bool { return false }

func (CodexHarness) EgressHosts() []string {
	// api.openai.com serves model requests. auth0.openai.com and chatgpt.com
	// support the ChatGPT login flow used without an API key.
	return []string{"api.openai.com", "auth0.openai.com", "chatgpt.com"}
}

func (CodexHarness) Env(_ string) []string {
	env := []string{
		// Disable Codex's telemetry exporters at source so an egress proxy does
		// not have to reject and log the same requests for every run.
		"RUST_LOG=error,opentelemetry_sdk=off,opentelemetry_otlp=off",
		"OMO_CODEX_SEND_ANONYMOUS_TELEMETRY=0",
		"OMO_CODEX_DISABLE_POSTHOG=1",
	}
	return append(env, passthroughEnv("CODEX_API_KEY")...)
}

func (CodexHarness) StateEnv(dir string) []string {
	return []string{"CODEX_HOME=" + dir}
}

func (CodexHarness) DefaultModels() []ModelDefault {
	// IDs mirror Codex's built-in model catalog. The Codex-tuned model comes
	// first because that is the CLI's default when --model is absent.
	return []ModelDefault{
		{Name: "GPT-5.3 Codex", ID: "gpt-5.3-codex", Tier: "high"},
		{Name: "GPT-5.4 mini", ID: "gpt-5.4-mini", Tier: "mid"},
		{Name: "GPT-5.4", ID: "gpt-5.4"},
		{Name: "GPT-5.5", ID: "gpt-5.5", Tier: "max"},
		{Name: "GPT-5.2", ID: "gpt-5.2"},
	}
}

// codexAccountPhrases are provider account failures that an immediate retry
// cannot fix.
var codexAccountPhrases = []string{
	"rate_limit",
	"rate limit",
	"too many requests",
	"429",
	"insufficient_quota",
	"quota exceeded",
	"invalid_api_key",
	"incorrect api key",
	"account is not active",
}

func (CodexHarness) AccountErrorText(s string) string {
	return matchAccountPhrase(s, codexAccountPhrases)
}
