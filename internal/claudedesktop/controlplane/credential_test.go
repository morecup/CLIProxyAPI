package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type testControlCredentials struct {
	current    *cliproxyauth.Auth
	errCurrent error
	refresh    func(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
	refreshes  int
}

func testActiveControlAuth(t *testing.T) *cliproxyauth.Auth {
	t.Helper()
	auth := testDesktopAuth(t)
	if _, err := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentActive, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	return auth
}

func (c *testControlCredentials) Current(*cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return c.current.Clone(), c.errCurrent
}
func (c *testControlCredentials) Refresh(ctx context.Context, previous *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	c.refreshes++
	return c.refresh(ctx, previous)
}

type credentialControlDoer struct {
	recordingControlDoer
	statuses      []int
	archives      int
	archiveBodies []string
	access        []string
}

type credentialBudgetDoerFunc func(*http.Request) (*http.Response, error)

func (f credentialBudgetDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (d *credentialControlDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := d.recordingControlDoer.Do(request)
	if strings.HasSuffix(request.URL.Path, "/archive") {
		d.access = append(d.access, request.Header.Get("Authorization"))
		d.archives++
		index := d.archives - 1
		if index < len(d.statuses) {
			response.StatusCode = d.statuses[index]
		}
		if index < len(d.archiveBodies) {
			response.Body = io.NopCloser(strings.NewReader(d.archiveBodies[index]))
		}
	}
	return response, err
}

func TestTeardownOAuthRefreshOnceStatusMatrix(t *testing.T) {
	for _, first := range []int{200, 400, 401, 403, 404, 408, 429, 500, 502} {
		for _, second := range []int{200, 401, 404, 502} {
			t.Run(fmt.Sprintf("%d-%d", first, second), func(t *testing.T) {
				auth := testActiveControlAuth(t)
				original := auth.Clone()
				source := &testControlCredentials{current: auth.Clone()}
				source.refresh = func(ctx context.Context, failed *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
					if failed.Metadata["access_token"] != "account-token" {
						t.Fatal("wrong failed credential")
					}
					failed.Metadata["access_token"] = "refreshed-account-token"
					source.current = failed.Clone()
					return failed, nil
				}
				doer := &credentialControlDoer{statuses: []int{first, second}}
				bundle, err := claudeprofile.BuiltinV140609()
				if err != nil {
					t.Fatal(err)
				}
				m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, Credentials: source,
					DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }})
				facts := queryFacts("refresh", t.Context())
				facts.PlaceholderSweepEnabled = func() (bool, error) { return true, nil }
				span, err := m.BeginRequest(t.Context(), auth, facts)
				if err != nil {
					t.Fatal(err)
				}
				m.Close()
				wantAttempts, final := 1, first
				if first == 401 {
					wantAttempts, final = 2, second
				}
				if doer.archives != wantAttempts || source.refreshes != wantAttempts-1 {
					t.Fatal("retry count", doer.archives, source.refreshes)
				}
				if span.session.state.Archived != (final == 200) || (m.Status().Failed == 0) != (final == 200) {
					t.Fatal("shutdown status", m.Status())
				}
				terminal := final < 500 && final != 401 && final != 408 && final != 429
				if (m.Status().PlaceholderPending == 0) != terminal {
					t.Fatal("placeholder disposition", m.Status())
				}
				if doer.access[0] != "Bearer account-token" || (wantAttempts == 2 && doer.access[1] != "Bearer refreshed-account-token") {
					t.Fatal("credential selection")
				}
				if auth.Metadata["access_token"] != original.Metadata["access_token"] {
					t.Fatal("request auth mutated")
				}
				m.Close()
				if doer.archives != wantAttempts {
					t.Fatal("repeated close repeated archive")
				}
			})
		}
	}
}

func TestTeardownRefreshDoesNotReplayUnsafeOrUnownedCredentials(t *testing.T) {
	for _, variant := range []string{"snapshot-only", "untrusted-device", "refresh-error", "nil-result", "account-change", "proxy-change", "disabled", "device-change", "cancel"} {
		t.Run(variant, func(t *testing.T) {
			auth := testActiveControlAuth(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := &testControlCredentials{current: auth.Clone()}
			source.refresh = func(_ context.Context, failed *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
				switch variant {
				case "refresh-error":
					return nil, errors.New("synthetic refresh failure")
				case "nil-result":
					return nil, nil
				case "account-change":
					failed.ID = "other"
				case "proxy-change":
					failed.ProxyURL = "http://127.0.0.1:9999"
				case "disabled":
					failed.Disabled = true
				case "device-change":
					failed.Metadata[claudedesktop.MetadataTrustedDeviceTokenKey] = ""
				case "cancel":
					cancel()
				default:
					t.Fatal("unexpected refresh")
				}
				source.current = failed.Clone()
				return failed, nil
			}
			doer := &credentialControlDoer{statuses: []int{401, 200}}
			bundle, err := claudeprofile.BuiltinV140609()
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, Credentials: source,
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }})
			span, err := m.BeginRequest(t.Context(), auth, queryFacts("refresh", t.Context()))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Quarantine()
			if variant == "snapshot-only" {
				m.credentials = nil
			}
			if variant == "untrusted-device" {
				doer.statuses[0] = 403
				doer.archiveBodies = []string{`{"error":{"resource":"untrusted_device"}}`}
			}
			span.session.opMu.Lock()
			err = span.session.archiveOnTeardownLocked(ctx)
			span.session.opMu.Unlock()
			if err == nil || doer.archives != 1 {
				t.Fatal("unexpected replay", err, doer.archives)
			}
			if variant == "snapshot-only" || variant == "untrusted-device" {
				if source.refreshes != 0 {
					t.Fatal("unowned or non-OAuth refresh")
				}
			}
		})
	}
}

func TestOAuthControlRequestReadsLatestOwnerWithoutChangingWorkerJWT(t *testing.T) {
	auth := testActiveControlAuth(t)
	source := &testControlCredentials{current: auth.Clone()}
	doer := &credentialControlDoer{}
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, Credentials: source,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }})
	span, err := m.BeginRequest(t.Context(), auth, queryFacts("latest", t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Quarantine()
	source.current.Metadata["access_token"] = "current-token"
	s := span.session
	s.opMu.Lock()
	defer s.opMu.Unlock()
	request, _, err := s.buildRequest(t.Context(), claudeprofile.ControlEndpointSessionRead, s.state.ArchiveSessionID, nil)
	if err != nil || request.Header.Get("Authorization") != "Bearer current-token" {
		t.Fatal("stale OAuth credential", err)
	}
	request, _, err = s.buildRequest(t.Context(), claudeprofile.ControlEndpointWorkerRead, s.state.RemoteSessionID, nil)
	if err != nil || request.Header.Get("Authorization") != "Bearer worker-token" {
		t.Fatal("worker credential changed", err)
	}
	source.errCurrent = cliproxyauth.ErrCredentialOwnerChanged
	_, _, err = s.buildRequest(t.Context(), claudeprofile.ControlEndpointSessionRead, s.state.ArchiveSessionID, nil)
	if !errors.Is(err, cliproxyauth.ErrCredentialOwnerChanged) {
		t.Fatal("old owner admitted", err)
	}
}

func TestTeardownRefreshEligibilityUsesNativeRemainingBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budget  int
		elapsed int64
		want    int
	}{
		{"default-more-than-200", 1500, 1299, 2},
		{"default-exactly-200", 1500, 1300, 2},
		{"default-only-199", 1500, 1301, 1},
		{"already-exhausted", 1500, 2000, 1},
		{"minimum-exactly-200", 500, 300, 2},
		{"minimum-only-199", 500, 301, 1},
		{"maximum-exactly-200", 2000, 1800, 2},
		{"wall-clock-moved-back", 1500, -1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := testActiveControlAuth(t)
			source := &testControlCredentials{current: auth.Clone()}
			now := time.UnixMilli(10000)
			source.refresh = func(ctx context.Context, failed *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
				if _, hasDeadline := ctx.Deadline(); hasDeadline {
					t.Fatal("eligibility installed a network deadline")
				}
				// The native branch tests eligibility before refresh, not again
				// afterward. Its separate acquisition race is not a network deadline.
				now = now.Add(3 * time.Second)
				failed.Metadata["access_token"] = "synthetic-refreshed"
				source.current = failed.Clone()
				return failed, nil
			}
			bundle, err := claudeprofile.BuiltinV140609()
			if err != nil {
				t.Fatal(err)
			}
			bundle.ControlPlane.TeardownArchiveBudgetMillis = tc.budget
			doer := &credentialControlDoer{statuses: []int{401, 204}}
			m := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, Credentials: source, DisableLoops: true, Now: func() time.Time { return now },
				DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
					return credentialBudgetDoerFunc(func(request *http.Request) (*http.Response, error) {
						if _, hasDeadline := request.Context().Deadline(); hasDeadline {
							t.Fatal("archive request acquired a network deadline")
						}
						response, errDo := doer.Do(request)
						if strings.HasSuffix(request.URL.Path, "/archive") && doer.archives == 1 {
							now = now.Add(time.Duration(tc.elapsed) * time.Millisecond)
						}
						return response, errDo
					}), nil
				}})
			if _, err = m.BeginRequest(context.Background(), auth, queryFacts("budget", t.Context())); err != nil {
				t.Fatal(err)
			}
			m.Close()
			if doer.archives != tc.want || source.refreshes != tc.want-1 {
				t.Fatalf("archive/refresh attempts = %d/%d, want %d/%d", doer.archives, source.refreshes, tc.want, tc.want-1)
			}
		})
	}
}
