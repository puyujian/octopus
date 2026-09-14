package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	inboundOpenAI "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
)

func TestResponsesSearchMapsToAnthropicHostedTool(t *testing.T) {
	for _, tool := range []string{
		`{"type":"web_search"}`,
		`{"type":"web_search_preview"}`,
		`{"type":"web_search","filters":{"allowed_domains":["example.org"]},"user_location":{"country":"CN","city":"Shanghai","timezone":"Asia/Shanghai"}}`,
		`{"type":"web_search","filters":{"blocked_domains":["blocked.example"]}}`,
	} {
		req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{"model":"glm-5.3","input":"hello","tools":[{"type":"function","name":"weather","parameters":{"type":"object","properties":{}}},`+tool+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		out, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		var payload struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal(body, &payload)
		if len(payload.Tools) != 2 || string(payload.Tools[0]["name"]) != `"weather"` {
			t.Fatalf("tools dropped or reordered: %s", body)
		}
		search := payload.Tools[1]
		if string(search["type"]) != `"web_search_20250305"` || string(search["name"]) != `"web_search"` {
			t.Fatalf("not an Anthropic hosted search tool: %s", body)
		}
		if !strings.Contains(out.Header.Get("anthropic-beta"), "web-search-2025-03-05") {
			t.Fatal("missing search beta header")
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			if strings.Contains(tool, field) && len(search[field]) == 0 {
				t.Fatalf("lost %s: %s", field, body)
			}
		}
		if strings.Contains(tool, "Shanghai") && !strings.Contains(string(search["user_location"]), "Shanghai") {
			t.Fatalf("location lost: %s", body)
		}
	}
}

func TestAnthropicSearchDoesNotEnableExplicitlyOfflineSearch(t *testing.T) {
	req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{"model":"glm-5.3","input":"hello","tools":[{"type":"web_search","external_web_access":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
	if err == nil || !strings.Contains(err.Error(), "external_web_access=false") {
		t.Fatalf("offline restriction was silently weakened: %v", err)
	}
}

func TestResponsesForcedSearchUsesAnthropicToolChoice(t *testing.T) {
	for _, kind := range []string{"web_search", "web_search_preview"} {
		req, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(`{"model":"glm-5.3","input":"search","tools":[{"type":"`+kind+`"}],"tool_choice":{"type":"`+kind+`"}}`))
		if err != nil {
			t.Fatal(err)
		}
		out, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://example.test/v1", "test-key")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		var payload struct {
			Choice struct{ Type, Name string } `json:"tool_choice"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Choice.Type != "tool" || payload.Choice.Name != "web_search" {
			t.Fatalf("forced hosted search changed: %s", body)
		}
	}
}
