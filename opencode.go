package harness

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
)

// OpencodeHarness drives OpenCode in headless run mode. OpenCode is
// provider-neutral, so its environment and egress list cover Anthropic,
// OpenAI, and OpenCode's model registry by default.
type OpencodeHarness struct{}

func (OpencodeHarness) Binary() string { return "opencode" }

// Args builds opencode run argv. OpenCode discovers SKILL.md but does not
// invoke a named skill itself, so Prompt points at the staged file. --auto
// suppresses interactive permission prompts and --format json selects JSONL.
// The caller remains responsible for process isolation.
func (OpencodeHarness) Args(j Job) []string {
	args := []string{
		"run",
		"--format", "json",
		"--auto",
	}
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}
	if id := safeSessionID(j.ResumeSessionID); id != "" {
		args = append(args, "--session", id)
	}
	return append(args, "--", safePrompt(OpencodeHarness{}.Prompt(j)))
}

func (OpencodeHarness) Prompt(j Job) string {
	return explicitSkillPrompt(j, "./.opencode/skill/"+j.SkillName)
}

func (OpencodeHarness) ParseStream(r io.Reader, emit func(Event)) {
	scanJSONL(r, emit, parseOpencodeLine)
}

// opencodeLine is the subset of opencode run --format json used here. Each
// event has a type and sessionID, with its payload nested under part.
type opencodeLine struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      *opencodePart   `json:"part"`
	Error     json.RawMessage `json:"error"`
}

type opencodePart struct {
	Type  string            `json:"type"`
	Text  string            `json:"text"`
	Tool  string            `json:"tool"`
	Name  string            `json:"name"`
	State opencodeToolState `json:"state"`
	// step_finish carries per-step cost and tokens here rather than at the
	// event top level.
	Cost   float64         `json:"cost"`
	Tokens *opencodeTokens `json:"tokens"`
}

type opencodeToolState struct {
	Input json.RawMessage `json:"input"`
}

type opencodeTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

func parseOpencodeLine(raw []byte, emit func(Event)) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return
	}
	var event opencodeLine
	if err := json.Unmarshal(raw, &event); err != nil {
		emit(Event{Kind: KindText, Text: line})
		return
	}
	switch {
	case event.Type == "step_start" && event.SessionID != "":
		emit(Event{Kind: KindSession, SessionID: event.SessionID})
	case isOpencodeToolEvent(event):
		name := event.Part.Tool
		if name == "" {
			name = event.Part.Name
		}
		emit(Event{Kind: KindTool, Tool: name, Text: summariseInput(name, event.Part.State.Input)})
	case event.Type == "error" || len(event.Error) > 0:
		emit(Event{Kind: KindError, Text: opencodeErrorText(event.Error, line)})
	case isOpencodeReasoningEvent(event):
		emit(Event{Kind: KindThinking, Text: event.Part.Text})
	case isOpencodeTextEvent(event):
		emit(Event{Kind: KindText, Text: event.Part.Text})
	case event.Type == "step_finish" && event.Part != nil:
		emit(Event{
			Kind:    KindResult,
			CostUSD: event.Part.Cost,
			Turns:   1,
			Usage:   opencodeUsage(event.Part.Tokens),
		})
	case event.Type == "step_finish":
		// A marker with no part has no cost, usage, or text to record.
	default:
		// New event types stay visible until the parser gains a specific
		// mapping, which prevents CLI upgrades from silently dropping output.
		emit(Event{Kind: KindText, Text: line})
	}
}

func opencodeUsage(tokens *opencodeTokens) Usage {
	if tokens == nil {
		return Usage{}
	}
	// OpenCode reports reasoning separately. The shared Usage type has no
	// reasoning field, so include it in output for complete token totals.
	return Usage{
		InputTokens:      tokens.Input,
		OutputTokens:     tokens.Output + tokens.Reasoning,
		CacheReadTokens:  tokens.Cache.Read,
		CacheWriteTokens: tokens.Cache.Write,
	}
}

func isOpencodeToolEvent(event opencodeLine) bool {
	if event.Part == nil {
		return false
	}
	return event.Type == "tool" || event.Part.Type == "tool" ||
		event.Part.Tool != "" || event.Part.Name != ""
}

func isOpencodeReasoningEvent(event opencodeLine) bool {
	if event.Part == nil || event.Part.Text == "" {
		return false
	}
	return event.Type == "reasoning" || event.Part.Type == "reasoning"
}

func isOpencodeTextEvent(event opencodeLine) bool {
	if event.Part == nil || event.Part.Text == "" {
		return false
	}
	return event.Type == "text" || event.Part.Type == "text"
}

func opencodeErrorText(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var value struct {
		Message string `json:"message"`
		Name    string `json:"name"`
		Code    string `json:"code"`
		// Typed provider errors put the provider's message in data. That is
		// where useful rate-limit and quota wording normally appears.
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &value) == nil {
		for _, candidate := range []string{value.Data.Message, value.Message, value.Code, value.Name} {
			if candidate != "" {
				return candidate
			}
		}
	}
	return strings.TrimSpace(string(raw))
}

func (OpencodeHarness) SkillDir(workspace, name string) string {
	return filepath.Join(workspace, ".opencode", "skill", name)
}

func (OpencodeHarness) GuideFilename() string { return "AGENTS.md" }

func (OpencodeHarness) SystemPromptViaArgs() bool { return false }

func (OpencodeHarness) EgressHosts() []string {
	// Operators using another provider, such as Bedrock or Azure, must extend
	// the caller's allowlist with that provider's hosts.
	return []string{"models.dev", "api.openai.com", "*.anthropic.com"}
}

// Env ignores baseURL because OpenCode has no single provider endpoint. A
// caller can configure each provider through OPENCODE_CONFIG_CONTENT.
func (OpencodeHarness) Env(_ string) []string {
	// --auto grants tool permissions. OPENCODE_PERMISSION is deliberately
	// absent because OpenCode parses it as a JSON value and has no scalar
	// "allow all" setting.
	env := []string{
		"OPENCODE_DISABLE_AUTOUPDATE=true",
		"OPENCODE_DISABLE_MODELS_FETCH=true",
		"OPENCODE_DISABLE_SHARE=true",
		// OpenCode's server catch-all replaces uncaught defects with
		// "Unexpected server error. Check server logs for details." and writes
		// the cause via Effect.logError, which by default goes only to
		// <XDG_DATA_HOME>/opencode/log/opencode.log. OPENCODE_PRINT_LOGS=1
		// mirrors that logger to stderr so ParseStream (and the caller's scan
		// log) receive the underlying error as text lines. The default level is
		// Info, which would flood every successful run with startup and session
		// bookkeeping; restricting to error keeps only the failure causes.
		"OPENCODE_PRINT_LOGS=1",
		"OPENCODE_LOG_LEVEL=error",
	}
	// OpenCode reads provider credentials from its auth config or the
	// provider's environment. Pass through whichever form the caller set.
	return append(env, passthroughEnv(
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"OPENCODE_CONFIG_CONTENT",
		"OPENCODE_AUTH_CONTENT",
	)...)
}

func (OpencodeHarness) StateEnv(dir string) []string {
	return []string{
		"OPENCODE_CONFIG_DIR=" + dir,
		"OPENCODE_DB=" + filepath.Join(dir, "opencode.db"),
	}
}

func (OpencodeHarness) DefaultModels() []ModelDefault {
	// With ANTHROPIC_API_KEY and no extra provider config, OpenCode selects
	// Anthropic. The provider prefix is required by --model and is removed by
	// normalizeModelID for price lookup.
	claude := ClaudeHarness{}.DefaultModels()
	models := make([]ModelDefault, len(claude))
	for i, model := range claude {
		models[i] = ModelDefault{
			Name: model.Name,
			ID:   "anthropic/" + model.ID,
			Tier: model.Tier,
		}
	}
	return models
}

// opencodeAccountPhrases cover the common provider failures surfaced through
// OpenCode's nested error message.
var opencodeAccountPhrases = []string{
	"rate limit",
	"rate_limit",
	"too many requests",
	"429",
	"usage limit",
	"quota",
	"insufficient_quota",
	"invalid_api_key",
	"incorrect api key",
	"invalid x-api-key",
	"credit balance",
	"billing",
}

func (OpencodeHarness) AccountErrorText(s string) string {
	return matchAccountPhrase(s, opencodeAccountPhrases)
}
