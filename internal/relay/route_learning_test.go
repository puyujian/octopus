package relay

import (
	"errors"
	"net/http"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestDetectProtocolFallbackIsConservative(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		err        error
		wantType   outbound.OutboundType
		want       bool
	}{
		{"invalid endpoint", http.StatusNotFound, errors.New("Invalid URL (POST /v1/messages)"), outbound.OutboundTypeAnthropic, true},
		{"explicit responses target", http.StatusBadRequest, errors.New("use /v1/responses; Responses API required"), outbound.OutboundTypeOpenAIResponse, true},
		{"auth", http.StatusUnauthorized, errors.New("invalid api key"), 0, false},
		{"rate limit", http.StatusTooManyRequests, errors.New("rate limited"), 0, false},
		{"server error", http.StatusBadGateway, errors.New("upstream unavailable"), 0, false},
		{"semantic bad request", http.StatusBadRequest, errors.New("temperature must be between 0 and 2"), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, got := detectProtocolFallback(tt.statusCode, inbound.InboundTypeOpenAIChat, tt.err)
			if got != tt.want || gotType != tt.wantType {
				t.Fatalf("detectProtocolFallback() = (%d, %t), want (%d, %t)", gotType, got, tt.wantType, tt.want)
			}
		})
	}
}
