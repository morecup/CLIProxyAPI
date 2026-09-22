package prompt

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

type sdkResumeInterruption struct {
	kind    string
	message *sdkResumeRow
}

func sdkResumeHistoryFilterThinking(rows []*sdkResumeRow, events *[]sdkResumeEvent) []*sdkResumeRow {
	kept := sdkResumeFilterThinking(rows, false)
	present := make(map[*sdkResumeRow]bool, len(kept))
	for _, row := range kept {
		present[row] = true
	}
	for _, row := range rows {
		if present[row] || row.kind() != "assistant" {
			continue
		}
		attributes := map[string]any{"blockCount": len(row.blocks)}
		if id, exists := row.message["id"]; exists {
			attributes["messageId"] = id
		}
		if id, valid := sdkWireString(row.fields["uuid"]); valid {
			conforms := len(id) > 0 && len(id) <= 128
			for _, r := range id {
				conforms = conforms && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
			}
			if !conforms {
				id = "nonconforming"
			}
			attributes["messageUUID"] = id
		}
		*events = append(*events, sdkResumeEvent{"tengu_filtered_orphaned_thinking_message", attributes})
	}
	return kept
}

func sdkResumeTruthy(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("false")) {
		return false
	}
	if value, valid := sdkWireString(raw); valid {
		return value != ""
	}
	if raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9' {
		value, err := strconv.ParseFloat(string(raw), 64)
		return value != 0 && (err == nil || math.IsInf(value, 0))
	}
	return true
}

func sdkResumeSingleText(row *sdkResumeRow) (string, bool) {
	if value, valid := sdkWireString(row.message["content"]); valid {
		return value, true
	}
	if row.array && len(row.blocks) == 1 && sdkResumeString(row.blocks[0], "type") == "text" {
		return sdkWireString(row.blocks[0]["text"])
	}
	return "", false
}

func sdkResumeDropPlaceholders(rows []*sdkResumeRow, options SDKResumeHistoryOptions) []*sdkResumeRow {
	if !options.TolerateContextAppends {
		return rows
	}
	kept := make([]*sdkResumeRow, 0, len(rows))
	for i, row := range rows {
		text, valid := sdkResumeSingleText(row)
		placeholder := row.kind() == "assistant" && !sdkResumeTruthy(row.fields["isApiErrorMessage"]) && sdkResumeString(row.message, "model") == "<synthetic>" && valid && text == "No response requested."
		if i > 0 && rows[i-1].kind() == "user" && placeholder && sdkResumeString(row.fields, "uuid") != options.RewindUUID {
			previous := rows[i-1]
			text, valid = sdkResumeSingleText(previous)
			_, source := previous.fields["promptSource"]
			if sdkResumeBool(previous.fields, "isMeta") && !source && valid && (text == options.ResumePrompt || text == "Continue from where you left off.") && sdkResumeString(previous.fields, "uuid") != options.RewindUUID && len(kept) > 0 {
				kept = kept[:len(kept)-1]
			}
			continue
		}
		kept = append(kept, row)
	}
	return kept
}

func sdkResumeAtRewind(rows []*sdkResumeRow, options SDKResumeHistoryOptions) bool {
	if options.RewindUUID == "" {
		return false
	}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.kind() != "user" && row.kind() != "assistant" {
			continue
		}
		if sdkResumeString(row.fields, "uuid") == options.RewindUUID {
			return true
		}
		if !sdkResumeContextAppend(row, options.TolerateContextAppends) {
			return false
		}
	}
	return false
}

// The native setting uses Number(), including whitespace and unsigned radix
// prefixes. Invalid, infinite or negative nonzero values select one hour.
func sdkResumeMaxAge(value string) float64 {
	value = sdkWireTrim(value)
	if value == "" {
		return 0
	}
	if len(value) > 2 && value[0] == '0' {
		base := 0
		switch value[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			digits := value[2:]
			for _, r := range digits {
				if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
					return 3600000
				}
			}
			n, ok := new(big.Int).SetString(digits, base)
			if !ok {
				return 3600000
			}
			result, _ := new(big.Float).SetInt(n).Float64()
			if math.IsInf(result, 0) {
				return 3600000
			}
			return result
		}
	}
	// Go also accepts signed hexadecimal floating literals; Number() does
	// not. Unsigned integer radix forms were handled explicitly above.
	if strings.ContainsAny(value, "xXpP") {
		return 3600000
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 3600000
	}
	return n
}

func sdkResumeStale(rows []*sdkResumeRow, options SDKResumeHistoryOptions) bool {
	if options.MaxAgeMilliseconds == "" {
		return false
	}
	maxAge := sdkResumeMaxAge(options.MaxAgeMilliseconds)
	if maxAge == 0 {
		return false
	}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.kind() == "system" || row.kind() == "progress" || sdkResumeContextAppend(row, options.TolerateContextAppends) {
			continue
		}
		// Owned transcript timestamps are native ISO strings. Malformed rows
		// do not acquire a fabricated recent timestamp during restoration.
		at, err := time.Parse(time.RFC3339Nano, sdkResumeString(row.fields, "timestamp"))
		if err == nil {
			return float64(options.Now().UnixMilli()-at.UnixMilli()) >= maxAge
		}
	}
	return true
}

func sdkResumeToolResult(row *sdkResumeRow) bool {
	return row.kind() == "user" && (row.array && len(row.blocks) > 0 && sdkResumeString(row.blocks[0], "type") == "tool_result" || sdkResumeTruthy(row.fields["toolUseResult"]))
}

func sdkResumeRefusal(row *sdkResumeRow) bool {
	return sdkResumeString(row.message, "stop_reason") == "refusal" || sdkResumeString(row.fields, "apiError") == "dlp_request_denied"
}

func sdkResumeInterruptionMarker(row *sdkResumeRow) bool {
	value, valid := sdkResumeSingleText(row)
	return valid && (value == "[Request interrupted by user]" || value == "[Request interrupted by user for tool use]")
}

func sdkResumeTerminalResult(row *sdkResumeRow, rows []*sdkResumeRow, before int, options SDKResumeHistoryOptions) bool {
	if !row.array || len(row.blocks) == 0 || sdkResumeString(row.blocks[0], "type") != "tool_result" {
		return false
	}
	id := sdkResumeString(row.blocks[0], "tool_use_id")
	for i := before - 1; i >= 0; i-- {
		if rows[i].kind() != "assistant" {
			continue
		}
		for _, block := range rows[i].blocks {
			if sdkResumeString(block, "type") != "tool_use" || sdkResumeString(block, "id") != id {
				continue
			}
			name := sdkResumeString(block, "name")
			if name == "SendUserMessage" || name == "Brief" || name == "SendUserFile" {
				return true
			}
			if sdkResumeTruthy(row.blocks[0]["is_error"]) {
				return false
			}
			for _, terminal := range strings.Split(options.TerminalMCPTools, ",") {
				if terminal = sdkWireTrim(terminal); terminal != "" && terminal == name {
					return true
				}
			}
			return false
		}
	}
	return false
}

func sdkResumeClassify(rows []*sdkResumeRow, superseded bool, options SDKResumeHistoryOptions, events *[]sdkResumeEvent) sdkResumeInterruption {
	state := func(interrupted bool) sdkResumeInterruption {
		if interrupted {
			return sdkResumeInterruption{kind: "interrupted_turn"}
		}
		return sdkResumeInterruption{kind: "none"}
	}
	assistantState := func(row *sdkResumeRow) {
		if sdkResumeTruthy(row.fields["isApiErrorMessage"]) {
			verdict := "refusal"
			if sdkResumeString(row.fields, "apiError") == "dlp_request_denied" {
				verdict = "dlp_denied"
			}
			*events = append(*events, sdkResumeEvent{"tengu_refusal_turn_classified_complete", map[string]any{"verdict": verdict}})
		}
	}
	last := -1
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.kind() == "system" || row.kind() == "progress" || row.kind() == "assistant" && sdkResumeTruthy(row.fields["isApiErrorMessage"]) && !sdkResumeRefusal(row) {
			continue
		}
		last = i
		break
	}
	if last < 0 {
		return state(superseded)
	}
	row := rows[last]
	if row.kind() == "assistant" {
		assistantState(row)
		return state(false)
	}
	context := row.kind() == "attachment" || sdkResumeContextAppend(row, options.TolerateContextAppends)
	if row.kind() == "user" && !context {
		if sdkResumeTruthy(row.fields["isMeta"]) || sdkResumeTruthy(row.fields["isCompactSummary"]) {
			return state(superseded)
		}
		if sdkResumeInterruptionMarker(row) {
			return state(sdkResumeTruthy(row.fields["interruptedByShutdown"]))
		}
		if sdkResumeToolResult(row) {
			if sdkResumeTerminalResult(row, rows, last, options) {
				return state(false)
			}
			allPlugin := row.array && len(row.blocks) > 0
			for _, block := range row.blocks {
				allPlugin = allPlugin && sdkResumeString(block, "type") == "tool_result" && sdkResumeString(block, "content") == "[Request interrupted by a plugin for tool use]"
			}
			if allPlugin {
				return state(false)
			}
			var result map[string]json.RawMessage
			if json.Unmarshal(row.fields["toolUseResult"], &result) == nil && sdkResumeBool(result, "backgroundedByTurnAbort") {
				return state(false)
			}
			return state(true)
		}
		var origin map[string]json.RawMessage
		text, valid := sdkResumeSingleText(row)
		if json.Unmarshal(row.fields["origin"], &origin) == nil && sdkResumeString(origin, "kind") == "task-notification" && valid && strings.HasPrefix(text, "<task-notification>") {
			return state(true)
		}
		return sdkResumeInterruption{kind: "interrupted_prompt", message: row}
	}
	if context {
		sawUser := row.kind() == "user"
		for i := last - 1; i >= 0; i-- {
			current := rows[i]
			if current.kind() == "system" || current.kind() == "progress" || current.kind() == "attachment" || sdkResumeContextAppend(current, options.TolerateContextAppends) {
				sawUser = sawUser || current.kind() == "user"
				continue
			}
			if current.kind() == "assistant" {
				isError := sdkResumeTruthy(current.fields["isApiErrorMessage"])
				if isError && !sdkResumeRefusal(current) {
					continue
				}
				assistantState(current)
				if sawUser && !isError {
					return state(superseded)
				}
				return state(false)
			}
			if current.kind() == "user" && sdkResumeTruthy(current.fields["isCompactSummary"]) {
				return state(superseded)
			}
			if current.kind() == "user" && sdkResumeInterruptionMarker(current) {
				return state(sdkResumeTruthy(current.fields["interruptedByShutdown"]))
			}
			if current.kind() == "user" && sdkResumeToolResult(current) && sdkResumeTerminalResult(current, rows, i, options) {
				return state(false)
			}
			return state(true)
		}
		if sawUser {
			return state(superseded)
		}
		return state(true)
	}
	return state(false)
}
