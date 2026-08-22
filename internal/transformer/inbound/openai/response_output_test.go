package openai

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestStreamCompletedEventHasNonEmptyOutputWithMessage(t *testing.T) {
	text := "hello"
	stop := "stop"
	chunks := []*model.InternalLLMResponse{
		{
			ID:     "resp_01",
			Model:  "gpt-4o",
			Object: "chat.completion.chunk",
			Choices: []model.Choice{{
				Index: 0,
				Delta: &model.Message{
					Role:    "assistant",
					Content: model.MessageContent{Content: &text},
				},
			}},
		},
		{
			ID:     "resp_01",
			Model:  "gpt-4o",
			Object: "chat.completion.chunk",
			Choices: []model.Choice{{
				Index:        0,
				Delta:        &model.Message{Role: "assistant"},
				FinishReason: &stop,
			}},
		},
		{
			ID:     "resp_01",
			Model:  "gpt-4o",
			Object: "chat.completion.chunk",
			Usage: &model.Usage{
				PromptTokens:     1,
				CompletionTokens: 1,
				TotalTokens:      2,
			},
		},
	}
	events := feedStream(t, chunks)

	var completed *ResponsesStreamEvent
	for i := range events {
		if events[i].Type == "response.completed" {
			completed = &events[i]
			break
		}
	}
	if completed == nil || completed.Response == nil {
		t.Fatalf("expected response.completed event, got events: %+v", eventTypes(events))
	}
	if len(completed.Response.Output) == 0 {
		t.Fatalf("response.completed.output must be non-empty (O-H3)")
	}
	first := completed.Response.Output[0]
	if first.Type != "message" {
		t.Fatalf("first output type = %q, want message", first.Type)
	}
}

func TestStreamCompletedSynthesizesShellWhenEmpty(t *testing.T) {
	stop := "stop"
	chunks := []*model.InternalLLMResponse{
		{
			ID:     "resp_02",
			Model:  "gpt-4o",
			Object: "chat.completion.chunk",
			Choices: []model.Choice{{
				Index:        0,
				Delta:        &model.Message{Role: "assistant"},
				FinishReason: &stop,
			}},
		},
		{
			ID:    "resp_02",
			Model: "gpt-4o",
			Usage: &model.Usage{
				PromptTokens:     0,
				CompletionTokens: 0,
				TotalTokens:      0,
			},
		},
	}
	events := feedStream(t, chunks)

	var completed *ResponsesStreamEvent
	for i := range events {
		if events[i].Type == "response.completed" {
			completed = &events[i]
		}
	}
	if completed == nil || completed.Response == nil {
		t.Fatalf("expected response.completed event")
	}
	if len(completed.Response.Output) == 0 {
		t.Fatalf("output must be non-empty even when no items were emitted")
	}
	first := completed.Response.Output[0]
	if first.Type != "message" {
		t.Fatalf("synthetic output type = %q, want message", first.Type)
	}
	if first.Status == nil || *first.Status != "completed" {
		t.Fatalf("synthetic status = %v, want completed", first.Status)
	}
	_ = lo.ToPtr("ignore")
}

func TestConvertToResponsesAPIResponsePreservesRefusalContent(t *testing.T) {
	stop := "refusal"
	resp := &model.InternalLLMResponse{
		ID:      "resp_refusal",
		Model:   "gpt-4o",
		Created: 123,
		Choices: []model.Choice{{
			Message: &model.Message{
				Role:    "assistant",
				Refusal: "I cannot help with that.",
			},
			FinishReason: &stop,
		}},
	}

	out := convertToResponsesAPIResponse(resp)
	if len(out.Output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(out.Output))
	}
	msg := out.Output[0]
	if msg.Type != "message" || msg.Content == nil || len(msg.Content.Items) != 1 {
		t.Fatalf("unexpected message shape: %+v", msg)
	}
	part := msg.Content.Items[0]
	if part.Type != "refusal" || part.Refusal == nil || *part.Refusal != "I cannot help with that." {
		t.Fatalf("expected refusal content item, got %+v", part)
	}
	if out.Status == nil || *out.Status != "failed" {
		t.Fatalf("expected failed status for refusal stop, got %v", out.Status)
	}
}

func TestConvertToResponsesAPIResponsePreservesReasoningToolsAndAnnotations(t *testing.T) {
	start, end := int64(0), int64(7)
	resp := &model.InternalLLMResponse{
		ID:      "resp_semantics",
		Model:   "gpt-5",
		Created: 456,
		Choices: []model.Choice{{
			Message: &model.Message{
				ID:   "msg_semantics",
				Role: "assistant",
				ReasoningItems: []model.ReasoningItem{
					{ID: "rsn_1", Content: "first thought", Signature: "sig_1"},
					{ID: "rsn_2", Signature: "sig_2"},
				},
				ToolCalls: []model.ToolCall{
					{
						ID:   "call_fn",
						Type: "function",
						Function: model.FunctionCall{
							Name:      "lookup",
							Namespace: "docs",
							Arguments: `{"q":"octopus"}`,
						},
					},
					{
						ID:   "item_custom",
						Type: "responses_custom_tool",
						ResponseCustomToolCall: &model.ResponseCustomToolCall{
							CallID: "call_custom",
							Name:   "apply_patch",
							Input:  `{"patch":"..."}`,
						},
					},
				},
				InlineToolResults: []model.InlineToolResult{{
					ToolCallID: "call_server",
					Output:     `{"source":"web"}`,
				}},
				Content: model.MessageContent{Content: stringPtr("answer")},
				Annotations: []model.Annotation{{
					Type:       "url_citation",
					StartIndex: &start,
					EndIndex:   &end,
					URLCitation: &model.URLCitation{
						URL:   "https://example.com",
						Title: "Example",
					},
				}},
			},
		}},
	}

	out := convertToResponsesAPIResponse(resp)
	if len(out.Output) != 6 {
		t.Fatalf("expected two reasoning items, two tool calls, one inline result and one message, got %d: %#v", len(out.Output), out.Output)
	}
	if out.Output[0].Type != "reasoning" || out.Output[0].ID != "rsn_1" || out.Output[0].EncryptedContent == nil || *out.Output[0].EncryptedContent != "sig_1" {
		t.Fatalf("first reasoning item was not preserved: %#v", out.Output[0])
	}
	if out.Output[1].Type != "reasoning" || out.Output[1].ID != "rsn_2" || out.Output[1].EncryptedContent == nil || *out.Output[1].EncryptedContent != "sig_2" {
		t.Fatalf("signature-only reasoning item was not preserved: %#v", out.Output[1])
	}
	if out.Output[2].Type != "function_call" || out.Output[2].Namespace != "docs" {
		t.Fatalf("function namespace was not preserved: %#v", out.Output[2])
	}
	if out.Output[3].Type != "custom_tool_call" || out.Output[3].CallID != "call_custom" || out.Output[3].Input == nil {
		t.Fatalf("custom tool call was not preserved: %#v", out.Output[3])
	}
	if out.Output[4].Type != "function_call_output" || out.Output[4].CallID != "call_server" || out.Output[4].Output == nil || out.Output[4].Output.Text == nil {
		t.Fatalf("inline tool result was not preserved: %#v", out.Output[4])
	}
	message := out.Output[5]
	if message.ID != "msg_semantics" || message.Content == nil || len(message.Content.Items) != 1 {
		t.Fatalf("message item was not preserved: %#v", message)
	}
	annotations := message.Content.Items[0].Annotations
	if annotations == nil || len(*annotations) != 1 || (*annotations)[0].URL == nil || *(*annotations)[0].URL != "https://example.com" {
		t.Fatalf("message annotations were not preserved: %#v", annotations)
	}
}

func TestConvertToResponsesAPIResponsePreservesCompactionAndAudio(t *testing.T) {
	resp := &model.InternalLLMResponse{
		ID:    "resp_compact",
		Model: "gpt-5",
		Choices: []model.Choice{{Message: &model.Message{
			Role: "assistant",
			Audio: &struct {
				Data       string `json:"data,omitempty"`
				ExpiresAt  int64  `json:"expires_at,omitempty"`
				ID         string `json:"id,omitempty"`
				Transcript string `json:"transcript,omitempty"`
			}{ID: "aud_1", Data: "AAA=", ExpiresAt: 123, Transcript: "hello"},
			Content: model.MessageContent{MultipleContent: []model.MessageContentPart{
				{Type: "text", Text: stringPtr("before")},
				{Type: "compaction_summary", Compact: &model.CompactContent{
					ID: "cmp_1", EncryptedContent: "compact-secret", CreatedBy: stringPtr("server"),
				}},
			}},
		}}},
	}

	out := convertToResponsesAPIResponse(resp)
	if len(out.Output) != 2 {
		t.Fatalf("expected compaction and audio-bearing message, got %#v", out.Output)
	}
	if out.Output[0].Type != "compaction_summary" || out.Output[0].ID != "cmp_1" || out.Output[0].EncryptedContent == nil || *out.Output[0].EncryptedContent != "compact-secret" {
		t.Fatalf("compaction item was not preserved: %#v", out.Output[0])
	}
	if out.Output[1].Audio == nil || out.Output[1].Audio.ID != "aud_1" || out.Output[1].Audio.Transcript != "hello" {
		t.Fatalf("audio metadata was not preserved: %#v", out.Output[1])
	}
}

func TestResponseInboundTransformResponseReplaysRawOutput(t *testing.T) {
	rawOutput := json.RawMessage(`[{"type":"computer_call","id":"cc_1","action":{"type":"click"}}]`)
	inbound := &ResponseInbound{}
	body, err := inbound.TransformResponse(nil, &model.InternalLLMResponse{
		ID:                      "resp_raw",
		Model:                   "gpt-5",
		RawResponsesOutputItems: rawOutput,
	})
	if err != nil {
		t.Fatalf("TransformResponse failed: %v", err)
	}
	var payload struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response body was not valid JSON: %v", err)
	}
	if string(payload.Output) != string(rawOutput) {
		t.Fatalf("expected raw output replay, got %s", payload.Output)
	}
}

func TestConvertToInternalRequestPreservesOutputAnnotations(t *testing.T) {
	start, end := int64(1), int64(4)
	input := ResponsesInput{Items: []ResponsesItem{{
		Type: "message",
		Role: "assistant",
		Content: &ResponsesInput{Items: []ResponsesItem{{
			Type: "output_text",
			Text: stringPtr("text"),
			Annotations: &[]ResponsesAnnotation{{
				Type: "url_citation", StartIndex: &start, EndIndex: &end,
				URL: stringPtr("https://example.com"), Title: stringPtr("Example"),
			}},
		}}},
	}}}
	messages, err := convertInputToMessages(&input)
	if err != nil {
		t.Fatalf("convertInputToMessages failed: %v", err)
	}
	if len(messages) != 1 || len(messages[0].Annotations) != 1 {
		t.Fatalf("expected one preserved annotation, got %#v", messages)
	}
	annotation := messages[0].Annotations[0]
	if annotation.URLCitation == nil || annotation.URLCitation.URL != "https://example.com" || annotation.StartIndex == nil || *annotation.StartIndex != start {
		t.Fatalf("unexpected converted annotation: %#v", annotation)
	}
}
