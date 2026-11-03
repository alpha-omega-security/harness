package harness

import (
	"slices"
	"strings"
	"testing"
)

func TestCodexArgsAndStream(t *testing.T) {
	t.Parallel()

	args := CodexHarness{}.Args(Job{
		Prompt:          "Check it.",
		Model:           "gpt-5.3-codex",
		BaseURL:         "https://models.example.test/v1",
		ResumeSessionID: "thread-1",
		ResumePrompt:    "Check it.",
	})
	for _, want := range []string{"exec", "--json", "gpt-5.3-codex", "resume", "thread-1", "Check it."} {
		if !slices.Contains(args, want) {
			t.Errorf("Args() missing %q: %v", want, args)
		}
	}

	input := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-1"}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":5}}`,
	}, "\n")
	var events []Event
	CodexHarness{}.ParseStream(strings.NewReader(input), func(event Event) {
		events = append(events, event)
	})
	if len(events) != 4 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[1].Kind != KindTool || events[1].Text != "go test ./..." {
		t.Errorf("tool event = %+v", events[1])
	}
	if events[3].Usage.CacheReadTokens != 20 || events[3].Turns != 1 {
		t.Errorf("result event = %+v", events[3])
	}
}

func TestCodexArgsConfigOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		job    Job
		want   []string
		absent string
	}{
		{
			name: "defaults",
			want: []string{
				"-c", "analytics.enabled=false",
				"-c", "features.apps=false",
				"-c", "features.plugins=false",
				"exec",
			},
			absent: "model_reasoning_effort=",
		},
		{
			name: "effort",
			job:  Job{Effort: "xhigh"},
			want: []string{"-c", `model_reasoning_effort="xhigh"`, "exec"},
		},
		{
			name: "effort that would break out of the TOML string",
			job:  Job{Effort: "high\"\nsandbox_mode=\"x"},
			want: []string{"-c", `model_reasoning_effort=""`, "exec"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			args := CodexHarness{}.Args(test.job)
			exec := slices.Index(args, "exec")
			if exec+1 < len(test.want) || !slices.Equal(args[exec-len(test.want)+1:exec+1], test.want) {
				t.Errorf("Args() = %q, want %q before the exec subcommand", args, test.want)
			}
			if test.absent == "" {
				return
			}
			if i := slices.IndexFunc(args, func(arg string) bool { return strings.HasPrefix(arg, test.absent) }); i >= 0 {
				t.Errorf("Args() contains %q: %q", args[i], args)
			}
		})
	}
}

func TestCodexToolItems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		tool  string
		text  string
	}{
		{
			name:  "web search",
			input: `{"type":"item.completed","item":{"id":"item_19","type":"web_search","query":"codex exec json events","action":{"type":"search","query":"codex exec json events"}}}`,
			tool:  "web_search",
			text:  "codex exec json events",
		},
		{
			name:  "file change",
			input: `{"type":"item.completed","item":{"id":"item_22","type":"file_change","changes":[{"path":"/tmp/tests.json","kind":"add"},{"path":"/tmp/codex.go","kind":"update"}],"status":"completed"}}`,
			tool:  "file_change",
			text:  "/tmp/tests.json, /tmp/codex.go",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var events []Event
			CodexHarness{}.ParseStream(strings.NewReader(test.input), func(event Event) {
				events = append(events, event)
			})
			if len(events) != 1 {
				t.Fatalf("got %d events: %+v", len(events), events)
			}
			if events[0].Kind != KindTool || events[0].Tool != test.tool || events[0].Text != test.text {
				t.Errorf("tool event = %+v", events[0])
			}
		})
	}
}

func TestCodexTodoListItemsAreDropped(t *testing.T) {
	t.Parallel()

	for _, eventType := range []string{"item.updated", "item.completed"} {
		t.Run(eventType, func(t *testing.T) {
			t.Parallel()

			input := `{"type":"` + eventType + `","item":{"id":"item_5","type":"todo_list","items":[{"text":"Inspect inputs","completed":true},{"text":"Run tests","completed":false}]}}`
			var events []Event
			CodexHarness{}.ParseStream(strings.NewReader(input), func(event Event) {
				events = append(events, event)
			})
			if len(events) != 0 {
				t.Fatalf("got %d events for progress-only todo list: %+v", len(events), events)
			}
		})
	}
}

func TestCodexFailureEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want Event
	}{
		{
			name: "turn failed",
			line: `{"type":"turn.failed","error":{"message":"Quota exceeded. Check your plan and billing details."}}`,
			want: Event{Kind: KindError, Text: "Quota exceeded. Check your plan and billing details."},
		},
		{
			name: "top-level error",
			line: `{"type":"error","message":"Reconnecting... 2/5 (stream disconnected before completion)"}`,
			want: Event{Kind: KindError, Text: "Reconnecting... 2/5 (stream disconnected before completion)"},
		},
		{
			name: "string error",
			line: `{"error":"insufficient_quota"}`,
			want: Event{Kind: KindError, Text: "insufficient_quota"},
		},
		{
			name: "error without message",
			line: `{"type":"error"}`,
			want: Event{Kind: KindText, Text: `{"type":"error"}`},
		},
		{
			name: "malformed error",
			line: `{"type":"turn.failed","error":42}`,
			want: Event{Kind: KindText, Text: `{"type":"turn.failed","error":42}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var events []Event
			CodexHarness{}.ParseStream(strings.NewReader(tt.line), func(event Event) {
				events = append(events, event)
			})
			if len(events) != 1 || events[0] != tt.want {
				t.Fatalf("events = %+v, want %+v", events, tt.want)
			}
		})
	}
}

func TestCodexTurnFailedIsAnAccountError(t *testing.T) {
	t.Parallel()

	var text string
	CodexHarness{}.ParseStream(strings.NewReader(`{"type":"turn.failed","error":{"message":"Quota exceeded. Check your plan and billing details."}}`), func(event Event) {
		if event.Kind == KindError {
			text = event.Text
		}
	})
	if (CodexHarness{}).AccountErrorText(text) == "" {
		t.Fatalf("AccountErrorText(%q) is empty", text)
	}
	if detail := (CodexHarness{}).AccountErrorText("Reconnecting... 2/5 (stream disconnected before completion)"); detail != "" {
		t.Fatalf("a stream retry is an account error: %q", detail)
	}
}
