package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func (o *ResponseOutbound) responsesSearchEvents(event ResponsesStreamEvent) []model.StreamEvent {
	var updates []ResponsesItem
	if event.Item != nil && event.Item.Type == "web_search_call" {
		updates = append(updates, *event.Item)
	}
	if event.Response != nil {
		for _, item := range event.Response.Output {
			if item.Type == "web_search_call" {
				updates = append(updates, item)
			}
		}
	}
	switch event.Type {
	case "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		id := valueOrEmpty(event.ItemID)
		if id == "" {
			id = o.outputItems[event.OutputIndex].ID
		}
		if id != "" {
			status := strings.TrimPrefix(event.Type, "response.web_search_call.")
			updates = append(updates, ResponsesItem{ID: id, Type: "web_search_call", Status: &status})
		}
	}
	if len(updates) == 0 {
		return nil
	}
	for _, item := range updates {
		o.searchCalls = model.MergeWebSearchCalls(o.searchCalls, model.WebSearchCall{ID: item.ID, Status: valueOrEmpty(item.Status), Action: item.Action})
	}
	events := make([]model.StreamEvent, 0, len(o.searchCalls))
	for _, call := range o.searchCalls {
		call := call
		events = append(events, model.StreamEvent{Kind: model.StreamEventKindWebSearch, ID: o.streamID, Model: o.streamModel, WebSearchCall: &call})
	}
	return events
}

func prepareAnthropicResponsesRequest(request *model.InternalLLMRequest) (*model.InternalLLMRequest, error) {
	if request.RawAPIFormat != model.APIFormatAnthropicMessage {
		return request, nil
	}
	prepared := *request
	prepared.Tools = append([]model.Tool(nil), request.Tools...)
	prepared.Messages = append([]model.Message(nil), request.Messages...)
	for index := range prepared.Messages {
		message := &prepared.Messages[index]
		parts := append([]model.MessageContentPart(nil), message.Content.MultipleContent...)
		for partIndex := range parts {
			part := &parts[partIndex]
			text := ""
			if part.ServerToolUse != nil && part.ServerToolUse.Name == "web_search" {
				text = "Web search " + part.ServerToolUse.ID + ": " + string(part.ServerToolUse.Input)
			} else if part.ServerToolResult != nil && part.ServerToolResult.BlockType == "web_search_tool_result" {
				text = "Web search result " + part.ServerToolResult.ToolUseID + ": " + string(part.ServerToolResult.Content)
			}
			if text != "" {
				*part = model.MessageContentPart{Type: "text", Text: &text, CacheControl: part.CacheControl}
			}
		}
		message.Content.MultipleContent = parts
	}
	hasSearch := false
	for index := range prepared.Tools {
		tool := &prepared.Tools[index]
		if strings.HasPrefix(tool.Type, "web_search_") {
			search := &model.WebSearch{}
			if err := json.Unmarshal(tool.AnthropicServerSpec, search); err != nil {
				return nil, fmt.Errorf("invalid Anthropic search tool: %w", err)
			}
			tool.Type, tool.WebSearch = "web_search", search
		}
		if tool.Type == "web_search" {
			hasSearch = true
		}
	}
	if hasSearch {
		prepared.Include = append([]string(nil), request.Include...)
		found := false
		for _, include := range prepared.Include {
			if include == "web_search_call.action.sources" {
				found = true
			}
		}
		if !found {
			prepared.Include = append(prepared.Include, "web_search_call.action.sources")
		}
	}
	if choice := request.ToolChoice; choice != nil {
		if choice.ToolChoice != nil && *choice.ToolChoice == "any" {
			mode := "required"
			prepared.ToolChoice = &model.ToolChoice{ToolChoice: &mode}
		} else if named := choice.NamedToolChoice; named != nil {
			copied := *named
			name := named.ResolvedFunctionName()
			if named.Type == "tool" {
				copied.Type = "function"
				for _, tool := range prepared.Tools {
					if tool.Type == "web_search" && tool.Function.Name == name {
						copied.Type = "web_search"
					}
				}
				// A declared client function wins a name collision.
				for _, tool := range prepared.Tools {
					if tool.Type == "function" && tool.Function.Name == name {
						copied.Type = "function"
					}
				}
				if copied.Type == "web_search" {
					copied.Function, copied.Name = nil, nil
				}
			}
			switch named.Type {
			case "auto", "none", "any":
				mode := named.Type
				if mode == "any" {
					mode = "required"
				}
				prepared.ToolChoice = &model.ToolChoice{ToolChoice: &mode}
			default:
				prepared.ToolChoice = &model.ToolChoice{NamedToolChoice: &copied}
			}
			if named.DisableParallelToolUse != nil {
				parallel := !*named.DisableParallelToolUse
				prepared.ParallelToolCalls = &parallel
			}
		}
	}
	return &prepared, nil
}

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
