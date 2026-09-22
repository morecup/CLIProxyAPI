package telemetry

import (
	"context"
	"fmt"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	log "github.com/sirupsen/logrus"
)

const FactSDKTurnEnd = "turn_end"

const (
	FactSDKTTFT   = "sdk_ttft"
	FactSDKResult = "sdk_result"
)

const maxPromptIssues = 1024

// FinishPromptFailure receives the invocation owner's terminal decision. A
// provisional retry failure must not emit the prompt's terminal SDK events.
func (s *RequestSpan) FinishPromptFailure(ctx context.Context, category string, err error) {
	if s == nil || s.manager == nil || s.facts.Prompt == nil || !s.facts.Prompt.Snapshot().Failed {
		return
	}
	s.mu.Lock()
	if s.promptFailureFinished {
		s.mu.Unlock()
		return
	}
	s.promptFailureFinished = true
	s.mu.Unlock()
	if errSDK := s.manager.finishSDKPrompt(s); errSDK != nil {
		s.sdkWorker.recordQueueFailure(errSDK)
		log.WithError(errSDK).Warn("claude desktop SDK telemetry: terminal prompt event was not persisted")
	}
	now := s.manager.now()
	s.manager.finishRendererOutcome(ctx, s, category, safeErrorClass(err), now, now.Sub(s.facts.StartedAt))
}

func (m *Manager) recordPromptIssue(span *RequestSpan, attemptFailed bool) {
	prompt := span.facts.Prompt
	snapshot := prompt.Snapshot()
	key := rendererSessionKey(span.worker, span.facts.SessionID) + "\x00" + snapshot.PromptID
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.promptIssues == nil {
		m.promptIssues = make(map[string]string)
	}
	reason := ""
	if snapshot.IncompleteReason != "" {
		reason = snapshot.IncompleteReason
	} else if attemptFailed && !snapshot.Failed {
		reason = "awaiting-terminal-retry-decision"
	} else if snapshot.Complete || snapshot.Failed {
		delete(m.promptIssues, key)
	}
	if reason != "" {
		if _, exists := m.promptIssues[key]; exists || len(m.promptIssues) < maxPromptIssues {
			m.promptIssues[key] = reason
		} else {
			// Bound retained identifiers without turning discarded unresolved
			// facts into a healthy endpoint. A new runtime clears this latch.
			m.promptIssuesOverflow = true
		}
	}
	if len(m.promptIssues) > 0 || m.promptIssuesOverflow {
		reason = fmt.Sprintf("%d retained prompt(s) have unresolved lifecycle facts or terminal retry decisions", len(m.promptIssues))
		if m.promptIssuesOverflow {
			reason += "; additional unresolved prompts exceeded the diagnostic retention limit"
		}
		m.setEndpointState(m.profile.EndpointRole, "awaiting-prompt-completion", reason)
	} else {
		m.setEndpointState(m.profile.EndpointRole, "ready", "")
	}
}

type sdkTurnEndMetadata struct {
	SubscriptionType    string `json:"subscription_type"`
	PromptID            string `json:"cc_prompt_id"`
	DurationMS          int64  `json:"duration_ms"`
	GoalActive          bool   `json:"goal_active"`
	IsError             bool   `json:"is_error"`
	IsSubagent          bool   `json:"is_subagent"`
	QuerySource         string `json:"query_source"`
	QuerySourceCategory string `json:"query_source_category"`
	TerminalReason      string `json:"terminal_reason"`
}

// finishSDKPrompt runs after API accounting or an explicit terminal decision.
// Its owner observes continuations/retries across requests; a single tool-use
// response, title/helper, missing owner or provisional failure cannot emit it.
// Result/TTFT use their separately observed accounting, not APICalls or a
// network-first-byte timestamp. Helpers cannot finish the parent prompt.
func (m *Manager) finishSDKPrompt(span *RequestSpan) error {
	if span == nil || span.sdkWorker == nil || span.facts.Role != claudeprofile.RoleMain || span.facts.Prompt == nil {
		return nil
	}
	snapshot := span.facts.Prompt.Snapshot()
	if !span.facts.Prompt.CompletedPrompt() && !snapshot.Failed {
		// An observed compact continuation can expose a permanent evidence gap
		// while pending tool ownership prevents a terminal prompt. Report it
		// now; waiting for completion would silently hide SDK/Datadog degradation.
		switch snapshot.SDK.IncompleteReason {
		case "unobserved-precompact-tool-result", "unobserved-sdk-compaction-adoption", "unobserved-sdk-compaction-user-yields":
			return m.recordSDKPromptFactIssue(span, true)
		}
		return nil
	}
	span.mu.Lock()
	if span.sdkPromptFinished {
		span.mu.Unlock()
		return nil
	}
	span.sdkPromptFinished = true
	span.mu.Unlock()
	accounting := span.facts.Prompt.SDKResultSnapshot()
	if snapshot.Failed && !accounting.CancelledStreaming {
		// API errors, timeouts and truncated streams are not user interrupts.
		// Their SDK result variants require separately observed internal facts.
		return m.recordSDKPromptFactIssue(span, true)
	}
	terminalReason, subtype := "completed", "success"
	if accounting.CancelledStreaming {
		terminalReason, subtype = "aborted_streaming", "terminated"
	}
	if errEnqueue := m.enqueueSDKEvent(span.sdkWorker, FactSDKTurnEnd, span.facts, sdkTurnEndMetadata{
		SubscriptionType: subscriptionType(span.sdkWorker.authSnapshot()),
		PromptID:         snapshot.PromptID,
		DurationMS:       sdkRoundedDurationMS(snapshot.FinishedAt.Sub(snapshot.StartedAt)),
		QuerySource:      "sdk", QuerySourceCategory: "main", TerminalReason: terminalReason,
	}); errEnqueue != nil {
		return errEnqueue
	}
	if !accounting.CompleteFacts {
		return m.recordSDKPromptFactIssue(span, true)
	}
	// The cancelled error-result branch returns before successful TTFT emission,
	// even when an earlier closed thinking/text block yielded an assistant.
	if !accounting.CancelledStreaming {
		if errEnqueue := m.enqueueSDKEvent(span.sdkWorker, FactSDKTTFT, span.facts, sdkTTFTMetadata{
			SubscriptionType: subscriptionType(span.sdkWorker.authSnapshot()), PromptID: snapshot.PromptID,
			Model: span.facts.Model, TTFTMS: sdkRoundedDurationMS(accounting.FirstAssistantMessageAt.Sub(snapshot.StartedAt)),
		}); errEnqueue != nil {
			return errEnqueue
		}
	}
	var retryStatus *int
	if accounting.SawRetry {
		retryStatus = &accounting.RetryStatus
	}
	if errEnqueue := m.enqueueSDKEvent(span.sdkWorker, FactSDKResult, span.facts, sdkResultMetadata{
		SubscriptionType: subscriptionType(span.sdkWorker.authSnapshot()), PromptID: snapshot.PromptID,
		DurationMS: sdkRoundedDurationMS(snapshot.FinishedAt.Sub(snapshot.StartedAt)), DurationAPIMS: accounting.APIDurationMS,
		NumTurns: accounting.NumTurns, ToolUseCount: accounting.ToolUseCount,
		MCPToolCalls: accounting.MCPToolCalls, BuiltinToolCalls: accounting.BuiltinToolCalls, ToolSearchCalls: accounting.ToolSearchCalls,
		SawRetry: accounting.SawRetry, SawCompact: accounting.SawCompact, RetryStatus: retryStatus, IsError: snapshot.Failed, Subtype: subtype,
	}); errEnqueue != nil {
		return errEnqueue
	}
	return m.recordSDKPromptFactIssue(span, false)
}

// SDK result assembly uses Math.max(0, Math.round(monotonicElapsedMS)).
func sdkRoundedDurationMS(elapsed time.Duration) int64 {
	return maxInt64(0, elapsed.Round(time.Millisecond).Milliseconds())
}

type sdkTTFTMetadata struct {
	SubscriptionType string `json:"subscription_type"`
	PromptID         string `json:"cc_prompt_id"`
	Model            string `json:"model"`
	TTFTMS           int64  `json:"ttft_ms"`
}

type sdkResultMetadata struct {
	RetryStatus      *int   `json:"retry_status,omitempty"`
	SubscriptionType string `json:"subscription_type"`
	PromptID         string `json:"cc_prompt_id"`
	DurationMS       int64  `json:"duration_ms"`
	DurationAPIMS    int64  `json:"duration_api_ms"`
	NumTurns         int    `json:"num_turns"`
	ToolUseCount     int    `json:"tool_use_count"`
	MCPToolCalls     int    `json:"mcp_tool_calls"`
	BuiltinToolCalls int    `json:"builtin_tool_calls"`
	ToolSearchCalls  int    `json:"toolsearch_calls"`
	IsError          bool   `json:"is_error"`
	SawRetry         bool   `json:"saw_retry"`
	SawCompact       bool   `json:"saw_compact"`
	Subtype          string `json:"subtype"`
}
