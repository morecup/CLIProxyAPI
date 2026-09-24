package helps

import (
	"context"
	"errors"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type ClaudeDesktopNativeCompactionParams struct {
	Summary   ClaudeDesktopReactiveSummaryParams
	Owner     *claudeprompt.Request
	Telemetry *claudetelemetry.RequestSpan
	// Apply must prepare the final continuation and call Application.Commit.
	// Returning nil without a committed native application never emits success.
	Apply func(*claudeprompt.SDKCompactionApplication) error
}

type ClaudeDesktopNativeCompactionResult struct {
	Applied bool
	Reason  string
}

// RunClaudeDesktopNativeCompaction is the shared in-process lifecycle for an
// explicit manual command, a native threshold decision, or claimed PTL recovery.
// It does not manufacture a threshold decision from a wire header. Native
// callers supply an already-bound context owner and the real adoption callback.
func RunClaudeDesktopNativeCompaction(ctx context.Context, executor ClaudeDesktopSummaryExecutor, params ClaudeDesktopNativeCompactionParams) (ClaudeDesktopNativeCompactionResult, error) {
	var outcome ClaudeDesktopNativeCompactionResult
	summary := params.Summary
	if ctx == nil || executor == nil || params.Apply == nil || summary.Auth == nil || summary.Bundle == nil ||
		!summary.Origin.Valid() || !summary.View.CompactionOwnedBy(params.Owner) {
		return outcome, ErrClaudeDesktopRecoveryContext
	}
	if summary.Origin.Kind == "reactive" && !summary.View.ReactiveFailureOwnedBy(params.Owner) {
		return outcome, ErrClaudeDesktopRecoveryContext
	}
	native, err := ResolveClaudeDesktopRecoveryContext(ctx, cliproxyexecutor.ClaudeDesktopSessionBinding{
		AccountID: summary.Auth.ID, ProfileID: summary.Bundle.ProfileID, Egress: summary.Auth.ProxyURL, SessionID: summary.SessionID}, summary.View)
	if err != nil || native == nil {
		return outcome, ErrClaudeDesktopRecoveryContext
	}
	if err = ctx.Err(); err != nil {
		return outcome, err
	}
	operation := params.Telemetry.BeginSDKCompaction(summary.View, summary.Origin)
	defer operation.Close()
	var instructions *string
	if summary.CustomInstructions != "" {
		value := summary.CustomInstructions
		instructions = &value
	}
	pre, err := claudeprompt.RunSDKPreCompactHooks(ctx, native.Hooks, summary.Origin.HookTrigger(), instructions, false)
	if err != nil {
		operation.RecordContextUnavailable()
		return outcome, err
	}
	if pre.BlockedBy != "" {
		return ClaudeDesktopNativeCompactionResult{Reason: "hook_blocked"}, nil
	}
	if pre.NewCustomInstructions != "" {
		if summary.CustomInstructions != "" {
			summary.CustomInstructions += "\n\n"
		}
		summary.CustomInstructions += pre.NewCustomInstructions
	}
	// Freeze the callback before installing the combined observer.
	observe := summary.ObserveAttempt
	summary.ObserveAttempt = func(attempt claudeprompt.SDKReactiveAttempt) {
		operation.ObserveAttempt(attempt)
		if observe != nil {
			observe(attempt)
		}
	}
	result, err := RunClaudeDesktopReactiveSummary(ctx, executor, summary)
	defer result.Payload.Discard()
	if err != nil {
		operation.RecordContextUnavailable()
		return outcome, err
	}
	var preTokens *int64
	if estimate := summary.View.History().TokenEstimate; estimate.Known {
		value := estimate.Tokens
		preTokens = &value
	}
	if !result.ReadyToApply {
		var truncations *int
		if result.SplitKind != "" {
			value := result.HeadTruncations
			truncations = &value
		}
		operation.RecordFailure(claudetelemetry.SDKReactiveCompactionFailure{Reason: result.Reason, Attempts: result.Attempts,
			TotalGroups: result.TotalGroups, SplitKind: result.SplitKind, HeadTruncations: truncations, PreCompactTokens: preTokens, Status: result.Status})
		return ClaudeDesktopNativeCompactionResult{Reason: result.Reason}, nil
	}
	var diagnostics ClaudeDesktopCompactionDiagnostics
	if native.Diagnostics != nil {
		diagnostics, err = native.Diagnostics(ctx, append([]claudeprompt.SDKHistoryMessage(nil), result.Preserve...))
		if err != nil {
			// Optional diagnostics must not prevent a valid context recovery.
			diagnostics = ClaudeDesktopCompactionDiagnostics{}
			operation.RecordContextUnavailable()
		}
	} else {
		operation.RecordContextUnavailable()
	}
	if diagnostics.Breakdown == nil || diagnostics.CacheCold == nil || diagnostics.KeptThinkingBlockCount == nil || diagnostics.KeptThinkingStripped == nil {
		operation.RecordContextUnavailable()
	}
	preservedUUIDs, preservedUUIDsKnown := summary.View.PreservedTranscriptUUIDCount(result.Preserve)
	if count := diagnostics.PreservedUUIDCount; count != nil && *count >= 0 && *count <= len(result.Preserve) {
		preservedUUIDs, preservedUUIDsKnown = *count, true
	}
	if !preservedUUIDsKnown {
		operation.RecordContextUnavailable()
	}
	ops, err := native.SnapshotAndReset(ctx, append([]claudeprompt.SDKHistoryMessage(nil), result.Preserve...))
	if err != nil {
		operation.RecordContextUnavailable()
		return outcome, err
	}
	restored, err := result.Payload.Application.RestoreContext(ctx, claudeprompt.SDKCompactionRestoreParams{
		Owner: params.Owner, Trigger: summary.Origin.HookTrigger(), Summary: result.Payload.Text, Operations: ops,
		PostHooks: native.Hooks, Normalize: native.Normalize, RemoteEnabled: native.RemoteEnabled})
	if err != nil || restored.RestoreError != nil || restored.FallbackError != nil {
		operation.RecordContextUnavailable()
	}
	if err != nil {
		return outcome, err
	}
	if err = ctx.Err(); err != nil {
		return outcome, err
	}
	if err = params.Apply(result.Payload.Application); err != nil {
		operation.RecordContextUnavailable()
		return outcome, err
	}
	if !result.Payload.Application.CommittedNative() {
		operation.RecordContextUnavailable()
		return outcome, errors.New("native compaction callback did not commit its application")
	}
	postTokens := result.Payload.Application.PostTokens()
	operation.RecordSuccess(claudetelemetry.SDKReactiveCompactionSuccess{
		Attempts: result.Attempts, GroupsPreserved: result.GroupsPreserved, TotalGroups: result.TotalGroups,
		PreservedUUIDCount: preservedUUIDs, PreservedUUIDCountKnown: &preservedUUIDsKnown, PreservedMessageCount: len(result.Preserve),
		ForkAssistantMessageCount: result.Payload.AssistantMessages, RestoredItemCount: len(restored.Attachments) + len(restored.HookResults),
		SplitKind: result.SplitKind, HeadTruncations: result.HeadTruncations, PreCompactTokens: preTokens, PostCompactTokens: &postTokens,
		Usage: result.Payload.Usage, UsageKnown: result.Payload.UsageKnown, Breakdown: diagnostics.Breakdown, CacheCold: diagnostics.CacheCold,
		KeptThinkingBlockCount: diagnostics.KeptThinkingBlockCount, KeptThinkingStripped: diagnostics.KeptThinkingStripped,
		KeptThinkingStripDecidedBy: diagnostics.KeptThinkingStripDecidedBy,
	})
	return ClaudeDesktopNativeCompactionResult{Applied: true}, nil
}
