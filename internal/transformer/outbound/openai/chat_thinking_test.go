package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestChatOutbound_ThinkingParameter(t *testing.T) {
	tests := []struct {
		name            string
		thinking        *model.ThinkingConfig
		expectInPayload bool
		expectedType    string
	}{
		{
			name:            "thinking disabled",
			thinking:        &model.ThinkingConfig{Type: "disabled"},
			expectInPayload: true,
			expectedType:    "disabled",
		},
		{
			name:            "thinking enabled",
			thinking:        &model.ThinkingConfig{Type: "enabled"},
			expectInPayload: true,
			expectedType:    "enabled",
		},
		{
			name:            "thinking not set",
			thinking:        nil,
			expectInPayload: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := "Hello"
			req := &model.InternalLLMRequest{
				Model: "deepseek-v4-flash",
				Messages: []model.Message{
					{
						Role: "user",
						Content: model.MessageContent{
							Content: &content,
						},
					},
				},
				Thinking: tt.thinking,
			}

			outbound := &ChatOutbound{}
			httpReq, err := outbound.TransformRequest(context.Background(), req, "https://api.deepseek.com/v1", "test-key")
			if err != nil {
				t.Fatalf("TransformRequest failed: %v", err)
			}

			body, err := io.ReadAll(httpReq.Body)
			if err != nil {
				t.Fatalf("failed to read request body: %v", err)
			}

			var payload map[string]interface{}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("failed to unmarshal payload: %v", err)
			}

			thinkingField, exists := payload["thinking"]
			if tt.expectInPayload {
				if !exists {
					t.Fatalf("expected 'thinking' field in payload, but it was missing")
				}
				thinkingMap, ok := thinkingField.(map[string]interface{})
				if !ok {
					t.Fatalf("expected 'thinking' to be an object, got %T", thinkingField)
				}
				typeValue, ok := thinkingMap["type"].(string)
				if !ok {
					t.Fatalf("expected 'thinking.type' to be a string, got %T", thinkingMap["type"])
				}
				if typeValue != tt.expectedType {
					t.Fatalf("expected thinking.type=%q, got %q", tt.expectedType, typeValue)
				}
			} else {
				if exists {
					t.Fatalf("expected 'thinking' field to be omitted, but got: %v", thinkingField)
				}
			}
		})
	}
}

func TestChatOutbound_ThinkingWithReasoningEffort(t *testing.T) {
	// Test that both thinking and reasoning_effort can coexist
	content := "Hello"
	req := &model.InternalLLMRequest{
		Model: "deepseek-v4-flash",
		Messages: []model.Message{
			{
				Role: "user",
				Content: model.MessageContent{
					Content: &content,
				},
			},
		},
		Thinking:        &model.ThinkingConfig{Type: "disabled"},
		ReasoningEffort: "high",
	}

	outbound := &ChatOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://api.deepseek.com/v1", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}

	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("failed to read request body: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}

	// Both fields should be present
	if _, exists := payload["thinking"]; !exists {
		t.Fatalf("expected 'thinking' field in payload")
	}
	if _, exists := payload["reasoning_effort"]; !exists {
		t.Fatalf("expected 'reasoning_effort' field in payload")
	}
}

func TestChatOutbound_ResponsesDeepSeekDefaultsThinking(t *testing.T) {
	content := "hello"
	request := &model.InternalLLMRequest{
		Model:        "deepseek-ai/deepseek-v4-pro-0813",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages:     []model.Message{{Role: "user", Content: model.MessageContent{Content: &content}}},
	}

	outbound := &ChatOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), request, "https://example.test/v1", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("failed to read request body: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	var thinking model.ThinkingConfig
	if err := json.Unmarshal(payload["thinking"], &thinking); err != nil {
		t.Fatalf("failed to decode thinking: %v", err)
	}
	if thinking.Type != "enabled" {
		t.Fatalf("expected Responses-to-Chat DeepSeek default to enable thinking, got %q", thinking.Type)
	}
}

func TestChatOutbound_ResponsesDeepSeekKeepsExplicitThinkingAndDisable(t *testing.T) {
	content := "hello"
	for _, test := range []struct {
		name   string
		model  string
		effort string
		think  *model.ThinkingConfig
		want   string
	}{
		{name: "explicit disabled", model: "deepseek-ai/deepseek-v4-pro-0813", think: &model.ThinkingConfig{Type: "disabled"}, want: "disabled"},
		{name: "reasoning none", model: "deepseek-ai/deepseek-v4-pro-0813", effort: "none", want: ""},
		{name: "other model", model: "deepseek-ai/deepseek-v3", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &model.InternalLLMRequest{
				Model:           test.model,
				RawAPIFormat:    model.APIFormatOpenAIResponse,
				ReasoningEffort: test.effort,
				Thinking:        test.think,
				Messages:        []model.Message{{Role: "user", Content: model.MessageContent{Content: &content}}},
			}
			httpReq, err := (&ChatOutbound{}).TransformRequest(context.Background(), request, "https://example.test/v1", "test-key")
			if err != nil {
				t.Fatalf("TransformRequest failed: %v", err)
			}
			body, err := io.ReadAll(httpReq.Body)
			if err != nil {
				t.Fatalf("failed to read request body: %v", err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("failed to unmarshal payload: %v", err)
			}
			if test.want == "" {
				if _, present := payload["thinking"]; present {
					t.Fatalf("expected thinking to remain omitted, payload has %s", payload["thinking"])
				}
				return
			}
			var thinking model.ThinkingConfig
			if err := json.Unmarshal(payload["thinking"], &thinking); err != nil {
				t.Fatalf("failed to decode thinking: %v", err)
			}
			if thinking.Type != test.want {
				t.Fatalf("expected thinking.type=%q, got %q", test.want, thinking.Type)
			}
		})
	}
}
