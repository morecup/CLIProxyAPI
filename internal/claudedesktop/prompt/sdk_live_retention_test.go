package prompt

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSDKLiveRequestSurvivesTTLPruneAndPersistsLateResponse(t *testing.T) {
	structural, content := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
	transcript := &sdkTranscriptMemoryStore{}
	tracker := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	tracker.transcript = pausedTranscriptWriter(t, transcript)
	t.Cleanup(func() { _ = tracker.Close() })
	at := time.Now().UTC().Truncate(time.Millisecond)
	initial := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic input"}]}`, at)
	request := tracker.Begin(initial)
	request.ObserveSDKQuery(initial.Body)
	firstUUID := request.SDKHistory().Messages[0].UUID
	other := sdkStateTestInput(`{"messages":[{"role":"user","content":"other account"}]}`, at.Add(2*time.Hour))
	other.AccountID = "other-account"
	tracker.Begin(other)
	if got := tracker.NativeContent(initial.AccountID, initial.SessionID); len(got.Messages) != 1 || got.Messages[0].UUID != firstUUID {
		t.Fatal("TTL pruning evicted a live request's native content owner")
	}
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"id":"msg_late","type":"message","role":"assistant","content":[{"type":"text","text":"synthetic late response"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), false, other.StartedAt.Add(time.Second))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(other.StartedAt.Add(time.Second), "end_turn", nil)
	request.RecordSDKAPISuccess(19)
	if err := request.CheckpointSDKSessionState(); err != nil {
		t.Fatal("late completion lost its durable owner", err)
	}
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	scope := digest(initial.AccountID, initial.SessionID)
	index, _ := transcript.LoadTranscript(scope)
	if len(index.UUIDs) != 2 || index.UUIDs[0] != firstUUID {
		t.Fatal("late completion was not appended under its original identity")
	}
	record, err := decodeSDKSession(structural.data[scope], scope)
	if err != nil || len(record.History.Messages) != 2 || record.DurationMS != 19 || !bytes.Contains(content.data[scope], []byte("synthetic late response")) {
		t.Fatal("late completion did not reach structural and native checkpoints", err)
	}
}

func TestSDKLiveRequestsAreNotCapacityEvictionCandidates(t *testing.T) {
	var tracker Tracker
	at := time.Now()
	initial := sdkStateTestInput(`{"messages":[{"role":"user","content":"first"}]}`, at)
	first := tracker.Begin(initial)
	for i := 0; i < maxPrompts; i++ {
		next := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"}]}`, at.Add(time.Duration(i+1)*time.Millisecond))
		next.AccountID = fmt.Sprint("account-", i)
		tracker.Begin(next)
	}
	if tracker.prompts[first.call.state.key] != first.call.state || len(tracker.prompts) != maxPrompts+1 {
		t.Fatal("capacity policy evicted a live request")
	}
	first.FinalizeFailure(at.Add(time.Second))
	tracker.mu.Lock()
	tracker.prune(at.Add(2 * time.Second))
	tracker.mu.Unlock()
	if tracker.prompts[first.call.state.key] != nil || len(tracker.prompts) != maxPrompts {
		t.Fatal("settled owner was not eligible for capacity eviction")
	}
}

func TestSDKBoundHelperSurvivesTTLAndPersistsLateAccounting(t *testing.T) {
	store := &sdkMemorySessionStore{}
	tracker := NewTracker(store)
	at := time.Now()
	initial := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"}]}`, at)
	request := tracker.Begin(initial)
	request.ObserveSDKQuery(initial.Body)
	sdkStateComplete(t, request, `{"id":"msg_parent","type":"message","role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn"}`, at.Add(time.Second), 17)
	input := initial
	input.ParentPromptID = request.Identity().PromptID
	helper := tracker.BindSDKHelper(input)
	defer helper.Close()
	if helper == nil {
		t.Fatal("parent was not bound")
	}
	other := sdkStateTestInput(`{"messages":[{"role":"user","content":"other"}]}`, at.Add(2*time.Hour))
	other.AccountID = "other-account"
	tracker.Begin(other)
	helper.RecordSuccess("generate_session_title", "late-helper", 7)
	scope := digest(initial.AccountID, initial.SessionID)
	record, err := decodeSDKSession(store.data[scope], scope)
	if err != nil || record.DurationMS != 24 {
		t.Fatal("live helper updated an orphaned ledger instead of durable state", err, record.DurationMS)
	}
}

func TestSDKDurableRestoreDoesNotRejectValidScopeAtCacheTarget(t *testing.T) {
	store := &sdkMemorySessionStore{}
	tracker := NewTracker(store)
	at := time.Now()
	initial := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"}]}`, at)
	request := tracker.Begin(initial)
	request.ObserveSDKQuery(initial.Body)
	sdkStateComplete(t, request, `{"id":"msg_restore","type":"message","role":"assistant","content":[{"type":"text","text":"reply"}],"stop_reason":"end_turn"}`, at.Add(time.Second), 17)
	restarted := NewTracker(store)
	for i := 0; i < maxPrompts-1; i++ {
		next := sdkStateTestInput(`{"messages":[{"role":"user","content":"active"}]}`, at.Add(2*time.Second))
		next.AccountID = uuid.NewString()
		restarted.Begin(next)
	}
	input := initial
	input.ParentPromptID = request.Identity().PromptID
	input.StartedAt = at.Add(3 * time.Second)
	bound := restarted.BindSDKHelper(input)
	defer bound.Close()
	if bound == nil || bound.SDKSessionStateError() != nil {
		t.Fatal("resident cache target was treated as corruption of a valid durable session")
	}
}

func TestSDKLiveRetrySurvivesTTLWithoutRebindingOrReopening(t *testing.T) {
	var tracker Tracker
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic retry"}]}`, at)
	first := tracker.Begin(input)
	first.ObserveSDKQuery(input.Body)
	first.FinishFailure()
	input.StartedAt, input.Attempt = at.Add(2*time.Hour), 2
	retry := tracker.Begin(input)
	if retry.call != first.call || retry.Identity().PromptID != first.Identity().PromptID || retry.Identity().StartsPrompt {
		t.Fatal("TTL pruning reassigned the live retry chain")
	}
	if first.FinalizeFailure(input.StartedAt) {
		t.Fatal("obsolete attempt settled the retained retry")
	}
	if !retry.FinalizeCancellation(input.StartedAt.Add(time.Second)) {
		t.Fatal("current retry could not be cancelled")
	}
	tracker.mu.Lock()
	tracker.prune(input.StartedAt.Add(stateTTL + 2*time.Second))
	tracker.mu.Unlock()
	if tracker.prompts[first.call.state.key] != nil {
		t.Fatal("terminal cancellation leaked a live owner")
	}
}

func TestSDKLiveOwnerRetainsPendingNativeAPICallback(t *testing.T) {
	store := &sdkMemorySessionStore{}
	tracker := NewTracker(store)
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic callback"}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	request.FinishSuccess(at.Add(time.Second), "end_turn", nil)
	tracker.mu.Lock()
	tracker.prune(at.Add(2 * time.Hour))
	tracker.mu.Unlock()
	if tracker.prompts[request.call.state.key] == nil {
		t.Fatal("HTTP completion detached the not-yet-recorded native callback")
	}
	request.RecordSDKAPISuccess(11)
	scope := digest(input.AccountID, input.SessionID)
	record, err := decodeSDKSession(store.data[scope], scope)
	if err != nil || record.DurationMS != 11 {
		t.Fatal("retained callback was not persisted", err)
	}
	tracker.mu.Lock()
	tracker.prune(at.Add(2 * time.Hour))
	tracker.mu.Unlock()
	if tracker.prompts[request.call.state.key] != nil {
		t.Fatal("completed native accounting did not release retention")
	}
}

func TestSDKHelperLeaseReleaseIsIdempotentAndStopsLateMutation(t *testing.T) {
	var tracker Tracker
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic helper"}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	request.FinishSuccess(at.Add(time.Second), "end_turn", nil)
	request.RecordSDKAPISuccess(17)
	input.ParentPromptID = request.Identity().PromptID
	helper := tracker.BindSDKHelper(input)
	if helper == nil || request.call.state.helperOwners != 1 {
		t.Fatal("helper did not acquire an owned lease")
	}
	before := helper.Snapshot()
	var group sync.WaitGroup
	for range 8 {
		group.Go(helper.Close)
	}
	group.Wait()
	helper.RecordSuccess("generate_session_title", "after-release", 7)
	helper.RecordCompactionSuccess("compact", "after-release", 7, SDKCompactionObservation{})
	helper.ObserveUnmodeledHelper()
	if request.call.state.helperOwners != 0 || helper.Snapshot() != before {
		t.Fatal("released callback modified the parent or released another lease")
	}
	tracker.mu.Lock()
	tracker.prune(time.Now().Add(2 * time.Hour))
	tracker.mu.Unlock()
	if tracker.prompts[request.call.state.key] != nil {
		t.Fatal("closed helper retained an idle scope")
	}
	_ = tracker.Close()
	if tracker.BindSDKHelper(input) != nil {
		t.Fatal("closed tracker created a helper lease")
	}
}

func TestSDKLiveOwnedCheckpointCanExceedResidentTarget(t *testing.T) {
	var tracker Tracker
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic concurrent session"}]}`, at)
	for range maxPrompts + 1 {
		input.ClientRequestID, input.PromptID = uuid.NewString(), uuid.NewString()
		tracker.Begin(input)
	}
	if len(tracker.prompts) != maxPrompts+1 {
		t.Fatal("live session was reduced to the idle cache target")
	}
	store := &sdkMemorySessionStore{}
	scope := digest(input.AccountID, input.SessionID)
	tracker.store = store
	tracker.sessions = map[string]*sdkSessionPersistence{scope: {}}
	tracker.mu.Lock()
	tracker.saveSDKSessionLocked(scope)
	tracker.mu.Unlock()
	record, err := decodeSDKSession(store.data[scope], scope)
	if err != nil || len(record.Prompts) != maxPrompts+1 {
		t.Fatal("valid live checkpoint exceeded a cache-only validation limit", err)
	}
	restarted := NewTracker(store)
	input.ParentPromptID = input.PromptID
	if helper := restarted.BindSDKHelper(input); helper != nil {
		helper.Close()
		t.Fatal("unobserved parent acquired a helper")
	}
	if len(restarted.prompts) != maxPrompts+1 {
		t.Fatal("valid checkpoint was not restored above the cache target")
	}
	for _, state := range restarted.prompts {
		if state.hasLiveOwner() || !state.failed || state.incompleteReason != "sdk-session-interrupted-during-query" {
			t.Fatal("restart resurrected process-local callbacks or invented their success")
		}
	}
}

func TestSDKRestoreDoesNotResurrectUnrecordedAPICallback(t *testing.T) {
	store := &sdkMemorySessionStore{}
	tracker := NewTracker(store)
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic interrupted callback"}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	request.FinishSuccess(at.Add(time.Second), "end_turn", nil)
	if !request.call.state.hasLiveOwner() {
		t.Fatal("original API callback did not retain its live owner")
	}
	restarted := NewTracker(store)
	input.ParentPromptID = request.Identity().PromptID
	helper := restarted.BindSDKHelper(input)
	if helper == nil {
		t.Fatal("valid historical parent could not be restored")
	}
	helper.Close()
	restored := restarted.prompts[request.call.state.key]
	if restored.hasLiveOwner() {
		t.Fatal("restored HTTP success resurrected a dead process's API callback")
	}
	if restored.sdk.ledger.durationMS != 0 || restored.sdk.ledger.incompleteReason != "sdk-session-unsettled-api-accounting" || helper.SDKSessionStateError() == nil {
		t.Fatal("unrecorded API callback was counted or its loss was hidden")
	}
	restarted.mu.Lock()
	restarted.prune(at.Add(2 * time.Hour))
	restarted.mu.Unlock()
	if restarted.prompts[request.call.state.key] != nil {
		t.Fatal("restored dead callback permanently retained an idle scope")
	}
}
