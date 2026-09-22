package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	factIssuesFile           = "fact-issues.json"
	factIssueSDKPrompt       = "sdk-prompt"
	factIssueSDKInput        = "sdk-input"
	factIssueOwnedContext    = "owned-context"
	factIssueSDKSessionState = "sdk-session-state"
	factIssueSDKCompaction   = "sdk-compaction"
	factIssueSDKFeatures     = "sdk-features"
	factIssueSDKExposure     = "sdk-feature-exposure"
	factIssueSDKBridge       = "sdk-bridge-lifecycle"
	factIssueSDKTranscript   = "sdk-transcript-reconstruction"
	factIssueTitle           = "title-response"
	factIssueCacheDiagnosis  = "cache-diagnosis"
	factIssueFastOverage     = "fast-overage"
)

// The ledger is local diagnostic state, never an upstream event. It adds scoped
// hashes and fixed categories to the existing encrypted account binding. Prompt
// text, response content and unsupported source values are never retained.
type factIssueRecord struct {
	Version  int               `json:"version"`
	Binding  Binding           `json:"binding"`
	Issues   map[string]string `json:"issues"`
	Overflow bool              `json:"overflow"`
}

type FactIssueStatus struct {
	sessionStateUnresolved int
	compactionUnresolved   int
	inputUnresolved        int
	ownedContextUnresolved int
	featureUnresolved      int
	Status                 string `json:"status"`
	Reason                 string `json:"reason"`
	Unresolved             int    `json:"unresolved"`
	Overflow               bool   `json:"overflow"`
	StorageError           bool   `json:"storage_error"`
}

func readFactIssueRecord(directory string) (factIssueRecord, error) {
	file := filepath.Join(directory, factIssuesFile)
	info, errStat := os.Stat(file)
	if errStat != nil {
		return factIssueRecord{}, errStat
	}
	if info.Size() > 1024*1024 {
		return factIssueRecord{}, fmt.Errorf("telemetry fact issue record exceeds limit")
	}
	encoded, errRead := readProtectedFile(file)
	if errRead != nil {
		return factIssueRecord{}, fmt.Errorf("telemetry fact issue record cannot be read")
	}
	var record factIssueRecord
	if json.Unmarshal(encoded, &record) != nil || record.Version != 1 || len(record.Issues) > maxPromptIssues {
		return factIssueRecord{}, fmt.Errorf("invalid telemetry fact issue record")
	}
	for key, kind := range record.Issues {
		_, errDecode := hex.DecodeString(key)
		if len(key) != sha256.Size*2 || errDecode != nil || !validFactIssueKind(kind) {
			return factIssueRecord{}, fmt.Errorf("invalid telemetry fact issue entry")
		}
	}
	return record, nil
}

func (w *accountWorker) loadFactIssues() {
	record, errRead := readFactIssueRecord(w.directory)
	if errors.Is(errRead, os.ErrNotExist) {
		return
	}
	if errRead != nil || record.Binding != w.binding {
		// Preserve unreadable evidence; do not overwrite it on the next request.
		w.factIssuesUnreadable = true
		return
	}
	w.factIssues, w.factIssuesOverflow = record.Issues, record.Overflow
}

// setFactIssue resolves only this exact kind/session/owner within the worker's
// auth/profile/egress binding. Success in another request cannot clear it.
func (w *accountWorker) setFactIssue(kind, session, owner string, missing bool) error {
	if w == nil {
		return nil
	}
	if !validFactIssueKind(kind) {
		return fmt.Errorf("invalid telemetry fact issue kind")
	}
	identity, _ := json.Marshal([]string{w.binding.BindingRevision, kind, session, owner})
	digest := sha256.Sum256(identity)
	key := hex.EncodeToString(digest[:])
	w.factIssueMu.Lock()
	defer w.factIssueMu.Unlock()
	if w.factIssues == nil {
		w.factIssues = make(map[string]string)
	}
	_, exists := w.factIssues[key]
	changed := false
	if missing {
		if !exists && len(w.factIssues) < maxPromptIssues {
			w.factIssues[key] = kind
			changed = true
		} else if !exists && !w.factIssuesOverflow {
			w.factIssuesOverflow, changed = true, true
		}
	} else if exists && session != "" && owner != "" {
		delete(w.factIssues, key)
		changed = true
	}
	if w.factIssuesUnreadable {
		return fmt.Errorf("telemetry fact issue store is unreadable; original retained")
	}
	if !changed && !w.factIssuesWriteFailed {
		return nil
	}
	encoded, errEncode := json.Marshal(factIssueRecord{Version: 1, Binding: w.binding, Issues: w.factIssues, Overflow: w.factIssuesOverflow})
	if errEncode == nil {
		errEncode = writeProtectedFile(filepath.Join(w.directory, factIssuesFile), encoded)
	}
	w.factIssuesWriteFailed = errEncode != nil
	if errEncode != nil {
		return fmt.Errorf("telemetry fact issue state could not be persisted")
	}
	return nil
}

func validFactIssueKind(kind string) bool {
	switch kind {
	case factIssueSDKPrompt, factIssueSDKInput, factIssueOwnedContext, factIssueSDKSessionState, factIssueSDKCompaction, factIssueSDKFeatures, factIssueSDKExposure, factIssueSDKBridge, factIssueSDKTranscript, factIssueTitle, factIssueCacheDiagnosis, factIssueFastOverage:
		return true
	default:
		return false
	}
}

func requestFactOwner(facts RequestFacts) string {
	// Retry attempts share a caller request ID but not response ownership.
	encoded, _ := json.Marshal([]any{facts.ClientRequestID, facts.Attempt, facts.StartedAt.UTC().Format(time.RFC3339Nano)})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (s *RequestSpan) retainResponseFactIssue(kind string, facts RequestFacts) {
	if errSet := s.sdkWorker.setFactIssue(kind, facts.SessionID, requestFactOwner(facts), true); errSet != nil {
		s.sdkWorker.recordQueueFailure(errSet)
	}
}

func (w *accountWorker) factIssueSnapshot() *FactIssueStatus {
	w.factIssueMu.Lock()
	defer w.factIssueMu.Unlock()
	count := len(w.factIssues) + len(w.featureHostIssues)
	if count == 0 && !w.factIssuesOverflow && !w.factIssuesUnreadable && !w.factIssuesWriteFailed {
		return nil
	}
	status := "awaiting-response-facts"
	for _, kind := range w.factIssues {
		if kind == factIssueSDKPrompt || kind == factIssueSDKInput || kind == factIssueOwnedContext || kind == factIssueSDKSessionState || kind == factIssueSDKCompaction || kind == factIssueSDKTranscript {
			status = "awaiting-sdk-prompt-facts"
			break
		}
	}
	reason := fmt.Sprintf("%d unresolved telemetry fact scope(s)", count)
	inputIssues := 0
	contextIssues := 0
	compactionIssues := 0
	sessionStateIssues := 0
	featureIssues := len(w.featureHostIssues)
	bridgeIssues := 0
	transcriptIssues := 0
	for _, kind := range w.factIssues {
		if kind == factIssueSDKTranscript {
			transcriptIssues++
		}
		if kind == factIssueSDKBridge {
			bridgeIssues++
		}
		if kind == factIssueSDKFeatures || kind == factIssueSDKExposure {
			featureIssues++
		}
		if kind == factIssueSDKInput {
			inputIssues++
		}
		if kind == factIssueOwnedContext {
			contextIssues++
		}
		if kind == factIssueSDKCompaction {
			compactionIssues++
		}
		if kind == factIssueSDKSessionState {
			sessionStateIssues++
		}
	}
	if inputIssues > 0 {
		reason += fmt.Sprintf("; %d input submission fact scope(s) unavailable", inputIssues)
	}
	if contextIssues > 0 {
		reason += "; owned conversation context could not be verified or persisted"
	}
	if compactionIssues > 0 {
		reason += "; reactive compaction facts or event persistence are unavailable"
	}
	if sessionStateIssues > 0 {
		reason += "; SDK session ownership state could not be verified or persisted"
	}
	if featureIssues > 0 {
		reason += "; SDK feature cache, refresh or exposure persistence is unavailable"
		if status == "awaiting-response-facts" {
			status = "awaiting-sdk-feature-facts"
		}
	}
	if bridgeIssues > 0 {
		reason += "; SDK bridge lifecycle facts or event persistence are unavailable"
		if status == "awaiting-response-facts" {
			status = "awaiting-sdk-bridge-facts"
		}
	}
	if transcriptIssues > 0 {
		reason += "; SDK transcript reconstruction facts or event persistence are unavailable"
	}
	if w.factIssuesOverflow {
		reason += "; additional unresolved scopes exceeded the retention limit"
	}
	storageError := w.factIssuesUnreadable || w.factIssuesWriteFailed
	if storageError {
		status = "fact-issue-storage-error"
		reason += "; diagnostic state cannot be reliably restored or persisted"
	}
	return &FactIssueStatus{Status: status, Reason: reason, Unresolved: count, Overflow: w.factIssuesOverflow, StorageError: storageError, inputUnresolved: inputIssues, ownedContextUnresolved: contextIssues, compactionUnresolved: compactionIssues, sessionStateUnresolved: sessionStateIssues, featureUnresolved: featureIssues}
}

func (m *Manager) retainFactRestoreFailure(role string) {
	m.endpointMu.Lock()
	defer m.endpointMu.Unlock()
	if m.factRestoreFailures == nil {
		m.factRestoreFailures = make(map[string]bool)
	}
	m.factRestoreFailures[role] = true
}

func (m *Manager) recordSDKPromptFactIssue(span *RequestSpan, missing bool) error {
	snapshot := span.facts.Prompt.Snapshot()
	errSDK := span.sdkWorker.setFactIssue(factIssueSDKPrompt, span.facts.SessionID, snapshot.PromptID, missing)
	errLogs := span.auxiliaryWorkers[datadogLogsRole].setFactIssue(factIssueSDKPrompt, span.facts.SessionID, snapshot.PromptID, missing)
	return errors.Join(errSDK, errLogs)
}

// ObserveOwnedContextState keeps a session-scoped failure visible across API
// successes and restarts. Only an actual successful adopted-state write clears
// it; an empty lookup or successful HTTP delivery cannot repair lost history.
func (s *RequestSpan) ObserveOwnedContextState(saved bool) {
	if s == nil || s.facts.Role != "main" {
		return
	}
	for _, worker := range []*accountWorker{s.sdkWorker, s.auxiliaryWorkers[datadogLogsRole]} {
		if err := worker.setFactIssue(factIssueOwnedContext, s.facts.SessionID, "owned-context", !saved); err != nil {
			worker.recordQueueFailure(err)
		}
	}
}

// This is independent of adopted-prefix storage. A successful compaction write
// cannot repair a lost SDK UUID/ledger checkpoint. No success event clears it.
func (s *RequestSpan) ObserveSDKSessionStateUnavailable() {
	if s == nil {
		return
	}
	for _, worker := range []*accountWorker{s.sdkWorker, s.auxiliaryWorkers[datadogLogsRole]} {
		if err := worker.setFactIssue(factIssueSDKSessionState, s.facts.SessionID, "sdk-session-state", true); err != nil {
			worker.recordQueueFailure(err)
		}
	}
}

// Apply retained issues after transient material/readiness updates. A successful
// delivery confirms transport, not that an omitted event's facts were recovered.
func (m *Manager) applyFactIssueStatus(status *Status) {
	byRole := make(map[string]FactIssueStatus)
	for _, account := range status.Accounts {
		if issue := account.FactIssues; issue != nil {
			combined := byRole[account.EndpointRole]
			combined.Unresolved += issue.Unresolved
			combined.inputUnresolved += issue.inputUnresolved
			combined.ownedContextUnresolved += issue.ownedContextUnresolved
			combined.compactionUnresolved += issue.compactionUnresolved
			combined.sessionStateUnresolved += issue.sessionStateUnresolved
			combined.featureUnresolved += issue.featureUnresolved
			combined.Overflow = combined.Overflow || issue.Overflow
			combined.StorageError = combined.StorageError || issue.StorageError
			if combined.Status == "" || issue.Status == "awaiting-sdk-prompt-facts" {
				combined.Status = issue.Status
			}
			byRole[account.EndpointRole] = combined
		}
	}
	m.endpointMu.RLock()
	defer m.endpointMu.RUnlock()
	for i := range status.DeliveryEndpoints {
		endpoint := &status.DeliveryEndpoints[i]
		issue, exists := byRole[endpoint.Role]
		if m.factRestoreFailures[endpoint.Role] {
			issue.StorageError, exists = true, true
		}
		if !exists {
			continue
		}
		reason := fmt.Sprintf("%d unresolved telemetry fact scope(s)", issue.Unresolved)
		if issue.inputUnresolved > 0 {
			reason += fmt.Sprintf("; %d input submission fact scope(s) unavailable", issue.inputUnresolved)
		}
		if issue.ownedContextUnresolved > 0 {
			reason += "; owned conversation context could not be verified or persisted"
		}
		if issue.compactionUnresolved > 0 {
			reason += "; reactive compaction facts or event persistence are unavailable"
		}
		if issue.sessionStateUnresolved > 0 {
			reason += "; SDK session ownership state could not be verified or persisted"
		}
		if issue.featureUnresolved > 0 {
			reason += "; SDK feature cache, refresh or exposure persistence is unavailable"
		}
		if issue.Overflow {
			reason += "; retention limit exceeded"
		}
		if issue.StorageError {
			issue.Status = "fact-issue-storage-error"
			reason += "; diagnostic state could not be restored or persisted"
		}
		if endpoint.Status == "ready" || endpoint.Status == "" || endpoint.Status == "awaiting-sdk-prompt-facts" || endpoint.Status == "awaiting-response-facts" || endpoint.Status == "awaiting-sdk-feature-facts" {
			endpoint.Status, endpoint.Reason = issue.Status, reason
		} else {
			endpoint.Reason += "; " + reason
		}
	}
}
