package executor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Native tool lifecycle telemetry of the owned tool family: the progress
// items a tool yields during call (tengu_tool_use_progress), the BUr
// fallthrough of a call that threw (tengu_feature_bad tool_call_threw) and
// the subagent finalize cache hint (tengu_cache_eviction_hint subagent_end).
// All three are recorded span-less against the owned query's SDK session.

// claudeDesktopToolCallThrew reports whether err is an error thrown inside
// call that the native gOs catch classifies through BUr: not a validateInput
// rejection (tengu_feature_sad before the call) and not an abort (!Se && !oe:
// the abort signal did not fire and the error is not an abort error).
func claudeDesktopToolCallThrew(err error) bool {
	if err == nil || claudetasks.IsValidateInputRejection(err) {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// claudeDesktopToolProgressObserver returns the claudetasks.Options.ToolProgress
// hook of one owned query.
func (e *ClaudeExecutor) claudeDesktopToolProgressObserver(auth *cliproxyauth.Auth, sessionID string) func(context.Context, claudetasks.Caller, claudetasks.ToolCall, claudetasks.ToolProgress) {
	return func(ctx context.Context, caller claudetasks.Caller, call claudetasks.ToolCall, _ claudetasks.ToolProgress) {
		if e == nil || e.desktopTelemetry == nil || auth == nil {
			return
		}
		use := claudetelemetry.ToolUse{MessageID: call.MessageID, ToolName: call.Name, Input: call.Input, Depth: caller.Depth}
		if err := e.desktopTelemetry.RecordSDKToolUseProgress(context.WithoutCancel(ctx), auth, sessionID, caller.Model, caller.PromptID, use); err != nil {
			helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: tool progress telemetry was not persisted")
		}
	}
}

// claudeDesktopToolLifecycleExecutedObserver returns the ToolExecuted hook
// that first records the wrapper events (claudeDesktopToolExecutedObserver:
// allowed, feature_ok, success or feature_sad) and then, for a call that
// threw, the native BUr fallthrough tengu_feature_bad tool_call_threw.
func (e *ClaudeExecutor) claudeDesktopToolLifecycleExecutedObserver(auth *cliproxyauth.Auth, sessionID string) func(context.Context, claudetasks.Caller, claudetasks.ToolCall, json.RawMessage, error, time.Duration) {
	wrapper := e.claudeDesktopToolExecutedObserver(auth, sessionID)
	return func(ctx context.Context, caller claudetasks.Caller, call claudetasks.ToolCall, data json.RawMessage, err error, duration time.Duration) {
		wrapper(ctx, caller, call, data, err, duration)
		if e == nil || e.desktopTelemetry == nil || auth == nil || !claudeDesktopToolCallThrew(err) {
			return
		}
		if errRecord := e.desktopTelemetry.RecordSDKFeatureBad(context.WithoutCancel(ctx), auth, sessionID, caller.Model, caller.PromptID, call.Name); errRecord != nil {
			helps.LogWithRequestID(ctx).WithError(errRecord).Warn("claude desktop: feature bad telemetry was not persisted")
		}
	}
}

// claudeDesktopSubagentEndObserver returns the claudetasks.Options.SubagentEnd
// hook: the DMt cache hint with the child's last upstream request id. The
// telemetry manager emits nothing when the id is unknown.
func (e *ClaudeExecutor) claudeDesktopSubagentEndObserver(auth *cliproxyauth.Auth, sessionID string) func(context.Context, claudetasks.SubagentEnd) {
	return func(ctx context.Context, end claudetasks.SubagentEnd) {
		if e == nil || e.desktopTelemetry == nil || auth == nil {
			return
		}
		hint := claudetelemetry.CacheEvictionHint{Scope: claudetelemetry.CacheEvictionScopeSubagentEnd, LastRequestID: end.LastRequestID}
		if err := e.desktopTelemetry.RecordSDKCacheEvictionHint(context.WithoutCancel(ctx), auth, sessionID, end.Caller.Model, end.Caller.PromptID, hint); err != nil {
			helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: cache eviction hint telemetry was not persisted")
		}
	}
}
