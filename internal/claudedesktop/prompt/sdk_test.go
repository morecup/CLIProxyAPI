package prompt

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSDKToolsCountClosedOccurrencesNotReturnedResults(t *testing.T) {
	var tracker Tracker
	i := input("tools")
	i.Body = []byte(`{"messages":[{"role":"user","content":"synthetic"}],"tools":[{"name":"alias","mcpInfo":{}},{"name":"ToolSearch","mcpInfo":{}}]}`)
	r := tracker.Begin(i)
	r.ObserveSDKQuery()
	tools := []ToolObservation{{ID: "same", Name: "custom"}, {ID: "same", Name: "custom"}, {Name: "alias"}, {Name: "ToolSearch"}, {Name: "mcp__server__tool"}}
	r.ObserveSDKAssistantMessage(baseTime.Add(time.Second), tools[:1])
	r.ObserveSDKAssistantMessage(baseTime.Add(2*time.Second), tools)
	r.ObserveSDKAssistantMessage(baseTime.Add(3*time.Second), tools)
	s := r.Snapshot()
	if s.ToolResults != 0 || s.SDK.ToolUseCount != 5 || s.SDK.BuiltinToolCalls != 2 || s.SDK.MCPToolCalls != 3 || s.SDK.ToolSearchCalls != 0 {
		t.Fatalf("occurrence/category accounting: %+v", s)
	}
	if s.SDK.FirstAssistantMessageAt != baseTime.Add(time.Second) {
		t.Fatal("polling replaced the first assistant timestamp")
	}
}

func TestSDKUnknownToolNamesAreBuiltinsAndCatalogIsCurrent(t *testing.T) {
	var tracker Tracker
	r := tracker.Begin(input("first"))
	r.ObserveSDKQuery()
	r.ObserveSDKAssistantMessage(baseTime, []ToolObservation{{ID: "tool", Name: "ToolSearch"}})
	r.FinishSuccess(baseTime.Add(time.Second), "tool_use", []string{"tool"})
	r.RecordSDKAPISuccess(1000)
	i := continuation("second", "tool")
	i.Body = []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool"}]}],"tools":[{"name":"ToolSearch","mcpInfo":{}}]}`)
	next := tracker.Begin(i)
	next.ObserveSDKQuery()
	next.ObserveSDKAssistantMessage(baseTime.Add(6*time.Second), []ToolObservation{{Name: "custom"}})
	s := next.Snapshot().SDK
	if s.ToolUseCount != 2 || s.MCPToolCalls != 1 || s.BuiltinToolCalls != 1 || s.ToolSearchCalls != 0 {
		t.Fatalf("current catalog did not classify prior occurrences: %+v", s)
	}
	if s.NumTurns != 2 || s.ToolResultUserYields != 1 || s.CompleteFacts || s.IncompleteReason != "unsettled-sdk-api-accounting" {
		t.Fatalf("canonical tool-result yield or unsettled API accounting: %+v", s)
	}
}

func TestSDKRetryAndStaleObserversDoNotAddUserTurns(t *testing.T) {
	var tracker Tracker
	i := input("retry")
	old := tracker.Begin(i)
	old.ObserveSDKQuery()
	old.FinishFailure()
	i.Attempt = 2
	r := tracker.Begin(i)
	r.ObserveSDKQuery()
	old.ObserveSDKAssistantMessage(baseTime, []ToolObservation{{Name: "stale"}})
	if old.FinalizeCancellation(baseTime) {
		t.Fatal("obsolete attempt cancelled its replacement")
	}
	r.ObserveSDKAssistantMessage(baseTime.Add(2*time.Second), nil)
	r.FinishSuccess(baseTime.Add(3*time.Second), "end_turn", nil)
	r.RecordSDKAPISuccess(3000)
	r.RecordSDKAPISuccess(9000)
	s := r.SDKResultSnapshot()
	if !s.CompleteFacts || s.Queries != 1 || s.NumTurns != 1 || !s.SawRetry || s.ToolUseCount != 0 || s.APIDurationMS != 3000 {
		t.Fatalf("retry accounting: %+v", s)
	}
}

func TestSDKCancellationSeparatesZeroAPISuccessFromMissingFacts(t *testing.T) {
	for _, assistant := range []bool{false, true} {
		t.Run(fmt.Sprint(assistant), func(t *testing.T) {
			var tracker Tracker
			r := tracker.Begin(input("cancel"))
			r.ObserveSDKQuery()
			if assistant {
				r.ObserveSDKAssistantMessage(baseTime.Add(time.Second), nil)
			}
			r.FinishFailure()
			if !r.FinalizeCancellation(baseTime.Add(33409*time.Millisecond)) || r.FinalizeCancellation(baseTime.Add(time.Minute)) {
				t.Fatal("cancellation finalizer is not exactly once")
			}
			s := r.SDKResultSnapshot()
			if !s.CompleteFacts || !s.CancelledStreaming || s.NumTurns != 2 || s.APIDurationMS != 0 || s.Queries != 1 {
				t.Fatalf("cancellation accounting: %+v", s)
			}
			next := tracker.Begin(input("continue"))
			next.ObserveSDKQuery()
			next.ObserveSDKAssistantMessage(baseTime.Add(time.Minute), nil)
			next.FinishSuccess(baseTime.Add(2*time.Minute), "end_turn", nil)
			next.RecordSDKAPISuccess(1893)
			continued := next.SDKResultSnapshot()
			if continued.CancelledStreaming || continued.NumTurns != 1 || continued.APIDurationMS != 1893 || !continued.CompleteFacts {
				t.Fatalf("continuation inherited cancellation: %+v", continued)
			}
			if r.Identity().PromptID == next.Identity().PromptID || r.SDKResultSnapshot() != s {
				t.Fatal("new prompt rewrote cancelled result/identity")
			}
		})
	}
}

func TestSDKFailureAndToolDrainCannotPretendToBeKnownCancellation(t *testing.T) {
	for _, kind := range []string{"failure", "before-query", "tool-drain"} {
		t.Run(kind, func(t *testing.T) {
			var tracker Tracker
			r := tracker.Begin(input(kind))
			if kind != "before-query" {
				r.ObserveSDKQuery()
			}
			if kind == "tool-drain" {
				r.ObserveSDKAssistantMessage(baseTime, []ToolObservation{{Name: "Read"}})
			}
			if kind == "failure" {
				r.FinalizeFailure(baseTime.Add(time.Second))
			} else {
				r.FinalizeCancellation(baseTime.Add(time.Second))
			}
			s := r.SDKResultSnapshot()
			if s.CompleteFacts || (kind != "tool-drain" && s.CancelledStreaming) {
				t.Fatalf("unknown SDK terminal facts became healthy: %+v", s)
			}
		})
	}
}

func TestSDKLedgerSnapshotsIncludeBoundLateHelpersWithoutRewritingResults(t *testing.T) {
	var tracker Tracker
	first := tracker.Begin(input("first"))
	first.ObserveSDKQuery()
	first.ObserveSDKAssistantMessage(baseTime, nil)
	first.FinishSuccess(baseTime.Add(time.Second), "end_turn", nil)
	first.RecordSDKAPISuccess(100)
	one := first.SDKResultSnapshot()
	second := tracker.Begin(input("second"))
	second.ObserveSDKQuery()
	// This callback belongs to the older prompt, but lands inside the newer
	// shared session window. Retry/source names do not alter main-loop counters.
	first.RecordSDKHelperSuccess("agent_classifier", "explicit-stream-callback", 50, true)
	first.RecordSDKHelperSuccess("agent_classifier", "explicit-stream-callback", 50, true)
	second.ObserveSDKAssistantMessage(baseTime.Add(2*time.Second), nil)
	second.FinishSuccess(baseTime.Add(3*time.Second), "end_turn", nil)
	second.RecordSDKAPISuccess(200)
	two := second.SDKResultSnapshot()
	if one.APIDurationMS != 100 || two.APIDurationMS != 250 || two.SawRetry || two.NumTurns != 1 || first.SDKResultSnapshot() != one {
		t.Fatalf("shared ledger/result snapshots: first=%+v second=%+v", one, two)
	}
	first.RecordSDKHelperSuccess("compact", "after-result", 75, true)
	if second.SDKResultSnapshot() != two {
		t.Fatal("late helper rewrote a sealed result")
	}
	third := tracker.Begin(input("third"))
	if got := third.Snapshot().SDK; got.APIDurationMS != 0 || got.SawCompact || got.SawRetry || got.NumTurns != 1 {
		t.Fatalf("new window inherited old counters/duration: %+v", got)
	}
}

func TestSDKLedgerIsolationAndConcurrentHelperDeduplication(t *testing.T) {
	var tracker Tracker
	owner := tracker.Begin(input("owner"))
	foreign := input("foreign")
	foreign.AccountID = "other-account"
	account := tracker.Begin(foreign)
	foreign = input("other-session")
	foreign.SessionID = "other-session"
	session := tracker.Begin(foreign)
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			owner.RecordSDKHelperSuccess("title", "same-callback", 125, false)
		}()
	}
	group.Wait()
	if owner.Snapshot().SDK.APIDurationMS != 125 || account.Snapshot().SDK.APIDurationMS != 0 || session.Snapshot().SDK.APIDurationMS != 0 {
		t.Fatal("helper callbacks duplicated or crossed account/session")
	}
}

func TestSDKLateUnknownHelperDegradesOpenWindowNotSealedResult(t *testing.T) {
	var tracker Tracker
	first := tracker.Begin(input("first"))
	first.ObserveSDKQuery()
	first.ObserveSDKAssistantMessage(baseTime, nil)
	first.FinishSuccess(baseTime.Add(time.Second), "end_turn", nil)
	first.RecordSDKAPISuccess(1000)
	sealed := first.SDKResultSnapshot()
	next := tracker.Begin(input("next"))
	first.ObserveSDKUnmodeledHelper()
	if first.SDKResultSnapshot() != sealed {
		t.Fatal("late uncertainty rewrote a sealed result")
	}
	if s := next.Snapshot().SDK; s.CompleteFacts || s.IncompleteReason != "unobserved-sdk-helper-boundary" {
		t.Fatalf("shared ledger uncertainty was hidden: %+v", s)
	}
}

func TestSDKRetryStatusIsOwnedMaximumAndHelpersDoNotChangeIt(t *testing.T) {
	var tracker Tracker
	r := tracker.Begin(input("retry-status"))
	r.ObserveSDKQuery()
	r.ObserveSDKRetry(503)
	r.ObserveSDKRetry(502)
	r.RecordSDKHelperSuccess("title", "helper", 1, true)
	r.ObserveSDKAssistantMessage(baseTime, nil)
	r.FinishSuccess(baseTime.Add(time.Second), "end_turn", nil)
	r.RecordSDKAPISuccess(1000)
	r.ObserveSDKRetry(599)
	if got := r.SDKResultSnapshot(); got.RetryStatus != 503 || !got.SawRetry || got.NumTurns != 1 {
		t.Fatalf("retry status/turn accounting: %+v", got)
	}
}

func TestSDKToolCatalogOverflowDoesNotFabricateBuiltinClassification(t *testing.T) {
	var tools []any
	for index := 0; index <= maxToolsPerRequest; index++ {
		tools = append(tools, map[string]any{"name": fmt.Sprint("alias", index), "mcpInfo": map[string]any{}})
	}
	body, err := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "synthetic"}}, "tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	i := input("catalog-limit")
	i.Body = body
	var tracker Tracker
	r := tracker.Begin(i)
	r.ObserveSDKQuery()
	r.ObserveSDKAssistantMessage(baseTime, []ToolObservation{{Name: fmt.Sprint("alias", maxToolsPerRequest)}})
	if got := r.Snapshot().SDK; got.CompleteFacts || got.IncompleteReason != "unobserved-sdk-tool-catalog" {
		t.Fatalf("dropped MCP aliases were silently counted as builtins: %+v", got)
	}
}
