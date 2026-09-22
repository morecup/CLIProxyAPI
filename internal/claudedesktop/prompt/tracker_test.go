package prompt

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var baseTime = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func input(id string) Input {
	return Input{AccountID: "account-a", SessionID: "session-a", PromptID: "11111111-1111-4111-8111-111111111111", ClientRequestID: id, Role: "main", Body: []byte(`{"messages":[{"role":"user","content":"synthetic prompt"}]}`), StartedAt: baseTime, Attempt: 1}
}

func continuation(id, toolID string) Input {
	i := input(id)
	i.Body = []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"synthetic result"}]}]}`, toolID))
	i.StartedAt = baseTime.Add(5 * time.Second)
	return i
}

func TestToolContinuationOwnsOnePrompt(t *testing.T) {
	var tracker Tracker
	first := tracker.Begin(input("request-1"))
	first.FinishSuccess(baseTime.Add(2*time.Second), "tool_use", []string{"tool-1"})
	if first.CompletedPrompt() || first.Snapshot().PendingTools != 1 {
		t.Fatal("tool_use completed the prompt")
	}
	next := tracker.Begin(continuation("request-2", "tool-1"))
	a, b := first.Identity(), next.Identity()
	if a.PromptID != b.PromptID || a.QueryChainID != b.QueryChainID || b.QueryDepth != 1 || b.StartsPrompt {
		t.Fatalf("continuation identity: %#v -> %#v", a, b)
	}
	next.FinishSuccess(baseTime.Add(7*time.Second), "end_turn", nil)
	s := next.Snapshot()
	if !next.CompletedPrompt() || !s.Complete || s.APICalls != 2 || s.APIDuration != 4*time.Second || s.ToolRequests != 1 || s.ToolResults != 1 || s.PendingTools != 0 {
		t.Fatalf("prompt result: %#v", s)
	}
	if first.CompletedPrompt() {
		t.Fatal("older request acquired completion ownership")
	}
	third := tracker.Begin(input("request-3"))
	if third.Identity().PromptID == a.PromptID || !third.Identity().StartsPrompt || third.Identity().QueryDepth != 0 {
		t.Fatal("new human prompt inherited the old loop")
	}
}

func TestRetriesPreserveCallAndDoNotDoubleCount(t *testing.T) {
	var tracker Tracker
	i := input("request-1")
	first := tracker.Begin(i)
	first.FinishFailure()
	i.Attempt = 2
	i.StartedAt = baseTime.Add(3 * time.Second)
	i.Body = []byte(`{"speed":"standard","model":"different-model","messages":[{"content":"synthetic prompt","role":"user"}]}`)
	retry := tracker.Begin(i)
	if retry.Identity().PromptID != first.Identity().PromptID || retry.Identity().QueryDepth != 0 || retry.Identity().StartsPrompt {
		t.Fatal("retry changed prompt/call identity")
	}
	first.FinishSuccess(baseTime.Add(4*time.Second), "end_turn", nil)
	if retry.Snapshot().Complete {
		t.Fatal("late failed attempt closed current retry")
	}
	retry.FinishSuccess(baseTime.Add(6*time.Second), "end_turn", nil)
	retry.FinishSuccess(baseTime.Add(8*time.Second), "end_turn", nil)
	if s := retry.Snapshot(); !s.Complete || s.APICalls != 1 || s.APIDuration != 6*time.Second {
		t.Fatalf("retry accounting: %#v", s)
	}
}

func TestChangedRetryInputCannotMergePrompts(t *testing.T) {
	var tracker Tracker
	i := input("same-request")
	old := tracker.Begin(i)
	old.FinishFailure()
	i.Attempt = 2
	i.Body = []byte(`{"messages":[{"role":"user","content":"different human prompt"}]}`)
	next := tracker.Begin(i)
	if next.Identity().PromptID == old.Identity().PromptID {
		t.Fatal("changed message history inherited old prompt")
	}
}

func TestContinuationCannotCrossAccountSessionOrHelper(t *testing.T) {
	for _, change := range []string{"account", "session", "helper"} {
		t.Run(change, func(t *testing.T) {
			var tracker Tracker
			first := tracker.Begin(input("first"))
			first.FinishSuccess(baseTime.Add(time.Second), "tool_use", []string{"shared-tool-id"})
			i := continuation("next", "shared-tool-id")
			switch change {
			case "account":
				i.AccountID = "account-b"
			case "session":
				i.SessionID = "session-b"
			case "helper":
				i.Role = "title"
			}
			next := tracker.Begin(i)
			if change == "helper" {
				if next != nil {
					t.Fatal("helper entered main prompt tracker")
				}
				return
			}
			next.FinishSuccess(baseTime.Add(6*time.Second), "end_turn", nil)
			if next.Snapshot().Complete || next.Identity().QueryChainID == first.Identity().QueryChainID || next.Snapshot().IncompleteReason != "unobserved-tool-owner" {
				t.Fatal("foreign continuation was considered complete")
			}
		})
	}
}

func TestParallelToolResultsMustResolveAllPendingIDs(t *testing.T) {
	var tracker Tracker
	first := tracker.Begin(input("first"))
	first.FinishSuccess(baseTime.Add(time.Second), "tool_use", []string{"a", "b"})
	next := tracker.Begin(continuation("second", "a"))
	next.FinishSuccess(baseTime.Add(6*time.Second), "end_turn", nil)
	if next.CompletedPrompt() || next.Snapshot().PendingTools != 1 {
		t.Fatal("partial tool results completed the loop")
	}
	last := tracker.Begin(continuation("third", "b"))
	last.FinishSuccess(baseTime.Add(8*time.Second), "end_turn", nil)
	if !last.CompletedPrompt() || last.Snapshot().ToolResults != 2 {
		t.Fatal("remaining result did not complete loop")
	}
}

func TestUnprovableInputAndOutcomesStayIncomplete(t *testing.T) {
	for _, test := range []struct {
		name, body, stop string
		tools            []string
	}{
		{"mixed", `{"messages":[{"role":"user","content":[{"type":"text","text":"new prompt"},{"type":"tool_result","tool_use_id":"tool"}]}]}`, "end_turn", nil},
		{"duplicate results", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool"},{"type":"tool_result","tool_use_id":"tool"}]}]}`, "end_turn", nil},
		{"missing user", `{"messages":[]}`, "end_turn", nil},
		{"missing tool", "", "tool_use", nil},
		{"duplicate tool", "", "tool_use", []string{"tool", "tool"}},
		{"empty tool id", "", "tool_use", []string{""}},
		{"unknown stop", "", "", nil},
		{"server tool pause", "", "pause_turn", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var tracker Tracker
			i := input("request")
			if test.body != "" {
				i.Body = []byte(test.body)
			}
			r := tracker.Begin(i)
			r.FinishSuccess(baseTime.Add(time.Second), test.stop, test.tools)
			if s := r.Snapshot(); s.Complete || s.IncompleteReason == "" {
				t.Fatalf("unproven outcome: %#v", s)
			}
		})
	}
}

func TestExpiredAndConcurrentOwnersAreNotGuessed(t *testing.T) {
	var tracker Tracker
	first := tracker.Begin(input("first"))
	first.FinishSuccess(baseTime.Add(time.Second), "tool_use", []string{"tool"})
	i := continuation("second", "tool")
	i.StartedAt = baseTime.Add(2 * time.Hour)
	next := tracker.Begin(i)
	if next.Snapshot().IncompleteReason != "unobserved-tool-owner" {
		t.Fatal("expired owner resurrected")
	}
	var concurrent Tracker
	for n := range 2 {
		r := concurrent.Begin(input(fmt.Sprint(n)))
		r.FinishSuccess(baseTime.Add(time.Second), "tool_use", []string{"tool"})
	}
	ambiguous := concurrent.Begin(continuation("ambiguous", "tool"))
	if ambiguous.Snapshot().IncompleteReason != "unobserved-tool-owner" {
		t.Fatal("ambiguous owner was guessed")
	}
}

func TestTrackerConcurrentAccounts(t *testing.T) {
	var tracker Tracker
	var wg sync.WaitGroup
	for n := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i := input("request")
			i.AccountID = fmt.Sprint(n)
			r := tracker.Begin(i)
			r.FinishSuccess(baseTime.Add(time.Second), "end_turn", nil)
			if !r.CompletedPrompt() {
				t.Error("account prompt did not complete")
			}
		}()
	}
	wg.Wait()
}

func TestTerminalFailureCannotRaceASuccessfulRetry(t *testing.T) {
	var tracker Tracker
	i := input("request")
	first := tracker.Begin(i)
	first.FinishFailure()
	i.Attempt = 2
	retry := tracker.Begin(i)
	if first.FinalizeFailure(baseTime.Add(time.Second)) {
		t.Fatal("obsolete failure closed newer attempt")
	}
	if !retry.ClaimControlInput() || retry.ClaimControlInput() {
		t.Fatal("retry did not claim exactly one human-input event")
	}
	retry.FinishSuccess(baseTime.Add(2*time.Second), "end_turn", nil)
	if retry.FinalizeFailure(baseTime.Add(3 * time.Second)) {
		t.Fatal("late failure overrode success")
	}
	if !retry.Snapshot().Complete || retry.Snapshot().Failed {
		t.Fatal("retry result lost")
	}
	third := tracker.Begin(input("new-request"))
	third.FinishFailure()
	if !third.FinalizeFailure(baseTime.Add(4*time.Second)) || third.FinalizeFailure(baseTime.Add(5*time.Second)) {
		t.Fatal("terminal failure was not exactly once")
	}
	if !third.Snapshot().Failed || third.Snapshot().Complete {
		t.Fatal("terminal failure reported as success")
	}
}

func TestResponseNeedsCompleteMessage(t *testing.T) {
	var response Response
	response.ObserveStreamLine([]byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"tool"}}`))
	response.ObserveStreamLine([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`))
	if _, _, complete := response.Outcome(); complete {
		t.Fatal("message_delta prematurely completed stream")
	}
	response.ObserveStreamLine([]byte("data: [DONE]"))
	if _, _, complete := response.Outcome(); complete {
		t.Fatal("foreign protocol DONE completed stream")
	}
	response.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
	stop, ids, complete := response.Outcome()
	if stop != "tool_use" || !complete || len(ids) != 1 || ids[0] != "tool" {
		t.Fatal("stream response facts missing")
	}
	for _, streaming := range []bool{false, true} {
		var r Response
		body := `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"must not be retained"}]}`
		if streaming {
			body = "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n"
		}
		r.ObservePayload([]byte(body), streaming)
		if stop, _, complete := r.Outcome(); stop != "end_turn" || !complete {
			t.Fatal("buffered payload did not complete")
		}
		if strings.Contains(fmt.Sprintf("%+v", &r), "must not be retained") {
			t.Fatal("response retained content")
		}
	}
}
