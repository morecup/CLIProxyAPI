package tasks

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/transcript"
)

// Observation hooks of the owned tool family that feed the native tool
// lifecycle telemetry (Claude Code 2.1.247, module _448.js):
//   - ToolProgress: the generic wrapper (pOs) emits tengu_tool_use_progress
//     for every progress item a tool yields whose data.type is not
//     "tool_heartbeat". The TaskOutput tool yields exactly one
//     {type:"waiting_for_task", taskDescription, taskType} item in block mode
//     after the task lookup and before the aps wait, whether or not the task
//     is already terminal.
//   - SubagentEnd: the agent finalize (DMt) emits tengu_cache_eviction_hint
//     {scope:"subagent_end", last_request_id} when the subagent's last
//     assistant message carries a requestId (the upstream request-id of the
//     child's last API response).

// ToolProgress is the data object of one native progress item.
type ToolProgress struct {
	Type            string `json:"type"`
	TaskDescription string `json:"taskDescription"`
	TaskType        string `json:"taskType"`
}

// ProgressWaitingForTask is the native TaskOutput block-mode progress type.
const ProgressWaitingForTask = "waiting_for_task"

// SubagentEnd describes one completed child generation the way DMt sees it.
type SubagentEnd struct {
	// Caller is the identity of the launching agent (the same caller the
	// Agent tool call was observed with); Model falls back to the child's
	// model for records restored from the store.
	Caller Caller
	// TaskID is the child agent id; Depth its spawn depth.
	TaskID string
	Depth  int
	// LastRequestID is the upstream request-id of the child's last assistant
	// transcript row; empty when the gateway never observed one (native
	// h.requestId undefined: no hint is emitted).
	LastRequestID string
}

type progressCallKey struct{}

type progressCall struct {
	caller Caller
	call   ToolCall
}

// withProgressCall scopes the tool call whose progress items are observed.
func withProgressCall(ctx context.Context, caller Caller, call ToolCall) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, progressCallKey{}, progressCall{caller: caller, call: call})
}

// reportProgress hands one non-heartbeat progress item to Options.ToolProgress.
// It must be called without holding r.mu; the hook must not block.
func (r *Runtime) reportProgress(ctx context.Context, data ToolProgress) {
	if r == nil || r.options.ToolProgress == nil || ctx == nil {
		return
	}
	scope, ok := ctx.Value(progressCallKey{}).(progressCall)
	if !ok {
		return
	}
	r.options.ToolProgress(ctx, scope.caller, scope.call, data)
}

// lastAssistantRequestID mirrors native vb(messages).requestId over one
// observed batch of transcript rows: the request id of the last assistant
// row (possibly empty), or previous when the batch holds no assistant row.
func lastAssistantRequestID(rows []transcript.Message, previous string) string {
	for index := len(rows) - 1; index >= 0; index-- {
		if rows[index].Type == "assistant" {
			return rows[index].RequestID
		}
	}
	return previous
}

// subagentEndLocked builds the SubagentEnd of a generation that finished
// without a run error (native DMt runs only when the loop produced its
// result; failed and killed generations never reach it). nil when the hook
// is not installed or the completion is not a DMt completion.
func (r *Runtime) subagentEndLocked(t *task, runErr error) *SubagentEnd {
	if r.options.SubagentEnd == nil || runErr != nil || t.Status != "completed" {
		return nil
	}
	model := t.parentModel
	if model == "" {
		model = t.Model
	}
	return &SubagentEnd{
		Caller:        Caller{AgentID: t.ParentAgentID, PromptID: t.ParentPromptID, Model: model, Depth: t.Depth - 1},
		TaskID:        t.ID,
		Depth:         t.Depth,
		LastRequestID: t.lastRequestID,
	}
}

// notifySubagentEnd runs the SubagentEnd hook outside r.mu, on the run
// goroutine, before the generation's done channel closes so a foreground
// Agent call observes it before its own ToolExecuted hook (native order:
// DMt inside call, then the wrapper's success events).
func (r *Runtime) notifySubagentEnd(ctx context.Context, end *SubagentEnd) {
	if r == nil || end == nil || r.options.SubagentEnd == nil {
		return
	}
	r.options.SubagentEnd(context.WithoutCancel(ctx), *end)
}
