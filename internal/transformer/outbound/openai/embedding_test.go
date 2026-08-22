package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestEmbeddingOutboundUsesAxonHubRequest(t *testing.T) {
	dimensions := int64(3)
	input := model.EmbeddingInput{Single: stringPtr("hello")}
	req := &model.InternalLLMRequest{
		Model:               "text-embedding-3-small",
		EmbeddingInput:      &input,
		EmbeddingDimensions: &dimensions,
		RawAPIFormat:        model.APIFormatOpenAIEmbedding,
	}

	outbound := &EmbeddingOutbound{}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://api.openai.com/v1", "sk-test")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	if got := httpReq.URL.String(); got != "https://api.openai.com/v1/embeddings" {
		t.Fatalf("unexpected embedding URL: %s", got)
	}
	if got := httpReq.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("unexpected authorization header: %q", got)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if payload["model"] != req.Model || payload["input"] != "hello" || payload["dimensions"] != float64(3) {
		t.Fatalf("unexpected embedding payload: %#v", payload)
	}
}

func TestEmbeddingOutboundRestoresResponseEnvelope(t *testing.T) {
	outbound := &EmbeddingOutbound{}
	input := model.EmbeddingInput{Single: stringPtr("hello")}
	req := &model.InternalLLMRequest{
		Model:          "text-embedding-3-small",
		EmbeddingInput: &input,
		RawAPIFormat:   model.APIFormatOpenAIEmbedding,
	}
	httpReq, err := outbound.TransformRequest(context.Background(), req, "https://api.openai.com/v1", "sk-test")
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	body := `{"id":"emb-1","object":"list","created":1710000000,"model":"text-embedding-3-small","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`
	response, err := outbound.TransformResponse(context.Background(), &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    httpReq,
	})
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	if response.ID != "emb-1" || response.Object != "list" || response.Created != 1710000000 || response.Model != req.Model {
		t.Fatalf("response envelope was not preserved: %+v", response)
	}
	if len(response.EmbeddingData) != 1 || response.EmbeddingData[0].Object != "embedding" || len(response.EmbeddingData[0].Embedding.FloatArray) != 2 {
		t.Fatalf("unexpected embedding data: %+v", response.EmbeddingData)
	}
	if response.Usage == nil || response.Usage.PromptTokens != 2 || response.Usage.TotalTokens != 2 {
		t.Fatalf("unexpected embedding usage: %+v", response.Usage)
	}
}
