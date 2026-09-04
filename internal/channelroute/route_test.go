package channelroute

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestResolveAutoRoutePriority(t *testing.T) {
	channel := dbmodel.Channel{
		Type: outbound.OutboundTypeAuto,
		ModelRoutes: dbmodel.ChannelModelRoutes{
			FallbackType: outbound.OutboundTypeOpenAIChat,
			Overrides:    map[string]outbound.OutboundType{"CLAUDE-X": outbound.OutboundTypeOpenAIResponse},
			Learned:      map[string]outbound.OutboundType{"gpt-5.6": outbound.OutboundTypeOpenAIChat},
		},
	}
	tests := []struct {
		name       string
		model      string
		format     transformermodel.APIFormat
		wantType   outbound.OutboundType
		wantSource Source
	}{
		{"manual", "claude-x", transformermodel.APIFormatOpenAIChatCompletion, outbound.OutboundTypeOpenAIResponse, SourceOverride},
		{"learned", "gpt-5.6", transformermodel.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIChat, SourceLearned},
		{"inferred", "vendor/gemini-3-pro", transformermodel.APIFormatOpenAIChatCompletion, outbound.OutboundTypeGemini, SourceInferred},
		{"fallback", "custom-model", transformermodel.APIFormatOpenAIChatCompletion, outbound.OutboundTypeOpenAIChat, SourceFallback},
		{"required embedding", "custom-model", transformermodel.APIFormatOpenAIEmbedding, outbound.OutboundTypeOpenAIEmbedding, SourceRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(channel, tt.model, tt.format)
			if got.Type != tt.wantType || got.Source != tt.wantSource {
				t.Fatalf("Resolve() = %+v, want type=%d source=%s", got, tt.wantType, tt.wantSource)
			}
		})
	}
}

func TestInferModelFamilies(t *testing.T) {
	tests := map[string]outbound.OutboundType{
		"BAAI/bge-m3":           outbound.OutboundTypeOpenAIEmbedding,
		"anthropic/claude-opus": outbound.OutboundTypeAnthropic,
		"google/gemini-3-pro":   outbound.OutboundTypeGemini,
		"openai/gpt-5.6-sol":    outbound.OutboundTypeOpenAIResponse,
		"doubao-seed-1-8":       outbound.OutboundTypeVolcengine,
	}
	for name, want := range tests {
		got, ok := Infer(name)
		if !ok || got != want {
			t.Fatalf("Infer(%q) = %d, %t; want %d, true", name, got, ok, want)
		}
	}
}
