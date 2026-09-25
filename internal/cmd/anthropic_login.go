package cmd

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
)

// LoginOptions contains options for the login processes.
// It provides configuration for authentication flows including browser behavior
// and interactive prompting capabilities.
type LoginOptions struct {
	// NoBrowser indicates whether to skip opening the browser automatically.
	NoBrowser bool

	// CallbackPort overrides the local OAuth callback port when set (>0).
	CallbackPort int

	// Prompt allows the caller to provide interactive input when needed.
	Prompt func(prompt string) (string, error)
}

// DoClaudeLogin runs the Claude Desktop email magic-link and enrollment flow.
//
// Parameters:
//   - cfg: The application configuration
//   - options: Login options including interactive prompting
func DoClaudeLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}

	manager := newAuthManager()

	authOpts := &sdkAuth.LoginOptions{
		CallbackPort: options.CallbackPort,
		Metadata:     map[string]string{},
		Prompt:       promptFn,
	}

	_, savedPath, err := manager.Login(context.Background(), "claude", cfg, authOpts)
	if err != nil {
		fmt.Printf("Claude Desktop authentication failed: %v\n", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}

	fmt.Println("Claude Desktop authentication and enrollment successful!")
}
