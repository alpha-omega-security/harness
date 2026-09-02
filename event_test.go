package harness

import (
	"strings"
	"testing"
	"time"
)

func TestClaudeParseStream(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"session-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}},{"type":"text","text":"done"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ignored"}]}}`,
		`{"type":"future_event","payload":{"value":1}}`,
		`{"type":"result","result":"ok","total_cost_usd":0.42,"num_turns":2,"usage":{"input_tokens":10,"output_tokens":6}}`,
	}, "\n")

	var events []Event
	ClaudeHarness{}.ParseStream(strings.NewReader(input), func(event Event) {
		events = append(events, event)
	})
	if len(events) != 6 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Kind != KindSession || events[0].SessionID != "session-1" {
		t.Errorf("session event = %+v", events[0])
	}
	if events[1].Kind != KindThinking || events[1].Text != "hmm" {
		t.Errorf("thinking event = %+v", events[1])
	}
	if events[2].Kind != KindTool || events[2].Text != "go test ./..." {
		t.Errorf("tool event = %+v", events[2])
	}
	if events[3].Kind != KindText || events[3].Text != "done" {
		t.Errorf("text event = %+v", events[3])
	}
	if events[4].Kind != KindText || !strings.Contains(events[4].Text, "future_event") {
		t.Errorf("unknown event = %+v", events[4])
	}
	if events[5].Kind != KindResult || events[5].Turns != 2 || events[5].CostUSD != 0.42 {
		t.Errorf("result event = %+v", events[5])
	}
}

func TestRateLimitInfo(t *testing.T) {
	t.Parallel()

	limit := &RateLimitInfo{
		Status:         "allowed",
		OverageStatus:  "rejected",
		IsUsingOverage: true,
		ResetsAt:       1_782_990_000,
		Type:           "five_hour",
	}
	if !limit.Rejected() {
		t.Fatal("Rejected() = false")
	}
	want := time.Unix(limit.ResetsAt, 0).UTC()
	if got := limit.ResetTime(); got == nil || !got.Equal(want) {
		t.Errorf("ResetTime() = %v, want %v", got, want)
	}
	if got := FormatEvent(Event{Kind: KindRateLimit, RateLimit: limit}); !strings.Contains(got, "five_hour") {
		t.Errorf("FormatEvent() = %q", got)
	}
}

func TestCostFromUsage(t *testing.T) {
	t.Parallel()

	usage := Usage{
		InputTokens:      1_000_000,
		OutputTokens:     1_000_000,
		CacheReadTokens:  200_000,
		CacheWriteTokens: 100_000,
	}
	if got, want := CostFromUsage("anthropic/claude-sonnet-4-6[1m]", usage), 17.535; got != want {
		t.Errorf("CostFromUsage() = %v, want %v", got, want)
	}
	if got := CostFromUsage("unknown", usage); got != 0 {
		t.Errorf("unknown model cost = %v", got)
	}
	if got, want := CostFromUsage("claude-fable-5-1[1m]", usage), 58.3; got != want {
		t.Errorf("CostFromUsage(fable 5.1) = %v, want %v", got, want)
	}
	if got, want := CostFromUsage("claude-fable-5[1m]", usage), 58.45; got != want {
		t.Errorf("CostFromUsage(fable 5) = %v, want %v", got, want)
	}
}
