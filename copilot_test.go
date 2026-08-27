package harness

import (
	"math"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestCopilotArgs(t *testing.T) {
	t.Parallel()

	args := CopilotHarness{}.Args(Job{
		Prompt:          "Check it.",
		Model:           "claude-sonnet-4.6",
		MaxTurns:        7,
		ResumeSessionID: "session-1",
		ResumePrompt:    "Check it.",
	})
	for _, want := range []string{
		"-p",
		"Check it.",
		"--output-format",
		"json",
		"--autopilot",
		"7",
		"--allow-all",
		"--no-ask-user",
		"claude-sonnet-4.6",
		"--resume=session-1",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("Args() missing %q: %v", want, args)
		}
	}
}

func TestCopilotEnvBYOKPassthrough(t *testing.T) {
	t.Setenv("COPILOT_PROVIDER_API_KEY", "secret")
	t.Setenv("COPILOT_MODEL", "gpt-5.6-sol")

	env := CopilotHarness{}.Env("https://byok.example.com")
	for _, want := range []string{
		"COPILOT_PROVIDER_BASE_URL=https://byok.example.com",
		"COPILOT_PROVIDER_API_KEY",
		"COPILOT_MODEL",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("Env() missing %q: %v", want, env)
		}
	}
	for _, entry := range (CopilotHarness{}).Env("") {
		if strings.HasPrefix(entry, "COPILOT_PROVIDER_") || entry == "COPILOT_MODEL" {
			t.Errorf("Env(\"\") passed BYOK setting %q", entry)
		}
	}
}

func TestCopilotStreamFixture(t *testing.T) {
	t.Parallel()

	file, err := os.Open("testdata/copilot-1.0.75.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	var events []Event
	CopilotHarness{}.ParseStream(file, func(event Event) {
		events = append(events, event)
	})
	if len(events) != 5 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	kinds := []string{
		KindThinking,
		KindTool,
		KindText,
		KindSession,
		KindResult,
	}
	for i, want := range kinds {
		if events[i].Kind != want {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, want)
		}
	}
	if events[1].Text != "go test ./..." {
		t.Errorf("tool summary = %q", events[1].Text)
	}
	if events[4].Turns != 1 || events[4].Usage.CacheReadTokens != 80 {
		t.Errorf("result event = %+v", events[4])
	}
	if events[4].CostUSD <= 0 {
		t.Errorf("result cost = %f, want a list-price estimate", events[4].CostUSD)
	}
	if events[3].SessionID != "34870a09-5067-4978-97bc-10d0d112ef64" {
		t.Errorf("session event = %+v", events[3])
	}
}

func TestCopilotStreamAccumulatesOneTerminalResult(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.usage","data":{"model":"claude-sonnet-4.6","inputTokens":100,"outputTokens":10,"cacheReadTokens":20,"cacheWriteTokens":5}}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"}}`,
		`{"type":"assistant.usage","data":{"model":"claude-sonnet-4.6","inputTokens":200,"outputTokens":30,"cacheReadTokens":40,"cacheWriteTokens":7}}`,
		`{"type":"assistant.turn_end","data":{"turnId":"1"}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	var events []Event
	CopilotHarness{}.ParseStream(strings.NewReader(stream), func(event Event) {
		events = append(events, event)
	})
	if len(events) != 2 {
		t.Fatalf("events = %+v, want session and one result", events)
	}
	if events[0].Kind != KindSession || events[0].SessionID != "session-1" {
		t.Errorf("first event = %+v, want session", events[0])
	}
	result := events[1]
	if result.Kind != KindResult || result.Turns != 2 {
		t.Fatalf("terminal event = %+v, want two-turn result", result)
	}
	wantUsage := Usage{
		InputTokens:      300,
		OutputTokens:     40,
		CacheReadTokens:  60,
		CacheWriteTokens: 12,
	}
	if result.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", result.Usage, wantUsage)
	}
	wantCost := CostFromUsage("claude-sonnet-4.6", wantUsage)
	if math.Abs(result.CostUSD-wantCost) > 1e-12 {
		t.Errorf("cost = %.12f, want %.12f", result.CostUSD, wantCost)
	}
}
