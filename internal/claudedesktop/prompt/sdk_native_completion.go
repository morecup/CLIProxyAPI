package prompt

import (
	"encoding/json"
)

// NativeCompletedMessage is the content consumer for owned child execution.
// It uses the same pinned SDK reducer as the transcript observer. A generic
// Anthropic SSE collector has different initial-block and citation semantics.
// The per-yield UUIDs used internally here are never published or persisted.
func (r *Response) NativeCompletedMessage() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.complete || !r.assistant || !r.nativeEnabled || r.nativeIssue != "" {
		return nil, ErrSDKSessionInvalid
	}
	var message map[string]json.RawMessage
	if len(r.nativeRows) > 0 {
		if json.Unmarshal(r.nativeRows[len(r.nativeRows)-1].Message, &message) != nil {
			return nil, ErrSDKSessionInvalid
		}
	} else {
		if r.nativeHeader == nil || r.stopReason == "" {
			return nil, ErrSDKSessionInvalid
		}
		message = make(map[string]json.RawMessage, len(r.nativeHeader))
		for key, value := range r.nativeHeader {
			message[key] = value
		}
	}
	content := make([]json.RawMessage, 0)
	for _, row := range r.nativeRows {
		var current struct{ Content []json.RawMessage }
		if json.Unmarshal(row.Message, &current) != nil {
			return nil, ErrSDKSessionInvalid
		}
		content = append(content, current.Content...)
	}
	message["content"], _ = json.Marshal(content)
	return json.Marshal(message)
}
