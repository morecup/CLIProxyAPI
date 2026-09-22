package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/tidwall/gjson"
)

// thinkingSignatureFixtures are signature shapes that survive a JSON round trip
// only when every stage performs targeted edits instead of re-encoding the body.
// They cover base64 padding, JSON metacharacters, escape sequences, astral-plane
// runes and an oversized value.
func thinkingSignatureFixtures() []string {
	return []string{
		"ErUBCkYIBRgCKkDq+9zN/vQ7aB1c2dEf==",
		`sig/with+slashes==and"quotes"and\backslashes`,
		"line\nbreak\ttab\u0000null\u001fcontrol",
		"unicode-\u4e2d\u6587-\U0001f600-\u200b-\ufeff",
		"a/b<c>d&e'f\u2028\u2029",
		strings.Repeat("EqQBCkYIBRgCKkD", 400) + "==",
	}
}

// collectThinkingSignatures returns every messages[].content[].signature value in
// document order.
func collectThinkingSignatures(t *testing.T, body []byte) []string {
	t.Helper()
	var found []string
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			if signature := block.Get("signature"); signature.Exists() {
				found = append(found, signature.String())
			}
			return true
		})
		return true
	})
	return found
}

// buildThinkingHistoryPayload renders a multi-turn conversation whose assistant
// turns carry thinking blocks with the supplied signatures, plus a declared tool
// so the OAuth MCP alias pass has real work to do.
func buildThinkingHistoryPayload(t *testing.T, signatures []string, firstUserText string) []byte {
	t.Helper()
	type block map[string]any
	messages := []any{
		map[string]any{"role": "user", "content": []any{block{"type": "text", "text": firstUserText}}},
	}
	for i, signature := range signatures {
		messages = append(messages, map[string]any{
			"role": "assistant",
			"content": []any{
				block{"type": "thinking", "thinking": "reasoning step", "signature": signature},
				block{"type": "tool_use", "id": "toolu_" + string(rune('a'+i)), "name": "search_web", "input": map[string]any{}},
			},
		})
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{
				block{"type": "tool_result", "tool_use_id": "toolu_" + string(rune('a'+i)), "content": "tool output"},
			},
		})
	}
	payload := map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 1024,
		"thinking":   map[string]any{"type": "adaptive"},
		"messages":   messages,
		"tools": []any{
			map[string]any{"name": "search_web", "input_schema": map[string]any{"type": "object"}},
		},
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal fixture payload: %v", errMarshal)
	}
	return encoded
}

// TestClaudeThinkingSignaturesSurviveUpstreamPreparation pins the roadmap
// requirement that thinking-block signatures replay byte-for-byte through the
// upstream request pipeline: cloaking (system blocks, currentDate, CCH signing)
// followed by the OAuth MCP tool alias pass.
func TestClaudeThinkingSignaturesSurviveUpstreamPreparation(t *testing.T) {
	signatures := thinkingSignatureFixtures()
	payload := buildThinkingHistoryPayload(t, signatures, "first question")

	if got := collectThinkingSignatures(t, payload); len(got) != len(signatures) {
		t.Fatalf("fixture built %d signatures, want %d", len(got), len(signatures))
	}

	executor := newClaudeDesktopTestExecutor(t)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(payload, claudeprofile.RoleMain, "claude-opus-5", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	facts := executor.newClaudeDesktopRuntimeFacts(nil, "session", "claude-opus-5", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "", map[string]any{"working_dir": `C:\code`})
	profiled, _, errProfile := executor.applyClaudeDesktopMessageProfile(context.Background(), nil, payload, true, plan, facts)
	if errProfile != nil {
		t.Fatal(errProfile)
	}

	prepared, reverseMap := prepareClaudeDesktopToolNamesForUpstream(profiled, claudeMCPAliasOptions{secret: "signature-fixture-caller"})
	if len(reverseMap) == 0 {
		t.Fatal("expected the MCP alias pass to rewrite the declared tool")
	}

	for stage, body := range map[string][]byte{"profiled": profiled, "prepared": prepared} {
		got := collectThinkingSignatures(t, body)
		if len(got) != len(signatures) {
			t.Fatalf("%s stage produced %d signatures, want %d", stage, len(got), len(signatures))
		}
		for i, want := range signatures {
			if got[i] != want {
				t.Fatalf("%s stage signature[%d] mutated:\n got  %q\n want %q", stage, i, got[i], want)
			}
		}
	}
}

// TestClaudeThinkingSignaturesSurviveSensitiveWordObfuscation guards the case
// where cloaking rewrites message text: obfuscation must never reach into an
// opaque thinking signature, even when the signature contains the trigger word.
