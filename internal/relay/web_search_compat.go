package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
)

type webSearchWireFormat string

const (
	webSearchPreview webSearchWireFormat = "web_search_preview"
	webSearchNested  webSearchWireFormat = "web_search.enable"
)

var rejectedToolIndex = regexp.MustCompile(`tools\[(\d+)\]\.(type|web_search|function)`)

// sendRequest adapts only explicitly rejected search-tool schemas. A gateway
// may balance between providers with different dialects, so do not cache one
// dialect for the entire channel. Each alternative is tried at most once and
// never removes search or weakens a domain/offline constraint.
func (ra *relayAttempt) sendRequest(req *http.Request) (*http.Response, error) {
	tried := make(map[webSearchWireFormat]bool)
	for {
		response, err := ra.sendRequestWithPromptCacheKeyCompatibility(req)
		if err != nil || response == nil || response.StatusCode != http.StatusBadRequest ||
			ra.channel.Type != outbound.OutboundTypeOpenAIResponse || len(tried) >= 2 {
			return response, err
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("failed to read rejected search response: %w", readErr)
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		requestBody, err := readOutboundRequestBody(req)
		if err != nil {
			return nil, err
		}
		modified, format := adaptRejectedWebSearch(requestBody, body)
		if len(modified) == 0 || tried[format] {
			return response, nil
		}
		tried[format] = true
		setPromptCacheCompatibilityBody(req, modified)
		ra.metrics.SetTransportRequestPayload(modified, ra.internalRequest.Model)
		ra.retryAfter = 0
		log.Warnf("channel %s model %s rejected web search schema; retrying with %s", ra.channel.Name, ra.internalRequest.Model, format)
	}
}

func adaptRejectedWebSearch(requestBody, errorBody []byte) ([]byte, webSearchWireFormat) {
	var rejection struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(errorBody, &rejection) != nil {
		return nil, ""
	}
	message := strings.ToLower(rejection.Error.Message)
	var payload map[string]json.RawMessage
	if json.Unmarshal(requestBody, &payload) != nil {
		return nil, ""
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(payload["tools"], &tools) != nil {
		return nil, ""
	}
	index := -1
	if match := rejectedToolIndex.FindStringSubmatch(message); len(match) > 0 {
		index, _ = strconv.Atoi(match[1])
	}
	var format webSearchWireFormat
	switch {
	case index >= 0 && strings.Contains(message, ".web_search") &&
		(strings.Contains(message, "can not be null") || strings.Contains(message, "cannot be null") || strings.Contains(message, "must not be null")):
		format = webSearchNested
	case index >= 0 && strings.Contains(message, ".type") && strings.Contains(message, "web_search_preview") &&
		(strings.Contains(message, "input should be") || strings.Contains(message, "must be one of") || strings.Contains(message, "supported types")):
		format = webSearchPreview
	case strings.EqualFold(rejection.Error.Code, "MissingParameter") && strings.Contains(message, "missing `***.function` parameter"):
		// This legacy gateway error omits the index. Only attribute it to
		// search when all other tools are well-formed Responses functions.
		for i, tool := range tools {
			if isResponsesSearchTool(rawJSONString(tool["type"])) {
				if index >= 0 {
					return nil, ""
				}
				index = i
			} else if rawJSONString(tool["type"]) != "function" || strings.TrimSpace(rawJSONString(tool["name"])) == "" {
				return nil, ""
			}
		}
		format = webSearchPreview
	default:
		return nil, ""
	}
	if index < 0 || index >= len(tools) {
		return nil, ""
	}
	tool := tools[index]
	oldType := rawJSONString(tool["type"])
	if !isResponsesSearchTool(oldType) {
		return nil, ""
	}
	// Legacy preview supports location/context size, but not domain filters,
	// offline-only search or modern token budgets. Nested provider search is
	// safe only for a bare tool with no client search controls.
	for field := range tool {
		if field == "type" || field == "web_search" {
			continue
		}
		if format == webSearchPreview && (field == "user_location" || field == "search_context_size") {
			continue
		}
		return nil, ""
	}
	newType := "web_search"
	if format == webSearchPreview {
		if oldType == "web_search_preview" || oldType == "web_search_preview_2025_03_11" {
			return nil, ""
		}
		if raw, present := tool["web_search"]; present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var config map[string]json.RawMessage
			if json.Unmarshal(raw, &config) != nil || len(config) != 1 || string(config["enable"]) != "true" {
				return nil, ""
			}
		}
		newType = "web_search_preview"
		delete(tool, "web_search")
	} else {
		// Never overwrite an explicit provider configuration, including a
		// disabled search flag. Only fill the missing/null object it rejected.
		if raw, present := tool["web_search"]; present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, ""
		}
		tool["web_search"] = json.RawMessage(`{"enable":true}`)
	}
	tool["type"], _ = json.Marshal(newType)
	payload["tools"], _ = json.Marshal(tools)
	if oldType != newType {
		payload["tool_choice"] = rewriteSearchToolChoice(payload["tool_choice"], oldType, newType)
		if len(payload["tool_choice"]) == 0 {
			delete(payload, "tool_choice")
		}
	}
	modified, err := json.Marshal(payload)
	if err != nil {
		return nil, ""
	}
	return modified, format
}

func rawJSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func isResponsesSearchTool(toolType string) bool {
	return toolType == "web_search" || toolType == "web_search_preview" || toolType == "web_search_preview_2025_03_11"
}

func rewriteSearchToolChoice(raw json.RawMessage, oldType, newType string) json.RawMessage {
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return raw
	}
	if rawJSONString(choice["type"]) == oldType {
		choice["type"], _ = json.Marshal(newType)
	}
	if rawJSONString(choice["type"]) == "allowed_tools" {
		var options []json.RawMessage
		if json.Unmarshal(choice["tools"], &options) == nil {
			for i := range options {
				options[i] = rewriteSearchToolChoice(options[i], oldType, newType)
			}
			choice["tools"], _ = json.Marshal(options)
		}
	}
	modified, err := json.Marshal(choice)
	if err != nil {
		return raw
	}
	return modified
}
