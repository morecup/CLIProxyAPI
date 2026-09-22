package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type resultText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type resultSections struct {
	NoteCount int    `json:"harnessNoteCount"`
	TailCount int    `json:"harnessTailCount"`
	Hash      string `json:"harnessSectionHash"`
}

func prepareResult(raw json.RawMessage, provenance bool) (json.RawMessage, resultSections, error) {
	var blocks []resultText
	if err := json.Unmarshal(textContent(raw), &blocks); err != nil {
		return nil, resultSections{}, err
	}
	var findings []resultFinding
	for i := range blocks {
		result, err := sanitizeResult(blocks[i].Text, provenance, false)
		if err != nil {
			return nil, resultSections{}, err
		}
		blocks[i].Text = result.Sanitized
		findings = append(findings, result.Findings...)
	}
	profile, err := compiledResultProfile()
	if err != nil {
		return nil, resultSections{}, err
	}
	var sections resultSections
	if marker := resultMarker(profile, findings); marker != "" {
		blocks = append([]resultText{{Type: "text", Text: marker + "\n"}}, blocks...)
		sections.NoteCount = 1
	}
	sections.Hash = resultSectionHash(blocks)
	content, err := json.Marshal(blocks)
	return content, sections, err
}

func jsTextLength(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

func resultSectionHash(blocks []resultText) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strconv.Itoa(len(blocks))))
	for _, block := range blocks {
		_, _ = fmt.Fprintf(hash, ":%d:%s", jsTextLength(block.Text), block.Text)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

var resultLineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n", "\u0085", "\n", "\v", "\n", "\f", "\n", "\u001c", "\n", "\u001d", "\n", "\u001e", "\n")

func indentResult(text string) string {
	return "  " + strings.ReplaceAll(resultLineBreaks.Replace(text), "\n", "\n  ")
}

func splitResultSections(blocks []resultText, notes, tail float64, hash string) ([]resultText, []resultText, []resultText) {
	// Native Yue only trusts counts bound to the complete report hash. Bad
	// metadata degrades to one untrusted body, never to trusted harness notes.
	n, m := int(notes), int(tail)
	if notes != float64(n) || tail != float64(m) || n < 0 || m < 0 || n > len(blocks) || m > len(blocks)-n ||
		(n+m > 0 && hash != resultSectionHash(blocks)) {
		n, m = 0, 0
	}
	return blocks[:n], blocks[n : len(blocks)-m], blocks[len(blocks)-m:]
}

func frameResult(blocks []resultText, notes, tail float64, hash string) (string, error) {
	profile, err := compiledResultProfile()
	if err != nil {
		return "", err
	}
	head, report, foot := splitResultSections(blocks, notes, tail, hash)
	parts := make([]string, 0, len(head)+len(foot)+1)
	for _, block := range head {
		parts = append(parts, indentResult(block.Text))
	}
	for _, block := range foot {
		parts = append(parts, indentResult(block.Text))
	}
	var body []string
	for _, block := range report {
		body = append(body, block.Text)
	}
	text := strings.Join(body, "\n")
	if text == "" {
		text = "(no text output)"
	}
	parts = append(parts, profile.FramePreamble+"\n"+indentResult(text))
	return strings.Join(parts, "\n"), nil
}

func renderAgentResult(raw json.RawMessage, provenance bool) ([]resultText, error) {
	var value struct {
		Status               string       `json:"status"`
		AgentID              string       `json:"agentId"`
		AgentType            string       `json:"agentType"`
		Content              []resultText `json:"content"`
		HarnessNoteCount     float64      `json:"harnessNoteCount"`
		HarnessTailCount     float64      `json:"harnessTailCount"`
		HarnessSectionHash   string       `json:"harnessSectionHash"`
		HandoffReviewSkipped bool         `json:"handoffReviewSkipped"`
		TotalTokens          int64        `json:"totalTokens"`
		TotalToolUseCount    int          `json:"totalToolUseCount"`
		TotalDurationMS      int64        `json:"totalDurationMs"`
		CanReadOutputFile    bool         `json:"canReadOutputFile"`
		OutputFile           string       `json:"outputFile"`
		WorktreePath         string       `json:"worktreePath"`
		WorktreeBranch       string       `json:"worktreeBranch"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	switch value.Status {
	case "async_launched":
		text := fmt.Sprintf("Async agent launched successfully. (This tool result is internal metadata — never quote or paste any part of it, including the agentId below, into a user-facing reply.)\nagentId: %s (internal ID - do not mention to user. Use SendMessage with to: '%s', summary: '<5-10 word recap>' to continue this agent.)\nThe agent is working in the background. You will be notified automatically when it completes. You know nothing about its results until that notification arrives — do not report, assume, or predict them; continue other work or respond to the user in the meantime.\n", value.AgentID, value.AgentID)
		if value.CanReadOutputFile {
			text += "Do not duplicate this agent's work — avoid working with the same files or topics it is using.\noutput_file: " + value.OutputFile + "\nDo NOT Read or tail this file via the shell tool — it is the full subagent JSONL transcript and reading it will overflow your context. If the user asks for progress, say the agent is still running; you'll get a completion notification."
		} else {
			text += "In your own words, briefly tell the user what you launched — do not echo this tool result. Agent results will arrive in a subsequent message. If the user asks for progress, say the agent is still running."
		}
		return []resultText{{Type: "text", Text: text}}, nil
	case "completed":
		blocks := value.Content
		if len(blocks) == 0 {
			blocks = []resultText{{Type: "text", Text: "(Subagent completed but returned no output.)"}}
			value.HarnessNoteCount, value.HarnessTailCount = 0, 0
		}
		framed := provenance || value.HandoffReviewSkipped
		if framed {
			text, err := frameResult(blocks, value.HarnessNoteCount, value.HarnessTailCount, value.HarnessSectionHash)
			if err != nil {
				return nil, err
			}
			blocks = []resultText{{Type: "text", Text: text}}
		}
		worktree := ""
		if value.WorktreePath != "" {
			worktree = "\nworktreePath: " + value.WorktreePath
			if value.WorktreeBranch != "" {
				worktree += "\nworktreeBranch: " + value.WorktreeBranch
			}
		}
		if (value.AgentType == "Explore" || value.AgentType == "Plan") && worktree == "" {
			return blocks, nil
		}
		usage := fmt.Sprintf("agentId: %s (use SendMessage with to: '%s', summary: '<5-10 word recap>' to continue this agent)%s\n<usage>subagent_tokens: %d\ntool_uses: %d\nduration_ms: %d</usage>", value.AgentID, value.AgentID, worktree, value.TotalTokens, value.TotalToolUseCount, value.TotalDurationMS)
		if framed {
			blocks[0].Text += "\n" + usage
		} else {
			blocks = append(blocks, resultText{Type: "text", Text: usage})
		}
		return blocks, nil
	default:
		return nil, errors.New("Unexpected agent tool result status: " + value.Status)
	}
}

func jsTrimSpace(r rune) bool {
	return strings.ContainsRune("\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff", r)
}

func resultTextTail(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	n, start := 0, len(runes)
	for start > 0 {
		size := 1
		if runes[start-1] > 0xffff {
			size = 2
		}
		if n+size > limit {
			break
		}
		n += size
		start--
	}
	return string(runes[start:])
}

func truncateResult(text, path string, limit int, omit bool) (string, error) {
	if jsTextLength(text) <= limit {
		return text, nil
	}
	if omit {
		note := func(n int) string {
			return fmt.Sprintf("[Truncated to the last %d characters; the earlier part of the report is not retrievable.]\n\n", n)
		}
		tail := resultTextTail(text, limit-jsTextLength(note(limit)))
		return note(jsTextLength(tail)) + tail, nil
	}
	if path == "" {
		return "", errors.New("Native task output projection is unavailable; cannot publish a full-output path")
	}
	note := "[Truncated. Full output: " + path + "]\n\n"
	return note + resultTextTail(text, limit-jsTextLength(note)), nil
}

func renderTaskOutput(raw json.RawMessage, provenance bool, limit int, path func(string) string) (string, error) {
	return renderTaskOutputWithPolicy(raw, func() bool { return provenance }, limit, path)
}

func renderTaskOutputWithPolicy(raw json.RawMessage, policy func() bool, limit int, path func(string) string) (string, error) {
	var value struct {
		RetrievalStatus string `json:"retrieval_status"`
		Task            *struct {
			TaskID          string       `json:"task_id"`
			TaskType        string       `json:"task_type"`
			Status          string       `json:"status"`
			Output          string       `json:"output"`
			HarnessHead     string       `json:"harnessHead"`
			Error           string       `json:"error"`
			ExitCode        *json.Number `json:"exitCode"`
			IsRawTranscript bool         `json:"isRawTranscript"`
			OmitOutputPath  bool         `json:"omitOutputPath"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	fields := []string{"<retrieval_status>" + value.RetrievalStatus + "</retrieval_status>"}
	if task := value.Task; task != nil {
		fields = append(fields, "<task_id>"+task.TaskID+"</task_id>", "<task_type>"+task.TaskType+"</task_type>", "<status>"+task.Status+"</status>")
		if task.ExitCode != nil {
			fields = append(fields, "<exit_code>"+task.ExitCode.String()+"</exit_code>")
		}
		if strings.TrimFunc(task.Output, jsTrimSpace) != "" {
			outputPath := ""
			if path != nil && jsTextLength(task.Output) > limit && !task.OmitOutputPath {
				outputPath = path(task.TaskID)
			}
			text, err := truncateResult(task.Output, outputPath, limit, task.OmitOutputPath)
			if err != nil {
				return "", err
			}
			text = strings.TrimRightFunc(text, jsTrimSpace)
			if task.TaskType != "local_bash" {
				result, err := sanitizeResult(text, policy(), !task.IsRawTranscript)
				if err != nil {
					return "", err
				}
				text = result.Sanitized
			}
			if task.HarnessHead != "" {
				text = strings.TrimRightFunc(task.HarnessHead, jsTrimSpace) + "\n\n" + text
			}
			fields = append(fields, "<output>\n"+text+"\n</output>")
		}
		if task.Error != "" {
			fields = append(fields, "<error>"+task.Error+"</error>")
		}
	}
	return strings.Join(fields, "\n\n"), nil
}

// ToolResult uses this exact runtime's policy, never caller-provided headers or
// a process-global feature value. A nil runtime still renders tool errors.
func (r *Runtime) ToolResult(call ToolCall, data json.RawMessage, err error) json.RawMessage {
	var content any = string(data)
	if err == nil {
		policy := func() bool { return r != nil && r.options.HandbackProvenance != nil && r.options.HandbackProvenance() }
		name, _ := CanonicalToolName(call.Name)
		switch name {
		case "SendMessage":
			// Native SendMessage keeps its own block mapper: one text block with
			// the JSON minus display/hand-back fields, ordered tool_use_id first.
			if block, mapErr := sendMessageBlock(call.ID, data, policy, frameHandback); mapErr == nil {
				return block
			} else {
				err = mapErr
			}
		case "Agent":
			var state struct {
				Status string `json:"status"`
			}
			provenance := json.Unmarshal(data, &state) == nil && state.Status == "completed" && policy()
			content, err = renderAgentResult(data, provenance)
		case "TaskOutput":
			content, err = renderTaskOutputWithPolicy(data, policy, r.taskOutputMaxLength(), r.outputPath)
		case ToolSearchName:
			// Native mapToolResultToToolResultBlockParam: tool_reference
			// blocks for matches, a plain string when there are none.
			content, err = toolSearchResultContent(data)
		}
	}
	if err != nil {
		content = err.Error()
		// The native tool wrapper renders validateInput rejections as
		// <tool_use_error>message</tool_use_error>. Errors thrown inside call
		// (VD formatter), schema (zod) failures and runtime errors stay bare
		// until their native rendering is pinned.
		var rejected validationError
		if errors.As(err, &rejected) {
			content = "<tool_use_error>" + rejected.message + "</tool_use_error>"
		}
	}
	value := map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": content}
	if err != nil {
		value["is_error"] = true
	}
	raw, _ := json.Marshal(value)
	return raw
}
