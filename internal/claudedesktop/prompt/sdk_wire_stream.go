package prompt

import (
	"bytes"
	"encoding/json"
)

// The pinned 2.1.247 query parser clears initial text/input/thinking, appends
// text/thinking/input deltas, replaces signatures, and ignores citation deltas.
// This bounded response-local assembler hashes the resulting wire content.
// No schema coercion, batch-tool decomposition or tool execution is invented.
type sdkWireStream struct {
	blocks  []*sdkWireStreamBlock
	bytes   int
	started bool
	invalid bool
}

type sdkWireStreamBlock struct {
	fields map[string]json.RawMessage
	value  []byte
	kind   string
	closed bool
}

func (s *sdkWireStream) invalidate() {
	s.invalid = true
	s.blocks = nil
}

func (s *sdkWireStream) reserve(n int) bool {
	if s.invalid || n < 0 || n > maxSDKCompactionViewBytes-s.bytes {
		s.invalidate()
		return false
	}
	s.bytes += n
	return true
}

func (s *sdkWireStream) begin(assistant bool) {
	if s.started || !assistant {
		s.invalidate()
	}
	s.started = true
}

func (s *sdkWireStream) start(index *int, raw json.RawMessage) {
	if !s.started || index == nil || *index != len(s.blocks) || *index >= 4096 || !s.reserve(len(raw)) {
		s.invalidate()
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		s.invalidate()
		return
	}
	kind, known := sdkWireString(fields["type"])
	if !known || kind == "" {
		s.invalidate()
		return
	}
	switch kind {
	case "tool_use", "server_tool_use":
		fields["input"] = json.RawMessage(`""`)
	case "text":
		fields["text"] = json.RawMessage(`""`)
	case "thinking":
		fields["thinking"], fields["signature"] = json.RawMessage(`""`), json.RawMessage(`""`)
	}
	s.blocks = append(s.blocks, &sdkWireStreamBlock{fields: fields, kind: kind})
}

func (s *sdkWireStream) open(index *int) *sdkWireStreamBlock {
	if s.invalid || index == nil || *index < 0 || *index >= len(s.blocks) || s.blocks[*index].closed {
		s.invalidate()
		return nil
	}
	return s.blocks[*index]
}

func (s *sdkWireStream) delta(index *int, raw json.RawMessage) {
	b := s.open(index)
	if b == nil || !s.reserve(len(raw)) {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		s.invalidate()
		return
	}
	kind, _ := sdkWireString(fields["type"])
	field, expected := "", ""
	switch kind {
	case "citations_delta":
		return
	case "thinking_delta":
		if b.kind == "redacted_thinking" {
			return
		}
		field, expected = "thinking", "thinking"
	case "signature_delta":
		field, expected = "signature", "thinking"
	case "text_delta":
		field, expected = "text", "text"
	case "input_json_delta":
		field, expected = "partial_json", "tool_use"
		if b.kind == "server_tool_use" {
			expected = b.kind
		}
	default:
		// The pinned parser has no default mutation for unknown delta types.
		return
	}
	value, known := sdkWireString(fields[field])
	if b.kind != expected || !known {
		s.invalidate()
		return
	}
	if kind == "signature_delta" {
		b.fields["signature"], _ = json.Marshal(value)
	} else {
		b.value = append(b.value, value...)
	}
}

func (s *sdkWireStream) close(index *int) {
	b := s.open(index)
	if b == nil {
		return
	}
	switch b.kind {
	case "tool_use", "server_tool_use":
		if len(b.value) == 0 || bytes.Equal(bytes.TrimSpace(b.value), []byte("null")) {
			b.fields["input"] = json.RawMessage(`{}`)
		} else if json.Valid(b.value) {
			b.fields["input"] = json.RawMessage(b.value)
		} else {
			// Native malformed-input sentinels require their own normalization;
			// a broken tool input cannot prove future caller content ownership.
			s.invalidate()
			return
		}
	case "text", "thinking":
		b.fields[b.kind], _ = json.Marshal(string(b.value))
	}
	b.value, b.closed = nil, true
}

func (s *sdkWireStream) finish() (string, bool) {
	defer func() { s.blocks = nil }()
	if s.invalid || !s.started || len(s.blocks) == 0 {
		return "", false
	}
	blocks := make([]map[string]json.RawMessage, 0, len(s.blocks))
	for _, b := range s.blocks {
		if !b.closed {
			return "", false
		}
		blocks = append(blocks, b.fields)
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return "", false
	}
	return sdkWireMessageFingerprint("assistant", content)
}
