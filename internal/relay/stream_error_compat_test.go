package relay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

const overloadedStreamEvent = `{"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded.","type":"service_unavailable_error"}}`
const startedResponsesStream = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_error\",\"model\":\"gpt-5.6-luna\",\"status\":\"in_progress\",\"output\":[]}}\n\n"

func TestResponsesStreamErrorsAreDeliveredAndMarkedFailed(t *testing.T) {
	for _, failure := range []string{
		overloadedStreamEvent,
		`{"type":"response.failed","response":{"id":"resp_error","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}}`,
		`{"type":"error","code":"server_is_overloaded","message":"Our servers are currently overloaded."}`,
	} {
		for _, started := range []bool{false, true} {
			ra, recorder := newEmptyStreamTestAttempt(t, inbound.InboundTypeOpenAIResponse, transformerModel.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIResponse)
			body := "data: " + failure + "\n\n"
			if started {
				body = startedResponsesStream + body
			}
			err := ra.handleStreamResponseV2(context.Background(), sseTestResponse(body))
			if !errors.Is(err, errUpstreamStreamError) || !strings.Contains(err.Error(), "overloaded") {
				t.Fatalf("error disappeared: %v", err)
			}
			output := recorder.Body.String()
			if started {
				if !strings.Contains(output, "response.failed") || !strings.Contains(output, "server_is_overloaded") || strings.Contains(output, "response.completed") {
					t.Fatalf("client did not receive terminal failure: %s", output)
				}
			} else if output != "" {
				t.Fatalf("first error committed the response instead of allowing fallback: %s", output)
			}
		}
	}
}

func TestResponsesPassthroughStreamErrorIsNotCountedAsSuccess(t *testing.T) {
	ra, recorder := newEmptyStreamTestAttempt(t, inbound.InboundTypeOpenAIResponse, transformerModel.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIResponse)
	cfg := ra.outAdapter.(transformerModel.PassthroughCapable).PassthroughConfig()
	body := startedResponsesStream + "data: " + overloadedStreamEvent + "\n\n"
	err := ra.handleStreamResponsePassthroughV2(context.Background(), sseTestResponse(body), cfg)
	if !errors.Is(err, errUpstreamStreamError) || !strings.Contains(recorder.Body.String(), "overloaded") {
		t.Fatalf("passthrough swallowed failure: err=%v output=%s", err, recorder.Body.String())
	}
}
