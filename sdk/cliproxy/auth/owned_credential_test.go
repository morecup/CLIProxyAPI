package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type ownedCredentialTestStore struct {
	mu    sync.Mutex
	saved *Auth
	err   error
}

func (s *ownedCredentialTestStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *ownedCredentialTestStore) Delete(context.Context, string) error  { return nil }
func (s *ownedCredentialTestStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.saved = auth.Clone()
	return auth.ID, nil
}

func ownedCredentialFixture(t *testing.T) (*Manager, *unauthorizedRefreshExecutor, *Auth, *ownedCredentialTestStore) {
	t.Helper()
	store := &ownedCredentialTestStore{}
	m := NewManager(store, nil, nil)
	exec := &unauthorizedRefreshExecutor{id: "owned-refresh"}
	m.RegisterExecutor(exec)
	auth := &Auth{ID: "account-one", Provider: exec.id, Status: StatusActive, Metadata: map[string]any{
		"access_token": "old-access", "refresh_token": "old-refresh", "binding": map[string]any{"device": "device-one"},
	}}
	registered, err := m.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return m, exec, registered, store
}

func rotateOwnedCredential(_ context.Context, auth *Auth) (*Auth, error) {
	auth.Metadata["access_token"] = "new-access"
	auth.Metadata["refresh_token"] = "new-refresh"
	return auth, nil
}

func TestOwnedCredentialPersistsBeforePublicationAndRetainsRuntimeState(t *testing.T) {
	m, exec, auth, store := ownedCredentialFixture(t)
	updated, err := m.RefreshOwnedCredential(t.Context(), exec, auth, func(ctx context.Context, input *Auth) (*Auth, error) {
		latest, _ := m.GetByID(auth.ID)
		latest.Label = "concurrent label"
		latest.Success = 17
		latest.Unavailable = true
		m.mu.Lock()
		m.auths[auth.ID] = latest
		m.mu.Unlock()
		return rotateOwnedCredential(ctx, input)
	})
	if err != nil || authAccessToken(updated) != "new-access" || updated.Label != "concurrent label" || updated.Success != 17 || !updated.Unavailable {
		t.Fatal(updated, err)
	}
	current, _ := m.GetByID(auth.ID)
	if authAccessToken(current) != "new-access" || authAccessToken(store.saved) != "new-access" || authAccessToken(auth) != "old-access" {
		t.Fatal("publication/persistence or request snapshot mismatch")
	}
	if exec.RefreshCalls() != 0 {
		t.Fatal("application executor reentered")
	}
}

func TestOwnedCredentialRejectsLifecycleAndBindingChanges(t *testing.T) {
	for _, change := range []string{"remove", "remove-readd", "disable", "provider", "proxy", "attributes", "file-path", "device", "replace-executor", "token", "refresh-token", "cancel"} {
		t.Run(change, func(t *testing.T) {
			m, exec, auth, store := ownedCredentialFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, err := m.RefreshOwnedCredential(ctx, exec, auth, func(ctx context.Context, input *Auth) (*Auth, error) {
				latest, _ := m.GetByID(auth.ID)
				switch change {
				case "remove":
					m.Remove(ctx, auth.ID)
				case "remove-readd":
					m.Remove(ctx, auth.ID)
					if _, err := m.Register(ctx, latest); err != nil {
						t.Fatal(err)
					}
				case "replace-executor":
					m.RegisterExecutor(&unauthorizedRefreshExecutor{id: exec.id})
				case "cancel":
					cancel()
				default:
					switch change {
					case "disable":
						latest.Disabled = true
					case "provider":
						latest.Provider = "replacement"
					case "proxy":
						latest.ProxyURL = "http://127.0.0.1:9999"
					case "attributes":
						latest.Attributes = map[string]string{"binding": "new"}
					case "file-path":
						latest.Attributes = map[string]string{AttributePath: "different-local-target.json"}
					case "device":
						latest.Metadata["binding"] = map[string]any{"device": "replacement"}
					case "token":
						latest.Metadata["access_token"] = "replacement-access"
					case "refresh-token":
						latest.Metadata["refresh_token"] = "replacement-refresh"
					}
					if _, err := m.Update(ctx, latest); err != nil {
						t.Fatal(err)
					}
				}
				return rotateOwnedCredential(ctx, input)
			})
			if err == nil {
				t.Fatal("stale refresh succeeded")
			}
			if authAccessToken(store.saved) == "new-access" {
				t.Fatal("stale refresh persisted")
			}
			if current, ok := m.GetByID(auth.ID); ok && authAccessToken(current) == "new-access" {
				t.Fatal("stale refresh published")
			}
		})
	}
}

func TestOwnedCredentialFailuresAndNestedMutationDoNotPublish(t *testing.T) {
	for _, variant := range []string{"storage", "acquisition", "nested-mutation", "nil", "empty-token", "missing-refresh"} {
		t.Run(variant, func(t *testing.T) {
			m, exec, auth, store := ownedCredentialFixture(t)
			if variant == "storage" {
				store.err = errors.New("synthetic storage failure")
			}
			if variant == "missing-refresh" {
				delete(auth.Metadata, "refresh_token")
				_, _ = m.Update(t.Context(), auth)
			}
			_, err := m.RefreshOwnedCredential(t.Context(), exec, auth, func(ctx context.Context, input *Auth) (*Auth, error) {
				switch variant {
				case "acquisition":
					return nil, errors.New("synthetic acquisition failure")
				case "nested-mutation":
					input.Metadata["binding"].(map[string]any)["device"] = "poisoned"
				case "nil":
					return nil, nil
				case "empty-token":
					input.Metadata["access_token"] = ""
					return input, nil
				case "missing-refresh":
					t.Fatal("missing refresh called acquisition")
				}
				return rotateOwnedCredential(ctx, input)
			})
			if err == nil {
				t.Fatal("invalid refresh succeeded")
			}
			current, _ := m.GetByID(auth.ID)
			if authAccessToken(current) != "old-access" || current.Metadata["binding"].(map[string]any)["device"] != "device-one" {
				t.Fatal("failed callback mutated registry")
			}
		})
	}
}

func TestOwnedCredentialConcurrentRefreshAndCancellation(t *testing.T) {
	m, exec, auth, _ := ownedCredentialFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var acquisitions atomic.Int32
	refresh := func(ctx context.Context, input *Auth) (*Auth, error) {
		if acquisitions.Add(1) == 1 {
			close(entered)
		}
		<-release
		return rotateOwnedCredential(ctx, input)
	}
	first := make(chan error, 1)
	go func() { _, err := m.RefreshOwnedCredential(t.Context(), exec, auth, refresh); first <- err }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	canceled := make(chan error, 1)
	go func() { _, err := m.RefreshOwnedCredential(ctx, exec, auth, refresh); canceled <- err }()
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	request := make(chan error, 1)
	go func() { _, err := m.refreshAuthForRequest(t.Context(), auth.ID, "old-access"); request <- err }()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-request; err != nil {
		t.Fatal(err)
	}
	if acquisitions.Load() != 1 || exec.RefreshCalls() != 0 {
		t.Fatal("refresh lock not shared")
	}
	if _, err := m.RefreshOwnedCredential(t.Context(), exec, auth, refresh); err != nil || acquisitions.Load() != 1 {
		t.Fatal("new token not reused", err)
	}
}

func TestOwnedCredentialRejectsObsoleteSourceBeforeNetwork(t *testing.T) {
	m, exec, auth, _ := ownedCredentialFixture(t)
	m.RegisterExecutor(&unauthorizedRefreshExecutor{id: exec.id})
	if _, err := m.CurrentOwnedCredential(exec, auth); !errors.Is(err, ErrCredentialOwnerChanged) {
		t.Fatal(err)
	}
	_, err := m.RefreshOwnedCredential(t.Context(), exec, auth, func(context.Context, *Auth) (*Auth, error) {
		t.Fatal("obsolete owner acquired credentials")
		return nil, nil
	})
	if !errors.Is(err, ErrCredentialOwnerChanged) {
		t.Fatal(err)
	}
}

func TestOwnedCredentialSnapshotNormalizesTypedMetadataAndLargeIntegers(t *testing.T) {
	m, exec, auth, _ := ownedCredentialFixture(t)
	auth.Metadata["typed"] = struct {
		Z string `json:"z"`
		A uint64 `json:"a"`
	}{Z: "synthetic", A: 18446744073709551615}
	auth, err := m.Update(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.CurrentOwnedCredential(exec, auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.CurrentOwnedCredential(exec, snapshot); err != nil {
		t.Fatal("snapshot changed owner", err)
	}
	if _, err = m.RefreshOwnedCredential(t.Context(), exec, snapshot, rotateOwnedCredential); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedCredentialSerializedRefreshResult(t *testing.T) {
	for _, variant := range []string{"same-owner", "foreign-id", "foreign-index", "foreign-file", "foreign-generation", "foreign-device", "disabled", "removed-readded"} {
		t.Run(variant, func(t *testing.T) {
			m, exec, auth, store := ownedCredentialFixture(t)
			auth.FileName = "local-owned.json"
			auth.Label = "local label"
			auth.Success = 19
			auth.Runtime = &struct{ Name string }{Name: "local runtime"}
			auth, err := m.Update(t.Context(), auth)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := m.RefreshOwnedCredential(t.Context(), exec, auth, func(ctx context.Context, input *Auth) (*Auth, error) {
				rotated, _ := rotateOwnedCredential(ctx, input)
				raw, err := json.Marshal(rotated)
				if err != nil {
					t.Fatal(err)
				}
				var result Auth
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				if err = decoder.Decode(&result); err != nil {
					t.Fatal(err)
				}
				if result.credentialGeneration != 0 || result.FileName != "" || result.Runtime != nil || result.Index != "" {
					t.Fatal("fixture retained local-only fields")
				}
				result.Label = "remote label must not replace local state"
				switch variant {
				case "foreign-id":
					result.ID = "foreign"
				case "foreign-index":
					result.Index = "foreign"
				case "foreign-file":
					result.FileName = "foreign.json"
				case "foreign-generation":
					result.credentialGeneration = auth.credentialGeneration + 1
				case "foreign-device":
					result.Metadata["binding"].(map[string]any)["device"] = "foreign"
				case "disabled":
					result.Disabled = true
				case "removed-readded":
					m.Remove(ctx, auth.ID)
					if _, err = m.Register(ctx, auth); err != nil {
						t.Fatal(err)
					}
				}
				return &result, nil
			})
			if variant != "same-owner" {
				if err == nil || authAccessToken(store.saved) == "new-access" {
					t.Fatal("foreign or replaced serialized owner was committed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if authAccessToken(updated) != "new-access" || authAccessToken(store.saved) != "new-access" || updated.FileName != auth.FileName || updated.Runtime != auth.Runtime || updated.credentialGeneration != auth.credentialGeneration || updated.Index != auth.Index || updated.Label != auth.Label || updated.Success != auth.Success {
				t.Fatal("serialized refresh failed to preserve registered local ownership")
			}
		})
	}
}

func TestOwnedCredentialHomeSourceDoesNotCreateALocalStoreBinding(t *testing.T) {
	m, exec, auth, _ := ownedCredentialFixture(t)
	m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	updated, err := m.RefreshOwnedCredential(t.Context(), exec, auth, func(ctx context.Context, input *Auth) (*Auth, error) {
		rotated, _ := rotateOwnedCredential(ctx, input)
		raw, err := json.Marshal(rotated)
		if err != nil {
			return nil, err
		}
		var result Auth
		if err = json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		result.Attributes = map[string]string{AttributePath: "/synthetic/remote.json", AttributeSource: "/synthetic/remote.json", AttributeSourceBackend: "remote-store"}
		return &result, nil
	})
	if err != nil || updated.Attributes != nil || authAccessToken(updated) != "new-access" {
		t.Fatal("remote store annotations changed local ownership", err)
	}
}
