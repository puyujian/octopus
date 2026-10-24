package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	inboundOpenAI "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestParallelToolStreamPreservesCandidateIndex(t *testing.T) {
	for _, candidateIndex := range []int{0, 3} {
		outbound := &MessagesOutbound{}
		payload := []byte(fmt.Sprintf(`{"candidates":[{"index":%d,"content":{"role":"model","parts":[
			{"functionCall":{"id":"call-a","name":"search","args":{"q":"x"}}},
			{"functionCall":{"id":"call-b","name":"lookup","args":{"q":"y"}}}
		]}}]}`, candidateIndex))
		events, err := outbound.TransformStreamEvent(context.Background(), payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Index != candidateIndex {
				t.Errorf("%s choice index = %d, want %d", event.Kind, event.Index, candidateIndex)
			}
		}
		client := &inboundOpenAI.ChatInbound{}
		wire, err := client.TransformStreamEvents(context.Background(), events)
		if err != nil {
			t.Fatal(err)
		}
		var resp model.InternalLLMResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(string(wire), "data: "))), &resp); err != nil {
			t.Fatalf("invalid Chat SSE chunk %q: %v", wire, err)
		}
		if len(resp.Choices) != 1 || resp.Choices[0].Index != candidateIndex {
			t.Fatalf("parallel calls split into phantom choices: %+v", resp)
		}
		calls := resp.Choices[0].Delta.ToolCalls
		if len(calls) != 2 || calls[0].Index != 0 || calls[1].Index != 1 || calls[0].ID != "call-a" || calls[1].ID != "call-b" {
			t.Fatalf("parallel tool identities lost: %+v", calls)
		}
		if calls[0].Function.Arguments != `{"q":"x"}` || calls[1].Function.Arguments != `{"q":"y"}` {
			t.Fatalf("parallel tool arguments lost: %+v", calls)
		}
	}
}

func TestGeminiStreamToChatRetainsTextParts(t *testing.T) {
	outbound := &MessagesOutbound{}
	events, err := outbound.TransformStreamEvent(context.Background(), []byte(`{
		"candidates":[{"index":0,"content":{"role":"model","parts":[
			{"text":"hello "},{"text":"world"}
		]}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	client := &inboundOpenAI.ChatInbound{}
	wire, err := client.TransformStreamEvents(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"content":"hello world"`) {
		t.Fatalf("Chat client lost text parts: %s", wire)
	}
}
