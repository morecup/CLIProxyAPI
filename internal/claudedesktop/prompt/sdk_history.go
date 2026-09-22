package prompt

import "github.com/google/uuid"

const maxSDKHistoryMessages = 16384

// SDKHistoryMessage records structural ownership only. Neither message content,
// tool arguments/results nor credentials enter this history. Virtual/resumed
// flags belong to native message objects, not user-controlled HTTP headers.
type SDKHistoryMessage struct {
	TokenEstimate SDKTokenEstimate `json:"token_estimate"`
	Usage         SDKTokenUsage    `json:"usage"`
	UsageKnown    bool             `json:"usage_known,omitempty"`
	UsageExcluded bool             `json:"usage_excluded,omitempty"`
	Synthetic     bool             `json:"synthetic,omitempty"`
	Type          string           `json:"type"`
	UUID          string           `json:"uuid"`
	MessageID     string           `json:"message_id,omitempty"`
	Subtype       string           `json:"subtype,omitempty"`
	// A hashed, previously owned tool-result ID binds one native user yield
	// to its actual block even when the wire combines several result rows.
	WireToolResultID string `json:"wire_tool_result_id,omitempty"`
	// Restored native attachments can normalize into the preceding user row
	// or produce no wire content. These links are assigned only by adoption,
	// never inferred from strings in a caller's request.
	WireParentUUID                string `json:"wire_parent_uuid,omitempty"`
	NoWireContent                 bool   `json:"no_wire_content,omitempty"`
	IsVirtual                     bool   `json:"is_virtual,omitempty"`
	ResumedFromIncompleteThinking bool   `json:"resumed_from_incomplete_thinking,omitempty"`
}

// SDKHistorySnapshot is the proxy-owned message model. Even an unbroken local
// history is not a claim to reconstruct a caller's private hooks or UI objects.
// Compaction must additionally own its query, restoration and application.
type SDKHistorySnapshot struct {
	TokenEstimate      SDKTokenEstimate
	Messages           []SDKHistoryMessage
	Groups             [][]SDKHistoryMessage
	OwnedMessagesKnown bool
	IncompleteReason   string
}

type sdkHistory struct {
	messages               []SDKHistoryMessage
	incompleteReason       string
	expectedText           []string
	expectedTextKnown      bool
	pendingReconciliations int
}

// GroupSDKHistory follows v140609 Lhe/qQ: retain the last compact boundary,
// remove progress, and start a group on a changed non-resumed assistant ID.
// Virtual user/assistant objects do not update the previous assistant ID.
func GroupSDKHistory(messages []SDKHistoryMessage) [][]SDKHistoryMessage {
	start := 0
	for index, message := range messages {
		if message.Type == "system" && message.Subtype == "compact_boundary" {
			start = index
		}
	}
	var groups [][]SDKHistoryMessage
	var current []SDKHistoryMessage
	previousID := ""
	for _, message := range messages[start:] {
		if message.Type == "progress" {
			continue
		}
		if (message.Type == "user" || message.Type == "assistant") && message.IsVirtual {
			current = append(current, message)
			continue
		}
		if message.Type == "assistant" && message.MessageID != previousID && !message.ResumedFromIncompleteThinking && len(current) != 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, message)
		if message.Type == "assistant" {
			previousID = message.MessageID
		}
	}
	if len(current) != 0 {
		groups = append(groups, current)
	}
	return groups
}

func (h *sdkHistory) unknown(reason string) {
	if h != nil && h.incompleteReason == "" {
		h.incompleteReason = reason
	}
}

func (h *sdkHistory) append(message SDKHistoryMessage) {
	if h == nil {
		return
	}
	if len(h.messages) >= maxSDKHistoryMessages {
		h.unknown("sdk-history-retention-limit")
		return
	}
	if message.UUID == "" {
		message.UUID = uuid.NewString()
	}
	if message.Type == "assistant" && message.MessageID == "" {
		h.unknown("missing-assistant-message-id")
	}
	h.messages = append(h.messages, message)
}

func (h *sdkHistory) boundary() {
	if h == nil {
		return
	}
	h.messages = nil
	h.expectedText, h.expectedTextKnown = nil, false
	h.append(SDKHistoryMessage{Type: "system", Subtype: "compact_boundary"})
}

func (h *sdkHistory) snapshot() SDKHistorySnapshot {
	if h == nil {
		return SDKHistorySnapshot{IncompleteReason: "unobserved-sdk-session-history"}
	}
	reason := h.incompleteReason
	if reason == "" && h.pendingReconciliations > 0 {
		reason = "awaiting-sdk-history-reconciliation"
	}
	estimate := EstimateSDKHistory(h.messages)
	if reason != "" {
		estimate = SDKTokenEstimate{}
	}
	return SDKHistorySnapshot{TokenEstimate: estimate, Messages: append([]SDKHistoryMessage(nil), h.messages...), Groups: GroupSDKHistory(h.messages), OwnedMessagesKnown: reason == "", IncompleteReason: reason}
}

// ObserveSDKHistory consumes cumulative, completed assistant yields from this
// physical response. Duplicate polling and obsolete retry callbacks cannot add
// messages. Partial response blocks have no UUID and are not materialized.
func (r *Request) ObserveSDKHistory(messages []SDKHistoryMessage, issue string) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.finished || r.attempt != r.call.attempt {
		return
	}
	h := r.call.state.sdk.history
	if h == nil {
		return
	}
	if issue != "" {
		h.unknown(issue)
	}
	if len(messages) < r.sdkHistoryObserved {
		h.unknown("nonmonotonic-sdk-history-observation")
		return
	}
	// Native yielded objects share their response usage, which is updated by
	// later message_delta events. Refresh those numeric facts without yielding
	// the same assistant again, including when another prompt interleaves.
	for index, offset := range r.sdkHistoryOffsets {
		if index >= len(messages) || offset < 0 || offset >= len(h.messages) || h.messages[offset].UUID != messages[index].UUID {
			h.unknown("changed-sdk-response-history")
			return
		}
		h.messages[offset].Usage = messages[index].Usage
		h.messages[offset].UsageKnown = messages[index].UsageKnown
	}
	for _, message := range messages[r.sdkHistoryObserved:] {
		if message.Type != "assistant" || message.UUID == "" || len(message.UUID) > 128 || len(message.MessageID) > 1024 {
			h.unknown("invalid-sdk-assistant-history-observation")
			continue
		}
		r.sdkHistoryOffsets = append(r.sdkHistoryOffsets, len(h.messages))
		h.append(message)
		r.call.sdkAssistantYields++
	}
	r.sdkHistoryObserved = len(messages)
}

// SDKHistory returns the current session history, not a mutable view or the
// sealed SDK result. Later prompts can append to the same session history.
func (r *Request) SDKHistory() SDKHistorySnapshot {
	if r == nil {
		return (*sdkHistory)(nil).snapshot()
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	return r.call.state.sdk.history.snapshot()
}

func (t *Tracker) SDKHistory(accountID, sessionID string) SDKHistorySnapshot {
	if t == nil {
		return (*sdkHistory)(nil).snapshot()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	scope := digest(accountID, sessionID)
	if restored := t.restoredHistories[scope]; restored != nil {
		return restored.history.snapshot()
	}
	for _, state := range t.prompts {
		if state.scope == scope {
			return state.sdk.history.snapshot()
		}
	}
	return (*sdkHistory)(nil).snapshot()
}
