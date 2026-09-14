package openai

import (
	"encoding/json"
	"fmt"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// Patch only search tools in AxonHub's output. Replacing the entire tools
// array would discard native tools/namespaces and raw function extensions.
func overlayResponsesSearchTools(body []byte, tools []model.Tool) ([]byte, error) {
	var searches []ResponsesTool
	for _, tool := range tools {
		if tool.Type == "web_search" {
			searches = append(searches, convertToolsToResponses([]model.Tool{tool})...)
		}
	}
	if len(searches) == 0 {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var wireTools []json.RawMessage
	if err := json.Unmarshal(payload["tools"], &wireTools); err != nil {
		return nil, err
	}
	index := 0
	for i, raw := range wireTools {
		var tool map[string]json.RawMessage
		if json.Unmarshal(raw, &tool) != nil {
			continue
		}
		kind := decodeRawString(tool["type"])
		if kind != "web_search" && kind != "web_search_preview" && kind != "web_search_preview_2025_03_11" {
			continue
		}
		if index >= len(searches) {
			return nil, fmt.Errorf("unexpected extra Responses search tool after conversion")
		}
		overlay, err := json.Marshal(searches[index])
		if err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(overlay, &fields)
		for key, value := range fields {
			tool[key] = value
		}
		wireTools[i], err = json.Marshal(tool)
		if err != nil {
			return nil, err
		}
		index++
	}
	if index != len(searches) {
		return nil, fmt.Errorf("Responses search tool lost during conversion")
	}
	payload["tools"], _ = json.Marshal(wireTools)
	return json.Marshal(payload)
}
