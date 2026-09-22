package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestToolLifecycleMetadataFollowsNativeOrder(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-tool-lifecycle-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Events map[string]struct {
			Keys []string `json:"keys"`
		} `json:"events"`
		Classifier struct {
			Cases []struct {
				Label string `json:"label"`
				Code  string `json:"code"`
				IsSad bool   `json:"isSad"`
			} `json:"cases"`
		} `json:"call_error_classifier"`
		DatadogMirror map[string]bool `json:"datadog_mirror"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	depth := 0
	progress, _ := json.Marshal(toolUseProgressMetadata("pro", "p", ToolUse{MessageID: "msg_01", ToolName: "TaskOutput"}, "chain", &depth))
	if string(progress) != `{"subscription_type":"pro","cc_prompt_id":"p","messageID":"msg_01","toolName":"TaskOutput","isMcp":false,"queryChainId":"chain","queryDepth":0}` {
		t.Fatalf("progress metadata = %s", progress)
	}
	assertNativeKeyOrder(t, "tengu_tool_use_progress", jsonKeysInOrder(t, progress)[2:], golden.Events["tool_use_progress"].Keys)
	hint, _ := json.Marshal(cacheEvictionHintMetadata("pro", "p", CacheEvictionHint{Scope: CacheEvictionScopeSubagentEnd, LastRequestID: "req_011CVGjJHiE6NiHqyMcgEuUx"}))
	if string(hint) != `{"subscription_type":"pro","cc_prompt_id":"p","scope":"subagent_end","last_request_id":"req_011CVGjJHiE6NiHqyMcgEuUx"}` {
		t.Fatalf("cache eviction metadata = %s", hint)
	}
	assertNativeKeyOrder(t, "tengu_cache_eviction_hint", jsonKeysInOrder(t, hint)[2:], golden.Events["cache_eviction_hint_subagent_end"].Keys)
	if nonconforming, _ := json.Marshal(cacheEvictionHintMetadata("", "", CacheEvictionHint{Scope: CacheEvictionScopeSubagentEnd, LastRequestID: "req 01"})); string(nonconforming) != `{"scope":"subagent_end","last_request_id":"nonconforming"}` {
		t.Fatalf("Ms guard lost on last_request_id: %s", nonconforming)
	}
	bad, _ := json.Marshal(sdkFeatureMetadata{SubscriptionType: "pro", PromptID: "p", FeatureName: SDKToolFeatureName("TaskStop"), ErrorCode: ToolFeatureErrorCallThrew})
	if string(bad) != `{"subscription_type":"pro","cc_prompt_id":"p","feature_name":"tool_task_stop","error_code":"tool_call_threw"}` {
		t.Fatalf("feature_bad metadata = %s", bad)
	}
	assertNativeKeyOrder(t, "tengu_feature_bad", jsonKeysInOrder(t, bad)[2:], golden.Events["feature_bad"].Keys)
	if empty, _ := json.Marshal(sdkResumePrintMetadata{SubscriptionType: "pro"}); string(empty) != `{"subscription_type":"pro"}` {
		t.Fatalf("resume_print metadata = %s", empty)
	}
	if len(golden.Events["resume_print"].Keys) != 0 {
		t.Fatalf("resume_print is an empty native literal: %v", golden.Events["resume_print"].Keys)
	}
	// The vm-executed BUr classifier: plain errors thrown by the owned tools
	// fall through to tool_call_threw / isSad false.
	plainErrorPinned := false
	for _, c := range golden.Classifier.Cases {
		if c.Label == "plain_error" {
			plainErrorPinned = true
			if c.Code != ToolFeatureErrorCallThrew || c.IsSad {
				t.Fatalf("plain Error classification = %+v", c)
			}
		}
	}
	if !plainErrorPinned {
		t.Fatal("golden lacks the plain_error classifier case")
	}
	if !golden.DatadogMirror["tengu_feature_bad"] || golden.DatadogMirror["tengu_tool_use_progress"] || golden.DatadogMirror["tengu_cache_eviction_hint"] || golden.DatadogMirror["tengu_resume_print"] {
		t.Fatalf("datadog mirror membership = %v", golden.DatadogMirror)
	}
}

func toolLifecycleTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(b *claudeprofile.Bundle) {
		b.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		for fact, name := range map[string]string{
			FactSDKToolUseProgress: "tengu_tool_use_progress", FactSDKCacheEvictionHint: "tengu_cache_eviction_hint", FactSDKFeatureBad: "tengu_feature_bad",
			FactSDKResumePrint: "tengu_resume_print", FactSDKSessionResumed: "tengu_session_resumed",
		} {
			b.SDKTelemetry.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
		}
		if b.AuxiliaryTelemetry.DatadogLogs.Events == nil {
			b.AuxiliaryTelemetry.DatadogLogs.Events = map[string]claudeprofile.TelemetryEventProfile{}
		}
		b.AuxiliaryTelemetry.DatadogLogs.Events[FactSDKFeatureBad] = claudeprofile.TelemetryEventProfile{EventName: "tengu_feature_bad"}
	})
}

func TestToolLifecycleEventsAreDelivered(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := toolLifecycleTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	session, promptID, model := uuid.NewString(), uuid.NewString(), "claude-opus-5"
	if err := m.RecordSDKSessionResumed(t.Context(), auth, session, model, "", SessionResumed{Duration: 250 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	use := ToolUse{MessageID: "msg_01", ToolName: "TaskOutput", Input: json.RawMessage(`{"task_id":"a1"}`)}
	if err := m.RecordSDKToolUseProgress(t.Context(), auth, session, model, promptID, use); err != nil {
		t.Fatal(err)
	}
	child := ToolUse{MessageID: "msg_02", ToolName: "TaskOutput", Depth: 1}
	if err := m.RecordSDKToolUseProgress(t.Context(), auth, session, model, promptID, child); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKFeatureBad(t.Context(), auth, session, model, promptID, "TaskStop"); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKCacheEvictionHint(t.Context(), auth, session, model, promptID, CacheEvictionHint{Scope: CacheEvictionScopeSubagentEnd, LastRequestID: ""}); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKCacheEvictionHint(t.Context(), auth, session, model, promptID, CacheEvictionHint{Scope: "session_end", LastRequestID: "req_01"}); err == nil {
		t.Fatal("session_end is not an owned moment")
	}
	if err := m.RecordSDKCacheEvictionHint(t.Context(), auth, session, model, promptID, CacheEvictionHint{Scope: CacheEvictionScopeSubagentEnd, LastRequestID: "req_011CVGjJHiE6NiHqyMcgEuUx"}); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKToolUseProgress(t.Context(), auth, session, "claude-unknown-model", promptID, use); err == nil {
		t.Fatal("unprofiled model must not invent betas")
	}
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	betas, _ := m.sdkProfile.InputBetaHeader(model)
	var names []string
	payloads := map[string][]string{}
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		if json.Unmarshal(request.Body, &batch) != nil {
			continue
		}
		for _, wrapper := range batch.Events {
			data := wrapper.EventData
			switch data.EventName {
			case "tengu_resume_print", "tengu_session_resumed", "tengu_tool_use_progress", "tengu_feature_bad", "tengu_cache_eviction_hint":
			default:
				continue
			}
			names = append(names, data.EventName)
			if data.SessionID != session || data.Model != model || data.Betas != betas || data.UserType != "external" {
				t.Fatalf("borrowed dimensions on %s: %+v", data.EventName, data)
			}
			decoded, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			payloads[data.EventName] = append(payloads[data.EventName], string(decoded))
		}
	}
	want := "tengu_resume_print,tengu_session_resumed,tengu_tool_use_progress,tengu_tool_use_progress,tengu_feature_bad,tengu_cache_eviction_hint"
	if strings.Join(names, ",") != want {
		t.Fatalf("lifecycle sequence = %v, want %s", names, want)
	}
	if got := payloads["tengu_resume_print"][0]; got != `{"subscription_type":"pro"}` {
		t.Fatalf("resume_print payload = %s", got)
	}
	worker, errWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	chain, _ := sdkQueryLineage(worker, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: promptID})
	if got := payloads["tengu_tool_use_progress"][0]; got != `{"subscription_type":"pro","cc_prompt_id":"`+promptID+`","messageID":"msg_01","toolName":"TaskOutput","isMcp":false,"queryChainId":"`+chain+`","queryDepth":0}` {
		t.Fatalf("progress payload = %s", got)
	}
	if got := payloads["tengu_tool_use_progress"][1]; !strings.HasSuffix(got, `"messageID":"msg_02","toolName":"TaskOutput","isMcp":false}`) {
		t.Fatalf("child progress payload = %s", got)
	}
	if got := payloads["tengu_feature_bad"][0]; !strings.HasSuffix(got, `"feature_name":"tool_task_stop","error_code":"tool_call_threw"}`) {
		t.Fatalf("feature_bad payload = %s", got)
	}
	if got := payloads["tengu_cache_eviction_hint"]; len(got) != 1 || !strings.HasSuffix(got[0], `"scope":"subagent_end","last_request_id":"req_011CVGjJHiE6NiHqyMcgEuUx"}`) {
		t.Fatalf("cache eviction payloads = %v", got)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		executable[pair.EndpointRole+"/"+pair.EventName] = pair.Executable
	}
	for _, key := range []string{"sdk-event-logging/tengu_tool_use_progress", "sdk-event-logging/tengu_cache_eviction_hint", "sdk-event-logging/tengu_feature_bad", "sdk-event-logging/tengu_resume_print", datadogLogsRole + "/tengu_feature_bad"} {
		if !executable[key] {
			t.Fatalf("%s is not counted as executable: %+v", key, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d gaps=%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.ObservableEndpointEventCount-coverage.LiveEndpointEventCount)
}
