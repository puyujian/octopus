package openai

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	inboundOpenAI "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
)

func TestResponsesSearchToolControlsSurviveNormalization(t *testing.T) {
	for _, tool := range []string{
		`{"type":"web_search","filters":{"allowed_domains":["example.org"],"blocked_domains":["blocked.example"]},"external_web_access":false,"return_token_budget":1200,"search_context_size":"low","user_location":{"type":"approximate","country":"CN"}}`,
		`{"type":"web_search_preview","search_context_size":"high","user_location":{"type":"approximate","city":"Shanghai"}}`,
		`{"type":"web_search_preview_2025_03_11"}`,
	} {
		req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{"model":"glm-5.3","input":"hello","tools":[`+tool+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Tools) != 1 || req.Tools[0].Type != "web_search" {
			t.Fatalf("search tool missing: %+v", req.Tools)
		}
		out, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		var payload struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(body, &payload)
		if len(payload.Tools) != 1 {
			t.Fatalf("search tool lost/duplicated: %s", body)
		}
		var want, got map[string]any
		_ = json.Unmarshal([]byte(tool), &want)
		_ = json.Unmarshal(payload.Tools[0], &got)
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("search controls changed: want %s, got %s", wantJSON, gotJSON)
		}
	}
}

func TestResponsesSearchOverlayPreservesOtherNativeTools(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-luna","input":"hello","tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.org/mcp"},{"type":"function","name":"weather","parameters":{"type":"object","properties":{}},"strict":true},{"type":"web_search","search_context_size":"low"}]}`)
	req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	var payload struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &payload)
	if len(payload.Tools) != 3 || string(payload.Tools[0]["server_label"]) != `"docs"` || string(payload.Tools[1]["strict"]) != "true" || string(payload.Tools[2]["search_context_size"]) != `"low"` {
		t.Fatalf("search overlay damaged neighboring tools: %s", body)
	}
}

func TestResponsesRequiredToolChoiceSurvivesNormalization(t *testing.T) {
	req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{"model":"gpt-5.6-luna","input":"call weather","tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{}}}],"tool_choice":"required"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := (&ResponseOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(body, &payload)
	if string(payload["tool_choice"]) != `"required"` {
		t.Fatalf("tool choice wire shape is invalid: %s", body)
	}
}
