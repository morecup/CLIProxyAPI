package executor

import (
	"context"
	"encoding/json"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Native tool-search deferral telemetry for owned requests (main turns of the
// remote input actor and child generations). The three events are derived
// from facts the executor already holds at span start: the request body the
// tasks runtime assembled and the request role. Downstream client requests
// never reach these emitters because they carry no claudetasks.Caller.
const (
	claudeDesktopToolSearchToolName = "ToolSearch"
	// Native deferred_tools_delta header line (rendering case in _448.js).
	claudeDesktopDeferredToolsReminderHeader = "The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — calling them directly will fail with InputValidationError. Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:"
	claudeDesktopToolSearchDefaultMaxResults = 5
)

// Native feature tengu_tool_search_unsupported_models default.
var claudeDesktopToolSearchUnsupportedModels = []string{"claude-3-5-haiku", "claude-3-haiku"}

func (s *claudeDesktopRequestSpan) ObserveToolSearchModeDecision(decision claudetelemetry.ToolSearchModeDecision) {
	if s != nil && s.telemetry != nil {
		s.telemetry.ObserveToolSearchModeDecision(decision)
	}
}

func (s *claudeDesktopRequestSpan) ObserveDeferredToolsPoolChange(change claudetelemetry.DeferredToolsPoolChange) {
	if s != nil && s.telemetry != nil {
		s.telemetry.ObserveDeferredToolsPoolChange(change)
	}
}

// observeClaudeDesktopToolSearchRequest reports the native request-build
// telemetry (j1e decision, then the XHr pool change when this request carries
// a new deferred-tools reminder) for one owned request. Retries reuse the
// native request parameters and must not repeat the events.
func observeClaudeDesktopToolSearchRequest(ctx context.Context, span *claudeDesktopRequestSpan, role claudeprofile.RequestRole, model string, body []byte, attempt int, toolSearchDisabled bool, unsupportedModels []string) {
	if span == nil || span.telemetry == nil || attempt > 1 || !claudetasks.IsOwnedRequest(ctx) {
		return
	}
	if role != claudeprofile.RoleMain && role != claudeprofile.RoleSubagent {
		return
	}
	span.ObserveToolSearchModeDecision(claudeDesktopToolSearchModeDecision(model, body, toolSearchDisabled, unsupportedModels))
	if change, ok := claudeDesktopDeferredToolsPoolChange(role, body); ok {
		span.ObserveDeferredToolsPoolChange(change)
	}
}

// claudeDesktopToolSearchModeDecision replays the native j1e branches from
// the assembled request: an unsupported model reports mode "standard" before
// anything else; a request without tools reports no_tools_in_request under
// the current mode; ToolSearch present in the request means the tst decision
// enabled it; otherwise the pool was not registered (standard mode).
// nil unsupportedModels selects the native default list.
func claudeDesktopToolSearchModeDecision(model string, body []byte, toolSearchDisabled bool, unsupportedModels []string) claudetelemetry.ToolSearchModeDecision {
	decision := claudetelemetry.ToolSearchModeDecision{CheckedModel: model, MCPNonBlocking: true}
	tools := gjson.GetBytes(body, "tools").Array()
	hasToolSearch := false
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == claudeDesktopToolSearchToolName {
			hasToolSearch = true
		}
		if strings.HasPrefix(name, "mcp__") {
			decision.MCPToolCount++
		}
	}
	if unsupportedModels == nil {
		unsupportedModels = claudeDesktopToolSearchUnsupportedModels
	}
	lower := strings.ToLower(model)
	for _, entry := range unsupportedModels {
		if entry = strings.ToLower(strings.TrimSpace(entry)); entry != "" && strings.Contains(lower, entry) {
			decision.Mode, decision.Reason = claudetelemetry.ToolSearchModeStandard, claudetelemetry.ToolSearchReasonModelUnsupported
			return decision
		}
	}
	decision.Mode = claudetelemetry.ToolSearchModeTST
	if toolSearchDisabled {
		decision.Mode = claudetelemetry.ToolSearchModeStandard
	}
	switch {
	case len(tools) == 0:
		decision.Reason = claudetelemetry.ToolSearchReasonNoToolsInRequest
	case !hasToolSearch:
		decision.Reason = claudetelemetry.ToolSearchReasonNotRegistered
	case toolSearchDisabled:
		decision.Reason = claudetelemetry.ToolSearchReasonStandardMode
	default:
		decision.Enabled, decision.Reason = true, claudetelemetry.ToolSearchReasonTSTEnabled
	}
	return decision
}

// claudeDesktopDeferredToolsPoolChange recovers the native XHr counters from
// the wire-shaped owned history: the current reminder is the deferred-tools
// block merged into the last user row, prior announcements are the same
// blocks in earlier rows. Owned history has no attachment rows, so
// attachmentCount stays zero and dtdCount counts the prior reminder blocks.
func claudeDesktopDeferredToolsPoolChange(role claudeprofile.RequestRole, body []byte) (claudetelemetry.DeferredToolsPoolChange, bool) {
	rows := gjson.GetBytes(body, "messages").Array()
	lastUser := -1
	for index, row := range rows {
		if row.Get("role").String() == "user" {
			lastUser = index
		}
	}
	if lastUser < 0 {
		return claudetelemetry.DeferredToolsPoolChange{}, false
	}
	prior := make(map[string]struct{})
	priorBlocks := 0
	for _, row := range rows[:lastUser] {
		for _, names := range claudeDesktopDeferredToolsReminderNames(row) {
			priorBlocks++
			for _, name := range names {
				prior[name] = struct{}{}
			}
		}
	}
	current := claudeDesktopDeferredToolsReminderNames(rows[lastUser])
	if len(current) == 0 {
		return claudetelemetry.DeferredToolsPoolChange{}, false
	}
	names := current[len(current)-1]
	for _, names := range current[:len(current)-1] {
		priorBlocks++
		for _, name := range names {
			prior[name] = struct{}{}
		}
	}
	added := make(map[string]struct{}, len(names))
	for _, name := range names {
		added[name] = struct{}{}
	}
	readded := 0
	for name := range added {
		if _, announced := prior[name]; announced {
			readded++
		}
	}
	callSite := claudetelemetry.DeferredToolsCallSiteMain
	if role == claudeprofile.RoleSubagent {
		callSite = claudetelemetry.DeferredToolsCallSiteSubagent
	}
	return claudetelemetry.DeferredToolsPoolChange{
		AddedCount:          len(added),
		ReaddedCount:        readded,
		UnlistedCount:       len(names),
		PriorAnnouncedCount: len(prior),
		MessagesLength:      len(rows),
		DTDCount:            priorBlocks,
		CallSite:            callSite,
	}, true
}

// claudeDesktopDeferredToolsReminderNames returns, per reminder block in the
// row (in order), the tool names listed under the native header line.
func claudeDesktopDeferredToolsReminderNames(row gjson.Result) [][]string {
	if row.Get("role").String() != "user" {
		return nil
	}
	var blocks [][]string
	inspect := func(text string) {
		text = strings.TrimPrefix(text, "<system-reminder>\n")
		for _, paragraph := range strings.Split(text, "\n\n") {
			lines := strings.Split(strings.TrimSuffix(strings.TrimSuffix(paragraph, "\n</system-reminder>"), "</system-reminder>"), "\n")
			if len(lines) == 0 || lines[0] != claudeDesktopDeferredToolsReminderHeader {
				continue
			}
			names := make([]string, 0, len(lines)-1)
			for _, line := range lines[1:] {
				if line = strings.TrimSpace(line); line != "" {
					names = append(names, line)
				}
			}
			blocks = append(blocks, names)
		}
	}
	content := row.Get("content")
	if content.Type == gjson.String {
		inspect(content.String())
		return blocks
	}
	for _, block := range content.Array() {
		if block.Get("type").String() == "text" {
			inspect(block.Get("text").String())
		}
	}
	return blocks
}

// claudeDesktopToolSearchOutcomeObserver returns the callback an owned actor
// invokes after ExecuteTool for a ToolSearch call. input is the tool_use
// input, data the runtime's data JSON
// ({"matches":[...],"query":"...","total_deferred_tools":N}). sessionID is the
// owned query's SDK session (claudeDesktopRuntimeFacts.SessionID).
func (e *ClaudeExecutor) claudeDesktopToolSearchOutcomeObserver(auth *cliproxyauth.Auth, sessionID string) func(context.Context, claudetasks.Caller, json.RawMessage, json.RawMessage) {
	return func(ctx context.Context, caller claudetasks.Caller, input, data json.RawMessage) {
		if e == nil || e.desktopTelemetry == nil || auth == nil {
			return
		}
		query := gjson.GetBytes(input, "query").String()
		maxResults := claudeDesktopToolSearchDefaultMaxResults
		if value := gjson.GetBytes(input, "max_results"); value.Type == gjson.Number {
			maxResults = int(value.Int())
		}
		outcome := claudetelemetry.ToolSearchOutcomeFromQuery(query, maxResults, len(gjson.GetBytes(data, "matches").Array()), int(gjson.GetBytes(data, "total_deferred_tools").Int()))
		if err := e.desktopTelemetry.RecordSDKToolSearchOutcome(ctx, auth, sessionID, caller.Model, caller.PromptID, outcome); err != nil {
			helps.LogWithRequestID(ctx).WithError(err).Warn("claude desktop: tool search outcome telemetry was not persisted")
		}
	}
}
