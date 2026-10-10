package harness

import (
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCopilotArgs(t *testing.T) {
	t.Parallel()

	args := CopilotHarness{}.Args(Job{
		Prompt:          "Check it.",
		Model:           "claude-sonnet-4.6",
		Effort:          "high",
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
		"--no-remote-export",
		"claude-sonnet-4.6",
		"--effort",
		"high",
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

	events := collectNonUsage(t, file)
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
	if events[4].Text != "Done." {
		t.Errorf("result text = %q", events[4].Text)
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

	events := collectNonUsage(t, strings.NewReader(stream))
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
	// A list-price estimate is per invocation, not a session total.
	if result.SessionCostUSD != 0 {
		t.Errorf("session cost = %v, want 0 without a checkpoint", result.SessionCostUSD)
	}
}

func TestCopilotStreamUsesLatestUsageCheckpointCost(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.usage","data":{"model":"claude-sonnet-4.6","inputTokens":1000000,"outputTokens":1000000}}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"}}`,
		`{"type":"session.usage_checkpoint","data":{"totalNanoAiu":7313250000,"totalPremiumRequests":1}}`,
		`{"type":"assistant.turn_end","data":{"turnId":"1"}}`,
		`{"type":"session.usage_checkpoint","data":{"totalNanoAiu":8148135000,"totalPremiumRequests":1}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	events := collectNonUsage(t, strings.NewReader(stream))
	if len(events) != 2 {
		t.Fatalf("events = %+v, want session and result", events)
	}
	result := events[1]
	if result.Turns != 2 || result.Usage.InputTokens != 1000000 {
		t.Errorf("result = %+v", result)
	}
	const wantCostUSD = 0.08148135
	if math.Abs(result.CostUSD-wantCostUSD) > 1e-12 {
		t.Errorf("cost = %.12f, want checkpoint %.12f", result.CostUSD, wantCostUSD)
	}
	if result.SessionCostUSD != result.CostUSD {
		t.Errorf("session cost = %.12f, want the checkpoint %.12f", result.SessionCostUSD, result.CostUSD)
	}
}

func TestCopilotStreamReassemblesMessageChunks(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.message","data":{"apiCallId":"old-call","content":"old response"}}`,
		`{"type":"assistant.message","data":{"apiCallId":"final-call","chunkIndex":1,"chunkCount":3,"content":"middle "}}`,
		`{"type":"assistant.message","data":{"apiCallId":"final-call","chunkIndex":0,"chunkCount":3,"content":"first "}}`,
		`{"type":"assistant.message","data":{"apiCallId":"final-call","chunkIndex":2,"chunkCount":3,"content":"last"}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	var result Event
	CopilotHarness{}.ParseStream(strings.NewReader(stream), func(event Event) {
		if event.Kind == KindResult {
			result = event
		}
	})
	if result.Text != "first middle last" {
		t.Errorf("result text = %q, want reassembled final model call", result.Text)
	}

	stream = strings.Join([]string{
		`{"type":"assistant.message","data":{"messageId":"m","chunkIndex":0,"chunkCount":2,"content":"a"}}`,
		`{"type":"assistant.message","data":{"messageId":"m","chunkIndex":1,"chunkCount":2,"content":"b"}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")
	CopilotHarness{}.ParseStream(strings.NewReader(stream), func(event Event) {
		if event.Kind == KindResult {
			result = event
		}
	})
	if result.Text != "ab" {
		t.Errorf("result text = %q, want messageId fallback reassembly", result.Text)
	}
}

func TestCopilotStreamDeduplicatesMessageReasoning(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.message","id":"message-event","data":{"apiCallId":"call-1","reasoningText":"checking","content":"done"}}`,
		`{"type":"assistant.reasoning","parentId":"message-event","data":{"reasoningId":"reasoning-1","content":"checking"}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	var thinking []string
	CopilotHarness{}.ParseStream(strings.NewReader(stream), func(event Event) {
		if event.Kind == KindThinking {
			thinking = append(thinking, event.Text)
		}
	})
	if !slices.Equal(thinking, []string{"checking"}) {
		t.Errorf("thinking events = %q, want one copy", thinking)
	}
}

func TestCopilotStreamFiltersSubagentConversation(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.intent","data":{"intent":"Checking the parent task."}}`,
		`{"type":"assistant.message","agentId":"child-1","data":{"content":"nested answer"}}`,
		`{"type":"assistant.reasoning","agentId":"child-1","data":{"content":"nested reasoning"}}`,
		`{"type":"tool.execution_start","agentId":"child-1","data":{"toolName":"shell","arguments":{"command":"nested"}}}`,
		`{"type":"assistant.usage","agentId":"child-1","data":{"model":"claude-sonnet-4.6","inputTokens":50,"outputTokens":5}}`,
		`{"type":"assistant.turn_end","agentId":"child-1","data":{"turnId":"child"}}`,
		`{"type":"assistant.message","data":{"content":"parent answer"}}`,
		`{"type":"assistant.turn_end","data":{"turnId":"parent"}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	events := collectNonUsage(t, strings.NewReader(stream))
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Kind != KindThinking || events[0].Text != "Checking the parent task." {
		t.Errorf("intent event = %+v", events[0])
	}
	if events[1].Kind != KindText || events[1].Text != "parent answer" {
		t.Errorf("parent message = %+v", events[1])
	}
	result := events[3]
	if result.Kind != KindResult || result.Text != "parent answer" || result.Turns != 1 {
		t.Errorf("result = %+v", result)
	}
	if result.Usage.InputTokens != 50 || result.Usage.OutputTokens != 5 {
		t.Errorf("subagent usage was not retained: %+v", result.Usage)
	}
}

func TestCopilotStreamQuotaRateLimits(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.usage","data":{"model":"gpt-5.6-sol","inputTokens":10,"quotaSnapshots":{` +
			`"premium":{"hasQuota":false,"resetDate":"2026-09-01T00:00:00Z"},` +
			`"unlimited":{"hasQuota":false,"isUnlimitedEntitlement":true},` +
			`"paid":{"hasQuota":false,"overage":3,"overageAllowedWithExhaustedQuota":true},` +
			`"inferred":{"remainingPercentage":0,"usageAllowedWithExhaustedQuota":true},` +
			`"available":{"hasQuota":true}}}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	var limits []*RateLimitInfo
	CopilotHarness{}.ParseStream(strings.NewReader(stream), func(event Event) {
		if event.Kind == KindRateLimit {
			limits = append(limits, event.RateLimit)
		}
	})
	if len(limits) != 3 {
		t.Fatalf("rate limits = %+v, want three exhausted snapshots", limits)
	}
	byType := make(map[string]*RateLimitInfo)
	for _, limit := range limits {
		byType[limit.Type] = limit
	}
	premium := byType["premium"]
	if premium == nil || !premium.Rejected() {
		t.Fatalf("premium limit = %+v, want rejected", premium)
	}
	wantReset := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if premium.ResetTime() == nil || !premium.ResetTime().Equal(wantReset) {
		t.Errorf("premium reset = %v, want %v", premium.ResetTime(), wantReset)
	}
	if paid := byType["paid"]; paid == nil || paid.Status != "allowed" || !paid.IsUsingOverage {
		t.Errorf("paid limit = %+v, want allowed overage", paid)
	}
	if inferred := byType["inferred"]; inferred == nil || inferred.Status != "allowed" {
		t.Errorf("inferred limit = %+v, want allowed", inferred)
	}
	if _, ok := byType["unlimited"]; ok {
		t.Errorf("unlimited entitlement should not be reported as exhausted")
	}
}

func TestCopilotStreamErrorEvents(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"model.call_failure","data":{"errorMessage":"upstream 503","model":"gpt-5.6-sol","failureKind":"upstream","errorType":"ServiceUnavailable","statusCode":503}}`,
		`{"type":"session.error","data":{"error":{"message":"quota exceeded"},"errorCode":"quota_exceeded","statusCode":429}}`,
		`{"type":"abort","data":{"reason":"user cancelled"}}`,
		`{"type":"session.warning","data":{"message":"BYOK provider missing region"}}`,
		`{"type":"result","sessionId":"session-1","exitCode":2}`,
	}, "\n")

	events := collectNonUsage(t, strings.NewReader(stream))
	want := []Event{
		{Kind: KindError, Text: "upstream 503 (gpt-5.6-sol, upstream, ServiceUnavailable, status 503)"},
		{Kind: KindError, Text: "quota exceeded (quota_exceeded, status 429)"},
		{Kind: KindError, Text: "copilot aborted: user cancelled"},
		{Kind: KindText, Text: "BYOK provider missing region"},
		{Kind: KindSession, SessionID: "session-1"},
		{Kind: KindError, Text: "copilot exited with code 2"},
	}
	if len(events) != len(want)+1 {
		t.Fatalf("events = %+v", events)
	}
	for i := range want {
		if events[i].Kind != want[i].Kind || events[i].Text != want[i].Text || events[i].SessionID != want[i].SessionID {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestCopilotStreamRequiresTerminalEnvelope(t *testing.T) {
	t.Parallel()

	stream := `{"type":"assistant.usage","data":{"model":"claude-sonnet-4.6","inputTokens":100}}`
	events := collectNonUsage(t, strings.NewReader(stream))
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none without a result envelope", events)
	}
}

// collectNonUsage parses a stream and drops usage events, which dedicated
// tests cover.
func collectNonUsage(_ *testing.T, r io.Reader) []Event {
	var events []Event
	CopilotHarness{}.ParseStream(r, func(event Event) {
		if event.Kind != KindUsage {
			events = append(events, event)
		}
	})
	return events
}

func TestCopilotStreamEmitsUsageEvents(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant.usage","data":{"model":"claude-sonnet-4.6","inputTokens":100,"outputTokens":10,"cacheReadTokens":20,"cacheWriteTokens":5}}`,
		`{"type":"assistant.usage","agentId":"child-1","data":{"model":"claude-sonnet-4.6","inputTokens":50,"outputTokens":5}}`,
		`{"type":"result","sessionId":"session-1","exitCode":0}`,
	}, "\n")

	var events []Event
	CopilotHarness{}.ParseStream(strings.NewReader(stream), func(event Event) {
		events = append(events, event)
	})
	if len(events) != 4 {
		t.Fatalf("events = %+v, want two usage then session and result", events)
	}
	want := []Usage{
		{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 20, CacheWriteTokens: 5},
		{InputTokens: 50, OutputTokens: 5},
	}
	var total float64
	for i, usage := range want {
		got := events[i]
		if got.Kind != KindUsage || got.Model != "claude-sonnet-4.6" || got.Usage != usage {
			t.Errorf("usage event %d = %+v", i, got)
		}
		if wantCost := CostFromUsage("claude-sonnet-4.6", usage); got.CostUSD != wantCost || wantCost <= 0 {
			t.Errorf("usage event %d cost = %v, want %v", i, got.CostUSD, wantCost)
		}
		total += got.CostUSD
	}
	result := events[3]
	if result.Kind != KindResult || result.Usage.InputTokens != 150 || result.CostUSD != total {
		t.Errorf("result = %+v", result)
	}
}
