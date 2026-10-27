package relay

import (
	"context"
	"encoding/json"
	"fmt"

	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// projectReplayForOutbound keeps exact raw history authoritative for Responses
// routes, but gives other protocols the same structured projection as an
// ordinary Responses request. Never mutate the canonical request: a later
// attempt may use a native Responses route.
func projectReplayForOutbound(ctx context.Context, req *model.InternalLLMRequest, typ outbound.OutboundType) (*model.InternalLLMRequest, error) {
	if req == nil || !req.IsOpenAIExactReplayRequest() || typ == outbound.OutboundTypeOpenAIResponse || typ == outbound.OutboundTypeVolcengine {
		return req, nil
	}
	body, err := json.Marshal(struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}{Model: req.Model, Input: req.OpenAIRawInputItems()})
	if err != nil {
		return nil, fmt.Errorf("failed to encode replay input: %w", err)
	}
	parsed, err := (&openaiInbound.ResponseInbound{}).TransformRequest(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("failed to project replay input: %w", err)
	}
	if req.HasOpenAIResponsesPassthrough() || parsed.HasOpenAIResponsesPassthrough() {
		return nil, fmt.Errorf("replay history requires a native Responses route: %s", parsed.OpenAIResponsesPassthroughReasonTextValue())
	}
	projected := cloneInternalRequest(req)
	projected.Messages = append(retainInstructionMessages(req.Messages), parsed.Messages...)
	delete(projected.TransformerMetadata, model.TransformerMetadataWSExecutionMode)
	return projected, nil
}
