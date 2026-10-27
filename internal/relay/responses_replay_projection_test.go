package relay

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestConvertedReplayWindowIncludesAssistantTurn(t *testing.T) {
	ctx := context.Background()
	request, err := (&openaiInbound.ResponseInbound{}).TransformRequest(ctx, []byte(`{"model":"test","input":"original-marker"}`))
	if err != nil {
		t.Fatal(err)
	}
	reply := "assistant-marker"
	state := &wsConversationState{}
	state.ApplySuccessfulTurn(request, &model.InternalLLMResponse{ID: "prev", Choices: []model.Choice{{Message: &model.Message{Role: "assistant", Content: model.MessageContent{Content: &reply}, ToolCalls: []model.ToolCall{{ID: "call_1", Type: "function", Function: model.FunctionCall{Name: "probe", Arguments: "{}"}}}}}}})
	for _, marker := range []string{"original-marker", "assistant-marker", "call_1", "probe"} {
		if !strings.Contains(string(state.ReplayWindowItems), marker) {
			t.Fatalf("replay window lost %q: %s", marker, state.ReplayWindowItems)
		}
	}
}

func TestReplayProjectionPreservesHistoryAndNativeRetry(t *testing.T) {
	ctx := context.Background()
	current, err := (&openaiInbound.ResponseInbound{}).TransformRequest(ctx, []byte(`{"model":"test","instructions":"keep instruction","previous_response_id":"prev","input":[{"type":"function_call_output","call_id":"call_1","output":"tool-result"},{"role":"user","content":"followup-marker"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	state := &wsConversationState{LastResponseID: "prev", ReplayWindowItems: json.RawMessage(`[{"role":"user","content":"original-marker"},{"role":"assistant","content":"assistant-marker"},{"type":"function_call","call_id":"call_1","name":"probe","arguments":"{}"}]`)}
	replay := state.BuildReplayRequest(current)
	if replay == nil {
		t.Fatal("could not build replay")
	}
	before, _ := json.Marshal(replay)
	for _, typ := range []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeAnthropic, outbound.OutboundTypeGemini} {
		projected, err := projectReplayForOutbound(ctx, replay, typ)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := outbound.Get(typ).TransformRequest(ctx, projected, "http://example.invalid/v1", "fixture-key")
		if err != nil {
			t.Fatalf("type=%d: %v", typ, err)
		}
		body, _ := io.ReadAll(wire.Body)
		wire.Body.Close()
		for _, marker := range []string{"keep instruction", "original-marker", "assistant-marker", "call_1", "probe", "tool-result", "followup-marker"} {
			if !strings.Contains(string(body), marker) {
				t.Fatalf("type=%d lost %q: %s", typ, marker, body)
			}
		}
		if strings.Contains(string(body), "previous_response_id") {
			t.Fatalf("continuation id leaked into converted replay: %s", body)
		}
	}
	after, _ := json.Marshal(replay)
	if string(before) != string(after) || !replay.IsOpenAIExactReplayRequest() || len(replay.Messages) != 1 {
		t.Fatal("projection mutated canonical raw replay")
	}
	for _, typ := range []outbound.OutboundType{outbound.OutboundTypeOpenAIResponse, outbound.OutboundTypeVolcengine} {
		native, err := projectReplayForOutbound(ctx, replay, typ)
		if err != nil || native != replay {
			t.Fatalf("native route lost original replay: %v", err)
		}
	}
}

func TestReplayProjectionRejectsOpaqueHistory(t *testing.T) {
	ctx := context.Background()
	req, err := (&openaiInbound.ResponseInbound{}).TransformRequest(ctx, []byte(`{"model":"test","input":[{"type":"computer_call","id":"opaque"},{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.MarkOpenAIExactReplayRequest()
	if _, err := projectReplayForOutbound(ctx, req, outbound.OutboundTypeOpenAIChat); err == nil {
		t.Fatal("silently dropped native-only replay history")
	}
	if native, err := projectReplayForOutbound(ctx, req, outbound.OutboundTypeOpenAIResponse); err != nil || native != req {
		t.Fatal("native-only replay should remain available")
	}
}
