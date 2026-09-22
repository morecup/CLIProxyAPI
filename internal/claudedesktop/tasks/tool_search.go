package tasks

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Tool-search deferral for the owned built-in pool (Claude Code 2.1.247:
// ToolSearch tool jWe, placeholder PGr, enable decision j1e, request assembly,
// discovery fA, reference filter fQs/uGt and the deferred_tools_delta
// attachment XHr). SendMessage, TaskOutput and TaskStop are deferred: they
// leave the tools array until the model discovers them through ToolSearch.

const (
	ToolSearchName              = "ToolSearch"
	DeferredToolPlaceholderName = "DeferredToolPlaceholder"
)

const (
	placeholderDescription = "Reserved placeholder that keeps deferred tool loading active; never call this tool."
	// placeholderJSON is PGr()'s literal object in its declaration order.
	placeholderJSON = `{"name":"` + DeferredToolPlaceholderName + `","description":"` + placeholderDescription + `","input_schema":{"type":"object","properties":{}},"defer_loading":true}`
	// noMatchingDeferredTools is the string tool_result content of an empty search.
	noMatchingDeferredTools = "No matching deferred tools found"
	// deferredToolsHeader is the first line of the deferred_tools_delta reminder.
	deferredToolsHeader = "The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded \u2014 calling them directly will fail with InputValidationError. Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:"
	// deferredToolsRemovedHeader opens the removed-names paragraph (up to
	// deferredToolsListLimit names, one per line).
	deferredToolsRemovedHeader = "The following deferred tools are no longer available (their MCP server disconnected). Do not search for them \u2014 ToolSearch will return no match:"
	// deferredToolsAmbientNote follows a removed-tools paragraph.
	deferredToolsAmbientNote = "This is ambient context \u2014 do not narrate it to the user unless they ask or it is directly relevant to their request."
	// deferredToolsListLimit is the SDK's md list limit for removed names.
	deferredToolsListLimit = 30
	toolReferencesRemoved  = "[Tool references removed - tools no longer available]"
	toolReferencesDisabled = "[Tool references removed - tool search not enabled]"
	deferLoadingMember     = `,"defer_loading":true`
)

// ToolSearch prompt pieces (Kbo, Ybo, Xbo, Zbo): jst() = head + rule + tail.
const (
	toolSearchPromptHead        = "Fetches full schema definitions for deferred tools so they can be called.\n\nDeferred tools appear by name in <system-reminder> messages."
	toolSearchPromptNoFetchRule = " Until fetched, only the name is known \u2014 there is no parameter schema, so the tool cannot be invoked."
	toolSearchPromptFetchRule   = " Until fetched, only the name is known \u2014 there is no parameter schema, so calling the tool fails with InputValidationError. When any instruction, system reminder, or other tool's description names a deferred tool, fetch it with query \"select:<name>\" before calling it."
	toolSearchPromptTail        = " This tool takes a query, matches it against the deferred tool list, and returns the matched tools' complete JSONSchema definitions inside a <functions> block. Once a tool's schema appears in that result, it is callable exactly like any tool defined at the top of the prompt.\n\n" +
		"Result format: each matched tool appears as one <function>{\"description\": \"...\", \"name\": \"...\", \"parameters\": {...}}</function> line inside the <functions> block \u2014 the same encoding as the tool list at the top of this prompt.\n\n" +
		"Query forms:\n- \"select:Read,Edit,Grep\" \u2014 fetch these exact tools by name\n- \"notebook jupyter\" \u2014 keyword search, up to max_results best matches\n- \"+slack send\" \u2014 require \"slack\" in the name, rank by remaining terms"
)

// toolSearchSchema is the zod v4 output of z8o(): query is a described
// string; max_results is optional().default(5).describe(...), which the
// converter flattens as description, default, then the inner number type,
// and lists under required because a default always yields a value.
const toolSearchSchema = jsonSchemaHead + `"query":{"description":"Query to find deferred tools. Use \"select:<tool_name>\" for direct selection, or keywords to search.","type":"string"},"max_results":{"description":"Maximum number of results to return (default: 5)","default":5,"type":"number"}},"required":["query","max_results"],"additionalProperties":false}`

// defaultToolSearchUnsupportedModels is the tengu_tool_search_unsupported_models fallback.
var defaultToolSearchUnsupportedModels = []string{"claude-3-5-haiku", "claude-3-haiku"}

// announcementExclusions (Jon) never count as announced deferred tools.
var announcementExclusions = map[string]bool{"Frame": true, "FrameRead": true, "TeamCreate": true, "TeamDelete": true, "SuggestBackgroundPR": true, "AutofixPr": true}

// poolTool mirrors the tool object fields the SDK reads on the request and
// search paths: name, aliases, searchHint, shouldDefer and the prompt that
// doubles as the serialized description.
type poolTool struct {
	name        string
	aliases     []string
	searchHint  string
	shouldDefer bool
	prompt      func(DefinitionContext) string
	schema      func(DefinitionContext) string
}

func constantText(text string) func(DefinitionContext) string {
	return func(DefinitionContext) string { return text }
}

// toolPool is the owned built-in pool in Yh (name.localeCompare) order. The
// placeholder is not a pool member; it is spliced into the serialized array.
var toolPool = []poolTool{
	{name: "Agent", aliases: []string{"Task"}, searchHint: "delegate work to a subagent", prompt: agentPrompt, schema: agentSchema},
	{name: "SendMessage", searchHint: "send messages to agent teammates", shouldDefer: true, prompt: sendMessagePrompt, schema: sendMessageSchema},
	{name: "TaskOutput", aliases: []string{"AgentOutputTool", "BashOutputTool", "AgentOutput", "BashOutput"}, searchHint: "read output/logs from a background task", shouldDefer: true, prompt: constantText(taskOutputPrompt), schema: constantText(taskOutputSchema)},
	{name: "TaskStop", aliases: []string{"KillShell", "KillBash"}, searchHint: "kill a running background task", shouldDefer: true, prompt: constantText(taskStopPrompt), schema: constantText(taskStopSchema)},
	{name: ToolSearchName, prompt: toolSearchPrompt, schema: constantText(toolSearchSchema)},
}

func (t poolTool) definition(ctx DefinitionContext) json.RawMessage {
	return definition(t.name, t.prompt(ctx), t.schema(ctx), ctx.EagerInputStreaming)
}

// matches is the alias-aware lookup Ft: the exact name or one of the aliases.
func (t poolTool) matches(name string) bool {
	return t.name == name || slices.Contains(t.aliases, name)
}

// deferrable follows aS for the owned family: non-deferrable builtins and
// ToolSearch never defer; the rest follow shouldDefer (Agent does not set it).
func (t poolTool) deferrable(ctx DefinitionContext) bool {
	if slices.Contains(ctx.NonDeferrableBuiltins, t.name) || t.name == ToolSearchName {
		return false
	}
	return t.shouldDefer
}

func poolNames() map[string]bool {
	names := make(map[string]bool, len(toolPool))
	for _, tool := range toolPool {
		names[tool.name] = true
	}
	return names
}

func deferrableTools(ctx DefinitionContext) []poolTool {
	var result []poolTool
	for _, tool := range toolPool {
		if tool.deferrable(ctx) {
			result = append(result, tool)
		}
	}
	return result
}

func toolSearchPrompt(ctx DefinitionContext) string {
	rule := toolSearchPromptNoFetchRule
	if ctx.ToolSearchFetchRule {
		rule = toolSearchPromptFetchRule
	}
	return toolSearchPromptHead + rule + toolSearchPromptTail
}

// ToolSearchDefinition serializes the ToolSearch tool exactly as the SDK
// does: name, description (jst()), input_schema and eager_input_streaming.
func ToolSearchDefinition(ctx DefinitionContext) json.RawMessage {
	tool, _ := lookupPoolTool(toolPool, ToolSearchName)
	return tool.definition(ctx)
}

// PlaceholderDefinition returns PGr()'s DeferredToolPlaceholder object.
func PlaceholderDefinition() json.RawMessage {
	return json.RawMessage(placeholderJSON)
}

// toolSearchModelSupported follows Zb: the lowercased model must not contain
// any entry of the unsupported list.
func toolSearchModelSupported(ctx DefinitionContext) bool {
	unsupported := ctx.ToolSearchUnsupportedModels
	if unsupported == nil {
		unsupported = defaultToolSearchUnsupportedModels
	}
	model := strings.ToLower(ctx.Model)
	for _, entry := range unsupported {
		if strings.Contains(model, strings.ToLower(entry)) {
			return false
		}
	}
	return true
}

// toolSearchRegistered is the part of j1e/qhe shared by the request tools and
// the reminder: mode is not "standard" and the model supports tool_reference.
func toolSearchRegistered(ctx DefinitionContext) bool {
	return !ctx.ToolSearchDisabled && toolSearchModelSupported(ctx)
}

// ToolSearchEnabled reports the pinned j1e decision for the owned pool in the
// Desktop worker lane: mode tst, first party, ToolSearch registered, model
// supported, and at least one deferrable tool to search.
func ToolSearchEnabled(ctx DefinitionContext) bool {
	return toolSearchRegistered(ctx) && len(deferrableTools(ctx)) > 0
}

// withDeferLoading appends the defer_loading member after the serialized
// definition's last member, as the definition builder does.
func withDeferLoading(def json.RawMessage) json.RawMessage {
	out := make([]byte, 0, len(def)+len(deferLoadingMember))
	out = append(out, def[:len(def)-1]...)
	out = append(out, deferLoadingMember...)
	return append(out, '}')
}

// RequestTools returns the tools array of one owned request. messages are
// the API-shaped rows ({"role","content"}) that will be sent, in order.
// Enabled: deferrable tools appear only once discovered (with defer_loading),
// ToolSearch always, and the placeholder is spliced before the last entry.
// Disabled: the four owned tools without ToolSearch.
func RequestTools(ctx DefinitionContext, messages []json.RawMessage) []json.RawMessage {
	enabled := ToolSearchEnabled(ctx)
	var discovered map[string]bool
	if enabled {
		discovered = DiscoveredTools(messages)
	}
	tools := make([]json.RawMessage, 0, len(toolPool)+1)
	for _, tool := range toolPool {
		deferred := enabled && tool.deferrable(ctx)
		if enabled {
			if deferred && !discovered[tool.name] {
				continue
			}
		} else if tool.name == ToolSearchName {
			continue
		}
		def := tool.definition(ctx)
		if deferred {
			def = withDeferLoading(def)
		}
		tools = append(tools, def)
	}
	if enabled && !ctx.DeferredStubDisabled {
		tools = slices.Insert(tools, max(len(tools)-1, 0), PlaceholderDefinition())
	}
	return tools
}

// DiscoveredTools follows fA: every tool_reference item inside a tool_result
// block with array content in a user row names a discovered tool. Rows and
// blocks of other shapes are ignored.
func DiscoveredTools(messages []json.RawMessage) map[string]bool {
	discovered := map[string]bool{}
	for _, raw := range messages {
		var row struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Role != "user" {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(row.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			var result struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(block, &result) != nil || result.Type != "tool_result" {
				continue
			}
			var items []json.RawMessage
			if json.Unmarshal(result.Content, &items) != nil {
				continue
			}
			for _, item := range items {
				if name, ok := toolReferenceName(item); ok && name != "" {
					discovered[name] = true
				}
			}
		}
	}
	return discovered
}

// toolReferenceName reports whether item is a tool_reference block and its
// string tool_name (empty when the member is missing or not a string).
func toolReferenceName(item json.RawMessage) (string, bool) {
	var reference struct {
		Type     string          `json:"type"`
		ToolName json.RawMessage `json:"tool_name"`
	}
	if json.Unmarshal(item, &reference) != nil || reference.Type != "tool_reference" {
		return "", false
	}
	var name string
	if json.Unmarshal(reference.ToolName, &name) != nil {
		return "", true
	}
	return name, true
}

// FilterToolReferences applies fQs (enabled) or uGt (disabled) to the rows:
// tool_reference items naming tools outside the pool (after the legacy alias
// map) are dropped, every tool_reference is dropped when tool search is off,
// and a tool_result left empty gets the native replacement text. Untouched
// rows keep their bytes.
func FilterToolReferences(ctx DefinitionContext, messages []json.RawMessage) []json.RawMessage {
	enabled := ToolSearchEnabled(ctx)
	available := poolNames()
	rows := make([]json.RawMessage, len(messages))
	for i, raw := range messages {
		rows[i] = raw
		if rewritten, changed := filterRowToolReferences(raw, enabled, available); changed {
			rows[i] = rewritten
		}
	}
	return rows
}

func filterRowToolReferences(raw json.RawMessage, enabled bool, available map[string]bool) (json.RawMessage, bool) {
	fields, ok := decodeObjectFields(raw)
	if !ok {
		return raw, false
	}
	role, content := fieldIndex(fields, "role"), fieldIndex(fields, "content")
	var roleName string
	if role < 0 || content < 0 || json.Unmarshal(fields[role].value, &roleName) != nil || roleName != "user" {
		return raw, false
	}
	var blocks []json.RawMessage
	if json.Unmarshal(fields[content].value, &blocks) != nil {
		return raw, false
	}
	changed := false
	for i, block := range blocks {
		blockFields, ok := decodeObjectFields(block)
		if !ok {
			continue
		}
		kind, blockContent := fieldIndex(blockFields, "type"), fieldIndex(blockFields, "content")
		var kindName string
		if kind < 0 || blockContent < 0 || json.Unmarshal(blockFields[kind].value, &kindName) != nil || kindName != "tool_result" {
			continue
		}
		var items []json.RawMessage
		if json.Unmarshal(blockFields[blockContent].value, &items) != nil {
			continue
		}
		kept := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			if !dropToolReference(item, enabled, available) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(items) {
			continue
		}
		changed = true
		if len(kept) == 0 {
			text := toolReferencesRemoved
			if !enabled {
				text = toolReferencesDisabled
			}
			kept = []json.RawMessage{json.RawMessage(`{"type":"text","text":` + jsQuote(text) + `}`)}
		}
		blockFields[blockContent].value = encodeArray(kept)
		blocks[i] = encodeObjectFields(blockFields)
	}
	if !changed {
		return raw, false
	}
	fields[content].value = encodeArray(blocks)
	return encodeObjectFields(fields), true
}

// dropToolReference decides one tool_result item: fQs keeps items whose
// legacy-resolved tool_name is a pool name (and items without a truthy
// tool_name); uGt drops every tool_reference.
func dropToolReference(item json.RawMessage, enabled bool, available map[string]bool) bool {
	var reference struct {
		Type     string          `json:"type"`
		ToolName json.RawMessage `json:"tool_name"`
	}
	if json.Unmarshal(item, &reference) != nil || reference.Type != "tool_reference" {
		return false
	}
	if !enabled {
		return true
	}
	if !jsTruthy(reference.ToolName) {
		return false
	}
	var name string
	if json.Unmarshal(reference.ToolName, &name) != nil {
		return true
	}
	if canonical, ok := toolAliases[name]; ok {
		name = canonical
	}
	return !available[name]
}

// jsTruthy reports JavaScript truthiness of a JSON value (absent, null,
// false, 0 and "" are falsy).
func jsTruthy(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	switch value {
	case "", "null", "false", `""`:
		return false
	}
	if number, err := strconv.ParseFloat(value, 64); err == nil {
		return number != 0
	}
	return true
}

// DeferredToolsReminder returns the wrapped <system-reminder> text to append
// as a user row after the last row of messages, or "" when nothing changed.
// Prior announcements are recovered from earlier user text that starts a
// paragraph with the exact reminder header (owned history is wire-shaped and
// carries no attachment rows).
func DeferredToolsReminder(ctx DefinitionContext, messages []json.RawMessage) string {
	if !toolSearchRegistered(ctx) {
		return ""
	}
	announced := announcedDeferredTools(messages)
	announcedSet := make(map[string]bool, len(announced))
	for _, name := range announced {
		announcedSet[name] = true
	}
	deferrable := map[string]bool{}
	var added []string
	for _, tool := range deferrableTools(ctx) {
		deferrable[tool.name] = true
		if !announcedSet[tool.name] {
			added = append(added, tool.name)
		}
	}
	pool := poolNames()
	var removed []string
	for _, name := range announced {
		if !deferrable[name] && !pool[name] {
			removed = append(removed, name)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	sort.Strings(added)
	sort.Strings(removed)
	var paragraphs []string
	if len(added) > 0 {
		paragraphs = append(paragraphs, deferredToolsHeader+"\n"+strings.Join(added, "\n"))
	}
	if len(removed) > 0 {
		if len(removed) > deferredToolsListLimit {
			paragraphs = append(paragraphs, strconv.Itoa(len(removed))+" deferred tools are no longer available (MCP server disconnected): "+strings.Join(removed, ", ")+". Do not search for them \u2014 ToolSearch will return no match.")
		} else {
			paragraphs = append(paragraphs, deferredToolsRemovedHeader+"\n"+strings.Join(removed, "\n"))
		}
		paragraphs = append(paragraphs, deferredToolsAmbientNote)
	}
	return "<system-reminder>\n" + strings.Join(paragraphs, "\n\n") + "\n</system-reminder>"
}

// announcedDeferredTools lists, in first-seen order, the names announced by
// earlier reminders and not removed since: the lines that follow the exact
// added or removed header line up to the end of that paragraph, in user rows'
// string content, text blocks and tool_result text (the wire merge folds a
// reminder into a trailing string tool_result).
func announcedDeferredTools(messages []json.RawMessage) []string {
	var names []string
	seen := map[string]bool{}
	for _, text := range userRowTexts(messages) {
		lines := strings.Split(text, "\n")
		for i := 0; i < len(lines); i++ {
			added := lines[i] == deferredToolsHeader
			if !added && lines[i] != deferredToolsRemovedHeader {
				continue
			}
			for i++; i < len(lines); i++ {
				name := lines[i]
				if name == "" || name == "</system-reminder>" {
					break
				}
				switch {
				case added && !seen[name] && !announcementExclusions[name]:
					seen[name] = true
					names = append(names, name)
				case !added && seen[name]:
					delete(seen, name)
					names = slices.DeleteFunc(names, func(known string) bool { return known == name })
				}
			}
		}
	}
	return names
}

// userRowTexts yields the text the model saw in user rows, in order.
func userRowTexts(messages []json.RawMessage) []string {
	var texts []string
	for _, raw := range messages {
		var row struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(row.Content, &text) == nil {
			texts = append(texts, text)
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(row.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			var value struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(block, &value) != nil {
				continue
			}
			switch value.Type {
			case "text":
				texts = append(texts, value.Text)
			case "tool_result":
				if json.Unmarshal(value.Content, &text) == nil {
					texts = append(texts, text)
					continue
				}
				var items []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(value.Content, &items) != nil {
					continue
				}
				for _, item := range items {
					if item.Type == "text" {
						texts = append(texts, item.Text)
					}
				}
			}
		}
	}
	return texts
}

// definitionContext resolves the runtime's gates for one request model the
// way Definitions(model) does.
func (r *Runtime) definitionContext(model string) DefinitionContext {
	ctx := DefinitionContext{SubscriptionType: "pro"}
	if r != nil && r.options.Definitions != nil {
		ctx = r.options.Definitions(model)
	}
	if ctx.Model == "" {
		ctx.Model = model
	}
	return ctx
}

// RequestTools resolves the runtime's gates for model and assembles the tools array.
func (r *Runtime) RequestTools(model string, messages []json.RawMessage) []json.RawMessage {
	return RequestTools(r.definitionContext(model), messages)
}

// DeferredToolsReminder resolves the runtime's gates for model and renders the reminder.
func (r *Runtime) DeferredToolsReminder(model string, messages []json.RawMessage) string {
	return DeferredToolsReminder(r.definitionContext(model), messages)
}

// FilterToolReferences resolves the runtime's gates for model and filters the rows.
func (r *Runtime) FilterToolReferences(model string, messages []json.RawMessage) []json.RawMessage {
	return FilterToolReferences(r.definitionContext(model), messages)
}

// jsonField is one member of a JSON object in document order.
type jsonField struct {
	key   string
	value json.RawMessage
}

// decodeObjectFields splits a JSON object into its members without losing
// their order; anything that is not exactly one object fails.
func decodeObjectFields(raw json.RawMessage) ([]jsonField, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var fields []jsonField
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		fields = append(fields, jsonField{key: key, value: value})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}

func fieldIndex(fields []jsonField, key string) int {
	for i, field := range fields {
		if field.key == key {
			return i
		}
	}
	return -1
}

func encodeObjectFields(fields []jsonField) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsQuote(field.key))
		b.WriteByte(':')
		b.Write(field.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func encodeArray(items []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(item)
	}
	b.WriteByte(']')
	return b.Bytes()
}
