package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

var ErrSDKResumeReconstructionRequired = errors.New("saved SDK history requires native interruption, attachment or hook reconstruction")

// SDKRemoteWireResume is the exact previously committed protocol projection.
// It is deliberately not an FNo/Oyt result. Histories needing reconstruction
// must use the native loader, rather than being silently flattened or cleared.
// Original native rows, UUIDs, counters and protected files remain unchanged.
type SDKRemoteWireResume struct {
	tracker                            *Tracker
	account, session, revision         string
	structuralRevision, nativeRevision string
	messages                           []json.RawMessage
}

func (*SDKRemoteWireResume) MarshalJSON() ([]byte, error) { return nil, ErrSDKSessionInvalid }

// PrepareRemoteWireResume accepts only a lossless projection that independently
// matches the last committed wire-history checkpoint. It joins transcript writes
// through RestoreNativeContent, but never invents a request or successful turn.
func (t *Tracker) PrepareRemoteWireResume(account, session string) (*SDKRemoteWireResume, error) {
	content, err := t.RestoreNativeContent(account, session)
	if err != nil {
		return nil, err
	}
	rows, err := committedNativeWireRowsWithClear(content.ActiveMessages, content.ClearedToEmpty)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
	}{rows})
	actual, known := sdkWireHistoryFingerprints(body, false)
	if content.ClearedToEmpty && len(rows) == 0 {
		actual, known = []string{}, true
	}
	if !known {
		return nil, ErrSDKResumeReconstructionRequired
	}
	scope := digest(account, session)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.nativeSessionActiveLocked(scope) {
		return nil, ErrSDKSessionActive
	}
	n, persisted := t.nativeContent[scope], t.sessions[scope]
	if n == nil || persisted == nil {
		return nil, ErrSDKSessionUnavailable
	}
	if persisted.revision != content.structuralRevision || n.persisted.revision != content.nativeRevision {
		return nil, ErrSDKSessionStale
	}
	if err := t.verifyNativeResumeCheckpointsLocked(scope, n, persisted); err != nil {
		return nil, err
	}
	var history *sdkHistory
	if restored := t.restoredHistories[scope]; restored != nil {
		history = restored.history
	}
	for _, state := range t.prompts {
		if state.scope == scope {
			history = state.sdk.history
			break
		}
	}
	if history == nil || history.incompleteReason != "" || history.pendingReconciliations != 0 || !history.expectedTextKnown || !slices.Equal(actual, history.expectedText) {
		return nil, ErrSDKResumeReconstructionRequired
	}
	return &SDKRemoteWireResume{tracker: t, account: account, session: session, revision: content.TranscriptRevision,
		structuralRevision: persisted.revision, nativeRevision: n.persisted.revision, messages: rows}, nil
}

// Read revalidates immediately before the new actor takes ownership. A second
// writer or completed callback cannot make the earlier preparation authoritative.
func (r *SDKRemoteWireResume) Read() ([]json.RawMessage, error) {
	if r == nil || r.tracker == nil {
		return nil, ErrSDKSessionInvalid
	}
	current, err := r.tracker.PrepareRemoteWireResume(r.account, r.session)
	if err != nil {
		return nil, err
	}
	if current.revision != r.revision || current.structuralRevision != r.structuralRevision || current.nativeRevision != r.nativeRevision {
		return nil, ErrSDKSessionStale
	}
	rows := make([]json.RawMessage, len(r.messages))
	for i, row := range r.messages {
		rows[i] = bytes.Clone(row)
	}
	return rows, nil
}

// Native assistant block yields share one API message ID. Reassemble those
// blocks without serializing transcript metadata or merging different messages.
// The committed fingerprint gate above proves this projection against actual
// observed wire content; grouping alone never grants permission to resume.
func committedNativeWireRows(native []SDKNativeMessage) ([]json.RawMessage, error) {
	return committedNativeWireRowsWithClear(native, false)
}

func committedNativeWireRowsWithClear(native []SDKNativeMessage, cleared bool) ([]json.RawMessage, error) {
	if cleared {
		if len(native) != 0 {
			return nil, ErrSDKSessionInvalid
		}
		return []json.RawMessage{}, nil
	}
	type message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var messages []message
	var lastID string
	seenIDs := make(map[string]bool)
	for _, row := range native {
		if row.Type == "system" && row.Subtype == "compact_boundary" {
			continue
		}
		if row.Type != "user" && row.Type != "assistant" {
			return nil, ErrSDKResumeReconstructionRequired
		}
		var value struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(row.Message, &value) != nil || value.Role != row.Type || !json.Valid(value.Content) {
			return nil, ErrSDKSessionInvalid
		}
		if row.Type == "user" && len(row.Origin) != 0 {
			var origin struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal(row.Origin, &origin) != nil {
				return nil, ErrSDKSessionInvalid
			}
			if origin.Kind == "task-notification" {
				var text string
				if json.Unmarshal(value.Content, &text) != nil {
					return nil, ErrSDKResumeReconstructionRequired
				}
				value.Content, _ = json.Marshal(SDKTaskNotificationWire(text))
			}
		}
		if row.Type == "assistant" {
			if value.ID == "" {
				return nil, ErrSDKResumeReconstructionRequired
			}
			if value.ID == lastID && len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
				var previous, next []json.RawMessage
				if json.Unmarshal(messages[len(messages)-1].Content, &previous) != nil || json.Unmarshal(value.Content, &next) != nil || previous == nil || next == nil {
					return nil, ErrSDKResumeReconstructionRequired
				}
				messages[len(messages)-1].Content, _ = json.Marshal(append(previous, next...))
				continue
			}
			if seenIDs[value.ID] {
				return nil, ErrSDKResumeReconstructionRequired
			}
			seenIDs[value.ID] = true
		}
		lastID = value.ID
		messages = append(messages, message{Role: value.Role, Content: bytes.Clone(value.Content)})
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
		return nil, ErrSDKResumeReconstructionRequired
	}
	rows := make([]json.RawMessage, len(messages))
	for i, value := range messages {
		rows[i], _ = json.Marshal(value)
	}
	return rows, nil
}
