package volcengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

var supportedReasoningEffortModel = map[string]bool{
	"doubao-seed-1-8-251228":      true,
	"doubao-seed-1-6-lite-251015": true,
	"doubao-seed-1-6-251015":      true,
}

// ResponseOutbound reuses the OpenAI Responses transformer and applies the
// Volcengine-only thinking and partial input fields after common conversion.
type ResponseOutbound struct {
	inner openaiOutbound.ResponseOutbound
}

func (o *ResponseOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseURL, key string) (*http.Request, error) {
	if request == nil {
		return nil, fmt.Errorf("request is nil")
	}

	request.NormalizeMessages()
	commonReq, err := o.inner.TransformRequest(ctx, request, baseURL, key)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(commonReq.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read Responses request: %w", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to decode Responses request: %w", err)
	}
	delete(payload, "metadata")
	if !supportedReasoningEffortModel[request.Model] {
		delete(payload, "reasoning")
	}

	if thinking := thinkingFor(request.ReasoningEffort); thinking.Type != "" {
		encoded, marshalErr := json.Marshal(thinking)
		if marshalErr != nil {
			return nil, marshalErr
		}
		payload["thinking"] = encoded
	}

	if input, ok := payload["input"]; ok {
		var commonInput openaiOutbound.ResponsesInput
		if err := json.Unmarshal(input, &commonInput); err == nil {
			converted := convertToResponsesInput(commonInput)
			encoded, marshalErr := json.Marshal(converted)
			if marshalErr != nil {
				return nil, marshalErr
			}
			payload["input"] = encoded
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Volcengine Responses request: %w", err)
	}
	commonReq.Body = io.NopCloser(bytes.NewReader(encoded))
	commonReq.ContentLength = int64(len(encoded))
	commonReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(encoded)), nil
	}
	return commonReq, nil
}

func (o *ResponseOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	return o.inner.TransformResponse(ctx, response)
}

func (o *ResponseOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	return o.inner.TransformStream(ctx, eventData)
}

type ThinkingType string

const (
	ThinkingTypeAuto     ThinkingType = "auto"
	ThinkingTypeDisabled ThinkingType = "disabled"
	ThinkingTypeEnabled  ThinkingType = "enabled"
)

type Thinking struct {
	Type ThinkingType `json:"type"`
}

func thinkingFor(effort string) Thinking {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "none":
		return Thinking{Type: ThinkingTypeDisabled}
	case "low", "medium", "high", "max":
		return Thinking{Type: ThinkingTypeEnabled}
	default:
		return Thinking{}
	}
}

type ResponsesInput struct {
	Text  *string
	Items []ResponsesItem
}

func (i ResponsesInput) MarshalJSON() ([]byte, error) {
	if i.Text != nil {
		return json.Marshal(i.Text)
	}
	return json.Marshal(i.Items)
}

func (i *ResponsesInput) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		i.Text = &text
		return nil
	}
	var items []ResponsesItem
	if err := json.Unmarshal(data, &items); err == nil {
		i.Items = items
		return nil
	}
	return fmt.Errorf("invalid input format")
}

type ResponsesItem struct {
	openaiOutbound.ResponsesItem
	Partial bool `json:"partial,omitempty"`
}

func convertToResponsesInput(input openaiOutbound.ResponsesInput) ResponsesInput {
	result := ResponsesInput{Text: input.Text}
	if input.Text != nil {
		return result
	}
	for _, item := range input.Items {
		result.Items = append(result.Items, ResponsesItem{ResponsesItem: item})
	}
	if len(result.Items) > 0 {
		last := len(result.Items) - 1
		if result.Items[last].Role == "assistant" {
			result.Items[last].Partial = true
		}
	}
	return result
}
