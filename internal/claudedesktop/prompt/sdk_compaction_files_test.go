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

	"github.com/tidwall/gjson"
)

func TestSDKCompactionFileRestorationNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-restoration-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		NoContent []struct {
			Text     string
			Expected bool
		}
		Files []struct {
			Name                string
			State               json.RawMessage
			Preserved           []json.RawMessage
			ExcludeSessionReads bool
			NullReads           []string
			Costs               map[string]int64
			Options             struct {
				MemoryPaths      []string
				SessionReadPaths []string
				MemoryPathError  bool
				SessionReadError bool
				PlanPathError    bool
			}
			Expected []json.RawMessage
			Error    string
			Effects  []struct{ Read string }
		}
	}
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors.Files) != 14 || len(vectors.NoContent) != 9 {
		t.Fatalf("invalid native file vectors: %v", err)
	}
	for index, vector := range vectors.NoContent {
		if SDKReadResultHasNoContent(vector.Text) != vector.Expected {
			t.Fatalf("no-content result differs from native $7 at vector %d", index)
		}
	}
	for _, vector := range vectors.Files {
		t.Run(vector.Name, func(t *testing.T) {
			var files []SDKCompactionReadFile
			gjson.ParseBytes(vector.State).ForEach(func(key, value gjson.Result) bool {
				files = append(files, SDKCompactionReadFile{Filename: key.String(), Timestamp: value.Get("timestamp").Float()})
				return true
			})
			var mu sync.Mutex
			var reads []string
			ops := SDKCompactionFileRestoreOps{
				NormalizePath: func(path string) (string, error) { return path, nil },
				MemoryPaths: func() ([]string, error) {
					if vector.Options.MemoryPathError {
						return nil, errors.New("synthetic-memory-path")
					}
					return vector.Options.MemoryPaths, nil
				},
				SessionReadPaths: func(context.Context) ([]string, error) {
					if vector.Options.SessionReadError {
						return nil, errors.New("synthetic-session-read")
					}
					return vector.Options.SessionReadPaths, nil
				},
				PlanPath: func() (string, error) {
					if vector.Options.PlanPathError {
						return "", errors.New("synthetic-plan-path")
					}
					return "synthetic-plan", nil
				},
				ReadFile: func(ctx context.Context, path string, options SDKCompactionFileReadOptions) (json.RawMessage, error) {
					if options.MaxTokens != 5000 || options.Source != "compact" || options.SuccessEvent != "tengu_post_compact_file_restore_success" || options.ErrorEvent != "tengu_post_compact_file_restore_error" {
						t.Error("incorrect native restore reader options")
					}
					mu.Lock()
					reads = append(reads, path)
					mu.Unlock()
					for _, missing := range vector.NullReads {
						if missing == path {
							return nil, nil
						}
					}
					return json.Marshal(map[string]string{"type": "file", "filename": path})
				},
				WrapAttachment: func(attachment json.RawMessage) (json.RawMessage, error) {
					return json.Marshal(map[string]any{"type": "attachment", "attachment": attachment})
				},
				WrappedTokens: func(message json.RawMessage) (int64, error) {
					if value, known := vector.Costs[gjson.GetBytes(message, "attachment.filename").String()]; known {
						return value, nil
					}
					return 1, nil
				},
			}
			actual, errRestore := RestoreSDKCompactionFiles(context.Background(), files, vector.Preserved, "Read", vector.ExcludeSessionReads, ops)
			if (errRestore != nil) != (vector.Error != "") || (errRestore != nil && errRestore.Error() != vector.Error) {
				t.Fatalf("file restoration error: got %v want %s", errRestore, vector.Error)
			}
			var wantReads []string
			for _, effect := range vector.Effects {
				wantReads = append(wantReads, effect.Read)
			}
			sort.Strings(reads)
			sort.Strings(wantReads)
			if !reflect.DeepEqual(reads, wantReads) {
				t.Fatalf("different candidate reads: got %v want %v", reads, wantReads)
			}
			if len(actual) != len(vector.Expected) {
				t.Fatalf("different retained attachments: got %d want %d", len(actual), len(vector.Expected))
			}
			for index := range actual {
				var got, want any
				if json.Unmarshal(actual[index], &got) != nil || json.Unmarshal(vector.Expected[index], &want) != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("native file attachment/order mismatch at %d", index)
				}
			}
		})
	}
}

func TestSDKCompactionFileRestorationRequiresOwnedOperations(t *testing.T) {
	if _, err := RestoreSDKCompactionFiles(context.Background(), nil, nil, "Read", false, SDKCompactionFileRestoreOps{}); !errors.Is(err, ErrSDKCompactionFilesUnknown) {
		t.Fatal("empty file state hid unconfigured restoration operations")
	}
	if _, err := SDKPreservedReadPaths(nil, "Read", nil); !errors.Is(err, ErrSDKCompactionFilesUnknown) {
		t.Fatal("missing path ownership accepted")
	}
	want := errors.New("synthetic-path")
	message := json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","id":"read","input":{"file_path":"synthetic"}}]}}`)
	if _, err := SDKPreservedReadPaths([]json.RawMessage{message}, "Read", func(string) (string, error) { return "", want }); !errors.Is(err, want) {
		t.Fatal("normalization failure silently created a retained path")
	}
}

func TestSDKCompactionRestorationContentIsNotSerialized(t *testing.T) {
	result := SDKCompactionRestoration{Attachments: []json.RawMessage{syntheticRestorationRow("private")},
		HookResults: []json.RawMessage{syntheticRestorationRow("private-hook")}, RestoreError: errors.New("private diagnostic")}
	encoded, err := json.Marshal(result)
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("request-local restoration leaked through default JSON encoding: %s %v", encoded, err)
	}
}
