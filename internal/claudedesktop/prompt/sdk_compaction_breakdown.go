package prompt

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

// SDKCompactionBreakdown retains only numeric diagnostics, never content,
// paths, tool IDs or arbitrary metadata supplied by a caller.
type SDKCompactionBreakdown struct{ values map[string]int64 }

func (b *SDKCompactionBreakdown) Metadata() map[string]int64 {
	if b == nil {
		return nil
	}
	copy := make(map[string]int64, len(b.values))
	for key, value := range b.values {
		copy[key] = value
	}
	return copy
}

var sdkBreakdownName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ObserveSDKCompactionBreakdown implements Code 2.1.280 xX/CX on the native
// normalizer's output BEFORE API cache markers are inserted. Its JSON block
// estimate differs deliberately from EstimateSDKContent: it counts the whole
// serialized block, not just its text or a fixed image cost. Native attachment
// counts are supplied separately because normalization merges/drops envelopes.
func ObserveSDKCompactionBreakdown(rows []json.RawMessage, attachments []string, normalizeTool func(string) string) (*SDKCompactionBreakdown, error) {
	values := map[string]int64{"total_tokens": 0, "human_message_tokens": 0, "assistant_message_tokens": 0,
		"local_command_output_tokens": 0, "other_tokens": 0, "duplicate_read_tokens": 0, "duplicate_read_file_count": 0}
	toolNames, readPaths := map[string]string{}, map[string]string{}
	type reads struct{ count, tokens int64 }
	files := map[string]reads{}
	requests, results := map[string]int64{}, map[string]int64{}
	for _, name := range attachments {
		if !sdkBreakdownName.MatchString(name) {
			return nil, ErrSDKCompactionContentUnknown
		}
		values["attachment_"+name+"_count"]++
	}
	textCategory := func(role, text string) string {
		if role == "user" {
			if strings.Contains(text, "local-command-stdout") || strings.Contains(text, "local-command-stderr") {
				return "local_command_output_tokens"
			}
			return "human_message_tokens"
		}
		return "assistant_message_tokens"
	}
	for _, raw := range rows {
		var row struct {
			Role    string
			Content json.RawMessage
		}
		if json.Unmarshal(raw, &row) != nil || (row.Role != "user" && row.Role != "assistant") {
			return nil, ErrSDKCompactionContentUnknown
		}
		var text string
		if json.Unmarshal(row.Content, &text) == nil {
			n := sdkRoundedTokens(sdkTextUnits(text))
			values["total_tokens"] += n
			values[textCategory(row.Role, text)] += n
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(row.Content, &blocks) != nil {
			return nil, ErrSDKCompactionContentUnknown
		}
		for _, rawBlock := range blocks {
			var block struct {
				Type, Text, Name, ID string
				ToolUseID            string `json:"tool_use_id"`
				Input                struct {
					Path string `json:"file_path"`
				}
				CacheControl json.RawMessage `json:"cache_control"`
			}
			if json.Unmarshal(rawBlock, &block) != nil || block.Type == "" || len(block.CacheControl) != 0 {
				return nil, ErrSDKCompactionContentUnknown
			}
			units, known := sdkJSONUnits(rawBlock, 0)
			if !known {
				return nil, ErrSDKReactiveEstimateUnknown
			}
			n := sdkRoundedTokens(units)
			values["total_tokens"] += n
			switch block.Type {
			case "text":
				values[textCategory(row.Role, block.Text)] += n
			case "tool_use":
				if normalizeTool == nil || block.ID == "" {
					return nil, ErrSDKCompactionContentUnknown
				}
				name := normalizeTool(block.Name)
				if !sdkBreakdownName.MatchString(name) {
					return nil, ErrSDKCompactionContentUnknown
				}
				toolNames[block.ID] = name
				requests[name] += n
				if block.Name == "Read" && block.Input.Path != "" {
					readPaths[block.ID] = block.Input.Path
				}
			case "tool_result":
				name := toolNames[block.ToolUseID]
				if name == "" {
					name = "unknown"
				}
				results[name] += n
				if path := readPaths[block.ToolUseID]; name == "Read" && path != "" {
					value := files[path]
					value.count++
					value.tokens += n
					files[path] = value
				}
			case "image", "server_tool_use", "web_search_tool_result", "search_result", "document", "thinking", "redacted_thinking", "code_execution_tool_result", "mcp_tool_use", "mcp_tool_result", "container_upload", "web_fetch_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "tool_search_tool_result", "advisor_tool_result", "compaction", "fallback", "mid_conv_system":
				values["other_tokens"] += n
			default:
				return nil, ErrSDKCompactionContentUnknown
			}
		}
	}
	for _, read := range files {
		if read.count > 1 {
			values["duplicate_read_file_count"]++
			values["duplicate_read_tokens"] += (read.tokens / read.count) * (read.count - 1)
		}
	}
	total := values["total_tokens"]
	percent := func(n int64) int64 { return int64(math.Round(float64(n) / float64(total) * 100)) }
	if total > 0 {
		for _, name := range []string{"human_message", "assistant_message", "local_command_output", "duplicate_read"} {
			values[name+"_percent"] = percent(values[name+"_tokens"])
		}
	}
	for prefix, tools := range map[string]map[string]int64{"tool_request": requests, "tool_result": results} {
		var sum int64
		for name, tokens := range tools {
			values[prefix+"_"+name+"_tokens"] = tokens
			sum += tokens
			if total > 0 {
				values[prefix+"_"+name+"_percent"] = percent(tokens)
			}
		}
		if total > 0 {
			values[prefix+"_percent"] = percent(sum)
		}
	}
	return &SDKCompactionBreakdown{values: values}, nil
}
