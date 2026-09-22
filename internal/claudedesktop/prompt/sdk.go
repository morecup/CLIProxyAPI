package prompt

import (
	"encoding/json"
	"strings"
	"time"
)

// SDKSnapshot keeps query/message accounting separate from generic HTTP calls
// and network-first-byte observations. CompleteFacts applies to the proxy-owned
// message model; it does not attest to unobservable native hooks or UI work.
type SDKSnapshot struct {
	RecoveredAPIFailures                                          int
	CompactionSummaryUserYields                                   int
	PendingCompactions                                            int
	ToolResultUserYields, InterruptionUserYields                  int
	RetryStatus                                                   int
	Queries, NumTurns                                             int
	APIDurationMS                                                 int64
	FirstAssistantMessageAt                                       time.Time
	ToolUseCount, MCPToolCalls, BuiltinToolCalls, ToolSearchCalls int
	SawRetry, SawCompact                                          bool
	CancelledStreaming                                            bool
	CompleteFacts                                                 bool
	IncompleteReason                                              string
}

// A root session and its explicitly bound forks share the API ledger. Each
// prompt snapshots it independently; helper completion is not prompt ownership.
type sdkAPILedger struct {
	durationMS       int64
	incompleteReason string
}

type sdkToolOccurrences struct {
	count int
	kind  string
}

type sdkAccounting struct {
	reactiveCompactionAttempted                  bool
	recoveredAPIFailures                         int
	history                                      *sdkHistory
	compactions                                  map[string]*sdkCompaction
	compactionSummaryUserYields                  int
	unknownCompactionYields                      bool
	pendingCompactions                           int
	toolResultUserYields, interruptionUserYields int
	catalogComplete                              bool
	retryStatus                                  int
	queries, numTurns, apiSuccesses              int
	ledger                                       *sdkAPILedger
	apiBaselineMS                                int64
	firstAssistantMessageAt                      time.Time
	sawRetry, sawCompact                         bool
	cancelledStreaming, unknownUserYields        bool
	tools                                        map[string]sdkToolOccurrences
	mcpAliases                                   map[string]struct{}
	helpers                                      map[string]struct{}
	incompleteReason                             string
	result                                       *SDKSnapshot
}

// ToolObservation is transient protocol metadata, never arguments/results.
type ToolObservation struct{ ID, Name string }

func (r *Request) ObserveSDKQuery(body ...[]byte) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	if r.finished || r.attempt != r.call.attempt || r.call.sdkQueryObserved {
		return
	}
	r.call.sdkQueryObserved = true
	var observed []byte
	if len(body) == 1 {
		observed = body[0]
	}
	r.call.observeSDKWireHistory(observed)
	r.call.applySDKCompaction(observed)
	r.call.observeSDKUserTokens(observed)
	r.call.state.sdk.queries++
}

// ObserveSDKAssistantMessage observes an assistant object yielded by the SDK
// when a content block closes, not the first response byte, first delta, or
// completion of the enclosing response. One response can yield several objects.
func (r *Request) ObserveSDKAssistantMessage(at time.Time, tools []ToolObservation) {
	if r == nil || at.IsZero() {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.finished || r.attempt != r.call.attempt {
		return
	}
	s := &r.call.state.sdk
	if s.firstAssistantMessageAt.IsZero() {
		s.firstAssistantMessageAt = at
	}
	// Response returns a cumulative list of closed tool blocks. Consume its
	// suffix by occurrence, not by tool ID, so polling cannot double count and
	// distinct blocks reusing an ID still count as distinct SDK observations.
	if len(tools) < r.sdkToolsObserved {
		s.incompleteReason = "nonmonotonic-tool-observations"
		return
	}
	if s.tools == nil {
		s.tools = make(map[string]sdkToolOccurrences)
	}
	for _, tool := range tools[r.sdkToolsObserved:] {
		if tool.Name == "" {
			s.incompleteReason = "missing-tool-name"
			continue
		}
		key := digest(tool.Name)
		old := s.tools[key]
		if old.count == 0 && len(s.tools) >= maxRequestsPerPrompt*maxToolsPerRequest {
			s.incompleteReason = "tool-accounting-limit"
			continue
		}
		old.kind = sdkToolKind(tool.Name)
		old.count++
		s.tools[key] = old
	}
	r.sdkToolsObserved = len(tools)
}

// RecordSDKAPISuccess uses the same retry-inclusive duration as api_success.
// Response decoding settles the logical call before its success is persisted.
// Return persistence health before releasing the lock/retention boundary, so
// another request's cache eviction cannot hide this callback's write failure.
func (r *Request) RecordSDKAPISuccess(durationMS int64) (stateErr error) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer func() {
		scope := r.call.state.scope
		r.tracker.saveSDKSessionLocked(scope)
		if r.tracker.nativeContentErrorLocked(scope) {
			stateErr = ErrSDKSessionUnavailable
		}
		if persisted := r.tracker.sessions[scope]; persisted != nil && persisted.err != nil {
			stateErr = ErrSDKSessionUnavailable
		}
	}()
	if r.attempt == r.call.attempt && r.call.succeeded {
		r.call.sdkAPICallbackPending = false
	}
	if r.attempt != r.call.attempt || !r.call.succeeded || r.call.sdkAPIRecorded || r.call.state.failed {
		return
	}
	r.call.sdkAPIRecorded = true
	s := &r.call.state.sdk
	s.apiSuccesses++
	if durationMS < 0 {
		s.incompleteReason = "invalid-api-duration"
		s.ledger.incompleteReason = s.incompleteReason
		return
	}
	s.ledger.durationMS += durationMS
	return
}

func (r *Request) ObserveSDKRetry(status ...int) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	if r.attempt == r.call.attempt && !r.call.settled && !r.call.state.complete && !r.call.state.failed {
		r.call.state.sdk.sawRetry = true
		if len(status) != 0 && status[0] > r.call.state.sdk.retryStatus {
			r.call.state.sdk.retryStatus = status[0]
		}
	}
}

// RecordSDKHelperSuccess is called only for a verified ledger-writing path and
// an explicitly bound account/session parent. Source names are deduplication
// dimensions, not inclusion rules. Late callbacks update the shared ledger but
// cannot rewrite a sealed result. Helper retry/compaction is not a main-loop yield.
func (r *Request) RecordSDKHelperSuccess(source, callID string, durationMS int64, _ bool) {
	if r == nil || callID == "" {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	r.call.state.sdk.recordHelperSuccess(source, callID, durationMS)
}

// recordHelperSuccess runs under the tracker lock and reports whether a new,
// valid callback was recorded. Role selection belongs to the caller, not to
// the source label, which is only part of the callback deduplication key.
func (s *sdkAccounting) recordHelperSuccess(source, callID string, durationMS int64) bool {
	if s.helpers == nil {
		s.helpers = make(map[string]struct{})
	}
	key := digest(source, callID)
	if _, exists := s.helpers[key]; exists {
		return false
	}
	if len(s.helpers) >= maxRequestsPerPrompt {
		s.incompleteReason = "helper-accounting-limit"
		s.ledger.incompleteReason = s.incompleteReason
		return false
	}
	s.helpers[key] = struct{}{}
	if durationMS < 0 {
		s.incompleteReason = "invalid-helper-duration"
		s.ledger.incompleteReason = s.incompleteReason
		return false
	}
	s.ledger.durationMS += durationMS
	return true
}

// RecordSDKCompactionSuccess records the verified streaming API callback, but
// not adoption of the resulting summary. Both full and reactive compact paths
// write the shared API ledger. Missing adoption/discard facts are prompt-local:
// they must not poison the known numeric ledger for every subsequent prompt.
func (r *Request) RecordSDKCompactionSuccess(source, callID string, durationMS int64, observations ...SDKCompactionObservation) {
	if r == nil || callID == "" {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	r.call.state.sdk.recordCompactionSuccess(source, callID, durationMS, observations)
}

func (s *sdkAccounting) recordCompactionSuccess(source, callID string, durationMS int64, observations []SDKCompactionObservation) {
	if s.recordHelperSuccess(source, callID, durationMS) && s.result == nil {
		s.pendingCompactions++
		if s.compactions == nil {
			s.compactions = make(map[string]*sdkCompaction)
		}
		operation := &sdkCompaction{}
		if len(observations) == 1 {
			operation.observation = observations[0]
		}
		s.compactions[digest(source, callID)] = operation
	}
}

func sdkToolKind(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		return "mcp"
	}
	if name == "ToolSearch" {
		return "toolsearch"
	}
	return "builtin"
}

// Only the current tool catalog's MCP aliases are retained, as hashes. Tool
// schemas, credentials and arguments never become accounting state.
func sdkMCPAliases(body []byte) (map[string]struct{}, bool) {
	var value struct {
		Tools []struct {
			Name    string `json:"name"`
			MCPInfo any    `json:"mcpInfo"`
		} `json:"tools"`
	}
	aliases := make(map[string]struct{})
	if json.Unmarshal(body, &value) != nil {
		return aliases, false
	}
	complete := true
	for _, tool := range value.Tools {
		if tool.Name == "" {
			continue
		}
		present := tool.MCPInfo != nil
		switch info := tool.MCPInfo.(type) {
		case bool:
			present = info
		case string:
			present = info != ""
		case float64:
			present = info != 0
		}
		if present {
			key := digest(tool.Name)
			if _, exists := aliases[key]; !exists && len(aliases) >= maxToolsPerRequest {
				complete = false
				continue
			}
			aliases[key] = struct{}{}
		}
	}
	return aliases, complete
}

// A helper's HTTP success alone cannot establish a compaction boundary or the
// internal user objects yielded by an unmodeled helper path.
func (r *Request) ObserveSDKUnmodeledHelper() {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	r.call.state.sdk.ledger.incompleteReason = "unobserved-sdk-helper-boundary"
	if r.call.state.sdk.result == nil {
		r.call.state.sdk.incompleteReason = "unobserved-sdk-helper-boundary"
	}
}

// SDKResultSnapshot freezes the ledger difference at result assembly, not at
// HTTP completion or at eventual telemetry delivery. It is idempotent.
func (r *Request) SDKResultSnapshot() SDKSnapshot {
	if r == nil {
		return SDKSnapshot{}
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	s := r.call.state
	value := s.sdkSnapshot()
	if (s.complete || s.failed) && s.sdk.result == nil {
		s.sdk.result = &value
	}
	return value
}

func (s *state) sdkSnapshot() SDKSnapshot {
	a := &s.sdk
	if a.result != nil {
		return *a.result
	}
	value := SDKSnapshot{Queries: a.queries, NumTurns: a.numTurns, FirstAssistantMessageAt: a.firstAssistantMessageAt,
		RecoveredAPIFailures: a.recoveredAPIFailures,
		SawRetry:             a.sawRetry, SawCompact: a.sawCompact, RetryStatus: a.retryStatus, CancelledStreaming: a.cancelledStreaming,
		ToolResultUserYields: a.toolResultUserYields, InterruptionUserYields: a.interruptionUserYields,
		CompactionSummaryUserYields: a.compactionSummaryUserYields,
		PendingCompactions:          a.pendingCompactions}
	if a.ledger != nil {
		value.APIDurationMS = a.ledger.durationMS - a.apiBaselineMS
	}
	for key, tool := range a.tools {
		value.ToolUseCount += tool.count
		_, alias := a.mcpAliases[key]
		switch {
		case alias || tool.kind == "mcp":
			value.MCPToolCalls += tool.count
		case tool.kind == "toolsearch":
			value.ToolSearchCalls += tool.count
		default:
			value.BuiltinToolCalls += tool.count
		}
	}
	reason := a.incompleteReason
	if reason == "" && a.ledger != nil {
		reason = a.ledger.incompleteReason
	}
	if reason == "" {
		reason = s.incompleteReason
	}
	if reason == "" && a.unknownUserYields {
		reason = "unobserved-sdk-user-yields"
	}
	if reason == "" && a.pendingCompactions != 0 {
		reason = "unobserved-sdk-compaction-disposition"
	}
	if reason == "" && a.unknownCompactionYields {
		reason = "unobserved-sdk-compaction-user-yields"
	}
	if reason == "" && !a.catalogComplete && value.ToolUseCount != 0 {
		reason = "unobserved-sdk-tool-catalog"
	}
	if reason == "" && a.queries == 0 {
		reason = "unobserved-sdk-query"
	}
	expectedSuccesses := a.queries - a.recoveredAPIFailures
	if a.cancelledStreaming {
		expectedSuccesses--
	}
	if reason == "" && a.apiSuccesses != expectedSuccesses {
		reason = "unsettled-sdk-api-accounting"
	}
	if reason == "" && !a.cancelledStreaming && a.firstAssistantMessageAt.IsZero() {
		reason = "unobserved-assistant-message"
	}
	if reason == "" && s.failed && !a.cancelledStreaming {
		reason = "unmodeled-sdk-terminal-failure"
	}
	value.CompleteFacts, value.IncompleteReason = reason == "", reason
	return value
}
