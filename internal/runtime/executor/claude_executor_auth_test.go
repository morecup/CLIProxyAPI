package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func newEnrolledClaudeDesktopAuth(_ string, accessToken string) *cliproxyauth.Auth {
	accountUUID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	organizationUUID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	id, errAuthID := claudedesktop.StableAuthID(accountUUID, organizationUUID)
	if errAuthID != nil {
		panic(errAuthID)
	}
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: claudedesktop.Provider,
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey:   accessToken,
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
		Metadata: map[string]any{
			"type":                            claudedesktop.Provider,
			claudedesktop.MetadataAuthFlowKey: claudedesktop.AuthFlowDesktop,
			"account_uuid":                    accountUUID,
			"organization_uuid":               organizationUUID,
			claudedesktop.MetadataTrustedDeviceTokenKey: "trusted-device-token",
			"claude_device_ids":                         []string{claudedesktop.RequestDeviceID("cccccccc-cccc-4ccc-8ccc-cccccccccccc")},
			claudedesktop.MetadataEnrollmentKey: claudedesktop.Enrollment{
				Version:          claudedesktop.EnrollmentSchemaVersion,
				State:            claudedesktop.EnrollmentReady,
				AuthID:           id,
				AccountUUID:      accountUUID,
				OrganizationUUID: organizationUUID,
				DeviceID:         "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
				DisplayName:      "Claude Desktop on test · win32",
				ProfileVersion:   claudedesktop.DefaultDesktopVersion,
			},
		},
	}
}

func TestClaudeExecutorDuplicateMetadataIsRequestScoped(t *testing.T) {
	testCases := []struct {
		name string
		run  func(context.Context, *ClaudeExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, executor *ClaudeExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, errExecute := executor.Execute(ctx, auth, req, opts)
				return errExecute
			},
		},
		{
			name: "stream",
			run: func(ctx context.Context, executor *ClaudeExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, errStream := executor.ExecuteStream(ctx, auth, req, opts)
				return errStream
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			upstreamCalled := false
			transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalled = true
				return nil, errors.New("unexpected upstream request")
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(transport))
			auth := newEnrolledClaudeDesktopAuth("claude-desktop-duplicate.json", "sk-ant-oat-duplicate-metadata")
			req := cliproxyexecutor.Request{
				Model: "claude-opus-5",
				Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],` +
					`"metadata":{"user_id":"{}"},"metadata":{"user_id":"{}"}}`),
			}
			errRun := testCase.run(ctx, NewClaudeExecutor(&config.Config{}), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if errRun == nil {
				t.Fatal("duplicate metadata error = nil")
			}
			if upstreamCalled {
				t.Fatal("duplicate metadata reached upstream")
			}
			var requestErr cliproxyexecutor.RequestScopedError
			if !errors.As(errRun, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
				t.Fatalf("duplicate metadata error = %T %v, want request-scoped", errRun, errRun)
			}
		})
	}
}

func TestClaudeExecutorPrepareRequestAuthRequiresCompletedDesktopEnrollment(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	enrolled := newEnrolledClaudeDesktopAuth("claude-desktop-ready.json", "sk-ant-oat-ready")
	if executor.ShouldPrepareRequestAuth(enrolled) {
		t.Fatal("ShouldPrepareRequestAuth() = true; Desktop login must complete binding before scheduling")
	}
	prepared, errPrepare := executor.PrepareRequestAuth(context.Background(), enrolled)
	if errPrepare != nil || prepared != enrolled {
		t.Fatalf("PrepareRequestAuth() = %#v, %v; want enrolled credential", prepared, errPrepare)
	}

	legacy := &cliproxyauth.Auth{
		ID:         "claude-code-token.json",
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "sk-ant-oat-legacy", "auth_kind": "oauth"},
		Metadata:   map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
	}
	prepared, errPrepare = executor.PrepareRequestAuth(context.Background(), legacy)
	if errPrepare == nil || prepared != nil || !strings.Contains(errPrepare.Error(), "not enrolled by Claude Desktop login") {
		t.Fatalf("legacy PrepareRequestAuth() = %#v, %v; want enrollment rejection", prepared, errPrepare)
	}
}

func TestClaudeExecutorPrepareRequestAuthRejectsBindingDrift(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := newEnrolledClaudeDesktopAuth("claude-desktop-drift.json", "sk-ant-oat-drift")
	auth.Metadata["organization_uuid"] = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	prepared, errPrepare := executor.PrepareRequestAuth(context.Background(), auth)
	if errPrepare == nil || prepared != nil || !strings.Contains(errPrepare.Error(), "organization binding changed") {
		t.Fatalf("PrepareRequestAuth() = %#v, %v; want binding drift rejection", prepared, errPrepare)
	}
}
