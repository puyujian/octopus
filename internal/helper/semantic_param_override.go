package helper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

type semanticWireRule struct {
	paths     [][]string
	conflicts [][]string
}

const (
	SemanticParamTemperature     = "temperature"
	SemanticParamTopP            = "top_p"
	SemanticParamMaxOutputTokens = "max_output_tokens"
	SemanticParamReasoningEffort = "reasoning_effort"
)

var supportedSemanticGroupParams = map[string]struct{}{
	SemanticParamTemperature:     {},
	SemanticParamTopP:            {},
	SemanticParamMaxOutputTokens: {},
	SemanticParamReasoningEffort: {},
}

var unsupportedParameterPattern = regexp.MustCompile(`(?i)(?:unsupported|unknown|unrecognized)\s+parameter(?:s)?\s*(?::|=|is)?\s*[\x60'\"]?([a-zA-Z0-9_.-]+)`)

var rejectedReasoningMaxValuePattern = regexp.MustCompile(`(?i)(?:unsupported|invalid)\s+(?:parameter\s+)?value|(?:value\s+)?[\x60'\"]?max[\x60'\"]?\s+is\s+not\s+supported`)

// HasSemanticGroupParamOverride reports whether the group contains parameters
// that must pass through the normalized transformer pipeline. Callers use this
// to avoid raw passthrough, which cannot translate cross-provider semantics.
func HasSemanticGroupParamOverride(paramOverride *string) bool {
	semantic, _, err := splitGroupParamOverride(paramOverride)
	return err == nil && len(semantic) > 0
}

// PrepareSemanticGroupParamOverride applies Octopus semantic parameters to a
// normalized request and returns the remaining provider-native override. The
// caller must transform request after this function and merge the returned raw
// override last.
func PrepareSemanticGroupParamOverride(
	request *transformerModel.InternalLLMRequest,
	paramOverride *string,
	outboundType outbound.OutboundType,
	excluded map[string]struct{},
) (*string, error) {
	semantic, rawOverride, err := splitGroupParamOverride(paramOverride)
	if err != nil {
		return nil, err
	}
	if len(semantic) == 0 {
		return rawOverride, nil
	}
	if request == nil {
		return nil, fmt.Errorf("cannot apply semantic param_override to a nil request")
	}

	for key, raw := range semantic {
		if _, skip := excluded[key]; skip {
			continue
		}
		switch key {
		case SemanticParamTemperature:
			value, err := semanticFloat(raw, key, 0, 2)
			if err != nil {
				return nil, err
			}
			request.Temperature = &value
		case SemanticParamTopP:
			value, err := semanticFloat(raw, key, 0, 1)
			if err != nil {
				return nil, err
			}
			request.TopP = &value
		case SemanticParamMaxOutputTokens:
			value, err := semanticPositiveInt(raw, key)
			if err != nil {
				return nil, err
			}
			switch outboundType {
			case outbound.OutboundTypeAnthropic, outbound.OutboundTypeGemini:
				request.MaxTokens = &value
				request.MaxCompletionTokens = nil
			default:
				request.MaxCompletionTokens = &value
				request.MaxTokens = nil
			}
		case SemanticParamReasoningEffort:
			value, err := semanticReasoningEffort(raw)
			if err != nil {
				return nil, err
			}
			// Provider-native effort sets differ. Preserve max for OpenAI while
			// coercing protocols without a max keyword to their highest tier.
			switch outboundType {
			case outbound.OutboundTypeAnthropic:
				// The broadly compatible Anthropic path uses a fixed thinking budget.
				// Its effective maximum is represented by the high tier.
				if value == "minimal" {
					value = "low"
				} else if value == "max" {
					value = "high"
				}
			case outbound.OutboundTypeGemini, outbound.OutboundTypeVolcengine:
				if value == "max" {
					value = "high"
				}
			}
			request.ReasoningEffort = value
		default:
			return nil, fmt.Errorf("unsupported semantic param_override %q", key)
		}
	}
	return rawOverride, nil
}

// SemanticParamForRejectedWireName maps a provider error's parameter name
// back to a semantic override. This enables one safe retry without the single
// unsupported forced parameter while preserving the remaining group rules.
func SemanticParamForRejectedWireName(paramOverride *string, wireName string) string {
	semantic, _, err := splitGroupParamOverride(paramOverride)
	if err != nil || len(semantic) == 0 {
		return ""
	}
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(wireName), "`'\""))
	aliases := map[string][]string{
		SemanticParamTemperature:     {"temperature", "generationconfig.temperature", "generation_config.temperature"},
		SemanticParamTopP:            {"top_p", "topp", "generationconfig.topp", "generation_config.top_p"},
		SemanticParamMaxOutputTokens: {"max_tokens", "max_completion_tokens", "max_output_tokens", "generationconfig.maxoutputtokens", "generation_config.max_output_tokens"},
		SemanticParamReasoningEffort: {"reasoning_effort", "reasoning", "reasoning.effort", "thinking", "thinking.type", "thinking.budget_tokens", "generationconfig.thinkingconfig", "generation_config.thinking_config"},
	}
	for semanticKey, names := range aliases {
		if _, configured := semantic[semanticKey]; !configured {
			continue
		}
		for _, name := range names {
			if normalized == name || strings.HasSuffix(normalized, "."+name) {
				return semanticKey
			}
		}
	}
	return ""
}

// RejectedUpstreamParameter extracts only an explicit unsupported-parameter
// error. Generic 400 responses deliberately do not trigger compatibility
// fallback, so unrelated validation and authentication failures stay visible.
func RejectedUpstreamParameter(body []byte) string {
	message := upstreamErrorText(body)
	match := unsupportedParameterPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return ""
	}
	return strings.Trim(match[1], " .,:;\t\r\n\x60'\"")
}

// RejectedUpstreamReasoningMax reports a model-level rejection of the max
// reasoning tier. The check is deliberately narrow so unrelated 400 responses
// cannot trigger a hidden retry. Callers can safely retry the same semantic
// group rule at high, which is the widest commonly supported upper tier.
func RejectedUpstreamReasoningMax(paramOverride *string, body []byte) bool {
	semantic, _, err := splitGroupParamOverride(paramOverride)
	if err != nil {
		return false
	}
	raw, configured := semantic[SemanticParamReasoningEffort]
	if !configured {
		return false
	}
	effort, err := semanticReasoningEffort(raw)
	if err != nil || effort != "max" {
		return false
	}

	message := strings.ToLower(upstreamErrorText(body))
	if !strings.Contains(message, "max") || !rejectedReasoningMaxValuePattern.MatchString(message) {
		return false
	}
	mentionsEffort := strings.Contains(message, "reasoning") || strings.Contains(message, "effort")
	listsLowerTiers := strings.Contains(message, "supported value") && strings.Contains(message, "high") &&
		(strings.Contains(message, "medium") || strings.Contains(message, "low"))
	return mentionsEffort || listsLowerTiers
}

func upstreamErrorText(body []byte) string {
	message := strings.TrimSpace(string(body))
	var payload any
	if json.Unmarshal(body, &payload) == nil {
		if extracted := findUpstreamErrorText(payload); extracted != "" {
			message = extracted
		}
	}
	return message
}

func findUpstreamErrorText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"detail", "message", "error"} {
			if nested, ok := typed[key]; ok {
				if text := findUpstreamErrorText(nested); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

// ReapplySemanticGroupWirePrecedence restores provider-native values produced
// by the transformer after channel/raw overrides have been merged. This keeps
// the documented precedence (group > channel > client) without teaching the
// UI provider-specific JSON paths.
func ReapplySemanticGroupWirePrecedence(
	request *http.Request,
	transformedBody []byte,
	paramOverride *string,
	outboundType outbound.OutboundType,
	excluded map[string]struct{},
) error {
	if request == nil || request.Body == nil {
		return nil
	}
	currentBody, err := io.ReadAll(request.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}
	modified, err := ReapplySemanticGroupJSONPrecedence(currentBody, transformedBody, paramOverride, outboundType, excluded)
	if err != nil {
		setRequestBody(request, currentBody)
		return err
	}
	setRequestBody(request, modified)
	return nil
}

func ReapplySemanticGroupJSONPrecedence(
	currentBody, transformedBody []byte,
	paramOverride *string,
	outboundType outbound.OutboundType,
	excluded map[string]struct{},
) ([]byte, error) {
	semantic, _, err := splitGroupParamOverride(paramOverride)
	if err != nil || len(semantic) == 0 {
		return currentBody, err
	}
	var current, transformed map[string]json.RawMessage
	if json.Unmarshal(currentBody, &current) != nil || current == nil || json.Unmarshal(transformedBody, &transformed) != nil || transformed == nil {
		return currentBody, nil
	}
	for key := range semantic {
		if _, skip := excluded[key]; skip {
			continue
		}
		rule, ok := semanticWireRuleFor(outboundType, key)
		if !ok {
			continue
		}
		for _, path := range rule.paths {
			if value, exists := rawMessageAtPath(transformed, path); exists {
				if err := setRawMessageAtPath(current, path, value); err != nil {
					return currentBody, err
				}
			} else {
				deleteRawMessageAtPath(current, path)
			}
		}
		for _, path := range rule.conflicts {
			deleteRawMessageAtPath(current, path)
		}
	}
	modified, err := json.Marshal(current)
	if err != nil {
		return currentBody, fmt.Errorf("failed to encode semantic param_override precedence: %w", err)
	}
	return modified, nil
}

func semanticWireRuleFor(outboundType outbound.OutboundType, key string) (semanticWireRule, bool) {
	switch outboundType {
	case outbound.OutboundTypeOpenAIChat:
		switch key {
		case SemanticParamTemperature:
			return semanticWireRule{paths: [][]string{{"temperature"}}}, true
		case SemanticParamTopP:
			return semanticWireRule{paths: [][]string{{"top_p"}}}, true
		case SemanticParamMaxOutputTokens:
			return semanticWireRule{paths: [][]string{{"max_completion_tokens"}}, conflicts: [][]string{{"max_tokens"}}}, true
		case SemanticParamReasoningEffort:
			return semanticWireRule{paths: [][]string{{"reasoning_effort"}}}, true
		}
	case outbound.OutboundTypeOpenAIResponse:
		switch key {
		case SemanticParamTemperature:
			return semanticWireRule{paths: [][]string{{"temperature"}}}, true
		case SemanticParamTopP:
			return semanticWireRule{paths: [][]string{{"top_p"}}}, true
		case SemanticParamMaxOutputTokens:
			return semanticWireRule{paths: [][]string{{"max_output_tokens"}}, conflicts: [][]string{{"max_tokens"}, {"max_completion_tokens"}}}, true
		case SemanticParamReasoningEffort:
			return semanticWireRule{paths: [][]string{{"reasoning"}}, conflicts: [][]string{{"reasoning_effort"}}}, true
		}
	case outbound.OutboundTypeAnthropic:
		switch key {
		case SemanticParamTemperature:
			return semanticWireRule{paths: [][]string{{"temperature"}}}, true
		case SemanticParamTopP:
			return semanticWireRule{paths: [][]string{{"top_p"}}}, true
		case SemanticParamMaxOutputTokens:
			return semanticWireRule{paths: [][]string{{"max_tokens"}}, conflicts: [][]string{{"max_completion_tokens"}, {"max_output_tokens"}}}, true
		case SemanticParamReasoningEffort:
			return semanticWireRule{
				paths:     [][]string{{"thinking"}, {"output_config"}, {"temperature"}},
				conflicts: [][]string{{"reasoning_effort"}, {"reasoning"}, {"top_p"}, {"top_k"}},
			}, true
		}
	case outbound.OutboundTypeGemini:
		switch key {
		case SemanticParamTemperature:
			return semanticWireRule{paths: [][]string{{"generationConfig", "temperature"}}}, true
		case SemanticParamTopP:
			return semanticWireRule{paths: [][]string{{"generationConfig", "topP"}}}, true
		case SemanticParamMaxOutputTokens:
			return semanticWireRule{paths: [][]string{{"generationConfig", "maxOutputTokens"}}, conflicts: [][]string{{"max_tokens"}, {"max_completion_tokens"}, {"max_output_tokens"}}}, true
		case SemanticParamReasoningEffort:
			return semanticWireRule{paths: [][]string{{"generationConfig", "thinkingConfig"}}, conflicts: [][]string{{"reasoning_effort"}, {"reasoning"}, {"thinking"}}}, true
		}
	case outbound.OutboundTypeVolcengine:
		switch key {
		case SemanticParamTemperature:
			return semanticWireRule{paths: [][]string{{"temperature"}}}, true
		case SemanticParamTopP:
			return semanticWireRule{paths: [][]string{{"top_p"}}}, true
		case SemanticParamMaxOutputTokens:
			return semanticWireRule{paths: [][]string{{"max_output_tokens"}}, conflicts: [][]string{{"max_tokens"}, {"max_completion_tokens"}}}, true
		case SemanticParamReasoningEffort:
			return semanticWireRule{paths: [][]string{{"thinking"}, {"reasoning"}}, conflicts: [][]string{{"reasoning_effort"}}}, true
		}
	}
	return semanticWireRule{}, false
}

func rawMessageAtPath(root map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	current := root
	for index, segment := range path {
		value, ok := current[segment]
		if !ok {
			return nil, false
		}
		if index == len(path)-1 {
			return append(json.RawMessage(nil), value...), true
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(value, &nested) != nil || nested == nil {
			return nil, false
		}
		current = nested
	}
	return nil, false
}

func setRawMessageAtPath(root map[string]json.RawMessage, path []string, value json.RawMessage) error {
	if len(path) == 0 {
		return nil
	}
	if len(path) == 1 {
		root[path[0]] = append(json.RawMessage(nil), value...)
		return nil
	}
	var nested map[string]json.RawMessage
	if existing, ok := root[path[0]]; !ok || json.Unmarshal(existing, &nested) != nil || nested == nil {
		nested = make(map[string]json.RawMessage)
	}
	if err := setRawMessageAtPath(nested, path[1:], value); err != nil {
		return err
	}
	encoded, err := json.Marshal(nested)
	if err != nil {
		return err
	}
	root[path[0]] = encoded
	return nil
}

func deleteRawMessageAtPath(root map[string]json.RawMessage, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(root, path[0])
		return
	}
	existing, ok := root[path[0]]
	if !ok {
		return
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(existing, &nested) != nil || nested == nil {
		return
	}
	deleteRawMessageAtPath(nested, path[1:])
	if len(nested) == 0 {
		delete(root, path[0])
		return
	}
	if encoded, err := json.Marshal(nested); err == nil {
		root[path[0]] = encoded
	}
}

func validateSemanticGroupOverride(raw json.RawMessage) error {
	var semantic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &semantic); err != nil || semantic == nil {
		return fmt.Errorf("param_override %s must be a JSON object", semanticGroupOverrideKey)
	}
	for key, value := range semantic {
		if _, ok := supportedSemanticGroupParams[key]; !ok {
			return fmt.Errorf("unsupported semantic param_override %q", key)
		}
		switch key {
		case SemanticParamTemperature:
			if _, err := semanticFloat(value, key, 0, 2); err != nil {
				return err
			}
		case SemanticParamTopP:
			if _, err := semanticFloat(value, key, 0, 1); err != nil {
				return err
			}
		case SemanticParamMaxOutputTokens:
			if _, err := semanticPositiveInt(value, key); err != nil {
				return err
			}
		case SemanticParamReasoningEffort:
			if _, err := semanticReasoningEffort(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitGroupParamOverride(paramOverride *string) (map[string]json.RawMessage, *string, error) {
	if paramOverride == nil || strings.TrimSpace(*paramOverride) == "" {
		return nil, nil, nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(*paramOverride)), &decoded); err != nil {
		return nil, nil, fmt.Errorf("invalid param_override: %w", err)
	}
	if decoded == nil {
		return nil, nil, fmt.Errorf("param_override must be a JSON object")
	}

	var semantic map[string]json.RawMessage
	if raw, ok := decoded[semanticGroupOverrideKey]; ok {
		if err := json.Unmarshal(raw, &semantic); err != nil || semantic == nil {
			return nil, nil, fmt.Errorf("param_override %s must be a JSON object", semanticGroupOverrideKey)
		}
		if err := validateSemanticGroupOverride(raw); err != nil {
			return nil, nil, err
		}
		delete(decoded, semanticGroupOverrideKey)
	}
	if semantic == nil {
		semantic = make(map[string]json.RawMessage)
	}

	// Upgrade historical group rows in memory. These were previously inserted
	// directly into the final wire payload, which is exactly how a Chat-only
	// name such as reasoning_effort leaked into Responses/Anthropic/Gemini.
	legacyTopLevel := []struct {
		rawKey      string
		semanticKey string
	}{
		{SemanticParamTemperature, SemanticParamTemperature},
		{SemanticParamTopP, SemanticParamTopP},
		{"max_completion_tokens", SemanticParamMaxOutputTokens},
		{"max_tokens", SemanticParamMaxOutputTokens},
		{"max_output_tokens", SemanticParamMaxOutputTokens},
		{SemanticParamReasoningEffort, SemanticParamReasoningEffort},
	}
	for _, legacy := range legacyTopLevel {
		raw, exists := decoded[legacy.rawKey]
		if !exists {
			continue
		}
		if _, alreadySet := semantic[legacy.semanticKey]; !alreadySet {
			semantic[legacy.semanticKey] = raw
		}
		delete(decoded, legacy.rawKey)
	}
	if reasoningRaw, exists := decoded["reasoning"]; exists {
		var reasoning map[string]json.RawMessage
		if json.Unmarshal(reasoningRaw, &reasoning) == nil && reasoning != nil {
			if effort, hasEffort := reasoning["effort"]; hasEffort {
				if _, alreadySet := semantic[SemanticParamReasoningEffort]; !alreadySet {
					semantic[SemanticParamReasoningEffort] = effort
				}
				delete(reasoning, "effort")
				if len(reasoning) == 0 {
					delete(decoded, "reasoning")
				} else if encoded, marshalErr := json.Marshal(reasoning); marshalErr == nil {
					decoded["reasoning"] = encoded
				}
			}
		}
	}
	if len(semantic) > 0 {
		encoded, err := json.Marshal(semantic)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to encode semantic param_override: %w", err)
		}
		if err := validateSemanticGroupOverride(encoded); err != nil {
			return nil, nil, err
		}
	}
	if len(decoded) == 0 {
		return semantic, nil, nil
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode raw param_override: %w", err)
	}
	value := string(raw)
	return semantic, &value, nil
}

func semanticFloat(raw json.RawMessage, key string, min, max float64) (float64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, fmt.Errorf("semantic param_override %q must be a number", key)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("semantic param_override %q must be a number", key)
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) || parsed < min || parsed > max {
		return 0, fmt.Errorf("semantic param_override %q must be between %g and %g", key, min, max)
	}
	return parsed, nil
}

func semanticPositiveInt(raw json.RawMessage, key string) (int64, error) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, fmt.Errorf("semantic param_override %q must be a positive integer", key)
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("semantic param_override %q must be a positive integer", key)
	}
	return value, nil
}

func semanticReasoningEffort(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("semantic param_override %q must be a string", SemanticParamReasoningEffort)
	}
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "minimal", "low", "medium", "high", "max":
		return value, nil
	default:
		return "", fmt.Errorf("semantic param_override %q must be minimal, low, medium, high, or max", SemanticParamReasoningEffort)
	}
}
