package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestValidateClaudeOpus55Request(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		directAnthropic bool
		wantErr         bool
	}{
		{name: "forced any", body: `{"model":"claude-opus-5-5","tool_choice":{"type":"any"}}`, directAnthropic: true, wantErr: true},
		{name: "forced named tool", body: `{"model":"claude-opus-5-5","tool_choice":{"type":"tool","name":"computer"}}`, directAnthropic: true, wantErr: true},
		{name: "auto", body: `{"model":"claude-opus-5-5","tool_choice":{"type":"auto"}}`, directAnthropic: true},
		{name: "none", body: `{"model":"claude-opus-5-5","tool_choice":{"type":"none"}}`, directAnthropic: true},
		{name: "fast mode", body: `{"model":"claude-opus-5-5","speed":"fast"}`, directAnthropic: true},
		{name: "old computer 20250124 direct", body: `{"model":"claude-opus-5-5","tools":[{"type":"computer_20250124","name":"computer"}]}`, directAnthropic: true, wantErr: true},
		{name: "old computer 20251124 direct", body: `{"model":"claude-opus-5-5","tools":[{"type":"computer_20251124","name":"computer"}]}`, directAnthropic: true, wantErr: true},
		{name: "old computer delegated", body: `{"model":"claude-opus-5-5","tools":[{"type":"computer_20251124","name":"computer"}]}`},
		{name: "current computer toolset", body: `{"model":"claude-opus-5-5","tools":[{"type":"computer_toolset_20260801"}]}`, directAnthropic: true},
		{name: "opus alias", body: `{"model":"opus","tool_choice":{"type":"any"}}`, directAnthropic: true, wantErr: true},
		{name: "bedrock model forced tool", body: `{"model":"anthropic.claude-opus-5-5-20260922-v1:0","tool_choice":{"type":"tool","name":"computer"}}`, wantErr: true},
		{name: "bedrock model old computer", body: `{"model":"anthropic.claude-opus-5-5-20260922-v1:0","tools":[{"type":"computer_20251124","name":"computer"}]}`},
		{name: "older model retains compatibility rewrite", body: `{"model":"claude-opus-5","thinking":{"type":"adaptive"},"tool_choice":{"type":"any"},"tools":[{"type":"computer_20251124","name":"computer"}]}`, directAnthropic: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateClaudeOpus55Request([]byte(test.body), test.directAnthropic)
			if test.wantErr {
				assertClaudeOpus55RequestValidationError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("validateClaudeOpus55Request() error = %v", err)
			}
		})
	}
}

func TestClaudeExecutorOpus55RequestGate(t *testing.T) {
	runners := []struct {
		name string
		run  func(context.Context, *ClaudeExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, executor *ClaudeExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, err := executor.Execute(ctx, auth, req, opts)
				return err
			},
		},
		{
			name: "stream",
			run: func(ctx context.Context, executor *ClaudeExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					return err
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
		},
	}

	tests := []struct {
		name                 string
		model                string
		payload              string
		baseURL              string
		wantErr              bool
		wantThinkingRemoved  bool
		wantUpstreamToolType string
		forbidUpstreamName   bool
		wantBeta             string
		forbidBeta           string
	}{
		{
			name:    "reject forced any",
			model:   "claude-opus-5-5",
			payload: `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"}}`,
			wantErr: true,
		},
		{
			name:     "accept fast mode",
			model:    "claude-opus-5-5",
			payload:  `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"speed":"fast"}`,
			wantBeta: claudeFastModeBeta,
		},
		{
			name:    "reject old computer 20250124",
			model:   "claude-opus-5-5",
			payload: `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_20250124","name":"computer"}]}`,
			wantErr: true,
		},
		{
			name:    "reject old computer 20251124",
			model:   "claude-opus-5-5",
			payload: `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_20251124","name":"computer"}]}`,
			wantErr: true,
		},
		{
			name:                 "accept current computer toolset",
			model:                "claude-opus-5-5",
			payload:              `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_toolset_20260801"}]}`,
			wantUpstreamToolType: "computer_toolset_20260801",
			forbidUpstreamName:   true,
			forbidBeta:           "computer-use-2026-08-03",
		},
		{
			name:                 "delegated gateway retains older computer tool",
			model:                "claude-opus-5-5",
			payload:              `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_20251124","name":"computer"}]}`,
			baseURL:              "https://gateway.example",
			wantUpstreamToolType: "computer_20251124",
		},
		{
			name:                "older model keeps forced tool compatibility rewrite",
			model:               "claude-opus-5",
			payload:             `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tool_choice":{"type":"any"}}`,
			wantThinkingRemoved: true,
		},
	}

	for _, runner := range runners {
		for _, test := range tests {
			t.Run(runner.name+"/"+test.name, func(t *testing.T) {
				var upstreamCalls atomic.Int32
				var upstreamBody []byte
				var upstreamHeaders http.Header
				transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					upstreamCalls.Add(1)
					upstreamHeaders = req.Header.Clone()
					var errRead error
					upstreamBody, errRead = io.ReadAll(req.Body)
					if errRead != nil {
						return nil, errRead
					}
					contentType := "application/json"
					responseBody := `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					if runner.name == "stream" {
						contentType = "text/event-stream"
						responseBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       io.NopCloser(strings.NewReader(responseBody)),
						Request:    req,
					}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
				executor := newAnthropicCompatibleTestExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": test.baseURL}}
				req := cliproxyexecutor.Request{Model: test.model, Payload: []byte(test.payload)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

				err := runner.run(ctx, executor, auth, req, opts)
				if test.wantErr {
					assertClaudeOpus55RequestValidationError(t, err)
					if got := upstreamCalls.Load(); got != 0 {
						t.Fatalf("upstream calls = %d, want 0", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("request error = %v", err)
				}
				if got := upstreamCalls.Load(); got != 1 {
					t.Fatalf("upstream calls = %d, want 1", got)
				}
				if test.wantThinkingRemoved {
					if got := gjson.GetBytes(upstreamBody, "thinking"); got.Exists() {
						t.Fatalf("legacy forced tool choice retained thinking: %s", upstreamBody)
					}
					if got := gjson.GetBytes(upstreamBody, "output_config.effort"); got.Exists() {
						t.Fatalf("legacy forced tool choice retained effort: %s", upstreamBody)
					}
				}
				if test.wantUpstreamToolType != "" {
					if got := gjson.GetBytes(upstreamBody, "tools.0.type").String(); got != test.wantUpstreamToolType {
						t.Fatalf("upstream computer tool type = %q, want %q; body=%s", got, test.wantUpstreamToolType, upstreamBody)
					}
				}
				if test.forbidUpstreamName && gjson.GetBytes(upstreamBody, "tools.0.name").Exists() {
					t.Fatalf("upstream computer toolset unexpectedly contains a name: %s", upstreamBody)
				}
				if test.wantBeta != "" {
					if got := upstreamHeaders.Get("Anthropic-Beta"); !strings.Contains(got, test.wantBeta) {
						t.Fatalf("Anthropic-Beta = %q, want %q", got, test.wantBeta)
					}
				}
				if test.forbidBeta != "" {
					if got := upstreamHeaders.Get("Anthropic-Beta"); strings.Contains(got, test.forbidBeta) {
						t.Fatalf("Anthropic-Beta = %q, must not contain %q", got, test.forbidBeta)
					}
				}
			})
		}
	}
}

func TestClaudeDesktopOpus55SelectsOnlyObservedComputerVariants(t *testing.T) {
	base := claudeprofile.RequestVariant{
		AnthropicBeta: []string{"base"},
		AnthropicBetaVariants: map[string][]string{
			"computer":      {"base"},
			"fast-computer": {"base", claudeFastModeBeta},
		},
	}
	computerBody := []byte(`{"model":"claude-opus-5-5","tools":[{"type":"computer_toolset_20260801"}]}`)
	name, betas, err := selectClaudeDesktopBetaVariant(computerBody, nil, base)
	if err != nil || name != "computer" || strings.Join(betas, ",") != "base" {
		t.Fatalf("computer variant = %q %v, err=%v", name, betas, err)
	}

	combinedBody := []byte(`{"model":"claude-opus-5-5","speed":"fast","tools":[{"type":"computer_toolset_20260801"}]}`)
	name, betas, err = selectClaudeDesktopBetaVariant(combinedBody, nil, base)
	if err != nil || name != "fast-computer" || strings.Join(betas, ",") != "base,"+claudeFastModeBeta {
		t.Fatalf("combined variant = %q %v, err=%v", name, betas, err)
	}

	delete(base.AnthropicBetaVariants, "computer")
	if _, _, err = selectClaudeDesktopBetaVariant(computerBody, nil, base); err == nil || !strings.Contains(err.Error(), "no observed computer beta variant") {
		t.Fatalf("missing computer variant error = %v", err)
	}
}

func TestClaudeDesktopRawHTTPRequestRejectsOpus55ForcedToolChoice(t *testing.T) {
	var upstreamCalls atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, errors.New("unexpected upstream call")
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
	executor := NewClaudeExecutor(&config.Config{})
	t.Cleanup(executor.Close)
	auth := newClaudeDesktopRawRequestTestAuth(t)
	body := `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"}}`
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(body))
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	_, err := executor.HttpRequest(ctx, auth, request)
	assertClaudeOpus55RequestValidationError(t, err)
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("raw HTTP upstream calls = %d, want 0", got)
	}
}

func TestClaudeExecutorOpus55CountTokensGate(t *testing.T) {
	for _, test := range []struct {
		name       string
		payload    string
		wantErr    bool
		wantBeta   string
		forbidBeta string
	}{
		{name: "forced tool", payload: `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"any"}}`, wantErr: true},
		{name: "old computer", payload: `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_20251124","name":"computer"}]}`, wantErr: true},
		{name: "current computer", payload: `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_toolset_20260801"}]}`, wantBeta: claudeTokenCountingBeta, forbidBeta: "computer-use-2026-08-03"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			var upstreamHeaders http.Header
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				upstreamCalls.Add(1)
				upstreamHeaders = req.Header.Clone()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"input_tokens":1}`)),
					Request:    req,
				}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			executor := newAnthropicCompatibleTestExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
			req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(test.payload)}

			_, err := executor.CountTokens(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if test.wantErr {
				assertClaudeOpus55RequestValidationError(t, err)
				if got := upstreamCalls.Load(); got != 0 {
					t.Fatalf("count_tokens upstream calls = %d, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Fatalf("count_tokens upstream calls = %d, want 1", got)
			}
			if got := upstreamHeaders.Get("Anthropic-Beta"); !strings.Contains(got, test.wantBeta) {
				t.Fatalf("Anthropic-Beta = %q, want %q", got, test.wantBeta)
			}
			if got := upstreamHeaders.Get("Anthropic-Beta"); test.forbidBeta != "" && strings.Contains(got, test.forbidBeta) {
				t.Fatalf("Anthropic-Beta = %q, must not contain %q", got, test.forbidBeta)
			}
		})
	}
}

func assertClaudeOpus55RequestValidationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("request validation error = nil")
	}
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(err, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
		t.Fatalf("error = %T %v, want request-scoped", err, err)
	}
	var statusError interface{ StatusCode() int }
	if !errors.As(err, &statusError) || statusError.StatusCode() != http.StatusBadRequest {
		t.Fatalf("error = %T %v, want HTTP 400", err, err)
	}
}
