package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CopilotHarness drives GitHub Copilot CLI in non-interactive prompt mode.
// Its arguments and JSONL mapping target Copilot CLI 1.0.80 while retaining
// compatibility with the prompt-mode stream introduced in 1.0.75.
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
		"--no-remote-export",
	}
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}
	if j.Effort != "" {
		args = append(args, "--effort", j.Effort)
	}
	if id := safeSessionID(j.ResumeSessionID); id != "" {
		args = append(args, "--resume="+id)
	}
	return args
}

func (CopilotHarness) Prompt(j Job) string {
	return explicitSkillPrompt(j, "./.github/skills/"+j.SkillName)
}

// ParseStream combines per-call token usage and cumulative billing checkpoints
// into one result emitted after Copilot's final envelope. Sub-agent calls
// remain part of the usage total, but their nested conversation events stay out
// of the parent stream.
func (CopilotHarness) ParseStream(r io.Reader, emit func(Event)) {
	state := copilotStreamState{result: Event{Kind: KindResult}}
	scanJSONL(r, emit, state.parseLine)
	if !state.sawTerminal {
		return
	}
	state.result.CostUSD = state.estimatedCostUSD
	if state.sawCheckpoint {
		state.result.CostUSD = state.checkpointCostUSD
	}
	emit(state.result)
}

// copilotLine contains the shared envelope fields used by prompt-mode JSONL.
// Unknown event types pass through as text so a CLI update remains visible.
type copilotLine struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	AgentID   string          `json:"agentId"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	SessionID string          `json:"sessionId"`
	ExitCode  *int            `json:"exitCode"`
}

// copilotStreamState accumulates the terminal result across the JSONL stream.
type copilotStreamState struct {
	result            Event
	resultCallID      string
	resultChunks      []string
	messageReasoning  map[string]string
	estimatedCostUSD  float64
	checkpointCostUSD float64
	sawCheckpoint     bool
	sawTerminal       bool
}

type copilotMessageData struct {
	APICallID     string `json:"apiCallId"`
	ChunkCount    *int   `json:"chunkCount"`
	ChunkIndex    *int   `json:"chunkIndex"`
	Content       string `json:"content"`
	MessageID     string `json:"messageId"`
	ReasoningText string `json:"reasoningText"`
}

type copilotReasoningData struct {
	Content string `json:"content"`
}

type copilotIntentData struct {
	Intent string `json:"intent"`
}

type copilotToolData struct {
	ToolName  string          `json:"toolName"`
	Arguments json.RawMessage `json:"arguments"`
}

type copilotUsageData struct {
	Model            string                          `json:"model"`
	InputTokens      int                             `json:"inputTokens"`
	OutputTokens     int                             `json:"outputTokens"`
	CacheReadTokens  int                             `json:"cacheReadTokens"`
	CacheWriteTokens int                             `json:"cacheWriteTokens"`
	QuotaSnapshots   map[string]copilotQuotaSnapshot `json:"quotaSnapshots"`
}

type copilotQuotaSnapshot struct {
	HasQuota                         *bool    `json:"hasQuota"`
	EntitlementRequests              *float64 `json:"entitlementRequests"`
	IsUnlimitedEntitlement           bool     `json:"isUnlimitedEntitlement"`
	Overage                          float64  `json:"overage"`
	OverageAllowedWithExhaustedQuota bool     `json:"overageAllowedWithExhaustedQuota"`
	RemainingPercentage              *float64 `json:"remainingPercentage"`
	ResetDate                        string   `json:"resetDate"`
	UsageAllowedWithExhaustedQuota   bool     `json:"usageAllowedWithExhaustedQuota"`
	UsedRequests                     *float64 `json:"usedRequests"`
}

type copilotUsageCheckpointData struct {
	TotalNanoAIU *float64 `json:"totalNanoAiu"`
}

type copilotErrorData struct {
	Message    string          `json:"message"`
	Error      json.RawMessage `json:"error"`
	ErrorType  string          `json:"errorType"`
	ErrorCode  string          `json:"errorCode"`
	StatusCode *int            `json:"statusCode"`
}

type copilotAbortData struct {
	Reason string `json:"reason"`
}

type copilotInfoData struct {
	Message string `json:"message"`
}

type copilotModelCallFailureData struct {
	ErrorCode      string                          `json:"errorCode"`
	ErrorMessage   string                          `json:"errorMessage"`
	ErrorType      string                          `json:"errorType"`
	FailureKind    string                          `json:"failureKind"`
	Model          string                          `json:"model"`
	QuotaSnapshots map[string]copilotQuotaSnapshot `json:"quotaSnapshots"`
	StatusCode     *int                            `json:"statusCode"`
}

func (state *copilotStreamState) parseLine(raw []byte, emit func(Event)) {
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
		state.handleMessage(&event, emit)
	case "assistant.reasoning":
		state.handleReasoning(&event, emit)
	case "assistant.intent":
		emitCopilotIntent(&event, emit)
	case "tool.execution_start":
		emitCopilotTool(&event, emit)
	case "assistant.usage":
		var data copilotUsageData
		if json.Unmarshal(event.Data, &data) == nil {
			state.addUsage(data)
			emitCopilotRateLimits(data.QuotaSnapshots, emit)
		}
	case "session.usage_checkpoint":
		var data copilotUsageCheckpointData
		if json.Unmarshal(event.Data, &data) == nil && data.TotalNanoAIU != nil {
			// Checkpoints are cumulative for the session, so the latest value
			// replaces earlier values and any token-based estimate.
			state.checkpointCostUSD = copilotNanoAIUCostUSD(*data.TotalNanoAIU)
			state.sawCheckpoint = true
		}
	case "assistant.turn_end":
		if event.AgentID == "" {
			state.result.Turns++
		}
	case "result":
		state.sawTerminal = true
		if event.SessionID != "" {
			emit(Event{Kind: KindSession, SessionID: event.SessionID})
		}
		if event.ExitCode != nil && *event.ExitCode != 0 {
			emit(Event{Kind: KindError, Text: fmt.Sprintf("copilot exited with code %d", *event.ExitCode)})
		}
	case "model.call_failure":
		emitCopilotModelCallFailure(event.Data, emit)
	case "session.error", wireTypeError:
		emitCopilotError(event.Data, line, emit)
	case "abort":
		emitCopilotAbort(event.Data, line, emit)
	case "session.info", "session.warning":
		emitCopilotInfo(event.Data, emit)
	case "assistant.message_delta",
		"assistant.message_start",
		"assistant.reasoning_delta",
		"assistant.streaming_delta",
		"assistant.tool_call_delta",
		"assistant.turn_start",
		"assistant.idle",
		"model.call_start",
		"model.call_end",
		"mcp.prompts.list_changed",
		"mcp.resources.list_changed",
		"mcp.tools.list_changed",
		"session.idle",
		"session.mcp_server_status_changed",
		"session.mcp_servers_loaded",
		"session.skills_loaded",
		"session.task_complete",
		"session.tools_updated",
		"session.usage_info",
		"tool.execution_complete",
		"tool.execution_partial_result",
		"tool.execution_progress",
		"user.message":
	default:
		emit(Event{Kind: KindText, Text: line})
	}
}

func (state *copilotStreamState) handleMessage(event *copilotLine, emit func(Event)) {
	var data copilotMessageData
	if json.Unmarshal(event.Data, &data) != nil || event.AgentID != "" {
		return
	}
	if event.ID != "" && data.ReasoningText != "" {
		if state.messageReasoning == nil {
			state.messageReasoning = make(map[string]string)
		}
		state.messageReasoning[event.ID] = data.ReasoningText
	}
	if data.ReasoningText != "" {
		emit(Event{Kind: KindThinking, Text: data.ReasoningText})
	}
	if data.Content != "" {
		emit(Event{Kind: KindText, Text: data.Content})
		state.recordResultText(data)
	}
}

func (state *copilotStreamState) handleReasoning(event *copilotLine, emit func(Event)) {
	if event.AgentID != "" {
		return
	}
	var data copilotReasoningData
	if json.Unmarshal(event.Data, &data) != nil || data.Content == "" {
		return
	}
	if event.ParentID != "" {
		if reasoning, ok := state.messageReasoning[event.ParentID]; ok {
			delete(state.messageReasoning, event.ParentID)
			if data.Content == reasoning {
				return
			}
		}
	}
	emit(Event{Kind: KindThinking, Text: data.Content})
}

// recordResultText tracks the final assistant response. A model call can emit
// several complete message records around reasoning boundaries; those chunks
// are reassembled by index. A later API call replaces the result rather than
// appending to it so Text remains the final response, not a transcript.
func (state *copilotStreamState) recordResultText(data copilotMessageData) {
	callID := data.APICallID
	if callID == "" {
		callID = data.MessageID
	}
	if callID == "" ||
		data.ChunkCount == nil ||
		data.ChunkIndex == nil ||
		*data.ChunkCount <= 1 ||
		*data.ChunkIndex < 0 ||
		*data.ChunkIndex >= *data.ChunkCount {
		state.resultCallID = callID
		state.resultChunks = nil
		state.result.Text = data.Content
		return
	}
	if state.resultCallID != callID || len(state.resultChunks) != *data.ChunkCount {
		state.resultCallID = callID
		state.resultChunks = make([]string, *data.ChunkCount)
	}
	state.resultChunks[*data.ChunkIndex] = data.Content
	state.result.Text = strings.Join(state.resultChunks, "")
}

func (state *copilotStreamState) addUsage(data copilotUsageData) {
	usage := Usage{
		InputTokens:      data.InputTokens,
		OutputTokens:     data.OutputTokens,
		CacheReadTokens:  data.CacheReadTokens,
		CacheWriteTokens: data.CacheWriteTokens,
	}
	state.estimatedCostUSD += CostFromUsage(data.Model, usage)
	state.result.Usage.InputTokens += usage.InputTokens
	state.result.Usage.OutputTokens += usage.OutputTokens
	state.result.Usage.CacheReadTokens += usage.CacheReadTokens
	state.result.Usage.CacheWriteTokens += usage.CacheWriteTokens
}

const (
	nanoAIUPerAICredit = 1e9
	usdPerAICredit     = 0.01
)

// copilotNanoAIUCostUSD converts Copilot's billing unit to USD. One AI credit
// is $0.01 and nano-AIU uses the SI nano scale.
func copilotNanoAIUCostUSD(totalNanoAIU float64) float64 {
	return totalNanoAIU / nanoAIUPerAICredit * usdPerAICredit
}

// emitCopilotRateLimits maps exhausted Copilot quota snapshots to rate-limit
// events so callers can schedule a resume after ResetDate.
func emitCopilotRateLimits(snapshots map[string]copilotQuotaSnapshot, emit func(Event)) {
	keys := make([]string, 0, len(snapshots))
	for key, snapshot := range snapshots {
		if copilotQuotaExhausted(snapshot) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		snapshot := snapshots[key]
		status := "allowed"
		if !snapshot.UsageAllowedWithExhaustedQuota &&
			!snapshot.OverageAllowedWithExhaustedQuota {
			status = "rejected"
		}
		overageStatus := "rejected"
		if snapshot.OverageAllowedWithExhaustedQuota {
			overageStatus = "allowed"
		}
		info := &RateLimitInfo{
			Status:         status,
			OverageStatus:  overageStatus,
			IsUsingOverage: snapshot.Overage > 0 && snapshot.OverageAllowedWithExhaustedQuota,
			Type:           key,
		}
		if reset, err := time.Parse(time.RFC3339, snapshot.ResetDate); err == nil {
			info.ResetsAt = reset.Unix()
		}
		emit(Event{Kind: KindRateLimit, RateLimit: info})
	}
}

func copilotQuotaExhausted(snapshot copilotQuotaSnapshot) bool {
	// hasQuota:false indicates exhaustion only for a limited entitlement.
	if snapshot.IsUnlimitedEntitlement {
		return false
	}
	if snapshot.HasQuota != nil {
		return !*snapshot.HasQuota
	}
	if snapshot.RemainingPercentage != nil && *snapshot.RemainingPercentage <= 0 {
		return true
	}
	if snapshot.EntitlementRequests != nil && snapshot.UsedRequests != nil {
		return *snapshot.UsedRequests >= *snapshot.EntitlementRequests
	}
	return false
}

func emitCopilotIntent(event *copilotLine, emit func(Event)) {
	var data copilotIntentData
	if event.AgentID == "" && json.Unmarshal(event.Data, &data) == nil && data.Intent != "" {
		emit(Event{Kind: KindThinking, Text: data.Intent})
	}
}

func emitCopilotTool(event *copilotLine, emit func(Event)) {
	var data copilotToolData
	if event.AgentID != "" || json.Unmarshal(event.Data, &data) != nil || data.ToolName == "" {
		return
	}
	emit(Event{
		Kind: KindTool,
		Tool: data.ToolName,
		Text: summariseInput(data.ToolName, data.Arguments),
	})
}

func emitCopilotInfo(raw json.RawMessage, emit func(Event)) {
	var data copilotInfoData
	if json.Unmarshal(raw, &data) == nil && data.Message != "" {
		emit(Event{Kind: KindText, Text: data.Message})
	}
}

func emitCopilotAbort(raw json.RawMessage, fallback string, emit func(Event)) {
	var data copilotAbortData
	if json.Unmarshal(raw, &data) == nil && data.Reason != "" {
		emit(Event{Kind: KindError, Text: "copilot aborted: " + data.Reason})
		return
	}
	emit(Event{Kind: KindError, Text: fallback})
}

func emitCopilotModelCallFailure(raw json.RawMessage, emit func(Event)) {
	var data copilotModelCallFailureData
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	emitCopilotRateLimits(data.QuotaSnapshots, emit)
	text := data.ErrorMessage
	if text == "" {
		text = "model call failed"
	}
	emit(Event{
		Kind: KindError,
		Text: text + copilotErrorDetails(
			data.Model, data.FailureKind, data.ErrorType, data.ErrorCode, data.StatusCode,
		),
	})
}

func emitCopilotError(raw json.RawMessage, fallback string, emit func(Event)) {
	var data copilotErrorData
	if json.Unmarshal(raw, &data) == nil {
		text := data.Message
		if text == "" {
			text = copilotNestedErrorText(data.Error)
		}
		if text != "" {
			emit(Event{
				Kind: KindError,
				Text: text + copilotErrorDetails("", "", data.ErrorType, data.ErrorCode, data.StatusCode),
			})
			return
		}
	}
	emit(Event{Kind: KindError, Text: fallback})
}

func copilotNestedErrorText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var nested struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &nested) == nil {
		return nested.Message
	}
	return ""
}

func copilotErrorDetails(model, failureKind, errorType, errorCode string, statusCode *int) string {
	var details []string
	for _, detail := range []string{model, failureKind, errorType, errorCode} {
		if detail != "" {
			details = append(details, detail)
		}
	}
	if statusCode != nil {
		details = append(details, "status "+strconv.Itoa(*statusCode))
	}
	if len(details) == 0 {
		return ""
	}
	return " (" + strings.Join(details, ", ") + ")"
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
	// This is Copilot CLI 1.0.80's /model --list --json catalog. The reported
	// current model, GPT-5.6 Sol, is moved first because callers treat the first
	// entry as the default. Dotted Anthropic IDs are distinct from Claude Code's
	// hyphenated IDs.
	return []ModelDefault{
		{Name: "GPT-5.6 Sol", ID: "gpt-5.6-sol"},
		{Name: "Claude Sonnet 5", ID: modelClaudeSonnet5ID},
		{Name: "Claude Opus 5", ID: modelClaudeOpus5ID, Tier: modelTierMax},
		{Name: "Claude Opus 4.8", ID: "claude-opus-4.8"},
		{Name: "Claude Opus 4.7", ID: "claude-opus-4.7"},
		{Name: "Claude Sonnet 4.6", ID: "claude-sonnet-4.6", Tier: modelTierMid},
		{Name: "Claude Opus 4.6", ID: "claude-opus-4.6", Tier: modelTierHigh},
		{Name: "Claude Haiku 4.5", ID: "claude-haiku-4.5"},
		{Name: "GPT-5.6 Terra", ID: "gpt-5.6-terra"},
		{Name: "GPT-5.6 Luna", ID: "gpt-5.6-luna"},
		{Name: "GPT-5.5", ID: modelGPT55ID},
		{Name: "GPT-5.4", ID: modelGPT54ID},
		{Name: "GPT-5.4 mini", ID: modelGPT54MiniID},
		{Name: "GPT-5.3-Codex", ID: modelGPT53CodexID},
		{Name: "GPT-5 mini", ID: "gpt-5-mini"},
		{Name: "MAI-Code-1-Flash", ID: "mai-code-1-flash-picker"},
		{Name: "Gemini 3.7 Flash", ID: "gemini-3.7-flash"},
		{Name: "Gemini 3.6 Flash", ID: "gemini-3.6-flash"},
		{Name: "Gemini 3.5 Flash", ID: "gemini-3.5-flash"},
		{Name: "Gemini 3.1 Pro Preview", ID: "gemini-3.1-pro-preview"},
		{Name: "Grok 4.5", ID: "grok-4.5"},
		{Name: "Grok 4.6", ID: "grok-4.6"},
		{Name: "MAI-Code-1.1-Flash", ID: "mai-code-1.1-flash"},
	}
}

// copilotAccountPhrases cover authentication, entitlement, and request-limit
// failures where an immediate retry is unlikely to help.
var copilotAccountPhrases = []string{
	accountPhraseRateLimit,
	accountPhraseTooManyRequests,
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
