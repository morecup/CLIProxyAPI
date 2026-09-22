package prompt

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

// SDKSessionStore is owned by the runtime, never populated from request
// metadata. Save compares the last revision so an obsolete executor cannot
// overwrite another runtime's newer session. Implementations protect payloads
// at rest and retain unreadable originals.
type SDKSessionStore interface {
	Load(scope string) (payload []byte, revision string, err error)
	Save(scope, previousRevision string, payload []byte) (revision string, err error)
}

var (
	ErrSDKSessionUnavailable = errors.New("SDK session state is unavailable")
	ErrSDKSessionInvalid     = errors.New("SDK session state cannot be verified")
	ErrSDKSessionStale       = errors.New("SDK session state has a newer owner")
)

const MaxSDKSessionStateBytes = 32 << 20

type sdkSessionPersistence struct {
	revision   string
	digest     string
	err        error
	loadFailed bool
}

// NewTracker installs the ordinary runtime's durable session store. A nil store
// retains the existing in-memory behavior for standalone callers and tests.
func NewTracker(store SDKSessionStore, native ...SDKNativeContentOptions) Tracker {
	var options SDKNativeContentOptions
	if len(native) == 1 {
		options = native[0]
	}
	return Tracker{store: store, nativeOptions: options}
}

type sdkStoredSession struct {
	HistoryOnly      bool                       `json:"history_only,omitempty"`
	HistoryCleared   bool                       `json:"history_cleared,omitempty"`
	PersistenceIssue bool                       `json:"persistence_issue"`
	Version          int                        `json:"version"`
	Scope            string                     `json:"scope"`
	History          sdkStoredHistory           `json:"history"`
	DurationMS       int64                      `json:"duration_ms"`
	LedgerIssue      string                     `json:"ledger_issue"`
	Prompts          map[string]sdkStoredPrompt `json:"prompts"`
}

// A remote history can exist before this process has executed any prompt.
// It owns structural history, not an invented completed request or API ledger.
type sdkRestoredHistory struct {
	history *sdkHistory
	ledger  *sdkAPILedger
}

type sdkStoredHistory struct {
	Messages               []SDKHistoryMessage `json:"messages"`
	IncompleteReason       string              `json:"incompleteReason"`
	ExpectedText           []string            `json:"expectedText"`
	ExpectedTextKnown      bool                `json:"expectedTextKnown"`
	PendingReconciliations int                 `json:"pendingReconciliations"`
}

type sdkStoredPrompt struct {
	SdkInputClaimed     bool                     `json:"sdkInputClaimed"`
	ControlInputStarted bool                     `json:"controlInputStarted"`
	Failed              bool                     `json:"failed"`
	Key                 string                   `json:"key"`
	Identity            Identity                 `json:"identity"`
	StartedAt           time.Time                `json:"startedAt"`
	LastAt              time.Time                `json:"lastAt"`
	FinishedAt          time.Time                `json:"finishedAt"`
	FirstByteAt         time.Time                `json:"firstByteAt"`
	FirstSessionPrompt  bool                     `json:"firstSessionPrompt"`
	Pending             map[string]struct{}      `json:"pending"`
	ApiCalls            int                      `json:"apiCalls"`
	ApiDuration         time.Duration            `json:"apiDuration"`
	ToolRequests        int                      `json:"toolRequests"`
	ToolResults         int                      `json:"toolResults"`
	IncompleteReason    string                   `json:"incompleteReason"`
	Complete            bool                     `json:"complete"`
	Accounting          sdkStoredAccounting      `json:"accounting"`
	Requests            map[string]sdkStoredCall `json:"requests"`
	Active              string                   `json:"active"`
}

type sdkStoredCall struct {
	ReactiveCompactionFailure bool      `json:"reactiveCompactionFailure"`
	RetryReady                bool      `json:"retryReady"`
	SdkUserHistoryOffsets     []int     `json:"sdkUserHistoryOffsets"`
	ReconcileHistory          bool      `json:"reconcileHistory"`
	SdkWireInputs             []string  `json:"sdkWireInputs"`
	SdkWireInputKnown         bool      `json:"sdkWireInputKnown"`
	SdkWireRequestDigest      string    `json:"sdkWireRequestDigest"`
	SdkAssistantYields        int       `json:"sdkAssistantYields"`
	CompactionKey             string    `json:"compactionKey"`
	CompactionUnknownYields   bool      `json:"compactionUnknownYields"`
	SdkQueryObserved          bool      `json:"sdkQueryObserved"`
	SdkAPIRecorded            bool      `json:"sdkAPIRecorded"`
	Identity                  Identity  `json:"identity"`
	InputDigest               string    `json:"inputDigest"`
	StartedAt                 time.Time `json:"startedAt"`
	ResultIDs                 []string  `json:"resultIDs"`
	Attempt                   int       `json:"attempt"`
	Settled                   bool      `json:"settled"`
	Succeeded                 bool      `json:"succeeded"`
}

type sdkStoredAccounting struct {
	ReactiveCompactionAttempted bool                           `json:"reactiveCompactionAttempted"`
	RecoveredAPIFailures        int                            `json:"recoveredAPIFailures"`
	CompactionSummaryUserYields int                            `json:"compactionSummaryUserYields"`
	UnknownCompactionYields     bool                           `json:"unknownCompactionYields"`
	PendingCompactions          int                            `json:"pendingCompactions"`
	ToolResultUserYields        int                            `json:"toolResultUserYields"`
	InterruptionUserYields      int                            `json:"interruptionUserYields"`
	CatalogComplete             bool                           `json:"catalogComplete"`
	RetryStatus                 int                            `json:"retryStatus"`
	Queries                     int                            `json:"queries"`
	NumTurns                    int                            `json:"numTurns"`
	ApiSuccesses                int                            `json:"apiSuccesses"`
	ApiBaselineMS               int64                          `json:"apiBaselineMS"`
	FirstAssistantMessageAt     time.Time                      `json:"firstAssistantMessageAt"`
	SawRetry                    bool                           `json:"sawRetry"`
	SawCompact                  bool                           `json:"sawCompact"`
	CancelledStreaming          bool                           `json:"cancelledStreaming"`
	UnknownUserYields           bool                           `json:"unknownUserYields"`
	McpAliases                  map[string]struct{}            `json:"mcpAliases"`
	Helpers                     map[string]struct{}            `json:"helpers"`
	IncompleteReason            string                         `json:"incompleteReason"`
	Result                      *SDKSnapshot                   `json:"result"`
	Tools                       map[string]sdkStoredTool       `json:"tools"`
	Compactions                 map[string]sdkStoredCompaction `json:"compactions"`
}

type sdkStoredTool struct {
	Count int    `json:"count"`
	Kind  string `json:"kind"`
}
type sdkStoredCompaction struct {
	ToolResults     map[string]struct{} `json:"tool_results"`
	InputKnown      bool                `json:"input_known"`
	SummaryBytes    int                 `json:"summary_bytes"`
	SummaryHash     string              `json:"summary_hash"`
	SummaryReviewed bool                `json:"summary_reviewed"`
	Applied         bool                `json:"applied"`
	Discarded       bool                `json:"discarded"`
}

func storeSDKHistory(source *sdkHistory) sdkStoredHistory {
	return sdkStoredHistory{Messages: source.messages,
		IncompleteReason:       source.incompleteReason,
		ExpectedText:           source.expectedText,
		ExpectedTextKnown:      source.expectedTextKnown,
		PendingReconciliations: source.pendingReconciliations}
}
func restoreSDKHistory(source sdkStoredHistory) *sdkHistory {
	return &sdkHistory{messages: source.Messages,
		incompleteReason:       source.IncompleteReason,
		expectedText:           source.ExpectedText,
		expectedTextKnown:      source.ExpectedTextKnown,
		pendingReconciliations: source.PendingReconciliations}
}
func storeSDKCall(source *call) sdkStoredCall {
	return sdkStoredCall{ReactiveCompactionFailure: source.reactiveCompactionFailure,
		RetryReady:              source.retryReady,
		SdkUserHistoryOffsets:   source.sdkUserHistoryOffsets,
		ReconcileHistory:        source.reconcileHistory,
		SdkWireInputs:           source.sdkWireInputs,
		SdkWireInputKnown:       source.sdkWireInputKnown,
		SdkWireRequestDigest:    source.sdkWireRequestDigest,
		SdkAssistantYields:      source.sdkAssistantYields,
		CompactionKey:           source.compactionKey,
		CompactionUnknownYields: source.compactionUnknownYields,
		SdkQueryObserved:        source.sdkQueryObserved,
		SdkAPIRecorded:          source.sdkAPIRecorded,
		Identity:                source.identity,
		InputDigest:             source.inputDigest,
		StartedAt:               source.startedAt,
		ResultIDs:               source.resultIDs,
		Attempt:                 source.attempt,
		Settled:                 source.settled,
		Succeeded:               source.succeeded}
}
func restoreSDKCall(source sdkStoredCall, owner *state) *call {
	return &call{state: owner, reactiveCompactionFailure: source.ReactiveCompactionFailure,
		retryReady:              source.RetryReady,
		sdkUserHistoryOffsets:   source.SdkUserHistoryOffsets,
		reconcileHistory:        source.ReconcileHistory,
		sdkWireInputs:           source.SdkWireInputs,
		sdkWireInputKnown:       source.SdkWireInputKnown,
		sdkWireRequestDigest:    source.SdkWireRequestDigest,
		sdkAssistantYields:      source.SdkAssistantYields,
		compactionKey:           source.CompactionKey,
		compactionUnknownYields: source.CompactionUnknownYields,
		sdkQueryObserved:        source.SdkQueryObserved,
		sdkAPIRecorded:          source.SdkAPIRecorded,
		identity:                source.Identity,
		inputDigest:             source.InputDigest,
		startedAt:               source.StartedAt,
		resultIDs:               source.ResultIDs,
		attempt:                 source.Attempt,
		settled:                 source.Settled,
		succeeded:               source.Succeeded}
}
func storeSDKAccounting(source sdkAccounting) sdkStoredAccounting {
	result := sdkStoredAccounting{ReactiveCompactionAttempted: source.reactiveCompactionAttempted,
		RecoveredAPIFailures:        source.recoveredAPIFailures,
		CompactionSummaryUserYields: source.compactionSummaryUserYields,
		UnknownCompactionYields:     source.unknownCompactionYields,
		PendingCompactions:          source.pendingCompactions,
		ToolResultUserYields:        source.toolResultUserYields,
		InterruptionUserYields:      source.interruptionUserYields,
		CatalogComplete:             source.catalogComplete,
		RetryStatus:                 source.retryStatus,
		Queries:                     source.queries,
		NumTurns:                    source.numTurns,
		ApiSuccesses:                source.apiSuccesses,
		ApiBaselineMS:               source.apiBaselineMS,
		FirstAssistantMessageAt:     source.firstAssistantMessageAt,
		SawRetry:                    source.sawRetry,
		SawCompact:                  source.sawCompact,
		CancelledStreaming:          source.cancelledStreaming,
		UnknownUserYields:           source.unknownUserYields,
		McpAliases:                  source.mcpAliases,
		Helpers:                     source.helpers,
		IncompleteReason:            source.incompleteReason,
		Result:                      source.result}
	if source.tools != nil {
		result.Tools = make(map[string]sdkStoredTool, len(source.tools))
		for key, value := range source.tools {
			result.Tools[key] = sdkStoredTool{value.count, value.kind}
		}
	}
	if source.compactions != nil {
		result.Compactions = make(map[string]sdkStoredCompaction, len(source.compactions))
		for key, value := range source.compactions {
			if value != nil {
				result.Compactions[key] = sdkStoredCompaction{
					ToolResults: value.observation.input.toolResults, InputKnown: value.observation.input.known,
					SummaryBytes: value.observation.summary.bytes, SummaryHash: value.observation.summary.sha256, SummaryReviewed: value.observation.summary.reviewed,
					Applied: value.applied, Discarded: value.discarded}
			}
		}
	}
	return result
}
func restoreSDKAccounting(source sdkStoredAccounting, history *sdkHistory, ledger *sdkAPILedger) sdkAccounting {
	result := sdkAccounting{history: history, ledger: ledger, reactiveCompactionAttempted: source.ReactiveCompactionAttempted,
		recoveredAPIFailures:        source.RecoveredAPIFailures,
		compactionSummaryUserYields: source.CompactionSummaryUserYields,
		unknownCompactionYields:     source.UnknownCompactionYields,
		pendingCompactions:          source.PendingCompactions,
		toolResultUserYields:        source.ToolResultUserYields,
		interruptionUserYields:      source.InterruptionUserYields,
		catalogComplete:             source.CatalogComplete,
		retryStatus:                 source.RetryStatus,
		queries:                     source.Queries,
		numTurns:                    source.NumTurns,
		apiSuccesses:                source.ApiSuccesses,
		apiBaselineMS:               source.ApiBaselineMS,
		firstAssistantMessageAt:     source.FirstAssistantMessageAt,
		sawRetry:                    source.SawRetry,
		sawCompact:                  source.SawCompact,
		cancelledStreaming:          source.CancelledStreaming,
		unknownUserYields:           source.UnknownUserYields,
		mcpAliases:                  source.McpAliases,
		helpers:                     source.Helpers,
		incompleteReason:            source.IncompleteReason,
		result:                      source.Result}
	if source.Tools != nil {
		result.tools = make(map[string]sdkToolOccurrences, len(source.Tools))
		for key, value := range source.Tools {
			result.tools[key] = sdkToolOccurrences{value.Count, value.Kind}
		}
	}
	if source.Compactions != nil {
		result.compactions = make(map[string]*sdkCompaction, len(source.Compactions))
		for key, value := range source.Compactions {
			result.compactions[key] = &sdkCompaction{
				observation: SDKCompactionObservation{
					input:   SDKCompactionInput{toolResults: value.ToolResults, known: value.InputKnown},
					summary: SDKCompactionSummary{bytes: value.SummaryBytes, sha256: value.SummaryHash, reviewed: value.SummaryReviewed}},
				applied: value.Applied, discarded: value.Discarded}
		}
	}
	return result
}
func storeSDKPrompt(source *state) sdkStoredPrompt {
	result := sdkStoredPrompt{SdkInputClaimed: source.sdkInputClaimed,
		ControlInputStarted: source.controlInputStarted,
		Failed:              source.failed,
		Key:                 source.key,
		Identity:            source.identity,
		StartedAt:           source.startedAt,
		LastAt:              source.lastAt,
		FinishedAt:          source.finishedAt,
		FirstByteAt:         source.firstByteAt,
		FirstSessionPrompt:  source.firstSessionPrompt,
		Pending:             source.pending,
		ApiCalls:            source.apiCalls,
		ApiDuration:         source.apiDuration,
		ToolRequests:        source.toolRequests,
		ToolResults:         source.toolResults,
		IncompleteReason:    source.incompleteReason,
		Complete:            source.complete, Accounting: storeSDKAccounting(source.sdk), Requests: make(map[string]sdkStoredCall, len(source.requests))}
	for key, value := range source.requests {
		result.Requests[key] = storeSDKCall(value)
		if source.active == value {
			result.Active = key
		}
	}
	return result
}
func restoreSDKPrompt(source sdkStoredPrompt, scope string, history *sdkHistory, ledger *sdkAPILedger) *state {
	result := &state{scope: scope, sdkInputClaimed: source.SdkInputClaimed,
		controlInputStarted: source.ControlInputStarted,
		failed:              source.Failed,
		key:                 source.Key,
		identity:            source.Identity,
		startedAt:           source.StartedAt,
		lastAt:              source.LastAt,
		finishedAt:          source.FinishedAt,
		firstByteAt:         source.FirstByteAt,
		firstSessionPrompt:  source.FirstSessionPrompt,
		pending:             source.Pending,
		apiCalls:            source.ApiCalls,
		apiDuration:         source.ApiDuration,
		toolRequests:        source.ToolRequests,
		toolResults:         source.ToolResults,
		incompleteReason:    source.IncompleteReason,
		complete:            source.Complete, sdk: restoreSDKAccounting(source.Accounting, history, ledger), requests: make(map[string]*call, len(source.Requests))}
	for key, value := range source.Requests {
		result.requests[key] = restoreSDKCall(value, result)
	}
	result.active = result.requests[source.Active]
	return result
}

func (t *Tracker) loadSDKSessionLocked(scope string) {
	if t.store == nil {
		return
	}
	for _, s := range t.prompts {
		if s.scope == scope {
			return
		}
	}
	if t.sessions == nil {
		t.sessions = make(map[string]*sdkSessionPersistence)
	}
	persisted := &sdkSessionPersistence{}
	t.sessions[scope] = persisted
	payload, revision, err := t.store.Load(scope)
	persisted.revision = revision
	if err != nil {
		persisted.err, persisted.loadFailed = err, true
		return
	}
	if len(payload) == 0 {
		if revision != "" {
			persisted.err, persisted.loadFailed = ErrSDKSessionInvalid, true
		}
		return
	}
	record, err := decodeSDKSession(payload, scope)
	// The resident cache target is not a validity bound. Live owners can
	// legitimately exceed it; the authenticated payload retains its byte and
	// per-field limits, and restored callbacks never acquire live leases.
	if err != nil {
		persisted.err, persisted.loadFailed = ErrSDKSessionInvalid, true
		return
	}
	history := restoreSDKHistory(record.History)
	ledger := &sdkAPILedger{durationMS: record.DurationMS, incompleteReason: record.LedgerIssue}
	if record.HistoryOnly {
		if t.restoredHistories == nil {
			t.restoredHistories = make(map[string]*sdkRestoredHistory)
		}
		t.restoredHistories[scope] = &sdkRestoredHistory{history: history, ledger: ledger}
	}
	if record.PersistenceIssue {
		persisted.err = ErrSDKSessionUnavailable
	}
	for key, stored := range record.Prompts {
		s := restoreSDKPrompt(stored, scope, history, ledger)
		// A restarted process does not own the old network operation or callback.
		// Keep its UUIDs and observed facts, but never replay it or invent success.
		for _, c := range s.requests {
			if !c.settled {
				c.settled, c.retryReady = true, false
				s.failed, s.active = true, nil
				s.incompleteReason = "sdk-session-interrupted-during-query"
				s.sdk.incompleteReason = s.incompleteReason
				persisted.err = ErrSDKSessionUnavailable
				history.unknown(s.incompleteReason)
				ledger.incompleteReason = s.incompleteReason
			} else if c.succeeded && !c.sdkAPIRecorded {
				ledger.incompleteReason = "sdk-session-unsettled-api-accounting"
				persisted.err = ErrSDKSessionUnavailable
			}
		}
		t.prompts[key] = s
	}
	persisted.digest = sdkCompactionHash(string(payload))
}

func (t *Tracker) saveSDKSessionLocked(scope string) {
	t.saveNativeContentLocked(scope)
	if t.store == nil {
		return
	}
	persisted := t.sessions[scope]
	if persisted == nil || persisted.loadFailed {
		return
	}
	record := sdkStoredSession{Version: 1, Scope: scope, PersistenceIssue: persisted.err != nil, Prompts: make(map[string]sdkStoredPrompt)}
	var history *sdkHistory
	var ledger *sdkAPILedger
	for key, s := range t.prompts {
		if s.scope != scope {
			continue
		}
		if history == nil {
			history, ledger = s.sdk.history, s.sdk.ledger
		}
		if history == nil || ledger == nil || history != s.sdk.history || ledger != s.sdk.ledger {
			persisted.err = ErrSDKSessionInvalid
			return
		}
		record.Prompts[key] = storeSDKPrompt(s)
	}
	if len(record.Prompts) == 0 {
		restored := t.restoredHistories[scope]
		if restored == nil {
			return
		}
		history, ledger, record.HistoryOnly = restored.history, restored.ledger, true
	}
	record.History, record.DurationMS, record.LedgerIssue = storeSDKHistory(history), ledger.durationMS, ledger.incompleteReason
	if n := t.nativeContent[scope]; n != nil {
		record.HistoryCleared = n.clearedToEmpty
	}
	payload, err := json.Marshal(record)
	if err != nil || len(payload) > MaxSDKSessionStateBytes {
		persisted.err = ErrSDKSessionInvalid
		return
	}
	hash := sdkCompactionHash(string(payload))
	if hash == persisted.digest {
		return
	}
	revision, err := t.store.Save(scope, persisted.revision, payload)
	if err != nil {
		persisted.err = err
		if errors.Is(err, ErrSDKSessionStale) || errors.Is(err, ErrSDKSessionInvalid) {
			persisted.loadFailed = true
		}
		return
	}
	persisted.revision, persisted.digest = revision, hash
	// A later successful write cannot erase evidence of an earlier lost state.
}

// SDKSessionStateError reports protected state availability independently of
// an API result. It never returns a path, payload, credential or caller string.
func (t *Tracker) SDKSessionStateError(accountID, sessionID string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nativeContentErrorLocked(digest(accountID, sessionID)) {
		return ErrSDKSessionUnavailable
	}
	if state := t.sessions[digest(accountID, sessionID)]; state != nil && state.err != nil {
		return ErrSDKSessionUnavailable
	}
	return nil
}

func (r *Request) SDKSessionStateError() error {
	if r == nil {
		return nil
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.tracker.nativeContentErrorLocked(r.call.state.scope) {
		return ErrSDKSessionUnavailable
	}
	if state := r.tracker.sessions[r.call.state.scope]; state != nil && state.err != nil {
		return ErrSDKSessionUnavailable
	}
	return nil
}

// CheckpointSDKSessionState saves a completed observer boundary, not each text
// delta. Native UUID/usage changes therefore survive an interrupted response.
func (r *Request) CheckpointSDKSessionState() error {
	if r == nil {
		return nil
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	r.tracker.saveSDKSessionLocked(r.call.state.scope)
	if r.tracker.nativeContentErrorLocked(r.call.state.scope) {
		return ErrSDKSessionUnavailable
	}
	if state := r.tracker.sessions[r.call.state.scope]; state != nil && state.err != nil {
		return ErrSDKSessionUnavailable
	}
	return nil
}

func (o *SDKHelperOwner) SDKSessionStateError() error {
	if o == nil {
		return nil
	}
	o.tracker.mu.Lock()
	defer o.tracker.mu.Unlock()
	if o.tracker.nativeContentErrorLocked(o.state.scope) {
		return ErrSDKSessionUnavailable
	}
	if state := o.tracker.sessions[o.state.scope]; state != nil && state.err != nil {
		return ErrSDKSessionUnavailable
	}
	return nil
}

func validSDKStateHash(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32
}

func decodeSDKSession(payload []byte, scope string) (sdkStoredSession, error) {
	var record sdkStoredSession
	if len(payload) == 0 || len(payload) > MaxSDKSessionStateBytes {
		return record, ErrSDKSessionInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil {
		return record, ErrSDKSessionInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || record.Version != 1 || record.Scope != scope || !validSDKStateHash(scope) ||
		(len(record.Prompts) == 0 && !record.HistoryOnly) || (record.HistoryOnly && (len(record.Prompts) != 0 || record.DurationMS != 0 || record.LedgerIssue != "" || len(record.History.Messages) == 0 && !record.HistoryCleared)) || record.DurationMS < 0 || len(record.LedgerIssue) > 256 ||
		(record.HistoryCleared && (len(record.History.Messages) != 0 || len(record.History.ExpectedText) != 0 || !record.History.ExpectedTextKnown || record.History.PendingReconciliations != 0 || record.History.IncompleteReason != "")) ||
		len(record.History.Messages) > maxSDKHistoryMessages || len(record.History.ExpectedText) > maxSDKHistoryMessages ||
		record.History.PendingReconciliations < 0 || len(record.History.IncompleteReason) > 256 {
		return record, ErrSDKSessionInvalid
	}
	seen := make(map[string]bool)
	for _, message := range record.History.Messages {
		id, err := uuid.Parse(message.UUID)
		if err != nil || id == uuid.Nil || seen[message.UUID] || message.TokenEstimate.Tokens < 0 ||
			len(message.MessageID) > 1024 || len(message.Type) > 64 || len(message.Subtype) > 128 ||
			!validSDKStateUsage(message.Usage) || (message.WireParentUUID != "" && !validSDKStateUUID(message.WireParentUUID)) ||
			(message.WireToolResultID != "" && (message.Type != "user" || !validSDKStateHash(message.WireToolResultID))) {
			return record, ErrSDKSessionInvalid
		}
		seen[message.UUID] = true
	}
	for _, hash := range record.History.ExpectedText {
		if !validSDKStateHash(hash) {
			return record, ErrSDKSessionInvalid
		}
	}
	for key, s := range record.Prompts {
		if key != s.Key || key != digest(scope, s.Identity.PromptID) || !validSDKStateHash(key) ||
			!validSDKStateUUID(s.Identity.PromptID) || !validSDKStateUUID(s.Identity.QueryChainID) || s.Identity.QueryDepth < 0 ||
			len(s.Requests) == 0 || len(s.Requests) > maxRequestsPerPrompt || len(s.Pending) > maxRequestsPerPrompt*maxToolsPerRequest ||
			s.ApiCalls < 0 || s.ApiDuration < 0 || s.ToolRequests < 0 || s.ToolResults < 0 || len(s.IncompleteReason) > 256 ||
			s.Accounting.ApiBaselineMS < 0 || s.Accounting.ApiBaselineMS > record.DurationMS ||
			len(s.Accounting.Tools) > maxRequestsPerPrompt*maxToolsPerRequest || len(s.Accounting.Compactions) > maxRequestsPerPrompt ||
			len(s.Accounting.Helpers) > maxRequestsPerPrompt || len(s.Accounting.McpAliases) > maxToolsPerRequest ||
			!validSDKStateAccounting(s.Accounting) {
			return record, ErrSDKSessionInvalid
		}
		if s.Active != "" {
			if _, ok := s.Requests[s.Active]; !ok {
				return record, ErrSDKSessionInvalid
			}
		}
		for hash := range s.Pending {
			if !validSDKStateHash(hash) {
				return record, ErrSDKSessionInvalid
			}
		}
		for requestKey, c := range s.Requests {
			if !validSDKStateHash(requestKey) || !validSDKStateHash(c.InputDigest) || c.Identity.PromptID != s.Identity.PromptID ||
				c.Identity.QueryChainID != s.Identity.QueryChainID || c.Attempt < 1 || c.Identity.QueryDepth < 0 ||
				len(c.ResultIDs) > maxToolsPerRequest || len(c.SdkWireInputs) > maxSDKHistoryMessages ||
				len(c.SdkUserHistoryOffsets) > maxToolsPerRequest || c.SdkAssistantYields < 0 ||
				(c.SdkWireRequestDigest != "" && !validSDKStateHash(c.SdkWireRequestDigest)) ||
				(c.CompactionKey != "" && !validSDKStateHash(c.CompactionKey)) ||
				(c.Succeeded && !c.Settled) || (c.SdkAPIRecorded && !c.Succeeded) {
				return record, ErrSDKSessionInvalid
			}
			for _, hashes := range [][]string{c.ResultIDs, c.SdkWireInputs} {
				for _, hash := range hashes {
					if !validSDKStateHash(hash) {
						return record, ErrSDKSessionInvalid
					}
				}
			}
			for _, offset := range c.SdkUserHistoryOffsets {
				if offset < 0 {
					return record, ErrSDKSessionInvalid
				}
			}
		}
	}
	return record, nil
}

func validSDKStateUsage(usage SDKTokenUsage) bool {
	var total int64
	for _, value := range []int64{usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens} {
		if value < 0 || value > int64(1<<63-1)-total {
			return false
		}
		total += value
	}
	return true
}

func validSDKStateAccounting(accounting sdkStoredAccounting) bool {
	if len(accounting.IncompleteReason) > 256 {
		return false
	}
	for _, value := range []int{accounting.RecoveredAPIFailures, accounting.CompactionSummaryUserYields, accounting.PendingCompactions,
		accounting.ToolResultUserYields, accounting.InterruptionUserYields, accounting.RetryStatus, accounting.Queries, accounting.NumTurns, accounting.ApiSuccesses} {
		if value < 0 {
			return false
		}
	}
	for _, hashes := range []map[string]struct{}{accounting.Helpers, accounting.McpAliases} {
		for hash := range hashes {
			if !validSDKStateHash(hash) {
				return false
			}
		}
	}
	for hash, tool := range accounting.Tools {
		if !validSDKStateHash(hash) || tool.Count < 1 || (tool.Kind != "builtin" && tool.Kind != "mcp" && tool.Kind != "toolsearch") {
			return false
		}
	}
	for hash, compact := range accounting.Compactions {
		if !validSDKStateHash(hash) || compact.SummaryBytes < 0 || (compact.Applied && compact.Discarded) ||
			len(compact.ToolResults) > maxRequestsPerPrompt*maxToolsPerRequest ||
			(compact.SummaryHash != "" && !validSDKStateHash(compact.SummaryHash)) || (compact.SummaryReviewed && compact.SummaryHash == "") {
			return false
		}
		for tool := range compact.ToolResults {
			if !validSDKStateHash(tool) {
				return false
			}
		}
	}
	if result := accounting.Result; result != nil {
		if result.APIDurationMS < 0 || len(result.IncompleteReason) > 256 {
			return false
		}
		for _, value := range []int{result.RecoveredAPIFailures, result.CompactionSummaryUserYields, result.PendingCompactions,
			result.ToolResultUserYields, result.InterruptionUserYields, result.RetryStatus, result.Queries, result.NumTurns,
			result.ToolUseCount, result.MCPToolCalls, result.BuiltinToolCalls, result.ToolSearchCalls} {
			if value < 0 {
				return false
			}
		}
	}
	return true
}

func validSDKStateUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil
}
