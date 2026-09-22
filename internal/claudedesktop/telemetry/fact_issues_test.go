package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func factIssueTestManager(t *testing.T, root string, clock *testClock) *Manager {
	t.Helper()
	return newTelemetryTestManager(t, root, clock, &testDoer{}, func(bundle *claudeprofile.Bundle) {
		// Response-ledger tests intentionally omit the earlier submission stage.
		delete(bundle.SDKTelemetry.Events, FactSDKInput)
		bundle.SDKTelemetry.InputBetas = nil
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
}

func factIssueEndpoint(t *testing.T, manager *Manager, role string) DeliveryEndpointStatus {
	t.Helper()
	for _, endpoint := range manager.Status().DeliveryEndpoints {
		if endpoint.Role == role {
			return endpoint
		}
	}
	t.Fatal("endpoint not found")
	return DeliveryEndpointStatus{}
}

func TestSDKMissingPromptFactsSurviveNewRequestAndFlush(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
	manager := factIssueTestManager(t, t.TempDir(), clock)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`)
	facts := testRequestFacts(uuid.NewString())
	facts.Role, facts.StartedAt = claudeprofile.RoleMain, clock.Now()
	var tracker claudeprompt.Tracker
	facts.Prompt = tracker.Begin(claudeprompt.Input{AccountID: auth.ID, SessionID: facts.SessionID, ClientRequestID: facts.ClientRequestID, Role: "main", StartedAt: facts.StartedAt, Body: body})
	facts.PromptID = facts.Prompt.Snapshot().PromptID
	span := manager.BeginRequest(t.Context(), auth, facts)
	span.ObserveRequest(body, http.Header{})
	span.ObserveResponse("req_synthetic", "end_turn")
	clock.Advance(time.Second)
	// A valid terminal API response does not supply the separate assistant fact.
	facts.Prompt.FinishSuccess(clock.Now(), "end_turn", nil)
	span.FinishSuccess(t.Context())
	if issue := span.sdkWorker.factIssueSnapshot(); issue == nil || issue.Unresolved != 1 {
		t.Fatalf("missing SDK facts were not retained: %+v", issue)
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	manager.BeginRequest(t.Context(), auth, testRequestFacts(uuid.NewString()))
	other := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	manager.BeginRequest(t.Context(), other, testRequestFacts(uuid.NewString()))
	if endpoint := factIssueEndpoint(t, manager, "sdk-event-logging"); endpoint.Status != "awaiting-sdk-prompt-facts" {
		t.Fatalf("transport or request success hid missing facts: %+v", endpoint)
	}
	encoded, _ := json.Marshal(manager.Status())
	for _, secret := range []string{"PRIVATE_PROMPT", facts.SessionID, facts.PromptID, facts.ClientRequestID} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("fact status leaked an identity or prompt")
		}
	}
}

func TestFactIssueExactScopeAndEmptyQueueRestart(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
	manager := factIssueTestManager(t, root, clock)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	worker, errWorker := manager.workerForDelivery(auth, manager.sdkDelivery)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	const session, owner = "PRIVATE_SESSION", "PRIVATE_OWNER"
	if errSet := worker.setFactIssue(factIssueTitle, session, owner, true); errSet != nil {
		t.Fatal(errSet)
	}
	if errClear := worker.setFactIssue(factIssueTitle, "other-session", owner, false); errClear != nil {
		t.Fatal(errClear)
	}
	if errClear := worker.setFactIssue(factIssueSDKPrompt, session, owner, false); errClear != nil {
		t.Fatal(errClear)
	}
	other := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	otherWorker, errOther := manager.workerForDelivery(other, manager.sdkDelivery)
	if errOther != nil {
		t.Fatal(errOther)
	}
	if errClear := otherWorker.setFactIssue(factIssueTitle, session, owner, false); errClear != nil {
		t.Fatal(errClear)
	}
	files, errFiles := worker.scanQueue()
	if errFiles != nil || len(files) != 0 {
		t.Fatal("test requires a fully empty delivery queue")
	}
	encoded, errRead := os.ReadFile(filepath.Join(worker.directory, factIssuesFile))
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, secret := range []string{session, owner, testAccountA, auth.ID} {
		if len(secret) > 0 && bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("diagnostic ledger is not protected")
		}
	}
	manager.Quarantine()
	restored := factIssueTestManager(t, root, clock)
	if status := factIssueEndpoint(t, restored, "sdk-event-logging"); status.Status != "awaiting-response-facts" {
		t.Fatalf("queue-free restart lost the issue: %+v", status)
	}
	if len(restored.Status().Accounts) != 1 || restored.Status().Accounts[0].Health != "degraded" {
		t.Fatal("issue-only account omitted from health")
	}
	restoredWorker, errRestored := restored.workerForDelivery(auth, restored.sdkDelivery)
	if errRestored != nil {
		t.Fatal(errRestored)
	}
	if errClear := restoredWorker.setFactIssue(factIssueTitle, session, owner, false); errClear != nil {
		t.Fatal(errClear)
	}
	if restoredWorker.factIssueSnapshot() != nil {
		t.Fatal("exact observed resolution did not clear its issue")
	}
	restored.Quarantine()
	third := factIssueTestManager(t, root, clock)
	if endpoint := factIssueEndpoint(t, third, "sdk-event-logging"); endpoint.Status != "ready" {
		t.Fatal("resolved ledger revived an issue")
	}
}

func TestOwnedContextFailureSurvivesDeliveryAndRestart(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
	manager := factIssueTestManager(t, root, clock)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts(uuid.NewString())
	facts.Role = claudeprofile.RoleMain
	span := manager.BeginRequest(t.Context(), auth, facts)
	span.ObserveOwnedContextState(false)
	if err := manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status := factIssueEndpoint(t, manager, "sdk-event-logging"); status.Status != "awaiting-sdk-prompt-facts" || !strings.Contains(status.Reason, "owned conversation context") {
		t.Fatal("context persistence failure is not visible in endpoint health")
	}
	manager.Quarantine()
	manager = factIssueTestManager(t, root, clock)
	otherFacts := testRequestFacts(uuid.NewString())
	otherFacts.Role = claudeprofile.RoleMain
	manager.BeginRequest(t.Context(), auth, otherFacts).ObserveOwnedContextState(true)
	otherAuth := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	manager.BeginRequest(t.Context(), otherAuth, facts).ObserveOwnedContextState(true)
	if status := factIssueEndpoint(t, manager, "sdk-event-logging"); status.Status != "awaiting-sdk-prompt-facts" {
		t.Fatal("restart or another session/account cleared lost context")
	}
	encoded, _ := json.Marshal(manager.Status())
	if bytes.Contains(encoded, []byte(facts.SessionID)) || bytes.Contains(encoded, []byte(auth.ID)) {
		t.Fatal("health exposed session/account identity")
	}
	manager.BeginRequest(t.Context(), auth, facts).ObserveOwnedContextState(true)
	if status := factIssueEndpoint(t, manager, "sdk-event-logging"); strings.Contains(status.Reason, "owned conversation context") {
		t.Fatal("successful scoped save did not resolve context failure")
	}
}

func TestSDKSessionStateFailureSurvivesUnrelatedSuccessAndRestart(t *testing.T) {
	for _, role := range []claudeprofile.RequestRole{claudeprofile.RoleMain, claudeprofile.RoleTitle, claudeprofile.RoleCompaction} {
		t.Run(string(role), func(t *testing.T) {
			root := t.TempDir()
			clock := &testClock{now: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}
			manager := factIssueTestManager(t, root, clock)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
			facts := testRequestFacts(uuid.NewString())
			facts.Role, facts.ParentPromptID = role, uuid.NewString()
			span := manager.BeginRequest(t.Context(), auth, facts)
			span.ObserveSDKSessionStateUnavailable()
			span.ObserveSDKSessionStateUnavailable()
			span.ObserveOwnedContextState(false)
			span.ObserveOwnedContextState(true)
			check := func(manager *Manager) {
				t.Helper()
				for _, endpointRole := range []string{"sdk-event-logging", datadogLogsRole} {
					endpoint := factIssueEndpoint(t, manager, endpointRole)
					if (endpoint.Status != "awaiting-sdk-prompt-facts" && endpoint.Status != "awaiting-enrollment-material") || !strings.Contains(endpoint.Reason, "SDK session ownership state") || strings.Contains(endpoint.Reason, "owned conversation context") {
						t.Fatalf("independent session loss hidden for %s: %+v", endpointRole, endpoint)
					}
					found := false
					for _, account := range manager.Status().Accounts {
						if account.EndpointRole == endpointRole && account.FactIssues != nil && account.FactIssues.sessionStateUnresolved > 0 {
							found = true
							if account.Health != "degraded" || account.FactIssues.Unresolved != 1 || !strings.Contains(account.FactIssues.Reason, "SDK session ownership state") {
								t.Fatal("account loss was duplicated or hidden", account.FactIssues)
							}
						}
					}
					if !found {
						t.Fatal("account-level session issue omitted", endpointRole)
					}
				}
				encoded, _ := json.Marshal(manager.Status())
				for _, secret := range []string{facts.SessionID, facts.PromptID, facts.ParentPromptID, facts.ClientRequestID, auth.ID, root} {
					if secret != "" && bytes.Contains(encoded, []byte(secret)) {
						t.Fatal("session-state diagnostic exposed a private identifier or path")
					}
				}
			}
			check(manager)
			if err := manager.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			manager.Quarantine()
			manager = factIssueTestManager(t, root, clock)
			check(manager)
			// Even the same session's successful adopted-prefix write cannot
			// repair a different missing store. Neither can another owner.
			nextFacts := facts
			nextFacts.Role, nextFacts.PromptID, nextFacts.ClientRequestID = claudeprofile.RoleMain, uuid.NewString(), uuid.NewString()
			manager.BeginRequest(t.Context(), auth, nextFacts).ObserveOwnedContextState(true)
			nextFacts.SessionID = uuid.NewString()
			manager.BeginRequest(t.Context(), auth, nextFacts).ObserveOwnedContextState(true)
			other := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
			other.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = auxiliaryTestMaterials()
			otherSpan := manager.BeginRequest(t.Context(), other, nextFacts)
			otherSpan.ObserveOwnedContextState(true)
			if otherSpan.sdkWorker.factIssueSnapshot() != nil || otherSpan.auxiliaryWorkers[datadogLogsRole].factIssueSnapshot() != nil {
				t.Fatal("another account inherited session loss")
			}
			check(manager)
			manager.Quarantine()
			check(factIssueTestManager(t, root, clock))
		})
	}
}

func TestFactIssueCorruptionRemainsVisibleAndPreserved(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
	manager := factIssueTestManager(t, root, clock)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	worker, errWorker := manager.workerForDelivery(auth, manager.sdkDelivery)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	if errSet := worker.setFactIssue(factIssueTitle, "session", "owner", true); errSet != nil {
		t.Fatal(errSet)
	}
	manager.Quarantine()
	file := filepath.Join(worker.directory, factIssuesFile)
	corrupt := []byte("PRIVATE_CORRUPT_RECORD")
	if errWrite := os.WriteFile(file, corrupt, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	restored := factIssueTestManager(t, root, clock)
	if endpoint := factIssueEndpoint(t, restored, "sdk-event-logging"); endpoint.Status != "fact-issue-storage-error" {
		t.Fatal("corrupt empty-queue ledger silently disappeared")
	}
	span := restored.BeginRequest(t.Context(), auth, testRequestFacts(uuid.NewString()))
	if !span.Active() {
		t.Fatal("diagnostic failure blocked the model request")
	}
	if errSet := span.sdkWorker.setFactIssue(factIssueTitle, "new-session", "new-owner", true); errSet == nil {
		t.Fatal("corrupt store was overwritten")
	}
	after, errRead := os.ReadFile(file)
	if errRead != nil || !bytes.Equal(after, corrupt) {
		t.Fatal("original corrupt evidence changed")
	}
	encoded, _ := json.Marshal(restored.Status())
	if bytes.Contains(encoded, corrupt) || factIssueEndpoint(t, restored, "sdk-event-logging").Status != "fact-issue-storage-error" {
		t.Fatal("state reset hid corruption or leaked its contents")
	}
}

func TestFactIssueOverflowAndWriteFailureRemainDegraded(t *testing.T) {
	for _, scenario := range []string{"overflow", "write-failure"} {
		t.Run(scenario, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			manager := factIssueTestManager(t, t.TempDir(), clock)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			worker, errWorker := manager.workerForDelivery(auth, manager.sdkDelivery)
			if errWorker != nil {
				t.Fatal(errWorker)
			}
			if scenario == "overflow" {
				worker.factIssues = make(map[string]string)
				for i := 0; i < maxPromptIssues; i++ {
					worker.factIssues[fmt.Sprintf("%064x", i)] = factIssueTitle
				}
				if errSet := worker.setFactIssue(factIssueTitle, "session", "overflow-owner", true); errSet != nil {
					t.Fatal(errSet)
				}
				if len(worker.factIssues) != maxPromptIssues || !worker.factIssueSnapshot().Overflow {
					t.Fatal("overflow was unbounded or discarded")
				}
				read, errRead := readFactIssueRecord(worker.directory)
				if errRead != nil || !read.Overflow {
					t.Fatal("overflow latch was not persisted")
				}
			} else {
				if errMkdir := os.Mkdir(filepath.Join(worker.directory, factIssuesFile), 0o700); errMkdir != nil {
					t.Fatal(errMkdir)
				}
				if errSet := worker.setFactIssue(factIssueTitle, "session", "owner", true); errSet == nil {
					t.Fatal("write failure ignored")
				}
				if !worker.factIssueSnapshot().StorageError {
					t.Fatal("write failure lost from health")
				}
			}
			if worker.statusSnapshot().Health != "degraded" {
				t.Fatal("missing diagnostic persistence looked healthy")
			}
		})
	}
}

func TestResponseFactOwnerSeparatesRetriesAndReusedCallerIDs(t *testing.T) {
	facts := testRequestFacts("session")
	facts.StartedAt = time.Unix(100, 0)
	facts.Attempt = 1
	owner := requestFactOwner(facts)
	retry := facts
	retry.Attempt = 2
	if requestFactOwner(retry) == owner {
		t.Fatal("retry inherited response issue identity")
	}
	reused := facts
	reused.StartedAt = facts.StartedAt.Add(time.Second)
	if requestFactOwner(reused) == owner {
		t.Fatal("later reused caller request ID inherited response issue identity")
	}
}

func TestResponseFactVariantsPersistAcrossRestart(t *testing.T) {
	for _, kind := range []string{factIssueCacheDiagnosis, factIssueFastOverage} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			manager := factIssueTestManager(t, root, clock)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			facts := testRequestFacts(uuid.NewString())
			facts.Role = claudeprofile.RoleMain
			span := manager.BeginRequest(t.Context(), auth, facts)
			span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[],"speed":"fast"}`), nil)
			if kind == factIssueCacheDiagnosis {
				span.ObserveHTTPResponse(200, http.Header{"Request-Id": {"req_synthetic"}})
				span.ObserveResponsePayload([]byte(`{"type":"message","diagnostics":{"cache_miss_reason":{"type":"PRIVATE_REASON"}}}`), false)
			} else {
				span.ObserveHTTPResponse(429, http.Header{"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"PRIVATE_REASON"}})
			}
			if issue := span.sdkWorker.factIssueSnapshot(); issue == nil || issue.Unresolved != 1 {
				t.Fatal("unsupported response variant was not retained")
			}
			manager.Quarantine()
			restored := factIssueTestManager(t, root, clock)
			restored.BeginRequest(t.Context(), auth, testRequestFacts(uuid.NewString()))
			if endpoint := factIssueEndpoint(t, restored, "sdk-event-logging"); endpoint.Status != "awaiting-response-facts" {
				t.Fatal("new request or restart hid response variant gap")
			}
			encoded, _ := json.Marshal(restored.Status())
			if strings.Contains(string(encoded), "PRIVATE_REASON") {
				t.Fatal("unsupported source value leaked into status")
			}
		})
	}
}
