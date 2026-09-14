package openai

import (
	"context"
	"encoding/json"
	inboundOpenAI "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/samber/lo"
	"io"
	"testing"
)

// Strict Responses providers correlate tool results by call_id and reject
// the nested item_reference field added by older relay versions.
func TestResponsesToolOutputsUseCallID(t *testing.T) {
	msgs := []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "call_a", Type: "function", Function: model.FunctionCall{Name: "weather", Arguments: `{}`}},
			{ID: "call_b", Type: "function", Function: model.FunctionCall{Name: "time", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: lo.ToPtr("call_a"), Content: model.MessageContent{Content: lo.ToPtr("sunny")}},
		{Role: "tool", ToolCallID: lo.ToPtr("call_b"), Content: model.MessageContent{Content: lo.ToPtr("noon")}},
	}
	raw, err := MarshalResponsesInputItems(msgs)
	if err != nil {
		t.Fatal(err)
	}
	assertResponsesToolPairs(t, raw)
	req := &model.InternalLLMRequest{Model: "gpt-5.6-luna", Messages: msgs, TransformOptions: model.TransformOptions{ArrayInputs: lo.ToPtr(true)}}
	httpReq, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	defer httpReq.Body.Close()
	body, _ := io.ReadAll(httpReq.Body)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	assertResponsesToolPairs(t, payload["input"])
}

func assertResponsesToolPairs(t *testing.T, raw []byte) {
	t.Helper()
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("expected two calls and two results, got %s", raw)
	}
	for i, want := range []struct{ kind, id string }{
		{"function_call", "call_a"}, {"function_call", "call_b"},
		{"function_call_output", "call_a"}, {"function_call_output", "call_b"},
	} {
		if string(items[i]["type"]) != `"`+want.kind+`"` || string(items[i]["call_id"]) != `"`+want.id+`"` {
			t.Fatalf("tool pairing/order changed: %s", raw)
		}
		if _, present := items[i]["item_reference"]; present {
			t.Fatalf("unexpected nested item_reference: %s", raw)
		}
	}
	if string(items[2]["output"]) != `"sunny"` || string(items[3]["output"]) != `"noon"` {
		t.Fatalf("tool output changed: %s", raw)
	}
}

func TestResponsesRawToolHistoryPreservedWithoutInjectedReferences(t *testing.T) {
	raw := json.RawMessage(`[
  {"type":"function_call","call_id":"call_a","name":"weather","arguments":"{}"},
  {"type":"function_call","call_id":"call_b","name":"time","arguments":"{}"},
  {"type":"function_call_output","call_id":"call_a","output":"sunny"},
  {"type":"function_call_output","call_id":"call_b","output":"noon"}
 ]`)
	if got := sanitizeResponsesRawItems(raw); string(got) != string(raw) {
		t.Fatalf("valid raw history rewritten: %s", got)
	}
	req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{"model":"gpt-5.6-luna","input":`+string(raw)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	// Semantic group overrides force this normalized path in the live CPA case.
	req.ReasoningEffort = "max"
	httpReq, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	defer httpReq.Body.Close()
	body, _ := io.ReadAll(httpReq.Body)
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(body, &payload)
	assertResponsesToolPairs(t, payload["input"])
}

func TestSanitizeResponsesRemovesOnlyLegacyNestedReferences(t *testing.T) {
	for _, ref := range []string{`null`, `""`, `"fc_old"`} {
		raw := json.RawMessage(`[
   {"type":"function_call","id":"fc_old","call_id":"call_a","name":"f","arguments":"{}"},
   {"type":"function_call_output","call_id":"call_a","item_reference":` + ref + `,"output":"done"},
   {"type":"custom_tool_call_output","call_id":"custom_a","item_reference":` + ref + `,"output":"custom done"},
   {"type":"item_reference","id":"msg_saved"}
  ]`)
		var items []map[string]json.RawMessage
		_ = json.Unmarshal(sanitizeResponsesRawItems(raw), &items)
		for _, i := range []int{1, 2} {
			if _, present := items[i]["item_reference"]; present {
				t.Fatalf("legacy nested reference survived: %v", items[i])
			}
		}
		if string(items[0]["id"]) != `"fc_old"` || string(items[3]["type"]) != `"item_reference"` || string(items[3]["id"]) != `"msg_saved"` {
			t.Fatalf("valid IDs or standalone reference changed: %v", items)
		}
	}
}
