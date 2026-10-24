package model

import "testing"

func TestInternalResponseFromStreamEventsConcatenatesDeltas(t *testing.T) {
	events := []StreamEvent{
		{Kind: StreamEventKindTextDelta, Delta: &StreamDelta{Text: "hello "}},
		{Kind: StreamEventKindTextDelta, Delta: &StreamDelta{Text: "world"}},
		{Kind: StreamEventKindTextDelta, Delta: &StreamDelta{Refusal: "cannot "}},
		{Kind: StreamEventKindTextDelta, Delta: &StreamDelta{Refusal: "comply"}},
		{Kind: StreamEventKindThinkingDelta, Delta: &StreamDelta{Thinking: "first "}},
		{Kind: StreamEventKindThinkingDelta, Delta: &StreamDelta{Thinking: "second"}},
		{Kind: StreamEventKindSignatureDelta, Delta: &StreamDelta{Signature: "sig-"}},
		{Kind: StreamEventKindSignatureDelta, Delta: &StreamDelta{Signature: "tail"}},
	}
	got := InternalResponseFromStreamEvents(events)
	if got == nil || len(got.Choices) != 1 || got.Choices[0].Delta == nil {
		t.Fatalf("missing choice: %+v", got)
	}
	msg := got.Choices[0].Delta
	if msg.Content.Content == nil || *msg.Content.Content != "hello world" {
		t.Errorf("text fragments lost: %v", msg.Content.Content)
	}
	if msg.Refusal != "cannot comply" {
		t.Errorf("refusal fragments lost: %q", msg.Refusal)
	}
	if msg.ReasoningContent == nil || *msg.ReasoningContent != "first second" {
		t.Errorf("thinking fragments lost: %v", msg.ReasoningContent)
	}
	if msg.ReasoningSignature == nil || *msg.ReasoningSignature != "sig-tail" {
		t.Errorf("signature fragments lost: %v", msg.ReasoningSignature)
	}
}

func TestStreamAggregatorConcatenatesRefusal(t *testing.T) {
	var agg StreamAggregator
	for _, refusal := range []string{"cannot ", "comply"} {
		agg.Add(&InternalLLMResponse{Choices: []Choice{{Delta: &Message{Refusal: refusal}}}})
	}
	got := agg.Response()
	if got.Choices[0].Message.Refusal != "cannot comply" {
		t.Fatalf("aggregated refusal = %q", got.Choices[0].Message.Refusal)
	}
}
