package auth

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func healthTestManager(t *testing.T, dir, token string) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{AuthDir: dir})
	a := &Auth{ID: "health-test.json", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": token}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return m, a
}

func TestCredentialHealthCapturesStreamBootstrapFailure(t *testing.T) {
	executor := &claudeCancellationTestExecutor{
		streamFn: func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
			return nil, compactTestStatusError{code: 401, msg: `{"error":{"message":"OAuth access token has been revoked."}}`}
		},
	}
	m, auth, model := newClaudeCancellationTestManager(t, executor, nil)
	m.SetConfig(&config.Config{AuthDir: t.TempDir()})
	_, err := m.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("synthetic revoked credential unexpectedly succeeded")
	}
	if health := m.CredentialHealthSnapshot(auth.ID); health == nil || health.State != "credential_revoked" || health.Source != "upstream_response" {
		t.Fatalf("stream failure did not reach passive health observation: %+v", health)
	}
}

func TestCredentialHealthFailedImportDoesNotChangeMemory(t *testing.T) {
	dir := t.TempDir()
	m, a := healthTestManager(t, dir, "token")
	if err := os.WriteFile(dir+"/.credential-health", []byte("blocks directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err := m.ImportCredentialHealth(a.ID, credentialHealthFingerprint(a), &Error{HTTPStatus: 401, Message: "OAuth access token has been revoked."}, now, now)
	if err == nil || m.CredentialHealthSnapshot(a.ID) != nil {
		t.Fatal("failed import left a successful-looking in-memory observation")
	}
}

func TestCredentialHealthClassification(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
		status              int
	}{
		{"revoked", `{"type":"error","error":{"type":"authentication_error","message":"OAuth access token has been revoked."}}`, "credential_revoked", 401},
		{"account", "Your account has been disabled.", "account_disabled", 403},
		{"organization", `{"error":{"message":"This organization has been disabled."}}`, "organization_disabled", 403},
		{"permission_only", "Access denied", "permission_denied", 403},
		{"not_a_ban", "The requested feature is disabled for your account", "permission_denied", 403},
		{"quoted_account", "The prompt mentions: your account has been banned", "permission_denied", 403},
		{"expired", "OAuth token expired", "authentication_failed", 401},
		{"quota", "Your account has been disabled.", "rate_limited", 429},
		{"transport", "broken pipe", "", 500},
		{"request", "Your account has been disabled.", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCredentialHealth(&Error{HTTPStatus: tc.status, Message: tc.message})
			if tc.want == "" {
				if got != nil {
					t.Fatalf("unexpected observation: %+v", got)
				}
			} else if got == nil || got.State != tc.want {
				t.Fatalf("health = %+v, want %s", got, tc.want)
			}
		})
	}
	if classifyCredentialHealth(NewRequestScopedError("Access denied", 403)) != nil {
		t.Fatal("request-scoped error was classified as credential failure")
	}
}

func TestCredentialHealthSurvivesRestartAndEnablementWithoutTokenWrites(t *testing.T) {
	dir := t.TempDir()
	m, auth := healthTestManager(t, dir, "synthetic-private-access-token")
	ctx := context.Background()
	m.MarkResult(ctx, Result{AuthID: auth.ID, Model: "model", Error: &Error{HTTPStatus: 401, Message: "OAuth access token has been revoked."}})
	first := m.CredentialHealthSnapshot(auth.ID)
	if first == nil || first.State != "credential_revoked" || first.FirstObservedAt.IsZero() {
		t.Fatalf("missing automatic observation: %+v", first)
	}
	for _, result := range []Result{
		{AuthID: auth.ID, Error: &Error{HTTPStatus: 500, Message: "broken pipe"}},
		{AuthID: auth.ID, Error: &Error{HTTPStatus: 429, Message: "rate limit"}},
		{AuthID: auth.ID, Success: true},
	} {
		m.MarkResult(ctx, result)
	}
	updated, _ := m.GetByID(auth.ID)
	updated.Disabled = true
	if _, err := m.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	updated.Disabled = false
	if _, err := m.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	restarted, _ := healthTestManager(t, dir, "synthetic-private-access-token")
	got := restarted.CredentialHealthSnapshot(auth.ID)
	if got == nil || got.State != "credential_revoked" || !got.ResolvedAt.IsZero() || !got.FirstObservedAt.Equal(first.FirstObservedAt) {
		t.Fatalf("restart/toggle/late success erased evidence: %+v", got)
	}
	data, err := os.ReadFile(m.credentialHealthPath(auth))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic-private-access-token") {
		t.Fatal("persisted secret")
	}
	var saved map[string]any
	if json.Unmarshal(data, &saved) != nil {
		t.Fatal("invalid persisted record")
	}
	if _, ok := auth.Metadata["credential_health"]; ok {
		t.Fatal("health mutated credential metadata")
	}
	rotated, _ := healthTestManager(t, dir, "replacement-token")
	if rotated.CredentialHealthSnapshot(auth.ID) != nil {
		t.Fatal("old token failure attached to replacement token")
	}
}

func TestCredentialHealthTransientRecoveryAndStaleCredential(t *testing.T) {
	m, a := healthTestManager(t, t.TempDir(), "old-token")
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Error: &Error{HTTPStatus: 429, Message: "limit"}})
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Success: true})
	if health := m.CredentialHealthSnapshot(a.ID); health == nil || health.ResolvedAt.IsZero() {
		t.Fatal("successful request did not resolve transient observation")
	}
	oldFingerprint := credentialHealthFingerprint(a)
	a.Metadata["access_token"] = "new-token"
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m.MarkResult(context.Background(), Result{AuthID: a.ID, credentialFingerprint: oldFingerprint, Error: &Error{HTTPStatus: 401, Message: "OAuth access token has been revoked."}})
	if m.CredentialHealthSnapshot(a.ID) != nil {
		t.Fatal("late result poisoned replacement credential")
	}
}

func TestCredentialHealthHistoricalImportDoesNotFabricateTraffic(t *testing.T) {
	m, a := healthTestManager(t, t.TempDir(), "token")
	first := time.Now().Add(-time.Hour).UTC()
	last := first.Add(time.Minute)
	failure := &Error{HTTPStatus: 401, Message: "OAuth access token has been revoked."}
	if err := m.ImportCredentialHealth(a.ID, credentialHealthFingerprint(a), failure, first, last); err != nil {
		t.Fatal(err)
	}
	health := m.CredentialHealthSnapshot(a.ID)
	current, _ := m.GetByID(a.ID)
	if health.Source != "historical_log" || !health.FirstObservedAt.Equal(first) || current.Success != 0 || current.Failed != 0 || current.Disabled {
		t.Fatal("historical import mutated scheduling/counters or lost provenance")
	}
	if err := m.ImportCredentialHealth(a.ID, "wrong-token", failure, first, last); err == nil {
		t.Fatal("accepted stale credential")
	}
	if err := m.ImportCredentialHealth(a.ID, credentialHealthFingerprint(a), failure, first, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("accepted future evidence")
	}
}
