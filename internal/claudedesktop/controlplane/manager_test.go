package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type recordedControlRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

type recordingControlDoer struct {
	mu       sync.Mutex
	requests []recordedControlRequest
}

func (d *recordingControlDoer) Do(request *http.Request) (*http.Response, error) {
	body, errRead := io.ReadAll(request.Body)
	if errRead != nil {
		return nil, errRead
	}
	d.mu.Lock()
	d.requests = append(d.requests, recordedControlRequest{
		method: request.Method,
		path:   request.URL.Path,
		header: request.Header.Clone(),
		body:   append([]byte(nil), body...),
	})
	d.mu.Unlock()
	payload := `{}`
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
		payload = `{"session":{"id":"cse_test-session"}}`
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/bridge"):
		payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"worker-token"}`
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/worker/heartbeat"):
		payload = `{"heartbeat_interval_seconds":20}`
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/worker/events"):
		payload = `{"results":[]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    request,
	}, nil
}

func (d *recordingControlDoer) snapshot() []recordedControlRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedControlRequest(nil), d.requests...)
}

func TestManagerRunsCapturedLifecycleAndProtectsWorkerCredential(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	auth := testDesktopAuth(t)
	doer := &recordingControlDoer{}
	manager := NewManager(Options{
		StatePath:    t.TempDir(),
		Bundle:       bundle,
		DisableLoops: true,
		Hostname: func() (string, error) {
			return "WIN-H5SN36Q3260", nil
		},
		WorkingDir: `C:\code`,
		Now: func() time.Time {
			return time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
		},
		DoerFactory: func(_ context.Context, role string, _ *cliproxyauth.Auth) (HTTPDoer, error) {
			_ = role
			return doer, nil
		},
	})
	if errEnsure := manager.EnsureSession(context.Background(), auth, "local-session", "claude-sonnet-5"); errEnsure != nil {
		t.Fatalf("EnsureSession() error = %v", errEnsure)
	}

	initial := doer.snapshot()
	if len(initial) != 5 {
		t.Fatalf("initial request count = %d, want 5", len(initial))
	}
	wantInitial := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/code/sessions"},
		{http.MethodPost, "/v1/code/sessions/cse_test-session/bridge"},
		{http.MethodGet, "/v1/code/sessions/cse_test-session/worker"},
		{http.MethodPut, "/v1/code/sessions/cse_test-session/worker"},
		{http.MethodPut, "/v1/code/sessions/cse_test-session/worker"},
	}
	for index, want := range wantInitial {
		if initial[index].method != want.method || initial[index].path != want.path {
			t.Fatalf("initial[%d] = %s %s, want %s %s", index, initial[index].method, initial[index].path, want.method, want.path)
		}
	}
	if got := initial[0].header.Get("Authorization"); got != "Bearer account-token" {
		t.Fatalf("create authorization = %q", got)
	}
	if got := initial[2].header.Get("Authorization"); got != "Bearer worker-token" {
		t.Fatalf("worker authorization = %q", got)
	}
	var createBody createSessionRequest
	if errJSON := json.Unmarshal(initial[0].body, &createBody); errJSON != nil {
		t.Fatal(errJSON)
	}
	if createBody.Config.Model != "claude-sonnet-5" || createBody.Config.CWD != `C:\code` || len(createBody.Tags) != 1 || createBody.Tags[0] != "remote-control-sdk" {
		t.Fatalf("create body = %#v", createBody)
	}
	if !strings.HasPrefix(createBody.Title, "win-h5sn36q3260-") {
		t.Fatalf("create title = %q", createBody.Title)
	}
	var permissionBody struct {
		WorkerEpoch      int64 `json:"worker_epoch"`
		ExternalMetadata struct {
			PermissionMode string `json:"permission_mode"`
		} `json:"external_metadata"`
	}
	if errJSON := json.Unmarshal(initial[4].body, &permissionBody); errJSON != nil {
		t.Fatal(errJSON)
	}
	if permissionBody.WorkerEpoch != 1 || permissionBody.ExternalMetadata.PermissionMode != "auto" {
		t.Fatalf("permission body = %#v", permissionBody)
	}

	stateFiles, errGlob := filepath.Glob(filepath.Join(manager.root, "control-plane", "sessions", "*.json"))
	if errGlob != nil || len(stateFiles) != 1 {
		t.Fatalf("state files = %#v, error = %v", stateFiles, errGlob)
	}
	protected, errRead := os.ReadFile(stateFiles[0])
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(protected), "worker-token") || strings.Contains(string(protected), "local-session") {
		t.Fatal("protected state contains plaintext runtime identity or worker credential")
	}

	manager.Close()
	manager.Close()
	requests := doer.snapshot()
	if len(requests) != 8 {
		t.Fatalf("total request count = %d, want 8", len(requests))
	}
	if requests[5].path != "/v1/code/sessions/cse_test-session/worker/events" || requests[6].path != requests[5].path || requests[7].path != "/v1/sessions/session_test-session/archive" {
		t.Fatalf("shutdown order = %s, %s, %s", requests[5].path, requests[6].path, requests[7].path)
	}
	if got := requests[7].header.Get("Authorization"); got != "Bearer account-token" {
		t.Fatalf("archive authorization = %q", got)
	}
	if got := requests[7].header.Get("x-organization-uuid"); got != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" {
		t.Fatalf("archive organization binding = %q", got)
	}
	var shutdown struct {
		WorkerEpoch int64 `json:"worker_epoch"`
		Events      []struct {
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	if errJSON := json.Unmarshal(requests[5].body, &shutdown); errJSON != nil {
		t.Fatal(errJSON)
	}
	if shutdown.WorkerEpoch != 1 || len(shutdown.Events) != 1 || shutdown.Events[0].Payload["subtype"] != "worker_shutting_down" || shutdown.Events[0].Payload["reason"] != "host_exit" {
		t.Fatalf("shutdown event = %#v", shutdown)
	}
	var result struct {
		Events []struct {
			Payload struct {
				Type       string `json:"type"`
				Subtype    string `json:"subtype"`
				DurationMS int    `json:"duration_ms"`
				NumTurns   int    `json:"num_turns"`
				Result     string `json:"result"`
			} `json:"payload"`
		} `json:"events"`
	}
	if errJSON := json.Unmarshal(requests[6].body, &result); errJSON != nil {
		t.Fatal(errJSON)
	}
	if len(result.Events) != 1 || result.Events[0].Payload.Type != "result" || result.Events[0].Payload.Subtype != "success" || result.Events[0].Payload.DurationMS != 0 || result.Events[0].Payload.NumTurns != 0 || result.Events[0].Payload.Result != "" {
		t.Fatalf("zero-turn result = %#v", result)
	}
}

func TestRequestSpanReplaysCapturedMainTurnOrderAndPayloads(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &recordingControlDoer{}
	now := time.Date(2026, 9, 3, 19, 20, 6, 0, time.UTC)
	manager := NewManager(Options{
		StatePath:    t.TempDir(),
		Bundle:       bundle,
		DisableLoops: true,
		Now:          func() time.Time { return now },
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return doer, nil
		},
	})
	t.Cleanup(manager.Close)

	titleSpan, errTitle := manager.BeginRequest(context.Background(), testDesktopAuth(t), RequestFacts{
		Role:           claudeprofile.RoleTitle,
		LocalSessionID: "local-request-session",
		Model:          "claude-haiku-4-5-20251001",
		Attempt:        1,
	})
	if errTitle != nil {
		t.Fatal(errTitle)
	}
	titleSpan.ObserveResponsePayload([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-haiku-4-5-20251001\",\"id\":\"msg_title\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null,\"usage\":{},\"diagnostics\":null}}\n"+
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"{\\\"title\\\":\\\"Aligned title\\\"}\"}}\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n"), true)
	titleSpan.FinishSuccess(context.Background())

	requestBody := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":[{"type":"text","text":"synthetic reminder"},{"type":"text","text":"actual prompt"}]}],"output_config":{"effort":"high"}}`)
	span, errBegin := manager.BeginRequest(context.Background(), testDesktopAuth(t), RequestFacts{
		Role:           claudeprofile.RoleMain,
		LocalSessionID: "local-request-session",
		PromptID:       "prompt-one",
		Model:          "claude-sonnet-5",
		PermissionMode: "auto",
		Effort:         "high",
		Attempt:        1,
		Body:           requestBody,
	})
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	responseHeaders := http.Header{
		"request-id":                                          {"req_test"},
		"anthropic-ratelimit-unified-status":                  {"allowed"},
		"anthropic-ratelimit-unified-reset":                   {"1788477000"},
		"anthropic-ratelimit-unified-representative-claim":    {"five_hour"},
		"anthropic-ratelimit-unified-overage-status":          {"rejected"},
		"anthropic-ratelimit-unified-overage-disabled-reason": {"org_level_disabled"},
		"anthropic-ratelimit-unified-5h-reset":                {"1788477000"},
		"anthropic-ratelimit-unified-5h-utilization":          {"0.02"},
		"anthropic-ratelimit-unified-7d-reset":                {"1788958800"},
		"anthropic-ratelimit-unified-7d-utilization":          {"0"},
	}
	span.ObserveHTTPResponse(responseHeaders)
	span.ObserveResponsePayload([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\",\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null,\"usage\":{\"input_tokens\":2,\"cache_creation_input_tokens\":10,\"cache_read_input_tokens\":20,\"output_tokens\":1,\"service_tier\":\"standard\"},\"diagnostics\":null}}\n"+
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"aligned response\"}}\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n"), true)
	span.FinishSuccess(context.Background())

	requests := doer.snapshot()
	if len(requests) != 11 {
		t.Fatalf("request count = %d, want 11", len(requests))
	}
	wantTail := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/code/sessions/cse_test-session/worker/events"},
		{http.MethodGet, "/v1/sessions/session_test-session"},
		{http.MethodPut, "/v1/code/sessions/cse_test-session/worker"},
		{http.MethodPost, "/v1/code/sessions/cse_test-session/worker/events"},
		{http.MethodPatch, "/v1/sessions/session_test-session"},
		{http.MethodPost, "/v1/code/sessions/cse_test-session/worker/events"},
	}
	for index, want := range wantTail {
		got := requests[index+5]
		if got.method != want.method || got.path != want.path {
			t.Fatalf("request[%d] = %s %s, want %s %s", index+5, got.method, got.path, want.method, want.path)
		}
	}

	var user workerEventRequest
	if errJSON := json.Unmarshal(requests[5].body, &user); errJSON != nil {
		t.Fatal(errJSON)
	}
	userPayload, errJSON := json.Marshal(user.Events[0].Payload)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	var postedUser userEventPayload
	if errJSON = json.Unmarshal(userPayload, &postedUser); errJSON != nil {
		t.Fatal(errJSON)
	}
	if postedUser.Message.Content != "actual prompt" || postedUser.Origin.Kind != "human" {
		t.Fatalf("user event = %#v", postedUser)
	}

	var running workerRunningPayload
	if errJSON = json.Unmarshal(requests[7].body, &running); errJSON != nil {
		t.Fatal(errJSON)
	}
	if running.WorkerStatus != "running" || running.ExternalMetadata.EffortLevel != "high" || running.ExternalMetadata.RateLimitInfo == nil || running.ExternalMetadata.RateLimitInfo.RateLimitType != "five_hour" {
		t.Fatalf("running state = %#v", running)
	}

	var batch workerEventRequest
	if errJSON = json.Unmarshal(requests[8].body, &batch); errJSON != nil {
		t.Fatal(errJSON)
	}
	if len(batch.Events) != 5 {
		t.Fatalf("batch event count = %d, want 5", len(batch.Events))
	}
	wantTypes := []string{"system/background_tasks_changed", "rate_limit_event/", "system/status", "system/init", "assistant/"}
	for index, event := range batch.Events {
		payload, _ := json.Marshal(event.Payload)
		var identity struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		_ = json.Unmarshal(payload, &identity)
		if got := identity.Type + "/" + identity.Subtype; got != wantTypes[index] {
			t.Fatalf("batch[%d] = %q, want %q", index, got, wantTypes[index])
		}
	}
	assistantJSON, _ := json.Marshal(batch.Events[4].Payload)
	var assistant assistantEventPayload
	if errJSON = json.Unmarshal(assistantJSON, &assistant); errJSON != nil {
		t.Fatal(errJSON)
	}
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if errJSON = json.Unmarshal(assistant.Message.Content, &content); errJSON != nil {
		t.Fatal(errJSON)
	}
	if assistant.RequestID != "req_test" || assistant.Message.ID != "msg_test" || content.Type != "text" || content.Text != "aligned response" {
		t.Fatalf("assistant event = %#v", assistant)
	}
	var usage struct {
		OutputTokens int `json:"output_tokens"`
	}
	if errJSON = json.Unmarshal(assistant.Message.Usage, &usage); errJSON != nil || usage.OutputTokens != 1 {
		t.Fatalf("assistant initial usage = %s, error = %v", assistant.Message.Usage, errJSON)
	}

	var title struct {
		Title string `json:"title"`
	}
	if errJSON = json.Unmarshal(requests[9].body, &title); errJSON != nil || title.Title != "Aligned title" {
		t.Fatalf("title patch = %#v, error = %v", title, errJSON)
	}
	var result workerEventRequest
	if errJSON = json.Unmarshal(requests[10].body, &result); errJSON != nil {
		t.Fatal(errJSON)
	}
	resultJSON, _ := json.Marshal(result.Events[0].Payload)
	var resultPayload zeroTurnResultPayload
	if errJSON = json.Unmarshal(resultJSON, &resultPayload); errJSON != nil || resultPayload.Subtype != "success" || resultPayload.IsError || resultPayload.NumTurns != 0 {
		t.Fatalf("result event = %#v, error = %v", resultPayload, errJSON)
	}

	batchText := string(requests[8].body)
	if strings.Index(batchText, `"worker_epoch"`) > strings.Index(batchText, `"events"`) ||
		strings.Index(batchText, `"background_tasks_changed"`) > strings.Index(batchText, `"rate_limit_event"`) ||
		!strings.Contains(batchText, `"content":{"type":"text","text":"aligned response"}`) {
		t.Fatalf("batch JSON key/event order drifted: %s", batchText)
	}
}

func TestRequestSpanReplaysThinkingAndToolBlocksInCapturedOrder(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &recordingControlDoer{}
	manager := NewManager(Options{
		StatePath:    t.TempDir(),
		Bundle:       bundle,
		DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return doer, nil
		},
	})
	t.Cleanup(manager.Close)

	span, errBegin := manager.BeginRequest(context.Background(), testDesktopAuth(t), RequestFacts{
		Role:           claudeprofile.RoleMain,
		LocalSessionID: "thinking-tool-session",
		PromptID:       "thinking-tool-prompt",
		Model:          "claude-sonnet-5",
		PermissionMode: "auto",
		Effort:         "high",
		Attempt:        1,
		Body:           []byte(`{"messages":[{"role":"user","content":"delegate"}]}`),
	})
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	span.ObserveHTTPResponse(http.Header{"request-id": {"req_thinking_tool"}})
	signature := strings.Repeat("AAAA", 80) // 240 decoded bytes, matching Desktop's ceil(bytes/4) estimate of 60.
	span.ObserveResponsePayload([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\",\"id\":\"msg_thinking_tool\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null,\"usage\":{\"input_tokens\":2,\"cache_creation_input_tokens\":10,\"cache_read_input_tokens\":20,\"output_tokens\":1,\"service_tier\":\"standard\"},\"diagnostics\":null}}\n"+
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"\",\"estimated_tokens\":50}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\""+signature+"\"}}\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n"+
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_task\",\"name\":\"Agent\",\"input\":{}}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"description\\\":\\\"inspect\\\",\\\"prompt\\\":\\\"check\\\",\\\"subagent_type\\\":\\\"general-purpose\\\",\\\"run_in_background\\\":false}\"}}\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":1}\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":20}}\n"), true)
	span.FinishSuccess(context.Background())

	type capturedPayload struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content json.RawMessage `json:"content"`
			Usage   json.RawMessage `json:"usage"`
		} `json:"message"`
		EstimatedTokens      int `json:"estimated_tokens"`
		EstimatedTokensDelta int `json:"estimated_tokens_delta"`
	}
	var captured []capturedPayload
	var rawCaptured []string
	for _, request := range doer.snapshot() {
		if !strings.HasSuffix(request.path, "/worker/events") {
			continue
		}
		var envelope struct {
			Events []struct {
				Payload json.RawMessage `json:"payload"`
			} `json:"events"`
		}
		if json.Unmarshal(request.body, &envelope) != nil {
			continue
		}
		for _, event := range envelope.Events {
			var payload capturedPayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.Subtype == "thinking_tokens" || payload.Type == "assistant" {
				captured = append(captured, payload)
				rawCaptured = append(rawCaptured, string(event.Payload))
			}
		}
	}
	if len(captured) != 4 {
		t.Fatalf("captured response event count = %d, want 4", len(captured))
	}
	if captured[0].Subtype != "thinking_tokens" || captured[0].EstimatedTokens != 50 || captured[0].EstimatedTokensDelta != 50 {
		t.Fatalf("first thinking token event = %#v", captured[0])
	}
	if captured[1].Subtype != "thinking_tokens" || captured[1].EstimatedTokens != 60 || captured[1].EstimatedTokensDelta != 10 {
		t.Fatalf("signature thinking token event = %#v", captured[1])
	}
	var thinking struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	if errJSON := json.Unmarshal(captured[2].Message.Content, &thinking); errJSON != nil || thinking.Type != "thinking" || thinking.Thinking != "" || thinking.Signature != signature {
		t.Fatalf("thinking assistant block = %#v, error = %v", thinking, errJSON)
	}
	var tool struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		Caller struct {
			Type string `json:"type"`
		} `json:"caller"`
	}
	if errJSON := json.Unmarshal(captured[3].Message.Content, &tool); errJSON != nil || tool.Type != "tool_use" || tool.ID != "toolu_task" || tool.Name != "Agent" || tool.Caller.Type != "direct" {
		t.Fatalf("tool assistant block = %#v, error = %v", tool, errJSON)
	}
	for index := 2; index <= 3; index++ {
		var initialUsage struct {
			OutputTokens int `json:"output_tokens"`
		}
		if errJSON := json.Unmarshal(captured[index].Message.Usage, &initialUsage); errJSON != nil || initialUsage.OutputTokens != 1 {
			t.Fatalf("assistant[%d] initial usage = %s, error = %v", index, captured[index].Message.Usage, errJSON)
		}
	}
	if raw := rawCaptured[0]; strings.Index(raw, `"type":"system"`) > strings.Index(raw, `"subtype":"thinking_tokens"`) || strings.Index(raw, `"estimated_tokens":50`) > strings.Index(raw, `"estimated_tokens_delta":50`) {
		t.Fatalf("thinking token JSON key order drifted: %s", raw)
	}
	if raw := rawCaptured[2]; !strings.Contains(raw, `"content":{"type":"thinking","thinking":"","signature":"`) {
		t.Fatalf("thinking content JSON shape drifted: %s", raw)
	}
	if raw := rawCaptured[3]; !strings.Contains(raw, `"content":{"type":"tool_use","id":"toolu_task","name":"Agent","input":`) || strings.Index(raw, `"input":`) > strings.Index(raw, `"caller":{"type":"direct"}`) {
		t.Fatalf("tool content JSON key order drifted: %s", raw)
	}
}

func TestRequestSpanDoesNotInventTaskFromObservedAgentToolUse(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &recordingControlDoer{}
	now := time.Date(2026, 9, 3, 19, 27, 26, 0, time.UTC)
	manager := NewManager(Options{
		StatePath:    t.TempDir(),
		Bundle:       bundle,
		DisableLoops: true,
		WorkingDir:   `C:\code`,
		Now:          func() time.Time { return now },
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return doer, nil
		},
	})
	t.Cleanup(manager.Close)
	auth := testDesktopAuth(t)

	mainSpan, errMain := manager.BeginRequest(context.Background(), auth, RequestFacts{
		Role:           claudeprofile.RoleMain,
		LocalSessionID: "subagent-session",
		PromptID:       "main-prompt",
		Model:          "claude-sonnet-5",
		PermissionMode: "auto",
		Effort:         "high",
		Attempt:        1,
		Body:           []byte(`{"messages":[{"role":"user","content":"delegate"}]}`),
	})
	if errMain != nil {
		t.Fatal(errMain)
	}
	mainSpan.ObserveHTTPResponse(http.Header{"request-id": {"req_parent"}})
	mainSpan.ObserveResponsePayload([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\",\"id\":\"msg_parent\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":1},\"diagnostics\":null}}\n"+
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_agent\",\"name\":\"Agent\",\"input\":{}}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"description\\\":\\\"inspect repository\\\",\\\"prompt\\\":\\\"trace the lifecycle\\\",\\\"subagent_type\\\":\\\"general-purpose\\\",\\\"run_in_background\\\":false}\"}}\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":20}}\n"), true)
	mainSpan.FinishSuccess(context.Background())

	now = now.Add(time.Second)
	subagentBody := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"agent preamble"},{"type":"text","text":"trace the lifecycle"}]},{"role":"system","content":[{"type":"text","text":"runtime reminder"}]}]}`)
	subagentSpan, errSubagent := manager.BeginRequest(context.Background(), auth, RequestFacts{
		Role:           claudeprofile.RoleSubagent,
		LocalSessionID: "subagent-session",
		PromptID:       "child-prompt",
		Model:          "claude-sonnet-5",
		PermissionMode: "auto",
		Effort:         "high",
		Attempt:        1,
		Body:           subagentBody,
	})
	if errSubagent != nil {
		t.Fatal(errSubagent)
	}
	subagentSpan.ObserveHTTPResponse(http.Header{"request-id": {"req_child"}})
	subagentSpan.ObserveResponsePayload([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\",\"id\":\"msg_child\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"stop_details\":null,\"usage\":{\"input_tokens\":2,\"cache_creation_input_tokens\":100,\"cache_read_input_tokens\":20,\"output_tokens\":2},\"diagnostics\":null}}\n"+
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n"+
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"child result\"}}\n"+
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n"+
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":2,\"cache_creation_input_tokens\":100,\"cache_read_input_tokens\":20,\"output_tokens\":30}}\n"), true)
	now = now.Add(2588 * time.Millisecond)
	subagentSpan.FinishSuccess(context.Background())

	type lifecyclePayload struct {
		Type           string `json:"type"`
		Subtype        string `json:"subtype"`
		TaskID         string `json:"task_id"`
		ToolUseID      string `json:"tool_use_id"`
		Description    string `json:"description"`
		Prompt         string `json:"prompt"`
		SubagentType   string `json:"subagent_type"`
		IsBackgrounded bool   `json:"is_backgrounded"`
		SpawnDepth     int    `json:"spawn_depth"`
		TaskType       string `json:"task_type"`
		OutputFile     string `json:"output_file"`
		Summary        string `json:"summary"`
		Patch          struct {
			Status  string `json:"status"`
			EndTime int64  `json:"end_time"`
		} `json:"patch"`
		Usage   taskNotificationUsage `json:"usage"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var lifecycle []lifecyclePayload
	userEvents := 0
	for _, request := range doer.snapshot() {
		if !strings.HasSuffix(request.path, "/worker/events") {
			continue
		}
		var envelope struct {
			Events []struct {
				Payload json.RawMessage `json:"payload"`
			} `json:"events"`
		}
		if json.Unmarshal(request.body, &envelope) != nil {
			continue
		}
		for _, event := range envelope.Events {
			var payload lifecyclePayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.Type == "user" {
				userEvents++
			}
			blockTypeName := blockType(payload.Message.Content)
			if (payload.Type == "assistant" && blockTypeName == "tool_use") || strings.HasPrefix(payload.Subtype, "task_") {
				lifecycle = append(lifecycle, payload)
			}
		}
	}
	if len(lifecycle) != 1 || lifecycle[0].Type != "assistant" || lifecycle[0].TaskID != "" {
		t.Fatalf("observation fabricated an executable task: %#v", lifecycle)
	}
	if userEvents != 1 {
		t.Fatalf("user event count = %d, want only the parent request event", userEvents)
	}
}

func TestRequestSpanSuppressesTerminalFailureWhenRetryIsScheduled(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &recordingControlDoer{}
	manager := NewManager(Options{
		StatePath:    t.TempDir(),
		Bundle:       bundle,
		DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return doer, nil
		},
	})
	t.Cleanup(manager.Close)
	auth := testDesktopAuth(t)
	facts := RequestFacts{
		Role:           claudeprofile.RoleMain,
		LocalSessionID: "retry-session",
		PromptID:       "retry-prompt",
		Model:          "claude-sonnet-5",
		PermissionMode: "auto",
		Effort:         "high",
		Attempt:        1,
		Body:           []byte(`{"messages":[{"role":"user","content":"retry me"}]}`),
	}
	first, errBegin := manager.BeginRequest(context.Background(), auth, facts)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	first.ObserveHTTPResponse(http.Header{"request-id": {"req_failed"}})
	first.FinishFailure(context.Background(), &statusError{code: http.StatusBadGateway})
	first.RecordScheduledRetry()
	time.Sleep(controlFailureSettleDelay + 100*time.Millisecond)
	if got := len(doer.snapshot()); got != 7 {
		t.Fatalf("request count after scheduled retry = %d, want 7 without terminal worker events", got)
	}

	facts.Attempt = 2
	second, errRetry := manager.BeginRequest(context.Background(), auth, facts)
	if errRetry != nil {
		t.Fatal(errRetry)
	}
	second.ObserveHTTPResponse(http.Header{"request-id": {"req_success"}})
	second.ObserveResponsePayload([]byte(`{"model":"claude-sonnet-5","id":"msg_success","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"usage":{"input_tokens":1,"output_tokens":1},"diagnostics":null}`), false)
	second.FinishSuccess(context.Background())

	requests := doer.snapshot()
	if len(requests) != 10 {
		t.Fatalf("request count after successful retry = %d, want 10", len(requests))
	}
	userEvents := 0
	resultEvents := 0
	for _, request := range requests {
		if !strings.HasSuffix(request.path, "/worker/events") {
			continue
		}
		var envelope workerEventRequest
		if json.Unmarshal(request.body, &envelope) != nil {
			continue
		}
		for _, event := range envelope.Events {
			payload, _ := json.Marshal(event.Payload)
			var identity struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(payload, &identity)
			switch identity.Type {
			case "user":
				userEvents++
			case "result":
				resultEvents++
			}
		}
	}
	if userEvents != 1 || resultEvents != 1 {
		t.Fatalf("retry event counts: user=%d result=%d", userEvents, resultEvents)
	}
}

type refreshingControlDoer struct {
	mu          sync.Mutex
	bridgeCount int
	requests    []recordedControlRequest
}

func (d *refreshingControlDoer) Do(request *http.Request) (*http.Response, error) {
	body, errRead := io.ReadAll(request.Body)
	if errRead != nil {
		return nil, errRead
	}
	d.mu.Lock()
	d.requests = append(d.requests, recordedControlRequest{method: request.Method, path: request.URL.Path, header: request.Header.Clone(), body: body})
	payload := `{}`
	status := http.StatusOK
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/code/sessions":
		payload = `{"session":{"id":"cse_refresh"}}`
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/bridge"):
		d.bridgeCount++
		payload = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"worker-` + strconv.Itoa(d.bridgeCount) + `"}`
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/worker") && request.Header.Get("Authorization") == "Bearer worker-1":
		status = http.StatusUnauthorized
	}
	d.mu.Unlock()
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
}

func TestManagerWorkerReadRetriesKeepNativeCredentialSnapshot(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &refreshingControlDoer{}
	manager := NewManager(Options{
		StatePath:    t.TempDir(),
		Bundle:       bundle,
		DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) {
			return doer, nil
		},
	})
	manager.workerReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(manager.Close)
	if errEnsure := manager.EnsureSession(context.Background(), testDesktopAuth(t), "refresh-session", "claude-opus-5"); errEnsure != nil {
		t.Fatalf("EnsureSession() error = %v", errEnsure)
	}
	doer.mu.Lock()
	bridgeCount := doer.bridgeCount
	requests := append([]recordedControlRequest(nil), doer.requests...)
	doer.mu.Unlock()
	if bridgeCount != 1 {
		t.Fatalf("worker GET switched credential snapshots: bridge count = %d, want 1", bridgeCount)
	}
	var workerReads []recordedControlRequest
	for _, request := range requests {
		if request.method == http.MethodGet && strings.HasSuffix(request.path, "/worker") {
			workerReads = append(workerReads, request)
		}
	}
	if len(workerReads) != 10 {
		t.Fatalf("worker GET attempts = %d, want native ten-attempt budget", len(workerReads))
	}
	for _, read := range workerReads {
		if read.header.Get("Authorization") != "Bearer worker-1" {
			t.Fatal("GET adopted another worker credential")
		}
	}
	if status := manager.Status(); status.WorkerStateReadFailed != 1 || status.InitializationFailed != 0 {
		t.Fatal("failed read did not register with visible degraded restoration", status)
	}
}

func testDesktopAuth(t *testing.T) *cliproxyauth.Auth {
	t.Helper()
	accountUUID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	organizationUUID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	deviceID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	authID, errAuthID := claudedesktop.StableAuthID(accountUUID, organizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	device := claudedesktop.TrustedDevice{DeviceID: deviceID, DeviceToken: "trusted-device-token", DisplayName: "Desktop"}
	enrollment := claudedesktop.NewEnrollment(authID, claudedesktop.AccountIdentity{
		AccountUUID: accountUUID, OrganizationUUID: organizationUUID,
	}, device, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	return &cliproxyauth.Auth{
		ID:       authID,
		Provider: claudedesktop.Provider,
		Metadata: map[string]any{
			"access_token":                              "account-token",
			"account_uuid":                              accountUUID,
			"organization_uuid":                         organizationUUID,
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			"claude_device_ids":                         []string{claudedesktop.RequestDeviceID(deviceID)},
		},
	}
}
