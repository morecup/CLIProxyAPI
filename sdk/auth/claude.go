package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ClaudeAuthenticator performs the Claude Desktop magic-link OAuth flow.
// It intentionally does not share Claude Code's localhost callback or scopes.
type ClaudeAuthenticator struct {
	// CallbackPort is retained for source compatibility. Desktop magic-link
	// login does not expose an OAuth callback listener.
	CallbackPort int

	newService func(*config.Config) *claudedesktop.Service
}

func NewClaudeAuthenticator() *ClaudeAuthenticator {
	return &ClaudeAuthenticator{
		CallbackPort: 54545,
		newService:   claudedesktop.NewService,
	}
}

func (a *ClaudeAuthenticator) Provider() string {
	return claudedesktop.Provider
}

func (a *ClaudeAuthenticator) RefreshLead() *time.Duration {
	return new(4 * time.Hour)
}

func (a *ClaudeAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	newService := a.newService
	if newService == nil {
		newService = claudedesktop.NewService
	}
	service := newService(cfg)
	metadata := opts.Metadata
	loginOptions := claudedesktop.MagicLinkLoginOptions{
		MagicLink:            firstMetadataValue(metadata, "claude-desktop-magic-link", "magic-link", "magic_link"),
		Locale:               firstMetadataValue(metadata, "locale"),
		InteractiveSessionID: firstMetadataValue(metadata, claudedesktop.InteractiveSessionMetadataKey),
		Prompt:               opts.Prompt,
		Timeout:              5 * time.Minute,
	}
	fmt.Println("Starting Claude Desktop email magic-link authentication")
	result, errLogin := service.Login(ctx, loginOptions)
	if errLogin != nil {
		return nil, errLogin
	}
	return authRecordFromClaudeLoginResult(result)
}

// LoginWithSessionKey imports an already authenticated Claude.ai sessionKey
// and performs the same Desktop OAuth and enrollment flow as magic-link login.
func (a *ClaudeAuthenticator) LoginWithSessionKey(ctx context.Context, cfg *config.Config, sessionKey string) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	newService := a.newService
	if newService == nil {
		newService = claudedesktop.NewService
	}
	result, errLogin := newService(cfg).LoginWithSessionKey(ctx, sessionKey, 3*time.Minute)
	if errLogin != nil {
		return nil, errLogin
	}
	return authRecordFromClaudeLoginResult(result)
}

func authRecordFromClaudeLoginResult(result *claudedesktop.LoginResult) (*coreauth.Auth, error) {
	if result == nil {
		return nil, fmt.Errorf("cliproxy auth: Claude Desktop login returned no result")
	}
	activeEnrollment, errActivate := claudedesktop.TransitionEnrollment(result.Enrollment, claudedesktop.EnrollmentActive, "", time.Now())
	if errActivate != nil {
		return nil, fmt.Errorf("cliproxy auth: activate Claude Desktop enrollment: %w", errActivate)
	}
	result.Enrollment = activeEnrollment
	storage, errStorage := claudedesktop.NewTokenStorage(result)
	if errStorage != nil {
		return nil, errStorage
	}
	fileName := result.AuthID
	return &coreauth.Auth{
		ID:       fileName,
		Provider: claudedesktop.Provider,
		FileName: fileName,
		Storage:  storage,
		Status:   coreauth.StatusActive,
		Metadata: claudedesktop.MetadataFromLogin(result),
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
		},
	}, nil
}

func firstMetadataValue(metadata map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return value
		}
	}
	return ""
}
