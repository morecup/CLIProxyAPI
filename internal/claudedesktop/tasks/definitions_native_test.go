package tasks

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The vectors come from the pinned SDK 2.1.247 serializer (o8e with the
// native zod schemas and prompt builders) executed by
// knowledge-kit/scripts/analysis/audit-sdk-tool-definitions-source.mjs.
type toolDefinitionsNative struct {
	SDKSHA256 string `json:"sdk_sha256"`
	Constants struct {
		Order            []string          `json:"order"`
		LegacyAliases    map[string]string `json:"legacy_aliases"`
		LeanPromptModels []string          `json:"lean_prompt_models"`
		PermissionModes  []string          `json:"permission_modes"`
	} `json:"constants"`
	ModelCases []struct {
		Model     string `json:"model"`
		Canonical string `json:"canonical"`
		Lean      bool   `json:"lean"`
	} `json:"model_cases"`
	Lanes []struct {
		Name  string `json:"name"`
		Gates struct {
			Model                   string `json:"model"`
			Teams                   bool   `json:"teams"`
			CrossSession            bool   `json:"crossSession"`
			Fork                    bool   `json:"fork"`
			BackgroundDisabled      bool   `json:"backgroundDisabled"`
			SteerDefault            bool   `json:"steerDefault"`
			Pro                     bool   `json:"pro"`
			EagerInputStreaming     bool   `json:"eagerInputStreaming"`
			GeneralPurposeAvailable bool   `json:"generalPurposeAvailable"`
			Lean                    bool   `json:"lean"`
		} `json:"gates"`
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
		ToolJSON    []string            `json:"tool_json"`
		Aliases     map[string][]string `json:"aliases"`
		ShouldDefer map[string]bool     `json:"should_defer"`
	} `json:"lanes"`
	AliasCases []struct {
		Name                  string  `json:"name"`
		Resolved              *string `json:"resolved"`
		ResolvedWithoutLegacy *string `json:"resolved_without_legacy"`
	} `json:"alias_cases"`
	Ordering struct {
		Input  []string `json:"input"`
		Sorted []string `json:"sorted"`
	} `json:"ordering"`
	DeferrableCases []struct {
		Name       string `json:"name"`
		Deferrable bool   `json:"deferrable"`
	} `json:"deferrable_cases"`
}

func loadToolDefinitionsNative(t *testing.T) toolDefinitionsNative {
	t.Helper()
	raw, err := os.ReadFile("testdata/tool-definitions-native.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors toolDefinitionsNative
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if vectors.SDKSHA256 != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" {
		t.Fatalf("vectors come from an unreviewed SDK: %s", vectors.SDKSHA256)
	}
	if len(vectors.Lanes) == 0 || len(vectors.AliasCases) == 0 || len(vectors.ModelCases) == 0 {
		t.Fatal("vectors are incomplete")
	}
	return vectors
}

func definitionContextForLane(gates struct {
	Model                   string `json:"model"`
	Teams                   bool   `json:"teams"`
	CrossSession            bool   `json:"crossSession"`
	Fork                    bool   `json:"fork"`
	BackgroundDisabled      bool   `json:"backgroundDisabled"`
	SteerDefault            bool   `json:"steerDefault"`
	Pro                     bool   `json:"pro"`
	EagerInputStreaming     bool   `json:"eagerInputStreaming"`
	GeneralPurposeAvailable bool   `json:"generalPurposeAvailable"`
	Lean                    bool   `json:"lean"`
}) DefinitionContext {
	ctx := DefinitionContext{Model: gates.Model, TeamsEnabled: gates.Teams, CrossSessionEnabled: gates.CrossSession, ForkEnabled: gates.Fork,
		BackgroundDisabled: gates.BackgroundDisabled, EagerInputStreaming: gates.EagerInputStreaming, GeneralPurposeUnavailable: !gates.GeneralPurposeAvailable,
		SubscriptionType: "max"}
	if gates.Pro {
		ctx.SubscriptionType = "pro"
	}
	if !gates.SteerDefault {
		ctx.SubagentSteer = "lean"
	}
	return ctx
}

func TestCatalogMatchesNativeSerializer(t *testing.T) {
	vectors := loadToolDefinitionsNative(t)
	for _, lane := range vectors.Lanes {
		t.Run(lane.Name, func(t *testing.T) {
			ctx := definitionContextForLane(lane.Gates)
			if got := LeanPromptModel(ctx.Model); got != lane.Gates.Lean {
				t.Fatalf("lean prompt for %s = %v, native %v", ctx.Model, got, lane.Gates.Lean)
			}
			catalog := Catalog(ctx)
			if len(catalog) != len(lane.ToolJSON) {
				t.Fatalf("catalog has %d tools, native %d", len(catalog), len(lane.ToolJSON))
			}
			for i, want := range lane.ToolJSON {
				if string(catalog[i]) == want {
					continue
				}
				var decoded struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					Schema      json.RawMessage `json:"input_schema"`
				}
				if err := json.Unmarshal(catalog[i], &decoded); err != nil {
					t.Fatalf("tool %d is not JSON: %v", i, err)
				}
				if decoded.Description != lane.Tools[i].Description {
					t.Fatalf("%s description differs at %s", lane.Tools[i].Name, firstDifference(decoded.Description, lane.Tools[i].Description))
				}
				t.Fatalf("%s serialization differs at %s", lane.Tools[i].Name, firstDifference(string(catalog[i]), want))
			}
			for i, name := range vectors.Constants.Order {
				if lane.Tools[i].Name != name {
					t.Fatalf("native order %v, lane %d is %s", vectors.Constants.Order, i, lane.Tools[i].Name)
				}
			}
		})
	}
}

func firstDifference(got, want string) string {
	limit := len(got)
	if len(want) < limit {
		limit = len(want)
	}
	for i := 0; i < limit; i++ {
		if got[i] != want[i] {
			from := i - 40
			if from < 0 {
				from = 0
			}
			return "offset " + strconv.Itoa(i) + ": got " + quoteWindow(got, from, i+80) + " want " + quoteWindow(want, from, i+80)
		}
	}
	return "length " + strconv.Itoa(len(got)) + " vs " + strconv.Itoa(len(want))
}

func quoteWindow(value string, from, to int) string {
	if to > len(value) {
		to = len(value)
	}
	return strconv.QuoteToASCII(value[from:to])
}

func TestDefinitionsDefaultLaneAndRuntimeGates(t *testing.T) {
	vectors := loadToolDefinitionsNative(t)
	var defaultLane *struct {
		Name string
		JSON []string
	}
	for _, lane := range vectors.Lanes {
		if lane.Name == "default_sonnet" {
			defaultLane = &struct {
				Name string
				JSON []string
			}{lane.Name, lane.ToolJSON}
		}
	}
	if defaultLane == nil {
		t.Fatal("default_sonnet lane missing")
	}
	// The package default advertises the non-lean pro lane; a model-aware
	// runtime without gates only changes the model.
	for i, raw := range Definitions() {
		var decoded struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &decoded)
		if decoded.Name != vectors.Constants.Order[i] {
			t.Fatalf("default lane order %d = %s", i, decoded.Name)
		}
	}
	runtime := &Runtime{options: Options{}}
	for i, raw := range runtime.Definitions("claude-sonnet-5") {
		if string(raw) != defaultLane.JSON[i] {
			t.Fatalf("runtime default lane differs at %s", firstDifference(string(raw), defaultLane.JSON[i]))
		}
	}
	gated := &Runtime{options: Options{Definitions: func(model string) DefinitionContext {
		return DefinitionContext{Model: model, EagerInputStreaming: true, SubscriptionType: "pro"}
	}}}
	for _, raw := range gated.Definitions("claude-sonnet-5") {
		if !strings.HasSuffix(string(raw), `,"eager_input_streaming":true}`) {
			t.Fatalf("gated runtime lost eager_input_streaming: %s", raw[len(raw)-60:])
		}
	}
	// An unknown subscription omits the plan paragraph, as the SDK does when
	// the OAuth account carries no subscriptionType.
	unknown := Catalog(DefinitionContext{Model: "claude-sonnet-5"})
	if strings.Contains(string(unknown[0]), "Do not spawn agents unless the user asks") {
		t.Fatal("unknown subscription must not add the pro paragraph")
	}
}

func TestCanonicalToolNameMatchesNativeLookup(t *testing.T) {
	vectors := loadToolDefinitionsNative(t)
	for _, c := range vectors.AliasCases {
		got, ok := CanonicalToolName(c.Name)
		if c.Resolved == nil {
			if ok {
				t.Fatalf("%q resolved to %s, native unresolved", c.Name, got)
			}
			continue
		}
		if !ok || got != *c.Resolved {
			t.Fatalf("%q resolved to %s/%v, native %s", c.Name, got, ok, *c.Resolved)
		}
		if c.ResolvedWithoutLegacy == nil || *c.ResolvedWithoutLegacy != *c.Resolved {
			t.Fatalf("%q legacy and alias resolution disagree natively", c.Name)
		}
	}
	for _, lane := range vectors.Lanes[:1] {
		for name, aliases := range lane.Aliases {
			for _, alias := range aliases {
				if got, ok := CanonicalToolName(alias); !ok || got != name {
					t.Fatalf("alias %s -> %s/%v, native %s", alias, got, ok, name)
				}
			}
		}
		for name, defer_ := range lane.ShouldDefer {
			if defer_ == (name == "Agent") {
				t.Fatalf("native shouldDefer for %s = %v", name, defer_)
			}
		}
	}
	if len(vectors.Ordering.Sorted) != 4 || strings.Join(vectors.Ordering.Sorted, ",") != "Agent,SendMessage,TaskOutput,TaskStop" {
		t.Fatalf("native ordering %v", vectors.Ordering.Sorted)
	}
	for _, c := range vectors.DeferrableCases {
		if c.Deferrable == (c.Name == "Agent") {
			t.Fatalf("native deferrable for %s = %v", c.Name, c.Deferrable)
		}
	}
}

func TestLeanPromptModelMatchesNativeRules(t *testing.T) {
	vectors := loadToolDefinitionsNative(t)
	for _, c := range vectors.ModelCases {
		if got := CanonicalModelID(c.Model); got != c.Canonical {
			t.Fatalf("canonical(%s) = %s, native %s", c.Model, got, c.Canonical)
		}
		if got := LeanPromptModel(c.Model); got != c.Lean {
			t.Fatalf("lean(%s) = %v, native %v", c.Model, got, c.Lean)
		}
	}
	for _, model := range vectors.Constants.LeanPromptModels {
		if !leanPromptModels[model] {
			t.Fatalf("lean capability model %s missing", model)
		}
	}
	if LeanPromptModel("") {
		t.Fatal("empty model must not take the lean lane")
	}
}

func TestExecuteToolAcceptsNativeAliases(t *testing.T) {
	runtime := newRuntime(t, Options{Execute: func(context.Context, Invocation) ([]byte, error) { return reply("done"), nil }})
	owner := caller()
	for _, alias := range []string{"KillShell", "KillBash"} {
		_, err := runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_alias", Name: alias, Input: json.RawMessage(`{"task_id":"missing"}`)})
		if err == nil || !strings.Contains(err.Error(), "No task found with ID: missing") {
			t.Fatalf("%s must dispatch to TaskStop: %v", alias, err)
		}
	}
	agentID := launch(t, runtime, owner, "toolu_task", "")
	for _, alias := range []string{"AgentOutputTool", "BashOutputTool", "AgentOutput", "BashOutput"} {
		data, err := runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_alias", Name: alias, Input: json.RawMessage(`{"task_id":"` + agentID + `","block":false}`)})
		if err != nil || !strings.Contains(string(data), `"task_id":"`+agentID+`"`) {
			t.Fatalf("%s must dispatch to TaskOutput: %s %v", alias, data, err)
		}
	}
	if _, err := runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_alias", Name: "Task", Input: json.RawMessage(`{"description":"Inspect"}`)}); err == nil || !strings.Contains(err.Error(), "Agent requires") {
		t.Fatalf("Task must dispatch to Agent validation: %v", err)
	}
	_, err := runtime.ExecuteTool(t.Context(), owner, ToolCall{ID: "toolu_alias", Name: "ListPeers", Input: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "no authorized implementation") {
		t.Fatalf("ListPeers is outside the owned family: %v", err)
	}
	result := runtime.ToolResult(ToolCall{ID: "toolu_alias", Name: "AgentOutput"}, json.RawMessage(`{"retrieval_status":"success","task":{"task_id":"a0123456789abcdef","task_type":"local_agent","status":"completed","omitOutputPath":true}}`), nil)
	if !strings.Contains(string(result), `"tool_use_id":"toolu_alias"`) || strings.Contains(string(result), `"is_error":true`) || !strings.Contains(string(result), `\u003cretrieval_status\u003esuccess\u003c/retrieval_status\u003e`) {
		t.Fatalf("AgentOutput alias must render through the TaskOutput mapper: %s", result)
	}
}
