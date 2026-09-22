package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// ParseResponse validates the entire tool batch before any tool can run.
// Server-managed tools are not local execution capabilities.
func ParseResponse(raw []byte) (Message, []ToolCall, error) {
	var response struct {
		ID         string          `json:"id"`
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Role != "assistant" || len(response.Content) == 0 {
		return Message{}, nil, errors.New("Claude Desktop response has no complete assistant message")
	}
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	var calls []ToolCall
	if json.Unmarshal(response.Content, &blocks) == nil {
		seen := map[string]bool{}
		for _, block := range blocks {
			if block.Type != "tool_use" {
				continue
			}
			var object map[string]json.RawMessage
			if block.ID == "" || block.Name == "" || seen[block.ID] || json.Unmarshal(block.Input, &object) != nil || object == nil {
				return Message{}, nil, errors.New("Claude Desktop response contains an invalid tool call")
			}
			seen[block.ID] = true
			calls = append(calls, ToolCall{ID: block.ID, Name: block.Name, Input: bytes.Clone(block.Input), MessageID: response.ID})
		}
	} else {
		var text string
		if json.Unmarshal(response.Content, &text) != nil || text == "" {
			return Message{}, nil, ErrInvalid
		}
	}
	if len(calls) > 0 && response.StopReason != "tool_use" {
		return Message{}, nil, errors.New("Claude Desktop tool batch has no tool-use terminal")
	}
	if response.StopReason == "tool_use" && len(calls) == 0 {
		return Message{}, nil, errors.New("Claude Desktop tool-use terminal has no local tool call")
	}
	return Message{Role: "assistant", Content: bytes.Clone(response.Content)}, calls, nil
}

func ToolResult(call ToolCall, data json.RawMessage, err error) json.RawMessage {
	return (*Runtime)(nil).ToolResult(call, data, err)
}

func (r *Runtime) run(ctx context.Context, id string, generation uint64, execution *execution) {
	var runErr error
	for {
		r.mu.Lock()
		t := r.tasks[id]
		if t == nil || t.Generation != generation {
			r.mu.Unlock()
			return
		}
		if ctx.Err() != nil || t.Status != "running" {
			r.mu.Unlock()
			runErr = context.Canceled
			break
		}
		invocation := Invocation{AgentID: t.ID, AgentType: t.AgentType, ParentAgentID: t.ParentAgentID,
			ParentPromptID: t.ParentPromptID, PromptID: t.PromptID, Kind: t.Kind, Model: t.Model, Depth: t.Depth, Messages: cloneMessages(t.Messages)}
		if r.options.OpenTranscript != nil {
			t.nativeInvocation++
			invocation.BeginNativeResponse = r.nativeObserverFactory(ctx, id, generation, execution, t.nativeInvocation)
		}
		r.mu.Unlock()
		raw, err := r.options.Execute(ctx, invocation)
		if err != nil {
			runErr = err
			break
		}
		message, calls, err := ParseResponse(raw)
		if err != nil {
			runErr = err
			break
		}
		var report json.RawMessage
		var sections resultSections
		if len(calls) == 0 {
			provenance := r.options.HandbackProvenance != nil && r.options.HandbackProvenance()
			report, sections, err = prepareResult(message.Content, provenance)
			if err != nil {
				runErr = err
				break
			}
		}
		r.mu.Lock()
		t = r.tasks[id]
		if t == nil || t.Generation != generation {
			r.mu.Unlock()
			return
		}
		if ctx.Err() != nil || t.Status != "running" {
			r.mu.Unlock()
			runErr = context.Canceled
			break
		}
		if t.transcript != nil && t.transcript.VerifyResponse(raw) != nil {
			t.TranscriptFailed = true
		}
		t.Messages = append(t.Messages, message)
		tokens, known := responseTokens(raw)
		// Native DMt reports the last assistant usage, not the sum of API
		// attempts. The separate SDK session ledger owns aggregate API usage.
		t.Tokens, t.UsageKnown = tokens, known
		if err := r.saveLocked(); err != nil {
			t.persistenceFailed = true
			r.mu.Unlock()
			runErr = err
			break
		}
		r.mu.Unlock()
		results := make([]json.RawMessage, 0, len(calls))
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				runErr = err
				break
			}
			data, err := r.ExecuteTool(ctx, Caller{AgentID: id, PromptID: invocation.PromptID, Model: invocation.Model, Depth: invocation.Depth}, call)
			if ctx.Err() != nil {
				runErr = ctx.Err()
				break
			}
			results = append(results, r.ToolResult(call, data, err))
		}
		if runErr != nil {
			break
		}
		r.mu.Lock()
		t = r.tasks[id]
		if t == nil || t.Generation != generation {
			r.mu.Unlock()
			return
		}
		if ctx.Err() != nil || t.Status != "running" {
			r.mu.Unlock()
			runErr = context.Canceled
			break
		}
		if len(results) != 0 {
			content, _ := json.Marshal(results)
			t.Messages = append(t.Messages, Message{Role: "user", Content: content})
			r.appendTranscriptInputLocked(t, t.Messages[len(t.Messages)-1], r.options.Now())
			t.ToolUses += len(results)
		}
		queued := r.consumePendingLocked(t, r.options.Now())
		if len(calls) == 0 && !queued {
			t.Result, t.ResultSections = report, sections
			end := r.finishLocked(t, execution, nil)
			r.mu.Unlock()
			r.notifySubagentEnd(ctx, end)
			return
		}
		err = r.saveLocked()
		if err != nil {
			t.persistenceFailed = true
		}
		r.mu.Unlock()
		if err != nil {
			runErr = err
			break
		}
	}
	r.mu.Lock()
	t := r.tasks[id]
	if t == nil || t.Generation != generation {
		r.mu.Unlock()
		return
	}
	end := r.finishLocked(t, execution, runErr)
	r.mu.Unlock()
	r.notifySubagentEnd(ctx, end)
}

// Terminal state, the generation result and its event obligation are published
// together. A following SendMessage cannot overtake or erase this transition.
// The returned SubagentEnd (nil unless the generation produced its result and
// the hook is installed) is delivered by the caller after releasing r.mu.
func (r *Runtime) finishLocked(t *task, execution *execution, runErr error) *SubagentEnd {
	t.Status = "completed"
	t.Error = ""
	if runErr != nil {
		t.Error = runErr.Error()
		t.Status = "failed"
		if errors.Is(runErr, context.Canceled) || t.StoppedByUser {
			t.Status = "killed"
		}
	}
	t.FinishedAt = r.options.Now()
	t.cancel, t.active = nil, nil
	r.enqueueEventLocked(t, "finished", t.Background)
	if err := r.saveLocked(); err != nil {
		t.persistenceFailed = true
	}
	execution.result = t.record
	execution.result.Messages = cloneMessages(t.Messages)
	execution.result.Result = bytes.Clone(t.Result)
	r.wakeDeliveryLocked()
	return r.subagentEndLocked(t, runErr)
}

func textContent(content json.RawMessage) json.RawMessage {
	var text string
	if json.Unmarshal(content, &text) == nil {
		raw, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		return raw
	}
	var blocks []json.RawMessage
	_ = json.Unmarshal(content, &blocks)
	result := make([]json.RawMessage, 0)
	for _, raw := range blocks {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "text" {
			result = append(result, raw)
		}
	}
	raw, _ := json.Marshal(result)
	return raw
}

func responseTokens(raw []byte) (int64, bool) {
	var value struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Usage == nil {
		return 0, false
	}
	var total int64
	for _, name := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		v, exists := value.Usage[name]
		if !exists {
			if name == "input_tokens" || name == "output_tokens" {
				return 0, false
			}
			continue
		}
		var n int64
		if json.Unmarshal(v, &n) != nil || n < 0 {
			return 0, false
		}
		total += n
	}
	return total, true
}
