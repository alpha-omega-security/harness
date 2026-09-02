package harness

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// ClaudeHarness drives Claude Code in print mode. It is the default backend
// because the original caller used Claude before the shared interface existed.
type ClaudeHarness struct{}

func (ClaudeHarness) Binary() string { return "claude" }

// Args builds the claude -p invocation. An allowed-tools list uses
// acceptEdits only when the job has a legitimate output file; otherwise the
// default permission mode keeps the requested read-only boundary intact.
func (ClaudeHarness) Args(j Job) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
	}
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}
	if j.AllowedTools != "" {
		args = append(args,
			"--permission-mode", claudePermissionMode(j),
			"--allowedTools", j.AllowedTools+",Skill",
		)
	} else {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if j.SystemPrompt != "" {
		args = append(args, "--system-prompt", j.SystemPrompt)
	}
	if j.Effort != "" {
		args = append(args, "--effort", j.Effort)
	}
	if id := safeSessionID(j.ResumeSessionID); id != "" {
		args = append(args, "--resume", id)
	}
	maxTurns := j.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	return append(args, "--", safePrompt(ClaudeHarness{}.Prompt(j)))
}

// claudePermissionMode selects the mode for a job with an allowed-tools list.
// acceptEdits lets a skill write its report without prompting, but it also
// approves edits omitted from the allowlist. A job with no output file has
// nothing legitimate to write, so it stays in the default mode.
func claudePermissionMode(j Job) string {
	if j.OutputFile == "" {
		return "default"
	}
	return "acceptEdits"
}

func (ClaudeHarness) Prompt(j Job) string {
	switch {
	case j.ResumeSessionID != "" && j.ResumePrompt != "":
		return j.ResumePrompt
	case j.ResumeSessionID != "":
		return buildResumePrompt(j)
	case j.Prompt != "":
		return j.Prompt
	case j.SkillName != "":
		return buildSkillPrompt(j)
	default:
		return ""
	}
}

// ParseStream reads Claude's stream-json output. scanJSONL uses a buffered
// reader rather than Scanner so an oversized thinking or tool-result line
// cannot discard the later result event that carries usage and turn counts.
func (ClaudeHarness) ParseStream(r io.Reader, emit func(Event)) {
	scanJSONL(r, emit, parseClaudeLine)
}

func (ClaudeHarness) SkillDir(workspace, name string) string {
	return filepath.Join(workspace, ".claude", "skills", name)
}

func (ClaudeHarness) GuideFilename() string { return "CLAUDE.md" }

func (ClaudeHarness) SystemPromptViaArgs() bool { return true }

func (ClaudeHarness) EgressHosts() []string { return []string{"*.anthropic.com"} }

func (ClaudeHarness) Env(baseURL string) []string {
	env := []string{
		// Suppress telemetry, updates, bug reporting, and non-essential model
		// calls that a headless run does not need. An egress proxy may deny
		// them too, but disabling them at source keeps the stream quiet.
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"OTEL_SDK_DISABLED=true",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_BUG_COMMAND=1",
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_NON_ESSENTIAL_MODEL_CALLS=1",
	}
	// Credentials stay as bare passthrough keys so process runners can inject
	// them without putting secret values in argv.
	env = append(env, passthroughEnv("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN")...)
	if baseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+baseURL)
	}
	return env
}

func (ClaudeHarness) StateEnv(dir string) []string {
	return []string{"CLAUDE_CONFIG_DIR=" + dir}
}

func (ClaudeHarness) AccountErrorText(s string) string {
	return claudeAccountErrorText(s)
}

func (ClaudeHarness) DefaultModels() []ModelDefault {
	// Explicit tier tags avoid forcing callers to infer defaults from display
	// names. The first model remains the backend default.
	return []ModelDefault{
		{Name: "Opus 4.6", ID: "claude-opus-4-6", Tier: "high"},
		{Name: "Opus 4.7", ID: "claude-opus-4-7"},
		{Name: "Opus 4.8", ID: "claude-opus-4-8"},
		{Name: "Opus 5.0", ID: "claude-opus-5", Tier: "max"},
		{Name: "Sonnet 4.6", ID: "claude-sonnet-4-6", Tier: "mid"},
		{Name: "Sonnet 5.0", ID: "claude-sonnet-5"},
		{Name: "Fable 5", ID: "claude-fable-5[1m]"},
		{Name: "Fable 5.1", ID: "claude-fable-5-1[1m]"},
	}
}

type claudeLine struct {
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype"`
	SessionID     string          `json:"session_id"`
	Message       *claudeMessage  `json:"message"`
	Result        json.RawMessage `json:"result"`
	CostUSD       *float64        `json:"total_cost_usd"`
	NumTurns      *int            `json:"num_turns"`
	Usage         *Usage          `json:"usage"`
	Error         json.RawMessage `json:"error"`
	RateLimitInfo *RateLimitInfo  `json:"rate_limit_info"`
}

type claudeMessage struct {
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type     string          `json:"type"`
	Thinking string          `json:"thinking"`
	Text     string          `json:"text"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

func parseClaudeLine(raw []byte, emit func(Event)) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return
	}
	var message claudeLine
	if err := json.Unmarshal(raw, &message); err != nil {
		emit(Event{Kind: KindText, Text: line})
		return
	}
	switch message.Type {
	case "system":
		// The init event is the only reliable session identifier. It arrives
		// early enough to persist before a crash and proves that --resume
		// loaded the saved conversation. A failed resume emits no init, then
		// reports a new throwaway session id in its result, which must not be
		// treated as a successful resume.
		if message.Subtype == "init" && message.SessionID != "" {
			emit(Event{Kind: KindSession, SessionID: message.SessionID})
		}
	case "assistant":
		emitClaudeAssistant(message.Message, emit)
	case "result":
		emit(claudeResultEvent(message))
		if message.Subtype == "error_max_turns" {
			emit(Event{Kind: KindError, Text: "hit max turns"})
		}
	case "error":
		var text string
		if json.Unmarshal(message.Error, &text) != nil {
			text = string(message.Error)
		}
		emit(Event{Kind: KindError, Text: text})
	case "rate_limit_event":
		if message.RateLimitInfo != nil {
			emit(Event{Kind: KindRateLimit, RateLimit: message.RateLimitInfo})
		}
	case "user":
		// Tool results echo file contents and command output. The matching
		// tool-use event already carries a concise description.
	default:
		emit(Event{Kind: KindText, Text: line})
	}
}

func emitClaudeAssistant(message *claudeMessage, emit func(Event)) {
	if message == nil {
		return
	}
	for _, block := range message.Content {
		switch block.Type {
		case "thinking":
			if block.Thinking != "" {
				emit(Event{Kind: KindThinking, Text: block.Thinking})
			}
		case "text":
			if block.Text != "" {
				emit(Event{Kind: KindText, Text: block.Text})
			}
		case "tool_use":
			emit(Event{Kind: KindTool, Tool: block.Name, Text: summariseInput(block.Name, block.Input)})
		}
	}
}

func claudeResultEvent(message claudeLine) Event {
	event := Event{Kind: KindResult}
	if len(message.Result) > 0 {
		var text string
		if json.Unmarshal(message.Result, &text) == nil {
			event.Text = text
		} else {
			event.Text = string(message.Result)
		}
	}
	if message.CostUSD != nil {
		event.CostUSD = *message.CostUSD
	}
	if message.NumTurns != nil {
		event.Turns = *message.NumTurns
	}
	if message.Usage != nil {
		event.Usage = *message.Usage
	}
	return event
}
