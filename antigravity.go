package harness

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
)

// AntigravityHarness drives Google's Antigravity CLI (agy) in print mode
// against the Gemini API.
type AntigravityHarness struct{}

func (AntigravityHarness) Binary() string { return "agy" }

// antigravitySettings selects the Gemini API key provider. agy reads
// modelProvider only from settings.json, and --settings applies it to this run
// without writing the file.
const antigravitySettings = `{"modelProvider":"gemini"}`

// Args builds agy print-mode argv. The prompt is attached to --print because
// agy refuses a detached -p value that names one of its own flags. agy has no
// turn cap, so Job.MaxTurns is ignored.
func (AntigravityHarness) Args(j Job) []string {
	args := []string{
		"--print=" + AntigravityHarness{}.Prompt(j),
		flagOutputFormat, "stream-json",
		"--dangerously-skip-permissions",
		"--settings", antigravitySettings,
	}
	if j.Model != "" {
		args = append(args, "--model", j.Model)
	}
	if j.Effort != "" {
		args = append(args, "--effort", j.Effort)
	}
	if id := safeSessionID(j.ResumeSessionID); id != "" {
		args = append(args, "--conversation", id)
	}
	return args
}

func (AntigravityHarness) Prompt(j Job) string {
	return explicitSkillPrompt(j, "./.agents/skills/"+j.SkillName)
}

// ParseStream maps agy --output-format stream-json onto backend-neutral
// events. These shapes were verified against agy 1.5.15.
func (AntigravityHarness) ParseStream(r io.Reader, emit func(Event)) {
	state := antigravityStream{text: map[antigravityStepKey]string{}, tools: map[antigravityStepKey]bool{}}
	scanJSONL(r, emit, state.parseLine)
}

type antigravityLine struct {
	Event          string             `json:"event"`
	ConversationID string             `json:"conversation_id"`
	Init           *antigravityInit   `json:"init"`
	Step           *antigravityStep   `json:"step_update"`
	Result         *antigravityResult `json:"result"`
}

type antigravityInit struct {
	Model string `json:"model"`
}

type antigravityStep struct {
	Conversation string            `json:"conversation_id"`
	Index        int               `json:"step_index"`
	State        string            `json:"state"`
	Type         string            `json:"step_type"`
	ToolName     string            `json:"tool_name"`
	TextDelta    string            `json:"text_delta"`
	Usage        *antigravityUsage `json:"usage"`
	ToolInfo     *struct {
		Parameters json.RawMessage `json:"parameters"`
	} `json:"tool_info"`
}

type antigravityResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// antigravityUsage reports input_tokens without the cached part and
// output_tokens with thinking included.
type antigravityUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

// antigravityStepKey names a step across conversations, so a subagent's steps
// cannot merge with the parent's steps of the same index.
type antigravityStepKey struct {
	conversation string
	index        int
}

type antigravityStream struct {
	model string
	// text holds agent_response deltas until their step leaves ACTIVE.
	text  map[antigravityStepKey]string
	tools map[antigravityStepKey]bool
	// usage sums this invocation's steps. The result event's usage is
	// cumulative over a resumed conversation.
	usage Usage
}

func (s *antigravityStream) parseLine(raw []byte, emit func(Event)) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return
	}
	var event antigravityLine
	if err := json.Unmarshal(raw, &event); err != nil {
		// AGY_ERROR repeats the "error:" line above it as JSON, and the
		// result event carries the same message.
		if !strings.HasPrefix(line, "AGY_ERROR: ") {
			emit(Event{Kind: KindText, Text: line})
		}
		return
	}
	switch {
	case event.Event == wireTypeInit:
		if event.Init != nil {
			s.model = event.Init.Model
		}
		emit(Event{Kind: KindSession, SessionID: event.ConversationID})
	case event.Event == "step_update" && event.Step != nil:
		s.step(event.Step, emit)
	case event.Event == wireTypeResult && event.Result != nil:
		if event.Result.Status != "SUCCESS" {
			text := event.Result.Error
			if text == "" {
				text = event.Result.Status
			}
			emit(Event{Kind: KindError, Text: text})
		}
		emit(Event{Kind: KindResult, Turns: 1, Usage: s.usage, CostUSD: CostFromUsage(s.model, s.usage)})
	default:
		emit(Event{Kind: KindText, Text: line})
	}
}

func (s *antigravityStream) step(step *antigravityStep, emit func(Event)) {
	key := antigravityStepKey{step.Conversation, step.Index}
	if step.ToolName != "" && !s.tools[key] {
		s.tools[key] = true
		var params json.RawMessage
		if step.ToolInfo != nil {
			params = step.ToolInfo.Parameters
		}
		emit(Event{Kind: KindTool, Tool: step.ToolName, Text: antigravityToolText(params)})
	}
	s.text[key] += step.TextDelta
	if step.State == "ACTIVE" {
		return
	}
	if text := strings.TrimSpace(s.text[key]); text != "" {
		emit(Event{Kind: KindText, Text: text})
	}
	delete(s.text, key)
	if step.Usage != nil {
		// CostFromUsage expects InputTokens to include cache reads.
		usage := Usage{
			InputTokens:     step.Usage.InputTokens + step.Usage.CacheReadTokens,
			OutputTokens:    step.Usage.OutputTokens,
			CacheReadTokens: step.Usage.CacheReadTokens,
		}
		s.usage.InputTokens += usage.InputTokens
		s.usage.OutputTokens += usage.OutputTokens
		s.usage.CacheReadTokens += usage.CacheReadTokens
		emit(Event{Kind: KindUsage, Model: s.model, Usage: usage, CostUSD: requestCost(s.model, usage, usage.InputTokens)})
	}
}

// antigravityToolText shows the command of a run_command call and the raw
// parameters of any other tool.
func antigravityToolText(params json.RawMessage) string {
	var input struct {
		CommandLine string `json:"CommandLine"`
	}
	if json.Unmarshal(params, &input) == nil && input.CommandLine != "" {
		return input.CommandLine
	}
	if len(params) > 0 {
		return truncate(string(params))
	}
	return ""
}

func (AntigravityHarness) SkillDir(workspace, name string) string {
	return filepath.Join(workspace, ".agents", "skills", name)
}

func (AntigravityHarness) GuideFilename() string { return agentsGuideFilename }

func (AntigravityHarness) SystemPromptViaArgs() bool { return false }

func (AntigravityHarness) EgressHosts() []string {
	// agy also posts usage logs to play.googleapis.com. A run does not depend
	// on them, so the host stays off the list.
	return []string{"generativelanguage.googleapis.com"}
}

func (AntigravityHarness) Env(baseURL string) []string {
	env := passthroughEnv("GEMINI_API_KEY")
	if baseURL != "" {
		env = append(env, "GOOGLE_GEMINI_BASE_URL="+baseURL)
	}
	return env
}

// StateEnv moves HOME because agy keeps its conversations under
// $HOME/.gemini and has no variable to relocate them.
func (AntigravityHarness) StateEnv(dir string) []string {
	return []string{"HOME=" + dir}
}

func (AntigravityHarness) DefaultModels() []ModelDefault {
	// Gemini API model ids. agy maps Job.Effort onto the model's thinking
	// level, so no id carries an effort suffix.
	return []ModelDefault{
		{Name: "Gemini 4 Neon", ID: "gemini-4-neon", Tier: modelTierHigh},
		{Name: "Gemini 4 Carbon", ID: "gemini-4-carbon", Tier: modelTierMax},
		{Name: "Gemini 4 Hydrogen", ID: "gemini-4-hydrogen", Tier: modelTierMid},
	}
}

// antigravityAccountPhrases cover the Gemini API quota and key errors agy
// prints once its own retries give up. An AGY_ERROR line carries a hex
// error_id, so the status code is matched with its "Error " prefix.
var antigravityAccountPhrases = []string{
	"error 429",
	"resource_exhausted",
	accountPhraseQuota,
	"api_key_invalid",
	"api key not valid",
}

func (AntigravityHarness) AccountErrorText(s string) string {
	return matchAccountPhrase(s, antigravityAccountPhrases)
}
