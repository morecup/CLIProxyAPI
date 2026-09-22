package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
)

func TestClaudeDesktopToolCallThrewClassifiesOwnedErrors(t *testing.T) {
	if claudeDesktopToolCallThrew(nil) {
		t.Fatal("a resolved call is not a thrown call")
	}
	if claudeDesktopToolCallThrew(claudetasks.NewValidateInputRejection("Task ID is required")) {
		t.Fatal("validateInput rejections are tengu_feature_sad, not bad")
	}
	if claudeDesktopToolCallThrew(context.Canceled) || claudeDesktopToolCallThrew(context.DeadlineExceeded) {
		t.Fatal("aborts never reach BUr (!Se && !oe)")
	}
	if !claudeDesktopToolCallThrew(errors.New("Task a1 is owned by a2; agent a3 cannot stop it.")) || !claudeDesktopToolCallThrew(claudetasks.ErrUnavailable) {
		t.Fatal("plain errors thrown inside call are the BUr fallthrough")
	}
	caller := claudetasks.Caller{AgentID: "main", PromptID: "prompt-1", Model: "claude-opus-5"}
	call := claudetasks.ToolCall{ID: "toolu_01", Name: "TaskOutput", MessageID: "msg_01", Input: json.RawMessage(`{"task_id":"a1"}`)}
	var executor *ClaudeExecutor
	// nil executor / nil telemetry must be no-ops for every hook.
	executor.claudeDesktopToolProgressObserver(nil, "session")(t.Context(), caller, call, claudetasks.ToolProgress{Type: claudetasks.ProgressWaitingForTask})
	executor.claudeDesktopToolLifecycleExecutedObserver(nil, "session")(t.Context(), caller, call, nil, errors.New("boom"), time.Second)
	executor.claudeDesktopSubagentEndObserver(nil, "session")(t.Context(), claudetasks.SubagentEnd{Caller: caller, TaskID: "a1", Depth: 1, LastRequestID: "req_01"})
}
