package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeAccountExecutorH73LifecycleAcceptance(t *testing.T) {
	stateRoot := t.TempDir()
	authA := newClaudeAccountRuntimeTestAuth(t,
		"aa730000-0000-4000-8000-000000000001",
		"ba730000-0000-4000-8000-000000000001",
		"ca730000-0000-4000-8000-000000000001",
	)
	authB := newClaudeAccountRuntimeTestAuth(t,
		"aa730000-0000-4000-8000-000000000002",
		"ba730000-0000-4000-8000-000000000002",
		"ca730000-0000-4000-8000-000000000002",
	)
	prepareH73RuntimeAuth(authA)
	prepareH73RuntimeAuth(authB)

	first := newH73ClaudeAccountExecutor(stateRoot)
	t.Cleanup(first.Close)
	if errProvision := first.Provision(authA); errProvision != nil {
		t.Fatal(errProvision)
	}
	if errProvision := first.Provision(authB); errProvision != nil {
		t.Fatal(errProvision)
	}
	firstA := accountRuntimeForAuth(t, first, authA.ID)
	firstB := accountRuntimeForAuth(t, first, authB.ID)
	firstStatus := waitForH73Startup(t, first, authA.ID)
	if firstStatus.State != claudedesktop.EnrollmentActive || firstStatus.LifecycleState != claudeDesktopAppRunning || !firstStatus.RuntimeLoaded {
		t.Fatalf("initial runtime status = %+v", firstStatus)
	}
	if firstStatus.Startup.EndpointCount == 0 || firstStatus.Startup.Attempted != firstStatus.Startup.EndpointCount || firstStatus.Startup.Completed != firstStatus.Startup.EndpointCount {
		t.Fatalf("initial startup coverage = %+v", firstStatus.Startup)
	}

	emitH73TelemetryCycle(t, firstA.executor.desktopTelemetry, authA)
	initialTelemetry := waitForH73TelemetryRoles(t, firstA.executor.desktopTelemetry)
	if initialTelemetry.Enabled != true || len(initialTelemetry.DeliveryEndpoints) != 7 {
		t.Fatalf("initial telemetry status = %+v", initialTelemetry)
	}
	initialObligations := h73TelemetryObligations(initialTelemetry)
	if initialObligations == 0 {
		t.Fatal("initial telemetry queue is empty")
	}

	if _, errDisabled := claudedesktop.TransitionMetadataEnrollment(authA.Metadata, authA.ID, claudedesktop.EnrollmentDisabled, "operator disabled", time.Now()); errDisabled != nil {
		t.Fatal(errDisabled)
	}
	authA.Disabled = true
	authA.Status = cliproxyauth.StatusDisabled
	first.SyncAuth(authA)
	disabled := first.AccountStatus(authA.ID)
	if disabled.RuntimeLoaded || disabled.LifecycleState != claudeDesktopAppStopped || disabled.Startup.State != "stopped" {
		t.Fatalf("disabled runtime status = %+v", disabled)
	}
	if h73TelemetryObligations(firstA.executor.desktopTelemetry.Status()) <= initialObligations {
		t.Fatal("normal disable did not persist the evidence-backed shutdown sequence")
	}
	if currentB := accountRuntimeForAuth(t, first, authB.ID); currentB != firstB {
		t.Fatal("disabling one account disrupted the other account runtime")
	}

	if _, errReady := claudedesktop.TransitionMetadataEnrollment(authA.Metadata, authA.ID, claudedesktop.EnrollmentReady, "", time.Now()); errReady != nil {
		t.Fatal(errReady)
	}
	if _, errActive := claudedesktop.TransitionMetadataEnrollment(authA.Metadata, authA.ID, claudedesktop.EnrollmentActive, "", time.Now()); errActive != nil {
		t.Fatal(errActive)
	}
	authA.Disabled = false
	authA.Status = cliproxyauth.StatusActive
	first.SyncAuth(authA)
	secondA := accountRuntimeForAuth(t, first, authA.ID)
	cleanRestart := waitForH73Startup(t, first, authA.ID)
	if secondA == firstA || cleanRestart.LifecycleGen != firstStatus.LifecycleGen+1 || cleanRestart.AppSessionHash == firstStatus.AppSessionHash {
		t.Fatalf("clean restart identity = %+v, first = %+v", cleanRestart, firstStatus)
	}
	if cleanRestart.PreviousExit != claudeDesktopExitClean || cleanRestart.RecoveredUnclean {
		t.Fatalf("clean restart classification = %+v", cleanRestart)
	}
	emitH73TelemetryCycle(t, secondA.executor.desktopTelemetry, authA)
	beforeCrash := h73TelemetryObligations(waitForH73TelemetryRoles(t, secondA.executor.desktopTelemetry))

	// Model abrupt child-process loss: network resources stop, but the wrapper
	// never runs the normal app lifecycle finalizer. The persisted running state
	// must therefore be recovered as an unclean exit by the next runtime.
	secondA.executor.Quarantine()
	afterCrash := h73TelemetryObligations(secondA.executor.desktopTelemetry.Status())
	if afterCrash != beforeCrash {
		t.Fatalf("abnormal exit changed queue obligations: before=%d after=%d", beforeCrash, afterCrash)
	}
	first.mu.Lock()
	delete(first.runtimes, authA.ID)
	first.mu.Unlock()
	secondA.mu.Lock()
	secondA.retiring = true
	secondA.quarantined = true
	secondA.closed = true
	secondA.mu.Unlock()

	recoveredExecutor := newH73ClaudeAccountExecutor(stateRoot)
	t.Cleanup(recoveredExecutor.Close)
	if errProvision := recoveredExecutor.Provision(authA); errProvision != nil {
		t.Fatal(errProvision)
	}
	recoveredA := accountRuntimeForAuth(t, recoveredExecutor, authA.ID)
	recovered := waitForH73Startup(t, recoveredExecutor, authA.ID)
	if recovered.LifecycleGen != cleanRestart.LifecycleGen+1 || recovered.AppSessionHash == cleanRestart.AppSessionHash {
		t.Fatalf("unclean recovery identity = %+v, previous = %+v", recovered, cleanRestart)
	}
	if recovered.PreviousExit != claudeDesktopExitUnclean || !recovered.RecoveredUnclean {
		t.Fatalf("unclean recovery classification = %+v", recovered)
	}
	if restored := h73TelemetryObligations(waitForH73TelemetryRoles(t, recoveredA.executor.desktopTelemetry)); restored < beforeCrash {
		t.Fatalf("unclean recovery lost durable telemetry: before=%d restored=%d", beforeCrash, restored)
	}

	oldRevision := recovered.ApprovedRevision
	oldTelemetry := recoveredA.executor.desktopTelemetry
	beforeQuarantine := h73TelemetryObligations(oldTelemetry.Status())
	authA.ProxyURL = "http://127.0.0.1:18730"
	if errSchedule := recoveredExecutor.CanScheduleAuth(authA); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("runtime drift error = %v, want quarantine", errSchedule)
	}
	quarantined := recoveredExecutor.AccountStatus(authA.ID)
	if quarantined.State != claudedesktop.EnrollmentQuarantined || !quarantined.CanPromote || quarantined.LifecycleState != claudeDesktopAppQuarantined {
		t.Fatalf("quarantined runtime status = %+v", quarantined)
	}
	afterQuarantine := h73TelemetryObligations(oldTelemetry.Status())
	if afterQuarantine != beforeQuarantine {
		t.Fatalf("quarantine synthesized close telemetry: before=%d after=%d", beforeQuarantine, afterQuarantine)
	}
	if currentB := accountRuntimeForAuth(t, first, authB.ID); currentB != firstB {
		t.Fatal("quarantining one account disrupted the other account runtime")
	}

	if errPromote := recoveredExecutor.PromoteAuth(authA); errPromote != nil {
		t.Fatal(errPromote)
	}
	promoted := waitForH73Startup(t, recoveredExecutor, authA.ID)
	newRevision := promoted.ApprovedRevision
	if promoted.State != claudedesktop.EnrollmentActive || newRevision == oldRevision || promoted.PreviousRevision != oldRevision || promoted.CanPromote {
		t.Fatalf("promoted runtime status = %+v", promoted)
	}

	authA.ProxyURL = ""
	if errSchedule := recoveredExecutor.CanScheduleAuth(authA); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("rollback drift error = %v, want quarantine", errSchedule)
	}
	rollbackReady := recoveredExecutor.AccountStatus(authA.ID)
	if !rollbackReady.CanRollback || rollbackReady.ObservedRevision != oldRevision {
		t.Fatalf("rollback-ready runtime status = %+v", rollbackReady)
	}
	if errRollback := recoveredExecutor.RollbackAuth(authA); errRollback != nil {
		t.Fatal(errRollback)
	}
	rolledBack := waitForH73Startup(t, recoveredExecutor, authA.ID)
	if rolledBack.State != claudedesktop.EnrollmentActive || rolledBack.ApprovedRevision != oldRevision || rolledBack.PreviousRevision != newRevision || rolledBack.CanRollback {
		t.Fatalf("rolled-back runtime status = %+v", rolledBack)
	}
	rolledBackA := accountRuntimeForAuth(t, recoveredExecutor, authA.ID)
	if restored := h73TelemetryObligations(waitForH73TelemetryRoles(t, rolledBackA.executor.desktopTelemetry)); restored < beforeCrash {
		t.Fatalf("rollback lost old-revision durable telemetry: before=%d restored=%d", beforeCrash, restored)
	}
}

func prepareH73RuntimeAuth(auth *cliproxyauth.Auth) {
	auth.Metadata[claudedesktop.MetadataSessionKeyKey] = "session-key-for-h73-acceptance"
	auth.Metadata["subscription_created_at"] = int64(1_700_000_000)
	auth.Metadata["subscription_type"] = "pro"
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = claudedesktop.TelemetryMaterials{
		SegmentWriteKey:         "segment0123456789abcdef01234567",
		DatadogLogsAPIKey:       "datadog-logs-0123456789abcdef012345",
		DatadogRUMClientToken:   "datadog-rum-0123456789abcdef0123456",
		DatadogRUMApplicationID: "73737373-7373-4737-8373-737373737373",
		SentryPublicKey:         "abcdef0123456789abcdef0123456789",
	}
}

func newH73ClaudeAccountExecutor(stateRoot string) *ClaudeAccountExecutor {
	startupDoer := claudeAccountStartupDoerFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/eval/") {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":{"tengu_kestrel_moor":{"defaultValue":true}}}`))}, nil
		}
		if request.Method == http.MethodGet && request.URL.Path == "/api/desktop/win32/x64/msix/update" {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"currentRelease":"1.40609.0","releases":[]}`))}, nil
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Proto:      "HTTP/2.0",
			ProtoMajor: 2,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	telemetryDoer := claudetelemetry.HTTPDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Proto:      "HTTP/2.0",
			ProtoMajor: 2,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
		}, nil
	})
	return NewClaudeAccountExecutorWithOptions(
		&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}},
		ClaudeAccountExecutorOptions{
			StartupDoerFactory: func(context.Context, string, *cliproxyauth.Auth) (claudestartup.HTTPDoer, error) {
				return startupDoer, nil
			},
			TelemetryEndpointDoerFactory: func(string, string, *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
				return telemetryDoer
			},
		},
	)
}

func waitForH73Startup(t *testing.T, executor *ClaudeAccountExecutor, authID string) ClaudeAccountRuntimeStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status := executor.AccountStatus(authID)
		if status.Startup.State == "ready" {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup did not become ready: %+v", status.Startup)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func emitH73TelemetryCycle(t *testing.T, manager *claudetelemetry.Manager, auth *cliproxyauth.Auth) {
	t.Helper()
	span := manager.BeginRequest(context.Background(), auth, claudetelemetry.RequestFacts{
		Role:            claudeprofile.RoleMain,
		SessionID:       "d7300000-0000-4000-8000-000000000001",
		PromptID:        "e7300000-0000-4000-8000-000000000001",
		ClientRequestID: "f7300000-0000-4000-8000-000000000001",
		Model:           "claude-sonnet-5",
		StartedAt:       time.Now(),
	})
	if !span.Active() {
		t.Fatal("telemetry request span is inactive")
	}
	span.ObserveRequest([]byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"acceptance"}]}`), make(http.Header))
	span.ObserveFirstByte(time.Now().Add(time.Millisecond))
	span.RecordScheduledRetry(context.Background(), 2, 100*time.Millisecond, errors.New("upstream unavailable"))
	span.FinishFailure(context.Background(), "server_error", errors.New("upstream unavailable"))
}

func waitForH73TelemetryRoles(t *testing.T, manager *claudetelemetry.Manager) claudetelemetry.Status {
	t.Helper()
	want := []string{
		"datadog-logs",
		"datadog-logs-browser",
		"datadog-rum",
		"desktop-event-logging",
		"sdk-event-logging",
		"segment",
		"sentry",
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status := manager.Status()
		roles := make([]string, 0, len(status.Accounts))
		for _, account := range status.Accounts {
			if account.Pending+account.Sending+account.DeadLetters > 0 {
				roles = append(roles, account.EndpointRole)
			}
		}
		sort.Strings(roles)
		if strings.Join(roles, ",") == strings.Join(want, ",") {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("telemetry roles = %v, want %v; status=%+v", roles, want, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func h73TelemetryObligations(status claudetelemetry.Status) int {
	total := 0
	for _, account := range status.Accounts {
		total += account.Pending + account.Sending + account.DeadLetters
	}
	return total
}
