package telemetry

import (
	"fmt"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
)

func promptIssueManager() *Manager {
	return &Manager{profile: claudeprofile.TelemetryProfile{EndpointRole: "desktop-event-logging"}, endpointStates: make(map[string]EndpointStatus)}
}

func TestPromptIssueRetentionIsBoundedWithoutHidingOverflow(t *testing.T) {
	manager := promptIssueManager()
	var tracker claudeprompt.Tracker
	for index := 0; index < maxPromptIssues+7; index++ {
		token := tracker.Begin(claudeprompt.Input{AccountID: "account", SessionID: "session", ClientRequestID: fmt.Sprint(index), Role: "main",
			StartedAt: time.Unix(1000, 0), Body: []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"unobserved","content":"PRIVATE_RESULT"}]}]}`)})
		manager.recordPromptIssue(&RequestSpan{facts: RequestFacts{Prompt: token}}, false)
	}
	if len(manager.promptIssues) != maxPromptIssues || !manager.promptIssuesOverflow {
		t.Fatalf("unbounded/lost diagnostic retention: size=%d overflow=%t", len(manager.promptIssues), manager.promptIssuesOverflow)
	}
	// Even after retained identifiers are released, lost unresolved ownership
	// must not be reported as healthy by a different successful prompt.
	clear(manager.promptIssues)
	token := tracker.Begin(claudeprompt.Input{AccountID: "account", SessionID: "session", ClientRequestID: "success", Role: "main", Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`)})
	token.FinishSuccess(time.Now(), "end_turn", nil)
	manager.recordPromptIssue(&RequestSpan{facts: RequestFacts{Prompt: token}}, false)
	status := manager.endpointStates[manager.profile.EndpointRole]
	if status.Status != "awaiting-prompt-completion" || !strings.Contains(status.Reason, "retention limit") || strings.Contains(status.Reason, "PRIVATE_") {
		t.Fatalf("overflow was hidden or sensitive: %+v", status)
	}
}

func TestSuccessfulRetryClearsOnlyItsPromptIssue(t *testing.T) {
	manager := promptIssueManager()
	var tracker claudeprompt.Tracker
	input := claudeprompt.Input{AccountID: "account", SessionID: "session", ClientRequestID: "retry", Role: "main", Attempt: 1, Body: []byte(`{"messages":[{"role":"user","content":"test"}]}`)}
	first := tracker.Begin(input)
	first.FinishFailure()
	manager.recordPromptIssue(&RequestSpan{facts: RequestFacts{Prompt: first}}, true)
	otherInput := input
	otherInput.ClientRequestID = "other"
	other := tracker.Begin(otherInput)
	other.FinishFailure()
	manager.recordPromptIssue(&RequestSpan{facts: RequestFacts{Prompt: other}}, true)
	input.Attempt = 2
	retry := tracker.Begin(input)
	retry.FinishSuccess(time.Now(), "end_turn", nil)
	manager.recordPromptIssue(&RequestSpan{facts: RequestFacts{Prompt: retry}}, false)
	if len(manager.promptIssues) != 1 || manager.endpointStates[manager.profile.EndpointRole].Status != "awaiting-prompt-completion" {
		t.Fatal("successful retry cleared an unrelated prompt issue")
	}
	other.FinalizeFailure(time.Now())
	manager.recordPromptIssue(&RequestSpan{facts: RequestFacts{Prompt: other}}, true)
	if len(manager.promptIssues) != 0 || manager.endpointStates[manager.profile.EndpointRole].Status != "ready" {
		t.Fatal("observed terminal decision did not clear its issue")
	}
}
