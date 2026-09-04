package grouphealth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestProbeRetriesWithoutRejectedSemanticGroupParameter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if calls.Add(1) == 1 {
			if payload["reasoning_effort"] != "high" {
				t.Fatalf("first probe should contain semantic reasoning effort: %s", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"Unsupported parameter: reasoning_effort"}`))
			return
		}
		if _, exists := payload["reasoning_effort"]; exists {
			t.Fatalf("compatibility probe retry must remove reasoning_effort: %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	groupOverride := `{"reasoning_effort":"high"}`
	channel := model.Channel{
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: server.URL + "/v1"}},
	}
	key := model.ChannelKey{ChannelKey: "sk-test"}
	result := NewProber().RunCandidateWithGroupOverride(context.Background(), channel, key, "plain-chat-model", &groupOverride)
	if !result.Success || result.HTTPStatus != http.StatusOK {
		t.Fatalf("expected compatibility probe retry to succeed: %#v", result)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly two probe calls, got %d", calls.Load())
	}
}

func TestProbeDowngradesRejectedSemanticReasoningMax(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if calls.Add(1) == 1 {
			if payload["reasoning_effort"] != "max" {
				t.Fatalf("first probe should preserve max: %s", body)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'max'. Supported values are: 'low', 'medium', and 'high'."}}`))
			return
		}
		if payload["reasoning_effort"] != "high" {
			t.Fatalf("compatibility probe retry should downgrade max to high: %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	groupOverride := `{"$octopus":{"reasoning_effort":"max"}}`
	channel := model.Channel{
		Type:     outbound.OutboundTypeOpenAIChat,
		BaseUrls: []model.BaseUrl{{URL: server.URL + "/v1"}},
	}
	key := model.ChannelKey{ChannelKey: "sk-test"}
	result := NewProber().RunCandidateWithGroupOverride(context.Background(), channel, key, "plain-chat-model", &groupOverride)
	if !result.Success || result.HTTPStatus != http.StatusOK {
		t.Fatalf("expected max-to-high probe retry to succeed: %#v", result)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly two probe calls, got %d", calls.Load())
	}
}
