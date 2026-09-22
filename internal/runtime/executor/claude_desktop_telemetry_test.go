package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type claudeDesktopTelemetryTestDoer struct {
	mu       sync.Mutex
	requests []claudeDesktopTelemetryRecordedRequest
}

type claudeDesktopTelemetryRecordedRequest struct {
	URL    string
	Header http.Header
	Body   []byte
}

func (d *claudeDesktopTelemetryTestDoer) Do(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	d.mu.Lock()
	d.requests = append(d.requests, claudeDesktopTelemetryRecordedRequest{URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
	d.mu.Unlock()
	proto, protoMajor, protoMinor := "HTTP/2.0", 2, 0
	if request.URL.Hostname() == "api.anthropic.com" {
		proto, protoMajor, protoMinor = "HTTP/1.1", 1, 1
	}
	return &http.Response{StatusCode: http.StatusOK, Proto: proto, ProtoMajor: protoMajor, ProtoMinor: protoMinor, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func (d *claudeDesktopTelemetryTestDoer) Body() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		return nil
	}
	return append([]byte(nil), d.requests[len(d.requests)-1].Body...)
}

func (d *claudeDesktopTelemetryTestDoer) Requests() []claudeDesktopTelemetryRecordedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	requests := make([]claudeDesktopTelemetryRecordedRequest, len(d.requests))
	for index, request := range d.requests {
		requests[index] = claudeDesktopTelemetryRecordedRequest{
			URL:    request.URL,
			Header: request.Header.Clone(),
			Body:   append([]byte(nil), request.Body...),
		}
	}
	return requests
}

func newClaudeDesktopTelemetryTestAuth(t *testing.T) *cliproxyauth.Auth {
	t.Helper()
	identity := claudedesktop.AccountIdentity{
		AccountUUID:      "11111111-1111-4111-8111-111111111111",
		OrganizationUUID: "22222222-2222-4222-8222-222222222222",
	}
	device := claudedesktop.TrustedDevice{
		DeviceID:    "33333333-3333-4333-8333-333333333333",
		DeviceToken: "trusted-device",
		DisplayName: "Desktop",
	}
	authID, errAuthID := claudedesktop.StableAuthID(identity.AccountUUID, identity.OrganizationUUID)
	if errAuthID != nil {
		t.Fatal(errAuthID)
	}
	enrollment := claudedesktop.NewEnrollment(authID, identity, device, time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	return &cliproxyauth.Auth{
		ID:       authID,
		Provider: "claude",
		Metadata: map[string]any{
			claudedesktop.MetadataAuthFlowKey:           claudedesktop.AuthFlowDesktop,
			claudedesktop.MetadataEnrollmentKey:         enrollment,
			claudedesktop.MetadataTrustedDeviceTokenKey: device.DeviceToken,
			"access_token":      "test-access-token",
			"account_uuid":      identity.AccountUUID,
			"organization_uuid": identity.OrganizationUUID,
			"claude_device_ids": []string{claudedesktop.RequestDeviceID(device.DeviceID)},
		},
	}
}

func TestClaudeDesktopTelemetryFactsStayRequestScoped(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &claudeDesktopTelemetryTestDoer{}
	manager := claudetelemetry.NewManager(claudetelemetry.Options{
		StatePath: t.TempDir(),
		Bundle:    bundle,
		DoerFactory: func(string) claudetelemetry.HTTPDoer {
			return doer
		},
	})
	t.Cleanup(manager.Close)
	executor := &ClaudeExecutor{desktopTelemetry: manager}
	auth := newClaudeDesktopTelemetryTestAuth(t)
	facts := claudeDesktopRuntimeFacts{
		SessionID:       "44444444-4444-4444-8444-444444444444",
		PromptID:        "55555555-5555-4555-8555-555555555555",
		ClientRequestID: "66666666-6666-4666-8666-666666666666",
		LogicalModel:    "claude-opus-5",
	}
	body := []byte(`{"tools":[{"name":"Read"},{"name":"mcp__files__read"},{"name":"mcp__files__write"},{"name":"mcp__browser__navigate"}]}`)
	span := executor.beginClaudeDesktopTelemetry(
		context.Background(),
		auth,
		claudeprofile.RoleMain,
		facts,
		body,
		map[string]any{"permission_mode": "acceptEdits"},
	)
	span.FinishSuccess(context.Background())
	if errFlush := manager.Flush(context.Background()); errFlush != nil {
		t.Fatal(errFlush)
	}

	var batch struct {
		Events []struct {
			EventData struct {
				EventName string `json:"event_name"`
				Metadata  string `json:"metadata"`
			} `json:"event_data"`
		} `json:"events"`
	}
	if errDecode := json.Unmarshal(doer.Body(), &batch); errDecode != nil {
		t.Fatal(errDecode)
	}
	var sessionMetadata map[string]any
	for _, event := range batch.Events {
		if event.EventData.EventName == "desktop_ccd_session_initialized" {
			if errMetadata := json.Unmarshal([]byte(event.EventData.Metadata), &sessionMetadata); errMetadata != nil {
				t.Fatal(errMetadata)
			}
		}
	}
	if sessionMetadata["mcp_server_count"] != float64(2) || sessionMetadata["permission_mode"] != "acceptEdits" {
		t.Fatalf("session metadata = %#v", sessionMetadata)
	}

	helperSpan := executor.beginClaudeDesktopTelemetry(context.Background(), auth, claudeprofile.RoleTitle, facts, body)
	if helperSpan == nil || !helperSpan.Active() {
		t.Fatal("title helper did not receive its independent SDK telemetry span")
	}
	helperSpan.ObserveRequest(body, http.Header{"anthropic-beta": {"oauth-2025-04-20"}})
	helperSpan.FinishSuccess(context.Background())
	statuses := manager.Status().Accounts
	var rendererPending, sdkPending int
	for _, status := range statuses {
		switch status.EndpointRole {
		case "renderer-event-logging":
			rendererPending += status.Pending
		case "sdk-event-logging":
			sdkPending += status.Pending
		}
	}
	if rendererPending != 0 {
		t.Fatalf("title helper inherited renderer telemetry: pending=%d", rendererPending)
	}
	if sdkPending == 0 {
		t.Fatal("title helper did not queue an SDK event")
	}
}

func TestClaudeDesktopMCPServerCountIgnoresBuiltinAndMalformedTools(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Read"},{"name":"mcp__files__read"},{"name":"mcp__files__write"},{"name":"mcp____missing"},{"name":"mcp__browser__navigate"}]}`)
	if got := claudeDesktopMCPServerCount(body); got != 2 {
		t.Fatalf("MCP server count = %d, want 2", got)
	}
}

func TestClaudeDesktopResponseMetricsMatchJavaScriptLengths(t *testing.T) {
	payload := []byte(`{"stop_reason":"tool_use","content":[{"type":"thinking","thinking":"想😀"},{"type":"text","text":"A😀"},{"type":"tool_use","name":"Read","input":{"path":"😀"}}]}`)
	metrics := claudeDesktopResponseContentMetrics(payload)
	if metrics.stopReason != "tool_use" || metrics.textContentLength != 3 {
		t.Fatalf("response metrics = %+v", metrics)
	}
	if thinking := metrics.thinkingLength(); thinking == nil || *thinking != 3 {
		t.Fatalf("thinkingContentLength = %#v, want 3", thinking)
	}
	wantToolLength := claudeDesktopCompactJSONLength(`{"path":"😀"}`)
	if got := metrics.toolUseContentLengths()["Read"]; got != wantToolLength {
		t.Fatalf("toolUseContentLengths.Read = %d, want %d", got, wantToolLength)
	}

	stream := claudeDesktopResponseMetricState{}
	for _, line := range [][]byte{
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想😀"}}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"A😀"}}`),
		[]byte(`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Read","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"😀\"}"}}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`),
	} {
		stream.observeStreamLine(line)
	}
	if stream.stopReason != "tool_use" || stream.textContentLength != 3 {
		t.Fatalf("stream metrics = %+v", stream)
	}
	if thinking := stream.thinkingLength(); thinking == nil || *thinking != 3 {
		t.Fatalf("stream thinkingContentLength = %#v, want 3", thinking)
	}
	if got := stream.toolUseContentLengths()["Read"]; got != wantToolLength {
		t.Fatalf("stream toolUseContentLengths.Read = %d, want %d", got, wantToolLength)
	}
}

func TestClaudeDesktopTelemetryFailureCategory(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{err: context.Canceled, want: "cancelled"},
		{err: context.DeadlineExceeded, want: "timeout"},
		{err: statusErr{code: http.StatusUnauthorized}, want: "authentication_error"},
		{err: statusErr{code: http.StatusTooManyRequests}, want: "rate_limit"},
		{err: statusErr{code: http.StatusBadRequest}, want: "invalid_request"},
		{err: statusErr{code: http.StatusServiceUnavailable}, want: "server_error"},
		{err: &url.Error{Op: "Post", URL: "https://sensitive.invalid", Err: errors.New("connection failed")}, want: "network_error"},
		{err: errClaudeDesktopStreamIncomplete, want: "incomplete_stream"},
	}
	for _, test := range tests {
		if got := claudeDesktopTelemetryFailureCategory(test.err); got != test.want {
			t.Errorf("category(%T) = %q, want %q", test.err, got, test.want)
		}
	}
}
