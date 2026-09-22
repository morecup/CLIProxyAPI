package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTaskOutputAndStopNativeSourceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/task-controls-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SDK   string `json:"sdk_sha256"`
		Cases []struct {
			Name, Status, Error string
			Block               bool
			Timeout             float64
			Report              *string
			Expected            json.RawMessage
			Notified            bool
		}
		StopCases []struct {
			Name, ID, Caller, Status string
			Expected                 json.RawMessage
			Error                    *struct{ Message, Code string }
		} `json:"stop_cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Cases) != 7 || len(fixture.StopCases) != 6 || fixture.SDK != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" {
		t.Fatal("unreviewed task-control fixture")
	}
	for _, vector := range fixture.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			r := newRuntime(t, Options{Execute: func(context.Context, Invocation) ([]byte, error) { return nil, ErrUnavailable }})
			row := &task{record: record{ID: "a01234567", Status: vector.Status, Description: "Inspect", Prompt: "inspect actual state", Error: vector.Error}}
			if vector.Status == "killed" {
				row.Error = context.Canceled.Error()
			}
			if vector.Report != nil {
				row.Result, _ = json.Marshal([]map[string]string{{"type": "text", "text": *vector.Report}})
			}
			r.mu.Lock()
			r.tasks[row.ID], r.order = row, []string{row.ID}
			r.mu.Unlock()
			input, _ := json.Marshal(map[string]any{"task_id": row.ID, "block": vector.Block, "timeout": vector.Timeout})
			got, err := r.ExecuteTool(t.Context(), caller(), ToolCall{ID: "read", Name: "TaskOutput", Input: input})
			if err != nil {
				t.Fatal(err)
			}
			assertControlJSON(t, got, vector.Expected)
			r.mu.Lock()
			notified := row.Notified
			r.mu.Unlock()
			if notified != vector.Notified {
				t.Fatal("terminal retrieval acknowledgment mismatch")
			}
		})
	}
	for _, vector := range fixture.StopCases {
		t.Run("stop_"+vector.Name, func(t *testing.T) {
			r := newRuntime(t, Options{Execute: func(context.Context, Invocation) ([]byte, error) { return nil, ErrUnavailable }})
			row := &task{record: record{ID: "a01234567", Status: vector.Status, Description: "Inspect", Name: "worker"}}
			r.mu.Lock()
			r.tasks[row.ID], r.order = row, []string{row.ID}
			// The name registry is a Map mirror (latest-wins binding plus
			// insertion order); the native Aus listing walks that order.
			r.registerNameLocked("worker", row.ID)
			r.mu.Unlock()
			input, _ := json.Marshal(map[string]string{"task_id": vector.ID})
			got, err := r.ExecuteTool(t.Context(), Caller{AgentID: vector.Caller}, ToolCall{ID: "stop", Name: "TaskStop", Input: input})
			if vector.Error != nil {
				if err == nil || err.Error() != vector.Error.Message {
					t.Fatalf("got %v; want %s", err, vector.Error.Message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertControlJSON(t, got, vector.Expected)
			if row.KilledBy != "parent" || row.StoppedByUser || row.UserStopCount != 0 {
				t.Fatal("tool stop became a user stop")
			}
		})
	}
}

func assertControlJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal(want, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("control result mismatch\ngot  %s\nwant %s", got, want)
	}
}

func TestTaskControlInputContracts(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `[]`, `{"task_id":null}`, `{"task_id":"x","block":null}`,
		`{"task_id":"x","block":"TRUE"}`, `{"task_id":"x","block":0}`,
		`{"task_id":"x","timeout":null}`, `{"task_id":"x","timeout":"0"}`,
		`{"task_id":"x","timeout":-1}`, `{"task_id":"x","timeout":600001}`,
		`{"task_id":"x","unknown":true}`,
	} {
		if _, err := decodeOutputInput(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"task_id":"x"}`, `{"task_id":"x","block":"true"}`} {
		value, err := decodeOutputInput(json.RawMessage(raw))
		if err != nil || !value.Block || value.Timeout != 30*time.Second {
			t.Fatal("defaults", value, err)
		}
	}
	value, err := decodeOutputInput(json.RawMessage(`{"task_id":"x","block":"false","timeout":0.5}`))
	if err != nil || value.Block || value.Timeout != 500*time.Microsecond {
		t.Fatal("native coercion/fraction", value, err)
	}
	value, err = decodeOutputInput(json.RawMessage(`{"task_id":"x","timeout":600000}`))
	if err != nil || value.Timeout != 10*time.Minute {
		t.Fatal("upper bound", value, err)
	}
	r := newRuntime(t, Options{Execute: func(context.Context, Invocation) ([]byte, error) { return reply("done"), nil }})
	for _, raw := range []string{`{}`, `{"task_id":"","shell_id":"x"}`, `{"task_id":null}`, `{"shell_id":2}`, `{"task_id":"x","unknown":1}`} {
		if _, err := r.ExecuteTool(t.Context(), caller(), ToolCall{ID: "stop", Name: "TaskStop", Input: json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted stop %s", raw)
		}
	}
}

func TestTaskOutputWaitCancellationAndQueryRetirement(t *testing.T) {
	for _, mode := range []string{"completion", "caller-cancel", "retirement", "snapshot", "zero-wait"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan Invocation, 1), make(chan struct{})
			r := newRuntime(t, Options{Execute: func(ctx context.Context, v Invocation) ([]byte, error) {
				entered <- v
				select {
				case <-release:
					return reply("actual completion"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}})
			id := launch(t, r, caller(), "agent", "")
			await(t, entered)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			input := map[string]any{"task_id": id}
			if mode == "snapshot" {
				input["block"] = "false"
			}
			if mode == "zero-wait" {
				input["timeout"] = 0
			}
			raw, _ := json.Marshal(input)
			type outcome struct {
				data json.RawMessage
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				data, err := r.ExecuteTool(ctx, caller(), ToolCall{ID: "read", Name: "TaskOutput", Input: raw})
				done <- outcome{data, err}
			}()
			if mode == "completion" || mode == "caller-cancel" || mode == "retirement" {
				select {
				case result := <-done:
					t.Fatalf("wait returned early: %s %v", result.data, result.err)
				case <-time.After(15 * time.Millisecond):
				}
			}
			switch mode {
			case "completion":
				close(release)
			case "caller-cancel":
				cancel()
			case "retirement":
				r.Close()
			}
			result := await(t, done)
			if mode == "caller-cancel" {
				if !errors.Is(result.err, context.Canceled) {
					t.Fatal(result.err)
				}
				return
			}
			if mode == "retirement" {
				if !errors.Is(result.err, ErrUnavailable) {
					t.Fatal(result.err)
				}
				return
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			var decoded outputResult
			if json.Unmarshal(result.data, &decoded) != nil {
				t.Fatal("result decode")
			}
			want := "success"
			if mode == "snapshot" {
				want = "not_ready"
			}
			if mode == "zero-wait" {
				want = "timeout"
			}
			if decoded.RetrievalStatus != want || decoded.Task.TaskID != id {
				t.Fatal(string(result.data))
			}
			if mode == "completion" && decoded.Task.Output != "actual completion" {
				t.Fatal("lost report")
			}
		})
	}
}

func TestTaskStopModelCanResumeUserStopCannotAndPersists(t *testing.T) {
	for _, userStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "model", true: "user"}[userStop], func(t *testing.T) {
			store := &memoryStore{}
			entered, exited := make(chan Invocation, 4), make(chan struct{}, 4)
			options := Options{Store: store, Execute: func(ctx context.Context, v Invocation) ([]byte, error) {
				entered <- v
				<-ctx.Done()
				exited <- struct{}{}
				return nil, ctx.Err()
			}}
			r := newRuntime(t, options)
			owner := caller()
			id := launch(t, r, owner, "launch", `,"name":"worker"`)
			await(t, entered)
			if userStop {
				if err := r.Stop(id, true); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := r.ExecuteTool(t.Context(), owner, ToolCall{ID: "stop", Name: "TaskStop", Input: json.RawMessage(`{"shell_id":"WORKER"}`)})
				if err != nil || !strings.Contains(string(data), "Successfully stopped task") {
					t.Fatal(string(data), err)
				}
			}
			await(t, exited)
			eventually(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.tasks[id].active == nil })
			r.Close()
			restored := newRuntime(t, options)
			owner.PromptID = uuid.NewString()
			result := send(t, restored, owner, "worker", "resume actual task")
			if userStop {
				if !strings.Contains(string(result), "stopped by the user") {
					t.Fatal(string(result))
				}
				if len(entered) != 0 {
					t.Fatal("user-stopped generation resumed")
				}
			} else {
				next := await(t, entered)
				if next.AgentID != id || next.Kind != "resume" {
					t.Fatal("model-stop lost resumability")
				}
				restored.mu.Lock()
				if row := restored.tasks[id]; row.KilledBy != "" || row.Error != "" {
					t.Error("old terminal reason leaked into resume")
				}
				restored.mu.Unlock()
			}
		})
	}
}

func TestTaskStopPersistenceFailureDoesNotCancelOrClaimSuccess(t *testing.T) {
	store := &memoryStore{}
	entered := make(chan Invocation, 1)
	r := newRuntime(t, Options{Store: store, Execute: func(ctx context.Context, v Invocation) ([]byte, error) {
		entered <- v
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	id := launch(t, r, caller(), "launch", "")
	await(t, entered)
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	raw, _ := json.Marshal(map[string]string{"task_id": id})
	if _, err := r.ExecuteTool(t.Context(), caller(), ToolCall{ID: "stop", Name: "TaskStop", Input: raw}); err == nil {
		t.Fatal("failed commit claimed success")
	}
	r.mu.Lock()
	if row := r.tasks[id]; row.Status != "running" || row.KilledBy != "" || !row.persistenceFailed {
		t.Error("failed stop mutated state")
	}
	r.mu.Unlock()
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
}
