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

	"github.com/bestruirui/octopus/internal/channelroute"
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

	effectiveType := channelroute.Resolve(channel, modelName, transformerModel.APIFormatOpenAIChatCompletion).Type
	httpClient, err := helper.ChannelHTTPClientWithContext(probeCtx, &channel)
	if err != nil {
		result.ErrorMessage = err.Error()
		result.DurationMS = time.Since(startedAt).Milliseconds()
		return result
	}

	excludedSemanticParams := make(map[string]struct{})
	reasoningMaxDowngraded := false
	for {
		request, rawGroupOverride, buildErr := buildProbeRequestWithGroupOverride(probeCtx, &channel, &usedKey, modelName, groupOverride, excludedSemanticParams, reasoningMaxDowngraded)
		if buildErr != nil {
			result.ErrorMessage = buildErr.Error()
			result.DurationMS = time.Since(startedAt).Milliseconds()
			return result
		}

		applyCustomHeaders(request, channel.CustomHeader)
		// 防止 Go 默认 User-Agent 泄露到上游
		if request.Header.Get("User-Agent") == "" {
			request.Header.Set("User-Agent", "")
		}
		semanticTransformedBody, _ := readProbeRequestBody(request)
		if err := helper.ApplyParamOverrides(request, channel.ParamOverride, rawGroupOverride); err != nil {
			result.ErrorMessage = err.Error()
			result.DurationMS = time.Since(startedAt).Milliseconds()
			return result
		}
		if err := helper.ReapplySemanticGroupWirePrecedence(request, semanticTransformedBody, groupOverride, effectiveType, excludedSemanticParams); err != nil {
			result.ErrorMessage = err.Error()
			result.DurationMS = time.Since(startedAt).Milliseconds()
			return result
		}

		response, requestErr := httpClient.Do(request)
		if requestErr != nil {
			result.ErrorMessage = requestErr.Error()
			result.DurationMS = time.Since(startedAt).Milliseconds()
			return result
		}
		result.HTTPStatus = response.StatusCode
		result.Header = response.Header.Clone()
		result.DurationMS = time.Since(startedAt).Milliseconds()

		if response.StatusCode >= 200 && response.StatusCode < 300 {
			_ = response.Body.Close()
			result.Success = true
			return result
		}

		body, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusBadRequest {
			if !reasoningMaxDowngraded && helper.RejectedUpstreamReasoningMax(groupOverride, body) {
				reasoningMaxDowngraded = true
				continue
			}
			rejectedWireParam := helper.RejectedUpstreamParameter(body)
			semanticParam := helper.SemanticParamForRejectedWireName(groupOverride, rejectedWireParam)
			if semanticParam != "" {
				if _, alreadyExcluded := excludedSemanticParams[semanticParam]; !alreadyExcluded {
					excludedSemanticParams[semanticParam] = struct{}{}
					continue
				}
			}
		}
		if len(body) > 0 {
			result.ErrorMessage = fmt.Sprintf("upstream error: %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		} else {
			result.ErrorMessage = fmt.Sprintf("upstream error: %d", response.StatusCode)
		}
		return result
	}
}

func readProbeRequestBody(request *http.Request) ([]byte, error) {
	if request == nil || request.Body == nil {
		return nil, nil
	}
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return io.ReadAll(body)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return body, nil
}

func buildProbeRequest(ctx context.Context, channel *model.Channel, usedKey *model.ChannelKey, modelName string) (*http.Request, error) {
	request, _, err := buildProbeRequestWithGroupOverride(ctx, channel, usedKey, modelName, nil, nil, false)
	return request, err
}

func buildProbeRequestWithGroupOverride(ctx context.Context, channel *model.Channel, usedKey *model.ChannelKey, modelName string, groupOverride *string, excludedSemanticParams map[string]struct{}, reasoningMaxDowngraded bool) (*http.Request, *string, error) {
	if channel == nil {
		return nil, nil, fmt.Errorf("channel is nil")
	}
	if usedKey == nil {
		return nil, nil, fmt.Errorf("channel key is nil")
	}
	if strings.TrimSpace(usedKey.ChannelKey) == "" {
		return nil, nil, fmt.Errorf("channel key is empty")
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, nil, fmt.Errorf("model name is empty")
	}

	resolution := channelroute.Resolve(*channel, modelName, transformerModel.APIFormatOpenAIChatCompletion)
	effectiveType := resolution.Type
	protocol := probeProtocolChannel
	if channel.Type != outbound.OutboundTypeAuto || (resolution.Source != channelroute.SourceOverride && resolution.Source != channelroute.SourceLearned) {
		protocol = classifyProbeProtocol(effectiveType, modelName)
	} else if effectiveType == outbound.OutboundTypeOpenAIEmbedding {
		protocol = probeProtocolEmbedding
	}
	if protocol == probeProtocolRerank {
		request, err := buildRerankProbeRequest(ctx, channel.GetBaseUrl(), usedKey.ChannelKey, modelName)
		return request, nil, err
	}

	if protocol == probeProtocolEmbedding {
		effectiveType = outbound.OutboundTypeOpenAIEmbedding
	}
	request := buildProbeInternalRequest(effectiveType, modelName)
	rawGroupOverride, err := helper.PrepareSemanticGroupParamOverride(request, groupOverride, effectiveType, excludedSemanticParams)
	if err != nil {
		return nil, nil, err
	}
	if reasoningMaxDowngraded && request.ReasoningEffort == "max" {
		request.ReasoningEffort = "high"
	}
	adapter := outbound.Get(effectiveType)
	if adapter == nil {
		return nil, nil, fmt.Errorf("unsupported outbound type: %d", effectiveType)
	}
	outboundRequest, err := adapter.TransformRequest(ctx, request, channel.GetBaseUrl(), usedKey.ChannelKey)
	return outboundRequest, rawGroupOverride, err
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
