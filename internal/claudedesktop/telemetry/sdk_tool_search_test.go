package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestToolSearchModeDecisionMetadataMatchesNativeShape(t *testing.T) {
	metadata := toolSearchModeDecisionMetadata("pro", "prompt-1", ToolSearchModeDecision{
		Enabled: true, Mode: ToolSearchModeTST, Reason: ToolSearchReasonTSTEnabled, CheckedModel: "claude-opus-5", MCPNonBlocking: true,
	})
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"subscription_type":"pro","cc_prompt_id":"prompt-1","enabled":true,"mode":"tst","reason":"tst_enabled","checkedModel":"claude-opus-5","mcpToolCount":0,"mcpNonBlocking":true,"userType":"external"}`
	if string(raw) != want {
		t.Fatalf("mode decision metadata = %s", raw)
	}
	raw, err = json.Marshal(toolSearchModeDecisionMetadata("", "", ToolSearchModeDecision{Mode: ToolSearchModeStandard, Reason: ToolSearchReasonModelUnsupported, CheckedModel: "claude-3-5-haiku-20241022", MCPNonBlocking: true}))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"enabled":false,"mode":"standard","reason":"model_unsupported","checkedModel":"claude-3-5-haiku-20241022","mcpToolCount":0,"mcpNonBlocking":true,"userType":"external"}`
	if string(raw) != want {
		t.Fatalf("disabled mode decision metadata = %s", raw)
	}
}

func TestDeferredToolsPoolChangeMetadataMatchesNativeShape(t *testing.T) {
	metadata := deferredToolsPoolChangeMetadata("pro", "prompt-1", DeferredToolsPoolChange{
		AddedCount: 3, UnlistedCount: 3, MessagesLength: 1, AttachmentCount: 2, CallSite: DeferredToolsCallSiteMain, QuerySource: "sdk",
		AttachmentTypesSeen: []string{"total_tokens_reminder", "hook_success", "hook_success"},
	})
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"subscription_type":"pro","cc_prompt_id":"prompt-1","addedCount":3,"readdedCount":0,"unlistedCount":3,"removedCount":0,"pendingChanged":false,"pendingCount":0,"lastPendingCount":0,"needsAuthChanged":false,"needsAuthCount":0,"lastNeedsAuthCount":0,"failedChanged":false,"failedCount":0,"lastFailedCount":0,"priorAnnouncedCount":0,"messagesLength":1,"attachmentCount":2,"dtdCount":0,"callSite":"attachments_main","querySource":"sdk","attachmentTypesSeen":"hook_success,total_tokens_reminder"}`
	if string(raw) != want {
		t.Fatalf("pool change metadata = %s", raw)
	}
	fallback := deferredToolsPoolChangeMetadata("pro", "", DeferredToolsPoolChange{})
	if fallback.CallSite != "unknown" || fallback.QuerySource != "unknown" || fallback.AttachmentTypesSeen != "" {
		t.Fatalf("native fallbacks lost: %+v", fallback)
	}
}

func TestToolSearchOutcomeMetadataMatchesNativeShape(t *testing.T) {
	selectOutcome := ToolSearchOutcomeFromQuery("select:SendMessage, TaskStop", 5, 2, 3)
	raw, err := json.Marshal(toolSearchOutcomeMetadata("pro", "prompt-1", selectOutcome))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"subscription_type":"pro","cc_prompt_id":"prompt-1","queryLength":28,"querySelectCount":2,"queryType":"select","matchCount":2,"totalDeferredTools":3,"maxResults":5,"hasMatches":true,"mcpServersConfigured":0,"mcpServersConnected":0,"mcpServersCached":0,"mcpServersPending":0,"mcpToolsInPool":0}`
	if string(raw) != want {
		t.Fatalf("select outcome metadata = %s", raw)
	}
	keyword := ToolSearchOutcomeFromQuery("nothing here", 5, 0, 3)
	raw, err = json.Marshal(toolSearchOutcomeMetadata("pro", "prompt-1", keyword))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"subscription_type":"pro","cc_prompt_id":"prompt-1","queryLength":12,"queryType":"keyword","matchCount":0,"totalDeferredTools":3,"maxResults":5,"hasMatches":false,"mcpServersConfigured":0,"mcpServersConnected":0,"mcpServersCached":0,"mcpServersPending":0,"mcpToolsInPool":0}`
	if string(raw) != want {
		t.Fatalf("keyword outcome metadata = %s", raw)
	}
	// The select regex is case-insensitive and queryLength counts UTF-16 units.
	upper := ToolSearchOutcomeFromQuery("SELECT:Agent", 1, 1, 3)
	if upper.QueryType != ToolSearchQueryTypeSelect || upper.QuerySelectCount != 1 {
		t.Fatalf("case-insensitive select lost: %+v", upper)
	}
	if astral := ToolSearchOutcomeFromQuery("😀 tool", 5, 0, 3); astral.QueryLength != 7 || astral.QueryType != ToolSearchQueryTypeKeyword {
		t.Fatalf("JavaScript string length lost: %+v", astral)
	}
	if bare := ToolSearchOutcomeFromQuery("select:", 5, 0, 3); bare.QueryType != ToolSearchQueryTypeKeyword {
		t.Fatalf("empty select capture must fall through to keyword: %+v", bare)
	}
}

func TestToolSearchEventsAreDeliveredWithOwnedDimensions(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := bridgeTestManager(t, clock, doer)
	beforeCoverage := m.Status().LiveEmitterCoverage
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.Metadata["subscription_type"] = "pro"
	session, promptID, model := uuid.NewString(), uuid.NewString(), "claude-opus-5"
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleSubagent, SessionID: session, PromptID: promptID, Model: model})
	if !span.Active() {
		t.Fatal("subagent span inactive")
	}
	span.ObserveToolSearchModeDecision(ToolSearchModeDecision{Enabled: true, Mode: ToolSearchModeTST, Reason: ToolSearchReasonTSTEnabled, CheckedModel: model, MCPNonBlocking: true})
	span.ObserveDeferredToolsPoolChange(DeferredToolsPoolChange{AddedCount: 3, UnlistedCount: 3, MessagesLength: 1, CallSite: DeferredToolsCallSiteSubagent, QuerySource: "sdk"})
	if err := m.RecordSDKToolSearchOutcome(t.Context(), auth, session, model, promptID, ToolSearchOutcomeFromQuery("select:SendMessage", 5, 1, 3)); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSDKToolSearchOutcome(t.Context(), auth, session, "claude-unknown-model", promptID, ToolSearchOutcome{}); err == nil {
		t.Fatal("unprofiled model must not invent betas")
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
			if !strings.HasPrefix(data.EventName, "tengu_tool_search_") && data.EventName != "tengu_deferred_tools_pool_change" {
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
			switch data.EventName {
			case "tengu_tool_search_mode_decision":
				if !strings.HasSuffix(string(raw), `"enabled":true,"mode":"tst","reason":"tst_enabled","checkedModel":"claude-opus-5","mcpToolCount":0,"mcpNonBlocking":true,"userType":"external"}`) {
					t.Fatalf("mode decision payload = %s", raw)
				}
			case "tengu_deferred_tools_pool_change":
				if !strings.Contains(string(raw), `"callSite":"attachments_subagent","querySource":"sdk","attachmentTypesSeen":""}`) {
					t.Fatalf("pool change payload = %s", raw)
				}
			case "tengu_tool_search_outcome":
				if !strings.Contains(string(raw), `"queryLength":18,"querySelectCount":1,"queryType":"select","matchCount":1,"totalDeferredTools":3,"maxResults":5,"hasMatches":true,`) {
					t.Fatalf("outcome payload = %s", raw)
				}
			}
		}
	}
	if strings.Join(names, ",") != "tengu_tool_search_mode_decision,tengu_deferred_tools_pool_change,tengu_tool_search_outcome" {
		t.Fatalf("lost, duplicate or reordered tool search events: %v", names)
	}
	coverage := m.Status().LiveEmitterCoverage
	executable := map[string]bool{}
	for _, pair := range coverage.EndpointEvents {
		if pair.EndpointRole == "sdk-event-logging" {
			executable[pair.EventName] = pair.Executable
		}
	}
	for _, name := range []string{"tengu_tool_search_mode_decision", "tengu_deferred_tools_pool_change", "tengu_tool_search_outcome"} {
		if !executable[name] {
			t.Fatalf("%s is captured but not counted as executable: %+v", name, coverage.UnverifiedDeclaredEventNames)
		}
	}
	if coverage.ObservableEndpointEventCount != 303 || coverage.LiveEndpointEventCount != beforeCoverage.LiveEndpointEventCount || coverage.LiveEventNameCount != beforeCoverage.LiveEventNameCount {
		t.Fatalf("tool search registration moved coverage unexpectedly: %d/%d names=%d", coverage.LiveEndpointEventCount, coverage.ObservableEndpointEventCount, coverage.LiveEventNameCount)
	}
}

func TestToolSearchEventsIgnoreFinishedOrInactiveSpans(t *testing.T) {
	var inactive *RequestSpan
	inactive.ObserveToolSearchModeDecision(ToolSearchModeDecision{})
	(&RequestSpan{}).ObserveDeferredToolsPoolChange(DeferredToolsPoolChange{})
	clock := &testClock{now: time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	m := bridgeTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := m.BeginRequest(t.Context(), auth, RequestFacts{Role: claudeprofile.RoleSubagent, SessionID: uuid.NewString(), PromptID: uuid.NewString(), Model: "claude-opus-5"})
	span.mu.Lock()
	span.finished = true
	span.mu.Unlock()
	span.ObserveToolSearchOutcome(ToolSearchOutcome{})
	if err := m.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, request := range doer.Requests() {
		if strings.Contains(string(request.Body), "tengu_tool_search") {
			t.Fatal("finished span emitted a tool search event")
		}
	}
}
