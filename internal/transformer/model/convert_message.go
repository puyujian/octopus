package model

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/llm"
	axonShared "github.com/looplj/axonhub/llm/transformer/shared"
)

const legacyGeminiThoughtSignatureMetadataKey = "gemini_thought_signature"

func ToLLMMessage(m *Message) (*llm.Message, error) {
	if m == nil {
		return nil, nil
	}
	out := &llm.Message{
		ID:                 m.ID,
		Role:               m.Role,
		Name:               m.Name,
		Refusal:            m.Refusal,
		MessageIndex:       m.MessageIndex,
		ToolCallID:         m.ToolCallID,
		ToolCallName:       m.ToolCallName,
		ToolCallIsError:    m.ToolCallIsError,
		ReasoningContent:   m.ReasoningContent,
		Reasoning:          m.Reasoning,
		ReasoningSignature: m.ReasoningSignature,
		Attribution:        m.Attribution,
		CacheControl:       ToLLMCacheControl(m.CacheControl),
	}
	if len(m.Annotations) > 0 {
		out.Annotations = ToLLMAnnotations(m.Annotations)
	}
	if m.Audio != nil {
		out.Audio = &llm.OutputAudio{ID: m.Audio.ID, Data: m.Audio.Data, ExpiresAt: m.Audio.ExpiresAt, Transcript: m.Audio.Transcript}
	}
	if len(m.InlineToolResults) > 0 {
		out.InlineToolResults = make([]llm.InlineToolResult, 0, len(m.InlineToolResults))
		for _, result := range m.InlineToolResults {
			out.InlineToolResults = append(out.InlineToolResults, llm.InlineToolResult{
				ToolCallID:          result.ToolCallID,
				Output:              result.Output,
				IsError:             result.IsError,
				TransformerMetadata: cloneAnyMap(result.TransformerMetadata),
			})
		}
	}
	if m.Content.Content != nil {
		out.Content.Content = m.Content.Content
	}
	for i := range m.Content.MultipleContent {
		part, err := ToLLMContentPart(&m.Content.MultipleContent[i])
		if err != nil {
			return nil, err
		}
		out.Content.MultipleContent = append(out.Content.MultipleContent, *part)
	}
	for i := range m.ToolCalls {
		tc, err := ToLLMToolCall(&m.ToolCalls[i])
		if err != nil {
			return nil, err
		}
		out.ToolCalls = append(out.ToolCalls, *tc)
	}
	if len(m.ReasoningItems) > 0 {
		for _, item := range m.ReasoningItems {
			out.ReasoningItems = append(out.ReasoningItems, llm.ReasoningItem{ID: item.ID, Content: item.Content, Signature: item.Signature})
		}
	} else if len(m.ReasoningBlocks) > 0 {
		ToLLMReasoningBlocks(m, out)
	} else if len(m.RedactedThinkingBlocks) > 0 {
		// AxonHub currently has one opaque redacted-reasoning field. Preserve
		// the first block rather than dropping the entire signal when callers
		// populated the legacy compatibility slice without ReasoningBlocks.
		for _, block := range m.RedactedThinkingBlocks {
			if block == "" {
				continue
			}
			content := block
			out.RedactedReasoningContent = &content
			break
		}
	} else if m.ReasoningContent != nil || m.ReasoningSignature != nil {
		// AxonHub still uses the legacy scalar fields for some provider stream
		// events. Preserve a scalar-only reasoning/signature chunk as an item
		// instead of silently dropping its opaque signature.
		item := llm.ReasoningItem{}
		if m.ReasoningContent != nil {
			item.Content = *m.ReasoningContent
		}
		if m.ReasoningSignature != nil {
			item.Signature = *m.ReasoningSignature
		}
		if item.Content != "" || item.Signature != "" {
			out.ReasoningItems = append(out.ReasoningItems, item)
		}
	}
	return out, nil
}

func ToLLMReasoningBlocks(m *Message, out *llm.Message) {
	if len(m.ReasoningBlocks) == 0 {
		return
	}
	var redacted []string
	for i := range m.ReasoningBlocks {
		block := m.ReasoningBlocks[i]
		switch block.Kind {
		case ReasoningBlockKindThinking, ReasoningBlockKindSignature:
			out.ReasoningItems = append(out.ReasoningItems, llm.ReasoningItem{
				ID:        block.ID,
				Content:   block.Text,
				Signature: block.Signature,
			})
		case ReasoningBlockKindRedacted:
			if block.Data != "" {
				redacted = append(redacted, block.Data)
			}
		}
	}
	if len(redacted) > 0 {
		content := redacted[0]
		out.RedactedReasoningContent = &content
	}
}

func FromLLMMessage(m *llm.Message) *Message {
	if m == nil {
		return nil
	}
	out := &Message{
		ID:                 m.ID,
		Role:               m.Role,
		Name:               m.Name,
		Refusal:            m.Refusal,
		MessageIndex:       m.MessageIndex,
		ToolCallID:         m.ToolCallID,
		ToolCallName:       m.ToolCallName,
		ToolCallIsError:    m.ToolCallIsError,
		ReasoningContent:   m.ReasoningContent,
		Reasoning:          m.Reasoning,
		ReasoningSignature: m.ReasoningSignature,
		Attribution:        m.Attribution,
		CacheControl:       FromLLMCacheControl(m.CacheControl),
	}
	if len(m.Annotations) > 0 {
		out.Annotations = FromLLMAnnotations(m.Annotations)
	}
	if m.Audio != nil {
		out.Audio = &struct {
			Data       string `json:"data,omitempty"`
			ExpiresAt  int64  `json:"expires_at,omitempty"`
			ID         string `json:"id,omitempty"`
			Transcript string `json:"transcript,omitempty"`
		}{ID: m.Audio.ID, Data: m.Audio.Data, ExpiresAt: m.Audio.ExpiresAt, Transcript: m.Audio.Transcript}
	}
	if len(m.InlineToolResults) > 0 {
		out.InlineToolResults = make([]InlineToolResult, 0, len(m.InlineToolResults))
		for _, result := range m.InlineToolResults {
			out.InlineToolResults = append(out.InlineToolResults, InlineToolResult{
				ToolCallID:          result.ToolCallID,
				Output:              result.Output,
				IsError:             result.IsError,
				TransformerMetadata: cloneAnyMap(result.TransformerMetadata),
			})
		}
	}
	if m.Content.Content != nil {
		out.Content.Content = m.Content.Content
	}
	for i := range m.Content.MultipleContent {
		out.Content.MultipleContent = append(out.Content.MultipleContent, *FromLLMContentPart(&m.Content.MultipleContent[i]))
	}
	for i := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, *FromLLMToolCall(&m.ToolCalls[i]))
	}
	FromLLMReasoningItems(m, out)
	return out
}

func FromLLMReasoningItems(m *llm.Message, out *Message) {
	if m == nil || out == nil {
		return
	}
	for i := range m.ReasoningItems {
		item := m.ReasoningItems[i]
		out.ReasoningItems = append(out.ReasoningItems, ReasoningItem{ID: item.ID, Content: item.Content, Signature: item.Signature})
		if item.Signature != "" {
			out.ReasoningBlocks = append(out.ReasoningBlocks, ReasoningBlock{ID: item.ID, Kind: ReasoningBlockKindThinking, Text: item.Content, Signature: item.Signature})
		} else if item.Content != "" || item.ID != "" {
			out.ReasoningBlocks = append(out.ReasoningBlocks, ReasoningBlock{ID: item.ID, Kind: ReasoningBlockKindThinking, Text: item.Content})
		}
	}
	// Some AxonHub stream paths expose only the legacy scalar fields. Preserve
	// those fields as a reasoning block, including a signature-only chunk.
	if len(m.ReasoningItems) == 0 && (m.ReasoningContent != nil || m.ReasoningSignature != nil) {
		block := ReasoningBlock{Kind: ReasoningBlockKindThinking}
		if m.ReasoningContent != nil {
			block.Text = *m.ReasoningContent
		}
		if m.ReasoningSignature != nil {
			block.Signature = *m.ReasoningSignature
		}
		if block.Text != "" || block.Signature != "" {
			if block.Text == "" {
				block.Kind = ReasoningBlockKindSignature
			}
			out.ReasoningBlocks = append(out.ReasoningBlocks, block)
		}
	}
	if m.RedactedReasoningContent != nil && *m.RedactedReasoningContent != "" {
		out.RedactedThinkingBlocks = append(out.RedactedThinkingBlocks, *m.RedactedReasoningContent)
		out.ReasoningBlocks = append(out.ReasoningBlocks, ReasoningBlock{Kind: ReasoningBlockKindRedacted, Data: *m.RedactedReasoningContent})
	}
}

func ToLLMContentPart(p *MessageContentPart) (*llm.MessageContentPart, error) {
	if p == nil {
		return nil, nil
	}
	out := &llm.MessageContentPart{
		ID:                  p.ID,
		Type:                p.Type,
		Text:                p.Text,
		CacheControl:        ToLLMCacheControl(p.CacheControl),
		TransformerMetadata: cloneAnyMap(p.TransformerMetadata),
	}
	if p.ImageURL != nil {
		out.ImageURL = &llm.ImageURL{URL: p.ImageURL.URL, MIMEType: p.ImageURL.MIMEType, Detail: p.ImageURL.Detail}
	}
	if p.VideoURL != nil {
		out.VideoURL = &llm.VideoURL{URL: p.VideoURL.URL}
	}
	if p.Audio != nil {
		out.InputAudio = &llm.InputAudio{Format: p.Audio.Format, Data: p.Audio.Data}
	}
	if p.File != nil {
		switch {
		case p.File.FileID != "":
			out.Document = &llm.DocumentURL{URL: p.File.FileID, MIMEType: "application/octet-stream"}
		case p.File.FileURL != "":
			out.Document = &llm.DocumentURL{URL: p.File.FileURL, MIMEType: "application/octet-stream"}
		default:
			out.Document = &llm.DocumentURL{URL: p.File.FileData, MIMEType: "application/octet-stream"}
		}
		if out.TransformerMetadata == nil {
			out.TransformerMetadata = map[string]any{}
		}
		if p.File.Filename != "" {
			out.TransformerMetadata["octopus_file_name"] = p.File.Filename
		}
		if p.File.FileID != "" {
			out.TransformerMetadata["octopus_file_id"] = p.File.FileID
		}
		if p.File.FileURL != "" {
			out.TransformerMetadata["octopus_file_url"] = p.File.FileURL
		}
	}
	if p.Document != nil {
		if out.TransformerMetadata == nil {
			out.TransformerMetadata = make(map[string]any)
		}
		out.Document = documentToLLMURL(p.Document, out.TransformerMetadata)
	}
	if p.Compact != nil {
		out.Compact = &llm.CompactContent{ID: p.Compact.ID, EncryptedContent: p.Compact.EncryptedContent, CreatedBy: p.Compact.CreatedBy}
	}
	if p.ServerToolUse != nil {
		meta := toServerToolMetadata(p.Type, nil, p.ServerToolUse.Input)
		meta["anthropic_name"] = p.ServerToolUse.Name
		if p.ServerToolUse.ID != "" {
			meta["anthropic_id"] = p.ServerToolUse.ID
		}
		out.TransformerMetadata = mergeAnyMaps(out.TransformerMetadata, meta)
	}
	if p.ServerToolResult != nil {
		meta := toServerToolResultMetadata(p.ServerToolResult.BlockType, p.ServerToolResult.Content)
		if p.ServerToolResult.ToolUseID != "" {
			meta["anthropic_tool_use_id"] = p.ServerToolResult.ToolUseID
		}
		if p.ServerToolResult.IsError != nil {
			meta["anthropic_is_error"] = *p.ServerToolResult.IsError
		}
		out.TransformerMetadata = mergeAnyMaps(out.TransformerMetadata, meta)
	}
	if p.ProviderExtensions != nil && p.ProviderExtensions.Anthropic != nil {
		meta := out.TransformerMetadata
		if meta == nil {
			meta = map[string]any{}
		}
		if p.ProviderExtensions.Anthropic.ServerTool != nil {
			meta["anthropic_server_tool"] = p.ProviderExtensions.Anthropic.ServerTool
		}
		if p.ProviderExtensions.Anthropic.Container != nil {
			meta["anthropic_container"] = p.ProviderExtensions.Anthropic.Container
		}
		if len(p.ProviderExtensions.Anthropic.Beta) > 0 {
			meta["anthropic_beta"] = p.ProviderExtensions.Anthropic.Beta
		}
		out.TransformerMetadata = meta
	}
	return out, nil
}

func FromLLMContentPart(p *llm.MessageContentPart) *MessageContentPart {
	if p == nil {
		return nil
	}
	out := &MessageContentPart{
		ID:                  p.ID,
		Type:                p.Type,
		Text:                p.Text,
		CacheControl:        FromLLMCacheControl(p.CacheControl),
		TransformerMetadata: cloneAnyMap(p.TransformerMetadata),
	}
	if p.ImageURL != nil {
		out.ImageURL = &ImageURL{URL: p.ImageURL.URL, MIMEType: p.ImageURL.MIMEType, Detail: p.ImageURL.Detail}
	}
	if p.VideoURL != nil {
		out.VideoURL = &VideoURL{URL: p.VideoURL.URL}
	}
	if p.InputAudio != nil {
		out.Audio = &Audio{Format: p.InputAudio.Format, Data: p.InputAudio.Data}
	}
	if p.Document != nil && p.Document.URL != "" {
		out.Document = documentFromLLMURL(p.Document, p.TransformerMetadata)
	}
	if p.Compact != nil {
		out.Compact = &CompactContent{ID: p.Compact.ID, EncryptedContent: p.Compact.EncryptedContent, CreatedBy: p.Compact.CreatedBy}
	}
	if len(p.TransformerMetadata) > 0 {
		if t, ok := p.TransformerMetadata["anthropic_type"].(string); ok {
			if isServerToolResultType(t) {
				result := &ServerToolResultBlock{BlockType: t, Content: rawFromMeta(p.TransformerMetadata, "anthropic_tool_result_content")}
				if id, ok := p.TransformerMetadata["anthropic_tool_use_id"].(string); ok {
					result.ToolUseID = id
				}
				if isErr, ok := p.TransformerMetadata["anthropic_is_error"].(bool); ok {
					result.IsError = &isErr
				}
				out.ServerToolResult = result
			} else {
				name := t
				if n, ok := p.TransformerMetadata["anthropic_name"].(string); ok && n != "" {
					name = n
				}
				out.ServerToolUse = &ServerToolUseBlock{Name: name, Input: rawFromMeta(p.TransformerMetadata, "anthropic_input")}
				if out.ServerToolUse.Input == nil {
					out.ServerToolUse.Input = rawFromMeta(p.TransformerMetadata, "anthropic_caller")
				}
				if id, ok := p.TransformerMetadata["anthropic_id"].(string); ok {
					out.ServerToolUse.ID = id
				}
			}
		}
		out.ProviderExtensions = providerExtensionsFromMetadata(p.TransformerMetadata)
	}
	return out
}

func isServerToolResultType(t string) bool {
	return t != "" && (t == "server_tool_result" || strings.HasSuffix(t, "_tool_result") || strings.HasSuffix(t, "tool_result"))
}

func rawFromMeta(meta map[string]any, key string) json.RawMessage {
	if meta == nil {
		return nil
	}
	switch v := meta[key].(type) {
	case json.RawMessage:
		return v
	case []byte:
		return json.RawMessage(v)
	case string:
		return json.RawMessage(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return raw
	}
}

func toServerToolMetadata(blockType string, caller json.RawMessage, raw json.RawMessage) map[string]any {
	meta := map[string]any{}
	if blockType != "" {
		meta["anthropic_type"] = blockType
	}
	if len(caller) > 0 {
		meta["anthropic_caller"] = caller
	}
	if len(raw) > 0 {
		meta["anthropic_input"] = cloneRawMessage(raw)
	}
	return meta
}

func toServerToolResultMetadata(blockType string, raw json.RawMessage) map[string]any {
	meta := map[string]any{}
	if blockType != "" {
		meta["anthropic_type"] = blockType
	}
	if len(raw) > 0 {
		meta["anthropic_tool_result_content"] = raw
	}
	return meta
}

func ToLLMToolCall(tc *ToolCall) (*llm.ToolCall, error) {
	if tc == nil {
		return nil, nil
	}
	out := &llm.ToolCall{
		ID:           tc.ID,
		Type:         tc.Type,
		Index:        tc.Index,
		CacheControl: ToLLMCacheControl(tc.CacheControl),
		Function: llm.FunctionCall{
			Name:      tc.Function.Name,
			Namespace: tc.Function.Namespace,
			Arguments: tc.Function.Arguments,
		},
	}
	if tc.ResponseCustomToolCall != nil {
		out.ResponseCustomToolCall = &llm.ResponseCustomToolCall{
			CallID: tc.ResponseCustomToolCall.CallID,
			Name:   tc.ResponseCustomToolCall.Name,
			Input:  tc.ResponseCustomToolCall.Input,
		}
	}
	geminiExtensions := tc.GetGeminiExtensions()
	if geminiExtensions.ThoughtSignature != "" {
		out.TransformerMetadata = map[string]any{
			axonShared.TransformerMetadataKeyGoogleThoughtSignature: geminiExtensions.ThoughtSignature,
		}
	}
	if tc.ProviderExtensions != nil && tc.ProviderExtensions.Anthropic != nil {
		meta := out.TransformerMetadata
		if meta == nil {
			meta = map[string]any{}
		}
		if tc.ProviderExtensions.Anthropic.ServerTool != nil {
			meta["anthropic_server_tool"] = tc.ProviderExtensions.Anthropic.ServerTool
		}
		out.TransformerMetadata = meta
	}
	return out, nil
}

func FromLLMToolCall(tc *llm.ToolCall) *ToolCall {
	if tc == nil {
		return nil
	}
	out := &ToolCall{
		ID:           tc.ID,
		Type:         tc.Type,
		Index:        tc.Index,
		CacheControl: FromLLMCacheControl(tc.CacheControl),
		Function: FunctionCall{
			Name:      tc.Function.Name,
			Namespace: tc.Function.Namespace,
			Arguments: tc.Function.Arguments,
		},
	}
	if tc.ResponseCustomToolCall != nil {
		out.ResponseCustomToolCall = &ResponseCustomToolCall{
			CallID: tc.ResponseCustomToolCall.CallID,
			Name:   tc.ResponseCustomToolCall.Name,
			Input:  tc.ResponseCustomToolCall.Input,
		}
	}
	if len(tc.TransformerMetadata) > 0 {
		out.ThoughtSignature = geminiThoughtSignatureFromMetadata(tc.TransformerMetadata)
	}
	if len(tc.TransformerMetadata) > 0 {
		out.ProviderExtensions = providerExtensionsFromMetadata(tc.TransformerMetadata)
	}
	return out
}

func ToLLMTool(t *Tool) (*llm.Tool, error) {
	if t == nil {
		return nil, nil
	}
	out := &llm.Tool{
		Type:         t.Type,
		CacheControl: ToLLMCacheControl(t.CacheControl),
		Function: llm.Function{
			Name:                 t.Function.Name,
			Description:          t.Function.Description,
			Parameters:           t.Function.Parameters,
			ParametersJsonSchema: cloneRawMessage(t.Function.ParametersJsonSchema),
			Strict:               t.Function.Strict,
		},
	}
	if t.ImageGeneration != nil {
		out.ImageGeneration = &llm.ImageGeneration{
			Model:             t.ImageGeneration.Model,
			Background:        t.ImageGeneration.Background,
			InputFidelity:     t.ImageGeneration.InputFidelity,
			InputImageMask:    t.ImageGeneration.InputImageMask,
			Moderation:        t.ImageGeneration.Moderation,
			OutputCompression: t.ImageGeneration.OutputCompression,
			OutputFormat:      t.ImageGeneration.OutputFormat,
			PartialImages:     t.ImageGeneration.PartialImages,
			N:                 t.ImageGeneration.N,
			ResponseFormat:    t.ImageGeneration.ResponseFormat,
			Quality:           t.ImageGeneration.Quality,
			Size:              t.ImageGeneration.Size,
			Style:             t.ImageGeneration.Style,
			Watermark:         t.ImageGeneration.Watermark,
		}
	}
	if t.WebSearch != nil {
		out.WebSearch = &llm.WebSearch{
			MaxUses:        t.WebSearch.MaxUses,
			Strict:         t.WebSearch.Strict,
			AllowedDomains: append([]string(nil), t.WebSearch.AllowedDomains...),
			BlockedDomains: append([]string(nil), t.WebSearch.BlockedDomains...),
			UserLocation: llm.WebSearchToolUserLocation{
				Type: t.WebSearch.UserLocation.Type, City: t.WebSearch.UserLocation.City,
				Country: t.WebSearch.UserLocation.Country, Region: t.WebSearch.UserLocation.Region,
				Timezone: t.WebSearch.UserLocation.Timezone,
			},
		}
	}
	if t.Google != nil {
		out.Google = &llm.GoogleTools{}
		if t.Google.Search != nil {
			out.Google.Search = &llm.GoogleSearch{}
		}
		if t.Google.CodeExecution != nil {
			out.Google.CodeExecution = &llm.GoogleCodeExecution{}
		}
		if t.Google.UrlContext != nil {
			out.Google.UrlContext = &llm.GoogleUrlContext{}
		}
	}
	if t.ResponseCustomTool != nil {
		out.ResponseCustomTool = &llm.ResponseCustomTool{Name: t.ResponseCustomTool.Name, Description: t.ResponseCustomTool.Description}
		if t.ResponseCustomTool.Format != nil {
			out.ResponseCustomTool.Format = &llm.ResponseCustomToolFormat{Type: t.ResponseCustomTool.Format.Type, Syntax: t.ResponseCustomTool.Format.Syntax, Definition: t.ResponseCustomTool.Format.Definition}
		}
	}
	return out, nil
}

func FromLLMTool(t *llm.Tool) *Tool {
	if t == nil {
		return nil
	}
	out := &Tool{
		Type:         t.Type,
		CacheControl: FromLLMCacheControl(t.CacheControl),
		Function: Function{
			Name:                 t.Function.Name,
			Description:          t.Function.Description,
			Parameters:           t.Function.Parameters,
			ParametersJsonSchema: cloneRawMessage(t.Function.ParametersJsonSchema),
			Strict:               t.Function.Strict,
		},
	}
	if t.ImageGeneration != nil {
		out.ImageGeneration = &ImageGeneration{
			Model:             t.ImageGeneration.Model,
			Background:        t.ImageGeneration.Background,
			InputFidelity:     t.ImageGeneration.InputFidelity,
			InputImageMask:    t.ImageGeneration.InputImageMask,
			Moderation:        t.ImageGeneration.Moderation,
			OutputCompression: t.ImageGeneration.OutputCompression,
			OutputFormat:      t.ImageGeneration.OutputFormat,
			PartialImages:     t.ImageGeneration.PartialImages,
			N:                 t.ImageGeneration.N,
			ResponseFormat:    t.ImageGeneration.ResponseFormat,
			Quality:           t.ImageGeneration.Quality,
			Size:              t.ImageGeneration.Size,
			Style:             t.ImageGeneration.Style,
			Watermark:         t.ImageGeneration.Watermark,
		}
	}
	if t.WebSearch != nil {
		out.WebSearch = &WebSearch{
			MaxUses:        t.WebSearch.MaxUses,
			Strict:         t.WebSearch.Strict,
			AllowedDomains: append([]string(nil), t.WebSearch.AllowedDomains...),
			BlockedDomains: append([]string(nil), t.WebSearch.BlockedDomains...),
			UserLocation: WebSearchToolUserLocation{
				Type: t.WebSearch.UserLocation.Type, City: t.WebSearch.UserLocation.City,
				Country: t.WebSearch.UserLocation.Country, Region: t.WebSearch.UserLocation.Region,
				Timezone: t.WebSearch.UserLocation.Timezone,
			},
		}
	}
	if t.Google != nil {
		out.Google = &GoogleTools{}
		if t.Google.Search != nil {
			out.Google.Search = &GoogleSearch{}
		}
		if t.Google.CodeExecution != nil {
			out.Google.CodeExecution = &GoogleCodeExecution{}
		}
		if t.Google.UrlContext != nil {
			out.Google.UrlContext = &GoogleUrlContext{}
		}
	}
	if t.ResponseCustomTool != nil {
		out.ResponseCustomTool = &ResponseCustomTool{Name: t.ResponseCustomTool.Name, Description: t.ResponseCustomTool.Description}
		if t.ResponseCustomTool.Format != nil {
			out.ResponseCustomTool.Format = &ResponseCustomToolFormat{Type: t.ResponseCustomTool.Format.Type, Syntax: t.ResponseCustomTool.Format.Syntax, Definition: t.ResponseCustomTool.Format.Definition}
		}
	}
	return out
}

func ToLLMCacheControl(c *CacheControl) *llm.CacheControl {
	if c == nil {
		return nil
	}
	return &llm.CacheControl{Type: c.Type, TTL: c.TTL}
}

func FromLLMCacheControl(c *llm.CacheControl) *CacheControl {
	if c == nil {
		return nil
	}
	return &CacheControl{Type: c.Type, TTL: c.TTL}
}

func cloneAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, value := range src {
		switch v := value.(type) {
		case json.RawMessage:
			out[k] = cloneRawMessage(v)
		case []byte:
			out[k] = append([]byte(nil), v...)
		case map[string]any:
			out[k] = cloneAnyMap(v)
		case []any:
			copyValues := make([]any, len(v))
			for i := range v {
				if nested, ok := v[i].(map[string]any); ok {
					copyValues[i] = cloneAnyMap(nested)
				} else {
					copyValues[i] = v[i]
				}
			}
			out[k] = copyValues
		default:
			out[k] = value
		}
	}
	return out
}

func mergeAnyMaps(first, second map[string]any) map[string]any {
	if len(first) == 0 && len(second) == 0 {
		return nil
	}
	out := cloneAnyMap(first)
	if out == nil {
		out = make(map[string]any, len(second))
	}
	for k, v := range second {
		out[k] = v
	}
	return out
}

func ToLLMAnnotations(in []Annotation) []llm.Annotation {
	if in == nil {
		return nil
	}
	out := make([]llm.Annotation, 0, len(in))
	for _, item := range in {
		annotation := llm.Annotation{Type: item.Type}
		if item.StartIndex != nil {
			v := *item.StartIndex
			annotation.StartIndex = &v
		}
		if item.EndIndex != nil {
			v := *item.EndIndex
			annotation.EndIndex = &v
		}
		if item.URLCitation != nil {
			annotation.URLCitation = &llm.URLCitation{URL: item.URLCitation.URL, Title: item.URLCitation.Title}
		}
		out = append(out, annotation)
	}
	return out
}

func FromLLMAnnotations(in []llm.Annotation) []Annotation {
	if in == nil {
		return nil
	}
	out := make([]Annotation, 0, len(in))
	for _, item := range in {
		annotation := Annotation{Type: item.Type}
		if item.StartIndex != nil {
			v := *item.StartIndex
			annotation.StartIndex = &v
		}
		if item.EndIndex != nil {
			v := *item.EndIndex
			annotation.EndIndex = &v
		}
		if item.URLCitation != nil {
			annotation.URLCitation = &URLCitation{URL: item.URLCitation.URL, Title: item.URLCitation.Title}
		}
		out = append(out, annotation)
	}
	return out
}

func documentToLLMURL(doc *DocumentSource, metadata map[string]any) *llm.DocumentURL {
	if doc == nil {
		return nil
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["octopus_document_type"] = doc.Type
	metadata["octopus_document_media_type"] = doc.MediaType
	metadata["octopus_document_title"] = doc.Title
	metadata["octopus_document_context"] = doc.Context
	if doc.Citations != nil {
		metadata["octopus_document_citations"] = doc.Citations.Enabled
	}
	if len(doc.Content) > 0 {
		metadata["octopus_document_content"] = cloneRawMessage(doc.Content)
	}

	mediaType := doc.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	switch strings.ToLower(doc.Type) {
	case "url":
		return &llm.DocumentURL{URL: doc.URL, MIMEType: mediaType}
	case "base64":
		return &llm.DocumentURL{URL: "data:" + mediaType + ";base64," + doc.Data, MIMEType: mediaType}
	case "text":
		return &llm.DocumentURL{URL: "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(doc.Text)), MIMEType: "text/plain"}
	case "content":
		return &llm.DocumentURL{URL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(doc.Content), MIMEType: mediaType}
	default:
		if doc.URL != "" {
			return &llm.DocumentURL{URL: doc.URL, MIMEType: mediaType}
		}
		if doc.Data != "" {
			return &llm.DocumentURL{URL: "data:" + mediaType + ";base64," + doc.Data, MIMEType: mediaType}
		}
		return &llm.DocumentURL{URL: "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(doc.Text)), MIMEType: "text/plain"}
	}
}

func documentFromLLMURL(doc *llm.DocumentURL, metadata map[string]any) *DocumentSource {
	if doc == nil {
		return nil
	}
	result := &DocumentSource{Type: metaString(metadata, "octopus_document_type"), MediaType: doc.MIMEType, URL: doc.URL}
	result.Title = metaString(metadata, "octopus_document_title")
	result.Context = metaString(metadata, "octopus_document_context")
	if enabled, ok := metadata["octopus_document_citations"].(bool); ok {
		result.Citations = &DocumentCitations{Enabled: enabled}
	}
	if raw := rawFromMeta(metadata, "octopus_document_content"); len(raw) > 0 {
		result.Content = cloneRawMessage(raw)
	}
	if strings.HasPrefix(doc.URL, "data:") {
		data := strings.TrimPrefix(doc.URL, "data:")
		comma := strings.IndexByte(data, ',')
		if comma >= 0 {
			header, payload := data[:comma], data[comma+1:]
			if strings.HasSuffix(header, ";base64") {
				if decoded, err := base64.StdEncoding.DecodeString(payload); err == nil {
					if result.Type == "text" {
						result.Text = string(decoded)
					} else if result.Type == "content" {
						result.Content = decoded
					} else {
						result.Data = payload
					}
				}
			}
		}
	}
	if result.Type == "" {
		result.Type = "url"
	}
	return result
}

func metaString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if value, ok := metadata[key].(string); ok {
		return value
	}
	return ""
}

func providerExtensionsFromMetadata(metadata map[string]any) *ProviderExtensions {
	if len(metadata) == 0 {
		return nil
	}
	ext := &ProviderExtensions{}
	if value := rawFromMeta(metadata, "anthropic_server_tool"); len(value) > 0 || len(rawFromMeta(metadata, "anthropic_container")) > 0 || len(rawFromMeta(metadata, "anthropic_beta")) > 0 {
		ext.Anthropic = &AnthropicExtension{ServerTool: value, Container: rawFromMeta(metadata, "anthropic_container")}
		if beta, ok := metadata["anthropic_beta"].([]string); ok {
			ext.Anthropic.Beta = append([]string(nil), beta...)
		}
	}
	if sig := geminiThoughtSignatureFromMetadata(metadata); sig != "" {
		ext.Gemini = &GeminiExtension{ThoughtSignature: sig}
	}
	if ext.Anthropic == nil && ext.Gemini == nil {
		return nil
	}
	return ext
}

func geminiThoughtSignatureFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{
		axonShared.TransformerMetadataKeyGoogleThoughtSignature,
		legacyGeminiThoughtSignatureMetadataKey,
	} {
		if sig, ok := metadata[key].(string); ok && strings.TrimSpace(sig) != "" {
			return sig
		}
	}
	return ""
}

// ensureConvertTypes prevents the compiler from optimizing away type checks.
func ensureConvertTypes() {
	_ = fmt.Sprintf("%T", llm.DoneResponse)
}
