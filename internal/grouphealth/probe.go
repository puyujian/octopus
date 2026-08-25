package grouphealth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

type ProbeResult struct {
	Success      bool
	HTTPStatus   int
	DurationMS   int64
	ErrorMessage string
	Header       http.Header // 上游响应头，供 POR 门3 做 Cloudflare 指纹识别
}

type Prober struct {
	CandidateTimeout time.Duration
}

type probeProtocol int

const (
	probeProtocolChannel probeProtocol = iota
	probeProtocolEmbedding
	probeProtocolRerank
)

func NewProber() *Prober {
	return &Prober{
		CandidateTimeout: 12 * time.Second,
	}
}

func (p *Prober) RunCandidate(ctx context.Context, channel model.Channel, usedKey model.ChannelKey, modelName string) ProbeResult {
	return p.runCandidate(ctx, channel, usedKey, modelName, nil)
}

// RunCandidateWithGroupOverride probes a channel using the same group-level
// force override as a normal relay request. The separate method keeps the
// existing channelProber interface compatible with outlier-retirement tests.
func (p *Prober) RunCandidateWithGroupOverride(ctx context.Context, channel model.Channel, usedKey model.ChannelKey, modelName string, groupOverride *string) ProbeResult {
	return p.runCandidate(ctx, channel, usedKey, modelName, groupOverride)
}

func (p *Prober) runCandidate(ctx context.Context, channel model.Channel, usedKey model.ChannelKey, modelName string, groupOverride *string) ProbeResult {
	startedAt := time.Now()
	result := ProbeResult{}

	timeout := p.CandidateTimeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, err := buildProbeRequest(probeCtx, &channel, &usedKey, modelName)
	if err != nil {
		result.ErrorMessage = err.Error()
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}

	applyCustomHeaders(request, channel.CustomHeader)
	// 防止 Go 默认 User-Agent 泄露到上游
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "")
	}
	if err := helper.ApplyParamOverrides(request, channel.ParamOverride, groupOverride); err != nil {
		result.ErrorMessage = err.Error()
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}

	httpClient, err := helper.ChannelHTTPClientWithContext(probeCtx, &channel)
	if err != nil {
		result.ErrorMessage = err.Error()
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}

	response, err := httpClient.Do(request)
	if err != nil {
		result.ErrorMessage = err.Error()
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}
	defer response.Body.Close()

	result.HTTPStatus = response.StatusCode
	result.Header = response.Header.Clone()
	result.DurationMS = time.Since(startedAt).Milliseconds()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Success = true
		return result
	}

	body, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
	if len(body) > 0 {
		result.ErrorMessage = fmt.Sprintf("upstream error: %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	} else {
		result.ErrorMessage = fmt.Sprintf("upstream error: %d", response.StatusCode)
	}
	return result
}

func buildProbeRequest(ctx context.Context, channel *model.Channel, usedKey *model.ChannelKey, modelName string) (*http.Request, error) {
	if channel == nil {
		return nil, fmt.Errorf("channel is nil")
	}
	if usedKey == nil {
		return nil, fmt.Errorf("channel key is nil")
	}
	if strings.TrimSpace(usedKey.ChannelKey) == "" {
		return nil, fmt.Errorf("channel key is empty")
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, fmt.Errorf("model name is empty")
	}

	protocol := classifyProbeProtocol(channel.Type, modelName)
	if protocol == probeProtocolRerank {
		return buildRerankProbeRequest(ctx, channel.GetBaseUrl(), usedKey.ChannelKey, modelName)
	}

	effectiveType := channel.Type
	if protocol == probeProtocolEmbedding {
		effectiveType = outbound.OutboundTypeOpenAIEmbedding
	}
	request := buildProbeInternalRequest(effectiveType, modelName)
	adapter := outbound.Get(effectiveType)
	if adapter == nil {
		return nil, fmt.Errorf("unsupported outbound type: %d", effectiveType)
	}
	return adapter.TransformRequest(ctx, request, channel.GetBaseUrl(), usedKey.ChannelKey)
}

// classifyProbeProtocol keeps explicitly configured non-Chat channels on their
// native protocol. OpenAI-compatible Chat channels can expose mixed model
// families, so health checks infer low-cost embedding and rerank probes from
// the actual group-item model instead of blindly calling chat/completions.
func classifyProbeProtocol(channelType outbound.OutboundType, modelName string) probeProtocol {
	if channelType == outbound.OutboundTypeOpenAIEmbedding {
		return probeProtocolEmbedding
	}
	if channelType != outbound.OutboundTypeOpenAIChat {
		return probeProtocolChannel
	}

	normalized := strings.ToLower(strings.TrimSpace(modelName))
	if normalized == "" {
		return probeProtocolChannel
	}
	if strings.Contains(normalized, "rerank") {
		return probeProtocolRerank
	}
	if isEmbeddingModelName(normalized) {
		return probeProtocolEmbedding
	}
	return probeProtocolChannel
}

func isEmbeddingModelName(normalized string) bool {
	if strings.Contains(normalized, "embedding") || strings.Contains(normalized, "embed-") || strings.Contains(normalized, "embed_") {
		return true
	}

	// Model IDs commonly include a provider namespace. Classify the final
	// segment so aliases such as BAAI/bge-m3 and cf/bge-m3 are handled alike.
	name := normalized
	if index := strings.LastIndexAny(name, "/:"); index >= 0 && index+1 < len(name) {
		name = name[index+1:]
	}
	for _, prefix := range []string{
		"bge-",
		"e5-",
		"gte-",
		"m3e-",
		"multilingual-e5-",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func buildRerankProbeRequest(ctx context.Context, baseURL, key, modelName string) (*http.Request, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	body, err := json.Marshal(struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	}{
		Model:     modelName,
		Query:     "ping",
		Documents: []string{"ping"},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal rerank probe: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to build rerank probe: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func buildProbeInternalRequest(channelType outbound.OutboundType, modelName string) *transformerModel.InternalLLMRequest {
	stream := false
	ping := "ping"
	one := int64(1)

	switch channelType {
	case outbound.OutboundTypeOpenAIEmbedding:
		return &transformerModel.InternalLLMRequest{
			Model:        modelName,
			RawAPIFormat: transformerModel.APIFormatOpenAIEmbedding,
			EmbeddingInput: &transformerModel.EmbeddingInput{
				Single: &ping,
			},
		}
	case outbound.OutboundTypeOpenAIResponse:
		return &transformerModel.InternalLLMRequest{
			Model:               modelName,
			RawAPIFormat:        transformerModel.APIFormatOpenAIResponse,
			Messages:            []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &ping}}},
			Stream:              &stream,
			MaxCompletionTokens: &one,
		}
	case outbound.OutboundTypeAnthropic:
		return &transformerModel.InternalLLMRequest{
			Model:        modelName,
			RawAPIFormat: transformerModel.APIFormatAnthropicMessage,
			Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &ping}}},
			Stream:       &stream,
			MaxTokens:    &one,
		}
	case outbound.OutboundTypeGemini:
		return &transformerModel.InternalLLMRequest{
			Model:        modelName,
			RawAPIFormat: transformerModel.APIFormatGeminiContents,
			Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &ping}}},
			Stream:       &stream,
			MaxTokens:    &one,
		}
	case outbound.OutboundTypeVolcengine:
		return &transformerModel.InternalLLMRequest{
			Model:        modelName,
			RawAPIFormat: transformerModel.APIFormatOpenAIChatCompletion,
			Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &ping}}},
			Stream:       &stream,
			MaxTokens:    &one,
		}
	default:
		return &transformerModel.InternalLLMRequest{
			Model:        modelName,
			RawAPIFormat: transformerModel.APIFormatOpenAIChatCompletion,
			Messages:     []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &ping}}},
			Stream:       &stream,
			MaxTokens:    &one,
		}
	}
}

func applyCustomHeaders(request *http.Request, headers []model.CustomHeader) {
	if request == nil {
		return
	}
	for _, header := range headers {
		key := strings.TrimSpace(header.HeaderKey)
		if key == "" {
			continue
		}
		request.Header.Set(key, header.HeaderValue)
	}
}
