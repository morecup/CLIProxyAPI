package executor

import (
	"context"
	"fmt"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (e *ClaudeExecutor) ShouldPrepareRequestAuth(*cliproxyauth.Auth) bool {
	// Desktop credentials are fully bound during login. Request execution must
	// never upgrade a Claude Code token or synthesize enrollment metadata.
	return false
}

func (e *ClaudeExecutor) PrepareRequestAuth(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if errEligibility := e.validateClaudeDesktopAuth(auth); errEligibility != nil {
		return nil, errEligibility
	}
	return auth, nil
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debug("claude desktop executor: refresh called")
	if refreshed, handled, errRefresh := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, errRefresh
	}
	if auth == nil {
		return nil, fmt.Errorf("claude desktop executor: auth is nil")
	}
	if _, errEnrollment := claudedesktop.ValidateEnrollment(auth.ID, auth.Metadata); errEnrollment != nil {
		return nil, fmt.Errorf("claude desktop executor: enrollment is invalid: %w", errEnrollment)
	}
	refreshToken := claudeauth.ReadMetadataString(&auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return nil, fmt.Errorf("claude desktop executor: refresh token is missing")
	}
	service := claudedesktop.NewServiceWithProxyURL(e.cfg, auth.ProxyURL)
	token, errRefresh := service.Refresh(ctx, refreshToken)
	if errRefresh != nil {
		e.recordDesktopOAuthRefreshFailure(ctx, auth, errRefresh) // W5: native gPt(`refresh`, failure result).
		return nil, errRefresh
	}
	claudeauth.EnsureMetadataMap(&auth.Metadata)
	claudeauth.StoreMetadataValue(&auth.Metadata, "access_token", token.AccessToken)
	claudeauth.StoreMetadataString(&auth.Metadata, "refresh_token", token.RefreshToken)
	claudeauth.StoreMetadataValue(&auth.Metadata, "expired", token.Expire)
	claudeauth.StoreMetadataValue(&auth.Metadata, "type", claudedesktop.Provider)
	claudeauth.StoreMetadataValue(&auth.Metadata, claudedesktop.MetadataAuthFlowKey, claudedesktop.AuthFlowDesktop)
	claudeauth.StoreMetadataValue(&auth.Metadata, "last_refresh", time.Now().UTC().Format(time.RFC3339))
	return auth, nil
}
