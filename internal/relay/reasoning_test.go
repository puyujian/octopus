package relay

import "testing"

func TestExtractReasoningEffort(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "openai", body: `{"reasoning_effort":"high"}`, want: "high"},
		{name: "responses", body: `{"reasoning":{"effort":"medium"}}`, want: "medium"},
		{name: "anthropic", body: `{"output_config":{"effort":"low"}}`, want: "low"},
		{name: "gemini level", body: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"}}}`, want: "high"},
		{name: "budget", body: `{"thinking":{"budget_tokens":4096}}`, want: "budget 4096"},
		{name: "enabled", body: `{"enable_thinking":true}`, want: "enabled"},
		{name: "disabled", body: `{"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`, want: "disabled"},
		{name: "invalid", body: `{"reasoning_effort":12}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractReasoningEffort([]byte(tt.body)); got != tt.want {
				t.Fatalf("extractReasoningEffort() = %q, want %q", got, tt.want)
			}
		})
	}
}
