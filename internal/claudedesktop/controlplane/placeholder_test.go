package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type placeholderDoer func(*http.Request) (*http.Response, error)

func (f placeholderDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func placeholderFixture(t *testing.T, status int, body string, archiveStatus int) (*Manager, *sessionRuntime, *[]string) {
	t.Helper()
	m := newQueryControlManager(t, &queryControlDoer{})
	m.now = func() time.Time { return time.Unix(1800000000, 0) }
	requests := []string{}
	m.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return placeholderDoer(func(r *http.Request) (*http.Response, error) {
			if _, ok := r.Context().Deadline(); ok {
				t.Error("sweep installed a network deadline")
			}
			requests = append(requests, r.Method+" "+r.URL.Path)
			code, payload := status, body
			if r.Method == http.MethodPost {
				code, payload = archiveStatus, `{}`
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != `{}` {
					t.Error("unexpected archive body", string(data), err)
				}
			}
			return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
		}), nil
	}
	m.processProbe = func(int) (processIdentity, error) { return processIdentity{Dead: true}, nil }
	s := &sessionRuntime{manager: m, ctx: context.Background(), auth: testDesktopAuth(t), placeholderGate: func() (bool, error) { return true, nil }}
	s.state.RemoteSessionID = "cse_current"
	return m, s, &requests
}

func seedPlaceholder(t *testing.T, m *Manager, id string, owner processIdentity, age time.Duration) placeholderRecord {
	t.Helper()
	record := placeholderRecord{PID: 1234, ProcessStart: owner.Start, CreatedAt: m.now().Add(-age).UnixMilli()}
	if err := writeProtectedState(m.placeholderPath(), placeholderState{Version: 1, Records: map[string]placeholderRecord{id: record}}); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPlaceholderSweepNativeDecisions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		archive int
		remove  bool
		calls   int
		used    int64
	}{
		{"unused", 200, `{"created_at":"same","updated_at":"same"}`, 200, true, 2, 0},
		{"used", 200, `{"created_at":"first","updated_at":"second"}`, 200, true, 1, 1},
		{"missing", 404, `{}`, 200, true, 1, 0},
		{"missing-timestamps", 200, `{}`, 200, false, 1, 0},
		{"empty-timestamp", 200, `{"created_at":"","updated_at":"same"}`, 200, false, 1, 0},
		{"read-401", 401, `{}`, 200, false, 1, 0},
		{"read-503", 503, `{}`, 200, false, 1, 0},
		{"read-invalid", 200, `not-json`, 200, false, 1, 0},
		{"archive-401", 200, `{"created_at":"same","updated_at":"same"}`, 401, false, 2, 0},
		{"archive-408", 200, `{"created_at":"same","updated_at":"same"}`, 408, false, 2, 0},
		{"archive-429", 200, `{"created_at":"same","updated_at":"same"}`, 429, false, 2, 0},
		{"archive-502", 200, `{"created_at":"same","updated_at":"same"}`, 502, false, 2, 0},
		{"archive-404", 200, `{"created_at":"same","updated_at":"same"}`, 404, true, 2, 0},
		{"archive-409", 200, `{"created_at":"same","updated_at":"same"}`, 409, true, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, requests := placeholderFixture(t, tc.status, tc.body, tc.archive)
			seedPlaceholder(t, m, "cse_old", processIdentity{Start: "old"}, 6*time.Minute)
			_ = s.sweepPlaceholders()
			if len(*requests) != tc.calls || m.Status().PlaceholderUsedPreserved != tc.used {
				t.Fatal("wrong remote actions", *requests, m.Status())
			}
			var state placeholderState
			if err := readProtectedState(m.placeholderPath(), &state); err != nil {
				t.Fatal(err)
			}
			if (len(state.Records) == 0) != tc.remove {
				t.Fatal("wrong durable disposition", state)
			}
			if tc.calls > 1 && (*requests)[1] != "POST /v1/sessions/session_old/archive" {
				t.Fatal("wrong pinned CRUD path", *requests)
			}
		})
	}
}

func TestPlaceholderOwnerProofAndAge(t *testing.T) {
	for _, tc := range []struct {
		name, id, stored, current string
		dead, inaccessible        bool
		age                       time.Duration
		calls                     int
		remove                    bool
	}{
		{"alive", "cse_old", "same", "same", false, false, 6 * time.Minute, 0, false},
		{"reuse", "cse_old", "old", "new", false, false, 6 * time.Minute, 1, true},
		{"unknown-stored", "cse_old", "", "new", false, false, 6 * time.Minute, 0, false},
		{"unknown-current", "cse_old", "old", "", false, false, 6 * time.Minute, 0, false},
		{"inaccessible", "cse_old", "old", "", false, true, 6 * time.Minute, 0, false},
		{"young", "cse_old", "old", "", true, false, 4 * time.Minute, 0, false},
		{"future-young", "cse_old", "old", "", true, false, -4 * time.Minute, 0, false},
		{"future-old", "cse_old", "old", "", true, false, -6 * time.Minute, 1, true},
		{"minimum-boundary", "cse_old", "old", "", true, false, 5 * time.Minute, 1, true},
		{"self-alias", "session_current", "old", "", true, false, 31 * 24 * time.Hour, 0, false},
		{"invalid", "other_bad", "old", "", true, false, 6 * time.Minute, 0, true},
		{"expired-live", "cse_old", "same", "same", false, false, 31 * 24 * time.Hour, 0, true},
		{"expiry-boundary", "cse_old", "same", "same", false, false, 30 * 24 * time.Hour, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, requests := placeholderFixture(t, 404, `{}`, 200)
			seedPlaceholder(t, m, tc.id, processIdentity{Start: tc.stored}, tc.age)
			m.processProbe = func(int) (processIdentity, error) {
				if tc.inaccessible {
					return processIdentity{}, errors.New("not accessible")
				}
				return processIdentity{Start: tc.current, Dead: tc.dead}, nil
			}
			_ = s.sweepPlaceholders()
			if len(*requests) != tc.calls || (m.Status().PlaceholderPending == 0) != tc.remove {
				t.Fatal("unsafe owner/age decision", *requests, m.Status())
			}
		})
	}
}

func TestPlaceholderJournalProtectionCapAliasAndRestart(t *testing.T) {
	m, _, _ := placeholderFixture(t, 200, `{}`, 200)
	m.processProbe = inspectProcess
	for i := 0; i < 25; i++ {
		at := time.Unix(1800000000+int64(i), 0)
		m.now = func() time.Time { return at }
		if err := m.registerPlaceholder(fmt.Sprintf("cse_record-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.registerPlaceholder("session_record-24"); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(m.placeholderPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "record-") || strings.Contains(string(encoded), "procStart") {
		t.Fatal("journal leaked identity")
	}
	next := NewManager(Options{StatePath: m.root})
	next.placeholderMu.Lock()
	state, err := next.loadPlaceholdersLocked()
	next.placeholderMu.Unlock()
	if err != nil || len(state.Records) != 20 || state.Records["session_record-24"].PID != os.Getpid() || state.Records["session_record-24"].ProcessStart == "" {
		t.Fatal("restart/cap/owner", state, err)
	}
	if _, ok := state.Records["cse_record-24"]; ok {
		t.Fatal("duplicate alias")
	}
	if _, ok := state.Records["cse_record-00"]; ok {
		t.Fatal("oldest marker retained past cap")
	}
	if normalizePlaceholder("session_cse_same") != "cse_same" {
		t.Fatal("alias normalized twice")
	}
	other := NewManager(Options{StatePath: t.TempDir()})
	state, err = other.loadPlaceholdersLocked()
	if err != nil || len(state.Records) != 0 {
		t.Fatal("cross-account journal")
	}
}

func TestPlaceholderCorruptionAndReplacementArePreserved(t *testing.T) {
	m, s, requests := placeholderFixture(t, 404, `{}`, 200)
	original := seedPlaceholder(t, m, "cse_old", processIdentity{Start: "old"}, 6*time.Minute)
	replacement := original
	replacement.ProcessStart = "replacement"
	if err := writeProtectedState(m.placeholderPath(), placeholderState{Version: 1, Records: map[string]placeholderRecord{"session_old": replacement}}); err != nil {
		t.Fatal(err)
	}
	if err := m.removePlaceholder("cse_old", &original); err != nil || m.Status().PlaceholderPending != 1 {
		t.Fatal("late removal erased new owner", err)
	}
	encoded, _ := os.ReadFile(m.placeholderPath())
	encoded[len(encoded)/2] ^= 1
	if err := os.WriteFile(m.placeholderPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepPlaceholders(); err == nil || !m.Status().PlaceholderStorageFailed || len(*requests) != 0 {
		t.Fatal("corruption was hidden", err)
	}
	after, _ := os.ReadFile(m.placeholderPath())
	if string(after) != string(encoded) {
		t.Fatal("corrupt evidence overwritten")
	}
}

func TestPlaceholderGateCancellationAndSerialization(t *testing.T) {
	m, s, requests := placeholderFixture(t, 404, `{}`, 200)
	seedPlaceholder(t, m, "cse_old", processIdentity{Start: "old"}, 6*time.Minute)
	s.placeholderGate = func() (bool, error) { return false, nil }
	if err := s.sweepPlaceholders(); err != nil || len(*requests) != 0 {
		t.Fatal("disabled feature dispatched", err)
	}
	s.placeholderGate = func() (bool, error) { return true, nil }
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.sweepPlaceholders(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(*requests) != 1 {
		t.Fatal("concurrent sweep replayed cleanup", *requests)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = canceled
	if !errors.Is(s.sweepPlaceholders(), context.Canceled) || len(*requests) != 1 {
		t.Fatal("retired Host dispatched")
	}
}

func TestPlaceholderRegistrationFollowsRealBeginRequest(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			m := newQueryControlManager(t, &queryControlDoer{})
			facts := queryFacts("placeholder-host", t.Context())
			reads := 0
			facts.PlaceholderSweepEnabled = func() (bool, error) { reads++; return enabled, nil }
			span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts)
			if err != nil || span == nil {
				t.Fatal(err)
			}
			if reads != 1 || m.Status().PlaceholderPending != map[bool]int64{false: 0, true: 1}[enabled] {
				t.Fatal("not driven by real admission", reads, m.Status())
			}
			m.PrepareClose()
			m.Close()
			if m.Status().PlaceholderPending != 0 {
				t.Fatal("terminal archive retained marker")
			}
		})
	}
}

func TestPlaceholderUntrustedDeviceAndEmptyStatusBody(t *testing.T) {
	m, s, _ := placeholderFixture(t, 200, `{}`, 200)
	for _, body := range []string{`{"error":{"resource":"untrusted_device"}}`, `{"error":{"resource":"untrusted_\u0064evice"}}`, `{"error":{"message":"must use a trusted device"}}`, `{"error":"denied","message":"must use a trusted device"}`} {
		m.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return placeholderDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		}
		if archiveTerminal(s.doJSON(t.Context(), claudeprofile.ControlEndpointSessionArchive, "session_old", struct{}{}, nil)) {
			t.Fatal("untrusted device treated as terminal")
		}
	}
	m.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return placeholderDoer(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 404}, nil }), nil
	}
	if !archiveTerminal(s.doJSON(t.Context(), claudeprofile.ControlEndpointSessionArchive, "session_old", struct{}{}, nil)) {
		t.Fatal("missing response body hid terminal HTTP status")
	}
	for _, body := range []string{`{"error":{"resource":"other","message":"trusted device"}}`, `{"message":"other","error":{"message":"trusted device"}}`, `{"error":{"resource":null,"message":"trusted device"}}`} {
		if isUntrustedDevice([]byte(body)) {
			t.Fatal("native resource/message precedence lost", body)
		}
	}
}

func TestPlaceholderActualProcessIdentity(t *testing.T) {
	a, err := inspectProcess(os.Getpid())
	if err != nil || a.Dead || a.Start == "" {
		t.Fatal("actual process identity unavailable", a, err)
	}
	b, err := inspectProcess(os.Getpid())
	if err != nil || a != b {
		t.Fatal("live process identity unstable", a, b, err)
	}
	for _, pid := range []int{-1, 0, 1} {
		owner, err := inspectProcess(pid)
		if err == nil || owner.Dead {
			t.Fatal("invalid PID became dead proof", pid)
		}
	}
}

func TestPlaceholderNativeEqualTimestampCap(t *testing.T) {
	m, _, _ := placeholderFixture(t, 200, `{}`, 200)
	m.processProbe = func(int) (processIdentity, error) { return processIdentity{Start: "owned"}, nil }
	for i := 0; i < 21; i++ {
		if err := m.registerPlaceholder(fmt.Sprintf("cse_%02d", 20-i)); err != nil {
			t.Fatal(err)
		}
	}
	state, err := m.loadPlaceholdersLocked()
	if err != nil || len(state.Order) != 20 || state.Order[0] != "cse_20" || state.Order[19] != "cse_01" {
		t.Fatal("equal timestamp native order changed", state.Order, err)
	}
	if _, ok := state.Records["cse_00"]; ok {
		t.Fatal("new tied record displaced an older tied entry")
	}
}

func TestPlaceholderHostOwnsOneShotTimerAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			m, s, requests := placeholderFixture(t, 404, `{}`, 200)
			seedPlaceholder(t, m, "cse_old", processIdentity{Start: "old"}, 6*time.Minute)
			m.disableLoops = false
			s.placeholderRegistered = true
			synctest.Test(t, func(t *testing.T) {
				s.ctx, s.cancel = context.WithCancel(context.Background())
				defer s.cancel()
				s.opMu.Lock()
				s.preparePlaceholderLocked()
				s.preparePlaceholderLocked()
				s.opMu.Unlock()
				synctest.Wait()
				time.Sleep(placeholderStartDelay - time.Millisecond)
				if len(*requests) != 0 {
					t.Fatal("sweep ran before native delay")
				}
				if cancelEarly {
					s.cancel()
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				s.loops.Wait()
				want := 1
				if cancelEarly {
					want = 0
				}
				if len(*requests) != want {
					t.Fatal("Host timer repeated or escaped cancellation", *requests)
				}
			})
		})
	}
}

func TestPlaceholderRegistrationFailureDoesNotRejectRequest(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	if err := writeProtectedState(m.placeholderPath(), placeholderState{Version: 9}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(m.placeholderPath())
	facts := queryFacts("failed-journal", t.Context())
	facts.PlaceholderSweepEnabled = func() (bool, error) { return true, nil }
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts)
	if err != nil || span == nil || !m.Status().PlaceholderStorageFailed {
		t.Fatal("optional journal rejected request or hid failure", err, m.Status())
	}
	after, _ := os.ReadFile(m.placeholderPath())
	if string(after) != string(before) {
		t.Fatal("unknown journal overwritten")
	}
}

func TestPlaceholderSweepCancellationInterruptsIOWithoutArchive(t *testing.T) {
	m, s, _ := placeholderFixture(t, 200, `{}`, 200)
	seedPlaceholder(t, m, "cse_old", processIdentity{Start: "old"}, 6*time.Minute)
	started := make(chan struct{})
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	m.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
		return placeholderDoer(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet {
				t.Error("canceled scan archived a session")
			}
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}), nil
	}
	done := make(chan error, 1)
	go func() { done <- s.sweepPlaceholders() }()
	awaitQueryControl(t, started)
	s.cancel()
	if err := awaitQueryControl(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m.Status().PlaceholderPending != 1 {
		t.Fatal("canceled scan discarded recovery state")
	}
}
