package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type claudeAccountStartupDoerFunc func(*http.Request) (*http.Response, error)

func (f claudeAccountStartupDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newClaudeAccountTestExecutor(cfg *config.Config) *ClaudeAccountExecutor {
	startupFactory := func(_ context.Context, role string, _ *cliproxyauth.Auth) (claudestartup.HTTPDoer, error) {
		return claudeAccountStartupDoerFunc(func(*http.Request) (*http.Response, error) {
			if role == "startup-sdk-eval" {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"features":{"tengu_kestrel_moor":{"defaultValue":true}}}`))}, nil
			}
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}), nil
	}
	telemetryFactory := func(_ string, _ string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		return claudetelemetry.HTTPDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Proto:      "HTTP/2.0",
				ProtoMajor: 2,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})
	}
	return NewClaudeAccountExecutorWithOptions(cfg, ClaudeAccountExecutorOptions{
		StartupDoerFactory: startupFactory, TelemetryEndpointDoerFactory: telemetryFactory,
	})
}

func TestClaudeAccountExecutorIsolatesRuntimeStatePerAccount(t *testing.T) {
	authA := newClaudeAccountRuntimeTestAuth(t,
		"10000000-0000-4000-8000-000000000001",
		"20000000-0000-4000-8000-000000000001",
		"30000000-0000-4000-8000-000000000001",
	)
	authB := newClaudeAccountRuntimeTestAuth(t,
		"10000000-0000-4000-8000-000000000002",
		"20000000-0000-4000-8000-000000000002",
		"30000000-0000-4000-8000-000000000002",
	)
	cfg := &config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath: t.TempDir(),
		MachineProfiles: []config.ClaudeDesktopMachineProfile{
			{ID: "machine-a", TotalMemoryBytes: 16 << 30, AvailableMemoryBytes: 8 << 30, CPUModel: "CPU A", OSVersion: "10.0.26100"},
			{ID: "machine-b", TotalMemoryBytes: 32 << 30, AvailableMemoryBytes: 20 << 30, CPUModel: "CPU B", OSVersion: "10.0.26100"},
		},
		MachineProfileBindings: map[string]string{
			authA.ID: "machine-a",
			authB.ID: "machine-b",
		},
	}}
	executor := newClaudeAccountTestExecutor(cfg)
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(authA); errProvision != nil {
		t.Fatalf("Provision(authA) error = %v", errProvision)
	}
	if errProvision := executor.Provision(authB); errProvision != nil {
		t.Fatalf("Provision(authB) error = %v", errProvision)
	}

	runtimeA := accountRuntimeForAuth(t, executor, authA.ID)
	runtimeB := accountRuntimeForAuth(t, executor, authB.ID)
	if runtimeA == runtimeB || runtimeA.executor == runtimeB.executor {
		t.Fatal("accounts share a logical Claude Desktop runtime")
	}
	if runtimeA.executor.desktopTelemetry == runtimeB.executor.desktopTelemetry {
		t.Fatal("accounts share a telemetry manager")
	}
	if runtimeA.executor.desktopTransports == runtimeB.executor.desktopTransports {
		t.Fatal("accounts share a transport registry")
	}
	if runtimeA.stateDirectory == runtimeB.stateDirectory {
		t.Fatalf("accounts share state directory %q", runtimeA.stateDirectory)
	}
	if filepath.Dir(filepath.Dir(runtimeA.stateDirectory)) == filepath.Dir(filepath.Dir(runtimeB.stateDirectory)) {
		t.Fatal("accounts share the same account state root")
	}
	for _, runtimeRef := range []*claudeAccountRuntime{runtimeA, runtimeB} {
		if _, errStat := os.Stat(runtimeRef.stateDirectory); errStat != nil {
			t.Fatalf("state directory %q is unavailable: %v", runtimeRef.stateDirectory, errStat)
		}
	}

	statuses := claudetelemetry.StatusSnapshots()
	statusA := telemetryStatusForMachine(t, statuses, "machine-a")
	statusB := telemetryStatusForMachine(t, statuses, "machine-b")
	if statusA.AppSessionIDHash == "" || statusA.AppSessionIDHash == statusB.AppSessionIDHash {
		t.Fatalf("account app-session hashes are not isolated: A=%q B=%q", statusA.AppSessionIDHash, statusB.AppSessionIDHash)
	}
	if statusA.RuntimeStartedAt == nil || statusB.RuntimeStartedAt == nil {
		t.Fatal("account runtime start timestamps are missing")
	}
}

func TestClaudeAccountExecutorPersistsStableMachineAssignment(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"40000000-0000-4000-8000-000000000001",
		"50000000-0000-4000-8000-000000000001",
		"60000000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	profiles := []config.ClaudeDesktopMachineProfile{{ID: "pool-a"}, {ID: "pool-b"}}
	first := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath:       stateRoot,
		MachineProfiles: profiles,
	}})
	if errProvision := first.Provision(auth); errProvision != nil {
		t.Fatalf("first Provision() error = %v", errProvision)
	}
	firstRuntime := accountRuntimeForAuth(t, first, auth.ID)
	firstProfile := firstRuntime.machineProfileID
	firstAccountRoot := filepath.Dir(filepath.Dir(firstRuntime.stateDirectory))
	first.Close()

	second := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath:       stateRoot,
		MachineProfiles: []config.ClaudeDesktopMachineProfile{profiles[1], profiles[0]},
	}})
	t.Cleanup(second.Close)
	if errProvision := second.Provision(auth); errProvision != nil {
		t.Fatalf("second Provision() error = %v", errProvision)
	}
	secondRuntime := accountRuntimeForAuth(t, second, auth.ID)
	if secondRuntime.machineProfileID != firstProfile {
		t.Fatalf("machine assignment changed after pool reorder: first=%q second=%q", firstProfile, secondRuntime.machineProfileID)
	}
	if secondAccountRoot := filepath.Dir(filepath.Dir(secondRuntime.stateDirectory)); secondAccountRoot != firstAccountRoot {
		t.Fatalf("account state root changed: first=%q second=%q", firstAccountRoot, secondAccountRoot)
	}
	bindingPayload, errRead := os.ReadFile(filepath.Join(firstAccountRoot, "machine-binding.json"))
	if errRead != nil {
		t.Fatalf("read machine binding: %v", errRead)
	}
	var binding claudeDesktopMachineBinding
	if errDecode := json.Unmarshal(bindingPayload, &binding); errDecode != nil {
		t.Fatalf("decode machine binding: %v", errDecode)
	}
	if binding.MachineProfileID != firstProfile {
		t.Fatalf("persisted machine profile = %q, want %q", binding.MachineProfileID, firstProfile)
	}
}

func TestClaudeAccountExecutorReadyEnrollmentStaysOutsideScheduler(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"42000000-0000-4000-8000-000000000001",
		"52000000-0000-4000-8000-000000000001",
		"62000000-0000-4000-8000-000000000001",
	)
	enrollment, errEnrollment := claudedesktop.ParseEnrollment(auth.Metadata)
	if errEnrollment != nil {
		t.Fatal(errEnrollment)
	}
	enrollment.State = claudedesktop.EnrollmentReady
	auth.Metadata[claudedesktop.MetadataEnrollmentKey] = enrollment

	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatalf("Provision(ready) error = %v", errProvision)
	}
	if errSchedule := executor.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "not active") {
		t.Fatalf("CanScheduleAuth(ready) error = %v, want active-only rejection", errSchedule)
	}
	executor.mu.Lock()
	runtimeRef := executor.runtimes[auth.ID]
	executor.mu.Unlock()
	if runtimeRef != nil {
		t.Fatal("ready enrollment created a schedulable runtime")
	}
}

func TestClaudeAccountExecutorGenericDisabledFlagStopsActiveEnrollmentRuntime(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"42500000-0000-4000-8000-000000000001",
		"52500000-0000-4000-8000-000000000001",
		"62500000-0000-4000-8000-000000000001",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	auth.Disabled = true
	auth.Status = cliproxyauth.StatusDisabled
	executor.SyncAuth(auth)
	if errSchedule := executor.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "disabled") {
		t.Fatalf("CanScheduleAuth(disabled) error = %v", errSchedule)
	}
	executor.mu.Lock()
	runtimeRef := executor.runtimes[auth.ID]
	executor.mu.Unlock()
	if runtimeRef != nil {
		t.Fatal("generic disabled state retained an active Desktop runtime")
	}
}

func TestClaudeAccountExecutorSchedulesInferenceWithoutTrustedDevice(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"42500000-0000-4000-8000-000000000001",
		"52500000-0000-4000-8000-000000000001",
		"62500000-0000-4000-8000-000000000001",
	)
	delete(auth.Metadata, claudedesktop.MetadataTrustedDeviceTokenKey)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errSchedule := executor.CanScheduleAuth(auth); errSchedule != nil {
		t.Fatalf("CanScheduleAuth(without trusted device) error = %v", errSchedule)
	}
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatalf("Provision(without trusted device) error = %v", errProvision)
	}
	if accountRuntimeForAuth(t, executor, auth.ID) == nil {
		t.Fatal("inference credential did not create an account runtime")
	}
}

func TestClaudeAccountExecutorEnableWaitsForDrainingLifetime(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"42600000-0000-4000-8000-000000000001",
		"52600000-0000-4000-8000-000000000001",
		"62600000-0000-4000-8000-000000000001",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	first := accountRuntimeForAuth(t, executor, auth.ID)
	firstStatus := executor.AccountStatus(auth.ID)
	if !first.acquire() {
		t.Fatal("failed to hold an active request on the first runtime")
	}

	if _, errDisabled := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentDisabled, "operator disabled", time.Now()); errDisabled != nil {
		t.Fatal(errDisabled)
	}
	auth.Disabled = true
	auth.Status = cliproxyauth.StatusDisabled
	executor.SyncAuth(auth)

	if _, errReady := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentReady, "", time.Now()); errReady != nil {
		t.Fatal(errReady)
	}
	if _, errActive := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentActive, "", time.Now()); errActive != nil {
		t.Fatal(errActive)
	}
	auth.Disabled = false
	auth.Status = cliproxyauth.StatusActive
	executor.SyncAuth(auth)

	waiting := executor.AccountStatus(auth.ID)
	if waiting.RuntimeLoaded || !waiting.RuntimeStopping {
		t.Fatalf("enable did not wait for the previous lifetime: %+v", waiting)
	}
	if waiting.State != claudedesktop.EnrollmentActive || waiting.QuarantineReason != "" {
		t.Fatalf("normal draining was misclassified as quarantine: %+v", waiting)
	}
	if errProvision := executor.Provision(auth); !errors.Is(errProvision, errClaudeDesktopRuntimeDraining) {
		t.Fatalf("Provision() error = %v, want typed draining error", errProvision)
	}

	first.release()
	stopped := executor.AccountStatus(auth.ID)
	if stopped.LifecycleState != claudeDesktopAppStopped || stopped.RuntimeStopping {
		t.Fatalf("drained lifetime did not close cleanly: %+v", stopped)
	}
	executor.SyncAuth(auth)
	second := accountRuntimeForAuth(t, executor, auth.ID)
	secondStatus := executor.AccountStatus(auth.ID)
	if second == first {
		t.Fatal("re-enable reused the stopped application lifetime")
	}
	if secondStatus.LifecycleGen != firstStatus.LifecycleGen+1 || secondStatus.AppSessionHash == firstStatus.AppSessionHash {
		t.Fatalf("re-enable lifecycle identity = %+v, first = %+v", secondStatus, firstStatus)
	}
	if secondStatus.PreviousExit != claudeDesktopExitClean || secondStatus.RecoveredUnclean {
		t.Fatalf("clean restart was not recorded: %+v", secondStatus)
	}
}

func TestClaudeAccountExecutorRecoversUncleanApplicationLifetime(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"42700000-0000-4000-8000-000000000001",
		"52700000-0000-4000-8000-000000000001",
		"62700000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	first := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	if errProvision := first.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	firstRuntime := accountRuntimeForAuth(t, first, auth.ID)
	firstStatus := first.AccountStatus(auth.ID)

	// Simulate process death: release process-owned resources without running
	// the account wrapper's normal lifecycle finalizer.
	first.mu.Lock()
	delete(first.runtimes, auth.ID)
	first.closed = true
	first.mu.Unlock()
	firstRuntime.executor.Quarantine()
	firstRuntime.mu.Lock()
	firstRuntime.retiring = true
	firstRuntime.quarantined = true
	firstRuntime.closed = true
	firstRuntime.mu.Unlock()

	second := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	t.Cleanup(second.Close)
	if errProvision := second.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	secondStatus := second.AccountStatus(auth.ID)
	if secondStatus.LifecycleGen != firstStatus.LifecycleGen+1 || secondStatus.AppSessionHash == firstStatus.AppSessionHash {
		t.Fatalf("unclean recovery lifecycle identity = %+v, first = %+v", secondStatus, firstStatus)
	}
	if secondStatus.PreviousExit != claudeDesktopExitUnclean || !secondStatus.RecoveredUnclean {
		t.Fatalf("unclean process exit was not recovered: %+v", secondStatus)
	}
}

func TestClaudeAccountExecutorTokenRefreshPreservesRuntimeIdentity(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"43000000-0000-4000-8000-000000000001",
		"53000000-0000-4000-8000-000000000001",
		"63000000-0000-4000-8000-000000000001",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	before := accountRuntimeForAuth(t, executor, auth.ID)
	beforeStatus := executor.AccountStatus(auth.ID)
	bindingPath := filepath.Join(filepath.Dir(filepath.Dir(before.stateDirectory)), "runtime-binding.json")
	bindingPayload, errRead := os.ReadFile(bindingPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, secretOrIdentity := range []string{
		auth.ID,
		"43000000-0000-4000-8000-000000000001",
		"53000000-0000-4000-8000-000000000001",
		auth.Metadata[claudedesktop.MetadataTrustedDeviceTokenKey].(string),
		auth.Attributes[cliproxyauth.AttributeAPIKey],
	} {
		if strings.Contains(string(bindingPayload), secretOrIdentity) {
			t.Fatalf("runtime binding contains raw account identity or secret %q", secretOrIdentity)
		}
	}

	auth.Attributes[cliproxyauth.AttributeAPIKey] = "sk-ant-oat-refreshed-access-token"
	auth.Metadata["access_token"] = "sk-ant-oat-refreshed-access-token"
	auth.Metadata["refresh_token"] = "refreshed-refresh-token"
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatalf("Provision(after refresh) error = %v", errProvision)
	}
	after := accountRuntimeForAuth(t, executor, auth.ID)
	afterStatus := executor.AccountStatus(auth.ID)
	if after != before {
		t.Fatal("token refresh replaced the account runtime")
	}
	if after.revision != before.revision || after.accountKey != before.accountKey {
		t.Fatal("token refresh changed stable runtime identity")
	}
	if afterStatus.ApprovedRevision != beforeStatus.ApprovedRevision || afterStatus.AuthIDHash != beforeStatus.AuthIDHash {
		t.Fatalf("token refresh changed runtime binding: before=%+v after=%+v", beforeStatus, afterStatus)
	}
}

func TestClaudeAccountExecutorConcurrentAccountsRemainIsolated(t *testing.T) {
	authA := newClaudeAccountRuntimeTestAuth(t,
		"44000000-0000-4000-8000-000000000001",
		"54000000-0000-4000-8000-000000000001",
		"64000000-0000-4000-8000-000000000001",
	)
	authB := newClaudeAccountRuntimeTestAuth(t,
		"44000000-0000-4000-8000-000000000002",
		"54000000-0000-4000-8000-000000000002",
		"64000000-0000-4000-8000-000000000002",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)

	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, auth := range []*cliproxyauth.Auth{authA, authB} {
		workers.Add(1)
		go func(candidate *cliproxyauth.Auth) {
			defer workers.Done()
			<-start
			errors <- executor.Provision(candidate)
		}(auth)
	}
	close(start)
	workers.Wait()
	close(errors)
	for errProvision := range errors {
		if errProvision != nil {
			t.Fatal(errProvision)
		}
	}

	runtimeA := accountRuntimeForAuth(t, executor, authA.ID)
	runtimeB := accountRuntimeForAuth(t, executor, authB.ID)
	if runtimeA == runtimeB || runtimeA.executor == runtimeB.executor || runtimeA.executor.desktopTelemetry == runtimeB.executor.desktopTelemetry || runtimeA.executor.desktopTransports == runtimeB.executor.desktopTransports {
		t.Fatal("concurrent accounts share mutable Desktop runtime resources")
	}
	if runtimeA.accountKey == runtimeB.accountKey || runtimeA.stateDirectory == runtimeB.stateDirectory {
		t.Fatal("concurrent accounts share a persistent partition")
	}
}

func TestClaudeAccountExecutorInvalidPresentTelemetryMaterialsFailClosed(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"45000000-0000-4000-8000-000000000001",
		"55000000-0000-4000-8000-000000000001",
		"65000000-0000-4000-8000-000000000001",
	)
	auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey] = map[string]any{
		"segment_write_key": "invalid material with spaces",
	}
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errSchedule := executor.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "materials are invalid") {
		t.Fatalf("CanScheduleAuth(invalid materials) error = %v", errSchedule)
	}
	executor.mu.Lock()
	runtimeRef := executor.runtimes[auth.ID]
	executor.mu.Unlock()
	if runtimeRef != nil {
		t.Fatal("invalid-present telemetry materials created a runtime")
	}
}

func TestClaudeAccountExecutorPreloadsBindingsAndBalancesMachinePool(t *testing.T) {
	authA := newClaudeAccountRuntimeTestAuth(t,
		"41000000-0000-4000-8000-000000000001",
		"51000000-0000-4000-8000-000000000001",
		"61000000-0000-4000-8000-000000000001",
	)
	authB := newClaudeAccountRuntimeTestAuth(t,
		"41000000-0000-4000-8000-000000000002",
		"51000000-0000-4000-8000-000000000002",
		"61000000-0000-4000-8000-000000000002",
	)
	stateRoot := t.TempDir()
	profiles := []config.ClaudeDesktopMachineProfile{{ID: "pool-a"}, {ID: "pool-b"}}
	first := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath:       stateRoot,
		MachineProfiles: profiles,
	}})
	if errProvision := first.Provision(authA); errProvision != nil {
		t.Fatal(errProvision)
	}
	profileA := accountRuntimeForAuth(t, first, authA.ID).machineProfileID
	first.Close()

	second := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath:       stateRoot,
		MachineProfiles: []config.ClaudeDesktopMachineProfile{profiles[1], profiles[0]},
	}})
	t.Cleanup(second.Close)
	if errProvision := second.Provision(authB); errProvision != nil {
		t.Fatal(errProvision)
	}
	profileB := accountRuntimeForAuth(t, second, authB.ID).machineProfileID
	if profileB == profileA {
		t.Fatalf("new account reused occupied machine profile %q while another profile was available", profileA)
	}
	if errProvision := second.Provision(authA); errProvision != nil {
		t.Fatal(errProvision)
	}
	if restored := accountRuntimeForAuth(t, second, authA.ID).machineProfileID; restored != profileA {
		t.Fatalf("persisted machine profile = %q after restart, want %q", restored, profileA)
	}
}

func TestClaudeAccountExecutorProxyMigrationQuarantinesOnlyOneRuntimeUntilPromotion(t *testing.T) {
	authA := newClaudeAccountRuntimeTestAuth(t,
		"70000000-0000-4000-8000-000000000001",
		"80000000-0000-4000-8000-000000000001",
		"90000000-0000-4000-8000-000000000001",
	)
	authB := newClaudeAccountRuntimeTestAuth(t,
		"70000000-0000-4000-8000-000000000002",
		"80000000-0000-4000-8000-000000000002",
		"90000000-0000-4000-8000-000000000002",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(authA); errProvision != nil {
		t.Fatal(errProvision)
	}
	if errProvision := executor.Provision(authB); errProvision != nil {
		t.Fatal(errProvision)
	}
	firstA := accountRuntimeForAuth(t, executor, authA.ID)
	firstB := accountRuntimeForAuth(t, executor, authB.ID)

	authA.ProxyURL = "http://127.0.0.1:18080"
	if errSchedule := executor.CanScheduleAuth(authA); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("CanScheduleAuth(authA) error = %v, want quarantine", errSchedule)
	}
	statusA := executor.AccountStatus(authA.ID)
	if statusA.State != claudedesktop.EnrollmentQuarantined || statusA.RuntimeLoaded {
		t.Fatalf("quarantined status = %+v", statusA)
	}
	secondB := accountRuntimeForAuth(t, executor, authB.ID)
	if secondB != firstB {
		t.Fatal("one account proxy drift replaced another account runtime")
	}
	if errSchedule := executor.CanScheduleAuth(authB); errSchedule != nil {
		t.Fatalf("CanScheduleAuth(authB) error = %v", errSchedule)
	}
	if errPromote := executor.PromoteAuth(authA); errPromote != nil {
		t.Fatalf("PromoteAuth(authA) error = %v", errPromote)
	}
	secondA := accountRuntimeForAuth(t, executor, authA.ID)
	if secondA == firstA || secondA.revision == firstA.revision {
		t.Fatal("explicit promotion reused the previous proxy revision")
	}
	if filepath.Dir(filepath.Dir(secondA.stateDirectory)) != filepath.Dir(filepath.Dir(firstA.stateDirectory)) {
		t.Fatal("proxy promotion changed the stable account state root")
	}
	if currentB := accountRuntimeForAuth(t, executor, authB.ID); currentB != firstB {
		t.Fatal("promoting one account disrupted another account runtime")
	}
}

func TestClaudeAccountExecutorPromotionAndRollbackPreserveRevisionState(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"70500000-0000-4000-8000-000000000001",
		"80500000-0000-4000-8000-000000000001",
		"90500000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	newExecutor := func() *ClaudeAccountExecutor {
		return newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	}

	first := newExecutor()
	if errProvision := first.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	oldRuntime := accountRuntimeForAuth(t, first, auth.ID)
	oldRevision := oldRuntime.revision
	preservedPath := filepath.Join(oldRuntime.stateDirectory, "preserved-queue-proof")
	if errWrite := os.WriteFile(preservedPath, []byte("durable"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	first.Close()

	auth.ProxyURL = "http://127.0.0.1:18085"
	second := newExecutor()
	if errSchedule := second.CanScheduleAuth(auth); errSchedule == nil {
		t.Fatal("proxy drift did not quarantine the old approved revision")
	}
	if errPromote := second.PromoteAuth(auth); errPromote != nil {
		t.Fatal(errPromote)
	}
	promoted := second.AccountStatus(auth.ID)
	newRevision := promoted.ApprovedRevision
	if promoted.State != claudedesktop.EnrollmentActive || promoted.PreviousRevision != oldRevision || newRevision == oldRevision {
		t.Fatalf("promotion status = %+v", promoted)
	}
	second.Close()

	auth.ProxyURL = ""
	third := newExecutor()
	t.Cleanup(third.Close)
	if errSchedule := third.CanScheduleAuth(auth); errSchedule == nil {
		t.Fatal("restored previous configuration did not surface rollback drift")
	}
	if errRollback := third.RollbackAuth(auth); errRollback != nil {
		t.Fatal(errRollback)
	}
	rolledBack := third.AccountStatus(auth.ID)
	if rolledBack.State != claudedesktop.EnrollmentActive || rolledBack.ApprovedRevision != oldRevision || rolledBack.PreviousRevision != newRevision {
		t.Fatalf("rollback status = %+v", rolledBack)
	}
	if payload, errRead := os.ReadFile(preservedPath); errRead != nil || string(payload) != "durable" {
		t.Fatalf("old revision durable state was not preserved: payload=%q err=%v", payload, errRead)
	}
}

func TestClaudeAccountExecutorPromotionFailureRestoresPreviousBinding(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"70550000-0000-4000-8000-000000000001",
		"80550000-0000-4000-8000-000000000001",
		"90550000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	first := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	if errProvision := first.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	oldRevision := first.AccountStatus(auth.ID).ApprovedRevision
	first.Close()

	auth.ProxyURL = "http://127.0.0.1:18086"
	second := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	t.Cleanup(second.Close)
	if errSchedule := second.CanScheduleAuth(auth); errSchedule == nil {
		t.Fatal("proxy drift did not quarantine the old approved revision")
	}
	drifted := second.AccountStatus(auth.ID)
	if !drifted.CanPromote || drifted.ObservedRevision == "" {
		t.Fatalf("drift status = %+v", drifted)
	}
	accountDirectory := filepath.Join(stateRoot, "accounts", claudeDesktopAccountKey(auth.ID))
	instancesDirectory := filepath.Join(accountDirectory, "instances")
	if errMkdir := os.MkdirAll(instancesDirectory, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	blockedInstancePath := filepath.Join(instancesDirectory, claudeDesktopRevisionInstanceID(drifted.ObservedRevision))
	if errWrite := os.WriteFile(blockedInstancePath, []byte("block directory creation"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	if errPromote := second.PromoteAuth(auth); errPromote == nil {
		t.Fatal("promotion unexpectedly succeeded with an unavailable instance directory")
	}
	restored := second.AccountStatus(auth.ID)
	if restored.ApprovedRevision != oldRevision || restored.ObservedRevision != drifted.ObservedRevision || restored.State != claudedesktop.EnrollmentQuarantined || !restored.CanPromote {
		t.Fatalf("failed promotion did not restore the previous binding: before=%+v after=%+v", drifted, restored)
	}
}

func TestClaudeAccountExecutorRejectsMalformedPersistedRevisionWithoutPanic(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"70600000-0000-4000-8000-000000000001",
		"80600000-0000-4000-8000-000000000001",
		"90600000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	t.Cleanup(executor.Close)
	accountKey := claudeDesktopAccountKey(auth.ID)
	accountDirectory := filepath.Join(stateRoot, "accounts", accountKey)
	if errMkdir := os.MkdirAll(accountDirectory, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	payload := []byte(`{"version":1,"auth_id_hash":"` + accountKey + `","state":"active","approved_revision":"short","machine_profile_id":"local-host","desktop_profile":"1.4.0.609"}`)
	if errWrite := os.WriteFile(filepath.Join(accountDirectory, "runtime-binding.json"), payload, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	status := executor.AccountStatus(auth.ID)
	if status.ApprovedRevision != "" || status.RuntimeLoaded {
		t.Fatalf("malformed binding was exposed as valid: %+v", status)
	}
	if errSchedule := executor.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "binding is unreadable") {
		t.Fatalf("CanScheduleAuth() error = %v, want unreadable binding", errSchedule)
	}
}

func TestClaudeAccountExecutorQuarantinePersistsAcrossRestart(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"71000000-0000-4000-8000-000000000001",
		"81000000-0000-4000-8000-000000000001",
		"91000000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	first := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	if errProvision := first.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	first.Close()

	auth.ProxyURL = "http://127.0.0.1:18081"
	second := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	if errSchedule := second.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("first drift check error = %v", errSchedule)
	}
	second.Close()

	third := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: stateRoot}})
	t.Cleanup(third.Close)
	if errSchedule := third.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("restart drift check error = %v", errSchedule)
	}
	status := third.AccountStatus(auth.ID)
	if status.State != claudedesktop.EnrollmentQuarantined || status.ApprovedRevision == "" || status.ObservedRevision == "" || status.ApprovedRevision == status.ObservedRevision {
		t.Fatalf("persisted quarantine status = %+v", status)
	}
	if errPromote := third.PromoteAuth(auth); errPromote != nil {
		t.Fatalf("PromoteAuth() error = %v", errPromote)
	}
	if promoted := third.AccountStatus(auth.ID); promoted.State != claudedesktop.EnrollmentActive || !promoted.RuntimeLoaded || promoted.ObservedRevision != "" {
		t.Fatalf("promoted status = %+v", promoted)
	}
}

func TestClaudeAccountExecutorMachineProfileDriftRequiresPromotion(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"72000000-0000-4000-8000-000000000001",
		"82000000-0000-4000-8000-000000000001",
		"92000000-0000-4000-8000-000000000001",
	)
	stateRoot := t.TempDir()
	first := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath: stateRoot,
		MachineProfiles: []config.ClaudeDesktopMachineProfile{{
			ID: "bound-machine", CPUModel: "measured-cpu-a", OSVersion: "10.0.26100",
		}},
		MachineProfileBindings: map[string]string{auth.ID: "bound-machine"},
	}})
	if errProvision := first.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	first.Close()

	second := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{
		StatePath: stateRoot,
		MachineProfiles: []config.ClaudeDesktopMachineProfile{{
			ID: "bound-machine", CPUModel: "measured-cpu-b", OSVersion: "10.0.26100",
		}},
		MachineProfileBindings: map[string]string{auth.ID: "bound-machine"},
	}})
	t.Cleanup(second.Close)
	if errSchedule := second.CanScheduleAuth(auth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("CanScheduleAuth(profile drift) error = %v", errSchedule)
	}
	if status := second.AccountStatus(auth.ID); status.State != claudedesktop.EnrollmentQuarantined || status.MachineProfileID != "bound-machine" {
		t.Fatalf("profile drift status = %+v", status)
	}
}

func TestClaudeAccountExecutorManagerLifecycleSeparatesRegistryAndSchedulerPool(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"73000000-0000-4000-8000-000000000001",
		"83000000-0000-4000-8000-000000000001",
		"93000000-0000-4000-8000-000000000001",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	first := accountRuntimeForAuth(t, executor, auth.ID)
	if !manager.HasProviderAuth(claudedesktop.Provider) {
		t.Fatal("active Desktop auth is missing from scheduler pool")
	}

	if _, errDisable := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentDisabled, "operator disabled", time.Now()); errDisable != nil {
		t.Fatal(errDisable)
	}
	auth.Disabled = true
	auth.Status = cliproxyauth.StatusDisabled
	if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if manager.HasProviderAuth(claudedesktop.Provider) {
		t.Fatal("disabled Desktop auth remained in scheduler pool")
	}
	if stored, ok := manager.GetByID(auth.ID); !ok || stored == nil {
		t.Fatal("disabled Desktop auth was removed from account registry")
	}
	executor.mu.Lock()
	disabledRuntime := executor.runtimes[auth.ID]
	executor.mu.Unlock()
	if disabledRuntime != nil {
		t.Fatal("disabled Desktop auth retained a live runtime")
	}

	if _, errReady := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentReady, "", time.Now()); errReady != nil {
		t.Fatal(errReady)
	}
	if _, errActive := claudedesktop.TransitionMetadataEnrollment(auth.Metadata, auth.ID, claudedesktop.EnrollmentActive, "", time.Now()); errActive != nil {
		t.Fatal(errActive)
	}
	auth.Disabled = false
	auth.Status = cliproxyauth.StatusActive
	if _, errUpdate := manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	second := accountRuntimeForAuth(t, executor, auth.ID)
	if second == first {
		t.Fatal("re-enabled account reused its stopped application runtime")
	}
	if second.accountKey != first.accountKey || second.revision != first.revision {
		t.Fatal("re-enable changed stable account identity or approved binding")
	}
	if !manager.HasProviderAuth(claudedesktop.Provider) {
		t.Fatal("re-enabled Desktop auth did not return to scheduler pool")
	}
}

func TestClaudeAccountExecutorCloseAuthPreservesOtherAccounts(t *testing.T) {
	authA := newClaudeAccountRuntimeTestAuth(t,
		"a0000000-0000-4000-8000-000000000001",
		"b0000000-0000-4000-8000-000000000001",
		"c0000000-0000-4000-8000-000000000001",
	)
	authB := newClaudeAccountRuntimeTestAuth(t,
		"a0000000-0000-4000-8000-000000000002",
		"b0000000-0000-4000-8000-000000000002",
		"c0000000-0000-4000-8000-000000000002",
	)
	executor := newClaudeAccountTestExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(authA); errProvision != nil {
		t.Fatal(errProvision)
	}
	if errProvision := executor.Provision(authB); errProvision != nil {
		t.Fatal(errProvision)
	}
	runtimeB := accountRuntimeForAuth(t, executor, authB.ID)
	executor.CloseAuth(authA.ID)

	executor.mu.Lock()
	_, stillMappedA := executor.runtimes[authA.ID]
	currentB := executor.runtimes[authB.ID]
	executor.mu.Unlock()
	if stillMappedA {
		t.Fatal("removed account still has a runtime mapping")
	}
	if currentB != runtimeB {
		t.Fatal("removing one account disrupted another account runtime")
	}
}

func TestClaudeAccountRuntimeResponseBodyReleasesOnce(t *testing.T) {
	releases := 0
	body := &claudeAccountRuntimeResponseBody{
		ReadCloser: &claudeAccountTestReadCloser{},
		release: func() {
			releases++
		},
	}
	if errClose := body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errClose := body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if releases != 1 {
		t.Fatalf("runtime releases = %d, want 1", releases)
	}
	if body.ReadCloser.(*claudeAccountTestReadCloser).closeCalls != 1 {
		t.Fatalf("underlying body closes = %d, want 1", body.ReadCloser.(*claudeAccountTestReadCloser).closeCalls)
	}
}

type claudeAccountTestReadCloser struct {
	closeCalls int
}

func (*claudeAccountTestReadCloser) Read([]byte) (int, error) { return 0, nil }

func (b *claudeAccountTestReadCloser) Close() error {
	b.closeCalls++
	return nil
}

func newClaudeAccountRuntimeTestAuth(t *testing.T, accountUUID, organizationUUID, deviceID string) *cliproxyauth.Auth {
	t.Helper()
	authID, errAuthID := claudedesktop.StableAuthID(accountUUID, organizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	device := claudedesktop.TrustedDevice{DeviceID: deviceID, DeviceToken: "trusted-" + deviceID, DisplayName: "Desktop"}
	enrollment := claudedesktop.NewEnrollment(authID, claudedesktop.AccountIdentity{
		AccountUUID: accountUUID, OrganizationUUID: organizationUUID,
	}, device, time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC))
	enrollment, errEnrollment := claudedesktop.TransitionEnrollment(
		enrollment,
		claudedesktop.EnrollmentActive,
		"",
		time.Date(2026, time.September, 2, 12, 0, 1, 0, time.UTC),
	)
	if errEnrollment != nil {
		t.Fatal(errEnrollment)
	}
	return &cliproxyauth.Auth{
		ID:       authID,
		Provider: claudedesktop.Provider,
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey:   "sk-ant-oat-" + strings.ReplaceAll(accountUUID, "-", ""),
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
		Metadata: map[string]any{
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			"account_uuid":      accountUUID,
			"organization_uuid": organizationUUID,
			"claude_device_ids": []string{claudedesktop.RequestDeviceID(deviceID)},
		},
	}
}

func accountRuntimeForAuth(t *testing.T, executor *ClaudeAccountExecutor, authID string) *claudeAccountRuntime {
	t.Helper()
	executor.mu.Lock()
	defer executor.mu.Unlock()
	runtimeRef := executor.runtimes[authID]
	if runtimeRef == nil {
		t.Fatalf("runtime for auth %q is missing", authID)
	}
	return runtimeRef
}

func telemetryStatusForMachine(t *testing.T, statuses []claudetelemetry.Status, machineProfileID string) claudetelemetry.Status {
	t.Helper()
	for _, status := range statuses {
		if status.MachineProfileID == machineProfileID {
			return status
		}
	}
	t.Fatalf("telemetry status for machine profile %q is missing: %#v", machineProfileID, statuses)
	return claudetelemetry.Status{}
}

func TestClaudeAccountExecutorPromoteReactivatesApprovedRevision(t *testing.T) {
	auth := newClaudeAccountRuntimeTestAuth(t,
		"46000000-0000-4000-8000-000000000001",
		"56000000-0000-4000-8000-000000000001",
		"66000000-0000-4000-8000-000000000001",
	)
	auth.ProxyURL = "socks5h://127.0.0.1:11080"
	executor := newClaudeAccountTestExecutor(&config.Config{
		SDKConfig:     config.SDKConfig{ProxyURL: "http://127.0.0.1:3128"},
		ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()},
	})
	t.Cleanup(executor.Close)
	if errProvision := executor.Provision(auth); errProvision != nil {
		t.Fatal(errProvision)
	}
	approved := accountRuntimeForAuth(t, executor, auth.ID).revision

	// A registration path that drops auth.ProxyURL (e.g. the file token store)
	// falls back to the global proxy and observes a different revision.
	noProxyAuth := auth.Clone()
	noProxyAuth.ProxyURL = ""
	if errSchedule := executor.CanScheduleAuth(noProxyAuth); errSchedule == nil || !strings.Contains(errSchedule.Error(), "quarantined") {
		t.Fatalf("CanScheduleAuth(drifted) error = %v, want quarantine", errSchedule)
	}
	status := executor.AccountStatus(auth.ID)
	if status.State != claudedesktop.EnrollmentQuarantined || status.ObservedRevision == "" || status.ObservedRevision == approved {
		t.Fatalf("drift quarantine was not recorded: %+v", status)
	}

	// A different revision still cannot be promoted past the stale observation.
	thirdAuth := auth.Clone()
	thirdAuth.ProxyURL = "socks5h://127.0.0.1:22020"
	if errPromote := executor.PromoteAuth(thirdAuth); errPromote == nil || !strings.Contains(errPromote.Error(), "observed runtime revision changed") {
		t.Fatalf("PromoteAuth(third revision) error = %v, want observed-mismatch rejection", errPromote)
	}

	// The desired revision already matches the approved binding, so promotion
	// re-activates it instead of deadlocking on the stale observation.
	if errPromote := executor.PromoteAuth(auth); errPromote != nil {
		t.Fatalf("PromoteAuth(approved revision) error = %v", errPromote)
	}
	status = executor.AccountStatus(auth.ID)
	if status.State != claudedesktop.EnrollmentActive || !status.RuntimeLoaded || status.ApprovedRevision != approved || status.ObservedRevision != "" {
		t.Fatalf("promoted binding = %+v, want active approved revision without stale observation", status)
	}
	if errSchedule := executor.CanScheduleAuth(auth); errSchedule != nil {
		t.Fatalf("CanScheduleAuth(after promote) error = %v", errSchedule)
	}
}
