package prompt

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSDKHelperBindingRequiresObservedExactParent(t *testing.T) {
	for _, scenario := range []string{"valid", "unobserved", "missing", "invalid", "account", "session", "expired", "evicted"} {
		t.Run(scenario, func(t *testing.T) {
			var tracker Tracker
			i := input(uuid.NewString())
			i.PromptID = uuid.NewString()
			owner := tracker.Begin(i)
			if scenario != "unobserved" {
				owner.ObserveSDKQuery(i.Body)
			}
			helper := i
			helper.ParentPromptID = owner.Identity().PromptID
			switch scenario {
			case "missing":
				helper.ParentPromptID = ""
			case "invalid":
				helper.ParentPromptID = "not-a-uuid"
			case "account":
				helper.AccountID = "foreign"
			case "session":
				helper.SessionID = "foreign"
			case "expired":
				owner.FinishSuccess(i.StartedAt.Add(time.Second), "end_turn", nil)
				owner.RecordSDKAPISuccess(0)
				helper.StartedAt = i.StartedAt.Add(stateTTL + 2*time.Second)
			case "evicted":
				tracker.prompts = nil
			}
			bound := tracker.BindSDKHelper(helper)
			defer bound.Close()
			if (bound != nil) != (scenario == "valid") {
				t.Fatalf("binding existence=%v", bound != nil)
			}
			bound.RecordSuccess("compact", "helper", 5)
			if got := owner.Snapshot().SDK.APIDurationMS; (got == 5) != (scenario == "valid") {
				t.Fatalf("foreign or unobserved callback changed ledger: %d", got)
			}
		})
	}
}

func TestSDKHelperBoundCallbackIsDeduplicatedAndCannotRewriteSealedResult(t *testing.T) {
	var tracker Tracker
	i := input(uuid.NewString())
	owner := tracker.Begin(i)
	owner.ObserveSDKQuery(i.Body)
	i.ParentPromptID = owner.Identity().PromptID
	bound := tracker.BindSDKHelper(i)
	if bound == nil {
		t.Fatal("observed owner was not bound")
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() { bound.RecordSuccess("generate_session_title", "same-helper", 70) })
	}
	group.Wait()
	if got := owner.Snapshot().SDK.APIDurationMS; got != 70 {
		t.Fatalf("duplicate helper callback: %d", got)
	}
	owner.FinishSuccess(i.StartedAt.Add(time.Second), "end_turn", nil)
	owner.RecordSDKAPISuccess(100)
	sealed := owner.SDKResultSnapshot()
	bound.RecordCompactionSuccess("compact", "late-helper", 80, SDKCompactionObservation{})
	if got := owner.SDKResultSnapshot(); got != sealed {
		t.Fatal("late helper changed sealed result")
	}
	if owner.call.state.sdk.ledger.durationMS != 250 || owner.call.state.sdk.pendingCompactions != 0 {
		t.Fatal("late callback lost ledger ownership or created disposition on a sealed prompt")
	}
}
