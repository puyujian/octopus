package anthropic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// Buffer search metadata until the terminal snapshot: output_item.done may
// be sparse, and sources can arrive only in response.completed/incomplete.
func searchResponseBlocks(calls []model.WebSearchCall) []MessageContentBlock {
	var merged []model.WebSearchCall
	for _, call := range calls {
		merged = model.MergeWebSearchCalls(merged, call)
	}
	var blocks []MessageContentBlock
	for _, call := range merged {
		sum := sha256.Sum256([]byte(call.ID))
		id := "srvtoolu_" + hex.EncodeToString(sum[:12])
		name := "web_search"
		input := map[string]any{}
		if call.Action != nil {
			if call.Action.Query != "" {
				input["query"] = call.Action.Query
			} else if len(call.Action.Queries) > 0 {
				input["query"] = call.Action.Queries[0]
			}
		}
		rawInput, _ := json.Marshal(input)
		blocks = append(blocks, MessageContentBlock{Type: "server_tool_use", ID: id, Name: &name, Input: rawInput})
		results := make([]map[string]string, 0)
		if call.Action != nil {
			for _, source := range call.Action.Sources {
				if source.URL == "" {
					continue
				}
				title := source.Title
				if title == "" {
					title = source.URL
				}
				results = append(results, map[string]string{"type": "web_search_result", "url": source.URL, "title": title})
			}
		}
		var rawResults json.RawMessage
		if call.Status == "failed" || call.Status == "cancelled" || call.Status == "in_progress" || call.Status == "searching" {
			rawResults = json.RawMessage(`{"type":"web_search_tool_result_error","error_code":"unavailable"}`)
		} else {
			rawResults, _ = json.Marshal(results)
		}
		blocks = append(blocks, MessageContentBlock{Type: "web_search_tool_result", ToolUseID: &id, Content: &MessageContent{Raw: rawResults}})
	}
	return blocks
}
