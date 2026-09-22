package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type bridgeCheckpointStore struct {
	mu       sync.Mutex
	payload  []byte
	revision string
	err      error
}

func (s *bridgeCheckpointStore) Load(string) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.payload), s.revision, nil
}
func (s *bridgeCheckpointStore) Save(_ string, previous string, payload []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	if previous != s.revision {
		return "", features.ErrStale
	}
	s.payload, s.revision = bytes.Clone(payload), uuid.NewString()
	return s.revision, nil
}
func (s *bridgeCheckpointStore) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func bridgeCheckpointRecord(t *testing.T, store *bridgeCheckpointStore) (*claudesessions.Registry, *features.Host, claudesessions.Snapshot) {
	t.Helper()
	r := claudesessions.NewRegistry(store)
	host, record, err := r.Resolve(t.Context(), strings.Repeat("a", 64), strings.Repeat("b", 64), func(id *string) (*features.Host, error) {
		resume := ""
		if id != nil {
			resume = *id
		}
		return features.NewHost(nil, resume), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	return r, host, record
}

func TestBridgeCheckpointLifecycleNotPerFrameOrJWTRefresh(t *testing.T) {
	store := &bridgeCheckpointStore{}
	r, host, record := bridgeCheckpointRecord(t, store)
	auth := testActiveControlAuth(t)
	enrollment, _ := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	bundle, _ := claudeprofile.BuiltinV140609()
	credentials := &testControlCredentials{current: auth}
	m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, Credentials: credentials,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return credentialBudgetDoerFunc(func(req *http.Request) (*http.Response, error) {
				body := `{}`
				if req.URL.Path == "/v1/code/sessions" {
					body = `{"session":{"id":"cse_checkpoint"}}`
				}
				if strings.HasSuffix(req.URL.Path, "/bridge") {
					body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"2","worker_jwt":"new-worker"}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		}})
	t.Cleanup(m.Close)
	g := r.BridgeForHost(host)
	var transcriptMu sync.Mutex
	var transcriptRows []claudesessions.BridgeState
	s, err := m.sessionForQuery(auth, enrollment, RequestFacts{Role: claudeprofile.RoleMain, LocalSessionID: record.SDKSessionID,
		DesktopSessionID: record.ID, QueryID: record.QueryID, QueryLifetime: host.Context(), Model: "claude-sonnet-5", BridgeRecord: g,
		BridgeTranscript: func(value claudesessions.BridgeState, _ func(error)) error {
			transcriptMu.Lock()
			defer transcriptMu.Unlock()
			transcriptRows = append(transcriptRows, value)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, _, _ := store.Load("")
	for _, frame := range []workerFrame{{id: "41", event: "server_event"}, {id: "17", event: "server_event"}, {id: "900", event: "ephemeral_event"}} {
		if err := s.consumeInboundFrame(t.Context(), frame); err != nil {
			t.Fatal(err)
		}
	}
	s.opMu.Lock()
	err = s.bridgeLocked(t.Context())
	request, _, errRequest := s.buildRequest(t.Context(), claudeprofile.ControlEndpointWorkerStream, s.state.RemoteSessionID, nil)
	s.opMu.Unlock()
	if err != nil || errRequest != nil || request.Header.Get("Last-Event-ID") != "41" || request.URL.Query().Get("from_sequence_num") != "41" {
		t.Fatal("JWT refresh lost live cursor", err, errRequest)
	}
	after, _, _ := store.Load("")
	if !bytes.Equal(before, after) {
		t.Fatal("frames or token refresh created a native lifecycle checkpoint")
	}
	m.Close()
	transcriptMu.Lock()
	if len(transcriptRows) != 2 || transcriptRows[0].LastSequenceNum != 0 || transcriptRows[1].LastSequenceNum != 41 {
		t.Fatal("transcript checkpoints did not follow attachment/cleanup", transcriptRows)
	}
	transcriptMu.Unlock()
	saved, err := g.Load(enrollment.AccountUUID, enrollment.OrganizationUUID)
	if err != nil || saved.LastSequenceNum != 41 {
		t.Fatal("cleanup missed the actual live cursor", saved, err)
	}
}

func TestBridgeTranscriptFailureIsVisibleWithoutRejectingBridge(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "delayed"}[delayed], func(t *testing.T) {
			store := &bridgeCheckpointStore{}
			r, host, _ := bridgeCheckpointRecord(t, store)
			auth := testActiveControlAuth(t)
			enrollment, _ := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
			m := NewManager(Options{})
			defer m.cancel()
			var report func(error)
			s := &sessionRuntime{manager: m, ctx: host.Context(), done: make(chan struct{}), bridgeRecord: r.BridgeForHost(host),
				enrollment: enrollment, state: controlSessionState{RemoteSessionID: "cse_owned"},
				bridgeTranscript: func(_ claudesessions.BridgeState, observe func(error)) error {
					report = observe
					if delayed {
						return nil
					}
					return errors.New("synthetic transcript unavailable")
				}}
			m.sessions["test"] = s
			if err := s.checkpointBridgeLocked(); err != nil {
				t.Fatal("native asynchronous metadata failure vetoed the bridge", err)
			}
			if delayed {
				if m.Status().BridgeTranscriptFailed != 0 {
					t.Fatal("invented an append failure before completion")
				}
				report(errors.New("synthetic late append failure"))
			}
			if status := m.Status(); status.BridgeTranscriptFailed != 1 || status.InitializationFailed != 0 || status.Failed != 0 {
				t.Fatal("metadata failure hidden or mislabeled as initialization/retirement failure", status)
			}
			if saved, err := s.bridgeRecord.Load(enrollment.AccountUUID, enrollment.OrganizationUUID); err != nil || saved.SessionID != "cse_owned" {
				t.Fatal("metadata failure rolled back the independent record", err)
			}
		})
	}
}

func TestBridgeCheckpointRemintResetsCursorAndCommitsAtomically(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "write_failure"}[fail], func(t *testing.T) {
			store := &bridgeCheckpointStore{}
			r, host, record := bridgeCheckpointRecord(t, store)
			auth := testActiveControlAuth(t)
			enrollment, _ := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
			g := r.BridgeForHost(host)
			initial := claudesessions.BridgeState{SessionID: "cse_old", LastSequenceNum: 91, NoHistoryBackfill: true,
				OwnerAccountUUID: enrollment.AccountUUID, OwnerOrganizationUUID: enrollment.OrganizationUUID}
			if err := g.Save(initial); err != nil {
				t.Fatal(err)
			}
			before, _, _ := store.Load("")
			bundle, _ := claudeprofile.BuiltinV140609()
			m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true,
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
					return credentialBudgetDoerFunc(func(req *http.Request) (*http.Response, error) {
						body, code := `{}`, 200
						switch {
						case strings.HasSuffix(req.URL.Path, "/unarchive"):
							code = 404
						case req.URL.Path == "/v1/code/sessions":
							body = `{"session":{"id":"cse_new"}}`
						case strings.HasSuffix(req.URL.Path, "/bridge"):
							body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"3","worker_jwt":"worker-new"}`
						case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/worker") && fail:
							store.fail(errors.New("synthetic checkpoint failure"))
						}
						return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
					}), nil
				}})
			t.Cleanup(m.Close)
			s, err := m.sessionForQuery(auth, enrollment, RequestFacts{Role: claudeprofile.RoleMain, LocalSessionID: record.SDKSessionID,
				DesktopSessionID: record.ID, QueryID: record.QueryID, QueryLifetime: host.Context(), Model: "claude-sonnet-5", BridgeRecord: g})
			if err != nil {
				t.Fatal(err)
			}
			err = s.ensure(t.Context())
			if (err != nil) != fail {
				t.Fatal("wrong initialization outcome", err)
			}
			s.opMu.Lock()
			request, _, errRequest := s.buildRequest(t.Context(), claudeprofile.ControlEndpointWorkerStream, s.state.RemoteSessionID, nil)
			s.opMu.Unlock()
			if errRequest != nil || request.URL.Path != "/v1/code/sessions/cse_new/worker/events/stream" || request.URL.Query().Has("from_sequence_num") || request.Header.Get("Last-Event-ID") != "" {
				t.Fatal("new remote session inherited old cursor", errRequest)
			}
			saved, err := g.Load(enrollment.AccountUUID, enrollment.OrganizationUUID)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				after, _, _ := store.Load("")
				if !bytes.Equal(before, after) || saved.SessionID != "cse_old" || saved.LastSequenceNum != 91 || m.Status().InitializationFailed != 1 {
					t.Fatal("partial replacement overwrote saved checkpoint or hid its failure")
				}
			} else if saved.SessionID != "cse_new" || saved.LastSequenceNum != 0 || !saved.NoHistoryBackfill {
				t.Fatal("replacement lost cursor reset or sticky suppression", saved)
			}
			store.fail(nil)
			m.Close()
		})
	}
}

func TestBridgeCheckpointChangedCredentialOwnerPreservesSavedState(t *testing.T) {
	store := &bridgeCheckpointStore{}
	r, host, record := bridgeCheckpointRecord(t, store)
	auth := testActiveControlAuth(t)
	enrollment, _ := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	g := r.BridgeForHost(host)
	value := claudesessions.BridgeState{SessionID: "cse_owned", LastSequenceNum: 7, OwnerAccountUUID: enrollment.AccountUUID, OwnerOrganizationUUID: enrollment.OrganizationUUID}
	if err := g.Save(value); err != nil {
		t.Fatal(err)
	}
	before, _, _ := store.Load("")
	changed := auth.Clone()
	changed.Metadata["account_uuid"] = "different"
	s := &sessionRuntime{manager: &Manager{credentials: &testControlCredentials{current: changed}}, auth: auth, enrollment: enrollment,
		bridgeRecord: g, state: controlSessionState{LocalSessionID: record.SDKSessionID, RemoteSessionID: value.SessionID}}
	s.lastSequence.Store(100)
	if err := s.checkpointBridgeLocked(); !errors.Is(err, cliproxyauth.ErrCredentialOwnerChanged) {
		t.Fatal("changed account wrote checkpoint", err)
	}
	after, _, _ := store.Load("")
	if !bytes.Equal(before, after) {
		t.Fatal("changed owner replaced original record")
	}
}
