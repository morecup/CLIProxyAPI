package tasks

import (
	"encoding/json"
	"os"
	"testing"
)

// golden51 is the part of testdata/task-controls-native-51.json (lane B1) the
// Go runtime can reproduce. Fields the runtime does not model are decoded so
// each case can be skipped with its divergence named instead of dropped.
type golden51 struct {
	SDK          string `json:"sdk_sha256"`
	DisplayCases []struct {
		Input, Zy, Y1 string
		ZyLength      int `json:"zy_length"`
	} `json:"display_cases"`
	TruncationCases []struct {
		Env       *string
		Effective int
		Clamp     struct{ Status string }
	} `json:"truncation_cases"`
	NotFoundCases []struct {
		Label, Input        string
		Caller              *string
		Names               map[string]string
		Tasks               []golden51Task
		Lookup              struct{ Status, Suggestion string }
		StopNotFound        string           `json:"stop_not_found"`
		OutputNotFound      string           `json:"output_not_found"`
		StopValidateInput   golden51Validate `json:"stop_validate_input"`
		OutputValidateInput golden51Validate `json:"output_validate_input"`
	} `json:"not_found_cases"`
	StopCases []struct {
		Label               string
		Input               json.RawMessage
		Caller              *string
		Names               map[string]string
		Tasks               []golden51Task
		ValidateInput       golden51Validate                `json:"validate_input"`
		CoreError           *struct{ Message, Code string } `json:"core_error"`
		ResultJSON          *string                         `json:"result_json"`
		RegistryAfterCall   []struct{ ID, Status string }   `json:"registry_after_call"`
		ToolUseErrorContent *string                         `json:"tool_use_error_content"`
	} `json:"stop_cases"`
	OutputCallCases []struct {
		Label               string
		Input               json.RawMessage
		ValidateInput       golden51Validate `json:"validate_input"`
		ToolUseErrorContent *string          `json:"tool_use_error_content"`
	} `json:"output_call_cases"`
}

type golden51Validate struct {
	Result  bool
	Message string
}

type golden51Task struct {
	ID, Type, Status string
	Description      *string
	IsBackgrounded   bool `json:"isBackgrounded"`
	IsObserver       bool `json:"isObserver"`
	AgentType        *string
	AgentID          *string  `json:"agentId"`
	KeepaliveReasons []string `json:"keepalive_reasons"`
}

func readGolden51(t *testing.T) golden51 {
	t.Helper()
	raw, err := os.ReadFile("testdata/task-controls-native-51.json")
	if err != nil {
		t.Skipf("boundary: lane B1 golden not present: %v", err)
	}
	var fixture golden51
	if json.Unmarshal(raw, &fixture) != nil || fixture.SDK != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" {
		t.Fatal("unreviewed task-controls-51 fixture")
	}
	if len(fixture.DisplayCases) != 14 || len(fixture.TruncationCases) != 12 || len(fixture.NotFoundCases) != 12 || len(fixture.StopCases) != 14 || len(fixture.OutputCallCases) != 8 {
		t.Fatal("task-controls-51 fixture case counts changed")
	}
	return fixture
}

// seedGolden51 builds the Go registry for one native synthetic registry.
// Only local_agent rows exist here; the listing helpers ignore other types
// natively as well. The Go name registry is filled in golden map order.
func seedGolden51(t *testing.T, tasks []golden51Task, names map[string]string) *Runtime {
	t.Helper()
	var rows []record
	for _, item := range tasks {
		if item.Type != "local_agent" {
			continue
		}
		row := record{ID: item.ID, Status: item.Status, Background: item.IsBackgrounded, AgentType: "general-purpose"}
		if item.Description != nil {
			row.Description = *item.Description
		}
		if item.AgentType != nil {
			row.AgentType = *item.AgentType
		}
		for name, id := range names {
			if id == item.ID {
				row.Name = name
			}
		}
		rows = append(rows, row)
	}
	return seedControlTasks(t, rows...)
}

func TestNative51DisplaySanitizers(t *testing.T) {
	for _, row := range readGolden51(t).DisplayCases {
		if got := displayID(row.Input); got != row.Zy {
			t.Errorf("Zy(%q)\ngot  %q\nwant %q", row.Input, got, row.Zy)
		} else if jsTextLength(got) != row.ZyLength {
			t.Errorf("Zy(%q) UTF-16 length %d, want %d", row.Input, jsTextLength(got), row.ZyLength)
		}
		if got := displayDescription(row.Input); got != row.Y1 {
			t.Errorf("Y1(%q)\ngot  %q\nwant %q", row.Input, got, row.Y1)
		}
	}
}

func TestNative51TruncationEnvironment(t *testing.T) {
	for _, row := range readGolden51(t).TruncationCases {
		env := "<unset>"
		configured := 0
		if row.Env != nil {
			env = *row.Env
			configured = ParseTaskOutputMaxLength(env)
		}
		r := &Runtime{options: Options{TaskOutputMaxLength: func() int { return configured }}}
		if got := r.taskOutputMaxLength(); got != row.Effective {
			t.Errorf("TASK_MAX_OUTPUT_LENGTH=%q: got %d, want %d (%s)", env, got, row.Effective, row.Clamp.Status)
		}
	}
}

func TestNative51NotFoundMessages(t *testing.T) {
	for _, row := range readGolden51(t).NotFoundCases {
		t.Run(row.Label, func(t *testing.T) {
			for _, item := range row.Tasks {
				if item.IsObserver {
					t.Skipf("boundary: observer tasks (isObserver) are not modelled by the Go registry")
				}
				if len(item.KeepaliveReasons) != 0 {
					t.Skipf("boundary: keepalive (completed + keepaliveReasons) tasks are not modelled; memo gaps 2c-3/4")
				}
			}
			r := seedGolden51(t, row.Tasks, row.Names)
			callerID := ""
			if row.Caller != nil {
				callerID = *row.Caller
			}
			if err := outputError(t, r, callerID, row.Input); err.Error() != row.OutputNotFound || err.Error() != row.OutputValidateInput.Message {
				t.Fatalf("TaskOutput not-found\ngot  %q\nwant %q", err, row.OutputNotFound)
			}
			switch {
			case row.Lookup.Suggestion != "":
				t.Skipf("boundary: TaskStop 'Did you mean' needs the unread _2e fuzzy suggestion (native %q)", row.StopNotFound)
			case row.Lookup.Status == "found":
				input, _ := json.Marshal(map[string]string{"task_id": row.Input})
				if _, err := r.ExecuteTool(t.Context(), Caller{AgentID: callerID}, ToolCall{ID: "stop", Name: "TaskStop", Input: input}); err != nil {
					t.Fatalf("native lookup found %q; Go case-folded fallback did not: %v", row.Input, err)
				}
				t.Skipf("boundary: native lookup resolves %q (Dr normalization); the c$t text %q is not reachable", row.Input, row.StopNotFound)
			}
			if err := stopError(t, r, callerID, row.Input); err.Error() != row.StopNotFound || err.Error() != row.StopValidateInput.Message {
				t.Fatalf("TaskStop not-found\ngot  %q\nwant %q", err, row.StopNotFound)
			}
		})
	}
}

func TestNative51StopCases(t *testing.T) {
	skips := map[string]string{
		"not_running_by_name_sanitized": "native qVe resolves \"wor\\u0007 ker\" to name \"wor ker\" through Dr (Cc strip, whitespace -> '-'); resolveStopLocked only case-folds",
		"not_owner_by_id":               "Go local agents own themselves (agentId == id); a synthetic foreign agentId owner is not representable",
		"not_owner_by_name":             "Go local agents own themselves (agentId == id); a synthetic foreign agentId owner is not representable",
		"not_owner_main_owned":          "agentId null ('main session' via Oye) is unreachable for owned tasks",
		"unsupported_type":              "only local_agent tasks exist in this runtime (native monitor_ws)",
		"keepalive_cascade":             "keepalive/cascade (memo gaps 2c-3/4) are not modelled",
	}
	for _, row := range readGolden51(t).StopCases {
		t.Run(row.Label, func(t *testing.T) {
			if reason, skip := skips[row.Label]; skip {
				t.Skipf("boundary: %s", reason)
			}
			r := seedGolden51(t, row.Tasks, row.Names)
			callerID := ""
			if row.Caller != nil {
				callerID = *row.Caller
				// Native ownership is lMt(caller, task.agentId). Go's owner is the
				// task itself, so a caller that natively owns the target stops
				// it as that agent; the result data does not depend on it.
				for _, item := range row.Tasks {
					if item.AgentID != nil && *item.AgentID == callerID {
						callerID = item.ID
					}
				}
			}
			data, err := r.ExecuteTool(t.Context(), Caller{AgentID: callerID}, ToolCall{ID: "toolu_stop", Name: "TaskStop", Input: row.Input})
			if !row.ValidateInput.Result {
				if err == nil || err.Error() != row.ValidateInput.Message {
					t.Fatalf("validateInput\ngot  %v\nwant %q", err, row.ValidateInput.Message)
				}
				content, isError := decodedToolResult(t, r.ToolResult(ToolCall{ID: "toolu_stop", Name: "TaskStop"}, nil, err))
				if row.ToolUseErrorContent == nil || !isError || content != *row.ToolUseErrorContent {
					t.Fatalf("tool_use_error content\ngot  %q\nwant %v", content, row.ToolUseErrorContent)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if row.ResultJSON == nil || string(data) != *row.ResultJSON {
				t.Fatalf("result_json bytes\ngot  %s\nwant %v", data, row.ResultJSON)
			}
			if content, isError := decodedToolResult(t, r.ToolResult(ToolCall{ID: "toolu_stop", Name: "TaskStop"}, data, nil)); isError || content != *row.ResultJSON {
				t.Fatalf("result block content is le(data): %q", content)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			for _, after := range row.RegistryAfterCall {
				if row := r.tasks[after.ID]; row != nil && row.Status != after.Status {
					t.Errorf("task %s status %q after stop; native %q", after.ID, row.Status, after.Status)
				}
			}
		})
	}
}

func TestNative51OutputCallValidation(t *testing.T) {
	// The native output_call registry: one running backgrounded agent
	// a1111111111111111 ("Inspect"); the caller-excluded case calls as it.
	inspect := "Inspect"
	registry := []golden51Task{{ID: "a1111111111111111", Type: "local_agent", Status: "running", Description: &inspect, IsBackgrounded: true}}
	for _, row := range readGolden51(t).OutputCallCases {
		if row.ValidateInput.Result {
			continue // data/progress/wait cases need the native output file and are covered by the §49 vectors
		}
		t.Run(row.Label, func(t *testing.T) {
			r := seedGolden51(t, registry, nil)
			callerID := ""
			if row.Label == "not_found_caller_excluded" {
				callerID = "a1111111111111111"
			}
			_, err := r.ExecuteTool(t.Context(), Caller{AgentID: callerID}, ToolCall{ID: "toolu_read", Name: "TaskOutput", Input: row.Input})
			if err == nil || err.Error() != row.ValidateInput.Message {
				t.Fatalf("validateInput\ngot  %v\nwant %q", err, row.ValidateInput.Message)
			}
			content, isError := decodedToolResult(t, r.ToolResult(ToolCall{ID: "toolu_read", Name: "TaskOutput"}, nil, err))
			if row.ToolUseErrorContent == nil || !isError || content != *row.ToolUseErrorContent {
				t.Fatalf("tool_use_error content\ngot  %q\nwant %v", content, row.ToolUseErrorContent)
			}
		})
	}
}
