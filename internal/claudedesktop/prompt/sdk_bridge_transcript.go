package prompt

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// SDKBridgeTranscriptRecord is native transcript metadata, not a user message,
// worker credential, or management response. Empty bridge identity is a clear.
type SDKBridgeTranscriptRecord struct {
	Type                  string   `json:"type"`
	SessionID             string   `json:"sessionId"`
	BridgeSessionID       string   `json:"bridgeSessionId"`
	LastSequenceNum       int64    `json:"lastSequenceNum"`
	DeclaredDialogKinds   []string `json:"declaredDialogKinds,omitempty"`
	SessionGroupingID     string   `json:"sessionGroupingId,omitempty"`
	NoHistoryBackfill     bool     `json:"noHistoryBackfill,omitempty"`
	OwnerAccountUUID      string   `json:"ownerAccountUuid,omitempty"`
	OwnerOrganizationUUID string   `json:"ownerOrganizationUuid,omitempty"`
}

func cloneSDKBridgeTranscript(value *SDKBridgeTranscriptRecord) *SDKBridgeTranscriptRecord {
	if value == nil {
		return nil
	}
	copy := *value
	copy.DeclaredDialogKinds = slices.Clone(value.DeclaredDialogKinds)
	return &copy
}

// ParseSDKBridgeTranscriptRecord validates the metadata emitted by this owned
// writer. It is not the native loader's tolerance policy for arbitrary files.
func ParseSDKBridgeTranscriptRecord(raw []byte) (SDKBridgeTranscriptRecord, error) {
	var value SDKBridgeTranscriptRecord
	var fields map[string]json.RawMessage
	if decodeNativeTranscriptRow(raw, &value) != nil || json.Unmarshal(raw, &fields) != nil {
		return value, ErrSDKSessionInvalid
	}
	for _, key := range []string{"type", "sessionId", "bridgeSessionId", "lastSequenceNum"} {
		if len(fields[key]) == 0 || bytes.Equal(fields[key], []byte("null")) {
			return value, ErrSDKSessionInvalid
		}
	}
	id, err := uuid.Parse(value.SessionID)
	if err != nil || id == uuid.Nil || id.String() != value.SessionID || value.Type != "bridge-session" ||
		value.LastSequenceNum < 0 || value.LastSequenceNum > 9007199254740991 {
		return value, ErrSDKSessionInvalid
	}
	if value.BridgeSessionID == "" {
		if value.LastSequenceNum != 0 || len(fields) != 4 {
			return value, ErrSDKSessionInvalid
		}
		return value, nil
	}
	suffix := strings.TrimPrefix(value.BridgeSessionID, "cse_")
	if suffix == value.BridgeSessionID || len(suffix) == 0 || len(suffix) > 64 {
		return value, ErrSDKSessionInvalid
	}
	for _, ch := range suffix {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			return value, ErrSDKSessionInvalid
		}
	}
	return value, nil
}

// RecordNativeBridgeTranscript receives an exact record/Host lifecycle fact.
// It neither begins a prompt nor writes a fake message to create a transcript.
// Before the first native row it retains a metadata seed; afterward every
// checkpoint appends, including repeated identical values (no UUID/stamp dedup).
func (t *Tracker) RecordNativeBridgeTranscript(accountID string, value SDKBridgeTranscriptRecord, onFailure func(error)) error {
	if t == nil || accountID == "" {
		return ErrSDKSessionUnavailable
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ErrSDKSessionInvalid
	}
	if _, err = ParseSDKBridgeTranscriptRecord(raw); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.store == nil || t.nativeOptions.Store == nil || t.nativeOptions.TranscriptStore == nil {
		return ErrSDKSessionUnavailable
	}
	scope := digest(accountID, value.SessionID)
	if t.prompts == nil {
		t.prompts = make(map[string]*state)
	}
	t.loadSDKSessionLocked(scope)
	t.loadNativeContentLocked(scope, value.SessionID)
	n := t.nativeContent[scope]
	if n == nil || n.persisted.loadFailed || n.issue != "" || t.transcript == nil {
		return ErrSDKSessionUnavailable
	}
	return t.transcript.enqueueBridge(scope, value, onFailure)
}

func (w *sdkTranscriptWriter) enqueueBridge(scope string, value SDKBridgeTranscriptRecord, onFailure func(error)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	state := w.scopes[scope]
	if w.sealed || state == nil || state.loadErr != nil || state.path == "" {
		return ErrSDKSessionUnavailable
	}
	if state.revision == "" && len(state.known) == 0 {
		// Native saveBridgeSession updates current metadata even when no
		// transcript file exists. Its first-message seed supplies the row.
		state.pendingBridge, state.bridgeFailure = nil, nil
		if value.BridgeSessionID != "" {
			state.pendingBridge, state.bridgeFailure = cloneSDKBridgeTranscript(&value), onFailure
		}
		return nil
	}
	w.enqueueBridgeLocked(scope, value, onFailure)
	return nil
}

func (w *sdkTranscriptWriter) enqueueBridgeLocked(scope string, value SDKBridgeTranscriptRecord, onFailure func(error)) {
	row := cloneSDKBridgeTranscript(&value)
	if _, exists := w.queues[scope]; !exists {
		w.order = append(w.order, scope)
	}
	w.queues[scope] = append(w.queues[scope], sdkTranscriptItem{
		done: make(chan struct{}), onFailure: onFailure,
		serialize: func() ([]byte, error) {
			var buffer bytes.Buffer
			encoder := json.NewEncoder(&buffer)
			encoder.SetEscapeHTML(false)
			if encoder.Encode(row) != nil {
				return nil, ErrSDKSessionInvalid
			}
			return buffer.Bytes(), nil
		},
	})
	w.scheduleLocked()
}
