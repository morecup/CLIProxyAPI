package prompt

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"unicode/utf16"
)

// SDKResumeCoreOptions carries owned native decisions, never request headers.
// These options describe the selected pure transforms, not the complete Oyt
// deserializer, interrupted-turn policy, hooks or record admission.
type SDKResumeCoreOptions struct {
	PendingToolUseIDs                 []string
	DropSiblingBlocks                 bool
	ShutdownUnwindResultsDoNotResolve bool
	AllowTrailingThinking             bool
	TolerateContextAppends            bool
}

// SDKResumeCore keeps transformed native rows private. It is neither an API
// request nor a capability to resume a Desktop record or replace a live Host.
type SDKResumeCore struct {
	rows       []json.RawMessage
	superseded []string
	toolNames  map[string]string
}

func (r SDKResumeCore) Messages() []json.RawMessage {
	rows := make([]json.RawMessage, len(r.rows))
	for i, row := range r.rows {
		rows[i] = bytes.Clone(row)
	}
	return rows
}

func (r SDKResumeCore) SupersededToolUses() ([]string, map[string]string) {
	names := make(map[string]string, len(r.toolNames))
	for id, name := range r.toolNames {
		names[id] = name
	}
	return append([]string(nil), r.superseded...), names
}

type sdkResumeRow struct {
	fields  map[string]json.RawMessage
	message map[string]json.RawMessage
	blocks  []map[string]json.RawMessage
	array   bool
}

func sdkResumeString(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func sdkResumeBool(fields map[string]json.RawMessage, key string) bool {
	return bytes.Equal(bytes.TrimSpace(fields[key]), []byte("true"))
}

func parseSDKResumeRow(raw json.RawMessage) (*sdkResumeRow, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrSDKSessionInvalid
	}
	row := &sdkResumeRow{fields: fields}
	switch sdkResumeString(fields, "type") {
	case "user", "assistant":
		if json.Unmarshal(fields["message"], &row.message) != nil || row.message == nil {
			return nil, ErrSDKSessionInvalid
		}
		content := bytes.TrimSpace(row.message["content"])
		if len(content) == 0 {
			return nil, ErrSDKSessionInvalid
		}
		row.array = content[0] == '['
		if row.array && json.Unmarshal(content, &row.blocks) != nil {
			return nil, ErrSDKSessionInvalid
		}
		for _, block := range row.blocks {
			if block == nil {
				return nil, ErrSDKSessionInvalid
			}
		}
	}
	return row, nil
}

func (r *sdkResumeRow) kind() string { return sdkResumeString(r.fields, "type") }
func (r *sdkResumeRow) id() string   { return sdkResumeString(r.message, "id") }

func (r *sdkResumeRow) encode() (json.RawMessage, error) {
	if r.message != nil {
		if r.array {
			var err error
			r.message["content"], err = json.Marshal(r.blocks)
			if err != nil {
				return nil, ErrSDKSessionInvalid
			}
		}
		var err error
		r.fields["message"], err = json.Marshal(r.message)
		if err != nil {
			return nil, ErrSDKSessionInvalid
		}
	}
	return json.Marshal(r.fields)
}

// NormalizeSDKResumeCore follows Dyt, RNo, jG, QU and JU in native order. Rows
// remain native rows, including attachments and system boundaries. Attachment
// preparation, API-invalid-block filtering, rewind and interruption synthesis
// are separate Oyt stages and must not be inferred from this result.
func NormalizeSDKResumeCore(raw []json.RawMessage, options SDKResumeCoreOptions) (SDKResumeCore, error) {
	rows := make([]*sdkResumeRow, 0, len(raw))
	for _, value := range raw {
		row, err := parseSDKResumeRow(value)
		if err != nil {
			return SDKResumeCore{}, err
		}
		rows = append(rows, row)
	}
	rows = sdkResumeDropRetracted(rows)
	rows = sdkResumeDropInvalidText(rows)
	rows, superseded, names := sdkResumeReconcileTools(rows, options)
	rows = sdkResumeFilterThinking(rows, options.AllowTrailingThinking)
	rows = sdkResumeFilterWhitespace(rows)
	result := SDKResumeCore{superseded: superseded, toolNames: names}
	for _, row := range rows {
		encoded, err := row.encode()
		if err != nil {
			return SDKResumeCore{}, err
		}
		result.rows = append(result.rows, encoded)
	}
	return result, nil
}

func sdkResumeUUIDPrefix(value string) string {
	units := utf16.Encode([]rune(value))
	if len(units) > 24 {
		units = units[:24]
	}
	key := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(key[i*2:], unit)
	}
	return string(key)
}

func sdkResumeDropRetracted(rows []*sdkResumeRow) []*sdkResumeRow {
	retracted := make(map[string]bool)
	for _, row := range rows {
		if row.kind() != "system" || sdkResumeString(row.fields, "subtype") != "model_refusal_fallback" {
			continue
		}
		var ids []string
		_ = json.Unmarshal(row.fields["retractedMessageUuids"], &ids)
		for _, id := range ids {
			retracted[sdkResumeUUIDPrefix(id)] = true
		}
	}
	var kept []*sdkResumeRow
	for _, row := range rows {
		if row.kind() == "system" || !retracted[sdkResumeUUIDPrefix(sdkResumeString(row.fields, "uuid"))] {
			kept = append(kept, row)
		}
	}
	return kept
}

func sdkResumeDropInvalidText(rows []*sdkResumeRow) []*sdkResumeRow {
	var kept []*sdkResumeRow
	for _, row := range rows {
		if !row.array {
			kept = append(kept, row)
			continue
		}
		blocks := make([]map[string]json.RawMessage, 0, len(row.blocks))
		changed := false
		for _, block := range row.blocks {
			if sdkResumeString(block, "type") == "text" {
				if _, valid := sdkWireString(block["text"]); !valid {
					changed = true
					continue
				}
			}
			blocks = append(blocks, block)
		}
		if !changed || len(blocks) != 0 {
			row.blocks = blocks
			kept = append(kept, row)
		}
	}
	return kept
}

func sdkResumeShutdownResult(row *sdkResumeRow) bool {
	if row.kind() != "user" || !sdkResumeBool(row.fields, "interruptedByShutdown") {
		return false
	}
	for _, block := range row.blocks {
		if sdkResumeString(block, "type") == "tool_result" {
			return true
		}
	}
	return false
}

func sdkResumeReconcileTools(rows []*sdkResumeRow, options SDKResumeCoreOptions) ([]*sdkResumeRow, []string, map[string]string) {
	uses, resolved := make(map[string]bool), make(map[string]bool)
	for _, id := range options.PendingToolUseIDs {
		resolved[id] = true
	}
	for _, row := range rows {
		shutdown := options.ShutdownUnwindResultsDoNotResolve && sdkResumeShutdownResult(row)
		for _, block := range row.blocks {
			switch sdkResumeString(block, "type") {
			case "tool_use":
				uses[sdkResumeString(block, "id")] = true
			case "tool_result":
				if !shutdown {
					resolved[sdkResumeString(block, "tool_use_id")] = true
				}
			}
		}
	}
	orphans := make(map[string]bool)
	for id := range uses {
		if !resolved[id] {
			orphans[id] = true
		}
	}
	var superseded []string
	names, reported := make(map[string]string), make(map[string]bool)
	sawAssistant := false
	for i := len(rows) - 1; i >= 0 && len(orphans) != 0; i-- {
		row := rows[i]
		switch row.kind() {
		case "system", "progress", "attachment":
			continue
		case "user":
			toolResult := false
			for _, block := range row.blocks {
				toolResult = toolResult || sdkResumeString(block, "type") == "tool_result"
			}
			if toolResult || sdkResumeBool(row.fields, "interruptedByShutdown") || !sawAssistant && sdkResumeContextAppend(row, options.TolerateContextAppends) {
				continue
			}
			i = -1
			continue
		case "assistant":
			sawAssistant = true
			for _, block := range row.blocks {
				id := sdkResumeString(block, "id")
				if sdkResumeString(block, "type") != "tool_use" || !orphans[id] {
					continue
				}
				if !reported[id] {
					superseded, reported[id] = append(superseded, id), true
				}
				if name, valid := sdkWireString(block["name"]); valid {
					names[id] = name
				}
			}
		}
	}
	dropIDs, keepIDs := make(map[string]bool), make(map[string]bool)
	var kept []*sdkResumeRow
	for _, row := range rows {
		if options.ShutdownUnwindResultsDoNotResolve && sdkResumeShutdownResult(row) {
			continue
		}
		uses, missing := 0, 0
		if row.kind() == "assistant" {
			for _, block := range row.blocks {
				if sdkResumeString(block, "type") == "tool_use" {
					uses++
					if orphans[sdkResumeString(block, "id")] {
						missing++
					}
				}
			}
		}
		if uses != 0 && uses == missing {
			if options.DropSiblingBlocks && row.id() != "" {
				dropIDs[row.id()] = true
			}
			continue
		}
		if uses != 0 && options.DropSiblingBlocks && row.id() != "" {
			keepIDs[row.id()] = true
		}
		kept = append(kept, row)
	}
	result := kept[:0]
	for _, row := range kept {
		if row.kind() == "assistant" && row.array && dropIDs[row.id()] && !keepIDs[row.id()] {
			hasTool := false
			for _, block := range row.blocks {
				hasTool = hasTool || sdkResumeString(block, "type") == "tool_use"
			}
			if !hasTool {
				continue
			}
		}
		result = append(result, row)
	}
	return result, superseded, names
}

func sdkResumeContextAppend(row *sdkResumeRow, enabled bool) bool {
	if !enabled || row.kind() != "user" {
		return false
	}
	if origin, exists := row.fields["origin"]; exists {
		var fields map[string]json.RawMessage
		if json.Unmarshal(origin, &fields) != nil || sdkResumeString(fields, "kind") != "human" {
			return false
		}
	}
	if _, exists := row.fields["promptSource"]; exists && sdkResumeString(row.fields, "promptSource") != "sdk" {
		return false
	}
	var texts []string
	if text, valid := sdkWireString(row.message["content"]); valid {
		texts = append(texts, text)
	} else {
		for _, block := range row.blocks {
			text, valid := sdkWireString(block["text"])
			if sdkResumeString(block, "type") != "text" || !valid {
				return false
			}
			texts = append(texts, text)
		}
	}
	for _, text := range texts {
		if strings.HasPrefix(text, "<system-reminder>\n") && sdkWirePollEvent(text[18:]) {
			return false
		}
		remaining := strings.TrimRightFunc(text, sdkResumeSpace)
		if remaining == "" {
			return false
		}
		for remaining != "" {
			if !strings.HasPrefix(remaining, "<system-reminder>") {
				return false
			}
			end := strings.Index(remaining[17:], "</system-reminder>")
			if end < 0 || strings.Contains(remaining[17:17+end], "<system-reminder>") {
				return false
			}
			remaining = strings.TrimLeftFunc(remaining[17+end+18:], sdkResumeSpace)
		}
	}
	return len(texts) != 0
}

func sdkResumeSpace(r rune) bool { return sdkWireTrim(string(r)) == "" }

func sdkResumeThinking(row *sdkResumeRow) (only, unsigned bool) {
	if row.kind() != "assistant" || !row.array || len(row.blocks) == 0 {
		return false, false
	}
	for _, block := range row.blocks {
		kind := sdkResumeString(block, "type")
		if kind != "thinking" && kind != "redacted_thinking" {
			return false, false
		}
		unsigned = unsigned || kind == "thinking" && !sdkResumeTruthy(block["signature"])
	}
	return true, unsigned
}

func sdkResumeSkippable(row *sdkResumeRow) bool {
	return row.kind() == "progress" || row.kind() == "system" && sdkResumeString(row.fields, "subtype") != "local_command" ||
		(row.kind() == "user" || row.kind() == "assistant") && sdkResumeBool(row.fields, "isVirtual") ||
		row.kind() == "assistant" && sdkResumeBool(row.fields, "isApiErrorMessage") && sdkResumeString(row.message, "model") == "<synthetic>"
}

func sdkResumeFilterThinking(rows []*sdkResumeRow, allowTrailing bool) []*sdkResumeRow {
	withContent := make(map[string]bool)
	for _, row := range rows {
		if row.kind() == "assistant" && row.array && row.id() != "" {
			for _, block := range row.blocks {
				kind := sdkResumeString(block, "type")
				if kind != "thinking" && kind != "redacted_thinking" {
					withContent[row.id()] = true
				}
			}
		}
	}
	keep := make([]bool, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		only, unsigned := sdkResumeThinking(row)
		if !only {
			keep[i] = true
			continue
		}
		next := i + 1
		for next < len(rows) {
			_, incomplete := sdkResumeThinking(rows[next])
			if !sdkResumeSkippable(rows[next]) && !incomplete {
				break
			}
			next++
		}
		keep[i] = withContent[row.id()] || allowTrailing && i == len(rows)-1 || next < len(rows) && rows[next].kind() == "assistant" &&
			sdkResumeBool(rows[next].fields, "resumedFromIncompleteThinking") && keep[next] && !unsigned
	}
	var result []*sdkResumeRow
	for i, row := range rows {
		if keep[i] {
			result = append(result, row)
		}
	}
	return result
}

func sdkResumeWhitespace(row *sdkResumeRow) bool {
	if row.kind() != "assistant" || !row.array || len(row.blocks) == 0 {
		return false
	}
	for _, block := range row.blocks {
		if sdkResumeString(block, "type") != "text" {
			return false
		}
		text := sdkWireTrim(sdkResumeString(block, "text"))
		if text != "" && text != "(no content)" {
			return false
		}
	}
	return true
}

func sdkResumeFilterWhitespace(rows []*sdkResumeRow) []*sdkResumeRow {
	needed := false
	for _, row := range rows {
		needed = needed || sdkResumeWhitespace(row)
	}
	if !needed {
		return rows
	}
	useful := make(map[string]bool)
	for _, row := range rows {
		if row.kind() != "assistant" || row.id() == "" {
			continue
		}
		for _, block := range row.blocks {
			kind := sdkResumeString(block, "type")
			text := sdkWireTrim(sdkResumeString(block, "text"))
			if kind != "thinking" && kind != "redacted_thinking" && (kind != "text" || text != "" && text != "(no content)") {
				useful[row.id()] = true
			}
		}
	}
	var kept []*sdkResumeRow
	for _, row := range rows {
		if sdkResumeWhitespace(row) && !useful[row.id()] {
			continue
		}
		if len(kept) != 0 {
			previous := kept[len(kept)-1]
			if row.kind() == "user" && previous.kind() == "user" && !sdkResumeBool(row.fields, "interruptedByShutdown") && !sdkResumeBool(previous.fields, "interruptedByShutdown") {
				sdkResumeMergeUsers(previous, row)
				continue
			}
		}
		kept = append(kept, row)
	}
	return kept
}

func sdkResumeMergeUsers(left, right *sdkResumeRow) {
	for _, row := range []*sdkResumeRow{left, right} {
		if !row.array {
			row.blocks = []map[string]json.RawMessage{{"type": json.RawMessage(`"text"`), "text": bytes.Clone(row.message["content"])}}
			row.array = true
		}
	}
	if len(left.blocks) > 0 && len(right.blocks) > 0 {
		tail := left.blocks[len(left.blocks)-1]
		if sdkResumeString(tail, "type") == "text" && sdkResumeString(right.blocks[0], "type") == "text" {
			// Append to the encoded string without round-tripping its UTF-16
			// contents through Go runes, preserving escaped lone surrogates.
			raw := bytes.TrimSpace(tail["text"])
			if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
				tail["text"] = append(bytes.Clone(raw[:len(raw)-1]), []byte(`\n"`)...)
			}
		}
	}
	joined := append(left.blocks, right.blocks...)
	left.blocks = make([]map[string]json.RawMessage, 0, len(joined))
	for _, block := range joined {
		if sdkResumeString(block, "type") == "tool_result" {
			left.blocks = append(left.blocks, block)
		}
	}
	for _, block := range joined {
		if sdkResumeString(block, "type") != "tool_result" {
			left.blocks = append(left.blocks, block)
		}
	}
	if _, a := left.fields["collapseSources"]; a {
		sdkResumeMergeCollapseSources(left, right)
	} else if _, b := right.fields["collapseSources"]; b {
		sdkResumeMergeCollapseSources(left, right)
	}
	if sdkResumeBool(left.fields, "isMeta") {
		left.fields["uuid"] = bytes.Clone(right.fields["uuid"])
	}
}

func sdkResumeMergeCollapseSources(left, right *sdkResumeRow) {
	var result []json.RawMessage
	for _, row := range []*sdkResumeRow{left, right} {
		var values []json.RawMessage
		if raw := row.fields["collapseSources"]; len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			_ = json.Unmarshal(raw, &values)
		} else {
			values = []json.RawMessage{row.fields["uuid"]}
		}
		result = append(result, values...)
	}
	left.fields["collapseSources"], _ = json.Marshal(result)
}
