package registry

import "testing"

func TestClaudeModelsIncludeOpus55(t *testing.T) {
	opus5Index := -1
	for index, model := range GetClaudeModels() {
		if model != nil && model.ID == "claude-opus-5" {
			opus5Index = index
		}
		if model == nil || model.ID != "claude-opus-5-5" {
			continue
		}
		if opus5Index < 0 || opus5Index >= index {
			t.Fatalf("Claude model order must place Opus 5 before Opus 5.5")
		}
		if model.Created != 1790035200 || model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 {
			t.Fatalf("Opus 5.5 limits or release timestamp are wrong: %+v", model)
		}
		if model.Thinking == nil || model.Thinking.ZeroAllowed || !model.Thinking.DynamicAllowed {
			t.Fatalf("Opus 5.5 thinking capability is wrong: %+v", model.Thinking)
		}
		wantLevels := []string{"low", "medium", "high", "xhigh", "max"}
		if len(model.Thinking.Levels) != len(wantLevels) {
			t.Fatalf("Opus 5.5 levels = %v, want %v", model.Thinking.Levels, wantLevels)
		}
		for i, want := range wantLevels {
			if model.Thinking.Levels[i] != want {
				t.Fatalf("Opus 5.5 levels = %v, want %v", model.Thinking.Levels, wantLevels)
			}
		}
		return
	}
	t.Fatal("embedded Claude models do not contain claude-opus-5-5")
}
