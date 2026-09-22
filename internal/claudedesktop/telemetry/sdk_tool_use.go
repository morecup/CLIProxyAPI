package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Tool-use events of the generic tool wrapper of the pinned SDK (Claude Code
// 2.1.247, module _448.js; pins in testdata/sdk-telemetry-tool-use-native.json).
// Per tool_use the wrapper emits tengu_tool_use_can_use_tool_allowed once the
// permission result is allow (the owned family has no permission prompt), then
// after a resolved call tengu_feature_ok (feature counters, module _823.js)
// immediately followed by tengu_tool_use_success. A validateInput rejection
// happens before the permission check: it emits tengu_feature_sad
// tool_validate_input_rejected (plus tengu_tool_use_error, not emitted here)
// and none of the allow/success events. Property order follows the native
// object literals; the native logger prepends subscription_type and
// cc_prompt_id.
const (
	FactSDKToolUseCanUseToolAllowed = "tool_use_can_use_tool_allowed"
	FactSDKToolUseSuccess           = "tool_use_success"
	FactSDKFeatureOk                = "feature_ok"
	FactSDKFeatureSad               = "feature_sad"

	// Native te(f, code) error codes of the wrapper.
	ToolFeatureErrorValidateInputRejected = "tool_validate_input_rejected"
	ToolFeatureErrorInputValidationFailed = "tool_input_validation_failed"
)

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKToolUseCanUseToolAllowed: "tengu_tool_use_can_use_tool_allowed",
		FactSDKToolUseSuccess:           "tengu_tool_use_success",
		FactSDKFeatureOk:                "tengu_feature_ok",
		FactSDKFeatureSad:               "tengu_feature_sad",
	})
	// Native _675.js Datadog mirror set (nK) contains the three names.
	registerExecutableEvents(datadogLogsRole, map[string]string{
		FactSDKToolUseSuccess: "tengu_tool_use_success",
		FactSDKFeatureOk:      "tengu_feature_ok",
		FactSDKFeatureSad:     "tengu_feature_sad",
	})
}

// ToolUse identifies one owned tool execution the way the wrapper sees it.
type ToolUse struct {
	// MessageID is the assistant message id that carried the tool_use
	// (native messageID: yt(assistantMessage.id)).
	MessageID string
	// ToolName is the native tool name; the loggable form (fr) is applied at
	// serialization.
	ToolName string
	// Input is the tool_use input (toolInputSizeBytes = JSON.stringify(input).length).
	Input json.RawMessage
	// Depth is the caller's agent depth; 0 is the main agent whose query
	// lineage this package derives itself.
	Depth int
}

// ToolUseOutcome is the result of one owned tool execution.
type ToolUseOutcome struct {
	ToolUse ToolUse
	// Success is true when call resolved (data returned).
	Success bool
	// DurationMs is the call wall clock (native Date.now()-U).
	DurationMs int64
	// ResultSizeBytes is the native toolResultSizeBytes: the rendered
	// tool_result content string length, or JSON.stringify(content).length
	// for block arrays (UTF-16 units).
	ResultSizeBytes int
	// ValidationRejected marks a native validateInput rejection.
	ValidationRejected bool
	// InputValidationFailed marks a schema/JSON input failure (gOs).
	InputValidationFailed bool
}

type sdkToolUseCanUseToolAllowedMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	MessageID        string `json:"messageID,omitempty"`
	ToolName         string `json:"toolName"`
	QueryChainID     string `json:"queryChainId,omitempty"`
	QueryDepth       *int   `json:"queryDepth,omitempty"`
}

// rss/heapUsed/external deltas are process.memoryUsage() differences across
// the call natively; the gateway has no per-call memory sample, so the three
// keys are omitted (documented deviation, never a synthetic value).
type sdkToolUseSuccessMetadata struct {
	SubscriptionType      string `json:"subscription_type,omitempty"`
	PromptID              string `json:"cc_prompt_id,omitempty"`
	MessageID             string `json:"messageID,omitempty"`
	ToolName              string `json:"toolName"`
	IsMCP                 bool   `json:"isMcp"`
	DurationMs            int64  `json:"durationMs"`
	PreToolHookDurationMs int64  `json:"preToolHookDurationMs"`
	PermissionDurationMs  int64  `json:"permissionDurationMs"`
	ToolResultSizeBytes   int    `json:"toolResultSizeBytes"`
	ToolInputSizeBytes    int    `json:"toolInputSizeBytes"`
	QueryChainID          string `json:"queryChainId,omitempty"`
	QueryDepth            *int   `json:"queryDepth,omitempty"`
}

type sdkFeatureMetadata struct {
	SubscriptionType string `json:"subscription_type,omitempty"`
	PromptID         string `json:"cc_prompt_id,omitempty"`
	FeatureName      string `json:"feature_name"`
	ErrorCode        string `json:"error_code,omitempty"`
}

var sdkLoggableIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SDKLoggableID mirrors the native yt/Ms guard for a defined id: conforming
// ids pass, anything else (including an empty string) becomes
// "nonconforming". An absent id (native undefined) is handled by the caller,
// which leaves the key out as JSON.stringify does.
func SDKLoggableID(id string) string {
	if sdkLoggableIDPattern.MatchString(id) {
		return id
	}
	return "nonconforming"
}

// sdkOptionalLoggableID applies SDKLoggableID to an id the gateway may not
// know: an empty gateway value is the native undefined and stays omitted.
func sdkOptionalLoggableID(id string) string {
	if id == "" {
		return ""
	}
	return SDKLoggableID(id)
}

// SDKLoggableToolName mirrors fr/lU: MCP tools collapse to "mcp_tool"; the Bl
// alias map only renames two non-owned internal names, so owned names pass
// through.
func SDKLoggableToolName(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		return "mcp_tool"
	}
	return name
}

// sdkSnakeWords splits an ASCII tool name the way lodash words() does for
// the bundled snakeCase compounder (written without look-ahead because Go
// regexp has none): an acronym run keeps its letters except the one that
// starts the following capitalised word, a capitalised or lowercase word ends
// at the next non-letter, digit runs are their own words, and any other
// character separates words. The pinned reducer (_823.js Ze) joins the
// lowercased words with "_". The owned tool names contain no digits; the
// digit split follows lodash rather than the audit's re-created regex.
func sdkSnakeWords(name string) []string {
	isUpper := func(c byte) bool { return c >= 'A' && c <= 'Z' }
	isLower := func(c byte) bool { return c >= 'a' && c <= 'z' }
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	isWord := func(c byte) bool { return isUpper(c) || isLower(c) || isDigit(c) || c == '_' }
	var words []string
	for i := 0; i < len(name); {
		c := name[i]
		switch {
		case isUpper(c):
			j := i
			for j < len(name) && isUpper(name[j]) {
				j++
			}
			if run := j - i; run >= 2 {
				if j < len(name) && isLower(name[j]) && run-1 >= 2 {
					words = append(words, name[i:j-1])
					i = j - 1
					continue
				}
				if j >= len(name) || !isWord(name[j]) {
					words = append(words, name[i:j])
					i = j
					continue
				}
			}
			k := i + 1
			if k < len(name) && isLower(name[k]) {
				for k < len(name) && isLower(name[k]) {
					k++
				}
				words = append(words, name[i:k])
				i = k
				continue
			}
			words = append(words, name[i:i+1])
			i++
		case isLower(c):
			k := i
			for k < len(name) && isLower(name[k]) {
				k++
			}
			words = append(words, name[i:k])
			i = k
		case isDigit(c):
			k := i
			for k < len(name) && isDigit(name[k]) {
				k++
			}
			words = append(words, name[i:k])
			i = k
		default:
			i++
		}
	}
	return words
}

// SDKToolFeatureName mirrors toolFeature (_823.js Be): "tool_" + lodash
// snakeCase(name).
func SDKToolFeatureName(name string) string {
	words := sdkSnakeWords(strings.ReplaceAll(strings.ReplaceAll(name, "'", ""), "\u2019", ""))
	for index, word := range words {
		words[index] = strings.ToLower(word)
	}
	return "tool_" + strings.Join(words, "_")
}

// SDKToolInputSizeBytes mirrors le(input).length: the compact JSON text
// length in UTF-16 units.
func SDKToolInputSizeBytes(input json.RawMessage) int {
	if len(bytes.TrimSpace(input)) == 0 {
		return 0
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, input); err != nil {
		return javascriptUTF16Length(string(input))
	}
	return javascriptUTF16Length(compact.String())
}

// SDKToolResultSizeBytes mirrors toolResultSizeBytes over a rendered
// tool_result block: string content length, JSON.stringify length for block
// arrays, 0 for empty content.
func SDKToolResultSizeBytes(block json.RawMessage) int {
	var result struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(block, &result) != nil || len(result.Content) == 0 || string(result.Content) == "null" {
		return 0
	}
	var text string
	if json.Unmarshal(result.Content, &text) == nil {
		return javascriptUTF16Length(text)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, result.Content); err != nil {
		return 0
	}
	return len(utf16.Encode([]rune(compact.String())))
}

// RecordSDKToolUse emits the wrapper events of one owned tool execution
// outside a request span (the tool runs between two owned requests). session
// and model identify the owned query's SDK session; promptID is the caller's
// prompt (native cc_prompt_id). Query lineage is derived for depth-0 callers
// exactly as the main request span derives it (sdkQueryLineage); deeper
// callers omit queryChainId/queryDepth because their chain is only known to
// the request span.
func (m *Manager) RecordSDKToolUse(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID string, outcome ToolUseOutcome) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if m.ctx.Err() != nil {
		return fmt.Errorf("Claude Desktop telemetry owner is unavailable")
	}
	session, model = strings.TrimSpace(session), strings.TrimSpace(model)
	if session == "" || model == "" {
		return fmt.Errorf("Claude Desktop tool use identity is incomplete")
	}
	betas, known := m.sdkProfile.InputBetaHeader(model)
	if !known {
		return fmt.Errorf("Claude Desktop tool use model %q has no profiled beta header", model)
	}
	worker, errWorker := m.workerForDelivery(auth, m.sdkDelivery)
	if errWorker != nil {
		return errWorker
	}
	facts := RequestFacts{SessionID: session, Model: model, Betas: betas, PromptID: strings.TrimSpace(promptID)}
	var chainID string
	var depth *int
	if outcome.ToolUse.Depth == 0 {
		chainID, depth = sdkQueryLineage(worker, RequestFacts{Role: claudeprofile.RoleMain, SessionID: session, PromptID: facts.PromptID})
	}
	subscription := subscriptionType(worker.authSnapshot())
	emit := func(fact string, metadata any) error {
		if errEnqueue := m.enqueueSDKEvent(worker, fact, facts, metadata); errEnqueue != nil {
			worker.recordQueueFailure(errEnqueue)
			return errEnqueue
		}
		return nil
	}
	feature := SDKToolFeatureName(outcome.ToolUse.ToolName)
	switch {
	case outcome.ValidationRejected:
		return emit(FactSDKFeatureSad, sdkFeatureMetadata{SubscriptionType: subscription, PromptID: facts.PromptID, FeatureName: feature, ErrorCode: ToolFeatureErrorValidateInputRejected})
	case outcome.InputValidationFailed:
		return emit(FactSDKFeatureSad, sdkFeatureMetadata{SubscriptionType: subscription, PromptID: facts.PromptID, FeatureName: feature, ErrorCode: ToolFeatureErrorInputValidationFailed})
	}
	messageID, toolName := sdkOptionalLoggableID(outcome.ToolUse.MessageID), SDKLoggableToolName(outcome.ToolUse.ToolName)
	if err := emit(FactSDKToolUseCanUseToolAllowed, sdkToolUseCanUseToolAllowedMetadata{SubscriptionType: subscription, PromptID: facts.PromptID, MessageID: messageID, ToolName: toolName, QueryChainID: chainID, QueryDepth: depth}); err != nil {
		return err
	}
	if !outcome.Success {
		// Errors thrown inside call feed tengu_feature_sad/bad through BUr,
		// whose classification tail is not pinned; only the allow event is
		// emitted for them.
		return nil
	}
	if err := emit(FactSDKFeatureOk, sdkFeatureMetadata{SubscriptionType: subscription, PromptID: facts.PromptID, FeatureName: feature}); err != nil {
		return err
	}
	return emit(FactSDKToolUseSuccess, sdkToolUseSuccessMetadata{
		SubscriptionType: subscription, PromptID: facts.PromptID, MessageID: messageID, ToolName: toolName, IsMCP: false,
		DurationMs: outcome.DurationMs, ToolResultSizeBytes: outcome.ResultSizeBytes, ToolInputSizeBytes: SDKToolInputSizeBytes(outcome.ToolUse.Input),
		QueryChainID: chainID, QueryDepth: depth,
	})
}
