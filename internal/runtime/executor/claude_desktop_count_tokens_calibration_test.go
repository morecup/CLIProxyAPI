package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/tidwall/gjson"
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
