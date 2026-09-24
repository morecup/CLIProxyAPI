package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// These are observer memory bounds, not upstream message or summary limits.
const maxSDKCompactionTextBytes = 2 * 1024 * 1024

type SDKCompactionSummary struct {
	bytes    int
	sha256   string
	reviewed bool
}

// SDKCompactionInput retains only hashed result identities. The helper body
// is inspected transiently; no instructions, arguments or results are retained.
type SDKCompactionInput struct {
	toolResults map[string]struct{}
	known       bool
}

func ObserveSDKCompactionInput(body []byte) SDKCompactionInput {
	var input struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &input) != nil || len(input.Messages) == 0 {
		return SDKCompactionInput{}
	}
	result := SDKCompactionInput{toolResults: make(map[string]struct{}), known: true}
	for _, message := range input.Messages {
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			ID   string `json:"tool_use_id"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			return SDKCompactionInput{}
		}
		for _, block := range blocks {
			if block.Type != "tool_result" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" || len(result.toolResults) >= maxRequestsPerPrompt*maxToolsPerRequest {
				return SDKCompactionInput{}
			}
			result.toolResults[digest(id)] = struct{}{}
		}
	}
	return result
}

type SDKCompactionObservation struct {
	input   SDKCompactionInput
	summary SDKCompactionSummary
}

func CompletedSDKCompaction(input SDKCompactionInput, summary SDKCompactionSummary) SDKCompactionObservation {
	return SDKCompactionObservation{input: input, summary: summary}
}

type sdkCompactionBlock struct {
	kind   string
	closed bool
	text   []byte
}

// SDKCompactionResponse follows native S1e/w1e: prefer the last assistant with
// a literal summary tag, otherwise the last text-bearing assistant, then use
// its first text block. Buffered content belongs to one assistant; ordinary
// streamed closed blocks each yield a separate assistant. Text is never joined.
// Callers serialize observations and always call TakeSummary or Discard.
type SDKCompactionResponse struct {
	text                   []byte
	blocks                 []sdkCompactionBlock
	selected               int
	started, done, invalid bool
	stopReason             string
	tagged                 bool
	receivedTextBytes      int
}

func (r *SDKCompactionResponse) appendText(target *[]byte, text string) {
	if r.invalid {
		return
	}
	if len(text) > maxSDKCompactionTextBytes-r.receivedTextBytes {
		r.invalid, r.text = true, nil
		for index := range r.blocks {
			r.blocks[index].text = nil
		}
		return
	}
	r.receivedTextBytes += len(text)
	*target = append(*target, text...)
}

func (r *SDKCompactionResponse) ObserveJSON(payload []byte) {
	if r.invalid {
		return
	}
	var event struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Index   *int   `json:"index"`
		Message struct {
			Role string `json:"role"`
		} `json:"message"`
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
		Block struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type       string  `json:"type"`
			Text       *string `json:"text"`
			StopReason string  `json:"stop_reason"`
		} `json:"delta"`
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(payload, &event) != nil {
		r.invalid, r.text = true, nil
		return
	}
	if event.Type == "ping" {
		return
	}
	if r.done {
		r.invalid, r.text = true, nil
		return
	}
	switch event.Type {
	case "message":
		if r.started || event.Role != "assistant" || len(event.Content) == 0 || len(event.Content) > 4096 {
			r.invalid = true
			break
		}
		r.selected = -1
		for index, block := range event.Content {
			if block.Type == "text" && block.Text == nil {
				r.invalid = true
				break
			}
			if block.Type == "text" && r.selected < 0 {
				r.selected = index
				r.appendText(&r.text, *block.Text)
			}
		}
		r.started, r.done, r.stopReason = true, true, event.StopReason
	case "message_start":
		if r.started || event.Message.Role != "assistant" {
			r.invalid = true
		}
		r.started, r.selected = true, -1
	case "content_block_start":
		if !r.started || event.Index == nil || *event.Index != len(r.blocks) || len(r.blocks) >= 4096 || event.Block.Type == "" {
			r.invalid = true
			break
		}
		r.blocks = append(r.blocks, sdkCompactionBlock{kind: event.Block.Type})
		// Native stream initialization replaces initial text with an empty
		// string. Only subsequent deltas contribute to the yielded assistant.
	case "content_block_delta":
		if event.Index == nil || *event.Index < 0 || *event.Index >= len(r.blocks) || r.blocks[*event.Index].closed {
			r.invalid = true
			break
		}
		if r.blocks[*event.Index].kind == "text" {
			if event.Delta.Type == "text_delta" && event.Delta.Text != nil {
				r.appendText(&r.blocks[*event.Index].text, *event.Delta.Text)
			} else if event.Delta.Type != "citations_delta" {
				r.invalid = true
			}
		}
	case "content_block_stop":
		if event.Index == nil || *event.Index < 0 || *event.Index >= len(r.blocks) || r.blocks[*event.Index].closed {
			r.invalid = true
			break
		}
		r.blocks[*event.Index].closed = true
		if block := &r.blocks[*event.Index]; block.kind == "text" {
			tagged := bytes.Contains(block.text, []byte("<summary>"))
			if tagged || !r.tagged {
				r.text, r.selected, r.tagged = block.text, *event.Index, tagged
			}
			block.text = nil
		}
	case "message_delta":
		if !r.started {
			r.invalid = true
		}
		if event.Delta.StopReason != "" {
			r.stopReason = event.Delta.StopReason
		}
	case "message_stop":
		if !r.started || len(r.blocks) == 0 {
			r.invalid = true
		}
		for _, block := range r.blocks {
			if !block.closed {
				r.invalid = true
			}
		}
		r.done = true
	default:
		r.invalid = true
	}
	if r.invalid {
		r.text = nil
	}
}

func (r *SDKCompactionResponse) ObserveStreamLine(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.HasPrefix(line, []byte("data:")) {
		r.ObserveJSON(bytes.TrimSpace(line[len("data:"):]))
	}
}

var sdkCompactAnalysis = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)
var sdkCompactSummary = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
var sdkCompactBlankLines = regexp.MustCompile(`\n{2,}`)

func sdkNormalizeCompactionSummary(text string) string {
	value, _ := sdkNormalizeCompactionSummaryChecked(text)
	return value
}

func sdkNormalizeCompactionSummaryChecked(text string) (string, bool) {
	if len(text) > maxSDKCompactionTextBytes {
		return "", false
	}
	if loc := sdkCompactAnalysis.FindStringIndex(text); loc != nil {
		text = text[:loc[0]] + text[loc[1]:]
	}
	if loc := sdkCompactSummary.FindStringSubmatchIndex(text); loc != nil {
		limit := maxSDKCompactionTextBytes - loc[0] - (len(text) - loc[1])
		replacement := sdkCompactJSReplacement("Summary:\n"+trimInputSpace(text[loc[2]:loc[3]]), text, loc[0], loc[1], limit)
		if replacement == "" {
			return "", false
		}
		text = text[:loc[0]] + replacement + text[loc[1]:]
	}
	return trimInputSpace(sdkCompactBlankLines.ReplaceAllString(text, "\n\n")), true
}

// The native second replace uses a string replacement and a regex without
// capture groups. JS therefore expands $$, $&, $` and $', but not $1 or $<x>.
// Replacement contents are not recursively interpreted.
func sdkCompactJSReplacement(replacement, text string, start, end, limit int) string {
	var result strings.Builder
	// Replacement tokens can expand quadratically. Bound every write, including
	// the unchanged outer text, before allocating or hashing an expanded value.
	write := func(value string) bool {
		if len(value) > limit-result.Len() {
			return false
		}
		result.WriteString(value)
		return true
	}
	for index := 0; index < len(replacement); index++ {
		if replacement[index] != '$' || index+1 == len(replacement) {
			if !write(replacement[index : index+1]) {
				return ""
			}
			continue
		}
		var value string
		switch replacement[index+1] {
		case '$':
			value = "$"
		case '&':
			value = text[start:end]
		case '`':
			value = text[:start]
		case '\'':
			value = text[end:]
		default:
			if !write("$") {
				return ""
			}
			continue
		}
		if !write(value) {
			return ""
		}
		index++
	}
	return result.String()
}

func sdkCompactionHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// TakeSummary erases response text even when the response or version is unknown.
// A version-reviewed complete response can supply a fingerprint. Native text
// selection does not require end_turn; abort/API-error ownership is checked by
// the query lifecycle, not inferred from a stop reason in this selector.
func (r *SDKCompactionResponse) TakeSummary(desktopVersion, codeVersion string) SDKCompactionSummary {
	text, _ := r.TakeText(desktopVersion, codeVersion)
	return text.Fingerprint()
}

// TakeText transfers selected content to a request-local compaction operation.
// Empty selected text is an error; nonempty text which normalizes to empty is
// not: native _Jo checks w1e before Nhe/yJo removes analysis. No content is
// exported by SDKCompactionText's JSON representation or retained by Tracker.
func (r *SDKCompactionResponse) TakeText(desktopVersion, codeVersion string) (SDKCompactionText, bool) {
	defer r.Discard()
	versionKnown := desktopVersion == "1.40609.0.0" && codeVersion == "2.1.247" ||
		desktopVersion == "2.7032.0" && codeVersion == "2.1.280"
	if r.invalid || !r.started || !r.done || r.selected < 0 || !versionKnown {
		return SDKCompactionText{}, false
	}
	selected := trimInputSpace(string(r.text))
	if selected == "" {
		return SDKCompactionText{}, false
	}
	normalized, known := sdkNormalizeCompactionSummaryChecked(selected)
	if !known {
		return SDKCompactionText{}, false
	}
	return SDKCompactionText{selected: selected, normalized: normalized, known: true}, true
}

func (r *SDKCompactionResponse) Discard() {
	r.text, r.blocks = nil, nil
	r.invalid = true
}
