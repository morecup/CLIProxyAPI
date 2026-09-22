package executor

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/tidwall/gjson"
)

// assertNativeToolCatalog checks that a captured owned Messages body advertises
// the native request tools for the given definition context: the deferred
// shape the SDK request builder derives from the same rows (non-deferrable
// tools, the deferrable tools discovered through tool_reference blocks, the
// placeholder and ToolSearch). The request envelope is produced by
// encoding/json, so the tools are compared after the same compaction and HTML
// escaping; key order and every value stay native.
func assertNativeToolCatalog(t *testing.T, body []byte, ctx claudetasks.DefinitionContext) {
	t.Helper()
	tools := gjson.GetBytes(body, "tools").Array()
	var rows []json.RawMessage
	for _, row := range gjson.GetBytes(body, "messages").Array() {
		rows = append(rows, json.RawMessage(row.Raw))
	}
	want := claudetasks.RequestTools(ctx, rows)
	if len(tools) != len(want) {
		t.Fatalf("request advertises %d tools, native request tools %d", len(tools), len(want))
	}
	for i, tool := range tools {
		encoded, err := json.Marshal(want[i])
		if err != nil {
			t.Fatal(err)
		}
		if tool.Raw != string(encoded) {
			t.Fatalf("tool %d (%s) differs from the native catalog for %s", i, tool.Get("name").String(), ctx.Model)
		}
	}
}

// Owned runtime requests declare the SDK's first-party family byte-exact and
// the SDK sends those names unaliased; the same names declared by a downstream
// client stay third-party and keep receiving MCP aliases.
func TestClaudeDesktopFirstPartyToolNamesStayNativeOnOwnedRequests(t *testing.T) {
	catalog, err := json.Marshal(claudetasks.Catalog(claudetasks.DefinitionContext{Model: "claude-sonnet-5", SubscriptionType: "pro"}))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{}},{"type":"tool_use","id":"toolu_2","name":"Inspect","input":{}}]}],"tools":` + string(catalog[:len(catalog)-1]) + `,{"name":"Inspect","input_schema":{"type":"object"}}]}`)
	owned := claudetasks.WithCaller(t.Context(), claudetasks.Caller{PromptID: "prompt_owned", Model: "claude-sonnet-5"})
	remapped, reverse := prepareClaudeDesktopToolNamesForUpstream(body, resolveClaudeMCPAliasOptions(owned))
	tools := gjson.GetBytes(remapped, "tools").Array()
	for i, want := range claudetasks.Catalog(claudetasks.DefinitionContext{Model: "claude-sonnet-5", SubscriptionType: "pro"}) {
		encoded, _ := json.Marshal(want)
		if tools[i].Raw != string(encoded) {
			t.Fatalf("owned request altered first-party tool %d: %s", i, tools[i].Get("name").String())
		}
	}
	if name := tools[4].Get("name").String(); !strings.HasPrefix(name, "mcp__") || !strings.HasSuffix(name, "_Inspect") {
		t.Fatalf("client-shaped tool in an owned request must still be aliased: %s", name)
	}
	if gjson.GetBytes(remapped, "messages.0.content.0.name").String() != "Agent" || !strings.HasPrefix(gjson.GetBytes(remapped, "messages.0.content.1.name").String(), "mcp__") {
		t.Fatalf("owned history renamed unexpectedly: %s", gjson.GetBytes(remapped, "messages.0.content").Raw)
	}
	for alias, original := range reverse {
		if claudetasks.FirstPartyToolNames()[original] {
			t.Fatalf("first-party name %s must not enter the alias table as %s", original, alias)
		}
	}
	client, _ := prepareClaudeDesktopToolNamesForUpstream(body, resolveClaudeMCPAliasOptions(t.Context()))
	for _, tool := range gjson.GetBytes(client, "tools").Array() {
		if !strings.HasPrefix(tool.Get("name").String(), "mcp__") {
			t.Fatalf("downstream client tool %s must be aliased", tool.Get("name").String())
		}
	}
}

func TestClaudeDesktopAgentDefinitionContextFollowsAccountAndFeatures(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	host := inner.desktopATIS.featureHosts.Warm()

	// Defaults: the Desktop worker has no teams, fork or cross-session gates,
	// the steer is the SDK default and the account subscription is reported.
	ctx := inner.desktopAgentDefinitionContext(auth, host, "claude-sonnet-5")
	if !reflect.DeepEqual(ctx, claudetasks.DefinitionContext{Model: "claude-sonnet-5", SubscriptionType: "pro"}) {
		t.Fatalf("default definition context %+v", ctx)
	}
	catalog := claudetasks.Catalog(ctx)
	if len(catalog) != 4 || !strings.Contains(string(catalog[0]), "Do not spawn agents unless the user asks") {
		t.Fatalf("pro lane catalog: %d tools", len(catalog))
	}
	auth.Metadata["subscription_type"] = "max"
	if got := inner.desktopAgentDefinitionContext(auth, host, "claude-sonnet-5").SubscriptionType; got != "max" {
		t.Fatalf("subscription follows the account metadata: %s", got)
	}
	delete(auth.Metadata, "subscription_type")

	// Observed features gate the catalog the way the SDK reads them.
	ticket, sink, err := inner.desktopATIS.bindFeatureHost(auth, host)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := host.Service().Observe(ticket, []byte(`{"features":{"tengu_fgts":{"value":true},"tengu_thistle_grebe":{"value":"lean"},"tengu_velvet_tide":{"value":true},"tengu_harbor_kite":{"value":true},"tengu_harbor_kite_win":{"value":true}}}`), host.SessionID(), sink)
	if err != nil || !accepted {
		t.Fatalf("seed features: accepted=%v err=%v", accepted, err)
	}
	ctx = inner.desktopAgentDefinitionContext(auth, host, "claude-sonnet-5")
	if !ctx.EagerInputStreaming || ctx.SubagentSteer != "lean" || !ctx.LeanPromptForced || !ctx.CrossSessionEnabled || ctx.TeamsEnabled || ctx.ForkEnabled || ctx.BackgroundDisabled {
		t.Fatalf("feature-gated definition context %+v", ctx)
	}
	// The lean-prompt override is only read for models outside the lean lane.
	if opus := inner.desktopAgentDefinitionContext(auth, host, "claude-opus-5"); opus.LeanPromptForced || !claudetasks.LeanPromptModel(opus.Model) {
		t.Fatalf("lean-lane model must not read tengu_velvet_tide: %+v", opus)
	}
	for _, raw := range claudetasks.Catalog(ctx) {
		if !strings.HasSuffix(string(raw), `,"eager_input_streaming":true}`) {
			t.Fatalf("gated catalog lost eager_input_streaming: %s", raw[len(raw)-80:])
		}
	}
	if !strings.Contains(string(claudetasks.Catalog(ctx)[1]), "notify_when_idle") {
		t.Fatal("cross-session SendMessage schema missing")
	}
}
