// Package modelproxytest writes the model API responses an agent CLI accepts, so a test can stand a fake
// upstream behind modelproxy and drive a real CLI without a model key.
package modelproxytest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// messages numbers the Messages API replies, since ClaudeHarness counts the usage of each message ID once.
var messages atomic.Int64

// AnthropicText streams a Messages API reply made of one text block, which ends the turn.
//
//nolint:goconst // protocol field names are data
func AnthropicText(w http.ResponseWriter, model, text string) {
	anthropic(w, model, "end_turn",
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "text_delta", "text": text})
}

// AnthropicToolUse streams a Messages API reply that calls the tool name with input, a JSON object. Each call
// in one conversation needs its own id.
//
//nolint:goconst // protocol field names are data
func AnthropicToolUse(w http.ResponseWriter, model, id, name, input string) {
	anthropic(w, model, "tool_use",
		map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		map[string]any{"type": "input_json_delta", "partial_json": input})
}

//nolint:goconst // protocol field names are data
func anthropic(w http.ResponseWriter, model, stop string, block, delta map[string]any) {
	stream := newStream(w)
	stream.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + strconv.FormatInt(messages.Add(1), 10), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0},
	}})
	stream.event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
	stream.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
	stream.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	stream.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	stream.event("message_stop", map[string]any{"type": "message_stop"})
}

// ResponsesFunctionCall streams a Responses API reply that calls the function name with arguments, a JSON
// object, as Codex expects.
//
//nolint:goconst // protocol field names are data
func ResponsesFunctionCall(w http.ResponseWriter, model, callID, name, arguments string) {
	item := map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": name, "arguments": arguments}
	stream := newStream(w)
	stream.event("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "status": "in_progress", "output": []any{}}})
	stream.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	stream.event("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{
		"id": "resp_fixture", "status": "completed", "model": model, "output": []any{item},
		"usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	}})
}

// CodexStartup answers the ChatGPT routes Codex calls before its first model request when it signs in with
// an access token, and reports whether r was one of them.
func CodexStartup(w http.ResponseWriter, r *http.Request) bool {
	var body string
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/user-auth-credential/whoami"):
		body = `{"email":"agent@example.com","chatgpt_user_id":"user-fixture","chatgpt_account_id":"account-fixture","chatgpt_plan_type":"enterprise","chatgpt_account_is_fedramp":false}`
	case strings.HasSuffix(r.URL.Path, "/wham/config/bundle"):
		body = `{}`
	case strings.HasSuffix(r.URL.Path, "/codex/models"):
		body = `{"models":[]}`
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
	return true
}

type stream struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newStream(w http.ResponseWriter) stream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	return stream{w, http.NewResponseController(w)}
}

func (s stream) event(kind string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, raw)
	_ = s.rc.Flush()
}
