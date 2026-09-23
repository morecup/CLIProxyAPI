package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeDesktopCountTokensCalibrationMatchesCapturedModelBursts(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	tests := []struct {
		model       string
		wantCount   int
		wantZero    int
		wantSingles int
		wantIntro   int
	}{
		{model: "claude-opus-4-6", wantCount: 46, wantZero: 16, wantSingles: 28, wantIntro: 6},
		{model: "claude-opus-4-7", wantCount: 46, wantZero: 16, wantSingles: 28, wantIntro: 6},
		{model: "claude-opus-4-8", wantCount: 38, wantZero: 12, wantSingles: 24, wantIntro: 1},
		{model: "claude-opus-5", wantCount: 41, wantZero: 15, wantSingles: 24, wantIntro: 1},
		{model: "claude-sonnet-4-6", wantCount: 46, wantZero: 16, wantSingles: 28, wantIntro: 6},
		{model: "claude-sonnet-5", wantCount: 42, wantZero: 16, wantSingles: 24, wantIntro: 6},
		{model: "claude-haiku-4-5-20251001", wantCount: 46, wantZero: 16, wantSingles: 28, wantIntro: 6},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			mainBody := renderClaudeDesktopCalibrationMainBody(t, executor, test.model)
			sections, errSections := claudeDesktopCalibrationSystemSections(test.model, mainBody)
			if errSections != nil {
				t.Fatal(errSections)
			}
			system := gjson.GetBytes(mainBody, "system").Array()
			if got := strings.Join(sections[:test.wantIntro], "\n\n"); got != system[2].Get("text").String() {
				t.Fatal("intro calibration sections do not reconstruct the captured main system block")
			}
			if got := strings.Join(sections[test.wantIntro:], "\n\n"); got != system[3].Get("text").String() {
				t.Fatal("session calibration sections do not reconstruct the captured main system block")
			}
			if !strings.HasPrefix(sections[len(sections)-1], "\n\nWhen referencing files") {
				t.Fatalf("last calibration section lost captured leading newlines: %q", sections[len(sections)-1][:min(len(sections[len(sections)-1]), 40)])
			}
			requests, errBuild := executor.buildClaudeDesktopCalibrationRequests(test.model, mainBody)
			if errBuild != nil {
				t.Fatal(errBuild)
			}
			if len(requests) != test.wantCount {
				t.Fatalf("requests = %d, want %d", len(requests), test.wantCount)
			}
			counts := make(map[int]int)
			for index, request := range requests {
				if !gjson.ValidBytes(request.body) {
					t.Fatalf("request %d is invalid JSON", index)
				}
				if !strings.HasPrefix(string(request.body), `{"model":`) || !strings.Contains(string(request.body), `,"messages":[`) || !strings.Contains(string(request.body), `],"tools":[`) {
					t.Fatalf("request %d has the wrong top-level order: %s", index, request.body)
				}
				for _, forbidden := range []string{"CALLER_SYSTEM_SENTINEL", "CALLER_MCP_SENTINEL"} {
					if strings.Contains(string(request.body), forbidden) {
						t.Fatalf("request %d inherited %q", index, forbidden)
					}
				}
				if gjson.GetBytes(request.body, "system").Exists() || gjson.GetBytes(request.body, "metadata").Exists() {
					t.Fatalf("request %d inherited a top-level system or metadata field", index)
				}
				counts[len(gjson.GetBytes(request.body, "tools").Array())]++
			}
			if counts[0] != test.wantZero || counts[1] != test.wantSingles || counts[17] != 1 || counts[66] != 1 {
				t.Fatalf("tool-count distribution = %#v", counts)
			}
		})
	}
}

func TestClaudeDesktopCountTokensCalibrationMatchesV270320Opus55Burst(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	mainBody := renderClaudeDesktopCalibrationMainBody(t, executor, "claude-opus-5-5")
	requests, errBuild := executor.buildClaudeDesktopCalibrationRequests("claude-opus-5-5", mainBody)
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	if len(requests) != 39 {
		t.Fatalf("requests = %d, want 39", len(requests))
	}

	toolCounts := make(map[int]int)
	for index, request := range requests {
		if !gjson.ValidBytes(request.body) {
			t.Fatalf("request %d is invalid JSON", index)
		}
		if got := gjson.GetBytes(request.body, "model").String(); got != "claude-opus-5-5" {
			t.Fatalf("request %d model = %q", index, got)
		}
		if !strings.HasPrefix(string(request.body), `{"model":`) ||
			!strings.Contains(string(request.body), `,"messages":[`) ||
			!strings.Contains(string(request.body), `],"tools":[`) {
			t.Fatalf("request %d has the wrong top-level order: %s", index, request.body)
		}
		if gjson.GetBytes(request.body, "system").Exists() || gjson.GetBytes(request.body, "metadata").Exists() {
			t.Fatalf("request %d inherited a top-level system or metadata field", index)
		}
		toolCounts[len(gjson.GetBytes(request.body, "tools").Array())]++
	}
	if toolCounts[0] != 12 || toolCounts[1] != 25 || toolCounts[17] != 1 || toolCounts[117] != 1 {
		t.Fatalf("tool-count distribution = %#v", toolCounts)
	}
	if got := gjson.GetBytes(requests[0].body, "messages.0.content").String(); got != "hello" {
		t.Fatalf("fallback dynamic content = %q, want caller user content", got)
	}
	for index := 0; index < 12; index++ {
		if got := len(gjson.GetBytes(requests[index].body, "tools").Array()); got != 0 {
			t.Fatalf("content request %d has %d tools, want 0", index, got)
		}
	}
	if got := gjson.GetBytes(requests[12].body, "tools.0.name").String(); got != "Skill" {
		t.Fatalf("skill preload tool = %q, want Skill", got)
	}
	if got := len(gjson.GetBytes(requests[13].body, "tools").Array()); got != 117 {
		t.Fatalf("MCP tool count = %d, want 117", got)
	}
	if got := len(gjson.GetBytes(requests[14].body, "tools").Array()); got != 17 {
		t.Fatalf("builtin tool count = %d, want 17", got)
	}
}

func TestClaudeDesktopCountTokensCalibrationPreservesV270320CapturedDynamicHistory(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	mainBody := renderClaudeDesktopCalibrationMainBody(t, executor, "claude-opus-5-5")
	historyMarkers := []string{
		"# Environment",
		"You are powered by the model named",
		"The following deferred tools are now available",
		"Available agent types for the Agent tool:",
		"# MCP Server Instructions",
		"The following skills are available for use with the Skill tool:",
		"While auto mode is active:",
		"<total_tokens>",
		"Today's date is",
	}
	historySections := make([]string, len(historyMarkers))
	for index, marker := range historyMarkers {
		historySections[index] = marker + "\nsection-" + string(rune('0'+index))
	}
	messages, errJSON := json.Marshal([]any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "user-zero"},
			map[string]any{"type": "text", "text": "user-one"},
			map[string]any{"type": "text", "text": "user-two"},
		}},
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": strings.Join(historySections, "\n\n")},
		}},
	})
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	mainBody, errSet := sjson.SetRawBytes(mainBody, "messages", messages)
	if errSet != nil {
		t.Fatal(errSet)
	}
	requests, errBuild := executor.buildClaudeDesktopCalibrationRequests("claude-opus-5-5", mainBody)
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	content := gjson.GetBytes(requests[0].body, "messages.0.content")
	if got := len(content.Array()); got != 12 {
		t.Fatalf("captured dynamic block count = %d, want 12", got)
	}
	for path, want := range map[string]string{
		"8.text":  "user-zero",
		"10.text": "user-one",
		"11.text": "user-two",
	} {
		if got := content.Get(path).String(); got != want {
			t.Fatalf("captured dynamic content %s = %q, want %q", path, got, want)
		}
	}
	if got := content.Get("9.text").String(); !strings.Contains(got, "Today's date is") {
		t.Fatalf("captured date history block moved: %q", got)
	}

	driftedMessages, errJSON := json.Marshal([]any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "user-zero"},
			map[string]any{"type": "text", "text": "user-one"},
			map[string]any{"type": "text", "text": "user-two"},
		}},
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "# Environment\n\ndrifted"},
		}},
	})
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	driftedBody, errSet := sjson.SetRawBytes(mainBody, "messages", driftedMessages)
	if errSet != nil {
		t.Fatal(errSet)
	}
	if _, errBuild = executor.buildClaudeDesktopCalibrationRequests("claude-opus-5-5", driftedBody); errBuild == nil || !strings.Contains(errBuild.Error(), "calibration history") {
		t.Fatalf("captured history drift error = %v", errBuild)
	}
}

func TestClaudeDesktopCountTokensCalibrationRunsV270320Opus55OncePerSession(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	mainBody := renderClaudeDesktopCalibrationMainBody(t, executor, "claude-opus-5-5")
	type observedRequest struct {
		path   string
		header http.Header
	}
	var mu sync.Mutex
	var observed []observedRequest
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, request.Body)
		mu.Lock()
		observed = append(observed, observedRequest{path: request.URL.Path, header: request.Header.Clone()})
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
			Request:    request,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor.runClaudeDesktopCountTokensCalibration(ctx, auth, claudeprofile.RoleMain, "opus55-session-one", mainBody)
	executor.runClaudeDesktopCountTokensCalibration(ctx, auth, claudeprofile.RoleMain, "opus55-session-one", mainBody)

	mu.Lock()
	requests := append([]observedRequest(nil), observed...)
	mu.Unlock()
	if len(requests) != 39 {
		t.Fatalf("requests = %d, want one 39-request burst", len(requests))
	}
	clientRequestIDs := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if request.path != "/v1/messages/count_tokens" {
			t.Fatalf("request %d path = %q", index, request.path)
		}
		for name, want := range map[string]string{
			"X-Claude-Code-Session-Id": "opus55-session-one",
			"anthropic-client-version": "2.7032.0",
			"anthropic-beta":           "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01",
		} {
			if got := headerValueFold(request.header, name); got != want {
				t.Fatalf("request %d %s = %q, want %q", index, name, got, want)
			}
		}
		if got := headerValueFold(request.header, "X-Stainless-Timeout"); got != "" {
			t.Fatalf("request %d inherited timeout %q", index, got)
		}
		requestID := headerValueFold(request.header, "X-Client-Request-Id")
		if requestID == "" {
			t.Fatalf("request %d has no client request id", index)
		}
		clientRequestIDs[requestID] = struct{}{}
	}
	if len(clientRequestIDs) != len(requests) {
		t.Fatalf("unique client request ids = %d, want %d", len(clientRequestIDs), len(requests))
	}

	executor.runClaudeDesktopCountTokensCalibration(ctx, auth, claudeprofile.RoleMain, "opus55-session-two", mainBody)
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 78 {
		t.Fatalf("requests after second session = %d, want 78", len(observed))
	}
}

func TestClaudeDesktopCountTokensCalibrationUsesIndependentHeadersAndRunsOncePerSession(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	mainBody := renderClaudeDesktopCalibrationMainBody(t, executor, "claude-opus-5")
	type observedRequest struct {
		path   string
		header http.Header
		body   []byte
	}
	var mu sync.Mutex
	var observed []observedRequest
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		observed = append(observed, observedRequest{path: request.URL.Path, header: request.Header.Clone(), body: body})
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
			Request:    request,
		}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor.runClaudeDesktopCountTokensCalibration(ctx, auth, claudeprofile.RoleMain, "session-one", mainBody)
	executor.runClaudeDesktopCountTokensCalibration(ctx, auth, claudeprofile.RoleMain, "session-one", mainBody)

	mu.Lock()
	requests := append([]observedRequest(nil), observed...)
	mu.Unlock()
	if len(requests) != 41 {
		t.Fatalf("requests = %d, want one 41-request burst", len(requests))
	}
	clientRequestIDs := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		if request.path != "/v1/messages/count_tokens" {
			t.Fatalf("request %d path = %q", index, request.path)
		}
		for name, want := range map[string]string{
			"Accept":                                    "application/json",
			"Accept-Encoding":                           "gzip, deflate, br, zstd",
			"Authorization":                             "Bearer sk-ant-oat-test-token",
			"Content-Type":                              "application/json",
			"User-Agent":                                executor.desktopProfile.Software.UserAgent,
			"X-Claude-Code-Session-Id":                  "session-one",
			"X-Stainless-Arch":                          "x64",
			"X-Stainless-Lang":                          "js",
			"X-Stainless-OS":                            "Windows",
			"X-Stainless-Package-Version":               "0.112.1",
			"X-Stainless-Retry-Count":                   "0",
			"X-Stainless-Runtime":                       "node",
			"X-Stainless-Runtime-Version":               "v26.3.0",
			"anthropic-beta":                            "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01",
			"anthropic-dangerous-direct-browser-access": "true",
			"anthropic-version":                         "2023-06-01",
			"x-app":                                     "cli",
		} {
			if got := headerValueFold(request.header, name); got != want {
				t.Fatalf("request %d %s = %q, want %q", index, name, got, want)
			}
		}
		if got := headerValueFold(request.header, "X-Stainless-Timeout"); got != "" {
			t.Fatalf("request %d inherited timeout %q", index, got)
		}
		if got := headerValueFold(request.header, "Cookie"); got != "" {
			t.Fatalf("request %d inherited Cookie %q", index, got)
		}
		requestID := headerValueFold(request.header, "X-Client-Request-Id")
		if requestID == "" {
			t.Fatalf("request %d has no client request id", index)
		}
		clientRequestIDs[requestID] = struct{}{}
	}
	if len(clientRequestIDs) != len(requests) {
		t.Fatalf("unique client request ids = %d, want %d", len(clientRequestIDs), len(requests))
	}

	executor.runClaudeDesktopCountTokensCalibration(ctx, auth, claudeprofile.RoleMain, "session-two", mainBody)
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 82 {
		t.Fatalf("requests after second session = %d, want 82", len(observed))
	}
}

func TestClaudeDesktopCountTokensCalibrationUsesCapturedPerModelBetas(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	tests := []struct {
		model string
		beta  string
	}{
		{model: "claude-opus-4-6", beta: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
		{model: "claude-opus-4-7", beta: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
		{model: "claude-opus-4-8", beta: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
		{model: "claude-opus-5", beta: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
		{model: "claude-sonnet-4-6", beta: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
		{model: "claude-sonnet-5", beta: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
		{model: "claude-haiku-4-5-20251001", beta: "oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			mainBody := renderClaudeDesktopCalibrationMainBody(t, executor, test.model)
			requests, errBuild := executor.buildClaudeDesktopCalibrationRequests(test.model, mainBody)
			if errBuild != nil {
				t.Fatal(errBuild)
			}
			plan, errPlan := executor.planClaudeDesktopRequestWithHints(requests[0].body, claudeprofile.RoleCountTokens, test.model, nil)
			if errPlan != nil {
				t.Fatal(errPlan)
			}
			req, errRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens?beta=true", strings.NewReader(string(requests[0].body)))
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			if errHeaders := executor.applyClaudeHeadersWithProfile(req, auth, "sk-ant-oat-test-token", false, nil, requests[0].body, plan, nil, "session"); errHeaders != nil {
				t.Fatal(errHeaders)
			}
			if got := headerValueFold(req.Header, "anthropic-beta"); got != test.beta {
				t.Fatalf("anthropic-beta = %q, want %q", got, test.beta)
			}
			if got := headerValueFold(req.Header, "X-Stainless-Timeout"); got != "" {
				t.Fatalf("X-Stainless-Timeout = %q, want omitted", got)
			}
		})
	}
}

func renderClaudeDesktopCalibrationMainBody(t *testing.T, executor *ClaudeExecutor, model string) []byte {
	t.Helper()
	body := []byte(`{"model":"` + model + `","max_tokens":4096,"system":"CALLER_SYSTEM_SENTINEL","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"CALLER_MCP_SENTINEL","description":"caller","input_schema":{"type":"object"}}],"diagnostics":{"previous_message_id":null}}`)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, model, nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	facts := claudeDesktopRuntimeFacts{
		Now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), SessionID: "session",
		PromptID: "22222222-2222-4222-8222-222222222222", ClientRequestID: "33333333-3333-4333-8333-333333333333",
		LogicalModel: model, WorkingDir: `C:\code`, UserHome: `C:\Users\tester`,
		MemoryDir: `C:\Users\tester\.claude\projects\C--code\memory`, ScratchpadDir: `C:\Temp\claude\session`,
	}
	profiled, _, errProfile := executor.applyClaudeDesktopMessageProfile(context.Background(), nil, body, true, plan, facts)
	if errProfile != nil {
		t.Fatal(errProfile)
	}
	finalized, errFinalize := executor.finalizeClaudeDesktopBody(profiled, plan)
	if errFinalize != nil {
		t.Fatal(errFinalize)
	}
	return finalized
}
