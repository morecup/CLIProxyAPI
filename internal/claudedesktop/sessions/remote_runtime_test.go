package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

type recordWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *recordWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func awaitRecord[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("record operation did not complete")
		var zero T
		return zero
	}
}

func TestRemoteRecordWaitsForExactActorBeforeReplacement(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "cancel-wait"}[canceled], func(t *testing.T) {
			r := NewRegistry(&testStore{})
			owner := strings.Repeat("a", 64)
			grant, before, err := r.CreateRemote(t.Context(), owner, "cse_test", "folder", "model", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
			if err != nil {
				t.Fatal(err)
			}
			_, host, _, _, _, _ := grant.Read()
			t.Cleanup(host.Close)
			drained := make(chan struct{})
			if err := grant.BindInputDone(drained); err != nil {
				t.Fatal(err)
			}
			if err := grant.BindInputDone(make(chan struct{})); !errors.Is(err, ErrRemoteBound) {
				t.Fatal("actor ownership replaced", err)
			}
			host.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waiting := &recordWaitContext{Context: ctx, waiting: make(chan struct{})}
			result := make(chan error, 1)
			started := make(chan *features.Host, 1)
			go func() {
				_, _, err := r.Resolve(waiting, owner, grant.record.Scope, func(resume *string) (*features.Host, error) {
					next := features.NewHost(nil, *resume)
					started <- next
					return next, nil
				})
				result <- err
			}()
			awaitRecord(t, waiting.waiting)
			// The waiter must release the registry lock before the callback
			// finishes. A sibling record remains usable during this interval.
			sibling := make(chan error, 1)
			go func() {
				s, _, err := r.Resolve(t.Context(), owner, strings.Repeat("b", 64), startTestHost(""))
				if s != nil {
					s.Close()
				}
				sibling <- err
			}()
			if err := awaitRecord(t, sibling); err != nil {
				t.Fatal(err)
			}
			select {
			case next := <-started:
				next.Close()
				t.Fatal("replacement started before the old callback returned")
			default:
			}
			if canceled {
				cancel()
				if err := awaitRecord(t, result); !errors.Is(err, context.Canceled) {
					t.Fatal("canceled admission was accepted", err)
				}
				close(drained)
				select {
				case next := <-started:
					next.Close()
					t.Fatal("canceled waiter created a successor")
				default:
				}
				return
			}
			close(drained)
			if err := awaitRecord(t, result); err != nil {
				t.Fatal(err)
			}
			next := awaitRecord(t, started)
			defer next.Close()
			if next.SessionID() != before.SDKSessionID || next.ID() == before.QueryID {
				t.Fatal("replacement changed the wrong identity")
			}
			if err := grant.BindInputDone(drained); !errors.Is(err, ErrStaleQuery) {
				t.Fatal("old grant rebound a new generation", err)
			}
		})
	}
}

func TestRemoteConfigurationCommitsBeforeAcknowledgment(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner := strings.Repeat("a", 64)
	grant, before, err := r.CreateRemote(t.Context(), owner, "cse_settings", "folder", "initial", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
	if err != nil {
		t.Fatal(err)
	}
	_, host, _, _, _, _ := grant.Read()
	defer host.Close()
	if model, baseline, system, err := grant.Configuration(); err != nil || model != "initial" || baseline != "initial" || system != "" {
		t.Fatal("old record defaults were not preserved", err)
	}
	failure := errors.New("synthetic storage failure")
	store.err = failure
	if err := grant.SetConfiguration(t.Context(), "next", "private synthetic system"); !errors.Is(err, failure) {
		t.Fatal("storage failure acknowledged", err)
	}
	if model, baseline, system, err := grant.Configuration(); err != nil || model != "initial" || baseline != "initial" || system != "" {
		t.Fatal("failed commit changed live record", err)
	}
	store.err = nil
	if err := grant.SetConfiguration(t.Context(), "next", "private synthetic system"); err != nil {
		t.Fatal(err)
	}
	restored := NewRegistry(store)
	list, err := restored.List(owner)
	if err != nil || len(list) != 1 || list[0].ID != before.ID {
		t.Fatal("record did not reload", err)
	}
	saved := restored.records[grant.record.Scope].Remote
	if saved.Model != "next" || saved.DefaultModel != "initial" || saved.System != "private synthetic system" {
		t.Fatal("protected configuration was not restored")
	}
	public, err := json.Marshal(list)
	if err != nil || strings.Contains(string(public), "private synthetic system") || strings.Contains(string(public), "initial") {
		t.Fatal("private configuration leaked in management snapshot")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := grant.SetConfiguration(ctx, "late", "late"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled change committed", err)
	}
	host.Close()
	if err := grant.SetConfiguration(t.Context(), "late", "late"); !errors.Is(err, ErrStaleQuery) {
		t.Fatal("retired query changed configuration", err)
	}
	if _, _, _, err := grant.Configuration(); !errors.Is(err, ErrStaleQuery) {
		t.Fatal("retired grant read private configuration", err)
	}
}
