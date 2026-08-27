package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// CopilotHarness drives GitHub Copilot CLI in non-interactive prompt mode.
// Its arguments and JSONL mapping are based on Copilot CLI 1.0.75.
type CopilotHarness struct{}

func (CopilotHarness) Binary() string { return "copilot" }

// Args enables autopilot and tool use without interactive confirmation. The
// caller must isolate the process and workspace because --allow-all grants the
// CLI every tool it exposes.
func (CopilotHarness) Args(j Job) []string {
	maxTurns := j.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	args := []string{
		"-p", CopilotHarness{}.Prompt(j),
		"--output-format", "json",
		"--autopilot",
		"--max-autopilot-continues", strconv.Itoa(maxTurns),
		"--allow-all",
		"--no-ask-user",
		"--no-auto-update",
		"--no-color",
	}
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}
	if id := safeSessionID(j.ResumeSessionID); id != "" {
		args = append(args, "--resume="+id)
	}
	return args
}

func (CopilotHarness) Prompt(j Job) string {
	return explicitSkillPrompt(j, "./.github/skills/"+j.SkillName)
}

// ParseStream combines per-call assistant.usage and per-turn
// assistant.turn_end records into one result emitted after the stream ends.
// This gives callers the same single-run totals exposed by the other backends
// and prevents consumers that retain only the last result from losing usage.
func (CopilotHarness) ParseStream(r io.Reader, emit func(Event)) {
	total := Event{Kind: KindResult}
	sawResult := false
	scanJSONL(r, func(event Event) {
		if event.Kind != KindResult {
			emit(event)
			return
		}
		sawResult = true
		total.CostUSD += event.CostUSD
		total.Turns += event.Turns
		total.Usage.InputTokens += event.Usage.InputTokens
		total.Usage.OutputTokens += event.Usage.OutputTokens
		total.Usage.CacheReadTokens += event.Usage.CacheReadTokens
		total.Usage.CacheWriteTokens += event.Usage.CacheWriteTokens
		if event.Text != "" {
			total.Text = event.Text
		}
	}, parseCopilotLine)
	if sawResult {
		emit(total)
	}
}

// copilotLine contains the shared envelope fields used by prompt-mode JSONL.
// Unknown event types pass through as text so a CLI update remains visible.
type copilotLine struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	SessionID string          `json:"sessionId"`
	ExitCode  *int            `json:"exitCode"`
}

type copilotMessageData struct {
	Content       string `json:"content"`
	ReasoningText string `json:"reasoningText"`
}

type copilotReasoningData struct {
	Content string `json:"content"`
}

type copilotToolData struct {
	ToolName  string          `json:"toolName"`
	Arguments json.RawMessage `json:"arguments"`
}

type copilotUsageData struct {
	Model            string `json:"model"`
	InputTokens      int    `json:"inputTokens"`
	OutputTokens     int    `json:"outputTokens"`
	CacheReadTokens  int    `json:"cacheReadTokens"`
	CacheWriteTokens int    `json:"cacheWriteTokens"`
}

type copilotErrorData struct {
	Message string `json:"message"`
	Error   string `json:"error"`
}

func parseCopilotLine(raw []byte, emit func(Event)) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return
	}
	var event copilotLine
	if err := json.Unmarshal(raw, &event); err != nil {
		emit(Event{Kind: KindText, Text: line})
		return
	}
	switch event.Type {
	case "assistant.message":
		emitCopilotMessage(event.Data, emit)
	case "assistant.reasoning":
		var data copilotReasoningData
		if json.Unmarshal(event.Data, &data) == nil && data.Content != "" {
			emit(Event{Kind: KindThinking, Text: data.Content})
		}
	case "tool.execution_start":
		var data copilotToolData
		if json.Unmarshal(event.Data, &data) == nil {
			emit(Event{
				Kind: KindTool,
				Tool: data.ToolName,
				Text: summariseInput(data.ToolName, data.Arguments),
			})
		}
	case "assistant.usage":
		var data copilotUsageData
		if json.Unmarshal(event.Data, &data) == nil {
			usage := Usage{
				InputTokens:      data.InputTokens,
				OutputTokens:     data.OutputTokens,
				CacheReadTokens:  data.CacheReadTokens,
				CacheWriteTokens: data.CacheWriteTokens,
			}
			emit(Event{
				Kind:    KindResult,
				CostUSD: CostFromUsage(data.Model, usage),
				Usage:   usage,
			})
		}
	case "assistant.turn_end":
		// Copilot reports turns separately from token usage. ParseStream merges
		// both records into one result after all turns finish.
		emit(Event{Kind: KindResult, Turns: 1})
	case "result":
		if event.SessionID != "" {
			emit(Event{Kind: KindSession, SessionID: event.SessionID})
		}
		if event.ExitCode != nil && *event.ExitCode != 0 {
			emit(Event{Kind: KindError, Text: fmt.Sprintf("copilot exited with code %d", *event.ExitCode)})
		}
		// The final envelope guarantees a terminal result even if a failed or
		// empty run produced no usage and no completed turn.
		emit(Event{Kind: KindResult})
	case "abort", "session.error", "error":
		emitCopilotError(event.Data, line, emit)
	case "assistant.message_delta",
		"assistant.message_start",
		"assistant.reasoning_delta",
		"assistant.tool_call_delta",
		"assistant.turn_start",
		"assistant.idle",
		"model.call_start",
		"model.call_end",
		"session.mcp_server_status_changed",
		"session.mcp_servers_loaded",
		"session.skills_loaded",
		"session.tools_updated",
		"session.usage_checkpoint",
		"session.usage_info",
		"user.message":
	default:
		emit(Event{Kind: KindText, Text: line})
	}
}

func emitCopilotMessage(raw json.RawMessage, emit func(Event)) {
	var data copilotMessageData
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	if data.ReasoningText != "" {
		emit(Event{Kind: KindThinking, Text: data.ReasoningText})
	}
	if data.Content != "" {
		emit(Event{Kind: KindText, Text: data.Content})
	}
}

func emitCopilotError(raw json.RawMessage, fallback string, emit func(Event)) {
	var data copilotErrorData
	if json.Unmarshal(raw, &data) == nil {
		if data.Message != "" {
			emit(Event{Kind: KindError, Text: data.Message})
			return
		}
		if data.Error != "" {
			emit(Event{Kind: KindError, Text: data.Error})
			return
		}
	}
	emit(Event{Kind: KindError, Text: fallback})
}

func (CopilotHarness) SkillDir(workspace, name string) string {
	return filepath.Join(workspace, ".github", "skills", name)
}

func (CopilotHarness) GuideFilename() string {
	return filepath.Join(".github", "copilot-instructions.md")
}

func (CopilotHarness) SystemPromptViaArgs() bool { return false }

func (CopilotHarness) EgressHosts() []string {
	// GitHub hosts authentication and MCP traffic; githubcopilot.com serves
	// Copilot's model and session APIs.
	return []string{
		"github.com",
		"api.github.com",
		"api.mcp.github.com",
		"*.githubcopilot.com",
	}
}

func (CopilotHarness) Env(baseURL string) []string {
	env := []string{
		"COPILOT_AUTO_UPDATE=false",
		"COPILOT_OTEL_ENABLED=false",
		"NO_COLOR=1",
	}
	env = append(env, passthroughEnv("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")...)
	if baseURL != "" {
		env = append(env, "COPILOT_PROVIDER_BASE_URL="+baseURL)
		env = append(env, passthroughEnv(copilotBYOKEnv...)...)
	}
	return env
}

// copilotBYOKEnv are Copilot's bring-your-own-key provider settings, passed
// through as bare keys so container and remote runners can inject their values
// without placing secrets in argv.
var copilotBYOKEnv = []string{
	"COPILOT_MODEL",
	"COPILOT_PROVIDER_API_KEY",
	"COPILOT_PROVIDER_BEARER_TOKEN",
	"COPILOT_PROVIDER_TYPE",
	"COPILOT_PROVIDER_WIRE_API",
	"COPILOT_PROVIDER_TRANSPORT",
	"COPILOT_PROVIDER_AZURE_API_VERSION",
	"COPILOT_PROVIDER_MODEL_ID",
	"COPILOT_PROVIDER_WIRE_MODEL",
	"COPILOT_PROVIDER_MAX_PROMPT_TOKENS",
	"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS",
	"COPILOT_PROVIDER_HEADERS",
}

func (CopilotHarness) StateEnv(dir string) []string {
	return []string{"COPILOT_HOME=" + dir}
}

func (CopilotHarness) DefaultModels() []ModelDefault {
	// These IDs are accepted by Copilot CLI 1.0.75. Dotted Anthropic IDs are
	// distinct from Claude Code's hyphenated provider IDs.
	return []ModelDefault{
		{Name: "Claude Sonnet 4.6", ID: "claude-sonnet-4.6", Tier: "high"},
		{Name: "Claude Haiku 4.5", ID: "claude-haiku-4.5", Tier: "mid"},
		{Name: "Claude Opus 4.6", ID: "claude-opus-4.6", Tier: "max"},
		{Name: "GPT-5.3 Codex", ID: "gpt-5.3-codex"},
		{Name: "GPT-5.4", ID: "gpt-5.4"},
	}
}

// copilotAccountPhrases cover authentication, entitlement, and request-limit
// failures where an immediate retry is unlikely to help.
var copilotAccountPhrases = []string{
	"rate limit",
	"too many requests",
	"quota",
	"not entitled",
	"copilot access",
	"authentication failed",
	"unauthorized",
	"forbidden",
	"token expired",
	"429",
}

func (CopilotHarness) AccountErrorText(s string) string {
	return matchAccountPhrase(s, copilotAccountPhrases)
}
