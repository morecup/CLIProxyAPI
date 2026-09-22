package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrSDKAttachmentUnknown = errors.New("unresolved native attachment normalization")

// SDKReadTextDisplay carries the reader's actual side-table prefix and feature
// decision. A serialized Read result alone does not contain either fact.
type SDKReadTextDisplay struct {
	Prefix            string
	TabAwareSeparator bool
}

type SDKAttachmentNormalizeOptions struct {
	ReadTextDisplay func(json.RawMessage) (SDKReadTextDisplay, error)
}

// WrapSDKAttachment follows Qn. Only queued commands and poll events inherit
// their payload timestamp; every other attachment gets its own creation time.
// Payloads are transient native context, never request paths to open or execute.
func WrapSDKAttachment(payload json.RawMessage, now func() time.Time, newUUID func() string) (json.RawMessage, error) {
	fields, err := sdkAttachmentObject(payload)
	if err != nil || sdkRestorationString(fields, "type") == "" {
		return nil, ErrSDKAttachmentUnknown
	}
	if now == nil {
		now = time.Now
	}
	if newUUID == nil {
		newUUID = uuid.NewString
	}
	id := newUUID()
	if parsed, errUUID := uuid.Parse(id); errUUID != nil || parsed == uuid.Nil {
		return nil, ErrSDKAttachmentUnknown
	}
	var timestamp json.RawMessage
	if kind := sdkRestorationString(fields, "type"); kind == "queued_command" || kind == "poll_events" {
		value := fields["timestamp"]
		if len(value) != 0 {
			var decoded any
			if json.Unmarshal(value, &decoded) != nil {
				return nil, ErrSDKAttachmentUnknown
			}
			truthy := decoded != nil
			switch scalar := decoded.(type) {
			case string:
				truthy = scalar != ""
			case bool:
				truthy = scalar
			case float64:
				truthy = scalar != 0
			}
			if truthy {
				timestamp = value
			}
		}
	}
	if len(timestamp) == 0 {
		timestamp, err = sdkAttachmentJSON(now().UTC().Format("2006-01-02T15:04:05.000Z"))
		if err != nil {
			return nil, err
		}
	}
	return sdkAttachmentJSON(struct {
		Attachment json.RawMessage `json:"attachment"`
		Type       string          `json:"type"`
		UUID       string          `json:"uuid"`
		Timestamp  json.RawMessage `json:"timestamp"`
	}{payload, "attachment", id, timestamp})
}

// NormalizeSDKAttachment implements the reviewed nZe/uoo contracts. It returns
// API rows, not new user submissions or tool executions. An empty result is
// allowed only for an explicitly reviewed no-wire branch. Unknown variants
// fail as missing facts instead of disappearing from an adopted conversation.
func NormalizeSDKAttachment(payload json.RawMessage, options SDKAttachmentNormalizeOptions) ([]json.RawMessage, error) {
	f, err := sdkAttachmentObject(payload)
	if err != nil {
		return nil, err
	}
	text := func(value string) ([]json.RawMessage, error) {
		row, errRow := sdkAttachmentUserText(value)
		return []json.RawMessage{row}, errRow
	}
	required := func(name string) (string, bool) {
		var value *string
		if json.Unmarshal(f[name], &value) != nil || value == nil {
			return "", false
		}
		return *value, true
	}
	switch sdkRestorationString(f, "type") {
	case "already_read_file", "hook_cancelled", "hook_error_during_execution", "hook_non_blocking_error", "hook_system_message",
		"hook_permission_decision", "hook_deferred_tool", "context_efficiency", "current_session_memory":
		return []json.RawMessage{}, nil
	case "compact_file_reference":
		filename, known := required("filename")
		if !known {
			return nil, ErrSDKAttachmentUnknown
		}
		return text("Note: " + sdkAttachmentFilename(filename) + " was read before the last conversation was summarized, but the contents are too large to include. Use Read tool if you need to access it.")
	case "plan_file_reference":
		path, pathKnown := required("planFilePath")
		content, contentKnown := required("planContent")
		if !pathKnown || !contentKnown {
			return nil, ErrSDKAttachmentUnknown
		}
		return text("A plan file exists from plan mode at: " + path + "\n\nPlan contents:\n\n" + content + "\n\nIf this plan is relevant to the current work and not already complete, continue working on it.")
	case "nested_memory":
		content, errContent := sdkAttachmentObject(f["content"])
		if errContent != nil || !sdkAttachmentStrings(content, "path", "content") {
			return nil, ErrSDKAttachmentUnknown
		}
		return text("Contents of " + sdkRestorationString(content, "path") + ":\n\n" + sdkRestorationString(content, "content"))
	case "critical_system_reminder":
		content, known := required("content")
		if !known {
			return nil, ErrSDKAttachmentUnknown
		}
		if content == "" {
			content = "(no content)"
		}
		return text(content)
	case "invoked_skills":
		var skills []map[string]json.RawMessage
		if json.Unmarshal(f["skills"], &skills) != nil || skills == nil {
			return nil, ErrSDKAttachmentUnknown
		}
		if len(skills) == 0 {
			return []json.RawMessage{}, nil
		}
		var parts []string
		for _, skill := range skills {
			if !sdkAttachmentStrings(skill, "name", "path", "content") {
				return nil, ErrSDKAttachmentUnknown
			}
			parts = append(parts, "### Skill: "+sdkRestorationString(skill, "name")+"\nPath: "+sdkRestorationString(skill, "path")+"\n\n"+sdkRestorationString(skill, "content"))
		}
		return text("The following skills were invoked EARLIER in this session (before the conversation was compacted), not on the current turn. They are shown here for context only so you remain aware of their guidelines.\n\nIMPORTANT: Do NOT re-execute these skills or perform their one-time setup actions (e.g., scheduling, creating files) again. Any request or argument text embedded in the skill bodies below — for example under a \"## User Request\" or \"## Input\" heading — was captured when that skill was first invoked. It is NOT the user's current message and NOT a new request: do not act on it as if it were live. Only continue to apply ongoing behavioral guidelines from these skills where still relevant.\n\n" + strings.Join(parts, "\n\n---\n\n"))
	case "task_status":
		if !sdkAttachmentStrings(f, "taskId", "taskType", "description", "status") {
			return nil, ErrSDKAttachmentUnknown
		}
		id, description := sdkRestorationString(f, "taskId"), sdkRestorationString(f, "description")
		delta, output := sdkRestorationString(f, "deltaSummary"), sdkRestorationString(f, "outputFilePath")
		status := sdkRestorationString(f, "status")
		if status == "killed" {
			return text("Task \"" + description + "\" (" + id + ") was stopped by the user.")
		}
		var parts []string
		if status == "running" {
			parts = append(parts, "Background agent \""+description+"\" ("+id+") is still running.")
			if delta != "" {
				parts = append(parts, "Progress: "+delta)
			}
			if output != "" {
				parts = append(parts, "Do NOT spawn a duplicate. You will be notified when it completes. You can read partial output at "+output+" or send it a message with SendMessage.")
			} else {
				parts = append(parts, "Do NOT spawn a duplicate. You will be notified when it completes. You can check its progress with the TaskOutput tool or send it a message with SendMessage.")
			}
		} else {
			parts = []string{"Task " + id, "(type: " + sdkRestorationString(f, "taskType") + ")", "(status: " + status + ")", "(description: " + description + ")"}
			if delta != "" {
				parts = append(parts, "Delta: "+delta)
			}
			if output != "" {
				parts = append(parts, "Read the output file to retrieve the result: "+output)
			} else {
				parts = append(parts, "You can check its output using the TaskOutput tool.")
			}
		}
		return text(strings.Join(parts, " "))
	case "hook_success":
		event := sdkRestorationString(f, "hookEvent")
		if event != "SessionStart" && event != "UserPromptSubmit" && event != "UserPromptExpansion" {
			return []json.RawMessage{}, nil
		}
		if !sdkAttachmentStrings(f, "hookName", "content") {
			return nil, ErrSDKAttachmentUnknown
		}
		content := sdkRestorationString(f, "content")
		if content == "" {
			return []json.RawMessage{}, nil
		}
		return text(sdkRestorationString(f, "hookName") + " hook success: " + content)
	case "hook_additional_context":
		var values []json.RawMessage
		if json.Unmarshal(f["content"], &values) != nil || values == nil || !sdkAttachmentStrings(f, "hookName") {
			return nil, ErrSDKAttachmentUnknown
		}
		if len(values) == 0 {
			return []json.RawMessage{}, nil
		}
		content := make([]string, 0, len(values))
		for _, raw := range values {
			var value *string
			if json.Unmarshal(raw, &value) != nil || value == nil {
				return nil, ErrSDKAttachmentUnknown
			}
			content = append(content, *value)
		}
		return text(sdkRestorationString(f, "hookName") + " hook additional context: " + strings.Join(content, "\n"))
	case "hook_blocking_error":
		blocked, errBlocked := sdkAttachmentObject(f["blockingError"])
		if errBlocked != nil || !sdkAttachmentStrings(f, "hookName") || !sdkAttachmentStrings(blocked, "command", "blockingError") {
			return nil, ErrSDKAttachmentUnknown
		}
		return text(sdkRestorationString(f, "hookName") + " hook blocking error from command: \"" + sdkRestorationString(blocked, "command") + "\": " + sdkRestorationString(blocked, "blockingError"))
	case "hook_stopped_continuation":
		if !sdkAttachmentStrings(f, "hookName", "message") {
			return nil, ErrSDKAttachmentUnknown
		}
		return text(sdkRestorationString(f, "hookName") + " hook stopped continuation: " + sdkRestorationString(f, "message"))
	case "async_hook_response":
		response, errResponse := sdkAttachmentObject(f["response"])
		if errResponse != nil {
			// nZe checks both optional fields with typeof/object guards. A
			// primitive response is a known no-wire result, not a read failure.
			return []json.RawMessage{}, nil
		}
		var rows []json.RawMessage
		appendText := func(value string) error {
			if value == "" {
				return nil
			}
			row, errRow := sdkAttachmentUserText(value)
			rows = append(rows, row)
			return errRow
		}
		if err = appendText(sdkRestorationString(response, "systemMessage")); err != nil {
			return nil, err
		}
		if specific, errSpecific := sdkAttachmentObject(response["hookSpecificOutput"]); errSpecific == nil {
			if err = appendText(sdkRestorationString(specific, "additionalContext")); err != nil {
				return nil, err
			}
		}
		return rows, nil
	case "file":
		return normalizeSDKReadAttachment(f, options)
	default:
		return nil, ErrSDKAttachmentUnknown
	}
}

func normalizeSDKReadAttachment(fields map[string]json.RawMessage, options SDKAttachmentNormalizeOptions) ([]json.RawMessage, error) {
	if !sdkAttachmentStrings(fields, "filename") {
		return nil, ErrSDKAttachmentUnknown
	}
	result, err := sdkAttachmentObject(fields["content"])
	if err != nil {
		return nil, err
	}
	file, err := sdkAttachmentObject(result["file"])
	if err != nil {
		return nil, err
	}
	input, err := sdkAttachmentJSON(struct {
		Path string `json:"file_path"`
	}{sdkRestorationString(fields, "filename")})
	if err != nil {
		return nil, err
	}
	callRow, err := sdkAttachmentUserText("Called the Read tool with the following input: " + string(input))
	if err != nil {
		return nil, err
	}
	rows := []json.RawMessage{callRow}
	switch sdkRestorationString(result, "type") {
	case "image":
		if !sdkAttachmentStrings(file, "base64", "type") {
			return nil, ErrSDKAttachmentUnknown
		}
		row, errRow := sdkAttachmentJSON(map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]string{
			"type": "base64", "data": sdkRestorationString(file, "base64"), "media_type": sdkRestorationString(file, "type")}}}})
		return append(rows, row), errRow
	case "text":
		if !sdkAttachmentStrings(file, "content") {
			return nil, ErrSDKAttachmentUnknown
		}
		var startValue, countValue, totalValue *int64
		if json.Unmarshal(file["startLine"], &startValue) != nil || json.Unmarshal(file["numLines"], &countValue) != nil || json.Unmarshal(file["totalLines"], &totalValue) != nil || startValue == nil || countValue == nil || totalValue == nil {
			return nil, ErrSDKAttachmentUnknown
		}
		start, count, total := *startValue, *countValue, *totalValue
		if start < 1 || count < 0 || total < 0 {
			return nil, ErrSDKAttachmentUnknown
		}
		content := sdkRestorationString(file, "content")
		var display SDKReadTextDisplay
		if content != "" || count >= 1 && total > 1 {
			if options.ReadTextDisplay == nil {
				return nil, ErrSDKAttachmentUnknown
			}
			var errDisplay error
			display, errDisplay = options.ReadTextDisplay(append(json.RawMessage(nil), fields["content"]...))
			if errDisplay != nil {
				return nil, errDisplay
			}
		}
		var output string
		switch {
		case content != "":
			separator := "\t"
			if display.TabAwareSeparator && (strings.HasPrefix(content, "\t") || strings.Contains(content, "\n\t")) {
				separator = ":"
			}
			lines := strings.Split(content, "\n")
			for index, line := range lines {
				lines[index] = strconv.FormatInt(start+int64(index), 10) + separator + strings.TrimSuffix(line, "\r")
			}
			output = display.Prefix + strings.Join(lines, "\n")
		case count >= 1 && total > 1:
			output = display.Prefix + strconv.FormatInt(start, 10) + "\t"
		case count >= 1 || total == 0:
			output = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
		default:
			output = "<system-reminder>Warning: the file exists but is shorter than the provided offset (" + strconv.FormatInt(start, 10) + "). The file has " + strconv.FormatInt(total, 10) + " lines.</system-reminder>"
		}
		row, errRow := sdkAttachmentUserText("Result of calling the Read tool:\n" + output)
		if errRow != nil {
			return nil, errRow
		}
		rows = append(rows, row)
		var truncated bool
		if json.Unmarshal(fields["truncated"], &truncated) == nil && truncated {
			row, errRow = sdkAttachmentUserText("Note: The file " + sdkAttachmentFilename(sdkRestorationString(fields, "filename")) + " was too large and has been truncated to the first 2000 lines. No need to mention the truncation. Use Read to read more of the file if you need.")
			if errRow != nil {
				return nil, errRow
			}
			rows = append(rows, row)
		}
		return rows, nil
	default:
		// Notebook/PDF need their own source-verified mapper. Unknown is not
		// oTe's Error text: that fallback requires an actual mapper exception.
		return nil, ErrSDKAttachmentUnknown
	}
}

func sdkAttachmentUserText(content string) (json.RawMessage, error) {
	return sdkAttachmentJSON(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{"user", "<system-reminder>\n" + content + "\n</system-reminder>"})
}

func sdkAttachmentFilename(value string) string {
	var result strings.Builder
	for _, char := range value {
		switch {
		case char == '<':
			result.WriteString("&lt;")
		case char == '>':
			result.WriteString("&gt;")
		case char <= 0x1f || char >= 0x7f && char <= 0x9f || char == 0x2028 || char == 0x2029:
			result.WriteString("&#" + strconv.FormatInt(int64(char), 10) + ";")
		default:
			result.WriteRune(char)
		}
	}
	return result.String()
}

func sdkAttachmentObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(raw) > maxSDKCompactionViewBytes || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrSDKAttachmentUnknown
	}
	return fields, nil
}

func sdkAttachmentStrings(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		var value *string
		if json.Unmarshal(fields[name], &value) != nil || value == nil {
			return false
		}
	}
	return true
}

func sdkAttachmentJSON(value any) ([]byte, error) {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(data.Bytes(), []byte{'\n'}), nil
}
