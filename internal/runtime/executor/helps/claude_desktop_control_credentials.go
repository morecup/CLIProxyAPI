package helps

import (
	"context"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ClaudeDesktopControlCredentials connects a runtime to its registered account
// owner. Acquisition is injected separately from application runtime admission.
type ClaudeDesktopControlCredentials struct {
	Manager *cliproxyauth.Manager
	Owner   cliproxyauth.ProviderExecutor
	Acquire cliproxyauth.CredentialRefresh
}

func (c *ClaudeDesktopControlCredentials) Current(expected *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return c.Manager.CurrentOwnedCredential(c.Owner, expected)
}

func (c *ClaudeDesktopControlCredentials) Refresh(ctx context.Context, expected *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return c.Manager.RefreshOwnedCredential(ctx, c.Owner, expected, c.Acquire)
}
