package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type sdkMemorySessionStore struct {
	mu        sync.Mutex
	data      map[string][]byte
	revisions map[string]string
	failSave  error
	failLoad  error
	saves     int
}

func (s *sdkMemorySessionStore) Load(scope string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.data[scope]), s.revisions[scope], s.failLoad
}
func (s *sdkMemorySessionStore) Save(scope, previous string, payload []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave != nil {
		return "", s.failSave
	}
	if s.revisions[scope] != previous {
		return "", ErrSDKSessionStale
	}
	if s.data == nil {
		s.data = make(map[string][]byte)
		s.revisions = make(map[string]string)
	}
	revision := uuid.NewString()
	s.data[scope], s.revisions[scope] = bytes.Clone(payload), revision
	s.saves++
	return revision, nil
}
func sdkStateTestInput(body string, at time.Time) Input {
	return Input{AccountID: "synthetic-account", SessionID: "synthetic-session", PromptID: uuid.NewString(), ClientRequestID: uuid.NewString(), Role: "main", Body: []byte(body), StartedAt: at}
}
func sdkStateComplete(t *testing.T, r *Request, payload string, at time.Time, ms int64) {
	t.Helper()
	var response Response
	response.ObservePayloadAt([]byte(payload), false, at)
	r.ObserveSDKHistory(response.SDKHistoryMessages())
	r.ObserveSDKWireResponse(response.SDKWireFingerprint())
	first, tools := response.SDKAssistantMessage()
	r.ObserveSDKAssistantMessage(first, tools)
	stop, ids, complete := response.Outcome()
	if !complete {
		t.Fatal("test response incomplete")
	}
	r.FinishSuccess(at, stop, ids)
	r.RecordSDKAPISuccess(ms)
}

func TestSDKSessionStatePreservesUUIDsLedgerAndExactNextHistory(t *testing.T) {
	storage := &sdkMemorySessionStore{}
	tracker := NewTracker(storage)
	at := time.Now()
	initial := sdkStateTestInput(`{"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`, at)
	request := tracker.Begin(initial)
	if !request.ClaimSDKInput() {
		t.Fatal("input claim missing")
	}
	request.ObserveSDKQuery(initial.Body)
	sdkStateComplete(t, request, `{"id":"msg_private","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"PRIVATE_RESPONSE"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, at.Add(time.Millisecond), 17)
	request.SDKResultSnapshot()
	history := request.SDKHistory()
	if !history.OwnedMessagesKnown || len(history.Messages) != 2 {
		t.Fatal(history)
	}
	saved, _ := storage.data[digest(initial.AccountID, initial.SessionID)]
	if bytes.Contains(saved, []byte("PRIVATE_PROMPT")) || bytes.Contains(saved, []byte("PRIVATE_RESPONSE")) {
		t.Fatal("content entered structural state")
	}

	restarted := NewTracker(storage)
	next := sdkStateTestInput(`{"messages":[{"role":"user","content":"PRIVATE_PROMPT"},{"role":"assistant","content":"PRIVATE_RESPONSE"},{"role":"user","content":"followup"}]}`, at.Add(2*time.Hour))
	followup := restarted.Begin(next)
	followup.ObserveSDKQuery(next.Body)
	restored := followup.SDKHistory()
	if followup.SDKSessionStateError() != nil || !restored.OwnedMessagesKnown || len(restored.Messages) != 3 {
		t.Fatal(restored, followup.SDKSessionStateError())
	}
	if !reflect.DeepEqual(restored.Messages[:2], history.Messages) {
		t.Fatal("native UUID/usage history was regenerated")
	}
	if restored.Messages[2].UUID == history.Messages[0].UUID || !followup.Identity().StartsPrompt || followup.Identity().PromptID == request.Identity().PromptID {
		t.Fatal("new submission reused prior identity")
	}
	if followup.call.state.sdk.ledger.durationMS != 17 || followup.call.state.sdk.apiBaselineMS != 17 {
		t.Fatal("session ledger was reset")
	}
	if _, err := followup.CompactionView(next.Body); err != nil {
		t.Fatal("restored history cannot feed actual compaction", err)
	}
	if snapshot := followup.Snapshot().SDK; snapshot.NumTurns != 1 || snapshot.APIDurationMS != 0 {
		t.Fatal(snapshot)
	}
}

func TestSDKSessionStateResumesActualPendingToolOwner(t *testing.T) {
	storage := &sdkMemorySessionStore{}
	tracker := NewTracker(storage)
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"read synthetic input"}],"tools":[]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	sdkStateComplete(t, request, `{"id":"msg_tool","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"tool_owned","name":"Read","input":{"file_path":"C:/synthetic/not-opened"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":3}}`, at.Add(time.Millisecond), 11)
	before := request.SDKHistory()
	restarted := NewTracker(storage)
	next := sdkStateTestInput(`{"messages":[{"role":"user","content":"read synthetic input"},{"role":"assistant","content":[{"type":"tool_use","id":"tool_owned","name":"Read","input":{"file_path":"C:/synthetic/not-opened"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_owned","content":"PRIVATE_TOOL_RESULT"}]}],"tools":[]}`, at.Add(2*time.Millisecond))
	followup := restarted.Begin(next)
	followup.ObserveSDKQuery(next.Body)
	if followup.SDKSessionStateError() != nil {
		t.Fatal(followup.SDKSessionStateError())
	}
	if followup.Identity().StartsPrompt || followup.Identity().PromptID != request.Identity().PromptID || followup.Identity().QueryDepth != 1 {
		t.Fatal("pending tool lost actual owner", followup.Identity())
	}
	if got := followup.SDKHistory().Messages; len(got) != len(before.Messages)+1 || !reflect.DeepEqual(got[:len(before.Messages)], before.Messages) {
		t.Fatal("tool continuation regenerated history")
	}
	sdkStateComplete(t, followup, `{"id":"msg_after_tool","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`, at.Add(3*time.Millisecond), 23)
	snapshot := followup.Snapshot()
	if !snapshot.Complete || snapshot.PendingTools != 0 || snapshot.ToolResults != 1 || snapshot.SDK.NumTurns != 2 || snapshot.SDK.APIDurationMS != 34 || snapshot.SDK.ToolUseCount != 1 {
		t.Fatal(snapshot)
	}
}

func TestSDKSessionStateInterruptedResponsePreservesObservedUUIDsWithoutSuccess(t *testing.T) {
	storage := &sdkMemorySessionStore{}
	tracker := NewTracker(storage)
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic pending"}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	for _, line := range []string{
		`data: {"type":"message_start","message":{"id":"msg_pending","role":"assistant","model":"claude-opus-5","usage":{"input_tokens":3,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	} {
		response.ObserveStreamLineAt([]byte(line), at)
	}
	request.ObserveSDKHistory(response.SDKHistoryMessages())
	if err := request.CheckpointSDKSessionState(); err != nil {
		t.Fatal(err)
	}
	prior := request.SDKHistory()
	restarted := NewTracker(storage)
	next := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic pending"},{"role":"assistant","content":"partial"},{"role":"user","content":"continue"}]}`, at.Add(time.Second))
	followup := restarted.Begin(next)
	followup.ObserveSDKQuery(next.Body)
	got := followup.SDKHistory()
	if followup.SDKSessionStateError() == nil || got.OwnedMessagesKnown || got.IncompleteReason != "sdk-session-interrupted-during-query" {
		t.Fatal(got, followup.SDKSessionStateError())
	}
	if !reflect.DeepEqual(got.Messages[:len(prior.Messages)], prior.Messages) {
		t.Fatal("observed partial UUIDs lost")
	}
	old := restarted.prompts[request.call.state.key]
	if old.active != nil || old.apiCalls != 0 || old.sdk.ledger.durationMS != 0 || old.complete {
		t.Fatal("restart invented a completed query")
	}
}

func TestSDKSessionStateExplicitHelperRestoresBeyondMemoryTTL(t *testing.T) {
	storage := &sdkMemorySessionStore{}
	tracker := NewTracker(storage)
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	sdkStateComplete(t, request, `{"id":"msg_helper_owner","type":"message","role":"assistant","content":[{"type":"text","text":"response"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, at.Add(time.Second), 17)
	sealed := request.SDKResultSnapshot()
	restarted := NewTracker(storage)
	helper := restarted.BindSDKHelper(Input{AccountID: input.AccountID, SessionID: input.SessionID, ParentPromptID: request.Identity().PromptID, StartedAt: at.Add(2 * time.Hour)})
	if helper == nil || helper.SDKSessionStateError() != nil {
		t.Fatal("memory TTL discarded an explicitly bound durable owner")
	}
	helper.RecordSuccess("title", "synthetic_helper", 9)
	helper.RecordSuccess("title", "synthetic_helper", 9)
	// A restart intentionally discards process-local monotonic clock data.
	before, _ := json.Marshal(sealed)
	after, _ := json.Marshal(helper.Snapshot())
	if !bytes.Equal(before, after) {
		t.Fatal("late helper rewrote the original frozen result")
	}
	nextInput := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"},{"role":"assistant","content":"response"},{"role":"user","content":"next"}]}`, at.Add(2*time.Hour+time.Second))
	next := restarted.Begin(nextInput)
	next.ObserveSDKQuery(nextInput.Body)
	if next.SDKSessionStateError() != nil || next.call.state.sdk.ledger.durationMS != 26 || next.call.state.sdk.apiBaselineMS != 26 {
		t.Fatal("persisted helper callback lost or duplicated its actual duration")
	}
}

func TestSDKSessionStateCorruptionFailureAndForeignScopeStaySeparate(t *testing.T) {
	for _, kind := range []string{"load", "invalid", "save"} {
		t.Run(kind, func(t *testing.T) {
			storage := &sdkMemorySessionStore{}
			input := sdkStateTestInput(`{"messages":[{"role":"user","content":"private"}]}`, time.Now())
			scope := digest(input.AccountID, input.SessionID)
			switch kind {
			case "load":
				storage.failLoad = errors.New("synthetic read failure")
			case "invalid":
				storage.data = map[string][]byte{scope: []byte(`{"version":1}`)}
				storage.revisions = map[string]string{scope: "old"}
			case "save":
				storage.failSave = errors.New("synthetic write failure")
			}
			tracker := NewTracker(storage)
			request := tracker.Begin(input)
			request.ObserveSDKQuery(input.Body)
			if request.SDKSessionStateError() == nil {
				t.Fatal("lost checkpoint silently became healthy")
			}
			if storage.saves != 0 {
				t.Fatal("unreadable original overwritten")
			}
			if kind == "save" {
				storage.failSave = nil
				if request.CheckpointSDKSessionState() == nil {
					t.Fatal("later write erased original loss")
				}
				if storage.saves == 0 {
					t.Fatal("transient write could not retry")
				}
				restarted := NewTracker(storage)
				next := restarted.Begin(input)
				if next.SDKSessionStateError() == nil {
					t.Fatal("restart erased retained state issue")
				}
			}
		})
	}
	storage := &sdkMemorySessionStore{}
	tracker := NewTracker(storage)
	first := sdkStateTestInput(`{"messages":[{"role":"user","content":"private"}]}`, time.Now())
	request := tracker.Begin(first)
	request.ObserveSDKQuery(first.Body)
	sdkStateComplete(t, request, `{"id":"msg_owner","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"response"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, time.Now(), 7)
	for _, field := range []string{"account", "session"} {
		restarted := NewTracker(storage)
		foreign := first
		foreign.ClientRequestID = uuid.NewString()
		if field == "account" {
			foreign.AccountID += "-foreign"
		} else {
			foreign.SessionID += "-foreign"
		}
		next := restarted.Begin(foreign)
		if len(next.SDKHistory().Messages) != 1 || next.call.state.sdk.ledger.durationMS != 0 {
			t.Fatal("cross-scope restoration")
		}
	}
}

func TestSDKSessionStateCheckpointFieldCoverage(t *testing.T) {
	for _, tc := range []struct {
		kind  reflect.Type
		names []string
	}{
		{reflect.TypeOf(state{}), []string{"sdkInputClaimed", "controlInputStarted", "failed", "key", "identity", "startedAt", "lastAt", "finishedAt", "firstByteAt", "firstSessionPrompt", "pending", "apiCalls", "apiDuration", "toolRequests", "toolResults", "incompleteReason", "complete", "sdk", "scope", "requests", "active"}},
		{reflect.TypeOf(call{}), []string{"reactiveCompactionFailure", "retryReady", "sdkUserHistoryOffsets", "reconcileHistory", "sdkWireInputs", "sdkWireInputKnown", "sdkWireRequestDigest", "sdkAssistantYields", "compactionKey", "compactionUnknownYields", "sdkQueryObserved", "sdkAPIRecorded", "identity", "inputDigest", "startedAt", "resultIDs", "attempt", "settled", "succeeded", "state"}},
		{reflect.TypeOf(sdkAccounting{}), []string{"reactiveCompactionAttempted", "recoveredAPIFailures", "compactionSummaryUserYields", "unknownCompactionYields", "pendingCompactions", "toolResultUserYields", "interruptionUserYields", "catalogComplete", "retryStatus", "queries", "numTurns", "apiSuccesses", "apiBaselineMS", "firstAssistantMessageAt", "sawRetry", "sawCompact", "cancelledStreaming", "unknownUserYields", "mcpAliases", "helpers", "incompleteReason", "result", "history", "ledger", "tools", "compactions"}},
		{reflect.TypeOf(sdkHistory{}), []string{"messages", "incompleteReason", "expectedText", "expectedTextKnown", "pendingReconciliations"}},
	} {
		actual := make([]string, tc.kind.NumField())
		for i := range actual {
			actual[i] = tc.kind.Field(i).Name
		}
		// Live callbacks belong to this process, not to the checkpoint.
		// Keep them in this exhaustive classification without restoring leases.
		if tc.kind == reflect.TypeOf(state{}) {
			tc.names = append(tc.names, "helperOwners")
		}
		if tc.kind == reflect.TypeOf(call{}) {
			// A pre-dispatch carrier proof is bound to the current physical
			// transformation. Never restore it as authority over a future body.
			tc.names = append(tc.names, "sdkAPICallbackPending", "sdkInstructionProjection")
		}
		sort.Strings(actual)
		sort.Strings(tc.names)
		if !reflect.DeepEqual(actual, tc.names) {
			t.Fatalf("%s has state not covered by its checkpoint schema: %v != %v", tc.kind.Name(), actual, tc.names)
		}
	}
}

func TestSDKSessionStateCodecRoundTripPreservesDispositionAndOwnership(t *testing.T) {
	tracker := NewTracker(nil)
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"}]}`, time.Now())
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	s := request.call.state
	helper := tracker.BindSDKHelper(Input{AccountID: input.AccountID, SessionID: input.SessionID, ParentPromptID: request.Identity().PromptID, StartedAt: input.StartedAt})
	if helper == nil || s.helperOwners != 1 {
		t.Fatal("test did not acquire a process-local helper lease")
	}
	defer helper.Close()
	s.sdk.compactions = map[string]*sdkCompaction{digest("compact"): {
		observation: SDKCompactionObservation{input: SDKCompactionInput{toolResults: map[string]struct{}{digest("tool"): {}}, known: true}, summary: SDKCompactionSummary{bytes: 3, sha256: digest("summary"), reviewed: true}}, applied: true}}
	s.sdk.tools = map[string]sdkToolOccurrences{digest("Read"): {count: 2, kind: "builtin"}}
	s.sdk.helpers = map[string]struct{}{digest("helper"): {}}
	s.sdk.history.expectedText = []string{digest("text")}
	s.sdk.history.expectedTextKnown = true
	s.sdk.history.pendingReconciliations = 1
	s.active.sdkAPICallbackPending = true
	s.active.sdkInstructionProjection = &sdkInstructionProjection{before: []string{digest("native")}, after: []string{digest("wire")}}
	stored := storeSDKPrompt(s)
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var detached sdkStoredPrompt
	if err = json.Unmarshal(encoded, &detached); err != nil {
		t.Fatal(err)
	}
	h := restoreSDKHistory(storeSDKHistory(s.sdk.history))
	ledger := &sdkAPILedger{durationMS: 12, incompleteReason: "synthetic-retained"}
	restored := restoreSDKPrompt(detached, s.scope, h, ledger)
	// JSON preserves wall clocks, not process-local monotonic clock readings.
	reencoded, err := json.Marshal(storeSDKPrompt(restored))
	if err != nil || !bytes.Equal(reencoded, encoded) {
		t.Fatal("checkpoint lost private state")
	}
	if restored.active != restored.requests[digest(input.ClientRequestID)] || restored.active.state != restored || restored.sdk.history != h || restored.sdk.ledger != ledger {
		t.Fatal("checkpoint disconnected ownership pointers")
	}
	if restored.helperOwners != 0 || restored.active.sdkAPICallbackPending || restored.active.sdkInstructionProjection != nil {
		t.Fatal("checkpoint resurrected a process-local callback lease")
	}
	// Closing also updates last activity. Compare the lease field alone so
	// this schema check does not depend on the host wall-clock resolution.
	withoutOwner := *s
	withoutOwner.helperOwners = 0
	withoutLease, err := json.Marshal(storeSDKPrompt(&withoutOwner))
	if err != nil || !bytes.Equal(withoutLease, encoded) {
		t.Fatal("process-local helper lease entered the durable checkpoint")
	}
}

func TestSDKSessionStateRejectsInvalidNestedFacts(t *testing.T) {
	storage := &sdkMemorySessionStore{}
	tracker := NewTracker(storage)
	at := time.Now()
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic"}]}`, at)
	r := tracker.Begin(input)
	r.ObserveSDKQuery(input.Body)
	sdkStateComplete(t, r, `{"id":"msg_state","type":"message","role":"assistant","content":[{"type":"text","text":"synthetic response"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, at.Add(time.Second), 5)
	r.SDKResultSnapshot()
	scope := r.call.state.scope
	payload := storage.data[scope]
	for _, test := range []struct {
		name   string
		mutate func(*sdkStoredSession, *sdkStoredPrompt, *sdkStoredCall)
	}{
		{"usage-negative", func(s *sdkStoredSession, _ *sdkStoredPrompt, _ *sdkStoredCall) {
			s.History.Messages[0].Usage.InputTokens = -1
		}},
		{"usage-overflow", func(s *sdkStoredSession, _ *sdkStoredPrompt, _ *sdkStoredCall) {
			s.History.Messages[0].Usage = SDKTokenUsage{InputTokens: 1<<63 - 1, OutputTokens: 1}
		}},
		{"wire-parent", func(s *sdkStoredSession, _ *sdkStoredPrompt, _ *sdkStoredCall) {
			s.History.Messages[0].WireParentUUID = "private-invalid"
		}},
		{"wire-tool-result-hash", func(s *sdkStoredSession, _ *sdkStoredPrompt, _ *sdkStoredCall) {
			s.History.Messages[0].WireToolResultID = "private-invalid"
		}},
		{"wire-tool-result-role", func(s *sdkStoredSession, _ *sdkStoredPrompt, _ *sdkStoredCall) {
			s.History.Messages[1].WireToolResultID = digest("synthetic-tool")
		}},
		{"duplicate-uuid", func(s *sdkStoredSession, _ *sdkStoredPrompt, _ *sdkStoredCall) {
			s.History.Messages[1].UUID = s.History.Messages[0].UUID
		}},
		{"prompt-depth", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) { p.Identity.QueryDepth = -1 }},
		{"query-count", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) { p.Accounting.Queries = -1 }},
		{"tool-count", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Tools = map[string]sdkStoredTool{digest("tool"): {Count: -1, Kind: "builtin"}}
		}},
		{"tool-kind", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Tools = map[string]sdkStoredTool{digest("tool"): {Count: 1, Kind: "private-invalid"}}
		}},
		{"helper-id", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Helpers = map[string]struct{}{"private-invalid": {}}
		}},
		{"mcp-alias", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.McpAliases = map[string]struct{}{"private-invalid": {}}
		}},
		{"compact-disposition", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Compactions = map[string]sdkStoredCompaction{digest("compact"): {Applied: true, Discarded: true}}
		}},
		{"compact-length", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Compactions = map[string]sdkStoredCompaction{digest("compact"): {SummaryBytes: -1}}
		}},
		{"compact-summary", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Compactions = map[string]sdkStoredCompaction{digest("compact"): {SummaryReviewed: true}}
		}},
		{"compact-tool-id", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Compactions = map[string]sdkStoredCompaction{digest("compact"): {ToolResults: map[string]struct{}{"private-invalid": {}}}}
		}},
		{"result-duration", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) {
			p.Accounting.Result.APIDurationMS = -1
		}},
		{"result-turn-count", func(_ *sdkStoredSession, p *sdkStoredPrompt, _ *sdkStoredCall) { p.Accounting.Result.NumTurns = -1 }},
		{"wire-hash", func(_ *sdkStoredSession, _ *sdkStoredPrompt, c *sdkStoredCall) {
			c.SdkWireRequestDigest = "private-invalid"
		}},
		{"wire-input", func(_ *sdkStoredSession, _ *sdkStoredPrompt, c *sdkStoredCall) {
			c.SdkWireInputs = []string{"private-invalid"}
		}},
		{"result-id", func(_ *sdkStoredSession, _ *sdkStoredPrompt, c *sdkStoredCall) {
			c.ResultIDs = []string{"private-invalid"}
		}},
		{"compact-key", func(_ *sdkStoredSession, _ *sdkStoredPrompt, c *sdkStoredCall) { c.CompactionKey = "private-invalid" }},
		{"unsettled-success", func(_ *sdkStoredSession, _ *sdkStoredPrompt, c *sdkStoredCall) { c.Settled = false }},
		{"unobserved-api-success", func(_ *sdkStoredSession, _ *sdkStoredPrompt, c *sdkStoredCall) { c.Succeeded = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := decodeSDKSession(payload, scope)
			if err != nil {
				t.Fatal("valid producer checkpoint rejected", err)
			}
			p := record.Prompts[r.call.state.key]
			requestKey := digest(input.ClientRequestID)
			c := p.Requests[requestKey]
			test.mutate(&record, &p, &c)
			p.Requests[requestKey] = c
			record.Prompts[r.call.state.key] = p
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeSDKSession(encoded, scope); !errors.Is(err, ErrSDKSessionInvalid) {
				t.Fatal("invalid nested state entered native ownership")
			}
		})
	}
}
