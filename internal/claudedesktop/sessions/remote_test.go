package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
)

func TestRemoteRecordReconstructionRejectsDuplicateBindings(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner := strings.Repeat("c", 64)
	for _, id := range []string{"cse_one", "cse_two"} {
		grant, _, err := r.CreateRemote(t.Context(), owner, id, `C:\synthetic`, "model", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
		if err != nil {
			t.Fatal(err)
		}
		_, host, _, _, _, _ := grant.Read()
		t.Cleanup(host.Close)
	}
	var saved catalog
	if err := json.Unmarshal(store.payload, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Records[1].Remote.CCRSessionID = saved.Records[0].Remote.CCRSessionID
	store.payload, _ = json.Marshal(saved)
	original := string(store.payload)
	if _, err := NewRegistry(store).List(owner); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate persisted remote ownership accepted", err)
	}
	if string(store.payload) != original {
		t.Fatal("invalid catalog was overwritten")
	}
}

func TestRemoteRecordCreationOwnsOriginWithoutUserInput(t *testing.T) {
	store := &testStore{}
	r := NewRegistry(store)
	owner := strings.Repeat("a", 64)
	var started *features.Host
	grant, value, err := r.CreateRemote(t.Context(), owner, "session_Synthetic1", `C:\synthetic`, "claude-sonnet-5", func(scope string) (*features.Host, error) {
		if !validKey(scope) {
			t.Fatal("invalid owned scope")
		}
		started = features.NewHost(nil, "")
		return started, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer started.Close()
	if value.RemoteState != "pending" || value.QueryID != started.ID() || !value.Running {
		t.Fatalf("%+v", value)
	}
	current, host, remoteID, folder, model, err := grant.Read()
	if err != nil || current.ID != value.ID || host != started || remoteID != "cse_Synthetic1" || folder != `C:\synthetic` || model != "claude-sonnet-5" {
		t.Fatal("origin or owner lost", err)
	}
	if err = r.BindBridge(host, remoteID); err != nil {
		t.Fatal(err)
	}
	list, err := NewRegistry(store).List(owner)
	if err != nil || len(list) != 1 || list[0].RemoteState != "attached" || list[0].Running || list[0].QueryID != "" {
		t.Fatalf("restore %+v %v", list, err)
	}
	if strings.Contains(string(store.payload), "messages") || strings.Contains(string(store.payload), host.ID()) {
		t.Fatal("fabricated input or live query persisted")
	}
	_, _, err = r.CreateRemote(t.Context(), owner, remoteID, folder, model, func(string) (*features.Host, error) { t.Fatal("duplicate spawned"); return nil, nil })
	if !errors.Is(err, ErrRemoteBound) {
		t.Fatal(err)
	}
}

func TestRemoteRecordCreationReservesAndRechecksAcrossAsyncStart(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-remote-create", true: "ordinary-bridge-bound-during-start"}[collision], func(t *testing.T) {
			r := NewRegistry(&testStore{})
			owner := strings.Repeat("a", 64)
			var candidate *features.Host
			grant, _, err := r.CreateRemote(t.Context(), owner, "cse_Synthetic", `C:\synthetic`, "model", func(string) (*features.Host, error) {
				if collision {
					ordinary, _, err := r.Resolve(t.Context(), owner, strings.Repeat("b", 64), startTestHost(""))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(ordinary.Close)
					if err = r.BindBridge(ordinary, "cse_Synthetic"); err != nil {
						t.Fatal(err)
					}
				} else {
					_, _, err := r.CreateRemote(t.Context(), owner, "session_Synthetic", `C:\synthetic`, "model", func(string) (*features.Host, error) { t.Fatal("overlapping create spawned"); return nil, nil })
					if !errors.Is(err, ErrRemoteBound) {
						t.Fatal(err)
					}
				}
				candidate = features.NewHost(nil, "")
				return candidate, nil
			})
			defer candidate.Close()
			if collision {
				if !errors.Is(err, ErrRemoteBound) || grant != nil || candidate.Context().Err() == nil {
					t.Fatal("late collision was adopted", err)
				}
			} else if err != nil || grant == nil {
				t.Fatal(err)
			}
			if len(r.remoteCreates) != 0 {
				t.Fatal("reservation leaked")
			}
		})
	}
}

func TestRemoteCreationFailureReleasesReservation(t *testing.T) {
	r := NewRegistry(nil)
	owner := strings.Repeat("a", 64)
	failure := errors.New("synthetic start failure")
	for range 2 {
		_, _, err := r.CreateRemote(t.Context(), owner, "cse_Synthetic", "folder", "model", func(string) (*features.Host, error) { return nil, failure })
		if !errors.Is(err, failure) || len(r.remoteCreates) != 0 {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	var host *features.Host
	_, _, err := r.CreateRemote(ctx, owner, "cse_Synthetic", "folder", "model", func(string) (*features.Host, error) { host = features.NewHost(nil, ""); cancel(); return host, nil })
	if !errors.Is(err, context.Canceled) || host.Context().Err() == nil || len(r.records) != 0 || len(r.remoteCreates) != 0 {
		t.Fatal("canceled start retained authority", err)
	}
}

func TestRemoteGrantCannotGraftOrReachStaleQuery(t *testing.T) {
	r := NewRegistry(nil)
	owner := strings.Repeat("a", 64)
	ordinary, value, err := r.Resolve(t.Context(), owner, strings.Repeat("b", 64), startTestHost(""))
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	if _, err = r.Remote(owner, value.ID, value.QueryID); !errors.Is(err, ErrInvalid) {
		t.Fatal("ordinary record gained remote origin", err)
	}
	grant, value, err := r.CreateRemote(t.Context(), owner, "cse_Synthetic", "folder", "model", func(string) (*features.Host, error) { return features.NewHost(nil, ""), nil })
	if err != nil {
		t.Fatal(err)
	}
	_, host, _, _, _, _ := grant.Read()
	defer host.Close()
	if _, err = r.Remote(strings.Repeat("c", 64), value.ID, value.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = r.Remote(owner, value.ID, "wrong-query"); !errors.Is(err, ErrStaleQuery) {
		t.Fatal(err)
	}
	if err = r.BindBridge(host, "cse_Other"); !errors.Is(err, ErrRemoteMismatch) {
		t.Fatal(err)
	}
	if _, _, _, _, _, err = grant.Read(); !errors.Is(err, ErrRemoteDetached) {
		t.Fatal(err)
	}
	if list, _ := r.List(owner); len(list) != 2 {
		t.Fatal("mismatch deleted record")
	}
}

func TestRemoteSessionNormalizerMatchesNativeAdmission(t *testing.T) {
	for _, prefix := range []string{"cse_", "session_"} {
		for _, suffix := range []string{"a", "A9", "staging_a9", strings.Repeat("z", 64)} {
			if got := NormalizeRemoteSessionID(prefix + suffix); got != "cse_"+suffix {
				t.Fatal(got)
			}
		}
	}
	for _, value := range []string{"", "cse_", "CSE_a", "cse_a-b", "cse_a/b", "cse_a?x", "cse_a#x", " cse_a", "cse_" + strings.Repeat("a", 65)} {
		if NormalizeRemoteSessionID(value) != "" {
			t.Fatal("invalid origin accepted")
		}
	}
}
