package prompt

import (
	"encoding/json"
	"strconv"
)

func sdkResumeServerTool(name string) bool {
	switch name {
	case "advisor", "bash_code_execution", "code_execution", "text_editor_code_execution", "tool_search_tool_bm25", "tool_search_tool_regex", "web_fetch", "web_search":
		return true
	}
	return false
}

// Repair is gated by the same native iNo trigger. A server result is scoped
// to an assistant message group; a client result's ownership is cross-group.
func sdkResumeRepairAPIBlocks(rows []*sdkResumeRow) ([]*sdkResumeRow, bool) {
	needed := false
	for _, row := range rows {
		if row.kind() != "assistant" {
			continue
		}
		for _, block := range row.blocks {
			kind := sdkResumeString(block, "type")
			needed = needed || kind == "tool_result" || kind == "server_tool_use" && !sdkResumeServerTool(sdkResumeString(block, "name"))
		}
	}
	if !needed {
		return rows, false
	}
	groups := make([]string, len(rows))
	turn := 0
	for i, row := range rows {
		if row.kind() == "assistant" {
			id := sdkResumeString(row.message, "id")
			if _, exists := row.message["id"]; !exists {
				id = "undefined"
			}
			groups[i] = strconv.Itoa(turn) + ":" + id
			continue
		}
		isResult := false
		for _, block := range row.blocks {
			isResult = isResult || sdkResumeString(block, "type") == "tool_result"
		}
		if row.kind() == "user" && !isResult || row.kind() == "system" && sdkResumeString(row.fields, "subtype") == "local_command" {
			turn++
		}
	}
	server, client, assistant, user := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, row := range rows {
		for _, block := range row.blocks {
			kind := sdkResumeString(block, "type")
			id := sdkResumeString(block, "tool_use_id")
			if row.kind() == "user" {
				if kind == "tool_result" {
					user[id] = true
				}
				continue
			}
			if row.kind() != "assistant" {
				continue
			}
			if kind == "tool_result" {
				assistant[id] = true
			}
			if _, valid := sdkWireString(block["tool_use_id"]); !valid {
				continue
			}
			key := groups[i] + "\n" + id
			if kind == "tool_result" {
				client[key] = true
			} else if kind != "tool_use" && kind != "server_tool_use" && kind != "mcp_tool_use" {
				server[key] = true
			}
		}
	}
	type sequence struct{ thinking, removed bool }
	sequences := map[string]sequence{}
	kept := make([]*sdkResumeRow, 0, len(rows))
	removed := false
	for i, row := range rows {
		if row.kind() != "assistant" || !row.array {
			kept = append(kept, row)
			continue
		}
		group := groups[i]
		prior := sequences[group]
		blocks := make([]map[string]json.RawMessage, 0, len(row.blocks))
		changed := false
		for _, block := range row.blocks {
			kind := sdkResumeString(block, "type")
			id, validID := sdkWireString(block["id"])
			drop := kind == "tool_result"
			if kind == "tool_use" {
				drop = assistant[id] && !user[id]
			}
			if kind == "server_tool_use" || kind == "mcp_tool_use" {
				key := group + "\n" + id
				drop = !(validID && server[key]) && (validID && client[key] || kind == "server_tool_use" && !sdkResumeServerTool(sdkResumeString(block, "name")))
			}
			if drop {
				removed = true
				changed = true
				prior.removed = true
				continue
			}
			thinking := kind == "thinking" || kind == "redacted_thinking"
			if prior.removed && prior.thinking && thinking {
				blocks = append(blocks, map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": json.RawMessage(`"[Unsupported tool content removed]"`), "citations": json.RawMessage(`[]`)})
				changed = true
			}
			blocks = append(blocks, block)
			prior.thinking, prior.removed = thinking, false
		}
		sequences[group] = prior
		if changed {
			if len(blocks) == 0 {
				continue
			}
			row.blocks = blocks
		}
		kept = append(kept, row)
	}
	return kept, removed
}
