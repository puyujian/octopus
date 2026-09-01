package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samber/lo"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

// ResponseInbound implements the Inbound interface for OpenAI Responses API.
type ResponseInbound struct {
	// State tracking
	hasResponseCreated      bool
	hasMessageItemStarted   bool
	hasReasoningItemStarted bool
	hasContentPartStarted   bool
	hasRefusalPartStarted   bool
	hasFinished             bool
	responseCompleted       bool
	finalFinishReason       string

	// Response metadata
	responseID string
	model      string
	createdAt  int64
	truncation *string

	// Content tracking
	outputIndex    int
	contentIndex   int
	sequenceNumber int
	currentItemID  string

	// messageContentOrder preserves the order content parts were emitted so
	// closeMessageItem can rebuild the final ResponsesInput.Items array with
	// the correct output_text / refusal sequencing.
	messageContentOrder []string

	// Content accumulation
	accumulatedText      strings.Builder
	accumulatedReasoning strings.Builder
	accumulatedRefusal   strings.Builder

	// reasoningBlockSignatures preserves per-thinking-block signatures in
	// arrival order. 0 → no encrypted_content, 1 → raw string (common case),
	// N → JSON-encoded array string so information is not lost.
	reasoningBlockSignatures []string

	// Tool call tracking
	toolCalls           map[int]*model.ToolCall
	toolCallItemStarted map[int]bool
	toolCallOutputIndex map[int]int

	// Usage tracking
	usage *model.Usage

	// completedOutputItems buffers every ResponsesItem emitted during streaming
	// (message / reasoning / function_call) so the terminal response.completed
	// event can echo the full output array. Upstream Responses clients treat
	// an empty output on response.completed as an error (O-H3).
	completedOutputItems []ResponsesItem

	streamAggregator model.StreamAggregator
	// storedResponse stores the non-stream response
	storedResponse *model.InternalLLMResponse
}

func (i *ResponseInbound) TransformRequest(ctx context.Context, body []byte) (*model.InternalLLMRequest, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("failed to decode responses api request: %w", err)
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}

	i.truncation = req.Truncation

	return convertToInternalRequest(&req, body)
}

func (i *ResponseInbound) TransformResponse(ctx context.Context, response *model.InternalLLMResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("response is nil")
	}

	// Store the response for later retrieval
	i.storedResponse = response

	// Convert to Responses API format
	resp := convertToResponsesAPIResponse(response)
	// AxonHub keeps the exact Responses output array when the upstream response
	// was already in Responses format. Prefer that payload for the wire response
	// so provider-specific output items (for example custom tools, annotations,
	// or newer multimodal items) are not narrowed by the local compatibility
	// structs below.
	if len(response.RawResponsesOutputItems) > 0 && json.Valid(response.RawResponsesOutputItems) {
		resp.RawOutput = cloneRaw(response.RawResponsesOutputItems)
	}
	if i.truncation != nil {
		resp.Truncation = i.truncation
	}

	body, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal responses api response: %w", err)
	}

	return body, nil
}

func (i *ResponseInbound) TransformStream(ctx context.Context, stream *model.InternalLLMResponse) ([]byte, error) {
	// Handle [DONE] marker
	if stream.Object == "[DONE]" {
		return []byte("data: [DONE]\n\n"), nil
	}

	// Preserve the original chunk for aggregation; the stream-event view is a
	// normalized projection and intentionally drops some transport-only fields.
	i.streamAggregator.Add(stream)
	if i.createdAt == 0 && stream.Created != 0 {
		i.createdAt = stream.Created
	}
	return i.processStreamEvents(ctx, model.StreamEventsFromInternalResponse(stream), false)
}

func (i *ResponseInbound) TransformStreamEvents(ctx context.Context, events []model.StreamEvent) ([]byte, error) {
	return i.processStreamEvents(ctx, events, true)
}

func (i *ResponseInbound) processStreamEvents(ctx context.Context, events []model.StreamEvent, aggregate bool) ([]byte, error) {
	if len(events) == 0 {
		return nil, nil
	}
	if aggregate {
		if stream := model.InternalResponseFromStreamEvents(events); stream != nil && stream.Object != "[DONE]" {
			i.streamAggregator.Add(stream)
		}
	}

	var out [][]byte

	// Initialize tool call tracking maps if needed
	if i.toolCalls == nil {
		i.toolCalls = make(map[int]*model.ToolCall)
		i.toolCallItemStarted = make(map[int]bool)
		i.toolCallOutputIndex = make(map[int]int)
	}

	for _, event := range events {
		if event.ID != "" {
			i.responseID = event.ID
		}
		if event.Model != "" {
			i.model = event.Model
		}

		switch event.Kind {
		case model.StreamEventKindMessageStart:
			if !i.hasResponseCreated {
				i.hasResponseCreated = true
				response := &ResponsesResponse{
					Object:     "response",
					ID:         i.responseID,
					Model:      i.model,
					CreatedAt:  i.createdAt,
					Status:     lo.ToPtr("in_progress"),
					Truncation: i.truncation,
					Output:     []ResponsesItem{},
				}
				out = append(out, i.enqueueEvent(&ResponsesStreamEvent{Type: "response.created", Response: response}))
				out = append(out, i.enqueueEvent(&ResponsesStreamEvent{Type: "response.in_progress", Response: response}))
			}

		case model.StreamEventKindTextDelta:
			if event.Delta == nil {
				continue
			}
			if event.Delta.Refusal != "" {
				out = append(out, i.handleRefusalContent(event.Delta.Refusal)...)
			} else if event.Delta.Text != "" {
				out = append(out, i.handleTextContent(&event.Delta.Text)...)
			}

		case model.StreamEventKindThinkingDelta:
			if event.Delta != nil && (event.Delta.Thinking != "" || event.Delta.Signature != "") {
				if event.Delta.Signature != "" {
					i.reasoningBlockSignatures = append(i.reasoningBlockSignatures, event.Delta.Signature)
				}
				if event.Delta.Thinking != "" {
					out = append(out, i.handleReasoningContent(&event.Delta.Thinking)...)
				} else {
					out = append(out, i.ensureReasoningItemStarted()...)
				}
			}

		case model.StreamEventKindSignatureDelta:
			if event.Delta != nil && event.Delta.Signature != "" {
				i.reasoningBlockSignatures = append(i.reasoningBlockSignatures, event.Delta.Signature)
				out = append(out, i.ensureReasoningItemStarted()...)
			}

		case model.StreamEventKindContentBlockStart:
			if event.ContentBlock != nil && event.ContentBlock.Type == string(model.ReasoningBlockKindRedacted) {
				out = append(out, i.ensureReasoningItemStarted()...)
			}

		case model.StreamEventKindToolCallStart:
			if event.ToolCall != nil {
				out = append(out, i.handleToolCalls([]model.ToolCall{*event.ToolCall})...)
			}

		case model.StreamEventKindToolCallDelta:
			if event.ToolCall != nil {
				out = append(out, i.handleToolCalls([]model.ToolCall{*event.ToolCall})...)
			}

		case model.StreamEventKindMessageStop:
			if !i.hasFinished {
				i.hasFinished = true
				i.finalFinishReason = event.StopReason.String()
				out = append(out, i.closeCurrentContentPart()...)
				out = append(out, i.closeCurrentOutputItem()...)
			}

		case model.StreamEventKindUsageDelta:
			if event.Usage != nil && i.hasFinished && !i.responseCompleted {
				i.responseCompleted = true
				i.usage = event.Usage
				eventType, status := responsesTerminalEvent(i.finalFinishReason)
				output := i.finalOutputItems()
				if event.ProviderExtensions != nil && event.ProviderExtensions.OpenAI != nil && len(event.ProviderExtensions.OpenAI.RawResponseItems) > 0 {
					var items []ResponsesItem
					if err := json.Unmarshal(event.ProviderExtensions.OpenAI.RawResponseItems, &items); err == nil {
						output = items
					}
				}
				response := &ResponsesResponse{
					Object:     "response",
					ID:         i.responseID,
					Model:      i.model,
					CreatedAt:  i.createdAt,
					Status:     &status,
					Truncation: i.truncation,
					Output:     output,
					Usage:      convertUsageToResponses(i.usage),
				}
				out = append(out, i.enqueueEvent(&ResponsesStreamEvent{Type: eventType, Response: response}))
			}

		case model.StreamEventKindDone:
			if len(out) == 0 {
				return []byte("data: [DONE]\n\n"), nil
			}

		case model.StreamEventKindError:
			if event.Error == nil {
				continue
			}
			i.responseCompleted = true
			response := &ResponsesResponse{
				Object:    "response",
				ID:        i.responseID,
				Model:     i.model,
				CreatedAt: i.createdAt,
				Status:    lo.ToPtr("failed"),
				Error: &ResponsesError{
					Code:    500,
					Message: event.Error.Detail.Message,
				},
			}
			out = append(out, i.enqueueEvent(&ResponsesStreamEvent{Type: "response.failed", Response: response}))
		}
	}

	if len(out) == 0 {
		return nil, nil
	}

	result := make([]byte, 0)
	for _, event := range out {
		if event != nil {
			result = append(result, event...)
		}
	}
	return result, nil
}

func (i *ResponseInbound) enqueueEvent(ev *ResponsesStreamEvent) []byte {
	ev.SequenceNumber = i.sequenceNumber
	i.sequenceNumber++

	data, err := json.Marshal(ev)
	if err != nil {
		return nil
	}

	return formatSSEData(data)
}

func (i *ResponseInbound) handleReasoningContent(content *string) [][]byte {
	var events [][]byte

	events = append(events, i.ensureReasoningItemStarted()...)

	// Accumulate reasoning content
	i.accumulatedReasoning.WriteString(*content)

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.reasoning.delta",
		ItemID:      &i.currentItemID,
		OutputIndex: lo.ToPtr(i.outputIndex),
		Delta:       *content,
	}))

	// Emit reasoning_summary_text.delta
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_text.delta",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Delta:        *content,
	}))

	return events
}

func (i *ResponseInbound) ensureReasoningItemStarted() [][]byte {
	if i.hasReasoningItemStarted {
		return nil
	}

	var events [][]byte

	events = append(events, i.closeCurrentOutputItem()...)

	i.hasReasoningItemStarted = true
	i.currentItemID = generateItemID()

	item := &ResponsesItem{
		ID:      i.currentItemID,
		Type:    "reasoning",
		Status:  lo.ToPtr("in_progress"),
		Summary: []ResponsesReasoningSummary{},
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: lo.ToPtr(i.outputIndex),
		Item:        item,
	}))

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_part.added",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Part:         &ResponsesContentPart{Type: "summary_text"},
	}))

	return events
}

func (i *ResponseInbound) handleTextContent(content *string) [][]byte {
	var events [][]byte

	// Close reasoning item if it was started
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	// Close refusal part if active — text becomes a new content part
	if i.hasRefusalPartStarted {
		events = append(events, i.closeCurrentContentPart()...)
	}

	// Start message output item if not started
	if !i.hasMessageItemStarted {
		i.hasMessageItemStarted = true
		i.currentItemID = generateItemID()

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.outputIndex),
			Item: &ResponsesItem{
				ID:      i.currentItemID,
				Type:    "message",
				Status:  lo.ToPtr("in_progress"),
				Role:    "assistant",
				Content: &ResponsesInput{Items: []ResponsesItem{}},
			},
		}))
	}

	// Start content part if not started
	if !i.hasContentPartStarted {
		i.hasContentPartStarted = true
		i.messageContentOrder = append(i.messageContentOrder, "output_text")

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.content_part.added",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: lo.ToPtr(i.contentIndex),
			Part: &ResponsesContentPart{
				Type: "output_text",
				Text: lo.ToPtr(""),
			},
		}))
	}

	// Accumulate text content
	i.accumulatedText.WriteString(*content)

	// Emit output_text.delta
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		ContentIndex: lo.ToPtr(i.contentIndex),
		Delta:        *content,
	}))

	return events
}

// handleRefusalContent mirrors handleTextContent but emits refusal-family
// stream events (response.content_part.added with Part.Type="refusal",
// response.refusal.delta). Refusal is a distinct content part: if a text
// part was open it is closed first so the two parts land at separate
// content_index values.
func (i *ResponseInbound) handleRefusalContent(content string) [][]byte {
	var events [][]byte

	// Close reasoning item if it was started
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	// Close text part if active — refusal becomes a new content part
	if i.hasContentPartStarted {
		events = append(events, i.closeCurrentContentPart()...)
	}

	// Start message output item if not started
	if !i.hasMessageItemStarted {
		i.hasMessageItemStarted = true
		i.currentItemID = generateItemID()

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:        "response.output_item.added",
			OutputIndex: lo.ToPtr(i.outputIndex),
			Item: &ResponsesItem{
				ID:      i.currentItemID,
				Type:    "message",
				Status:  lo.ToPtr("in_progress"),
				Role:    "assistant",
				Content: &ResponsesInput{Items: []ResponsesItem{}},
			},
		}))
	}

	// Start refusal content part if not started
	if !i.hasRefusalPartStarted {
		i.hasRefusalPartStarted = true
		i.messageContentOrder = append(i.messageContentOrder, "refusal")

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.content_part.added",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: lo.ToPtr(i.contentIndex),
			Part: &ResponsesContentPart{
				Type: "refusal",
				Text: lo.ToPtr(""),
			},
		}))
	}

	// Accumulate refusal content
	i.accumulatedRefusal.WriteString(content)

	// Emit refusal.delta
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.refusal.delta",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		ContentIndex: lo.ToPtr(i.contentIndex),
		Delta:        content,
	}))

	return events
}

func (i *ResponseInbound) handleToolCalls(toolCalls []model.ToolCall) [][]byte {
	var events [][]byte

	// Close message item if it was started
	if i.hasMessageItemStarted {
		events = append(events, i.closeMessageItem()...)
	}

	// Close reasoning item if it was started
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	for _, tc := range toolCalls {
		toolCallIndex := tc.Index

		// Initialize tool call tracking if needed
		if _, ok := i.toolCalls[toolCallIndex]; !ok {
			events = append(events, i.closeCurrentContentPart()...)
			events = append(events, i.closeCurrentOutputItem()...)

			i.toolCalls[toolCallIndex] = &model.ToolCall{
				Index: toolCallIndex,
				ID:    tc.ID,
				Type:  tc.Type,
				Function: model.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: "",
				},
			}

			itemID := tc.ID
			if itemID == "" {
				itemID = generateItemID()
			}

			item := &ResponsesItem{
				ID:     itemID,
				Type:   "function_call",
				Status: lo.ToPtr("in_progress"),
				CallID: tc.ID,
				Name:   tc.Function.Name,
			}

			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:        "response.output_item.added",
				OutputIndex: lo.ToPtr(i.outputIndex),
				Item:        item,
			}))

			i.toolCallItemStarted[toolCallIndex] = true
			i.toolCallOutputIndex[toolCallIndex] = i.outputIndex
			i.currentItemID = itemID
			i.outputIndex++
		}

		// Accumulate arguments
		i.toolCalls[toolCallIndex].Function.Arguments += tc.Function.Arguments

		// Emit function_call_arguments.delta
		if tc.Function.Arguments != "" {
			itemID := i.toolCalls[toolCallIndex].ID
			if itemID == "" {
				itemID = i.currentItemID
			}

			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:         "response.function_call_arguments.delta",
				ItemID:       &itemID,
				OutputIndex:  lo.ToPtr(i.toolCallOutputIndex[toolCallIndex]),
				ContentIndex: lo.ToPtr(0),
				Delta:        tc.Function.Arguments,
			}))
		}
	}

	return events
}

func (i *ResponseInbound) closeReasoningItem() [][]byte {
	if !i.hasReasoningItemStarted {
		return nil
	}

	var events [][]byte
	i.hasReasoningItemStarted = false
	fullReasoning := i.accumulatedReasoning.String()

	// Emit reasoning_summary_text.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_text.done",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Text:         fullReasoning,
	}))

	// Emit reasoning_summary_part.done
	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:         "response.reasoning_summary_part.done",
		ItemID:       &i.currentItemID,
		OutputIndex:  lo.ToPtr(i.outputIndex),
		SummaryIndex: lo.ToPtr(0),
		Part:         &ResponsesContentPart{Type: "summary_text", Text: &fullReasoning},
	}))

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.reasoning.done",
		ItemID:      &i.currentItemID,
		OutputIndex: lo.ToPtr(i.outputIndex),
		Text:        fullReasoning,
	}))

	// Emit output_item.done with encrypted_content if signatures were accumulated.
	// Single signature is emitted as a bare string (the common Anthropic extended-thinking
	// case — one thinking block per turn — so the field round-trips verbatim). Multiple
	// signatures are JSON-encoded into an array string so per-block provenance is not
	// lost; downstream consumers that only read the scalar still see non-empty content.
	item := ResponsesItem{
		ID:   i.currentItemID,
		Type: "reasoning",
		Summary: []ResponsesReasoningSummary{{
			Type: "summary_text",
			Text: fullReasoning,
		}},
	}

	switch len(i.reasoningBlockSignatures) {
	case 0:
		// no encrypted_content
	case 1:
		sig := i.reasoningBlockSignatures[0]
		item.EncryptedContent = &sig
	default:
		if encoded, err := json.Marshal(i.reasoningBlockSignatures); err == nil {
			sig := string(encoded)
			item.EncryptedContent = &sig
		}
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: lo.ToPtr(i.outputIndex),
		Item:        &item,
	}))

	i.completedOutputItems = append(i.completedOutputItems, item)
	i.outputIndex++
	i.accumulatedReasoning.Reset()
	i.reasoningBlockSignatures = nil

	return events
}

func (i *ResponseInbound) closeMessageItem() [][]byte {
	if !i.hasMessageItemStarted {
		return nil
	}

	var events [][]byte
	i.hasMessageItemStarted = false

	// Close whichever content part (text or refusal) is still open
	events = append(events, i.closeCurrentContentPart()...)

	fullText := i.accumulatedText.String()
	fullRefusal := i.accumulatedRefusal.String()

	contentItems := make([]ResponsesItem, 0, 2)
	for _, t := range i.messageContentOrder {
		switch t {
		case "output_text":
			if fullText != "" {
				text := fullText
				contentItems = append(contentItems, ResponsesItem{
					Type: "output_text",
					Text: &text,
				})
			}
		case "refusal":
			if fullRefusal != "" {
				refusal := fullRefusal
				contentItems = append(contentItems, ResponsesItem{
					Type:    "refusal",
					Refusal: &refusal,
				})
			}
		}
	}

	// Preserve legacy shape: a message with no accumulated text still
	// produces a single empty output_text item so downstream clients never
	// see a zero-length content array.
	if len(contentItems) == 0 {
		contentItems = append(contentItems, ResponsesItem{
			Type: "output_text",
			Text: lo.ToPtr(fullText),
		})
	}

	item := ResponsesItem{
		ID:      i.currentItemID,
		Type:    "message",
		Status:  lo.ToPtr("completed"),
		Role:    "assistant",
		Content: &ResponsesInput{Items: contentItems},
	}

	events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: lo.ToPtr(i.outputIndex),
		Item:        &item,
	}))

	i.completedOutputItems = append(i.completedOutputItems, item)
	i.outputIndex++
	i.contentIndex = 0
	i.accumulatedText.Reset()
	i.accumulatedRefusal.Reset()
	i.messageContentOrder = nil

	return events
}

// closeCurrentContentPart flushes whichever content part (output_text or
// refusal) is currently open. The accumulated text for the part is NOT
// reset here — closeMessageItem reads both accumulators to build the final
// output_item.done content array, then resets at message level.
func (i *ResponseInbound) closeCurrentContentPart() [][]byte {
	var events [][]byte

	switch {
	case i.hasContentPartStarted:
		i.hasContentPartStarted = false
		fullText := i.accumulatedText.String()

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.output_text.done",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: lo.ToPtr(i.contentIndex),
			Text:         fullText,
		}))

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.content_part.done",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: lo.ToPtr(i.contentIndex),
			Part: &ResponsesContentPart{
				Type: "output_text",
				Text: lo.ToPtr(fullText),
			},
		}))

		i.contentIndex++

	case i.hasRefusalPartStarted:
		i.hasRefusalPartStarted = false
		fullRefusal := i.accumulatedRefusal.String()

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.refusal.done",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: lo.ToPtr(i.contentIndex),
			Text:         fullRefusal,
		}))

		events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
			Type:         "response.content_part.done",
			ItemID:       &i.currentItemID,
			OutputIndex:  lo.ToPtr(i.outputIndex),
			ContentIndex: lo.ToPtr(i.contentIndex),
			Part: &ResponsesContentPart{
				Type: "refusal",
				Text: lo.ToPtr(fullRefusal),
			},
		}))

		i.contentIndex++
	}

	return events
}

func (i *ResponseInbound) closeCurrentOutputItem() [][]byte {
	var events [][]byte

	// Close message item if open
	if i.hasMessageItemStarted {
		events = append(events, i.closeMessageItem()...)
	}

	// Close reasoning item if open
	if i.hasReasoningItemStarted {
		events = append(events, i.closeReasoningItem()...)
	}

	// Close any open tool call items
	for idx, tc := range i.toolCalls {
		if i.toolCallItemStarted[idx] {
			itemID := tc.ID
			if itemID == "" {
				itemID = i.currentItemID
			}

			// Emit function_call_arguments.done
			toolCallOutputIdx := i.toolCallOutputIndex[idx]
			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:        "response.function_call_arguments.done",
				ItemID:      &itemID,
				OutputIndex: &toolCallOutputIdx,
				Arguments:   tc.Function.Arguments,
			}))

			// Emit output_item.done
			item := ResponsesItem{
				ID:        itemID,
				Type:      "function_call",
				Status:    lo.ToPtr("completed"),
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}

			events = append(events, i.enqueueEvent(&ResponsesStreamEvent{
				Type:        "response.output_item.done",
				OutputIndex: &toolCallOutputIdx,
				Item:        &item,
			}))

			i.completedOutputItems = append(i.completedOutputItems, item)
			i.toolCallItemStarted[idx] = false
		}
	}

	return events
}

// finalOutputItems returns the accumulated output items for response.completed,
// synthesizing an empty message shell when nothing was emitted. The Responses
// spec requires a non-empty output on terminal events.
func (i *ResponseInbound) finalOutputItems() []ResponsesItem {
	if len(i.completedOutputItems) > 0 {
		out := make([]ResponsesItem, len(i.completedOutputItems))
		copy(out, i.completedOutputItems)
		return out
	}
	emptyText := ""
	return []ResponsesItem{
		{
			ID:   generateItemID(),
			Type: "message",
			Role: "assistant",
			Content: &ResponsesInput{
				Items: []ResponsesItem{
					{Type: "output_text", Text: &emptyText},
				},
			},
			Status: lo.ToPtr("completed"),
		},
	}
}

// GetInternalResponse returns the complete internal response for logging, statistics, etc.
// For streaming: aggregates all stored stream chunks into a complete response
// For non-streaming: returns the stored response
func (i *ResponseInbound) GetInternalResponse(ctx context.Context) (*model.InternalLLMResponse, error) {
	if i.storedResponse != nil {
		return i.storedResponse, nil
	}
	return i.streamAggregator.BuildAndReset(), nil
}

// formatSSEData formats data as SSE data line
func formatSSEData(data []byte) []byte {
	return []byte(fmt.Sprintf("data: %s\n\n", string(data)))
}

// Request types

type ResponsesRequest struct {
	Model             string                `json:"model"`
	Instructions      string                `json:"instructions,omitempty"`
	Input             ResponsesInput        `json:"input"`
	Tools             []ResponsesTool       `json:"tools,omitempty"`
	ToolChoice        *ResponsesToolChoice  `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                 `json:"parallel_tool_calls,omitempty"`
	Stream            *bool                 `json:"stream,omitempty"`
	Text              *ResponsesTextOptions `json:"text,omitempty"`
	Store             *bool                 `json:"store,omitempty"`
	ServiceTier       *string               `json:"service_tier,omitempty"`
	User              *string               `json:"user,omitempty"`
	Metadata          map[string]string     `json:"metadata,omitempty"`
	MaxOutputTokens   *int64                `json:"max_output_tokens,omitempty"`
	Temperature       *float64              `json:"temperature,omitempty"`
	TopP              *float64              `json:"top_p,omitempty"`
	Reasoning         *ResponsesReasoning   `json:"reasoning,omitempty"`
	Include           []string              `json:"include,omitempty"`
	TopLogprobs       *int64                `json:"top_logprobs,omitempty"`
	Truncation        *string               `json:"truncation,omitempty"`

	// Pass-through fields for OpenAI Responses API
	PreviousResponseID   *string         `json:"previous_response_id,omitempty"`
	Background           *bool           `json:"background,omitempty"`
	Prompt               json.RawMessage `json:"prompt,omitempty"`
	PromptCacheKey       *string         `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention *string         `json:"prompt_cache_retention,omitempty"`
	SafetyIdentifier     *string         `json:"safety_identifier,omitempty"`
	MaxToolCalls         *int64          `json:"max_tool_calls,omitempty"`
	Conversation         json.RawMessage `json:"conversation,omitempty"`
	ContextManagement    json.RawMessage `json:"context_management,omitempty"`
	StreamOptions        json.RawMessage `json:"stream_options,omitempty"`
}

type ResponsesInput struct {
	Text  *string
	Items []ResponsesItem
}

func (i ResponsesInput) MarshalJSON() ([]byte, error) {
	if i.Text != nil {
		return json.Marshal(i.Text)
	}
	return json.Marshal(i.Items)
}

func (i *ResponsesInput) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		i.Text = &text
		return nil
	}
	var items []ResponsesItem
	if err := json.Unmarshal(data, &items); err == nil {
		i.Items = items
		return nil
	}
	return fmt.Errorf("invalid input format")
}

type ResponsesItem struct {
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Role     string          `json:"role,omitempty"`
	Content  *ResponsesInput `json:"content,omitempty"`
	Status   *string         `json:"status,omitempty"`
	Text     *string         `json:"text,omitempty"`
	Refusal  *string         `json:"refusal,omitempty"`
	ImageURL *string         `json:"image_url,omitempty"`
	Detail   *string         `json:"detail,omitempty"`

	// Annotations for output_text content
	Annotations *[]ResponsesAnnotation `json:"annotations,omitempty"`

	// Function call fields
	CallID    string  `json:"call_id,omitempty"`
	Name      string  `json:"name,omitempty"`
	Namespace string  `json:"namespace,omitempty"`
	Arguments string  `json:"arguments,omitempty"`
	Input     *string `json:"input,omitempty"`

	// Function call output
	Output        *ResponsesInput `json:"output,omitempty"`
	ItemReference *string         `json:"item_reference,omitempty"`

	// Image generation fields
	Result       *string `json:"result,omitempty"`
	Background   *string `json:"background,omitempty"`
	OutputFormat *string `json:"output_format,omitempty"`
	Quality      *string `json:"quality,omitempty"`
	Size         *string `json:"size,omitempty"`

	// Reasoning fields
	Summary          []ResponsesReasoningSummary `json:"summary,omitempty"`
	EncryptedContent *string                     `json:"encrypted_content,omitempty"`
	CreatedBy        *string                     `json:"created_by,omitempty"`

	// Multimodal input fields. OpenAI Responses accepts an `input_file`
	// item as either { file_id } for an uploaded file, { filename,
	// file_data } with an inline base64 (optionally a data URL), or
	// { file_url } for fetch-on-demand. O-H6.
	FileID   *string `json:"file_id,omitempty"`
	Filename *string `json:"filename,omitempty"`
	FileData *string `json:"file_data,omitempty"`
	FileURL  *string `json:"file_url,omitempty"`

	// InputAudio carries the `input_audio` nested object for audio inputs.
	// O-H6.
	InputAudio *ResponsesInputAudio `json:"input_audio,omitempty"`

	// Audio carries Chat Completions-compatible generated audio metadata when
	// the source protocol exposes it. Responses-compatible providers which do
	// not use this field can safely ignore it.
	Audio *ResponsesOutputAudio `json:"audio,omitempty"`
}

// ResponsesInputAudio mirrors OpenAI's `input_audio` content shape used for
// audio inputs on Responses requests. O-H6.
type ResponsesInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format,omitempty"`
}

type ResponsesOutputAudio struct {
	ID         string `json:"id,omitempty"`
	Data       string `json:"data,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

func (item ResponsesItem) isOutputMessageContent() bool {
	if item.Content == nil || len(item.Content.Items) == 0 {
		return false
	}
	for _, ci := range item.Content.Items {
		if ci.Type == "output_text" || ci.Type == "refusal" {
			return true
		}
	}
	return false
}

func (item ResponsesItem) GetContentItems() []ResponsesContentItem {
	if item.Content == nil || len(item.Content.Items) == 0 {
		return nil
	}
	result := make([]ResponsesContentItem, 0, len(item.Content.Items))
	for _, ci := range item.Content.Items {
		text := ""
		if ci.Text != nil {
			text = *ci.Text
		}
		result = append(result, ResponsesContentItem{
			Type:        ci.Type,
			Text:        text,
			Refusal:     valueOrEmpty(ci.Refusal),
			Annotations: cloneResponsesAnnotations(ci.Annotations),
		})
	}
	return result
}

type ResponsesContentItem struct {
	Type        string                `json:"type"`
	Text        string                `json:"text,omitempty"`
	Refusal     string                `json:"refusal,omitempty"`
	Annotations []ResponsesAnnotation `json:"annotations,omitempty"`
}

type ResponsesReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponsesAnnotation struct {
	Type       string  `json:"type"`
	StartIndex *int64  `json:"start_index,omitempty"`
	EndIndex   *int64  `json:"end_index,omitempty"`
	URL        *string `json:"url,omitempty"`
	Title      *string `json:"title,omitempty"`
	FileID     *string `json:"file_id,omitempty"`
	Filename   *string `json:"filename,omitempty"`
}

func cloneResponsesAnnotations(annotations *[]ResponsesAnnotation) []ResponsesAnnotation {
	if annotations == nil {
		return nil
	}
	return append([]ResponsesAnnotation(nil), (*annotations)...)
}

type ResponsesTool struct {
	Type              string                          `json:"type,omitempty"`
	Name              string                          `json:"name,omitempty"`
	Description       string                          `json:"description,omitempty"`
	Parameters        map[string]any                  `json:"parameters,omitempty"`
	Strict            *bool                           `json:"strict,omitempty"`
	Tools             []ResponsesTool                 `json:"tools,omitempty"`
	Format            *ResponsesCustomToolFormat      `json:"format,omitempty"`
	Filters           *ResponsesWebSearchFilters      `json:"filters,omitempty"`
	UserLocation      *ResponsesWebSearchUserLocation `json:"user_location,omitempty"`
	Background        string                          `json:"background,omitempty"`
	OutputFormat      string                          `json:"output_format,omitempty"`
	Quality           string                          `json:"quality,omitempty"`
	Size              string                          `json:"size,omitempty"`
	OutputCompression *int64                          `json:"output_compression,omitempty"`
}

type ResponsesToolChoice struct {
	Mode  *string                     `json:"mode,omitempty"`
	Type  *string                     `json:"type,omitempty"`
	Name  *string                     `json:"name,omitempty"`
	Tools []ResponsesToolChoiceOption `json:"tools,omitempty"`
}

type ResponsesToolChoiceOption struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type ResponsesWebSearchFilters struct {
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

type ResponsesWebSearchUserLocation struct {
	Type     string `json:"type,omitempty"`
	City     string `json:"city,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type ResponsesCustomToolFormat struct {
	Type       string `json:"type"`
	Syntax     string `json:"syntax,omitempty"`
	Definition string `json:"definition,omitempty"`
}

func (t *ResponsesToolChoice) UnmarshalJSON(data []byte) error {
	var mode string
	if err := json.Unmarshal(data, &mode); err == nil {
		t.Mode = &mode
		return nil
	}

	type Alias ResponsesToolChoice
	var alias Alias
	if err := json.Unmarshal(data, &alias); err == nil {
		*t = ResponsesToolChoice(alias)
		return nil
	}

	return fmt.Errorf("invalid tool choice format")
}

type ResponsesTextOptions struct {
	Format    *ResponsesTextFormat `json:"format,omitempty"`
	Verbosity *string              `json:"verbosity,omitempty"`
}

type ResponsesTextFormat struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

type ResponsesReasoning struct {
	Context         string  `json:"context,omitempty"`
	Effort          string  `json:"effort,omitempty"`
	MaxTokens       *int64  `json:"max_tokens,omitempty"`
	Summary         *string `json:"summary,omitempty"`
	GenerateSummary *string `json:"generate_summary,omitempty"`
}

// Response types

type ResponsesResponse struct {
	Object             string                      `json:"object"`
	ID                 string                      `json:"id"`
	Model              string                      `json:"model"`
	CreatedAt          int64                       `json:"created_at"`
	PreviousResponseID *string                     `json:"previous_response_id,omitempty"`
	Output             []ResponsesItem             `json:"output"`
	Status             *string                     `json:"status,omitempty"`
	Truncation         *string                     `json:"truncation,omitempty"`
	IncompleteDetails  *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`
	Usage              *ResponsesUsage             `json:"usage,omitempty"`
	Error              *ResponsesError             `json:"error,omitempty"`
	RawOutput          json.RawMessage             `json:"-"`
}

// MarshalJSON lets the Responses inbound adapter replay an exact output array
// captured by the AxonHub bridge. The normal typed Output remains the fallback
// for responses produced by other protocols.
func (r ResponsesResponse) MarshalJSON() ([]byte, error) {
	type responseAlias ResponsesResponse
	base, err := json.Marshal(responseAlias(r))
	if err != nil {
		return nil, err
	}
	if len(r.RawOutput) == 0 || !json.Valid(r.RawOutput) {
		return base, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(base, &object); err != nil {
		return nil, err
	}
	object["output"] = cloneRaw(r.RawOutput)
	return json.Marshal(object)
}

type ResponsesIncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type ResponsesUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	InputTokenDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens       int64 `json:"output_tokens"`
	OutputTokenDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`
}

type ResponsesError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ResponsesStreamEvent struct {
	Type           string                `json:"type"`
	SequenceNumber int                   `json:"sequence_number"`
	Response       *ResponsesResponse    `json:"response,omitempty"`
	OutputIndex    *int                  `json:"output_index,omitempty"`
	Item           *ResponsesItem        `json:"item,omitempty"`
	ItemID         *string               `json:"item_id,omitempty"`
	ContentIndex   *int                  `json:"content_index,omitempty"`
	Delta          string                `json:"delta,omitempty"`
	Text           string                `json:"text,omitempty"`
	Name           string                `json:"name,omitempty"`
	CallID         string                `json:"call_id,omitempty"`
	Namespace      string                `json:"namespace,omitempty"`
	Arguments      string                `json:"arguments,omitempty"`
	SummaryIndex   *int                  `json:"summary_index,omitempty"`
	Part           *ResponsesContentPart `json:"part,omitempty"`
	Code           string                `json:"code,omitempty"`
	Message        string                `json:"message,omitempty"`
}

type ResponsesContentPart struct {
	Type        string                `json:"type"`
	Text        *string               `json:"text,omitempty"`
	Annotations []ResponsesAnnotation `json:"annotations,omitempty"`
}

// Conversion functions

func convertToInternalRequest(req *ResponsesRequest, rawBody ...[]byte) (*model.InternalLLMRequest, error) {
	chatReq := &model.InternalLLMRequest{
		Model:               req.Model,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		Stream:              req.Stream,
		Store:               req.Store,
		ServiceTier:         req.ServiceTier,
		Truncation:          req.Truncation,
		User:                req.User,
		Metadata:            req.Metadata,
		MaxCompletionTokens: req.MaxOutputTokens,
		TopLogprobs:         req.TopLogprobs,
		ParallelToolCalls:   req.ParallelToolCalls,
		RawAPIFormat:        model.APIFormatOpenAIResponse,
		TransformerMetadata: map[string]string{},
		Include:             append([]string(nil), req.Include...),
	}

	rawRequestParts := parseRawResponsesRequestParts(req, rawBody...)
	if req.Input.Text == nil && len(req.Input.Items) > 0 {
		chatReq.TransformOptions.ArrayInputs = lo.ToPtr(true)
		if rawItems := marshalRawResponsesItems(rawRequestParts.InputItems); len(rawItems) > 0 {
			chatReq.SetOpenAIRawInputItems(rawItems)
		}
	}
	markOpenAIResponsesPassthroughIfNeeded(req, chatReq)

	var reasoningSummary *string
	var reasoningGenerateSummary *string

	// Convert reasoning
	if req.Reasoning != nil {
		// Reasoning.Context is provider-scoped Responses metadata. Keep it
		// separate from the common reasoning fields so AxonHub can reconstruct
		// the exact Responses request shape.
		if effort := validateReasoningEffort(req.Reasoning.Effort); effort != "" {
			chatReq.ReasoningEffort = effort
		}
		if req.Reasoning.MaxTokens != nil {
			chatReq.ReasoningBudget = req.Reasoning.MaxTokens
		}
		if req.Reasoning.Summary != nil {
			if summary := validateReasoningSummary(*req.Reasoning.Summary); summary != "" {
				reasoningSummary = &summary
			}
		}
		reasoningGenerateSummary = req.Reasoning.GenerateSummary
	}

	chatReq.SetOpenAIResponsesOptions(model.OpenAIResponsesOptions{
		PreviousResponseID:       req.PreviousResponseID,
		Background:               req.Background,
		Prompt:                   req.Prompt,
		PromptCacheKey:           req.PromptCacheKey,
		PromptCacheRetention:     req.PromptCacheRetention,
		SafetyIdentifier:         req.SafetyIdentifier,
		MaxToolCalls:             req.MaxToolCalls,
		Conversation:             req.Conversation,
		ContextManagement:        req.ContextManagement,
		StreamOptions:            req.StreamOptions,
		ReasoningSummary:         reasoningSummary,
		ReasoningGenerateSummary: reasoningGenerateSummary,
		ReasoningContext:         valueOrString(req.Reasoning, func(reasoning *ResponsesReasoning) string { return reasoning.Context }),
		RawTools:                 buildRawResponsesToolFragments(req.Tools, rawRequestParts.Tools),
		ToolSignatures:           buildResponsesToolSignatures(req.Tools),
		RawToolChoice:            rawResponsesToolChoice(req.ToolChoice, rawRequestParts.ToolChoice),
		RawInputFragments:        buildRawResponsesInputFragments(rawRequestParts.InputItems),
		RawInputItems:            chatReq.OpenAIRawInputItems(),
	})

	// Convert tool choice
	if req.ToolChoice != nil {
		chatReq.ToolChoice = convertToolChoiceToInternal(req.ToolChoice)
	}

	// Convert instructions to system message
	messages := make([]model.Message, 0)
	if req.Instructions != "" {
		messages = append(messages, model.Message{
			Role: "system",
			Content: model.MessageContent{
				Content: lo.ToPtr(req.Instructions),
			},
		})
	}

	// Convert input to messages
	inputMessages, err := convertInputToMessages(&req.Input)
	if err != nil {
		return nil, err
	}
	messages = append(messages, inputMessages...)
	chatReq.Messages = messages

	// Convert tools
	if len(req.Tools) > 0 {
		tools, err := convertToolsToInternal(req.Tools)
		if err != nil {
			return nil, err
		}
		chatReq.Tools = tools
	}

	// Convert text format
	if req.Text != nil && req.Text.Format != nil && req.Text.Format.Type != "" {
		rf := &model.ResponseFormat{
			Type:        req.Text.Format.Type,
			Name:        req.Text.Format.Name,
			Description: req.Text.Format.Description,
			Strict:      req.Text.Format.Strict,
		}
		if len(req.Text.Format.Schema) > 0 {
			rf.RawSchema = req.Text.Format.Schema
			rf.JSONSchema = req.Text.Format.Schema
			if parsed, err := model.ParseSchema(req.Text.Format.Schema); err == nil {
				rf.Schema = parsed
			}
		}
		chatReq.ResponseFormat = rf
	}
	if req.Text != nil {
		chatReq.Verbosity = req.Text.Verbosity
	}

	return chatReq, nil
}

// rawResponsesRequestParts retains the original wire fragments needed by
// AxonHub's provider extension merger. The normalized local request continues
// to carry the complete input array for websocket exact replay.
type rawResponsesRequestParts struct {
	Tools      []json.RawMessage
	ToolChoice json.RawMessage
	InputItems []json.RawMessage
}

func parseRawResponsesRequestParts(req *ResponsesRequest, rawBody ...[]byte) rawResponsesRequestParts {
	var raw struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Input      json.RawMessage   `json:"input"`
	}
	if len(rawBody) > 0 && len(rawBody[0]) > 0 {
		_ = json.Unmarshal(rawBody[0], &raw)
	}
	if len(raw.Tools) == 0 && req != nil && len(req.Tools) > 0 {
		for _, tool := range req.Tools {
			if encoded, err := json.Marshal(tool); err == nil {
				raw.Tools = append(raw.Tools, encoded)
			}
		}
	}
	if len(raw.ToolChoice) == 0 && req != nil && req.ToolChoice != nil {
		if encoded, err := json.Marshal(req.ToolChoice); err == nil {
			raw.ToolChoice = encoded
		}
	}
	if len(raw.Input) == 0 && req != nil && req.Input.Text == nil && len(req.Input.Items) > 0 {
		if encoded, err := json.Marshal(req.Input.Items); err == nil {
			raw.Input = encoded
		}
	}
	var inputItems []json.RawMessage
	if len(raw.Input) > 0 {
		_ = json.Unmarshal(raw.Input, &inputItems)
	}
	return rawResponsesRequestParts{
		Tools:      raw.Tools,
		ToolChoice: cloneRaw(raw.ToolChoice),
		InputItems: inputItems,
	}
}

func marshalRawResponsesItems(items []json.RawMessage) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return encoded
}

func buildRawResponsesToolFragments(tools []ResponsesTool, rawTools []json.RawMessage) []model.OpenAIResponsesRawFragment {
	if len(rawTools) == 0 {
		return nil
	}
	fragments := make([]model.OpenAIResponsesRawFragment, 0)
	for index, rawTool := range rawTools {
		var probe struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(rawTool, &probe)
		if isStructurallyRepresentedResponsesToolType(probe.Type) {
			continue
		}
		representedToolCount := 0
		if index < len(tools) {
			representedToolCount = representedNamespaceToolCount(tools[index])
		}
		fragments = append(fragments, model.OpenAIResponsesRawFragment{
			Type:                 probe.Type,
			Name:                 probe.Name,
			OriginalIndex:        index,
			RepresentedToolCount: representedToolCount,
			Raw:                  cloneRaw(rawTool),
		})
	}
	return fragments
}

func buildResponsesToolSignatures(tools []ResponsesTool) []string {
	if len(tools) == 0 {
		return nil
	}
	signatures := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Type == "namespace" {
			for _, subTool := range tool.Tools {
				if subTool.Type == "function" {
					signatures = append(signatures, responseToolSignature(ResponsesTool{
						Type: "function",
						Name: namespaceFunctionName(tool.Name, subTool.Name),
					}))
				}
			}
			continue
		}
		if !isStructurallyRepresentedResponsesToolType(tool.Type) {
			continue
		}
		signatures = append(signatures, responseToolSignature(tool))
	}
	return signatures
}

func isStructurallyRepresentedResponsesToolType(toolType string) bool {
	switch toolType {
	case "function", "image_generation", "web_search", "custom":
		return true
	default:
		return false
	}
}

func responseToolSignature(tool ResponsesTool) string {
	switch tool.Type {
	case "function", "custom":
		return tool.Type + ":" + tool.Name
	default:
		return tool.Type
	}
}

func representedNamespaceToolCount(tool ResponsesTool) int {
	if tool.Type != "namespace" {
		return 0
	}
	count := 0
	for _, subTool := range tool.Tools {
		if subTool.Type == "function" {
			count++
		}
	}
	return count
}

// namespaceFunctionName is the flattened name used by the shared internal
// function-tool representation. Keep it aligned with AxonHub's Responses
// inbound conversion so raw-fragment merging and provider tool signatures use
// the same identity.
func namespaceFunctionName(namespaceName, functionName string) string {
	return namespaceName + "__" + functionName
}

func buildRawResponsesInputFragments(rawItems []json.RawMessage) []model.OpenAIResponsesRawFragment {
	if len(rawItems) == 0 {
		return nil
	}
	fragments := make([]model.OpenAIResponsesRawFragment, 0)
	for index, rawItem := range rawItems {
		var probe struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(rawItem, &probe)
		if isStructurallyRepresentedResponsesInputItem(probe.Type, rawItem) {
			continue
		}
		fragments = append(fragments, model.OpenAIResponsesRawFragment{
			Type:          probe.Type,
			Name:          probe.Name,
			CallID:        probe.CallID,
			OriginalIndex: index,
			Raw:           cloneRaw(rawItem),
		})
	}
	return fragments
}

func isStructurallyRepresentedResponsesInputType(itemType string) bool {
	switch itemType {
	case "", "message", "input_text", "input_image", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning", "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

// isStructurallyRepresentedResponsesInputItem is stricter than the type-only
// check for message items. AxonHub can rebuild text and images inside a
// message, but its Responses converter does not yet emit nested input_file or
// input_audio parts. Preserve the complete parent item in that case instead of
// silently dropping only the unsupported nested part.
func isStructurallyRepresentedResponsesInputItem(itemType string, raw json.RawMessage) bool {
	if !isStructurallyRepresentedResponsesInputType(itemType) {
		return false
	}
	if itemType != "message" {
		return true
	}

	var item struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &item) != nil || len(item.Content) == 0 {
		return true
	}
	var contentItems []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(item.Content, &contentItems) != nil {
		return true
	}
	for _, contentItem := range contentItems {
		if !isStructurallyRepresentedResponsesContentType(contentItem.Type) {
			return false
		}
	}
	return true
}

func isStructurallyRepresentedResponsesContentType(contentType string) bool {
	switch contentType {
	case "", "input_text", "text", "output_text", "input_image", "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func rawResponsesToolChoice(choice *ResponsesToolChoice, raw json.RawMessage) json.RawMessage {
	if choice == nil || len(raw) == 0 {
		return nil
	}
	if len(choice.Tools) > 0 {
		return cloneRaw(raw)
	}
	return nil
}

func valueOrString(reasoning *ResponsesReasoning, value func(*ResponsesReasoning) string) string {
	if reasoning == nil || value == nil {
		return ""
	}
	return value(reasoning)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func markOpenAIResponsesPassthroughIfNeeded(req *ResponsesRequest, chatReq *model.InternalLLMRequest) {
	if req == nil || chatReq == nil {
		return
	}
	if unsupportedToolType := firstUnsupportedResponsesToolType(req.Tools); unsupportedToolType != "" {
		chatReq.MarkOpenAIResponsesPassthroughRequired("tool:" + unsupportedToolType)
	}
	if unsupportedItemType := firstUnsupportedResponsesInputType(&req.Input); unsupportedItemType != "" {
		chatReq.MarkOpenAIResponsesPassthroughRequired("input:" + unsupportedItemType)
	}
}

func firstUnsupportedResponsesToolType(tools []ResponsesTool) string {
	for _, tool := range tools {
		switch tool.Type {
		case "function", "image_generation", "web_search", "custom", "namespace":
			continue
		case "":
			return "<empty>"
		default:
			return tool.Type
		}
	}
	return ""
}

func firstUnsupportedResponsesInputType(input *ResponsesInput) string {
	if input == nil || len(input.Items) == 0 {
		return ""
	}
	for _, item := range input.Items {
		if unsupported := firstUnsupportedResponsesTopLevelItemType(&item); unsupported != "" {
			return unsupported
		}
	}
	return ""
}

func firstUnsupportedResponsesTopLevelItemType(item *ResponsesItem) string {
	if item == nil {
		return ""
	}
	switch item.Type {
	case "", "message", "input_text", "input_image", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning", "compaction", "compaction_summary":
	default:
		return item.Type
	}
	if unsupported := firstUnsupportedResponsesContentItemType(item.Content); unsupported != "" {
		return unsupported
	}
	if unsupported := firstUnsupportedResponsesContentItemType(item.Output); unsupported != "" {
		return unsupported
	}
	return ""
}

func firstUnsupportedResponsesContentItemType(input *ResponsesInput) string {
	if input == nil || input.Text != nil || len(input.Items) == 0 {
		return ""
	}
	for _, item := range input.Items {
		switch item.Type {
		case "input_text", "text", "output_text", "input_image", "compaction", "compaction_summary":
			continue
		case "":
			return "<empty>"
		default:
			return item.Type
		}
	}
	return ""
}

func convertToolChoiceToInternal(src *ResponsesToolChoice) *model.ToolChoice {
	if src == nil {
		return nil
	}

	result := &model.ToolChoice{}
	if src.Mode != nil {
		result.ToolChoice = src.Mode
	} else if src.Type != nil {
		result.NamedToolChoice = &model.NamedToolChoice{
			Type: *src.Type,
		}
		if src.Name != nil {
			name := *src.Name
			result.NamedToolChoice.Function = &model.ToolFunction{Name: name}
			result.NamedToolChoice.Name = &name
		}
	}
	return result
}

func convertInputToMessages(input *ResponsesInput) ([]model.Message, error) {
	if input == nil {
		return nil, nil
	}

	// Simple text input
	if input.Text != nil {
		return []model.Message{
			{
				Role: "user",
				Content: model.MessageContent{
					Content: input.Text,
				},
			},
		}, nil
	}

	// Array of items
	messages := make([]model.Message, 0, len(input.Items))
	for itemIndex := 0; itemIndex < len(input.Items); {
		item := input.Items[itemIndex]

		// Responses represents parallel function calls as consecutive output
		// items. Chat Completions represents the same turn as one assistant
		// message with multiple tool_calls followed by the matching tool
		// messages. Keeping one assistant message per Responses item produces
		// an invalid Chat history (assistant, assistant, tool, tool) that strict
		// upstreams reject before seeing the later tool outputs.
		if isResponsesToolCallItem(item.Type) {
			assistant := model.Message{Role: "assistant"}
			for itemIndex < len(input.Items) && isResponsesToolCallItem(input.Items[itemIndex].Type) {
				toolCallMessage, err := convertItemToMessage(&input.Items[itemIndex])
				if err != nil {
					return nil, err
				}
				if toolCallMessage != nil {
					assistant.ToolCalls = append(assistant.ToolCalls, toolCallMessage.ToolCalls...)
				}
				itemIndex++
			}
			if len(assistant.ToolCalls) > 0 {
				messages = append(messages, assistant)
			}
			continue
		}

		msg, err := convertItemToMessage(&item)
		if err != nil {
			return nil, err
		}
		if msg != nil {
			messages = append(messages, *msg)
		}
		itemIndex++
	}

	return messages, nil
}

func isResponsesToolCallItem(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func convertItemToMessage(item *ResponsesItem) (*model.Message, error) {
	if item == nil {
		return nil, nil
	}

	switch item.Type {
	case "message", "input_text", "":
		msg := &model.Message{
			ID:   item.ID,
			Role: item.Role,
		}

		if item.Content != nil && len(item.Content.Items) > 0 && item.isOutputMessageContent() {
			contentItems := item.GetContentItems()
			msg.Content = convertContentItemsToMessageContent(contentItems)
			for _, contentItem := range contentItems {
				if contentItem.Refusal != "" {
					msg.Refusal += contentItem.Refusal
				}
				msg.Annotations = append(msg.Annotations, fromResponsesAnnotations(contentItem.Annotations)...)
			}
		} else if item.Content != nil {
			msg.Content = convertInputToMessageContent(*item.Content)
		} else if item.Text != nil {
			msg.Content = model.MessageContent{Content: item.Text}
		}

		return msg, nil

	case "input_image", "input_file", "input_audio":
		role := item.Role
		if role == "" {
			role = "user"
		}
		content := convertInputToMessageContent(ResponsesInput{
			Items: []ResponsesItem{*item},
		})
		if content.Content == nil && len(content.MultipleContent) == 0 {
			return nil, nil
		}
		return &model.Message{
			Role:    role,
			Content: content,
		}, nil

	case "function_call":
		return &model.Message{
			Role: "assistant",
			ToolCalls: []model.ToolCall{
				{
					ID:   item.CallID,
					Type: "function",
					Function: model.FunctionCall{
						Name:      item.Name,
						Namespace: item.Namespace,
						Arguments: item.Arguments,
					},
				},
			},
		}, nil

	case "custom_tool_call":
		input := ""
		if item.Input != nil {
			input = *item.Input
		}
		return &model.Message{
			Role: "assistant",
			ToolCalls: []model.ToolCall{{
				ID:   item.CallID,
				Type: "responses_custom_tool",
				ResponseCustomToolCall: &model.ResponseCustomToolCall{
					CallID: item.CallID,
					Name:   item.Name,
					Input:  input,
				},
			}},
		}, nil

	case "function_call_output":
		if item.Output == nil {
			return nil, fmt.Errorf("function_call_output item must have non-nil output")
		}
		return &model.Message{
			Role:       "tool",
			ToolCallID: lo.ToPtr(item.CallID),
			Content:    convertInputToMessageContent(*item.Output),
		}, nil

	case "custom_tool_call_output":
		if item.Output == nil {
			return nil, fmt.Errorf("custom_tool_call_output item must have non-nil output")
		}
		return &model.Message{
			Role:       "tool",
			ToolCallID: lo.ToPtr(item.CallID),
			ToolCallName: func() *string {
				if item.Name == "" {
					return nil
				}
				return lo.ToPtr(item.Name)
			}(),
			Content: convertInputToMessageContent(*item.Output),
		}, nil

	case "reasoning":
		msg := &model.Message{
			ID:   item.ID,
			Role: "assistant",
		}

		var reasoningText strings.Builder
		for _, summary := range item.Summary {
			reasoningText.WriteString(summary.Text)
		}

		if reasoningText.Len() > 0 || item.EncryptedContent != nil {
			itemContent := model.ReasoningItem{ID: item.ID, Content: reasoningText.String()}
			if item.EncryptedContent != nil {
				itemContent.Signature = *item.EncryptedContent
			}
			msg.ReasoningItems = []model.ReasoningItem{itemContent}
			if reasoningText.Len() > 0 {
				msg.ReasoningContent = lo.ToPtr(reasoningText.String())
			}
			if itemContent.Signature != "" {
				msg.ReasoningSignature = lo.ToPtr(itemContent.Signature)
			}
		}

		return msg, nil

	case "compaction", "compaction_summary":
		return &model.Message{
			ID:   item.ID,
			Role: "assistant",
			Content: model.MessageContent{MultipleContent: []model.MessageContentPart{{
				ID:   item.ID,
				Type: item.Type,
				Compact: &model.CompactContent{
					ID:               item.ID,
					EncryptedContent: valueOrEmpty(item.EncryptedContent),
					CreatedBy:        item.CreatedBy,
				},
			}}},
		}, nil

	default:
		return nil, nil
	}
}

func convertInputToMessageContent(input ResponsesInput) model.MessageContent {
	if input.Text != nil {
		return model.MessageContent{Content: input.Text}
	}

	parts := make([]model.MessageContentPart, 0, len(input.Items))
	for _, item := range input.Items {
		switch item.Type {
		case "input_text", "text", "output_text":
			if item.Text != nil {
				parts = append(parts, model.MessageContentPart{
					Type: "text",
					Text: item.Text,
				})
			}
		case "input_image":
			if item.ImageURL != nil {
				parts = append(parts, model.MessageContentPart{
					Type: "image_url",
					ImageURL: &model.ImageURL{
						URL:    *item.ImageURL,
						Detail: item.Detail,
					},
				})
			}
		case "input_file":
			// O-H6: OpenAI Responses accepts three shapes for input_file —
			// keep whichever representation the caller provided so
			// downstream transformers can route the reference verbatim.
			file := &model.File{}
			if item.FileID != nil {
				file.FileID = *item.FileID
			}
			if item.FileURL != nil {
				file.FileURL = *item.FileURL
			}
			if item.Filename != nil {
				file.Filename = *item.Filename
			}
			if item.FileData != nil {
				file.FileData = *item.FileData
			}
			if file.FileID == "" && file.FileURL == "" && file.FileData == "" {
				continue
			}
			parts = append(parts, model.MessageContentPart{
				Type: "file",
				File: file,
			})
		case "input_audio":
			// O-H6: `input_audio` rides in a nested object per the
			// Responses schema ({ data, format }).
			if item.InputAudio == nil {
				continue
			}
			parts = append(parts, model.MessageContentPart{
				Type: "input_audio",
				Audio: &model.Audio{
					Format: item.InputAudio.Format,
					Data:   item.InputAudio.Data,
				},
			})
		case "compaction", "compaction_summary":
			parts = append(parts, model.MessageContentPart{
				ID:   item.ID,
				Type: item.Type,
				Compact: &model.CompactContent{
					ID:               item.ID,
					EncryptedContent: valueOrEmpty(item.EncryptedContent),
					CreatedBy:        item.CreatedBy,
				},
			})
		}
	}

	if len(parts) == 1 && parts[0].Type == "text" && parts[0].Text != nil {
		return model.MessageContent{Content: parts[0].Text}
	}

	return model.MessageContent{MultipleContent: parts}
}

func convertContentItemsToMessageContent(items []ResponsesContentItem) model.MessageContent {
	if len(items) == 1 && (items[0].Type == "output_text" || items[0].Type == "input_text" || items[0].Type == "text") {
		return model.MessageContent{Content: lo.ToPtr(items[0].Text)}
	}

	parts := make([]model.MessageContentPart, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "output_text", "input_text", "text":
			parts = append(parts, model.MessageContentPart{
				Type: "text",
				Text: lo.ToPtr(item.Text),
			})
		}
	}

	return model.MessageContent{MultipleContent: parts}
}

func fromResponsesAnnotations(annotations []ResponsesAnnotation) []model.Annotation {
	if len(annotations) == 0 {
		return nil
	}
	result := make([]model.Annotation, 0, len(annotations))
	for _, annotation := range annotations {
		converted := model.Annotation{
			Type:       annotation.Type,
			StartIndex: annotation.StartIndex,
			EndIndex:   annotation.EndIndex,
		}
		if annotation.URL != nil || annotation.Title != nil {
			converted.URLCitation = &model.URLCitation{}
			if annotation.URL != nil {
				converted.URLCitation.URL = *annotation.URL
			}
			if annotation.Title != nil {
				converted.URLCitation.Title = *annotation.Title
			}
		}
		result = append(result, converted)
	}
	return result
}

func convertToolsToInternal(tools []ResponsesTool) ([]model.Tool, error) {
	result := make([]model.Tool, 0, len(tools))

	for _, tool := range tools {
		switch tool.Type {
		case "function":
			params, err := json.Marshal(tool.Parameters)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal function parameters: %w", err)
			}

			result = append(result, model.Tool{
				Type: "function",
				Function: model.Function{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  params,
					Strict:      tool.Strict,
				},
			})

		case "image_generation":
			result = append(result, model.Tool{
				Type: "image_generation",
				ImageGeneration: &model.ImageGeneration{
					Background:        tool.Background,
					OutputFormat:      tool.OutputFormat,
					Quality:           tool.Quality,
					Size:              tool.Size,
					OutputCompression: tool.OutputCompression,
				},
			})

		case "web_search":
			webSearch := &model.WebSearch{}
			if tool.Filters != nil {
				webSearch.AllowedDomains = append(webSearch.AllowedDomains, tool.Filters.AllowedDomains...)
			}
			if tool.UserLocation != nil {
				locationType := tool.UserLocation.Type
				if locationType == "" {
					locationType = "approximate"
				}
				webSearch.UserLocation = model.WebSearchToolUserLocation{
					Type: locationType, City: tool.UserLocation.City,
					Country: tool.UserLocation.Country, Region: tool.UserLocation.Region,
					Timezone: tool.UserLocation.Timezone,
				}
			}
			result = append(result, model.Tool{Type: "web_search", WebSearch: webSearch})

		case "custom":
			custom := &model.ResponseCustomTool{Name: tool.Name, Description: tool.Description}
			if tool.Format != nil {
				custom.Format = &model.ResponseCustomToolFormat{
					Type: tool.Format.Type, Syntax: tool.Format.Syntax, Definition: tool.Format.Definition,
				}
			}
			result = append(result, model.Tool{Type: "responses_custom_tool", ResponseCustomTool: custom})

		case "namespace":
			for _, subTool := range tool.Tools {
				if subTool.Type != "function" {
					continue
				}
				params, err := json.Marshal(subTool.Parameters)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal namespace tool parameters: %w", err)
				}
				result = append(result, model.Tool{
					Type: "function",
					Function: model.Function{
						Name:        namespaceFunctionName(tool.Name, subTool.Name),
						Description: subTool.Description,
						Parameters:  params,
						Strict:      subTool.Strict,
					},
				})
			}
		}
	}

	return result, nil
}

func convertToResponsesAPIResponse(resp *model.InternalLLMResponse) *ResponsesResponse {
	result := &ResponsesResponse{
		Object:             "response",
		ID:                 resp.ID,
		Model:              resp.Model,
		CreatedAt:          resp.Created,
		PreviousResponseID: cloneResponseStringPtr(resp.PreviousResponseID),
		Output:             make([]ResponsesItem, 0),
		Status:             lo.ToPtr("completed"),
	}

	// Convert usage
	result.Usage = convertUsageToResponses(resp.Usage)

	// Convert choices to output items
	for _, choice := range resp.Choices {
		var message *model.Message
		if choice.Message != nil {
			message = choice.Message
		} else if choice.Delta != nil {
			message = choice.Delta
		}

		if message == nil {
			continue
		}
		messageID := message.ID
		if messageID == "" {
			messageID = generateItemID()
		}

		// Keep each reasoning item separate. A single scalar reasoning field is
		// retained only as a compatibility fallback for older providers.
		result.Output = append(result.Output, buildResponsesReasoningItems(*message)...)

		// Handle function and custom tool calls.
		if len(message.ToolCalls) > 0 {
			for _, toolCall := range message.ToolCalls {
				if toolCall.ResponseCustomToolCall != nil {
					callID := toolCall.ResponseCustomToolCall.CallID
					itemID := toolCall.ID
					if itemID == "" {
						itemID = callID
					}
					result.Output = append(result.Output, ResponsesItem{
						ID:     itemID,
						Type:   "custom_tool_call",
						CallID: callID,
						Name:   toolCall.ResponseCustomToolCall.Name,
						Input:  lo.ToPtr(toolCall.ResponseCustomToolCall.Input),
						Status: lo.ToPtr("completed"),
					})
					continue
				}

				result.Output = append(result.Output, ResponsesItem{
					ID:        toolCall.ID,
					Type:      "function_call",
					CallID:    toolCall.ID,
					Name:      toolCall.Function.Name,
					Namespace: toolCall.Function.Namespace,
					Arguments: toolCall.Function.Arguments,
					Status:    lo.ToPtr("completed"),
				})
			}
		}

		// Anthropic server-side tool results can be carried inline on the
		// assistant message. Responses has no generic inline-result item, so
		// represent them as function_call_output while retaining their call id
		// and textual output.
		for _, inline := range message.InlineToolResults {
			result.Output = append(result.Output, responsesItemFromInlineToolResult(inline))
		}

		// Handle message content
		contentItems := make([]ResponsesItem, 0, 2)
		if message.Content.Content != nil && *message.Content.Content != "" {
			text := *message.Content.Content
			contentItems = append(contentItems, ResponsesItem{
				Type:        "output_text",
				Text:        &text,
				Annotations: responsesAnnotations(message.Annotations),
			})
		} else if len(message.Content.MultipleContent) > 0 {

			for _, part := range message.Content.MultipleContent {
				switch part.Type {
				case "text":
					if part.Text != nil {
						text := *part.Text
						contentItems = append(contentItems, ResponsesItem{
							Type:        "output_text",
							Text:        &text,
							Annotations: responsesAnnotations(nil),
						})
					}
				case "image_url":
					if part.ImageURL != nil {
						result.Output = append(result.Output, ResponsesItem{
							ID:           generateItemID(),
							Type:         "image_generation_call",
							Role:         "assistant",
							Result:       lo.ToPtr(xurl.ExtractBase64FromDataURL(part.ImageURL.URL)),
							Status:       lo.ToPtr("completed"),
							Background:   responsesMetadataString(part.TransformerMetadata, "background"),
							OutputFormat: responsesMetadataString(part.TransformerMetadata, "output_format"),
							Quality:      responsesMetadataString(part.TransformerMetadata, "quality"),
							Size:         responsesMetadataString(part.TransformerMetadata, "size"),
						})
					}
				case "compaction", "compaction_summary":
					if part.Compact != nil {
						result.Output = append(result.Output, responsesCompactionItem(part, part.Type))
					}
				}
			}
		}

		if message.Refusal != "" {
			refusal := message.Refusal
			contentItems = append(contentItems, ResponsesItem{
				Type:    "refusal",
				Refusal: &refusal,
			})
		}

		if len(contentItems) > 0 || message.Audio != nil {
			attachResponsesAnnotations(contentItems, message.Annotations)
			messageItem := ResponsesItem{
				ID:      messageID,
				Type:    "message",
				Role:    "assistant",
				Content: &ResponsesInput{Items: contentItems},
				Status:  lo.ToPtr("completed"),
			}
			if message.Audio != nil {
				messageItem.Audio = &ResponsesOutputAudio{
					ID: message.Audio.ID, Data: message.Audio.Data,
					ExpiresAt: message.Audio.ExpiresAt, Transcript: message.Audio.Transcript,
				}
			}
			result.Output = append(result.Output, messageItem)
		}

		// Set status based on finish reason
		if choice.FinishReason != nil {
			_, status := responsesTerminalEvent(*choice.FinishReason)
			result.Status = lo.ToPtr(status)
		}
	}

	// If no output items, create empty message
	if len(result.Output) == 0 {
		emptyText := ""
		result.Output = []ResponsesItem{
			{
				ID:   generateItemID(),
				Type: "message",
				Role: "assistant",
				Content: &ResponsesInput{
					Items: []ResponsesItem{
						{
							Type: "output_text",
							Text: &emptyText,
						},
					},
				},
				Status: lo.ToPtr("completed"),
			},
		}
	}

	return result
}

func buildResponsesReasoningItems(message model.Message) []ResponsesItem {
	reasoningItems := message.ReasoningItems
	if len(reasoningItems) == 0 {
		var content string
		if message.ReasoningContent != nil {
			content = *message.ReasoningContent
		} else if message.Reasoning != nil {
			content = *message.Reasoning
		}
		var signature string
		if message.ReasoningSignature != nil {
			signature = *message.ReasoningSignature
		}
		if content != "" || signature != "" {
			reasoningItems = []model.ReasoningItem{{Content: content, Signature: signature}}
		}
	}

	result := make([]ResponsesItem, 0, len(reasoningItems))
	for _, reasoning := range reasoningItems {
		if reasoning.Content == "" && reasoning.Signature == "" {
			continue
		}
		itemID := reasoning.ID
		if itemID == "" {
			itemID = generateItemID()
		}
		item := ResponsesItem{
			ID:      itemID,
			Type:    "reasoning",
			Status:  lo.ToPtr("completed"),
			Summary: []ResponsesReasoningSummary{},
		}
		if reasoning.Content != "" {
			item.Summary = append(item.Summary, ResponsesReasoningSummary{
				Type: "summary_text",
				Text: reasoning.Content,
			})
		}
		if reasoning.Signature != "" {
			item.EncryptedContent = lo.ToPtr(reasoning.Signature)
		}
		result = append(result, item)
	}
	return result
}

func responsesAnnotations(annotations []model.Annotation) *[]ResponsesAnnotation {
	result := make([]ResponsesAnnotation, 0, len(annotations))
	for _, annotation := range annotations {
		converted := ResponsesAnnotation{
			Type:       annotation.Type,
			StartIndex: annotation.StartIndex,
			EndIndex:   annotation.EndIndex,
		}
		if annotation.URLCitation != nil {
			converted.URL = lo.ToPtr(annotation.URLCitation.URL)
			converted.Title = lo.ToPtr(annotation.URLCitation.Title)
		}
		result = append(result, converted)
	}
	return &result
}

func attachResponsesAnnotations(items []ResponsesItem, annotations []model.Annotation) {
	if len(annotations) == 0 {
		return
	}
	for index := range items {
		if items[index].Type == "output_text" || items[index].Type == "input_text" || items[index].Type == "text" {
			items[index].Annotations = responsesAnnotations(annotations)
			return
		}
	}
}

func responsesCompactionItem(part model.MessageContentPart, contentType string) ResponsesItem {
	item := ResponsesItem{Type: contentType}
	if part.Compact == nil {
		return item
	}
	item.ID = part.Compact.ID
	item.EncryptedContent = lo.ToPtr(part.Compact.EncryptedContent)
	item.CreatedBy = part.Compact.CreatedBy
	return item
}

func responsesItemFromInlineToolResult(result model.InlineToolResult) ResponsesItem {
	itemType := "function_call_output"
	if result.TransformerMetadata != nil {
		if value, ok := result.TransformerMetadata["responses_type"].(string); ok {
			switch value {
			case "function_call_output", "custom_tool_call_output":
				itemType = value
			}
		}
	}
	output := result.Output
	return ResponsesItem{
		Type:   itemType,
		CallID: result.ToolCallID,
		Output: &ResponsesInput{Text: &output},
		Status: lo.ToPtr("completed"),
	}
}

func responsesMetadataString(metadata map[string]any, key string) *string {
	if metadata == nil {
		return nil
	}
	switch value := metadata[key].(type) {
	case string:
		if value != "" {
			return lo.ToPtr(value)
		}
	case *string:
		if value != nil && *value != "" {
			return lo.ToPtr(*value)
		}
	}
	return nil
}

func cloneResponseStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func convertUsageToResponses(usage *model.Usage) *ResponsesUsage {
	if usage == nil {
		return nil
	}

	inputTokens := usage.PromptTokens
	cachedTokens := int64(0)
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	if usage.HasAnthropicCacheSemantic() {
		// Anthropic stored PromptTokens as non-cached; add cache read/create back so
		// OpenAI clients see the conventional total input count.
		inputTokens += usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		if cachedTokens == 0 && usage.CacheReadInputTokens > 0 {
			cachedTokens = usage.CacheReadInputTokens
		}
	}

	result := &ResponsesUsage{
		InputTokens:  inputTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}

	if cachedTokens > 0 {
		result.InputTokenDetails.CachedTokens = cachedTokens
	}

	if usage.CompletionTokensDetails != nil {
		result.OutputTokenDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}

	return result
}

func generateItemID() string {
	return fmt.Sprintf("item_%s", lo.RandomString(16, lo.AlphanumericCharset))
}

// validateReasoningEffort whitelists the values OpenAI's Responses API
// accepts for `reasoning.effort`. Unknown inputs are dropped (empty
// return) so the upstream schema validator never sees garbage; callers
// fall back to the provider default.
func validateReasoningEffort(effort string) string {
	switch effort {
	case "minimal", "low", "medium", "high":
		return effort
	case "":
		return ""
	default:
		return ""
	}
}

// validateReasoningSummary whitelists the values OpenAI's Responses API
// accepts for `reasoning.summary`. Unknown inputs are dropped.
func validateReasoningSummary(summary string) string {
	switch summary {
	case "auto", "concise", "detailed":
		return summary
	case "":
		return ""
	default:
		return ""
	}
}

// responsesTerminalEvent picks the correct terminal stream event + status
// pair based on the canonical FinishReason (see model/finishreason.go).
// Length-truncated or paused turns map to response.incomplete; safety /
// refusal / error-class stops map to response.failed; everything else is
// the normal response.completed.
func responsesTerminalEvent(finishReason string) (eventType string, status string) {
	r := model.ParseFinishReason(finishReason)
	switch {
	case r.IsZero():
		return "response.completed", "completed"
	case r == model.FinishReasonLength || r == model.FinishReasonPauseTurn:
		return "response.incomplete", "incomplete"
	case r == model.FinishReasonError || r == model.FinishReasonMalformedCall:
		return "response.failed", "failed"
	case r.IsSafetyBlock():
		return "response.failed", "failed"
	default:
		return "response.completed", "completed"
	}
}
