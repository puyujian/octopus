package helper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func TestApplyJSONParamOverridesPrecedenceAndDeepMerge(t *testing.T) {
	channel := `{"temperature":0.2,"nested":{"channel_only":true},"array":[2],"nullable":null}`
	group := `{"temperature":0.7,"nested":{"group_only":true},"array":[3],"nullable":"restored"}`
	body, err := ApplyJSONParamOverrides(
		[]byte(`{"temperature":1,"nested":{"original":true},"array":[1],"nullable":"value"}`),
		&channel,
		&group,
	)
	if err != nil {
		t.Fatalf("ApplyJSONParamOverrides returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode merged body: %v", err)
	}
	if got["temperature"] != float64(0.7) {
		t.Fatalf("group override did not win: %#v", got["temperature"])
	}
	array, ok := got["array"].([]any)
	if !ok || len(array) != 1 || array[0] != float64(3) {
		t.Fatalf("arrays should be replaced as a whole: %#v", got["array"])
	}
	if got["nullable"] != "restored" {
		t.Fatalf("scalar/null replacement failed: %#v", got["nullable"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["channel_only"] != true || nested["group_only"] != true {
		t.Fatalf("expected recursive merge of channel and group objects: %#v", got["nested"])
	}
	if _, exists := nested["original"]; exists {
		t.Fatalf("channel shallow merge should replace the original nested object: %#v", nested)
	}
}

func TestApplyJSONParamOverridesProtectsTopLevelRoutingFields(t *testing.T) {
	group := `{"model":"forced","nested":{"model":"allowed"}}`
	body := []byte(`{"model":"actual","stream":true,"nested":{"model":"original"}}`)
	got, err := ApplyJSONParamOverrides(body, nil, &group)
	if err == nil || !strings.Contains(err.Error(), "protected field") {
		t.Fatalf("expected protected-field error, got %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("failed group override should leave body unchanged: %s", got)
	}

	allowed := `{"generation_config":{"thinking_config":{"thinking_level":"high"}}}`
	got, err = ApplyJSONParamOverrides(body, nil, &allowed)
	if err != nil {
		t.Fatalf("nested business override returned error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "actual" || decoded["stream"] != true {
		t.Fatalf("protected fields changed unexpectedly: %#v", decoded)
	}
}

func TestApplyParamOverrideRestoresRequestBody(t *testing.T) {
	override := `{"max_tokens":42}`
	req, err := http.NewRequest(http.MethodPost, "http://example.test", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyParamOverride(req, &override); err != nil {
		t.Fatal(err)
	}
	if req.ContentLength <= 0 || req.GetBody == nil {
		t.Fatalf("override did not refresh request body metadata")
	}
	body, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["max_tokens"] != float64(42) {
		t.Fatalf("unexpected request override: %#v", decoded)
	}
}

func TestValidateGroupParamOverride(t *testing.T) {
	for _, value := range []string{"", `{"temperature":0.5}`, ` {"nested":{"x":1}} `, `{"$octopus":{"reasoning_effort":"high","max_output_tokens":2048}}`, `{"$octopus":{"reasoning_effort":"max"}}`} {
		if err := ValidateGroupParamOverride(value); err != nil {
			t.Errorf("valid override %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"[]",
		`{"MODEL":"m"}`,
		`{"stream":false}`,
		"not-json",
		`{"$octopus":[]}`,
		`{"$octopus":{"reasoning_effort":"extreme"}}`,
		`{"$octopus":{"temperature":3}}`,
		`{"$octopus":{"unknown":true}}`,
	} {
		if err := ValidateGroupParamOverride(value); err == nil {
			t.Errorf("invalid/protected override %q accepted", value)
		}
	}
}

func TestPrepareSemanticGroupParamOverride(t *testing.T) {
	override := `{"$octopus":{"temperature":0.4,"top_p":0.8,"max_output_tokens":2048,"reasoning_effort":"minimal"},"service_tier":"priority"}`
	request := &transformerModel.InternalLLMRequest{}
	raw, err := PrepareSemanticGroupParamOverride(request, &override, outbound.OutboundTypeAnthropic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.Temperature == nil || *request.Temperature != 0.4 || request.TopP == nil || *request.TopP != 0.8 {
		t.Fatalf("sampling semantic parameters not applied: %#v", request)
	}
	if request.MaxTokens == nil || *request.MaxTokens != 2048 || request.MaxCompletionTokens != nil {
		t.Fatalf("Anthropic max token mapping is incorrect: %#v", request)
	}
	if request.ReasoningEffort != "low" {
		t.Fatalf("Anthropic minimal effort should be promoted to low, got %q", request.ReasoningEffort)
	}
	if raw == nil || *raw != `{"service_tier":"priority"}` {
		t.Fatalf("unexpected provider-native remainder: %v", raw)
	}

	excluded := map[string]struct{}{SemanticParamReasoningEffort: {}}
	request = &transformerModel.InternalLLMRequest{}
	if _, err := PrepareSemanticGroupParamOverride(request, &override, outbound.OutboundTypeOpenAIResponse, excluded); err != nil {
		t.Fatal(err)
	}
	if request.ReasoningEffort != "" {
		t.Fatalf("excluded semantic parameter was still applied: %#v", request)
	}
	if request.MaxCompletionTokens == nil || *request.MaxCompletionTokens != 2048 || request.MaxTokens != nil {
		t.Fatalf("Responses max token mapping is incorrect: %#v", request)
	}
}

func TestPrepareSemanticGroupParamOverrideUpgradesLegacyReasoningName(t *testing.T) {
	override := `{"reasoning_effort":"high","custom_vendor_flag":true}`
	request := &transformerModel.InternalLLMRequest{}
	raw, err := PrepareSemanticGroupParamOverride(request, &override, outbound.OutboundTypeOpenAIResponse, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.ReasoningEffort != "high" {
		t.Fatalf("legacy reasoning_effort was not upgraded: %#v", request)
	}
	if raw == nil || *raw != `{"custom_vendor_flag":true}` {
		t.Fatalf("legacy upgrade did not preserve raw vendor field: %v", raw)
	}
	if !HasSemanticGroupParamOverride(&override) {
		t.Fatal("legacy semantic key should disable unsafe raw passthrough")
	}
}

func TestSemanticNamespaceNeverLeaksToWireMerge(t *testing.T) {
	override := `{"$octopus":{"reasoning_effort":"high"},"service_tier":"priority"}`
	body, err := ApplyJSONParamOverrides([]byte(`{"model":"m"}`), nil, &override)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, leaked := decoded[semanticGroupOverrideKey]; leaked {
		t.Fatalf("semantic namespace leaked upstream: %s", body)
	}
	if decoded["service_tier"] != "priority" {
		t.Fatalf("raw override was not preserved: %s", body)
	}
}

func TestReapplySemanticGroupJSONPrecedence(t *testing.T) {
	override := `{"$octopus":{"temperature":0.7,"reasoning_effort":"high"}}`
	transformed := []byte(`{"temperature":0.7,"reasoning":{"effort":"high","summary":"auto"}}`)
	current := []byte(`{"temperature":0.2,"reasoning":{"effort":"low"},"vendor":true}`)
	got, err := ReapplySemanticGroupJSONPrecedence(current, transformed, &override, outbound.OutboundTypeOpenAIResponse, nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["temperature"] != 0.7 || decoded["vendor"] != true {
		t.Fatalf("semantic precedence or raw preservation failed: %s", got)
	}
	reasoning, ok := decoded["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("native reasoning object was not restored: %s", got)
	}
}

func TestSemanticParamForRejectedWireName(t *testing.T) {
	override := `{"$octopus":{"reasoning_effort":"high","max_output_tokens":2048}}`
	for wireName, want := range map[string]string{
		"reasoning_effort":                 SemanticParamReasoningEffort,
		"reasoning.effort":                 SemanticParamReasoningEffort,
		"generationConfig.thinkingConfig":  SemanticParamReasoningEffort,
		"max_completion_tokens":            SemanticParamMaxOutputTokens,
		"generationConfig.maxOutputTokens": SemanticParamMaxOutputTokens,
		"unrelated":                        "",
	} {
		if got := SemanticParamForRejectedWireName(&override, wireName); got != want {
			t.Errorf("SemanticParamForRejectedWireName(%q) = %q, want %q", wireName, got, want)
		}
	}
}

func TestRejectedUpstreamReasoningMax(t *testing.T) {
	maxOverride := `{"$octopus":{"reasoning_effort":"max"}}`
	highOverride := `{"$octopus":{"reasoning_effort":"high"}}`
	tests := []struct {
		name     string
		override *string
		body     string
		want     bool
	}{
		{
			name:     "model lists lower supported tiers",
			override: &maxOverride,
			body:     `{"error":{"message":"Unsupported value: 'max' is not supported with this model. Supported values are: 'low', 'medium', and 'high'."}}`,
			want:     true,
		},
		{
			name:     "field-specific invalid value",
			override: &maxOverride,
			body:     `{"detail":"Invalid value for reasoning_effort: max"}`,
			want:     true,
		},
		{
			name:     "unrelated max value",
			override: &maxOverride,
			body:     `{"detail":"Invalid value for service_tier: max"}`,
			want:     false,
		},
		{
			name:     "high override does not downgrade",
			override: &highOverride,
			body:     `{"detail":"Invalid value for reasoning_effort: max"}`,
			want:     false,
		},
		{name: "generic bad request", override: &maxOverride, body: `{"detail":"bad request"}`, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RejectedUpstreamReasoningMax(test.override, []byte(test.body)); got != test.want {
				t.Fatalf("RejectedUpstreamReasoningMax() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSemanticReasoningEffortUsesProviderNativeWireShape(t *testing.T) {
	override := `{"$octopus":{"reasoning_effort":"high"}}`
	message := "hello"
	tests := []struct {
		name         string
		outboundType outbound.OutboundType
		model        string
		path         []string
		want         any
	}{
		{name: "openai chat", outboundType: outbound.OutboundTypeOpenAIChat, model: "gpt-5", path: []string{"reasoning_effort"}, want: "high"},
		{name: "openai responses", outboundType: outbound.OutboundTypeOpenAIResponse, model: "gpt-5", path: []string{"reasoning", "effort"}, want: "high"},
		{name: "anthropic", outboundType: outbound.OutboundTypeAnthropic, model: "claude-sonnet-4", path: []string{"thinking", "type"}, want: "enabled"},
		{name: "gemini", outboundType: outbound.OutboundTypeGemini, model: "gemini-3-flash", path: []string{"generationConfig", "thinkingConfig", "thinkingLevel"}, want: "high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := false
			request := &transformerModel.InternalLLMRequest{
				Model:    test.model,
				Messages: []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &message}}},
				Stream:   &stream,
			}
			if _, err := PrepareSemanticGroupParamOverride(request, &override, test.outboundType, nil); err != nil {
				t.Fatal(err)
			}
			adapter := outbound.Get(test.outboundType)
			wireRequest, err := adapter.TransformRequest(context.Background(), request, "http://example.test/v1", "test-key")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(wireRequest.Body)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode provider body: %v (%s)", err, body)
			}
			value := decoded
			for _, segment := range test.path {
				object, ok := value.(map[string]any)
				if !ok {
					t.Fatalf("wire path %v missing in %s", test.path, body)
				}
				value = object[segment]
			}
			if value != test.want {
				t.Fatalf("wire path %v = %#v, want %#v; body=%s", test.path, value, test.want, body)
			}
		})
	}
}

func TestSemanticMaxReasoningEffortUsesHighestProviderTier(t *testing.T) {
	override := `{"$octopus":{"reasoning_effort":"max"}}`
	message := "hello"
	tests := []struct {
		name         string
		outboundType outbound.OutboundType
		model        string
		path         []string
		want         any
	}{
		{name: "openai chat preserves max", outboundType: outbound.OutboundTypeOpenAIChat, model: "gpt-5", path: []string{"reasoning_effort"}, want: "max"},
		{name: "openai responses preserves max", outboundType: outbound.OutboundTypeOpenAIResponse, model: "gpt-5", path: []string{"reasoning", "effort"}, want: "max"},
		{name: "anthropic uses maximum fixed budget", outboundType: outbound.OutboundTypeAnthropic, model: "claude-sonnet-4", path: []string{"thinking", "budget_tokens"}, want: float64(30000)},
		{name: "gemini coerces max to high", outboundType: outbound.OutboundTypeGemini, model: "gemini-3-flash", path: []string{"generationConfig", "thinkingConfig", "thinkingLevel"}, want: "high"},
		{name: "volcengine coerces max to high", outboundType: outbound.OutboundTypeVolcengine, model: "doubao-seed-1-6-251015", path: []string{"reasoning", "effort"}, want: "high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := false
			request := &transformerModel.InternalLLMRequest{
				Model:    test.model,
				Messages: []transformerModel.Message{{Role: "user", Content: transformerModel.MessageContent{Content: &message}}},
				Stream:   &stream,
			}
			if _, err := PrepareSemanticGroupParamOverride(request, &override, test.outboundType, nil); err != nil {
				t.Fatal(err)
			}
			wireRequest, err := outbound.Get(test.outboundType).TransformRequest(context.Background(), request, "http://example.test/v1", "test-key")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(wireRequest.Body)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode provider body: %v (%s)", err, body)
			}
			value := decoded
			for _, segment := range test.path {
				object, ok := value.(map[string]any)
				if !ok {
					t.Fatalf("wire path %v missing in %s", test.path, body)
				}
				value = object[segment]
			}
			if value != test.want {
				t.Fatalf("wire path %v = %#v, want %#v; body=%s", test.path, value, test.want, body)
			}
		})
	}
}
