package prompt

import (
	"encoding/json"
	"strings"
)

// MergeSDKWireUserContent implements the flag-independent roo/AQs cases.
// The two native feature branches agree for ordinary text, polling results,
// result-only appends and string-result text folding. Other media/result folds
// need an owned feature decision and are not silently assigned a default.
func MergeSDKWireUserContent(left, right json.RawMessage) (json.RawMessage, error) {
	blocks := func(raw json.RawMessage) ([]json.RawMessage, error) {
		if text, known := sdkWireString(raw); known {
			block, err := sdkAttachmentJSON(map[string]string{"type": "text", "text": text})
			return []json.RawMessage{block}, err
		}
		var result []json.RawMessage
		if json.Unmarshal(raw, &result) != nil || result == nil {
			return nil, ErrSDKCompactionContentUnknown
		}
		return result, nil
	}
	l, err := blocks(left)
	if err != nil {
		return nil, err
	}
	r, err := blocks(right)
	if err != nil {
		return nil, err
	}
	if len(l) == 0 || len(r) == 0 {
		return sdkAttachmentJSON(append(l, r...))
	}
	tail, err := sdkAttachmentObject(l[len(l)-1])
	if err != nil {
		return nil, err
	}
	appendOnly := sdkRestorationString(tail, "type") != "tool_result" || strings.HasPrefix(sdkRestorationString(tail, "tool_use_id"), "poll_")
	allText, allResults := true, true
	var texts []string
	for _, raw := range r {
		block, err := sdkAttachmentObject(raw)
		if err != nil {
			return nil, err
		}
		kind := sdkRestorationString(block, "type")
		allResults = allResults && kind == "tool_result"
		allText = allText && kind == "text"
		if kind == "text" {
			text, known := sdkWireString(block["text"])
			if !known {
				return nil, ErrSDKCompactionContentUnknown
			}
			if strings.HasPrefix(text, "<system-reminder>\n") && sdkWirePollEvent(text[18:]) {
				appendOnly = true
			}
			texts = append(texts, text)
		}
	}
	if appendOnly || allResults {
		return sdkAttachmentJSON(append(l, r...))
	}
	previous, stringContent := sdkWireString(tail["content"])
	if !allText || !stringContent || sdkWirePollEvent(previous) {
		return nil, ErrSDKCompactionContentUnknown
	}
	var nonempty []string
	for _, value := range append([]string{previous}, texts...) {
		if value = sdkWireTrim(value); value != "" {
			nonempty = append(nonempty, value)
		}
	}
	tail["content"], _ = sdkAttachmentJSON(strings.Join(nonempty, "\n\n"))
	l[len(l)-1], err = sdkAttachmentJSON(tail)
	if err != nil {
		return nil, err
	}
	return sdkAttachmentJSON(l)
}

func sdkWirePollEvent(text string) bool {
	return strings.HasPrefix(text, "<system>authentic event nonces for this delivery: ") || strings.HasPrefix(text, "<event ")
}

func sdkWireTrim(text string) string {
	return strings.TrimFunc(text, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return false
	})
}
