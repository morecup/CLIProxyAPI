package executor

import (
	"errors"
	"fmt"
	"testing"

	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type remotePolicyUnavailableStore struct{}

func (remotePolicyUnavailableStore) Load(string) ([]byte, string, error) {
	return nil, "", errors.New("synthetic unavailable feature cache")
}

func (remotePolicyUnavailableStore) Save(string, string, []byte) (string, error) {
	return "", errors.New("synthetic unavailable feature cache")
}

func TestClaudeDesktopRemoteHydrationPolicyCacheFailureDoesNotVeto(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	inner := accountRuntimeForAuth(t, e, auths[0].ID).executor
	host := claudefeatures.NewHost(remotePolicyUnavailableStore{}, "synthetic-cache-failure-session")
	t.Cleanup(host.Close)
	var unhealthy bool
	inner.desktopATIS.featureHostObserverFactory = func(_ *cliproxyauth.Auth, owner string) (func(bool) error, func(), error) {
		if owner != "sdk-query:"+host.ID() {
			t.Error("feature cache failure reported against another session")
		}
		return func(healthy bool) error { unhealthy = unhealthy || !healthy; return nil }, func() {}, nil
	}
	policy, err := inner.desktopRemoteHydrationPolicy(auths[0], host)
	if err != nil || policy != (helps.ClaudeDesktopRemoteHydrationPolicy{}) || !unhealthy {
		t.Fatal("cache failure vetoed recovery, changed fallback or disappeared from health", policy, err, unhealthy)
	}
}

func TestClaudeDesktopRemoteHydrationPolicyUsesOwnedHost(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	var exposures []claudefeatures.Exposure
	for account, auth := range auths {
		inner := accountRuntimeForAuth(t, e, auth.ID).executor
		inner.desktopATIS.featureSink = func(current *cliproxyauth.Auth, exposure claudefeatures.Exposure) (bool, error) {
			if current.ID != auth.ID {
				t.Error("hydration exposure crossed account ownership")
			}
			exposures = append(exposures, exposure)
			return true, nil
		}
		seed := func(host *claudefeatures.Host, delta, skip string) {
			t.Helper()
			ticket, sink, err := inner.desktopATIS.bindFeatureHost(auth, host)
			if err != nil {
				t.Fatal(err)
			}
			payload := fmt.Sprintf(`{"features":{
				"tengu_ccr_delta_rehydrate":{"value":%s,"source":"experiment","experiment":{"key":"delta-policy"},"experimentResult":{"variationId":1}},
				"tengu_ccr_subagent_skip_on_delta":{"value":%s,"source":"experiment","experiment":{"key":"skip-policy"},"experimentResult":{"variationId":0}}
			}}`, delta, skip)
			if accepted, err := host.Service().Observe(ticket, []byte(payload), host.SessionID(), sink); !accepted || err != nil {
				t.Fatal("seed owned feature response", err)
			}
		}
		warm, err := inner.desktopATIS.featureHosts.Main("policy-warm", "")
		if err != nil {
			t.Fatal(err)
		}
		// Leave the prewarm host with opposite values. The target's in-memory
		// decision must win even when another host later updates the disk cache.
		seed(warm, "false", "true")
		host, err := inner.desktopATIS.featureHosts.Main("policy-owned", "")
		if err != nil || host == warm {
			t.Fatal("test did not establish separate SDK hosts", err)
		}
		seed(host, `"false"`, "0")
		seed(warm, "false", "true")
		before := len(exposures)
		for read := 0; read < 2; read++ {
			policy, err := inner.desktopRemoteHydrationPolicy(auth, host)
			if err != nil || policy != (helps.ClaudeDesktopRemoteHydrationPolicy{DeltaEnabled: true}) {
				t.Fatal("policy borrowed prewarm values or parsed a string as a boolean", account, policy, err)
			}
		}
		if len(exposures) != before+2 || exposures[before].SessionID != host.SessionID() || exposures[before+1].SessionID != host.SessionID() ||
			exposures[before].FeatureID != "tengu_ccr_delta_rehydrate" || exposures[before+1].FeatureID != "tengu_ccr_subagent_skip_on_delta" {
			t.Fatal("hydration reads lost native order, host identity or exposure dedup")
		}
		host.Close()
		if _, err := inner.desktopRemoteHydrationPolicy(auth, host); !errors.Is(err, claudefeatures.ErrStale) {
			t.Fatal("retired owner still evaluated hydration policy", err)
		}
		if len(exposures) != before+2 || warm.Context().Err() != nil {
			t.Fatal("retired read exposed another query or retired its sibling")
		}
	}
}
