package prompt

import (
	"bytes"
	"encoding/json"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
)

// SDKResumeHistoryOptions contains owned native loader decisions. Callers must
// supply the session's provider, feature flags and path semantics; downstream
// headers cannot select these policies. This is not record/Host admission.
type SDKResumeHistoryOptions struct {
	PendingToolUseIDs      []string
	FirstPartyAPI          bool
	ResumeInterruptedTurn  bool
	TolerateContextAppends bool
	ReplyOnResume          bool
	RewindUUID             string
	MaxAgeMilliseconds     string
	ResumePrompt           string
	TerminalMCPTools       string
	Cwd                    string
	RelativePath           func(cwd, name string) (string, error)
	Now                    func() time.Time
	NewUUID                func() string
}

type sdkResumeEvent struct {
	Name       string         `json:"name"`
	Attributes map[string]any `json:"attributes"`
}

type sdkResumeDiagnostic struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SDKResumeHistory is private reconstructed content, not API wire history or
// an executable resume capability. FNo session/skill state and SessionStart
// execution still belong to the owning runtime. Never serialize this object.
type SDKResumeHistory struct {
	core             SDKResumeCore
	interruption     sdkResumeInterruption
	rescueSuppressed bool
	events           []sdkResumeEvent
	diagnostics      []sdkResumeDiagnostic
}

func (h SDKResumeHistory) Messages() []json.RawMessage { return h.core.Messages() }
func (h SDKResumeHistory) SupersededToolUses() ([]string, map[string]string) {
	return h.core.SupersededToolUses()
}
func (h SDKResumeHistory) RescueSuppressed() bool { return h.rescueSuppressed }
func (h SDKResumeHistory) Interruption() (string, json.RawMessage) {
	var raw json.RawMessage
	if h.interruption.message != nil {
		raw, _ = h.interruption.message.encode()
	}
	return h.interruption.kind, raw
}

func parseSDKResumeRows(raw []json.RawMessage) ([]*sdkResumeRow, error) {
	rows := make([]*sdkResumeRow, 0, len(raw))
	for _, value := range raw {
		row, err := parseSDKResumeRow(value)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func cloneSDKResumeRows(rows []*sdkResumeRow) ([]*sdkResumeRow, error) {
	raw := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		value, err := row.encode()
		if err != nil {
			return nil, err
		}
		raw = append(raw, value)
	}
	return parseSDKResumeRows(raw)
}

// NormalizeSDKResumeHistory follows pinned Oyt, including its conditional
// repair, staleness/rewind rescue and native factories. It does not execute
// hooks, restore external skill state, mutate protected storage or authorize
// replacement of an old query. Loaded classifier metadata is never trusted.
func NormalizeSDKResumeHistory(raw []json.RawMessage, options SDKResumeHistoryOptions) (SDKResumeHistory, error) {
	rows, err := parseSDKResumeRows(raw)
	if err != nil {
		return SDKResumeHistory{}, err
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewUUID == nil {
		options.NewUUID = uuid.NewString
	}
	if options.ResumePrompt == "" {
		options.ResumePrompt = "Continue from where you left off."
	}
	output := SDKResumeHistory{}
	length := len(rows)
	rows = sdkResumeDropRetracted(rows)
	if len(rows) != length {
		output.events = append(output.events, sdkResumeEvent{"tengu_resume_retracted_dropped", map[string]any{"dropped": length - len(rows), "chain_length": length}})
	}
	rows, err = sdkResumePrepareAttachments(rows, options)
	if err != nil {
		return SDKResumeHistory{}, err
	}
	rows = sdkResumeDropInvalidText(rows)
	if options.FirstPartyAPI {
		var removed bool
		rows, removed = sdkResumeRepairAPIBlocks(rows)
		if removed {
			output.diagnostics = append(output.diagnostics, sdkResumeDiagnostic{"transcript_api_invalid_blocks", "blocks_dropped"})
		}
	}
	for _, row := range rows {
		if row.kind() != "user" {
			continue
		}
		if _, exists := row.fields["permissionMode"]; exists {
			switch sdkResumeString(row.fields, "permissionMode") {
			case "acceptEdits", "auto", "bypassPermissions", "default", "dontAsk", "plan":
			default:
				delete(row.fields, "permissionMode")
			}
		}
		delete(row.fields, "promptId")
	}
	rewind := sdkResumeAtRewind(rows, options)
	stale := sdkResumeStale(rows, options)
	rescue := options.ResumeInterruptedTurn && len(options.PendingToolUseIDs) == 0 && !options.ReplyOnResume
	work, err := cloneSDKResumeRows(rows)
	if err != nil {
		return SDKResumeHistory{}, err
	}
	coreOptions := SDKResumeCoreOptions{PendingToolUseIDs: options.PendingToolUseIDs, TolerateContextAppends: options.TolerateContextAppends,
		DropSiblingBlocks: rescue && !stale, ShutdownUnwindResultsDoNotResolve: rescue && !stale}
	work, ids, names := sdkResumeReconcileTools(work, coreOptions)
	if !rescue || stale {
		ids, names = nil, nil
	}
	work = sdkResumeFilterWhitespace(sdkResumeHistoryFilterThinking(work, &output.events))
	state := sdkResumeInterruption{kind: "none"}
	if len(options.PendingToolUseIDs) == 0 && !options.ReplyOnResume {
		state = sdkResumeClassify(work, len(ids) > 0, options, &output.events)
	}
	recheck := rescue && stale && state.kind == "none"
	if recheck {
		alternative, err := cloneSDKResumeRows(rows)
		if err != nil {
			return SDKResumeHistory{}, err
		}
		alternative, superseded, _ := sdkResumeReconcileTools(alternative, SDKResumeCoreOptions{DropSiblingBlocks: true,
			ShutdownUnwindResultsDoNotResolve: true, TolerateContextAppends: options.TolerateContextAppends})
		if len(superseded) > 0 {
			state = sdkResumeClassify(sdkResumeFilterWhitespace(sdkResumeHistoryFilterThinking(alternative, &output.events)), true, options, &output.events)
		}
	}
	rewindSuppressed := state.kind != "none" && (rewind || sdkResumeAtRewind(work, options))
	staleSuppressed := false
	if !rewindSuppressed && state.kind != "none" {
		if len(ids) > 0 || recheck {
			staleSuppressed = stale
		} else {
			staleSuppressed = sdkResumeStale(work, options)
		}
	}
	if staleSuppressed && options.ResumeInterruptedTurn {
		output.events = append(output.events, sdkResumeEvent{"tengu_resume_stale_turn_suppressed", map[string]any{"kind": state.kind}})
	}
	output.rescueSuppressed = rewindSuppressed || staleSuppressed
	if output.rescueSuppressed {
		state = sdkResumeInterruption{kind: "none"}
	} else if state.kind == "interrupted_turn" {
		row, err := sdkResumeContinueMessage(options)
		if err != nil {
			return SDKResumeHistory{}, err
		}
		work = append(work, row)
		state = sdkResumeInterruption{kind: "interrupted_prompt", message: row}
	}
	if !options.ReplyOnResume {
		for i := len(work) - 1; i >= 0; i-- {
			if work[i].kind() == "system" || work[i].kind() == "progress" {
				continue
			}
			if work[i].kind() == "user" {
				row, err := sdkResumeNoResponseMessage(options)
				if err != nil {
					return SDKResumeHistory{}, err
				}
				work = append(work, nil)
				copy(work[i+2:], work[i+1:])
				work[i+1] = row
			}
			break
		}
	}
	output.interruption = state
	output.core = SDKResumeCore{superseded: ids, toolNames: names}
	for _, row := range work {
		value, err := row.encode()
		if err != nil {
			return SDKResumeHistory{}, err
		}
		output.core.rows = append(output.core.rows, value)
	}
	return output, nil
}

func sdkResumePrepareAttachments(rows []*sdkResumeRow, options SDKResumeHistoryOptions) ([]*sdkResumeRow, error) {
	valid := make([]*sdkResumeRow, 0, len(rows))
	for _, row := range rows {
		if row.kind() == "attachment" && !sdkResumeValidAttachment(row.fields["attachment"]) {
			continue
		}
		valid = append(valid, row)
	}
	valid = sdkResumeDropPlaceholders(valid, options)
	kept := make([]*sdkResumeRow, 0, len(valid))
	for _, row := range valid {
		if row.kind() == "attachment" {
			var payload map[string]json.RawMessage
			_ = json.Unmarshal(row.fields["attachment"], &payload)
			switch sdkResumeString(payload, "type") {
			case "compaction_reminder", "companion_intro", "echo_activities", "pen_mode_enter", "pen_mode_exit", "verify_plan_reminder", "fold_nudge", "context_tip", "new_file", "new_directory":
				continue
			}
			if _, exists := payload["displayPath"]; !exists {
				name := ""
				for _, key := range []string{"filename", "path", "skillDir"} {
					if value, valid := sdkWireString(payload[key]); valid {
						name = value
						break
					}
				}
				if name != "" {
					if options.RelativePath == nil {
						return nil, ErrSDKSessionUnavailable
					}
					display, err := options.RelativePath(options.Cwd, name)
					if err != nil {
						return nil, ErrSDKSessionInvalid
					}
					payload["displayPath"], _ = json.Marshal(display)
					row.fields["attachment"], _ = json.Marshal(payload)
				}
			}
		}
		kept = append(kept, row)
	}
	return kept, nil
}

func sdkResumeValidAttachment(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	kind, valid := sdkWireString(fields["type"])
	if !valid {
		return false
	}
	var values []json.RawMessage
	array := func(key string) bool {
		raw := bytes.TrimSpace(fields[key])
		return len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &values) == nil
	}
	switch kind {
	case "invoked_skills":
		if !array("skills") {
			return false
		}
		for _, value := range values {
			v := bytes.TrimSpace(value)
			if len(v) == 0 || v[0] != '{' && v[0] != '[' {
				return false
			}
		}
	case "hook_success":
		_, valid := sdkWireString(fields["content"])
		return valid
	case "skill_listing":
		if _, exists := fields["names"]; !exists {
			return true
		}
		if !array("names") || len(values) > 4096 {
			return false
		}
		for _, value := range values {
			text, valid := sdkWireString(value)
			if !valid || len(utf16.Encode([]rune(text))) > 512 {
				return false
			}
		}
	case "hook_additional_context":
		if !array("content") {
			return false
		}
		for _, value := range values {
			if _, valid := sdkWireString(value); !valid {
				return false
			}
		}
	}
	return true
}

func sdkResumeFactoryRow(value any) (*sdkResumeRow, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrSDKSessionInvalid
	}
	return parseSDKResumeRow(raw)
}

func sdkResumeContinueMessage(options SDKResumeHistoryOptions) (*sdkResumeRow, error) {
	return sdkResumeFactoryRow(map[string]any{"type": "user", "uuid": options.NewUUID(), "timestamp": nativeContentTimestamp(options.Now()), "isMeta": true,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": options.ResumePrompt}}}})
}

func sdkResumeNoResponseMessage(options SDKResumeHistoryOptions) (*sdkResumeRow, error) {
	id := options.NewUUID()
	at := nativeContentTimestamp(options.Now())
	messageID := options.NewUUID()
	return sdkResumeFactoryRow(map[string]any{"type": "assistant", "uuid": id, "timestamp": at, "isApiErrorMessage": false,
		"message": map[string]any{"diagnostics": nil, "id": messageID, "container": nil, "model": "<synthetic>", "role": "assistant",
			"stop_details": nil, "stop_reason": "stop_sequence", "stop_sequence": "", "type": "message", "context_management": nil,
			"content": []any{map[string]any{"type": "text", "text": "No response requested."}},
			"usage": map[string]any{"output_tokens_details": nil, "input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
				"server_tool_use": map[string]any{"web_search_requests": 0, "web_fetch_requests": 0}, "service_tier": nil,
				"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 0}, "inference_geo": nil, "iterations": nil, "speed": nil}}})
}
