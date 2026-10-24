package anthropic

import (
	"context"
	"strings"
	"testing"
)

func TestDynamicToolChanges(t *testing.T) {
	request := `{"model":"test","max_tokens":32,"tools":[{"name":"old","input_schema":{}}],"messages":[
		{"role":"user","content":"hello"},
		{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"weather","description":"Weather","input_schema":{"type":"object"}}}},{"type":"text","text":"use weather"}]},
		{"role":"system","content":[{"type":"tool_addition","tool":{"name":"weather","input_schema":{}}},{"type":"tool_removal","tool_name":"old"}]},
		{"role":"system","content":[{"type":"tool_removal","tool":{"name":"weather"}},{"type":"tool_addition","definition":{"name":"weather","description":"new definition","input_schema":{}}}]}
	]}`
	got, err := (&MessagesInbound{}).TransformRequest(context.Background(), []byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "weather" || got.Tools[0].Function.Description != "new definition" {
		t.Fatalf("active tools = %+v", got.Tools)
	}
	if len(got.Messages) != 2 || got.Messages[1].Role != "system" || got.Messages[1].Content.Content == nil || *got.Messages[1].Content.Content != "use weather" {
		t.Fatalf("carrier messages or text changed: %+v", got.Messages)
	}
}

func TestDynamicToolChangesRejectInvalidDefinitions(t *testing.T) {
	for _, block := range []string{
		`{"type":"tool_addition"}`, `{"type":"tool_addition","tool":{"name":"bad"}}`,
		`{"type":"tool_addition","tool":{"name":"","input_schema":{}}}`,
		`{"type":"tool_addition","tool":{"type":"tool_reference","name":"missing"}}`,
		`{"type":"tool_removal"}`,
	} {
		_, err := (&MessagesInbound{}).TransformRequest(context.Background(), []byte(`{"model":"test","messages":[{"role":"system","content":[`+block+`]}]}`))
		if err == nil || !strings.Contains(err.Error(), "tool_") {
			t.Fatalf("invalid block %s accepted: %v", block, err)
		}
	}
}

func TestDynamicToolReferenceCanReactivateRemovedTool(t *testing.T) {
	raw := []byte(`{"model":"test","tools":[{"name":"weather","input_schema":{}}],"messages":[{"role":"user","content":"hello"},{"role":"system","content":[{"type":"tool_removal","tool":"weather"},{"type":"tool_addition","tool":{"type":"tool_reference","name":"weather"}}]}]}`)
	got, err := (&MessagesInbound{}).TransformRequest(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "weather" || len(got.Messages) != 1 {
		t.Fatalf("reactivation failed: %+v", got)
	}
}
