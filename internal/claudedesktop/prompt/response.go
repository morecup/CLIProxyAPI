package prompt

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Response retains structural facts and fingerprints after completion. Its
// bounded wire assembler holds content only until the response ends. A
// message_delta alone cannot prove that a truncated stream completed.
type Response struct {
	nativeEnabled      bool
	nativeRows         []SDKNativeMessage
	nativeHeader       map[string]json.RawMessage
	nativeUsage        map[string]json.RawMessage
	nativeIssue        string
	nativeRequestID    string
	sdkTokens          sdkResponseTokens
	sdkWireStream      sdkWireStream
	sdkWireKnown       bool
	sdkWireFingerprint string
	messageID          string
	sdkHistory         []SDKHistoryMessage
	sdkHistoryIssue    string
	mu                 sync.Mutex
	stopReason         string
	toolIDs            []string
	complete           bool
	assistant          bool
	sdkTools           []ToolObservation
	firstAssistantAt   time.Time
	sdkBlocks          map[int]sdkResponseBlock
}

type sdkResponseBlock struct {
	tool   ToolObservation
	isTool bool
	isText bool
	closed bool
}

func (r *Response) ObservePayload(body []byte, streaming bool) {
	r.ObservePayloadAt(body, streaming, time.Now())
}

// ObservePayloadAt is for a payload whose complete arrival time is known.
// Buffered SSE callers must observe lines as they arrive to retain SDK timing.
func (r *Response) ObservePayloadAt(body []byte, streaming bool, at time.Time) {
	if streaming {
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			r.ObserveStreamLineAt(line, at)
		}
		return
	}
	var message struct {
		Model      string          `json:"model"`
		Usage      json.RawMessage `json:"usage"`
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &message) != nil || message.Type != "message" {
		return
	}
	var content []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(message.Content, &content)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.complete {
		return
	}
	r.stopReason = message.StopReason
	r.assistant = message.Role == "assistant"
	if r.assistant {
		r.sdkTokens.synthetic = message.Model == "<synthetic>"
		r.sdkTokens.observeUsage(message.Usage)
		r.firstAssistantAt = at
		r.messageID = message.ID
		r.recordSDKHistoryAssistant(EstimateSDKContent(message.Content))
		if len(r.sdkHistory) > 0 {
			r.sdkHistory[len(r.sdkHistory)-1].UsageExcluded = sdkUsageContentExcluded(message.Content)
		}
		r.sdkWireFingerprint, r.sdkWireKnown = sdkWireMessageFingerprint("assistant", message.Content)
		r.recordNativeBuffered(body, at)
	}
	for _, block := range content {
		if block.Type == "tool_use" && len(r.toolIDs) <= maxToolsPerRequest {
			r.toolIDs = append(r.toolIDs, block.ID)
			r.sdkTools = append(r.sdkTools, ToolObservation{ID: block.ID, Name: block.Name})
		}
	}
	r.complete = true
}

func (r *Response) ObserveStreamLine(line []byte) {
	r.ObserveStreamLineAt(line, time.Now())
}

// ObserveStreamLineAt observes the SDK assistant boundary at content_block_stop,
// independently of whether the enclosing HTTP/SSE response later completes.
func (r *Response) ObserveStreamLineAt(line []byte, at time.Time) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	var event struct {
		Usage   json.RawMessage `json:"usage"`
		Type    string          `json:"type"`
		Index   *int            `json:"index"`
		Message struct {
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
			ID    string          `json:"id"`
			Role  string          `json:"role"`
		} `json:"message"`
		Block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			StopReason string `json:"stop_reason"`
			Type       string `json:"type"`
			Text       string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &event) != nil {
		return
	}
	var tokenEvent struct {
		Block json.RawMessage `json:"content_block"`
		Delta json.RawMessage `json:"delta"`
	}
	_ = json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &tokenEvent)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.complete {
		return
	}
	switch event.Type {
	case "message_start":
		r.beginNativeResponse(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		r.sdkTokens.synthetic = event.Message.Model == "<synthetic>"
		r.sdkTokens.observeUsage(event.Message.Usage)
		r.assistant = event.Message.Role == "assistant"
		r.messageID = event.Message.ID
		r.sdkWireStream.begin(r.assistant)
	case "content_block_start":
		r.sdkWireStream.start(event.Index, tokenEvent.Block)
		if event.Index != nil {
			r.sdkTokens.start(*event.Index, tokenEvent.Block)
		}
		if event.Block.Type == "tool_use" && len(r.toolIDs) <= maxToolsPerRequest {
			r.toolIDs = append(r.toolIDs, event.Block.ID)
		}
		if r.assistant && event.Index != nil && *event.Index >= 0 && *event.Index < 4096 && event.Block.Type != "" {
			if r.sdkBlocks == nil {
				r.sdkBlocks = make(map[int]sdkResponseBlock)
			}
			if _, exists := r.sdkBlocks[*event.Index]; !exists {
				r.sdkBlocks[*event.Index] = sdkResponseBlock{isText: event.Block.Type == "text", isTool: event.Block.Type == "tool_use", tool: ToolObservation{ID: event.Block.ID, Name: event.Block.Name}}
			}
		}
	case "content_block_delta":
		r.sdkWireStream.delta(event.Index, tokenEvent.Delta)
		if event.Index != nil {
			r.sdkTokens.delta(*event.Index, tokenEvent.Delta)
		}
	case "content_block_stop":
		r.sdkWireStream.close(event.Index)
		if !r.assistant || event.Index == nil {
			break
		}
		block, exists := r.sdkBlocks[*event.Index]
		if !exists || block.closed {
			break
		}
		block.closed = true
		r.sdkBlocks[*event.Index] = block
		estimate := r.sdkTokens.close(*event.Index)
		if *event.Index < len(r.sdkWireStream.blocks) {
			wireBlock := r.sdkWireStream.blocks[*event.Index]
			if wireBlock.closed && wireBlock.kind == "server_tool_use" {
				// Generic native blocks estimate their fully assembled JSON,
				// not the empty input object in content_block_start.
				raw, err := json.Marshal(wireBlock.fields)
				if err == nil {
					estimate.Tokens, estimate.Known = sdkBlockTokens(raw, 0)
				}
			}
		}
		r.recordSDKHistoryAssistant(estimate)
		r.recordNativeBlock(*event.Index, at)
		if tokenBlock := r.sdkTokens.blocks[*event.Index]; tokenBlock != nil && len(r.sdkHistory) > 0 {
			r.sdkHistory[len(r.sdkHistory)-1].UsageExcluded = tokenBlock.usageExcluded
		}
		if r.firstAssistantAt.IsZero() {
			r.firstAssistantAt = at
		}
		if block.isTool && len(r.sdkTools) <= maxToolsPerRequest {
			r.sdkTools = append(r.sdkTools, block.tool)
		}
	case "message_delta":
		r.updateNativeResponse(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		r.sdkTokens.observeUsage(event.Usage)
		for index := range r.sdkHistory {
			r.sdkHistory[index].Usage = r.sdkTokens.usage
			r.sdkHistory[index].UsageKnown = r.sdkTokens.knownUsage()
		}
		if event.Delta.StopReason != "" {
			r.stopReason = event.Delta.StopReason
		}
	case "message_stop":
		r.complete = true
		r.sdkWireFingerprint, r.sdkWireKnown = r.sdkWireStream.finish()
	case "error":
		r.sdkWireStream.invalidate()
	}
}

func (r *Response) Outcome() (string, []string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopReason, append([]string(nil), r.toolIDs...), r.complete
}

// SDKAssistantMessage reports the first yielded assistant object. Desktop's SDK
// yields one for a completed content block, before message_delta/message_stop.
// This observation does not prove a successful response or completed prompt.
func (r *Response) SDKAssistantMessage() (time.Time, []ToolObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstAssistantAt, append([]ToolObservation(nil), r.sdkTools...)
}

func (r *Response) recordSDKHistoryAssistant(estimate SDKTokenEstimate) {
	if r.messageID == "" {
		r.sdkHistoryIssue = "missing-assistant-message-id"
	}
	if len(r.messageID) > 1024 || len(r.sdkHistory) >= 4096 {
		r.sdkHistoryIssue = "sdk-response-history-limit"
		return
	}
	r.sdkHistory = append(r.sdkHistory, SDKHistoryMessage{Type: "assistant", UUID: uuid.NewString(), MessageID: r.messageID,
		TokenEstimate: estimate, Usage: r.sdkTokens.usage, UsageKnown: r.sdkTokens.knownUsage(), Synthetic: r.sdkTokens.synthetic})
}

func (r *Response) SDKHistoryMessages() ([]SDKHistoryMessage, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SDKHistoryMessage(nil), r.sdkHistory...), r.sdkHistoryIssue
}

func (r *Response) SDKWireFingerprint() (string, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sdkWireFingerprint, r.sdkWireKnown, r.complete
}
