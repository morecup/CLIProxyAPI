package profile

import (
	"strings"
	"testing"
)

func TestBuiltinCurrentUsesV270320Opus55RequestProfile(t *testing.T) {
	bundle, errBundle := BuiltinCurrent()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	if len(bundle.RequestProfiles) == 0 {
		t.Fatal("current bundle has no request profiles")
	}
	current := bundle.RequestProfiles[len(bundle.RequestProfiles)-1]
	if current.ProfileID != "claude-desktop/windows-x64/2.7032.0-code-2.1.280" ||
		current.DesktopVersion != "2.7032.0" || current.CodeVersion != "2.1.280" ||
		current.AgentSDKVersion != "0.3.280" {
		t.Fatalf("current request profile identity = %+v", current)
	}
	if current.CountTokensCatalog != CountTokensLayoutV270320 {
		t.Fatalf("count_tokens catalog = %q, want %q", current.CountTokensCatalog, CountTokensLayoutV270320)
	}
	wantLifecycleBetas := "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07"
	if got := strings.Join(current.SDKInputBetas["claude-opus-5-5"], ","); got != wantLifecycleBetas {
		t.Fatalf("Opus 5.5 SDK lifecycle betas = %q, want %q", got, wantLifecycleBetas)
	}
	computerVariantCount := 0
	for index, variant := range current.Variants {
		if variant.Headers.ClientVersion != "2.7032.0" {
			t.Fatalf("variant %d client version = %q", index, variant.Headers.ClientVersion)
		}
		_, hasFast := variant.AnthropicBetaVariants["fast"]
		_, hasFastComputer := variant.AnthropicBetaVariants["fast-computer"]
		if hasFast || hasFastComputer {
			t.Fatalf("variant %d exposes an unobserved fast beta variant: %+v", index, variant.AnthropicBetaVariants)
		}
		switch variant.Key.Role {
		case RoleMain, RoleCountTokens:
			computer, okComputer := variant.AnthropicBetaVariants["computer"]
			if !okComputer {
				t.Fatalf("variant %d role %q has no observed computer beta variant", index, variant.Key.Role)
			}
			if strings.Join(computer, "\x00") != strings.Join(variant.AnthropicBeta, "\x00") {
				t.Fatalf("variant %d computer beta = %q, want base %q", index, computer, variant.AnthropicBeta)
			}
			if strings.Contains(strings.Join(computer, ","), "computer-use-2026-08-03") {
				t.Fatalf("variant %d computer beta contains the rejected legacy beta", index)
			}
			computerVariantCount++
		case RoleSubagent:
			if _, okComputer := variant.AnthropicBetaVariants["computer"]; okComputer {
				t.Fatalf("subagent variant %d unexpectedly exposes computer", index)
			}
		}
	}
	if computerVariantCount != 3 {
		t.Fatalf("observed computer beta variants = %d, want 3", computerVariantCount)
	}

	session := current.Artifacts["session-opus-5-5"]
	if session.SHA256 != "e039ac64a85388d8ad6de9da279fb24dbce698da9288fb5af082f6afd0d0005e" ||
		session.Bytes != 9583 {
		t.Fatalf("session artifact identity = %+v", session)
	}
	if !strings.Contains(session.Text, "`{{MEMORY_DIR}}\\`") ||
		!strings.Contains(session.Text, "sync_with_base_branch") {
		t.Fatal("session artifact is missing the v270320 memory or worktree behavior")
	}
	subagentPrompt := current.Artifacts["subagent-prompt"]
	if subagentPrompt.SHA256 != "40e32d086053e6330806075b7b7915848e51712001179416af6655522379a602" ||
		subagentPrompt.Bytes != 2734 {
		t.Fatalf("subagent artifact identity = %+v", subagentPrompt)
	}

	subagent, errSubagent := bundle.Resolve(RequestVariantKey{
		Model:           "claude-opus-5-5",
		LogicalModel:    "claude-opus-5-5",
		Role:            RoleSubagent,
		Diagnostics:     true,
		ThinkingDisplay: "updates",
	})
	if errSubagent != nil {
		t.Fatal(errSubagent)
	}
	if subagent.Headers.ClientVersion != "2.7032.0" ||
		subagent.Headers.ClientPlatform != "desktop_app" ||
		subagent.Headers.RequestClass != "subagent" ||
		subagent.TopLevelSystemPolicy != SystemPolicyPreserveVerified ||
		subagent.InstructionCarrier != CarrierNone ||
		subagent.SystemBlockCount != 3 || len(subagent.System) != 3 {
		t.Fatalf("subagent request variant = %+v", subagent)
	}
	if got := strings.Join(subagent.AnthropicBeta, ","); !strings.Contains(got, "thinking-display-updates-2026-08-18") ||
		!strings.Contains(got, "extended-cache-ttl-2025-04-11") ||
		strings.Contains(got, "fallback-credit-2026-06-01") {
		t.Fatalf("subagent beta profile = %q", got)
	}
	body, errBody := bundle.BodyForVariant(subagent)
	if errBody != nil {
		t.Fatal(errBody)
	}
	if body.MaxTokens != 128000 || !body.EnsureTools || !body.RemoveUnlistedKeys ||
		!strings.Contains(string(body.Thinking), `"display":"updates"`) ||
		!strings.Contains(string(body.OutputConfig), `"effort":"medium"`) ||
		!strings.Contains(string(body.Diagnostics), `"previous_message_id":null`) {
		t.Fatalf("subagent body profile = %+v", body)
	}

	for _, diagnostics := range []bool{false, true} {
		compaction, errCompaction := bundle.Resolve(RequestVariantKey{
			Model:           "claude-opus-5-5",
			LogicalModel:    "claude-opus-5-5",
			Role:            RoleCompaction,
			Diagnostics:     diagnostics,
			ThinkingDisplay: "updates",
		})
		if errCompaction != nil {
			t.Fatal(errCompaction)
		}
		if compaction.Headers.ClientVersion != "2.7032.0" ||
			compaction.Headers.ClientPlatform != "desktop_app" ||
			compaction.Headers.DispatchID != "v2d" ||
			compaction.Headers.RequestClass != "compaction" ||
			compaction.SystemBlockCount != 4 || len(compaction.System) != 4 {
			t.Fatalf("compaction request variant = %+v", compaction)
		}
		if compaction.System[2].CacheControl == nil || compaction.System[2].CacheControl.Scope != "global" || compaction.System[2].CacheControl.TTL != "" ||
			compaction.System[3].CacheControl == nil || compaction.System[3].CacheControl.TTL != "" {
			t.Fatalf("compaction cache controls = %+v", compaction.System)
		}
		betas := strings.Join(compaction.AnthropicBeta, ",")
		for _, required := range []string{"server-side-fallback-2026-07-01", "fallback-credit-2026-06-01", "thinking-display-updates-2026-08-18"} {
			if !strings.Contains(betas, required) {
				t.Fatalf("compaction beta profile %q is missing %q", betas, required)
			}
		}
		for _, forbidden := range []string{"afk-mode-2026-01-31", "extended-cache-ttl-2025-04-11"} {
			if strings.Contains(betas, forbidden) {
				t.Fatalf("compaction beta profile %q contains %q", betas, forbidden)
			}
		}
		compactionBody, errCompactionBody := bundle.BodyForVariant(compaction)
		if errCompactionBody != nil {
			t.Fatal(errCompactionBody)
		}
		wantOrder := "model,messages,system,tools,metadata,max_tokens,thinking,context_management,fallbacks,output_config,diagnostics,stream"
		if compactionBody.MaxTokens != 128000 || !compactionBody.EnsureTools || !compactionBody.RemoveUnlistedKeys ||
			string(compactionBody.Fallbacks) != `"default"` ||
			!strings.Contains(string(compactionBody.Thinking), `"display":"updates"`) ||
			!strings.Contains(string(compactionBody.OutputConfig), `"effort":"medium"`) ||
			strings.Join(compactionBody.TopLevelOrder, ",") != wantOrder {
			t.Fatalf("compaction body profile = %+v", compactionBody)
		}
	}

	tools, errTools := bundle.CountTokensCalibrationTools("claude-opus-5-5")
	if errTools != nil {
		t.Fatal(errTools)
	}
	if tools.Layout != CountTokensLayoutV270320 || tools.ExpectedRequests != 39 || len(tools.ToolRequests) != 27 {
		t.Fatalf("current calibration layout = %q requests=%d tool_sets=%d", tools.Layout, tools.ExpectedRequests, len(tools.ToolRequests))
	}
	wantCounts := []int{1, 117, 17}
	for index, want := range wantCounts {
		if got := len(tools.ToolRequests[index]); got != want {
			t.Fatalf("tool request %d count = %d, want %d", index, got, want)
		}
	}
	if rawToolName(tools.ToolRequests[0][0]) != "Skill" ||
		!strings.HasPrefix(rawToolName(tools.ToolRequests[1][0]), "mcp__") ||
		rawToolName(tools.ToolRequests[1][116]) != "mcp__visualize__show_widget" ||
		rawToolName(tools.ToolRequests[2][0]) != "Agent" {
		t.Fatal("current calibration tool request ordering changed")
	}
}
