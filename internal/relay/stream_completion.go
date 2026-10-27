package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/bestruirui/octopus/internal/relay/stream"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// upstreamStreamCompletion watches provider events before conversion. Inbound
// adapters may emit their own finish events, which cannot prove upstream
// completion. Only small protocol metadata is retained, never response text.
type upstreamStreamCompletion struct {
	format   model.APIFormat
	terminal bool
	done     bool
	choices  map[int]bool
}

func (s *upstreamStreamCompletion) observe(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		// A transport marker cannot replace a missing semantic finish event.
		if !s.complete() {
			return stream.ErrIncompleteUpstreamStream
		}
		s.done = true
		return nil
	}
	var event struct {
		Type    string `json:"type"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Candidates []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if json.Unmarshal(data, &event) != nil {
		return nil // The outbound adapter handles malformed provider payloads.
	}
	switch {
	case strings.HasPrefix(event.Type, "response."):
		s.format = model.APIFormatOpenAIResponse
		switch event.Type {
		case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			s.terminal = true
		}
	case event.Type == "message_start" || event.Type == "message_delta" || event.Type == "message_stop":
		s.format = model.APIFormatAnthropicMessage
		if event.Type == "message_stop" {
			s.terminal = true
		}
	case len(event.Choices) > 0:
		s.format = model.APIFormatOpenAIChatCompletion
		for _, choice := range event.Choices {
			s.observeChoice(choice.Index, choice.FinishReason != nil && *choice.FinishReason != "")
		}
	case len(event.Candidates) > 0:
		s.format = model.APIFormatGeminiContents
		for _, candidate := range event.Candidates {
			s.observeChoice(candidate.Index, candidate.FinishReason != "")
		}
	case event.PromptFeedback != nil && event.PromptFeedback.BlockReason != "":
		s.format = model.APIFormatGeminiContents
		s.terminal = true
	}
	return nil
}

func (s *upstreamStreamCompletion) observeChoice(index int, finished bool) {
	if s.choices == nil {
		s.choices = make(map[int]bool)
	}
	s.choices[index] = s.choices[index] || finished
}

func (s *upstreamStreamCompletion) complete() bool {
	if s.terminal {
		return true
	}
	if s.format != model.APIFormatOpenAIChatCompletion && s.format != model.APIFormatGeminiContents {
		return false
	}
	if len(s.choices) == 0 {
		return false
	}
	for _, finished := range s.choices {
		if !finished {
			return false
		}
	}
	return true
}

// Flush a converted terminal event when a provider ends at its semantic finish
// without a separate transport marker (or without optional usage). This is only
// allowed after completion has been verified against the provider's own events.
func (ra *relayAttempt) finishConvertedStream(ctx context.Context, completion *upstreamStreamCompletion) error {
	if !completion.complete() {
		return stream.ErrIncompleteUpstreamStream
	}
	if completion.done || ctx.Err() != nil {
		return nil
	}
	data, err := ra.inAdapter.TransformStream(ctx, &model.InternalLLMResponse{Object: "[DONE]"})
	if err != nil || len(data) == 0 {
		return err
	}
	writer := ra.getStreamWriter()
	if _, err := writer.Write(data); err != nil {
		return err
	}
	writer.Flush()
	return nil
}
