package tasks

import (
	"encoding/json"
	"strconv"
	"strings"
)

// FormatNotification follows the pinned W2/ql local-agent notification. The
// output path must come from an actual owned projection; absence is not proof
// of native output-file parity. Content belongs to this event's generation.
func FormatNotification(event Event, outputFile string) (string, error) {
	if event.Kind != "finished" || event.TaskID == "" {
		return "", ErrInvalid
	}
	ending := "finished"
	switch event.Status {
	case "completed":
	case "failed":
		detail := event.Error
		if detail == "" {
			detail = "Unknown error"
		}
		ending = "failed: " + detail
	case "killed":
		ending = "was stopped"
		if event.StoppedByUser {
			ending = "was stopped by user"
		}
	default:
		return "", ErrInvalid
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(event.Content) != 0 && string(event.Content) != "null" && json.Unmarshal(event.Content, &blocks) != nil {
		return "", ErrInvalid
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	var result strings.Builder
	result.WriteString("<task-notification>")
	for _, field := range [][2]string{{"task-id", event.TaskID}, {"tool-use-id", event.ToolUseID}, {"output-file", outputFile},
		{"status", event.Status}, {"summary", notificationEscape("Agent \"" + event.Description + "\" " + ending)}} {
		if field[1] != "" {
			result.WriteString("\n<" + field[0] + ">" + field[1] + "</" + field[0] + ">")
		}
	}
	result.WriteString("\n<note>A task-notification fires each time this agent stops with no live background children of its own. The user can send it another message and resume it, so the same task-id may notify more than once.</note>")
	if text := strings.Join(texts, "\n"); text != "" {
		result.WriteString("\n<result>" + notificationEscape(text) + "</result>")
	}
	if event.UsageKnown {
		result.WriteString("\n<usage><subagent_tokens>" + strconv.FormatInt(event.Tokens, 10) + "</subagent_tokens><tool_uses>" + strconv.Itoa(event.ToolUses) + "</tool_uses><duration_ms>" + strconv.FormatInt(event.DurationMS, 10) + "</duration_ms></usage>")
	}
	result.WriteString("\n</task-notification>")
	return result.String(), nil
}

// Native Ur escapes only these three characters, not quotes or line breaks.
func notificationEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}
