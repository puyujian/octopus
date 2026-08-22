package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestConvertGeminiToLLMResponseCarriesMetadata(t *testing.T) {
	geminiResp := &model.GeminiGenerateContentResponse{
		ResponseId:   "resp-42",
		ModelVersion: "gemini-2.5-pro-v20251007",
		CreateTime:   "2026-04-21T12:34:56Z",
		Candidates: []*model.GeminiCandidate{
			{
				Index: 0,
				Content: &model.GeminiContent{
					Role: "model",
					Parts: []*model.GeminiPart{
						{Text: "hi"},
					},
				},
			},
		},
	}

	resp := convertGeminiToLLMResponse(geminiResp, false, nil)
	if resp.ID != "resp-42" {
		t.Fatalf("ID = %q, want resp-42", resp.ID)
	}
	if resp.Model != "gemini-2.5-pro-v20251007" {
		t.Fatalf("Model = %q, want gemini-2.5-pro-v20251007", resp.Model)
	}
	if resp.Created == 0 {
		t.Fatalf("Created should be parsed from createTime, got 0")
	}
}

func TestConvertGeminiToLLMResponseSynthesizesBlockedChoice(t *testing.T) {
	geminiResp := &model.GeminiGenerateContentResponse{
		PromptFeedback: &model.GeminiPromptFeedback{
			BlockReason: "SAFETY",
		},
	}
	resp := convertGeminiToLLMResponse(geminiResp, false, nil)
	if len(resp.Choices) != 1 {
		t.Fatalf("expected synthesized choice for blocked prompt, got %d", len(resp.Choices))
	}
	fr := resp.Choices[0].FinishReason
	if fr == nil {
		t.Fatalf("FinishReason should be set on blocked prompt choice")
	}
	if got := *fr; got != string(model.FinishReasonSafety) && got != string(model.FinishReasonContentFilter) {
		t.Fatalf("FinishReason = %q, want safety-family or content_filter", got)
	}
}

func TestTransformResponseMergesGeminiMetadataWithAxonHubResponse(t *testing.T) {
	reason := "STOP"
	geminiResp := &model.GeminiGenerateContentResponse{
		ResponseId:   "resp-42",
		ModelVersion: "gemini-3.1-pro",
		Candidates: []*model.GeminiCandidate{{
			Index:        0,
			FinishReason: &reason,
			Content:      &model.GeminiContent{Role: "model", Parts: []*model.GeminiPart{{Text: "hello"}}},
			GroundingMetadata: &model.GeminiGroundingMetadata{
				WebSearchQueries: []string{"octopus"},
				GroundingChunks:  []*model.GeminiGroundingChunk{{Web: &model.GeminiGroundingChunkWeb{URI: "https://example.test", Title: "Example"}}},
			},
			CitationMetadata: &model.GeminiCitationMetadata{CitationSources: []*model.GeminiCitationSource{{
				StartIndex: 0, EndIndex: 5, URI: "https://example.test", License: "MIT",
			}}},
			UrlContextMetadata: &model.GeminiUrlContextMetadata{URLMetadata: []*model.GeminiURLMetadata{{
				RetrievedURL: "https://example.test", URLRetrievalStatus: "URL_RETRIEVAL_STATUS_SUCCESS",
			}}},
			SafetyRatings: []*model.GeminiSafetyRating{{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Probability: "LOW"}},
		}},
	}
	body, err := json.Marshal(geminiResp)
	if err != nil {
		t.Fatalf("marshal Gemini response: %v", err)
	}

	response, err := (&MessagesOutbound{}).TransformResponse(context.Background(), &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	})
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	if response == nil || len(response.Choices) != 1 {
		t.Fatalf("unexpected transformed response: %#v", response)
	}
	choice := response.Choices[0]
	if choice.Grounding == nil || len(choice.Grounding.Sources) != 1 || choice.Grounding.Sources[0].URI != "https://example.test" {
		t.Fatalf("grounding metadata was lost: %#v", choice.Grounding)
	}
	if len(choice.Citations) != 1 || choice.Citations[0].License != "MIT" {
		t.Fatalf("citation metadata was lost: %#v", choice.Citations)
	}
	if choice.URLContext == nil || len(choice.URLContext.URLs) != 1 {
		t.Fatalf("URL context metadata was lost: %#v", choice.URLContext)
	}
	if len(choice.SafetyRatings) != 1 || choice.SafetyRatings[0].Probability != "LOW" {
		t.Fatalf("safety metadata was lost: %#v", choice.SafetyRatings)
	}
}
