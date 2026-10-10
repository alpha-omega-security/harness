package harness

import (
	"math"
	"slices"
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

func TestFormatEgressEvent(t *testing.T) {
	t.Parallel()

	if got := FormatEvent(Event{Kind: KindEgress, Text: "proxy denied blocked.test"}); got != "[egress] proxy denied blocked.test" {
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

func claudeEvents(input string) []Event {
	var events []Event
	ClaudeHarness{}.ParseStream(strings.NewReader(input), func(event Event) {
		events = append(events, event)
	})
	return events
}

func TestClaudeArgsIncludePartialMessages(t *testing.T) {
	t.Parallel()

	args := ClaudeHarness{}.Args(Job{Prompt: "go"})
	if i := slices.Index(args, "--verbose"); i < 0 || args[i+1] != "--include-partial-messages" {
		t.Errorf("Args() = %v, want --include-partial-messages after --verbose", args)
	}
}

func TestClaudeUsageEvents(t *testing.T) {
	t.Parallel()

	const model = "claude-haiku-4-5-20251001"
	usage := `"usage":{"input_tokens":10,"cache_creation_input_tokens":4570,"cache_read_input_tokens":16626,"output_tokens":3}`
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_X","model":"` + model + `",` + usage + `}},"api_message_id":"msg_X"}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"api_message_id":"msg_X"}`,
		`{"type":"assistant","message":{"id":"msg_X","model":"` + model + `","content":[{"type":"text","text":"hi"}],` + usage + `}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}},"api_message_id":"msg_X"}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"api_message_id":"msg_X"}`,
		`{"type":"assistant","message":{"id":"msg_X","model":"` + model + `","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}],` + usage + `}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"cache_creation_input_tokens":4570,"cache_read_input_tokens":16626,"output_tokens":202}},"api_message_id":"msg_X"}`,
		`{"type":"stream_event","event":{"type":"message_stop"},"api_message_id":"msg_X"}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"id":"msg_sub","model":"` + model + `","content":[{"type":"text","text":"sub"}],"usage":{"input_tokens":2,"output_tokens":7}}}`,
		`{"type":"assistant","message":{"id":"msg_syn","model":"<synthetic>","content":[{"type":"text","text":"none"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"assistant","message":{"id":"msg_unk","model":"mystery-model","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"result","result":"ok","total_cost_usd":0.1,"num_turns":1,"usage":{"input_tokens":12,"output_tokens":210}}`,
	}, "\n")

	events := claudeEvents(input)
	var kinds []string
	var usageEvents []Event
	for _, event := range events {
		kinds = append(kinds, event.Kind)
		if event.Kind == KindUsage {
			usageEvents = append(usageEvents, event)
		}
	}
	// Delta lines and the zero-usage synthetic message emit no usage event
	// while the repeated assistant usage is reported once.
	wantKinds := []string{KindUsage, KindText, KindTool, KindUsage, KindText, KindUsage, KindText, KindUsage, KindResult}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("kinds = %v, want %v", kinds, wantKinds)
	}
	var main Usage
	for _, event := range usageEvents[:2] {
		main.InputTokens += event.Usage.InputTokens
		main.OutputTokens += event.Usage.OutputTokens
		main.CacheReadTokens += event.Usage.CacheReadTokens
		main.CacheWriteTokens += event.Usage.CacheWriteTokens
	}
	want := Usage{InputTokens: 10, OutputTokens: 202, CacheReadTokens: 16626, CacheWriteTokens: 4570}
	if main != want {
		t.Errorf("summed usage = %+v, want %+v", main, want)
	}
	first := usageEvents[0]
	if first.Model != model || first.Usage != (Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 16626, CacheWriteTokens: 4570}) {
		t.Errorf("message_start usage = %+v", first)
	}
	// Cache tokens are priced on top of input_tokens, per million tokens.
	if wantCost := (10*1.0 + 16626*0.1 + 4570*1.25 + 3*5.0) / 1e6; math.Abs(first.CostUSD-wantCost) > 1e-9 {
		t.Errorf("cost = %v, want %v", first.CostUSD, wantCost)
	}
	if delta := usageEvents[1]; delta.Model != model || delta.Usage != (Usage{OutputTokens: 199}) {
		t.Errorf("message_delta usage = %+v", delta)
	}
	if sub := usageEvents[2]; sub.Model != model || sub.Usage != (Usage{InputTokens: 2, OutputTokens: 7}) || sub.CostUSD <= 0 {
		t.Errorf("subagent usage = %+v", sub)
	}
	if unknown := usageEvents[3]; unknown.Model != "mystery-model" || unknown.CostUSD != 0 || unknown.Usage.InputTokens != 1 {
		t.Errorf("unknown model usage = %+v", unknown)
	}
	if result := events[len(events)-1]; result.Kind != KindResult || result.CostUSD != 0.1 || result.Usage.OutputTokens != 210 {
		t.Errorf("result = %+v", result)
	}
}

func TestClaudeUsageEventsWithoutAPIMessageID(t *testing.T) {
	t.Parallel()

	// Older producers and plugin-rewritten events omit api_message_id, so the
	// delta must be attributed to the message the last message_start opened.
	const model = "claude-haiku-4-5-20251001"
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_A","model":"` + model + `","usage":{"input_tokens":10,"output_tokens":3}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":273}}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_B","model":"` + model + `","usage":{"input_tokens":20,"output_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"output_tokens":40}}}`,
	}, "\n")

	var got []Usage
	for _, event := range claudeEvents(input) {
		if event.Kind == KindUsage {
			if event.Model != model {
				t.Errorf("usage event model = %q, want %q", event.Model, model)
			}
			got = append(got, event.Usage)
		}
	}
	want := []Usage{
		{InputTokens: 10, OutputTokens: 3},
		{OutputTokens: 270},
		{InputTokens: 20, OutputTokens: 1},
		{OutputTokens: 39},
	}
	if !slices.Equal(got, want) {
		t.Errorf("usage events = %+v, want %+v", got, want)
	}
}

func TestFormatUsageEvent(t *testing.T) {
	t.Parallel()

	usage := Usage{InputTokens: 10, OutputTokens: 202, CacheReadTokens: 16626, CacheWriteTokens: 4570}
	got := FormatEvent(Event{Kind: KindUsage, Model: "claude-haiku-4-5", Usage: usage, CostUSD: 0.0123})
	if want := "[usage] claude-haiku-4-5 in=10 out=202 cache_read=16626 cache_write=4570 cost=$0.0123"; got != want {
		t.Errorf("FormatEvent() = %q, want %q", got, want)
	}
	got = FormatEvent(Event{Kind: KindUsage, Usage: usage})
	if want := "[usage] in=10 out=202 cache_read=16626 cache_write=4570 cost=$0.0000"; got != want {
		t.Errorf("FormatEvent() without model = %q, want %q", got, want)
	}
}

func TestClaudeUsageEventsPriceOneHourCacheWritesOnce(t *testing.T) {
	t.Parallel()

	const model = "claude-haiku-4-5-20251001"
	usage := `"usage":{"input_tokens":8,"cache_creation_input_tokens":1314,"cache_read_input_tokens":21196,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1314},"output_tokens":1}`
	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_Y","model":"` + model + `",` + usage + `}},"api_message_id":"msg_Y"}`,
		`{"type":"assistant","message":{"id":"msg_Y","model":"` + model + `","content":[{"type":"text","text":"done"}],` + usage + `}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":8,"cache_creation_input_tokens":1314,"cache_read_input_tokens":21196,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1314},"output_tokens":78}},"api_message_id":"msg_Y"}`,
	}, "\n")

	var total float64
	for _, event := range claudeEvents(input) {
		if event.Kind == KindUsage {
			total += event.CostUSD
		}
	}
	// One-hour cache writes bill at twice the input rate, charged once even
	// though the snapshot repeats on every line of the message.
	want := (8*1.0 + 21196*0.1 + 1314*2.0 + 78*5.0) / 1e6
	if math.Abs(total-want) > 1e-12 {
		t.Errorf("summed cost = %v, want %v", total, want)
	}
}
