package openai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	anthropic "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound/gemini"
	openai "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

func TestDynamicToolsReachUpstreamProtocols(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  model.Outbound
	}{
		{"chat", &openai.ChatOutbound{}}, {"responses", &openai.ResponseOutbound{}}, {"gemini", &gemini.MessagesOutbound{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"test","max_tokens":32,"messages":[{"role":"user","content":"weather?"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}}}]}]}`)
			req, err := (&anthropic.MessagesInbound{}).TransformRequest(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			upstream, err := tc.out.TransformRequest(context.Background(), req, "https://example.test/v1", "key")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(upstream.Body)
			upstream.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(body, []byte(`"get_weather"`)) || !bytes.Contains(body, []byte(`"city"`)) || bytes.Contains(body, []byte("tool_addition")) {
				t.Fatalf("dynamic tool lost: %s", body)
			}
		})
	}
}

func TestResponsesSearchResponseAndHistory(t *testing.T) {
	ctx := context.Background()
	out := &openai.ResponseOutbound{}
	raw := `{"id":"resp_1","model":"test","status":"completed","output":[{"id":"ws_a","type":"web_search_call","status":"completed","action":{"type":"search","query":"Debian","sources":[{"type":"url","url":"https://debian.org","title":"Debian"}]}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Answer"}]}]}`
	converted, err := out.TransformResponse(ctx, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))})
	if err != nil {
		t.Fatal(err)
	}
	client := &anthropic.MessagesInbound{}
	wire, err := client.TransformResponse(ctx, converted)
	if err != nil {
		t.Fatal(err)
	}
	var response anthropic.Message
	if err := json.Unmarshal(wire, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 3 || response.Content[0].Type != "server_tool_use" || response.Content[1].Type != "web_search_tool_result" || response.Content[2].Type != "text" {
		t.Fatalf("search blocks missing: %s", wire)
	}
	if !bytes.Contains(response.Content[1].Content.Raw, []byte("https://debian.org")) || !bytes.Contains(response.Content[0].Input, []byte("Debian")) {
		t.Fatalf("search metadata lost: %s", wire)
	}
	if len(converted.Choices[0].Message.ToolCalls) != 0 {
		t.Fatal("server search became a pending function")
	}
	request := map[string]any{"model": "test", "max_tokens": 32, "messages": []any{
		map[string]any{"role": "user", "content": "search Debian"}, map[string]any{"role": "assistant", "content": response.Content}, map[string]any{"role": "user", "content": "continue"},
	}}
	requestBytes, _ := json.Marshal(request)
	history, err := (&anthropic.MessagesInbound{}).TransformRequest(ctx, requestBytes)
	if err != nil {
		t.Fatalf("echoed search history rejected: %v", err)
	}
	upstream, err := (&openai.ResponseOutbound{}).TransformRequest(ctx, history, "https://example.test/v1", "key")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(upstream.Body)
	upstream.Body.Close()
	if !bytes.Contains(body, []byte("https://debian.org")) || !bytes.Contains(body, []byte("Debian")) || bytes.Contains(body, []byte(`"type":"function_call"`)) {
		t.Fatalf("search history lost or replayed as a function: %s", body)
	}
}

func TestResponsesSearchLateSourcesAcrossTerminalStates(t *testing.T) {
	for _, terminal := range []string{"completed", "incomplete", "failed", "cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			out := &openai.ResponseOutbound{}
			client := &anthropic.MessagesInbound{}
			chunks := []string{
				`{"type":"response.created","response":{"id":"resp_1","model":"test"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_a","type":"web_search_call","status":"in_progress","action":{"query":"first","sources":[{"url":"https://a.test"}]}}}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"ws_a","type":"web_search_call","status":"completed"}}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"id":"ws_b","type":"web_search_call","status":"completed","action":{"query":"second","sources":[{"url":"https://b.test","title":"B"}]}}}`,
				`{"type":"response.` + terminal + `","response":{"id":"resp_1","status":"` + terminal + `","output":[{"id":"ws_a","type":"web_search_call","status":"completed","action":{"sources":[{"url":"https://a.test","title":"A"},{"url":"https://a.test","title":"duplicate"},{"url":"https://late.test","title":"Late"}]}},{"id":"ws_b","type":"web_search_call","status":"completed"}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
				`[DONE]`,
			}
			var wire bytes.Buffer
			for _, chunk := range chunks {
				events, err := out.TransformStreamEvent(context.Background(), []byte(chunk))
				if err != nil {
					t.Fatal(err)
				}
				data, err := client.TransformStreamEvents(context.Background(), events)
				if err != nil {
					t.Fatal(err)
				}
				wire.Write(data)
			}
			var blocks []anthropic.MessageContentBlock
			for _, line := range strings.Split(wire.String(), "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				var event anthropic.StreamEvent
				if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
					t.Fatal(err)
				}
				if event.Type == "content_block_start" && event.ContentBlock != nil {
					blocks = append(blocks, *event.ContentBlock)
				}
			}
			if len(blocks) != 4 {
				t.Fatalf("search calls lost or duplicated: %s", wire.String())
			}
			for index, name := range []string{"first", "second"} {
				use, result := blocks[index*2], blocks[index*2+1]
				if use.Type != "server_tool_use" || !bytes.Contains(use.Input, []byte(name)) || result.Type != "web_search_tool_result" || result.ToolUseID == nil || *result.ToolUseID != use.ID {
					t.Fatalf("wrong search/result pairing: %+v %+v", use, result)
				}
			}
			if bytes.Count(blocks[1].Content.Raw, []byte("https://a.test")) != 1 || !bytes.Contains(blocks[1].Content.Raw, []byte("https://late.test")) || bytes.Contains(blocks[3].Content.Raw, []byte("https://a.test")) {
				t.Fatalf("late sources lost or attributed to wrong search: %s", wire.String())
			}
			aggregated, err := client.GetInternalResponse(context.Background())
			if err != nil || aggregated == nil || len(aggregated.WebSearchCalls) != 2 || aggregated.Usage == nil || aggregated.Usage.TotalTokens != 12 {
				t.Fatalf("search logging lost: %+v %v", aggregated, err)
			}
		})
	}
}

func TestResponsesSearchProgressCompletesWithoutFinalOutput(t *testing.T) {
	out := &openai.ResponseOutbound{}
	client := &anthropic.MessagesInbound{}
	var wire bytes.Buffer
	for _, chunk := range []string{
		`{"type":"response.output_item.added","output_index":2,"item":{"id":"ws_a","type":"web_search_call","status":"in_progress","action":{"query":"first","sources":[{"url":"https://a.test"}]}}}`,
		`{"type":"response.web_search_call.completed","output_index":2}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
	} {
		events, err := out.TransformStreamEvent(context.Background(), []byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
		data, err := client.TransformStreamEvents(context.Background(), events)
		if err != nil {
			t.Fatal(err)
		}
		wire.Write(data)
	}
	if !bytes.Contains(wire.Bytes(), []byte("https://a.test")) || bytes.Contains(wire.Bytes(), []byte("web_search_tool_result_error")) {
		t.Fatalf("completed search reported as failed: %s", wire.String())
	}
}
