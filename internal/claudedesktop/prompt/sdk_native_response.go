package prompt

import (
	"bytes"
	"encoding/json"
	"time"
)

func (r *Response) EnableNativeContent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nativeEnabled = true
}

func (r *Response) SetNativeRequestID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(id) <= 1024 {
		r.nativeRequestID = id
	}
}

func (r *Response) NativeContentMessages() ([]SDKNativeMessage, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := make([]SDKNativeMessage, len(r.nativeRows))
	for index, row := range r.nativeRows {
		rows[index] = cloneNativeMessage(row)
	}
	return rows, r.nativeIssue
}

func (r *Response) recordNativeBuffered(body []byte, at time.Time) {
	if !r.nativeEnabled || len(r.sdkHistory) == 0 {
		return
	}
	if len(body) > maxSDKCompactionViewBytes {
		r.nativeIssue = "sdk-native-content-retention-limit"
		return
	}
	r.nativeRows = append(r.nativeRows, SDKNativeMessage{Type: "assistant", UUID: r.sdkHistory[len(r.sdkHistory)-1].UUID,
		Timestamp: nativeContentTimestamp(at), RequestID: r.nativeRequestID, Message: bytes.Clone(body)})
}

func (r *Response) beginNativeResponse(raw []byte) {
	if !r.nativeEnabled {
		return
	}
	var event struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &event) != nil || event.Message == nil {
		r.nativeIssue = "unobserved-sdk-native-response-header"
		return
	}
	r.nativeHeader = event.Message
	r.nativeUsage = nativeUsageMerge(nil, event.Message["usage"])
}

func (r *Response) recordNativeBlock(index int, at time.Time) {
	if !r.nativeEnabled || len(r.sdkHistory) == 0 {
		return
	}
	if r.nativeHeader == nil || r.sdkWireStream.invalid || index < 0 || index >= len(r.sdkWireStream.blocks) || !r.sdkWireStream.blocks[index].closed {
		r.nativeIssue = "unobserved-sdk-native-response-content"
		return
	}
	message := make(map[string]json.RawMessage, len(r.nativeHeader))
	for key, value := range r.nativeHeader {
		message[key] = value
	}
	message["content"], _ = json.Marshal([]map[string]json.RawMessage{r.sdkWireStream.blocks[index].fields})
	encoded, err := json.Marshal(message)
	if err != nil {
		r.nativeIssue = "invalid-sdk-native-response-content"
		return
	}
	r.nativeRows = append(r.nativeRows, SDKNativeMessage{Type: "assistant", UUID: r.sdkHistory[len(r.sdkHistory)-1].UUID,
		Timestamp: nativeContentTimestamp(at), RequestID: r.nativeRequestID, Message: encoded})
}

// Native message_delta mutates every yielded message's usage, stop_reason and
// stop_details. Content and the per-yield UUID/timestamp remain unchanged.
func (r *Response) updateNativeResponse(raw []byte) {
	if !r.nativeEnabled {
		return
	}
	var event struct {
		Usage json.RawMessage            `json:"usage"`
		Delta map[string]json.RawMessage `json:"delta"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	r.nativeUsage = nativeUsageMerge(r.nativeUsage, event.Usage)
	usage, _ := json.Marshal(r.nativeUsage)
	details := event.Delta["stop_details"]
	if len(details) == 0 {
		details = json.RawMessage(`null`)
	} else {
		var fields map[string]json.RawMessage
		if json.Unmarshal(details, &fields) == nil && fields != nil {
			// The pinned reducer removes this internal credit before assigning
			// stop_details to the native message object.
			delete(fields, "fallback_credit_token")
			details, _ = json.Marshal(fields)
		}
	}
	// Retain the actual terminal header even when an empty response yields no
	// assistant content rows. That does not create a synthetic transcript row.
	if r.nativeHeader != nil {
		r.nativeHeader["usage"], r.nativeHeader["stop_details"] = usage, details
		if reason, exists := event.Delta["stop_reason"]; exists {
			r.nativeHeader["stop_reason"] = reason
		} else {
			delete(r.nativeHeader, "stop_reason")
		}
	}
	for index := range r.nativeRows {
		var message map[string]json.RawMessage
		if json.Unmarshal(r.nativeRows[index].Message, &message) != nil {
			r.nativeIssue = "invalid-sdk-native-response-content"
			continue
		}
		message["usage"], message["stop_details"] = usage, details
		if reason, exists := event.Delta["stop_reason"]; exists {
			message["stop_reason"] = reason
		} else {
			delete(message, "stop_reason")
		}
		r.nativeRows[index].Message, _ = json.Marshal(message)
	}
}

// Aj in the pinned query module uses positive-only input counts, nullish
// output/metadata updates and two nested cache-duration counters. This differs
// from blindly overlaying message_delta usage onto message_start usage.
func nativeUsageMerge(previous map[string]json.RawMessage, raw json.RawMessage) map[string]json.RawMessage {
	if previous == nil {
		_ = json.Unmarshal([]byte(`{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"service_tier":"standard","inference_geo":"","iterations":[],"speed":"standard"}`), &previous)
	}
	var current map[string]json.RawMessage
	_ = json.Unmarshal(raw, &current)
	result := make(map[string]json.RawMessage, len(previous))
	for key, value := range previous {
		result[key] = bytes.Clone(value)
	}
	number := func(value json.RawMessage) float64 {
		var n float64
		_ = json.Unmarshal(value, &n)
		return n
	}
	for _, key := range []string{"input_tokens", "cache_read_input_tokens"} {
		if number(current[key]) > 0 {
			result[key] = current[key]
		}
	}
	var cache map[string]json.RawMessage
	_ = json.Unmarshal(current["cache_creation"], &cache)
	sum := number(cache["ephemeral_1h_input_tokens"]) + number(cache["ephemeral_5m_input_tokens"])
	if number(current["cache_creation_input_tokens"]) > 0 {
		result["cache_creation_input_tokens"] = current["cache_creation_input_tokens"]
	} else if sum > 0 {
		result["cache_creation_input_tokens"], _ = json.Marshal(sum)
	}
	for _, key := range []string{"output_tokens", "service_tier", "inference_geo", "iterations", "speed"} {
		if value := current[key]; len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			result[key] = value
		}
	}
	for key, children := range map[string][]string{
		"output_tokens_details": {"thinking_tokens"},
		"server_tool_use":       {"web_search_requests", "web_fetch_requests"},
		"cache_creation":        {"ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens"},
	} {
		if key == "cache_creation" && sum <= 0 {
			continue
		}
		var oldValues, newValues map[string]json.RawMessage
		_ = json.Unmarshal(result[key], &oldValues)
		_ = json.Unmarshal(current[key], &newValues)
		if oldValues == nil {
			oldValues = make(map[string]json.RawMessage)
		}
		for _, child := range children {
			if value := newValues[child]; len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				oldValues[child] = value
			}
		}
		result[key], _ = json.Marshal(oldValues)
	}
	return result
}
