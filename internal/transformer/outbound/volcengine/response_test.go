package volcengine

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestTransformRequestAddsVolcengineThinkingAndPartial(t *testing.T) {
	content := "question"
	answer := "existing answer"
	req := &model.InternalLLMRequest{
		Model: "doubao-seed-1-5-unsupported",
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: &content}},
			{Role: "assistant", Content: model.MessageContent{Content: &answer}},
		},
		ReasoningEffort: "max",
		Metadata:        map[string]string{"must_not_forward": "true"},
	}

	outbound := &ResponseOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://ark.example.com/api/v3", "ark-key")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if _, ok := payload["metadata"]; ok {
		t.Fatalf("metadata must not be sent to Volcengine: %s", body)
	}
	if _, ok := payload["reasoning"]; ok {
		t.Fatalf("reasoning must be removed for unsupported Volcengine models: %s", body)
	}
	var thinking Thinking
	if err := json.Unmarshal(payload["thinking"], &thinking); err != nil || thinking.Type != ThinkingTypeEnabled {
		t.Fatalf("expected thinking.enabled, got %s", payload["thinking"])
	}
	var input []map[string]any
	if err := json.Unmarshal(payload["input"], &input); err != nil {
		t.Fatalf("decode input: %v", err)
	}
	if len(input) == 0 || input[len(input)-1]["role"] != "assistant" || input[len(input)-1]["partial"] != true {
		t.Fatalf("expected last assistant input to be partial: %#v", input)
	}
}

func TestTransformRequestKeepsReasoningForSupportedVolcengineModel(t *testing.T) {
	content := "question"
	req := &model.InternalLLMRequest{
		Model:           "doubao-seed-1-6-251015",
		Messages:        []model.Message{{Role: "user", Content: model.MessageContent{Content: &content}}},
		ReasoningEffort: "minimal",
	}

	httpReq, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://ark.example.com/api/v3", "ark-key")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if _, ok := payload["reasoning"]; !ok {
		t.Fatalf("expected reasoning to remain for supported model: %s", body)
	}
	var thinking Thinking
	if err := json.Unmarshal(payload["thinking"], &thinking); err != nil || thinking.Type != ThinkingTypeDisabled {
		t.Fatalf("expected thinking.disabled, got %s", payload["thinking"])
	}
}
