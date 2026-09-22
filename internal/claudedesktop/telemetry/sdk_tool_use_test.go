package telemetry

import (
	"bytes"
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

// jsonKeysInOrder returns the top-level property names of an object in
// document order.
func jsonKeysInOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("metadata is not an object: %s", raw)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, token.(string))
		var discard json.RawMessage
		if err := decoder.Decode(&discard); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// assertNativeKeyOrder checks that the emitted keys (after the logger prefix)
// appear in the golden literal's member order, skipping omitted members.
func assertNativeKeyOrder(t *testing.T, label string, emitted, golden []string) {
	t.Helper()
	cursor := 0
	for _, key := range emitted {
		found := false
		for cursor < len(golden) {
			member := golden[cursor]
			cursor++
			if member == key || strings.Contains(member, key+":") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: key %q is not in native order; emitted=%v golden=%v", label, key, emitted, golden)
		}
	}
}

func TestToolUseMetadataFollowsNativeOrderAndBuilders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-tool-use-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Events map[string]struct {
			Keys []string `json:"keys"`
		} `json:"events"`
		FeatureNameCases []struct {
			Name        string `json:"name"`
			FeatureName string `json:"feature_name"`
		} `json:"feature_name_cases"`
		IDCases []struct {
			ID       *string `json:"id"`
			Defined  bool    `json:"defined"`
			Loggable *string `json:"loggable"`
		} `json:"id_cases"`
		InputSizeCases []struct {
			JSON               string `json:"json"`
			ToolInputSizeBytes int    `json:"toolInputSizeBytes"`
		} `json:"input_size_cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for _, c := range golden.FeatureNameCases {
		// The golden's re-created word split mis-derives digit boundaries
		// (Agent2); lodash yields agent_2, which this implementation follows.
		if c.Name == "Agent2" {
			continue
		}
		if got := SDKToolFeatureName(c.Name); got != c.FeatureName {
			t.Fatalf("feature name %s = %q, want %q", c.Name, got, c.FeatureName)
		}
	}
	if got := SDKToolFeatureName("Agent2"); got != "tool_agent_2" {
		t.Fatalf("lodash digit boundary lost: %q", got)
	}
	for _, c := range golden.IDCases {
		if !c.Defined || c.ID == nil {
			continue
		}
		want := ""
		if c.Loggable != nil {
			want = *c.Loggable
		}
		if got := SDKLoggableID(*c.ID); got != want {
			t.Fatalf("loggable id %q = %q, want %q", *c.ID, got, want)
		}
	}
	for _, c := range golden.InputSizeCases {
		if got := SDKToolInputSizeBytes(json.RawMessage(c.JSON)); got != c.ToolInputSizeBytes {
			t.Fatalf("input size of %s = %d, want %d", c.JSON, got, c.ToolInputSizeBytes)
		}
	}
	if SDKToolInputSizeBytes(json.RawMessage(" {\"task_id\" : \"a1\"} ")) != len(`{"task_id":"a1"}`) {
		t.Fatal("input size must measure the compact JSON text")
	}
	if SDKLoggableToolName("mcp__srv__tool") != "mcp_tool" || SDKLoggableToolName("TaskOutput") != "TaskOutput" {
		t.Fatal("loggable tool name mapping lost")
	}
	if SDKToolResultSizeBytes(json.RawMessage(`{"type":"tool_result","tool_use_id":"t","content":"abc😀"}`)) != 5 {
		t.Fatal("string content size must count UTF-16 units")
	}
	if SDKToolResultSizeBytes(json.RawMessage(`{"type":"tool_result","tool_use_id":"t","content":[{"type":"tool_reference","tool_name":"SendMessage"}]}`)) != len(`[{"type":"tool_reference","tool_name":"SendMessage"}]`) {
		t.Fatal("block content size must be the JSON.stringify length")
	}
	depth := 0
	success, _ := json.Marshal(sdkToolUseSuccessMetadata{SubscriptionType: "pro", PromptID: "p", MessageID: "msg_1", ToolName: "TaskStop", DurationMs: 3, ToolResultSizeBytes: 10, ToolInputSizeBytes: 16, QueryChainID: "c", QueryDepth: &depth})
	assertNativeKeyOrder(t, "tengu_tool_use_success", jsonKeysInOrder(t, success)[2:], golden.Events["tool_use_success"].Keys)
	allowed, _ := json.Marshal(sdkToolUseCanUseToolAllowedMetadata{SubscriptionType: "pro", PromptID: "p", MessageID: "msg_1", ToolName: "TaskStop", QueryChainID: "c", QueryDepth: &depth})
	assertNativeKeyOrder(t, "tengu_tool_use_can_use_tool_allowed", jsonKeysInOrder(t, allowed)[2:], golden.Events["tool_use_can_use_tool_allowed"].Keys)
	sad, _ := json.Marshal(sdkFeatureMetadata{SubscriptionType: "pro", PromptID: "p", FeatureName: "tool_task_stop", ErrorCode: ToolFeatureErrorValidateInputRejected})
	if string(sad) != `{"subscription_type":"pro","cc_prompt_id":"p","feature_name":"tool_task_stop","error_code":"tool_validate_input_rejected"}` {
		t.Fatalf("feature_sad metadata = %s", sad)
	}
	ok, _ := json.Marshal(sdkFeatureMetadata{SubscriptionType: "pro", PromptID: "p", FeatureName: "tool_agent"})
	if string(ok) != `{"subscription_type":"pro","cc_prompt_id":"p","feature_name":"tool_agent"}` {
		t.Fatalf("feature_ok metadata = %s", ok)
	}
}

func toolUseTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(b *claudeprofile.Bundle) {
		b.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		for fact, name := range map[string]string{FactSDKToolUseCanUseToolAllowed: "tengu_tool_use_can_use_tool_allowed", FactSDKToolUseSuccess: "tengu_tool_use_success", FactSDKFeatureOk: "tengu_feature_ok", FactSDKFeatureSad: "tengu_feature_sad"} {
			b.SDKTelemetry.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
		}
		if b.AuxiliaryTelemetry.DatadogLogs.Events == nil {
			b.AuxiliaryTelemetry.DatadogLogs.Events = map[string]claudeprofile.TelemetryEventProfile{}
		}
		for fact, name := range map[string]string{FactSDKToolUseSuccess: "tengu_tool_use_success", FactSDKFeatureOk: "tengu_feature_ok", FactSDKFeatureSad: "tengu_feature_sad"} {
			b.AuxiliaryTelemetry.DatadogLogs.Events[fact] = claudeprofile.TelemetryEventProfile{EventName: name}
		}
	})
}

func TestToolUseEventsAreDeliveredInNativeSequence(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := toolUseTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	session, promptID, model := uuid.NewString(), uuid.NewString(), "claude-opus-5"
	input := json.RawMessage(`{"task_id":"a1"}`)
	success := ToolUseOutcome{ToolUse: ToolUse{MessageID: "msg_01", ToolName: "TaskStop", Input: input}, Success: true, DurationMs: 12, ResultSizeBytes: 90}
	if err := m.RecordSDKToolUse(t.Context(), auth, session, model, promptID, success); err != nil {
		t.Fatal(err)
	}
	rejected := ToolUseOutcome{ToolUse: ToolUse{MessageID: "msg_02", ToolName: "TaskOutput", Input: input, Depth: 1}, ValidationRejected: true, DurationMs: 1}
	if err := m.RecordSDKToolUse(t.Context(), auth, session, model, promptID, rejected); err != nil {
		t.Fatal(err)
	}
	thrown := ToolUseOutcome{ToolUse: ToolUse{MessageID: "msg_03", ToolName: "SendMessage", Input: input, Depth: 1}, DurationMs: 1}
	if err := m.RecordSDKToolUse(t.Context(), auth, session, model, promptID, thrown); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKToolUse(t.Context(), auth, session, "claude-unknown-model", promptID, success); err == nil {
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
			if !strings.HasPrefix(data.EventName, "tengu_tool_use_") && !strings.HasPrefix(data.EventName, "tengu_feature_") {
				continue
			}
			names = append(names, data.EventName)
			if data.SessionID != session || data.Model != model || data.Betas != betas || data.UserType != "external" {
				t.Fatalf("borrowed dimensions on %s: %+v", data.EventName, data)
			}
			raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(raw), `{"subscription_type":"pro","cc_prompt_id":"`+promptID+`",`) {
				t.Fatalf("%s metadata lacks the native logger dimensions: %s", data.EventName, raw)
			}
			payloads[data.EventName] = append(payloads[data.EventName], string(raw))
		}
	}
	want := "tengu_tool_use_can_use_tool_allowed,tengu_feature_ok,tengu_tool_use_success,tengu_feature_sad,tengu_tool_use_can_use_tool_allowed"
	if strings.Join(names, ",") != want {
		t.Fatalf("tool use sequence = %v, want %s", names, want)
	}
	successPayload := payloads["tengu_tool_use_success"][0]
	if !strings.Contains(successPayload, `"messageID":"msg_01","toolName":"TaskStop","isMcp":false,"durationMs":12,"preToolHookDurationMs":0,"permissionDurationMs":0,"toolResultSizeBytes":90,"toolInputSizeBytes":16,"queryChainId":"`) || !strings.HasSuffix(successPayload, `","queryDepth":0}`) {
		t.Fatalf("success payload = %s", successPayload)
	}
	// Depth-0 lineage equals the main request span's derivation.
	worker, errWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	chain, depth := sdkQueryLineage(worker, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: promptID})
	if chain == "" || depth == nil || !strings.Contains(successPayload, `"queryChainId":"`+chain+`"`) {
		t.Fatalf("query lineage mismatch: chain=%q payload=%s", chain, successPayload)
	}
	if got := payloads["tengu_feature_sad"][0]; !strings.HasSuffix(got, `"feature_name":"tool_task_output","error_code":"tool_validate_input_rejected"}`) {
		t.Fatalf("feature_sad payload = %s", got)
	}
	if got := payloads["tengu_feature_ok"][0]; !strings.HasSuffix(got, `"feature_name":"tool_task_stop"}`) {
		t.Fatalf("feature_ok payload = %s", got)
	}
	// Depth>0 callers omit the lineage keys.
	if got := payloads["tengu_tool_use_can_use_tool_allowed"][1]; !strings.HasSuffix(got, `"messageID":"msg_03","toolName":"SendMessage"}`) {
		t.Fatalf("child allow payload = %s", got)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" || pair.EndpointRole == datadogLogsRole {
			executable[pair.EndpointRole+"/"+pair.EventName] = pair.Executable
		}
	}
	for _, key := range []string{"sdk-event-logging/tengu_tool_use_can_use_tool_allowed", "sdk-event-logging/tengu_tool_use_success", "sdk-event-logging/tengu_feature_ok", "sdk-event-logging/tengu_feature_sad", datadogLogsRole + "/tengu_tool_use_success", datadogLogsRole + "/tengu_feature_ok", datadogLogsRole + "/tengu_feature_sad"} {
		if !executable[key] {
			t.Fatalf("%s is not counted as executable: %+v", key, coverage.UnverifiedDeclaredEventNames)
		}
	}
	t.Logf("coverage names=%d endpoint_events=%d/%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount)
}
