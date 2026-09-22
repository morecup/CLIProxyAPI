package tasks

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// DefinitionContext carries the session gates the SDK reads while it
// serializes the owned local-agent tool family (definition builder o8e with
// the Agent/SendMessage/TaskOutput/TaskStop prompt and schema builders, Claude
// Code 2.1.247). Zero values are the Desktop CCR worker lane: first party,
// non-interactive (no fork), agent teams and cross-session off, background
// tasks enabled and the default subagent steer. The subscription is not
// defaulted here: only "pro" adds the spawn guard, exactly as the SDK reads the
// OAuth account's subscriptionType.
type DefinitionContext struct {
	// Model is the request model; it selects the lean Agent prompt lane.
	Model string
	// TeamsEnabled mirrors the agent-teams gate: it keeps the Agent name,
	// team_name and mode fields and the structured SendMessage protocol forms.
	TeamsEnabled bool
	// CrossSessionEnabled mirrors the cross-session lane (tengu_harbor_kite):
	// SendMessage addressing text, notify_when_idle and message defaults.
	CrossSessionEnabled bool
	// ForkEnabled mirrors the interactive-only fork gate. A forked worker is
	// offered when the gate is on and no agent definition is named "fork".
	ForkEnabled bool
	// BackgroundDisabled mirrors the backgroundTasksDisabled setting.
	BackgroundDisabled bool
	// SubagentSteer is the resolved tengu_thistle_grebe steer; empty means
	// "default" and keeps the proactive-use guidance.
	SubagentSteer string
	// SubscriptionType is the OAuth account subscription; "pro" adds the
	// do-not-spawn paragraph.
	SubscriptionType string
	// LeanPromptForced mirrors tengu_velvet_tide, which moves every model to
	// the lean Agent prompt.
	LeanPromptForced bool
	// EagerInputStreaming mirrors tengu_fgts for first-party requests.
	EagerInputStreaming bool
	// GeneralPurposeUnavailable is set when the built-in general-purpose
	// agent is denied for the session; the prompt then requires subagent_type.
	GeneralPurposeUnavailable bool
	// ToolSearchFetchRule mirrors client data juniper_shoal.gorse_hollow
	// (toolSearchFetchRule, default false): it selects the ToolSearch prompt
	// variant that tells the model to fetch named deferred tools first.
	ToolSearchFetchRule bool
	// ToolSearchDisabled mirrors tool-search mode "standard": a truthy
	// CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS, hipaa, or a falsy
	// ENABLE_TOOL_SEARCH; ToolSearch is then not registered at all.
	ToolSearchDisabled bool
	// ToolSearchUnsupportedModels mirrors feature
	// tengu_tool_search_unsupported_models; nil keeps the SDK default
	// ["claude-3-5-haiku","claude-3-haiku"] (lowercase substring match).
	ToolSearchUnsupportedModels []string
	// DeferredStubDisabled mirrors feature tengu_deferred_stub_tool == false:
	// the DeferredToolPlaceholder is then not spliced into the tools array.
	DeferredStubDisabled bool
	// NonDeferrableBuiltins mirrors feature tengu_non_deferrable_builtins
	// (resolved for the model) and settings non_deferrable_builtins: the
	// named tools are never deferred and stay in every request.
	NonDeferrableBuiltins []string
}

// Definitions expose only implementations owned by this runtime in the
// default lane, with the subscription the telemetry layer reports when the
// account metadata carries none. Advertising captured filesystem tools would
// not create permission to execute them.
func Definitions() []json.RawMessage {
	return Catalog(DefinitionContext{SubscriptionType: "pro"})
}

// Definitions resolves the runtime's gates for one request model.
func (r *Runtime) Definitions(model string) []json.RawMessage {
	ctx := DefinitionContext{SubscriptionType: "pro"}
	if r != nil && r.options.Definitions != nil {
		ctx = r.options.Definitions(model)
	}
	if ctx.Model == "" {
		ctx.Model = model
	}
	return Catalog(ctx)
}

// Catalog serializes the four owned tools exactly as the SDK does: name,
// description (the tool prompt), input_schema (zod v4 draft 2020-12 output)
// and eager_input_streaming, in the built-in pool's localeCompare order.
func Catalog(ctx DefinitionContext) []json.RawMessage {
	return []json.RawMessage{
		definition("Agent", agentPrompt(ctx), agentSchema(ctx), ctx.EagerInputStreaming),
		definition("SendMessage", sendMessagePrompt(ctx), sendMessageSchema(ctx), ctx.EagerInputStreaming),
		definition("TaskOutput", taskOutputPrompt, taskOutputSchema, ctx.EagerInputStreaming),
		definition("TaskStop", taskStopPrompt, taskStopSchema, ctx.EagerInputStreaming),
	}
}

// FirstPartyToolNames lists the owned family, ToolSearch and the deferred
// placeholder. The SDK declares these tools as first-party, so requests
// issued by this runtime keep the names on the wire instead of representing
// them as MCP extensions.
func FirstPartyToolNames() map[string]bool {
	return map[string]bool{"Agent": true, "SendMessage": true, "TaskOutput": true, "TaskStop": true, ToolSearchName: true, DeferredToolPlaceholderName: true}
}

// CanonicalToolName resolves the owned tool a tool_use name addresses: the
// exact name or one of its aliases, which the legacy rename map also lists.
// Names outside the owned family stay unresolved.
func CanonicalToolName(name string) (string, bool) {
	switch name {
	case "Agent", "SendMessage", "TaskOutput", "TaskStop":
		return name, true
	}
	if canonical, ok := toolAliases[name]; ok {
		return canonical, true
	}
	return name, false
}

var toolAliases = map[string]string{
	"Task":            "Agent",
	"KillShell":       "TaskStop",
	"KillBash":        "TaskStop",
	"AgentOutputTool": "TaskOutput",
	"BashOutputTool":  "TaskOutput",
	"AgentOutput":     "TaskOutput",
	"BashOutput":      "TaskOutput",
}

// LeanPromptModel reports whether a model takes the lean Agent prompt in the
// default feature lane: -eap builds, models with the lean_prompt capability
// (claude-opus-4-8, claude-opus-5, claude-fable-5) and claude-mythos-5 do;
// claude-3-*, haiku, sonnet and opus 4.0-4.7 do not; other first-party ids do.
func LeanPromptModel(model string) bool {
	if model == "" {
		return false
	}
	if eapModelPattern.MatchString(model) {
		return true
	}
	canonical := CanonicalModelID(model)
	if leanPromptModels[canonical] || canonical == "claude-mythos-5" {
		return true
	}
	if strings.Contains(canonical, "claude-3-") || strings.Contains(canonical, "haiku") || strings.Contains(canonical, "sonnet") {
		return false
	}
	switch canonical {
	case "claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-5", "claude-opus-4-6", "claude-opus-4-7":
		return false
	}
	return true
}

// CanonicalModelID maps family aliases to their catalog default and
// normalizes dated, provider-prefixed and 1m-suffixed ids through the SDK's
// substring chain to the catalog id.
func CanonicalModelID(model string) string {
	if alias, ok := modelAliasDefaults[model]; ok {
		model = alias
	}
	id := strings.ToLower(model)
	for _, rule := range canonicalModelRules {
		if rule.pattern != nil {
			if rule.pattern.MatchString(id) {
				return rule.id
			}
		} else if strings.Contains(id, rule.contains) {
			return rule.id
		}
	}
	return datedModelSuffix.ReplaceAllString(id, "")
}

var (
	eapModelPattern  = regexp.MustCompile(`(?i)-eap($|\[)`)
	datedModelSuffix = regexp.MustCompile(`-\d{8}$`)
	leanPromptModels = map[string]bool{"claude-opus-4-8": true, "claude-opus-5": true, "claude-fable-5": true}
	// modelAliasDefaults mirrors the catalog aliases' first-party defaults.
	modelAliasDefaults = map[string]string{"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "claude-haiku-4-5", "fable": "claude-fable-5"}
	// canonicalModelRules follow the SDK's substring chain in order.
	canonicalModelRules = []struct {
		contains string
		pattern  *regexp.Regexp
		id       string
	}{
		{contains: "claude-fable-5", id: "claude-fable-5"},
		{contains: "claude-mythos-5", id: "claude-mythos-5"},
		{contains: "claude-opus-5", id: "claude-opus-5"},
		{contains: "claude-opus-4-8", id: "claude-opus-4-8"},
		{contains: "claude-opus-4-7", id: "claude-opus-4-7"},
		{contains: "claude-opus-4-6", id: "claude-opus-4-6"},
		{contains: "claude-opus-4-5", id: "claude-opus-4-5"},
		{contains: "claude-opus-4-1", id: "claude-opus-4-1"},
		{pattern: regexp.MustCompile(`claude-opus-4(?:$|[^-]|-(?:$|[^\d]|\d\d))`), id: "claude-opus-4-0"},
		{contains: "claude-sonnet-5", id: "claude-sonnet-5"},
		{contains: "claude-sonnet-4-6", id: "claude-sonnet-4-6"},
		{contains: "claude-sonnet-4-5", id: "claude-sonnet-4-5"},
		{pattern: regexp.MustCompile(`claude-sonnet-4(?:$|[^-]|-(?:$|[^\d]|\d\d))`), id: "claude-sonnet-4-0"},
		{contains: "claude-haiku-4-5", id: "claude-haiku-4-5"},
		{contains: "claude-3-7-sonnet", id: "claude-3-7-sonnet"},
		{contains: "claude-3-5-sonnet", id: "claude-3-5-sonnet"},
		{contains: "claude-3-5-haiku", id: "claude-3-5-haiku"},
		{contains: "claude-3-opus", id: "claude-3-opus"},
		{contains: "claude-3-sonnet", id: "claude-3-sonnet"},
		{contains: "claude-3-haiku", id: "claude-3-haiku"},
	}
)

// definition writes the serializer's key order without map re-ordering.
func definition(name, description, schema string, eager bool) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"name":`)
	b.WriteString(jsQuote(name))
	b.WriteString(`,"description":`)
	b.WriteString(jsQuote(description))
	b.WriteString(`,"input_schema":`)
	b.WriteString(schema)
	if eager {
		b.WriteString(`,"eager_input_streaming":true`)
	}
	b.WriteString("}")
	return json.RawMessage(b.String())
}

// jsQuote matches JSON.stringify for strings: only quotes, backslashes and
// C0 controls are escaped; every other code point stays literal.
func jsQuote(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				hex := strconv.FormatInt(int64(r), 16)
				if len(hex) == 1 {
					b.WriteByte('0')
				}
				b.WriteString(hex)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

const jsonSchemaHead = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{`

const (
	agentDescriptionProperty     = `"description":{"description":"A short (3-5 word) description of the task","type":"string"}`
	agentPromptProperty          = `"prompt":{"description":"The task for the agent to perform","type":"string"}`
	agentSubagentTypeProperty    = `"subagent_type":{"description":"The type of specialized agent to use for this task","type":"string"}`
	agentModelProperty           = `"model":{"description":"Optional model override for this agent. Takes precedence over the agent definition's model frontmatter. If omitted, uses the agent definition's model, or inherits from the parent. Ignored for subagent_type: \"fork\" — forks always inherit the parent model.","type":"string","enum":["sonnet","opus","haiku","fable"]}`
	agentBackgroundProperty      = `"run_in_background":{"description":"Agents run in the background by default; you will be notified when one completes. Set to false only when your very next action depends on this agent's result and nothing else could usefully happen while it runs — otherwise leave it in the background so the user can hand you other work.","type":"boolean"}`
	agentNameProperty            = `"name":{"description":"Name for the spawned agent. Makes it addressable via SendMessage({to: name}) while running.","type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"}`
	agentTeamNameProperty        = `"team_name":{"description":"Deprecated; ignored. The session has a single implicit team.","type":"string"}`
	agentModeProperty            = `"mode":{"description":"Deprecated; ignored. Subagents inherit the parent session's permission mode; agent-definition frontmatter may override it.","type":"string","enum":["acceptEdits","auto","bypassPermissions","default","dontAsk","plan"]}`
	agentIsolationProperty       = `"isolation":{"description":"Isolation mode. \"worktree\" creates a temporary git worktree so the agent works on an isolated copy of the repo. \"remote\" launches the agent in a remote cloud environment (always runs in background; availability is gated).","type":"string","enum":["worktree","remote"]}`
	sendMessageRecipientPatterns = `"type":"string","allOf":[{"pattern":"^[^\\n\\r]*$"},{"pattern":"^[\\s\\S]{0,300}$"}]`
	sendMessageProtocolForms     = `{"anyOf":[{"type":"object","properties":{"type":{"type":"string","const":"shutdown_request"},"reason":{"type":"string"}},"required":["type"],"additionalProperties":false},{"type":"object","properties":{"type":{"type":"string","const":"shutdown_response"},"request_id":{"type":"string","minLength":1,"allOf":[{"pattern":"^[^\\n\\r]*$"},{"pattern":"^[\\s\\S]{0,300}$"}]},"approve":{"type":"boolean"},"reason":{"type":"string"}},"required":["type","request_id","approve"],"additionalProperties":false},{"type":"object","properties":{"type":{"type":"string","const":"plan_approval_response"},"request_id":{"type":"string","minLength":1,"allOf":[{"pattern":"^[^\\n\\r]*$"},{"pattern":"^[\\s\\S]{0,300}$"}]},"approve":{"type":"boolean"},"feedback":{"type":"string"}},"required":["type","request_id","approve"],"additionalProperties":false}]}`
	taskOutputSchema             = jsonSchemaHead + `"task_id":{"description":"The task ID to get output from","type":"string"},"block":{"description":"Whether to wait for completion","default":true,"type":"boolean"},"timeout":{"description":"Max wait time in ms","default":30000,"type":"number","minimum":0,"maximum":600000}},"required":["task_id","block","timeout"],"additionalProperties":false}`
	taskStopSchema               = jsonSchemaHead + `"task_id":{"description":"The ID of the background task to stop. Agent-team teammates and named background agents are also accepted by agent ID or name.","type":"string"},"shell_id":{"description":"Deprecated: use task_id instead","type":"string"}},"additionalProperties":false}`
)

// agentSchema follows ous(): the base object, the teams merge, the isolation
// extension, cwd omitted, run_in_background omitted when background tasks are
// disabled or the fork gate is on, and the teams fields stripped afterwards.
func agentSchema(ctx DefinitionContext) string {
	properties := []string{agentDescriptionProperty, agentPromptProperty, agentSubagentTypeProperty, agentModelProperty}
	if !ctx.BackgroundDisabled && !ctx.ForkEnabled {
		properties = append(properties, agentBackgroundProperty)
	}
	if ctx.TeamsEnabled {
		properties = append(properties, agentNameProperty, agentTeamNameProperty, agentModeProperty)
	}
	properties = append(properties, agentIsolationProperty)
	return jsonSchemaHead + strings.Join(properties, ",") + `},"required":["description","prompt"],"additionalProperties":false}`
}

// sendMessageSchema follows GNt(): the recipient/summary descriptions switch
// with the cross-session lane and the message accepts the legacy protocol
// forms only with agent teams.
func sendMessageSchema(ctx DefinitionContext) string {
	to := `"to":{"description":"Recipient: teammate name",` + sendMessageRecipientPatterns + `}`
	summary := `"summary":{"description":"A 5-10 word summary shown as a one-line preview in the UI. Defaults to the first line of a plain-text message; longer summaries are truncated to 200 characters rather than rejected.","type":"string","maxLength":200}`
	text := `{"description":"Plain text message content","type":"string"}`
	var message, idle string
	if ctx.CrossSessionEnabled {
		to = `"to":{"description":"Recipient: a name from ListAgents (append its \" [ref]\" only when a listing or an error shows one), a teammate name, \"main\", or a background agent's agentId",` + sendMessageRecipientPatterns + `}`
		summary = `"summary":{"description":"A 5-10 word label for your own transcript row (not transmitted — the recipient previews the first line of ` + "`message`" + `). Truncated to 200 characters rather than rejected.","type":"string","maxLength":200}`
		text = `{"description":"Plain text message content. The recipient's human sees only the FIRST LINE as a one-line preview until they expand it, so make the first line a clear, self-contained sentence saying what this is about — not a greeting, preamble, or bare @-mention.","type":"string"}`
		idle = `,"notify_when_idle":{"description":"Ask a session ON THIS MACHINE to send you ONE notice when it next goes idle (finishes its turn with nothing queued) or exits — opt-in, one-shot, no polling. With a message: deliver it now AND subscribe. Without a message (omit it): a pure subscription that costs the other session nothing.","type":"boolean"}`
	}
	switch {
	case ctx.TeamsEnabled && ctx.CrossSessionEnabled:
		message = `"message":{"default":"","anyOf":[` + text + `,` + sendMessageProtocolForms + `]}`
	case ctx.TeamsEnabled:
		message = `"message":{"anyOf":[` + text + `,` + sendMessageProtocolForms + `]}`
	case ctx.CrossSessionEnabled:
		message = `"message":{"default":"",` + text[1:]
	default:
		message = `"message":` + text
	}
	return jsonSchemaHead + to + `,` + summary + `,` + message + idle + `},"required":["to","message"],"additionalProperties":false}`
}

const taskStopPrompt = "\n- Stops a running background task by its ID\n- Takes a task_id parameter identifying the task to stop\n- To stop an agent-team teammate, pass its agent ID (\"name@team\") or bare teammate name as task_id\n- To stop a background agent spawned with a name, pass that name as task_id\n- Returns a success or failure status\n- Use this tool when you need to terminate a long-running task\n"

const taskOutputPrompt = "DEPRECATED: Background tasks return their output file path in the tool result, and you receive a <task-notification> with the same path when the task completes.\n" +
	"- For bash tasks: prefer using the Read tool on that output file path — it contains stdout/stderr.\n" +
	"- For local_agent tasks: use the Agent tool result directly. Do NOT Read the .output file — it is a symlink to the full subagent conversation transcript (JSONL) and will overflow your context window.\n" +
	"- For remote_agent tasks: prefer using the Read tool on the output file path — it contains the streamed remote session output (same as bash).\n\n" +
	"- Retrieves output from a running or completed task (background shell, agent, or remote session)\n" +
	"- Takes a task_id parameter identifying the task\n" +
	"- Returns the task output along with status information\n" +
	"- Use block=true (default) to wait for task completion\n" +
	"- Use block=false for non-blocking check of current status\n" +
	"- Task IDs can be found using the /tasks command\n" +
	"- Works with all task types: background shells, async agents, and remote sessions"

// sendMessagePrompt follows tTr(teams) with the cross-session sections.
func sendMessagePrompt(ctx DefinitionContext) string {
	var rows, cross string
	if ctx.CrossSessionEnabled {
		rows = "\n| `\"worker\"` | Any agent from `ListAgents` — subagent, another local Claude session |\n| `\"worker [3fa9c1]\"` | Same, plus its `[ref]` — only when a listing or an error shows one |"
		cross = "\n\n## Cross-session\n\n" +
			"Use `ListAgents` to discover targets. Every row leads with the agent's `name [ref]` — the name IS the address; there is no separate address syntax.\n\n" +
			"```json\n{\"to\": \"worker\", \"message\": \"check if tests pass over there\"}\n{\"to\": \"worker [3fa9c1]\", \"message\": \"you, specifically\"}\n```\n\n" +
			"Send the bare name — a name that exactly matches one live agent or session (on this machine, on another machine, or in the cloud) delivers directly. Append the ` [ref]` only when the bare name is not enough — `ListAgents` shows two rows with it, or an error asks you to disambiguate (you typed only a prefix, or a session list could not be checked). A ref you did not just read from a listing or an error will not resolve, and if the same name also names an in-process agent, the bare name always wins — use the in-process one.\n\n" +
			"A listed peer is alive and will process your message; messages enqueue and drain at the receiver's next tool round (its `ListAgents` row says whether it is busy or idle right now). Your message arrives wrapped as `<cross-session-message from=\"...\">`. **To reply to an incoming message, copy its `from` attribute as your `to`.**\n\n" +
			"To hear when a session ON THIS MACHINE finishes what it is doing, pass `notify_when_idle: true` (from the main conversation only) — one-shot and opt-in: exactly one `[Cross-session idle notice]` arrives when it next goes idle (or exits) — shown to you, or only to your user when this session holds peer messages for approval (the tool result says which); if it never signals within the subscription's lifetime (it may still be busy, may refuse inbound requests, or may have ended abruptly) the notice says the subscription expired instead. Omit `message` for a pure subscription that costs that session nothing; include one to deliver it now AND subscribe. Never poll `ListAgents` in a loop or send \"are you done?\" messages instead.\n\n" +
			"Permission boundaries are per-session: NEVER ask a peer to perform an action that was denied or blocked in your session, or that you expect your own permission settings would block — a peer doing it for you bypasses the user's permission decision (cross-session permission laundering). Route blocked work back to your user instead."
	}
	var protocol string
	if ctx.TeamsEnabled {
		protocol = "\n\n## Protocol responses (legacy)\n\nIf you receive a JSON message with `type: \"shutdown_request\"` or `type: \"plan_approval_request\"`, respond with the matching `_response` type — echo the `request_id`, set `approve` true/false:\n\n" +
			"```json\n{\"to\": \"team-lead\", \"message\": {\"type\": \"shutdown_response\", \"request_id\": \"...\", \"approve\": true}}\n{\"to\": \"researcher\", \"message\": {\"type\": \"plan_approval_response\", \"request_id\": \"...\", \"approve\": false, \"feedback\": \"add error handling\"}}\n```\n\n" +
			"Approving shutdown terminates your process. Rejecting plan sends the teammate back to revise. Don't originate `shutdown_request` unless asked. Don't send structured JSON status messages — report progress through your task tools if you have them, otherwise in plain prose."
	}
	text := "\n# SendMessage\n\nSend a message to another agent.\n\n" +
		"```json\n{\"to\": \"researcher\", \"summary\": \"assign task 1\", \"message\": \"start on task #1\"}\n```\n\n" +
		"| `to` | |\n|---|---|\n| `\"researcher\"` | Teammate by name |\n| `\"main\"` | The main conversation (background subagents only) |" + rows + "\n\n" +
		"Your plain text output is NOT visible to other agents — to communicate, you MUST call this tool. Messages from teammates are delivered automatically; you don't check an inbox. Refer to agents by name — names keep working after an agent completes (a send resumes it from its transcript). Use the raw `agentId` (format `a...-...`) from its spawn result only when the agent has no name, or when a newer agent took the name (latest wins). When relaying, don't quote the original — it's already rendered to the user." +
		cross + protocol + "\n"
	return strings.TrimFunc(text, jsTrimSpace)
}

const (
	agentSubagentTypeRequired = "subagent_type is required: the general-purpose agent is not available in this session"
	agentSearchDirectly       = "For a single-fact lookup where you already know the file, symbol, or value, search directly. Once you've delegated a search, don't also run it yourself — wait for the result."
	agentAuditPrompt          = "  prompt: \"Audit what's left before this branch can ship. Check: uncommitted changes, commits ahead of main, whether tests exist, whether the GrowthBook gate is wired up, whether CI-relevant files changed. Report a punch list — done vs. missing. Under 200 words.\"\n"
	agentMigrationPrompt      = "  prompt: \"Review migration 0042_user_schema.sql for safety. Context: we're adding a NOT NULL column to a 50M-row table. Existing rows get a backfill default. I want a second opinion on whether the backfill approach is safe under concurrent writes — I've checked locking behavior but want independent verification. Report: is this safe, and if not, what specifically breaks?\"\n"
	agentAuditBack            = "assistant: Audit's back. Three blockers: no tests for the new prompt path, GrowthBook gate wired but not in build_flags.yaml, and one uncommitted file.\n"
	agentMidWaitExample       = "<example>\nuser: \"so is the gate wired up or not\"\n<commentary>\nUser asks mid-wait. The audit%s was launched to answer exactly this, and it hasn't returned. %sGive status, not a fabricated result.\n</commentary>\nassistant: Still waiting on the audit — that's one of the things it's checking. Should land shortly.\n</example>\n"
	agentSecondOpinionHead    = "<example>\nuser: \"Can you get a second opinion on whether this migration is safe?\"\nassistant: <thinking>I'll ask the code-reviewer agent — it won't see my analysis, so it can give an independent read.</thinking>\n"
)

// agentPrompt follows xor() for a non-coordinator caller: the fork section,
// the prompt-writing guide, the background/foreground examples, the lean and
// full usage notes, and the plan/steer paragraphs.
func agentPrompt(ctx DefinitionContext) string {
	fork := ctx.ForkEnabled
	generalPurpose := !ctx.GeneralPurposeUnavailable
	background := !ctx.BackgroundDisabled
	lean := ctx.LeanPromptForced || LeanPromptModel(ctx.Model)
	steerDefault := ctx.SubagentSteer == "" || ctx.SubagentSteer == "default"
	pro := ctx.SubscriptionType == "pro"

	forkOr := ""
	if fork {
		forkOr = "`\"fork\"` or "
	}
	chooseListed := agentSubagentTypeRequired + ", so choose " + forkOr + "one of the listed agent types."

	forkSection := ""
	if fork {
		forkSection = "\n\n## When to fork\n\n" +
			"Fork yourself (pass `subagent_type: \"fork\"`) when the intermediate tool output isn't worth keeping in your context. The criterion is qualitative — \"will I need this output again\" — not task size. Fork open-ended questions. If research can be broken into independent questions, launch parallel forks in one message. A fork beats a fresh subagent for this — it inherits context and shares your cache.\n\n" +
			"Forks are cheap because they share your prompt cache.\n\n" +
			"**Don't peek.** The tool result includes an `output_file` path — do not Read or tail it. You get a completion notification; trust it. Reading the transcript mid-flight pulls the fork's tool noise into your context, which defeats the point of forking.\n\n" +
			"**Don't race.** After launching, you know nothing about what the fork found. Never fabricate or predict fork results in any format — not as prose, summary, or structured output. The notification arrives as a user-role message in a later turn; it is never something you write yourself. If the user asks a follow-up before the notification lands, tell them the fork is still running — give status, not a guess.\n\n" +
			"**Writing a fork prompt.** Since the fork inherits your context, the prompt is a *directive* — what to do, not what the situation is. Be specific about scope: what's in, what's out, what another agent is handling. Don't re-explain background.\n"
	}

	freshLead, terse := "", "Terse"
	if fork {
		freshLead, terse = "Any agent other than a fork starts with zero context. ", "For fresh agents, terse"
	}
	writing := "\n\n## Writing the prompt\n\n" + freshLead +
		"Brief the agent like a smart colleague who just walked into the room — it hasn't seen this conversation, doesn't know what you've tried, doesn't understand why this task matters.\n" +
		"- Explain what you're trying to accomplish and why.\n" +
		"- Describe what you've already learned or ruled out.\n" +
		"- Give enough context about the surrounding problem that the agent can make judgment calls rather than just following a narrow instruction.\n" +
		"- If you need a short response, say so (\"report in under 200 words\").\n" +
		"- Lookups: hand over the exact command. Investigations: hand over the question — prescribed steps become dead weight when the premise is wrong.\n\n" +
		terse + " command-style prompts produce shallow, generic work.\n\n" +
		"**Never delegate understanding.** Don't write \"based on your findings, fix the bug\" or \"based on the research, implement it.\" Those phrases push synthesis onto the agent instead of doing it yourself. Write prompts that prove you understood: include file paths, line numbers, what specifically to change."

	forkExamples := "Example usage:\n\n<example>\nuser: \"What's left on this branch before we can ship?\"\n" +
		"assistant: <thinking>Forking this — it's a survey question. I want the punch list, not the git output in my context.</thinking>\n" +
		"Agent({\n  subagent_type: \"fork\",\n  name: \"ship-audit\",\n  description: \"Branch ship-readiness audit\",\n" + agentAuditPrompt + "})\n" +
		"assistant: Ship-readiness audit running.\n<commentary>\n" +
		"Turn ends here. The coordinator knows nothing about the findings yet. What follows is a SEPARATE turn — the notification arrives from outside, as a user-role message. It is not something the coordinator writes.\n</commentary>\n" +
		"[later turn — notification arrives as user message]\n" + agentAuditBack + "</example>\n\n" +
		strings.Replace(strings.Replace(agentMidWaitExample, "%s", " fork", 1), "%s", "The coordinator does not have this answer. ", 1) + "\n" +
		agentSecondOpinionHead + "<commentary>\n" +
		"A non-fork subagent_type is specified, so the agent starts fresh. It needs full context in the prompt. The briefing explains what to assess and why.\n</commentary>\n" +
		"Agent({\n  name: \"migration-review\",\n  description: \"Independent migration review\",\n  subagent_type: \"code-reviewer\",\n" + agentMigrationPrompt + "})\n</example>\n"

	secondOpinion := agentSecondOpinionHead + "Agent({\n  description: \"Independent migration review\",\n  subagent_type: \"code-reviewer\",\n" + agentMigrationPrompt + "})\n<commentary>\n" +
		"The agent starts with no context from this conversation, so the prompt briefs it: what to assess, the relevant background, and what form the answer should take.\n</commentary>\n</example>\n"

	auditHead := "<example>\nuser: \"What's left on this branch before we can ship?\"\n" +
		"assistant: <thinking>A survey question across git state, tests, and config. I'll delegate it and ask for a short report so the raw command output stays out of my context.</thinking>\n" +
		"Agent({\n  description: \"Branch ship-readiness audit\",\n" + agentAuditPrompt + "})\n"
	freshExamples := "Example usage:\n\n"
	switch {
	case !generalPurpose:
	case background:
		freshExamples += auditHead + "assistant: Ship-readiness audit running in the background.\n<commentary>\n" +
			"The prompt is self-contained: it states the goal, lists what to check, and caps the response length. The agent runs in the background (the default), so the turn ends here — nothing about its findings is known yet. The report arrives in a SEPARATE turn, as a completion notification from outside; it is never something you write yourself.\n</commentary>\n" +
			"[later turn — notification arrives as user message]\n" + agentAuditBack + "</example>\n\n" +
			strings.Replace(strings.Replace(agentMidWaitExample, "%s", "", 1), "%s", "", 1) + "\n"
	default:
		freshExamples += auditHead + "<commentary>\n" +
			"The prompt is self-contained: it states the goal, lists what to check, and caps the response length. The agent's report comes back as the tool result; relay the findings to the user.\n</commentary>\n</example>\n\n"
	}
	freshExamples += secondOpinion

	plan := ""
	if pro {
		plan = "\n\n**Do not spawn agents unless the user asks.** Each spawn starts cold and re-derives context you already have — it's the expensive path on this plan. A task with \"multiple angles,\" \"thorough,\" or several parts is not a request to spawn; handle it inline with your own tools. Only use this tool when the user explicitly says to use a subagent, or names one of the available agent types."
	}
	var selection string
	if fork {
		other := "any other type starts a fresh agent. " + chooseListed
		if generalPurpose {
			other = "any other type — or omitting it — starts a fresh agent (general-purpose by default)."
		}
		selection = "When using the Agent tool, specify a subagent_type to select an agent: `\"fork\"` forks yourself (the fork inherits your full conversation context and always runs on your model — a `model` override is ignored); " + other
	} else {
		other := chooseListed
		if generalPurpose {
			other = "If omitted, the general-purpose agent is used."
		}
		selection = "When using the Agent tool, specify a subagent_type parameter to select which agent type to use. " + other
	}
	header := "Launch a new agent to handle complex, multi-step tasks. Each agent type has specific capabilities and tools available to it.\n\n" +
		"Available agent types are listed in <system-reminder> messages in the conversation." + plan + "\n\n" + selection

	whenNotToUse := ""
	if !fork {
		whenNotToUse = "\n## When not to use\n\n" +
			"If the target is already known, use the direct tool: Read for a known path, the Grep tool for a specific symbol or string. Reserve this tool for open-ended questions that span the codebase, or tasks that match an available agent type.\n"
	}

	examples := freshExamples
	if fork {
		examples = forkExamples
	}
	forkResume, forkResumeFull := "", ""
	if fork {
		forkResume = " (except subagent_type: \"fork\", which inherits your context)"
		forkResumeFull = " (except subagent_type: \"fork\")"
	}

	if lean {
		// The fork gate and fork availability coincide here (no agent definition
		// named fork), so the gate-on/fork-unavailable wording never applies.
		backgroundNote := ""
		if background && !fork {
			backgroundNote = "\n- Subagents run in the background by default; you'll be notified when one completes. Pass `run_in_background: false` only when your very next action depends on the result and nothing else could usefully happen while it runs — otherwise background it so the user can interject. Never fabricate or predict a pending agent's results — the notification is never something you write yourself; if the user asks before it arrives, say it's still running."
		}
		forkNote := ""
		if fork {
			forkNote = "\n\nA fork runs in the background and keeps its tool output out of your context. If you are the fork, execute directly — don't re-delegate. Subagents run in the background; you'll be notified when one completes. Never fabricate or predict a pending agent's results — the notification is never something you write yourself; if the user asks before it arrives, say it's still running."
		}
		whenToUse := ""
		if plan == "" {
			if steerDefault {
				whenToUse = "\n\n## When to use\n\nReach for this when the task matches an available agent type, when you have independent work to run in parallel, or when answering would mean reading across several files — delegate it and you keep the conclusion, not the file dumps. " + agentSearchDirectly
			} else {
				whenToUse = "\n\n## When to use\n\n" + agentSearchDirectly
			}
		}
		report := "The agent's final message is returned to you as the tool result; it is not shown to the user — relay what matters."
		if background {
			report = "The agent's final report is not shown to the user — relay what matters."
		}
		return header + whenToUse + forkNote + "\n\n- " + report + "\n" +
			"- Use SendMessage with the agent's ID or name to continue a previously spawned agent with its context intact; a new Agent call starts fresh" + forkResume + ".\n" +
			"- Each agent type's model, reasoning effort, and tools come from its definition (`.claude/agents/*.md` frontmatter or SDK `agents`).\n" +
			"- `isolation: \"worktree\"` gives the agent its own git worktree (auto-cleaned if unchanged)." + backgroundNote
	}

	done := "When the agent is done, it will return a single message back to you. The result returned by the agent is not visible to the user. To show the user the result, you should send a text message back to the user with a concise summary of the result."
	if background {
		done = "When the agent is done, its final report is not visible to the user. To show the user the result, you should send a text message back to the user with a concise summary of the result."
	}
	backgroundNotes := ""
	if background && !fork {
		backgroundNotes = "\n- Agents run in the background by default. When an agent runs in the background, you will be automatically notified when it completes — do NOT sleep, poll, or proactively check on its progress. Continue with other work or respond to the user instead.\n" +
			"- **Foreground vs background**: Pass `run_in_background: false` only when your very next action depends on the agent's result and nothing else could usefully happen while it runs — e.g., a research agent whose finding gates the edit you're about to make. Otherwise let it run in the background (the default) — this includes fire-and-forget work, independent investigations, and anything where the user might hand you something else in the meantime. Wanting the result \"next\" is not enough on its own."
	}
	raceNote := ""
	if background && !fork {
		raceNote = "\n- **Don't race**: after launching a background agent, you know nothing about its results. Never fabricate or predict them in any format — not as prose, summary, or structured output. The completion notification arrives in a later turn; it is never something you write yourself. If the user asks before it lands, say the agent is still running — give status, not a guess."
	}
	steerNotes := ""
	if steerDefault {
		steerNotes = "\n- If the agent description mentions that it should be used proactively, then you should try your best to use it without the user having to ask for it first.\n" +
			"- If the user specifies that they want you to run agents \"in parallel\", you MUST send a single message with multiple Agent tool use content blocks. For example, if you need to launch both a build-validator agent and a test-runner agent in parallel, send a single message with both tool calls."
	}
	return header + "\n" + whenNotToUse + "\n## Usage notes\n\n" +
		"- Always include a short description summarizing what the agent will do\n" +
		"- " + done + "\n" +
		"- Trust but verify: an agent's summary describes what it intended to do, not necessarily what it did. When an agent writes or edits code, check the actual changes before reporting the work as done." + backgroundNotes + raceNote + "\n" +
		"- To continue a previously spawned agent, use SendMessage with the agent's ID or name as the `to` field — that resumes it with full context. A new Agent call starts a fresh agent with no memory of prior runs" + forkResumeFull + ", so the prompt must be self-contained.\n" +
		"- Each agent type's model, reasoning effort, and tool access are set in its definition (`.claude/agents/*.md` frontmatter, or the SDK `agents` option); the `model` parameter here overrides the definition for this one call.\n" +
		"- Clearly tell the agent whether you expect it to write code or just to do research (search, file reads, web fetches, etc.), since a fresh agent is not aware of the user's intent" + steerNotes + "\n" +
		"- With `isolation: \"worktree\"`, the worktree is automatically cleaned up if the agent makes no changes; otherwise the path and branch are returned in the result." +
		forkSection + writing + "\n\n" + examples
}
