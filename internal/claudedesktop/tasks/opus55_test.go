package tasks

import "testing"

func TestOpus55CurrentModelRules(t *testing.T) {
	for _, test := range []struct {
		model string
		want  string
	}{
		{model: "opus", want: "claude-opus-5-5"},
		{model: "claude-opus-5-5", want: "claude-opus-5-5"},
		{model: "anthropic.claude-opus-5-5", want: "claude-opus-5-5"},
		{model: "anthropic.claude-opus-5-5-20260922-v1:0", want: "claude-opus-5-5"},
		{model: "claude-opus-5-5@20260922", want: "claude-opus-5-5"},
		{model: "claude-opus-5", want: "claude-opus-5"},
	} {
		t.Run(test.model, func(t *testing.T) {
			if got := CanonicalModelID(test.model); got != test.want {
				t.Fatalf("CanonicalModelID(%q) = %q, want %q", test.model, got, test.want)
			}
		})
	}

	if !LeanPromptModel("opus") || !LeanPromptModel("claude-opus-5-5") {
		t.Fatal("Opus 5.5 must use the lean Agent prompt")
	}
}
