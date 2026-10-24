package anthropic

import (
	"encoding/json"
	"strings"
)

// Server tool payloads include result records and error objects which are
// not ordinary message content. Preserve them verbatim on protocol replay.
func (b *MessageContentBlock) UnmarshalJSON(data []byte) error {
	type alias MessageContentBlock
	var decoded alias
	wire := struct {
		*alias
		Content json.RawMessage `json:"content"`
	}{alias: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*b = MessageContentBlock(decoded)
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	b.Content = &MessageContent{}
	if strings.HasSuffix(b.Type, "_tool_result") {
		b.Content.Raw = append(json.RawMessage(nil), wire.Content...)
		return nil
	}
	return json.Unmarshal(wire.Content, b.Content)
}
