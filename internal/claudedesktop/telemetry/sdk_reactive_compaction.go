package telemetry

import (
	"math"
	"strings"
	"sync"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

const (
	FactSDKReactiveCompactTriggered  = "reactive_compact_triggered"
	FactSDKReactiveCompactAttempt    = "reactive_compact_attempt"
	FactSDKReactiveCompactSucceeded  = "reactive_compact_succeeded"
	FactSDKReactiveCompactFailed     = "reactive_compact_failed"
	FactSDKAutoCompactRoutedReactive = "auto_compact_routed_reactive"
	FactSDKAutoCompactSucceeded      = "auto_compact_succeeded"
)

type SDKReactiveCompactionSuccess struct {
	Attempts, GroupsPreserved, TotalGroups       int
	PreservedUUIDCount, PreservedMessageCount    int
	ForkAssistantMessageCount, RestoredItemCount int
	SplitKind                                    string
	HeadTruncations                              int
	PreCompactTokens, PostCompactTokens          *int64
	Usage                                        claudeprompt.SDKTokenUsage
	UsageKnown                                   bool
	// Optional native decisions remain absent when they were not observed.
	PreservedUUIDCountKnown    *bool
	KeptThinkingBlockCount     *int
	KeptThinkingStripped       *bool
	KeptThinkingStripDecidedBy string
	CacheCold                  *bool
	Breakdown                  *claudeprompt.SDKCompactionBreakdown
}

type SDKReactiveCompactionFailure struct {
	Reason                string
	Attempts, TotalGroups int
	SplitKind             string
	HeadTruncations       *int
	PreCompactTokens      *int64
	CacheCold             *bool
	Status                *int
}

// SDKReactiveCompactionSpan follows a claimed main recovery, not its helper's
// HTTP lifetime. Main dimensions are frozen before the physical failure closes
// that request span. Terminal events require the direct summary-loop result or
// a committed native application after restoration and awaited PostCompact.
type SDKReactiveCompactionSpan struct {
	mu        sync.Mutex
	span      *RequestSpan
	view      *claudeprompt.SDKCompactionView
	facts     RequestFacts
	origin    claudeprompt.SDKCompactionOrigin
	seen      map[[2]int]struct{}
	startedAt time.Time
	terminal  bool
	closed    bool
}

// BeginSDKReactiveCompaction runs after the main recovery gates and claim,
// before summary preparation. The implemented direct path has no precompute
// candidate; "none" describes that concrete decision, not an unknown default.
// This entry owns only post-PTL recovery. Native preflight uses BeginSDKCompaction;
// neither a manual/auto wire hint nor helper HTTP success is that evidence.
func (s *RequestSpan) BeginSDKReactiveCompaction(view *claudeprompt.SDKCompactionView) *SDKReactiveCompactionSpan {
	return s.BeginSDKCompaction(view, claudeprompt.SDKCompactionOrigin{Kind: "reactive"})
}

// BeginSDKCompaction is the native lifecycle boundary. Manual and threshold
// callers must own the complete preflight history; an incoming compact helper
// cannot call this API on its own span to manufacture a parent lifecycle.
// This direct engine has no precomputed candidate. Manual therefore makes the
// concrete miss_not_ready decision, and does not emit reactive_triggered.
func (s *RequestSpan) BeginSDKCompaction(view *claudeprompt.SDKCompactionView, origin claudeprompt.SDKCompactionOrigin) *SDKReactiveCompactionSpan {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reactiveCompaction != nil {
		if s.reactiveCompaction.origin == origin && s.reactiveCompaction.view == view {
			return s.reactiveCompaction
		}
		return nil
	}
	facts := s.facts
	if s.finished || !s.requestObserved || facts.Role != claudeprofile.RoleMain || !origin.Valid() || !view.CompactionOwnedBy(facts.Prompt) {
		return nil
	}
	if origin.Kind == "reactive" {
		if !s.responseObserved || s.responseStatus != 400 || !view.ReactiveFailureOwnedBy(facts.Prompt) {
			return nil
		}
	} else if s.responseObserved {
		return nil
	}
	operation := &SDKReactiveCompactionSpan{span: s, view: view, facts: facts, origin: origin, seen: make(map[[2]int]struct{}), startedAt: s.manager.now()}
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
	if facts.QueryDepth != nil {
		depth := *facts.QueryDepth
		operation.facts.QueryDepth = &depth
	}
	if origin.Kind == "manual" {
		// The current manual command calls the summary engine directly, with
		// no querySource. Sharing an engine does not make it an auto trigger.
		operation.facts.QuerySource = ""
		return operation
	}
	if origin.Kind == "auto" {
		routed := operation.commonMetadata()
		routed["thresholdSource"] = origin.ThresholdSource
		operation.enqueue(FactSDKAutoCompactRoutedReactive, routed)
	}
	metadata := operation.commonMetadata()
	metadata["querySource"] = facts.QuerySource
	metadata["precomputed"] = false
	metadata["precomputedKind"] = "none"
	if origin.Kind == "auto" {
		metadata["thresholdSource"] = origin.ThresholdSource
	}
	if facts.EffortLevel != "" {
		metadata["effort_level"] = facts.EffortLevel
	}
	operation.enqueue(FactSDKReactiveCompactTriggered, metadata)
	return operation
}

func (s *SDKReactiveCompactionSpan) ownsView() bool {
	if s == nil || s.view == nil {
		return false
	}
	if s.origin.Kind == "reactive" {
		return s.view.ReactiveFailureOwnedBy(s.facts.Prompt)
	}
	return s.view.CompactionOwnedBy(s.facts.Prompt)
}

func (s *SDKReactiveCompactionSpan) addOrigin(metadata map[string]any) {
	metadata["trigger"] = s.origin.HookTrigger()
	if s.origin.Kind == "manual" {
		metadata["manualPrecomputeReuse"] = "miss_not_ready"
	}
	if s.origin.Kind == "auto" {
		metadata["thresholdSource"] = s.origin.ThresholdSource
	}
}

func (s *SDKReactiveCompactionSpan) commonMetadata() map[string]any {
	metadata := map[string]any{"subscription_type": subscriptionType(s.span.sdkWorker.authSnapshot()), "cc_prompt_id": s.facts.PromptID}
	desktopVersion := strings.TrimSpace(s.facts.DesktopVersion)
	if desktopVersion == "" && s.span != nil && s.span.manager != nil {
		desktopVersion = strings.TrimSpace(s.span.manager.sdkDesktopVersion)
	}
	if desktopVersion != "" {
		metadata["desktop_app_version"] = desktopVersion
	}
	return metadata
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
	if s.closed || s.terminal || !s.ownsView() {
		return
	}
	if !claudeprompt.ValidateSDKReactiveAttempt(s.view.History(), attempt) {
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
	metadata["splitKind"] = attempt.SplitKind
	metadata["headTruncations"] = attempt.HeadTruncations
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

func (s *SDKReactiveCompactionSpan) RecordFailure(failure SDKReactiveCompactionFailure) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.terminal || !s.ownsView() {
		return
	}
	reason := strings.TrimSpace(failure.Reason)
	validSplit := failure.SplitKind == "" && failure.HeadTruncations == nil
	if failure.SplitKind != "" && failure.HeadTruncations != nil {
		validSplit = validSDKReactiveSplit(failure.SplitKind, *failure.HeadTruncations)
	}
	if reason == "" || failure.Attempts < 0 || failure.TotalGroups < 0 || !validSplit {
		s.retainIssue()
		return
	}
	s.terminal = true
	metadata := s.commonMetadata()
	metadata["reason"] = reason
	s.addOrigin(metadata)
	if s.origin.Kind == "manual" {
		metadata["precomputedKind"] = "none"
	}
	metadata["attempts"] = failure.Attempts
	metadata["totalGroups"] = failure.TotalGroups
	metadata["durationMs"] = s.elapsedMilliseconds()
	if s.facts.QuerySource != "" {
		metadata["querySource"] = s.facts.QuerySource
	}
	if s.facts.EffortLevel != "" {
		metadata["effort_level"] = s.facts.EffortLevel
	}
	if failure.SplitKind != "" {
		metadata["splitKind"] = failure.SplitKind
	}
	if failure.HeadTruncations != nil {
		metadata["headTruncations"] = *failure.HeadTruncations
	}
	if failure.PreCompactTokens != nil {
		metadata["preCompactTokens"] = *failure.PreCompactTokens
	}
	if failure.CacheCold != nil {
		metadata["cacheCold"] = *failure.CacheCold
	}
	if failure.Status != nil && *failure.Status >= 100 && *failure.Status <= 599 {
		metadata["status"] = *failure.Status
	}
	s.enqueue(FactSDKReactiveCompactFailed, metadata)
}

func (s *SDKReactiveCompactionSpan) RecordSuccess(success SDKReactiveCompactionSuccess) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.terminal {
		return
	}
	validSelection := success.SplitKind == "round" && success.GroupsPreserved > 0 && success.GroupsPreserved < success.TotalGroups ||
		success.SplitKind == "summarize_all" && success.GroupsPreserved == 0
	if success.Attempts < 1 || success.Attempts > success.TotalGroups+3 || success.TotalGroups < 2 || !validSelection ||
		!validSDKReactiveSplit(success.SplitKind, success.HeadTruncations) ||
		success.PreservedUUIDCount < 0 || success.PreservedMessageCount < 0 || success.RestoredItemCount < 0 {
		s.retainIssue()
		return
	}
	s.terminal = true
	metadata := s.commonMetadata()
	metadata["attempts"] = success.Attempts
	metadata["groupsPreserved"] = success.GroupsPreserved
	metadata["totalGroups"] = success.TotalGroups
	metadata["splitKind"] = success.SplitKind
	metadata["headTruncations"] = success.HeadTruncations
	if success.PreservedUUIDCountKnown == nil || *success.PreservedUUIDCountKnown {
		metadata["preservedUuidCount"] = success.PreservedUUIDCount
	}
	metadata["preservedMessageCount"] = success.PreservedMessageCount
	metadata["forkAssistantMessageCount"] = success.ForkAssistantMessageCount
	s.addOrigin(metadata)
	metadata["restoredAttachmentCount"] = success.RestoredItemCount
	metadata["durationMs"] = s.elapsedMilliseconds()
	metadata["userWaitMs"] = metadata["durationMs"]
	metadata["precomputed"] = false
	if success.KeptThinkingBlockCount != nil && *success.KeptThinkingBlockCount >= 0 && success.KeptThinkingStripped != nil {
		metadata["keptThinkingBlockCount"] = *success.KeptThinkingBlockCount
		metadata["keptThinkingStripped"] = *success.KeptThinkingStripped
		if success.KeptThinkingStripDecidedBy != "" && validSDKThinkingDecision(success.KeptThinkingStripDecidedBy) {
			metadata["keptThinkingStripDecidedBy"] = success.KeptThinkingStripDecidedBy
		}
	}
	if success.CacheCold != nil {
		metadata["cacheCold"] = *success.CacheCold
	}
	if success.Breakdown != nil {
		for key, value := range success.Breakdown.Metadata() {
			metadata[key] = value
		}
	}
	if s.facts.QuerySource != "" {
		metadata["querySource"] = s.facts.QuerySource
	}
	if s.facts.EffortLevel != "" {
		metadata["effort_level"] = s.facts.EffortLevel
	}
	if success.PreCompactTokens != nil {
		metadata["preCompactTokens"] = *success.PreCompactTokens
	}
	if success.PostCompactTokens != nil {
		metadata["postCompactTokens"] = *success.PostCompactTokens
	}
	if success.UsageKnown {
		input := success.Usage.InputTokens
		output := success.Usage.OutputTokens
		cacheRead := success.Usage.CacheReadInputTokens
		cacheCreation := success.Usage.CacheCreationInputTokens
		totalInput := input + cacheRead + cacheCreation
		metadata["compactionInputTokens"] = input
		metadata["compactionOutputTokens"] = output
		metadata["compactionCacheReadTokens"] = cacheRead
		metadata["compactionCacheCreationTokens"] = cacheCreation
		metadata["compactionTotalTokens"] = totalInput + output
		cacheHitRate := float64(0)
		if totalInput > 0 {
			cacheHitRate = float64(cacheRead) / float64(totalInput)
		}
		metadata["cacheHitRate"] = cacheHitRate
	}
	s.enqueue(FactSDKReactiveCompactSucceeded, metadata)
	if s.origin.Kind == "auto" {
		// cer counts the summary plus restoration, excluding the preserved
		// tail and compact boundary. Reactive results do not own post-token
		// fields in the outer auto event, even though the inner event does.
		auto := s.commonMetadata()
		auto["thresholdSource"] = s.origin.ThresholdSource
		auto["routedThroughReactive"] = true
		auto["originalMessageCount"] = len(s.view.History().Messages)
		auto["compactedMessageCount"] = 1 + success.RestoredItemCount
		if success.PreCompactTokens != nil {
			auto["preCompactTokenCount"] = *success.PreCompactTokens
		}
		if s.facts.EffortLevel != "" {
			auto["effort_level"] = s.facts.EffortLevel
		}
		if s.facts.QueryChainID != "" {
			auto["queryChainId"] = s.facts.QueryChainID
		}
		if s.facts.QueryDepth != nil {
			auto["queryDepth"] = *s.facts.QueryDepth
		}
		for _, key := range []string{"compactionInputTokens", "compactionOutputTokens", "compactionCacheReadTokens", "compactionCacheCreationTokens", "compactionTotalTokens"} {
			if value, exists := metadata[key]; exists {
				auto[key] = value
			}
		}
		s.enqueue(FactSDKAutoCompactSucceeded, auto)
	}
}

func validSDKThinkingDecision(value string) bool {
	// Only reviewed native policy names can cross this diagnostics boundary.
	return value == "env" || value == "thinking_type" || value == "flag"
}

func validSDKReactiveSplit(splitKind string, headTruncations int) bool {
	switch splitKind {
	case "round":
		return headTruncations == 0
	case "summarize_all":
		return headTruncations >= 0 && headTruncations <= 3
	default:
		return false
	}
}

func (s *SDKReactiveCompactionSpan) elapsedMilliseconds() int64 {
	if s == nil || s.span == nil || s.span.manager == nil || s.startedAt.IsZero() {
		return 0
	}
	duration := s.span.manager.now().Sub(s.startedAt)
	if duration < 0 {
		duration = 0
	}
	return int64(math.Round(float64(duration) / float64(time.Millisecond)))
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
	if s.span != nil && s.ownsView() {
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
