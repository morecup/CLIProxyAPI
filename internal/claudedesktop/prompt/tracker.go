// Package prompt owns logical prompt identity across separate Messages calls.
// A successful HTTP response is not necessarily the end of an agent turn.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	stateTTL             = time.Hour
	maxPrompts           = 1024
	maxRequestsPerPrompt = 256
	maxToolsPerRequest   = 256
)

// Input contains no credentials. The optional native content owner copies only
// newly owned user content, never the request envelope, headers or metadata.
type Input struct {
	TaskNotification                                *string
	MetaInput                                       *SDKMetaInput
	ParentPromptID                                  string
	AccountID, SessionID, PromptID, ClientRequestID string
	Role                                            string
	Body                                            []byte
	StartedAt                                       time.Time
	Attempt                                         int
}

// Identity is immutable for a logical API call, including its retry attempts.
type Identity struct {
	PromptID     string
	QueryChainID string
	QueryDepth   int
	StartsPrompt bool
}

// Snapshot describes observed protocol facts, not SDK/tool execution metrics.
// In particular, ToolResults does not imply successful local tool execution,
// and APICalls is not a substitute for the SDK's internal num_turns counter.
type Snapshot struct {
	SDK                                     SDKSnapshot
	Failed                                  bool
	PromptID                                string
	StartedAt, FinishedAt                   time.Time
	FirstByteAt                             time.Time
	FirstSessionPrompt                      bool
	APICalls                                int
	APIDuration                             time.Duration
	ToolRequests, ToolResults, PendingTools int
	Complete                                bool
	IncompleteReason                        string
}

// Tracker is account/session partitioned and safe for concurrent executors.
// Structural checkpoints contain IDs, fingerprints and counts only. Native
// message content has its own protected owner/store and cannot leak into them.
// Lost in-flight callbacks remain explicitly unknown.
type Tracker struct {
	mu                sync.Mutex
	prompts           map[string]*state
	store             SDKSessionStore
	sessions          map[string]*sdkSessionPersistence
	restoredHistories map[string]*sdkRestoredHistory
	nativeOptions     SDKNativeContentOptions
	nativeContent     map[string]*sdkNativeContent
	transcript        *sdkTranscriptWriter
	sidechains        map[string]*SDKSidechain
	closed            bool
}

type state struct {
	helperOwners                  int
	sdkInputClaimed               bool
	sdk                           sdkAccounting
	controlInputStarted           bool
	failed                        bool
	scope, key                    string
	identity                      Identity
	startedAt, lastAt, finishedAt time.Time
	firstByteAt                   time.Time
	firstSessionPrompt            bool
	requests                      map[string]*call
	pending                       map[string]struct{}
	active                        *call
	apiCalls                      int
	apiDuration                   time.Duration
	toolRequests, toolResults     int
	incompleteReason              string
	complete                      bool
}

type call struct {
	sdkInstructionProjection  *sdkInstructionProjection
	sdkAPICallbackPending     bool
	reactiveCompactionFailure bool
	retryReady                bool
	sdkUserHistoryOffsets     []int
	reconcileHistory          bool
	sdkWireInputs             []string
	sdkWireInputKnown         bool
	sdkWireRequestDigest      string
	sdkAssistantYields        int
	compactionKey             string
	compactionUnknownYields   bool
	sdkQueryObserved          bool
	sdkAPIRecorded            bool
	state                     *state
	identity                  Identity
	inputDigest               string
	startedAt                 time.Time
	resultIDs                 []string
	attempt                   int
	settled                   bool
	succeeded                 bool
}

// Request binds one physical attempt to a logical call. Retry attempts get
// independent handles so a late completion cannot settle a newer attempt.
type Request struct {
	sdkHistoryOffsets    []int
	sdkWireResponse      string
	sdkWireResponseKnown bool
	sdkHistoryObserved   int
	tracker              *Tracker
	call                 *call
	identity             Identity
	attempt              int
	finished             bool
	completedPrompt      bool
	sdkToolsObserved     int
}

func (r *Request) Identity() Identity {
	if r == nil {
		return Identity{}
	}
	return r.identity
}

func (t *Tracker) Begin(input Input) *Request {
	if t == nil || input.Role != "main" || strings.TrimSpace(input.AccountID) == "" || strings.TrimSpace(input.SessionID) == "" || strings.TrimSpace(input.ClientRequestID) == "" {
		return nil
	}
	if input.StartedAt.IsZero() {
		input.StartedAt = time.Now()
	}
	if input.Attempt < 1 {
		input.Attempt = 1
	}
	input.PromptID = strings.TrimSpace(input.PromptID)
	if id, err := uuid.Parse(input.PromptID); err != nil || id == uuid.Nil {
		input.PromptID = uuid.NewString()
	}
	scope := digest(input.AccountID, input.SessionID)
	requestKey := digest(input.ClientRequestID)
	inputHash := requestIdentityDigest(input.Body)
	results, continuation, valid := lastUserToolResults(input.Body)
	var compactHistory sdkCompactionHistory
	parentID, parentErr := uuid.Parse(strings.TrimSpace(input.ParentPromptID))
	if parentErr == nil && parentID != uuid.Nil {
		compactHistory = observeSDKCompactionHistory(input.Body)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.prompts == nil {
		t.prompts = make(map[string]*state)
	}
	t.prune(input.StartedAt)
	t.loadSDKSessionLocked(scope)
	t.loadNativeContentLocked(scope, input.SessionID)
	defer t.saveSDKSessionLocked(scope)

	// Client request identity alone is insufficient: changed bodies are new
	// calls, not retries, and cannot inherit an unrelated prompt's state.
	for _, s := range t.prompts {
		if s.scope != scope {
			continue
		}
		if previous := s.requests[requestKey]; previous != nil && previous.inputDigest == inputHash && !previous.settled && input.Attempt > previous.attempt {
			if previous.sdkAssistantYields > 0 {
				s.sdk.history.unknown("unobserved-sdk-retry-retraction")
			}
			previous.attempt = input.Attempt
			previous.retryReady = false
			s.sdk.sawRetry = true
			s.sdk.mcpAliases, s.sdk.catalogComplete = sdkMCPAliases(input.Body)
			s.lastAt = input.StartedAt
			identity := previous.identity
			identity.StartsPrompt = false
			return &Request{tracker: t, call: previous, identity: identity, attempt: input.Attempt}
		}
	}

	var owner *state
	var compactionKey string
	var compactionUnknownYields bool
	reconcileHistory := false
	if compactHistory.known {
		candidate := t.prompts[digest(scope, parentID.String())]
		if candidate != nil && !candidate.complete && !candidate.failed && candidate.active == nil && candidate.sdk.result == nil && len(candidate.requests) < maxRequestsPerPrompt {
			compactionKey, compactionUnknownYields = candidate.sdk.matchCompaction(compactHistory)
			if compactionKey != "" {
				owner = candidate
				results = nil
			}
		}
	}
	if owner == nil && valid && continuation {
		for _, s := range t.prompts {
			if s.scope != scope || s.complete || s.failed || s.active != nil || len(s.requests) >= maxRequestsPerPrompt {
				continue
			}
			matches := true
			for _, id := range results {
				if _, ok := s.pending[id]; !ok {
					matches = false
					break
				}
			}
			if matches {
				if owner != nil {
					owner = nil
					break
				}
				owner = s
			}
		}
	}
	starts := owner == nil
	if starts {
		id := input.PromptID
		key := digest(scope, id)
		// Never merge new human input merely because its caller reused a UUID.
		if _, exists := t.prompts[key]; exists {
			id = uuid.NewString()
			key = digest(scope, id)
		}
		owner = &state{scope: scope, key: key, identity: Identity{PromptID: id, QueryChainID: uuid.NewString()}, startedAt: input.StartedAt, lastAt: input.StartedAt, requests: make(map[string]*call), pending: make(map[string]struct{})}
		ledger := &sdkAPILedger{}
		history := &sdkHistory{}
		retainedSession := false
		for _, existing := range t.prompts {
			if existing.scope == scope && existing.sdk.ledger != nil {
				ledger = existing.sdk.ledger
				history = existing.sdk.history
				retainedSession = true
				break
			}
		}
		if !retainedSession {
			if restored := t.restoredHistories[scope]; restored != nil {
				history, ledger, retainedSession = restored.history, restored.ledger, true
			}
		}
		owner.sdk = sdkAccounting{history: history, ledger: ledger, apiBaselineMS: ledger.durationMS, numTurns: 1}
		if !retainedSession && !ObserveSubmission(input.Body, input.StartedAt).FreshConversation {
			history.unknown("unobserved-sdk-initial-history")
		}
		if retainedSession {
			reconcileHistory = true
			history.pendingReconciliations++
		}
		history.append(SDKHistoryMessage{Type: "user"})
		if !valid {
			owner.incompleteReason = "unrecognized-prompt-input"
		} else if continuation {
			owner.incompleteReason = "unobserved-tool-owner"
		} else if input.Attempt > 1 {
			owner.incompleteReason = "unobserved-retry-owner"
		}
		t.prompts[key] = owner
	}
	owner.sdk.mcpAliases, owner.sdk.catalogComplete = sdkMCPAliases(input.Body)
	identity := owner.identity
	identity.QueryDepth = len(owner.requests)
	identity.StartsPrompt = starts
	c := &call{reconcileHistory: reconcileHistory, state: owner, identity: identity, inputDigest: inputHash, startedAt: input.StartedAt, resultIDs: results, attempt: input.Attempt, compactionKey: compactionKey, compactionUnknownYields: compactionUnknownYields}
	if starts {
		c.sdkUserHistoryOffsets = []int{len(owner.sdk.history.messages) - 1}
	}
	if !starts && compactionKey == "" {
		c.materializeToolResultUsers()
		c.reconcileHistory = true
		owner.sdk.history.pendingReconciliations++
	}
	// Duplicate client IDs must not erase a previous call/its depth.
	if _, exists := owner.requests[requestKey]; exists {
		requestKey = digest(requestKey, uuid.NewString())
	}
	owner.requests[requestKey] = c
	owner.active = c
	owner.lastAt = input.StartedAt
	t.observeNativeInputLocked(c, input.Body, input.TaskNotification, input.MetaInput)
	return &Request{tracker: t, call: c, identity: identity, attempt: input.Attempt}
}

// FinishSuccess requires an observed complete message, not just a 2xx header
// or a stream's first byte. Tool requests keep the prompt open until their
// actual result IDs arrive in a later main request and a terminal response ends.
func (r *Request) FinishSuccess(at time.Time, stopReason string, toolIDs []string) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	if r.finished || r.call.settled || r.attempt != r.call.attempt {
		return
	}
	r.finished = true
	r.call.settled = true
	r.call.succeeded = true
	r.call.sdkAPICallbackPending = r.call.sdkQueryObserved
	r.commitSDKWireHistory()
	s := r.call.state
	if at.IsZero() {
		at = time.Now()
	}
	s.lastAt = at
	if s.active == r.call {
		s.active = nil
	}
	s.apiCalls++
	if elapsed := at.Sub(r.call.startedAt); elapsed > 0 {
		s.apiDuration += elapsed
	}
	for _, id := range r.call.resultIDs {
		if _, ok := s.pending[id]; ok {
			delete(s.pending, id)
			s.toolResults++
		}
	}
	seen := make(map[string]struct{})
	for _, id := range toolIDs {
		if id = strings.TrimSpace(id); id == "" {
			s.incompleteReason = "missing-tool-id"
			continue
		}
		key := digest(id)
		if _, duplicate := seen[key]; duplicate {
			s.incompleteReason = "duplicate-tool-id"
			continue
		}
		seen[key] = struct{}{}
		if len(seen) > maxToolsPerRequest {
			s.incompleteReason = "tool-limit-exceeded"
			break
		}
		if _, exists := s.pending[key]; exists {
			s.incompleteReason = "reused-tool-id"
			continue
		}
		s.pending[key] = struct{}{}
		s.toolRequests++
	}
	switch strings.TrimSpace(stopReason) {
	case "tool_use":
		if len(toolIDs) == 0 {
			s.incompleteReason = "missing-tool-requests"
		}
	case "end_turn", "stop_sequence":
		if len(s.pending) == 0 && s.incompleteReason == "" {
			s.complete = true
			s.finishedAt = at
			r.completedPrompt = true
		}
	case "pause_turn":
		// Server-tool continuation has a different ownership protocol.
		s.incompleteReason = "server-tool-continuation-unmodeled"
	case "max_tokens", "refusal":
		s.incompleteReason = "terminal-policy-unmodeled"
	default:
		s.incompleteReason = "missing-terminal-stop-reason"
	}
}

// FinishFailure leaves the logical call available for an explicitly numbered
// retry. A failed attempt does not prove that the conductor exhausted retries.
func (r *Request) FinishFailure() {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	r.finished = true
	if r.attempt == r.call.attempt && !r.call.settled {
		r.call.retryReady = true
	}
}

// CanRetry reports whether a failed physical attempt still owns an unsettled
// query. A saved request-local continuation cannot reopen a completed prompt.
func (r *Request) CanRetry() bool {
	if r == nil {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	return r.call.retryReady && !r.call.settled && r.call.state.active == r.call && !r.call.state.complete && !r.call.state.failed
}

func (r *Request) CompletedPrompt() bool {
	if r == nil {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	return r.completedPrompt
}

func (r *Request) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	s := r.call.state
	return Snapshot{SDK: s.sdkSnapshot(), PromptID: s.identity.PromptID, StartedAt: s.startedAt, FinishedAt: s.finishedAt, FirstByteAt: s.firstByteAt, FirstSessionPrompt: s.firstSessionPrompt, APICalls: s.apiCalls, APIDuration: s.apiDuration, ToolRequests: s.toolRequests, ToolResults: s.toolResults, PendingTools: len(s.pending), Complete: s.complete, Failed: s.failed, IncompleteReason: s.incompleteReason}
}

// FinalizerKey isolates retry completion by account, session, prompt and call.
func (r *Request) FinalizerKey() string {
	if r == nil {
		return ""
	}
	return digest(r.call.state.key, strconv.Itoa(r.identity.QueryDepth))
}

// ClaimControlInput starts the human-input event on the first observable
// response, even if earlier transport attempts failed before headers arrived.
func (r *Request) ClaimControlInput() bool {
	if r == nil {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	s := r.call.state
	if s.controlInputStarted || r.identity.QueryDepth != 0 {
		return false
	}
	s.controlInputStarted = true
	return true
}

// FinalizeFailure is called only by the invocation owner after retry decisions,
// or when a direct (non-conductor) call terminates. It is idempotent and rejects
// obsolete attempts, including a late failure after a successful retry.
func (r *Request) FinalizeFailure(at time.Time) bool {
	return r.finalizeFailure(at, false)
}

// FinalizeCancellation is reserved for an observed context cancellation of an
// active upstream query, after the conductor has finished its retry decision.
func (r *Request) FinalizeCancellation(at time.Time) bool {
	return r.finalizeFailure(at, true)
}

func (r *Request) finalizeFailure(at time.Time, cancelled bool) bool {
	if r == nil {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	s := r.call.state
	if r.call.settled || r.attempt != r.call.attempt || s.complete || s.failed {
		return false
	}
	r.finished = true
	r.call.settled = true
	s.failed = true
	s.active = nil
	if at.IsZero() {
		at = time.Now()
	}
	s.finishedAt, s.lastAt = at, at
	if cancelled && r.call.sdkQueryObserved {
		s.sdk.cancelledStreaming = true
		// The SDK stream-abort branch yields one user interruption object.
		// Tool-drain user objects need separate facts and cannot be guessed.
		s.sdk.yieldUser(sdkInterruptionUserYield)
		if history := s.sdk.history; len(history.messages) != 0 {
			r.tracker.appendNativeContentLocked(s.scope, SDKNativeMessage{Type: "user", UUID: history.messages[len(history.messages)-1].UUID,
				Timestamp: nativeContentTimestamp(at), PromptID: r.identity.PromptID,
				Message: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}`)})
		}
		if r.hasUnobservedSDKToolDrain() {
			s.sdk.unknownUserYields = true
			s.sdk.history.unknown("unobserved-sdk-tool-drain-history")
		}
	} else if !cancelled {
		s.sdk.history.unknown("unobserved-sdk-error-assistant-history")
	}
	return true
}

// ObserveFirstByte records network timing only. It deliberately does not
// claim to measure the SDK's first-assistant message or renderer paint.
func (r *Request) ObserveFirstByte(at time.Time) bool {
	if r == nil || at.IsZero() {
		return false
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	s := r.call.state
	if !s.firstByteAt.IsZero() || r.finished {
		return false
	}
	s.firstByteAt = at
	return true
}

func (r *Request) ObserveSessionInitialized(first bool) {
	if r == nil || !first {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	r.call.state.firstSessionPrompt = true
}

func (t *Tracker) prune(now time.Time) {
	var idle []*state
	for key, s := range t.prompts {
		// Cache retention must not terminate an upstream operation or detach
		// a callback from the ledger/content it still owns. The target is soft
		// while all remaining owners are live; no network deadline is added.
		if s.hasLiveOwner() {
			continue
		}
		if now.Sub(s.lastAt) > stateTTL {
			delete(t.prompts, key)
			continue
		}
		idle = append(idle, s)
	}
	if excess := len(t.prompts) - maxPrompts + 1; excess > 0 {
		sort.Slice(idle, func(i, j int) bool { return idle[i].lastAt.Before(idle[j].lastAt) })
		for _, s := range idle[:min(excess, len(idle))] {
			delete(t.prompts, s.key)
		}
	}
	if len(t.sessions) != 0 || len(t.nativeContent) != 0 {
		retained := make(map[string]bool, len(t.prompts))
		for _, s := range t.prompts {
			retained[s.scope] = true
		}
		for scope := range t.sessions {
			if !retained[scope] {
				delete(t.sessions, scope)
				delete(t.restoredHistories, scope)
			}
		}
		for scope := range t.nativeContent {
			if !retained[scope] {
				if t.transcript != nil {
					t.transcript.retireScope(scope)
				}
				delete(t.nativeContent, scope)
			}
		}
	}
}

func (s *state) hasLiveOwner() bool {
	if s.helperOwners != 0 || (s.active != nil && !s.active.settled) {
		return true
	}
	// HTTP completion precedes the independent native API callback. Preserve
	// that short (but concurrently observable) ownership window as well.
	for _, c := range s.requests {
		if c.sdkAPICallbackPending {
			return true
		}
	}
	return false
}

func digest(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	h := sha256.Sum256(encoded)
	return hex.EncodeToString(h[:])
}

func requestIdentityDigest(body []byte) string {
	var value struct {
		Messages json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &value) != nil {
		return digest(string(body))
	}
	// Model, diagnostics and speed may legitimately change during a retry.
	// Compare the caller's message history, not per-attempt wire decoration.
	var messages any
	if json.Unmarshal(value.Messages, &messages) != nil {
		return digest(string(body))
	}
	encoded, _ := json.Marshal(messages)
	return digest(string(encoded))
}

func lastUserToolResults(body []byte) (ids []string, continuation, valid bool) {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Messages) == 0 {
		return nil, false, false
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != "user" {
		return nil, false, false
	}
	// Adjacent user rows may be unmerged SDK output or a single normalized
	// Messages row. Inspect the entire trailing run so an earlier result or
	// extra user message is not silently discarded by looking only at the last.
	start := len(request.Messages) - 1
	for start > 0 && request.Messages[start-1].Role == "user" {
		start--
	}
	seen := make(map[string]struct{})
	hasOther := false
	for _, message := range request.Messages[start:] {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			hasOther = true
			continue
		}
		var blocks []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil || len(blocks) == 0 {
			return nil, len(ids) != 0, false
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				hasOther = true
				continue
			}
			id := strings.TrimSpace(b.ToolUseID)
			if id == "" {
				return nil, true, false
			}
			key := digest(id)
			if _, duplicate := seen[key]; duplicate {
				return nil, true, false
			}
			seen[key] = struct{}{}
			ids = append(ids, key)
			if len(ids) > maxToolsPerRequest {
				return nil, true, false
			}
		}
	}
	if len(ids) == 0 {
		return nil, false, true
	}
	// Mixed human input and tool results cannot safely be assigned to a single
	// old prompt without a caller-owned boundary; do not guess from the text.
	if hasOther {
		return nil, true, false
	}
	return ids, true, true
}
