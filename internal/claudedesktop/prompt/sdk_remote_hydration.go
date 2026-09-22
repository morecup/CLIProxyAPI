package prompt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// SDKHydrationEvent is supplied by the epoch-owned internal-events reader,
// never by inbound request metadata. The raw payload stays private.
type SDKHydrationEvent struct {
	EventID        *string
	EventIDPresent bool
	AgentID        string
	Payload        json.RawMessage
}

type SDKHydrationRead struct {
	Events         []SDKHydrationEvent
	AnchorFallback string
}

type SDKHydrationReaders struct {
	Foreground   func(context.Context, string) (*SDKHydrationRead, error)
	Subagents    func(context.Context) (*SDKHydrationRead, error)
	ObserveChain func(SDKTranscriptChainEvent)
	// These are owned feature decisions, not guesses based on an epoch number.
	DeltaEnabled, LazySubagents, SkipSubagentsOnDelta bool
}

type SDKHydrationStatus struct {
	Applied, Delta, ReadFailed, SubagentReadFailed bool
	Fallback, SubagentMode                         string
}

// The mirror is separate from both immutable originals and the live normalized
// projection. The byte count and digest bind the complete local JSONL prefix,
// including bridge/ATIS records that do not have message UUIDs.
type sdkRemoteTranscriptRecord struct {
	Version     int     `json:"version"`
	SessionID   string  `json:"session_id"`
	RemoteID    string  `json:"remote_id"`
	Lines       []byte  `json:"lines"`
	Anchor      *string `json:"anchor,omitempty"`
	UpdatedAt   string  `json:"updated_at,omitempty"`
	LocalRows   int     `json:"local_rows"`
	LocalBytes  int     `json:"local_bytes"`
	LocalPrefix string  `json:"local_prefix,omitempty"`
}

type SDKRemoteHydration struct {
	tracker                         *Tracker
	account, session, remote, scope string
	store                           SDKSessionStore
	revision                        string
	record                          sdkRemoteTranscriptRecord
	local                           *SDKNativeResumeContent
	status                          SDKHydrationStatus
	observeChain                    func(SDKTranscriptChainEvent)
}

func (*SDKRemoteHydration) MarshalJSON() ([]byte, error) { return nil, ErrSDKSessionInvalid }
func (h *SDKRemoteHydration) Status() SDKHydrationStatus { return h.status }

// PrepareRemoteHydration joins the local writer and independently verifies
// existing content before lending a snapshot. Network work holds no tracker lock.
func (t *Tracker) PrepareRemoteHydration(account, session, remote string) (*SDKRemoteHydration, error) {
	if t == nil || account == "" || session == "" || remote == "" || t.nativeOptions.RemoteTranscriptStore == nil || t.nativeOptions.Store == nil {
		return nil, ErrSDKSessionUnavailable
	}
	h := &SDKRemoteHydration{tracker: t, account: account, session: session, remote: remote,
		scope: digest(account, session, "remote", remote), store: t.nativeOptions.RemoteTranscriptStore}
	payload, revision, err := h.store.Load(h.scope)
	if err != nil {
		return nil, err
	}
	h.revision = revision
	h.record = sdkRemoteTranscriptRecord{Version: 1, SessionID: session, RemoteID: remote}
	if len(payload) != 0 {
		if json.Unmarshal(payload, &h.record) != nil || h.record.Version != 1 || h.record.SessionID != session || h.record.RemoteID != remote || h.record.LocalRows < 0 || h.record.LocalBytes < 0 {
			return nil, ErrSDKSessionInvalid
		}
	}
	t.mu.Lock()
	if t.closed || t.nativeSessionActiveLocked(digest(account, session)) {
		t.mu.Unlock()
		return nil, ErrSDKSessionActive
	}
	_, storedRevision, err := t.nativeOptions.Store.Load(digest(account, session))
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if storedRevision != "" {
		local, err := t.RestoreNativeContent(account, session)
		if err != nil {
			return nil, err
		}
		h.local = &local
		if h.record.LocalRows > len(local.Snapshot.Messages) {
			return nil, ErrSDKSessionStale
		}
		reader, ok := t.nativeOptions.TranscriptStore.(SDKTranscriptReader)
		if !ok {
			return nil, ErrSDKSessionUnavailable
		}
		var original []byte
		index, err := reader.ReadTranscript(digest(account, session), func(lines []byte) error {
			if len(original)+len(lines) > MaxSDKSessionStateBytes {
				return ErrSDKSessionInvalid
			}
			original = append(original, lines...)
			return nil
		})
		if err != nil || index.Revision != local.TranscriptRevision {
			return nil, ErrSDKSessionStale
		}
		if h.record.LocalBytes > len(original) || h.record.LocalBytes > 0 && sdkCompactionHash(string(original[:h.record.LocalBytes])) != h.record.LocalPrefix {
			return nil, ErrSDKSessionStale
		}
		h.record.Lines = append(h.record.Lines, original[h.record.LocalBytes:]...)
		h.record.LocalBytes = len(original)
		h.record.LocalPrefix = sdkCompactionHash(string(original))
		h.record.LocalRows = len(local.Snapshot.Messages)
	}
	if len(h.record.Lines) > MaxSDKSessionStateBytes {
		return nil, ErrSDKSessionInvalid
	}
	return h, nil
}

func sdkHydrationJSONLine(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return nil, ErrSDKSessionInvalid
	}
	return buffer.Bytes(), nil
}

func sdkHydrationRows(lines []byte) []json.RawMessage {
	var rows []json.RawMessage
	for _, line := range bytes.Split(lines, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 && json.Valid(line) {
			rows = append(rows, bytes.Clone(line))
		}
	}
	return rows
}

func sdkHydrationFields(raw json.RawMessage) (kind, id string, hasID bool) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return
	}
	_ = json.Unmarshal(value["type"], &kind)
	hasID = json.Unmarshal(value["uuid"], &id) == nil && !bytes.Equal(bytes.TrimSpace(value["uuid"]), []byte("null"))
	return
}

func sdkHydrationContent(lines []byte) bool {
	for _, row := range sdkHydrationRows(lines) {
		kind, _, _ := sdkHydrationFields(row)
		if kind == "user" || kind == "assistant" {
			return true
		}
	}
	return false
}

func sdkHydrationTip(events []SDKHydrationEvent) *string {
	for i := len(events) - 1; i >= 0; i-- {
		kind, id, ok := sdkHydrationFields(events[i].Payload)
		if ok && (kind == "user" || kind == "assistant" || kind == "attachment" || kind == "system") {
			if events[i].EventID != nil {
				id = *events[i].EventID
			}
			return &id
		}
	}
	return nil
}

func sdkHydrationEncode(events []SDKHydrationEvent, skip map[string]bool) ([]byte, error) {
	var result []byte
	for _, event := range events {
		_, id, known := sdkHydrationFields(event.Payload)
		if known && skip[id] {
			continue
		}
		line, err := sdkHydrationJSONLine(event.Payload)
		if err != nil {
			return nil, err
		}
		result = append(result, line...)
		if len(result) > MaxSDKSessionStateBytes {
			return nil, ErrSDKSessionInvalid
		}
	}
	return result, nil
}

// Run follows nti's foreground replacement/delta guard. A failed later page is
// represented by the reader as a failed whole read, never a usable prefix.
// Ordinary errors retain the previous mirror. Typed ownership conflicts are
// passed through unchanged for the query owner to retire without remote writes.
func (h *SDKRemoteHydration) Run(ctx context.Context, readers SDKHydrationReaders) error {
	if h == nil || readers.Foreground == nil {
		return ErrSDKSessionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.observeChain = readers.ObserveChain
	hadLocalTail := h.local != nil || h.revision != "" || len(h.record.Lines) > 0
	tail := h.record.Lines
	if len(tail) > 65536 {
		tail = tail[len(tail)-65536:]
	}
	ids := map[string]bool{}
	for _, row := range sdkHydrationRows(tail) {
		_, id, ok := sdkHydrationFields(row)
		if ok {
			ids[id] = true
		}
	}
	anchor := ""
	if !readers.DeltaEnabled {
		h.status.Fallback = "client-gated"
	} else if h.record.Anchor == nil {
		h.status.Fallback = "no-sidecar"
	} else if !ids[*h.record.Anchor] {
		h.status.Fallback = "tip-not-in-tail"
	} else {
		anchor = *h.record.Anchor
	}
	read, err := readers.Foreground(ctx, anchor)
	if err != nil || read == nil {
		h.status.ReadFailed = true
		return err
	}
	events := read.Events
	anchorReturned := false
	for _, event := range events {
		_, id, _ := sdkHydrationFields(event.Payload)
		if event.EventIDPresent && event.EventID == nil {
			continue
		}
		if event.EventID != nil {
			id = *event.EventID
		}
		if id == anchor {
			anchorReturned = true
		}
	}
	coherent := ids[anchor] && len(tail) > 0 && tail[len(tail)-1] == '\n'
	delta := anchor != "" && read.AnchorFallback == "" && !anchorReturned && coherent
	if anchor != "" && read.AnchorFallback == "" && !anchorReturned && !coherent {
		h.status.Fallback = "tail-incoherent"
		full, err := readers.Foreground(ctx, "")
		if err != nil || full == nil {
			h.status.ReadFailed = true
			return err
		}
		events = full.Events
	}
	next := h.record
	if delta {
		added, err := sdkHydrationEncode(events, ids)
		if err != nil {
			return err
		}
		next.Lines = append(bytes.Clone(next.Lines), added...)
		h.status.Applied, h.status.Delta = true, true
	} else {
		if anchor != "" && h.status.Fallback != "tail-incoherent" {
			h.status.Fallback = "anchor-in-response"
			if read.AnchorFallback == "rejected" {
				h.status.Fallback = "anchor-rejected"
			}
			if read.AnchorFallback == "not-found" {
				h.status.Fallback = "anchor-not-found"
			}
		}
		lines, err := sdkHydrationEncode(events, nil)
		if err != nil {
			return err
		}
		if sdkHydrationContent(lines) || !sdkHydrationContent(next.Lines) {
			next.Lines = lines
			h.status.Applied = true
		}
	}
	if h.status.Applied {
		if tip := sdkHydrationTip(events); tip != nil {
			next.Anchor = tip
			next.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.verifyLocal(); err != nil {
			return err
		}
		payload, err := json.Marshal(next)
		if err != nil {
			return ErrSDKSessionInvalid
		}
		revision, err := h.store.Save(h.scope, h.revision, payload)
		if err != nil {
			return err
		}
		h.record, h.revision = next, revision
	}
	h.status.SubagentMode = "eager"
	if readers.LazySubagents && (delta || !hadLocalTail) {
		h.status.SubagentMode = "lazy"
	} else if readers.SkipSubagentsOnDelta && delta {
		h.status.SubagentMode = "skipped_delta"
	}
	if readers.Subagents != nil && h.status.SubagentMode == "eager" {
		read, err := readers.Subagents(ctx)
		if err != nil || read == nil {
			h.status.SubagentReadFailed = true
			return err
		}
		groups := map[string][]SDKHydrationEvent{}
		var order []string
		for _, event := range read.Events {
			if event.AgentID == "" {
				continue
			}
			if _, ok := groups[event.AgentID]; !ok {
				order = append(order, event.AgentID)
			}
			groups[event.AgentID] = append(groups[event.AgentID], event)
		}
		for _, id := range order {
			if !sdkHydrationSafeAgent(id) {
				continue
			}
			lines, err := sdkHydrationEncode(groups[id], nil)
			if err != nil {
				h.status.SubagentReadFailed = true
				continue
			}
			if !sdkHydrationContent(lines) {
				continue
			}
			scope := digest(h.scope, "agent", id)
			_, revision, err := h.store.Load(scope)
			if err == nil {
				payload, _ := json.Marshal(sdkRemoteTranscriptRecord{Version: 1, SessionID: h.session, RemoteID: h.remote, Lines: lines})
				_, err = h.store.Save(scope, revision, payload)
			}
			if err != nil {
				h.status.SubagentReadFailed = true
			}
		}
	}
	return ctx.Err()
}

func sdkHydrationSafeAgent(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c != '_' && c != '-' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (h *SDKRemoteHydration) verifyLocal() error {
	if h.local == nil {
		return nil
	}
	current, err := h.tracker.RestoreNativeContent(h.account, h.session)
	if err != nil {
		return err
	}
	if current.TranscriptRevision != h.local.TranscriptRevision || current.nativeRevision != h.local.nativeRevision || current.structuralRevision != h.local.structuralRevision {
		return ErrSDKSessionStale
	}
	return nil
}

// NativeRows returns a private copy after revision revalidation. Storage
// metadata and classifier rows are not messages in the live query projection.
func (h *SDKRemoteHydration) NativeRows() ([]json.RawMessage, error) {
	if err := h.verifyLocal(); err != nil {
		return nil, err
	}
	payload, revision, err := h.store.Load(h.scope)
	if err != nil || revision != h.revision {
		return nil, errors.Join(err, ErrSDKSessionStale)
	}
	if revision != "" {
		var record sdkRemoteTranscriptRecord
		if json.Unmarshal(payload, &record) != nil || !bytes.Equal(record.Lines, h.record.Lines) {
			return nil, ErrSDKSessionStale
		}
	}
	var rows []json.RawMessage
	for _, raw := range sdkHydrationRows(h.record.Lines) {
		kind, _, ok := sdkHydrationFields(raw)
		if ok && strings.Contains("|user|assistant|attachment|system|", "|"+kind+"|") && kind != "" {
			rows = append(rows, raw)
		}
	}
	return rows, nil
}
