package tasks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The literal expectations below come from the pinned SDK 2.1.247 rules
// (request assembly, fA, fQs/uGt, XHr rendering and jWe.call); the native
// golden test compares the same surfaces byte-for-byte.

const deferredToolsReminderDefault = "<system-reminder>\n" +
	"The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded \u2014 calling them directly will fail with InputValidationError. Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" +
	"SendMessage\nTaskOutput\nTaskStop\n" +
	"</system-reminder>"

func defaultToolSearchContext() DefinitionContext {
	return DefinitionContext{Model: "claude-sonnet-5", SubscriptionType: "pro"}
}

func toolNames(t *testing.T, tools []json.RawMessage) []string {
	t.Helper()
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		var decoded struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Name == "" {
			t.Fatalf("tool is not a named definition: %s (%v)", raw, err)
		}
		names = append(names, decoded.Name)
	}
	return names
}

func userRow(content string) json.RawMessage {
	return json.RawMessage(`{"role":"user","content":` + content + `}`)
}

func referenceRow(names ...string) json.RawMessage {
	items := make([]string, 0, len(names))
	for _, name := range names {
		items = append(items, `{"type":"tool_reference","tool_name":`+jsQuote(name)+`}`)
	}
	return userRow(`[{"type":"tool_result","tool_use_id":"toolu_search","content":[` + strings.Join(items, ",") + `]}]`)
}

func TestToolSearchDefinitionAndPlaceholder(t *testing.T) {
	ctx := defaultToolSearchContext()
	def := string(ToolSearchDefinition(ctx))
	head := "{\"name\":\"ToolSearch\",\"description\":\"Fetches full schema definitions for deferred tools so they can be called.\\n\\nDeferred tools appear by name in <system-reminder> messages. Until fetched, only the name is known \u2014 there is no parameter schema, so the tool cannot be invoked. This tool takes a query"
	if !strings.HasPrefix(def, head) {
		t.Fatalf("ToolSearch head differs at %s", firstDifference(def, head))
	}
	tail := "- \\\"+slack send\\\" \u2014 require \\\"slack\\\" in the name, rank by remaining terms\",\"input_schema\":" + toolSearchSchema + "}"
	if !strings.HasSuffix(def, tail) {
		t.Fatalf("ToolSearch tail differs: %s", def[len(def)-200:])
	}
	if !strings.Contains(def, `"max_results":{"description":"Maximum number of results to return (default: 5)","default":5,"type":"number"}},"required":["query","max_results"],"additionalProperties":false}`) {
		t.Fatalf("ToolSearch schema differs: %s", def)
	}
	if strings.Contains(def, "eager_input_streaming") || strings.Contains(def, "defer_loading") {
		t.Fatalf("ToolSearch must not carry eager_input_streaming or defer_loading by default: %s", def)
	}
	ctx.EagerInputStreaming = true
	if eager := string(ToolSearchDefinition(ctx)); !strings.HasSuffix(eager, `,"eager_input_streaming":true}`) {
		t.Fatalf("eager ToolSearch: %s", eager[len(eager)-80:])
	}
	ctx.ToolSearchFetchRule = true
	rule := string(ToolSearchDefinition(ctx))
	if !strings.Contains(rule, " so calling the tool fails with InputValidationError. When any instruction, system reminder, or other tool's description names a deferred tool, fetch it with query \\\"select:<name>\\\" before calling it. This tool takes a query") || strings.Contains(rule, "so the tool cannot be invoked") {
		t.Fatalf("fetch-rule ToolSearch prompt: %s", rule)
	}
	if got := string(PlaceholderDefinition()); got != `{"name":"DeferredToolPlaceholder","description":"Reserved placeholder that keeps deferred tool loading active; never call this tool.","input_schema":{"type":"object","properties":{}},"defer_loading":true}` {
		t.Fatalf("placeholder: %s", got)
	}
	if !json.Valid(ToolSearchDefinition(ctx)) || !json.Valid(PlaceholderDefinition()) {
		t.Fatal("definitions must be valid JSON")
	}
}

func TestToolSearchEnabledFollowsGates(t *testing.T) {
	cases := []struct {
		name string
		ctx  DefinitionContext
		want bool
	}{
		{"default", defaultToolSearchContext(), true},
		{"haiku_4_5", DefinitionContext{Model: "claude-haiku-4-5-20251001"}, true},
		{"haiku_3_5", DefinitionContext{Model: "claude-3-5-haiku-20241022"}, false},
		{"haiku_3_case", DefinitionContext{Model: "Claude-3-Haiku"}, false},
		{"disabled", DefinitionContext{Model: "claude-sonnet-5", ToolSearchDisabled: true}, false},
		{"stub_disabled_keeps_search", DefinitionContext{Model: "claude-sonnet-5", DeferredStubDisabled: true}, true},
		{"custom_unsupported", DefinitionContext{Model: "claude-sonnet-5", ToolSearchUnsupportedModels: []string{"sonnet"}}, false},
		{"empty_unsupported", DefinitionContext{Model: "claude-3-haiku", ToolSearchUnsupportedModels: []string{}}, true},
		{"some_non_deferrable", DefinitionContext{Model: "claude-sonnet-5", NonDeferrableBuiltins: []string{"SendMessage", "TaskOutput"}}, true},
		{"all_non_deferrable", DefinitionContext{Model: "claude-sonnet-5", NonDeferrableBuiltins: []string{"SendMessage", "TaskOutput", "TaskStop"}}, false},
	}
	for _, c := range cases {
		if got := ToolSearchEnabled(c.ctx); got != c.want {
			t.Fatalf("%s: enabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRequestToolsDefersOwnedFamilyUntilDiscovered(t *testing.T) {
	ctx := defaultToolSearchContext()
	catalog := Catalog(ctx)
	join := func(names []string) string { return strings.Join(names, ",") }

	tools := RequestTools(ctx, []json.RawMessage{userRow(`"hello"`)})
	if got := join(toolNames(t, tools)); got != "Agent,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("default request tools: %s", got)
	}
	if string(tools[0]) != string(catalog[0]) {
		t.Fatalf("Agent must keep its catalog bytes: %s", firstDifference(string(tools[0]), string(catalog[0])))
	}
	if string(tools[1]) != string(PlaceholderDefinition()) || string(tools[2]) != string(ToolSearchDefinition(ctx)) {
		t.Fatal("placeholder and ToolSearch must be the byte-exact definitions")
	}

	tools = RequestTools(ctx, []json.RawMessage{userRow(`"hello"`), referenceRow("SendMessage")})
	if got := join(toolNames(t, tools)); got != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("SendMessage discovered: %s", got)
	}
	if want := string(catalog[1][:len(catalog[1])-1]) + `,"defer_loading":true}`; string(tools[1]) != want {
		t.Fatalf("discovered SendMessage differs at %s", firstDifference(string(tools[1]), want))
	}
	for i, raw := range tools {
		if i != 1 && i != 2 && strings.Contains(string(raw), "defer_loading") {
			t.Fatalf("defer_loading leaked onto %s", raw)
		}
	}

	tools = RequestTools(ctx, []json.RawMessage{referenceRow("TaskStop"), userRow(`[{"type":"tool_result","tool_use_id":"x","content":"No matching deferred tools found"}]`), referenceRow("SendMessage", "TaskOutput")})
	if got := join(toolNames(t, tools)); got != "Agent,SendMessage,TaskOutput,TaskStop,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("all discovered: %s", got)
	}
	for i := 1; i <= 3; i++ {
		if !strings.HasSuffix(string(tools[i]), `,"defer_loading":true}`) {
			t.Fatalf("discovered tool %d lacks defer_loading: %s", i, tools[i][len(tools[i])-60:])
		}
	}

	// Discovery uses exact names: a Task reference does not discover Agent,
	// and Agent is never deferred anyway.
	discovered := DiscoveredTools([]json.RawMessage{referenceRow("Task")})
	if !discovered["Task"] || discovered["Agent"] || len(discovered) != 1 {
		t.Fatalf("discovered = %v", discovered)
	}
	if got := join(toolNames(t, RequestTools(ctx, []json.RawMessage{referenceRow("Task")}))); got != "Agent,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("Task reference: %s", got)
	}

	eager := ctx
	eager.EagerInputStreaming = true
	tools = RequestTools(eager, []json.RawMessage{referenceRow("TaskOutput")})
	if !strings.HasSuffix(string(tools[1]), `,"eager_input_streaming":true,"defer_loading":true}`) {
		t.Fatalf("defer_loading must follow eager_input_streaming: %s", tools[1][len(tools[1])-80:])
	}
	if !strings.HasSuffix(string(tools[3]), `,"eager_input_streaming":true}`) {
		t.Fatalf("eager ToolSearch: %s", tools[3][len(tools[3])-80:])
	}

	unsupported := DefinitionContext{Model: "claude-3-5-haiku-20241022", SubscriptionType: "pro"}
	tools = RequestTools(unsupported, []json.RawMessage{referenceRow("SendMessage")})
	if got := join(toolNames(t, tools)); got != "Agent,SendMessage,TaskOutput,TaskStop" {
		t.Fatalf("unsupported model: %s", got)
	}
	for i, raw := range tools {
		if string(raw) != string(Catalog(unsupported)[i]) {
			t.Fatalf("disabled lane must serialize the plain catalog: %s", firstDifference(string(raw), string(Catalog(unsupported)[i])))
		}
	}

	stub := ctx
	stub.DeferredStubDisabled = true
	if got := join(toolNames(t, RequestTools(stub, nil))); got != "Agent,ToolSearch" {
		t.Fatalf("stub disabled: %s", got)
	}

	pinned := ctx
	pinned.NonDeferrableBuiltins = []string{"SendMessage", "TaskOutput", "TaskStop"}
	if got := join(toolNames(t, RequestTools(pinned, nil))); got != "Agent,SendMessage,TaskOutput,TaskStop" {
		t.Fatalf("all non-deferrable: %s", got)
	}
	partial := ctx
	partial.NonDeferrableBuiltins = []string{"TaskStop"}
	tools = RequestTools(partial, nil)
	if got := join(toolNames(t, tools)); got != "Agent,TaskStop,DeferredToolPlaceholder,ToolSearch" || strings.Contains(string(tools[1]), "defer_loading") {
		t.Fatalf("TaskStop non-deferrable: %s %s", got, tools[1][len(tools[1])-40:])
	}

	// Malformed rows never panic and never discover anything.
	odd := []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`"text"`), json.RawMessage(`{"role":"user","content":[{"type":"tool_result","content":[1,"x",{"type":"tool_reference","tool_name":7},{"type":"tool_reference","tool_name":"TaskStop"}]}]}`), json.RawMessage(`{"role":"assistant","content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"SendMessage"}]}]}`)}
	if discovered := DiscoveredTools(odd); len(discovered) != 1 || !discovered["TaskStop"] {
		t.Fatalf("tolerant discovery = %v", discovered)
	}
}

func TestDeferredToolsReminderAnnouncesOnce(t *testing.T) {
	ctx := defaultToolSearchContext()
	prompt := userRow(`[{"type":"text","text":"inspect actual state"}]`)
	if got := DeferredToolsReminder(ctx, []json.RawMessage{prompt}); got != deferredToolsReminderDefault {
		t.Fatalf("reminder differs at %s", firstDifference(got, deferredToolsReminderDefault))
	}
	reminder, _ := json.Marshal(deferredToolsReminderDefault)
	asBlock := userRow(`[{"type":"text","text":"inspect actual state"},{"type":"text","text":` + string(reminder) + `}]`)
	assistant := json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_search","name":"ToolSearch","input":{"query":"select:SendMessage"}}]}`)
	if got := DeferredToolsReminder(ctx, []json.RawMessage{asBlock, assistant, referenceRow("SendMessage")}); got != "" {
		t.Fatalf("announced names must not repeat: %q", got)
	}
	if got := DeferredToolsReminder(ctx, []json.RawMessage{userRow(string(reminder))}); got != "" {
		t.Fatalf("string content announcement must count: %q", got)
	}
	merged, _ := json.Marshal("inspect actual state\n\n" + deferredToolsReminderDefault)
	if got := DeferredToolsReminder(ctx, []json.RawMessage{userRow(`[{"type":"tool_result","tool_use_id":"x","content":` + string(merged) + `}]`)}); got != "" {
		t.Fatalf("folded tool_result announcement must count: %q", got)
	}
	// A partial announcement lists only the missing names.
	partial, _ := json.Marshal("<system-reminder>\n" + deferredToolsHeader + "\nTaskStop\n</system-reminder>")
	want := "<system-reminder>\n" + deferredToolsHeader + "\nSendMessage\nTaskOutput\n</system-reminder>"
	if got := DeferredToolsReminder(ctx, []json.RawMessage{userRow(string(partial))}); got != want {
		t.Fatalf("partial announcement differs at %s", firstDifference(got, want))
	}
	// Assistant text never counts as an announcement.
	if got := DeferredToolsReminder(ctx, []json.RawMessage{prompt, json.RawMessage(`{"role":"assistant","content":` + string(reminder) + `}`)}); got != deferredToolsReminderDefault {
		t.Fatalf("assistant echo must not count: %q", got)
	}
	// Disabled lanes never announce; non-deferrable tools are not announced.
	for _, off := range []DefinitionContext{{Model: "claude-3-5-haiku-20241022"}, {Model: "claude-sonnet-5", ToolSearchDisabled: true}, {Model: "claude-sonnet-5", NonDeferrableBuiltins: []string{"SendMessage", "TaskOutput", "TaskStop"}}} {
		if got := DeferredToolsReminder(off, []json.RawMessage{prompt}); got != "" {
			t.Fatalf("%+v must not announce: %q", off, got)
		}
	}
	pinned := ctx
	pinned.NonDeferrableBuiltins = []string{"TaskOutput"}
	if got := DeferredToolsReminder(pinned, []json.RawMessage{prompt}); got != "<system-reminder>\n"+deferredToolsHeader+"\nSendMessage\nTaskStop\n</system-reminder>" {
		t.Fatalf("pinned TaskOutput: %q", got)
	}
	if got := DeferredToolsReminder(ctx, nil); got != deferredToolsReminderDefault {
		t.Fatalf("empty history: %q", got)
	}
	// A previously announced name outside the pool is reported removed once
	// (with the ambient note); the removal paragraph then counts as history.
	stale, _ := json.Marshal("<system-reminder>\n" + deferredToolsHeader + "\nGhostTool\nSendMessage\nTaskOutput\nTaskStop\n</system-reminder>")
	removed := "<system-reminder>\n" + deferredToolsRemovedHeader + "\nGhostTool\n\n" + deferredToolsAmbientNote + "\n</system-reminder>"
	if got := DeferredToolsReminder(ctx, []json.RawMessage{userRow(string(stale))}); got != removed {
		t.Fatalf("stale announcement differs at %s", firstDifference(got, removed))
	}
	removedRow, _ := json.Marshal(removed)
	if got := DeferredToolsReminder(ctx, []json.RawMessage{userRow(string(stale)), userRow(string(removedRow))}); got != "" {
		t.Fatalf("removal must not repeat: %q", got)
	}
	// Excluded names (Jon) never count as announced.
	excluded, _ := json.Marshal("<system-reminder>\n" + deferredToolsHeader + "\nFrame\nTeamCreate\n</system-reminder>")
	if got := DeferredToolsReminder(ctx, []json.RawMessage{userRow(string(excluded))}); got != deferredToolsReminderDefault {
		t.Fatalf("excluded names: %q", got)
	}
}

func TestToolSearchSelectAndKeywordForms(t *testing.T) {
	ctx := defaultToolSearchContext()
	cases := []struct {
		query string
		want  string
	}{
		{"select:SendMessage", "SendMessage"},
		{"select:SendMessage,TaskStop", "SendMessage,TaskStop"},
		{"select: TaskOutput , Missing", "TaskOutput"},
		{"select:Task", "Agent"},
		{"select:KillBash", "TaskStop"},
		{"select:BashOutput,AgentOutputTool,TaskOutput", "TaskOutput"},
		{"select:ToolSearch", "ToolSearch"},
		{"SELECT:sendmessage", ""},
		{"select:Missing", ""},
		{"select:", ""},
		{"send message", "SendMessage"},
		// TaskOutput and TaskStop tie on the name parts (pool order); the
		// SendMessage prompt mentions "assign task 1" and scores 2.
		{"task", "TaskOutput,TaskStop,SendMessage"},
		{"stop", "TaskStop"},
		{"output", "TaskOutput,SendMessage"},
		{"Agent", "Agent"},
		{" toolsearch ", "ToolSearch"},
		{"+task output", "TaskOutput,TaskStop,SendMessage"},
		{"+kill", "TaskStop"},
		{"+message stop", "SendMessage"},
		{"nothing here", ""},
		{"", ""},
		{"taskoutput", "TaskOutput"},
	}
	for _, c := range cases {
		input, _ := json.Marshal(map[string]string{"query": c.query})
		data, err := toolSearch(ctx, input)
		if err != nil {
			t.Fatalf("%q: %v", c.query, err)
		}
		var result struct {
			Matches       []string `json:"matches"`
			Query         string   `json:"query"`
			TotalDeferred int      `json:"total_deferred_tools"`
		}
		if err := json.Unmarshal(data, &result); err != nil || result.Query != c.query || result.TotalDeferred != 3 {
			t.Fatalf("%q: data %s (%v)", c.query, data, err)
		}
		if got := strings.Join(result.Matches, ","); got != c.want {
			t.Fatalf("%q: matches %q, want %q", c.query, got, c.want)
		}
		if !strings.HasPrefix(string(data), `{"matches":[`) || !strings.HasSuffix(string(data), `,"total_deferred_tools":3}`) {
			t.Fatalf("%q: key order %s", c.query, data)
		}
	}
	if data, err := toolSearch(ctx, json.RawMessage(`{"query":"task","max_results":1}`)); err != nil || string(data) != `{"matches":["TaskOutput"],"query":"task","total_deferred_tools":3}` {
		t.Fatalf("max_results 1: %s %v", data, err)
	}
	if data, err := toolSearch(ctx, json.RawMessage(`{"query":"task","max_results":0}`)); err != nil || string(data) != `{"matches":[],"query":"task","total_deferred_tools":3}` {
		t.Fatalf("max_results 0: %s %v", data, err)
	}
	if data, err := toolSearch(ctx, json.RawMessage(`{"query":"task","max_results":2.9}`)); err != nil || string(data) != `{"matches":["TaskOutput","TaskStop"],"query":"task","total_deferred_tools":3}` {
		t.Fatalf("fractional max_results truncates like slice: %s %v", data, err)
	}
	for _, bad := range []string{`{}`, `{"query":5}`, `{"query":null}`, `{"query":"x","max_results":"5"}`, `{"query":"x","max_results":null}`, `[]`, ``} {
		if _, err := toolSearch(ctx, json.RawMessage(bad)); err == nil || !strings.Contains(err.Error(), "ToolSearch requires a query string") {
			t.Fatalf("%s must fail validation: %v", bad, err)
		}
	}
	// The pinned tools shrink the deferred set and the total.
	pinned := ctx
	pinned.NonDeferrableBuiltins = []string{"TaskStop"}
	if data, _ := toolSearch(pinned, json.RawMessage(`{"query":"select:TaskStop,SendMessage"}`)); string(data) != `{"matches":["TaskStop","SendMessage"],"query":"select:TaskStop,SendMessage","total_deferred_tools":2}` {
		t.Fatalf("pinned select: %s", data)
	}
	if data, _ := toolSearch(pinned, json.RawMessage(`{"query":"stop"}`)); string(data) != `{"matches":[],"query":"stop","total_deferred_tools":2}` {
		t.Fatalf("pinned keyword excludes non-deferred tools: %s", data)
	}
	// Cross-session SendMessage text mentions "notify_when_idle" only in that lane.
	cross := ctx
	cross.CrossSessionEnabled = true
	if data, _ := toolSearch(cross, json.RawMessage(`{"query":"notify_when_idle"}`)); string(data) != `{"matches":["SendMessage"],"query":"notify_when_idle","total_deferred_tools":3}` {
		t.Fatalf("cross-session description: %s", data)
	}
	if data, _ := toolSearch(ctx, json.RawMessage(`{"query":"notify_when_idle"}`)); string(data) != `{"matches":[],"query":"notify_when_idle","total_deferred_tools":3}` {
		t.Fatalf("default description: %s", data)
	}
}

func TestExecuteToolRunsToolSearchAndMapsResults(t *testing.T) {
	runtime := newRuntime(t, Options{Execute: func(context.Context, Invocation) ([]byte, error) { return reply("done"), nil }})
	owner := caller()
	data, err := runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_search", Name: "ToolSearch", Input: json.RawMessage(`{"query":"select:SendMessage,Task"}`)})
	if err != nil || string(data) != `{"matches":["SendMessage","Agent"],"query":"select:SendMessage,Task","total_deferred_tools":3}` {
		t.Fatalf("ExecuteTool ToolSearch: %s %v", data, err)
	}
	if got := string(runtime.ToolResult(ToolCall{ID: "toolu_search", Name: "ToolSearch"}, data, nil)); got != `{"content":[{"type":"tool_reference","tool_name":"SendMessage"},{"type":"tool_reference","tool_name":"Agent"}],"tool_use_id":"toolu_search","type":"tool_result"}` {
		t.Fatalf("tool_reference mapping: %s", got)
	}
	data, err = runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_search", Name: "ToolSearch", Input: json.RawMessage(`{"query":"nothing here"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(runtime.ToolResult(ToolCall{ID: "toolu_search", Name: "ToolSearch"}, data, nil)); got != `{"content":"No matching deferred tools found","tool_use_id":"toolu_search","type":"tool_result"}` {
		t.Fatalf("no-match mapping: %s", got)
	}
	_, err = runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_search", Name: "ToolSearch", Input: json.RawMessage(`{"max_results":5}`)})
	if err == nil {
		t.Fatal("missing query must fail")
	}
	if got := string(runtime.ToolResult(ToolCall{ID: "toolu_search", Name: "ToolSearch"}, nil, err)); !strings.Contains(got, `"is_error":true`) || !strings.Contains(got, "ToolSearch requires a query string") {
		t.Fatalf("error mapping: %s", got)
	}
	// The runtime's gates drive the searchable descriptions and the tools.
	gated := &Runtime{options: Options{Definitions: func(model string) DefinitionContext {
		return DefinitionContext{Model: model, CrossSessionEnabled: true, EagerInputStreaming: true, SubscriptionType: "pro"}
	}}}
	data, err = gated.ExecuteTool(t.Context(), Caller{PromptID: owner.PromptID, Model: "claude-sonnet-5"}, ToolCall{ID: "toolu_search", Name: "ToolSearch", Input: json.RawMessage(`{"query":"notify_when_idle"}`)})
	if err != nil || string(data) != `{"matches":["SendMessage"],"query":"notify_when_idle","total_deferred_tools":3}` {
		t.Fatalf("gated ToolSearch: %s %v", data, err)
	}
	tools := gated.RequestTools("claude-sonnet-5", []json.RawMessage{referenceRow("SendMessage")})
	if got := strings.Join(toolNames(t, tools), ","); got != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" || !strings.HasSuffix(string(tools[1]), `,"eager_input_streaming":true,"defer_loading":true}`) {
		t.Fatalf("runtime request tools: %s %s", got, tools[1][len(tools[1])-80:])
	}
	if got := gated.DeferredToolsReminder("claude-3-5-haiku-20241022", nil); got != "" {
		t.Fatalf("runtime reminder for an unsupported model: %q", got)
	}
	if got := gated.DeferredToolsReminder("claude-sonnet-5", nil); got != deferredToolsReminderDefault {
		t.Fatalf("runtime reminder: %q", got)
	}
	var nilRuntime *Runtime
	if got := strings.Join(toolNames(t, nilRuntime.RequestTools("claude-sonnet-5", nil)), ","); got != "Agent,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("nil runtime falls back to the default lane: %s", got)
	}
}

func TestFilterToolReferencesDropsUnknownNames(t *testing.T) {
	ctx := defaultToolSearchContext()
	row := userRow(`[{"type":"text","text":"hi"},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"Missing"},{"type":"tool_reference","tool_name":"KillBash"},{"type":"text","text":"note"},{"type":"tool_reference","tool_name":"ToolSearch"}],"is_error":false}]`)
	rows := FilterToolReferences(ctx, []json.RawMessage{userRow(`"plain"`), row, referenceRow("SendMessage")})
	if len(rows) != 3 || string(rows[0]) != `{"role":"user","content":"plain"}` || string(rows[2]) != string(referenceRow("SendMessage")) {
		t.Fatalf("untouched rows must keep their bytes: %s", rows)
	}
	if want := `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"KillBash"},{"type":"text","text":"note"},{"type":"tool_reference","tool_name":"ToolSearch"}],"is_error":false}]}`; string(rows[1]) != want {
		t.Fatalf("filtered row differs at %s", firstDifference(string(rows[1]), want))
	}
	empty := userRow(`[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"tool_reference","tool_name":"Missing"}]}]`)
	if got := string(FilterToolReferences(ctx, []json.RawMessage{empty})[0]); got != `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"[Tool references removed - tools no longer available]"}]}]}` {
		t.Fatalf("emptied tool_result: %s", got)
	}
	// Items without a truthy tool_name stay, as fQs keeps them.
	blank := userRow(`[{"type":"tool_result","tool_use_id":"toolu_3","content":[{"type":"tool_reference"},{"type":"tool_reference","tool_name":""},{"type":"tool_reference","tool_name":"Nope"}]}]`)
	if got := string(FilterToolReferences(ctx, []json.RawMessage{blank})[0]); got != `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_3","content":[{"type":"tool_reference"},{"type":"tool_reference","tool_name":""}]}]}` {
		t.Fatalf("blank tool_name handling: %s", got)
	}
	off := DefinitionContext{Model: "claude-3-5-haiku-20241022"}
	disabled := FilterToolReferences(off, []json.RawMessage{row, referenceRow("SendMessage")})
	if want := `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"note"}],"is_error":false}]}`; string(disabled[0]) != want {
		t.Fatalf("disabled filter differs at %s", firstDifference(string(disabled[0]), want))
	}
	if got := string(disabled[1]); got != `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_search","content":[{"type":"text","text":"[Tool references removed - tool search not enabled]"}]}]}` {
		t.Fatalf("disabled emptied tool_result: %s", got)
	}
	// Assistant rows, string results and malformed rows pass through unchanged.
	passthrough := []json.RawMessage{json.RawMessage(`{"role":"assistant","content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"X"}]}]}`), userRow(`[{"type":"tool_result","tool_use_id":"t","content":"text"}]`), json.RawMessage(`null`), json.RawMessage(`{"role":"user"`)}
	for i, raw := range FilterToolReferences(ctx, passthrough) {
		if string(raw) != string(passthrough[i]) {
			t.Fatalf("row %d changed: %s", i, raw)
		}
	}
	if got := FilterToolReferences(ctx, nil); len(got) != 0 {
		t.Fatalf("nil rows: %v", got)
	}
}

func TestCatalogIgnoresToolSearchGates(t *testing.T) {
	ctx := defaultToolSearchContext()
	gated := ctx
	gated.ToolSearchDisabled, gated.DeferredStubDisabled, gated.ToolSearchFetchRule = true, true, true
	gated.NonDeferrableBuiltins = []string{"SendMessage"}
	base, other := Catalog(ctx), Catalog(gated)
	for i := range base {
		if string(base[i]) != string(other[i]) {
			t.Fatalf("catalog %d changed with tool-search gates", i)
		}
	}
	if names := FirstPartyToolNames(); !names[ToolSearchName] || !names[DeferredToolPlaceholderName] || !names["Agent"] {
		t.Fatalf("first-party names: %v", names)
	}
}
