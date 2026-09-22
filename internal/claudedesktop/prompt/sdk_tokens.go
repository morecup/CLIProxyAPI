package prompt

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// SDKTokenEstimate is the native local estimate, not a tokenizer result or the
// server's billable usage. Unknown estimates must not drive a compact decision.
type SDKTokenEstimate struct {
	Tokens int64 `json:"tokens"`
	Known  bool  `json:"known"`
}

// SDKTokenUsage is a response-local usage anchor, never a session aggregate.
type SDKTokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u SDKTokenUsage) total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

func sdkTextUnits(s string) int64 {
	var units int64
	for _, r := range s {
		units++
		if r > 0xffff {
			units++
		}
	}
	return units
}

func sdkRoundedTokens(units int64) int64 { return (units + 2) / 4 }

// EstimateSDKContent follows v140609 mV/he/Re with its default divisor of four.
// Text blocks are rounded separately. Images/documents have a native fixed
// estimate; tool results recurse and tool calls estimate name + JSON.stringify.
// Only transient decoded values are allocated; no content is retained.
func EstimateSDKContent(content json.RawMessage) SDKTokenEstimate {
	if len(content) == 0 || !json.Valid(content) {
		return SDKTokenEstimate{}
	}
	tokens, known := sdkContentTokens(content, 0)
	return SDKTokenEstimate{Tokens: tokens, Known: known}
}

func sdkContentTokens(raw json.RawMessage, depth int) (int64, bool) {
	if depth > 128 {
		return 0, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, true
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		return sdkRoundedTokens(sdkTextUnits(text)), true
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return 0, false
	}
	var total int64
	for _, block := range blocks {
		n, ok := sdkBlockTokens(block, depth+1)
		if !ok {
			return 0, false
		}
		total += n
	}
	return total, true
}

func sdkBlockTokens(raw json.RawMessage, depth int) (int64, bool) {
	if depth > 128 {
		return 0, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	if raw[0] == '"' {
		return sdkContentTokens(raw, depth+1)
	}
	var block map[string]json.RawMessage
	if json.Unmarshal(raw, &block) != nil || block == nil {
		return 0, false
	}
	var kind string
	_ = json.Unmarshal(block["type"], &kind)
	field := ""
	switch kind {
	case "text":
		field = "text"
	case "thinking":
		field = "thinking"
	case "redacted_thinking":
		field = "data"
	case "image", "document":
		return 2000, true
	case "tool_result":
		return sdkContentTokens(block["content"], depth+1)
	case "tool_use":
		var name string
		if json.Unmarshal(block["name"], &name) != nil {
			return 0, false
		}
		input := block["input"]
		if len(input) == 0 || bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
			input = json.RawMessage(`{}`)
		}
		n, ok := sdkJSONUnits(input, depth+1)
		return sdkRoundedTokens(sdkTextUnits(name) + n), ok
	default:
		n, ok := sdkJSONUnits(raw, depth+1)
		return sdkRoundedTokens(n), ok
	}
	var text string
	// The native string estimator returns zero for absent/non-string fields.
	_ = json.Unmarshal(block[field], &text)
	return sdkRoundedTokens(sdkTextUnits(text)), true
}

// Only the serialized length is needed. Property ordering cannot affect it;
// decoding objects to raw maps also implements JSON.parse's last duplicate key.
// Strings use JSON.stringify escaping, not Go's HTML/line-separator escaping.
func sdkJSONUnits(raw json.RawMessage, depth int) (int64, bool) {
	if depth > 128 {
		return 0, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	switch raw[0] {
	case '"':
		return sdkJSONStringUnits(raw)
	case '{':
		// Raw keys preserve distinct lone surrogates. Go's ordinary map decoder
		// would replace all of them with U+FFFD and merge unrelated properties.
		type property struct {
			keyUnits int64
			value    json.RawMessage
		}
		object := make(map[string]property)
		valid := true
		gjson.ParseBytes(raw).ForEach(func(key, value gjson.Result) bool {
			id, ok := sdkJSONKeyIdentity([]byte(key.Raw))
			n, okLength := sdkJSONStringUnits([]byte(key.Raw))
			if !ok || !okLength {
				valid = false
				return false
			}
			object[id] = property{keyUnits: n, value: json.RawMessage(value.Raw)}
			return true
		})
		if !valid {
			return 0, false
		}
		total := int64(2)
		for _, value := range object {
			n, ok := sdkJSONUnits(value.value, depth+1)
			if !ok {
				return 0, false
			}
			total += value.keyUnits + n + 2
		}
		if len(object) > 0 {
			total--
		}
		return total, true
	case '[':
		var array []json.RawMessage
		if json.Unmarshal(raw, &array) != nil {
			return 0, false
		}
		total := int64(2)
		for _, value := range array {
			n, ok := sdkJSONUnits(value, depth+1)
			if !ok {
				return 0, false
			}
			total += n + 1
		}
		if len(array) > 0 {
			total--
		}
		return total, true
	case 't':
		return 4, string(raw) == "true"
	case 'f':
		return 5, string(raw) == "false"
	case 'n':
		return 4, string(raw) == "null"
	default:
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil && !math.IsInf(number, 0) {
			return 0, false
		}
		if math.IsInf(number, 0) || math.IsNaN(number) {
			return 4, true
		}
		if number == 0 {
			return 1, true
		}
		format := byte('f')
		if math.Abs(number) < 1e-6 || math.Abs(number) >= 1e21 {
			format = 'e'
		}
		s := strconv.FormatFloat(number, format, -1, 64)
		s = strings.ReplaceAll(strings.ReplaceAll(s, "e-0", "e-"), "e+0", "e+")
		return int64(len(s)), true
	}
}

// Canonical UTF-16 keys implement JSON.parse's last-key-wins behavior even
// when the same key uses different escapes, or contains lone surrogates.
func sdkJSONKeyIdentity(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	var key []byte
	appendUnit := func(unit uint16) { key = append(key, byte(unit>>8), byte(unit)) }
	for i := 1; i < len(raw)-1; {
		if raw[i] == '\\' {
			i++
			if i >= len(raw)-1 {
				return "", false
			}
			var unit uint16
			switch raw[i] {
			case 'u':
				if i+4 >= len(raw)-1 {
					return "", false
				}
				n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
				if err != nil {
					return "", false
				}
				unit = uint16(n)
				i += 4
			case 'b':
				unit = '\b'
			case 'f':
				unit = '\f'
			case 'n':
				unit = '\n'
			case 'r':
				unit = '\r'
			case 't':
				unit = '\t'
			case '"', '\\', '/':
				unit = uint16(raw[i])
			default:
				return "", false
			}
			appendUnit(unit)
			i++
			continue
		}
		r, n := utf8.DecodeRune(raw[i:])
		if n == 0 {
			return "", false
		}
		i += n
		if r > 0xffff {
			a, b := utf16.EncodeRune(r)
			appendUnit(uint16(a))
			appendUnit(uint16(b))
		} else {
			appendUnit(uint16(r))
		}
	}
	return string(key), true
}

func sdkJSONStringUnits(raw json.RawMessage) (int64, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	n := int64(2)
	for _, r := range text {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			n += 2
		default:
			if r < 0x20 {
				n += 6
			} else if r > 0xffff {
				n += 2
			} else {
				n++
			}
		}
	}
	// Well-formed JSON.stringify escapes lone UTF-16 surrogates. Go replaces
	// them with U+FFFD, so account for those five extra code units explicitly.
	for i := 1; i+1 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' || i+4 >= len(raw) {
			continue
		}
		v, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return 0, false
		}
		i += 4
		if v < 0xd800 || v > 0xdfff {
			continue
		}
		if v <= 0xdbff && i+6 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
			w, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err == nil && utf16.IsSurrogate(rune(w)) && w >= 0xdc00 {
				i += 6
				continue
			}
		}
		n += 5
	}
	return n, true
}

// EstimateSDKHistory follows kp/JYn: the last eligible usage wins, but its
// anchor rewinds over every earlier yield sharing the same response ID until
// another real assistant ID is encountered. The tail is estimated separately.
func EstimateSDKHistory(messages []SDKHistoryMessage) SDKTokenEstimate {
	start, total := 0, int64(0)
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Type != "assistant" || m.Synthetic || m.UsageExcluded || !m.UsageKnown {
			continue
		}
		anchor := i
		if m.MessageID != "" {
			for j := i - 1; j >= 0; j-- {
				other := messages[j]
				if other.Type != "assistant" || other.Synthetic || other.MessageID == "" {
					continue
				}
				if other.MessageID != m.MessageID {
					break
				}
				anchor = j
			}
		}
		start, total = anchor+1, m.Usage.total()
		break
	}
	for _, m := range messages[start:] {
		switch m.Type {
		case "assistant", "user", "api_system", "attachment":
			if !m.TokenEstimate.Known {
				return SDKTokenEstimate{}
			}
			total += m.TokenEstimate.Tokens
		}
	}
	return SDKTokenEstimate{Tokens: total, Known: true}
}
