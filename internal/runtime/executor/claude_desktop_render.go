package executor

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/tidwall/gjson"
)

func (e *ClaudeExecutor) claudeDesktopArtifactValues(facts claudeDesktopRuntimeFacts, plan claudeDesktopRequestPlan) (map[string]string, error) {
	if e == nil || e.desktopProfile == nil {
		return nil, fmt.Errorf("claude desktop profile is unavailable")
	}
	logicalModel := strings.ToLower(strings.TrimSpace(facts.LogicalModel))
	if logicalModel == "" {
		logicalModel = strings.ToLower(strings.TrimSpace(plan.Variant.Key.LogicalModel))
	}
	displayName := strings.TrimSpace(e.desktopProfile.Environment.ModelDisplayNames[logicalModel])
	if displayName == "" {
		displayName = strings.TrimSpace(e.desktopProfile.Environment.ModelDisplayNames[plan.Variant.Key.Model])
	}
	if displayName == "" {
		return nil, fmt.Errorf("claude desktop profile has no display name for logical model %q", logicalModel)
	}
	return map[string]string{
		"CURRENT_DATE_ISO":   facts.Now.Format("2006-01-02"),
		"WORKING_DIR":        facts.WorkingDir,
		"USER_HOME":          facts.UserHome,
		"MEMORY_DIR":         facts.MemoryDir,
		"SCRATCHPAD_DIR":     facts.ScratchpadDir,
		"OS_VERSION":         e.desktopProfile.Environment.OSVersion,
		"MODEL_DISPLAY_NAME": displayName,
		"UUID":               facts.PromptID,
	}, nil
}

func generateClaudeDesktopBillingHeader(cchSigning bool, version string, payload []byte, plan claudeDesktopRequestPlan, facts claudeDesktopRuntimeFacts) string {
	buildHash := computeFingerprint(claudeBillingFingerprintMessageText(payload), version)
	fields := []string{"cc_version=" + version + "." + buildHash, "cc_entrypoint=claude-desktop"}
	if cchSigning {
		fields = append(fields, "cch=00000")
	}
	if facts.PreviousRequestID != "" && (plan.Variant.Key.Role == claudeprofile.RoleMain || plan.Variant.Key.Role == claudeprofile.RoleCompaction) {
		fields = append(fields, "cc_prev_req="+facts.PreviousRequestID)
	}
	if plan.Variant.Key.Role == claudeprofile.RoleSubagent {
		fields = append(fields, "cc_is_subagent=true")
	}
	if (plan.Variant.Key.Diagnostics && plan.Variant.Key.Role == claudeprofile.RoleMain) || plan.Variant.Key.Role == claudeprofile.RoleSubagent {
		fields = append(fields, "cc_prompt_id="+facts.PromptID)
	}
	return "x-anthropic-billing-header: " + strings.Join(fields, "; ") + ";"
}

func (e *ClaudeExecutor) renderClaudeDesktopSystem(plan claudeDesktopRequestPlan, values map[string]string, billing string) ([]byte, error) {
	blocks := make([]string, 0, len(plan.Variant.System))
	for _, block := range plan.Variant.System {
		text := billing
		if block.Kind == "artifact" {
			artifact, errArtifact := e.desktopProfile.Artifact(block.Artifact)
			if errArtifact != nil {
				return nil, errArtifact
			}
			var errRender error
			text, errRender = artifact.Render(values)
			if errRender != nil {
				return nil, errRender
			}
		}
		blocks = append(blocks, buildClaudeDesktopTextBlock(text, block.CacheControl))
	}
	return []byte("[" + strings.Join(blocks, ",") + "]"), nil
}

func buildClaudeDesktopTextBlock(text string, cacheControl *claudeprofile.CacheControl) string {
	block := `{"type":"text","text":` + marshalJSONStringWithoutHTMLEscape(text)
	if cacheControl != nil {
		block += `,"cache_control":{"type":` + marshalJSONStringWithoutHTMLEscape(cacheControl.Type)
		if cacheControl.TTL != "" {
			block += `,"ttl":` + marshalJSONStringWithoutHTMLEscape(cacheControl.TTL)
		}
		if cacheControl.Scope != "" {
			block += `,"scope":` + marshalJSONStringWithoutHTMLEscape(cacheControl.Scope)
		}
		block += "}"
	}
	return block + "}"
}

func (e *ClaudeExecutor) validateClaudeDesktopSystem(payload []byte, plan claudeDesktopRequestPlan, values map[string]string, cchSigning bool) error {
	system := gjson.GetBytes(payload, "system")
	if !system.IsArray() || len(system.Array()) != len(plan.Variant.System) {
		return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop profile expected %d top-level system blocks for role %q, got %d", len(plan.Variant.System), plan.Variant.Key.Role, claudeDesktopSystemBlockCount(system))}}
	}
	for index, blockPlan := range plan.Variant.System {
		actual := system.Array()[index]
		if actual.Get("type").String() != "text" || actual.Get("text").Type != gjson.String {
			return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop system.%d is not a text block", index)}}
		}
		if errCache := validateClaudeDesktopCacheControl(actual.Get("cache_control"), blockPlan.CacheControl); errCache != nil {
			return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop system.%d cache_control: %v", index, errCache)}}
		}
		if blockPlan.Kind == "billing" {
			if errBilling := validateClaudeDesktopBilling(actual.Get("text").String(), e.desktopProfile.CodeVersion, plan, cchSigning); errBilling != nil {
				return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: errBilling.Error()}}
			}
			continue
		}
		artifact, errArtifact := e.desktopProfile.Artifact(blockPlan.Artifact)
		if errArtifact != nil {
			return errArtifact
		}
		expected, errRender := artifact.Render(values)
		if errRender != nil {
			return errRender
		}
		if actual.Get("text").String() != expected {
			return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude desktop system.%d does not match artifact %q", index, blockPlan.Artifact)}}
		}
	}
	return nil
}

func validateClaudeDesktopCacheControl(actual gjson.Result, expected *claudeprofile.CacheControl) error {
	if expected == nil {
		if actual.Exists() {
			return fmt.Errorf("must be omitted")
		}
		return nil
	}
	if !actual.IsObject() || actual.Get("type").String() != expected.Type || actual.Get("ttl").String() != expected.TTL || actual.Get("scope").String() != expected.Scope {
		return fmt.Errorf("does not match the role profile")
	}
	count := 0
	actual.ForEach(func(_, _ gjson.Result) bool { count++; return true })
	want := 1
	if expected.TTL != "" {
		want++
	}
	if expected.Scope != "" {
		want++
	}
	if count != want {
		return fmt.Errorf("contains unprofiled fields")
	}
	return nil
}

var claudeDesktopVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9a-f]{3}$`)
var claudeDesktopCCHPattern = regexp.MustCompile(`^[0-9a-f]{5}$`)

func validateClaudeDesktopBilling(text, version string, plan claudeDesktopRequestPlan, cchSigning bool) error {
	const prefix = "x-anthropic-billing-header: "
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, ";") {
		return fmt.Errorf("claude desktop billing block has an invalid envelope")
	}
	fields := make(map[string]string)
	for _, part := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(text, prefix), ";"), ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || key == "" || value == "" {
			return fmt.Errorf("claude desktop billing block has a malformed field")
		}
		if _, duplicate := fields[key]; duplicate {
			return fmt.Errorf("claude desktop billing block repeats %q", key)
		}
		fields[key] = value
	}
	if !claudeDesktopVersionPattern.MatchString(fields["cc_version"]) || !strings.HasPrefix(fields["cc_version"], version+".") {
		return fmt.Errorf("claude desktop billing block has an invalid cc_version")
	}
	if fields["cc_entrypoint"] != "claude-desktop" {
		return fmt.Errorf("claude desktop billing block has an invalid cc_entrypoint")
	}
	if cchSigning && !claudeDesktopCCHPattern.MatchString(fields["cch"]) {
		return fmt.Errorf("claude desktop billing block has an invalid cch")
	}
	allowed := map[string]bool{"cc_version": true, "cc_entrypoint": true}
	if cchSigning {
		allowed["cch"] = true
	}
	if previous := fields["cc_prev_req"]; previous != "" {
		if (plan.Variant.Key.Role != claudeprofile.RoleMain && plan.Variant.Key.Role != claudeprofile.RoleCompaction) || !strings.HasPrefix(previous, "req_") {
			return fmt.Errorf("claude desktop billing block has an invalid cc_prev_req")
		}
		allowed["cc_prev_req"] = true
	}
	wantPromptID := (plan.Variant.Key.Role == claudeprofile.RoleMain && plan.Variant.Key.Diagnostics) || plan.Variant.Key.Role == claudeprofile.RoleSubagent
	if wantPromptID {
		if _, errUUID := uuid.Parse(fields["cc_prompt_id"]); errUUID != nil {
			return fmt.Errorf("claude desktop billing block has an invalid cc_prompt_id")
		}
		allowed["cc_prompt_id"] = true
	} else if fields["cc_prompt_id"] != "" {
		return fmt.Errorf("claude desktop billing block unexpectedly contains cc_prompt_id")
	}
	if plan.Variant.Key.Role == claudeprofile.RoleSubagent {
		if fields["cc_is_subagent"] != "true" {
			return fmt.Errorf("claude desktop subagent billing block is missing cc_is_subagent")
		}
		allowed["cc_is_subagent"] = true
	} else if fields["cc_is_subagent"] != "" {
		return fmt.Errorf("claude desktop billing block unexpectedly contains cc_is_subagent")
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("claude desktop billing block contains unprofiled field %q", key)
		}
	}
	return nil
}

func claudeDesktopSystemBlockCount(system gjson.Result) int {
	if !system.Exists() {
		return 0
	}
	if system.IsArray() {
		return len(system.Array())
	}
	return 1
}
