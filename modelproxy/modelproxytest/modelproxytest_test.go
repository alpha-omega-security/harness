package modelproxytest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type sse struct {
	kind string
	data map[string]any
}

func events(t *testing.T, write func(http.ResponseWriter)) []sse {
	t.Helper()
	rec := httptest.NewRecorder()
	write(rec)
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	var out []sse
	for _, chunk := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n") {
		kind, data, ok := strings.Cut(chunk, "\ndata: ")
		if !ok || !strings.HasPrefix(kind, "event: ") {
			t.Fatalf("malformed event %q", chunk)
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			t.Fatalf("event data %q: %v", data, err)
		}
		if value["type"] != strings.TrimPrefix(kind, "event: ") {
			t.Fatalf("event %q carries type %v", kind, value["type"])
		}
		out = append(out, sse{strings.TrimPrefix(kind, "event: "), value})
	}
	return out
}

func kinds(events []sse) string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.kind
	}
	return strings.Join(names, ",")
}

func TestAnthropicText(t *testing.T) {
	got := events(t, func(w http.ResponseWriter) { AnthropicText(w, "claude-test", "Done.") })
	if kinds(got) != "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Fatalf("events = %s", kinds(got))
	}
	if delta := got[2].data["delta"].(map[string]any); delta["text"] != "Done." {
		t.Errorf("delta = %v", delta)
	}
	if delta := got[4].data["delta"].(map[string]any); delta["stop_reason"] != "end_turn" {
		t.Errorf("message delta = %v", delta)
	}
}

func TestAnthropicToolUse(t *testing.T) {
	got := events(t, func(w http.ResponseWriter) {
		AnthropicToolUse(w, "claude-test", "tool_1", "Bash", `{"command":"true"}`)
	})
	block := got[1].data["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "tool_1" || block["name"] != "Bash" {
		t.Errorf("block = %v", block)
	}
	if delta := got[2].data["delta"].(map[string]any); delta["partial_json"] != `{"command":"true"}` {
		t.Errorf("delta = %v", delta)
	}
	if delta := got[4].data["delta"].(map[string]any); delta["stop_reason"] != "tool_use" {
		t.Errorf("message delta = %v", delta)
	}
}

func TestAnthropicRepliesHaveTheirOwnMessageID(t *testing.T) {
	first := events(t, func(w http.ResponseWriter) {
		AnthropicToolUse(w, "claude-test", "tool_1", "Bash", `{"command":"true"}`)
	})
	second := events(t, func(w http.ResponseWriter) { AnthropicText(w, "claude-test", "Done.") })
	id := func(got []sse) any { return got[0].data["message"].(map[string]any)["id"] }
	if id(first) == nil || id(first) == id(second) {
		t.Errorf("message IDs = %v, %v", id(first), id(second))
	}
}

func TestResponsesFunctionCall(t *testing.T) {
	got := events(t, func(w http.ResponseWriter) {
		ResponsesFunctionCall(w, "gpt-test", "call_1", "exec_command", `{"cmd":"true"}`)
	})
	if kinds(got) != "response.created,response.output_item.done,response.completed" {
		t.Fatalf("events = %s", kinds(got))
	}
	item := got[1].data["item"].(map[string]any)
	if item["call_id"] != "call_1" || item["name"] != "exec_command" || item["arguments"] != `{"cmd":"true"}` {
		t.Errorf("item = %v", item)
	}
	if response := got[2].data["response"].(map[string]any); response["status"] != "completed" || response["model"] != "gpt-test" {
		t.Errorf("response = %v", response)
	}
}

func TestCodexStartup(t *testing.T) {
	for path, want := range map[string]bool{
		"/model-proxy/auth/v1/user-auth-credential/whoami": true,
		"/backend-api/wham/config/bundle":                  true,
		"/backend-api/codex/models":                        true,
		"/backend-api/codex/responses":                     false,
	} {
		rec := httptest.NewRecorder()
		if got := CodexStartup(rec, httptest.NewRequest(http.MethodGet, path, nil)); got != want {
			t.Errorf("CodexStartup(%s) = %v", path, got)
			continue
		}
		if want && !json.Valid(rec.Body.Bytes()) {
			t.Errorf("%s body = %q", path, rec.Body.String())
		}
		if !want && rec.Body.Len() != 0 {
			t.Errorf("%s wrote %q", path, rec.Body.String())
		}
	}
}
