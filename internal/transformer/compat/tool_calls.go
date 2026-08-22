package compat

import (
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// PatchAnthropicRequest applies small protocol repairs before Anthropic wire
// conversion. It is intentionally narrow: only fixes cases known to trigger
// strict Anthropic schema errors.
func PatchAnthropicRequest(req *model.InternalLLMRequest) {
	if req == nil || len(req.Messages) == 0 {
		return
	}
	req.Messages = FixOrphanedToolCalls(req.Messages)
}

// FixOrphanedToolCalls inserts empty tool_result messages for assistant
// tool_use blocks that are not answered before the next assistant turn.
func FixOrphanedToolCalls(messages []model.Message) []model.Message {
	if len(messages) == 0 {
		return messages
	}

	// AxonHub groups an Anthropic tool_result with the following user content
	// through MessageIndex. When we synthesize a missing result, associate it
	// with the next user turn (if one exists) so the repair remains one
	// provider-visible user message instead of creating consecutive user turns.
	working := append([]model.Message(nil), messages...)
	nextMessageIndex := nextSyntheticMessageIndex(working)
	out := make([]model.Message, 0, len(messages))
	for i := 0; i < len(working); i++ {
		msg := working[i]
		out = append(out, msg)
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}

		answered := answeredToolCallIDsBeforeNextAssistant(working, i+1)
		associationIndex := syntheticToolResultMessageIndex(working, i+1, &nextMessageIndex)
		for _, toolCall := range msg.ToolCalls {
			id := strings.TrimSpace(toolCall.ID)
			if id == "" {
				continue
			}
			if _, ok := answered[id]; ok {
				continue
			}
			synthetic := emptyToolResult(toolCall)
			if associationIndex != nil {
				index := *associationIndex
				synthetic.MessageIndex = &index
			}
			out = append(out, synthetic)
		}
	}
	return out
}

func nextSyntheticMessageIndex(messages []model.Message) int {
	next := 0
	for _, msg := range messages {
		if msg.MessageIndex != nil && *msg.MessageIndex >= next {
			next = *msg.MessageIndex + 1
		}
	}
	return next
}

func syntheticToolResultMessageIndex(messages []model.Message, start int, next *int) *int {
	for i := start; i < len(messages); i++ {
		if messages[i].Role == "assistant" {
			return nil
		}
		if messages[i].Role != "user" {
			continue
		}
		if messages[i].MessageIndex == nil {
			index := *next
			(*next)++
			messages[i].MessageIndex = &index
		}
		return messages[i].MessageIndex
	}
	return nil
}

func answeredToolCallIDsBeforeNextAssistant(messages []model.Message, start int) map[string]struct{} {
	answered := make(map[string]struct{})
	for i := start; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == "assistant" {
			break
		}
		if msg.Role != "tool" || msg.ToolCallID == nil {
			continue
		}
		id := strings.TrimSpace(*msg.ToolCallID)
		if id != "" {
			answered[id] = struct{}{}
		}
	}
	return answered
}

func emptyToolResult(toolCall model.ToolCall) model.Message {
	id := toolCall.ID
	name := toolCall.Function.Name
	content := ""
	return model.Message{
		Role:         "tool",
		Content:      model.MessageContent{Content: &content},
		ToolCallID:   &id,
		ToolCallName: &name,
	}
}
