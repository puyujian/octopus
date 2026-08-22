package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/looplj/axonhub/llm"
	axonAnthropic "github.com/looplj/axonhub/llm/transformer/anthropic"
)

// ToLLMRequest converts the local IR into AxonHub's unified request model.
// The two models intentionally do not have identical field sets, so keep this
// conversion explicit rather than relying on JSON round-tripping.
func ToLLMRequest(r *InternalLLMRequest) (*llm.Request, error) {
	if r == nil {
		return nil, fmt.Errorf("internal request is nil")
	}

	out := &llm.Request{
		Model:               r.Model,
		FrequencyPenalty:    r.FrequencyPenalty,
		Logprobs:            r.Logprobs,
		MaxCompletionTokens: r.MaxCompletionTokens,
		MaxTokens:           r.MaxTokens,
		PresencePenalty:     r.PresencePenalty,
		Seed:                r.Seed,
		Store:               r.Store,
		Temperature:         r.Temperature,
		TopLogprobs:         r.TopLogprobs,
		TopP:                r.TopP,
		PromptCacheKey:      cloneStringPtr(r.PromptCacheKey),
		SafetyIdentifier:    r.SafetyIdentifier,
		User:                r.User,
		Verbosity:           r.Verbosity,
		ReasoningSummary:    cloneStringPtr(r.ReasoningSummary),
		LogitBias:           cloneInt64Map(r.LogitBias),
		Metadata:            cloneStringMap(r.Metadata),
		Modalities:          append([]string(nil), r.Modalities...),
		ReasoningBudget:     r.ReasoningBudget,
		ReasoningEffort:     r.ReasoningEffort,
		ServiceTier:         r.ServiceTier,
		ParallelToolCalls:   r.ParallelToolCalls,
		PreviousResponseID:  cloneStringPtr(r.PreviousResponseID),
		Stream:              r.Stream,
		ExtraBody:           cloneRawMessage(r.ExtraBody),
		TransformOptions:    ToLLMTransformOptions(r.TransformOptions),
		TransformerMetadata: toAnyMap(r.TransformerMetadata),
		APIFormat:           llm.APIFormat(r.RawAPIFormat),
		RequestType:         requestTypeFor(r),
	}
	if r.RawAPIFormat == APIFormatOpenAIResponse && r.ResponsesPromptCacheKey != nil {
		out.PromptCacheKey = cloneStringPtr(r.ResponsesPromptCacheKey)
	}
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	if userID := r.TransformerMetadataValue(TransformerMetadataAnthropicUserID); userID != "" {
		// AxonHub's Anthropic converter reads user_id from the structured
		// metadata map.  The local inbound adapter keeps it in the namespaced
		// transformer metadata to avoid leaking it to other providers.
		if out.Metadata["user_id"] == "" {
			out.Metadata["user_id"] = userID
		}
	}
	if cacheControl := r.GetAnthropicExtensions().CacheControl; cacheControl != nil {
		// AxonHub's Anthropic converter expects the top-level cache control in
		// its namespaced transformer metadata. Keep the local provider extension
		// as the source of truth so same-protocol and cross-protocol paths agree.
		out.TransformerMetadata[axonAnthropic.TransformerMetadataKeyCacheControl] = ToAxonAnthropicCacheControl(cacheControl)
	}
	if r.AdaptiveThinking {
		out.TransformerMetadata["thinking_type"] = "adaptive"
	} else if r.ReasoningEffort == "none" {
		out.TransformerMetadata["thinking_type"] = "disabled"
	}
	if r.ThinkingDisplay != "" {
		out.TransformerMetadata["thinking_display"] = r.ThinkingDisplay
	}
	if r.ReasoningEffort != "" && r.AdaptiveThinking {
		out.TransformerMetadata["output_config_effort"] = r.ReasoningEffort
	}
	if len(r.ResponsesStreamOptions) > 0 {
		var options map[string]any
		if json.Unmarshal(r.ResponsesStreamOptions, &options) == nil {
			if value, ok := options["include_obfuscation"]; ok {
				switch typed := value.(type) {
				case bool:
					out.TransformerMetadata["include_obfuscation"] = typed
				case *bool:
					out.TransformerMetadata["include_obfuscation"] = typed
				}
			}
		}
	}

	// AxonHub reads Responses-only options from TransformerMetadata.  Keep the
	// values in native types so its xmap helpers can consume them.
	if r.Include != nil {
		out.TransformerMetadata["include"] = append([]string(nil), r.Include...)
	}
	if r.Truncation != nil {
		out.TransformerMetadata["truncation"] = cloneStringPtr(r.Truncation)
	}
	if r.MaxToolCalls != nil {
		out.TransformerMetadata["max_tool_calls"] = cloneInt64Ptr(r.MaxToolCalls)
	}
	if r.PromptCacheRetention != nil {
		out.TransformerMetadata["prompt_cache_retention"] = cloneStringPtr(r.PromptCacheRetention)
	}
	if r.ResponsesPromptCacheKey != nil {
		out.TransformerMetadata["prompt_cache_key"] = cloneStringPtr(r.ResponsesPromptCacheKey)
	}
	if r.Background != nil {
		out.TransformerMetadata["background"] = cloneBoolPtr(r.Background)
	}
	if len(r.Prompt) > 0 {
		out.TransformerMetadata["prompt"] = cloneRawMessage(r.Prompt)
	}
	if len(r.Conversation) > 0 {
		out.TransformerMetadata["conversation"] = cloneRawMessage(r.Conversation)
	}
	if len(r.ContextManagement) > 0 {
		out.TransformerMetadata["context_management"] = cloneRawMessage(r.ContextManagement)
	}
	if len(r.ResponsesStreamOptions) > 0 {
		out.TransformerMetadata["responses_stream_options"] = cloneRawMessage(r.ResponsesStreamOptions)
	}
	if r.ReasoningSummary != nil {
		out.TransformerMetadata["reasoning_summary"] = cloneStringPtr(r.ReasoningSummary)
	}
	if r.ReasoningGenerateSummary != nil {
		out.TransformerMetadata["reasoning_generate_summary"] = cloneStringPtr(r.ReasoningGenerateSummary)
	}

	responsesOptions := r.GetOpenAIResponsesOptions()
	responsesRequestExt := &llm.OpenAIResponsesRequestExtensions{
		ReasoningContext: responsesOptions.ReasoningContext,
		RawTools:         toLLMOpenAIResponsesRawFragments(responsesOptions.RawTools),
		ToolSignatures:   append([]string(nil), responsesOptions.ToolSignatures...),
		RawToolChoice:    cloneRawMessage(responsesOptions.RawToolChoice),
		RawInputItems:    toLLMOpenAIResponsesRawFragments(responsesOptions.RawInputFragments),
	}
	if r.IsOpenAIExactReplayRequest() || (len(responsesRequestExt.RawInputItems) == 0 && len(r.Messages) == 0 && len(r.RawInputItems) > 0) {
		responsesRequestExt.RawInputItems = toLLMRawFragments(r.RawInputItems)
	}
	if responsesRequestExt.ReasoningContext != "" || len(responsesRequestExt.RawTools) > 0 || len(responsesRequestExt.ToolSignatures) > 0 || len(responsesRequestExt.RawToolChoice) > 0 || len(responsesRequestExt.RawInputItems) > 0 {
		ext := llm.EnsureOpenAIResponsesProviderExtensions(out)
		ext.Request = responsesRequestExt
	}

	if r.StreamOptions != nil {
		out.StreamOptions = &llm.StreamOptions{IncludeUsage: r.StreamOptions.IncludeUsage}
	}
	if r.Stop != nil {
		out.Stop = &llm.Stop{Stop: cloneStringPtr(r.Stop.Stop), MultipleStop: append([]string(nil), r.Stop.MultipleStop...)}
	}
	if r.ToolChoice != nil {
		out.ToolChoice = &llm.ToolChoice{ToolChoice: cloneStringPtr(r.ToolChoice.ToolChoice)}
		if named := r.ToolChoice.NamedToolChoice; named != nil {
			axonNamed := llm.NamedToolChoice{Type: named.Type}
			if named.Function != nil {
				axonNamed.Function = llm.ToolFunction{Name: named.Function.Name}
			}
			out.ToolChoice.NamedToolChoice = &axonNamed
		}
	}
	if r.ResponseFormat != nil {
		out.ResponseFormat = &llm.ResponseFormat{
			Type:       r.ResponseFormat.Type,
			JSONSchema: responseFormatSchema(r.ResponseFormat),
		}
	}

	out.Messages = make([]llm.Message, 0, len(r.Messages))
	if r.IsOpenAIExactReplayRequest() || (len(r.Messages) == 0 && len(responsesRequestExt.RawInputItems) > 0) {
		// Exact replay is deliberately raw-only. Ordinary Responses requests
		// retain the structured projection and give AxonHub only raw-only holes.
		out.Messages = nil
	} else {
		for i := range r.Messages {
			msg, err := ToLLMMessage(&r.Messages[i])
			if err != nil {
				return nil, err
			}
			if msg != nil {
				out.Messages = append(out.Messages, *msg)
			}
		}
	}

	if r.EmbeddingInput != nil {
		out.Embedding = &llm.EmbeddingRequest{
			EncodingFormat: valueOrEmpty(r.EmbeddingEncodingFormat),
			User:           valueOrEmpty(r.User),
		}
		if r.EmbeddingDimensions != nil {
			dimensions := int(*r.EmbeddingDimensions)
			out.Embedding.Dimensions = &dimensions
		}
		switch {
		case r.EmbeddingInput.Single != nil:
			out.Embedding.Input.String = *r.EmbeddingInput.Single
		case len(r.EmbeddingInput.Multiple) > 0:
			out.Embedding.Input.StringArray = append([]string(nil), r.EmbeddingInput.Multiple...)
		}
		out.Messages = nil
	}

	if r.Tools != nil {
		out.Tools = make([]llm.Tool, 0, len(r.Tools))
		for i := range r.Tools {
			tool, err := ToLLMTool(&r.Tools[i])
			if err != nil {
				return nil, err
			}
			if tool != nil {
				out.Tools = append(out.Tools, *tool)
			}
		}
	}

	return out, nil
}

// FromLLMRequest converts an AxonHub request back into the local IR. It is
// used by adapter tests and by code which needs to inspect an Axon request.
func FromLLMRequest(r *llm.Request) *InternalLLMRequest {
	if r == nil {
		return nil
	}

	out := &InternalLLMRequest{
		Model:               r.Model,
		FrequencyPenalty:    r.FrequencyPenalty,
		Logprobs:            r.Logprobs,
		MaxCompletionTokens: r.MaxCompletionTokens,
		MaxTokens:           r.MaxTokens,
		PresencePenalty:     r.PresencePenalty,
		Seed:                r.Seed,
		Store:               r.Store,
		Temperature:         r.Temperature,
		TopLogprobs:         r.TopLogprobs,
		TopP:                r.TopP,
		PromptCacheKey:      cloneStringPtr(r.PromptCacheKey),
		SafetyIdentifier:    cloneStringPtr(r.SafetyIdentifier),
		User:                cloneStringPtr(r.User),
		Verbosity:           r.Verbosity,
		LogitBias:           cloneInt64Map(r.LogitBias),
		Metadata:            cloneStringMap(r.Metadata),
		Modalities:          append([]string(nil), r.Modalities...),
		ReasoningBudget:     cloneInt64Ptr(r.ReasoningBudget),
		ReasoningEffort:     r.ReasoningEffort,
		ReasoningSummary:    cloneStringPtr(r.ReasoningSummary),
		ServiceTier:         cloneStringPtr(r.ServiceTier),
		ParallelToolCalls:   r.ParallelToolCalls,
		PreviousResponseID:  cloneStringPtr(r.PreviousResponseID),
		Stream:              r.Stream,
		ExtraBody:           cloneRawMessage(r.ExtraBody),
		RawAPIFormat:        APIFormat(r.APIFormat),
		TransformerMetadata: fromAnyMap(r.TransformerMetadata),
		TransformOptions:    FromLLMTransformOptions(r.TransformOptions),
	}

	if r.StreamOptions != nil {
		out.StreamOptions = &StreamOptions{IncludeUsage: r.StreamOptions.IncludeUsage}
	}
	if r.Stop != nil {
		out.Stop = &Stop{Stop: cloneStringPtr(r.Stop.Stop), MultipleStop: append([]string(nil), r.Stop.MultipleStop...)}
	}
	if r.ToolChoice != nil {
		out.ToolChoice = &ToolChoice{ToolChoice: cloneStringPtr(r.ToolChoice.ToolChoice)}
		if named := r.ToolChoice.NamedToolChoice; named != nil {
			localNamed := &NamedToolChoice{Type: named.Type}
			if named.Function.Name != "" {
				localNamed.Function = &ToolFunction{Name: named.Function.Name}
				name := named.Function.Name
				localNamed.Name = &name
			}
			out.ToolChoice.NamedToolChoice = localNamed
		}
	}
	if r.ResponseFormat != nil {
		out.ResponseFormat = responseFormatFromLLM(r.ResponseFormat)
	}
	for i := range r.Messages {
		if msg := FromLLMMessage(&r.Messages[i]); msg != nil {
			out.Messages = append(out.Messages, *msg)
		}
	}
	for i := range r.Tools {
		if tool := FromLLMTool(&r.Tools[i]); tool != nil {
			out.Tools = append(out.Tools, *tool)
		}
	}
	if r.Embedding != nil {
		out.EmbeddingInput = &EmbeddingInput{}
		switch {
		case r.Embedding.Input.String != "":
			value := r.Embedding.Input.String
			out.EmbeddingInput.Single = &value
		case len(r.Embedding.Input.StringArray) > 0:
			out.EmbeddingInput.Multiple = append([]string(nil), r.Embedding.Input.StringArray...)
		}
		if r.Embedding.Dimensions != nil {
			dimensions := int64(*r.Embedding.Dimensions)
			out.EmbeddingDimensions = &dimensions
		}
		if r.Embedding.EncodingFormat != "" {
			format := r.Embedding.EncodingFormat
			out.EmbeddingEncodingFormat = &format
		}
	}

	// Keep the typed AxonHub metadata map available while extracting values.
	// The local compatibility mirror is string-only, so reading the typed map
	// back from out.TransformerMetadata would make provider objects impossible
	// to recover (and is rejected by the compiler in any case).
	if values := r.TransformerMetadata; values != nil {
		out.Include = anyStringSlice(values["include"])
		out.Truncation = anyStringPtr(values["truncation"])
		out.MaxToolCalls = anyInt64Ptr(values["max_tool_calls"])
		out.PromptCacheRetention = anyStringPtr(values["prompt_cache_retention"])
		out.ResponsesPromptCacheKey = anyStringPtr(values["prompt_cache_key"])
		out.Background = anyBoolPtr(values["background"])
		out.Prompt = anyRawMessage(values["prompt"])
		out.Conversation = anyRawMessage(values["conversation"])
		out.ContextManagement = anyRawMessage(values["context_management"])
		out.ResponsesStreamOptions = anyRawMessage(values["responses_stream_options"])
		out.ReasoningSummary = anyStringPtr(values["reasoning_summary"])
		out.ReasoningGenerateSummary = anyStringPtr(values["reasoning_generate_summary"])
		if cacheControl, ok := values[axonAnthropic.TransformerMetadataKeyCacheControl].(*axonAnthropic.CacheControl); ok {
			out.SetAnthropicExtensions(AnthropicExtension{
				CacheControl: FromAxonAnthropicCacheControl(cacheControl),
			})
		}
	}

	if r.ProviderExtensions != nil && r.ProviderExtensions.OpenAIResponses != nil && r.ProviderExtensions.OpenAIResponses.Request != nil {
		ext := r.ProviderExtensions.OpenAIResponses.Request
		options := OpenAIResponsesOptions{
			ReasoningContext:  ext.ReasoningContext,
			RawTools:          fromLLMOpenAIResponsesRawFragments(ext.RawTools),
			ToolSignatures:    append([]string(nil), ext.ToolSignatures...),
			RawToolChoice:     cloneRawMessage(ext.RawToolChoice),
			RawInputFragments: fromLLMOpenAIResponsesRawFragments(ext.RawInputItems),
		}
		out.SetOpenAIResponsesOptions(options)
		if len(out.Messages) == 0 {
			if rawItems := fromLLMRawFragments(ext.RawInputItems); len(rawItems) > 0 {
				out.SetOpenAIRawInputItems(rawItems)
			}
		}
	}
	return out
}

func ToAxonAnthropicCacheControl(c *CacheControl) *axonAnthropic.CacheControl {
	if c == nil {
		return nil
	}
	return &axonAnthropic.CacheControl{Type: c.Type, TTL: c.TTL}
}

func FromAxonAnthropicCacheControl(c *axonAnthropic.CacheControl) *CacheControl {
	if c == nil {
		return nil
	}
	return &CacheControl{Type: c.Type, TTL: c.TTL}
}

func requestTypeFor(r *InternalLLMRequest) llm.RequestType {
	if r != nil && r.IsEmbeddingRequest() {
		return llm.RequestTypeEmbedding
	}
	return llm.RequestTypeChat
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func responseFormatSchema(r *ResponseFormat) json.RawMessage {
	if r == nil {
		return nil
	}
	schema := cloneRawMessage(r.RawSchema)
	if len(schema) == 0 && r.Schema != nil {
		if encoded, err := json.Marshal(r.Schema); err == nil {
			schema = encoded
		}
	}
	if len(schema) == 0 {
		schema = cloneRawMessage(r.JSONSchema)
	}
	if len(schema) == 0 || (r.Name == "" && r.Description == "" && r.Strict == nil) {
		return schema
	}
	wrapper := map[string]any{"schema": json.RawMessage(schema)}
	if r.Name != "" {
		wrapper["name"] = r.Name
	}
	if r.Description != "" {
		wrapper["description"] = r.Description
	}
	if r.Strict != nil {
		wrapper["strict"] = *r.Strict
	}
	encoded, err := json.Marshal(wrapper)
	if err != nil {
		return schema
	}
	return encoded
}

func responseFormatFromLLM(r *llm.ResponseFormat) *ResponseFormat {
	if r == nil {
		return nil
	}
	out := &ResponseFormat{Type: r.Type, JSONSchema: cloneRawMessage(r.JSONSchema)}
	if len(r.JSONSchema) == 0 {
		return out
	}
	var wrapper struct {
		Name        string          `json:"name,omitempty"`
		Description string          `json:"description,omitempty"`
		Strict      *bool           `json:"strict,omitempty"`
		Schema      json.RawMessage `json:"schema,omitempty"`
	}
	if json.Unmarshal(r.JSONSchema, &wrapper) == nil && len(wrapper.Schema) > 0 {
		out.Name = wrapper.Name
		out.Description = wrapper.Description
		out.Strict = wrapper.Strict
		out.RawSchema = cloneRawMessage(wrapper.Schema)
		if parsed, err := ParseSchema(wrapper.Schema); err == nil {
			out.Schema = parsed
		}
	} else {
		out.RawSchema = cloneRawMessage(r.JSONSchema)
		if parsed, err := ParseSchema(r.JSONSchema); err == nil {
			out.Schema = parsed
		}
	}
	return out
}

// AxonHub's raw fragments intentionally have json:"-" fields. Never marshal
// the fragment structs themselves: doing so produces [{}] and loses the raw
// Responses input/tool item.
func toLLMRawFragments(raw json.RawMessage) []llm.OpenAIResponsesRawFragment {
	items, ok := parseRawJSONArray(raw)
	if !ok {
		return nil
	}
	fragments := make([]llm.OpenAIResponsesRawFragment, 0, len(items))
	for index, item := range items {
		var probe struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(item, &probe)
		fragments = append(fragments, llm.OpenAIResponsesRawFragment{
			Type:          probe.Type,
			Name:          probe.Name,
			CallID:        probe.CallID,
			OriginalIndex: index,
			Raw:           cloneRawMessage(item),
		})
	}
	return fragments
}

func toLLMOpenAIResponsesRawFragments(fragments []OpenAIResponsesRawFragment) []llm.OpenAIResponsesRawFragment {
	if len(fragments) == 0 {
		return nil
	}
	out := make([]llm.OpenAIResponsesRawFragment, len(fragments))
	for i := range fragments {
		out[i] = llm.OpenAIResponsesRawFragment{
			Type:                 fragments[i].Type,
			Name:                 fragments[i].Name,
			CallID:               fragments[i].CallID,
			OriginalIndex:        fragments[i].OriginalIndex,
			RepresentedToolCount: fragments[i].RepresentedToolCount,
			Raw:                  cloneRawMessage(fragments[i].Raw),
		}
	}
	return out
}

func fromLLMOpenAIResponsesRawFragments(fragments []llm.OpenAIResponsesRawFragment) []OpenAIResponsesRawFragment {
	if len(fragments) == 0 {
		return nil
	}
	out := make([]OpenAIResponsesRawFragment, len(fragments))
	for i := range fragments {
		out[i] = OpenAIResponsesRawFragment{
			Type:                 fragments[i].Type,
			Name:                 fragments[i].Name,
			CallID:               fragments[i].CallID,
			OriginalIndex:        fragments[i].OriginalIndex,
			RepresentedToolCount: fragments[i].RepresentedToolCount,
			Raw:                  cloneRawMessage(fragments[i].Raw),
		}
	}
	return out
}

func fromLLMRawFragments(fragments []llm.OpenAIResponsesRawFragment) json.RawMessage {
	if len(fragments) == 0 {
		return nil
	}
	maxIndex := -1
	for _, fragment := range fragments {
		if fragment.OriginalIndex > maxIndex {
			maxIndex = fragment.OriginalIndex
		}
	}
	if maxIndex < 0 {
		return nil
	}
	items := make([]json.RawMessage, maxIndex+1)
	for _, fragment := range fragments {
		if fragment.OriginalIndex < 0 || fragment.OriginalIndex >= len(items) || len(fragment.Raw) == 0 {
			return nil
		}
		items[fragment.OriginalIndex] = cloneRawMessage(fragment.Raw)
	}
	for _, item := range items {
		if len(item) == 0 {
			return nil
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return encoded
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// toAnyMap/fromAnyMap bridge the local string-only metadata map and AxonHub's
// typed metadata map. JSON values are encoded compactly when crossing into
// the local representation so raw options do not become "map[...]" strings.
func toAnyMap(in map[string]string) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func fromAnyMap(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		switch typed := value.(type) {
		case string:
			out[key] = typed
		case json.RawMessage:
			out[key] = string(typed)
		default:
			encoded, err := json.Marshal(typed)
			if err == nil {
				out[key] = string(encoded)
			}
		}
	}
	return out
}

func anyRawMessage(value any) json.RawMessage {
	switch typed := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return cloneRawMessage(typed)
	case string:
		if json.Valid([]byte(typed)) {
			return json.RawMessage(typed)
		}
	default:
		encoded, err := json.Marshal(typed)
		if err == nil {
			return encoded
		}
	}
	return nil
}

func anyStringPtr(value any) *string {
	switch typed := value.(type) {
	case *string:
		return cloneStringPtr(typed)
	case string:
		return &typed
	case json.RawMessage:
		var decoded string
		if json.Unmarshal(typed, &decoded) == nil {
			return &decoded
		}
	}
	return nil
}

func anyBoolPtr(value any) *bool {
	switch typed := value.(type) {
	case *bool:
		return cloneBoolPtr(typed)
	case bool:
		return &typed
	case json.RawMessage:
		var decoded bool
		if json.Unmarshal(typed, &decoded) == nil {
			return &decoded
		}
	}
	return nil
}

func anyInt64Ptr(value any) *int64 {
	switch typed := value.(type) {
	case *int64:
		return cloneInt64Ptr(typed)
	case int64:
		return &typed
	case int:
		converted := int64(typed)
		return &converted
	case float64:
		converted := int64(typed)
		return &converted
	case json.RawMessage:
		var decoded int64
		if json.Unmarshal(typed, &decoded) == nil {
			return &decoded
		}
	case string:
		if decoded, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
			return &decoded
		}
	}
	return nil
}

func anyStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if stringValue, ok := item.(string); ok {
				out = append(out, stringValue)
			}
		}
		return out
	case json.RawMessage:
		var decoded []string
		if json.Unmarshal(typed, &decoded) == nil {
			return decoded
		}
	}
	return nil
}

func ToLLMTransformOptions(o TransformOptions) llm.TransformOptions {
	return llm.TransformOptions{ArrayInputs: cloneBoolPtr(o.ArrayInputs)}
}

func FromLLMTransformOptions(o llm.TransformOptions) TransformOptions {
	return TransformOptions{ArrayInputs: cloneBoolPtr(o.ArrayInputs)}
}
