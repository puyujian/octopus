package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// extractReasoningEffort reads the effective reasoning/thinking setting from
// the final provider payload. It intentionally understands the common native
// shapes instead of relying only on the normalized inbound request, because
// channel and group overrides are applied after protocol conversion.
func extractReasoningEffort(payload []byte) string {
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &root); err != nil || root == nil {
		return ""
	}

	for _, value := range []string{
		jsonString(root["reasoning_effort"]),
		jsonString(nestedRaw(root, "reasoning", "effort")),
		jsonString(nestedRaw(root, "output_config", "effort")),
		jsonString(nestedRaw(root, "outputConfig", "effort")),
		jsonString(nestedRaw(root, "generationConfig", "thinkingConfig", "thinkingLevel")),
		jsonString(nestedRaw(root, "generation_config", "thinking_config", "thinking_level")),
	} {
		if normalized := normalizeReasoningValue(value); normalized != "" {
			return normalized
		}
	}

	for _, raw := range []json.RawMessage{
		nestedRaw(root, "reasoning", "max_tokens"),
		nestedRaw(root, "thinking", "budget_tokens"),
		nestedRaw(root, "generationConfig", "thinkingConfig", "thinkingBudget"),
		nestedRaw(root, "generation_config", "thinking_config", "thinking_budget"),
	} {
		if budget := jsonBudget(raw); budget != "" {
			return budget
		}
	}

	if mode := normalizeThinkingMode(nestedRaw(root, "thinking", "type")); mode != "" {
		return mode
	}
	if enabled, ok := jsonBool(root["enable_thinking"]); ok {
		if enabled {
			return "enabled"
		}
		return "disabled"
	}
	if enabled, ok := jsonBool(nestedRaw(root, "generationConfig", "thinkingConfig", "includeThoughts")); ok {
		if enabled {
			return "enabled"
		}
		return "disabled"
	}
	return ""
}

func nestedRaw(root map[string]json.RawMessage, path ...string) json.RawMessage {
	var current json.RawMessage
	if len(path) == 0 {
		return nil
	}
	current = root[path[0]]
	for _, key := range path[1:] {
		var object map[string]json.RawMessage
		if len(current) == 0 || json.Unmarshal(current, &object) != nil || object == nil {
			return nil
		}
		current = object[key]
	}
	return current
}

func jsonString(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func jsonBudget(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if number, err := strconv.ParseFloat(string(trimmed), 64); err == nil {
		if number == float64(int64(number)) {
			return fmt.Sprintf("budget %d", int64(number))
		}
	}
	if value := jsonString(raw); value != "" {
		return "budget " + value
	}
	return ""
}

func normalizeReasoningValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "minimal", "low", "medium", "high", "none", "adaptive":
		return value
	default:
		return value
	}
}

func normalizeThinkingMode(raw json.RawMessage) string {
	value := strings.ToLower(strings.TrimSpace(jsonString(raw)))
	switch value {
	case "enabled", "disabled", "adaptive":
		return value
	default:
		return ""
	}
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}
