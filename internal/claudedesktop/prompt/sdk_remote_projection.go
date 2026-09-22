package prompt

import (
	"bytes"
	"encoding/json"
	"slices"
)

// message resolves the separately owned live view without changing immutable
// original rows. New local appends can name a restored parent in that view.
func (n *sdkNativeContent) message(id string) (SDKNativeMessage, bool) {
	if row, ok := n.projected[id]; ok {
		return row, true
	}
	index, ok := n.byUUID[id]
	if !ok {
		return SDKNativeMessage{}, false
	}
	return n.rows[index], true
}

func (n *sdkNativeContent) projectedMessages() []SDKNativeMessage {
	keys := make([]string, 0, len(n.projected))
	for key := range n.projected {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var rows []SDKNativeMessage
	for _, key := range keys {
		rows = append(rows, cloneNativeMessage(n.projected[key]))
	}
	return rows
}

// AdoptCompletedHistory is the actual completed-history adoption boundary.
// The raw mirror remains intact. Unsupported native reconstruction is reported
// explicitly; no hooks, incomplete-turn success or resume event is invented.
func (h *SDKRemoteHydration) AdoptCompletedHistory() (*SDKRemoteWireResume, error) {
	raw, err := h.NativeRows()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	rows := make([]SDKNativeMessage, 0, len(raw))
	seen := map[string]bool{}
	for _, value := range raw {
		var row SDKNativeMessage
		if json.Unmarshal(value, &row) != nil || !validNativeMessage(row, h.session) || seen[row.UUID] {
			return nil, ErrSDKResumeReconstructionRequired
		}
		if row.Type != "user" && row.Type != "assistant" && !(row.Type == "system" && row.Subtype == "compact_boundary") {
			return nil, ErrSDKResumeReconstructionRequired
		}
		seen[row.UUID] = true
		rows = append(rows, row)
	}
	// Serialized siblings are not necessarily part of the selected conversation.
	// Reconstruct the completed-content chain before producing any wire history.
	leaf, cleared, err := sdkRemoteTranscriptSelection(sdkHydrationRows(h.record.Lines), rows)
	if err != nil {
		return nil, err
	}
	allRows := rows
	if cleared {
		rows = nil
	} else {
		rows, err = sdkCompletedRemoteChain(raw, rows, leaf, h.observeChain)
		if err != nil {
			return nil, err
		}
		if err := h.restoreProvenRemoteTerminal(rows); err != nil {
			return nil, err
		}
	}
	wire, err := committedNativeWireRowsWithClear(rows, cleared)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
	}{wire})
	fingerprints, known := sdkWireHistoryFingerprints(body, false)
	if cleared && len(wire) == 0 {
		fingerprints, known = []string{}, true
	}
	if !known {
		return nil, ErrSDKResumeReconstructionRequired
	}
	history := &sdkHistory{expectedText: fingerprints, expectedTextKnown: true}
	for _, row := range rows {
		structural := SDKHistoryMessage{Type: row.Type, UUID: row.UUID, Subtype: row.Subtype}
		if row.Type != "system" {
			var message struct {
				ID      string          `json:"id"`
				Model   string          `json:"model"`
				Content json.RawMessage `json:"content"`
				Usage   json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(row.Message, &message) != nil {
				return nil, ErrSDKSessionInvalid
			}
			structural.MessageID = message.ID
			structural.TokenEstimate = EstimateSDKContent(message.Content)
			if row.Type == "assistant" {
				var usage sdkResponseTokens
				usage.observeUsage(message.Usage)
				structural.Usage, structural.UsageKnown = usage.usage, usage.knownUsage()
				structural.UsageExcluded = sdkUsageContentExcluded(message.Content)
				structural.Synthetic = message.Model == "<synthetic>"
			}
		}
		history.messages = append(history.messages, structural)
	}
	t := h.tracker
	t.mu.Lock()
	scope := digest(h.account, h.session)
	if h.local == nil {
		if t.prompts == nil {
			t.prompts = make(map[string]*state)
		}
		t.loadSDKSessionLocked(scope)
		t.loadNativeContentLocked(scope, h.session)
	}
	n, persisted := t.nativeContent[scope], t.sessions[scope]
	if t.closed || t.nativeSessionActiveLocked(scope) {
		t.mu.Unlock()
		return nil, ErrSDKSessionActive
	}
	if n == nil || persisted == nil {
		t.mu.Unlock()
		return nil, ErrSDKSessionStale
	}
	if h.local == nil {
		if n.persisted.revision != "" || persisted.revision != "" || n.persisted.err != nil || persisted.err != nil || len(n.rows) != 0 || len(n.active) != 0 {
			t.mu.Unlock()
			return nil, ErrSDKSessionStale
		}
		for _, store := range []SDKSessionStore{t.store, t.nativeOptions.Store} {
			payload, revision, err := store.Load(scope)
			if err != nil || revision != "" || len(payload) != 0 {
				t.mu.Unlock()
				return nil, ErrSDKSessionStale
			}
		}
		if t.restoredHistories == nil {
			t.restoredHistories = make(map[string]*sdkRestoredHistory)
		}
		t.restoredHistories[scope] = &sdkRestoredHistory{history: history, ledger: &sdkAPILedger{}}
	} else {
		if n.persisted.revision != h.local.nativeRevision || persisted.revision != h.local.structuralRevision {
			t.mu.Unlock()
			return nil, ErrSDKSessionStale
		}
		if err := t.verifyNativeResumeCheckpointsLocked(scope, n, persisted); err != nil {
			t.mu.Unlock()
			return nil, err
		}
		if restored := t.restoredHistories[scope]; restored != nil {
			restored.history = history
		}
	}
	// Keep excluded projected parents needed by immutable later local rows.
	// A newer active suffix does not authorize deleting that provenance.
	projected := make(map[string]SDKNativeMessage, len(n.projected)+len(rows))
	for id, row := range n.projected {
		projected[id] = row
	}
	if cleared {
		// Retain provenance even for a remote-only cleared session. The active
		// projection is empty, not the immutable source or remote mirror.
		for _, row := range allRows {
			projected[row.UUID] = cloneNativeMessage(row)
		}
	}
	for _, row := range rows {
		projected[row.UUID] = cloneNativeMessage(row)
	}
	if len(projected) > maxSDKHistoryMessages {
		t.mu.Unlock()
		return nil, ErrSDKSessionInvalid
	}
	n.projected = projected
	n.active = nil
	n.clearedToEmpty = cleared
	for _, row := range rows {
		n.projected[row.UUID] = cloneNativeMessage(row)
		n.active = append(n.active, row.UUID)
	}
	n.dirty = true
	for _, state := range t.prompts {
		if state.scope == scope {
			state.sdk.history = history
		}
	}
	t.saveSDKSessionLocked(scope)
	if n.persisted.err != nil || persisted.err != nil {
		t.mu.Unlock()
		return nil, ErrSDKSessionUnavailable
	}
	t.mu.Unlock()
	return t.PrepareRemoteWireResume(h.account, h.session)
}

// A deferred JSONL row may already be immutable when message_delta supplies
// stop_reason/usage. RestoreNativeContent independently verifies that its
// protected checkpoint differs only in those native mutable response fields.
// Reuse that proof only for the exact selected local terminal, never a changed
// server response, an unfinished checkpoint, or another branch's completion.
func (h *SDKRemoteHydration) restoreProvenRemoteTerminal(rows []SDKNativeMessage) error {
	if len(rows) == 0 || rows[len(rows)-1].Type != "assistant" {
		return ErrSDKResumeReconstructionRequired
	}
	last := &rows[len(rows)-1]
	var terminal struct {
		StopReason json.RawMessage `json:"stop_reason"`
	}
	if json.Unmarshal(last.Message, &terminal) != nil {
		return ErrSDKResumeReconstructionRequired
	}
	var reason string
	_ = json.Unmarshal(terminal.StopReason, &reason)
	if reason == "end_turn" || reason == "stop_sequence" {
		return nil
	}
	missing := len(terminal.StopReason) == 0 || bytes.Equal(bytes.TrimSpace(terminal.StopReason), []byte("null"))
	if !missing || h.local == nil || len(h.local.ActiveMessages) == 0 {
		return ErrSDKResumeReconstructionRequired
	}
	owned := h.local.ActiveMessages[len(h.local.ActiveMessages)-1]
	if owned.UUID != last.UUID || !nativeResponseContentUnchanged(last.Message, owned.Message) {
		return ErrSDKResumeReconstructionRequired
	}
	if json.Unmarshal(owned.Message, &terminal) != nil || json.Unmarshal(terminal.StopReason, &reason) != nil || (reason != "end_turn" && reason != "stop_sequence") {
		return ErrSDKResumeReconstructionRequired
	}
	left, right := *last, owned
	left.Message, right.Message = nil, nil
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	if errA != nil || errB != nil || !bytes.Equal(a, b) {
		return ErrSDKResumeReconstructionRequired
	}
	last.Message = bytes.Clone(owned.Message)
	return nil
}
