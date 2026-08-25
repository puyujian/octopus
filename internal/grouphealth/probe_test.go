package grouphealth

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestBuildProbeRequestForResponses(t *testing.T) {
	channel := &model.Channel{
		Type:     outbound.OutboundTypeOpenAIResponse,
		BaseUrls: []model.BaseUrl{{URL: "https://example.com/v1"}},
	}
	usedKey := &model.ChannelKey{ID: 1, ChannelKey: "sk-test"}

	req, err := buildProbeRequest(context.Background(), channel, usedKey, "gpt-5.4")
	if err != nil {
		t.Fatalf("buildProbeRequest returned error: %v", err)
	}
	if req.URL.Path != "/v1/responses" {
		t.Fatalf("expected /v1/responses, got %s", req.URL.Path)
	}
}

func TestBuildProbeRequestForEmbeddings(t *testing.T) {
	channel := &model.Channel{
		Type:     outbound.OutboundTypeOpenAIEmbedding,
		BaseUrls: []model.BaseUrl{{URL: "https://example.com/v1"}},
	}
	usedKey := &model.ChannelKey{ID: 1, ChannelKey: "sk-test"}

	req, err := buildProbeRequest(context.Background(), channel, usedKey, "text-embedding-3-large")
	if err != nil {
		t.Fatalf("buildProbeRequest returned error: %v", err)
	}
	if req.URL.Path != "/v1/embeddings" {
		t.Fatalf("expected /v1/embeddings, got %s", req.URL.Path)
	}
}

func TestBuildProbeRequestInfersEmbeddingFromModelName(t *testing.T) {
	channel := &model.Channel{
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: "https://example.com/v1"}},
	}
	usedKey := &model.ChannelKey{ID: 1, ChannelKey: "sk-test"}

	for _, modelName := range []string{
		"BAAI/bge-m3",
		"cf/bge-m3",
		"text-embedding-3-large",
		"intfloat/e5-large-v2",
	} {
		t.Run(modelName, func(t *testing.T) {
			req, err := buildProbeRequest(context.Background(), channel, usedKey, modelName)
			if err != nil {
				t.Fatalf("buildProbeRequest returned error: %v", err)
			}
			if req.URL.Path != "/v1/embeddings" {
				t.Fatalf("expected /v1/embeddings, got %s", req.URL.Path)
			}
		})
	}
}

func TestBuildProbeRequestInfersRerankFromModelName(t *testing.T) {
	channel := &model.Channel{
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: "https://example.com/v1"}},
	}
	usedKey := &model.ChannelKey{ID: 1, ChannelKey: "sk-test"}

	req, err := buildProbeRequest(context.Background(), channel, usedKey, "Pro/BAAI/bge-reranker-v2-m3")
	if err != nil {
		t.Fatalf("buildProbeRequest returned error: %v", err)
	}
	if req.URL.Path != "/v1/rerank" {
		t.Fatalf("expected /v1/rerank, got %s", req.URL.Path)
	}
	if req.Header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("expected bearer authorization header")
	}
	var body struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if body.Model != "Pro/BAAI/bge-reranker-v2-m3" || body.Query != "ping" || len(body.Documents) != 1 || body.Documents[0] != "ping" {
		t.Fatalf("unexpected rerank request body: %#v", body)
	}
}

func TestBuildProbeRequestKeepsChatForConversationModel(t *testing.T) {
	channel := &model.Channel{
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: "https://example.com/v1"}},
	}
	usedKey := &model.ChannelKey{ID: 1, ChannelKey: "sk-test"}

	req, err := buildProbeRequest(context.Background(), channel, usedKey, "deepseek-chat")
	if err != nil {
		t.Fatalf("buildProbeRequest returned error: %v", err)
	}
	if req.URL.Path != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions, got %s", req.URL.Path)
	}
}
