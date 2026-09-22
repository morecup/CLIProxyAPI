package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memoryStore struct {
	mu       sync.Mutex
	raw      []byte
	revision string
	fail     bool
}

func (s *memoryStore) Load(string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.raw), s.revision, nil
}
func (s *memoryStore) Save(_ string, revision string, raw []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail || revision != s.revision {
		return "", errors.New("synthetic commit failure")
	}
	s.raw, s.revision = bytes.Clone(raw), uuid.NewString()
	return s.revision, nil
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("task transition did not complete")
		var zero T
		return zero
	}
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !f() {
		select {
		case <-deadline:
			t.Fatal("task state did not settle")
		case <-time.After(time.Millisecond):
		}
	}
}
func caller() Caller { return Caller{PromptID: uuid.NewString(), Model: "claude-sonnet-5"} }
func reply(text string) []byte {
	raw, _ := json.Marshal(map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": text}},
		"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 3, "output_tokens": 2}})
	return raw
}
func newRuntime(t *testing.T, options Options) *Runtime {
	t.Helper()
	if options.Store == nil {
		options.Store = &memoryStore{}
	}
	options.Scope = "synthetic-session"
	if options.ResolveModel == nil {
		options.ResolveModel = func(parent, selected, kind string) (string, error) { return parent, nil }
	}
	r, err := New(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}
func launch(t *testing.T, r *Runtime, owner Caller, id, extra string) string {
	t.Helper()
	data, err := r.ExecuteTool(t.Context(), owner, ToolCall{ID: id, Name: "Agent", Input: json.RawMessage(`{"description":"Inspect","prompt":"inspect actual state"` + extra + `}`)})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID string `json:"agentId"`
	}
	if json.Unmarshal(data, &result) != nil || result.ID == "" {
		t.Fatalf("launch result: %s", data)
	}
	return result.ID
}
func send(t *testing.T, r *Runtime, owner Caller, to, message string) json.RawMessage {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"to": to, "message": message})
	data, err := r.ExecuteTool(t.Context(), owner, ToolCall{ID: uuid.NewString(), Name: "SendMessage", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRuntimeRunsToolsQueuesMessagesAndResumesCompletedGeneration(t *testing.T) {
	invocations := make(chan Invocation, 8)
	responses := make(chan []byte, 8)
	events := make(chan Event, 16)
	r := newRuntime(t, Options{Execute: func(ctx context.Context, invocation Invocation) ([]byte, error) {
		invocations <- invocation
		select {
		case response := <-responses:
			return response, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, Observe: func(_ context.Context, event Event) error { events <- event; return nil }})
	owner := caller()
	id := launch(t, r, owner, "tool1", `,"name":"worker"`)
	first := await(t, invocations)
	if first.AgentID != id || first.ParentPromptID != owner.PromptID || first.PromptID == owner.PromptID || first.Depth != 1 {
		t.Fatal("child ownership lost", first)
	}
	if !r.Snapshots()[0].Background {
		t.Fatal("Agent did not default to background")
	}
	if !strings.Contains(string(send(t, r, owner, "worker", "queued message")), "queued") {
		t.Fatal("live send did not queue")
	}
	responses <- []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"missing","name":"UnownedShell","input":{}}],"stop_reason":"tool_use"}`)
	second := await(t, invocations)
	wire, _ := json.Marshal(second.Messages)
	if !bytes.Contains(wire, []byte(`tool_result`)) || !bytes.Contains(wire, []byte(`is_error`)) || !bytes.Contains(wire, []byte(`queued message`)) || first.PromptID != second.PromptID {
		t.Fatal("tool result/live message did not reach continued inference", string(wire))
	}
	responses <- reply("generation one")
	eventually(t, func() bool { return r.Snapshots()[0].Status == "completed" })
	send(t, r, owner, id, "resume message")
	third := await(t, invocations)
	wire, _ = json.Marshal(third.Messages)
	if third.AgentID != id || third.Kind != "resume" || third.PromptID == first.PromptID || !bytes.Contains(wire, []byte("generation one")) || !bytes.Contains(wire, []byte("resume message")) {
		t.Fatal("completed task did not resume actual history")
	}
	responses <- reply("generation two")
	for i, want := range []string{"started", "finished", "started", "finished"} {
		event := await(t, events)
		if event.Kind != want || event.Generation != uint64(i/2+1) {
			t.Fatal("transition order/generation lost", event)
		}
	}
}

func TestRuntimeOrdersTransitionsAcrossBlockedDeliveryAndImmediateResume(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	events := make(chan Event, 16)
	r := newRuntime(t, Options{Execute: func(context.Context, Invocation) ([]byte, error) { return reply("actual result"), nil },
		Observe: func(ctx context.Context, event Event) error {
			if event.Kind == "started" && event.Generation == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			events <- event
			return nil
		}})
	owner := caller()
	id := launch(t, r, owner, "original", `,"run_in_background":false`)
	await(t, entered)
	// Delivery is blocked, but state inspection and resume admission must work.
	if r.Snapshots()[0].Status != "completed" {
		t.Fatal("foreground did not execute")
	}
	send(t, r, owner, id, "next")
	eventually(t, func() bool { return r.Snapshots()[0].Generation == 2 && r.Snapshots()[0].Status == "completed" })
	once.Do(func() { close(release) })
	for i, want := range []string{"started", "finished", "started", "finished"} {
		event := await(t, events)
		if event.Kind != want || event.Generation != uint64(i/2+1) {
			t.Fatal("event overtook its predecessor", event)
		}
	}
	eventually(t, func() bool { return r.Snapshots()[0].PendingEvents == 0 })
}

func TestRuntimeNamesDedupUserStopAndRetirement(t *testing.T) {
	started := make(chan Invocation, 8)
	exited := make(chan struct{}, 8)
	r := newRuntime(t, Options{Execute: func(ctx context.Context, invocation Invocation) ([]byte, error) {
		started <- invocation
		<-ctx.Done()
		exited <- struct{}{}
		return nil, ctx.Err()
	}})
	owner := caller()
	first := launch(t, r, owner, "first", `,"name":"same"`)
	await(t, started)
	if duplicate := launch(t, r, owner, "first", `,"name":"same"`); duplicate != first || len(started) != 0 {
		t.Fatal("tool retry launched another task")
	}
	second := launch(t, r, owner, "second", `,"name":"same"`)
	await(t, started)
	send(t, r, owner, "same", "latest")
	send(t, r, owner, first, "old ID")
	states := r.Snapshots()
	if states[0].Queued != 1 || states[1].Queued != 1 {
		t.Fatal("name/raw-ID resolution lost", states)
	}
	if err := r.Stop(first, true); err != nil {
		t.Fatal(err)
	}
	await(t, exited)
	if data := send(t, r, owner, first, "must not resume"); !strings.Contains(string(data), "stopped by the user") {
		t.Fatal("user stop bypassed", string(data))
	}
	r.Close()
	await(t, exited)
	states = r.Snapshots()
	if states[0].Generation != 1 || states[0].Status != "killed" || states[1].ID != second || states[1].Status != "killed" {
		t.Fatal("retirement did not join children", states)
	}
}

func TestRuntimePersistsFailedDeliveryWithoutRebindingOldQuery(t *testing.T) {
	store := &memoryStore{}
	failure := errors.New("synthetic unavailable worker")
	r := newRuntime(t, Options{Store: store, DeliveryScope: "old-query", Execute: func(context.Context, Invocation) ([]byte, error) { return reply("retained"), nil },
		Observe: func(context.Context, Event) error { return failure }})
	launch(t, r, caller(), "task", `,"run_in_background":false`)
	eventually(t, func() bool { return r.Snapshots()[0].DeliveryFailed })
	r.Close()
	old, _, _ := store.Load("")
	if !bytes.Contains(old, []byte(`"outbox"`)) || !bytes.Contains(old, []byte(`"delivery_failed":true`)) {
		t.Fatal("failed obligation was not durable")
	}
	var mu sync.Mutex
	var delivered []Event
	restored := newRuntime(t, Options{Store: store, DeliveryScope: "new-query", Execute: func(context.Context, Invocation) ([]byte, error) { return reply("new"), nil },
		Observe: func(_ context.Context, event Event) error {
			mu.Lock()
			delivered = append(delivered, event)
			mu.Unlock()
			return nil
		}})
	state := restored.Snapshots()[0]
	if !state.DeliveryFailed || state.PendingEvents != 2 || state.Status != "completed" {
		t.Fatal("restart lost obligation", state)
	}
	newID := launch(t, restored, caller(), "new", `,"run_in_background":false`)
	eventually(t, func() bool { return restored.Snapshots()[1].PendingEvents == 0 })
	mu.Lock()
	defer mu.Unlock()
	for _, event := range delivered {
		if event.TaskID != newID {
			t.Fatal("old event rebound to new query", event)
		}
	}
	if len(delivered) != 2 {
		t.Fatal("new query delivery missing", len(delivered))
	}
}

func TestRuntimeRejectsUncommittedAndStaleAdmission(t *testing.T) {
	store := &memoryStore{fail: true}
	r := newRuntime(t, Options{Store: store, Execute: func(context.Context, Invocation) ([]byte, error) { return nil, errors.New("must not execute") }})
	_, err := r.ExecuteTool(t.Context(), caller(), ToolCall{ID: "no-commit", Name: "Agent", Input: json.RawMessage(`{"description":"d","prompt":"p"}`)})
	if err == nil || len(r.Snapshots()) != 0 {
		t.Fatal("failed commit admitted task")
	}
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	other := newRuntime(t, Options{Store: store, Execute: func(context.Context, Invocation) ([]byte, error) { return reply("actual"), nil }})
	launch(t, other, caller(), "committed", `,"run_in_background":false`)
	_, err = r.ExecuteTool(t.Context(), caller(), ToolCall{ID: "stale", Name: "Agent", Input: json.RawMessage(`{"description":"d","prompt":"p"}`)})
	if err == nil || len(r.Snapshots()) != 0 {
		t.Fatal("stale owner admitted task")
	}
}

func TestParseResponseValidatesWholeBatchBeforeExecution(t *testing.T) {
	for _, content := range []string{
		`[{"type":"tool_use","id":"one","name":"Agent","input":{}},{"type":"tool_use","id":"one","name":"Agent","input":{}}]`,
		`[{"type":"tool_use","id":"one","name":"Agent","input":null}]`,
		`[]`,
	} {
		if _, _, err := ParseResponse([]byte(fmt.Sprintf(`{"role":"assistant","content":%s,"stop_reason":"tool_use"}`, content))); err == nil {
			t.Fatal("invalid tool batch accepted")
		}
	}
}
