package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSDKCompactionApplicationConcurrentCommitHasOneOwner(t *testing.T) {
	owner, _, application, i := stagedCompactionFixture(t)
	before := owner.Snapshot().SDK
	var group sync.WaitGroup
	results := make(chan *Request, 8)
	for range 8 {
		group.Go(func() {
			attempt := i
			attempt.ClientRequestID = uuid.NewString()
			next, err := application.Commit(t.Context(), attempt)
			if err == nil {
				results <- next
			} else if !errors.Is(err, ErrSDKCompactionViewStale) {
				t.Error(err)
			}
		})
	}
	group.Wait()
	close(results)
	if len(results) != 1 {
		t.Fatal("multiple concurrent applications acquired the same history")
	}
	next := <-results
	if next.Snapshot().SDK.NumTurns != before.NumTurns+1 {
		t.Fatal("concurrent commit duplicated summary yields")
	}
}

func TestSDKCompactionRecoveredFailureIsNotAPISuccess(t *testing.T) {
	owner, view, application, input := stagedCompactionFixture(t)
	if !view.ClaimReactiveFailure() || view.ClaimReactiveFailure() {
		t.Fatal("reactive failure was not claimed exactly once")
	}
	before := owner.Snapshot().SDK
	next, err := application.Commit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	after := next.Snapshot().SDK
	if after.RecoveredAPIFailures != before.RecoveredAPIFailures+1 || after.APIDurationMS != before.APIDurationMS {
		t.Fatal("failed query was charged as successful API work")
	}
	owner.RecordSDKAPISuccess(99999)
	if next.Snapshot().SDK != after || owner.call.succeeded {
		t.Fatal("retired failed query accepted a success callback")
	}
	application.Discard()
	if next.Snapshot().SDK != after {
		t.Fatal("discarding adopted draft changed accounting")
	}
}

func TestSDKCompactionDiscardResolvesOnlySelectedPendingHelper(t *testing.T) {
	owner, view, application, _ := stagedCompactionFixture(t)
	owner.RecordSDKCompactionSuccess("compact", "another-helper", 15)
	before := owner.Snapshot().SDK
	application.Discard()
	after := owner.Snapshot().SDK
	if after.PendingCompactions != before.PendingCompactions-1 || after.SawCompact || after.APIDurationMS != before.APIDurationMS {
		t.Fatal("discard adopted a summary or removed another helper's evidence")
	}
	view.DiscardHelper("synthetic-helper")
	if owner.Snapshot().SDK != after {
		t.Fatal("duplicate discard changed accounting")
	}
}

func stagedCompactionFixture(t *testing.T) (*Request, *SDKCompactionView, *SDKCompactionApplication, Input) {
	t.Helper()
	owner, body := compactionViewFixture(t, true)
	view, err := owner.CompactionView(body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(view.Discard)
	var response SDKCompactionResponse
	response.ObserveJSON([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"<summary>PRIVATE_SUMMARY</summary>"}]}`))
	text, known := response.TakeText("1.40609.0.0", "2.1.247")
	if !known {
		t.Fatal("synthetic summary was not selected")
	}
	history := view.History()
	groups := GroupSDKHistory(history.Messages)
	owner.RecordSDKCompactionSuccess("compact", "synthetic-helper", 100, CompletedSDKCompaction(ObserveSDKCompactionInput(body), text.Fingerprint()))
	application, err := view.PrepareApplication(text, SDKCompactionWrapOptions{SuppressFollowUpQuestions: true}, groups[len(groups)-1], "synthetic-helper")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.Discard)
	i := input(uuid.NewString())
	i.PromptID = owner.Identity().PromptID
	i.StartedAt = baseTime.Add(10 * time.Second)
	i.Body, err = json.Marshal(map[string]any{"messages": application.Messages()})
	if err != nil {
		t.Fatal(err)
	}
	return owner, view, application, i
}

func TestSDKCompactionApplicationReplacesHistoryAndQueryAtomically(t *testing.T) {
	owner, view, application, i := stagedCompactionFixture(t)
	before, oldHistory := owner.Snapshot().SDK, view.History()
	if before.SawCompact || before.PendingCompactions != 1 {
		t.Fatal("staging applied summary")
	}
	rows := application.Messages()
	rows[0][0] = 'x'
	encoded, _ := json.Marshal(application)
	if string(encoded) != "{}" {
		t.Fatal("staged plaintext serialized")
	}
	next, err := application.Commit(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	if view.Current() || next.Identity().PromptID != owner.Identity().PromptID || next.Identity().StartsPrompt || next.Identity().QueryDepth != owner.Identity().QueryDepth+1 {
		t.Fatal("active query ownership was not advanced")
	}
	after := next.Snapshot().SDK
	if !after.SawCompact || after.NumTurns != before.NumTurns+1 || after.Queries != before.Queries+1 || after.PendingCompactions != 0 || after.APIDurationMS != before.APIDurationMS {
		t.Fatal("native summary yield changed unrelated ledger/turn accounting")
	}
	history := next.SDKHistory()
	preserved := oldHistory.Groups[len(oldHistory.Groups)-1]
	if !history.OwnedMessagesKnown || len(history.Messages) != 2+len(preserved) || history.Messages[0].Subtype != "compact_boundary" || history.Messages[1].Type != "user" {
		t.Fatal("native history order differs")
	}
	var sum int64
	for index, message := range history.Messages[2:] {
		want := preserved[index]
		if want.Type == "assistant" {
			want.Usage = SDKTokenUsage{}
			want.UsageKnown = true
		}
		if message != want {
			t.Fatal("preserved UUID/content metadata was changed")
		}
	}
	for _, message := range history.Messages {
		sum += message.TokenEstimate.Tokens
	}
	if application.PostTokens() != sum {
		t.Fatal("post tokens did not use native content sum")
	}
	observeHistoryResponse(owner, "late-old-assistant", 1, baseTime)
	owner.RecordSDKAPISuccess(99999)
	owner.ObserveSDKRetry(502)
	owner.FinishFailure()
	if owner.FinalizeFailure(baseTime) || owner.FinalizeCancellation(baseTime) || !reflect.DeepEqual(history.Messages, next.SDKHistory().Messages) {
		t.Fatal("old callback changed the new history")
	}
	if next.Snapshot().SDK != after {
		t.Fatal("late success/retry callback changed committed continuation accounting")
	}
	if _, err = application.Commit(t.Context(), i); !errors.Is(err, ErrSDKCompactionViewStale) {
		t.Fatal("application replay was accepted")
	}
	if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(next.SDKHistory().IncompleteReason, "PRIVATE_") {
		t.Fatal("content persisted into diagnostics")
	}
	next.ObserveSDKQuery(i.Body)
	if next.Snapshot().SDK.Queries != after.Queries {
		t.Fatal("query observation counted committed request twice")
	}
	completeSDKHistoryText(next, "msg_after", "PRIVATE_AFTER")
	continuation := input(uuid.NewString())
	continuation.StartedAt = baseTime.Add(12 * time.Second)
	var normalized struct{ Messages []json.RawMessage }
	if json.Unmarshal(i.Body, &normalized) != nil {
		t.Fatal("bad prepared request")
	}
	normalized.Messages = append(normalized.Messages, json.RawMessage(`{"role":"assistant","content":"PRIVATE_AFTER"}`), json.RawMessage(`{"role":"user","content":"PRIVATE_NEXT"}`))
	continuation.Body, _ = json.Marshal(normalized)
	last := next.tracker.Begin(continuation)
	last.ObserveSDKQuery(continuation.Body)
	if !last.SDKHistory().OwnedMessagesKnown {
		t.Fatal("next ordinary turn lost compacted history reconciliation")
	}
}

func TestSDKReactiveApplicationAllowsOnlySummarizeAllWireSuffix(t *testing.T) {
	for _, mode := range []string{"partial", "empty"} {
		t.Run(mode, func(t *testing.T) {
			owner, body := compactionViewFixture(t, true)
			view, err := owner.CompactionView(body)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Discard()
			var response SDKCompactionResponse
			response.ObserveJSON([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"<summary>PRIVATE_SUMMARY</summary>"}]}`))
			text, known := response.TakeText("1.40609.0.0", "2.1.247")
			if !known {
				t.Fatal("synthetic summary was not selected")
			}
			history := view.History()
			var preserve []SDKHistoryMessage
			wantRows := 1
			if mode == "partial" {
				preserve = history.Messages[len(history.Messages)-1:]
				wantRows++
			}
			helperID := "reactive-" + mode
			owner.RecordSDKCompactionSuccess("compact", helperID, 100, CompletedSDKCompaction(ObserveSDKCompactionInput(body), text.Fingerprint()))
			options := SDKCompactionWrapOptions{SuppressFollowUpQuestions: true}
			if _, err = view.PrepareApplication(text, options, preserve, helperID); !errors.Is(err, ErrSDKCompactionContentUnknown) {
				t.Fatal("strict round application accepted an empty or partial group suffix")
			}
			application, err := view.PrepareReactiveApplication(text, options, preserve, "summarize_all", helperID)
			if err != nil {
				t.Fatal(err)
			}
			defer application.Discard()
			if rows := application.Messages(); len(rows) != wantRows {
				t.Fatalf("application rows=%d want=%d", len(rows), wantRows)
			}
			if _, err = view.PrepareReactiveApplication(text, options, preserve, "unknown", helperID); !errors.Is(err, ErrSDKCompactionContentUnknown) {
				t.Fatal("unknown reactive split kind was accepted")
			}
			if mode == "partial" {
				changed := append([]SDKHistoryMessage(nil), preserve...)
				changed[0].UUID = "foreign"
				if _, err = view.PrepareReactiveApplication(text, options, changed, "summarize_all", helperID); !errors.Is(err, ErrSDKCompactionContentUnknown) {
					t.Fatal("non-owned reactive suffix was accepted")
				}
			}
		})
	}
}

func TestSDKCompactionApplicationRejectsWrongOrStaleCommitWithoutMutation(t *testing.T) {
	for _, mode := range []string{"account", "session", "parent", "role", "retry", "body", "identity", "cancelled", "context-cancelled", "concurrent", "discarded", "unknown-helper", "unobserved-helper-input", "already-adopted-helper", "different-summary"} {
		t.Run(mode, func(t *testing.T) {
			owner, _, application, i := stagedCompactionFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "account":
				i.AccountID = "other"
			case "session":
				i.SessionID = "other"
			case "parent":
				i.PromptID = uuid.NewString()
			case "role":
				i.Role = "compaction"
			case "retry":
				i.Attempt = 2
			case "body":
				i.Body = []byte(strings.ReplaceAll(string(i.Body), "PRIVATE_SUMMARY", "changed"))
			case "identity":
				i.ClientRequestID = "not UUID"
			case "cancelled":
				owner.FinishFailure()
			case "context-cancelled":
				cancel()
			case "concurrent":
				other := input(uuid.NewString())
				other.StartedAt = i.StartedAt
				other.Body = i.Body
				owner.tracker.Begin(other)
			case "discarded":
				application.Discard()
			case "unknown-helper":
				application.helperKey = digest("compact", "unobserved-helper")
			case "unobserved-helper-input":
				owner.call.state.sdk.compactions[application.helperKey].observation.input = SDKCompactionInput{}
			case "already-adopted-helper":
				owner.call.state.sdk.compactions[application.helperKey].applied = true
			case "different-summary":
				owner.call.state.sdk.compactions[application.helperKey].observation.summary = SDKCompactionSummary{}
			}
			before := owner.SDKHistory()
			if _, err := application.Commit(ctx, i); err == nil {
				t.Fatal("invalid application accepted")
			}
			if !reflect.DeepEqual(before, owner.SDKHistory()) || owner.Snapshot().SDK.SawCompact {
				t.Fatal("failed application mutated shared history")
			}
		})
	}
}
