package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func newQueryOwnerTestExecutor(t *testing.T) (*ClaudeExecutor, *cliproxyauth.Auth) {
	t.Helper()
	e := newClaudeDesktopTestExecutor(t)
	m, auth := nativeATISTestManager(t, func(model string) any { return "PIN-" + model })
	e.desktopATIS = m
	m.profileID = e.desktopProfile.ProfileID
	t.Cleanup(m.Close)
	return e, auth
}

func TestClaudeDesktopQueryOwnerSelectsExactHostForSharedTranscript(t *testing.T) {
	e, auth := newQueryOwnerTestExecutor(t)
	m := e.desktopATIS
	callerA, callerB := uuid.NewString(), uuid.NewString()
	ctxA, session := e.bindClaudeDesktopQueryContext(t.Context(), auth, callerA, claudeprofile.RoleMain)
	ctxB, otherSession := e.bindClaudeDesktopQueryContext(t.Context(), auth, callerB, claudeprofile.RoleMain)
	a := ctxA.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext).host
	b := ctxB.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext).host
	if a == nil || b == nil || a == b || session == otherSession {
		t.Fatal("test did not establish independent query roots")
	}
	// This is an explicit synthetic native reidentity, not a claim that the
	// Desktop fork/resume action producer is implemented by this test.
	if err := b.Reidentify(session); err != nil {
		t.Fatal(err)
	}
	ctxB, _ = e.bindClaudeDesktopQueryContext(t.Context(), auth, callerB, claudeprofile.RoleMain)
	seen := map[string]int{}
	m.featureSink = func(_ *cliproxyauth.Auth, exposure claudefeatures.Exposure) (bool, error) {
		if exposure.SessionID != session {
			t.Error("feature exposure lost native transcript identity")
		}
		seen[exposure.ExperimentID]++
		return true, nil
	}
	for i, host := range []*claudefeatures.Host{a, b} {
		_, observe, err := m.prepareSDKFeatureHost(auth, host)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte(`{"features":{"tengu_kestrel_moor":{"value":false,"source":"experiment","experiment":{"key":"query-a"},"experimentResult":{"variationId":0}}}}`)
		if i == 1 {
			payload = []byte(`{"features":{"tengu_kestrel_moor":{"value":true,"source":"experiment","experiment":{"key":"query-b"},"experimentResult":{"variationId":1}}}}`)
		}
		if err := observe(payload); err != nil {
			t.Fatal(err)
		}
	}
	if host, err := m.featureHosts.FindSession(session); host != nil || !errors.Is(err, claudefeatures.ErrAmbiguousSession) {
		t.Fatal("test did not produce two live query owners", err)
	}
	for _, ctx := range []context.Context{ctxA, ctxB, ctxA, ctxB} {
		pin, err := m.Assignment(ctx, auth, session, "claude-sonnet-5", "claude-sonnet-5", claudeprofile.RoleMain)
		if err != nil || pin != "PIN-claude-sonnet-5" {
			t.Fatal(pin, err)
		}
	}
	if seen["query-a"] != 1 || seen["query-b"] != 1 || len(seen) != 2 {
		t.Fatal("request selected another query's gate/exposure set", seen)
	}
	if m.featureHosts.Lookup(claudeDesktopATISScopeKey(session)) != nil {
		t.Fatal("ATIS allocated a third fallback host")
	}
	for _, parent := range []context.Context{ctxA, ctxB} {
		owner := parent.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		child := cliproxyexecutor.WithClaudeDesktopSessionBinding(parent, cliproxyexecutor.ClaudeDesktopSessionBinding{
			AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: session})
		for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleCompaction, claudeprofile.RoleCountTokens} {
			bound, actual := e.bindClaudeDesktopQueryContext(child, auth, uuid.NewString(), role)
			if actual != session || m.queryHostFromContext(bound, auth, owner.identity, session) != owner.host {
				t.Fatal("trusted helper lost its exact query owner", role)
			}
			if _, err := m.Assignment(bound, auth, session, "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", role); err != nil {
				t.Fatal(err)
			}
		}
	}
	if seen["query-a"] != 1 || seen["query-b"] != 1 {
		t.Fatal("helper reconstructed the query's exposure state", seen)
	}
	a.Close()
	stale := cliproxyexecutor.WithClaudeDesktopSessionBinding(ctxA, cliproxyexecutor.ClaudeDesktopSessionBinding{
		AccountID: auth.ID, ProfileID: e.desktopProfile.ProfileID, Egress: auth.ProxyURL, SessionID: session})
	stale, actual := e.bindClaudeDesktopQueryContext(stale, auth, uuid.NewString(), claudeprofile.RoleCompaction)
	if actual != session || stale.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext).host != nil {
		t.Fatal("retired query migrated to the surviving query on its transcript")
	}
	if _, err := m.Assignment(stale, auth, session, "claude-sonnet-5", "claude-sonnet-5", claudeprofile.RoleMain); !errors.Is(err, errClaudeDesktopQueryOwnerUnavailable) {
		t.Fatal("retired request silently created or selected a different query", err)
	}
	if m.featureHosts.Lookup(claudeDesktopATISScopeKey(session)) != nil || seen["query-b"] != 1 {
		t.Fatal("stale request created a fallback query or consumed the surviving query")
	}
}

func TestClaudeDesktopAmbiguousQueryOwnerKeepsBootstrapWithoutInventingHost(t *testing.T) {
	e, auth := newQueryOwnerTestExecutor(t)
	m := e.desktopATIS
	session := uuid.NewString()
	_, _ = m.featureHosts.Main("first-explicit-query", session)
	_, _ = m.featureHosts.Main("second-explicit-query", session)
	issues := 0
	m.featureStateObserver = func(_ *cliproxyauth.Auth, _ string, healthy bool) error {
		if !healthy {
			issues++
		}
		return nil
	}
	m.featureSink = func(*cliproxyauth.Auth, claudefeatures.Exposure) (bool, error) {
		t.Fatal("ambiguous request fabricated a feature read")
		return false, nil
	}
	pin, err := m.Assignment(t.Context(), auth, session, "claude-opus-5", "claude-opus-5", claudeprofile.RoleMain)
	if pin != "PIN-claude-opus-5" || !errors.Is(err, claudefeatures.ErrAmbiguousSession) || issues != 1 {
		t.Fatal("ambiguous ownership lost usable bootstrap data or its diagnostic", pin, err, issues)
	}
	if m.featureHosts.Lookup(claudeDesktopATISScopeKey(session)) != nil || m.Latch(session) != nil {
		t.Fatal("unowned request created a query or established a native latch")
	}
}

func TestClaudeDesktopQueryOwnerRejectsForeignAndRetiredBindings(t *testing.T) {
	e, auth := newQueryOwnerTestExecutor(t)
	m := e.desktopATIS
	ctx, session := e.bindClaudeDesktopQueryContext(t.Context(), auth, uuid.NewString(), claudeprofile.RoleMain)
	owner := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
	enrollment, err := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	identity := claudeDesktopATISIdentityHash(enrollment)
	for _, field := range []string{"runtime", "account", "profile", "egress", "identity", "session"} {
		t.Run(field, func(t *testing.T) {
			foreign := owner
			switch field {
			case "runtime":
				foreign.manager = nil
			case "account":
				foreign.accountID += "-foreign"
			case "profile":
				foreign.profileID += "-foreign"
			case "egress":
				foreign.egress += "-foreign"
			case "identity":
				foreign.identity += "-foreign"
			case "session":
				foreign.session = uuid.NewString()
			}
			foreignContext := context.WithValue(ctx, claudeDesktopQueryContextKey{}, foreign)
			if m.queryHostFromContext(foreignContext, auth, identity, session) != nil {
				t.Fatal("foreign query owner was accepted")
			}
			shadowed, _ := e.bindClaudeDesktopQueryContext(foreignContext, auth, uuid.NewString(), claudeprofile.RoleCountTokens)
			if shadowed.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext).host != nil {
				t.Fatal("unowned request retained inherited query ownership")
			}
		})
	}
	owner.host.Close()
	if m.queryHostFromContext(ctx, auth, identity, session) != nil {
		t.Fatal("closed query context remained usable")
	}
}
