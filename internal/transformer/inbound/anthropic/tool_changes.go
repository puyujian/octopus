package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// applyToolChanges projects the active tool set at the end of the history.
// The relay retains the original request bytes for native Anthropic replay.
func applyToolChanges(req *MessageRequest) error {
	catalog := make(map[string]Tool, len(req.Tools))
	for _, tool := range req.Tools {
		catalog[tool.Name] = tool
	}
	for messageIndex := range req.Messages {
		msg := &req.Messages[messageIndex]
		blocks := make([]MessageContentBlock, 0, len(msg.Content.MultipleContent))
		for _, block := range msg.Content.MultipleContent {
			if block.Type != "tool_addition" && block.Type != "tool_removal" {
				blocks = append(blocks, block)
				continue
			}
			if msg.Role != "system" {
				return fmt.Errorf("%s requires a system message", block.Type)
			}
			if block.Type == "tool_addition" {
				raw := block.Definition
				if len(raw) == 0 {
					raw = block.Tool
					var wrapper struct {
						Type       string          `json:"type"`
						Definition json.RawMessage `json:"definition"`
						Name       string          `json:"name"`
					}
					if err := json.Unmarshal(raw, &wrapper); err != nil {
						return fmt.Errorf("invalid tool_addition tool: %w", err)
					}
					if wrapper.Type == "tool_reference" {
						if referenced, ok := catalog[wrapper.Name]; ok {
							if !hasTool(req.Tools, wrapper.Name) {
								req.Tools = append(req.Tools, referenced)
							}
							continue
						}
						return fmt.Errorf("tool_addition references unknown tool %q", wrapper.Name)
					}
					if len(wrapper.Definition) > 0 {
						raw = wrapper.Definition
					} else if wrapper.Type == "tool_definition" {
						return fmt.Errorf("tool_addition tool_definition requires a definition")
					}
				}
				var tool Tool
				if err := json.Unmarshal(raw, &tool); err != nil {
					return fmt.Errorf("invalid tool_addition definition: %w", err)
				}
				if strings.TrimSpace(tool.Name) == "" {
					return fmt.Errorf("tool_addition requires a nonempty tool name")
				}
				if !tool.IsServerTool() {
					var schema map[string]json.RawMessage
					if json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
						return fmt.Errorf("tool_addition %q requires an input_schema object", tool.Name)
					}
				}
				if !hasTool(req.Tools, tool.Name) {
					req.Tools = append(req.Tools, tool)
					catalog[tool.Name] = tool
				}
			} else {
				name := block.ToolName
				if name == "" && block.Name != nil {
					name = *block.Name
				}
				if name == "" && len(block.Tool) > 0 {
					if json.Unmarshal(block.Tool, &name) != nil {
						var reference struct {
							Name string `json:"name"`
						}
						if err := json.Unmarshal(block.Tool, &reference); err != nil {
							return fmt.Errorf("invalid tool_removal reference: %w", err)
						}
						name = reference.Name
					}
				}
				if strings.TrimSpace(name) == "" {
					return fmt.Errorf("tool_removal requires a nonempty tool name")
				}
				active := make([]Tool, 0, len(req.Tools))
				for _, tool := range req.Tools {
					if tool.Name != name {
						active = append(active, tool)
					}
				}
				req.Tools = active
			}
		}
		msg.Content.MultipleContent = blocks
	}
	return nil
}

func hasTool(tools []Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
