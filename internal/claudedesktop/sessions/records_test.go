package sessions

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

type testStore struct {
	payload  []byte
	revision string
	err      error
	writes   int
}

func (s *testStore) Load(string) ([]byte, string, error) {
	return append([]byte(nil), s.payload...), s.revision, s.err
}
func (s *testStore) Save(_ string, previous string, payload []byte) (string, error) {
	s.writes++
	if s.err != nil {
		return "", s.err
	}
	if previous != s.revision {
		return "", features.ErrStale
	}
	s.payload, s.revision = append([]byte(nil), payload...), uuid.NewString()
	return s.revision, nil
}

func startTestHost(initial string) func(*string) (*features.Host, error) {
	return func(resume *string) (*features.Host, error) {
		session := initial
		if resume != nil {
			session = *resume
		}
		return features.NewHost(nil, session), nil
	}
}

func TestDesktopRecordsIndependentIdentityAndReconstruction(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner, scope, sibling := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	sdk := uuid.NewString()
	host, first, err := r.Resolve(t.Context(), owner, scope, startTestHost(sdk))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	otherHost, other, err := r.Resolve(t.Context(), owner, sibling, startTestHost(sdk))
	if err != nil {
		t.Fatal(err)
	}
	defer otherHost.Close()
	if first.ID == other.ID || first.SDKSessionID != other.SDKSessionID || first.ID == "local_"+sdk {
		t.Fatal("record identity was conflated with transcript or another record")
	}
	if bytes.Contains(store.payload, []byte(host.ID())) || bytes.Contains(store.payload, []byte("query_id")) {
		t.Fatal("live query state was persisted")
	}
	host.Close()
	restored := NewRegistry(store)
	list, err := restored.List(owner)
	if err != nil || len(list) != 2 || list[0].Running || list[0].QueryID != "" {
		t.Fatalf("restore: %+v %v", list, err)
	}
	nextHost, next, err := restored.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer nextHost.Close()
	if next.ID != first.ID || next.SDKSessionID != sdk || next.QueryID == first.QueryID || !next.Running {
		t.Fatalf("record/transcript/query restore mismatch: %+v %+v", first, next)
	}
}

func TestDesktopRecordStopExactOwnerAndGeneration(t *testing.T) {
	r := NewRegistry(&testStore{})
	owner, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	host, initial, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	prepareCount, emitCount := 0, 0
	prepare := func(value Snapshot) (func() error, error) {
		prepareCount++
		if !value.Running || host.Context().Err() != nil {
			t.Fatal("snapshot taken after cancellation")
		}
		return func() error {
			emitCount++
			if host.Context().Err() == nil {
				t.Fatal("emit preceded retirement")
			}
			return nil
		}, nil
	}
	retire := func(value *features.Host) { value.Close() }
	for _, badOwner := range []string{strings.Repeat("c", 64)} {
		if _, err := r.Stop(t.Context(), badOwner, initial.ID, initial.QueryID, prepare, retire); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if _, err := r.Stop(t.Context(), owner, initial.ID, "stale", prepare, retire); !errors.Is(err, ErrStaleQuery) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Stop(cancelled, owner, initial.ID, initial.QueryID, prepare, retire); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for range 2 {
		stopped, err := r.Stop(t.Context(), owner, initial.ID, initial.QueryID, prepare, retire)
		if err != nil || stopped.Running || stopped.SDKSessionID != initial.SDKSessionID || stopped.ID != initial.ID {
			t.Fatalf("stop: %+v %v", stopped, err)
		}
	}
	if prepareCount != 1 || emitCount != 1 {
		t.Fatalf("duplicate stop effects: %d/%d", prepareCount, emitCount)
	}
	next, snapshot, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := r.Stop(t.Context(), owner, initial.ID, initial.QueryID, nil, retire); !errors.Is(err, ErrStaleQuery) || next.Context().Err() != nil {
		t.Fatal("stale operation retired replacement", err)
	}
	if snapshot.SDKSessionID != initial.SDKSessionID || snapshot.ID != initial.ID {
		t.Fatal("stop discarded durable identity")
	}
}

func TestDesktopRecordsPreserveCorruptOriginalAndReportFailure(t *testing.T) {
	store := &testStore{payload: []byte(`{"version":9}`), revision: "existing"}
	r := NewRegistry(store)
	host, value, err := r.Resolve(t.Context(), strings.Repeat("a", 64), strings.Repeat("b", 64), startTestHost(uuid.NewString()))
	if !errors.Is(err, ErrInvalid) || host == nil || value.ID == "" {
		t.Fatal("inference lost its live identity or silently hid corruption", err)
	}
	defer host.Close()
	if store.writes != 0 || string(store.payload) != `{"version":9}` {
		t.Fatal("corrupt original replaced")
	}
	stopped, err := r.Stop(t.Context(), strings.Repeat("a", 64), value.ID, value.QueryID, nil, func(host *features.Host) { host.Close() })
	if !errors.Is(err, ErrInvalid) || stopped.Running {
		t.Fatal("corruption blocked cancellation or disappeared", err)
	}
}

func TestDesktopRecordsWriteFailureRecoversWithoutChangingLiveIdentity(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	_ = r.loadLocked()
	store.err = errors.New("synthetic unavailable")
	host, initial, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err == nil || host == nil {
		t.Fatal("missing failure")
	}
	defer host.Close()
	store.err = nil
	host.Close()
	next, value, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil || value.ID != initial.ID || value.SDKSessionID != initial.SDKSessionID {
		t.Fatalf("recovery: %+v %v", value, err)
	}
	defer next.Close()
	if store.writes != 2 {
		t.Fatal("checkpoint was not retried")
	}
}

func TestDesktopRecordsRetainUnverifiedAliasDiagnostic(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	host, first, err := r.Resolve(t.Context(), owner, scope, func(*string) (*features.Host, error) {
		return features.NewHost(nil, uuid.NewString()), errors.New("synthetic corrupt legacy alias")
	})
	if !errors.Is(err, ErrResumeUnverified) {
		t.Fatal("missing resume diagnostic", err)
	}
	defer host.Close()
	_, second, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if !errors.Is(err, ErrResumeUnverified) || second.ID != first.ID {
		t.Fatal("cached query hid alias failure", err)
	}
	host.Close()
	restored := NewRegistry(store)
	next, third, err := restored.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if !errors.Is(err, ErrResumeUnverified) || third.ID != first.ID {
		t.Fatal("restart hid alias failure", err)
	}
	defer next.Close()
}

func TestDesktopSessionHeartbeatBatchUsesOwnerScopedLiveHosts(t *testing.T) {
	r := NewRegistry(&testStore{})
	owner := strings.Repeat("a", 64)
	var hosts []*features.Host
	scopeDigits := "bcdef012"
	for index := 0; index < 8; index++ {
		scope := strings.Repeat(string(scopeDigits[index]), 64)
		host, _, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, host)
	}
	foreign, _, err := r.Resolve(t.Context(), strings.Repeat("f", 64), strings.Repeat("9", 64), startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	hosts[len(hosts)-1].Close()
	for _, host := range hosts[:len(hosts)-1] {
		defer host.Close()
	}
	batch, err := r.HeartbeatCheckBatch(owner)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Sent != 8 || batch.ProbeDispatched != 7 || batch.NoWorker != 1 || batch.Fresh != 0 || batch.RecentlyChecked != 0 || batch.Unknown != 0 {
		t.Fatalf("heartbeat batch = %+v", batch)
	}
	if _, err := r.HeartbeatCheckBatch("not-an-owner-key"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid owner heartbeat = %v", err)
	}
}
