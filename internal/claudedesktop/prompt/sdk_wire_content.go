package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Wire identities describe observed content, not a reconstruction of native
// tool execution or schema coercion. Only content-block cache decoration is
// excluded. In particular, a tool argument named cache_control is significant.
// Plain text retains the previous durable fingerprint representation.
func sdkWireMessageFingerprint(role string, content json.RawMessage) (string, bool) {
	if role != "user" && role != "assistant" || !json.Valid(content) || len(content) > maxSDKCompactionViewBytes {
		return "", false
	}
	blocks, ok := sdkWireContentBlocks(content)
	if !ok {
		return "", false
	}
	plain := true
	for _, block := range blocks {
		plain = plain && sdkWirePlainText(block)
	}
	if plain {
		return sdkTextMessageFingerprint(role, content)
	}
	h := sha256.New()
	_, _ = h.Write([]byte("sdk-wire-content-v1\x00" + role + "\x00"))
	for _, block := range blocks {
		if !sdkWireHashJSON(h, block, 0) {
			return "", false
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func sdkWirePlainText(raw json.RawMessage) bool {
	var block map[string]json.RawMessage
	if json.Unmarshal(raw, &block) != nil || len(block) != 2 {
		return false
	}
	if kind, known := sdkWireString(block["type"]); !known || kind != "text" {
		return false
	}
	_, ok := sdkWireString(block["text"])
	return ok
}

// Go replaces unpaired UTF-16 surrogates when decoding strings. Do not turn
// that lossy conversion into proof that two histories are identical.
func sdkWireString(raw json.RawMessage) (string, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	encoded, _ := json.Marshal(value)
	want, known := sdkJSONKeyIdentity(raw)
	actual, ok := sdkJSONKeyIdentity(encoded)
	return value, known && ok && want == actual
}

func sdkWireContentBlocks(raw json.RawMessage) ([]json.RawMessage, bool) {
	return sdkWireContentBlocksAt(raw, 0)
}

func sdkWireContentBlocksAt(raw json.RawMessage, depth int) ([]json.RawMessage, bool) {
	if depth > 128 {
		return nil, false
	}
	if text, ok := sdkWireString(raw); ok {
		block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		return []json.RawMessage{block}, true
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return nil, false
	}
	result := make([]json.RawMessage, 0, len(blocks))
	var textRun strings.Builder
	hasTextRun := false
	flushText := func() {
		if !hasTextRun {
			return
		}
		encoded, _ := json.Marshal(map[string]string{"type": "text", "text": textRun.String()})
		result = append(result, encoded)
		textRun.Reset()
		hasTextRun = false
	}
	for _, rawBlock := range blocks {
		var block map[string]json.RawMessage
		if json.Unmarshal(rawBlock, &block) != nil || block == nil {
			return nil, false
		}
		kind, known := sdkWireString(block["type"])
		if !known || kind == "" {
			return nil, false
		}
		// Validate property identities before the Go map can collapse them.
		validKeys := true
		gjson.ParseBytes(rawBlock).ForEach(func(key, _ gjson.Result) bool {
			_, validKeys = sdkWireString([]byte(key.Raw))
			return validKeys
		})
		if !validKeys {
			return nil, false
		}
		delete(block, "cache_control")
		if kind == "tool_result" && len(block["content"]) != 0 {
			// Nested result content is a content container, never tool input.
			nested, ok := sdkWireContentBlocksAt(block["content"], depth+1)
			if !ok {
				return nil, false
			}
			block["content"], _ = json.Marshal(nested)
		}
		encoded, err := json.Marshal(block)
		if err != nil {
			return nil, false
		}
		if sdkWirePlainText(encoded) {
			value, _ := sdkWireString(block["text"])
			textRun.WriteString(value)
			hasTextRun = true
		} else {
			flushText()
			result = append(result, encoded)
		}
	}
	flushText()
	return result, true
}

// Length-framed, typed canonical hashing preserves array order, every object
// field, last-duplicate-key JSON semantics, and exact UTF-16 string identity.
// It does not serialize or retain content in the session checkpoint.
func sdkWireHashJSON(h hash.Hash, raw json.RawMessage, depth int) bool {
	if depth > 128 || !json.Valid(raw) {
		return false
	}
	frame := func(kind, value string) {
		_, _ = h.Write([]byte(kind + strconv.Itoa(len(value)) + ":" + value))
	}
	raw = bytes.TrimSpace(raw)
	switch raw[0] {
	case '"':
		value, ok := sdkJSONKeyIdentity(raw)
		if !ok {
			return false
		}
		frame("s", value)
	case '{':
		object := make(map[string]json.RawMessage)
		valid := true
		gjson.ParseBytes(raw).ForEach(func(key, value gjson.Result) bool {
			id, ok := sdkJSONKeyIdentity([]byte(key.Raw))
			if !ok {
				valid = false
				return false
			}
			object[id] = json.RawMessage(value.Raw)
			return true
		})
		if !valid {
			return false
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		frame("o", strconv.Itoa(len(keys)))
		for _, key := range keys {
			frame("k", key)
			if !sdkWireHashJSON(h, object[key], depth+1) {
				return false
			}
		}
	case '[':
		var array []json.RawMessage
		if json.Unmarshal(raw, &array) != nil {
			return false
		}
		frame("a", strconv.Itoa(len(array)))
		for _, value := range array {
			if !sdkWireHashJSON(h, value, depth+1) {
				return false
			}
		}
	case 't', 'f', 'n':
		frame("v", string(raw))
	default:
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return false
		}
		if number == 0 {
			number = 0 // JSON.stringify normalizes negative zero.
		}
		frame("d", strconv.FormatFloat(number, 'g', -1, 64))
	}
	return true
}

func sdkWireToolResults(content json.RawMessage) ([]json.RawMessage, bool) {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return nil, false
	}
	for _, block := range blocks {
		var value struct{ Type string }
		if json.Unmarshal(block, &value) != nil || value.Type != "tool_result" {
			return nil, false
		}
	}
	return blocks, true
}
