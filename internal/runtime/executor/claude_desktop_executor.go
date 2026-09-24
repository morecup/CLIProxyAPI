package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type claudeDesktopEligibilityError struct {
	statusErr
}

type claudeDesktopRuntimeDrainingError struct {
	claudeDesktopEligibilityError
}

type claudeDesktopCapabilities struct {
	CredentialMetadata bool
	ToolAliases        bool
	Diagnostics        bool
	Cancellation       bool
}

func (claudeDesktopEligibilityError) IsRequestScoped() bool { return true }

func (claudeDesktopRuntimeDrainingError) Is(target error) bool {
	return target == errClaudeDesktopRuntimeDraining
}

func newClaudeDesktopEligibilityError(message string) error {
	return claudeDesktopEligibilityError{statusErr{
		code: http.StatusBadRequest,
		msg:  "claude desktop provider: " + message,
	}}
}

func newClaudeDesktopRuntimeDrainingError() error {
	return claudeDesktopRuntimeDrainingError{claudeDesktopEligibilityError{statusErr{
		code: http.StatusBadRequest,
		msg:  "claude desktop provider: " + errClaudeDesktopRuntimeDraining.Error(),
	}}}
}

// validateClaudeDesktopAuth enforces the provider boundary before any request
// identity is allocated or an upstream connection is opened.
func (e *ClaudeExecutor) validateClaudeDesktopAuth(auth *cliproxyauth.Auth) error {
	if e == nil || !e.desktopOnly {
		return nil
	}
	if auth == nil {
		return newClaudeDesktopEligibilityError("auth is required")
	}
	if e.desktopProfileErr != nil {
		return newClaudeDesktopEligibilityError(fmt.Sprintf("profile is unavailable: %v", e.desktopProfileErr))
	}
	if e.desktopProfile == nil {
		return newClaudeDesktopEligibilityError("profile is unavailable")
	}
	if e.cfg != nil && e.cfg.ClaudeDesktop.EmergencyStop {
		return newClaudeDesktopEligibilityError("emergency-stop is active")
	}
	if provider := strings.TrimSpace(auth.Provider); provider != "" && !strings.EqualFold(provider, "claude") {
		return newClaudeDesktopEligibilityError(fmt.Sprintf("credential provider %q is not claude", provider))
	}
	apiKey, baseURL := claudeCreds(auth)
	if auth.AuthKind() != cliproxyauth.AuthKindOAuth || !isClaudeOAuthToken(apiKey) {
		return newClaudeDesktopEligibilityError("only first-party Claude OAuth credentials are accepted; move API keys and custom gateways to anthropic-compatible")
	}
	enrollment, errEnrollment := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return newClaudeDesktopEligibilityError(fmt.Sprintf("credential is not enrolled by Claude Desktop login: %v", errEnrollment))
	}
	if !strings.EqualFold(strings.TrimSpace(enrollment.ProfileVersion), strings.TrimSpace(e.desktopProfile.DesktopVersion)) {
		return newClaudeDesktopEligibilityError(fmt.Sprintf("enrollment profile %q does not match active Desktop profile %q", enrollment.ProfileVersion, e.desktopProfile.DesktopVersion))
	}
	if baseURL != "" && !isAnthropicUpstreamBase(baseURL) {
		return newClaudeDesktopEligibilityError("custom base-url is not allowed; move this credential to anthropic-compatible")
	}
	for _, key := range []string{"fingerprint_profile", "fingerprint-profile", "cloak_mode"} {
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes[key]) != "" {
			return newClaudeDesktopEligibilityError(fmt.Sprintf("legacy %s metadata is not supported; remove it and complete Desktop enrollment", key))
		}
		if auth.Metadata != nil {
			if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				return newClaudeDesktopEligibilityError(fmt.Sprintf("legacy %s metadata is not supported; remove it and complete Desktop enrollment", key))
			}
		}
	}
	if e.cfg != nil && len(e.cfg.ClaudeDesktop.RolloutAuthIDs) > 0 {
		eligible := false
		for _, authID := range e.cfg.ClaudeDesktop.RolloutAuthIDs {
			if auth.ID == authID {
				eligible = true
				break
			}
		}
		if !eligible {
			return newClaudeDesktopEligibilityError("credential is not included in claude-desktop.rollout-auth-ids")
		}
	}
	return nil
}

func (e *ClaudeExecutor) desktopCapabilities() claudeDesktopCapabilities {
	if e == nil || !e.desktopOnly {
		return claudeDesktopCapabilities{}
	}
	return claudeDesktopCapabilities{
		CredentialMetadata: true,
		ToolAliases:        true,
		Diagnostics:        true,
		Cancellation:       true,
	}
}

func (e *ClaudeExecutor) newClaudeUpstreamHTTPClient(ctx context.Context, auth *cliproxyauth.Auth, plan claudeDesktopRequestPlan) (*http.Client, error) {
	if e == nil || !e.desktopOnly {
		return helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0), nil
	}
	if e.desktopTransports == nil {
		e.desktopTransports = helps.NewClaudeDesktopTransportRegistry()
	}
	client, errClient := e.desktopTransports.ClientForVariant(ctx, e.cfg, auth, e.desktopProfile, plan.Variant)
	if errClient != nil {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: "claude desktop transport is unavailable: " + errClient.Error()}}
	}
	return client, nil
}
