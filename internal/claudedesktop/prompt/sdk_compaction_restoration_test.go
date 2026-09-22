package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func syntheticRestorationRow(name string) json.RawMessage {
	row, _ := json.Marshal(map[string]any{"type": "attachment", "attachment": map[string]string{"type": name}})
	return row
}

func syntheticRestorationOps(operation func(string, bool) ([]json.RawMessage, error)) SDKCompactionRestorationOps {
	many := func(name string) func(context.Context) ([]json.RawMessage, error) {
		return func(context.Context) ([]json.RawMessage, error) {
			rows, err := operation(name, true)
			if err == nil && (name == "derived" || name == "remote") {
				for index, row := range rows {
					var value map[string]json.RawMessage
					_ = json.Unmarshal(row, &value)
					rows[index] = value["attachment"]
				}
			}
			return rows, err
		}
	}
	one := func(name string) func(context.Context) (json.RawMessage, error) {
		return func(context.Context) (json.RawMessage, error) {
			rows, err := operation(name, false)
			if err != nil || len(rows) == 0 {
				return nil, err
			}
			return rows[0], nil
		}
	}
	return SDKCompactionRestorationOps{ReadFiles: many("files"), LocalTasks: many("tasks"), PlanFile: one("plan_file"), PlanMode: one("plan_mode"),
		InvokedSkills: one("skills"), DerivedContext: many("derived"), RemoteNotice: many("remote"), SessionStart: many("session_start"),
		WrapAttachment: func(attachment json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(map[string]any{"type": "attachment", "attachment": attachment})
		}}
}

func TestSDKCompactionRestorationNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-restoration-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Restorations []struct {
			Options struct {
				Isolated bool
				Remote   bool
				Empty    bool
				Failure  string
			}
			Result struct {
				Attachments []json.RawMessage `json:"attachments"`
				HookResults []json.RawMessage `json:"hookResults"`
			}
			Effects []string
		}
	}
	if err = json.Unmarshal(data, &vectors); err != nil || len(vectors.Restorations) != 12 {
		t.Fatalf("invalid native restoration vectors: %v", err)
	}
	for index, vector := range vectors.Restorations {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			var mu sync.Mutex
			var effects []string
			var workers sync.WaitGroup
			workers.Add(1)
			if !vector.Options.Isolated {
				workers.Add(1)
			}
			ops := syntheticRestorationOps(func(name string, many bool) ([]json.RawMessage, error) {
				if name == "files" || name == "tasks" {
					defer workers.Done()
				}
				mu.Lock()
				effects = append(effects, name)
				mu.Unlock()
				if vector.Options.Failure == name {
					return nil, errors.New("synthetic-" + name)
				}
				if vector.Options.Empty {
					return nil, nil
				}
				return []json.RawMessage{syntheticRestorationRow(name)}, nil
			})
			ops.OnSessionStart = func() { effects = append(effects, "session_start_progress") }
			result, errRestore := RestoreSDKCompactionContext(context.Background(), ops, vector.Options.Isolated, vector.Options.Remote)
			workers.Wait()
			if errRestore != nil {
				t.Fatal(errRestore)
			}
			decode := func(rows []json.RawMessage) []any {
				values := make([]any, 0, len(rows))
				for _, row := range rows {
					var value any
					if err := json.Unmarshal(row, &value); err != nil {
						t.Fatal(err)
					}
					values = append(values, value)
				}
				return values
			}
			if !reflect.DeepEqual(decode(result.Attachments), decode(vector.Result.Attachments)) || !reflect.DeepEqual(decode(result.HookResults), decode(vector.Result.HookResults)) {
				t.Fatalf("native restoration result mismatch: %#v", result)
			}
			var expected []string
			for _, effect := range vector.Effects {
				if len(effect) < len("diagnostic:") || effect[:len("diagnostic:")] != "diagnostic:" {
					expected = append(expected, effect)
				}
			}
			// Goroutine dispatch can reorder the two independent initial starts.
			// Their result ordering and the sequential stages are checked above
			// and below; a rejected Promise.all may outlive its fallback.
			sequential := func(all []string) ([]string, []string) {
				var initial, rest []string
				for _, name := range all {
					if name == "files" || name == "tasks" {
						initial = append(initial, name)
					} else {
						rest = append(rest, name)
					}
				}
				sort.Strings(initial)
				return initial, rest
			}
			a, b := sequential(effects)
			c, d := sequential(expected)
			if !reflect.DeepEqual(a, c) || !reflect.DeepEqual(b, d) {
				t.Fatalf("native restoration stage order mismatch: got %v want %v", effects, expected)
			}
			if (result.RestoreError != nil) != (vector.Options.Failure != "") || (result.FallbackError != nil) != (vector.Options.Failure == "plan_mode") {
				t.Fatalf("restoration diagnostics lost: %#v", result)
			}
		})
	}
}

func TestSDKCompactionRestorationUnknownIsNotEmptySuccess(t *testing.T) {
	if _, err := RestoreSDKCompactionContext(context.Background(), SDKCompactionRestorationOps{}, false, false); !errors.Is(err, ErrSDKCompactionRestorationUnknown) {
		t.Fatal("missing native operations became successful empty restoration")
	}
	ops := syntheticRestorationOps(func(string, bool) ([]json.RawMessage, error) { return nil, nil })
	ops.LocalTasks, ops.RemoteNotice, ops.SessionStart = nil, nil, nil
	if _, err := RestoreSDKCompactionContext(context.Background(), ops, true, true); err != nil {
		t.Fatal("isolated context incorrectly requires skipped operations")
	}
	if _, err := RestoreSDKCompactionContext(context.Background(), ops, false, false); !errors.Is(err, ErrSDKCompactionRestorationUnknown) {
		t.Fatal("main context borrowed isolated empty operations")
	}
}

func TestSDKCompactionRestorationPromiseFailureDoesNotWaitOrCancelSibling(t *testing.T) {
	filesStarted, releaseFiles, filesFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() { close(releaseFiles); <-filesFinished }()
	ops := syntheticRestorationOps(func(string, bool) ([]json.RawMessage, error) { return nil, nil })
	ops.ReadFiles = func(ctx context.Context) ([]json.RawMessage, error) {
		close(filesStarted)
		<-releaseFiles
		defer close(filesFinished)
		if ctx.Err() != nil {
			t.Error("sibling failure injected cancellation")
		}
		return nil, nil
	}
	ops.LocalTasks = func(context.Context) ([]json.RawMessage, error) {
		<-filesStarted
		return nil, errors.New("synthetic-tasks")
	}
	completed := make(chan SDKCompactionRestoration, 1)
	go func() {
		result, _ := RestoreSDKCompactionContext(context.Background(), ops, false, false)
		completed <- result
	}()
	select {
	case result := <-completed:
		if result.RestoreError == nil {
			t.Fatal("missing real restoration failure")
		}
		select {
		case <-filesFinished:
			t.Fatal("pending sibling unexpectedly finished")
		default:
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restoration waited for pending sibling after Promise.all rejection")
	}
}

func TestSDKCompactionRestorationWaitsForSessionStart(t *testing.T) {
	atHook, release := make(chan struct{}), make(chan struct{})
	ops := syntheticRestorationOps(func(string, bool) ([]json.RawMessage, error) { return nil, nil })
	ops.SessionStart = func(context.Context) ([]json.RawMessage, error) {
		close(atHook)
		<-release
		return []json.RawMessage{syntheticRestorationRow("hook")}, nil
	}
	completed := make(chan SDKCompactionRestoration, 1)
	go func() {
		result, _ := RestoreSDKCompactionContext(context.Background(), ops, false, false)
		completed <- result
	}()
	<-atHook
	select {
	case <-completed:
		t.Fatal("restoration returned before SessionStart finished")
	default:
	}
	close(release)
	if len((<-completed).HookResults) != 1 {
		t.Fatal("SessionStart messages lost")
	}
}

func TestSDKCompactionRestorationWrapsDerivedAfterRemoteNotice(t *testing.T) {
	atRemote, release := make(chan struct{}), make(chan struct{})
	ops := syntheticRestorationOps(func(name string, many bool) ([]json.RawMessage, error) {
		return []json.RawMessage{syntheticRestorationRow(name)}, nil
	})
	ops.RemoteNotice = func(context.Context) ([]json.RawMessage, error) {
		close(atRemote)
		<-release
		return []json.RawMessage{json.RawMessage(`{"type":"remote"}`)}, nil
	}
	var wrapped []string
	ops.WrapAttachment = func(attachment json.RawMessage) (json.RawMessage, error) {
		var value map[string]string
		_ = json.Unmarshal(attachment, &value)
		wrapped = append(wrapped, value["type"])
		return syntheticRestorationRow(value["type"]), nil
	}
	completed := make(chan SDKCompactionRestoration, 1)
	go func() {
		result, _ := RestoreSDKCompactionContext(context.Background(), ops, false, true)
		completed <- result
	}()
	<-atRemote
	if len(wrapped) != 0 {
		t.Error("derived attachment timestamp/UUID was created before native remote await")
	}
	close(release)
	<-completed
	if !reflect.DeepEqual(wrapped, []string{"derived", "remote"}) {
		t.Fatalf("native attachment creation order mismatch: %v", wrapped)
	}
}
