package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newClaudeDesktopWireProbeRequest(t *testing.T) (*ClaudeExecutor, *http.Request) {
	t.Helper()
	executor := NewClaudeExecutor(&config.Config{})
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)
	plan, errPlan := executor.planClaudeDesktopRequest(body, claudeprofile.RoleMain)
	if errPlan != nil {
		t.Fatalf("plan Claude Desktop request: %v", errPlan)
	}
	plan.PromptID = "11111111-2222-4333-8444-555555555555"
	plan.ClientRequestID = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader("{}"))
	req.Header = make(http.Header)
	if errHeaders := executor.applyClaudeHeadersWithProfile(
		req,
		&cliproxyauth.Auth{Metadata: map[string]any{"access_token": "sk-ant-oat01-wire"}},
		"sk-ant-oat01-wire",
		false,
		nil,
		body,
		plan,
		nil,
		"bbbbbbbb-cccc-4ddd-8eee-ffffffffffff",
	); errHeaders != nil {
		t.Fatalf("apply Claude Desktop headers: %v", errHeaders)
	}
	return executor, req
}

func TestClaudeDesktopHeadersRemainCanonicalUntilSend(t *testing.T) {
	_, req := newClaudeDesktopWireProbeRequest(t)
	for canonical := range claudeWireHeaderCasing {
		if req.Header.Get(canonical) == "" {
			t.Fatalf("%s is unreadable through Header.Get before send", canonical)
		}
	}
}

func TestClaudeDesktopWireCasingPreservesProfileValues(t *testing.T) {
	_, req := newClaudeDesktopWireProbeRequest(t)
	wantValues := make(map[string]string, len(claudeWireHeaderCasing))
	for canonical := range claudeWireHeaderCasing {
		wantValues[canonical] = req.Header.Get(canonical)
	}
	applyClaudeWireHeaderCasing(req)
	for canonical, wire := range claudeWireHeaderCasing {
		if _, stillCanonical := req.Header[canonical]; stillCanonical {
			t.Fatalf("%s was not rewritten to %s", canonical, wire)
		}
		if got := strings.Join(req.Header[wire], ","); got != wantValues[canonical] {
			t.Fatalf("%s = %q, want %q", wire, got, wantValues[canonical])
		}
	}
}

func TestAnthropicCompatibleSendDoesNotApplyDesktopWireCasing(t *testing.T) {
	executor := newAnthropicCompatibleTestExecutor(&config.Config{})
	req, errNewRequest := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errNewRequest != nil {
		t.Fatalf("new request: %v", errNewRequest)
	}
	req.Header.Set("Anthropic-Beta", "caller-beta")
	var seen http.Header
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen = req.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	})}
	resp, errRequest := executor.doClaudeUpstreamRequest(client, req)
	if errRequest != nil {
		t.Fatalf("anthropic-compatible send: %v", errRequest)
	}
	_ = resp.Body.Close()
	if _, ok := seen["Anthropic-Beta"]; !ok {
		t.Fatalf("canonical caller header missing: %v", seen)
	}
	if _, ok := seen["anthropic-beta"]; ok {
		t.Fatalf("Desktop wire casing leaked into anthropic-compatible: %v", seen)
	}
}

func TestClaudeExecutorHasSingleUpstreamSendBoundary(t *testing.T) {
	for name, want := range map[string]struct {
		method string
		calls  int
	}{
		"claude_executor_execute.go":                 {"doClaudeDesktopRecoverableRequest", 1},
		"claude_executor_stream.go":                  {"doClaudeDesktopRecoverableRequest", 1},
		"claude_desktop_http_request.go":             {"doClaudeDesktopRecoverableRequest", 1},
		"claude_executor_tokens.go":                  {"doClaudeUpstreamRequest", 1},
		"claude_desktop_count_tokens_calibration.go": {"doClaudeUpstreamRequest", 1},
		"claude_desktop_recovery_executor.go":        {"doClaudeUpstreamRequest", 3},
	} {
		source, errRead := os.ReadFile(name)
		if errRead != nil {
			t.Fatalf("read %s: %v", name, errRead)
		}
		file, errParse := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if errParse != nil {
			t.Fatal(errParse)
		}
		calls := 0
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "Do" || selector.Sel.Name == "RoundTrip" {
				t.Errorf("%s bypasses the executor send boundary", name)
			}
			if selector.Sel.Name == want.method {
				calls++
			}
			return true
		})
		if calls != want.calls {
			t.Errorf("%s has %d calls to %s; want %d", name, calls, want.method, want.calls)
		}
	}
}
