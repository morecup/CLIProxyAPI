package helps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/tidwall/gjson"
)

func TestAgentTaskProtectedRestartRetainsExecutableHistory(t *testing.T) {
	root, scope := t.TempDir(), strings.Repeat("a", 64)
	invocations := make(chan claudetasks.Invocation, 4)
	options := claudetasks.Options{Scope: scope, DeliveryScope: "query-one", Store: NewClaudeDesktopAgentTaskStore(root, "owned-egress"),
		ResolveModel: func(parent, _, _ string) (string, error) { return parent, nil },
		Execute: func(_ context.Context, invocation claudetasks.Invocation) ([]byte, error) {
			invocations <- invocation
			return []byte(`{"role":"assistant","content":[{"type":"text","text":"retained private result"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`), nil
		}}
	r, err := claudetasks.New(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	owner := claudetasks.Caller{PromptID: uuid.NewString(), Model: "claude-sonnet-5"}
	result, err := r.ExecuteTool(t.Context(), owner, claudetasks.ToolCall{ID: "original", Name: "Agent", Input: json.RawMessage(`{"description":"private task","prompt":"retained private prompt","name":"worker","run_in_background":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	first := remoteInputAwait(t, invocations)
	id := gjson.GetBytes(result, "agentId").String()
	r.Close()
	files, err := filepath.Glob(filepath.Join(root, "sdk-agent-tasks", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("task namespace", files, err)
	}
	encoded, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "retained private") || !gjson.GetBytes(encoded, "ciphertext").Exists() {
		t.Fatal("task store is plaintext")
	}
	for _, foreign := range []*ClaudeDesktopSDKSessionStore{NewClaudeDesktopAgentTaskStore(root, "foreign-egress"), NewClaudeDesktopSDKSessionStore(root, "owned-egress"), NewClaudeDesktopNativeContentStore(root, "owned-egress")} {
		raw, revision, err := foreign.Load(scope)
		if err != nil || len(raw) != 0 || revision != "" {
			t.Fatal("task transcript crossed persistence boundary", err)
		}
	}
	options.Store = NewClaudeDesktopAgentTaskStore(root, "owned-egress")
	options.DeliveryScope = "query-two"
	restored, err := claudetasks.New(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	owner.PromptID = uuid.NewString()
	_, err = restored.ExecuteTool(t.Context(), owner, claudetasks.ToolCall{ID: "resume", Name: "SendMessage", Input: json.RawMessage(`{"to":"worker","message":"continue after process restart"}`)})
	if err != nil {
		t.Fatal(err)
	}
	next := remoteInputAwait(t, invocations)
	raw, _ := json.Marshal(next.Messages)
	if next.AgentID != id || next.PromptID == first.PromptID || next.Kind != "resume" || !strings.Contains(string(raw), "retained private result") || !strings.Contains(string(raw), "continue after process restart") {
		t.Fatal("restart reconstructed no executable history")
	}
}
