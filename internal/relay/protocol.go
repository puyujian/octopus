package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

var errUpstreamStreamError = errors.New("upstream stream error event")

// inspectFirstSSEEvent checks whether the first SSE payload to be written
// to the client represents an upstream error. If it does, the function
// returns a non-nil error so the StreamProcessor aborts before writing,
// allowing the relay to retry on a different channel.
//
// The input is the already-transformed SSE byte slice (e.g. "data: {...}\n\n").
// The inboundFormat determines which JSON fields to check.
func inspectFirstSSEEvent(inboundFormat model.APIFormat, output []byte) error {
	data := extractSSEData(output)
	if len(data) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "[DONE]" {
		return nil
	}

	switch inboundFormat {
	case model.APIFormatOpenAIChatCompletion:
		return inspectOpenAIChatEvent(data)
	case model.APIFormatOpenAIResponse:
		return inspectOpenAIResponseEvent(data)
	case model.APIFormatAnthropicMessage:
		return inspectAnthropicEvent(output, data)
	default:
		return nil
	}
}

// extractSSEData extracts the JSON payload from an SSE "data:" line.
func extractSSEData(output []byte) []byte {
	s := string(output)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			return []byte(strings.TrimSpace(line[5:]))
		}
	}
	return nil
}

func inspectOpenAIChatEvent(data []byte) error {
	var probe struct {
		Error *json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	if probe.Error == nil || string(*probe.Error) == "null" {
		return nil
	}
	var detail struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(*probe.Error, &detail)
	msg := detail.Message
	if msg == "" {
		msg = "upstream stream error"
	}
	return fmt.Errorf("%w: %s", errUpstreamStreamError, msg)
}

func inspectOpenAIResponseEvent(data []byte) error {
	var probe struct {
		Type     string `json:"type"`
		Response *struct {
			Status string `json:"status"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		} `json:"response"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}

	switch probe.Type {
	case "response.failed":
		if probe.Response != nil && probe.Response.Error != nil {
			return fmt.Errorf("%w: %s", errUpstreamStreamError, probe.Response.Error.Message)
		}
		return fmt.Errorf("%w: response failed", errUpstreamStreamError)
	case "response.incomplete":
		return fmt.Errorf("%w: response incomplete", errUpstreamStreamError)
	case "response.cancelled", "response.canceled":
		return fmt.Errorf("%w: response cancelled", errUpstreamStreamError)
	case "error":
		msg := probe.Message
		if msg == "" {
			msg = "responses stream error"
		}
		return fmt.Errorf("%w: %s", errUpstreamStreamError, msg)
	}

	return nil
}

func inspectAnthropicEvent(fullOutput []byte, data []byte) error {
	// Anthropic SSE may have an "event:" line. Check it first.
	eventType := extractSSEEventType(fullOutput)

	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}

	if eventType == "error" || probe.Type == "error" {
		var failure struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		msg := failure.Error.Message
		if msg == "" {
			msg = "anthropic stream error"
		}
		return fmt.Errorf("%w: %s", errUpstreamStreamError, msg)
	}

	return nil
}

func extractSSEEventType(output []byte) string {
	s := string(output)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			return strings.TrimSpace(line[6:])
		}
	}
	return ""
}
