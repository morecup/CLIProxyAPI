package prompt

import (
	"sync"
	"testing"
	"time"
)

func TestSDKCompactionAPIAccountingDoesNotClaimSummaryAdoption(t *testing.T) {
	var tracker Tracker
	main := beginSDKToolBatch(t, &tracker, "tool")
	main.RecordSDKCompactionSuccess("compact", "summary", 69857)
	main.RecordSDKCompactionSuccess("compact", "summary", 69857)
	s := main.Snapshot().SDK
	if s.APIDurationMS != 71857 || s.PendingCompactions != 1 || s.SawCompact || s.NumTurns != 1 || s.ToolUseCount != 1 || s.CompleteFacts || s.IncompleteReason != "unobserved-sdk-compaction-disposition" {
		t.Fatalf("helper success was treated as summary application: %+v", s)
	}
	if s.Queries != 1 || s.SawRetry || s.ToolResultUserYields != 0 {
		t.Fatal("compaction HTTP completion generated main-loop yields or retries")
	}
}

func TestSDKCompactionDispositionIsLocalButAPILedgerIsShared(t *testing.T) {
	var tracker Tracker
	old := tracker.Begin(input("old"))
	old.ObserveSDKQuery()
	old.RecordSDKCompactionSuccess("compact", "summary", 69857)
	old.ObserveSDKAssistantMessage(baseTime.Add(time.Second), nil)
	old.FinishSuccess(baseTime.Add(2*time.Second), "end_turn", nil)
	old.RecordSDKAPISuccess(5518)
	sealed := old.SDKResultSnapshot()
	if sealed.APIDurationMS != 75375 || sealed.CompleteFacts {
		t.Fatalf("known capture-style ledger was lost: %+v", sealed)
	}
	next := tracker.Begin(input("next"))
	next.ObserveSDKQuery()
	old.RecordSDKCompactionSuccess("compact", "late-summary", 250)
	next.ObserveSDKAssistantMessage(baseTime.Add(3*time.Second), nil)
	next.FinishSuccess(baseTime.Add(4*time.Second), "end_turn", nil)
	next.RecordSDKAPISuccess(1000)
	got := next.SDKResultSnapshot()
	if !got.CompleteFacts || got.PendingCompactions != 0 || got.APIDurationMS != 1250 || got.SawCompact || got.NumTurns != 1 {
		t.Fatalf("older disposition poisoned a later prompt: %+v", got)
	}
	if old.SDKResultSnapshot() != sealed {
		t.Fatal("late compact callback rewrote a sealed result")
	}
}

func TestSDKCompactionConcurrentCallbacksRemainAccountAndSessionLocal(t *testing.T) {
	var tracker Tracker
	owner := tracker.Begin(input("owner"))
	foreign := input("foreign")
	foreign.AccountID = "other-account"
	account := tracker.Begin(foreign)
	foreign = input("foreign-session")
	foreign.SessionID = "other-session"
	session := tracker.Begin(foreign)
	var group sync.WaitGroup
	for range 24 {
		group.Add(1)
		go func() {
			defer group.Done()
			owner.RecordSDKCompactionSuccess("compact", "same-callback", 100)
		}()
	}
	group.Wait()
	if s := owner.Snapshot().SDK; s.APIDurationMS != 100 || s.PendingCompactions != 1 {
		t.Fatalf("compaction callback duplicated: %+v", s)
	}
	for _, other := range []*Request{account, session} {
		if s := other.Snapshot().SDK; s.APIDurationMS != 0 || s.PendingCompactions != 0 {
			t.Fatal("compaction facts crossed scope")
		}
	}
}

func TestSDKInvalidCompactionDurationStillDegradesSharedLedger(t *testing.T) {
	var tracker Tracker
	owner := tracker.Begin(input("owner"))
	owner.RecordSDKCompactionSuccess("compact", "invalid", -1)
	next := tracker.Begin(input("next"))
	if s := next.Snapshot().SDK; s.CompleteFacts || s.IncompleteReason != "invalid-helper-duration" {
		t.Fatalf("unknown numeric ledger was incorrectly considered known: %+v", s)
	}
	if owner.Snapshot().SDK.PendingCompactions != 0 {
		t.Fatal("invalid callback recorded a completed compaction")
	}
}
