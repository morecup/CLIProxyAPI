package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// This is a request-local copy bound, not a native context-window limit.
const maxSDKCompactionViewBytes = 16 * 1024 * 1024

var ErrSDKCompactionContentUnknown = errors.New("unresolved native compaction content")
var ErrSDKCompactionViewStale = errors.New("native compaction input is no longer current")

// SDKCompactionView resolves owned UUIDs against the very same final request
// observed by the active query. Only this short-lived view holds content; it is
// never attached to Tracker, serialized, logged, or shared between accounts.
// The view has one synchronous owner. Observable content ownership does not
// reconstruct tool execution, the caller's pre-wire segmentation or hooks.
type SDKCompactionView struct {
	owner     *Request
	history   SDKHistorySnapshot
	rows      []json.RawMessage
	rowByUUID map[string]int
	owners    []int
}

func (r *Request) CompactionView(body []byte) (*SDKCompactionView, error) {
	if r == nil || len(body) > maxSDKCompactionViewBytes {
		return nil, ErrSDKCompactionContentUnknown
	}
	fingerprints, known := sdkWireRequestFingerprints(body)
	if !known {
		return nil, ErrSDKCompactionContentUnknown
	}
	var wire struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return nil, ErrSDKCompactionContentUnknown
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if !r.compactionViewActive() {
		return nil, ErrSDKCompactionViewStale
	}
	sum := sha256.Sum256(body)
	if !r.call.sdkWireInputKnown || len(fingerprints) != len(r.call.sdkWireInputs) || hex.EncodeToString(sum[:]) != r.call.sdkWireRequestDigest {
		return nil, ErrSDKCompactionContentUnknown
	}
	for index, fingerprint := range fingerprints {
		if fingerprint != r.call.sdkWireInputs[index] {
			return nil, ErrSDKCompactionContentUnknown
		}
	}
	history := r.call.state.sdk.history.snapshot()
	if !history.OwnedMessagesKnown || len(history.Messages) == 0 {
		return nil, ErrSDKCompactionContentUnknown
	}
	view := &SDKCompactionView{owner: r, history: history, rows: wire.Messages,
		rowByUUID: make(map[string]int, len(history.Messages)), owners: make([]int, len(wire.Messages))}
	nativeIndex := 0
	if first := history.Messages[0]; first.Type == "system" && first.Subtype == "compact_boundary" {
		view.rowByUUID[first.UUID] = -1
		nativeIndex++
	}
	for rowIndex, raw := range wire.Messages {
		var row struct {
			Role    string
			Content json.RawMessage
		}
		if json.Unmarshal(raw, &row) != nil || nativeIndex >= len(history.Messages) {
			return nil, ErrSDKCompactionContentUnknown
		}
		first := history.Messages[nativeIndex]
		if first.Type != row.Role || first.IsVirtual || first.ResumedFromIncompleteThinking || first.Synthetic {
			return nil, ErrSDKCompactionContentUnknown
		}
		end := nativeIndex + 1
		if row.Role == "user" && first.WireToolResultID != "" {
			blocks, pureResults := sdkWireToolResults(row.Content)
			if !pureResults || nativeIndex+len(blocks) > len(history.Messages) {
				return nil, ErrSDKCompactionContentUnknown
			}
			end = nativeIndex + len(blocks)
			for index, block := range blocks {
				var result struct {
					ID string `json:"tool_use_id"`
				}
				message := history.Messages[nativeIndex+index]
				if json.Unmarshal(block, &result) != nil || strings.TrimSpace(result.ID) == "" || message.Type != "user" || message.WireToolResultID != digest(strings.TrimSpace(result.ID)) {
					return nil, ErrSDKCompactionContentUnknown
				}
			}
		}
		if row.Role == "assistant" {
			if first.MessageID == "" {
				return nil, ErrSDKCompactionContentUnknown
			}
			// Every closed stream block has a UUID, but all blocks of the same
			// response normalize to one HTTP row and share one native group.
			for end < len(history.Messages) && history.Messages[end].Type == "assistant" && history.Messages[end].MessageID == first.MessageID {
				end++
			}
		}
		// Native attachments are not extra HTTP rows or user submissions.
		// A previously adopted attachment retains its exact owned collapse
		// link, including explicitly reviewed no-wire attachment variants.
		for end < len(history.Messages) && history.Messages[end].Type == "attachment" {
			attachment := history.Messages[end]
			ownedParent := false
			for _, parent := range history.Messages[nativeIndex:end] {
				ownedParent = ownedParent || (parent.Type == "user" && parent.UUID == attachment.WireParentUUID)
			}
			if !attachment.TokenEstimate.Known || attachment.Subtype == "" ||
				(!attachment.NoWireContent && (row.Role != "user" || !ownedParent)) ||
				(attachment.NoWireContent && (attachment.WireParentUUID != "" || attachment.TokenEstimate.Tokens != 0)) {
				return nil, ErrSDKCompactionContentUnknown
			}
			end++
		}
		for _, message := range history.Messages[nativeIndex:end] {
			if message.UUID == "" || message.IsVirtual || message.ResumedFromIncompleteThinking || message.Synthetic {
				return nil, ErrSDKCompactionContentUnknown
			}
			if _, duplicate := view.rowByUUID[message.UUID]; duplicate {
				return nil, ErrSDKCompactionContentUnknown
			}
			if message.Type == "attachment" && message.NoWireContent {
				view.rowByUUID[message.UUID] = -1
			} else {
				view.rowByUUID[message.UUID] = rowIndex
				view.owners[rowIndex]++
			}
		}
		nativeIndex = end
	}
	if nativeIndex != len(history.Messages) {
		return nil, ErrSDKCompactionContentUnknown
	}
	return view, nil
}

// Called only while holding the tracker lock. A partial response, replaced
// attempt or concurrent continuation cannot become a compaction input owner.
func (r *Request) compactionViewActive() bool {
	return r.tracker.prompts[r.call.state.key] == r.call.state && !r.finished && r.attempt == r.call.attempt && !r.call.settled && r.call.sdkQueryObserved &&
		r.call.state.active == r.call && !r.call.state.complete && !r.call.state.failed && r.sdkHistoryObserved == 0
}

func (v *SDKCompactionView) Current() bool {
	if v == nil || v.owner == nil {
		return false
	}
	r := v.owner
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	return v.currentLocked()
}

// ClaimReactiveFailure is called only after observing a prompt-too-long API
// failure. The failed physical query remains current while its summary runs;
// it must not be counted as an API success if the application later adopts it.
func (v *SDKCompactionView) ClaimReactiveFailure() bool {
	if v == nil || v.owner == nil {
		return false
	}
	r := v.owner
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	if !v.currentLocked() || r.call.state.sdk.reactiveCompactionAttempted {
		return false
	}
	r.call.state.sdk.reactiveCompactionAttempted = true
	r.call.reactiveCompactionFailure = true
	return true
}

// ReactiveFailureOwnedBy ties observers to the actually claimed main failure.
// A helper completion or a foreign/stale request cannot create this lifecycle.
func (v *SDKCompactionView) ReactiveFailureOwnedBy(owner *Request) bool {
	if v == nil || owner == nil || v.owner != owner {
		return false
	}
	owner.tracker.mu.Lock()
	defer owner.tracker.mu.Unlock()
	return v.currentLocked() && owner.call.reactiveCompactionFailure && owner.call.state.sdk.reactiveCompactionAttempted
}

func (v *SDKCompactionView) currentLocked() bool {
	r := v.owner
	if !r.compactionViewActive() {
		return false
	}
	history := r.call.state.sdk.history
	if history == nil || history.incompleteReason != "" || history.pendingReconciliations != 0 || len(history.messages) != len(v.history.Messages) {
		return false
	}
	for index, message := range history.messages {
		if message != v.history.Messages[index] {
			return false
		}
	}
	return true
}

func (v *SDKCompactionView) History() SDKHistorySnapshot {
	if v == nil {
		return SDKHistorySnapshot{IncompleteReason: "unresolved-sdk-compaction-content"}
	}
	history := v.history
	history.Messages = append([]SDKHistoryMessage(nil), history.Messages...)
	history.Groups = GroupSDKHistory(history.Messages)
	return history
}

// MatchesScope checks the same account/profile/egress and session tuple used
// when the request was admitted. A caller-supplied UUID alone cannot rebind it.
func (v *SDKCompactionView) MatchesScope(accountID, sessionID string) bool {
	return v.Current() && v.owner.call.state.scope == digest(accountID, sessionID)
}

func (v *SDKCompactionView) MatchesRequestBody(body []byte) bool {
	sum := sha256.Sum256(body)
	return v.Current() && hex.EncodeToString(sum[:]) == v.owner.call.sdkWireRequestDigest
}

func (v *SDKCompactionView) ParentPromptID() string {
	if !v.Current() {
		return ""
	}
	return v.owner.Identity().PromptID
}

// DiscardHelper records the observed decision not to apply this query's
// completed summary. It retains the API ledger and never emits adoption.
func (v *SDKCompactionView) DiscardHelper(clientID string) {
	if clientID != "" {
		v.discardHelperKey(digest("compact", clientID))
	}
}

func (v *SDKCompactionView) discardHelperKey(key string) {
	if v == nil || v.owner == nil {
		return
	}
	r := v.owner
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	defer r.tracker.saveSDKSessionLocked(r.call.state.scope)
	s := &r.call.state.sdk
	operation := s.compactions[key]
	if s.result == nil && operation != nil && !operation.applied && !operation.discarded {
		operation.discarded = true
		s.pendingCompactions--
	}
}

// Resolve preserves exact observed content blocks rather than converting
// them to concatenated text. A selection must cover whole normalized rows in
// order; a partial stream-yield selection has no lossless inverse and fails.
func (v *SDKCompactionView) Resolve(messages []SDKHistoryMessage) ([]json.RawMessage, error) {
	if !v.Current() {
		return nil, ErrSDKCompactionViewStale
	}
	if len(messages) == 0 {
		return nil, nil
	}
	positions := make(map[string]int, len(v.history.Messages))
	for index, message := range v.history.Messages {
		positions[message.UUID] = index
	}
	selected := make(map[int]int)
	previousPosition, previousRow := -1, -1
	var rowOrder []int
	for _, message := range messages {
		position, found := positions[message.UUID]
		if !found || position <= previousPosition || message != v.history.Messages[position] {
			return nil, ErrSDKCompactionContentUnknown
		}
		previousPosition = position
		rowIndex := v.rowByUUID[message.UUID]
		if rowIndex < 0 {
			continue
		}
		if selected[rowIndex] == 0 {
			if previousRow >= 0 && rowIndex != previousRow+1 {
				return nil, ErrSDKCompactionContentUnknown
			}
			rowOrder = append(rowOrder, rowIndex)
			previousRow = rowIndex
		}
		selected[rowIndex]++
	}
	result := make([]json.RawMessage, 0, len(rowOrder))
	for _, rowIndex := range rowOrder {
		if selected[rowIndex] != v.owners[rowIndex] {
			return nil, ErrSDKCompactionContentUnknown
		}
		result = append(result, append(json.RawMessage(nil), v.rows[rowIndex]...))
	}
	return result, nil
}

// Discard releases the view's references. Like ordinary Go request buffers,
// this does not promise cryptographic erasure of garbage-collected memory.
func (v *SDKCompactionView) Discard() {
	if v != nil {
		v.owner, v.rows, v.rowByUUID, v.owners = nil, nil, nil, nil
		v.history = SDKHistorySnapshot{}
	}
}
