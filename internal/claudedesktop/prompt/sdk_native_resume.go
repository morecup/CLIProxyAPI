package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

var ErrSDKSessionActive = errors.New("Claude Desktop SDK session still owns an active operation")

// SDKNativeResumeContent is verified local content, not an executable resume
// capability. Native deserialization, interruption reconciliation, hooks and
// exact record/Host admission must still run before another query can use it.
// Never serialize this type into management responses, logs or telemetry.
type SDKNativeResumeContent struct {
	structuralRevision  string
	nativeRevision      string
	Snapshot            SDKNativeContentSnapshot
	ActiveMessages      []SDKNativeMessage
	TranscriptRevision  string
	TranscriptATISLatch *string
	TranscriptBridge    *SDKBridgeTranscriptRecord
	HasCompletedTurns   bool
	ClearedToEmpty      bool
}

func (SDKNativeResumeContent) MarshalJSON() ([]byte, error) {
	return nil, ErrSDKSessionInvalid
}

// RestoreNativeContent loads the existing session explicitly, without inventing
// a Begin call, prompt UUID, input, API attempt or durable transcript append.
// It does not convert an interrupted query into a successful completion.
func (t *Tracker) RestoreNativeContent(accountID, sessionID string) (SDKNativeResumeContent, error) {
	if t == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(sessionID) == "" {
		return SDKNativeResumeContent{}, ErrSDKSessionInvalid
	}
	scope := digest(accountID, sessionID)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return SDKNativeResumeContent{}, ErrSDKSessionUnavailable
	}
	if t.nativeSessionActiveLocked(scope) {
		t.mu.Unlock()
		return SDKNativeResumeContent{}, ErrSDKSessionActive
	}
	writer := t.transcript
	t.mu.Unlock()
	if writer != nil {
		// This is an explicit local restoration flush. Never hold Tracker.mu
		// while waiting for serializers, which acquire that same mutex.
		writer.drain()
		writer.drainMu.Lock()
		defer writer.drainMu.Unlock()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	reader, ok := t.nativeOptions.TranscriptStore.(SDKTranscriptReader)
	if t.closed || t.store == nil || t.nativeOptions.Store == nil || !ok {
		return SDKNativeResumeContent{}, ErrSDKSessionUnavailable
	}
	if t.transcript != writer || t.nativeSessionActiveLocked(scope) {
		return SDKNativeResumeContent{}, ErrSDKSessionActive
	}
	if writer != nil {
		writer.mu.Lock()
		pending := len(writer.queues[scope]) != 0
		writer.mu.Unlock()
		if pending {
			// Another input completed during the flush. It needs a new explicit
			// attempt, not a false corruption diagnosis for its queued rows.
			return SDKNativeResumeContent{}, ErrSDKSessionActive
		}
	}
	if t.prompts == nil {
		t.prompts = make(map[string]*state)
	}
	t.loadSDKSessionLocked(scope)
	t.loadNativeContentLocked(scope, sessionID)
	persisted, n := t.sessions[scope], t.nativeContent[scope]
	if persisted == nil || n == nil || persisted.revision == "" || n.persisted.revision == "" {
		return SDKNativeResumeContent{}, ErrSDKSessionUnavailable
	}
	if persisted.err != nil || n.persisted.err != nil || n.issue != "" || t.nativeTranscriptErrorLocked(scope) {
		return SDKNativeResumeContent{}, ErrSDKSessionUnavailable
	}
	if err := t.verifyNativeResumeCheckpointsLocked(scope, n, persisted); err != nil {
		return SDKNativeResumeContent{}, err
	}
	// Reread the actual transcript rather than trusting a cached UUID index.
	// A later append/removal/corruption must not authorize an old snapshot.
	t.transcript.mu.Lock()
	transcriptState := t.transcript.scopes[scope]
	var revision string
	if transcriptState != nil {
		revision = transcriptState.revision
	}
	t.transcript.mu.Unlock()
	if revision == "" && (len(n.rows) != 0 || len(n.projected) == 0) {
		return SDKNativeResumeContent{}, ErrSDKSessionUnavailable
	}
	var metadata sdkResumeTranscriptMetadata
	verified, err := verifyNativeTranscript(reader, scope, n, revision, &metadata)
	if err != nil {
		n.unknown("sdk-transcript-content-checkpoint-mismatch")
		n.persisted.err, n.persisted.loadFailed = err, true
		return SDKNativeResumeContent{}, err
	}
	if err := t.verifyNativeResumeCheckpointsLocked(scope, n, persisted); err != nil {
		return SDKNativeResumeContent{}, err
	}
	result := SDKNativeResumeContent{structuralRevision: persisted.revision, nativeRevision: n.persisted.revision,
		Snapshot: t.nativeContentSnapshotLocked(scope), TranscriptRevision: revision, TranscriptATISLatch: verified,
		TranscriptBridge: cloneSDKBridgeTranscript(metadata.bridge), ClearedToEmpty: n.clearedToEmpty}
	for _, id := range n.active {
		row, exists := n.message(id)
		if !exists {
			return SDKNativeResumeContent{}, ErrSDKSessionInvalid
		}
		result.ActiveMessages = append(result.ActiveMessages, cloneNativeMessage(row))
	}
	for _, s := range t.prompts {
		if s.scope == scope && s.complete && !s.failed {
			result.HasCompletedTurns = true
		}
	}
	return result, nil
}

// A live owner cannot be replaced even if its network response has ended
// while a helper or accounting callback is still in flight.
func (t *Tracker) nativeSessionActiveLocked(scope string) bool {
	for _, s := range t.prompts {
		if s.scope != scope {
			continue
		}
		if s.helperOwners != 0 {
			return true
		}
		for _, call := range s.requests {
			if !call.settled || call.sdkAPICallbackPending {
				return true
			}
		}
	}
	return false
}

func (t *Tracker) verifyNativeResumeCheckpointsLocked(scope string, n *sdkNativeContent, structural *sdkSessionPersistence) error {
	for _, checkpoint := range []struct {
		store SDKSessionStore
		state *sdkSessionPersistence
	}{{t.store, structural}, {t.nativeOptions.Store, &n.persisted}} {
		payload, revision, err := checkpoint.store.Load(scope)
		if err != nil {
			err = ErrSDKSessionUnavailable
		} else if revision != checkpoint.state.revision || len(payload) == 0 || sdkCompactionHash(string(payload)) != checkpoint.state.digest {
			err = ErrSDKSessionStale
		}
		if err != nil {
			n.unknown("sdk-native-resume-checkpoint-changed")
			checkpoint.state.err, checkpoint.state.loadFailed = err, true
			return err
		}
	}
	return nil
}

// verifyNativeTranscript reconciles every immutable field and append position,
// not merely the set of UUIDs. Native block yields can be persisted before the
// final message_delta, and compaction can reset checkpoint usage; only the
// previously reviewed assistant delta fields may legitimately differ.
type sdkResumeTranscriptMetadata struct {
	bridge *SDKBridgeTranscriptRecord
}

func verifyNativeTranscript(reader SDKTranscriptReader, scope string, n *sdkNativeContent, revision string, metadata *sdkResumeTranscriptMetadata) (*string, error) {
	position := 0
	var latch *string
	var bridge *SDKBridgeTranscriptRecord
	index, err := reader.ReadTranscript(scope, func(lines []byte) error {
		if len(lines) == 0 || lines[len(lines)-1] != '\n' {
			return ErrSDKSessionInvalid
		}
		for _, line := range bytes.Split(lines[:len(lines)-1], []byte{'\n'}) {
			var header struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &header) != nil {
				return ErrSDKSessionInvalid
			}
			if header.Type == "bridge-session" {
				row, err := ParseSDKBridgeTranscriptRecord(line)
				if err != nil || row.SessionID != n.sessionID {
					return ErrSDKSessionInvalid
				}
				bridge = &row
				continue
			}
			if header.Type == "atis-latch" {
				var row struct {
					Type      string  `json:"type"`
					ATIS      *string `json:"atis"`
					SessionID string  `json:"sessionId"`
				}
				if decodeNativeTranscriptRow(line, &row) != nil || row.ATIS == nil || row.SessionID != n.sessionID {
					return ErrSDKSessionInvalid
				}
				latch = cloneNativeString(row.ATIS)
				continue
			}
			var row SDKNativeMessage
			if position >= len(n.rows) || decodeNativeTranscriptRow(line, &row) != nil || !validNativeMessage(row, n.sessionID) {
				return ErrSDKSessionInvalid
			}
			current := n.rows[position]
			position++
			message, currentMessage := row.Message, current.Message
			row.Message, current.Message = nil, nil
			attachment, currentAttachment := row.Attachment, current.Attachment
			row.Attachment, current.Attachment = nil, nil
			// Origin is raw JSON like the attachment: the checkpoint store and
			// the transcript writer may escape "<" differently for the same value.
			origin, currentOrigin := row.Origin, current.Origin
			row.Origin, current.Origin = nil, nil
			if !reflect.DeepEqual(row, current) || !equalNativeJSON(attachment, currentAttachment) || !equalNativeJSON(origin, currentOrigin) {
				return ErrSDKSessionInvalid
			}
			if row.Type == "assistant" {
				if !nativeResponseContentUnchanged(message, currentMessage) {
					return ErrSDKSessionInvalid
				}
			} else if !equalNativeJSON(message, currentMessage) {
				return ErrSDKSessionInvalid
			}
		}
		return nil
	})
	if err != nil {
		return nil, ErrSDKSessionInvalid
	}
	if index.Revision != revision {
		return nil, ErrSDKSessionStale
	}
	if position != len(n.rows) || len(index.UUIDs) != position {
		return nil, ErrSDKSessionInvalid
	}
	for i, id := range index.UUIDs {
		if id != n.rows[i].UUID {
			return nil, ErrSDKSessionInvalid
		}
	}
	if metadata != nil {
		metadata.bridge = cloneSDKBridgeTranscript(bridge)
	}
	return latch, nil
}

func decodeNativeTranscriptRow(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrSDKSessionInvalid
	}
	return nil
}

func equalNativeJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	decode := func(raw json.RawMessage, target *any) bool {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
	}
	return decode(a, &x) && decode(b, &y) && reflect.DeepEqual(x, y)
}
