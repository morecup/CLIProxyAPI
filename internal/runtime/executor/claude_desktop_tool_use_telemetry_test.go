package executor

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
)

func TestClaudeDesktopToolUseOutcomeClassifiesRuntimeResults(t *testing.T) {
	caller := claudetasks.Caller{AgentID: "main", PromptID: "prompt-1", Model: "claude-opus-5", Depth: 0}
	call := claudetasks.ToolCall{ID: "toolu_01", Name: "TaskStop", MessageID: "msg_01", Input: json.RawMessage(`{"task_id": "a1"}`)}
	data := json.RawMessage(`{"message":"Successfully stopped task: a1 (desc)","task_id":"a1","task_type":"local_agent","command":"desc"}`)
	success := claudeDesktopToolUseOutcome(caller, call, data, nil, 1500*time.Millisecond)
	if !success.Success || success.ValidationRejected || success.DurationMs != 1500 || success.ToolUse.MessageID != "msg_01" || success.ToolUse.ToolName != "TaskStop" {
		t.Fatalf("success outcome = %+v", success)
	}
	// TaskStop content is JSON.stringify(data): the size is the JSON text length.
	if success.ResultSizeBytes != len(string(data)) {
		t.Fatalf("toolResultSizeBytes = %d, want %d", success.ResultSizeBytes, len(string(data)))
	}
	rejected := claudeDesktopToolUseOutcome(caller, call, nil, claudetasks.NewValidateInputRejection("Missing required parameter: task_id"), time.Millisecond)
	if rejected.Success || !rejected.ValidationRejected {
		t.Fatalf("rejection outcome = %+v", rejected)
	}
	thrown := claudeDesktopToolUseOutcome(caller, call, nil, errors.New("boom"), time.Millisecond)
	if thrown.Success || thrown.ValidationRejected || thrown.InputValidationFailed {
		t.Fatalf("thrown outcome = %+v", thrown)
	}
	var executor *ClaudeExecutor
	// nil executor / nil telemetry must be a no-op.
	executor.claudeDesktopToolExecutedObserver(nil, "session")(t.Context(), caller, call, data, nil, time.Second)
}
