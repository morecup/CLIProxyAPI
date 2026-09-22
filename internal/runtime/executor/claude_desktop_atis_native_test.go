package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func nativeATISTestManager(t *testing.T, payload func(string) any) (*claudeDesktopATISManager, *cliproxyauth.Auth) {
	t.Helper()
	auth := newClaudeAccountRuntimeTestAuth(t, "15000000-0000-4000-8000-000000000001", "26000000-0000-4000-8000-000000000001", "37000000-0000-4000-8000-000000000001")
	manager := newClaudeDesktopATISManager(t.TempDir(), "2.1.247", func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
		return claudeDesktopATISTestDoerFunc(func(request *http.Request) (*http.Response, error) {
			body, err := json.Marshal(map[string]any{"client_data": map[string]any{"atis": payload(request.URL.Query().Get("model"))}, "oauth_account": map[string]any{"account_uuid": auth.Metadata["account_uuid"], "organization_uuid": auth.Metadata["organization_uuid"]}})
			if err != nil {
				return nil, err
			}
			return claudeDesktopATISTestResponse(http.StatusOK, string(body)), nil
		}), nil
	})
	return manager, auth
}

func TestClaudeDesktopATISNativeSingleExposureReadAndHealthRecovery(t *testing.T) {
	manager, auth := nativeATISTestManager(t, func(string) any { return "PIN" })
	payload := []byte(`{"features":{"tengu_kestrel_moor":{"value":true,"source":"experiment","experiment":{"key":"exp"},"experimentResult":{"variationId":0}}}}`)
	if err := manager.ObserveSDKFeatures(auth, payload); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	accepted := false
	manager.featureSink = func(*cliproxyauth.Auth, claudefeatures.Exposure) (bool, error) { attempts++; return accepted, nil }
	for i := 1; i <= 3; i++ {
		if i == 2 {
			accepted = true
		}
		pin, err := manager.Assignment(t.Context(), auth, "session", "claude-sonnet-5", "claude-sonnet-5", claudeprofile.RoleMain)
		if err != nil || pin != "PIN" {
			t.Fatal(pin, err)
		}
		want := i
		if want > 2 {
			want = 2
		}
		if attempts != want {
			t.Fatalf("one request created duplicate retry opportunities: %d, want %d", attempts, want)
		}
	}
	var states []bool
	manager.featureStateObserver = func(_ *cliproxyauth.Auth, owner string, boolValue bool) error {
		if strings.HasPrefix(owner, "sdk-query:") {
			states = append(states, boolValue)
		}
		return nil
	}
	if err := manager.ObserveSDKFeatures(auth, []byte(`invalid`)); err == nil {
		t.Fatal("invalid response accepted")
	}
	if _, err := manager.featureEnabled(auth, "session"); err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0] {
		t.Fatalf("cached read repaired failed refresh: %v", states)
	}
	if err := manager.ObserveSDKFeatures(auth, payload); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || !states[1] {
		t.Fatal("healthy refresh did not repair feature state", states)
	}
}

func TestClaudeDesktopATISNativeBootstrapAndStickyLatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  any
		want string
	}{
		{"uppercase", "AbCD0123456789EF", "AbCD0123456789EF"},
		{"opaque", "Opaque-PIN_+/=", "Opaque-PIN_+/="},
		{"structured", "v1.PIN.part-a.part-b.Signature", "v1.PIN.part-a.part-b.Signature"},
		{"empty", "", ""},
		{"null", nil, ""},
		{"non-string", 42, ""},
		{"space-is-negative", " pin ", ""},
		{"non-ascii-is-negative", "pin\u00e9", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, auth := nativeATISTestManager(t, func(model string) any {
				if model == "claude-opus-5" {
					return "DIFFERENT-MODEL-PIN"
				}
				return tc.pin
			})
			for _, model := range []string{"claude-sonnet-5", "claude-opus-5"} {
				got, err := manager.Assignment(t.Context(), auth, "native-conversation", model, model, claudeprofile.RoleMain)
				if err != nil || got != tc.want {
					t.Fatalf("native latch changed or rejected a bootstrap: model=%s matches=%v err=%v", model, got == tc.want, err)
				}
			}
			restored := newClaudeDesktopATISManager(manager.statePath, manager.codeVersion, func(context.Context, *cliproxyauth.Auth) (claudeDesktopATISHTTPDoer, error) {
				return nil, fmt.Errorf("unexpected bootstrap after native latch restore")
			})
			got, err := restored.Assignment(t.Context(), auth, "native-conversation", "claude-opus-5", "claude-opus-5", claudeprofile.RoleMain)
			if err != nil || got != tc.want {
				t.Fatal("native positive or explicit negative latch did not survive protected state restore")
			}
		})
	}
}

func TestClaudeDesktopATISNativeGateUsesRealEvaluationAndPreservesLatch(t *testing.T) {
	manager, auth := nativeATISTestManager(t, func(model string) any {
		if model == "claude-sonnet-5" {
			return "PIN-ONE"
		}
		return "PIN-TWO"
	})
	if _, err := manager.Assignment(t.Context(), auth, "main", "claude-sonnet-5", "", claudeprofile.RoleMain); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, payload, want string
	}{
		{"false-value-beats-default", `{"features":{"tengu_kestrel_moor":{"value":false,"defaultValue":true}}}`, "PIN-TWO"},
		{"valueless-does-not-erase", `{"features":{"tengu_kestrel_moor":{"rules":[]}}}`, "PIN-TWO"},
		{"scalar-entry-does-not-erase", `{"features":{"tengu_kestrel_moor":true}}`, "PIN-TWO"},
		{"true-default", `{"features":{"tengu_kestrel_moor":{"defaultValue":true}}}`, "PIN-ONE"},
		{"null-uses-fallback", `{"features":{"tengu_kestrel_moor":{"value":null,"defaultValue":false}}}`, "PIN-ONE"},
		{"false-default", `{"features":{"tengu_kestrel_moor":{"defaultValue":false}}}`, "PIN-TWO"},
		{"fresh-map-removes-absent-gate", `{"features":{"different-feature":{"value":0}}}`, "PIN-ONE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := manager.ObserveSDKFeatures(auth, []byte(tc.payload)); err != nil {
				t.Fatal(err)
			}
			got, err := manager.Assignment(t.Context(), auth, "main", "claude-opus-5", "", claudeprofile.RoleMain)
			if err != nil || got != tc.want {
				t.Fatal("effective header did not follow the native evaluated gate", err)
			}
			latch := manager.Latch("main")
			if latch == nil || *latch != "PIN-ONE" {
				t.Fatal("feature evaluation overwrote the stored conversation latch")
			}
			*latch = "foreign-mutation"
			if *manager.Latch("main") != "PIN-ONE" {
				t.Fatal("latch getter returned mutable owned storage")
			}
		})
	}
	if err := manager.ObserveSDKFeatures(auth, []byte(`{"features":{"tengu_kestrel_moor":{"value":false}}}`)); err != nil {
		t.Fatal(err)
	}
	restarted := newClaudeDesktopATISManager(manager.statePath, manager.codeVersion, nil)
	got, err := restarted.Assignment(t.Context(), auth, "main", "claude-opus-5", "", claudeprofile.RoleMain)
	if err != nil || got != "PIN-TWO" || *restarted.Latch("main") != "PIN-ONE" {
		t.Fatal("protected feature cache and latch did not independently survive restart", err)
	}
}

func TestClaudeDesktopATISNativeHelperAndForkIsolation(t *testing.T) {
	manager, auth := nativeATISTestManager(t, func(model string) any { return "PIN-" + model })
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleLightHelper, claudeprofile.RoleSecurityMonitor, claudeprofile.RoleCountTokens, claudeprofile.RoleCompaction, claudeprofile.RoleSubagent} {
		if _, err := manager.Assignment(t.Context(), auth, "main", "claude-haiku-4-5-20251001", "", role); err != nil {
			t.Fatal(err)
		}
		if manager.Latch("main") != nil {
			t.Fatalf("%s initialized the main conversation latch", role)
		}
	}
	if _, err := manager.Assignment(t.Context(), auth, "main", "claude-sonnet-5", "", claudeprofile.RoleMain); err != nil {
		t.Fatal(err)
	}
	if manager.Latch("main", "fork") != nil {
		t.Fatal("undefined fork implicitly inherited main")
	}
	got, err := manager.Assignment(t.Context(), auth, "main", "claude-opus-5", "", claudeprofile.RoleTitle, "fork")
	if err != nil || got != "PIN-claude-opus-5" || manager.Latch("main", "fork") != nil {
		t.Fatal("uninitialized fork did not use bootstrap without mutating main", err)
	}
	if _, err := manager.Assignment(t.Context(), auth, "main", "claude-opus-5", "", claudeprofile.RoleMain, "fork"); err != nil {
		t.Fatal(err)
	}
	if *manager.Latch("main", "fork") != "PIN-claude-opus-5" || *manager.Latch("main") != "PIN-claude-sonnet-5" || manager.Latch("other") != nil {
		t.Fatal("fork, main or foreign session latches crossed scope")
	}
}

func TestClaudeDesktopATISNativeGateTruthiness(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{{"null", true}, {"true", true}, {"false", false}, {"0", false}, {"1", true}, {`""`, false}, {`"false"`, true}, {"[]", true}, {"{}", true}} {
		if got := claudeDesktopATISFeatureEnabled(map[string]json.RawMessage{"tengu_kestrel_moor": json.RawMessage(tc.raw)}); got != tc.want {
			t.Fatalf("native gate truthiness differs for %s", tc.raw)
		}
	}
}

func TestClaudeDesktopATISNativePersistenceFailureDoesNotDiscardPositivePin(t *testing.T) {
	manager, auth := nativeATISTestManager(t, func(string) any { return "VALID-OPAQUE-PIN" })
	if err := os.WriteFile(filepath.Join(manager.statePath, "atis"), []byte("synthetic directory obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		pin, err := manager.Assignment(t.Context(), auth, "main", "claude-sonnet-5", "", claudeprofile.RoleMain)
		if err == nil || pin != "VALID-OPAQUE-PIN" || manager.Latch("main") == nil || *manager.Latch("main") != pin {
			t.Fatal("disk failure lost the observed bootstrap or disappeared on a cache hit")
		}
	}
}

func TestClaudeDesktopATISNativeConcurrentModelsCannotReplaceConversationLatch(t *testing.T) {
	manager, auth := nativeATISTestManager(t, func(model string) any { return "PIN-" + model })
	var wg sync.WaitGroup
	values := make(chan string, 16)
	for i := 0; i < 16; i++ {
		model := "claude-sonnet-5"
		if i%2 == 0 {
			model = "claude-opus-5"
		}
		wg.Go(func() {
			pin, err := manager.Assignment(t.Context(), auth, "one-conversation", model, "", claudeprofile.RoleMain)
			if err != nil {
				t.Error(err)
			}
			values <- pin
		})
	}
	wg.Wait()
	close(values)
	want := manager.Latch("one-conversation")
	if want == nil {
		t.Fatal("concurrent main calls did not initialize any latch")
	}
	for value := range values {
		if value != *want {
			t.Fatal("a competing model overwrote the already initialized native latch")
		}
	}
}

func TestClaudeDesktopATISNativeAuxiliaryGateUsesCurrentMainModel(t *testing.T) {
	manager, auth := nativeATISTestManager(t, func(model string) any { return "PIN-" + model })
	for _, model := range []string{"claude-sonnet-5", "claude-opus-5"} {
		pin, err := manager.Assignment(t.Context(), auth, "main", model, "", claudeprofile.RoleMain)
		if err != nil || pin != "PIN-claude-sonnet-5" {
			t.Fatal("main model change overwrote the initialized pin", err)
		}
	}
	if err := manager.ObserveSDKFeatures(auth, []byte(`{"features":{"tengu_kestrel_moor":{"value":false}}}`)); err != nil {
		t.Fatal(err)
	}
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleTitle, claudeprofile.RoleCountTokens, claudeprofile.RoleLightHelper} {
		pin, err := manager.Assignment(t.Context(), auth, "main", "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", role)
		if err != nil || pin != "PIN-claude-opus-5" || *manager.Latch("main") != "PIN-claude-sonnet-5" {
			t.Fatal("auxiliary wire model replaced current main bootstrap or its separate latch", err)
		}
	}
}

func TestClaudeDesktopATISNativeEveryProfileModelCanBootstrap(t *testing.T) {
	bundle, err := claudeprofile.Load("")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, variant := range bundle.Variants {
		model := variant.Key.LogicalModel
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		t.Run(model, func(t *testing.T) {
			calls := 0
			manager, auth := nativeATISTestManager(t, func(requested string) any {
				calls++
				if requested != model {
					t.Error("bootstrap silently replaced the selected profile model")
				}
				return "PIN"
			})
			pin, err := manager.Assignment(t.Context(), auth, "main", model, model, claudeprofile.RoleMain)
			if err != nil || pin != "PIN" || calls != 1 {
				t.Fatal("supported model was excluded from native bootstrap", err)
			}
		})
	}
	if len(seen) < 7 {
		t.Fatal("profile model regression matrix unexpectedly narrowed")
	}
}
