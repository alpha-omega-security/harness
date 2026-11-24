package harness

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func antigravityEvents(t *testing.T, stream string) []Event {
	t.Helper()
	var events []Event
	AntigravityHarness{}.ParseStream(strings.NewReader(stream), func(e Event) { events = append(events, e) })
	return events
}

func antigravityFixture(t *testing.T, name string) []Event {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return antigravityEvents(t, string(data))
}

func eventsOfKind(events []Event, kind string) []Event {
	var matched []Event
	for _, e := range events {
		if e.Kind == kind {
			matched = append(matched, e)
		}
	}
	return matched
}

func TestAntigravityArgs(t *testing.T) {
	t.Parallel()

	args := AntigravityHarness{}.Args(Job{
		SkillName:       "audit",
		Model:           "gemini-4-neon",
		Effort:          "high",
		MaxTurns:        7,
		ResumeSessionID: "conv-1",
		ResumePrompt:    "Repair the report.",
	})
	if args[0] != "--print=Repair the report." {
		t.Errorf("args[0] = %q, want the resume prompt attached to --print", args[0])
	}
	for _, want := range []string{
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--settings", antigravitySettings,
		"--model", "gemini-4-neon",
		"--effort", "high",
		"--conversation", "conv-1",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("Args() missing %q: %v", want, args)
		}
	}
	if slices.Contains(args, "7") {
		t.Errorf("Args() passed MaxTurns, which agy has no flag for: %v", args)
	}

	bare := AntigravityHarness{}.Args(Job{Prompt: "Check it."})
	for _, flag := range []string{"--model", "--effort", "--conversation"} {
		if slices.Contains(bare, flag) {
			t.Errorf("Args() without a value set %s: %v", flag, bare)
		}
	}
}

func TestAntigravityArgsKeepFlagShapedValuesInert(t *testing.T) {
	t.Parallel()

	args := AntigravityHarness{}.Args(Job{Prompt: "--model x"})
	if args[0] != "--print=--model x" {
		t.Errorf("args[0] = %q, want the prompt attached to --print", args[0])
	}
	args = AntigravityHarness{}.Args(Job{ResumeSessionID: "--settings"})
	if slices.Contains(args, "--conversation") {
		t.Errorf("Args() resumed a flag-shaped session id: %v", args)
	}
}

func TestAntigravityEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "key")

	if got := (AntigravityHarness{}).Env(""); !slices.Equal(got, []string{"GEMINI_API_KEY"}) {
		t.Errorf("Env(\"\") = %v", got)
	}
	got := AntigravityHarness{}.Env("https://proxy.example")
	if !slices.Contains(got, "GOOGLE_GEMINI_BASE_URL=https://proxy.example") {
		t.Errorf("Env(baseURL) = %v", got)
	}

	t.Setenv("GEMINI_API_KEY", "")
	if got := (AntigravityHarness{}).Env(""); len(got) != 0 {
		t.Errorf("Env(\"\") without a key = %v", got)
	}
}

func TestAntigravityStreamFixture(t *testing.T) {
	t.Parallel()

	events := antigravityFixture(t, "antigravity-1.5.15.jsonl")
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{KindSession, KindText, KindUsage, KindTool, KindText, KindUsage, KindResult}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if events[0].SessionID != "f2d933d3-fae9-4d26-91a1-f7e21ec7483a" {
		t.Errorf("session = %q", events[0].SessionID)
	}
	if events[1].Text != "I will run a command." {
		t.Errorf("text = %q", events[1].Text)
	}
	if events[3].Tool != "run_command" || events[3].Text != "echo hello_mock" {
		t.Errorf("tool = %+v", events[3])
	}
	if events[2].Model != "gemini-4-neon" || events[2].CostUSD == 0 {
		t.Errorf("usage event = %+v, want a priced gemini-4-neon call", events[2])
	}
	result := events[6]
	wantUsage := Usage{InputTokens: 2700, OutputTokens: 72, CacheReadTokens: 1900}
	if result.Usage != wantUsage || result.Turns != 1 {
		t.Errorf("result = %+v, want usage %+v", result, wantUsage)
	}
	if result.CostUSD != CostFromUsage("gemini-4-neon", wantUsage) {
		t.Errorf("result cost = %v", result.CostUSD)
	}
}

func TestAntigravityResumedResultCountsOnlyThisInvocation(t *testing.T) {
	t.Parallel()

	results := eventsOfKind(antigravityFixture(t, "antigravity-1.5.15-resume.jsonl"), KindResult)
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	want := Usage{InputTokens: 1500, OutputTokens: 20, CacheReadTokens: 1100}
	if results[0].Usage != want {
		t.Errorf("usage = %+v, want %+v", results[0].Usage, want)
	}
}

func TestAntigravityStreamJoinsPartialText(t *testing.T) {
	t.Parallel()

	texts := eventsOfKind(antigravityFixture(t, "antigravity-1.5.15-streamed.jsonl"), KindText)
	if len(texts) != 1 || texts[0].Text != "The audit found no injection in the parser." {
		t.Errorf("texts = %+v", texts)
	}
}

func TestAntigravityStreamKeepsPlainLinesAndStepsApart(t *testing.T) {
	t.Parallel()

	events := antigravityEvents(t, `warning: conversation "0b8c1d2e-0000-4000-8000-000000000000" not found
{"event":"step_update","step_update":{"conversation_id":"parent","step_index":2,"state":"ACTIVE","step_type":"agent_response","text_delta":"parent"}}
{"event":"step_update","step_update":{"conversation_id":"child","step_index":2,"state":"DONE","step_type":"agent_response","text_delta":"child"}}
{"event":"step_update","step_update":{"conversation_id":"parent","step_index":2,"state":"DONE","step_type":"agent_response"}}
`)
	var texts []string
	for _, e := range eventsOfKind(events, KindText) {
		texts = append(texts, e.Text)
	}
	want := []string{`warning: conversation "0b8c1d2e-0000-4000-8000-000000000000" not found`, "child", "parent"}
	if !slices.Equal(texts, want) {
		t.Errorf("texts = %q, want %q", texts, want)
	}
}

func TestAntigravityStreamKeepsPartialTextOnError(t *testing.T) {
	t.Parallel()

	events := antigravityFixture(t, "antigravity-1.5.15-cut.jsonl")
	texts := eventsOfKind(events, KindText)
	if len(texts) == 0 || texts[0].Text != "I found an injection in" {
		t.Errorf("texts = %+v", texts)
	}
	if errs := eventsOfKind(events, KindError); len(errs) != 1 || !strings.Contains(errs[0].Text, "INVALID_ARGUMENT") {
		t.Errorf("errors = %+v", errs)
	}
}

func TestAntigravityQuotaErrorFixture(t *testing.T) {
	t.Parallel()

	events := antigravityFixture(t, "antigravity-1.5.15-429.jsonl")
	errs := eventsOfKind(events, KindError)
	if len(errs) != 1 || !strings.Contains(errs[0].Text, "RESOURCE_EXHAUSTED") {
		t.Fatalf("errors = %+v", errs)
	}
	for _, e := range eventsOfKind(events, KindText) {
		if strings.HasPrefix(e.Text, "AGY_ERROR") {
			t.Errorf("AGY_ERROR line surfaced as text: %q", e.Text)
		}
	}
	if len(eventsOfKind(events, KindResult)) != 1 {
		t.Errorf("a failed run must still report its result: %+v", events)
	}
	detail := AntigravityHarness{}.AccountErrorText(errs[0].Text)
	if detail == "" || !AccountErrorResumable(detail) {
		t.Errorf("quota error not classified as a resumable account error: %q", detail)
	}
}

func TestAntigravityAccountErrorText(t *testing.T) {
	t.Parallel()

	invalidKey := "agent executor error: generating and executing: Error 400, Message: API key not valid. Please pass a valid API key., Status: INVALID_ARGUMENT"
	detail := AntigravityHarness{}.AccountErrorText(invalidKey)
	if detail == "" || AccountErrorResumable(detail) {
		t.Errorf("invalid key = %q, want a non-resumable account error", detail)
	}
	if got := (AntigravityHarness{}).AccountErrorText("The command printed hello_mock. Done."); got != "" {
		t.Errorf("ordinary text classified as an account error: %q", got)
	}
	internal := `AGY_ERROR: {"short_error":"agent executor error: generating and executing: Error 500, Message: Internal error encountered., Status: INTERNAL, Details: []","status":"INTERNAL","error_code":500,"code_kind":"http","retryable":true,"error_id":"3865a842-0282-4228-b95b-73ca0f342906-9"}`
	if got := (AntigravityHarness{}).AccountErrorText(internal); got != "" {
		t.Errorf("server error with 429 inside its error_id classified as an account error: %q", got)
	}
}
