package modelproxy

import "slices"

// Validate protocol positions, not arbitrary JSON in function schemas or tool arguments.
// A new provider capability must be reviewed before it can cross this boundary.
func safeRequest(v map[string]any, anthropic bool) bool {
	if anthropic {
		return fields(v, "model", "messages", "system", "tools", "tool_choice", "max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "thinking", "output_config", "metadata", "cache_control", "context_management") &&
			safeMessages(v["messages"]) && safeContent(v["system"], true) && safeToolList(v["tools"], true) && safeContextEdits(v["context_management"])
	}
	if !fields(v, "model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "store", "stream", "stream_options", "include", "text", "max_output_tokens", "max_tokens", "metadata", "client_metadata", "prompt_cache_key", "service_tier", "background") {
		return false
	}
	if background, ok := v["background"]; ok && background != false {
		return false
	}
	if include, ok := v["include"]; ok {
		values, ok := include.([]any)
		if !ok {
			return false
		}
		for _, value := range values {
			if value != "reasoning.encrypted_content" {
				return false
			}
		}
	}
	return safeItems(v["input"]) && safeToolList(v["tools"], false) && safeToolChoice(v["tool_choice"])
}

func fields(v map[string]any, allowed ...string) bool {
	for key := range v {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func safeToolList(v any, anthropic bool) bool {
	if v == nil {
		return true
	}
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok || !safeTool(t, anthropic) {
			return false
		}
	}
	return true
}

func safeTool(t map[string]any, anthropic bool) bool {
	if anthropic {
		return fields(t, "name", "description", "input_schema", "cache_control", "type", "strict", "defer_loading") && (t["type"] == nil || t["type"] == "custom") //nolint:goconst // protocol type names are data, like the field names around them
	}
	switch t["type"] {
	case "function":
		return fields(t, "type", "name", "description", "parameters", "strict", "defer_loading")
	case "custom":
		return fields(t, "type", "name", "description", "format", "defer_loading")
	case "namespace":
		return fields(t, "type", "name", "description", "tools") && safeToolList(t["tools"], false)
	case "local_shell", "apply_patch":
		return fields(t, "type")
	case "tool_search":
		return t["execution"] == "client" && fields(t, "type", "execution", "description", "parameters")
	default:
		return false
	}
}

func safeToolChoice(v any) bool {
	if v == nil || v == "auto" || v == "none" || v == "required" {
		return true
	}
	t, ok := v.(map[string]any)
	return ok && (t["type"] == "function" || t["type"] == "custom") && fields(t, "type", "name")
}

func safeItems(v any) bool {
	if v == nil {
		return true
	}
	if _, ok := v.(string); ok {
		return true
	}
	items, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		t, ok := item.(map[string]any)
		if !ok || !safeItem(t) {
			return false
		}
	}
	return true
}

func safeItem(t map[string]any) bool {
	switch t["type"] {
	case nil, "message":
		return (t["type"] != nil || t["id"] == nil) && fields(t, "type", "id", "role", "content", "status", "phase") && safeContent(t["content"], false)
	case "reasoning":
		return fields(t, "type", "id", "summary", "content", "encrypted_content", "status")
	case "function_call", "custom_tool_call":
		return fields(t, "type", "id", "call_id", "name", "arguments", "input", "status", "namespace")
	case "function_call_output", "custom_tool_call_output":
		return fields(t, "type", "id", "call_id", "output", "status") && safeContent(t["output"], false)
	case "additional_tools", "tool_search_output":
		return fields(t, "type", "id", "call_id", "role", "tools", "status", "execution") && safeToolList(t["tools"], false)
	case "tool_search_call":
		return t["execution"] == "client" && fields(t, "type", "id", "call_id", "arguments", "status", "execution")
	default:
		return false
	}
}

func safeMessages(v any) bool {
	if v == nil {
		return true
	}
	items, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || !fields(m, "role", "content", "output_config") || !safeContent(m["content"], true) {
			return false
		}
		if output, ok := m["output_config"]; ok {
			config, ok := output.(map[string]any)
			if !ok || !fields(config, "effort") {
				return false
			}
		}
	}
	return true
}

func safeContent(v any, anthropic bool) bool {
	if v == nil {
		return true
	}
	if _, ok := v.(string); ok {
		return true
	}
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok || !safeBlock(t, anthropic) {
			return false
		}
	}
	return true
}

func safeBlock(t map[string]any, anthropic bool) bool {
	if !anthropic {
		return slices.Contains([]any{"input_text", "output_text", "refusal"}, t["type"]) && fields(t, "type", "text", "refusal", "annotations", "logprobs")
	}
	switch t["type"] {
	case "text":
		return fields(t, "type", "text", "cache_control")
	case "thinking", "redacted_thinking":
		return fields(t, "type", "thinking", "signature", "data")
	case "tool_use":
		return fields(t, "type", "id", "name", "input", "cache_control", "caller") && directCaller(t["caller"])
	case "tool_result":
		return fields(t, "type", "tool_use_id", "content", "is_error", "cache_control") && safeContent(t["content"], true)
	case "tool_reference":
		return fields(t, "type", "tool_name")
	default:
		return false
	}
}

// directCaller accepts the caller of a tool_use block the model emitted itself. Every other caller is hosted
// code execution calling the tool.
func directCaller(v any) bool {
	if v == nil {
		return true
	}
	c, ok := v.(map[string]any)
	return ok && fields(c, "type") && c["type"] == "direct"
}

func safeContextEdits(v any) bool {
	if v == nil {
		return true
	}
	m, ok := v.(map[string]any)
	if !ok || !fields(m, "edits") {
		return false
	}
	edits, ok := m["edits"].([]any)
	if !ok {
		return false
	}
	for _, edit := range edits {
		e, ok := edit.(map[string]any)
		if !ok {
			return false
		}
		switch e["type"] {
		case "clear_thinking_20251015":
			if !fields(e, "type", "keep") {
				return false
			}
		case "clear_tool_uses_20250919":
			if !fields(e, "type", "trigger", "keep", "clear_at_least", "exclude_tools", "clear_tool_inputs") {
				return false
			}
		default:
			return false
		}
	}
	return true
}
