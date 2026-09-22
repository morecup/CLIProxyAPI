package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

// resumeBridgeGoldenKeys reads the native key order of one event from the
// pinned golden (spreads that contribute no keys are skipped).
func resumeBridgeGoldenKeys(t *testing.T, event, list string) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/sdk-telemetry-resume-bridge-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Events map[string]map[string]json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	var keys []struct {
		Key    string `json:"key"`
		Spread string `json:"spread"`
	}
	if err := json.Unmarshal(golden.Events[event][list], &keys); err != nil {
		t.Fatalf("%s.%s: %v", event, list, err)
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		if key.Key != "" {
			names = append(names, key.Key)
		}
	}
	return names
}

func jsonKeyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestResumeBridgeMetadataMatchesNativeKeyOrder(t *testing.T) {
	raw, err := json.Marshal(bridgeReplStartedMetadata("pro", "prompt-1", BridgeReplStarted{HasInitialMessages: true, ExpiresInS: 3600}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"subscription_type":"pro","cc_prompt_id":"prompt-1","has_initial_messages":true,"v2":true,"expires_in_s":3600,"inProtectedNamespace":false}`; string(raw) != want {
		t.Fatalf("bridge started metadata = %s", raw)
	}
	if got, want := strings.Join(jsonKeyOrder(t, raw)[2:], ","), strings.Join(resumeBridgeGoldenKeys(t, "tengu_bridge_repl_started", "keys"), ","); got != want {
		t.Fatalf("bridge started key order %s != golden %s", got, want)
	}
	raw, err = json.Marshal(sessionResumedMetadata("pro", "", SessionResumed{Duration: 234*time.Millisecond + 600*time.Microsecond}))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"subscription_type":"pro","entrypoint":"print","success":true,"interruption_kind":"none","resume_duration_ms":235}`; string(raw) != want {
		t.Fatalf("session resumed metadata = %s", raw)
	}
	if got, want := strings.Join(jsonKeyOrder(t, raw)[1:], ","), strings.Join(resumeBridgeGoldenKeys(t, "tengu_session_resumed", "success_keys"), ","); got != want {
		t.Fatalf("session resumed key order %s != golden %s", got, want)
	}
}

func resumeBridgeTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(b *claudeprofile.Bundle) {
		b.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		b.SDKTelemetry.Events[FactSDKBridgeReplStarted] = claudeprofile.TelemetryEventProfile{EventName: "tengu_bridge_repl_started", RequiredFacts: []string{"session_id", "model"}}
		b.SDKTelemetry.Events[FactSDKSessionResumed] = claudeprofile.TelemetryEventProfile{EventName: "tengu_session_resumed", RequiredFacts: []string{"session_id", "model"}}
	})
}

func TestResumeBridgeEventsAreDelivered(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := resumeBridgeTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	owner := bridgeTestOwner()
	promptID, model := uuid.NewString(), "claude-opus-5"
	observe, err := m.SDKResumeBridgeObserver(auth, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKSessionResumed(t.Context(), auth, owner.SDKSessionID, model, "", SessionResumed{Duration: 1200 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := observe(claudecontrol.BridgeEvent{Kind: claudecontrol.BridgeStarted, Owner: owner, Model: model, PromptID: promptID, At: clock.now, ExpiresIn: 900, HasInitialMessages: true}); err != nil {
		t.Fatal(err)
	}
	if err := observe(claudecontrol.BridgeEvent{Kind: claudecontrol.BridgeStarted, Owner: owner, Model: model, At: clock.now}); err == nil {
		t.Fatal("missing expires_in must not be invented")
	}
	if err := observe(claudecontrol.BridgeEvent{Kind: claudecontrol.BridgeStarted, Owner: bridgeTestOwner(), Model: model, At: clock.now, ExpiresIn: 900}); err == nil {
		t.Fatal("foreign owner must be rejected")
	}
	// Other kinds keep flowing through the lifecycle observer.
	if err := observe(claudecontrol.BridgeEvent{Kind: claudecontrol.BridgePlaceholderUsed, Owner: owner, Model: model, PromptID: promptID, At: clock.now}); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	betas, _ := m.sdkProfile.InputBetaHeader(model)
	var names []string
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		_ = json.Unmarshal(request.Body, &batch)
		for _, wrapper := range batch.Events {
			data := wrapper.EventData
			switch data.EventName {
			case "tengu_session_resumed", "tengu_bridge_repl_started", "tengu_bridge_placeholder_used_session":
			default:
				continue
			}
			names = append(names, data.EventName)
			if data.SessionID != owner.SDKSessionID || data.Model != model || data.Betas != betas || data.Auth.AccountUUID != testAccountA {
				t.Fatalf("borrowed dimensions on %s: %+v", data.EventName, data)
			}
			raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			switch data.EventName {
			case "tengu_session_resumed":
				if string(raw) != `{"subscription_type":"pro","entrypoint":"print","success":true,"interruption_kind":"none","resume_duration_ms":1200}` {
					t.Fatalf("session resumed payload = %s", raw)
				}
			case "tengu_bridge_repl_started":
				if string(raw) != `{"subscription_type":"pro","cc_prompt_id":"`+promptID+`","has_initial_messages":true,"v2":true,"expires_in_s":900,"inProtectedNamespace":false}` {
					t.Fatalf("bridge started payload = %s", raw)
				}
			}
		}
	}
	if strings.Join(names, ",") != "tengu_session_resumed,tengu_bridge_repl_started,tengu_bridge_placeholder_used_session" {
		t.Fatalf("lost, duplicate or reordered events: %v", names)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" {
			executable[pair.EventName] = pair.Executable
		}
	}
	for _, name := range []string{"tengu_bridge_repl_started", "tengu_session_resumed"} {
		if !executable[name] {
			t.Fatalf("%s is captured but not counted as executable: %+v", name, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount)
}
