package prompt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func hydrationFixture(t *testing.T, lines string, anchor *string) (*SDKRemoteHydration, *sdkMemorySessionStore) {
	t.Helper()
	store := &sdkMemorySessionStore{}
	tracker := NewTracker(&sdkMemorySessionStore{}, SDKNativeContentOptions{Store: &sdkMemorySessionStore{}, RemoteTranscriptStore: store})
	t.Cleanup(func() { _ = tracker.Close() })
	h, err := tracker.PrepareRemoteHydration("a", "s", "cse_test")
	if err != nil {
		t.Fatal(err)
	}
	h.record.Lines, h.record.Anchor = []byte(lines), anchor
	return h, store
}

func hydrationEvent(id, kind string) SDKHydrationEvent {
	raw, _ := json.Marshal(map[string]any{"uuid": id, "type": kind})
	return SDKHydrationEvent{Payload: raw}
}

func TestRemoteHydrationForegroundGuardAndDelta(t *testing.T) {
	for _, tc := range []struct {
		name, local, anchor, fallback string
		delta, applied                bool
		ids                           []string
	}{
		{"full", "", "", "client-gated", false, true, []string{"remote"}},
		{"zero-content-preserves-local", "{\"uuid\":\"local\",\"type\":\"user\"}\n", "", "client-gated", false, false, nil},
		{"delta-dedups-tail", "{\"uuid\":\"tip\",\"type\":\"user\"}\n{\"uuid\":\"seen\",\"type\":\"assistant\"}\n", "tip", "", true, true, []string{"seen", "remote"}},
		{"anchor-returned-full", "{\"uuid\":\"tip\",\"type\":\"user\"}\n", "tip", "anchor-in-response", false, true, []string{"tip", "remote"}},
		{"tip-outside-tail", "{\"uuid\":\"local\",\"type\":\"user\"}\n", "old", "tip-not-in-tail", false, true, []string{"remote"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var anchor *string
			if tc.anchor != "" {
				anchor = &tc.anchor
			}
			h, store := hydrationFixture(t, tc.local, anchor)
			var events []SDKHydrationEvent
			for _, id := range tc.ids {
				events = append(events, hydrationEvent(id, "user"))
			}
			err := h.Run(t.Context(), SDKHydrationReaders{DeltaEnabled: anchor != nil, Foreground: func(_ context.Context, a string) (*SDKHydrationRead, error) {
				want := tc.anchor
				if tc.name == "tip-outside-tail" {
					want = ""
				}
				if a != want {
					t.Fatal("unexpected anchor", a, want)
				}
				return &SDKHydrationRead{Events: events}, nil
			}})
			if err != nil || h.status.Delta != tc.delta || h.status.Applied != tc.applied || h.status.Fallback != tc.fallback {
				t.Fatal("wrong transition", err, h.status)
			}
			if !tc.applied && (store.saves != 0 || string(h.record.Lines) != tc.local) {
				t.Fatal("zero-content replacement destroyed local history")
			}
			if tc.delta && bytes.Count(h.record.Lines, []byte(`"uuid":"seen"`)) != 1 {
				t.Fatal("delta repeated local row")
			}
		})
	}
}

func TestRemoteHydrationTailRefetchAndAnchorFallback(t *testing.T) {
	for _, fallback := range []string{"", "rejected", "not-found"} {
		t.Run(fallback, func(t *testing.T) {
			anchor := "tip"
			h, _ := hydrationFixture(t, "{\"uuid\":\"tip\",\"type\":\"user\"}", &anchor)
			var calls []string
			err := h.Run(t.Context(), SDKHydrationReaders{DeltaEnabled: true, Foreground: func(_ context.Context, a string) (*SDKHydrationRead, error) {
				calls = append(calls, a)
				id := "full"
				if len(calls) == 1 && fallback == "" {
					id = "partial"
				}
				return &SDKHydrationRead{Events: []SDKHydrationEvent{hydrationEvent(id, "user")}, AnchorFallback: fallback}, nil
			}})
			if err != nil || h.status.Delta || !bytes.Contains(h.record.Lines, []byte("full")) || bytes.Contains(h.record.Lines, []byte("partial")) {
				t.Fatal("incorrect full fallback", err, h.status)
			}
			want := []string{"tip"}
			if fallback == "" {
				want = append(want, "")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal("incorrect reads", calls)
			}
		})
	}
}

func TestRemoteHydrationNullIDIsNotAnAbsentAnchorIdentity(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			anchor := "tip"
			local := "{\"uuid\":\"prefix\",\"type\":\"user\"}\n{\"uuid\":\"tip\",\"type\":\"assistant\"}\n"
			h, _ := hydrationFixture(t, local, &anchor)
			event := hydrationEvent("tip", "assistant")
			event.EventIDPresent = present
			err := h.Run(t.Context(), SDKHydrationReaders{DeltaEnabled: true, Foreground: func(context.Context, string) (*SDKHydrationRead, error) {
				return &SDKHydrationRead{Events: []SDKHydrationEvent{event, hydrationEvent("new", "user")}}, nil
			}})
			if err != nil || h.status.Delta != present || bytes.Contains(h.record.Lines, []byte("prefix")) != present || *h.record.Anchor != "new" {
				t.Fatal("null/absent event ID changed native replacement policy", err, h.status)
			}
		})
	}
	// Tip persistence uses ?? instead: an explicit null still falls back to
	// the payload UUID when choosing the next successfully persisted anchor.
	event := hydrationEvent("tip", "assistant")
	event.EventIDPresent = true
	if tip := sdkHydrationTip([]SDKHydrationEvent{event}); tip == nil || *tip != "tip" {
		t.Fatal("nullish tip fallback was changed with anchor comparison")
	}
}

func TestRemoteHydrationFailuresDoNotPublishPrefixOrAdvanceTip(t *testing.T) {
	for _, mode := range []string{"read", "full-refetch", "write", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			anchor := "tip"
			local := "{\"uuid\":\"tip\",\"type\":\"user\"}"
			h, store := hydrationFixture(t, local, &anchor)
			if mode == "write" {
				store.failSave = errors.New("offline synthetic failure")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			err := h.Run(ctx, SDKHydrationReaders{DeltaEnabled: true, Foreground: func(context.Context, string) (*SDKHydrationRead, error) {
				calls++
				if mode == "cancel" {
					cancel()
				}
				if mode == "read" || mode == "full-refetch" && calls == 2 {
					return nil, errors.New("synthetic read failure")
				}
				return &SDKHydrationRead{Events: []SDKHydrationEvent{hydrationEvent("remote", "user")}}, nil
			}})
			if err == nil || store.saves != 0 || string(h.record.Lines) != local || *h.record.Anchor != "tip" {
				t.Fatal("failed hydration published data", err, h.status)
			}
		})
	}
}

func TestRemoteHydrationSubagentIsolationAndModes(t *testing.T) {
	for _, mode := range []string{"eager", "lazy", "skipped_delta"} {
		t.Run(mode, func(t *testing.T) {
			anchor := "tip"
			h, store := hydrationFixture(t, "{\"uuid\":\"tip\",\"type\":\"user\"}\n", &anchor)
			called := false
			err := h.Run(t.Context(), SDKHydrationReaders{DeltaEnabled: true, LazySubagents: mode == "lazy", SkipSubagentsOnDelta: mode == "skipped_delta",
				Foreground: func(context.Context, string) (*SDKHydrationRead, error) { return &SDKHydrationRead{}, nil },
				Subagents: func(context.Context) (*SDKHydrationRead, error) {
					called = true
					var events []SDKHydrationEvent
					for _, id := range []string{"good-agent_1", "../escape", "C:/escape", "空", ""} {
						event := hydrationEvent("agent-row", "user")
						event.AgentID = id
						events = append(events, event)
					}
					zero := hydrationEvent("metadata", "system")
					zero.AgentID = "zero-content"
					events = append(events, zero)
					return &SDKHydrationRead{Events: events}, nil
				}})
			if err != nil || h.status.SubagentMode != mode || called != (mode == "eager") {
				t.Fatal("wrong policy", err, h.status)
			}
			want := 1
			if called {
				want = 2
			}
			if len(store.data) != want {
				t.Fatal("unsafe or zero-content agent written", len(store.data))
			}
		})
	}
}

func TestRemoteHydrationAdoptsRevisedHistoryWithoutRewritingOriginals(t *testing.T) {
	tracker, input, structural, content, transcript := nativeResumeTestState(t)
	remote := &sdkMemorySessionStore{}
	tracker.nativeOptions.RemoteTranscriptStore = remote
	defer tracker.Close()
	before := tracker.NativeContent(input.AccountID, input.SessionID)
	h, err := tracker.PrepareRemoteHydration(input.AccountID, input.SessionID, "cse_owned")
	if err != nil {
		t.Fatal(err)
	}
	var events []SDKHydrationEvent
	for _, row := range before.Messages {
		if row.Type == "assistant" {
			row.Message = bytes.ReplaceAll(row.Message, []byte("PRIVATE_REPLY"), []byte("SERVER_REVISED_REPLY"))
		}
		raw, _ := json.Marshal(row)
		events = append(events, SDKHydrationEvent{Payload: raw})
	}
	if err := h.Run(t.Context(), SDKHydrationReaders{Foreground: func(context.Context, string) (*SDKHydrationRead, error) {
		return &SDKHydrationRead{Events: events}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	resume, err := h.AdoptCompletedHistory()
	if err != nil {
		t.Fatal("adopt", err)
	}
	wire, err := resume.Read()
	if err != nil || len(wire) != 2 || !bytes.Contains(wire[1], []byte("SERVER_REVISED_REPLY")) {
		t.Fatal("adoption did not reach wire", err)
	}
	after := tracker.NativeContent(input.AccountID, input.SessionID)
	if !reflect.DeepEqual(after.Messages, before.Messages) || len(after.ProjectedMessages) != 2 {
		t.Fatal("immutable originals were changed")
	}
	// Execute another real tracker turn against the adopted projection.
	wire = append(wire, json.RawMessage(`{"role":"user","content":"NEXT_INPUT"}`))
	body, _ := json.Marshal(map[string]any{"messages": wire})
	next := input
	next.Body = body
	next.PromptID = uuid.NewString()
	next.ClientRequestID = uuid.NewString()
	next.StartedAt = time.Now().UTC().Truncate(time.Millisecond)
	request := tracker.Begin(next)
	request.ObserveSDKQuery(body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"id":"msg_next","type":"message","role":"assistant","content":[{"type":"text","text":"NEXT_REPLY"}],"stop_reason":"end_turn","usage":{"input_tokens":14,"output_tokens":3}}`), false, next.StartedAt.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(next.StartedAt.Add(time.Millisecond), "end_turn", nil)
	request.RecordSDKAPISuccess(1)
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript, RemoteTranscriptStore: remote})
	defer restarted.Close()
	prepared, err := restarted.PrepareRemoteWireResume(input.AccountID, input.SessionID)
	if err != nil {
		t.Fatal("restart after projected turn", err)
	}
	rows, err := prepared.Read()
	if err != nil || len(rows) != 4 || !bytes.Contains(rows[1], []byte("SERVER_REVISED_REPLY")) || !bytes.Contains(rows[3], []byte("NEXT_REPLY")) {
		t.Fatal("live projection lost on restart", err)
	}
	original := restarted.NativeContent(input.AccountID, input.SessionID)
	if !reflect.DeepEqual(original.Messages[:2], before.Messages) {
		t.Fatal("restart replaced original content")
	}
}

func TestRemoteHydrationInitialHistoryDoesNotInventLocalRequests(t *testing.T) {
	testRemoteHydrationInitialSelection(t, false)
}

func TestRemoteHydrationExplicitClearPersistsWithoutInventingRequests(t *testing.T) {
	testRemoteHydrationInitialSelection(t, true)
}

func testRemoteHydrationInitialSelection(t *testing.T, cleared bool) {
	t.Helper()
	structural, content, remote := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
	transcript := &sdkReadableTranscriptStore{}
	options := SDKNativeContentOptions{Store: content, TranscriptStore: transcript, RemoteTranscriptStore: remote}
	tracker := NewTracker(structural, options)
	defer tracker.Close()
	session := uuid.NewString()
	h, err := tracker.PrepareRemoteHydration("account", session, "cse_remote_only")
	if err != nil {
		t.Fatal(err)
	}
	userID, assistantID := uuid.NewString(), uuid.NewString()
	rows := []SDKNativeMessage{
		{Type: "user", UUID: userID, Timestamp: "2026-09-07T10:00:00.000Z", SessionID: session, Message: json.RawMessage(`{"role":"user","content":"REMOTE_USER"}`)},
		{Type: "assistant", UUID: assistantID, ParentUUID: &userID, Timestamp: "2026-09-07T10:00:01.000Z", SessionID: session, Message: json.RawMessage(`{"id":"msg_remote","role":"assistant","content":[{"type":"text","text":"REMOTE_REPLY"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)},
	}
	var events []SDKHydrationEvent
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		events = append(events, SDKHydrationEvent{Payload: raw})
	}
	wantHistory := 2
	if cleared {
		wantHistory = 0
		events = append(events, SDKHydrationEvent{Payload: json.RawMessage(`{"type":"last-prompt","leafUuid":null,"explicit":true}`)})
	}
	if err := h.Run(t.Context(), SDKHydrationReaders{Foreground: func(context.Context, string) (*SDKHydrationRead, error) {
		return &SDKHydrationRead{Events: events}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	resume, err := h.AdoptCompletedHistory()
	if err != nil {
		t.Fatal("initial adopt", err)
	}
	wire, err := resume.Read()
	if err != nil || len(wire) != wantHistory || len(tracker.prompts) != 0 || len(transcript.appends) != 0 {
		t.Fatal("initial adoption invented a prompt or transcript append", err)
	}
	payload, _, _ := structural.Load(digest("account", session))
	record, err := decodeSDKSession(payload, digest("account", session))
	if err != nil || !record.HistoryOnly || record.HistoryCleared != cleared || len(record.Prompts) != 0 || record.DurationMS != 0 {
		t.Fatal("remote history claimed API accounting", err)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, options)
	defer restarted.Close()
	prepared, err := restarted.PrepareRemoteWireResume("account", session)
	if err != nil {
		t.Fatal("history-only restart", err)
	}
	wire, err = prepared.Read()
	if err != nil || len(wire) != wantHistory || len(restarted.prompts) != 0 {
		t.Fatal("restart invented ownership", err)
	}
	wire = append(wire, json.RawMessage(`{"role":"user","content":"LOCAL_FIRST_INPUT"}`))
	body, _ := json.Marshal(map[string]any{"messages": wire})
	input := sdkStateTestInput(string(body), time.Now().UTC().Truncate(time.Millisecond))
	input.AccountID, input.SessionID = "account", session
	request := restarted.Begin(input)
	request.ObserveSDKQuery(body)
	if snapshot := request.SDKHistory(); !snapshot.OwnedMessagesKnown || len(snapshot.Messages) != wantHistory+1 {
		t.Fatal("first local turn lost restored history", snapshot.IncompleteReason)
	}
	if len(restarted.prompts) != 1 || request.call.state.sdk.ledger.durationMS != 0 {
		t.Fatal("remote history was counted as local requests")
	}
	original := restarted.NativeContent("account", session)
	if len(original.Messages) != 1 {
		t.Fatal("first local row missing")
	}
	parent := original.Messages[0].ParentUUID
	if cleared && parent != nil || !cleared && (parent == nil || *parent != assistantID) {
		t.Fatal("first local row did not follow the selected parent")
	}
	request.FinishFailure()
}

func TestRemoteHydrationFrozenTranscriptRetainsProvenLocalCompletion(t *testing.T) {
	for _, mode := range []string{"delta", "full", "changed-content", "changed-identity", "unfinished-checkpoint", "explicit-pending-tool", "changed-metadata", "other-local-branch"} {
		t.Run(mode, func(t *testing.T) {
			tracker, input, _, _, transcript := nativeResumeTestState(t)
			defer tracker.Close()
			tracker.nativeOptions.RemoteTranscriptStore = &sdkMemorySessionStore{}
			before := tracker.NativeContent(input.AccountID, input.SessionID)
			// A native JSONL row can flush before the terminal message_delta.
			// Its original bytes stay frozen while the owned checkpoint advances.
			transcript.mutate = func(lines []byte) []byte {
				return bytes.ReplaceAll(lines, []byte(`"stop_reason":"end_turn"`), []byte(`"stop_reason":null`))
			}
			h, err := tracker.PrepareRemoteHydration(input.AccountID, input.SessionID, "cse_frozen_completion")
			if err != nil {
				t.Fatal(err)
			}
			last := len(before.Messages) - 1
			h.record.Anchor = &before.Messages[last].UUID
			original := bytes.Clone(h.record.Lines)
			var events []SDKHydrationEvent
			if mode != "delta" {
				for _, raw := range sdkHydrationRows(original) {
					switch mode {
					case "changed-content":
						raw = bytes.ReplaceAll(raw, []byte("PRIVATE_REPLY"), []byte("SERVER_CHANGED_REPLY"))
					case "changed-identity":
						raw = bytes.ReplaceAll(raw, []byte("msg_native_resume"), []byte("msg_other_identity"))
					case "explicit-pending-tool":
						raw = bytes.ReplaceAll(raw, []byte(`"stop_reason":null`), []byte(`"stop_reason":"tool_use"`))
					case "changed-metadata":
						raw = bytes.ReplaceAll(raw, []byte(`"entrypoint":"claude-desktop"`), []byte(`"entrypoint":"other"`))
					}
					events = append(events, SDKHydrationEvent{Payload: raw})
				}
			}
			if mode == "unfinished-checkpoint" {
				h.local.ActiveMessages[last].Message = bytes.ReplaceAll(h.local.ActiveMessages[last].Message, []byte(`"stop_reason":"end_turn"`), []byte(`"stop_reason":null`))
			}
			if mode == "other-local-branch" {
				h.local.ActiveMessages[last].UUID = uuid.NewString()
			}
			if err := h.Run(t.Context(), SDKHydrationReaders{DeltaEnabled: mode == "delta", Foreground: func(context.Context, string) (*SDKHydrationRead, error) {
				return &SDKHydrationRead{Events: events}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			resume, err := h.AdoptCompletedHistory()
			if mode != "delta" && mode != "full" {
				if !errors.Is(err, ErrSDKResumeReconstructionRequired) || resume != nil {
					t.Fatal("unproven completion accepted", mode, err)
				}
				return
			}
			if err != nil || resume == nil {
				t.Fatal("frozen transcript erased proven local completion", err)
			}
			wire, err := resume.Read()
			if err != nil || len(wire) != 2 || !bytes.Contains(wire[1], []byte("PRIVATE_REPLY")) {
				t.Fatal("completion repair lost committed wire history", err)
			}
			if !bytes.Equal(h.record.Lines, original) || !reflect.DeepEqual(before.Messages, tracker.NativeContent(input.AccountID, input.SessionID).Messages) {
				t.Fatal("completion repair rewrote immutable originals or mirror")
			}
		})
	}
}
