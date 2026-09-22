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

// queryBuildGolden is the audit output of
// audit-sdk-telemetry-query-build-source.mjs (native key order per event).
type queryBuildGolden struct {
	Contracts []struct {
		Event   string   `json:"event"`
		Gateway string   `json:"gateway"`
		Keys    []string `json:"keys"`
	} `json:"contracts"`
	LoopCases []struct {
		Label  string `json:"label"`
		Events []struct {
			Name     string          `json:"name"`
			Metadata json.RawMessage `json:"metadata"`
		} `json:"events"`
	} `json:"loop_cases"`
}

func loadQueryBuildGolden(t *testing.T) queryBuildGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sdk-telemetry-query-build-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden queryBuildGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func queryBuildMetadataKeys(t *testing.T, value any) []string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("metadata is not an object: %s", raw)
	}
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

func TestQueryBuildMetadataKeyOrderMatchesNativeGolden(t *testing.T) {
	golden := loadQueryBuildGolden(t)
	depth := 0
	loop := QueryLoopAttachments{MessagesForQueryCount: 2, AssistantMessagesCount: 2, ToolResultsCount: 2, AttachmentTypes: []string{AttachmentTypeDeferredToolsDelta}}
	built := map[string]any{
		"tengu_query_before_attachments": queryBeforeAttachmentsMetadata("", "", loop, "chain-1", &depth),
		"tengu_attachments":              attachmentsMetadata("", "", loop.AttachmentTypes),
		"tengu_query_after_attachments":  queryAfterAttachmentsMetadata("", "", loop, "chain-1", &depth),
		"tengu_api_before_normalize":     apiBeforeNormalizeMetadata("", "", 7),
	}
	executable := 0
	for _, contract := range golden.Contracts {
		value, ok := built[contract.Event]
		if contract.Gateway != "executable" {
			if ok {
				t.Fatalf("%s is a native boundary but has an emitter", contract.Event)
			}
			continue
		}
		executable++
		if !ok {
			t.Fatalf("executable contract %s has no emitter", contract.Event)
		}
		if got := queryBuildMetadataKeys(t, value); strings.Join(got, ",") != strings.Join(contract.Keys, ",") {
			t.Fatalf("%s keys = %v, want native %v", contract.Event, got, contract.Keys)
		}
	}
	if executable != 4 {
		t.Fatalf("golden declares %d executable contracts, want 4", executable)
	}
	// The vm-executed loop case two_tool_results_with_reminder is the shape
	// the emitters reproduce byte for byte (without the logger prefix).
	for _, loopCase := range golden.LoopCases {
		if loopCase.Label != "two_tool_results_with_reminder" {
			continue
		}
		for _, event := range loopCase.Events {
			raw, err := json.Marshal(built[event.Name])
			if err != nil {
				t.Fatal(err)
			}
			var native bytes.Buffer
			if err := json.Compact(&native, event.Metadata); err != nil {
				t.Fatal(err)
			}
			if string(raw) != native.String() {
				t.Fatalf("%s = %s, native %s", event.Name, raw, native.String())
			}
		}
	}
	// The logger prefix precedes the native keys; unknown lineage leaves
	// queryChainId/queryDepth undefined as tengu_api_query does.
	raw, err := json.Marshal(queryBeforeAttachmentsMetadata("pro", "prompt-1", loop, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"subscription_type":"pro","cc_prompt_id":"prompt-1","messagesForQueryCount":2,"assistantMessagesCount":2,"toolResultsCount":2}` {
		t.Fatalf("before attachments metadata = %s", raw)
	}
	raw, err = json.Marshal(attachmentsMetadata("pro", "prompt-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"subscription_type":"pro","cc_prompt_id":"prompt-1","attachment_types":[]}` {
		t.Fatalf("attachments metadata = %s", raw)
	}
}

func queryBuildTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Events[FactSDKQueryBeforeAttachments] = claudeprofile.TelemetryEventProfile{EventName: "tengu_query_before_attachments", RequiredFacts: []string{"owned_request", "messages_for_query_count", "assistant_messages_count", "tool_results_count", "session_default_betas"}}
		bundle.SDKTelemetry.Events[FactSDKAttachments] = claudeprofile.TelemetryEventProfile{EventName: "tengu_attachments", RequiredFacts: []string{"owned_request", "attachment_types", "session_default_betas"}}
		bundle.SDKTelemetry.Events[FactSDKQueryAfterAttachments] = claudeprofile.TelemetryEventProfile{EventName: "tengu_query_after_attachments", RequiredFacts: []string{"owned_request", "total_tool_results_count", "file_change_attachment_count", "session_default_betas"}}
		bundle.SDKTelemetry.Events[FactSDKAPIBeforeNormalize] = claudeprofile.TelemetryEventProfile{EventName: "tengu_api_before_normalize", RequiredFacts: []string{"owned_request", "pre_normalized_message_count", "session_default_betas"}}
	})
}

func TestQueryBuildEventsAreDeliveredInNativeOrder(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := queryBuildTestManager(t, clock, doer)
	beforeCoverage := m.Status().LiveEmitterCoverage
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	session, promptID, model := uuid.NewString(), uuid.NewString(), "claude-opus-5"
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: promptID, Model: model})
	if !span.Active() {
		t.Fatal("main span inactive")
	}
	span.ObserveQueryLoopAttachments(QueryLoopAttachments{MessagesForQueryCount: 2, AssistantMessagesCount: 2, ToolResultsCount: 2, AttachmentTypes: []string{AttachmentTypeDeferredToolsDelta}})
	span.ObserveAPIBeforeNormalize(7)
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
			case "tengu_query_before_attachments", "tengu_attachments", "tengu_query_after_attachments", "tengu_api_before_normalize":
			default:
				continue
			}
			names = append(names, data.EventName)
			if data.SessionID != session || data.Model != model || data.Betas != betas || data.UserType != "external" || data.Auth.AccountUUID != testAccountA {
				t.Fatalf("borrowed dimensions on %s: %+v", data.EventName, data)
			}
			raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
			if err != nil {
				t.Fatal(err)
			}
			prefix := `{"subscription_type":"pro","cc_prompt_id":"` + promptID + `",`
			if !strings.HasPrefix(string(raw), prefix) {
				t.Fatalf("%s metadata does not start with the native logger dimensions: %s", data.EventName, raw)
			}
			body := strings.TrimPrefix(string(raw), prefix)
			want := map[string]string{
				"tengu_query_before_attachments": `"messagesForQueryCount":2,"assistantMessagesCount":2,"toolResultsCount":2,"queryChainId":"`,
				"tengu_attachments":              `"attachment_types":["deferred_tools_delta"]}`,
				"tengu_query_after_attachments":  `"totalToolResultsCount":3,"fileChangeAttachmentCount":0,"queryChainId":"`,
				"tengu_api_before_normalize":     `"preNormalizedMessageCount":7}`,
			}[data.EventName]
			if !strings.HasPrefix(body, want) {
				t.Fatalf("%s payload = %s", data.EventName, raw)
			}
			if strings.Contains(want, "queryChainId") && !strings.HasSuffix(body, `,"queryDepth":0}`) {
				t.Fatalf("%s lineage lost: %s", data.EventName, raw)
			}
		}
	}
	if strings.Join(names, ",") != "tengu_query_before_attachments,tengu_attachments,tengu_query_after_attachments,tengu_api_before_normalize" {
		t.Fatalf("lost, duplicate or reordered query build events: %v", names)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" {
			executable[pair.EventName] = pair.Executable
		}
	}
	for _, name := range names {
		if !executable[name] {
			t.Fatalf("%s is delivered but not counted as executable: %+v", name, coverage.UnverifiedDeclaredEventNames)
		}
	}
	if coverage.ObservableEndpointEventCount != 303 || coverage.LiveEndpointEventCount != beforeCoverage.LiveEndpointEventCount || coverage.LiveEventNameCount != beforeCoverage.LiveEventNameCount {
		t.Fatalf("query build registration moved coverage unexpectedly: %d/%d names=%d", coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.LiveEventNameCount)
	}
	t.Logf("coverage names=%d endpoint_events=%d gaps=%d", coverage.LiveEventNameCount, coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount-coverage.LiveEndpointEventCount)
}

func TestQueryBuildEventsSkipAttachmentsWhenNoneProducedAndIgnoreInactiveSpans(t *testing.T) {
	var inactive *RequestSpan
	inactive.ObserveQueryLoopAttachments(QueryLoopAttachments{})
	(&RequestSpan{}).ObserveAPIBeforeNormalize(1)
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := queryBuildTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleSubagent, SessionID: uuid.NewString(), PromptID: uuid.NewString(), Model: "claude-opus-5"})
	span.ObserveQueryLoopAttachments(QueryLoopAttachments{MessagesForQueryCount: 1, AssistantMessagesCount: 1, ToolResultsCount: 1})
	span.mu.Lock()
	span.finished = true
	span.mu.Unlock()
	span.ObserveAPIBeforeNormalize(3)
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, request := range doer.Requests() {
		var batch struct {
			Events []sdkEventWrapper `json:"events"`
		}
		_ = json.Unmarshal(request.Body, &batch)
		for _, wrapper := range batch.Events {
			switch wrapper.EventData.EventName {
			case "tengu_query_before_attachments", "tengu_attachments", "tengu_query_after_attachments", "tengu_api_before_normalize":
				names = append(names, wrapper.EventData.EventName)
			}
		}
	}
	if strings.Join(names, ",") != "tengu_query_before_attachments,tengu_query_after_attachments" {
		t.Fatalf("attachments without production or a finished span leaked events: %v", names)
	}
}
