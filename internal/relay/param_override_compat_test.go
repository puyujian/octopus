package relay

import "testing"

func TestUnsupportedUpstreamParameter(t *testing.T) {
	for _, test := range []struct {
		body string
		want string
	}{
		{`{"detail":"Unsupported parameter: reasoning_effort"}`, "reasoning_effort"},
		{"{\"error\":{\"message\":\"Validation: Unsupported parameter(s): `prompt_cache_key`\",\"type\":\"bad_response_status_code\"}}", "prompt_cache_key"},
		{`{"detail":"Unsupported parameters: 'reasoning_effort', 'temperature'"}`, "reasoning_effort"},
		{`{"error":{"message":"Unknown parameter 'reasoning.effort'"}}`, "reasoning.effort"},
		{`{"message":"unrecognized parameter = generationConfig.thinkingConfig"}`, "generationConfig.thinkingConfig"},
		{`{"detail":"invalid value for reasoning_effort"}`, ""},
		{`not json and not an explicit rejection`, ""},
	} {
		if got := unsupportedUpstreamParameter([]byte(test.body)); got != test.want {
			t.Errorf("unsupportedUpstreamParameter(%s) = %q, want %q", test.body, got, test.want)
		}
	}
}
