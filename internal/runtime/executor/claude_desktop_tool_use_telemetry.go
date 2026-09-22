package executor

import (
	"context"
	"encoding/json"
	"time"

	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Native tool-wrapper telemetry for the owned tool family (Agent, SendMessage,
// TaskOutput, TaskStop, ToolSearch). The tasks runtime invokes the observer
// after every ExecuteTool; the wrapper events are recorded span-less against
// the owned query's SDK session because the tool runs between two owned
// requests.

// claudeDesktopToolUseOutcome maps one runtime execution onto the native
// wrapper outcome: a resolved call is a success whose toolResultSizeBytes is
// measured on the runtime's own tool_result rendering; a validateInput
// rejection (rendered <tool_use_error>) is the tengu_feature_sad path.
func claudeDesktopToolUseOutcome(caller claudetasks.Caller, call claudetasks.ToolCall, data json.RawMessage, err error, duration time.Duration) claudetelemetry.ToolUseOutcome {
	outcome := claudetelemetry.ToolUseOutcome{
		ToolUse:    claudetelemetry.ToolUse{MessageID: call.MessageID, ToolName: call.Name, Input: call.Input, Depth: caller.Depth},
		DurationMs: duration.Milliseconds(),
	}
	switch {
	case err == nil:
		outcome.Success = true
		outcome.ResultSizeBytes = claudetelemetry.SDKToolResultSizeBytes(claudetasks.ToolResult(call, data, nil))
	case claudetasks.IsValidateInputRejection(err):
		outcome.ValidationRejected = true
	}
	return outcome
}

// claudeDesktopToolExecutedObserver returns the claudetasks.Options.ToolExecuted
// hook of one owned query. sessionID is the owned query's SDK session
// (claudeDesktopRuntimeFacts.SessionID).
func (e *ClaudeExecutor) claudeDesktopToolExecutedObserver(auth *cliproxyauth.Auth, sessionID string) func(context.Context, claudetasks.Caller, claudetasks.ToolCall, json.RawMessage, error, time.Duration) {
	return func(ctx context.Context, caller claudetasks.Caller, call claudetasks.ToolCall, data json.RawMessage, err error, duration time.Duration) {
		if e == nil || e.desktopTelemetry == nil || auth == nil {
			return
		}
		outcome := claudeDesktopToolUseOutcome(caller, call, data, err, duration)
		if errRecord := e.desktopTelemetry.RecordSDKToolUse(context.WithoutCancel(ctx), auth, sessionID, caller.Model, caller.PromptID, outcome); errRecord != nil {
			helps.LogWithRequestID(ctx).WithError(errRecord).Warn("claude desktop: tool use telemetry was not persisted")
		}
	}
}
