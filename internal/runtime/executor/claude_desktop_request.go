package executor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeDesktopPlanningError struct {
	statusErr
}

func (claudeDesktopPlanningError) IsRequestScoped() bool { return true }

// Desktop title generation uses the captured Haiku helper independently of
// the model selected for the main conversation.
const claudeDesktopTitleModel = "claude-haiku-4-5-20251001"

type claudeDesktopRequestPlan struct {
	Variant               claudeprofile.RequestVariant
	BetaVariant           string
	SelectedBetas         []string
	CompactionRequestKind string
	PromptID              string
	ClientRequestID       string
	NativePrompt          *claudeprompt.Request
	ProgramOwnedSystem    bool
}

type claudeDesktopRuntimeFacts struct {
	ContextLease      *helps.ClaudeDesktopContextLease
	ContextError      error
	ContextSaved      bool
	Input             claudeprompt.Submission
	Prompt            *claudeprompt.Request
	Now               time.Time
	SessionID         string
	PromptID          string
	ClientRequestID   string
	PreviousRequestID string
	LogicalModel      string
	DesktopVersion    string
	CodeVersion       string
	AgentSDKVersion   string
	WorkingDir        string
	UserHome          string
	MemoryDir         string
	ScratchpadDir     string
}

// One synchronous executor invocation owns these values. A recovery replaces
// them only after validating and committing the final continuation request.
type claudeDesktopRequestExecution struct {
	ctx         context.Context
	body        []byte
	sourceBody  []byte
	plan        claudeDesktopRequestPlan
	facts       claudeDesktopRuntimeFacts
	span        *claudeDesktopRequestSpan
	lineage     claudeDesktopLineageRequestState
	diagnostics claudeDiagnosticsRequestState
}

func (p claudeDesktopRequestPlan) anthropicBeta() string {
	betas := p.SelectedBetas
	if len(betas) == 0 {
		betas = p.Variant.AnthropicBeta
	}
	return strings.Join(betas, ",")
}

func (p claudeDesktopRequestPlan) hasBeta(beta string) bool {
	for _, candidate := range strings.Split(p.anthropicBeta(), ",") {
		if strings.TrimSpace(candidate) == beta {
			return true
		}
	}
	return false
}

func (e *ClaudeExecutor) planClaudeDesktopRequest(body []byte, role claudeprofile.RequestRole) (claudeDesktopRequestPlan, error) {
	return e.planClaudeDesktopRequestWithHints(body, role, "", nil)
}

func (e *ClaudeExecutor) planClaudeDesktopRequestWithHints(body []byte, role claudeprofile.RequestRole, logicalModel string, incomingHeaders http.Header) (claudeDesktopRequestPlan, error) {
	return e.planClaudeDesktopRequestInContext(nil, body, role, logicalModel, incomingHeaders)
}

func (e *ClaudeExecutor) planClaudeDesktopRequestInContext(ctx context.Context, body []byte, role claudeprofile.RequestRole, logicalModel string, incomingHeaders http.Header) (claudeDesktopRequestPlan, error) {
	if e == nil || !e.desktopOnly {
		return claudeDesktopRequestPlan{}, nil
	}
	if e.desktopProfileErr != nil {
		return claudeDesktopRequestPlan{}, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: "claude desktop profile is unavailable: " + e.desktopProfileErr.Error()}}
	}
	if e.desktopProfile == nil {
		return claudeDesktopRequestPlan{}, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: "claude desktop profile is unavailable"}}
	}
	if role == "" {
		role = e.classifyClaudeDesktopRequestRole(body)
	}
	model := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(logicalModel) == "" {
		logicalModel = model
	}
	if role == claudeprofile.RoleTitle {
		model = claudeDesktopTitleModel
		logicalModel = claudeDesktopTitleModel
	}
	key := claudeprofile.RequestVariantKey{
		Model:           model,
		LogicalModel:    logicalModel,
		Role:            role,
		Diagnostics:     gjson.GetBytes(body, "diagnostics").Exists(),
		ThinkingDisplay: claudeDesktopThinkingDisplay(body, role, model, logicalModel),
	}
	if role == claudeprofile.RoleCountTokens {
		key.Diagnostics = false
	}
	variant, errResolve := e.desktopProfile.Resolve(key)
	if errResolve != nil {
		return claudeDesktopRequestPlan{}, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: errResolve.Error()}}
	}
	selectedName, selectedBetas, errBetas := selectClaudeDesktopBetaVariant(body, incomingHeaders, variant)
	if errBetas != nil {
		return claudeDesktopRequestPlan{}, errBetas
	}
	plan := claudeDesktopRequestPlan{Variant: variant, BetaVariant: selectedName, SelectedBetas: selectedBetas}
	if role == claudeprofile.RoleCompaction {
		kind, errKind := helps.ClaudeDesktopCompactionRequestKind(ctx, incomingHeaders)
		if errKind != nil {
			return claudeDesktopRequestPlan{}, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: errKind.Error()}}
		}
		plan.CompactionRequestKind = kind
	}
	return plan, nil
}

func claudeDesktopThinkingDisplay(body []byte, role claudeprofile.RequestRole, model, logicalModel string) string {
	switch role {
	case claudeprofile.RoleMain:
		// Desktop 1.40609 switched the current-model main request profiles to
		// incremental thinking updates. Historical models and the observed
		// Sonnet fallback for a logical Opus request retain their compiled
		// display value instead of inheriting this current-model rule.
		if normalizeClaudeDesktopCurrentModel(model) != "" && strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(logicalModel)) {
			return "updates"
		}
		fallthrough
	case claudeprofile.RoleCompaction:
		if strings.EqualFold(strings.TrimSpace(model), "claude-opus-5-5") && strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(logicalModel)) {
			return "updates"
		}
		display := gjson.GetBytes(body, "thinking.display")
		if !display.Exists() {
			return "omitted"
		}
		if value := strings.ToLower(strings.TrimSpace(display.String())); value != "" {
			return value
		}
		return "present"
	case claudeprofile.RoleSubagent:
		if strings.EqualFold(strings.TrimSpace(model), "claude-sonnet-5") {
			return "updates"
		}
		display := gjson.GetBytes(body, "thinking.display")
		if !display.Exists() {
			return "omitted"
		}
		if value := strings.ToLower(strings.TrimSpace(display.String())); value != "" {
			return value
		}
		return "present"
	default:
		return ""
	}
}

func normalizeClaudeDesktopCurrentModel(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001":
		return strings.ToLower(strings.TrimSpace(model))
	default:
		return ""
	}
}

func selectClaudeDesktopBetaVariant(body []byte, incomingHeaders http.Header, variant claudeprofile.RequestVariant) (string, []string, error) {
	fast := strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "speed").String()), "fast")
	computer := claudeRequestUsesComputerToolset(body)
	requestedVariant := ""
	switch {
	case fast && computer:
		requestedVariant = "fast-computer"
	case fast:
		requestedVariant = "fast"
	case computer:
		requestedVariant = "computer"
	}
	if requestedVariant != "" {
		betas, ok := variant.AnthropicBetaVariants[requestedVariant]
		if !ok {
			return "", nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop profile has no observed %s beta variant for this request", requestedVariant)}}
		}
		return requestedVariant, append([]string(nil), betas...), nil
	}
	candidates := []string{strings.Join(incomingHeaders.Values("Anthropic-Beta"), ",")}
	bodyBetas := gjson.GetBytes(body, "anthropic_beta")
	if bodyBetas.Type == gjson.String {
		candidates = append(candidates, bodyBetas.String())
	} else if bodyBetas.IsArray() {
		values := make([]string, 0, len(bodyBetas.Array()))
		for _, value := range bodyBetas.Array() {
			values = append(values, value.String())
		}
		candidates = append(candidates, strings.Join(values, ","))
	}
	for _, candidate := range candidates {
		candidateBetas := splitClaudeDesktopBetas(candidate)
		if len(candidateBetas) == 0 {
			continue
		}
		if equalClaudeDesktopBetas(candidateBetas, variant.AnthropicBeta) {
			return "", append([]string(nil), variant.AnthropicBeta...), nil
		}
		for name, betas := range variant.AnthropicBetaVariants {
			if equalClaudeDesktopBetas(candidateBetas, betas) {
				return name, append([]string(nil), betas...), nil
			}
		}
	}
	return "", append([]string(nil), variant.AnthropicBeta...), nil
}

func splitClaudeDesktopBetas(value string) []string {
	var result []string
	for _, beta := range strings.Split(value, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			result = append(result, beta)
		}
	}
	return result
}

func equalClaudeDesktopBetas(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (e *ClaudeExecutor) classifyClaudeDesktopRequestRole(body []byte) claudeprofile.RequestRole {
	system := gjson.GetBytes(body, "system")
	if system.IsArray() && strings.Contains(system.Get("0.text").String(), "cc_is_subagent=true") {
		return claudeprofile.RoleSubagent
	}
	lastUserText := claudeDesktopLastUserText(body)
	if e != nil && e.desktopProfile != nil && helps.IsClaudeDesktopCompactionInstruction(e.desktopProfile.DesktopVersion, e.desktopProfile.CodeVersion, lastUserText) {
		return claudeprofile.RoleCompaction
	}
	maxTokens := gjson.GetBytes(body, "max_tokens").Int()
	stream := gjson.GetBytes(body, "stream")
	tools := gjson.GetBytes(body, "tools")
	if maxTokens == 64 && !stream.Exists() && len(tools.Array()) == 0 &&
		gjson.GetBytes(body, "thinking.type").String() == "disabled" &&
		gjson.GetBytes(body, "stop_sequences").IsArray() && len(gjson.GetBytes(body, "stop_sequences").Array()) > 0 &&
		claudeDesktopSystemBlockCount(system) == 3 {
		return claudeprofile.RoleSecurityMonitor
	}
	if maxTokens == 1024 && !stream.Exists() && (!tools.Exists() || len(tools.Array()) == 0) {
		return claudeprofile.RoleLightHelper
	}
	if maxTokens == 32000 && stream.Bool() && isClaudeDesktopWebSearchHelper(tools) {
		return claudeprofile.RoleWebSearchHelper
	}
	if maxTokens == 32000 && stream.Bool() && gjson.GetBytes(body, "output_config").Exists() && len(tools.Array()) == 0 {
		return claudeprofile.RoleTitle
	}
	return claudeprofile.RoleMain
}

// Only an executable task admitted by the owned remote query can select this
// role internally. Ordinary callers keep the existing profile classifier.
func (e *ClaudeExecutor) ownedClaudeDesktopRequestRole(ctx context.Context, body []byte) claudeprofile.RequestRole {
	if e.hasOwnedClaudeDesktopAgent(ctx) {
		return claudeprofile.RoleSubagent
	}
	return e.classifyClaudeDesktopRequestRole(body)
}

func (e *ClaudeExecutor) hasOwnedClaudeDesktopAgent(ctx context.Context) bool {
	if ctx != nil {
		owner, _ := ctx.Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if owner.manager == e.desktopATIS && owner.host != nil && owner.remoteInput && owner.agentID != "" && owner.host.Context().Err() == nil {
			return true
		}
	}
	return false
}

func claudeDesktopLastUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages").Array()
	for messageIndex := len(messages) - 1; messageIndex >= 0; messageIndex-- {
		message := messages[messageIndex]
		if message.Get("role").String() != "user" {
			continue
		}
		content := message.Get("content")
		if content.Type == gjson.String && strings.TrimSpace(content.String()) != "" {
			return content.String()
		}
		blocks := content.Array()
		for blockIndex := len(blocks) - 1; blockIndex >= 0; blockIndex-- {
			block := blocks[blockIndex]
			if block.Get("type").String() == "text" && strings.TrimSpace(block.Get("text").String()) != "" {
				return block.Get("text").String()
			}
		}
	}
	return ""
}

func isClaudeDesktopWebSearchHelper(tools gjson.Result) bool {
	items := tools.Array()
	if len(items) != 1 {
		return false
	}
	tool := items[0]
	return tool.Get("name").String() == "web_search" && tool.Get("type").String() == "web_search_20250305" && tool.Get("max_uses").Int() == 8
}

func (e *ClaudeExecutor) newClaudeDesktopRuntimeFacts(auth *cliproxyauth.Auth, sessionID, logicalModel, promptID, clientRequestID, previousRequestID string, metadata ...map[string]any) claudeDesktopRuntimeFacts {
	return e.newClaudeDesktopRuntimeFactsForPlan(auth, sessionID, logicalModel, promptID, clientRequestID, previousRequestID, claudeDesktopRequestPlan{}, metadata...)
}

func (e *ClaudeExecutor) newClaudeDesktopRuntimeFactsForPlan(auth *cliproxyauth.Auth, sessionID, logicalModel, promptID, clientRequestID, previousRequestID string, plan claudeDesktopRequestPlan, metadata ...map[string]any) claudeDesktopRuntimeFacts {
	if plan.Variant.Key.Role == claudeprofile.RoleTitle && strings.TrimSpace(plan.Variant.Key.LogicalModel) != "" {
		logicalModel = plan.Variant.Key.LogicalModel
	}
	workingDir := claudeDesktopWorkingDir(metadata...)
	desktopVersion, codeVersion, agentSDKVersion := "", "", ""
	if e != nil && e.desktopProfile != nil {
		desktopVersion = strings.TrimSpace(e.desktopProfile.DesktopVersion)
		codeVersion = strings.TrimSpace(e.desktopProfile.CodeVersion)
		agentSDKVersion = strings.TrimSpace(e.desktopProfile.AgentSDKVersion)
		profileWorkingDir := strings.TrimSpace(e.desktopProfile.Environment.DefaultWorkingDir)
		if plan.Variant.Key.Model != "" {
			if requestProfile, errProfile := e.desktopProfile.RequestProfileForVariant(plan.Variant); errProfile == nil {
				desktopVersion = strings.TrimSpace(requestProfile.DesktopVersion)
				codeVersion = strings.TrimSpace(requestProfile.CodeVersion)
				agentSDKVersion = strings.TrimSpace(requestProfile.AgentSDKVersion)
				profileWorkingDir = strings.TrimSpace(requestProfile.Environment.DefaultWorkingDir)
			}
		}
		if workingDir == "" {
			workingDir = profileWorkingDir
		}
	}
	userHome, _ := os.UserHomeDir()
	if userHome == "" {
		userHome = filepath.Dir(workingDir)
	}
	projectName := strings.NewReplacer(":", "-", "\\", "-", "/", "-").Replace(filepath.Clean(workingDir))
	memoryDir := filepath.Join(userHome, ".claude", "projects", projectName, "memory")
	scratchpadDir := filepath.Join(os.TempDir(), "claude", strings.TrimSpace(sessionID))
	return claudeDesktopRuntimeFacts{
		Now: claudeDesktopCurrentTime(auth), SessionID: strings.TrimSpace(sessionID), PromptID: strings.TrimSpace(promptID),
		ClientRequestID: strings.TrimSpace(clientRequestID), PreviousRequestID: strings.TrimSpace(previousRequestID),
		LogicalModel: strings.ToLower(strings.TrimSpace(logicalModel)), DesktopVersion: desktopVersion, CodeVersion: codeVersion,
		AgentSDKVersion: agentSDKVersion, WorkingDir: workingDir, UserHome: userHome,
		MemoryDir: memoryDir, ScratchpadDir: scratchpadDir,
	}
}

func claudeDesktopWorkingDir(metadata ...map[string]any) string {
	for _, values := range metadata {
		for _, key := range []string{"working_dir", "cwd"} {
			value, _ := values[key].(string)
			value = strings.TrimSpace(value)
			if value != "" && filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
		}
	}
	if current, errGetwd := os.Getwd(); errGetwd == nil {
		return filepath.Clean(current)
	}
	return ""
}

func (e *ClaudeExecutor) applyClaudeDesktopMessageProfile(ctx context.Context, auth *cliproxyauth.Auth, payload []byte, cchSigning bool, plan claudeDesktopRequestPlan, runtimeFacts ...claudeDesktopRuntimeFacts) ([]byte, bool, error) {
	if e == nil || !e.desktopOnly {
		return payload, false, nil
	}
	facts := claudeDesktopRuntimeFacts{Now: claudeDesktopCurrentTime(auth), LogicalModel: plan.Variant.Key.LogicalModel}
	if len(runtimeFacts) > 0 {
		facts = runtimeFacts[0]
	}
	requestProfile, errProfile := e.desktopProfile.RequestProfileForVariant(plan.Variant)
	if errProfile != nil {
		return nil, false, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: errProfile.Error()}}
	}
	var errBody error
	payload, errBody = e.normalizeClaudeDesktopBody(payload, plan)
	if errBody != nil {
		return nil, false, errBody
	}
	values, errValues := e.claudeDesktopArtifactValues(facts, plan)
	if errValues != nil {
		return nil, false, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: errValues.Error()}}
	}
	// External subagent requests still require their verified native system.
	// An executable owned task has no caller-supplied system: render its own
	// pinned role artifacts with the child request's identity and model facts.
	ownedAgent := plan.Variant.Key.Role == claudeprofile.RoleSubagent && e.hasOwnedClaudeDesktopAgent(ctx)
	if plan.Variant.TopLevelSystemPolicy == claudeprofile.SystemPolicyPreserveVerified && !ownedAgent && !plan.ProgramOwnedSystem {
		if errShape := e.validateClaudeDesktopSystem(payload, plan, values, cchSigning); errShape != nil {
			return nil, false, errShape
		}
		payload = enforceCacheControlLimitPreservingSystem(payload, 4)
		return payload, true, nil
	}
	// Verified Desktop Code input is a semantic input signal only. Its caller
	// system must not become an alternate instruction path after the owned
	// top-level system is rendered.
	var instructions []string
	if !plan.ProgramOwnedSystem {
		if errSystem := validateClaudeCallerSystemBlocks(gjson.GetBytes(payload, "system")); errSystem != nil {
			return nil, false, errSystem
		}
		var errInstructions error
		instructions, errInstructions = e.collectClaudeDesktopCallerInstructions(gjson.GetBytes(payload, "system"), values, plan)
		if errInstructions != nil {
			return nil, false, errInstructions
		}
	}
	billing := generateClaudeDesktopBillingHeader(cchSigning, requestProfile.CodeVersion, payload, plan, facts)
	systemJSON, errRender := e.renderClaudeDesktopSystem(plan, values, billing)
	if errRender != nil {
		return nil, false, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: errRender.Error()}}
	}
	payload, errSet := sjson.SetRawBytes(payload, "system", systemJSON)
	if errSet != nil {
		return nil, false, fmt.Errorf("render Claude Desktop system: %w", errSet)
	}
	beforeCarrier := payload
	payload = applyClaudeDesktopInstructionCarrier(payload, instructions, plan.Variant.InstructionCarrier)
	if len(instructions) != 0 && plan.Variant.InstructionCarrier != claudeprofile.CarrierNone {
		facts.Prompt.ObserveOwnedInstructionCarrier(beforeCarrier, payload)
	}
	if errShape := e.validateClaudeDesktopSystem(payload, plan, values, cchSigning); errShape != nil {
		return nil, false, errShape
	}
	payload = enforceCacheControlLimitPreservingSystem(payload, 4)
	_ = ctx
	return payload, true, nil
}

func claudeDesktopCurrentTime(auth *cliproxyauth.Auth) time.Time {
	if timezone := claudeCredentialTimezone(auth); timezone != "" {
		if location, errLocation := time.LoadLocation(timezone); errLocation == nil {
			return time.Now().In(location)
		}
	}
	return time.Now()
}

func (e *ClaudeExecutor) applyClaudeDesktopCountTokensProfile(payload []byte, plan claudeDesktopRequestPlan) ([]byte, bool, error) {
	if e == nil || !e.desktopOnly {
		return payload, false, nil
	}
	var errBody error
	payload, errBody = e.normalizeClaudeDesktopBody(payload, plan)
	if errBody != nil {
		return nil, false, errBody
	}
	if errSystem := validateClaudeCallerSystemBlocks(gjson.GetBytes(payload, "system")); errSystem != nil {
		return nil, false, errSystem
	}
	values, errValues := e.claudeDesktopArtifactValues(claudeDesktopRuntimeFacts{Now: time.Now(), LogicalModel: plan.Variant.Key.LogicalModel}, plan)
	if errValues != nil {
		return nil, false, errValues
	}
	instructions, errInstructions := e.collectClaudeDesktopCallerInstructions(gjson.GetBytes(payload, "system"), values, plan)
	if errInstructions != nil {
		return nil, false, errInstructions
	}
	if updated, errDelete := sjson.DeleteBytes(payload, "system"); errDelete == nil {
		payload = updated
	}
	payload = applyClaudeDesktopInstructionCarrier(payload, instructions, plan.Variant.InstructionCarrier)
	if gjson.GetBytes(payload, "system").Exists() {
		return nil, false, claudeDesktopPlanningError{statusErr{code: http.StatusInternalServerError, msg: "claude desktop count_tokens must omit top-level system"}}
	}
	payload = applyClaudeDesktopStreamPolicy(payload, plan, false)
	return payload, true, nil
}

func (e *ClaudeExecutor) normalizeClaudeDesktopBody(payload []byte, plan claudeDesktopRequestPlan) ([]byte, error) {
	if e == nil || !e.desktopOnly {
		return payload, nil
	}
	if e.desktopProfile == nil {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: "claude desktop profile is unavailable"}}
	}
	bodyProfile, errProfile := e.desktopProfile.BodyForVariant(plan.Variant)
	if errProfile != nil {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: errProfile.Error()}}
	}
	var errSet error
	if plan.Variant.Key.Role == claudeprofile.RoleTitle {
		payload, errSet = sjson.SetBytes(payload, "model", plan.Variant.Key.Model)
		if errSet != nil {
			return nil, fmt.Errorf("set Claude Desktop title model: %w", errSet)
		}
	}
	if bodyProfile.MaxTokens > 0 {
		payload, errSet = sjson.SetBytes(payload, "max_tokens", bodyProfile.MaxTokens)
		if errSet != nil {
			return nil, fmt.Errorf("set Claude Desktop max_tokens: %w", errSet)
		}
	}
	for key, raw := range map[string][]byte{
		"thinking":           bodyProfile.Thinking,
		"context_management": bodyProfile.ContextManagement,
		"fallbacks":          bodyProfile.Fallbacks,
		"output_config":      bodyProfile.OutputConfig,
		"diagnostics":        bodyProfile.Diagnostics,
		"temperature":        bodyProfile.Temperature,
		"tool_choice":        bodyProfile.ToolChoice,
	} {
		if key == "diagnostics" && len(raw) > 0 {
			previous := gjson.GetBytes(payload, "diagnostics.previous_message_id")
			if previous.Exists() && (previous.Type == gjson.String || previous.Type == gjson.Null) {
				if updated, errPrevious := sjson.SetRawBytes(raw, "previous_message_id", []byte(previous.Raw)); errPrevious == nil {
					raw = updated
				}
			}
		}
		if len(raw) > 0 {
			payload, errSet = sjson.SetRawBytes(payload, key, raw)
		} else {
			payload, errSet = sjson.DeleteBytes(payload, key)
		}
		if errSet != nil {
			return nil, fmt.Errorf("apply Claude Desktop %s profile: %w", key, errSet)
		}
	}
	if bodyProfile.EnsureTools && !gjson.GetBytes(payload, "tools").Exists() {
		payload, errSet = sjson.SetRawBytes(payload, "tools", []byte("[]"))
		if errSet != nil {
			return nil, fmt.Errorf("set Claude Desktop tools: %w", errSet)
		}
	}
	if len(bodyProfile.Diagnostics) == 0 && !plan.Variant.Key.Diagnostics {
		payload, errSet = sjson.DeleteBytes(payload, "diagnostics")
		if errSet != nil {
			return nil, fmt.Errorf("remove unprofiled Claude Desktop diagnostics: %w", errSet)
		}
	}
	return payload, nil
}

func (e *ClaudeExecutor) finalizeClaudeDesktopBody(payload []byte, plan claudeDesktopRequestPlan) ([]byte, error) {
	if e == nil || !e.desktopOnly {
		return payload, nil
	}
	bodyProfile, errProfile := e.desktopProfile.BodyForVariant(plan.Variant)
	if errProfile != nil {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: errProfile.Error()}}
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: "claude desktop request body must be a JSON object"}}
	}
	if !root.Get("model").Exists() || !root.Get("messages").IsArray() {
		return nil, claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: "claude desktop request body requires model and messages"}}
	}

	allowed := make(map[string]struct{}, len(bodyProfile.TopLevelOrder))
	ordered := make([]byte, 0, len(payload))
	ordered = append(ordered, '{')
	first := true
	for _, key := range bodyProfile.TopLevelOrder {
		allowed[key] = struct{}{}
		value := root.Get(key)
		if !value.Exists() {
			continue
		}
		if !first {
			ordered = append(ordered, ',')
		}
		first = false
		ordered = append(ordered, '"')
		ordered = append(ordered, key...)
		ordered = append(ordered, '"', ':')
		ordered = append(ordered, value.Raw...)
	}
	if !bodyProfile.RemoveUnlistedKeys {
		for key, value := range root.Map() {
			if _, ok := allowed[key]; ok {
				continue
			}
			if !first {
				ordered = append(ordered, ',')
			}
			first = false
			ordered = append(ordered, '"')
			ordered = append(ordered, key...)
			ordered = append(ordered, '"', ':')
			ordered = append(ordered, value.Raw...)
		}
	}
	ordered = append(ordered, '}')
	return ordered, nil
}

func applyClaudeDesktopInstructionCarrier(payload []byte, instructions []string, carrier claudeprofile.InstructionCarrier) []byte {
	if len(instructions) == 0 || carrier == claudeprofile.CarrierNone {
		return payload
	}
	if carrier == claudeprofile.CarrierUserSystemReminder {
		return prependClaudeSystemRemindersToFirstUserMessage(payload, instructions)
	}
	return insertClaudeMidConversationSystemMessages(payload, instructions)
}

func applyClaudeDesktopStreamPolicy(payload []byte, plan claudeDesktopRequestPlan, transportStream bool) []byte {
	switch plan.Variant.StreamPolicy {
	case claudeprofile.StreamPolicyRequiredTrue:
		return helps.SetBoolIfDifferent(payload, "stream", true)
	case claudeprofile.StreamPolicyOmitFalse:
		if transportStream {
			return helps.SetBoolIfDifferent(payload, "stream", true)
		}
		updated, errDelete := sjson.DeleteBytes(payload, "stream")
		if errDelete == nil {
			return updated
		}
	case claudeprofile.StreamPolicyForbidden:
		updated, errDelete := sjson.DeleteBytes(payload, "stream")
		if errDelete == nil {
			return updated
		}
	case claudeprofile.StreamPolicyTransport:
		return helps.SetBoolIfDifferent(payload, "stream", transportStream)
	}
	return payload
}

func (e *ClaudeExecutor) collectClaudeDesktopCallerInstructions(system gjson.Result, values map[string]string, plan claudeDesktopRequestPlan) ([]string, error) {
	reserved := map[string]struct{}{claudeDesktopHarnessIdentity: {}}
	requestProfile, errProfile := e.desktopProfile.RequestProfileForVariant(plan.Variant)
	if errProfile != nil {
		return nil, errProfile
	}
	for _, artifact := range requestProfile.Artifacts {
		rendered, errRender := artifact.Render(values)
		if errRender != nil {
			return nil, errRender
		}
		reserved[rendered] = struct{}{}
	}
	var instructions []string
	appendInstruction := func(text string) {
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "x-anthropic-billing-header:") || util.IsClaudeCodeAttributionSystemText(text) {
			return
		}
		if _, found := reserved[text]; found {
			return
		}
		instructions = append(instructions, text)
	}
	if system.IsArray() {
		system.ForEach(func(_, block gjson.Result) bool { appendInstruction(block.Get("text").String()); return true })
	} else if system.Type == gjson.String {
		appendInstruction(system.String())
	}
	return instructions, nil
}

func validateClaudeDesktopInstructionCarrier(payload []byte, plan claudeDesktopRequestPlan) error {
	if plan.Variant.InstructionCarrier == claudeprofile.CarrierNone {
		return nil
	}
	want := claudeprofile.CarrierMidConversationSystem
	if claudeUsesLegacySystemReminder(payload) {
		want = claudeprofile.CarrierUserSystemReminder
	}
	if plan.Variant.InstructionCarrier == want {
		return nil
	}
	return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: "claude desktop profile instruction carrier does not match the resolved model"}}
}

func validateClaudeDesktopSystemShape(payload []byte, plan claudeDesktopRequestPlan) error {
	count := claudeDesktopSystemBlockCount(gjson.GetBytes(payload, "system"))
	if count == plan.Variant.SystemBlockCount {
		return nil
	}
	return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop profile expected %d top-level system blocks for role %q, got %d", plan.Variant.SystemBlockCount, plan.Variant.Key.Role, count)}}
}

func (e *ClaudeExecutor) applyClaudeDesktopIdentity(body []byte, auth *cliproxyauth.Auth, sessionID string) ([]byte, error) {
	updated, _, errApply := helps.ApplyClaudeDesktopCredentialMetadata(body, auth, sessionID)
	if errApply != nil {
		return nil, fmt.Errorf("apply Claude Desktop credential metadata: %w", errApply)
	}
	return updated, nil
}
