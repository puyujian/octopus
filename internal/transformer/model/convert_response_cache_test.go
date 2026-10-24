package model

import (
	"testing"

	"github.com/looplj/axonhub/llm"
)

// AxonHub usage includes cache in PromptTokens. Only the Anthropic API boundary
// restores separate counters; a missing total must not change that convention.
func TestFromLLMUsageCacheSemantics(t *testing.T) {
	cases := []struct {
		name          string
		format        APIFormat
		usage         *llm.Usage
		wantPrompt    int64
		wantCacheRead int64
		wantNonCached int64
		wantEffective int64
	}{
		{
			name: "inclusive prompt keeps cache out of CacheReadInputTokens",
			usage: &llm.Usage{
				PromptTokens:        3677,
				CompletionTokens:    8,
				TotalTokens:         3685,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 3456},
			},
			wantPrompt:    3677,
			wantCacheRead: 0,
			wantNonCached: 221,
			wantEffective: 3677,
		},
		{
			name:   "Anthropic normalization restores exclusive prompt",
			format: APIFormatAnthropicMessage,
			usage: &llm.Usage{
				PromptTokens:        3677,
				CompletionTokens:    8,
				TotalTokens:         3685,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 3456},
			},
			wantPrompt:    221,
			wantCacheRead: 3456,
			wantNonCached: 221,
			wantEffective: 3677,
		},
		{
			name: "omitted total still uses inclusive prompt",
			usage: &llm.Usage{
				PromptTokens:        3677,
				CompletionTokens:    8,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 3456},
			},
			wantPrompt:    3677,
			wantNonCached: 221,
			wantEffective: 3677,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FromLLMUsage(tc.usage)
			NormalizeResponseUsageForAPI(&InternalLLMResponse{APIFormat: tc.format, Usage: got})
			if got.PromptTokens != tc.wantPrompt {
				t.Fatalf("prompt tokens: got %d want %d", got.PromptTokens, tc.wantPrompt)
			}
			if got.CacheReadInputTokens != tc.wantCacheRead {
				t.Fatalf("cache read tokens: got %d want %d", got.CacheReadInputTokens, tc.wantCacheRead)
			}
			if got.BillableNonCachedInput() != tc.wantNonCached {
				t.Fatalf("billable non-cached input: got %d want %d", got.BillableNonCachedInput(), tc.wantNonCached)
			}
			if got.EffectiveInputTokens() != tc.wantEffective {
				t.Fatalf("effective input tokens: got %d want %d", got.EffectiveInputTokens(), tc.wantEffective)
			}
			if got.PromptTokensDetails == nil || got.PromptTokensDetails.CachedTokens != 3456 {
				t.Fatalf("prompt tokens details must mirror the cached subset, got %#v", got.PromptTokensDetails)
			}
		})
	}
}
