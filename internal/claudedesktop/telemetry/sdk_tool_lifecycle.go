package telemetry

import (
	"context"
	"fmt"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Tool lifecycle events of the pinned SDK (Claude Code 2.1.247; pins in
// testdata/sdk-telemetry-tool-lifecycle-native.json):
//   - tengu_tool_use_progress (module _448.js, pOs): one event per progress
//     item a tool yields during call whose data.type !== "tool_heartbeat";
//     {messageID, toolName, isMcp, queryChainId, queryDepth, [mcpServerType],
//     [mcpServerBaseUrl], [requestId], ...Aw(name, mcpNameLoggable),
//     ...mcpInfo.pluginTelemetry}. The owned TaskOutput tool yields exactly
//     one waiting_for_task item in block mode before waiting.
//   - tengu_cache_eviction_hint (module _448.js, DMt agent finalize):
//     {scope:"subagent_end", last_request_id} after tengu_agent_tool_completed
//     when the subagent's last assistant message has a requestId.
//   - tengu_feature_bad (module _823.js Q, called from the _448.js gOs call
//     catch): {feature_name, error_code} for errors thrown inside call that
//     BUr classifies as not sad; plain Errors of the owned tools fall through
//     to {code:"tool_call_threw", isSad:false}. Not emitted when the abort
//     signal fired or the error is an abort error (!Se && !oe).
//   - tengu_resume_print (module chunk-zhnz59d4.js, print / sdk_url resume
//     lane): empty metadata (logger prefix only), emitted inside the same
//     `if(r.resume)` try block before the session lookup, so before
//     tengu_session_resumed of the same lane.
//
// The native logger (_675.js) prepends subscription_type and cc_prompt_id.
const (
	FactSDKToolUseProgress   = "tool_use_progress"
	FactSDKCacheEvictionHint = "cache_eviction_hint"
	FactSDKFeatureBad        = "feature_bad"
	FactSDKResumePrint       = "resume_print"

	// CacheEvictionScopeSubagentEnd is the DMt scope constant; the other
	// native scopes (session_end, conversation_clear) are not owned moments.
	CacheEvictionScopeSubagentEnd = "subagent_end"
	// ToolFeatureErrorCallThrew is the BUr fallthrough code (isSad false).
	ToolFeatureErrorCallThrew = "tool_call_threw"
)

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKToolUseProgress:   "tengu_tool_use_progress",
		FactSDKCacheEvictionHint: "tengu_cache_eviction_hint",
		FactSDKFeatureBad:        "tengu_feature_bad",
		FactSDKResumePrint:       "tengu_resume_print",
	})
	// Native _675.js Datadog mirror set (nK) contains tengu_feature_bad only.
	registerExecutableEvents(datadogLogsRole, map[string]string{
		FactSDKFeatureBad: "tengu_feature_bad",
	})
}

// CacheEvictionHint mirrors the DMt inputs: the scope constant and the last
// assistant message's requestId of the finished subagent.
type CacheEvictionHint struct {
	Scope         string
	LastRequestID string
}

type sdkToolUseProgressMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	MessageID        string `json:"messageID,omitempty"`
	ToolName         string `json:"toolName"`
	IsMCP            bool   `json:"isMcp"`
	QueryChainID     string `json:"queryChainId,omitempty"`
	QueryDepth       *int   `json:"queryDepth,omitempty"`
}

type sdkCacheEvictionHintMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	Scope            string `json:"scope"`
	LastRequestID    string `json:"last_request_id"`
}

// sdkResumePrintMetadata is the native empty object: only the logger prefix.
type sdkResumePrintMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
}

func toolUseProgressMetadata(subscription, promptID string, use ToolUse, chainID string, depth *int) sdkToolUseProgressMetadata {
	return sdkToolUseProgressMetadata{
		SubscriptionType: subscription,
		PromptID:         promptID,
		MessageID:        sdkOptionalLoggableID(use.MessageID),
		ToolName:         SDKLoggableToolName(use.ToolName),
		// e.isMcp ?? false: the owned family has no MCP tool.
		IsMCP:        false,
		QueryChainID: chainID,
		QueryDepth:   depth,
	}
}

func cacheEvictionHintMetadata(subscription, promptID string, hint CacheEvictionHint) sdkCacheEvictionHintMetadata {
	return sdkCacheEvictionHintMetadata{
		SubscriptionType: subscription,
		PromptID:         promptID,
		Scope:            hint.Scope,
		LastRequestID:    SDKLoggableID(hint.LastRequestID),
	}
}

// recordSDKToolLineageFact is recordSDKFact with the owned query lineage the
// span-less tool emitters derive for depth-0 callers (exactly as
// RecordSDKToolUse does); deeper callers omit queryChainId/queryDepth.
func (m *Manager) recordSDKToolLineageFact(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, depth int, fact string, build func(subscription, promptID, chainID string, queryDepth *int) any) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if m.ctx.Err() != nil {
		return fmt.Errorf("Claude Desktop telemetry owner is unavailable")
	}
	session, model = strings.TrimSpace(session), strings.TrimSpace(model)
	if session == "" || model == "" {
		return fmt.Errorf("Claude Desktop SDK event %q identity is incomplete", fact)
	}
	betas, known := m.sdkProfile.InputBetaHeader(model)
	if !known {
		return fmt.Errorf("Claude Desktop SDK event %q model %q has no profiled beta header", fact, model)
	}
	worker, errWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errWorker != nil {
		return errWorker
	}
	facts := RequestFacts{SessionID: session, Model: model, Betas: betas, PromptID: strings.TrimSpace(promptID)}
	var chainID string
	var queryDepth *int
	if depth == 0 {
		chainID, queryDepth = sdkQueryLineage(worker, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: facts.PromptID})
	}
	metadata := build(subscriptionType(worker.authSnapshot()), facts.PromptID, chainID, queryDepth)
	if errEnqueue := m.enqueueSDKEvent(worker, fact, facts, metadata); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		return errEnqueue
	}
	return nil
}

// RecordSDKToolUseProgress emits tengu_tool_use_progress for one non-heartbeat
// progress item of an owned tool call (span-less, between two owned requests).
func (m *Manager) RecordSDKToolUseProgress(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, use ToolUse) error {
	if strings.TrimSpace(use.ToolName) == "" {
		return fmt.Errorf("Claude Desktop SDK tool progress tool name is missing")
	}
	return m.recordSDKToolLineageFact(ctx, auth, session, model, promptID, use.Depth, FactSDKToolUseProgress, func(subscription, promptID, chainID string, depth *int) any {
		return toolUseProgressMetadata(subscription, promptID, use, chainID, depth)
	})
}

// RecordSDKFeatureBad emits tengu_feature_bad {feature_name, error_code:
// "tool_call_threw"} for an owned tool whose call threw a plain error (the
// BUr fallthrough). Validate-input rejections and aborts are not feature_bad.
func (m *Manager) RecordSDKFeatureBad(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID, toolName string) error {
	if strings.TrimSpace(toolName) == "" {
		return fmt.Errorf("Claude Desktop SDK feature bad tool name is missing")
	}
	return m.recordSDKFact(ctx, auth, session, model, promptID, FactSDKFeatureBad, func(subscription, promptID string) any {
		return sdkFeatureMetadata{SubscriptionType: subscription, PromptID: promptID, FeatureName: SDKToolFeatureName(toolName), ErrorCode: ToolFeatureErrorCallThrew}
	})
}

// RecordSDKCacheEvictionHint emits tengu_cache_eviction_hint for a finished
// subagent whose last assistant message carried a requestId. An empty id is
// the native undefined: nothing is emitted and no id is invented.
func (m *Manager) RecordSDKCacheEvictionHint(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, hint CacheEvictionHint) error {
	if hint.Scope != CacheEvictionScopeSubagentEnd {
		return fmt.Errorf("Claude Desktop SDK cache eviction scope %q is not an owned moment", hint.Scope)
	}
	if strings.TrimSpace(hint.LastRequestID) == "" {
		return nil
	}
	return m.recordSDKFact(ctx, auth, session, model, promptID, FactSDKCacheEvictionHint, func(subscription, promptID string) any {
		return cacheEvictionHintMetadata(subscription, promptID, hint)
	})
}

// RecordSDKResumePrint emits tengu_resume_print (empty native metadata) at
// the print / sdk_url resume lane moment; RecordSDKSessionResumed calls it
// first so the native relative order (print, then resumed) holds.
func (m *Manager) RecordSDKResumePrint(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string) error {
	return m.recordSDKFact(ctx, auth, session, model, promptID, FactSDKResumePrint, func(subscription, promptID string) any {
		return sdkResumePrintMetadata{SubscriptionType: subscription, PromptID: promptID}
	})
}

// sdkFactDeclared reports whether the loaded SDK profile maps fact to an
// event name (undeclared facts are skipped by the resume lane, like the
// startup registry skips facts the bundle does not declare).
func (m *Manager) sdkFactDeclared(fact string) bool {
	if m == nil {
		return false
	}
	profile, ok := m.sdkProfile.Events[fact]
	return ok && profile.EventName != ""
}
