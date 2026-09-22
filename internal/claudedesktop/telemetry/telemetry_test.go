package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSDKEventLoggingUsesIndependentEndpointAuthAndSchema(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Batch.JitterMinimum = 1
		bundle.SDKTelemetry.Batch.JitterMaximum = 1
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.PreviousRequestID = "req_previous"
	span := manager.BeginRequest(context.Background(), auth, facts)
	const imageData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	imagePayload, errImage := base64.StdEncoding.DecodeString(imageData)
	if errImage != nil {
		t.Fatal(errImage)
	}
	body := []byte(fmt.Sprintf(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"PROMPT_SENTINEL"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"UERG"}}]}],"system":[{"type":"text","text":"SYSTEM_SENTINEL","cache_control":{"type":"ephemeral"}}],"tools":[],"output_config":{"effort":"high"},"speed":"fast"}`, imageData))
	headers := http.Header{
		"anthropic-beta": {"claude-code-20250219,oauth-2025-04-20"},
		"Cookie":         {"session=COOKIE_SENTINEL"},
		"X-Caller-Only":  {"CALLER_HEADER_SENTINEL"},
	}
	span.ObserveRequest(body, headers)
	span.ObserveFirstByte(clock.Now().Add(125 * time.Millisecond))
	span.ObserveUsage(Usage{InputTokens: 17, OutputTokens: 3, CacheCreationInputTokens: 5, CacheReadInputTokens: 7})
	thinkingContentLength := 7
	span.ObserveResponseContentMetrics("req_sdk_test", "end_turn", 12, &thinkingContentLength, map[string]int{"Read": 36})
	clock.Advance(time.Second)
	span.FinishSuccess(context.Background())

	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	requests := doer.Requests()
	if len(requests) != 2 {
		t.Fatalf("delivery requests = %d, want renderer and SDK requests", len(requests))
	}
	var renderer, sdk *recordedRequest
	for index := range requests {
		request := &requests[index]
		switch {
		case strings.HasPrefix(request.URL, "https://claude.ai/"):
			renderer = request
		case strings.HasPrefix(request.URL, "https://api.anthropic.com/"):
			sdk = request
		}
	}
	if renderer == nil || sdk == nil {
		t.Fatalf("endpoint split missing: %+v", requests)
	}
	if renderer.Header.Get("Authorization") != "" {
		t.Fatal("renderer event logger inherited OAuth authorization")
	}
	if renderer.Header.Get("Cookie") != "" || renderer.Header.Get("X-Caller-Only") != "" {
		t.Fatal("renderer event logger inherited caller request headers")
	}
	if got := sdk.Header.Get("Authorization"); got != "Bearer sensitive-access-token" {
		t.Fatalf("SDK Authorization = %q", got)
	}
	if sdk.Header.Get("Cookie") != "" || sdk.Header.Get("X-Caller-Only") != "" {
		t.Fatal("SDK event logger inherited caller request headers")
	}
	for name, want := range map[string]string{
		"Accept":          "application/json, text/plain, */*",
		"Content-Type":    "application/json",
		"User-Agent":      "claude-code/2.1.247",
		"x-service-name":  "claude-code",
		"anthropic-beta":  "oauth-2025-04-20",
		"Accept-Encoding": "gzip, compress, deflate, br",
		"Connection":      "close",
	} {
		if got := sdk.Header.Get(name); got != want {
			t.Fatalf("SDK %s = %q, want %q", name, got, want)
		}
	}
	var batch struct {
		Events []struct {
			EventType string `json:"event_type"`
			EventData struct {
				EventName          string            `json:"event_name"`
				Model              string            `json:"model"`
				SessionID          string            `json:"session_id"`
				Betas              string            `json:"betas"`
				Entrypoint         string            `json:"entrypoint"`
				AgentSDKVersion    string            `json:"agent_sdk_version"`
				ClientType         string            `json:"client_type"`
				Process            string            `json:"process"`
				AdditionalMetadata string            `json:"additional_metadata"`
				Auth               map[string]string `json:"auth"`
				DeviceID           string            `json:"device_id"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(sdk.Body, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(batch.Events) != 4 || batch.Events[0].EventData.EventName != "tengu_api_after_normalize" || batch.Events[1].EventData.EventName != "tengu_api_cache_breakpoints" || batch.Events[2].EventData.EventName != "tengu_api_query" || batch.Events[3].EventData.EventName != "tengu_api_success" {
		t.Fatalf("SDK events = %+v", batch.Events)
	}
	var successMetadata map[string]any
	for _, event := range batch.Events {
		if event.EventType != "ClaudeCodeInternalEvent" || event.EventData.Model != "claude-opus-5" || event.EventData.SessionID != facts.SessionID || event.EventData.Entrypoint != "claude-desktop" || event.EventData.AgentSDKVersion != "0.3.247" || event.EventData.ClientType != "claude-desktop" {
			t.Fatalf("SDK envelope mismatch: %+v", event.EventData)
		}
		if event.EventData.Auth["account_uuid"] != testAccountA || event.EventData.Auth["organization_uuid"] != testOrgA || len(event.EventData.DeviceID) != 64 {
			t.Fatalf("SDK identity mismatch: %+v", event.EventData)
		}
		if event.EventData.Process != "" {
			t.Fatalf("SDK event fabricated an embedded-Node process snapshot: %q", event.EventData.Process)
		}
		decoded, errBase64 := base64.StdEncoding.DecodeString(event.EventData.AdditionalMetadata)
		if errBase64 != nil || !json.Valid(decoded) {
			t.Fatalf("SDK additional_metadata is not base64 JSON: %v", errBase64)
		}
		if strings.Contains(string(decoded), "PROMPT_SENTINEL") || strings.Contains(string(decoded), "SYSTEM_SENTINEL") || strings.Contains(string(decoded), "sensitive-access-token") {
			t.Fatalf("SDK metadata contains request content or credential material: %s", decoded)
		}
		if event.EventData.EventName == "tengu_api_success" {
			if errMetadata := json.Unmarshal(decoded, &successMetadata); errMetadata != nil {
				t.Fatal(errMetadata)
			}
		}
	}
	if strings.Contains(string(sdk.Body), "PROMPT_SENTINEL") || strings.Contains(string(sdk.Body), "SYSTEM_SENTINEL") || strings.Contains(string(sdk.Body), "sensitive-access-token") || strings.Contains(string(sdk.Body), "COOKIE_SENTINEL") {
		t.Fatalf("SDK batch leaked request content or credentials: %s", sdk.Body)
	}
	for key, want := range map[string]any{
		"requestId":               "req_sdk_test",
		"stop_reason":             "end_turn",
		"inputTokens":             float64(17),
		"outputTokens":            float64(3),
		"cachedInputTokens":       float64(7),
		"uncachedInputTokens":     float64(5),
		"ttftMs":                  float64(125),
		"durationMs":              float64(1000),
		"requestBodyChars":        float64(len(body)),
		"estimatedInputTokens":    float64(10),
		"gzipSkipReason":          "below_min_size",
		"thinkingContentLength":   float64(7),
		"toolUseContentLengths":   `{"Read":36}`,
		"imageBlockCount":         float64(1),
		"imageTotalPixels":        float64(1),
		"imageTotalBytes":         float64(len(imagePayload)),
		"documentBlockCount":      float64(1),
		"documentTotalBytes":      float64(3),
		"effort_level":            "high",
		"previousRequestId":       "req_previous",
		"messageClientPlatform":   "desktop_app",
		"fastMode":                true,
		"globalCacheStrategy":     "none",
		"prompt_cache_ttl":        "1h",
		"prompt_cache_ttl_reason": "subscriber",
		"default_model":           "claude-sonnet-5",
		"is_default_model":        false,
		"default_effort_level":    "high",
		"is_default_effort":       true,
		"queryDepth":              float64(0),
		"costUSD":                 float64(0.0002135),
	} {
		if got := successMetadata[key]; got != want {
			t.Fatalf("SDK success metadata %s = %#v, want %#v", key, got, want)
		}
	}
	if queryChainID, _ := successMetadata["queryChainId"].(string); len(queryChainID) != 36 {
		t.Fatalf("SDK success queryChainId = %#v, want UUID", successMetadata["queryChainId"])
	}
	if source := manager.Status().LiveEmitterCoverage.SDKProcessMetricsSource; source != "unavailable" {
		t.Fatalf("SDK process source = %q, want unavailable", source)
	}
}

func TestBeginRequestGeneratesDesktopOwnedRequestIdentities(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("")
	facts.SessionID = ""
	facts.PromptID = ""
	facts.ClientRequestID = ""

	span := manager.BeginRequest(context.Background(), auth, facts)
	if !span.Active() {
		t.Fatal("missing Desktop-owned identities disabled telemetry")
	}
	if span.facts.SessionID == "" || span.facts.PromptID == "" || span.facts.ClientRequestID == "" {
		t.Fatalf("generated identities = session %q, prompt %q, client request %q", span.facts.SessionID, span.facts.PromptID, span.facts.ClientRequestID)
	}
}

func TestSDKProcessUsesOnlyProvidedDesktopSnapshot(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	want := SDKProcessSnapshot{
		UptimeSeconds:         123.5,
		RSS:                   314572800,
		HeapTotal:             134217728,
		HeapUsed:              100663296,
		External:              8388608,
		ArrayBuffers:          1048576,
		ConstrainedMemory:     8589369344,
		CPUUserMicroseconds:   120000,
		CPUSystemMicroseconds: 30000,
	}
	manager.sdkProcessProvider = func() (SDKProcessSnapshot, bool) { return want, true }
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Role = claudeprofile.RoleTitle
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[]}`), nil)
	span.ObserveResponse("req_sdk_process", "end_turn")
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	var process string
	for _, request := range doer.Requests() {
		if !strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
			continue
		}
		var batch struct {
			Events []struct {
				EventData struct {
					Process string `json:"process"`
				} `json:"event_data"`
			} `json:"events"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(batch.Events) > 0 {
			process = batch.Events[0].EventData.Process
		}
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(process)
	if errDecode != nil {
		t.Fatalf("decode SDK process: %v", errDecode)
	}
	var got sdkProcess
	if errJSON := json.Unmarshal(decoded, &got); errJSON != nil {
		t.Fatalf("decode SDK process JSON: %v", errJSON)
	}
	if got.Uptime != want.UptimeSeconds || got.RSS != want.RSS || got.HeapTotal != want.HeapTotal || got.HeapUsed != want.HeapUsed ||
		got.External != want.External || got.ArrayBuffers != want.ArrayBuffers || got.ConstrainedMemory != want.ConstrainedMemory ||
		got.CPUUsage.User != want.CPUUserMicroseconds || got.CPUUsage.System != want.CPUSystemMicroseconds {
		t.Fatalf("SDK process = %+v, want snapshot %+v", got, want)
	}
	if source := manager.Status().LiveEmitterCoverage.SDKProcessMetricsSource; source != "provided-snapshot" {
		t.Fatalf("SDK process source = %q, want provided-snapshot", source)
	}
}

type telemetryStatusError struct{ status int }

func (e telemetryStatusError) Error() string   { return http.StatusText(e.status) }
func (e telemetryStatusError) StatusCode() int { return e.status }

func TestSDKSuccessPreservesRetryChainAndExplicitTranscriptSize(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	transcriptSize := int64(14504)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Attempt = 5
	facts.StartedAt = clock.Now()
	facts.ChainStartedAt = facts.StartedAt.Add(-500 * time.Millisecond)
	facts.TranscriptSize = &transcriptSize
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"safe"}]}`), nil)
	span.ObserveResponse("req_retry_success", "end_turn")
	clock.Advance(750 * time.Millisecond)
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}

	var success map[string]any
	var transcript any
	for _, request := range doer.Requests() {
		switch {
		case strings.HasPrefix(request.URL, "https://api.anthropic.com/"):
			success = sdkMetadataForEvent(t, request.Body, "tengu_api_success")
		case strings.HasPrefix(request.URL, "https://claude.ai/"):
			transcript = rendererMetadataForEvent(t, request.Body, "desktop_ccd_message_cycle_outcome")["transcript_size_bytes"]
		}
	}
	if success["attempt"] != float64(5) || success["durationMs"] != float64(750) || success["durationMsIncludingRetries"] != float64(1250) {
		t.Fatalf("retry-chain success metadata = %v", success)
	}
	if transcript != float64(transcriptSize) {
		t.Fatalf("transcript_size_bytes = %#v, want %d", transcript, transcriptSize)
	}
}

func TestSDKRetryUsesDesktopErrorTextAndActualAttempt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Attempt = 3
	facts.StartedAt = clock.Now()
	facts.ChainStartedAt = facts.StartedAt.Add(-time.Second)
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"safe"}]}`), nil)
	// Preparation and retry can be delivered in different batches. Force that
	// split so a timer firing during a slower test run cannot hide this case.
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	clock.Advance(200 * time.Millisecond)
	errUpstream := telemetryStatusError{status: http.StatusBadGateway}
	span.FinishFailure(context.Background(), "server_error", errUpstream)
	span.RecordScheduledRetry(context.Background(), 1, 300*time.Millisecond, errUpstream)
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	var retry map[string]any
	retryCount := 0
	for _, request := range doer.Requests() {
		if strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
			if candidate := findSDKMetadataForEvent(t, request.Body, "tengu_api_retry"); candidate != nil {
				retry = candidate
				retryCount++
			}
		}
	}
	if retryCount != 1 {
		t.Fatalf("SDK retry event count=%d, want 1", retryCount)
	}
	if retry["attempt"] != float64(3) || retry["attempt_duration_ms"] != float64(200) || retry["delayMs"] != float64(300) || retry["status"] != float64(502) {
		t.Fatalf("retry metadata = %v", retry)
	}
	if retry["error"] != "API error: type=server_error status=502" {
		t.Fatalf("retry error = %#v", retry["error"])
	}
	if got := sdkRetryError(&url.Error{Op: "Post", URL: "https://example.invalid", Err: errors.New("offline")}, 0); got != "Connection error." {
		t.Fatalf("connection retry error = %q", got)
	}
}

func TestSDKClassifierUsesReducedSuccessSchemaAndPerWorkerTiming(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)

	for index := 0; index < 2; index++ {
		facts := testRequestFacts(fmt.Sprintf("99999999-9999-4999-8999-%012d", index+1))
		facts.Role = claudeprofile.RoleSecurityMonitor
		facts.PromptID = fmt.Sprintf("77777777-7777-4777-8777-%012d", index+1)
		facts.Model = "claude-sonnet-5"
		span := manager.BeginRequest(context.Background(), auth, facts)
		span.ObserveRequest([]byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"safe"}]}`), http.Header{"anthropic-beta": {"oauth-2025-04-20"}})
		span.ObserveUsage(Usage{InputTokens: 11, CacheReadInputTokens: 7, CacheCreationInputTokens: 3, OutputTokens: 2})
		span.ObserveResponse("req_classifier", "end_turn")
		clock.Advance(2500 * time.Millisecond)
		span.FinishSuccess(context.Background())
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	requests := doer.Requests()
	if len(requests) != 1 {
		t.Fatalf("delivery requests = %d, want one SDK batch", len(requests))
	}
	var batch struct {
		Events []struct {
			EventData struct {
				EventName          string `json:"event_name"`
				AdditionalMetadata string `json:"additional_metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(requests[0].Body, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	wantKeys := map[string]struct{}{
		"cachedInputTokens": {}, "cc_prompt_id": {}, "durationMsIncludingRetries": {},
		"inputTokens": {}, "model": {}, "outputTokens": {}, "querySource": {},
		"requestId": {}, "stop_reason": {}, "subscription_type": {},
		"timeSinceLastApiCallMs": {}, "uncachedInputTokens": {},
	}
	successIndex := 0
	for _, event := range batch.Events {
		if event.EventData.EventName != "tengu_api_success" {
			continue
		}
		decoded, errBase64 := base64.StdEncoding.DecodeString(event.EventData.AdditionalMetadata)
		if errBase64 != nil {
			t.Fatal(errBase64)
		}
		var metadata map[string]any
		if errMetadata := json.Unmarshal(decoded, &metadata); errMetadata != nil {
			t.Fatal(errMetadata)
		}
		if successIndex == 0 {
			if _, exists := metadata["timeSinceLastApiCallMs"]; exists {
				t.Fatalf("first classifier call unexpectedly had timing: %v", metadata)
			}
		} else if got := metadata["timeSinceLastApiCallMs"]; got != float64(2500) {
			t.Fatalf("second classifier timing = %#v, want 2500", got)
		}
		if got := metadata["cachedInputTokens"]; got != float64(7) {
			t.Fatalf("cachedInputTokens = %#v, want 7", got)
		}
		if got := metadata["uncachedInputTokens"]; got != float64(3) {
			t.Fatalf("uncachedInputTokens = %#v, want 3", got)
		}
		for key := range metadata {
			if _, ok := wantKeys[key]; !ok {
				t.Fatalf("classifier success contains full-schema field %q: %v", key, metadata)
			}
		}
		wantCount := len(wantKeys)
		if successIndex == 0 {
			wantCount--
		}
		if len(metadata) != wantCount {
			t.Fatalf("classifier success keys = %v, want %d reduced fields", metadata, wantCount)
		}
		successIndex++
	}
	if successIndex != 2 {
		t.Fatalf("classifier success events = %d, want 2", successIndex)
	}
}

func TestSDKTitleSuccessUsesCapturedCacheProfile(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Role = claudeprofile.RoleTitle
	facts.Model = "claude-haiku-4-5-20251001"
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"safe"}]}`), nil)
	span.ObserveUsage(Usage{InputTokens: 13, OutputTokens: 2})
	span.ObserveResponse("req_title", "end_turn")
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	requests := doer.Requests()
	if len(requests) != 1 {
		t.Fatalf("delivery requests = %d, want one SDK batch", len(requests))
	}
	metadata := sdkSuccessMetadataFromBatch(t, requests[0].Body)
	for key, want := range map[string]any{
		"messageTokens":           float64(0),
		"querySource":             "generate_session_title",
		"globalCacheStrategy":     "system_prompt",
		"prompt_cache_ttl":        "5m",
		"prompt_cache_ttl_reason": "default",
	} {
		if got := metadata[key]; got != want {
			t.Fatalf("title metadata %s = %#v, want %#v", key, got, want)
		}
	}
	for _, key := range []string{"default_model", "is_default_model", "default_effort_level", "is_default_effort", "messageClientPlatform", "queryChainId", "queryDepth"} {
		if _, exists := metadata[key]; exists {
			t.Fatalf("title metadata unexpectedly contains %q: %v", key, metadata)
		}
	}
}

func TestSDKRoleProfilesMatchV140609(t *testing.T) {
	tests := []struct {
		role         claudeprofile.RequestRole
		wantSource   string
		wantStrategy string
		wantTTL      string
		wantPlatform bool
		wantDepth    *int
	}{
		{role: claudeprofile.RoleWebSearchHelper, wantSource: "web_search_tool", wantStrategy: "system_prompt", wantTTL: "5m"},
		{role: claudeprofile.RoleCompaction, wantSource: "compact", wantStrategy: "none", wantTTL: "5m", wantPlatform: true},
		{role: claudeprofile.RoleSubagent, wantSource: "sdk", wantStrategy: "none", wantTTL: "1h", wantPlatform: true},
	}
	for index, test := range tests {
		t.Run(string(test.role), func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			facts := testRequestFacts(fmt.Sprintf("99999999-9999-4999-8999-%012d", index+1))
			facts.Role = test.role
			span := manager.BeginRequest(context.Background(), auth, facts)
			span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"safe"}],"output_config":{"effort":"high"}}`), nil)
			span.ObserveResponse("req_role", "end_turn")
			span.FinishSuccess(context.Background())
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			metadata := sdkSuccessMetadataFromBatch(t, doer.Requests()[0].Body)
			if metadata["querySource"] != test.wantSource || metadata["globalCacheStrategy"] != test.wantStrategy || metadata["prompt_cache_ttl"] != test.wantTTL {
				t.Fatalf("role metadata = %v", metadata)
			}
			_, hasPlatform := metadata["messageClientPlatform"]
			if hasPlatform != test.wantPlatform {
				t.Fatalf("messageClientPlatform present = %t, want %t", hasPlatform, test.wantPlatform)
			}
			if test.wantDepth == nil {
				if _, exists := metadata["queryDepth"]; exists {
					t.Fatalf("queryDepth unexpectedly present: %v", metadata)
				}
			} else if metadata["queryDepth"] != float64(*test.wantDepth) {
				t.Fatalf("queryDepth = %#v, want %d", metadata["queryDepth"], *test.wantDepth)
			}
			for _, key := range []string{"default_model", "is_default_model", "default_effort_level", "is_default_effort"} {
				if _, exists := metadata[key]; exists {
					t.Fatalf("role %s unexpectedly contains main-only field %q", test.role, key)
				}
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func sdkSuccessMetadataFromBatch(t *testing.T, payload []byte) map[string]any {
	return sdkMetadataForEvent(t, payload, "tengu_api_success")
}

func sdkMetadataForEvent(t *testing.T, payload []byte, eventName string) map[string]any {
	t.Helper()
	metadata := findSDKMetadataForEvent(t, payload, eventName)
	if metadata == nil {
		t.Fatalf("SDK event %q missing", eventName)
	}
	return metadata
}

func findSDKMetadataForEvent(t *testing.T, payload []byte, eventName string) map[string]any {
	t.Helper()
	var batch struct {
		Events []struct {
			EventData struct {
				EventName          string `json:"event_name"`
				AdditionalMetadata string `json:"additional_metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(payload, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, event := range batch.Events {
		if event.EventData.EventName != eventName {
			continue
		}
		decoded, errBase64 := base64.StdEncoding.DecodeString(event.EventData.AdditionalMetadata)
		if errBase64 != nil {
			t.Fatal(errBase64)
		}
		var metadata map[string]any
		if errMetadata := json.Unmarshal(decoded, &metadata); errMetadata != nil {
			t.Fatal(errMetadata)
		}
		return metadata
	}
	return nil
}

func rendererMetadataForEvent(t *testing.T, payload []byte, eventName string) map[string]any {
	t.Helper()
	var batch struct {
		Events []struct {
			EventData struct {
				EventName string `json:"event_name"`
				Metadata  string `json:"metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(payload, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	for _, event := range batch.Events {
		if event.EventData.EventName != eventName {
			continue
		}
		var metadata map[string]any
		if errMetadata := json.Unmarshal([]byte(event.EventData.Metadata), &metadata); errMetadata != nil {
			t.Fatal(errMetadata)
		}
		return metadata
	}
	t.Fatalf("renderer event %q missing", eventName)
	return nil
}

func TestEmptySDKQueueDoesNotRequireOAuthCredential(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	delete(auth.Metadata, "access_token")
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Role = claudeprofile.RoleTitle

	span := manager.BeginRequest(context.Background(), auth, facts)
	if !span.Active() || span.worker != nil || span.sdkWorker == nil {
		t.Fatal("title request did not receive an isolated SDK worker")
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("empty SDK queue required an OAuth credential: %v", errFlush)
	}
	status := span.sdkWorker.statusSnapshot()
	if status.Pending != 0 || status.ConsecutiveFailures != 0 || status.LastError != "" {
		t.Fatalf("empty SDK queue was marked unhealthy: %+v", status)
	}
}

func TestRequestRolesUseIndependentTelemetryScopes(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	tests := []struct {
		role         claudeprofile.RequestRole
		wantRenderer bool
		wantSDK      bool
	}{
		{role: claudeprofile.RoleMain, wantRenderer: true, wantSDK: true},
		{role: claudeprofile.RoleCompaction, wantSDK: true},
		{role: claudeprofile.RoleSubagent, wantSDK: true},
		{role: claudeprofile.RoleTitle, wantSDK: true},
		{role: claudeprofile.RoleLightHelper, wantSDK: true},
		{role: claudeprofile.RoleWebSearchHelper, wantSDK: true},
		{role: claudeprofile.RoleSecurityMonitor, wantSDK: true},
		{role: claudeprofile.RoleCountTokens},
	}
	for index, test := range tests {
		facts := testRequestFacts(fmt.Sprintf("99999999-9999-4999-8999-%012d", index+1))
		facts.Role = test.role
		span := manager.BeginRequest(context.Background(), auth, facts)
		if got := span.worker != nil; got != test.wantRenderer {
			t.Errorf("role %q renderer scope = %t, want %t", test.role, got, test.wantRenderer)
		}
		if got := span.sdkWorker != nil; got != test.wantSDK {
			t.Errorf("role %q SDK scope = %t, want %t", test.role, got, test.wantSDK)
		}
	}
}

func TestRestoredSDKQueueWaitsForCredentialRebind(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	mutate := func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Batch.JitterMinimum = 1
		bundle.SDKTelemetry.Batch.JitterMaximum = 1
		bundle.SDKTelemetry.Batch.InitialBackoffMS = 1
		bundle.SDKTelemetry.Batch.MaxBackoffMS = 1
	}

	first := newTelemetryTestManager(t, root, clock, &testDoer{errors: []error{errors.New("offline")}}, mutate)
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	facts.Role = claudeprofile.RoleTitle
	span := first.BeginRequest(context.Background(), auth, facts)
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[]}`), http.Header{"anthropic-beta": {"oauth-2025-04-20"}})
	span.FinishSuccess(context.Background())
	if errFlush := first.Flush(context.Background()); errFlush == nil {
		t.Fatal("offline SDK delivery unexpectedly succeeded")
	}
	first.Close()

	doer := &testDoer{}
	second := newTelemetryTestManager(t, root, clock, doer, mutate)
	clock.Advance(2 * time.Millisecond)
	if errFlush := second.Flush(context.Background()); errFlush == nil || !strings.Contains(errFlush.Error(), "OAuth credential is unavailable") {
		t.Fatalf("restored SDK queue without an in-memory credential = %v", errFlush)
	}
	status := accountStatusForRole(t, second, second.sdkProfile.EndpointRole)
	if status.Pending != 4 || status.DeadLetters != 0 {
		t.Fatalf("restored SDK queue changed before credential rebind: %+v", status)
	}

	worker, errWorker := second.workerForDelivery(auth, second.sdkDelivery)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	if worker.authSnapshot() == nil {
		t.Fatal("SDK worker did not rebind the live OAuth credential")
	}
	if errFlush := second.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush after SDK credential rebind: %v", errFlush)
	}
	requests := doer.Requests()
	if len(requests) != 1 || requests[0].Header.Get("Authorization") != "Bearer sensitive-access-token" {
		t.Fatalf("SDK rebind delivery requests = %+v", requests)
	}
	if status = accountStatusForRole(t, second, second.sdkProfile.EndpointRole); status.Pending != 0 || status.ConsecutiveFailures != 0 {
		t.Fatalf("SDK queue did not recover after credential rebind: %+v", status)
	}
}

const (
	testAccountA = "11111111-1111-4111-8111-111111111111"
	testAccountB = "22222222-2222-4222-8222-222222222222"
	testOrgA     = "33333333-3333-4333-8333-333333333333"
	testOrgB     = "44444444-4444-4444-8444-444444444444"
	testDeviceA  = "55555555-5555-4555-8555-555555555555"
	testDeviceB  = "66666666-6666-4666-8666-666666666666"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type recordedRequest struct {
	URL    string
	Header http.Header
	Body   []byte
}

type testDoer struct {
	mu             sync.Mutex
	statuses       []int
	errors         []error
	requests       []recordedRequest
	factoryProxies []string
}

func (d *testDoer) Do(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	d.mu.Lock()
	d.requests = append(d.requests, recordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
	var err error
	if len(d.errors) > 0 {
		err = d.errors[0]
		d.errors = d.errors[1:]
	}
	status := http.StatusOK
	if len(d.statuses) > 0 {
		status = d.statuses[0]
		d.statuses = d.statuses[1:]
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	proto, protoMajor, protoMinor := "HTTP/2.0", 2, 0
	if request.URL.Hostname() == "api.anthropic.com" {
		proto, protoMajor, protoMinor = "HTTP/1.1", 1, 1
	}
	return &http.Response{StatusCode: status, Proto: proto, ProtoMajor: protoMajor, ProtoMinor: protoMinor, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func (d *testDoer) Requests() []recordedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedRequest(nil), d.requests...)
}

func (d *testDoer) recordFactoryProxy(proxyURL string) {
	d.mu.Lock()
	d.factoryProxies = append(d.factoryProxies, proxyURL)
	d.mu.Unlock()
}

func (d *testDoer) FactoryProxies() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.factoryProxies...)
}

func newTelemetryTestManager(t *testing.T, root string, clock *testClock, doer *testDoer, mutate func(*claudeprofile.Bundle)) *Manager {
	t.Helper()
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	bundle.Telemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
	bundle.Telemetry.Batch.JitterMinimum = 1
	bundle.Telemetry.Batch.JitterMaximum = 1
	if mutate != nil {
		mutate(bundle)
	}
	if errValidate := bundle.Validate(); errValidate != nil {
		t.Fatalf("validate test bundle: %v", errValidate)
	}
	manager := NewManager(Options{
		StatePath: root,
		Bundle:    bundle,
		DoerFactory: func(proxyURL string) HTTPDoer {
			doer.recordFactoryProxy(proxyURL)
			return doer
		},
		Now:         clock.Now,
		RandomFloat: func() float64 { return 0 },
	})
	t.Cleanup(manager.Close)
	return manager
}

func newTelemetryTestAuth(t *testing.T, accountUUID, organizationUUID, deviceUUID string) *cliproxyauth.Auth {
	t.Helper()
	authID, errAuthID := claudedesktop.StableAuthID(accountUUID, organizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	identity := claudedesktop.AccountIdentity{AccountUUID: accountUUID, OrganizationUUID: organizationUUID}
	device := claudedesktop.TrustedDevice{DeviceID: deviceUUID, DeviceToken: "trusted-device-token", DisplayName: "Desktop"}
	enrollment := claudedesktop.NewEnrollment(authID, identity, device, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	return &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			"account_uuid":      accountUUID,
			"organization_uuid": organizationUUID,
			"claude_device_ids": []string{claudedesktop.RequestDeviceID(deviceUUID)},
			"access_token":      "sensitive-access-token",
		},
	}
}

func testRequestFacts(sessionID string) RequestFacts {
	return RequestFacts{
		Role:            claudeprofile.RoleMain,
		SessionID:       sessionID,
		PromptID:        "77777777-7777-4777-8777-777777777777",
		ClientRequestID: "88888888-8888-4888-8888-888888888888",
		Model:           "claude-opus-5",
		PermissionMode:  "default",
		MCPServerCount:  3,
	}
}

func accountStatusForRole(t *testing.T, manager *Manager, role string) AccountStatus {
	t.Helper()
	for _, account := range manager.Status().Accounts {
		if account.EndpointRole == role {
			return account
		}
	}
	t.Fatalf("telemetry status has no account for endpoint role %q: %+v", role, manager.Status().Accounts)
	return AccountStatus{}
}

func TestSessionLineagePersistsAcrossManagerRestart(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	const sessionID = "99999999-9999-4999-8999-999999999999"

	first := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	token, previous, errBegin := first.BeginLineage(auth, sessionID)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if previous != "" || token == nil || !token.Active() {
		t.Fatalf("first lineage token=%v previous=%q", token, previous)
	}
	if errCommit := token.Commit("req_first"); errCommit != nil {
		t.Fatal(errCommit)
	}
	first.Close()

	second := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	token, previous, errBegin = second.BeginLineage(auth, sessionID)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if previous != "req_first" || token == nil || !token.Active() {
		t.Fatalf("restored lineage token=%v previous=%q", token, previous)
	}
	if errCommit := token.Commit("req_second"); errCommit != nil {
		t.Fatal(errCommit)
	}

	var lineageFiles []string
	errWalk := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".lineage") {
			lineageFiles = append(lineageFiles, path)
		}
		return nil
	})
	if errWalk != nil {
		t.Fatal(errWalk)
	}
	if len(lineageFiles) != 1 {
		t.Fatalf("lineage files = %d, want 1", len(lineageFiles))
	}
	raw, errRead := os.ReadFile(lineageFiles[0])
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, secret := range []string{sessionID, "req_first", "req_second", testAccountA} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("protected lineage file contains plaintext %q", secret)
		}
	}
}

func TestTelemetryBindingRevisionIncludesEgressDestinationAndTransport(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	manager.globalProxyURL = "http://user:secret@proxy-a.example:8080"
	first, errFirst := manager.bindingFor(auth)
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	manager.globalProxyURL = "http://user:secret@proxy-b.example:8080"
	second, errSecond := manager.bindingFor(auth)
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if first.BindingRevision == second.BindingRevision || first.EgressRevision == second.EgressRevision {
		t.Fatal("proxy/egress change did not create a new telemetry binding")
	}
	if first.DestinationRevision == "" || first.TransportRevision != manager.profile.Transport.Revision {
		t.Fatalf("binding omitted destination or transport revision: %#v", first)
	}
	auth.ProxyURL = "direct"
	direct, errDirect := manager.bindingFor(auth)
	if errDirect != nil {
		t.Fatal(errDirect)
	}
	if direct.EgressProxyURL != "direct" || direct.BindingRevision == second.BindingRevision {
		t.Fatalf("auth-level direct egress was not pinned: %#v", direct)
	}
}

func TestTelemetryFlushAcceptsAnySuccessfulStatus(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{statuses: []int{http.StatusNoContent}}
	manager := newTelemetryTestManager(t, root, clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.ProxyURL = "http://proxy-user:proxy-secret@proxy.example:8080"
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	worker, errWorker := manager.workerFor(auth)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	files, errFiles := worker.scanQueue()
	if errFiles != nil {
		t.Fatal(errFiles)
	}
	if len(files) != 0 {
		t.Fatalf("queue files after HTTP 204 = %d, want 0", len(files))
	}
}

func TestQueueFilesEncryptIdentityAndPayload(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, root, clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.ProxyURL = "http://proxy-user:proxy-secret@proxy.example:8080"
	facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	span := manager.BeginRequest(context.Background(), auth, facts)
	span.ObserveFirstByte(clock.Now().Add(125 * time.Millisecond))
	span.ObserveUsage(Usage{InputTokens: 17, CacheCreationInputTokens: 5, CacheReadInputTokens: 7})
	span.FinishSuccess(context.Background())

	worker, errWorker := manager.workerFor(auth)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	files, errFiles := worker.scanQueue()
	if errFiles != nil {
		t.Fatal(errFiles)
	}
	if len(files) != 4 {
		t.Fatalf("queue files = %d, want binary-resolved, session, start and outcome", len(files))
	}
	for _, file := range files {
		raw, errRead := os.ReadFile(file.path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		for _, secret := range []string{testAccountA, testOrgA, testDeviceA, facts.SessionID, facts.PromptID, "sensitive-access-token", "claude-opus-5", auth.ProxyURL, "proxy-secret"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("protected queue file %s contains plaintext %q", filepath.Base(file.path), secret)
			}
		}
	}
}

func testRendererRuntimeSnapshot() RendererRuntimeSnapshot {
	return RendererRuntimeSnapshot{
		ProcessFootprintSampleAgeMS:            12,
		ProcessGPURSSBytes:                     33 * 1024 * 1024,
		ProcessMainCommitBytes:                 140 * 1024 * 1024,
		ProcessMainCPUPct:                      1.29,
		ProcessMainExternalBytes:               8 * 1024 * 1024,
		ProcessMainFootprintBytes:              150 * 1024 * 1024,
		ProcessMainHeapLimitBytes:              2248146944,
		ProcessMainHeapTotalBytes:              90 * 1024 * 1024,
		ProcessMainHeapUsedBytes:               70 * 1024 * 1024,
		ProcessMainRSSBytes:                    145 * 1024 * 1024,
		ProcessRendererCPUSumPct:               0.45,
		ProcessRendererFootprintSumBytes:       210 * 1024 * 1024,
		ProcessRendererMainViewBlinkBytes:      15 * 1024 * 1024,
		ProcessRendererMainViewHeapLimitBytes:  2248146944,
		ProcessRendererMainViewHeapSampleAgeMS: 8,
		ProcessRendererMainViewHeapTotalBytes:  80 * 1024 * 1024,
		ProcessRendererMainViewHeapUsedBytes:   60 * 1024 * 1024,
		ProcessRendererRSSSumBytes:             205 * 1024 * 1024,
		ProcessUtilityRSSSumBytes:              25 * 1024 * 1024,
		WindowCount:                            2,
	}
}

func TestFlushMatchesDesktopContractAndAcknowledgesBatch(t *testing.T) {
	originalSnapshot := rendererHostSnapshotNow
	rendererHostSnapshotNow = func() rendererHostSnapshot {
		return rendererHostSnapshot{
			TotalMemoryBytes:     8 * rendererGiB,
			AvailableMemoryBytes: 3 * rendererGiB,
			CPUModel:             "Intel Xeon Processor (Skylake, IBRS)",
			OSBuild:              "26100.1.amd64fre.ge_release.240331-1435",
			OSRelease:            "10.0.26100",
			OSVersion:            "10.0.26100",
		}
	}
	t.Cleanup(func() { rendererHostSnapshotNow = originalSnapshot })

	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, root, clock, doer, nil)
	manager.rendererSnapshot = func() (RendererRuntimeSnapshot, bool) {
		return testRendererRuntimeSnapshot(), true
	}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	span.ObserveFirstByte(clock.Now().Add(75 * time.Millisecond))
	span.ObserveUsage(Usage{InputTokens: 11, CacheCreationInputTokens: 2, CacheReadInputTokens: 3})
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}

	requests := doer.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if got := request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := request.Header.Get("x-service-name"); got != "claude_desktop" {
		t.Fatalf("x-service-name = %q", got)
	}
	if got := request.Header.Get("User-Agent"); got != manager.bundle.Telemetry.Runtime.UserAgent {
		t.Fatalf("User-Agent = %q, want captured renderer UA %q", got, manager.bundle.Telemetry.Runtime.UserAgent)
	}
	for _, name := range []string{"Authorization", "Cookie", "Set-Cookie"} {
		if value := request.Header.Get(name); value != "" {
			t.Fatalf("telemetry inherited %s: %q", name, value)
		}
	}
	var batch struct {
		Events []struct {
			EventType string `json:"event_type"`
			EventData struct {
				EventName      string         `json:"event_name"`
				UserProperties map[string]any `json:"user_properties"`
				Metadata       string         `json:"metadata"`
				Auth           map[string]any `json:"auth"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
		t.Fatalf("decode batch: %v", errDecode)
	}
	if len(batch.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(batch.Events))
	}
	wantNames := []string{"desktop_ccd_binary_resolved", "desktop_ccd_session_initialized", "desktop_ccd_message_cycle_start", "desktop_ccd_message_cycle_outcome"}
	wantRendererSessionID := "local_99999999-9999-4999-8999-999999999999"
	for index, event := range batch.Events {
		if event.EventType != "TelemetryEvent" || event.EventData.EventName != wantNames[index] {
			t.Fatalf("event[%d] = type %q name %q", index, event.EventType, event.EventData.EventName)
		}
		if event.EventData.UserProperties["user_id"] != testAccountA || event.EventData.Auth["account_uuid"] != testAccountA || event.EventData.Auth["organization_uuid"] != testOrgA {
			t.Fatalf("event[%d] identity envelope mismatch", index)
		}
		var metadata map[string]any
		if errMetadata := json.Unmarshal([]byte(event.EventData.Metadata), &metadata); errMetadata != nil {
			t.Fatalf("event[%d] metadata is not JSON-string data: %v", index, errMetadata)
		}
		for _, forbidden := range []string{"authorization", "cookie", "prompt", "access_token", "refresh_token"} {
			if _, exists := metadata[forbidden]; exists {
				t.Fatalf("event[%d] metadata contains forbidden key %q", index, forbidden)
			}
		}
		for _, key := range []string{
			"total_memory", "available_memory", "free_memory", "cpu_model", "device_class",
			"os_build", "os_release", "os_version", "process_footprint_sample_age_ms",
			"process_gpu_rss_bytes", "process_main_commit_bytes", "process_main_cpu_pct",
			"process_main_external_bytes", "process_main_footprint_bytes", "process_main_heap_limit_bytes",
			"process_main_heap_total_bytes", "process_main_heap_used_bytes", "process_main_rss_bytes",
			"process_renderer_cpu_sum_pct", "process_renderer_footprint_sum_bytes",
			"process_renderer_main_view_blink_bytes", "process_renderer_main_view_heap_limit_bytes",
			"process_renderer_main_view_heap_sample_age_ms", "process_renderer_main_view_heap_total_bytes",
			"process_renderer_main_view_heap_used_bytes", "process_renderer_rss_sum_bytes",
			"process_utility_rss_sum_bytes", "window_count",
		} {
			if _, exists := metadata[key]; !exists {
				t.Fatalf("event[%d] missing v140609 runtime field %q", index, key)
			}
		}
		switch index {
		case 0:
			// The native binary preflight payload is main-process scoped and
			// carries no session_id: {resolution, resolved_version, required_version}.
			if _, exists := metadata["session_id"]; exists {
				t.Fatalf("binary resolved metadata fabricated a session_id: %v", metadata)
			}
			if metadata["resolution"] != "required_version" || metadata["resolved_version"] != manager.bundle.CodeVersion || metadata["required_version"] != manager.bundle.CodeVersion {
				t.Fatalf("binary resolved metadata = %v", metadata)
			}
			continue
		}
		if metadata["session_id"] != wantRendererSessionID {
			t.Fatalf("event[%d] session_id = %#v, want %q", index, metadata["session_id"], wantRendererSessionID)
		}
		switch index {
		case 1:
			if metadata["effort"] != "high" || metadata["effort_source"] != "persisted" || metadata["has_launch_tools"] != true || metadata["is_git_repo"] != false {
				t.Fatalf("session initialization metadata = %v", metadata)
			}
		case 2:
			if value, exists := metadata["cli_session_id"]; !exists || value != nil || metadata["input_origin"] != "user" {
				t.Fatalf("first cycle start metadata = %v", metadata)
			}
		case 3:
			if metadata["cli_session_id"] != "99999999-9999-4999-8999-999999999999" || metadata["input_origin"] != "user" || metadata["spawn_source"] != "cold" {
				t.Fatalf("cycle outcome identity metadata = %v", metadata)
			}
			for _, key := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "ms_to_first_token"} {
				if _, exists := metadata[key]; !exists {
					t.Fatalf("cycle outcome missing zero-preserving field %q: %v", key, metadata)
				}
			}
			if _, exists := metadata["transcript_size_bytes"]; exists {
				t.Fatalf("cycle outcome fabricated transcript size: %v", metadata)
			}
		}
	}
	status := manager.Status()
	rendererStatus := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if rendererStatus.Pending != 0 || rendererStatus.Sending != 0 || rendererStatus.DeadLetters != 0 {
		t.Fatalf("status after ACK = %+v", status.Accounts)
	}
}

func TestRendererRequestHeadersMatchCapturedProtectedSender(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	originalMemory := rendererPhysicalMemoryBytes
	rendererPhysicalMemoryBytes = func() uint64 { return 8 * rendererGiB }
	t.Cleanup(func() { rendererPhysicalMemoryBytes = originalMemory })

	requests := make([]*http.Request, 2)
	for index := range requests {
		request, errRequest := http.NewRequest(http.MethodPost, bundle.Telemetry.Endpoint, strings.NewReader("{}"))
		if errRequest != nil {
			t.Fatal(errRequest)
		}
		if errHeaders := applyRendererRequestHeaders(request, &bundle.Telemetry.Runtime, &bundle.Telemetry.Sentry); errHeaders != nil {
			t.Fatal(errHeaders)
		}
		requests[index] = request
	}

	want := map[string]string{
		"Accept-Encoding":                  "gzip, deflate, br, zstd",
		"Accept-Language":                  "en-US",
		"anthropic-client-app":             "com.anthropic.claudefordesktop",
		"anthropic-client-device-class":    "8",
		"anthropic-client-os-platform":     "win32",
		"anthropic-client-os-version":      "10.0.26100",
		"anthropic-client-platform":        "desktop_app",
		"anthropic-client-total-memory-gb": "8",
		"anthropic-client-version":         "1.40609.0",
		"anthropic-desktop-topbar":         "1",
		"sec-fetch-dest":                   "empty",
		"sec-fetch-mode":                   "no-cors",
		"sec-fetch-site":                   "none",
		"User-Agent":                       bundle.Telemetry.Runtime.UserAgent,
		"Priority":                         "u=4, i",
		"sentry-trace":                     "00000000000000000000000000000000-0000000000000000",
		"baggage":                          "sentry-environment=production,sentry-release=Claude%401.40609.0,sentry-public_key=2f98127cbffe4740b1f767a2de77d23b,sentry-trace_id=00000000000000000000000000000000,sentry-org_id=1158394",
	}
	for index, request := range requests {
		for name, value := range want {
			if got := request.Header.Get(name); got != value {
				t.Fatalf("request %d %s = %q, want %q", index, name, got, value)
			}
		}
	}
	if requests[0].Header.Get("sentry-trace") != requests[1].Header.Get("sentry-trace") || requests[0].Header.Get("baggage") != requests[1].Header.Get("baggage") {
		t.Fatal("protected renderer Sentry propagation changed between requests")
	}
}

func TestRendererCLISessionIdentityChangesAfterFirstTurn(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	const sessionID = "99999999-9999-4999-8999-999999999999"
	for turn := 0; turn < 2; turn++ {
		facts := testRequestFacts(sessionID)
		facts.PromptID = fmt.Sprintf("77777777-7777-4777-8777-%012d", turn+1)
		span := manager.BeginRequest(context.Background(), auth, facts)
		span.FinishSuccess(context.Background())
	}
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}
	requests := doer.Requests()
	if len(requests) != 1 {
		t.Fatalf("renderer requests = %d, want one", len(requests))
	}
	var batch struct {
		Events []struct {
			EventData struct {
				EventName string `json:"event_name"`
				Metadata  string `json:"metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(requests[0].Body, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	var starts, outcomes []any
	for _, event := range batch.Events {
		var metadata map[string]any
		if errMetadata := json.Unmarshal([]byte(event.EventData.Metadata), &metadata); errMetadata != nil {
			t.Fatal(errMetadata)
		}
		switch event.EventData.EventName {
		case "desktop_ccd_message_cycle_start":
			starts = append(starts, metadata["cli_session_id"])
		case "desktop_ccd_message_cycle_outcome":
			outcomes = append(outcomes, metadata["cli_session_id"])
		}
	}
	if len(starts) != 2 || starts[0] != nil || starts[1] != sessionID {
		t.Fatalf("cycle start cli_session_id values = %#v", starts)
	}
	if len(outcomes) != 2 || outcomes[0] != sessionID || outcomes[1] != sessionID {
		t.Fatalf("cycle outcome cli_session_id values = %#v", outcomes)
	}
}

func TestManagerCloseEmitsCapturedAppQuitRendererSequence(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 3, 19, 7, 2, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	manager.sdkProcessProvider = func() (SDKProcessSnapshot, bool) {
		return SDKProcessSnapshot{
			UptimeSeconds:      120,
			RSS:                285880320,
			FootprintBytes:     204849152,
			CommitBytes:        364740608,
			PeakFootprintBytes: 220356608,
			MemorySampleAgeMS:  6723,
		}, true
	}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	clock.Advance(4 * time.Second)
	span.FinishSuccess(context.Background())
	clock.Advance(265 * time.Second)
	manager.Close()

	requests := doer.Requests()
	var rendererRequest *recordedRequest
	for index := range requests {
		if strings.HasPrefix(requests[index].URL, "https://claude.ai/") {
			rendererRequest = &requests[index]
			break
		}
	}
	if rendererRequest == nil {
		t.Fatalf("renderer app-quit batch missing: %+v", requests)
	}
	var batch struct {
		Events []struct {
			EventData struct {
				EventName string `json:"event_name"`
				Metadata  string `json:"metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(rendererRequest.Body, &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	wantSuffix := []string{
		"desktop_ccd_session_stopped",
		"desktop_ccd_session_visibility_changed",
		"desktop_ccd_session_idle_timeout_started",
	}
	if len(batch.Events) < len(wantSuffix) {
		t.Fatalf("renderer app-quit events = %d, want at least %d", len(batch.Events), len(wantSuffix))
	}
	for index, want := range wantSuffix {
		event := batch.Events[len(batch.Events)-len(wantSuffix)+index]
		if event.EventData.EventName != want {
			t.Fatalf("app-quit event[%d] = %q, want %q", index, event.EventData.EventName, want)
		}
		var metadata map[string]any
		if errMetadata := json.Unmarshal([]byte(event.EventData.Metadata), &metadata); errMetadata != nil {
			t.Fatal(errMetadata)
		}
		switch want {
		case "desktop_ccd_session_stopped":
			if metadata["trigger"] != "app_quit" || metadata["had_pending_cycle"] != false || metadata["pending_had_first_response"] != nil || metadata["pending_seconds"] != nil {
				t.Fatalf("session stopped metadata = %+v", metadata)
			}
			if metadata["cli_rss_bytes"] != float64(285880320) || metadata["cli_footprint_bytes"] != float64(204849152) || metadata["cli_commit_bytes"] != float64(364740608) || metadata["cli_peak_footprint_bytes"] != float64(220356608) || metadata["cli_mem_sample_age_ms"] != float64(6723) {
				t.Fatalf("session stopped CLI process metadata = %+v", metadata)
			}
		case "desktop_ccd_session_visibility_changed":
			if metadata["is_visible"] != false || metadata["has_active_query"] != false {
				t.Fatalf("session visibility metadata = %+v", metadata)
			}
		case "desktop_ccd_session_idle_timeout_started":
			if metadata["timeout_ms"] != float64(900000) || metadata["configured_timeout_ms"] != float64(900000) || metadata["seconds_since_last_activity"] != float64(265) {
				t.Fatalf("session idle metadata = %+v", metadata)
			}
		}
	}
}

func TestManagerQuarantineFreezesQueueWithoutSyntheticShutdown(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	clock.Advance(time.Second)
	span.FinishSuccess(context.Background())
	worker, errWorker := manager.workerFor(auth)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	before, errScan := worker.scanQueue()
	if errScan != nil || len(before) == 0 {
		t.Fatalf("queue before quarantine = %d, %v", len(before), errScan)
	}

	manager.Quarantine()
	after, errScan := worker.scanQueue()
	if errScan != nil {
		t.Fatal(errScan)
	}
	if len(after) != len(before) {
		t.Fatalf("queue files after quarantine = %d, want %d", len(after), len(before))
	}
	if requests := doer.Requests(); len(requests) != 0 {
		t.Fatalf("quarantine delivered or synthesized %d telemetry request(s)", len(requests))
	}
}

func TestTelemetryWireRequiresHTTP2AndUsesCapturedRendererUserAgent(t *testing.T) {
	observed := make(chan *http.Request, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		observed <- request.Clone(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	manager.profile.Endpoint = server.URL
	manager.rendererDelivery.endpoint = server.URL
	manager.doerFactory = func(string) HTTPDoer { return server.Client() }
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush: %v", errFlush)
	}
	request := <-observed
	if request.ProtoMajor != 2 {
		t.Fatalf("protocol = %q, want HTTP/2", request.Proto)
	}
	if got := request.Header.Get("User-Agent"); got != manager.bundle.Telemetry.Runtime.UserAgent {
		t.Fatalf("User-Agent = %q, want captured renderer UA %q", got, manager.bundle.Telemetry.Runtime.UserAgent)
	}
}

func TestDeliveryRetryAndDeadLetterRecovery(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{statuses: []int{http.StatusInternalServerError, http.StatusOK}}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Batch.InitialBackoffMS = 1
		bundle.Telemetry.Batch.MaxBackoffMS = 1
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush == nil {
		t.Fatal("first 500 flush unexpectedly succeeded")
	}
	status := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if status.Pending != 4 || status.ConsecutiveFailures != 1 || status.LastError != "delivery_error" {
		t.Fatalf("retry status = %+v", status)
	}
	clock.Advance(2 * time.Millisecond)
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("retry flush: %v", errFlush)
	}
	if status = accountStatusForRole(t, manager, manager.profile.EndpointRole); status.Pending != 0 || status.ConsecutiveFailures != 0 {
		t.Fatalf("status after retry success = %+v", status)
	}

	deadDoer := &testDoer{errors: []error{errors.New("network request included https://sensitive.invalid/path?token=secret")}}
	deadManager := newTelemetryTestManager(t, t.TempDir(), clock, deadDoer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Batch.MaxRetries = 0
	})
	deadAuth := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	deadSpan := deadManager.BeginRequest(context.Background(), deadAuth, testRequestFacts("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	deadSpan.FinishFailure(context.Background(), "network_error", errors.New("do not persist this raw error"))
	if errFlush := deadManager.Flush(context.Background()); errFlush == nil {
		t.Fatal("network failure unexpectedly succeeded")
	}
	deadStatus := accountStatusForRole(t, deadManager, deadManager.profile.EndpointRole)
	if deadStatus.DeadLetters != 4 || strings.Contains(deadStatus.LastError, "sensitive.invalid") || deadStatus.LastError != "delivery_error" {
		t.Fatalf("dead-letter status = %+v", deadStatus)
	}
	count, errRetry := deadManager.RetryDeadLetters()
	if errRetry != nil || count != 4 {
		t.Fatalf("retry dead letters = %d, %v", count, errRetry)
	}
	if errFlush := deadManager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush retried dead letters: %v", errFlush)
	}
}

func TestStaleLeaseRecoveryAndAccountIsolation(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	authA := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	authB := newTelemetryTestAuth(t, testAccountB, testOrgB, testDeviceB)
	manager.BeginRequest(context.Background(), authA, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	manager.BeginRequest(context.Background(), authB, testRequestFacts("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	workerA, errA := manager.workerFor(authA)
	workerB, errB := manager.workerFor(authB)
	if errA != nil || errB != nil {
		t.Fatalf("workers: %v, %v", errA, errB)
	}
	if workerA.directory == workerB.directory {
		t.Fatalf("accounts share queue directory %q", workerA.directory)
	}
	claimed, _, errClaim := workerA.claimBatch(clock.Now())
	if errClaim != nil || len(claimed) == 0 {
		t.Fatalf("claim batch = %d, %v", len(claimed), errClaim)
	}
	clock.Advance(time.Duration(manager.profile.Batch.LeaseTimeoutMS+1) * time.Millisecond)
	if errRecover := workerA.recoverStaleClaims(clock.Now()); errRecover != nil {
		t.Fatalf("recover stale claims: %v", errRecover)
	}
	status := workerA.statusSnapshot()
	if status.Sending != 0 || status.Pending == 0 {
		t.Fatalf("status after lease recovery = %+v", status)
	}
}

func TestOnlyMainRequestsEmitAndFinishIsIdempotent(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	helperFacts := testRequestFacts("99999999-9999-4999-8999-999999999999")
	helperFacts.Role = claudeprofile.RoleTitle
	helperSpan := manager.BeginRequest(context.Background(), auth, helperFacts)
	if !helperSpan.Active() || helperSpan.worker != nil || helperSpan.sdkWorker == nil {
		t.Fatal("title request did not receive an independent SDK-only telemetry span")
	}
	if accounts := manager.Status().Accounts; len(accounts) != 0 {
		t.Fatalf("unobserved title request exposed idle telemetry state: %+v", accounts)
	}

	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			span.FinishSuccess(context.Background())
		}()
	}
	wait.Wait()
	status := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if status.Pending != 4 {
		t.Fatalf("idempotent finish left %d events, want binary-resolved, session, start and outcome", status.Pending)
	}
}

func TestTerminalFailureDoesNotSynthesizeSDKRetry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	// This test counts pending events, not timer delivery. Disable the SDK's
	// live flush timer as well as the renderer timer to keep that assertion local.
	manager := newSDKPreparationTestManager(t, clock, &testDoer{})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`), http.Header{
		"anthropic-beta": {"oauth-2025-04-20"},
	})
	span.FinishFailure(context.Background(), "network_error", errors.New("connection failed"))

	rendererStatus := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if rendererStatus.Pending != 4 {
		t.Fatalf("renderer failure events = %d, want binary-resolved, session, start, and outcome", rendererStatus.Pending)
	}
	sdkStatus := accountStatusForRole(t, manager, manager.sdkProfile.EndpointRole)
	if sdkStatus.Pending != 3 {
		t.Fatalf("SDK failure events = %d, want normalization, cache-breakpoint and query without a fabricated retry", sdkStatus.Pending)
	}

	status := manager.Status()
	if status.TransportFidelity.ClientHelloStatus != "captured-current-wire" || status.TransportFidelity.ClientHelloPreset != "chromium-148-v140609" || status.TransportFidelity.HTTP2StreamMode != "multiplexed" {
		t.Fatalf("transport fidelity status = %+v", status.TransportFidelity)
	}
	if len(status.DeliveryEndpoints) != 7 {
		t.Fatalf("delivery endpoint statuses = %d, want renderer, SDK, and five auxiliary endpoints", len(status.DeliveryEndpoints))
	}
	if renderer, sdk := status.DeliveryEndpoints[0], status.DeliveryEndpoints[1]; renderer.Role != manager.profile.EndpointRole || renderer.TransportProtocol != "http/2" ||
		sdk.Role != manager.sdkProfile.EndpointRole || sdk.TransportProtocol != "http/1.1" || sdk.AuthPolicy != "oauth-bearer" {
		t.Fatalf("delivery endpoint statuses = %+v", status.DeliveryEndpoints)
	}
	deliveryStatuses := make(map[string]string, len(status.DeliveryEndpoints))
	for _, endpoint := range status.DeliveryEndpoints {
		deliveryStatuses[endpoint.Role] = endpoint.Status
	}
	for _, role := range []string{"segment", "datadog-logs", "datadog-logs-browser", "datadog-rum", "sentry"} {
		if deliveryStatuses[role] != "awaiting-enrollment-material" {
			t.Fatalf("delivery endpoint %q status = %q, want awaiting-enrollment-material", role, deliveryStatuses[role])
		}
	}
	if status.TelemetryEvidence == nil || status.TelemetryEvidence.Corpus.FlowCount != 12109 || status.TelemetryEvidence.Corpus.EventCount != 15769 {
		t.Fatalf("telemetry evidence status = %+v", status.TelemetryEvidence)
	}
	if len(status.ObservedEndpoints) != 7 {
		t.Fatalf("observed endpoint statuses = %d, want 7", len(status.ObservedEndpoints))
	}
	observedStatuses := make(map[string]string, len(status.ObservedEndpoints))
	for _, endpoint := range status.ObservedEndpoints {
		observedStatuses[endpoint.Role] = endpoint.Status
	}
	if status.LiveEmitterCoverage.Status != "partial" || status.LiveEmitterCoverage.ObservableEventNameCount != 231 || status.LiveEmitterCoverage.UnmodeledCapturedEventCount == 0 {
		t.Fatalf("live emitter coverage must expose the unmodeled captured surface: %+v", status.LiveEmitterCoverage)
	}
	for _, endpoint := range status.ObservedEndpoints {
		if endpoint.Status != "captured-delivery-enabled" {
			t.Fatalf("observed endpoint %q status = %q, want captured-delivery-enabled", endpoint.Role, endpoint.Status)
		}
	}
	statusJSON, errStatusJSON := json.Marshal(status)
	if errStatusJSON != nil {
		t.Fatal(errStatusJSON)
	}
	if strings.Contains(string(statusJSON), manager.profile.Endpoint) || strings.Contains(string(statusJSON), manager.sdkProfile.Endpoint) {
		t.Fatal("management status exposed a telemetry endpoint URL")
	}
}

func TestDeadLetterLimitKeepsNewestRecords(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{statuses: []int{http.StatusInternalServerError}}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Batch.MaxRetries = 0
		bundle.Telemetry.Batch.MaxDeadLetters = 2
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	span := manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush == nil {
		t.Fatal("500 flush unexpectedly succeeded")
	}
	status := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if status.DeadLetters != 2 {
		t.Fatalf("dead letters = %d, want 2", status.DeadLetters)
	}
}

func TestOversizedSingleEventMovesToDeadLetterWithoutDelivery(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Batch.MaxBytes = 256
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	worker, errWorker := manager.workerFor(auth)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	payload := json.RawMessage(`{"event_type":"TelemetryEvent","event_data":{"padding":"` + strings.Repeat("x", 512) + `"}}`)
	if errEnqueue := worker.enqueue(Envelope{
		Version:      1,
		EventUUID:    "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		EndpointRole: manager.profile.EndpointRole,
		Binding:      worker.binding,
		Payload:      payload,
	}); errEnqueue != nil {
		t.Fatalf("enqueue oversized event: %v", errEnqueue)
	}

	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush oversized event: %v", errFlush)
	}
	status := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if status.Pending != 0 || status.Sending != 0 || status.DeadLetters != 1 || status.Health != "degraded" {
		t.Fatalf("oversized event status = %+v", status)
	}
	if got := len(doer.Requests()); got != 0 {
		t.Fatalf("oversized event reached upstream in %d request(s)", got)
	}
}

func TestSDKBatchUsesV140609Limits(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	if bundle.SDKTelemetry.Batch.FlushIntervalMS != 1000 || bundle.SDKTelemetry.Batch.MaxEvents != 258 || bundle.SDKTelemetry.Batch.MaxBytes != 534974 {
		t.Fatalf("SDK batch profile = %+v", bundle.SDKTelemetry.Batch)
	}
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.SDKTelemetry.Batch.FlushIntervalMS = int(time.Hour / time.Millisecond)
		bundle.SDKTelemetry.Batch.JitterMinimum = 1
		bundle.SDKTelemetry.Batch.JitterMaximum = 1
	})
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	worker, errWorker := manager.workerForDelivery(auth, manager.sdkDelivery)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	padding := strings.Repeat("x", 1960)
	for index := 0; index < 258; index++ {
		payload := json.RawMessage(fmt.Sprintf(`{"event_type":"ClaudeCodeInternalEvent","event_data":{"index":%d,"padding":"%s"}}`, index, padding))
		if errEnqueue := worker.enqueue(Envelope{
			Version:      1,
			EventUUID:    fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1),
			EndpointRole: manager.sdkDelivery.endpointRole,
			Binding:      worker.binding,
			Payload:      payload,
		}); errEnqueue != nil {
			t.Fatalf("enqueue event %d: %v", index, errEnqueue)
		}
	}
	for attempt := 0; attempt < 4 && worker.statusSnapshot().Pending > 0; attempt++ {
		if errFlush := manager.Flush(context.Background()); errFlush != nil {
			t.Fatal(errFlush)
		}
	}
	requests := doer.Requests()
	if len(requests) != 1 {
		t.Fatalf("SDK delivery requests = %d, want captured 258-event batch kept together", len(requests))
	}
	totalEvents := 0
	for index, request := range requests {
		if len(request.Body) > manager.sdkProfile.Batch.MaxBytes {
			t.Fatalf("SDK batch[%d] bytes = %d, max %d", index, len(request.Body), manager.sdkProfile.Batch.MaxBytes)
		}
		var batch struct {
			Events []json.RawMessage `json:"events"`
		}
		if errDecode := json.Unmarshal(request.Body, &batch); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(batch.Events) > manager.sdkProfile.Batch.MaxEvents {
			t.Fatalf("SDK batch[%d] events = %d, max %d", index, len(batch.Events), manager.sdkProfile.Batch.MaxEvents)
		}
		totalEvents += len(batch.Events)
	}
	if totalEvents != 258 {
		t.Fatalf("SDK delivered events = %d, want 258", totalEvents)
	}
	if len(requests[0].Body) <= 512*1024 {
		t.Fatalf("SDK batch bytes = %d, want captured batch larger than 512 KiB retained", len(requests[0].Body))
	}
}

func TestDeadLetterMigrationFailureIsReported(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	worker, errWorker := manager.workerFor(auth)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version:      1,
		EventUUID:    "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		EndpointRole: manager.profile.EndpointRole,
		Binding:      worker.binding,
		Payload:      json.RawMessage(`{"event_type":"TelemetryEvent"}`),
	}); errEnqueue != nil {
		t.Fatal(errEnqueue)
	}
	files, errFiles := worker.scanQueue()
	if errFiles != nil || len(files) != 1 {
		t.Fatalf("scan queue = %d, %v", len(files), errFiles)
	}
	file := files[0]
	if errCorrupt := os.WriteFile(file.path, []byte("{"), 0o600); errCorrupt != nil {
		t.Fatal(errCorrupt)
	}
	deadPath := filepath.Join(worker.directory, deadQueueName(file.sequence, file.eventUUID, file.attempt, clock.Now()))
	if errMkdir := os.Mkdir(deadPath, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}

	if errFlush := manager.Flush(context.Background()); errFlush == nil {
		t.Fatal("dead-letter migration failure was silently ignored")
	}
	status := accountStatusForRole(t, manager, manager.profile.EndpointRole)
	if status.Sending != 1 || status.ConsecutiveFailures != 1 || status.Health != "unhealthy" || status.QueueWritable || status.LastError == "" {
		t.Fatalf("migration failure status = %+v", status)
	}
	if got := len(doer.Requests()); got != 0 {
		t.Fatalf("corrupt event reached upstream in %d request(s)", got)
	}
}

func TestRestartRecoversQueueSequence(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	firstDoer := &testDoer{statuses: []int{http.StatusInternalServerError}}
	first := newTelemetryTestManager(t, root, clock, firstDoer, nil)
	first.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	first.Close()

	second := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	second.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	worker, errWorker := second.workerFor(auth)
	if errWorker != nil {
		t.Fatal(errWorker)
	}
	files, errFiles := worker.scanQueue()
	if errFiles != nil {
		t.Fatal(errFiles)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].sequence < files[j].sequence })
	if len(files) != 7 {
		t.Fatalf("recovered queue files = %d, want 7", len(files))
	}
	for index, file := range files {
		if want := uint64(index + 1); file.sequence != want {
			t.Fatalf("sequence[%d] = %d, want %d", index, file.sequence, want)
		}
	}
}

func TestRestartRestoresAndFlushesQueueWithoutAnotherRequest(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.ProxyURL = "http://127.0.0.1:19090"
	first := newTelemetryTestManager(t, root, clock, &testDoer{statuses: []int{http.StatusInternalServerError}}, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Batch.InitialBackoffMS = 1
		bundle.Telemetry.Batch.MaxBackoffMS = 1
	})
	first.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))
	first.Close()

	doer := &testDoer{}
	second := newTelemetryTestManager(t, root, clock, doer, func(bundle *claudeprofile.Bundle) {
		bundle.Telemetry.Batch.InitialBackoffMS = 1
		bundle.Telemetry.Batch.MaxBackoffMS = 1
	})
	status := second.Status()
	if len(status.Accounts) != 1 || status.Accounts[0].Pending != 6 {
		t.Fatalf("restored status = %+v", status.Accounts)
	}
	clock.Advance(2 * time.Millisecond)
	if errFlush := second.Flush(context.Background()); errFlush != nil {
		t.Fatalf("flush restored queue: %v", errFlush)
	}
	if got := len(doer.Requests()); got != 1 {
		t.Fatalf("restored queue requests = %d, want 1", got)
	}
	if proxies := doer.FactoryProxies(); len(proxies) != 1 || proxies[0] != auth.ProxyURL {
		t.Fatalf("restored queue used unexpected egress binding: %v", proxies)
	}
	if status = second.Status(); status.Accounts[0].Pending != 0 {
		t.Fatalf("status after restored flush = %+v", status.Accounts[0])
	}
}

func TestTelemetryWorkersArePartitionedByEffectiveEgress(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	doer := &testDoer{}
	manager := newTelemetryTestManager(t, t.TempDir(), clock, doer, nil)
	manager.globalProxyURL = "http://127.0.0.1:19091"
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)

	globalWorker, errGlobal := manager.workerFor(auth)
	if errGlobal != nil {
		t.Fatal(errGlobal)
	}
	overriddenAuth := *auth
	overriddenAuth.ProxyURL = "http://127.0.0.1:19092"
	overriddenWorker, errOverridden := manager.workerFor(&overriddenAuth)
	if errOverridden != nil {
		t.Fatal(errOverridden)
	}
	if globalWorker.directory == overriddenWorker.directory || globalWorker.binding.BindingRevision == overriddenWorker.binding.BindingRevision {
		t.Fatal("different telemetry egress routes shared a queue binding")
	}
	if globalWorker.binding.EgressProxyURL != manager.globalProxyURL || overriddenWorker.binding.EgressProxyURL != overriddenAuth.ProxyURL {
		t.Fatalf("effective egress mismatch: global=%q overridden=%q", globalWorker.binding.EgressProxyURL, overriddenWorker.binding.EgressProxyURL)
	}
	globalWorker.currentDoer()
	overriddenWorker.currentDoer()
	if proxies := doer.FactoryProxies(); len(proxies) != 2 || proxies[0] != manager.globalProxyURL || proxies[1] != overriddenAuth.ProxyURL {
		t.Fatalf("factory egress routes = %v", proxies)
	}
}

func TestStatusJSONOmitsStatePathAndUnsetTimestamps(t *testing.T) {
	root := t.TempDir()
	clock := &testClock{now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
	manager := newTelemetryTestManager(t, root, clock, &testDoer{}, nil)
	auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
	auth.ProxyURL = "http://telemetry-user:telemetry-secret@127.0.0.1:19093"
	manager.BeginRequest(context.Background(), auth, testRequestFacts("99999999-9999-4999-8999-999999999999"))

	payload, errMarshal := json.Marshal(manager.Status())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var decoded map[string]any
	if errDecode := json.Unmarshal(payload, &decoded); errDecode != nil {
		t.Fatal(errDecode)
	}
	if _, exists := decoded["state_path"]; exists || strings.Contains(string(payload), root) {
		t.Fatalf("status exposed telemetry state path: %s", payload)
	}
	if strings.Contains(string(payload), auth.ProxyURL) || strings.Contains(string(payload), "telemetry-secret") {
		t.Fatalf("status exposed telemetry egress details: %s", payload)
	}
	if strings.Contains(string(payload), "0001-01-01") {
		t.Fatalf("status exposed zero timestamps: %s", payload)
	}
}
