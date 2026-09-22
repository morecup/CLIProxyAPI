package telemetry

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	FactSDKRunHook          = "run_hook"
	FactSDKReplHookFinished = "repl_hook_finished"
)

type sdkRunHookMetadata struct {
	SubscriptionType    string `json:"subscription_type,omitempty"`
	PromptID            string `json:"cc_prompt_id,omitempty"`
	HookName            string `json:"hookName"`
	NumCommands         int    `json:"numCommands"`
	NumMatchAllMatchers *int   `json:"numMatchAllMatchers,omitempty"`
	NumSpecificMatchers *int   `json:"numSpecificMatchers,omitempty"`
	HookTypeCounts      string `json:"hookTypeCounts,omitempty"`
}

type sdkReplHookFinishedMetadata struct {
	SubscriptionType  string `json:"subscription_type,omitempty"`
	PromptID          string `json:"cc_prompt_id,omitempty"`
	HookName          string `json:"hookName"`
	NumCommands       int    `json:"numCommands"`
	NumSuccess        int    `json:"numSuccess"`
	NumBlocking       int    `json:"numBlocking"`
	NumNonBlockingErr int    `json:"numNonBlockingError"`
	NumCancelled      int    `json:"numCancelled"`
	TotalDurationMS   int64  `json:"totalDurationMs"`
}

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKRunHook:          "tengu_run_hook",
		FactSDKReplHookFinished: "tengu_repl_hook_finished",
	})
}

// RecordSDKCompactHook records only facts returned by the real compact-hook
// runner. Matcher and hook-type counts are omitted unless the owner supplied
// every classification; unknown values are never converted to zero counts.
func (m *Manager) RecordSDKCompactHook(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, input claudeprompt.SDKCompactHookInput, executions []claudeprompt.SDKCompactHookExecution, duration time.Duration, completed bool) error {
	if m == nil || !m.Enabled() || len(executions) == 0 {
		return nil
	}
	hookName := strings.TrimSpace(input.Event)
	if hookName == "" {
		return fmt.Errorf("Claude Desktop compact hook name is empty")
	}
	if trigger := strings.TrimSpace(input.Trigger); trigger != "" {
		hookName += ":" + trigger
	}
	if duration < 0 {
		duration = 0
	}
	matchAll, specific, hookTypes, classified := compactHookInventory(executions)
	errRun := m.recordSDKFact(ctx, auth, session, model, promptID, FactSDKRunHook, func(subscription, promptID string) any {
		metadata := sdkRunHookMetadata{
			SubscriptionType: subscription,
			PromptID:         promptID,
			HookName:         hookName,
			NumCommands:      len(executions),
		}
		if classified {
			metadata.NumMatchAllMatchers = &matchAll
			metadata.NumSpecificMatchers = &specific
			metadata.HookTypeCounts = hookTypes
		}
		return metadata
	})
	if errRun != nil || !completed {
		return errRun
	}
	finished := sdkReplHookFinishedMetadata{HookName: hookName, NumCommands: len(executions), TotalDurationMS: duration.Milliseconds()}
	for _, execution := range executions {
		switch {
		case execution.Cancelled:
			finished.NumCancelled++
		case execution.Blocked:
			finished.NumBlocking++
		case execution.Succeeded:
			finished.NumSuccess++
		default:
			finished.NumNonBlockingErr++
		}
	}
	return m.recordSDKFact(ctx, auth, session, model, promptID, FactSDKReplHookFinished, func(subscription, promptID string) any {
		finished.SubscriptionType = subscription
		finished.PromptID = promptID
		return finished
	})
}

func compactHookInventory(executions []claudeprompt.SDKCompactHookExecution) (matchAll, specific int, hookTypes string, complete bool) {
	command, callback := 0, 0
	for _, execution := range executions {
		switch execution.MatcherKind {
		case "match_all":
			matchAll++
		case "specific":
			specific++
		default:
			return 0, 0, "", false
		}
		switch execution.HookType {
		case "command":
			command++
		case "callback":
			callback++
		default:
			return 0, 0, "", false
		}
	}
	var counts strings.Builder
	counts.WriteByte('{')
	if command > 0 {
		counts.WriteString(`"command":`)
		counts.WriteString(strconv.Itoa(command))
	}
	if callback > 0 {
		if command > 0 {
			counts.WriteByte(',')
		}
		counts.WriteString(`"callback":`)
		counts.WriteString(strconv.Itoa(callback))
	}
	counts.WriteByte('}')
	return matchAll, specific, counts.String(), true
}
