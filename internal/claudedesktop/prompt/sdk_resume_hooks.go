package prompt

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var sdkResumePersistedOutput = regexp.MustCompile(`(?m)Full output saved to: [^\r\n\x{2028}\x{2029}]*`)

func sdkResumeHookContentKey(text string) string {
	if !strings.HasPrefix(text, "<persisted-output>") {
		return text
	}
	if match := sdkResumePersistedOutput.FindStringIndex(text); match != nil {
		return text[:match[0]] + "Full output saved to: <persisted>" + text[match[1]:]
	}
	return text
}

func sdkResumeHookScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "undefined"
	}
	if value, valid := sdkWireString(raw); valid {
		return value
	}
	return string(bytes.TrimSpace(raw))
}

func sdkResumeHookKeys(row *sdkResumeRow) []string {
	if row.kind() != "attachment" {
		return nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(row.fields["attachment"], &payload) != nil || sdkResumeString(payload, "hookEvent") != "SessionStart" {
		return nil
	}
	switch sdkResumeString(payload, "type") {
	case "hook_additional_context":
		var content []string
		if json.Unmarshal(payload["content"], &content) != nil {
			return nil
		}
		for i, value := range content {
			content[i] = sdkResumeHookContentKey(value)
		}
		return content
	case "hook_success":
		if content, valid := sdkWireString(payload["content"]); valid && content != "" {
			return []string{sdkResumeHookContentKey(content)}
		}
	case "hook_non_blocking_error":
		command := ""
		if raw := bytes.TrimSpace(payload["command"]); len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
			command = sdkResumeHookScalar(raw)
		}
		return []string{command + "\x00" + sdkResumeHookScalar(payload["exitCode"]) + "\x00" + sdkResumeHookScalar(payload["stderr"])}
	}
	return nil
}

// DeduplicateSDKResumeHooks applies native KIn to already-produced hook rows.
// It never runs a hook or reads a referenced output file. Non-hook rows alone
// do not make a repeated SessionStart batch newly applicable.
func DeduplicateSDKResumeHooks(history, hooks []json.RawMessage) ([]json.RawMessage, error) {
	prior, err := parseSDKResumeRows(history)
	if err != nil {
		return nil, err
	}
	next, err := parseSDKResumeRows(hooks)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, row := range prior {
		for _, key := range sdkResumeHookKeys(row) {
			known[key] = true
		}
	}
	kept := make([]*sdkResumeRow, 0, len(next))
	added := false
	for _, row := range next {
		if len(known) == 0 {
			kept = append(kept, row)
			added = true
			continue
		}
		keys := sdkResumeHookKeys(row)
		if len(keys) == 0 || row.kind() != "attachment" {
			kept = append(kept, row)
			continue
		}
		var payload map[string]json.RawMessage
		_ = json.Unmarshal(row.fields["attachment"], &payload)
		if sdkResumeString(payload, "type") == "hook_additional_context" {
			var content []json.RawMessage
			if json.Unmarshal(payload["content"], &content) == nil && len(content) > 1 {
				filtered := make([]json.RawMessage, 0, len(content))
				for i, value := range content {
					if !known[keys[i]] {
						filtered = append(filtered, value)
					}
				}
				if len(filtered) == 0 {
					continue
				}
				if len(filtered) != len(content) {
					payload["content"], _ = json.Marshal(filtered)
					row.fields["attachment"], _ = json.Marshal(payload)
				}
				kept = append(kept, row)
				added = true
				continue
			}
		}
		if known[keys[0]] {
			continue
		}
		kept = append(kept, row)
		added = true
	}
	result := make([]json.RawMessage, 0, len(kept))
	if !added {
		return result, nil
	}
	for _, row := range kept {
		value, err := row.encode()
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}
