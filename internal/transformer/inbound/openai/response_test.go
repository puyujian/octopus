package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

func TestConvertToInternalRequestPreservesRawInputItems(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: ResponsesInput{Items: []ResponsesItem{
			{Type: "input_text", Text: stringPtr("hello")},
		}},
	}

	internalReq, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatalf("convertToInternalRequest failed: %v", err)
	}
	if len(internalReq.RawInputItems) == 0 {
		t.Fatalf("expected raw input items to be preserved")
	}

	var items []map[string]any
	if err := json.Unmarshal(internalReq.RawInputItems, &items); err != nil {
		t.Fatalf("unmarshal raw input items failed: %v", err)
	}
	if len(items) != 1 || items[0]["type"] != "input_text" {
		t.Fatalf("expected original raw input items to be kept, got %#v", items)
	}
	if internalReq.TransformOptions.ArrayInputs == nil || !*internalReq.TransformOptions.ArrayInputs {
		t.Fatalf("expected array input flag to stay true")
	}
}

func TestConvertToInternalRequestGroupsParallelFunctionCalls(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: ResponsesInput{Items: []ResponsesItem{
			{
				Type: "message",
				Role: "user",
				Content: &ResponsesInput{Items: []ResponsesItem{{
					Type: "input_text",
					Text: stringPtr("run both"),
				}}},
			},
			{Type: "function_call", CallID: "call_a", Name: "first", Arguments: `{}`},
			{Type: "function_call", CallID: "call_b", Name: "second", Arguments: `{}`},
			{Type: "function_call_output", CallID: "call_a", Output: &ResponsesInput{Text: stringPtr("ok-a")}},
			{Type: "function_call_output", CallID: "call_b", Output: &ResponsesInput{Text: stringPtr("ok-b")}},
			{Type: "function_call", CallID: "call_c", Name: "third", Arguments: `{}`},
			{Type: "function_call_output", CallID: "call_c", Output: &ResponsesInput{Text: stringPtr("ok-c")}},
		}},
	}

	internalReq, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatalf("convertToInternalRequest failed: %v", err)
	}
	if len(internalReq.Messages) != 6 {
		t.Fatalf("expected two separate tool-call rounds, got %#v", internalReq.Messages)
	}
	assistant := internalReq.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 2 {
		t.Fatalf("expected one assistant turn with two tool calls, got %#v", assistant)
	}
	if assistant.ToolCalls[0].ID != "call_a" || assistant.ToolCalls[1].ID != "call_b" {
		t.Fatalf("parallel tool call IDs were not preserved: %#v", assistant.ToolCalls)
	}
	for index, wantID := range []string{"call_a", "call_b"} {
		toolMessage := internalReq.Messages[index+2]
		if toolMessage.Role != "tool" || toolMessage.ToolCallID == nil || *toolMessage.ToolCallID != wantID {
			t.Fatalf("tool output %d does not match %s: %#v", index, wantID, toolMessage)
		}
	}
	secondAssistant := internalReq.Messages[4]
	if secondAssistant.Role != "assistant" || len(secondAssistant.ToolCalls) != 1 || secondAssistant.ToolCalls[0].ID != "call_c" {
		t.Fatalf("expected the next tool-call round to stay separate, got %#v", secondAssistant)
	}
	if secondTool := internalReq.Messages[5]; secondTool.Role != "tool" || secondTool.ToolCallID == nil || *secondTool.ToolCallID != "call_c" {
		t.Fatalf("expected the second-round tool output to stay matched, got %#v", secondTool)
	}
}

func TestResponsesParallelFunctionCallsProduceValidChatWireOrder(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run both"}]},
			{"type":"function_call","call_id":"call_a","name":"first","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"second","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"ok-a"},
			{"type":"function_call_output","call_id":"call_b","output":"ok-b"}
		],
		"stream":false
	}`)

	internalReq, err := (&ResponseInbound{}).TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatalf("transform Responses request failed: %v", err)
	}
	httpReq, err := (&openaiOutbound.ChatOutbound{}).TransformRequest(
		context.Background(), internalReq, "https://api.example.com/v1", "test-key",
	)
	if err != nil {
		t.Fatalf("transform Chat request failed: %v", err)
	}
	wireBody, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read Chat request failed: %v", err)
	}

	var payload struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(wireBody, &payload); err != nil {
		t.Fatalf("decode Chat request failed: %v; body=%s", err, wireBody)
	}
	if len(payload.Messages) != 4 {
		t.Fatalf("expected four Chat messages, got %#v", payload.Messages)
	}
	if payload.Messages[1].Role != "assistant" || len(payload.Messages[1].ToolCalls) != 2 {
		t.Fatalf("expected grouped assistant tool calls on the wire, got %#v", payload.Messages[1])
	}
	if payload.Messages[1].ToolCalls[0].ID != "call_a" || payload.Messages[1].ToolCalls[1].ID != "call_b" {
		t.Fatalf("unexpected wire tool calls: %#v", payload.Messages[1].ToolCalls)
	}
	if payload.Messages[2].Role != "tool" || payload.Messages[2].ToolCallID != "call_a" ||
		payload.Messages[3].Role != "tool" || payload.Messages[3].ToolCallID != "call_b" {
		t.Fatalf("tool results are not immediately after their assistant turn: %#v", payload.Messages)
	}
}

func TestConvertToInternalRequestMarksPassthroughForUnsupportedToolType(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: ResponsesInput{Text: stringPtr("hello")},
		Tools: []ResponsesTool{{
			Type: "apply_patch",
		}},
	}

	internalReq, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatalf("convertToInternalRequest failed: %v", err)
	}
	if !internalReq.HasOpenAIResponsesPassthrough() {
		t.Fatalf("expected unsupported responses tool to require passthrough")
	}
	if ext := internalReq.GetOpenAIExtensions(); !ext.ResponsesPassthroughRequired || ext.ResponsesPassthroughReason != "tool:apply_patch" {
		t.Fatalf("expected OpenAI extension passthrough view, got %#v", ext)
	}
}

func TestConvertToInternalRequestMarksPassthroughForUnsupportedInputItem(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: ResponsesInput{Items: []ResponsesItem{{
			Type:   "apply_patch_call_output",
			CallID: "apc_123",
		}}},
	}

	internalReq, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatalf("convertToInternalRequest failed: %v", err)
	}
	if !internalReq.HasOpenAIResponsesPassthrough() {
		t.Fatalf("expected unsupported responses input item to require passthrough")
	}
	if ext := internalReq.GetOpenAIExtensions(); !ext.ResponsesPassthroughRequired || ext.ResponsesPassthroughReason != "input:apply_patch_call_output" {
		t.Fatalf("expected OpenAI extension passthrough view, got %#v", ext)
	}
}

func TestConvertToInternalRequestPreservesFileAndAudioInputsAsRawFragments(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: ResponsesInput{Items: []ResponsesItem{
			{
				Type: "message",
				Role: "user",
				Content: &ResponsesInput{Items: []ResponsesItem{
					{Type: "input_file", FileID: stringPtr("file_123")},
					{Type: "input_audio", InputAudio: &ResponsesInputAudio{Format: "wav", Data: "AAA="}},
				}},
			},
		}},
	}

	internalReq, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatalf("convertToInternalRequest failed: %v", err)
	}
	if !internalReq.HasOpenAIResponsesPassthrough() {
		t.Fatalf("expected file/audio inputs to require Responses raw preservation")
	}
	if len(internalReq.Messages) != 1 || len(internalReq.Messages[0].Content.MultipleContent) != 2 {
		t.Fatalf("expected supported file/audio inputs to normalize into message content, got %#v", internalReq.Messages)
	}
	if internalReq.Messages[0].Content.MultipleContent[0].Type != "file" {
		t.Fatalf("expected file content part, got %#v", internalReq.Messages[0].Content.MultipleContent[0])
	}
	if internalReq.Messages[0].Content.MultipleContent[1].Type != "input_audio" {
		t.Fatalf("expected input_audio content part, got %#v", internalReq.Messages[0].Content.MultipleContent[1])
	}
	ext := internalReq.GetOpenAIResponsesOptions()
	if len(ext.RawInputFragments) != 1 || ext.RawInputFragments[0].Type != "message" {
		t.Fatalf("expected mixed message to be retained as one raw fragment, got %#v", ext.RawInputFragments)
	}
}

func TestConvertToInternalRequestNormalizesTopLevelInputFile(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: ResponsesInput{Items: []ResponsesItem{{
			Type:     "input_file",
			FileID:   stringPtr("file_456"),
			Filename: stringPtr("notes.txt"),
		}}},
	}

	internalReq, err := convertToInternalRequest(req)
	if err != nil {
		t.Fatalf("convertToInternalRequest failed: %v", err)
	}
	if !internalReq.HasOpenAIResponsesPassthrough() {
		t.Fatalf("expected top-level input_file to require Responses raw preservation")
	}
	if len(internalReq.Messages) != 1 {
		t.Fatalf("expected one normalized message, got %#v", internalReq.Messages)
	}
	if internalReq.Messages[0].Role != "user" {
		t.Fatalf("expected top-level input_file to default to user role, got %#v", internalReq.Messages[0].Role)
	}
	if len(internalReq.Messages[0].Content.MultipleContent) != 1 || internalReq.Messages[0].Content.MultipleContent[0].Type != "file" {
		t.Fatalf("expected top-level input_file to become file content, got %#v", internalReq.Messages[0].Content)
	}
	if internalReq.Messages[0].Content.MultipleContent[0].File == nil || internalReq.Messages[0].Content.MultipleContent[0].File.FileID != "file_456" {
		t.Fatalf("expected normalized file reference to preserve file_id, got %#v", internalReq.Messages[0].Content.MultipleContent[0].File)
	}
	ext := internalReq.GetOpenAIResponsesOptions()
	if len(ext.RawInputFragments) != 1 || ext.RawInputFragments[0].Type != "input_file" {
		t.Fatalf("expected top-level input_file raw fragment, got %#v", ext.RawInputFragments)
	}
}

func stringPtr(value string) *string {
	return &value
}
