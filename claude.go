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
		"--include-partial-messages",
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
	state := newClaudeStreamState()
	scanJSONL(r, emit, state.parseLine)
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
		{Name: "Opus 4.6", ID: "claude-opus-4-6", Tier: modelTierHigh},
		{Name: "Opus 4.7", ID: "claude-opus-4-7"},
		{Name: "Opus 4.8", ID: "claude-opus-4-8"},
		{Name: "Opus 5.0", ID: modelClaudeOpus5ID, Tier: modelTierMax},
		{Name: "Sonnet 4.6", ID: "claude-sonnet-4-6", Tier: modelTierMid},
		{Name: "Sonnet 5.0", ID: modelClaudeSonnet5ID},
		{Name: "Fable 5", ID: "claude-fable-5[1m]"},
		{Name: "Fable 5.1", ID: "claude-fable-5-1[1m]"},
	}
}

type claudeLine struct {
	Type          string             `json:"type"`
	Subtype       string             `json:"subtype"`
	SessionID     string             `json:"session_id"`
	Message       *claudeMessage     `json:"message"`
	Result        json.RawMessage    `json:"result"`
	CostUSD       *float64           `json:"total_cost_usd"`
	NumTurns      *int               `json:"num_turns"`
	Usage         *Usage             `json:"usage"`
	Error         json.RawMessage    `json:"error"`
	RateLimitInfo *RateLimitInfo     `json:"rate_limit_info"`
	Event         *claudeStreamEvent `json:"event"`
	APIMessageID  string             `json:"api_message_id"`
}

// claudeStreamEvent is the inner event of a stream_event line, emitted with
// --include-partial-messages.
type claudeStreamEvent struct {
	Type    string         `json:"type"`
	Message *claudeMessage `json:"message"`
	Usage   *claudeUsage   `json:"usage"`
}

// claudeUsage adds the cache-write split to Usage. Claude Code writes one-hour
// cache entries, which bill at a higher rate than the five-minute writes the
// pricing table assumes.
type claudeUsage struct {
	Usage
	CacheCreation *struct {
		OneHourTokens int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u *claudeUsage) oneHourWrites() int {
	if u.CacheCreation == nil {
		return 0
	}
	return u.CacheCreation.OneHourTokens
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   *claudeUsage    `json:"usage"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type     string          `json:"type"`
	Thinking string          `json:"thinking"`
	Text     string          `json:"text"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

// claudeStreamState tracks the highest usage already reported per API message.
// One message arrives as several assistant lines that repeat the same usage
// snapshot plus stream_event lines that refine it, so usage events carry only
// the growth since the last report.
type claudeStreamState struct {
	reported map[string]claudeReported
	models   map[string]string
	// current is the main-thread message the latest message_start opened.
	// Main-thread stream events are sequential and subagents emit none, so a
	// message_delta without api_message_id belongs to it.
	current string
}

// claudeReported is the highest usage already reported for one API message.
type claudeReported struct {
	usage         Usage
	oneHourWrites int
}

func newClaudeStreamState() *claudeStreamState {
	return &claudeStreamState{reported: map[string]claudeReported{}, models: map[string]string{}}
}

// reportUsage emits a usage event for the growth of id's usage over what was
// already reported. Repeated assistant lines and zero-usage synthetic messages
// therefore emit nothing.
func (state *claudeStreamState) reportUsage(id, model string, usage *claudeUsage, emit func(Event)) {
	if id == "" || usage == nil {
		return
	}
	prev := state.reported[id]
	delta := Usage{
		InputTokens:      max(usage.InputTokens-prev.usage.InputTokens, 0),
		OutputTokens:     max(usage.OutputTokens-prev.usage.OutputTokens, 0),
		CacheReadTokens:  max(usage.CacheReadTokens-prev.usage.CacheReadTokens, 0),
		CacheWriteTokens: max(usage.CacheWriteTokens-prev.usage.CacheWriteTokens, 0),
	}
	oneHourWrites := max(usage.oneHourWrites()-prev.oneHourWrites, 0)
	state.reported[id] = claudeReported{
		usage: Usage{
			InputTokens:      max(usage.InputTokens, prev.usage.InputTokens),
			OutputTokens:     max(usage.OutputTokens, prev.usage.OutputTokens),
			CacheReadTokens:  max(usage.CacheReadTokens, prev.usage.CacheReadTokens),
			CacheWriteTokens: max(usage.CacheWriteTokens, prev.usage.CacheWriteTokens),
		},
		oneHourWrites: max(usage.oneHourWrites(), prev.oneHourWrites),
	}
	if delta == (Usage{}) {
		return
	}
	// Claude's input_tokens excludes cache reads and writes while
	// CostFromUsage expects InputTokens to include every prompt token. Price
	// a copy so the emitted Usage keeps the convention of the result event.
	priced := delta
	priced.InputTokens += delta.CacheReadTokens + delta.CacheWriteTokens
	cost := CostFromUsage(model, priced) + oneHourCacheWriteSurcharge(model, oneHourWrites)
	emit(Event{Kind: KindUsage, Model: model, Usage: delta, CostUSD: cost})
}

// handleStreamEvent reports usage from partial-message events and stays silent
// for every other stream event so deltas never reach the log.
func (state *claudeStreamState) handleStreamEvent(message claudeLine, emit func(Event)) {
	if message.Event == nil {
		return
	}
	switch message.Event.Type {
	case "message_start":
		if m := message.Event.Message; m != nil {
			state.current = m.ID
			if m.Model != "" {
				state.models[m.ID] = m.Model
			}
			state.reportUsage(m.ID, state.models[m.ID], m.Usage, emit)
		}
	case "message_delta":
		// api_message_id is internal to the CLI and absent from older
		// producers and from events a plugin produced or rewrote.
		id := message.APIMessageID
		if id == "" {
			id = state.current
		}
		state.reportUsage(id, state.models[id], message.Event.Usage, emit)
	}
}

func (state *claudeStreamState) parseLine(raw []byte, emit func(Event)) {
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
		// Subagent calls emit no stream_event lines, so they only reach here
		// and their output tokens are a lower bound until the result event.
		if m := message.Message; m != nil {
			state.reportUsage(m.ID, m.Model, m.Usage, emit)
		}
	case "stream_event":
		state.handleStreamEvent(message, emit)
	case "result":
		emit(claudeResultEvent(message))
		if message.Subtype == "error_max_turns" {
			emit(Event{Kind: KindError, Text: "hit max turns"})
		}
	case wireTypeError:
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
		case wireTypeText:
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
