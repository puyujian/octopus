package relay

import (
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func TestRelayLogTPSSetsGenerationRate(t *testing.T) {
	tests := []struct {
		name       string
		output     int
		ftut       int
		useTime    int
		want       float64
		wantAbsent bool
	}{
		{name: "generation window", output: 100, ftut: 500, useTime: 2500, want: 50},
		{name: "no first token", output: 100, useTime: 2000, want: 50},
		{name: "invalid first token falls back", output: 100, ftut: 2500, useTime: 2500, want: 40},
		{name: "no output", output: 0, useTime: 1000, wantAbsent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := model.RelayLog{OutputTokens: tt.output, Ftut: tt.ftut, UseTime: tt.useTime}
			log.PopulateDerivedMetrics()
			if tt.wantAbsent {
				if log.TPS != nil {
					t.Fatalf("TPS = %v, want nil", *log.TPS)
				}
				return
			}
			if log.TPS == nil || *log.TPS != tt.want {
				t.Fatalf("TPS = %v, want %v", log.TPS, tt.want)
			}
		})
	}
}

func TestRelayLogTPSUsesWholeDurationWhenFTUTUnavailable(t *testing.T) {
	log := model.RelayLog{OutputTokens: 3, Ftut: -1, UseTime: 1500}
	log.PopulateDerivedMetrics()
	if log.TPS == nil || *log.TPS != 2 {
		t.Fatalf("TPS = %v, want 2", log.TPS)
	}
}

func TestRelayMetricsSetTransportRequestPayloadExtractsReasoning(t *testing.T) {
	m := NewRelayMetrics(0, "model", nil, nil)
	m.SetTransportRequestPayload([]byte(`{"reasoning":{"effort":"high"}}`), "model")
	if m.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", m.ReasoningEffort)
	}
	if m.TransportInputTokens == nil || *m.TransportInputTokens == 0 {
		t.Fatalf("expected transport token estimate")
	}
}

func TestRelayLogTPSSaveCompatibility(t *testing.T) {
	start := time.Now()
	log := model.RelayLog{Time: start.Unix(), OutputTokens: 12, Ftut: 100, UseTime: 700}
	log.PopulateDerivedMetrics()
	if log.TPS == nil || *log.TPS != 20 {
		t.Fatalf("TPS = %v, want 20", log.TPS)
	}
}

// usage 完全缺失时，应使用 TransportInputTokens 兜底填充 input，output 保持 0。
func TestSetInternalResponseFallbackWhenUsageMissing(t *testing.T) {
	m := &RelayMetrics{TransportInputTokens: intPtr(123)}
	m.SetInternalResponse(&transformerModel.InternalLLMResponse{}, "test-model")

	if m.Stats.InputToken != 123 {
		t.Fatalf("input token: got %d want 123 (fallback)", m.Stats.InputToken)
	}
	if m.BillInputTokens == nil || *m.BillInputTokens != 123 {
		t.Fatalf("bill input tokens: got %v want 123", m.BillInputTokens)
	}
	if m.Stats.OutputToken != 0 {
		t.Fatalf("output token: got %d want 0", m.Stats.OutputToken)
	}
}

// usage 存在但输入侧全为 0（仅上报 output）时，input 兜底、output 保留。
func TestSetInternalResponseFallbackWhenInputZero(t *testing.T) {
	m := &RelayMetrics{TransportInputTokens: intPtr(50)}
	m.SetInternalResponse(&transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{PromptTokens: 0, CompletionTokens: 30},
	}, "test-model")

	if m.Stats.InputToken != 50 {
		t.Fatalf("input token: got %d want 50 (fallback)", m.Stats.InputToken)
	}
	if m.Stats.OutputToken != 30 {
		t.Fatalf("output token: got %d want 30 (preserved)", m.Stats.OutputToken)
	}
}

// 上游正常上报 input 时不触发兜底（保留真实值，而非估算值）。
func TestSetInternalResponseNoFallbackWhenInputReported(t *testing.T) {
	m := &RelayMetrics{TransportInputTokens: intPtr(999)}
	m.SetInternalResponse(&transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{PromptTokens: 12, CompletionTokens: 7},
	}, "test-model")

	if m.Stats.InputToken != 12 {
		t.Fatalf("input token: got %d want 12 (reported, not fallback)", m.Stats.InputToken)
	}
	if m.Stats.OutputToken != 7 {
		t.Fatalf("output token: got %d want 7", m.Stats.OutputToken)
	}
}

// 仅缓存命中（input_tokens=0 但 cache_read>0）属于已上报输入，不应被估算覆盖。
func TestSetInternalResponseNoFallbackWhenCacheOnly(t *testing.T) {
	m := &RelayMetrics{TransportInputTokens: intPtr(999)}
	m.SetInternalResponse(&transformerModel.InternalLLMResponse{
		Usage: &transformerModel.Usage{PromptTokens: 0, CacheReadInputTokens: 40, CompletionTokens: 5},
	}, "test-model")

	if m.Stats.InputToken != 0 {
		t.Fatalf("input token: got %d want 0 (cache-only is reported input)", m.Stats.InputToken)
	}
}
