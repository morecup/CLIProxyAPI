package helps

import (
	"context"
	"encoding/json"
	"errors"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

var ErrClaudeDesktopRecoveryContext = errors.New("unresolved Desktop recovery context binding")

type claudeDesktopRecoveryContextKey struct{}

// ClaudeDesktopRecoveryContext is an in-process binding supplied by a native
// context owner, not parsed from HTTP headers, bodies, metadata or host paths.
// SnapshotAndReset must snapshot read state, reset actual owned context, and
// bind restoration operations using the exact retained native message UUIDs.
// No empty file/task/hook adapters are installed for an absent context owner.
type ClaudeDesktopRecoveryContext struct {
	Binding          cliproxyexecutor.ClaudeDesktopSessionBinding
	Hooks            claudeprompt.SDKCompactHookRunner
	SnapshotAndReset func(context.Context, []claudeprompt.SDKHistoryMessage) (claudeprompt.SDKCompactionRestorationOps, error)
	Normalize        claudeprompt.SDKAttachmentNormalizeOptions
	RemoteEnabled    bool
	// Diagnostics observes the real precompact normalizer, cache age and
	// preserved-thinking decision. No wire row or API usage guesses replace it.
	Diagnostics func(context.Context, []claudeprompt.SDKHistoryMessage) (ClaudeDesktopCompactionDiagnostics, error)
}

type ClaudeDesktopCompactionDiagnostics struct {
	PreservedUUIDCount         *int
	Breakdown                  *claudeprompt.SDKCompactionBreakdown
	CacheCold                  *bool
	KeptThinkingBlockCount     *int
	KeptThinkingStripped       *bool
	KeptThinkingStripDecidedBy string
}

func WithClaudeDesktopRecoveryContext(ctx context.Context, value ClaudeDesktopRecoveryContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, claudeDesktopRecoveryContextKey{}, value)
}

// ResolveClaudeDesktopRecoveryContext rejects foreign bindings before any
// callback can run. A missing binding remains distinct from an invalid one.
func ResolveClaudeDesktopRecoveryContext(ctx context.Context, binding cliproxyexecutor.ClaudeDesktopSessionBinding, view *claudeprompt.SDKCompactionView) (*ClaudeDesktopRecoveryContext, error) {
	if ctx == nil {
		return nil, nil
	}
	value, present := ctx.Value(claudeDesktopRecoveryContextKey{}).(ClaudeDesktopRecoveryContext)
	if !present {
		return nil, nil
	}
	scope, _ := json.Marshal([]string{binding.AccountID, binding.ProfileID, binding.Egress})
	if value.Binding != binding || binding.AccountID == "" || binding.ProfileID == "" || binding.SessionID == "" ||
		!view.MatchesScope(string(scope), binding.SessionID) || value.Hooks == nil || value.SnapshotAndReset == nil {
		return nil, ErrClaudeDesktopRecoveryContext
	}
	return &value, nil
}
