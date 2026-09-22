package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// These are local task waits, not upstream request or connection deadlines.
const taskOutputDefaultWait = 30 * time.Second
const taskOutputPollInterval = 100 * time.Millisecond

// Native TASK_MAX_OUTPUT_LENGTH (yN): default 32000 UTF-16 units, capped at
// 160000; unset, unparsable or non-positive values select the default.
const taskOutputDefaultMaxLength = 32000
const taskOutputMaxLengthCap = 160000

// Native Y1/Zy cap displayed ids and descriptions at 160 UTF-16 units + "…".
const displayLimit = 160

// validationError is a native validateInput rejection. The generic tool
// wrapper renders those as <tool_use_error>message</tool_use_error>; errors
// thrown inside call reach the VD formatter, which is not pinned, so they
// keep their bare text.
type validationError struct{ message string }

func (e validationError) Error() string { return e.message }

// marshalJS encodes like JSON.stringify: no HTML escaping, and U+2028/U+2029
// stay raw (Go's encoder escapes those two unconditionally, JSON.stringify
// escapes only '"', '\\', U+0000-U+001F and lone surrogates).
func marshalJS(value any) (json.RawMessage, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if !bytes.Contains(raw, []byte(`\u202`)) {
		return raw, nil
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' || i+1 >= len(raw) {
			out = append(out, raw[i])
			continue
		}
		if i+6 <= len(raw) && raw[i+1] == 'u' && string(raw[i+2:i+5]) == "202" && (raw[i+5] == '8' || raw[i+5] == '9') {
			out = utf8.AppendRune(out, 0x2028+rune(raw[i+5]-'8'))
			i += 5
			continue
		}
		// Copy every other escape with its escaped byte so literal "\u2028"
		// text (encoded as "\\u2028") is never mistaken for the encoder's escape.
		out = append(out, raw[i], raw[i+1])
		i++
	}
	return out, nil
}

// truncateDisplay mirrors native Y1: the first 160 UTF-16 units followed by
// U+2026 when the text is longer. A surrogate pair is never split.
func truncateDisplay(text string) string {
	if jsTextLength(text) <= displayLimit {
		return text
	}
	units, end := 0, 0
	for offset, r := range text {
		size := 1
		if r > 0xffff {
			size = 2
		}
		if units+size > displayLimit {
			break
		}
		units += size
		end = offset + utf8.RuneLen(r)
	}
	return text[:end] + "\u2026"
}

// displayID mirrors native Zy for user-supplied ids in error texts: Cc/Cf
// characters are stripped (whitespace kept), whitespace runs collapse to one
// space, the result is trimmed and capped at 160 + U+2026.
func displayID(value string) string {
	var kept strings.Builder
	for _, r := range value {
		if jsTrimSpace(r) || !(unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)) {
			kept.WriteRune(r)
		}
	}
	return truncateDisplay(strings.Join(strings.FieldsFunc(kept.String(), jsTrimSpace), " "))
}

// displayDescription mirrors native Y1 for task descriptions in listings:
// Cc/Cf characters become spaces (golden: "a\x07b\x00c" -> "a b c"),
// whitespace runs collapse to one space, the result is trimmed and capped.
func displayDescription(text string) string {
	var kept strings.Builder
	for _, r := range text {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			kept.WriteByte(' ')
		} else {
			kept.WriteRune(r)
		}
	}
	return truncateDisplay(strings.Join(strings.FieldsFunc(kept.String(), jsTrimSpace), " "))
}

// ParseTaskOutputMaxLength mirrors the native TASK_MAX_OUTPUT_LENGTH reader
// (yN) for the pinned inputs: the value is trimmed, thousands separators are
// removed, it is read with Number semantics ("1e3" -> 1000, "12.5" -> 12) and
// non-numeric or non-positive values return 0 so the caller falls back to the
// default. The clamp to 160000 happens in taskOutputMaxLength. Syntaxes
// outside the golden (hex, Infinity) are not pinned.
func ParseTaskOutputMaxLength(value string) int {
	text := strings.ReplaceAll(strings.TrimFunc(value, jsTrimSpace), ",", "")
	if text == "" {
		return 0
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0
	}
	if number > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int(math.Floor(number))
}

// runningNamedAgentsLocked mirrors native Aus: agentNameRegistry entries in
// Map insertion order whose bound task is a live local agent. Keepalive
// (completed + keepaliveReasons) tasks do not exist in this runtime.
func (r *Runtime) runningNamedAgentsLocked() []string {
	var names []string
	for _, name := range r.nameOrder {
		if t := r.tasks[r.names[name]]; t != nil && t.Status == "running" {
			names = append(names, name)
		}
	}
	return names
}

// backgroundAgentsSuffixLocked mirrors native GVe: ". Running background
// agents: <id> (<Y1(description)>), ..." over live backgrounded local agents
// other than the caller and name-bound tasks. Observers and the main-session
// task never exist in this registry, so those native exclusions are implicit.
func (r *Runtime) backgroundAgentsSuffixLocked(callerID string) string {
	named := map[string]bool{}
	for _, id := range r.names {
		named[id] = true
	}
	var items []string
	for _, id := range r.order {
		t := r.tasks[id]
		if id == callerID || t.AgentType == "main-session" || !t.Background || named[id] || t.Status != "running" {
			continue
		}
		item := id
		if t.Description != "" {
			item = id + " (" + displayDescription(t.Description) + ")"
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return ""
	}
	return ". Running background agents: " + strings.Join(items, ", ")
}

// taskOutputMaxLength applies the native yN clamp to the configured value.
func (r *Runtime) taskOutputMaxLength() int {
	if r == nil || r.options.TaskOutputMaxLength == nil {
		return taskOutputDefaultMaxLength
	}
	limit := r.options.TaskOutputMaxLength()
	if limit <= 0 {
		return taskOutputDefaultMaxLength
	}
	return min(limit, taskOutputMaxLengthCap)
}

type outputInput struct {
	TaskID  string
	Block   bool
	Timeout time.Duration
}

func decodeObject(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrInvalid
	}
	for key := range fields {
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known {
			return nil, fmt.Errorf("Unrecognized key: %s", key)
		}
	}
	return fields, nil
}

func decodeOutputInput(raw json.RawMessage) (outputInput, error) {
	input := outputInput{Block: true, Timeout: taskOutputDefaultWait}
	fields, err := decodeObject(raw, "task_id", "block", "timeout")
	if err != nil {
		return input, err
	}
	if json.Unmarshal(fields["task_id"], &input.TaskID) != nil || input.TaskID == "" {
		return input, validationError{"Task ID is required"}
	}
	if value, ok := fields["block"]; ok {
		// Native _je accepts only the lowercase string forms in addition to
		// booleans. Null does not mean the omitted/default value.
		switch string(bytes.TrimSpace(value)) {
		case "true", `"true"`:
			input.Block = true
		case "false", `"false"`:
			input.Block = false
		default:
			return input, errors.New("block must be a boolean")
		}
	}
	if value, ok := fields["timeout"]; ok {
		var ms float64
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &ms) != nil || ms < 0 || ms > 600000 {
			return input, errors.New("timeout must be a number between 0 and 600000")
		}
		// The native schema accepts fractional milliseconds too.
		input.Timeout = time.Duration(ms * float64(time.Millisecond))
	}
	return input, nil
}

type outputTask struct {
	TaskID          string `json:"task_id"`
	TaskType        string `json:"task_type"`
	Status          string `json:"status"`
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	Result          string `json:"result"`
	Output          string `json:"output"`
	IsRawTranscript bool   `json:"isRawTranscript"`
	Error           string `json:"error,omitempty"`
	HarnessHead     string `json:"harnessHead,omitempty"`
}

type outputResult struct {
	RetrievalStatus string      `json:"retrieval_status"`
	Task            *outputTask `json:"task"`
}

func (r *Runtime) outputTool(ctx context.Context, caller Caller, raw json.RawMessage) (json.RawMessage, error) {
	input, err := decodeOutputInput(raw)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	progressed := false
	for {
		r.mu.Lock()
		if r.closed || r.ctx.Err() != nil || ctx.Err() != nil {
			r.mu.Unlock()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrUnavailable
		}
		t := r.tasks[input.TaskID]
		if t == nil {
			// Native Cfr: the id as typed (no Zy), then the GVe listing that
			// excludes the calling agent (VL: undefined for the main session).
			suffix := r.backgroundAgentsSuffixLocked(caller.AgentID)
			r.mu.Unlock()
			return nil, validationError{"No task found with ID: " + input.TaskID + suffix}
		}
		if input.Block && !progressed {
			// Native call yields one waiting_for_task progress item after the
			// task lookup and before aps waits, in block mode only, whether or
			// not the task is already terminal. Local agents are task_type
			// local_agent (the same constant bKe reports).
			progressed = true
			progress := ToolProgress{Type: ProgressWaitingForTask, TaskDescription: t.Description, TaskType: "local_agent"}
			r.mu.Unlock()
			r.reportProgress(ctx, progress)
			continue
		}
		terminal := t.Status != "running" && t.Status != "pending"
		if terminal || !input.Block || time.Since(started) >= input.Timeout {
			status := "success"
			if !terminal {
				status = "not_ready"
				if input.Block {
					status = "timeout"
				}
			} else if !t.Notified {
				// A queued notification already owns its immutable generation.
				// Retrieving output must not retract that earlier queue entry.
				t.Notified = true
				if err := r.saveLocked(); err != nil {
					t.Notified = false
					t.persistenceFailed = true
					r.mu.Unlock()
					return nil, err
				}
			}
			result, err := retainedOutput(t.record)
			transcript, failed := t.transcript, t.TranscriptFailed
			r.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if result.IsRawTranscript && r.options.OpenTranscript != nil {
				if transcript == nil || failed {
					return nil, ErrUnavailable
				}
				result.Output, err = transcript.ReadTail(8 << 20)
				if err != nil {
					return nil, err
				}
				result.Result = result.Output
			}
			return marshalJS(outputResult{RetrievalStatus: status, Task: result})
		}
		r.mu.Unlock()
		// aps polls the live registry, not one generation's completion promise.
		// A SendMessage resume between polls therefore remains a live task.
		timer := time.NewTimer(taskOutputPollInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-r.ctx.Done():
			timer.Stop()
			return nil, ErrUnavailable
		}
	}
}

func retainedOutput(t record) (*outputTask, error) {
	var blocks []resultText
	if len(t.Result) != 0 && string(t.Result) != "null" && json.Unmarshal(t.Result, &blocks) != nil {
		return nil, ErrInvalid
	}
	notes, body, _ := splitResultSections(blocks, float64(t.ResultSections.NoteCount), 0, t.ResultSections.Hash)
	var head []string
	for _, block := range notes {
		if t.Status != "killed" || !strings.HasPrefix(block.Text, "NOTE: this agent stopped at its ") {
			head = append(head, block.Text)
		}
	}
	harnessHead := strings.Join(head, "\n")
	var texts []string
	for _, block := range body {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" && harnessHead != "" {
		text = "[The agent produced no report text.]"
	}
	detail := t.Error
	if t.Status == "killed" {
		// Native uL records killedBy, not a task error. The runner keeps its
		// cancellation diagnostic privately; it is not bKe's error field.
		detail = ""
	}
	// With no retained report, the runtime supplies its owned JSONL tail. This
	// pure mapper cannot choose or open arbitrary caller-supplied paths.
	return &outputTask{TaskID: t.ID, TaskType: "local_agent", Status: t.Status, Description: t.Description,
		Prompt: t.Prompt, Result: text, Output: text, IsRawTranscript: text == "", Error: detail, HarnessHead: harnessHead}, nil
}

func (r *Runtime) stopTool(ctx context.Context, caller Caller, raw json.RawMessage) (json.RawMessage, error) {
	fields, err := decodeObject(raw, "task_id", "shell_id")
	if err != nil {
		return nil, err
	}
	var taskID, shellID *string
	for key, target := range map[string]**string{"task_id": &taskID, "shell_id": &shellID} {
		if value, exists := fields[key]; exists {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, target) != nil {
				return nil, ErrInvalid
			}
		}
	}
	if taskID == nil {
		taskID = shellID
	}
	if taskID == nil || *taskID == "" {
		return nil, validationError{"Missing required parameter: task_id"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	t := r.resolveStopLocked(*taskID)
	if t == nil {
		// Native c$t: Zy(id), then the Aus named-agent listing and the GVe
		// background listing. The ". Did you mean: <Zy(suggestion)>?" segment
		// is a boundary: the fuzzy _2e/Cus algorithm has not been read, so no
		// suggestion is ever produced here. Teammate listings need teams.
		message := "No task found with ID: " + displayID(*taskID)
		if names := r.runningNamedAgentsLocked(); len(names) != 0 {
			message += ". Running named agents: " + strings.Join(names, ", ")
		}
		return nil, validationError{message + r.backgroundAgentsSuffixLocked(caller.AgentID)}
	}
	if t.Status != "running" {
		// Native validateInput reports the id as typed, not the resolved
		// "name (id)" display that only the u$t errors below use.
		return nil, validationError{"Task " + *taskID + " is not running (status: " + t.Status + ")"}
	}
	display := t.ID
	if display != *taskID {
		display = displayID(*taskID) + " (" + t.ID + ")"
	}
	// Native local-agent task.agentId is its own ID, not parentAgentId or
	// ownerAgentId. Main may stop any task; a child may only stop itself.
	// This is thrown by u$t inside call, so it keeps its bare text.
	if caller.AgentID != "" && caller.AgentID != t.ID {
		return nil, fmt.Errorf("Task %s is owned by %s; agent %s cannot stop it.", display, displayID(t.ID), displayID(caller.AgentID))
	}
	previous := t.record
	t.Status, t.KilledBy, t.Pending = "killed", "parent", nil
	t.FinishedAt = r.options.Now()
	if err := r.saveLocked(); err != nil {
		t.record = previous
		t.persistenceFailed = true
		return nil, err
	}
	// The user-stop marker is deliberately unchanged. Model-issued TaskStop
	// is killedBy=parent; an explicit user stop is a separate operation.
	if t.cancel != nil {
		t.cancel()
	}
	// Native le(e) is JSON.stringify of this object; its bytes are the block content.
	return marshalJS(struct {
		Message  string `json:"message"`
		TaskID   string `json:"task_id"`
		TaskType string `json:"task_type"`
		Command  string `json:"command"`
	}{"Successfully stopped task: " + t.ID + " (" + t.Description + ")", t.ID, "local_agent", t.Description})
}

func (r *Runtime) resolveStopLocked(value string) *task {
	if t := r.tasks[value]; t != nil {
		return t
	}
	if id := r.names[value]; id != "" {
		return r.tasks[id]
	}
	// The admitted names are ASCII. Preserve registry insertion order for
	// the native case-folded fallback, including latest-wins exact bindings.
	for _, id := range r.order {
		t := r.tasks[id]
		if r.names[t.Name] == id && strings.EqualFold(t.Name, value) {
			return t
		}
	}
	return nil
}
