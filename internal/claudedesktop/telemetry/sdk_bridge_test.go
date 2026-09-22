package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func bridgeTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(b *claudeprofile.Bundle) {
		b.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
}

func bridgeTestOwner() claudecontrol.BridgeOwner {
	return claudecontrol.BridgeOwner{DesktopSessionID: uuid.NewString(), QueryID: uuid.NewString(), SDKSessionID: uuid.NewString()}
}

func TestSDKBridgeEventsUseOwnedModelDefaultsAndDurableDelivery(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-4-5-20251001"} {
		t.Run(model, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			m := bridgeTestManager(t, clock, doer)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			auth.Metadata["subscription_type"] = "pro"
			owner := bridgeTestOwner()
			observe, err := m.SDKBridgeObserver(auth, owner)
			if err != nil {
				t.Fatal(err)
			}
			event := claudecontrol.BridgeEvent{Kind: claudecontrol.BridgePlaceholderUsed, Owner: owner, Model: model, PromptID: uuid.NewString(), At: clock.Now()}
			if err := observe(event); err != nil {
				t.Fatal(err)
			}
			status := 204
			event.Kind = claudecontrol.BridgeTeardown
			event.Archive = &claudecontrol.BridgeArchiveOutcome{Status: "ok", Credential: "current", OK: true, HTTPStatus: &status}
			if err := observe(event); err != nil {
				t.Fatal(err)
			}
			if err := observe(event); err != nil {
				t.Fatal(err)
			}
			if err := m.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, request := range doer.Requests() {
				var batch struct {
					Events []sdkEventWrapper `json:"events"`
				}
				_ = json.Unmarshal(request.Body, &batch)
				for _, wrapper := range batch.Events {
					data := wrapper.EventData
					if !strings.HasPrefix(data.EventName, "tengu_bridge_") {
						continue
					}
					count++
					betas, _ := m.sdkProfile.InputBetaHeader(model)
					if data.Model != model || data.SessionID != owner.SDKSessionID || data.Betas != betas || data.Auth.AccountUUID != testAccountA || data.ClientTimestamp != "2026-09-07T03:00:00.000Z" {
						t.Fatal("lifecycle wrapper lost owner/defaults/clock", data)
					}
					raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
					if err != nil {
						t.Fatal(err)
					}
					var meta map[string]any
					if err := json.Unmarshal(raw, &meta); err != nil {
						t.Fatal(err)
					}
					if meta["cc_prompt_id"] != event.PromptID || meta["v2"] != true || meta["subscription_type"] != "pro" {
						t.Fatal(meta)
					}
					want := 3
					if data.EventName == "tengu_bridge_repl_teardown" {
						want = 9
						if meta["archive_http_status"] != float64(204) || meta["archive_ok"] != true || meta["archive_timeout"] != false || meta["archive_no_token"] != false || meta["archive_credential"] != "current" {
							t.Fatal(meta)
						}
					}
					if len(meta) != want {
						t.Fatal("extra/missing native fields", meta)
					}
				}
			}
			if count != 2 {
				t.Fatal("missing or duplicate delivery", count)
			}
		})
	}
}

func TestSDKBridgeOwnerMismatchAndMissingFactsRemainVisible(t *testing.T) {
	for _, bad := range []string{"query", "record", "sdk-session", "model", "time", "prompt", "kind"} {
		t.Run(bad, func(t *testing.T) {
			clock := &testClock{now: time.Now()}
			doer := &testDoer{}
			m := bridgeTestManager(t, clock, doer)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			owner := bridgeTestOwner()
			observe, err := m.SDKBridgeObserver(auth, owner)
			if err != nil {
				t.Fatal(err)
			}
			event := claudecontrol.BridgeEvent{Kind: claudecontrol.BridgePlaceholderUsed, Owner: owner, Model: "claude-opus-5", At: clock.Now()}
			switch bad {
			case "query":
				event.Owner.QueryID = uuid.NewString()
			case "record":
				event.Owner.DesktopSessionID = uuid.NewString()
			case "sdk-session":
				event.Owner.SDKSessionID = uuid.NewString()
			case "model":
				event.Model = "unknown-model"
			case "time":
				event.At = time.Time{}
			case "prompt":
				event.PromptID = "not-a-uuid"
			case "kind":
				event.Kind = "invented-event"
			}
			if err := observe(event); err == nil {
				t.Fatal("invalid facts acknowledged")
			}
			worker, err := m.workerForDelivery(auth, m.sdkDelivery)
			if err != nil {
				t.Fatal(err)
			}
			if issue := worker.factIssueSnapshot(); issue == nil || !strings.Contains(issue.Reason, "bridge lifecycle") {
				t.Fatal("failure invisible", issue)
			}
			event = claudecontrol.BridgeEvent{Kind: claudecontrol.BridgePlaceholderUsed, Owner: owner, Model: "claude-opus-5", At: clock.Now()}
			if err := observe(event); err != nil {
				t.Fatal(err)
			}
			if worker.factIssueSnapshot() == nil {
				t.Fatal("later success erased lost occurrence")
			}
		})
	}
}

func TestSDKBridgeQueueFailureAndQuarantineDoNotFakeAcceptance(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		t.Run(fmt.Sprint(quarantine), func(t *testing.T) {
			clock := &testClock{now: time.Now()}
			m := bridgeTestManager(t, clock, &testDoer{})
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			owner := bridgeTestOwner()
			observe, err := m.SDKBridgeObserver(auth, owner)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := m.workerForDelivery(auth, m.sdkDelivery)
			if err != nil {
				t.Fatal(err)
			}
			if quarantine {
				m.Quarantine()
			} else {
				worker.profile.batch.MaxPendingEvents = 0
			}
			event := claudecontrol.BridgeEvent{Kind: claudecontrol.BridgePlaceholderUsed, Owner: owner, Model: "claude-opus-5", At: clock.Now()}
			err = observe(event)
			if (err != nil) == quarantine {
				t.Fatal("incorrect closed/full queue behavior", err)
			}
			if !quarantine && worker.factIssueSnapshot() == nil {
				t.Fatal("failed persistence was hidden")
			}
		})
	}
}
