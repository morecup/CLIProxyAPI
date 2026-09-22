package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func newClaudeDesktopTestExecutor(t *testing.T) *ClaudeExecutor {
	t.Helper()
	executor := NewClaudeExecutor(&config.Config{ClaudeDesktop: config.ClaudeDesktopConfig{StatePath: t.TempDir()}})
	executor.desktopContexts = helps.NewClaudeDesktopContextStore(t.TempDir(), "")
	if executor.desktopProfileErr != nil {
		t.Fatalf("load Desktop profile: %v", executor.desktopProfileErr)
	}
	// Drain account-owned background writes before testing removes statePath.
	t.Cleanup(executor.Close)
	return executor
}

func TestClassifyClaudeDesktopRequestRoles(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	tests := []struct {
		name string
		body string
		want claudeprofile.RequestRole
	}{
		{name: "main", body: `{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}`, want: claudeprofile.RoleMain},
		{name: "ordinary marker text", body: `{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools"}]}`, want: claudeprofile.RoleMain},
		{name: "ordinary reminder text", body: `{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"REMINDER: Do NOT call any tools. Respond with plain text only"}]}`, want: claudeprofile.RoleMain},
		{name: "light helper", body: `{"model":"claude-haiku-4-5-20251001","max_tokens":1024,"messages":[{"role":"user","content":"x"}],"thinking":{"type":"disabled"}}`, want: claudeprofile.RoleLightHelper},
		{name: "web helper", body: `{"model":"claude-haiku-4-5-20251001","max_tokens":32000,"stream":true,"tools":[{"name":"web_search","type":"web_search_20250305","max_uses":8}],"messages":[{"role":"user","content":"x"}]}`, want: claudeprofile.RoleWebSearchHelper},
		{name: "title", body: `{"model":"claude-haiku-4-5-20251001","max_tokens":32000,"stream":true,"tools":[],"output_config":{},"messages":[{"role":"user","content":"x"}]}`, want: claudeprofile.RoleTitle},
		{name: "subagent", body: `{"model":"claude-opus-5","max_tokens":4096,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_is_subagent=true;"}],"messages":[{"role":"user","content":"x"}]}`, want: claudeprofile.RoleSubagent},
		{name: "security monitor", body: `{"model":"claude-sonnet-5","max_tokens":64,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"text","text":"c"}],"messages":[{"role":"user","content":"x"}],"thinking":{"type":"disabled"},"tools":[],"stop_sequences":["done"]}`, want: claudeprofile.RoleSecurityMonitor},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := executor.classifyClaudeDesktopRequestRole([]byte(test.body)); got != test.want {
				t.Fatalf("role = %q, want %q", got, test.want)
			}
		})
	}
}

func TestApplyClaudeDesktopStreamPolicy(t *testing.T) {
	tests := []struct {
		name            string
		policy          claudeprofile.StreamPolicy
		transportStream bool
		input           string
		wantExists      bool
		wantStream      bool
	}{
		{name: "main follows non-stream transport", policy: claudeprofile.StreamPolicyTransport, input: `{"stream":true}`, wantExists: true},
		{name: "main follows stream transport", policy: claudeprofile.StreamPolicyTransport, transportStream: true, input: `{"stream":false}`, wantExists: true, wantStream: true},
		{name: "title requires stream", policy: claudeprofile.StreamPolicyRequiredTrue, input: `{}`, wantExists: true, wantStream: true},
		{name: "light helper omits false", policy: claudeprofile.StreamPolicyOmitFalse, input: `{"stream":false}`},
		{name: "light helper can follow streaming transport", policy: claudeprofile.StreamPolicyOmitFalse, transportStream: true, input: `{}`, wantExists: true, wantStream: true},
		{name: "count tokens forbids stream", policy: claudeprofile.StreamPolicyForbidden, transportStream: true, input: `{"stream":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := claudeDesktopRequestPlan{Variant: claudeprofile.RequestVariant{StreamPolicy: test.policy}}
			updated := applyClaudeDesktopStreamPolicy([]byte(test.input), plan, test.transportStream)
			stream := gjson.GetBytes(updated, "stream")
			if stream.Exists() != test.wantExists {
				t.Fatalf("stream exists = %v, want %v: %s", stream.Exists(), test.wantExists, updated)
			}
			if stream.Exists() && stream.Bool() != test.wantStream {
				t.Fatalf("stream = %v, want %v: %s", stream.Bool(), test.wantStream, updated)
			}
		})
	}
}

func TestClaudeDesktopMainRendersBundleSystemAndCarriesCallerInstructions(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	body := []byte(`{"model":"claude-opus-5","max_tokens":4096,"system":[{"type":"text","text":"caller instruction"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello world from caller"}]}],"diagnostics":{"previous_message_id":null}}`)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	facts := claudeDesktopRuntimeFacts{
		Now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111",
		PromptID: "22222222-2222-4222-8222-222222222222", ClientRequestID: "33333333-3333-4333-8333-333333333333",
		PreviousRequestID: "req_previous", LogicalModel: "claude-opus-5", WorkingDir: `C:\code`, UserHome: `C:\Users\tester`,
		MemoryDir: `C:\Users\tester\.claude\projects\C--code\memory`, ScratchpadDir: `C:\Temp\claude\session`,
	}
	updated, applied, errApply := executor.applyClaudeDesktopMessageProfile(context.Background(), nil, body, true, plan, facts)
	if errApply != nil {
		t.Fatal(errApply)
	}
	if !applied {
		t.Fatal("Desktop profile was not applied")
	}
	if got := len(gjson.GetBytes(updated, "system").Array()); got != 4 {
		t.Fatalf("system blocks = %d, want 4", got)
	}
	billing := gjson.GetBytes(updated, "system.0.text").String()
	for _, want := range []string{"cc_version=2.1.247.", "cc_entrypoint=claude-desktop", "cc_prev_req=req_previous", "cc_prompt_id=22222222-2222-4222-8222-222222222222"} {
		if !strings.Contains(billing, want) {
			t.Fatalf("billing missing %q", want)
		}
	}
	if got := gjson.GetBytes(updated, "system.2.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("intro ttl = %q, want 1h", got)
	}
	if got := gjson.GetBytes(updated, "system.2.cache_control.scope").String(); got != "global" {
		t.Fatalf("intro scope = %q, want global", got)
	}
	if !claudePayloadHasMidSystemMessage(updated) {
		t.Fatal("Opus 5 caller instruction was not carried in a mid-conversation system turn")
	}
	if strings.Contains(gjson.GetBytes(updated, "system").Raw, "caller instruction") {
		t.Fatal("caller instruction leaked into Desktop-owned top-level system")
	}
}

func TestClaudeDesktopLegacyModelUsesUserReminderCarrier(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":4096,"system":"caller instruction","messages":[{"role":"user","content":"hello"}],"diagnostics":{"previous_message_id":null}}`)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-haiku-4-5-20251001", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	facts := executor.newClaudeDesktopRuntimeFacts(nil, "session", "claude-haiku-4-5-20251001", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "", map[string]any{"working_dir": `C:\code`})
	updated, _, errApply := executor.applyClaudeDesktopMessageProfile(context.Background(), nil, body, true, plan, facts)
	if errApply != nil {
		t.Fatal(errApply)
	}
	if claudePayloadHasMidSystemMessage(updated) {
		t.Fatal("legacy Haiku request contains a mid-conversation system turn")
	}
	if !strings.Contains(gjson.GetBytes(updated, "messages.0.content.0.text").String(), "caller instruction") {
		t.Fatal("legacy Haiku caller instruction was not carried in the user reminder")
	}
}

func TestClaudeDesktopSelectsOnlyObservedCompleteBetaVariants(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	body := []byte(`{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"hello"}],"diagnostics":{"previous_message_id":null}}`)
	base, errBase := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5", nil)
	if errBase != nil {
		t.Fatal(errBase)
	}
	afk := base.Variant.AnthropicBetaVariants["afk"]
	headers := http.Header{"Anthropic-Beta": []string{strings.Join(afk, ",")}}
	selected, errSelected := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5", headers)
	if errSelected != nil {
		t.Fatal(errSelected)
	}
	if selected.BetaVariant != "afk" || selected.anthropicBeta() != strings.Join(afk, ",") {
		t.Fatalf("selected beta variant = %q", selected.BetaVariant)
	}
	fastBody := []byte(`{"model":"claude-opus-5","max_tokens":4096,"speed":"fast","messages":[{"role":"user","content":"hello"}],"diagnostics":{"previous_message_id":null}}`)
	fast, errFast := executor.planClaudeDesktopRequestWithHints(fastBody, claudeprofile.RoleMain, "claude-opus-5", nil)
	if errFast != nil || fast.BetaVariant != "fast" {
		t.Fatalf("fast variant = %q, err=%v", fast.BetaVariant, errFast)
	}
	unknownHeaders := http.Header{"Anthropic-Beta": []string{"caller-beta,another-beta"}}
	unknown, errUnknown := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5", unknownHeaders)
	if errUnknown != nil {
		t.Fatal(errUnknown)
	}
	if unknown.anthropicBeta() != strings.Join(base.Variant.AnthropicBeta, ",") {
		t.Fatal("unobserved caller beta list changed the Desktop profile")
	}
}

func TestClaudeDesktopLogicalModelSelectsObservedFallback(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":4096,"messages":[{"role":"user","content":"hello"}],"diagnostics":{"previous_message_id":null}}`)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-opus-5", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	if got := plan.Variant.System[2].Artifact; got != "intro-opus-5-sonnet-fallback" {
		t.Fatalf("fallback intro = %q", got)
	}
}

func TestClaudeDesktopRoleHeadersUseProfileTimeoutAndStripCustomHeaders(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	tests := []struct {
		name        string
		body        string
		role        claudeprofile.RequestRole
		wantTimeout string
	}{
		{name: "main", body: `{"model":"claude-opus-5","max_tokens":4096,"messages":[{"role":"user","content":"x"}]}`, role: claudeprofile.RoleMain, wantTimeout: "900"},
		{name: "security", body: `{"model":"claude-sonnet-5","max_tokens":64,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"text","text":"c"}],"messages":[{"role":"user","content":"x"}],"thinking":{"type":"disabled"},"tools":[],"stop_sequences":["done"]}`, role: claudeprofile.RoleSecurityMonitor, wantTimeout: "60"},
		{name: "count", body: `{"model":"claude-opus-5","messages":[{"role":"user","content":"x"}]}`, role: claudeprofile.RoleCountTokens, wantTimeout: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, errPlan := executor.planClaudeDesktopRequestWithHints([]byte(test.body), test.role, "", nil)
			if errPlan != nil {
				t.Fatal(errPlan)
			}
			plan.ClientRequestID = "33333333-3333-4333-8333-333333333333"
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(test.body))
			req.Header.Set("X-Unprofiled", "remove-me")
			if errHeaders := executor.applyClaudeHeadersWithProfile(req, nil, "test-token", false, nil, []byte(test.body), plan, http.Header{"X-Unprofiled": []string{"remove-me"}}, "session-id"); errHeaders != nil {
				t.Fatal(errHeaders)
			}
			if got := req.Header.Get("X-Stainless-Timeout"); got != test.wantTimeout {
				t.Fatalf("timeout = %q, want %q", got, test.wantTimeout)
			}
			if got := req.Header.Get("User-Agent"); got != executor.desktopProfile.Software.UserAgent {
				t.Fatalf("User-Agent = %q", got)
			}
			if got := req.Header.Get("X-Unprofiled"); got != "" {
				t.Fatalf("unprofiled custom header survived: %q", got)
			}
		})
	}
}

func TestClaudeDesktopCountTokensUsesModelCarrierWithoutTopLevelSystem(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	body := []byte(`{"model":"claude-opus-5","system":"caller instruction","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleCountTokens, "claude-opus-5", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	updated, _, errApply := executor.applyClaudeDesktopCountTokensProfile(body, plan)
	if errApply != nil {
		t.Fatal(errApply)
	}
	if gjson.GetBytes(updated, "system").Exists() {
		t.Fatal("count_tokens inherited a top-level system")
	}
	if gjson.GetBytes(updated, "stream").Exists() {
		t.Fatal("count_tokens inherited a stream field")
	}
	if !claudePayloadHasMidSystemMessage(updated) {
		t.Fatal("count_tokens did not use the same Opus instruction carrier")
	}
}

func TestClaudeDesktopRequestLineageUsesRequestHeaderNotMessageID(t *testing.T) {
	lineage := &claudeDesktopLineageStore{}
	auth := &cliproxyauth.Auth{ID: "desktop-auth"}
	state, previous := lineage.begin(auth, "session")
	if previous != "" {
		t.Fatalf("first previous request = %q", previous)
	}
	lineage.commit(state, "msg_not_a_request_id")
	_, previous = lineage.begin(auth, "session")
	if previous != "" {
		t.Fatalf("message id entered request lineage: %q", previous)
	}
	state, _ = lineage.begin(auth, "session")
	lineage.commit(state, "req_upstream")
	_, previous = lineage.begin(auth, "session")
	if previous != "req_upstream" {
		t.Fatalf("previous request = %q", previous)
	}
}

func TestClaudeDesktopMemoryLineageIsExecutorScoped(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "desktop-auth"}
	first := &ClaudeExecutor{}
	second := &ClaudeExecutor{}

	state, _, errBegin := first.beginClaudeDesktopRequestLineage(auth, "session")
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errCommit := first.commitClaudeDesktopRequestLineage(state, "req_first"); errCommit != nil {
		t.Fatal(errCommit)
	}
	_, previousFirst, errFirst := first.beginClaudeDesktopRequestLineage(auth, "session")
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	_, previousSecond, errSecond := second.beginClaudeDesktopRequestLineage(auth, "session")
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if previousFirst != "req_first" {
		t.Fatalf("first executor previous request = %q", previousFirst)
	}
	if previousSecond != "" {
		t.Fatalf("second executor inherited lineage %q", previousSecond)
	}
}

func TestClaudeDesktopRequestUUIDsStayStableAcrossRetryMetadata(t *testing.T) {
	metadata := map[string]any{}
	prompt1, request1 := claudeDesktopRequestUUID(metadata)
	prompt2, request2 := claudeDesktopRequestUUID(metadata)
	if prompt1 != prompt2 || request1 != request2 {
		t.Fatalf("retry UUIDs changed: %q/%q then %q/%q", prompt1, request1, prompt2, request2)
	}
}

func TestClaudeDesktopBodyProfileForcesCapturedMainShapeAndOrder(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	body := []byte(`{"stream":false,"top_p":0.5,"messages":[{"role":"user","content":"hello"}],"model":"claude-sonnet-5","max_tokens":1,"thinking":{"type":"disabled"},"tools":[]}`)
	plan, errPlan := executor.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleMain, "claude-sonnet-5", nil)
	if errPlan != nil {
		t.Fatal(errPlan)
	}
	normalized, errNormalize := executor.normalizeClaudeDesktopBody(body, plan)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}
	ordered, errOrder := executor.finalizeClaudeDesktopBody(normalized, plan)
	if errOrder != nil {
		t.Fatal(errOrder)
	}
	wantPrefix := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}],"tools":[],"max_tokens":64000,"thinking":{"type":"adaptive","display":"updates"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"output_config":{"effort":"high"},"stream":false}`
	if string(ordered) != wantPrefix {
		t.Fatalf("ordered body differs:\n%s\nwant:\n%s", ordered, wantPrefix)
	}
	if gjson.GetBytes(ordered, "top_p").Exists() {
		t.Fatal("unprofiled sampling field survived Desktop canonicalization")
	}
}

func TestClaudeDesktopBodyProfileUsesCapturedRoleOrders(t *testing.T) {
	executor := newClaudeDesktopTestExecutor(t)
	tests := []struct {
		name string
		body string
		role claudeprofile.RequestRole
		want string
	}{
		{name: "count", body: `{"tools":[],"messages":[],"model":"claude-opus-5","system":"drop"}`, role: claudeprofile.RoleCountTokens, want: `{"model":"claude-opus-5","messages":[],"tools":[]}`},
		{name: "security", body: `{"metadata":{},"messages":[],"system":[],"model":"claude-sonnet-5","thinking":{"type":"adaptive"},"max_tokens":1,"stop_sequences":["x"],"tools":[]}`, role: claudeprofile.RoleSecurityMonitor, want: `{"model":"claude-sonnet-5","max_tokens":64,"system":[],"messages":[],"stop_sequences":["x"],"thinking":{"type":"disabled"},"metadata":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, errPlan := executor.planClaudeDesktopRequestWithHints([]byte(test.body), test.role, "", nil)
			if errPlan != nil {
				t.Fatal(errPlan)
			}
			normalized, errNormalize := executor.normalizeClaudeDesktopBody([]byte(test.body), plan)
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			ordered, errOrder := executor.finalizeClaudeDesktopBody(normalized, plan)
			if errOrder != nil {
				t.Fatal(errOrder)
			}
			if string(ordered) != test.want {
				t.Fatalf("body = %s, want %s", ordered, test.want)
			}
		})
	}
}
