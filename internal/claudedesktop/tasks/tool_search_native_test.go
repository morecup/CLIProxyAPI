package tasks

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The vectors come from the pinned SDK 2.1.247 tool-search modules executed
// by knowledge-kit/scripts/analysis/audit-sdk-tool-search-source.mjs: the
// native jst()/PGr()/o8e serialization, the request-tools assembly, fA,
// fQs/uGt, qhe/XHr with the deferred_tools_delta rendering and jWe.call.
type toolSearchNative struct {
	SDKSHA256 string `json:"sdk_sha256"`
	Constants struct {
		ToolSearch                    string   `json:"tool_search"`
		Placeholder                   string   `json:"placeholder"`
		PlaceholderDescription        string   `json:"placeholder_description"`
		PlaceholderJSON               string   `json:"placeholder_json"`
		UnsupportedModelsDefault      []string `json:"unsupported_models_default"`
		AnnouncementExclusions        []string `json:"announcement_exclusions"`
		MaxResultsDefault             float64  `json:"max_results_default"`
		ListLimit                     int      `json:"list_limit"`
		NoMatchText                   string   `json:"no_match_text"`
		ReminderHeader                string   `json:"reminder_header"`
		AmbientNote                   string   `json:"ambient_note"`
		ReferencesRemovedText         string   `json:"references_removed_text"`
		ReferencesRemovedDisabledText string   `json:"references_removed_disabled_text"`
		Order                         []string `json:"order"`
	} `json:"constants"`
	Gates struct {
		ModeDefault                  string   `json:"mode_default"`
		RegisteredDefault            bool     `json:"registered_default"`
		PlaceholderDefault           bool     `json:"placeholder_default"`
		FetchRuleDefault             bool     `json:"fetch_rule_default"`
		NonDeferrableBuiltinsDefault []string `json:"non_deferrable_builtins_default"`
	} `json:"gates"`
	Prompts struct {
		FetchRuleOff string `json:"fetch_rule_off"`
		FetchRuleOn  string `json:"fetch_rule_on"`
	} `json:"prompts"`
	Lanes []struct {
		Name  string `json:"name"`
		Gates struct {
			Model               string `json:"model"`
			EagerInputStreaming bool   `json:"eagerInputStreaming"`
			FetchRule           bool   `json:"fetchRule"`
			StubDisabled        bool   `json:"stubDisabled"`
			DefinitionsLane     string `json:"definitions_lane"`
		} `json:"gates"`
		ToolSearchJSON  string  `json:"tool_search_json"`
		PlaceholderJSON *string `json:"placeholder_json"`
		RequestTools    []struct {
			Discovered []string `json:"discovered"`
			Enabled    bool     `json:"enabled"`
			Names      []string `json:"names"`
			ToolJSON   []string `json:"tool_json"`
		} `json:"request_tools"`
	} `json:"lanes"`
	DiscoveryCases []struct {
		Label      string          `json:"label"`
		Messages   []nativeMessage `json:"messages"`
		Discovered []string        `json:"discovered"`
	} `json:"discovery_cases"`
	SearchCases []struct {
		Query              string          `json:"query"`
		MaxResults         float64         `json:"max_results"`
		Matches            []string        `json:"matches"`
		TotalDeferredTools int             `json:"total_deferred_tools"`
		QueryType          string          `json:"query_type"`
		Data               json.RawMessage `json:"data"`
		ResultBlock        json.RawMessage `json:"result_block"`
	} `json:"search_cases"`
	ReminderCases []struct {
		Label      string          `json:"label"`
		Model      string          `json:"model"`
		PriorNames []string        `json:"prior_names"`
		Attachment json.RawMessage `json:"attachment"`
		Text       *string         `json:"text"`
	} `json:"reminder_cases"`
	ReferenceFilterCases []struct {
		Label    string        `json:"label"`
		Row      nativeMessage `json:"row"`
		Enabled  nativeMessage `json:"enabled"`
		Disabled nativeMessage `json:"disabled"`
	} `json:"reference_filter_cases"`
	ModelSupportCases []struct {
		Model     string `json:"model"`
		Supported bool   `json:"supported"`
	} `json:"model_support_cases"`
}

// nativeMessage is an internal SDK row; message carries the API-shaped row.
type nativeMessage struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`
}

func loadToolSearchNative(t *testing.T) toolSearchNative {
	t.Helper()
	raw, err := os.ReadFile("testdata/tool-search-native.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors toolSearchNative
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if vectors.SDKSHA256 != "00e5be0a8b69893cad9259a1e8b80d59be8f3eb367d4a16c19f91bcd279423b7" {
		t.Fatalf("vectors come from an unreviewed SDK: %s", vectors.SDKSHA256)
	}
	if len(vectors.Lanes) == 0 || len(vectors.SearchCases) == 0 || len(vectors.ReminderCases) == 0 || len(vectors.ReferenceFilterCases) == 0 || len(vectors.DiscoveryCases) == 0 || len(vectors.ModelSupportCases) == 0 {
		t.Fatal("vectors are incomplete")
	}
	return vectors
}

// wireRows converts internal SDK rows to the API rows this runtime sends;
// rows without a message (compact boundaries) have no wire form.
func wireRows(t *testing.T, rows []nativeMessage) ([]json.RawMessage, bool) {
	t.Helper()
	var result []json.RawMessage
	complete := true
	for _, row := range rows {
		if len(row.Message) == 0 || string(row.Message) == "null" {
			complete = false
			continue
		}
		result = append(result, compactJSON(t, row.Message))
	}
	return result, complete
}

func nativeUserRow(content string) json.RawMessage {
	return json.RawMessage(`{"role":"user","content":` + content + `}`)
}

func nativeDiscoveryRows(discovered []string) []json.RawMessage {
	rows := []json.RawMessage{nativeUserRow(`"hello"`)}
	if len(discovered) > 0 {
		rows = append(rows, referenceRow(discovered...))
	}
	return rows
}

// toolSearchContextForLane maps a golden lane onto the DefinitionContext:
// the §49 definitions lane supplies the owned tool gates, the tool-search
// lane its model, streaming, fetch-rule and stub gates.
func toolSearchContextForLane(t *testing.T, definitions toolDefinitionsNative, lane string, model string, eager, fetchRule, stubDisabled bool) DefinitionContext {
	t.Helper()
	for _, candidate := range definitions.Lanes {
		if candidate.Name != lane {
			continue
		}
		ctx := definitionContextForLane(candidate.Gates)
		ctx.Model, ctx.EagerInputStreaming, ctx.ToolSearchFetchRule, ctx.DeferredStubDisabled = model, eager, fetchRule, stubDisabled
		return ctx
	}
	t.Fatalf("definitions lane %s missing from the §49 golden", lane)
	return DefinitionContext{}
}

func TestToolSearchConstantsMatchNative(t *testing.T) {
	vectors := loadToolSearchNative(t)
	c := vectors.Constants
	if c.ToolSearch != ToolSearchName || c.Placeholder != DeferredToolPlaceholderName || c.PlaceholderDescription != placeholderDescription {
		t.Fatalf("names: %+v", c)
	}
	if c.PlaceholderJSON != string(PlaceholderDefinition()) {
		t.Fatalf("placeholder differs at %s", firstDifference(string(PlaceholderDefinition()), c.PlaceholderJSON))
	}
	if !reflect.DeepEqual(c.UnsupportedModelsDefault, defaultToolSearchUnsupportedModels) {
		t.Fatalf("unsupported models default %v", c.UnsupportedModelsDefault)
	}
	exclusions := make([]string, 0, len(announcementExclusions))
	for name := range announcementExclusions {
		exclusions = append(exclusions, name)
	}
	sort.Strings(exclusions)
	native := append([]string(nil), c.AnnouncementExclusions...)
	sort.Strings(native)
	if !reflect.DeepEqual(exclusions, native) {
		t.Fatalf("announcement exclusions %v, native %v", exclusions, native)
	}
	if c.MaxResultsDefault != 5 || c.ListLimit != deferredToolsListLimit || c.NoMatchText != noMatchingDeferredTools {
		t.Fatalf("limits/texts: %+v", c)
	}
	if c.ReminderHeader != deferredToolsHeader {
		t.Fatalf("reminder header differs at %s", firstDifference(deferredToolsHeader, c.ReminderHeader))
	}
	if c.AmbientNote != deferredToolsAmbientNote {
		t.Fatalf("ambient note differs at %s", firstDifference(deferredToolsAmbientNote, c.AmbientNote))
	}
	if c.ReferencesRemovedText != toolReferencesRemoved || c.ReferencesRemovedDisabledText != toolReferencesDisabled {
		t.Fatalf("replacement texts: %+v", c)
	}
	names := []string{DeferredToolPlaceholderName}
	for _, tool := range toolPool {
		names = append(names, tool.name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, c.Order) {
		t.Fatalf("pool order %v, native %v", names, c.Order)
	}
	g := vectors.Gates
	if g.ModeDefault != "tst" || !g.RegisteredDefault || !g.PlaceholderDefault || g.FetchRuleDefault || len(g.NonDeferrableBuiltinsDefault) != 0 {
		t.Fatalf("Desktop defaults: %+v", g)
	}
	if got := toolSearchPrompt(DefinitionContext{}); got != vectors.Prompts.FetchRuleOff {
		t.Fatalf("prompt differs at %s", firstDifference(got, vectors.Prompts.FetchRuleOff))
	}
	if got := toolSearchPrompt(DefinitionContext{ToolSearchFetchRule: true}); got != vectors.Prompts.FetchRuleOn {
		t.Fatalf("fetch-rule prompt differs at %s", firstDifference(got, vectors.Prompts.FetchRuleOn))
	}
	for _, c := range vectors.ModelSupportCases {
		if got := toolSearchModelSupported(DefinitionContext{Model: c.Model}); got != c.Supported {
			t.Fatalf("model %s supported = %v, native %v", c.Model, got, c.Supported)
		}
	}
}

func TestRequestToolsMatchNativeAssembly(t *testing.T) {
	vectors := loadToolSearchNative(t)
	definitions := loadToolDefinitionsNative(t)
	for _, lane := range vectors.Lanes {
		t.Run(lane.Name, func(t *testing.T) {
			ctx := toolSearchContextForLane(t, definitions, lane.Gates.DefinitionsLane, lane.Gates.Model, lane.Gates.EagerInputStreaming, lane.Gates.FetchRule, lane.Gates.StubDisabled)
			if got := string(ToolSearchDefinition(ctx)); got != lane.ToolSearchJSON {
				t.Fatalf("ToolSearch differs at %s", firstDifference(got, lane.ToolSearchJSON))
			}
			if lane.PlaceholderJSON == nil {
				if !lane.Gates.StubDisabled {
					t.Fatal("native dropped the placeholder without the stub gate")
				}
			} else if got := string(PlaceholderDefinition()); got != *lane.PlaceholderJSON {
				t.Fatalf("placeholder differs at %s", firstDifference(got, *lane.PlaceholderJSON))
			}
			if len(lane.RequestTools) == 0 {
				t.Fatal("lane has no request_tools states")
			}
			for _, state := range lane.RequestTools {
				if got := ToolSearchEnabled(ctx); got != state.Enabled {
					t.Fatalf("enabled = %v, native %v", got, state.Enabled)
				}
				tools := RequestTools(ctx, nativeDiscoveryRows(state.Discovered))
				if len(tools) != len(state.ToolJSON) {
					t.Fatalf("discovered %v: %d tools (%v), native %d (%v)", state.Discovered, len(tools), toolNames(t, tools), len(state.ToolJSON), state.Names)
				}
				for i, want := range state.ToolJSON {
					if string(tools[i]) != want {
						t.Fatalf("discovered %v: tool %d (%s) differs at %s", state.Discovered, i, state.Names[i], firstDifference(string(tools[i]), want))
					}
				}
			}
		})
	}
}

func TestDiscoveredToolsMatchNative(t *testing.T) {
	vectors := loadToolSearchNative(t)
	for _, c := range vectors.DiscoveryCases {
		rows, complete := wireRows(t, c.Messages)
		if !complete {
			// Compact boundaries carry preCompactDiscoveredTools natively; the
			// owned wire history has no such rows (recorded boundary).
			continue
		}
		var got []string
		for name := range DiscoveredTools(rows) {
			got = append(got, name)
		}
		sort.Strings(got)
		want := append([]string(nil), c.Discovered...)
		sort.Strings(want)
		if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("%s: discovered %v, native %v", c.Label, got, want)
		}
	}
}

func TestToolSearchCallMatchesNative(t *testing.T) {
	vectors := loadToolSearchNative(t)
	definitions := loadToolDefinitionsNative(t)
	ctx := toolSearchContextForLane(t, definitions, "default_sonnet", "claude-sonnet-5", false, false, false)
	runtime := &Runtime{options: Options{Definitions: func(string) DefinitionContext { return ctx }}}
	for _, c := range vectors.SearchCases {
		input, _ := json.Marshal(map[string]any{"query": c.Query, "max_results": c.MaxResults})
		data, err := runtime.ExecuteTool(t.Context(), Caller{PromptID: "00000000-0000-4000-8000-000000000000", Model: "claude-sonnet-5"}, ToolCall{ID: "toolu_x", Name: ToolSearchName, Input: input})
		if err != nil {
			t.Fatalf("%q: %v", c.Query, err)
		}
		if want := string(compactJSON(t, c.Data)); string(data) != want {
			t.Fatalf("%q: data %s, native %s", c.Query, data, want)
		}
		var decoded struct {
			Matches []string `json:"matches"`
			Total   int      `json:"total_deferred_tools"`
		}
		if json.Unmarshal(data, &decoded) != nil || decoded.Total != c.TotalDeferredTools || len(decoded.Matches) != len(c.Matches) || (len(c.Matches) > 0 && !reflect.DeepEqual(decoded.Matches, c.Matches)) {
			t.Fatalf("%q: matches %v, native %v (%s)", c.Query, decoded.Matches, c.Matches, c.QueryType)
		}
		if isSelect := strings.HasPrefix(strings.ToLower(c.Query), "select:"); isSelect != (c.QueryType == "select") {
			t.Fatalf("%q: native query type %s", c.Query, c.QueryType)
		}
		result := runtime.ToolResult(ToolCall{ID: "toolu_x", Name: ToolSearchName}, data, nil)
		var got, want any
		if json.Unmarshal(result, &got) != nil || json.Unmarshal(c.ResultBlock, &want) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: tool_result %s, native %s", c.Query, result, compactJSON(t, c.ResultBlock))
		}
		if len(c.Matches) > 0 {
			native := string(compactJSON(t, c.ResultBlock))
			if !strings.Contains(native, `{"type":"tool_reference","tool_name":"`) || !strings.Contains(string(result), `{"type":"tool_reference","tool_name":"`) {
				t.Fatalf("%q: tool_reference member order %s, native %s", c.Query, result, native)
			}
		}
	}
}

func TestDeferredToolsReminderMatchesNative(t *testing.T) {
	vectors := loadToolSearchNative(t)
	for _, c := range vectors.ReminderCases {
		ctx := DefinitionContext{Model: c.Model, SubscriptionType: "pro"}
		rows := []json.RawMessage{nativeUserRow(`"hello"`)}
		if len(c.PriorNames) > 0 {
			// The prior attachment reached the wire as a reminder text row.
			prior, _ := json.Marshal("<system-reminder>\n" + deferredToolsHeader + "\n" + strings.Join(c.PriorNames, "\n") + "\n</system-reminder>")
			rows = append(rows, nativeUserRow(string(prior)), json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"ok"}]}`), nativeUserRow(`"next"`))
		}
		got := DeferredToolsReminder(ctx, rows)
		if c.Text == nil {
			if string(c.Attachment) != "null" {
				t.Fatalf("%s: native attachment without text: %s", c.Label, c.Attachment)
			}
			if got != "" {
				t.Fatalf("%s: reminder %q, native none", c.Label, got)
			}
			continue
		}
		if got != *c.Text {
			t.Fatalf("%s: reminder differs at %s", c.Label, firstDifference(got, *c.Text))
		}
		// The rendered text is what a merged user row carries; a second pass
		// over that history announces (or removes) nothing new.
		text, _ := json.Marshal(*c.Text)
		if again := DeferredToolsReminder(ctx, append(rows, nativeUserRow(`[{"type":"text","text":"more"},{"type":"text","text":`+string(text)+`}]`))); again != "" {
			t.Fatalf("%s: reminder repeated: %q", c.Label, again)
		}
	}
}

func TestFilterToolReferencesMatchesNative(t *testing.T) {
	vectors := loadToolSearchNative(t)
	enabled := DefinitionContext{Model: "claude-sonnet-5", SubscriptionType: "pro"}
	disabled := DefinitionContext{Model: "claude-3-5-haiku-20241022", SubscriptionType: "pro"}
	if !ToolSearchEnabled(enabled) || ToolSearchEnabled(disabled) {
		t.Fatal("filter lanes must differ in the enable decision")
	}
	for _, c := range vectors.ReferenceFilterCases {
		row := compactJSON(t, c.Row.Message)
		for _, variant := range []struct {
			name string
			ctx  DefinitionContext
			want nativeMessage
		}{{"enabled", enabled, c.Enabled}, {"disabled", disabled, c.Disabled}} {
			got := string(FilterToolReferences(variant.ctx, []json.RawMessage{row})[0])
			if want := string(compactJSON(t, variant.want.Message)); got != want {
				t.Fatalf("%s/%s: row differs at %s", c.Label, variant.name, firstDifference(got, want))
			}
		}
	}
}
