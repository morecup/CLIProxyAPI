package prompt

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestNativeTranscriptMandatoryMetadata(t *testing.T) {
	tracker := nativeContentTestTracker(&sdkMemorySessionStore{}, &sdkMemorySessionStore{})
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic metadata"}]}`, time.Now())
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"type":"message","id":"msg_metadata","role":"assistant","content":[{"type":"text","text":"synthetic answer"}],"stop_reason":"end_turn"}`), false, input.StartedAt.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	rows := tracker.NativeContent(input.AccountID, input.SessionID).Messages
	if len(rows) != 2 {
		t.Fatal("expected the owned user and assistant rows")
	}
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(encoded, &fields)
		if string(fields["userType"]) != `"external"` {
			t.Fatal("native transcript must contain the pinned external userType")
		}
		for _, absent := range []string{"sessionKind", "gitBranch", "slug"} {
			if _, ok := fields[absent]; ok {
				t.Fatalf("ordinary owned runtime invented %s", absent)
			}
		}
	}
}

func TestNativeTranscriptOwnedMetadataReplacesRowFields(t *testing.T) {
	for _, kind := range []string{"", "bg", "daemon", "daemon-worker", "main", "interactive", " BG "} {
		t.Run("kind="+kind, func(t *testing.T) {
			branch, slug := "", "owned-plan"
			calls := 0
			tracker := nativeContentTestTracker(&sdkMemorySessionStore{}, &sdkMemorySessionStore{})
			input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic input"}]}`, time.Now())
			tracker.nativeOptions.Metadata = func(sessionID string) SDKNativeTranscriptMetadata {
				calls++
				if sessionID != input.SessionID {
					t.Fatal("owned metadata queried with foreign session")
				}
				return SDKNativeTranscriptMetadata{SessionKind: kind, GitBranch: &branch, Slug: &slug}
			}
			tracker.Begin(input)
			if calls != 1 {
				t.Fatal("input chain metadata was not resolved once")
			}
			row := tracker.NativeContent(input.AccountID, input.SessionID).Messages[0]
			wantKind := kind
			if kind != "bg" && kind != "daemon" && kind != "daemon-worker" {
				wantKind = ""
			}
			if row.SessionKind != wantKind || row.UserType != "external" || row.GitBranch == nil || *row.GitBranch != "" || row.Slug == nil || *row.Slug != "owned-plan" {
				t.Fatal("native owned metadata or undefined/empty distinction differs")
			}
			branch, slug = "changed-branch", "changed-plan"
			if got := tracker.NativeContent(input.AccountID, input.SessionID).Messages[0]; *got.GitBranch != "" || *got.Slug != "owned-plan" {
				t.Fatal("provider mutation changed an already owned metadata snapshot")
			}
			*row.GitBranch, *row.Slug = "snapshot-mutation", "snapshot-mutation"
			if got := tracker.NativeContent(input.AccountID, input.SessionID).Messages[0]; *got.GitBranch != "" || *got.Slug != "owned-plan" {
				t.Fatal("public snapshot mutated native metadata")
			}
			tracker.nativeOptions.Metadata = nil
			row.UUID = "71111111-1111-4111-8111-111111111111"
			row.UserType, row.SessionKind = "foreign", "bg"
			tracker.mu.Lock()
			tracker.appendNativeContentLocked(digest(input.AccountID, input.SessionID), row)
			tracker.mu.Unlock()
			got := tracker.NativeContent(input.AccountID, input.SessionID).Messages[1]
			if got.UserType != "external" || got.SessionKind != "" || got.GitBranch != nil || got.Slug != nil {
				t.Fatal("row properties overrode missing owned metadata")
			}
		})
	}
}

func TestNativeTranscriptLatchInsertedAfterInitialInputBeforeNextChain(t *testing.T) {
	for _, pin := range []string{"", "v1.PIN.a.b.signature"} {
		t.Run("pin="+pin, func(t *testing.T) {
			store := &sdkTranscriptMemoryStore{indices: make(map[string]SDKTranscriptIndex)}
			structural, content := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
			options := SDKNativeContentOptions{Store: content, TranscriptStore: store}
			tracker := NewTracker(structural, options)
			tracker.transcript = pausedTranscriptWriter(t, store)
			input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic input"}]}`, time.Now())
			scope := digest(input.AccountID, input.SessionID)
			store.indices[scope] = SDKTranscriptIndex{Path: "synthetic/main.jsonl"}
			request := tracker.Begin(input)
			request.ObserveSDKQuery(input.Body)
			request.BindNativeATISLatch(&pin)
			if err := tracker.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			if len(store.appends) != 1 || bytes.Contains(store.appends[0].lines, []byte("atis-latch")) {
				t.Fatal("query latch retroactively stamped the already inserted input")
			}
			var response Response
			response.EnableNativeContent()
			response.ObservePayloadAt([]byte(`{"type":"message","id":"msg_latch","role":"assistant","content":[{"type":"text","text":"synthetic answer"}],"stop_reason":"end_turn"}`), false, input.StartedAt.Add(time.Millisecond))
			observeNativeTestResponse(request, &response)
			request.FinishSuccess(input.StartedAt.Add(time.Millisecond), "end_turn", nil)
			request.RecordSDKAPISuccess(1)
			if err := tracker.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSpace(store.appends[1].lines), []byte{'\n'})
			if len(lines) != 2 {
				t.Fatal("next chain did not enqueue metadata followed by actual assistant")
			}
			var row map[string]any
			_ = json.Unmarshal(lines[0], &row)
			if !reflect.DeepEqual(row, map[string]any{"type": "atis-latch", "atis": pin, "sessionId": input.SessionID}) {
				t.Fatal("ATIS metadata differs from the native no-UUID shape")
			}
			request.BindNativeATISLatch(nil)
			if got := tracker.NativeContent(input.AccountID, input.SessionID).ATISLatch; got == nil || *got != pin {
				t.Fatal("late completed callback cleared the native latch")
			}
			restarted := NewTracker(structural, options)
			restarted.transcript = pausedTranscriptWriter(t, store)
			input.ClientRequestID, input.PromptID = "next-request", ""
			input.StartedAt = input.StartedAt.Add(time.Hour)
			restarted.Begin(input)
			if got := restarted.NativeContent(input.AccountID, input.SessionID); got.ATISLatch == nil || *got.ATISLatch != pin || got.PersistenceError {
				t.Fatal("native content did not restore the defined latch")
			}
			if err := restarted.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(store.appends[2].lines, []byte(`{"type":"atis-latch"`)) {
				t.Fatal("process-local stamp was incorrectly restored as already written")
			}
		})
	}
}

func TestNativeTranscriptATISStampFailureRelocationAndEviction(t *testing.T) {
	store := &sdkTranscriptMemoryStore{indices: map[string]SDKTranscriptIndex{"main": {Path: "synthetic/main.jsonl"}}}
	w := pausedTranscriptWriter(t, store)
	w.load("main")
	pin := "PIN"
	w.enqueueATIS("main", "session", nil)
	if len(w.queues) != 0 {
		t.Fatal("undefined latch enqueued metadata")
	}
	w.enqueueATIS("main", "session", &pin)
	w.enqueueATIS("main", "session", &pin)
	if len(w.queues["main"]) != 1 || len(w.scopes["main"].known) != 0 {
		t.Fatal("ATIS stamp used message UUID deduplication")
	}
	store.before = func(string, []byte) error { return ErrSDKSessionUnavailable }
	w.drain()
	w.enqueueATIS("main", "session", &pin)
	if len(w.queues) != 0 || w.failure("main") == nil {
		t.Fatal("failed native metadata was silently retried or failure cleared")
	}
	store.before = nil
	w.scopes["main"].path = "synthetic/relocated.jsonl"
	w.enqueueATIS("main", "session", &pin)
	w.drain()
	if len(store.appends) != 1 || !bytes.Contains(store.appends[0].lines, []byte("atis-latch")) {
		t.Fatal("changed actual path did not produce a new stamp")
	}
	// Separate clean writer: idle cache eviction must not forget an accepted
	// stamp during the same process lifetime.
	clean := pausedTranscriptWriter(t, store)
	clean.load("main")
	clean.enqueueATIS("main", "session", &pin)
	clean.drain()
	clean.retireScope("main")
	clean.load("main")
	clean.enqueueATIS("main", "session", &pin)
	if len(clean.queues) != 0 {
		t.Fatal("cache eviction invented a new native metadata insertion")
	}
}

func TestNativeTranscriptLegacyMetadataIsNotInventedOnRestore(t *testing.T) {
	structural, content := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
	tracker := nativeContentTestTracker(structural, content)
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic historical input"}]}`, time.Now())
	request := tracker.Begin(input)
	request.FinalizeFailure(input.StartedAt)
	scope := digest(input.AccountID, input.SessionID)
	var record map[string]json.RawMessage
	if json.Unmarshal(content.data[scope], &record) != nil {
		t.Fatal("fixture has no native checkpoint")
	}
	var historical []map[string]json.RawMessage
	if json.Unmarshal(record["messages"], &historical) != nil || len(historical) != 1 {
		t.Fatal("fixture has no single historical row")
	}
	delete(historical[0], "userType")
	record["messages"], _ = json.Marshal(historical)
	content.data[scope], _ = json.Marshal(record)
	restarted := nativeContentTestTracker(structural, content)
	input.ClientRequestID, input.PromptID = "new-request", ""
	input.StartedAt = input.StartedAt.Add(time.Hour)
	input.Body = []byte(`{"messages":[{"role":"user","content":"synthetic current input"}]}`)
	restarted.Begin(input)
	snapshot := restarted.NativeContent(input.AccountID, input.SessionID)
	if snapshot.IncompleteReason != "unobserved-sdk-native-metadata" || len(snapshot.Messages) != 2 || snapshot.Messages[0].UserType != "" || snapshot.Messages[1].UserType != "external" {
		t.Fatal("restore fabricated historical metadata or concealed the fidelity gap")
	}
	encoded, err := json.Marshal(snapshot.Messages[0])
	if err != nil || bytes.Contains(encoded, []byte("userType")) {
		t.Fatal("legacy undefined metadata was silently materialized")
	}
}
