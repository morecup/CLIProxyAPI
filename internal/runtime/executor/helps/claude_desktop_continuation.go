package helps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

type claudeDesktopContinuationKey [32]byte

type claudeDesktopContinuation struct {
	query     context.Context
	owner     *claudeprompt.Request
	source    [32]byte
	continued [32]byte
	messages  json.RawMessage
	promptID  string
	clientID  string
}

func claudeDesktopContinuationScope(auth *cliproxyauth.Auth, profile, session, client string) claudeDesktopContinuationKey {
	value, _ := json.Marshal([]string{auth.ID, profile, auth.ProxyURL, session, client})
	return sha256.Sum256(value)
}

func claudeDesktopContinuationMessages(body []byte) ([]byte, bool) {
	var value struct {
		Messages []any `json:"messages"`
	}
	if json.Unmarshal(body, &value) != nil || len(value.Messages) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(value.Messages)
	return encoded, err == nil
}

// RememberClaudeDesktopContinuation retains only request-local content while
// an outer conductor may retry. Tracker and durable state remain plaintext-free.
func RememberClaudeDesktopContinuation(ctx, query context.Context, auth *cliproxyauth.Auth, profile, session, originalClient string, sourceBody, continuedBody []byte, owner *claudeprompt.Request, client string) {
	if auth == nil || owner == nil {
		return
	}
	source, sourceOK := claudeDesktopContinuationMessages(sourceBody)
	messages, messagesOK := claudeDesktopContinuationMessages(continuedBody)
	if !sourceOK || !messagesOK {
		return
	}
	value := &claudeDesktopContinuation{query: query, owner: owner, source: sha256.Sum256(source), continued: sha256.Sum256(messages), messages: messages,
		promptID: owner.Identity().PromptID, clientID: client}
	cliproxyexecutor.StoreUpstreamInvocationValue(ctx, claudeDesktopContinuationScope(auth, profile, session, originalClient), value)
	cliproxyexecutor.StoreUpstreamInvocationValue(ctx, claudeDesktopContinuationScope(auth, profile, session, client), value)
}

// ResumeClaudeDesktopContinuation replaces only the exact original history in
// the same invocation/account/profile/egress/session. The exact adopted body
// may already have been restored by the durable context. Different bodies, closed
// queries and independent helpers never borrow a saved compacted conversation.
func ResumeClaudeDesktopContinuation(ctx context.Context, auth *cliproxyauth.Auth, profile, session, promptID, clientID string, body []byte) (context.Context, []byte, string, string) {
	if auth == nil || ctx == nil || ctx.Err() != nil {
		return ctx, body, promptID, clientID
	}
	saved, found := cliproxyexecutor.LoadUpstreamInvocationValue(ctx, claudeDesktopContinuationScope(auth, profile, session, clientID))
	value, valid := saved.(*claudeDesktopContinuation)
	if !found || !valid || !value.owner.CanRetry() {
		return ctx, body, promptID, clientID
	}
	messages, known := claudeDesktopContinuationMessages(body)
	if !known || (sha256.Sum256(messages) != value.source && sha256.Sum256(messages) != value.continued) {
		return ctx, body, promptID, clientID
	}
	updated, err := sjson.SetRawBytes(body, "messages", value.messages)
	if err != nil {
		return ctx, body, promptID, clientID
	}
	next, sameOwner := cliproxyexecutor.WithNextOwnedUpstreamQueryAttempt(ctx, value.query, time.Now())
	if !sameOwner {
		return ctx, body, promptID, clientID
	}
	return next, updated, value.promptID, value.clientID
}
