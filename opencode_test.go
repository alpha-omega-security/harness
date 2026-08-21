package harness

import (
	"slices"
	"strings"
	"testing"
)

func TestOpencodeEnvMirrorsServerLogsToStderr(t *testing.T) {
	t.Parallel()

	env := OpencodeHarness{}.Env("")
	for _, want := range []string{"OPENCODE_PRINT_LOGS=1", "OPENCODE_LOG_LEVEL=error"} {
		if !slices.Contains(env, want) {
			t.Errorf("Env() = %v, missing %q", env, want)
		}
	}
}

func TestOpencodeStream(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"type":"step_start","sessionID":"session-1","part":{}}`,
		`{"type":"text","part":{"type":"text","text":"hello"}}`,
		`{"type":"reasoning","part":{"type":"reasoning","text":"thinking"}}`,
		`{"type":"tool","part":{"type":"tool","tool":"bash","state":{"input":{"command":"go test ./..."}}}}`,
		`{"type":"step_finish","part":{"type":"step-finish","cost":0.2,"tokens":{"input":100,"output":5,"reasoning":2,"cache":{"read":20,"write":3}}}}`,
		`not json`,
	}, "\n")

	var events []Event
	OpencodeHarness{}.ParseStream(strings.NewReader(input), func(event Event) {
		events = append(events, event)
	})
	if len(events) != 6 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[2].Kind != KindThinking {
		t.Errorf("reasoning event = %+v", events[2])
	}
	if events[3].Kind != KindTool || events[3].Text != "go test ./..." {
		t.Errorf("tool event = %+v", events[3])
	}
	if events[4].Kind != KindResult || events[4].Usage.OutputTokens != 7 {
		t.Errorf("result event = %+v", events[4])
	}
}
