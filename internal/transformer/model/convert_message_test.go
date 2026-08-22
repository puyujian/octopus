package model

import (
	"testing"

	"github.com/looplj/axonhub/llm"
	axonShared "github.com/looplj/axonhub/llm/transformer/shared"
)

func TestGeminiToolCallSignatureUsesAxonHubMetadataKey(t *testing.T) {
	converted, err := ToLLMToolCall(&ToolCall{ThoughtSignature: "sig-1"})
	if err != nil {
		t.Fatalf("ToLLMToolCall: %v", err)
	}
	if got := converted.TransformerMetadata[axonShared.TransformerMetadataKeyGoogleThoughtSignature]; got != "sig-1" {
		t.Fatalf("signature metadata = %#v, want google_thought_signature=sig-1", converted.TransformerMetadata)
	}
	if _, ok := converted.TransformerMetadata[legacyGeminiThoughtSignatureMetadataKey]; ok {
		t.Fatalf("did not expect legacy Gemini signature key in outbound metadata: %#v", converted.TransformerMetadata)
	}

	for _, key := range []string{axonShared.TransformerMetadataKeyGoogleThoughtSignature, legacyGeminiThoughtSignatureMetadataKey} {
		toolCall := FromLLMToolCall(&llm.ToolCall{TransformerMetadata: map[string]any{key: "sig-roundtrip"}})
		if toolCall == nil || toolCall.ThoughtSignature != "sig-roundtrip" {
			t.Fatalf("FromLLMToolCall did not read %s: %#v", key, toolCall)
		}
	}
}

func TestToLLMContentPartRetainsDocumentMetadataWithoutInputMetadata(t *testing.T) {
	part, err := ToLLMContentPart(&MessageContentPart{
		Type: "document",
		Document: &DocumentSource{
			Type:      "text",
			MediaType: "text/plain",
			Title:     "notes",
			Context:   "context",
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("ToLLMContentPart: %v", err)
	}
	if part == nil || part.Document == nil {
		t.Fatalf("expected converted document, got %#v", part)
	}
	if got := part.TransformerMetadata["octopus_document_title"]; got != "notes" {
		t.Fatalf("document title metadata = %#v", got)
	}
	if got := part.TransformerMetadata["octopus_document_context"]; got != "context" {
		t.Fatalf("document context metadata = %#v", got)
	}
	roundTrip := FromLLMContentPart(part)
	if roundTrip == nil || roundTrip.Document == nil || roundTrip.Document.Text != "hello" {
		t.Fatalf("document metadata/content did not round-trip: %#v", roundTrip)
	}
}
