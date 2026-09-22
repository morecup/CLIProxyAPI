package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func sdkEventsFromRequests(t *testing.T, requests []recordedRequest) []sdkEventWrapper {
	t.Helper()
	var result []sdkEventWrapper
	for _, request := range requests {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil || len(batch.Events) == 0 {
			continue
		}
		result = append(result, batch.Events...)
	}
	return result
}

func sdkMetadataJSON(t *testing.T, event sdkEventWrapper) string {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(event.EventData.AdditionalMetadata)
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}

func TestSDKCompactHookTelemetryUsesOnlyOwnerClassifications(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	input := claudeprompt.SDKCompactHookInput{Event: "PreCompact", Trigger: "auto"}
	executions := []claudeprompt.SDKCompactHookExecution{
		{Command: "PRIVATE_COMMAND_1", Output: "PRIVATE_OUTPUT_1", Succeeded: true, MatcherKind: "match_all", HookType: "command"},
		{Command: "PRIVATE_COMMAND_2", Output: "PRIVATE_OUTPUT_2", Blocked: true, MatcherKind: "specific", HookType: "callback"},
		{Command: "PRIVATE_COMMAND_3", Output: "PRIVATE_OUTPUT_3", Cancelled: true, MatcherKind: "match_all", HookType: "command"},
		{Command: "PRIVATE_COMMAND_4", Output: "PRIVATE_OUTPUT_4", MatcherKind: "specific", HookType: "callback"},
	}
	if err := manager.RecordSDKCompactHook(t.Context(), auth, "session-hook", "claude-opus-5", "prompt-hook", input, executions, 15*time.Millisecond, true); err != nil {
		t.Fatal(err)
	}
	// The runner returned executions and then failed. Native completion telemetry
	// stops after run_hook, and unknown matcher/type inventory stays omitted.
	if err := manager.RecordSDKCompactHook(t.Context(), auth, "session-hook", "claude-opus-5", "prompt-hook", claudeprompt.SDKCompactHookInput{Event: "PostCompact"}, []claudeprompt.SDKCompactHookExecution{{Command: "PRIVATE_FAILED_COMMAND", Succeeded: true}}, 7*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var target []sdkEventWrapper
	for _, event := range sdkEventsFromRequests(t, doer.Requests()) {
		if event.EventData.EventName == "tengu_run_hook" || event.EventData.EventName == "tengu_repl_hook_finished" {
			target = append(target, event)
		}
	}
	if len(target) != 3 || target[0].EventData.EventName != "tengu_run_hook" || target[1].EventData.EventName != "tengu_repl_hook_finished" || target[2].EventData.EventName != "tengu_run_hook" {
		t.Fatalf("compact hook event sequence = %+v", target)
	}
	wantRun := `{"subscription_type":"pro","cc_prompt_id":"prompt-hook","hookName":"PreCompact:auto","numCommands":4,"numMatchAllMatchers":2,"numSpecificMatchers":2,"hookTypeCounts":"{\"command\":2,\"callback\":2}"}`
	if got := sdkMetadataJSON(t, target[0]); got != wantRun {
		t.Fatalf("run_hook metadata = %s, want %s", got, wantRun)
	}
	wantFinished := `{"subscription_type":"pro","cc_prompt_id":"prompt-hook","hookName":"PreCompact:auto","numCommands":4,"numSuccess":1,"numBlocking":1,"numNonBlockingError":1,"numCancelled":1,"totalDurationMs":15}`
	if got := sdkMetadataJSON(t, target[1]); got != wantFinished {
		t.Fatalf("repl_hook_finished metadata = %s, want %s", got, wantFinished)
	}
	wantFailedRun := `{"subscription_type":"pro","cc_prompt_id":"prompt-hook","hookName":"PostCompact","numCommands":1}`
	if got := sdkMetadataJSON(t, target[2]); got != wantFailedRun {
		t.Fatalf("failed runner metadata = %s, want %s", got, wantFailedRun)
	}
	for _, request := range doer.Requests() {
		if strings.Contains(string(request.Body), "PRIVATE_") {
			t.Fatal("hook command or output entered telemetry")
		}
	}
}

func TestSDKCompactHookTelemetryDisabledAndEmptyAreNoops(t *testing.T) {
	var disabled *Manager
	if err := disabled.RecordSDKCompactHook(t.Context(), nil, "", "", "", claudeprompt.SDKCompactHookInput{}, []claudeprompt.SDKCompactHookExecution{{Command: "ignored"}}, time.Second, true); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{}
	if err := manager.RecordSDKCompactHook(t.Context(), nil, "", "", "", claudeprompt.SDKCompactHookInput{}, nil, time.Second, true); err != nil {
		t.Fatal(err)
	}
}
