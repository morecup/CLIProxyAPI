package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

func newLocalRegistryTest(t *testing.T) (*Registry, *testStore, string, Snapshot) {
	t.Helper()
	store := &testStore{}
	r := NewRegistry(store)
	owner := strings.Repeat("a", 64)
	view, err := r.CreateLocal(t.Context(), owner, LocalConversation{Model: "test-model", Folder: t.TempDir(), InitialMessage: "hello"}, func(string) (*features.Host, error) {
		host := features.NewHost(nil, uuid.NewString())
		t.Cleanup(host.Close)
		return host, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, store, owner, view
}

func TestLocalConversationColdReloadAndResume(t *testing.T) {
	r, store, owner, view := newLocalRegistryTest(t)
	turn, _, _, err := r.BeginLocalTurn(t.Context(), owner, view.ID, view.Generation, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.Finish(json.RawMessage(`{"id":"reply-1","role":"assistant","content":[{"type":"text","text":"answer"}]}`), LocalTurnObservation{}, nil); err != nil {
		t.Fatal(err)
	}
	turn.Host().Close()
	restored := NewRegistry(store)
	old, local, busy, err := restored.LocalConversation(owner, view.ID)
	if err != nil || busy || old.Running || !old.LocalConversation || old.SDKSessionID != view.SDKSessionID || len(local.Messages) != 2 {
		t.Fatal("cold reload", old, local, busy, err)
	}
	next, err := restored.ResumeLocal(t.Context(), owner, view.ID, view.Generation, func(_, sdk string) (*features.Host, error) {
		host := features.NewHost(nil, sdk)
		t.Cleanup(host.Close)
		return host, nil
	})
	if err != nil || !next.Running || next.Generation == view.Generation || next.SDKSessionID != view.SDKSessionID {
		t.Fatal("resume", next, err)
	}
	t.Log("LOCAL_COLD_RELOAD_VERIFIED messages=2 stable_session=true new_generation=true")
}

func TestLocalResponseSaveFailureDoesNotInventDurability(t *testing.T) {
	r, store, owner, view := newLocalRegistryTest(t)
	turn, _, _, err := r.BeginLocalTurn(t.Context(), owner, view.ID, view.Generation, "hello")
	if err != nil {
		t.Fatal(err)
	}
	before := string(store.payload)
	store.err = errors.New("synthetic save unavailable")
	response := json.RawMessage(`{"id":"reply-1","role":"assistant","content":[{"type":"text","text":"not saved"}]}`)
	if err := turn.Finish(response, LocalTurnObservation{}, nil); err == nil {
		t.Fatal("save failure hidden")
	}
	_, local, busy, err := r.LocalConversation(owner, view.ID)
	if err != nil || busy || len(local.Messages) != 1 || !strings.Contains(local.LastError, "not saved") || string(store.payload) != before {
		t.Fatal("uncommitted response became successful history", local, err)
	}
	store.err = nil
	_, cold, _, err := NewRegistry(store).LocalConversation(owner, view.ID)
	if err != nil || len(cold.Messages) != 1 {
		t.Fatal("cold history fabricated answer", cold, err)
	}
	t.Log("LOCAL_SAVE_FAILURE_VERIFIED failure_visible=true saved_reply=false busy_released=true")
}

func TestLocalStopJoinsPendingTurnBeforeResume(t *testing.T) {
	r, _, owner, view := newLocalRegistryTest(t)
	turn, _, _, err := r.BeginLocalTurn(t.Context(), owner, view.ID, view.Generation, "hello")
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := r.Stop(t.Context(), owner, view.ID, view.QueryID, nil, func(host *features.Host) { host.Close() })
	if err != nil || stopped.Running {
		t.Fatal(stopped, err)
	}
	start := func(_, sdk string) (*features.Host, error) {
		host := features.NewHost(nil, sdk)
		t.Cleanup(host.Close)
		return host, nil
	}
	if _, err := r.ResumeLocal(t.Context(), owner, view.ID, view.Generation, start); !errors.Is(err, ErrStaleQuery) {
		t.Fatal("premature resume", err)
	}
	done := make(chan error, 1)
	go func() { done <- r.WaitLocalTurn(t.Context(), owner, view.ID, view.QueryID) }()
	select {
	case err := <-done:
		t.Fatal("cancellation treated as completion", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := turn.Finish(nil, LocalTurnObservation{}, context.Canceled); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("join stuck")
	}
	if _, err := r.ResumeLocal(t.Context(), owner, view.ID, view.Generation, start); err != nil {
		t.Fatal(err)
	}
	t.Log("LOCAL_STOP_JOIN_VERIFIED early_resume=rejected completed_resume=accepted")
}
