package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func beginSDKToolBatch(t *testing.T, tracker *Tracker, ids ...string) *Request {
	t.Helper()
	r := tracker.Begin(input("tools"))
	r.ObserveSDKQuery()
	tools := make([]ToolObservation, len(ids))
	for index, id := range ids {
		tools[index] = ToolObservation{ID: id, Name: "Read"}
	}
	r.ObserveSDKAssistantMessage(baseTime.Add(time.Second), tools)
	r.FinishSuccess(baseTime.Add(2*time.Second), "tool_use", ids)
	r.RecordSDKAPISuccess(2000)
	return r
}

func TestSDKToolResultYieldsAreIndependentOfWireRowGrouping(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			var tracker Tracker
			first := beginSDKToolBatch(t, &tracker, "a", "b", "c")
			blocks := []string{
				`{"type":"tool_result","tool_use_id":"a","content":"PRIVATE_RESULT"}`,
				`{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"PRIVATE_ERROR"}`,
				`{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"PRIVATE_NESTED"}]}`,
			}
			rows := []string{`{"role":"user","content":[` + strings.Join(blocks, ",") + `]}`}
			if split {
				rows = nil
				for _, block := range blocks {
					rows = append(rows, `{"role":"user","content":[`+block+`]}`)
				}
			}
			i := input("results")
			i.Body = []byte(`{"messages":[` + strings.Join(rows, ",") + `]}`)
			i.StartedAt = baseTime.Add(4 * time.Second)
			next := tracker.Begin(i)
			if next.Identity().StartsPrompt || next.Identity().PromptID != first.Identity().PromptID {
				t.Fatal("pure tool-result tail lost its unique owner")
			}
			if before := next.Snapshot().SDK; before.NumTurns != 4 || before.ToolResultUserYields != 3 || before.Queries != 1 {
				t.Fatalf("results were not yielded before the next query: %+v", before)
			}
			next.ObserveSDKQuery()
			next.FinishFailure()
			i.Attempt = 2
			retry := tracker.Begin(i)
			retry.ObserveSDKQuery()
			retry.ObserveSDKAssistantMessage(baseTime.Add(5*time.Second), nil)
			retry.FinishSuccess(baseTime.Add(6*time.Second), "end_turn", nil)
			retry.RecordSDKAPISuccess(2000)
			s := retry.SDKResultSnapshot()
			if !s.CompleteFacts || s.NumTurns != 4 || s.ToolResultUserYields != 3 || s.InterruptionUserYields != 0 || s.Queries != 2 || s.ToolUseCount != 3 || s.APIDurationMS != 4000 {
				t.Fatalf("canonical result accounting: %+v", s)
			}
			if !s.SawRetry || !retry.CompletedPrompt() || retry.Snapshot().ToolResults != 3 {
				t.Fatal("retry or completion ownership changed")
			}
		})
	}
}

func TestSDKCancellationAfterReturnedToolsDoesNotInventPendingDrain(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(fmt.Sprint(unresolved), func(t *testing.T) {
			var tracker Tracker
			ids := []string{"a"}
			if unresolved {
				ids = append(ids, "b")
			}
			beginSDKToolBatch(t, &tracker, ids...)
			next := tracker.Begin(continuation("results", "a"))
			next.ObserveSDKQuery()
			next.ObserveSDKAssistantMessage(baseTime.Add(6*time.Second), nil)
			if !next.FinalizeCancellation(baseTime.Add(7*time.Second)) || next.FinalizeCancellation(baseTime.Add(8*time.Second)) {
				t.Fatal("cancellation yield is not exactly once")
			}
			s := next.SDKResultSnapshot()
			if s.NumTurns != 3 || s.ToolResultUserYields != 1 || s.InterruptionUserYields != 1 || !s.CancelledStreaming || s.APIDurationMS != 2000 {
				t.Fatalf("cancellation accounting: %+v", s)
			}
			if s.CompleteFacts == unresolved || unresolved && s.IncompleteReason != "unobserved-sdk-user-yields" {
				t.Fatalf("actual unresolved tool drain was misclassified: %+v", s)
			}
		})
	}
}

func TestSDKReturnedToolIDCanYieldAgainInALaterOccurrence(t *testing.T) {
	var tracker Tracker
	beginSDKToolBatch(t, &tracker, "reused")
	next := tracker.Begin(continuation("second", "reused"))
	next.ObserveSDKQuery()
	next.ObserveSDKAssistantMessage(baseTime.Add(6*time.Second), []ToolObservation{{ID: "reused", Name: "Read"}})
	next.FinishSuccess(baseTime.Add(7*time.Second), "tool_use", []string{"reused"})
	next.RecordSDKAPISuccess(2000)
	last := tracker.Begin(continuation("third", "reused"))
	last.ObserveSDKQuery()
	last.ObserveSDKAssistantMessage(baseTime.Add(8*time.Second), nil)
	last.FinishSuccess(baseTime.Add(9*time.Second), "end_turn", nil)
	last.RecordSDKAPISuccess(1000)
	s := last.SDKResultSnapshot()
	if !s.CompleteFacts || s.NumTurns != 3 || s.ToolResultUserYields != 2 || s.ToolUseCount != 2 || s.Queries != 3 {
		t.Fatalf("tool ID was treated as a global yield identity: %+v", s)
	}
}

func TestSDKInvalidOrMixedUserTailCannotMaterializeToolYields(t *testing.T) {
	for _, rows := range []string{
		`{"role":"user","content":"new input"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]},{"role":"user","content":[]}`,
	} {
		var tracker Tracker
		first := beginSDKToolBatch(t, &tracker, "a")
		i := input("invalid-results")
		i.Body = []byte(`{"messages":[` + rows + `]}`)
		next := tracker.Begin(i)
		if next.Identity().PromptID == first.Identity().PromptID || next.Snapshot().SDK.ToolResultUserYields != 0 || next.Snapshot().IncompleteReason == "" {
			t.Fatal("ambiguous user tail acquired a canonical tool-result yield")
		}
		if first.Snapshot().SDK.ToolResultUserYields != 0 {
			t.Fatal("rejected tail changed the old prompt")
		}
	}
}
