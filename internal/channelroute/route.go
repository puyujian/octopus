package channelroute

import (
	"net/http"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

type Source string

const (
	SourceFixed    Source = "fixed"
	SourceRequired Source = "required"
	SourceOverride Source = "manual"
	SourceLearned  Source = "learned"
	SourceInferred Source = "inferred"
	SourceFallback Source = "fallback"
)

type Resolution struct {
	Type   outbound.OutboundType
	Source Source
}

// DetectMismatch only recognizes endpoint/protocol failures. It intentionally
// excludes authentication, quota, timeouts and generic upstream failures.
func DetectMismatch(statusCode int, message string) (outbound.OutboundType, bool, bool) {
	lower := strings.ToLower(message)
	if statusCode != http.StatusNotFound && statusCode != http.StatusMethodNotAllowed {
		switch {
		case strings.Contains(lower, "responses api") || strings.Contains(lower, "use /v1/responses"):
			return outbound.OutboundTypeOpenAIResponse, true, true
		case strings.Contains(lower, "anthropic-version") || strings.Contains(lower, "use /v1/messages"):
			return outbound.OutboundTypeAnthropic, true, true
		default:
			return 0, false, false
		}
	}
	if strings.Contains(lower, "invalid url") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "unsupported endpoint") ||
		strings.Contains(lower, "method not allowed") {
		return 0, false, true
	}
	return 0, false, false
}

func Resolve(channel dbmodel.Channel, modelName string, apiFormat transformermodel.APIFormat) Resolution {
	if channel.Type != outbound.OutboundTypeAuto {
		return Resolution{Type: channel.Type, Source: SourceFixed}
	}
	if apiFormat == transformermodel.APIFormatOpenAIEmbedding {
		return Resolution{Type: outbound.OutboundTypeOpenAIEmbedding, Source: SourceRequired}
	}

	routes := channel.ModelRoutes.Normalize()
	key := dbmodel.NormalizeChannelModelRouteKey(modelName)
	if routeType, ok := routes.Overrides[key]; ok {
		return Resolution{Type: routeType, Source: SourceOverride}
	}
	if routeType, ok := routes.Learned[key]; ok {
		return Resolution{Type: routeType, Source: SourceLearned}
	}
	if routeType, ok := Infer(modelName); ok {
		return Resolution{Type: routeType, Source: SourceInferred}
	}
	return Resolution{Type: routes.FallbackType, Source: SourceFallback}
}

func Infer(modelName string) (outbound.OutboundType, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	if normalized == "" {
		return outbound.OutboundTypeOpenAIChat, false
	}
	name := normalized
	if index := strings.LastIndexAny(name, "/:"); index >= 0 && index+1 < len(name) {
		name = name[index+1:]
	}

	if strings.Contains(name, "embedding") || strings.Contains(name, "embed-") || strings.Contains(name, "embed_") {
		return outbound.OutboundTypeOpenAIEmbedding, true
	}
	for _, prefix := range []string{"bge-", "e5-", "gte-", "m3e-", "multilingual-e5-"} {
		if strings.HasPrefix(name, prefix) {
			return outbound.OutboundTypeOpenAIEmbedding, true
		}
	}
	if strings.HasPrefix(name, "claude") {
		return outbound.OutboundTypeAnthropic, true
	}
	if strings.HasPrefix(name, "gemini") {
		return outbound.OutboundTypeGemini, true
	}
	if strings.HasPrefix(name, "gpt-5") {
		return outbound.OutboundTypeOpenAIResponse, true
	}
	if strings.HasPrefix(name, "doubao-seed") {
		return outbound.OutboundTypeVolcengine, true
	}
	return outbound.OutboundTypeOpenAIChat, false
}

func Fallback(channel dbmodel.Channel, current outbound.OutboundType, suggested outbound.OutboundType) (outbound.OutboundType, bool) {
	if channel.Type != outbound.OutboundTypeAuto {
		return 0, false
	}
	routes := channel.ModelRoutes.Normalize()
	if suggested.IsConcrete() && suggested != current {
		return suggested, true
	}
	if routes.FallbackType != current {
		return routes.FallbackType, true
	}
	if current != outbound.OutboundTypeOpenAIChat {
		return outbound.OutboundTypeOpenAIChat, true
	}
	return 0, false
}
