package helps

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CollectClaudeMessageSSE restores native non-stream JSON when the Desktop
// profile requires streaming upstream (notably title generation). Existing
// message/block fields are retained; unknown incremental deltas fail visibly
// instead of silently returning an incomplete message.
func CollectClaudeMessageSSE(data []byte) ([]byte, error) {
	var message []byte
	started, complete := false, false
	blocks := make(map[int]bool)
	partial := make(map[int]*strings.Builder)
	set := func(key string, value any) error {
		var errSet error
		message, errSet = sjson.SetBytes(message, key, value)
		return errSet
	}
	setRaw := func(key, raw string) error {
		var errSet error
		message, errSet = sjson.SetRawBytes(message, key, []byte(raw))
		return errSet
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(payload) {
			return nil, fmt.Errorf("Claude stream has invalid event JSON")
		}
		root := gjson.ParseBytes(payload)
		typeName := root.Get("type").String()
		if typeName == "ping" {
			continue
		}
		if complete {
			return nil, fmt.Errorf("Claude stream has an event after message completion")
		}
		if typeName == "error" {
			return nil, fmt.Errorf("Claude stream contains an error event")
		}
		if typeName == "message_start" {
			if started || !root.Get("message").IsObject() || root.Get("message.type").String() != "message" {
				return nil, fmt.Errorf("Claude stream has an invalid message start")
			}
			message = []byte(root.Get("message").Raw)
			if content := root.Get("message.content"); !content.IsArray() || len(content.Array()) != 0 {
				return nil, fmt.Errorf("Claude stream starts with nonempty content")
			}
			started = true
			continue
		}
		if !started {
			return nil, fmt.Errorf("Claude stream has no message start")
		}
		indexValue := root.Get("index")
		index := int(indexValue.Int())
		base := "content." + strconv.Itoa(index)
		switch typeName {
		case "content_block_start", "content_block_delta", "content_block_stop":
			if indexValue.Type != gjson.Number || index < 0 || indexValue.Float() != float64(index) || index > len(blocks) {
				return nil, fmt.Errorf("Claude stream has an invalid content index")
			}
		}
		switch typeName {
		case "content_block_start":
			if _, exists := blocks[index]; exists || index != len(blocks) || !root.Get("content_block").IsObject() {
				return nil, fmt.Errorf("Claude stream has an invalid block start")
			}
			blocks[index] = false
			if err := setRaw(base, root.Get("content_block").Raw); err != nil {
				return nil, err
			}
		case "content_block_delta":
			done, exists := blocks[index]
			if !exists || done {
				return nil, fmt.Errorf("Claude stream delta has no open block")
			}
			delta := root.Get("delta")
			blockType := gjson.GetBytes(message, base+".type").String()
			field := ""
			switch delta.Get("type").String() {
			case "text_delta":
				if blockType != "text" {
					return nil, fmt.Errorf("Claude text delta targets another block type")
				}
				field = "text"
			case "thinking_delta":
				if blockType != "thinking" {
					return nil, fmt.Errorf("Claude thinking delta targets another block type")
				}
				field = "thinking"
			case "signature_delta":
				if blockType != "thinking" {
					return nil, fmt.Errorf("Claude signature delta targets another block type")
				}
				field = "signature"
			case "input_json_delta":
				if (blockType != "tool_use" && blockType != "server_tool_use") || delta.Get("partial_json").Type != gjson.String {
					return nil, fmt.Errorf("Claude stream has an invalid tool-input delta")
				}
				if partial[index] == nil {
					partial[index] = &strings.Builder{}
				}
				partial[index].WriteString(delta.Get("partial_json").String())
			case "citations_delta":
				if blockType != "text" || !delta.Get("citation").IsObject() {
					return nil, fmt.Errorf("Claude stream has an invalid citation")
				}
				if err := setRaw(base+".citations.-1", delta.Get("citation").Raw); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("Claude stream has an unsupported incremental delta")
			}
			if field != "" {
				value := delta.Get(field)
				if value.Type != gjson.String {
					return nil, fmt.Errorf("Claude stream has an invalid string delta")
				}
				if err := set(base+"."+field, gjson.GetBytes(message, base+"."+field).String()+value.String()); err != nil {
					return nil, err
				}
			}
		case "content_block_stop":
			done, exists := blocks[index]
			if !exists || done {
				return nil, fmt.Errorf("Claude stream stop has no open block")
			}
			if input := partial[index]; input != nil {
				if !gjson.Valid(input.String()) {
					return nil, fmt.Errorf("Claude stream has incomplete tool input")
				}
				if err := setRaw(base+".input", input.String()); err != nil {
					return nil, err
				}
				delete(partial, index)
			}
			blocks[index] = true
		case "message_delta":
			if !root.Get("delta").IsObject() || (root.Get("usage").Exists() && !root.Get("usage").IsObject()) {
				return nil, fmt.Errorf("Claude stream has an invalid message delta")
			}
			var errMerge error
			for _, property := range []string{"delta", "usage"} {
				root.Get(property).ForEach(func(key, value gjson.Result) bool {
					name := key.String()
					if property == "usage" {
						name = "usage." + name
					}
					errMerge = setRaw(name, value.Raw)
					return errMerge == nil
				})
				if errMerge != nil {
					return nil, errMerge
				}
			}
		case "message_stop":
			if gjson.GetBytes(message, "stop_reason").Type != gjson.String || gjson.GetBytes(message, "stop_reason").String() == "" {
				return nil, fmt.Errorf("Claude stream ended without a terminal reason")
			}
			for _, done := range blocks {
				if !done {
					return nil, fmt.Errorf("Claude stream ended with an open block")
				}
			}
			complete = true
		default:
			return nil, fmt.Errorf("Claude stream has an unsupported event")
		}
	}
	if !complete {
		return nil, fmt.Errorf("Claude stream ended before message completion")
	}
	return message, nil
}
