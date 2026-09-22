package telemetry

import (
	"sync"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

const (
	FactSDKReactiveCompactTriggered = "reactive_compact_triggered"
	FactSDKReactiveCompactAttempt   = "reactive_compact_attempt"
)

// SDKReactiveCompactionSpan follows a claimed main recovery, not its helper's
// HTTP lifetime. Main dimensions are frozen before the physical failure closes
// that request span. It never emits native application success: that requires
// restoration and awaited PostCompact, not merely a successful summary query.
type SDKReactiveCompactionSpan struct {
	mu     sync.Mutex
	span   *RequestSpan
	view   *claudeprompt.SDKCompactionView
	facts  RequestFacts
	seen   map[[2]int]struct{}
	closed bool
}

// BeginSDKReactiveCompaction runs after the main recovery gates and claim,
// before summary preparation. The implemented direct path has no precompute
// candidate; "none" describes that concrete decision, not an unknown default.
func (s *RequestSpan) BeginSDKReactiveCompaction(view *claudeprompt.SDKCompactionView) *SDKReactiveCompactionSpan {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reactiveCompaction != nil {
		return s.reactiveCompaction
	}
	facts := s.facts
	if s.finished || !s.requestObserved || !s.responseObserved || s.responseStatus != 400 || facts.Role != claudeprofile.RoleMain || !view.ReactiveFailureOwnedBy(facts.Prompt) {
		return nil
	}
	operation := &SDKReactiveCompactionSpan{span: s, view: view, facts: facts, seen: make(map[[2]int]struct{})}
	s.reactiveCompaction = operation
	betas, known := s.manager.sdkProfile.InputBetaHeader(facts.Model)
	if !known || facts.QuerySource != "sdk" || facts.PromptID != facts.Prompt.Identity().PromptID {
		operation.closed = true
		operation.retainIssue()
		return operation
	}
	// Native lifecycle events use session-default betas, not the failed main's
	// diagnostics/fast headers and not the compact helper's beta set.
	operation.facts.Betas = betas
	metadata := operation.commonMetadata()
	metadata["querySource"] = facts.QuerySource
	metadata["precomputed"] = false
	metadata["precomputedKind"] = "none"
	if facts.EffortLevel != "" {
		metadata["effort_level"] = facts.EffortLevel
	}
	operation.enqueue(FactSDKReactiveCompactTriggered, metadata)
	return operation
}

func (s *SDKReactiveCompactionSpan) commonMetadata() map[string]any {
	return map[string]any{"subscription_type": subscriptionType(s.span.sdkWorker.authSnapshot()), "cc_prompt_id": s.facts.PromptID}
}

// ObserveAttempt receives the actual native group selection before dispatch.
// Repeated physical HTTP attempts never reach this boundary. A media-stripped
// retry does, and legitimately reuses the same native attempt number.
func (s *SDKReactiveCompactionSpan) ObserveAttempt(attempt claudeprompt.SDKReactiveAttempt) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.view.ReactiveFailureOwnedBy(s.facts.Prompt) {
		return
	}
	groups := s.view.History().Groups
	if attempt.Attempt < 1 || attempt.Attempt > len(groups) || attempt.GroupsToSummarize < 1 || attempt.GroupsToPreserve < 1 ||
		attempt.GroupsToSummarize+attempt.GroupsToPreserve != len(groups) || attempt.MessagesToSummarize != len(attempt.Summarize) ||
		!sameReactiveSelection(groups[:attempt.GroupsToSummarize], attempt.Summarize) || !sameReactiveSelection(groups[attempt.GroupsToSummarize:], attempt.Preserve) {
		s.retainIssue()
		return
	}
	key := [2]int{attempt.Attempt, 0}
	if attempt.StrippedMedia {
		key[1] = 1
	}
	if _, exists := s.seen[key]; exists {
		return
	}
	s.seen[key] = struct{}{}
	metadata := s.commonMetadata()
	metadata["attempt"] = attempt.Attempt
	metadata["groupsToSummarize"] = attempt.GroupsToSummarize
	metadata["groupsToPreserve"] = attempt.GroupsToPreserve
	metadata["messagesToSummarize"] = attempt.MessagesToSummarize
	metadata["strippedMedia"] = attempt.StrippedMedia
	// Absent fields stay absent, including on the first unseeded attempt.
	// Effort and querySource belong to the trigger, not the attempt metadata.
	if attempt.StepMode != "" {
		metadata["stepMode"] = attempt.StepMode
	}
	if attempt.StepSize != nil {
		metadata["stepSize"] = *attempt.StepSize
	}
	if attempt.TokenGap != nil {
		metadata["tokenGap"] = *attempt.TokenGap
	}
	s.enqueue(FactSDKReactiveCompactAttempt, metadata)
}

func sameReactiveSelection(groups [][]claudeprompt.SDKHistoryMessage, selection []claudeprompt.SDKHistoryMessage) bool {
	index := 0
	for _, group := range groups {
		for _, message := range group {
			if index >= len(selection) || selection[index] != message {
				return false
			}
			index++
		}
	}
	return index == len(selection)
}

func (s *SDKReactiveCompactionSpan) enqueue(fact string, metadata map[string]any) {
	if err := s.span.manager.enqueueSDKEvent(s.span.sdkWorker, fact, s.facts, metadata); err != nil {
		s.span.sdkWorker.recordQueueFailure(err)
		s.retainIssue()
	}
}

func (s *SDKReactiveCompactionSpan) retainIssue() {
	s.span.retainResponseFactIssue(factIssueSDKCompaction, s.facts)
}

// RecordContextUnavailable records an actual missing or failed recovery stage.
// It is separate from the summary's API outcome and survives continuation.
func (s *SDKReactiveCompactionSpan) RecordContextUnavailable() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.span != nil && s.view.ReactiveFailureOwnedBy(s.facts.Prompt) {
		s.retainIssue()
	}
}

// Close releases the request-local history reference on success, cancellation
// or failure. It does not turn an unverified native restoration into success.
func (s *SDKReactiveCompactionSpan) Close() {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		s.view = nil
	}
}
