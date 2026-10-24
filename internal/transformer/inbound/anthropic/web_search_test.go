package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestSearchBlocksDoNotInventCallsOrQueries(t *testing.T) {
	if blocks := searchResponseBlocks([]model.WebSearchCall{{Status: "completed"}}); len(blocks) != 0 {
		t.Fatalf("invented call: %+v", blocks)
	}
	blocks := searchResponseBlocks([]model.WebSearchCall{{ID: "ws_a", Status: "completed"}})
	if len(blocks) != 2 || string(blocks[0].Input) != "{}" || string(blocks[1].Content.Raw) != "[]" {
		t.Fatalf("invented query or sources: %+v", blocks)
	}
}

func TestFailedSearchResultSurvivesHistory(t *testing.T) {
	blocks := searchResponseBlocks([]model.WebSearchCall{{ID: "ws_a", Status: "failed", Action: &model.WebSearchAction{Query: "query"}}})
	if len(blocks) != 2 || !bytes.Contains(blocks[1].Content.Raw, []byte("web_search_tool_result_error")) {
		t.Fatalf("missing failed result: %+v", blocks)
	}
	raw, _ := json.Marshal(map[string]any{"model": "test", "messages": []any{map[string]any{"role": "assistant", "content": blocks}}})
	got, err := (&MessagesInbound{}).TransformRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := got.Messages[0].Content.MultipleContent
	if len(parts) != 2 || parts[1].ServerToolResult == nil || !bytes.Equal(parts[1].ServerToolResult.Content, blocks[1].Content.Raw) {
		t.Fatalf("error result changed: %+v", parts)
	}
}
