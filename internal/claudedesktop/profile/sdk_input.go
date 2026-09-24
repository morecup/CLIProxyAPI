package profile

import (
	"fmt"
	"regexp"
	"strings"
)

var sdkInputBetaToken = regexp.MustCompile(`^[a-z][a-z0-9-]{1,100}$`)

func validateSDKInputProfile(p SDKTelemetryProfile) error {
	event, mapped := p.Events["input_prompt"]
	if !mapped && len(p.InputBetas) == 0 {
		return nil
	}
	if !mapped || event.EventName != "tengu_input_prompt" || len(p.InputBetas) == 0 {
		return fmt.Errorf("claude desktop profile: SDK input event and model betas must be configured together")
	}
	return validateSDKInputBetas(p.InputBetas)
}

func validateSDKInputBetas(input map[string][]string) error {
	for model, betas := range input {
		if model == "" || strings.TrimSpace(model) != model || len(betas) == 0 || len(betas) > 32 {
			return fmt.Errorf("claude desktop profile: invalid SDK input model beta policy")
		}
		seen := make(map[string]bool)
		for _, beta := range betas {
			if !sdkInputBetaToken.MatchString(beta) || seen[beta] {
				return fmt.Errorf("claude desktop profile: invalid or repeated SDK input beta")
			}
			seen[beta] = true
		}
	}
	return nil
}

// InputBetaHeader uses an exact evidence-backed model key, never a family
// substring, another model's policy, or the final Messages request's betas.
func (p SDKTelemetryProfile) InputBetaHeader(model string) (string, bool) {
	betas, ok := p.InputBetas[model]
	return strings.Join(betas, ","), ok && len(betas) > 0
}
