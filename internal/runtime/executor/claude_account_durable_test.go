package executor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestMigrateClaudeDesktopDurableStateUsesWhitelistAndNeverOverwrites(t *testing.T) {
	accountDirectory := t.TempDir()
	previousRevision := strings.Repeat("a", 64)
	approvedRevision := strings.Repeat("b", 64)
	previousRoot := filepath.Join(accountDirectory, "instances", claudeDesktopRevisionInstanceID(previousRevision))
	approvedRoot := filepath.Join(accountDirectory, "instances", claudeDesktopRevisionInstanceID(approvedRevision))
	durableRoot := filepath.Join(accountDirectory, "durable")

	for index, namespace := range claudeDesktopDurableNamespaces {
		writeClaudeDesktopDurableFixture(t, filepath.Join(previousRoot, namespace, "record.bin"), "previous-"+namespace)
		writeClaudeDesktopDurableFixture(t, filepath.Join(approvedRoot, namespace, "record.bin"), "approved-"+namespace)
		if index == 0 {
			writeClaudeDesktopDurableFixture(t, filepath.Join(durableRoot, namespace, "record.bin"), "accepted")
		}
	}
	writeClaudeDesktopDurableFixture(t, filepath.Join(approvedRoot, "sdk-sessions", "nested", "approved-only.bin"), "approved-only")
	writeClaudeDesktopDurableFixture(t, filepath.Join(previousRoot, "sdk-features", "feature.json"), "revision-only")

	binding := claudeDesktopRuntimeBinding{PreviousRevision: previousRevision, ApprovedRevision: approvedRevision}
	if err := migrateClaudeDesktopDurableState(accountDirectory, durableRoot, binding); err != nil {
		t.Fatal(err)
	}
	for index, namespace := range claudeDesktopDurableNamespaces {
		want := "previous-" + namespace
		if index == 0 {
			want = "accepted"
		}
		if got := readClaudeDesktopDurableFixture(t, filepath.Join(durableRoot, namespace, "record.bin")); got != want {
			t.Fatalf("%s durable payload = %q, want %q", namespace, got, want)
		}
	}
	if got := readClaudeDesktopDurableFixture(t, filepath.Join(durableRoot, "sdk-sessions", "nested", "approved-only.bin")); got != "approved-only" {
		t.Fatalf("approved fallback payload = %q", got)
	}
	if _, err := os.Stat(filepath.Join(durableRoot, "sdk-features", "feature.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revision-only feature cache migrated: %v", err)
	}

	writeClaudeDesktopDurableFixture(t, filepath.Join(previousRoot, "sdk-sessions", "record.bin"), "changed")
	if err := migrateClaudeDesktopDurableState(accountDirectory, durableRoot, binding); err != nil {
		t.Fatal(err)
	}
	if got := readClaudeDesktopDurableFixture(t, filepath.Join(durableRoot, "sdk-sessions", "record.bin")); got != "previous-sdk-sessions" {
		t.Fatalf("retry overwrote accepted durable payload: %q", got)
	}
}

func TestClaudeDesktopSessionSurvivesTrustedDeviceRevisionPromotion(t *testing.T) {
	executor, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	headers := http.Header{"X-Session-Id": {uuid.NewString()}}
	metadata := desktopExecutionMetadata("durable-revision-session")
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return executionSessionTestResponse(t, request), nil
	})))
	if err := invokeQueryLifetimeEntry(ctx, executor, auth, headers, "execute", nil, metadata); err != nil {
		t.Fatal(err)
	}
	beforeList, err := executor.ListDesktopSessions(auth.ID)
	if err != nil || len(beforeList) != 1 || beforeList[0].ID == "" || beforeList[0].SDKSessionID == "" || beforeList[0].QueryID == "" {
		t.Fatalf("initial session = %+v, err=%v", beforeList, err)
	}
	before := beforeList[0]
	oldRuntime := accountRuntimeForAuth(t, executor, auth.ID)
	legacyClaudeDesktopDurableState(t, oldRuntime)

	auth.Metadata[claudedesktop.MetadataTrustedDeviceTokenKey] = "trusted-rotated-" + uuid.NewString()
	if err := executor.CanScheduleAuth(auth); err == nil {
		t.Fatal("trusted-device revision drift did not quarantine the previous runtime")
	}
	if err := executor.PromoteAuth(auth); err != nil {
		t.Fatal(err)
	}
	newRuntime := accountRuntimeForAuth(t, executor, auth.ID)
	if newRuntime.revision == oldRuntime.revision || newRuntime.stateDirectory == oldRuntime.stateDirectory {
		t.Fatal("promotion reused the previous revision instance")
	}
	if newRuntime.executor.desktopDurableStatePath != oldRuntime.executor.desktopDurableStatePath {
		t.Fatal("promotion changed the account durable state path")
	}

	restored, err := executor.ListDesktopSessions(auth.ID)
	if err != nil || len(restored) != 1 || restored[0].ID != before.ID || restored[0].SDKSessionID != before.SDKSessionID || restored[0].Running || restored[0].QueryID != "" {
		t.Fatalf("promoted session = %+v, err=%v", restored, err)
	}
	prepareExecutionSessionAccountTest(t, executor, auths)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	executor.credentialManager = manager
	for _, current := range auths {
		current.Metadata["access_token"] = current.Attributes[cliproxyauth.AttributeAPIKey]
		if _, err := manager.Register(t.Context(), current); err != nil {
			t.Fatal(err)
		}
	}
	resumedValue, err := executor.ResumeDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: before.ID, ExpectedGeneration: before.Generation})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := executor.ListDesktopSessions(auth.ID)
	if err != nil || len(resumed) != 1 {
		t.Fatalf("resumed session = %+v, err=%v", resumed, err)
	}
	after := resumed[0]
	if resumedValue.ID != before.ID || resumedValue.SDKSessionID != before.SDKSessionID || resumedValue.Generation != after.Generation || resumedValue.QueryID != after.QueryID || !resumedValue.Running || after.ID != before.ID || after.SDKSessionID != before.SDKSessionID || after.Generation == before.Generation || after.QueryID == "" || after.QueryID == before.QueryID || !after.Running {
		t.Fatalf("cross-revision resume changed durable identity: before=%+v after=%+v", before, after)
	}
	stopped, err := executor.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: after.ID, ExpectedQueryID: after.QueryID})
	if err != nil || stopped.Running {
		t.Fatalf("stop after cross-revision resume = %+v, err=%v", stopped, err)
	}
}

func legacyClaudeDesktopDurableState(t *testing.T, runtime *claudeAccountRuntime) {
	t.Helper()
	for _, namespace := range claudeDesktopDurableNamespaces {
		source := filepath.Join(runtime.executor.desktopDurableStatePath, namespace)
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(runtime.stateDirectory, namespace)
		if err := os.Rename(source, destination); err != nil {
			t.Fatal(err)
		}
	}
}

func writeClaudeDesktopDurableFixture(t *testing.T, path, payload string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaudeDesktopDurableFixture(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
