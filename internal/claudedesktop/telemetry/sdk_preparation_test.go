package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func newSDKPreparationTestManager(t *testing.T, clock *testClock, doer *testDoer) *Manager {
	t.Helper()
	return newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		// These tests start after input submission and assert preparation or
		// response facts only. The input contract has its own full-boundary suite.
		delete(bundle.SDKTelemetry.Events, FactSDKInput)
		bundle.SDKTelemetry.InputBetas = nil
		// These tests assert a manually flushed preparation sequence. Keep the
		// production timer out of the assertion while filesystem writes run.
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Batch.JitterMinimum = 1
		bundle.SDKTelemetry.Batch.JitterMaximum = 1
	})
}

func TestSDKPreparationUsesWireFactsAndUnsignedBillingHash(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newSDKPreparationTestManager(t, clock, doer)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Role = claudeprofile.RoleMain
	facts.PreviousRequestID = "req_previous"
	span := manager.BeginRequest(context.Background(), auth, facts)
	const billing = "x-anthropic-billing-header: cc_version=2.1.247; cc_entrypoint=claude-desktop; cch=abcde;"
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-5", "messages": []any{map[string]any{"role": "user", "content": "PRIVATE_PROMPT"}},
		"system":   []any{map[string]any{"type": "text", "text": billing}, map[string]any{"type": "text", "text": "PRIVATE_SYSTEM"}},
		"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"}, "speed": "fast",
	})
	headers := http.Header{"Anthropic-Beta": {"captured-beta"}, "Cookie": {"PRIVATE_COOKIE"}}
	span.ObserveRequest(body, headers)
	span.ObserveRequest(body, headers)
	span.ObserveResponse("req_test", "end_turn")
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
			continue
		}
		names := capturedSDKEventNames(t, []recordedRequest{request})
		if names["tengu_api_query"] != 1 || names["tengu_sysprompt_block"] != 1 || names["tengu_api_after_normalize"] != 1 {
			t.Fatalf("preparation event counts: %v", names)
		}
		query := sdkMetadataForEvent(t, request.Body, "tengu_api_query")
		normalized := sdkMetadataForEvent(t, request.Body, "tengu_api_after_normalize")
		if normalized["postNormalizedMessageCount"] != float64(1) || normalized["apiSystemMessageCount"] != float64(0) {
			t.Fatalf("top-level system blocks counted as system messages: %v", normalized)
		}
		if strings.Index(string(request.Body), "tengu_api_after_normalize") > strings.Index(string(request.Body), "tengu_sysprompt_block") {
			t.Fatal("normalization event must precede system-block/cache/query events")
		}
		for key, want := range map[string]any{"model": "claude-sonnet-5", "messagesLength": float64(1), "temperature": float64(1), "thinkingType": "adaptive", "effortValue": "high", "querySource": "sdk", "fastMode": true, "betas": "captured-beta", "previousRequestId": "req_previous"} {
			if query[key] != want {
				t.Errorf("%s = %#v, want %#v", key, query[key], want)
			}
		}
		block := sdkMetadataForEvent(t, request.Body, "tengu_sysprompt_block")
		digest := sha256.Sum256([]byte(strings.Replace(billing, "cch=abcde;", "cch=00000;", 1)))
		if block["hash"] != hex.EncodeToString(digest[:]) || block["length"] != float64(len(billing)) {
			t.Fatalf("unsigned billing projection: %v", block)
		}
		for _, metadata := range []map[string]any{query, block, normalized} {
			encoded, _ := json.Marshal(metadata)
			if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), "x-anthropic-billing-header") {
				t.Fatal("preparation telemetry leaked request content")
			}
		}
		return
	}
	t.Fatal("SDK batch missing")
}

func TestSDKPreparationRoleAndRetryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		role       claudeprofile.RequestRole
		attempt    int
		wantQuery  bool
		wantPrompt bool
	}{
		{claudeprofile.RoleMain, 1, true, true}, {claudeprofile.RoleTitle, 1, true, false},
		{claudeprofile.RoleLightHelper, 1, true, true}, {claudeprofile.RoleSubagent, 1, true, true},
		{claudeprofile.RoleSecurityMonitor, 1, false, false}, {claudeprofile.RoleCountTokens, 1, false, false},
		{claudeprofile.RoleMain, 2, false, false},
	} {
		t.Run(string(tc.role)+string(rune('0'+tc.attempt)), func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
			facts.Role = tc.role
			facts.Attempt = tc.attempt
			span := manager.BeginRequest(context.Background(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
			span.ObserveRequest([]byte(`{"model":"claude-sonnet-5","messages":[],"system":"PRIVATE_SYSTEM"}`), nil)
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			var query map[string]any
			for _, request := range doer.Requests() {
				if strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
					names := capturedSDKEventNames(t, []recordedRequest{request})
					if names["tengu_api_query"] > 0 {
						query = sdkMetadataForEvent(t, request.Body, "tengu_api_query")
					}
					if (names["tengu_api_after_normalize"] > 0) != tc.wantQuery {
						t.Fatalf("normalization role/retry boundary: %v", names)
					}
					if names["tengu_api_after_normalize"] > 0 {
						normalized := sdkMetadataForEvent(t, request.Body, "tengu_api_after_normalize")
						_, hasPrompt := normalized["cc_prompt_id"]
						if hasPrompt != tc.wantPrompt {
							t.Fatalf("normalization prompt scope: %v", normalized)
						}
					}
					if names["tengu_sysprompt_block"] > 0 {
						t.Fatal("arbitrary caller system became a billing event")
					}
				}
			}
			if (query != nil) != tc.wantQuery {
				t.Fatalf("query present=%t want=%t", query != nil, tc.wantQuery)
			}
			if query != nil {
				_, hasPrompt := query["cc_prompt_id"]
				if hasPrompt != tc.wantPrompt {
					t.Fatalf("prompt scope=%t want=%t", hasPrompt, tc.wantPrompt)
				}
			}
		})
	}
}

func TestSDKAfterNormalizeCountsMidConversationSystemMessages(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newSDKPreparationTestManager(t, clock, doer)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	span := manager.BeginRequest(context.Background(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
	span.ObserveRequest([]byte(`{"model":"claude-sonnet-5","system":[{"type":"text","text":"PRIVATE_TOP"}],"messages":[{"role":"user","content":"PRIVATE_USER"},{"role":"system","content":[{"type":"text","text":"PRIVATE_CARRIER"}]},{"role":"assistant","content":"PRIVATE_REPLY"},{"role":"system","content":"PRIVATE_REMINDER"}]}`), nil)
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	found := false
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
			continue
		}
		if capturedSDKEventNames(t, []recordedRequest{request})["tengu_api_after_normalize"] == 0 {
			continue
		}
		value := sdkMetadataForEvent(t, request.Body, "tengu_api_after_normalize")
		if value["postNormalizedMessageCount"] != float64(4) || value["apiSystemMessageCount"] != float64(2) || len(value) != 4 {
			t.Fatalf("normalization fields = %v", value)
		}
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "PRIVATE_") {
			t.Fatal("message content leaked")
		}
		found = true
	}
	if !found {
		t.Fatal("normalization event missing")
	}
}

func TestSDKSystemBoundaryLayoutFieldsAndOrdering(t *testing.T) {
	for _, tc := range []struct {
		blocks  int
		role    claudeprofile.RequestRole
		attempt int
		want    string
	}{
		{4, claudeprofile.RoleMain, 1, "tengu_sysprompt_boundary_found"},
		{3, claudeprofile.RoleMain, 1, "tengu_sysprompt_missing_boundary_marker"},
		{3, claudeprofile.RoleTitle, 1, "tengu_sysprompt_missing_boundary_marker"},
		{4, claudeprofile.RoleSubagent, 1, "tengu_sysprompt_boundary_found"},
		{2, claudeprofile.RoleMain, 1, ""},
		{5, claudeprofile.RoleMain, 1, ""},
		{4, claudeprofile.RoleMain, 2, ""},
		{4, claudeprofile.RoleSecurityMonitor, 1, ""},
	} {
		t.Run(fmt.Sprintf("%s-%d-%d", tc.role, tc.blocks, tc.attempt), func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
			facts.Role, facts.Attempt = tc.role, tc.attempt
			texts := []string{"x-anthropic-billing-header: cch=aaaaa;", "PRIVATE_HARNESS", "PRIVATE_STATIC_😀", "PRIVATE_DYNAMIC_😀😀", "PRIVATE_UNKNOWN"}
			blocks := make([]map[string]string, 0, tc.blocks)
			for _, text := range texts[:tc.blocks] {
				blocks = append(blocks, map[string]string{"type": "text", "text": text})
			}
			body, _ := json.Marshal(map[string]any{"model": facts.Model, "messages": []any{}, "system": blocks})
			span := manager.BeginRequest(context.Background(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
			span.ObserveRequest(body, nil)
			span.ObserveRequest(body, nil)
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, request := range doer.Requests() {
				if !strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
					continue
				}
				names := capturedSDKEventNames(t, []recordedRequest{request})
				if tc.want == "" {
					if names["tengu_sysprompt_boundary_found"] != 0 || names["tengu_sysprompt_missing_boundary_marker"] != 0 {
						t.Fatalf("unobserved boundary emitted: %v", names)
					}
					continue
				}
				if names[tc.want] != 2 {
					t.Fatalf("boundary evaluations = %v", names)
				}
				metadata := sdkMetadataForEvent(t, request.Body, tc.want)
				if _, hasPrompt := metadata["cc_prompt_id"]; hasPrompt != (tc.role != claudeprofile.RoleTitle) {
					t.Fatal("boundary prompt identity escaped role scope")
				}
				if tc.blocks == 4 {
					if metadata["blockCount"] != float64(4) || metadata["staticBlockLength"] != float64(javascriptUTF16Length(texts[2])) || metadata["dynamicBlockLength"] != float64(javascriptUTF16Length(texts[3])) {
						t.Fatalf("boundary lengths: %v", metadata)
					}
				} else if metadata["promptBlockCount"] != float64(3) {
					t.Fatalf("missing-boundary fields: %v", metadata)
				}
				encoded, _ := json.Marshal(metadata)
				if strings.Contains(string(encoded), "PRIVATE_") {
					t.Fatal("boundary leaked system text")
				}
				raw := string(request.Body)
				before, after, billing := strings.Index(raw, tc.want), strings.LastIndex(raw, tc.want), strings.Index(raw, "tengu_sysprompt_block")
				if before >= billing || after <= billing || after >= strings.Index(raw, "tengu_api_cache_breakpoints") {
					t.Fatal("boundary/billing/cache sequence drifted")
				}
				found = true
			}
			if tc.want != "" && !found {
				t.Fatal("SDK boundary batch missing")
			}
		})
	}
}

func TestSDKQueryLineageIsPromptScopedAndDoesNotGuessHelperDepth(t *testing.T) {
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	first, depth := sdkQueryLineage(nil, facts)
	if first == "" || depth == nil || *depth != 0 {
		t.Fatal("main prompt did not start a query chain")
	}
	facts.Attempt = 5
	retry, _ := sdkQueryLineage(nil, facts)
	if retry != first {
		t.Fatal("retry changed query chain")
	}
	facts.PromptID = "88888888-8888-4888-8888-888888888888"
	next, _ := sdkQueryLineage(nil, facts)
	if next == first {
		t.Fatal("different prompt reused session-wide query chain")
	}
	facts.Role = claudeprofile.RoleLightHelper
	if chain, d := sdkQueryLineage(nil, facts); chain != "" || d != nil {
		t.Fatal("helper role fabricated a parent/depth")
	}
	facts.QueryChainID = first
	facts.QueryDepth = intPointer(4)
	if chain, d := sdkQueryLineage(nil, facts); chain != first || d == nil || *d != 4 {
		t.Fatal("explicit observed query lineage was not retained")
	}
}
