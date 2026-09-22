package tasks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

type outputStateStore struct {
	mu       sync.Mutex
	raw      []byte
	revision string
}

func (s *outputStateStore) Load(string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.raw), s.revision, nil
}
func (s *outputStateStore) Save(_ string, revision string, raw []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != s.revision {
		return "", errors.New("stale test state")
	}
	s.raw, s.revision = bytes.Clone(raw), uuid.NewString()
	return s.revision, nil
}
func outputAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("owned task transition timed out")
		var zero T
		return zero
	}
}
func outputSettled(t *testing.T, r *tasks.Runtime, id, status string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for _, row := range r.Snapshots() {
			if row.ID == id && row.Status == status {
				if row.PersistenceFailed {
					t.Fatal("transcript health degraded")
				}
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("task did not settle")
		case <-time.After(time.Millisecond):
		}
	}
}
func observedTaskResponse(t *testing.T, id, text string) ([]byte, *claudeprompt.Response) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"id": id, "type": "message", "role": "assistant", "model": "sonnet", "content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 2}})
	if err != nil {
		t.Fatal(err)
	}
	response := &claudeprompt.Response{}
	response.EnableNativeContent()
	response.SetNativeRequestID("req_" + id)
	response.ObservePayload(raw, false)
	return raw, response
}
func outputTool(t *testing.T, r *tasks.Runtime, owner tasks.Caller, id string) (json.RawMessage, json.RawMessage) {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"task_id": id, "block": false})
	call := tasks.ToolCall{ID: uuid.NewString(), Name: "TaskOutput", Input: input}
	data, err := r.ExecuteTool(t.Context(), owner, call)
	if err != nil {
		t.Fatal(err)
	}
	result := r.ToolResult(call, data, nil)
	if gjson.GetBytes(result, "is_error").Bool() {
		t.Fatal("native TaskOutput returned an error", gjson.GetBytes(result, "content").String())
	}
	return data, result
}

func TestTaskNativeOutputLongReportRunningAndRestart(t *testing.T) {
	root := t.TempDir()
	state := &outputStateStore{}
	invocations := make(chan tasks.Invocation, 4)
	replies := make(chan []byte, 4)
	newOwner := func() (*tasks.Runtime, *claudeprompt.Tracker) {
		tracker := claudeprompt.NewTracker(nil, claudeprompt.SDKNativeContentOptions{TranscriptStore: helps.NewClaudeDesktopTranscriptStore(root, "egress"), Version: "2.1.247", Entrypoint: "claude-desktop"})
		r, err := tasks.New(t.Context(), tasks.Options{Scope: "owned-session", Store: state,
			OpenTranscript: func(id, leaf string) (tasks.Transcript, error) {
				return tracker.OpenSidechain("account", "session", id, leaf)
			},
			ResolveModel: func(parent, selected, kind string) (string, error) { return parent, nil },
			Execute: func(ctx context.Context, inv tasks.Invocation) ([]byte, error) {
				invocations <- inv
				select {
				case raw := <-replies:
					return raw, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close(); _ = tracker.Close() })
		return r, &tracker
	}
	r, tracker := newOwner()
	owner := tasks.Caller{PromptID: uuid.NewString(), Model: "sonnet"}
	launch, err := r.ExecuteTool(t.Context(), owner, tasks.ToolCall{ID: "launch", Name: "Agent", Input: json.RawMessage(`{"description":"owned task","prompt":"PRIVATE_INITIAL"}`)})
	if err != nil {
		t.Fatal(err)
	}
	id, path := gjson.GetBytes(launch, "agentId").String(), gjson.GetBytes(launch, "outputFile").String()
	if id == "" || path == "" || !gjson.GetBytes(launch, "canReadOutputFile").Bool() {
		t.Fatal("launch did not publish genuine output capability")
	}
	inv := outputAwait(t, invocations)
	observe := inv.BeginNativeResponse()
	text := strings.Repeat("PRIVATE_LONG 😀 ", 5000) + " END_OF_REPORT"
	raw, response := observedTaskResponse(t, "msg_first", text)
	if err := observe(response.NativeContentMessages()); err != nil {
		t.Fatal(err)
	}
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	running, runningResult := outputTool(t, r, owner, id)
	if !gjson.GetBytes(running, "task.isRawTranscript").Bool() || gjson.GetBytes(running, "retrieval_status").String() != "not_ready" || !strings.Contains(gjson.GetBytes(runningResult, "content").String(), "Full output: "+path) {
		t.Fatal("running task did not retrieve/truncate actual JSONL")
	}
	replies <- raw
	outputSettled(t, r, id, "completed")
	completed, rendered := outputTool(t, r, owner, id)
	if gjson.GetBytes(completed, "task.isRawTranscript").Bool() || gjson.GetBytes(completed, "task.output").String() != text || !strings.Contains(gjson.GetBytes(rendered, "content").String(), "Full output: "+path) || !strings.Contains(gjson.GetBytes(rendered, "content").String(), "END_OF_REPORT") {
		t.Fatal("long retained report was lost or nominally rejected")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte("PRIVATE_INITIAL")) || !bytes.Contains(before, []byte("END_OF_REPORT")) {
		t.Fatal("published path lacks full native transcript")
	}
	if !errors.Is(observe(response.NativeContentMessages()), tasks.ErrNativeObserverRetired) {
		t.Fatal("terminal callback remained active")
	}
	r.Close()
	_ = tracker.Close()
	restored, nextTracker := newOwner()
	input, _ := json.Marshal(map[string]string{"to": id, "message": "PRIVATE_RESUME"})
	if _, err := restored.ExecuteTool(t.Context(), owner, tasks.ToolCall{ID: "resume", Name: "SendMessage", Input: input}); err != nil {
		t.Fatal(err)
	}
	second := outputAwait(t, invocations)
	if second.AgentID != id || second.PromptID == inv.PromptID || len(second.Messages) != 3 {
		t.Fatal("restart/resume lost owned history")
	}
	nextRaw, nextResponse := observedTaskResponse(t, "msg_second", "second report")
	if err := second.BeginNativeResponse()(nextResponse.NativeContentMessages()); err != nil {
		t.Fatal(err)
	}
	replies <- nextRaw
	outputSettled(t, restored, id, "completed")
	if err := nextTracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, before) || !bytes.Contains(after, []byte("PRIVATE_RESUME")) || !bytes.Contains(after, []byte("second report")) {
		t.Fatal("restart rewrote or disconnected output")
	}
}

func TestTaskNativeObserversRejectRetiredAttemptsAndGenerations(t *testing.T) {
	tracker := claudeprompt.NewTracker(nil, claudeprompt.SDKNativeContentOptions{TranscriptStore: helps.NewClaudeDesktopTranscriptStore(t.TempDir(), "egress")})
	t.Cleanup(func() { _ = tracker.Close() })
	invocations := make(chan tasks.Invocation, 4)
	replies := make(chan []byte, 4)
	r, err := tasks.New(t.Context(), tasks.Options{Scope: "s", Store: &outputStateStore{},
		OpenTranscript: func(id, leaf string) (tasks.Transcript, error) { return tracker.OpenSidechain("a", "s", id, leaf) },
		ResolveModel:   func(parent, selected, kind string) (string, error) { return parent, nil },
		Execute: func(ctx context.Context, inv tasks.Invocation) ([]byte, error) {
			invocations <- inv
			select {
			case raw := <-replies:
				return raw, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	owner := tasks.Caller{PromptID: uuid.NewString(), Model: "sonnet"}
	data, err := r.ExecuteTool(t.Context(), owner, tasks.ToolCall{ID: "launch", Name: "Agent", Input: json.RawMessage(`{"description":"owned","prompt":"initial"}`)})
	if err != nil {
		t.Fatal(err)
	}
	id, path := gjson.GetBytes(data, "agentId").String(), gjson.GetBytes(data, "outputFile").String()
	first := outputAwait(t, invocations)
	old := first.BeginNativeResponse()
	_, partial := observedTaskResponse(t, "msg_partial", "first attempt")
	if old(partial.NativeContentMessages()) != nil {
		t.Fatal("first observed attempt rejected")
	}
	next := first.BeginNativeResponse()
	_, stale := observedTaskResponse(t, "msg_stale", "MUST_NOT_APPEND")
	if !errors.Is(old(stale.NativeContentMessages()), tasks.ErrNativeObserverRetired) {
		t.Fatal("superseded attempt retained write authority")
	}
	raw, response := observedTaskResponse(t, "msg_completed", "completed report")
	if next(response.NativeContentMessages()) != nil {
		t.Fatal("current attempt lost authority")
	}
	replies <- raw
	outputSettled(t, r, id, "completed")
	resume, _ := json.Marshal(map[string]string{"to": id, "message": "resume"})
	if _, err := r.ExecuteTool(t.Context(), owner, tasks.ToolCall{ID: "resume", Name: "SendMessage", Input: resume}); err != nil {
		t.Fatal(err)
	}
	second := outputAwait(t, invocations)
	if !errors.Is(first.BeginNativeResponse()(stale.NativeContentMessages()), tasks.ErrNativeObserverRetired) {
		t.Fatal("old generation minted a new write capability")
	}
	current := second.BeginNativeResponse()
	if err := r.Stop(id, true); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(current(stale.NativeContentMessages()), tasks.ErrNativeObserverRetired) {
		t.Fatal("stopped generation accepted callback")
	}
	r.Close()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("MUST_NOT_APPEND")) || !bytes.Contains(content, []byte("first attempt")) || !bytes.Contains(content, []byte("completed report")) {
		t.Fatal("retry/generation history corrupted")
	}
}

func TestTaskNativeOutputFailuresRemainVisibleWithoutRejectingModelResult(t *testing.T) {
	for _, mode := range []string{"unobserved-response", "late-projection-corruption"} {
		t.Run(mode, func(t *testing.T) {
			tracker := claudeprompt.NewTracker(nil, claudeprompt.SDKNativeContentOptions{TranscriptStore: helps.NewClaudeDesktopTranscriptStore(t.TempDir(), "egress")})
			t.Cleanup(func() { _ = tracker.Close() })
			text := strings.Repeat("actual report ", 4000)
			r, err := tasks.New(t.Context(), tasks.Options{Scope: "s", Store: &outputStateStore{},
				OpenTranscript: func(id, leaf string) (tasks.Transcript, error) { return tracker.OpenSidechain("a", "s", id, leaf) },
				ResolveModel:   func(parent, selected, kind string) (string, error) { return parent, nil },
				Execute: func(_ context.Context, inv tasks.Invocation) ([]byte, error) {
					raw, response := observedTaskResponse(t, "msg_owned", text)
					if mode != "unobserved-response" {
						if err := inv.BeginNativeResponse()(response.NativeContentMessages()); err != nil {
							return nil, err
						}
					}
					return raw, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.Close)
			owner := tasks.Caller{PromptID: uuid.NewString(), Model: "sonnet"}
			data, err := r.ExecuteTool(t.Context(), owner, tasks.ToolCall{ID: "launch", Name: "Agent", Input: json.RawMessage(`{"description":"owned","prompt":"input","run_in_background":false}`)})
			if err != nil || gjson.GetBytes(data, "status").String() != "completed" {
				t.Fatal("transcript failure rejected successful model result", err)
			}
			id := gjson.GetBytes(data, "agentId").String()
			if mode == "late-projection-corruption" {
				if err := tracker.FlushNativeTranscript(); err != nil {
					t.Fatal(err)
				}
				// Obtain the actual owned file from the native long-result mapper.
				_, result := outputTool(t, r, owner, id)
				content := gjson.GetBytes(result, "content").String()
				start := strings.Index(content, "[Truncated. Full output: ")
				if start < 0 {
					t.Fatal("no real output path")
				}
				rest := content[start+len("[Truncated. Full output: "):]
				end := strings.Index(rest, "]")
				if end < 0 {
					t.Fatal("invalid path")
				}
				if err := os.WriteFile(rest[:end], []byte("CORRUPT_TEST_PROJECTION"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			input, _ := json.Marshal(map[string]any{"task_id": id, "block": false})
			call := tasks.ToolCall{ID: "read", Name: "TaskOutput", Input: input}
			output, err := r.ExecuteTool(t.Context(), owner, call)
			if err != nil {
				t.Fatal(err)
			}
			result := r.ToolResult(call, output, nil)
			if !gjson.GetBytes(result, "is_error").Bool() || strings.Contains(gjson.GetBytes(result, "content").String(), "Full output:") {
				t.Fatal("unverified output was advertised")
			}
			var health tasks.Health
			health.Observe(r.Snapshots(), nil)
			if health.PersistenceFailed != 1 || health.Completed != 1 || health.Failed != 0 {
				t.Fatal("missing/corrupt transcript is not separately visible", health)
			}
		})
	}
}
