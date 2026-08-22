package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/transformer"
	axonOpenAI "github.com/looplj/axonhub/llm/transformer/openai"
)

// EmbeddingOutbound uses AxonHub's OpenAI transformer for both request and
// response conversion. The local adapter only restores a few response
// envelope fields that AxonHub intentionally does not model yet.
type EmbeddingOutbound struct {
	inner transformer.Outbound
}

func (o *EmbeddingOutbound) ensureAxon(baseURL, key string) (transformer.Outbound, error) {
	if o.inner != nil {
		return o.inner, nil
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com/v1"
	}
	inner, err := axonOpenAI.NewOutboundTransformerWithConfig(&axonOpenAI.Config{
		PlatformType:   axonOpenAI.PlatformOpenAI,
		BaseURL:        baseURL,
		APIKeyProvider: auth.NewStaticKeyProvider(key),
	})
	if err != nil {
		return nil, err
	}
	o.inner = inner
	return inner, nil
}

func (o *EmbeddingOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseURL, key string) (*http.Request, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	if !request.IsEmbeddingRequest() {
		return nil, errors.New("not an embedding request")
	}

	inner, err := o.ensureAxon(baseURL, key)
	if err != nil {
		return nil, err
	}
	req, _, err := model.BuildAxonHTTPRequest(ctx, inner, request)
	if err != nil {
		return nil, fmt.Errorf("failed to transform embedding request with AxonHub: %w", err)
	}
	applyOpenAIOrgProjectHeaders(req, request)
	return req, nil
}

func (o *EmbeddingOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	inner, err := o.ensureAxon("", "")
	if err != nil {
		return nil, err
	}
	return transformEmbeddingResponse(ctx, inner, response)
}

func transformEmbeddingResponse(ctx context.Context, inner transformer.Outbound, response *http.Response) (*model.InternalLLMResponse, error) {
	if response == nil {
		return nil, errors.New("response is nil")
	}
	if response.Body == nil {
		return nil, errors.New("response body is nil")
	}

	// TransformResponse consumes the body, so keep a copy for envelope fields
	// that the current AxonHub embedding model does not expose (id/created).
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("response body is empty")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))

	converted, err := model.TransformResponse(ctx, inner, response)
	if err != nil {
		return nil, err
	}
	if converted == nil {
		return nil, errors.New("AxonHub returned nil embedding response")
	}

	var envelope struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if converted.ID == "" {
			converted.ID = envelope.ID
		}
		if converted.Created == 0 {
			converted.Created = envelope.Created
		}
		if converted.Model == "" {
			converted.Model = envelope.Model
		}
		if converted.Object == "" {
			converted.Object = envelope.Object
		}
	}
	if converted.Object == "" && len(converted.EmbeddingData) > 0 {
		converted.Object = "list"
	}
	return converted, nil
}

func (o *EmbeddingOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	return nil, errors.New("streaming is not supported for embedding API")
}
