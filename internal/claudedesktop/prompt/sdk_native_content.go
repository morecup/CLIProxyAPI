package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/transcript"
)

// SDKNativeContentOptions binds a live content owner to a separate protected
// checkpoint. TranscriptStore separately enables deferred JSONL appends;
// neither representation is the Renderer messageBuffer.
type SDKNativeContentOptions struct {
	Store                    SDKSessionStore
	TranscriptStore          SDKTranscriptStore
	RemoteTranscriptStore    SDKSessionStore
	Version, Entrypoint, Cwd string
	Metadata                 func(sessionID string) SDKNativeTranscriptMetadata
}

// SDKNativeTranscriptMetadata is supplied only by the owned runtime. A nil
// branch/slug means undefined, not an empty string or a request-supplied path.
// Providers return a snapshot and must not perform host operations here.
type SDKNativeTranscriptMetadata struct {
	SessionKind string
	GitBranch   *string
	Slug        *string
}

// SDKNativeMessage owns a native-shaped message, not a caller's metadata or a
// reconstructed host operation. Message retains the actual content, including
// thinking, tool arguments/results and media. Never log or export this type.
type SDKNativeMessage = transcript.Message

type SDKNativeContentSnapshot struct {
	Messages          []SDKNativeMessage
	ProjectedMessages []SDKNativeMessage
	ATISLatch         *string
	ActiveUUIDs       []string
	IncompleteReason  string
	PersistenceError  bool
}

type sdkNativeContent struct {
	sessionID      string
	rows           []SDKNativeMessage
	active         []string
	byUUID         map[string]int
	leaf           string
	leafTime       string
	issue          string
	dirty          bool
	bytes          int
	persisted      sdkSessionPersistence
	atisLatch      *string
	projected      map[string]SDKNativeMessage
	clearedToEmpty bool
}

type sdkNativeContentRecord struct {
	Version           int                `json:"version"`
	Scope             string             `json:"scope"`
	SessionID         string             `json:"session_id"`
	Messages          []SDKNativeMessage `json:"messages"`
	ActiveUUIDs       []string           `json:"active_uuids"`
	Leaf              string             `json:"leaf"`
	LeafTime          string             `json:"leaf_time"`
	Issue             string             `json:"issue"`
	WriteFailed       bool               `json:"write_failed"`
	ATISLatch         *string            `json:"atis_latch,omitempty"`
	ProjectedMessages []SDKNativeMessage `json:"projected_messages,omitempty"`
	ClearedToEmpty    bool               `json:"cleared_to_empty,omitempty"`
}

func nativeContentTimestamp(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

func cloneNativeMessage(row SDKNativeMessage) SDKNativeMessage {
	row.Message, row.Attachment = bytes.Clone(row.Message), bytes.Clone(row.Attachment)
	row.Origin = bytes.Clone(row.Origin)
	row.GitBranch, row.Slug = cloneNativeString(row.GitBranch), cloneNativeString(row.Slug)
	if row.ParentUUID != nil {
		parent := *row.ParentUUID
		row.ParentUUID = &parent
	}
	if row.LogicalParentUUID != nil {
		parent := *row.LogicalParentUUID
		row.LogicalParentUUID = &parent
	}
	return row
}

func cloneNativeString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (t *Tracker) nativeMetadataLocked(sessionID string) SDKNativeTranscriptMetadata {
	var metadata SDKNativeTranscriptMetadata
	if t.nativeOptions.Metadata != nil {
		metadata = t.nativeOptions.Metadata(sessionID)
	}
	switch metadata.SessionKind {
	case "bg", "daemon", "daemon-worker":
	default:
		metadata.SessionKind = ""
	}
	metadata.GitBranch, metadata.Slug = cloneNativeString(metadata.GitBranch), cloneNativeString(metadata.Slug)
	return metadata
}

func (n *sdkNativeContent) unknown(reason string) {
	if n != nil && n.issue == "" {
		n.issue, n.dirty = reason, true
	}
}

func (t *Tracker) loadNativeContentLocked(scope, sessionID string) {
	if t.nativeOptions.Store == nil || t.nativeContent[scope] != nil {
		return
	}
	if t.nativeContent == nil {
		t.nativeContent = make(map[string]*sdkNativeContent)
	}
	n := &sdkNativeContent{sessionID: sessionID, byUUID: make(map[string]int)}
	t.nativeContent[scope] = n
	payload, revision, err := t.nativeOptions.Store.Load(scope)
	n.persisted.revision = revision
	if err != nil || (len(payload) == 0 && revision != "") {
		n.persisted.err, n.persisted.loadFailed = ErrSDKSessionUnavailable, true
		return
	}
	if len(payload) != 0 {
		n.persisted.digest = sdkCompactionHash(string(payload))
		var record sdkNativeContentRecord
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		var extra any
		if len(payload) > MaxSDKSessionStateBytes || decoder.Decode(&record) != nil || decoder.Decode(&extra) != io.EOF ||
			record.Version != 1 || record.Scope != scope || record.SessionID != sessionID || len(record.Messages) > maxSDKHistoryMessages ||
			len(record.ActiveUUIDs) > maxSDKHistoryMessages || len(record.ProjectedMessages) > maxSDKHistoryMessages || len(record.Issue) > 256 {
			n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
			return
		}
		n.rows, n.active, n.leaf, n.leafTime, n.issue = record.Messages, record.ActiveUUIDs, record.Leaf, record.LeafTime, record.Issue
		n.clearedToEmpty = record.ClearedToEmpty
		if n.clearedToEmpty && len(n.active) != 0 {
			n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
			return
		}
		n.atisLatch = cloneNativeString(record.ATISLatch)
		n.projected = make(map[string]SDKNativeMessage, len(record.ProjectedMessages))
		for _, row := range record.ProjectedMessages {
			if !validNativeMessage(row, sessionID) || n.projected[row.UUID].UUID != "" {
				n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
				return
			}
			n.projected[row.UUID] = row
		}
		for index, row := range n.rows {
			if row.UserType != "external" {
				n.unknown("unobserved-sdk-native-metadata")
			}
			_, duplicate := n.byUUID[row.UUID]
			if !validNativeMessage(row, sessionID) || duplicate {
				n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
				return
			}
			n.byUUID[row.UUID] = index
			n.bytes += len(row.Message) + len(row.Attachment)
		}
		for index, row := range n.rows {
			for _, parent := range []*string{row.ParentUUID, row.LogicalParentUUID} {
				if parent != nil {
					if position, exists := n.byUUID[*parent]; (!exists || position >= index) && n.projected[*parent].UUID == "" {
						n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
						return
					}
				}
			}
			if row.SourceToolAssistantUUID != "" {
				position, exists := n.byUUID[row.SourceToolAssistantUUID]
				if ((!exists || position >= index || n.rows[position].Type != "assistant") && n.projected[row.SourceToolAssistantUUID].Type != "assistant") || row.ParentUUID == nil || *row.ParentUUID != row.SourceToolAssistantUUID {
					n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
					return
				}
			}
		}
		seen := make(map[string]bool)
		for _, id := range n.active {
			if _, exists := n.byUUID[id]; (!exists && n.projected[id].UUID == "") || seen[id] {
				n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
				return
			}
			seen[id] = true
		}
		if n.leaf != "" {
			index, exists := n.byUUID[n.leaf]
			if !exists || n.rows[index].Timestamp != n.leafTime {
				n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
				return
			}
		}
		if record.WriteFailed {
			n.persisted.err = ErrSDKSessionUnavailable
		}
	}
	// Independently protected files can disagree after a failed write/crash.
	// Keep both originals and expose the gap, never graft caller history onto it.
	matched := false
	for _, s := range t.prompts {
		if s.scope != scope {
			continue
		}
		matched = true
		if len(s.sdk.history.messages) != len(n.active) {
			n.unknown("unreconciled-sdk-native-content-checkpoint")
			break
		}
		for index, message := range s.sdk.history.messages {
			if n.active[index] != message.UUID {
				n.unknown("unreconciled-sdk-native-content-checkpoint")
				break
			}
		}
		break
	}
	if !matched && len(n.active) != 0 {
		restored := t.restoredHistories[scope]
		matched = restored != nil && len(restored.history.messages) == len(n.active)
		if matched {
			for index, row := range restored.history.messages {
				if row.UUID != n.active[index] {
					matched = false
					break
				}
			}
		}
		if !matched {
			n.unknown("unreconciled-sdk-native-content-checkpoint")
		}
	}
	if n.issue == "unreconciled-sdk-native-content-checkpoint" {
		n.persisted.err, n.persisted.loadFailed = ErrSDKSessionInvalid, true
	}
	if !n.persisted.loadFailed {
		t.loadNativeTranscriptLocked(scope, n)
	}
}

func validNativeMessage(row SDKNativeMessage, sessionID string) bool {
	id, err := uuid.Parse(row.UUID)
	if err != nil || id == uuid.Nil || row.SessionID != sessionID || len(row.Timestamp) != 24 {
		return false
	}
	if _, err = time.Parse("2006-01-02T15:04:05.000Z", row.Timestamp); err != nil {
		return false
	}
	if row.Type == "system" {
		return row.Subtype == "compact_boundary" && row.ParentUUID == nil
	}
	if row.Type == "attachment" {
		return json.Valid(row.Attachment)
	}
	var message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	return (row.Type == "user" || row.Type == "assistant") && json.Unmarshal(row.Message, &message) == nil &&
		message.Role == row.Type && json.Valid(message.Content)
}

func (t *Tracker) appendNativeContentLocked(scope string, row SDKNativeMessage, snapshots ...SDKNativeTranscriptMetadata) {
	n := t.nativeContent[scope]
	if n == nil || n.persisted.loadFailed {
		return
	}
	if _, exists := n.message(row.UUID); exists {
		return
	}
	if len(n.rows) >= maxSDKHistoryMessages || len(row.Message)+len(row.Attachment) > maxSDKCompactionViewBytes-n.bytes {
		n.unknown("sdk-native-content-retention-limit")
		return
	}
	row.SessionID, row.Version, row.Entrypoint, row.Cwd = n.sessionID, t.nativeOptions.Version, t.nativeOptions.Entrypoint, t.nativeOptions.Cwd
	var metadata SDKNativeTranscriptMetadata
	if len(snapshots) > 0 {
		metadata = snapshots[0]
	} else {
		metadata = t.nativeMetadataLocked(n.sessionID)
	}
	row.UserType, row.SessionKind, row.GitBranch, row.Slug = "external", metadata.SessionKind, metadata.GitBranch, metadata.Slug
	// aV derives the next chain parent from the last known message in the
	// active history. The writer's timestamp-monotonic leaf is not that parent:
	// after compaction a preserved message can be older than the new summary.
	parent := ""
	if len(n.active) != 0 {
		parent = n.active[len(n.active)-1]
	}
	if row.SourceToolAssistantUUID != "" {
		if _, exists := n.message(row.SourceToolAssistantUUID); exists {
			parent = row.SourceToolAssistantUUID
		} else {
			n.unknown("sdk-native-content-unowned-tool-parent")
		}
	}
	if row.Type == "system" && row.Subtype == "compact_boundary" {
		row.ParentUUID = nil
		if parent != "" {
			row.LogicalParentUUID = &parent
		}
	} else if parent != "" {
		row.ParentUUID = &parent
	}
	if !validNativeMessage(row, n.sessionID) {
		n.unknown("invalid-sdk-native-content")
		return
	}
	row = cloneNativeMessage(row)
	n.byUUID[row.UUID] = len(n.rows)
	n.rows = append(n.rows, row)
	n.active = append(n.active, row.UUID)
	n.clearedToEmpty = false
	n.bytes += len(row.Message) + len(row.Attachment)
	if n.leafTime == "" || row.Timestamp >= n.leafTime {
		n.leaf, n.leafTime = row.UUID, row.Timestamp
	}
	n.dirty = true
	t.enqueueNativeTranscriptLocked(scope, n, row.UUID)
}

func (t *Tracker) saveNativeContentLocked(scope string) {
	n := t.nativeContent[scope]
	if n == nil || n.persisted.loadFailed || !n.dirty {
		return
	}
	payload, err := json.Marshal(sdkNativeContentRecord{Version: 1, Scope: scope, SessionID: n.sessionID, Messages: n.rows,
		ActiveUUIDs: n.active, Leaf: n.leaf, LeafTime: n.leafTime, Issue: n.issue, WriteFailed: n.persisted.err != nil, ATISLatch: n.atisLatch, ProjectedMessages: n.projectedMessages(), ClearedToEmpty: n.clearedToEmpty})
	if err != nil || len(payload) > MaxSDKSessionStateBytes {
		n.persisted.err = ErrSDKSessionInvalid
		return
	}
	revision, err := t.nativeOptions.Store.Save(scope, n.persisted.revision, payload)
	if err != nil {
		n.persisted.err = err
		if errors.Is(err, ErrSDKSessionStale) || errors.Is(err, ErrSDKSessionInvalid) {
			n.persisted.loadFailed = true
		}
		return
	}
	n.persisted.revision, n.persisted.digest, n.dirty = revision, sdkCompactionHash(string(payload)), false
}

func (t *Tracker) nativeContentErrorLocked(scope string) bool {
	n := t.nativeContent[scope]
	return (n != nil && (n.persisted.err != nil || n.issue != "")) || t.nativeTranscriptErrorLocked(scope)
}

func (t *Tracker) NativeContent(accountID, sessionID string) SDKNativeContentSnapshot {
	if t == nil {
		return SDKNativeContentSnapshot{IncompleteReason: "unobserved-sdk-native-content"}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nativeContentSnapshotLocked(digest(accountID, sessionID))
}

func (t *Tracker) nativeContentSnapshotLocked(scope string) SDKNativeContentSnapshot {
	n := t.nativeContent[scope]
	if n == nil {
		return SDKNativeContentSnapshot{IncompleteReason: "unobserved-sdk-native-content"}
	}
	result := SDKNativeContentSnapshot{IncompleteReason: n.issue, PersistenceError: n.persisted.err != nil || t.nativeTranscriptErrorLocked(scope), ActiveUUIDs: append([]string(nil), n.active...), ATISLatch: cloneNativeString(n.atisLatch)}
	for _, row := range n.rows {
		result.Messages = append(result.Messages, cloneNativeMessage(row))
	}
	result.ProjectedMessages = n.projectedMessages()
	return result
}

// BindNativeATISLatch observes the native query-boundary latch, not its effective
// outgoing header. Initial input was already inserted; the next actual chain
// insertion writes metadata, including a defined empty latch. No row is forged
// merely because an API request has been prepared.
func (r *Request) BindNativeATISLatch(latch *string) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.finished || r.attempt != r.call.attempt {
		return
	}
	n := r.tracker.nativeContent[r.call.state.scope]
	if n == nil || n.persisted.loadFailed || (n.atisLatch == nil && latch == nil) ||
		(n.atisLatch != nil && latch != nil && *n.atisLatch == *latch) {
		return
	}
	n.atisLatch, n.dirty = cloneNativeString(latch), true
	r.tracker.saveNativeContentLocked(r.call.state.scope)
}

func (t *Tracker) observeNativeInputLocked(c *call, body []byte, notification *string, meta *SDKMetaInput) {
	n := t.nativeContent[c.state.scope]
	if n == nil || n.persisted.loadFailed {
		return
	}
	var root struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
	}
	if len(body) > maxSDKCompactionViewBytes || json.Unmarshal(body, &root) != nil || len(root.Messages) == 0 {
		n.unknown("unobserved-sdk-native-user-content")
		return
	}
	last := len(root.Messages) - 1
	var contents []json.RawMessage
	if len(c.resultIDs) == 0 {
		if root.Messages[last].Role == "user" {
			contents = []json.RawMessage{root.Messages[last].Content}
		}
	} else {
		start := last
		for start > 0 && root.Messages[start-1].Role == "user" {
			start--
		}
		for _, row := range root.Messages[start:] {
			blocks, ok := sdkWireToolResults(row.Content)
			if !ok {
				n.unknown("unobserved-sdk-native-tool-result")
				return
			}
			for _, block := range blocks {
				contents = append(contents, append(append(json.RawMessage{'['}, block...), ']'))
			}
		}
	}
	if len(contents) != len(c.sdkUserHistoryOffsets) {
		n.unknown("unobserved-sdk-native-user-content")
		return
	}
	metadata := t.nativeMetadataLocked(n.sessionID)
	for i, offset := range c.sdkUserHistoryOffsets {
		if offset < 0 || offset >= len(c.state.sdk.history.messages) {
			n.unknown("unobserved-sdk-native-user-identity")
			return
		}
		message := c.state.sdk.history.messages[offset]
		row := SDKNativeMessage{Type: "user", UUID: message.UUID, Timestamp: nativeContentTimestamp(c.startedAt), PromptID: c.identity.PromptID}
		if notification != nil && len(c.resultIDs) == 0 && len(contents) == 1 {
			row.Origin = json.RawMessage(`{"kind":"task-notification"}`)
			contents[i], _ = json.Marshal(*notification)
		} else if meta != nil && len(c.resultIDs) == 0 && len(contents) == 1 && json.Valid(meta.Origin) {
			// Native Knd projects the queued value in place before the row is
			// persisted, so the meta row keeps the wire content it was sent with.
			row.IsMeta, row.Origin = true, bytes.Clone(meta.Origin)
		}
		row.Message, _ = json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{"user", contents[i]})
		if i < len(c.resultIDs) {
			row.SourceToolAssistantUUID = n.toolAssistant(c.resultIDs[i])
			if row.SourceToolAssistantUUID == "" {
				n.unknown("unobserved-sdk-native-tool-parent")
			}
		}
		t.appendNativeContentLocked(c.state.scope, row, metadata)
	}
}

func (n *sdkNativeContent) toolAssistant(toolID string) string {
	for index := len(n.active) - 1; index >= 0; index-- {
		row, _ := n.message(n.active[index])
		if row.Type != "assistant" {
			continue
		}
		var message struct {
			Content []struct{ Type, ID string }
		}
		if json.Unmarshal(row.Message, &message) != nil {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "tool_use" && digest(strings.TrimSpace(block.ID)) == toolID {
				return row.UUID
			}
		}
	}
	return ""
}

// ObserveNativeContent follows the same per-attempt UUIDs as ObserveSDKHistory.
// Later message_delta fields update live objects, not native JSONL disk rows.
func (r *Request) ObserveNativeContent(rows []SDKNativeMessage, issue string) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.finished || r.attempt != r.call.attempt {
		return
	}
	n := r.tracker.nativeContent[r.call.state.scope]
	if n == nil || n.persisted.loadFailed {
		return
	}
	if issue != "" {
		n.unknown(issue)
	}
	metadata := r.tracker.nativeMetadataLocked(n.sessionID)
	for i, row := range rows {
		if i >= len(r.sdkHistoryOffsets) || r.sdkHistoryOffsets[i] >= len(r.call.state.sdk.history.messages) ||
			r.call.state.sdk.history.messages[r.sdkHistoryOffsets[i]].UUID != row.UUID {
			n.unknown("unowned-sdk-native-response")
			return
		}
		structural := r.call.state.sdk.history.messages[r.sdkHistoryOffsets[i]]
		var observed struct{ ID, Role string }
		if row.Type != "assistant" || json.Unmarshal(row.Message, &observed) != nil || observed.ID != structural.MessageID || observed.Role != "assistant" {
			n.unknown("unowned-sdk-native-response")
			return
		}
		if index, exists := n.byUUID[row.UUID]; exists {
			previous := &n.rows[index]
			if previous.Timestamp != row.Timestamp || previous.Type != row.Type {
				n.unknown("changed-sdk-native-response-identity")
				return
			}
			if !bytes.Equal(previous.Message, row.Message) {
				if !nativeResponseContentUnchanged(previous.Message, row.Message) {
					n.unknown("changed-sdk-native-response-content")
					return
				}
				delta := len(row.Message) - len(previous.Message)
				if delta > maxSDKCompactionViewBytes-n.bytes {
					n.unknown("sdk-native-content-retention-limit")
					return
				}
				previous.Message, n.bytes, n.dirty = bytes.Clone(row.Message), n.bytes+delta, true
			}
		} else {
			r.tracker.appendNativeContentLocked(r.call.state.scope, row, metadata)
		}
	}
}

func nativeResponseContentUnchanged(before, after json.RawMessage) bool {
	var a, b map[string]json.RawMessage
	if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil {
		return false
	}
	for _, key := range []string{"usage", "stop_reason", "stop_details"} {
		delete(a, key)
		delete(b, key)
	}
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

func (t *Tracker) commitNativeCompactionLocked(scope string, application *SDKCompactionApplication, promptID string) {
	n := t.nativeContent[scope]
	if n == nil || n.persisted.loadFailed {
		return
	}
	staged := make(map[string]SDKNativeMessage, len(application.nativeRows))
	for _, row := range application.nativeRows {
		if row.Type == "user" {
			row.PromptID = promptID
		}
		staged[row.UUID] = row
	}
	var active []string
	metadata := t.nativeMetadataLocked(n.sessionID)
	for _, structural := range application.messages {
		if row, exists := staged[structural.UUID]; exists {
			t.appendNativeContentLocked(scope, row, metadata)
		}
		row, exists := n.message(structural.UUID)
		if !exists {
			n.unknown("unobserved-sdk-native-compaction-content")
			continue
		}
		if structural.Type == "assistant" {
			var message map[string]json.RawMessage
			if json.Unmarshal(row.Message, &message) == nil {
				var usage map[string]json.RawMessage
				_ = json.Unmarshal(message["usage"], &usage)
				if usage == nil {
					usage = make(map[string]json.RawMessage)
				}
				for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
					usage[key] = json.RawMessage(`0`)
				}
				message["usage"], _ = json.Marshal(usage)
				raw, err := json.Marshal(message)
				if err == nil {
					if _, projected := n.projected[structural.UUID]; projected {
						row.Message = raw
						n.projected[structural.UUID] = row
					} else {
						index := n.byUUID[structural.UUID]
						n.bytes += len(raw) - len(n.rows[index].Message)
						n.rows[index].Message = raw
					}
				}
			}
		}
		active = append(active, structural.UUID)
	}
	n.active, n.dirty = active, true
}
