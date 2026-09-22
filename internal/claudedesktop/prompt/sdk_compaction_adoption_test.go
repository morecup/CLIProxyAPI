package prompt

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func compactAdoptionFixture(t *testing.T, tracker *Tracker) (*Request, Input) {
	t.Helper()
	main := beginSDKToolBatch(t, tracker, "tool-before-compact")
	summary, vectors := compactSummaryFixture(t)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-before-compact","content":"synthetic result"},{"type":"text","text":"synthetic compact instruction"}]}]}`)
	main.RecordSDKCompactionSuccess("compact", "compact-call", 1000, CompletedSDKCompaction(ObserveSDKCompactionInput(body), summary))
	next := input("after-compact")
	next.ParentPromptID = main.Identity().PromptID
	next.StartedAt = baseTime.Add(4 * time.Second)
	next.Body = sdkCompactJSON(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": vectors.Wrappers[12].Text}}})
	return main, next
}

func TestSDKCompactionAdoptionRunsAtQueryNotAtSelection(t *testing.T) {
	var tracker Tracker
	first, input := compactAdoptionFixture(t, &tracker)
	next := tracker.Begin(input)
	if next.Identity().StartsPrompt || next.Identity().PromptID != first.Identity().PromptID {
		t.Fatal("explicit matching compact continuation lost its parent")
	}
	before := next.Snapshot().SDK
	if before.SawCompact || before.PendingCompactions != 1 || before.NumTurns != 1 || before.ToolUseCount != 1 {
		t.Fatalf("selection alone applied the compact result: %+v", before)
	}
	next.ObserveSDKQuery(input.Body)
	next.ObserveSDKQuery(input.Body)
	after := next.Snapshot()
	if !after.SDK.SawCompact || after.SDK.PendingCompactions != 0 || after.SDK.NumTurns != 3 || after.SDK.CompactionSummaryUserYields != 1 || after.SDK.ToolResultUserYields != 1 || after.SDK.ToolUseCount != 0 || after.PendingTools != 0 || after.ToolResults != 1 {
		t.Fatalf("actual canonical boundary accounting: %+v", after)
	}
	next.ObserveSDKAssistantMessage(baseTime.Add(5*time.Second), nil)
	next.FinishSuccess(baseTime.Add(6*time.Second), "end_turn", nil)
	next.RecordSDKAPISuccess(2000)
	final := next.SDKResultSnapshot()
	if !next.CompletedPrompt() || !final.CompleteFacts || final.APIDurationMS != 5000 || final.NumTurns != 3 || final.Queries != 2 || !final.SawCompact {
		t.Fatalf("applied compact prompt result: %+v", final)
	}
	first.RecordSDKCompactionSuccess("compact", "late-other", 100)
	if next.SDKResultSnapshot() != final {
		t.Fatal("late helper changed the sealed adoption result")
	}
}

func TestSDKCompactionAdoptionIsNotRepeatedByRetry(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			var tracker Tracker
			_, input := compactAdoptionFixture(t, &tracker)
			first := tracker.Begin(input)
			if observed {
				first.ObserveSDKQuery(input.Body)
			}
			first.FinishFailure()
			input.Attempt = 2
			retry := tracker.Begin(input)
			retry.ObserveSDKQuery(input.Body)
			first.ObserveSDKQuery(input.Body)
			got := retry.Snapshot().SDK
			if got.CompactionSummaryUserYields != 1 || got.PendingCompactions != 0 || got.NumTurns != 3 || got.Queries != 2 || !got.SawRetry {
				t.Fatalf("retry duplicated or lost adoption: %+v", got)
			}
		})
	}
}

func TestSDKCompactionSelectionCancellationDoesNotApply(t *testing.T) {
	var tracker Tracker
	_, input := compactAdoptionFixture(t, &tracker)
	next := tracker.Begin(input)
	next.FinalizeCancellation(baseTime.Add(5 * time.Second))
	next.ObserveSDKQuery(input.Body)
	got := next.SDKResultSnapshot()
	if got.SawCompact || got.PendingCompactions != 1 || got.CompactionSummaryUserYields != 0 {
		t.Fatalf("cancelled selection became an applied boundary: %+v", got)
	}
}

func TestSDKCompactionCannotUseUnownedOrAmbiguousSummary(t *testing.T) {
	for _, mode := range []string{"missing-parent", "different-parent", "different-account", "different-session", "quoted", "changed", "duplicate-wrapper", "duplicate-helper", "finished-parent", "active-parent"} {
		t.Run(mode, func(t *testing.T) {
			var tracker Tracker
			owner, in := compactAdoptionFixture(t, &tracker)
			summary, vectors := compactSummaryFixture(t)
			switch mode {
			case "missing-parent":
				in.ParentPromptID = ""
			case "different-parent":
				in.ParentPromptID = "22222222-2222-4222-8222-222222222222"
			case "different-account":
				in.AccountID = "other"
			case "different-session":
				in.SessionID = "other"
			case "quoted", "changed":
				in.Body = sdkCompactJSON(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": mode + vectors.Wrappers[0].Text}}})
			case "duplicate-wrapper":
				in.Body = sdkCompactJSON(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": vectors.Wrappers[0].Text}, {"type": "text", "text": vectors.Wrappers[0].Text}}}}})
			case "duplicate-helper":
				owner.RecordSDKCompactionSuccess("compact", "different-call", 100, CompletedSDKCompaction(ObserveSDKCompactionInput([]byte(`{"messages":[{"role":"user","content":"synthetic"}]}`)), summary))
			case "finished-parent":
				last := tracker.Begin(continuation("finish", "tool-before-compact"))
				last.ObserveSDKQuery()
				last.ObserveSDKAssistantMessage(baseTime.Add(6*time.Second), nil)
				last.FinishSuccess(baseTime.Add(7*time.Second), "end_turn", nil)
				last.RecordSDKAPISuccess(1000)
				last.SDKResultSnapshot()
			case "active-parent":
				tracker.Begin(continuation("active", "tool-before-compact"))
			}
			next := tracker.Begin(in)
			next.ObserveSDKQuery(in.Body)
			if !next.Identity().StartsPrompt || next.call.state == owner.call.state || next.Snapshot().SDK.SawCompact || owner.Snapshot().SDK.SawCompact {
				t.Fatal("unowned or ambiguous summary acquired a parent or adoption")
			}
		})
	}
}

func TestSDKCompactionUnknownHistoryDoesNotFabricateUserYields(t *testing.T) {
	var tracker Tracker
	_, in := compactAdoptionFixture(t, &tracker)
	_, vectors := compactSummaryFixture(t)
	in.Body = sdkCompactJSON(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "unobserved hook or attachment"}, {"type": "text", "text": vectors.Wrappers[0].Text}}}}})
	next := tracker.Begin(in)
	next.ObserveSDKQuery(in.Body)
	next.ObserveSDKAssistantMessage(baseTime.Add(5*time.Second), nil)
	next.FinishSuccess(baseTime.Add(6*time.Second), "end_turn", nil)
	next.RecordSDKAPISuccess(2000)
	got := next.SDKResultSnapshot()
	if !got.SawCompact || got.PendingCompactions != 0 || got.CompleteFacts || got.IncompleteReason != "unobserved-sdk-compaction-user-yields" {
		t.Fatalf("unknown hook history was silently considered complete: %+v", got)
	}
}

func TestSDKCompactionAdoptionRequiresFinalWireCorroboration(t *testing.T) {
	for _, mode := range []string{"missing-body", "rewritten", "extra-hook", "late-ambiguous-helper"} {
		t.Run(mode, func(t *testing.T) {
			var tracker Tracker
			owner, in := compactAdoptionFixture(t, &tracker)
			next := tracker.Begin(in)
			body := in.Body
			summary, vectors := compactSummaryFixture(t)
			switch mode {
			case "missing-body":
				body = nil
			case "rewritten":
				body = []byte(`{"messages":[{"role":"user","content":"rewritten summary"}]}`)
			case "extra-hook":
				body = sdkCompactJSON(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": vectors.Wrappers[12].Text}, {"type": "text", "text": "unknown hook"}}}}})
			case "late-ambiguous-helper":
				owner.RecordSDKCompactionSuccess("compact", "late-duplicate", 100, CompletedSDKCompaction(ObserveSDKCompactionInput([]byte(`{"messages":[{"role":"user","content":"synthetic"}]}`)), summary))
			}
			next.ObserveSDKQuery(body)
			got := next.Snapshot().SDK
			if mode == "extra-hook" {
				if !got.SawCompact || got.IncompleteReason != "unobserved-sdk-compaction-user-yields" {
					t.Fatalf("final-wire hook was treated as known: %+v", got)
				}
			} else if got.SawCompact || got.CompactionSummaryUserYields != 0 || got.IncompleteReason != "unobserved-sdk-compaction-adoption" {
				t.Fatalf("selection bypassed final-wire evidence: %+v", got)
			}
		})
	}
}

func TestSDKCompactionMissingPrecompactResultStaysIncomplete(t *testing.T) {
	var tracker Tracker
	owner := beginSDKToolBatch(t, &tracker, "missing_result")
	summary, vectors := compactSummaryFixture(t)
	owner.RecordSDKCompactionSuccess("compact", "helper", 1000, CompletedSDKCompaction(ObserveSDKCompactionInput([]byte(`{"messages":[{"role":"user","content":"synthetic compact instruction"}]}`)), summary))
	in := input("after_missing_result")
	in.ParentPromptID = owner.Identity().PromptID
	in.Body = sdkCompactJSON(t, map[string]any{"messages": []map[string]any{{"role": "user", "content": vectors.Wrappers[0].Text}}})
	next := tracker.Begin(in)
	next.ObserveSDKQuery(in.Body)
	got := next.Snapshot()
	if !got.SDK.SawCompact || got.SDK.CompactionSummaryUserYields != 1 || got.SDK.ToolResultUserYields != 0 || got.PendingTools != 1 || got.SDK.CompleteFacts || got.SDK.IncompleteReason != "unobserved-precompact-tool-result" {
		t.Fatalf("missing precompact result was fabricated: %+v", got)
	}
}

func TestSDKCompactionConcurrentCallbacksAndSelectionAreSingleOwner(t *testing.T) {
	var tracker Tracker
	owner, in := compactAdoptionFixture(t, &tracker)
	summary, _ := compactSummaryFixture(t)
	observation := CompletedSDKCompaction(ObserveSDKCompactionInput([]byte(`{"messages":[{"role":"user","content":"duplicate ignored"}]}`)), summary)
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() { owner.RecordSDKCompactionSuccess("compact", "compact-call", 1000, observation) })
	}
	group.Wait()
	requests := make([]*Request, 8)
	for i := range requests {
		group.Go(func() {
			candidate := in
			candidate.ClientRequestID = fmt.Sprintf("concurrent-%d", i)
			requests[i] = tracker.Begin(candidate)
			requests[i].ObserveSDKQuery(candidate.Body)
		})
	}
	group.Wait()
	bound := 0
	for _, request := range requests {
		if request.call.state == owner.call.state {
			bound++
		}
	}
	got := owner.Snapshot().SDK
	if bound != 1 || got.CompactionSummaryUserYields != 1 || got.ToolResultUserYields != 1 || got.PendingCompactions != 0 || got.APIDurationMS != 3000 {
		t.Fatalf("concurrent callbacks/selection duplicated adoption: bound=%d sdk=%+v", bound, got)
	}
}
