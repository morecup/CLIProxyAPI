package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBridgeTeardownObservesFinalHTTPOutcomeOnce(t *testing.T) {
	for _, status := range []int{200, 204, 400, 401, 403, 404, 408, 429, 500, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m := newQueryControlManager(t, &queryControlDoer{archiveStatus: status})
			facts := queryFacts("bridge-events", t.Context())
			facts.PromptID = "77777777-7777-4777-8777-777777777777"
			var events []BridgeEvent
			facts.BridgeObserver = func(event BridgeEvent) error { events = append(events, event); return nil }
			span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts)
			if err != nil {
				t.Fatal(err)
			}
			m.Close()
			m.Close()
			if len(events) != 1 || events[0].Kind != BridgeTeardown || span.session.ctx.Err() == nil {
				t.Fatal("teardown did not produce one post-cancellation event", events)
			}
			event := events[0]
			want := BridgeOwner{facts.DesktopSessionID, facts.QueryID, facts.LocalSessionID}
			if event.Owner != want || event.Model != facts.Model || event.PromptID != facts.PromptID || event.At.IsZero() {
				t.Fatal("lost actual query context", event)
			}
			if event.Archive == nil || event.Archive.HTTPStatus == nil || *event.Archive.HTTPStatus != status || event.Archive.OK != (status < 400) || event.Archive.Timeout || event.Archive.NoToken || event.Archive.Credential != "current" {
				t.Fatal("archive outcome was inferred or dropped", event.Archive)
			}
		})
	}
}

func TestBridgeArchiveOutcomeDoesNotInventResponseOrTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		err        error
		noToken    bool
	}{
		{"network", "network_error", errors.New("synthetic"), false},
		{"canceled", "network_error", context.Canceled, false},
		{"deadline-is-not-native-budget", "network_error", context.DeadlineExceeded, false},
		{"missing-token", "skipped_no_token", errMissingOAuthAccessToken, true},
		{"owner-changed", "skipped_owner_changed", cliproxyauth.ErrCredentialOwnerChanged, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := bridgeArchiveOutcome(0, tc.err)
			if outcome.Status != tc.want || outcome.HTTPStatus != nil || outcome.OK || outcome.Timeout || outcome.NoToken != tc.noToken {
				t.Fatal(outcome)
			}
		})
	}
	if got := bridgeArchiveOutcome(403, &statusError{code: 403, untrustedDevice: true}); got.Status != "server_403_untrusted" {
		t.Fatal(got)
	}
}

func TestBridgeUsedPlaceholderIsObservedWithoutChangingRemovalPolicy(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			m, s, requests := placeholderFixture(t, 200, `{"created_at":"first","updated_at":"second"}`, 200)
			seedPlaceholder(t, m, "cse_orphan", processIdentity{Start: "old"}, 6*time.Minute)
			s.desktopID, s.queryID, s.state.LocalSessionID = "desktop", "query", "sdk"
			s.bridgeModel, s.bridgePromptID = "claude-opus-5", "current-main"
			var events []BridgeEvent
			s.bridgeObserver = func(event BridgeEvent) error {
				events = append(events, event)
				if fail {
					return errors.New("synthetic queue failure")
				}
				return nil
			}
			err := s.sweepPlaceholders()
			if (err != nil) != fail || len(events) != 1 || events[0].Kind != BridgePlaceholderUsed || events[0].Archive != nil || events[0].Owner.SDKSessionID != "sdk" || events[0].PromptID != "current-main" {
				t.Fatal(events, err)
			}
			if len(*requests) != 1 || m.Status().PlaceholderPending != 0 || m.Status().PlaceholderSweepFailed != fail {
				t.Fatal("logging changed native cleanup or hid failure", *requests, m.Status())
			}
			_ = s.sweepPlaceholders()
			if len(events) != 1 {
				t.Fatal("removed marker repeated used-session event")
			}
		})
	}
}

func TestBridgeHelperCannotReplaceMainTelemetryContext(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	facts := queryFacts("bridge-main", t.Context())
	facts.PromptID = "main-prompt"
	var events []BridgeEvent
	facts.BridgeObserver = func(event BridgeEvent) error { events = append(events, event); return nil }
	auth := testDesktopAuth(t)
	if _, err := m.BeginRequest(t.Context(), auth, facts); err != nil {
		t.Fatal(err)
	}
	facts.Role, facts.Model, facts.PromptID = claudeprofile.RoleSubagent, "claude-haiku-4-5-20251001", "helper-prompt"
	facts.BridgeObserver = func(BridgeEvent) error { t.Error("helper replaced lifecycle owner"); return nil }
	if _, err := m.BeginRequest(t.Context(), auth, facts); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if len(events) != 1 || events[0].PromptID != "main-prompt" || events[0].Model == facts.Model {
		t.Fatal("helper contaminated main lifecycle dimensions", events)
	}
}

func TestBridgeQuarantineDoesNotEmitGracefulTeardown(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	facts := queryFacts("quarantine-events", t.Context())
	facts.BridgeObserver = func(BridgeEvent) error { t.Error("quarantine emitted graceful lifecycle"); return nil }
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts); err != nil {
		t.Fatal(err)
	}
	m.Quarantine()
}

func TestBridgeTeardownReportsOnlyFinalRefreshAttempt(t *testing.T) {
	for _, final := range []int{200, 401, 502} {
		t.Run(fmt.Sprint(final), func(t *testing.T) {
			m := newQueryControlManager(t, &queryControlDoer{})
			auth := testActiveControlAuth(t)
			source := &testControlCredentials{current: auth.Clone()}
			source.refresh = func(_ context.Context, failed *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
				failed.Metadata["access_token"] = "synthetic-refreshed"
				source.current = failed.Clone()
				return failed, nil
			}
			m.credentials = source
			doer := &credentialControlDoer{statuses: []int{401, final}}
			m.doerFactory = func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }
			facts := queryFacts("retry-event", t.Context())
			var events []BridgeEvent
			facts.BridgeObserver = func(event BridgeEvent) error { events = append(events, event); return nil }
			if _, err := m.BeginRequest(t.Context(), auth, facts); err != nil {
				t.Fatal(err)
			}
			m.Close()
			if len(events) != 1 || events[0].Archive == nil || events[0].Archive.HTTPStatus == nil || *events[0].Archive.HTTPStatus != final || doer.archives != 2 || source.refreshes != 1 {
				t.Fatal("physical retry duplicated or contaminated final event", events, doer.archives, source.refreshes)
			}
		})
	}
}

func TestBridgeFailedAdmissionDoesNotInventRunningTeardown(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	base := m.doerFactory
	m.doerFactory = func(ctx context.Context, role string, auth *cliproxyauth.Auth) (HTTPDoer, error) {
		doer, err := base(ctx, role, auth)
		if err != nil {
			return nil, err
		}
		return placeholderDoer(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "/bridge") {
				return nil, errors.New("synthetic failed admission")
			}
			return doer.Do(request)
		}), nil
	}
	facts := queryFacts("failed-bridge", t.Context())
	facts.BridgeObserver = func(BridgeEvent) error { t.Error("provisional admission emitted running-bridge teardown"); return nil }
	if _, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts); err == nil {
		t.Fatal("expected bridge failure")
	}
	m.Close()
}
