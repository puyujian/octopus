package relay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/relay/stream"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestUpstreamStreamCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []string
		complete bool
	}{
		{"partial chat", []string{`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`}, false},
		{"finished chat", []string{`{"choices":[{"index":0,"finish_reason":"stop"}]}`}, true},
		{"length is semantic completion", []string{`{"choices":[{"index":0,"finish_reason":"length"}]}`}, true},
		{"one of two choices unfinished", []string{`{"choices":[{"index":0,"finish_reason":"stop"},{"index":1,"delta":{"content":"partial"}}]}`}, false},
		{"two choices finished", []string{`{"choices":[{"index":0,"finish_reason":"stop"},{"index":1,"delta":{"content":"partial"}}]}`, `{"choices":[{"index":1,"finish_reason":"tool_calls"}]}`}, true},
		{"responses item is not terminal", []string{`{"type":"response.output_item.done"}`}, false},
		{"responses complete", []string{`{"type":"response.completed"}`}, true},
		{"responses incomplete is terminal", []string{`{"type":"response.incomplete"}`}, true},
		{"anthropic delta is not terminal", []string{`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`}, false},
		{"anthropic stop", []string{`{"type":"message_stop"}`}, true},
		{"gemini partial", []string{`{"candidates":[{"index":0,"content":{"parts":[{"text":"partial"}]}}]}`}, false},
		{"gemini stop", []string{`{"candidates":[{"index":0,"finishReason":"STOP"}]}`}, true},
		{"gemini blocked prompt", []string{`{"promptFeedback":{"blockReason":"SAFETY"}}`}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := &upstreamStreamCompletion{}
			for _, ev := range tc.events {
				if err := tracker.observe([]byte(ev)); err != nil {
					t.Fatal(err)
				}
			}
			if tracker.complete() != tc.complete {
				t.Fatalf("complete=%t, want %t", tracker.complete(), tc.complete)
			}
			if err := tracker.observe([]byte("[DONE]")); (err == nil) != tc.complete {
				t.Fatalf("transport marker bypassed semantic completion: %v", err)
			}
		})
	}
}

func TestConvertedChatPrematureEOFFails(t *testing.T) {
	for _, inType := range []inbound.InboundType{inbound.InboundTypeOpenAIChat, inbound.InboundTypeOpenAIResponse} {
		for _, done := range []string{"", "data: [DONE]\n\n"} {
			ra, recorder := newEmptyStreamTestAttempt(t, inType, model.APIFormatOpenAIChatCompletion, outbound.OutboundTypeOpenAIChat)
			body := "data: {\"id\":\"chatcmpl-partial\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" + done
			err := ra.handleStreamResponseV2(context.Background(), sseTestResponse(body))
			if !errors.Is(err, stream.ErrIncompleteUpstreamStream) {
				t.Fatalf("partial stream counted as success: %v", err)
			}
			if !ra.streamPayloadWritten.Load() || !strings.Contains(recorder.Body.String(), "partial") {
				t.Fatal("partial payload was lost; must not retry this request")
			}
			if strings.Contains(recorder.Body.String(), "response.completed") {
				t.Fatal("synthesized success for a truncated stream")
			}
		}
	}
}

func TestConvertedChatFinishWithoutUsageEmitsResponsesTerminal(t *testing.T) {
	for _, done := range []string{"", "data: [DONE]\n\n"} {
		for _, reason := range []string{"stop", "length", "tool_calls"} {
			ra, recorder := newEmptyStreamTestAttempt(t, inbound.InboundTypeOpenAIResponse, model.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIChat)
			body := "data: {\"id\":\"chatcmpl-finished\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"chatcmpl-finished\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"" + reason + "\"}]}\n\n" + done
			if err := ra.handleStreamResponseV2(context.Background(), sseTestResponse(body)); err != nil {
				t.Fatalf("valid stream failed (finish=%s): %v", reason, err)
			}
			terminal := `"type":"response.completed"`
			if reason == "length" {
				terminal = `"type":"response.incomplete"`
			}
			if strings.Count(recorder.Body.String(), terminal) != 1 {
				t.Fatalf("missing or repeated terminal event: %s", recorder.Body.String())
			}
		}
	}
}

func TestPassthroughPrematureEOFFails(t *testing.T) {
	for _, tc := range []struct {
		in     inbound.InboundType
		format model.APIFormat
		out    outbound.OutboundType
		body   string
	}{
		{inbound.InboundTypeOpenAIResponse, model.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIResponse, startedResponsesStream},
		{inbound.InboundTypeAnthropic, model.APIFormatAnthropicMessage, outbound.OutboundTypeAnthropic, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\",\"model\":\"test\",\"content\":[]}}\n\n"},
	} {
		ra, recorder := newEmptyStreamTestAttempt(t, tc.in, tc.format, tc.out)
		cfg := ra.outAdapter.(model.PassthroughCapable).PassthroughConfig()
		err := ra.handleStreamResponsePassthroughV2(context.Background(), sseTestResponse(tc.body), cfg)
		if !errors.Is(err, stream.ErrIncompleteUpstreamStream) {
			t.Fatalf("truncated %s stream counted as success: %v", tc.format, err)
		}
		if recorder.Body.String() != tc.body {
			t.Fatal("passthrough payload was changed")
		}
	}
}

func TestPassthroughIncompleteAndCancelledAreFailures(t *testing.T) {
	for _, kind := range []string{"response.incomplete", "response.cancelled", "response.canceled"} {
		ra, _ := newEmptyStreamTestAttempt(t, inbound.InboundTypeOpenAIResponse, model.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIResponse)
		cfg := ra.outAdapter.(model.PassthroughCapable).PassthroughConfig()
		err := ra.handleStreamResponsePassthroughV2(context.Background(), sseTestResponse(startedResponsesStream+"data: {\"type\":\""+kind+"\",\"response\":{\"id\":\"resp_error\"}}\n\n"), cfg)
		if !errors.Is(err, errUpstreamStreamError) {
			t.Fatalf("%s counted as success: %v", kind, err)
		}
	}
}
