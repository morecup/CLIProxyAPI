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

func stoppedResumeRecord(t *testing.T) (*Registry, *testStore, *RemoteGrant, Snapshot, string) {
	t.Helper()
	store := &testStore{}
	r, owner := NewRegistry(store), strings.Repeat("a", 64)
	g, value, err := r.CreateRemote(t.Context(), owner, "cse_resume", "C:/private", "model", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetConfiguration(t.Context(), "next-model", "private-system"); err != nil {
		t.Fatal(err)
	}
	g.host.Close()
	return r, store, g, value, owner
}

func resumeTestStart(_ string, session string) (*features.Host, error) {
	return features.NewHost(nil, session), nil
}

func TestLocalResumeDurableGenerationAndOwner(t *testing.T) {
	store := &testStore{}
	owner, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	r := NewRegistry(store)
	host, first, err := r.Resolve(t.Context(), owner, scope, startTestHost(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	host.Close()
	restored := NewRegistry(store)
	prepareCalls := 0
	prepare := func(ResumeRecord) error {
		prepareCalls++
		return nil
	}
	if grant, value, err := restored.ResumeRemote(t.Context(), owner, first.ID, first.Generation, prepare, resumeTestStart); !errors.Is(err, ErrInvalid) || grant != nil || value.Running {
		t.Fatal("remote-only resume accepted an ordinary record", value, err)
	}
	grant, next, err := restored.ResumeSession(t.Context(), owner, first.ID, first.Generation, prepare, resumeTestStart)
	if err != nil {
		t.Fatal(err)
	}
	if grant != nil || prepareCalls != 0 || next.ID != first.ID || next.SDKSessionID != first.SDKSessionID || next.QueryID == first.QueryID || next.Generation == first.Generation || !next.Running {
		t.Fatal("local resume changed durable identity or created remote input", next)
	}
	stopped, err := restored.Stop(t.Context(), owner, next.ID, next.QueryID, nil, func(host *features.Host) { host.Close() })
	if err != nil || stopped.Running {
		t.Fatal("resumed local query could not be stopped", stopped, err)
	}
	listed, err := NewRegistry(store).List(owner)
	if err != nil || len(listed) != 1 || listed[0].Generation != next.Generation || listed[0].SDKSessionID != first.SDKSessionID || listed[0].Running {
		t.Fatal("local resume generation was not durable", listed, err)
	}
}

func TestRemoteResumeDurableGenerationAndOwner(t *testing.T) {
	_, store, _, first, owner := stoppedResumeRecord(t)
	r := NewRegistry(store)
	list, err := r.List(owner)
	if err != nil || len(list) != 1 || list[0].Generation != first.Generation || list[0].Running || list[0].QueryID != "" {
		t.Fatal("durable observation missing", list, err)
	}
	called := 0
	prepare := func(saved ResumeRecord) error {
		called++
		if saved.SDKSessionID != first.SDKSessionID || saved.Folder != "C:/private" || saved.Model != "next-model" || saved.DefaultModel != "model" || saved.System != "private-system" {
			t.Fatal("private configuration changed")
		}
		return nil
	}
	for _, tc := range []struct {
		owner, generation string
		want              error
	}{
		{strings.Repeat("b", 64), first.Generation, ErrNotFound}, {owner, uuid.NewString(), ErrStaleQuery}, {owner, "", ErrInvalid},
	} {
		if _, _, err := r.ResumeRemote(t.Context(), tc.owner, first.ID, tc.generation, prepare, resumeTestStart); !errors.Is(err, tc.want) {
			t.Fatal(err)
		}
	}
	if called != 0 {
		t.Fatal("unowned operation prepared history")
	}
	g, next, err := r.ResumeRemote(t.Context(), owner, first.ID, first.Generation, prepare, resumeTestStart)
	if err != nil {
		t.Fatal(err)
	}
	defer g.host.Close()
	if next.ID != first.ID || next.SDKSessionID != first.SDKSessionID || next.QueryID == first.QueryID || next.Generation == first.Generation || !next.Running {
		t.Fatal("resume changed identity or reused generation", next)
	}
	if required, err := g.RequiresSavedHistory(); err != nil || !required {
		t.Fatal("attach retry could lose saved history", err)
	}
	if _, _, err := r.ResumeRemote(t.Context(), owner, first.ID, next.Generation, prepare, resumeTestStart); !errors.Is(err, ErrStaleQuery) {
		t.Fatal("live query was replaced", err)
	}
	reader := NewRegistry(store)
	listed, err := reader.List(owner)
	if err != nil || listed[0].Generation != next.Generation || listed[0].Running {
		t.Fatal("new admission was not durable", listed, err)
	}
	if _, _, err := reader.ResumeRemote(t.Context(), owner, first.ID, first.Generation, prepare, resumeTestStart); !errors.Is(err, ErrStaleQuery) {
		t.Fatal("stale page revived old generation", err)
	}
}

func TestRemoteResumePersistenceFailurePublishesNothing(t *testing.T) {
	for _, kind := range []string{"io", "stale-catalog", "prepare", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			r, store, _, first, owner := stoppedResumeRecord(t)
			original := bytes.Clone(store.payload)
			var created *features.Host
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prepare := func(ResumeRecord) error {
				switch kind {
				case "io":
					store.err = errors.New("synthetic store unavailable")
				case "stale-catalog":
					store.revision = uuid.NewString()
				case "prepare":
					return ErrResumeUnverified
				case "cancel":
					cancel()
				}
				return nil
			}
			g, got, err := r.ResumeRemote(ctx, owner, first.ID, first.Generation, prepare, func(scope, session string) (*features.Host, error) {
				created = features.NewHost(nil, session)
				return created, nil
			})
			if err == nil || g != nil || got.Running || got.Generation != first.Generation || !bytes.Equal(store.payload, original) {
				t.Fatal("failed resume published new authority", got, err)
			}
			if created != nil && created.Context().Err() == nil {
				t.Fatal("failed admission left live host")
			}
			if len(r.resumes) != 0 {
				t.Fatal("preparation reservation leaked")
			}
			for _, record := range r.records {
				if record.Remote.ResumeHistory {
					t.Fatal("rollback changed prior remote configuration")
				}
			}
		})
	}
}

func TestRemoteResumeJoinsBothOwnersAndExcludesOrdinaryAdmission(t *testing.T) {
	r, _, prior, first, owner := stoppedResumeRecord(t)
	inputDone, bridgeDone := make(chan struct{}), make(chan struct{})
	prior.record.inputDone, prior.record.bridgeDone = inputDone, bridgeDone
	prepared, release := make(chan struct{}), make(chan struct{})
	ctx := &recordWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	result := make(chan *RemoteGrant, 1)
	errResult := make(chan error, 1)
	go func() {
		g, _, err := r.ResumeRemote(ctx, owner, first.ID, first.Generation, func(ResumeRecord) error { close(prepared); <-release; return nil }, resumeTestStart)
		result <- g
		errResult <- err
	}()
	awaitRecord(t, ctx.waiting)
	if _, _, err := r.ResumeRemote(t.Context(), owner, first.ID, first.Generation, func(ResumeRecord) error { t.Error("duplicate prepared"); return nil }, resumeTestStart); !errors.Is(err, ErrStaleQuery) {
		t.Fatal(err)
	}
	close(inputDone)
	select {
	case <-prepared:
		t.Fatal("bridge cleanup was not joined")
	default:
	}
	close(bridgeDone)
	awaitRecord(t, prepared)
	waiter := &recordWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	ordinary := make(chan *features.Host, 1)
	go func() {
		host, _, err := r.Resolve(waiter, owner, prior.record.Scope, func(*string) (*features.Host, error) {
			t.Error("ordinary admission bypassed resume")
			return nil, ErrInvalid
		})
		if err != nil {
			t.Error(err)
		}
		ordinary <- host
	}()
	awaitRecord(t, waiter.waiting)
	sibling, _, err := r.Resolve(t.Context(), owner, strings.Repeat("c", 64), startTestHost(""))
	if err != nil {
		t.Fatal("unrelated record blocked", err)
	}
	sibling.Close()
	close(release)
	grant := awaitRecord(t, result)
	if err := awaitRecord(t, errResult); err != nil {
		t.Fatal(err)
	}
	defer grant.host.Close()
	if host := awaitRecord(t, ordinary); host != grant.host {
		t.Fatal("ordinary request missed resumed owner")
	}
}

func TestRemoteResumeLegacyTokenMigrationPersistsBeforeListing(t *testing.T) {
	_, store, _, first, owner := stoppedResumeRecord(t)
	store.payload = bytes.ReplaceAll(store.payload, []byte(`,"generation":"`+first.Generation+`"`), nil)
	r := NewRegistry(store)
	list, err := r.List(owner)
	if err != nil || len(list) != 1 || !validUUID(list[0].Generation) {
		t.Fatal("legacy migration failed", err)
	}
	reader := NewRegistry(store)
	next, err := reader.List(owner)
	if err != nil || next[0].Generation != list[0].Generation {
		t.Fatal("legacy token was not durable", err)
	}
}

func TestRemoteResumeLegacyMigrationRetriesWithoutReplacingOriginals(t *testing.T) {
	_, store, _, first, owner := stoppedResumeRecord(t)
	store.payload = bytes.ReplaceAll(store.payload, []byte(`,"generation":"`+first.Generation+`"`), nil)
	original := bytes.Clone(store.payload)
	r := NewRegistry(store)
	if err := r.loadLocked(); err != nil {
		t.Fatal(err)
	}
	store.err = ErrUnavailable
	if _, err := r.List(owner); err == nil || !bytes.Equal(original, store.payload) {
		t.Fatal("failed migration rewrote originals")
	}
	store.err = nil
	list, err := r.List(owner)
	if err != nil || len(list) != 1 || !validUUID(list[0].Generation) {
		t.Fatal("migration could not recover", err)
	}
	next, err := NewRegistry(store).List(owner)
	if err != nil || next[0].Generation != list[0].Generation {
		t.Fatal("recovered migration was not durable", err)
	}
}
