package model

import (
	"encoding/base64"
	"unicode"

	"github.com/looplj/axonhub/llm"
)

func FromLLMResponseError(err *llm.ResponseError) *ResponseError {
	if err == nil {
		return nil
	}
	return &ResponseError{
		StatusCode: err.StatusCode,
		Detail: ErrorDetail{
			Code:      err.Detail.Code,
			Message:   err.Detail.Message,
			Type:      err.Detail.Type,
			Param:     err.Detail.Param,
			RequestID: err.Detail.RequestID,
		},
	}
}

// toLLMResponse converts the internal unified response into the axonhub llm.Response.
func ToLLMResponse(r *InternalLLMResponse) (*llm.Response, error) {
	if r == nil {
		return nil, nil
	}
	out := &llm.Response{
		ID:                  r.ID,
		Object:              r.Object,
		Created:             r.Created,
		Model:               r.Model,
		PreviousResponseID:  cloneStringPtr(r.PreviousResponseID),
		SystemFingerprint:   r.SystemFingerprint,
		ServiceTier:         r.ServiceTier,
		RequestType:         llm.RequestType(r.RequestType),
		APIFormat:           llm.APIFormat(r.APIFormat),
		TransformerMetadata: cloneAnyMap(r.TransformerMetadata),
	}
	if r.Usage != nil {
		out.Usage = ToLLMUsage(r.Usage)
	}
	if r.Error != nil {
		out.Error = &llm.ResponseError{
			StatusCode: r.Error.StatusCode,
			Detail: llm.ErrorDetail{
				Code:      r.Error.Detail.Code,
				Type:      r.Error.Detail.Type,
				Message:   r.Error.Detail.Message,
				Param:     r.Error.Detail.Param,
				RequestID: r.Error.Detail.RequestID,
			},
		}
	}
	for i := range r.Choices {
		choice := &r.Choices[i]
		outChoice := llm.Choice{Index: choice.Index, FinishReason: choice.FinishReason, TransformerMetadata: cloneAnyMap(choice.TransformerMetadata)}
		if choice.StopSequence != nil {
			if outChoice.TransformerMetadata == nil {
				outChoice.TransformerMetadata = map[string]any{}
			}
			outChoice.TransformerMetadata["stop_sequence"] = cloneStringPtr(choice.StopSequence)
		}
		if choice.Message != nil {
			msg, err := ToLLMMessage(choice.Message)
			if err != nil {
				return nil, err
			}
			outChoice.Message = msg
		}
		if choice.Delta != nil {
			msg, err := ToLLMMessage(choice.Delta)
			if err != nil {
				return nil, err
			}
			outChoice.Delta = msg
		}
		if choice.Logprobs != nil {
			outChoice.Logprobs = &llm.LogprobsContent{
				Content: ToLLMTokenLogprob(choice.Logprobs.Content),
			}
		}
		out.Choices = append(out.Choices, outChoice)
	}
	if r.EmbeddingData != nil {
		out.Embedding = &llm.EmbeddingResponse{
			ID:     r.ID,
			Object: r.Object,
			Data:   make([]llm.EmbeddingData, 0, len(r.EmbeddingData)),
		}
		if out.Embedding.Object == "" {
			out.Embedding.Object = "list"
		}
		for i := range r.EmbeddingData {
			item := r.EmbeddingData[i]
			emb := llm.Embedding{}
			if item.Embedding.FloatArray != nil {
				emb.Embedding = append([]float64(nil), item.Embedding.FloatArray...)
			}
			if item.Embedding.Base64String != nil {
				emb.Base64 = *item.Embedding.Base64String
			}
			out.Embedding.Data = append(out.Embedding.Data, llm.EmbeddingData{
				Embedding: emb,
				Index:     item.Index,
				Object:    item.Object,
			})
		}
	}
	return out, nil
}

// fromLLMResponse converts an axonhub llm.Response back into the internal unified response.
func FromLLMResponse(r *llm.Response) *InternalLLMResponse {
	if r == nil {
		return nil
	}
	out := &InternalLLMResponse{
		ID:                  r.ID,
		PreviousResponseID:  cloneStringPtr(r.PreviousResponseID),
		Object:              r.Object,
		Created:             r.Created,
		Model:               r.Model,
		SystemFingerprint:   r.SystemFingerprint,
		ServiceTier:         r.ServiceTier,
		RequestType:         string(r.RequestType),
		APIFormat:           APIFormat(r.APIFormat),
		TransformerMetadata: cloneAnyMap(r.TransformerMetadata),
	}
	if r.Usage != nil {
		out.Usage = FromLLMUsage(r.Usage)
	}
	if r.Error != nil {
		out.Error = &ResponseError{
			StatusCode: r.Error.StatusCode,
			Detail: ErrorDetail{
				Code:      r.Error.Detail.Code,
				Type:      r.Error.Detail.Type,
				Message:   r.Error.Detail.Message,
				Param:     r.Error.Detail.Param,
				RequestID: r.Error.Detail.RequestID,
			},
		}
	}
	for i := range r.Choices {
		choice := r.Choices[i]
		outChoice := Choice{Index: choice.Index, FinishReason: choice.FinishReason, TransformerMetadata: cloneAnyMap(choice.TransformerMetadata)}
		if value, ok := choice.TransformerMetadata["stop_sequence"]; ok {
			outChoice.StopSequence = anyStringPtr(value)
		}
		if choice.Message != nil {
			outChoice.Message = FromLLMMessage(choice.Message)
		}
		if choice.Delta != nil {
			outChoice.Delta = FromLLMMessage(choice.Delta)
		}
		if choice.Logprobs != nil {
			outChoice.Logprobs = &LogprobsContent{
				Content: FromLLMTokenLogprob(choice.Logprobs.Content),
			}
		}
		out.Choices = append(out.Choices, outChoice)
	}
	if r.Embedding != nil {
		if out.Object == "" {
			out.Object = r.Embedding.Object
		}
		if out.ID == "" {
			out.ID = r.Embedding.ID
		}
		out.EmbeddingData = make([]EmbeddingObject, 0, len(r.Embedding.Data))
		for i := range r.Embedding.Data {
			item := r.Embedding.Data[i]
			embedding := Embedding{FloatArray: append([]float64(nil), item.Embedding.Embedding...)}
			if item.Embedding.Base64 != "" {
				value := item.Embedding.Base64
				embedding.Base64String = &value
			}
			out.EmbeddingData = append(out.EmbeddingData, EmbeddingObject{
				Embedding: embedding,
				Index:     item.Index,
				Object:    item.Object,
			})
		}
	}
	return out
}

// RestoreAnthropicSignatures reverses AxonHub's storage encoding for
// provider-specific Anthropic signatures. AxonHub encodes non-base64 values
// before storing them in the unified model. The wire protocol expects the
// original printable value, while real opaque binary signatures must remain
// base64 encoded.
func RestoreAnthropicSignatures(response *InternalLLMResponse) {
	if response == nil {
		return
	}
	for i := range response.Choices {
		restoreAnthropicMessageSignatures(response.Choices[i].Message)
		restoreAnthropicMessageSignatures(response.Choices[i].Delta)
	}
}

func restoreAnthropicMessageSignatures(message *Message) {
	if message == nil {
		return
	}
	message.ReasoningSignature = restoreAnthropicSignaturePtr(message.ReasoningSignature)
	for i := range message.ReasoningItems {
		message.ReasoningItems[i].Signature = restoreAnthropicSignature(message.ReasoningItems[i].Signature)
	}
	for i := range message.ReasoningBlocks {
		message.ReasoningBlocks[i].Signature = restoreAnthropicSignature(message.ReasoningBlocks[i].Signature)
	}
}

func restoreAnthropicSignaturePtr(value *string) *string {
	if value == nil {
		return nil
	}
	restored := restoreAnthropicSignature(*value)
	return &restored
}

func restoreAnthropicSignature(value string) string {
	if value == "" {
		return value
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 {
		return value
	}
	for _, b := range decoded {
		if b < 0x20 || b > 0x7e {
			if !unicode.IsSpace(rune(b)) {
				return value
			}
		}
	}
	return string(decoded)
}

// NormalizeResponseUsageForAPI restores provider-specific accounting after an
// AxonHub response has been converted. AxonHub's Anthropic transformer uses
// OpenAI's convention internally (PromptTokens includes cache reads/writes),
// while Octopus' IR intentionally keeps those buckets separate so the
// Anthropic inbound transformer can emit the original wire fields.
func NormalizeResponseUsageForAPI(response *InternalLLMResponse) {
	if response == nil || response.Usage == nil || response.APIFormat != APIFormatAnthropicMessage {
		return
	}
	usage := response.Usage
	cached := int64(0)
	if usage.PromptTokensDetails != nil {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	created := usage.CacheCreationInputTokens
	if created == 0 {
		created = usage.CacheCreation5mInputTokens + usage.CacheCreation1hInputTokens
	}
	if cached > 0 || created > 0 {
		usage.CacheReadInputTokens = cached
		usage.CacheCreationInputTokens = created
		usage.PromptTokens -= cached + created
		if usage.PromptTokens < 0 {
			usage.PromptTokens = 0
		}
	}
	usage.TotalTokens = usage.EffectiveInputTokens() + usage.CompletionTokens
}

func ToLLMUsage(u *Usage) *llm.Usage {
	if u == nil {
		return nil
	}
	out := &llm.Usage{
		PromptTokens:                   u.PromptTokens,
		CompletionTokens:               u.CompletionTokens,
		TotalTokens:                    u.TotalTokens,
		PromptModalityTokenDetails:     ToLLMModalityTokenDetails(u.PromptModalityTokenDetails),
		CompletionModalityTokenDetails: ToLLMModalityTokenDetails(u.CompletionModalityTokenDetails),
	}
	if u.PromptTokensDetails != nil || u.CacheCreationInputTokens > 0 || u.CacheCreation5mInputTokens > 0 || u.CacheCreation1hInputTokens > 0 {
		var cachedTokens int64
		var audioTokens, textTokens, imageTokens int64
		if u.PromptTokensDetails != nil {
			cachedTokens = u.PromptTokensDetails.CachedTokens
			audioTokens = u.PromptTokensDetails.AudioTokens
			textTokens = u.PromptTokensDetails.TextTokens
			imageTokens = u.PromptTokensDetails.ImageTokens
		}
		out.PromptTokensDetails = &llm.PromptTokensDetails{
			CachedTokens:           cachedTokens,
			AudioTokens:            audioTokens,
			TextTokens:             textTokens,
			ImageTokens:            imageTokens,
			WriteCachedTokens:      u.CacheCreationInputTokens,
			WriteCached5MinTokens:  u.CacheCreation5mInputTokens,
			WriteCached1HourTokens: u.CacheCreation1hInputTokens,
		}
	}
	if u.CompletionTokensDetails != nil {
		out.CompletionTokensDetails = &llm.CompletionTokensDetails{
			ReasoningTokens:          u.CompletionTokensDetails.ReasoningTokens,
			AudioTokens:              u.CompletionTokensDetails.AudioTokens,
			AcceptedPredictionTokens: u.CompletionTokensDetails.AcceptedPredictionTokens,
			RejectedPredictionTokens: u.CompletionTokensDetails.RejectedPredictionTokens,
		}
	}
	return out
}

func FromLLMUsage(u *llm.Usage) *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:                   u.PromptTokens,
		CompletionTokens:               u.CompletionTokens,
		TotalTokens:                    u.TotalTokens,
		ToolUsePromptTokens:            0,
		CacheReadInputTokens:           0,
		PromptModalityTokenDetails:     FromLLMModalityTokenDetails(u.PromptModalityTokenDetails),
		CompletionModalityTokenDetails: FromLLMModalityTokenDetails(u.CompletionModalityTokenDetails),
	}
	if u.PromptTokensDetails != nil {
		out.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens: u.PromptTokensDetails.CachedTokens,
			AudioTokens:  u.PromptTokensDetails.AudioTokens,
			TextTokens:   u.PromptTokensDetails.TextTokens,
			ImageTokens:  u.PromptTokensDetails.ImageTokens,
		}
		out.CacheCreationInputTokens = u.PromptTokensDetails.WriteCachedTokens
		out.CacheCreation5mInputTokens = u.PromptTokensDetails.WriteCached5MinTokens
		out.CacheCreation1hInputTokens = u.PromptTokensDetails.WriteCached1HourTokens
	}
	// Anthropic uses explicit cache counters in the unified usage model. AxonHub
	// also mirrors cache reads in PromptTokensDetails for OpenAI-style callers;
	// preserve both representations when present.
	if u.PromptTokensDetails != nil {
		out.CacheReadInputTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.CompletionTokensDetails = &CompletionTokensDetails{
			ReasoningTokens:          u.CompletionTokensDetails.ReasoningTokens,
			AudioTokens:              u.CompletionTokensDetails.AudioTokens,
			AcceptedPredictionTokens: u.CompletionTokensDetails.AcceptedPredictionTokens,
			RejectedPredictionTokens: u.CompletionTokensDetails.RejectedPredictionTokens,
		}
	}
	return out
}

func ToLLMModalityTokenDetails(in []ModalityTokenCount) []llm.ModalityTokenCount {
	if in == nil {
		return nil
	}
	out := make([]llm.ModalityTokenCount, 0, len(in))
	for i := range in {
		out = append(out, llm.ModalityTokenCount{Modality: in[i].Modality, TokenCount: in[i].TokenCount})
	}
	return out
}

func FromLLMModalityTokenDetails(in []llm.ModalityTokenCount) []ModalityTokenCount {
	if in == nil {
		return nil
	}
	out := make([]ModalityTokenCount, 0, len(in))
	for i := range in {
		out = append(out, ModalityTokenCount{Modality: in[i].Modality, TokenCount: in[i].TokenCount})
	}
	return out
}

func ToLLMTokenLogprob(in []TokenLogprob) []llm.TokenLogprob {
	if in == nil {
		return nil
	}
	out := make([]llm.TokenLogprob, 0, len(in))
	for i := range in {
		item := in[i]
		outItem := llm.TokenLogprob{
			Token:       item.Token,
			Logprob:     item.Logprob,
			Bytes:       item.Bytes,
			TopLogprobs: ToLLMTopLogprobs(item.TopLogprobs),
		}
		out = append(out, outItem)
	}
	return out
}

func FromLLMTokenLogprob(in []llm.TokenLogprob) []TokenLogprob {
	if in == nil {
		return nil
	}
	out := make([]TokenLogprob, 0, len(in))
	for i := range in {
		item := in[i]
		outItem := TokenLogprob{
			Token:       item.Token,
			Logprob:     item.Logprob,
			Bytes:       item.Bytes,
			TopLogprobs: FromLLMTopLogprobs(item.TopLogprobs),
		}
		out = append(out, outItem)
	}
	return out
}

func ToLLMTopLogprobs(in []TopLogprob) []llm.TopLogprob {
	if in == nil {
		return nil
	}
	out := make([]llm.TopLogprob, 0, len(in))
	for i := range in {
		out = append(out, llm.TopLogprob{
			Token:   in[i].Token,
			Logprob: in[i].Logprob,
			Bytes:   in[i].Bytes,
		})
	}
	return out
}

func FromLLMTopLogprobs(in []llm.TopLogprob) []TopLogprob {
	if in == nil {
		return nil
	}
	out := make([]TopLogprob, 0, len(in))
	for i := range in {
		out = append(out, TopLogprob{
			Token:   in[i].Token,
			Logprob: in[i].Logprob,
			Bytes:   in[i].Bytes,
		})
	}
	return out
}
