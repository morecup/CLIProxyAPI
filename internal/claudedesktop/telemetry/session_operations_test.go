package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDesktopRecordRendererIdentityStopAndLateCompletion(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 6, 23, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts(uuid.NewString())
	facts.DesktopSessionID, facts.QueryID = "local_"+uuid.NewString(), uuid.NewString()
	query, cancel := context.WithCancel(t.Context())
	defer cancel()
	facts.QueryLifetime = query
	first := m.BeginRequest(t.Context(), auth, facts)
	otherFacts := facts
	otherFacts.DesktopSessionID, otherFacts.QueryID = "local_"+uuid.NewString(), uuid.NewString()
	otherFacts.QueryLifetime = nil
	other := m.BeginRequest(t.Context(), auth, otherFacts)
	if !first.firstTurn || !other.firstTurn || len(m.sessions) != 2 {
		t.Fatal("same transcript merged Desktop records or initialization markers")
	}
	first.ObserveFirstByte(clock.Now())
	clock.Advance(1500 * time.Millisecond)
	emit, err := m.PrepareDesktopSessionStop(auth.ID, facts.DesktopSessionID, facts.QueryID)
	if err != nil || emit == nil {
		t.Fatal("stop snapshot unavailable", err)
	}
	cancel()
	clock.Advance(10 * time.Second)
	for range 2 {
		if err := emit(); err != nil {
			t.Fatal(err)
		}
	}
	if duplicate, err := m.PrepareDesktopSessionStop(auth.ID, facts.DesktopSessionID, facts.QueryID); err != nil || duplicate != nil {
		t.Fatal("duplicate stop")
	}
	if late := m.BeginRequest(t.Context(), auth, facts); late.worker != nil {
		t.Fatal("closed query resurrected a Renderer cycle")
	}
	nextFacts := facts
	nextFacts.QueryID, nextFacts.QueryLifetime = uuid.NewString(), nil
	next := m.BeginRequest(t.Context(), auth, nextFacts)
	if next.firstTurn {
		t.Fatal("query recreation reinitialized the Desktop record")
	}
	first.FinishSuccess(t.Context())
	state := m.sessions[rendererSessionKey(next.worker, facts.DesktopSessionID)]
	if state.pendingRequests != 1 || state.facts.QueryID != nextFacts.QueryID || state.pendingHadFirstResponse {
		t.Fatal("late response overwrote replacement state")
	}
	other.FinishSuccess(t.Context())
	emitIdle, err := m.PrepareDesktopSessionStop(auth.ID, otherFacts.DesktopSessionID, otherFacts.QueryID)
	if err != nil || emitIdle == nil {
		t.Fatal(err)
	}
	if err := emitIdle(); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	stops := map[string]map[string]any{}
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://claude.ai/") {
			continue
		}
		var batch struct {
			Events []struct {
				EventData struct {
					EventName string `json:"event_name"`
					Metadata  string `json:"metadata"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil {
			t.Fatal("invalid telemetry body")
		}
		for _, event := range batch.Events {
			if event.EventData.EventName != "desktop_ccd_session_stopped" {
				continue
			}
			var metadata map[string]any
			if json.Unmarshal([]byte(event.EventData.Metadata), &metadata) != nil {
				t.Fatal("invalid stop metadata")
			}
			id, _ := metadata["session_id"].(string)
			if stops[id] != nil {
				t.Fatal("stop emitted more than once")
			}
			stops[id] = metadata
		}
	}
	if len(stops) != 2 {
		t.Fatalf("stops: %+v", stops)
	}
	pending, idle := stops[facts.DesktopSessionID], stops[otherFacts.DesktopSessionID]
	if pending["trigger"] != "user" || pending["cli_session_id"] != facts.SessionID || pending["pending_seconds"] != float64(2) || pending["pending_had_first_response"] != true {
		t.Fatalf("pre-close snapshot lost: %+v", pending)
	}
	if idle["had_pending_cycle"] != false || idle["pending_seconds"] != nil || idle["pending_had_first_response"] != nil {
		t.Fatalf("absent pending cycle fabricated: %+v", idle)
	}
	next.FinishSuccess(t.Context())
}

func TestDesktopRecordStopDoesNotAdoptForeignAccountOrGeneration(t *testing.T) {
	clock := &testClock{now: time.Now()}
	m := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts(uuid.NewString())
	facts.DesktopSessionID, facts.QueryID = "local_"+uuid.NewString(), uuid.NewString()
	span := m.BeginRequest(t.Context(), auth, facts)
	if callback, err := m.PrepareDesktopSessionStop("foreign", facts.DesktopSessionID, facts.QueryID); callback != nil || err != nil {
		t.Fatal("foreign account received stop owner")
	}
	if callback, err := m.PrepareDesktopSessionStop(auth.ID, facts.DesktopSessionID, "old-query"); callback != nil || err == nil {
		t.Fatal("stale generation accepted")
	}
	state := m.sessions[rendererSessionKey(span.worker, facts.DesktopSessionID)]
	if state.queryClosed || state.pendingRequests != 1 {
		t.Fatal("rejected operation changed Renderer")
	}
	span.FinishSuccess(t.Context())
}
