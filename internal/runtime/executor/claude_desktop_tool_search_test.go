package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// deferredToolsReminderText is the native deferred_tools_delta rendering for
// the static owned pool: header line, then one deferrable name per line.
const deferredToolsReminderText = "<system-reminder>\nThe following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — calling them directly will fail with InputValidationError. Use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\nSendMessage\nTaskOutput\nTaskStop\n</system-reminder>"

func toolNames(body []byte) []string {
	var names []string
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		names = append(names, tool.Get("name").String())
	}
	return names
}

// deferredReminderBlocks counts the announcement blocks in every user row and
// returns the index of the row holding the last one.
func deferredReminderBlocks(body []byte) (count, row int) {
	row = -1
	for i, message := range gjson.GetBytes(body, "messages").Array() {
		if message.Get("role").String() != "user" {
			continue
		}
		for _, block := range message.Get("content").Array() {
			if block.Get("type").String() == "text" && block.Get("text").String() == deferredToolsReminderText {
				count++
				row = i
			}
		}
	}
	return count, row
}

// isToolReferenceResult checks a ToolSearch tool_result content: exactly the
// given names as tool_reference blocks, in order.
func isToolReferenceResult(content gjson.Result, names ...string) bool {
	blocks := content.Array()
	if !content.IsArray() || len(blocks) != len(names) {
		return false
	}
	for i, block := range blocks {
		if block.Get("type").String() != "tool_reference" || block.Get("tool_name").String() != names[i] || len(block.Map()) != 2 {
			return false
		}
	}
	return true
}

func assertOwnedToolBytes(t *testing.T, body []byte, index int, want json.RawMessage) {
	t.Helper()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "tools").Array(); index >= len(got) || got[index].Raw != string(encoded) {
		t.Fatalf("tool %d is not the native definition: %s", index, gjson.GetBytes(body, "tools").Raw)
	}
}

// sdkDeliveredEvent is one SDK event-logging event as delivered to the
// synthetic endpoint: the event name and its decoded additional_metadata.
type sdkDeliveredEvent struct {
	name   string
	fields map[string]any
}

// sdkDeliveredEventStream returns the named SDK events in delivery order. The
// queue is FIFO per account, so the order of the delivered batches and of the
// events inside each batch is the emission order. Every kept event must belong
// to the owned SDK session and model.
func sdkDeliveredEventStream(t *testing.T, doer *claudeDesktopTelemetryTestDoer, session, model string, names ...string) []sdkDeliveredEvent {
	t.Helper()
	keep := make(map[string]bool, len(names))
	for _, name := range names {
		keep[name] = true
	}
	var stream []sdkDeliveredEvent
	for _, request := range doer.Requests() {
		for _, event := range gjson.GetBytes(request.Body, "events").Array() {
			data := event.Get("event_data")
			name := data.Get("event_name").String()
			if !keep[name] {
				continue
			}
			if data.Get("session_id").String() != session || data.Get("model").String() != model {
				t.Fatalf("%s left the owned SDK session or model: %s", name, data.Raw)
			}
			metadata, err := base64.StdEncoding.DecodeString(data.Get("additional_metadata").String())
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(metadata, &fields); err != nil {
				t.Fatal(err)
			}
			stream = append(stream, sdkDeliveredEvent{name: name, fields: fields})
		}
	}
	return stream
}

// assertSDKEventFields compares the decoded metadata against want: numbers
// are float64, absent keys are asserted with a nil value.
func assertSDKEventFields(t *testing.T, event sdkDeliveredEvent, want map[string]any) {
	t.Helper()
	for key, value := range want {
		got, present := event.fields[key]
		if value == nil {
			if present {
				t.Fatalf("%s carries %s=%v, want absent: %v", event.name, key, got, event.fields)
			}
			continue
		}
		if !present || got != value {
			t.Fatalf("%s %s=%v (present=%t), want %v: %v", event.name, key, got, present, value, event.fields)
		}
	}
	if prompt, _ := event.fields["cc_prompt_id"].(string); prompt == "" {
		t.Fatalf("%s lost the prompt identity: %v", event.name, event.fields)
	}
}

// This enters the real worker stream, account router, role planner and Messages
// transport with a synthetic model peer. Owned main turns and the child
// generation must advertise the deferred tool shape, announce the deferrable
// names exactly once per history, execute ToolSearch through the task runtime
// and grow the tools as tool_reference results discover them. The SDK
// event-logging deliveries are recorded so the native tool-search telemetry of
// these owned requests can be asserted end to end in emission order.
func TestClaudeDesktopOwnedRequestsDeferToolSearch(t *testing.T) {
	// Force the native 5% attachment-duration sample so its position and
	// values can be asserted in (e) (claude_desktop_query_context_telemetry.go).
	claudeDesktopAttachmentComputeDraw = func() float64 { return 0.01 }
	t.Cleanup(func() { claudeDesktopAttachmentComputeDraw = nil })
	sdkEvents := &claudeDesktopTelemetryTestDoer{}
	e, auths := newExecutionSessionAccountTest(t, func(_ string, role string, _ *cliproxyauth.Auth) claudetelemetry.HTTPDoer {
		if role == "sdk-event-logging" {
			return sdkEvents
		}
		return claudetelemetry.HTTPDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Proto: "HTTP/2.0", ProtoMajor: 2, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})
	})
	auth := auths[0]
	manager := cliproxyauth.NewManager(nil, nil, nil)
	failures := make(chan error, 16)
	manager.RegisterExecutor(&agentObservedExecutor{e, failures})
	e.credentialManager = manager
	auth.Metadata["access_token"] = auth.Attributes[cliproxyauth.AttributeAPIKey]
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	runtime := accountRuntimeForAuth(t, e, auth.ID)
	inner := runtime.executor
	defer func() {
		e.Close()
		deadline := time.After(10 * time.Second)
		for !runtime.isClosed() {
			select {
			case <-deadline:
				t.Error("account did not join Agent work")
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	inner.desktopControlPlane.Close()
	writers := make(chan *io.PipeWriter, 4)
	inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile,
		DoerFactory: func(_ context.Context, _ string, current *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
			if current.ID != auth.ID {
				return nil, errors.New("foreign account")
			}
			return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
				body, status := `{}`, 200
				switch {
				case strings.HasSuffix(request.URL.Path, "/unarchive"):
					status = 409
				case strings.HasSuffix(request.URL.Path, "/bridge"):
					body = `{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"1","worker_jwt":"synthetic-worker"}`
				case strings.HasSuffix(request.URL.Path, "/worker/events/stream"):
					reader, writer := io.Pipe()
					writers <- writer
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &remoteExecutorPipe{reader, writer}}, nil
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}), nil
		}})
	type observation struct {
		body  []byte
		owner claudeDesktopQueryContext
	}
	requests := make(chan observation, 24)
	var mainCalls, childCalls atomic.Int32
	setup := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/messages" {
			return executionSessionTestResponse(t, request), nil
		}
		reader, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			return nil, err
		}
		owner, ok := request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if !ok {
			return nil, errors.New("missing request owner")
		}
		requests <- observation{body, owner}
		content, stop := `[{"type":"text","text":"actual parent result"}]`, "end_turn"
		if owner.agentID != "" {
			switch childCalls.Add(1) {
			case 1:
				content, stop = `[{"type":"tool_use","id":"tool_child_search","name":"ToolSearch","input":{"query":"select:TaskStop"}}]`, "tool_use"
			default:
				content = `[{"type":"text","text":"actual child generation 2"}]`
			}
		} else {
			switch mainCalls.Add(1) {
			case 1:
				content, stop = `[{"type":"tool_use","id":"tool_search_select","name":"ToolSearch","input":{"query":"select:SendMessage"}}]`, "tool_use"
			case 2:
				content, stop = `[{"type":"tool_use","id":"tool_search_keyword","name":"ToolSearch","input":{"query":"nothing here"}}]`, "tool_use"
			case 3:
				content, stop = `[{"type":"tool_use","id":"tool_owned_agent","name":"Agent","input":{"description":"inspect","prompt":"inspect child state","run_in_background":false}}]`, "tool_use"
			}
		}
		payload := agentCanonicalSSE(t, content)
		payload = strings.Replace(payload, `"message":{"id"`, `"message":{"type":"message","content":[],"id"`, 1)
		payload = strings.ReplaceAll(payload, `"stop_reason":"tool_use"`, `"stop_reason":"`+stop+`"`)
		payload = strings.ReplaceAll(payload, `"model":"claude-opus-5"`, `"model":"`+gjson.GetBytes(body, "model").String()+`"`)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {"req_owned_search"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
	})))
	value, err := e.StartDesktopRemoteSession(setup, auth.ID, cliproxyexecutor.ClaudeDesktopRemoteStart{RemoteSessionID: "session_toolSearch", Folder: `C:\synthetic`, Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	writer := awaitRemoteExecutor(t, writers)
	inputDone := make(chan error, 1)
	go func() {
		_, err := fmt.Fprint(writer, "id: 1\nevent: client_event\ndata: {\"event_id\":\"owned-input\",\"event_type\":\"user\",\"payload\":{\"type\":\"user\",\"uuid\":\"human-input\",\"message\":{\"role\":\"user\",\"content\":\"delegate now\"}}}\n\n")
		inputDone <- err
	}()
	if err := awaitRemoteExecutor(t, inputDone); err != nil {
		t.Fatal(err)
	}
	next := func(agent bool) observation {
		t.Helper()
		select {
		case request := <-requests:
			if (request.owner.agentID != "") != agent || request.owner.accountID != auth.ID || request.owner.host.ID() != value.QueryID {
				t.Fatalf("request owner: agent=%q account=%t host=%t", request.owner.agentID, request.owner.accountID == auth.ID, request.owner.host.ID() == value.QueryID)
			}
			return request
		case err := <-failures:
			t.Fatal("actual executor rejected a request", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("request missing; control=%+v", inner.desktopControlPlane.Status())
		}
		return observation{}
	}
	// (a) Nothing discovered: the deferrable names are announced after the
	// prompt and only the placeholder and ToolSearch join Agent.
	first := next(false)
	mainContext := inner.desktopAgentDefinitionContext(auth, first.owner.host, "claude-opus-5")
	if got := toolNames(first.body); strings.Join(got, ",") != "Agent,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("first owned request tools %v", got)
	}
	assertOwnedToolBytes(t, first.body, 1, claudetasks.PlaceholderDefinition())
	assertOwnedToolBytes(t, first.body, 2, claudetasks.ToolSearchDefinition(mainContext))
	assertNativeToolCatalog(t, first.body, mainContext)
	rows := gjson.GetBytes(first.body, "messages").Array()
	last := rows[len(rows)-1]
	blocks := last.Get("content").Array()
	if last.Get("role").String() != "user" || !last.Get("content").IsArray() || len(blocks) != 2 ||
		blocks[0].Get("type").String() != "text" || blocks[0].Get("text").String() != "delegate now" ||
		blocks[1].Get("type").String() != "text" || blocks[1].Get("text").String() != deferredToolsReminderText {
		t.Fatalf("first owned request did not merge the reminder after the prompt: %s", last.Raw)
	}
	if count, _ := deferredReminderBlocks(first.body); count != 1 {
		t.Fatalf("first owned request announced %d times", count)
	}

	// (b) select:SendMessage returns one tool_reference and the next request
	// carries SendMessage deferred, without a second announcement.
	second := next(false)
	rows = gjson.GetBytes(second.body, "messages").Array()
	last = rows[len(rows)-1]
	result := last.Get("content.0")
	if last.Get("role").String() != "user" || result.Get("type").String() != "tool_result" || result.Get("tool_use_id").String() != "tool_search_select" ||
		result.Get("is_error").Exists() || !isToolReferenceResult(result.Get("content"), "SendMessage") {
		t.Fatalf("ToolSearch select result: %s", last.Raw)
	}
	if got := toolNames(second.body); strings.Join(got, ",") != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("discovered request tools %v", got)
	}
	if raw := gjson.GetBytes(second.body, "tools.1").Raw; !strings.HasSuffix(raw, `,"defer_loading":true}`) {
		t.Fatalf("discovered SendMessage is not deferred: %s", raw[len(raw)-80:])
	}
	assertOwnedToolBytes(t, second.body, 2, claudetasks.PlaceholderDefinition())
	assertOwnedToolBytes(t, second.body, 3, claudetasks.ToolSearchDefinition(mainContext))
	assertNativeToolCatalog(t, second.body, mainContext)
	if count, row := deferredReminderBlocks(second.body); count != 1 || row != 0 {
		t.Fatalf("second owned request announced %d times (row %d)", count, row)
	}

	// (c) A keyword query without a hit yields the native plain-string result;
	// the discovered set is unchanged.
	third := next(false)
	rows = gjson.GetBytes(third.body, "messages").Array()
	result = rows[len(rows)-1].Get("content.0")
	if result.Get("tool_use_id").String() != "tool_search_keyword" || result.Get("content").Type != gjson.String || result.Get("content").String() != "No matching deferred tools found" || result.Get("is_error").Exists() {
		t.Fatalf("ToolSearch keyword miss: %s", rows[len(rows)-1].Raw)
	}
	if got := toolNames(third.body); strings.Join(got, ",") != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("third owned request tools %v", got)
	}
	assertNativeToolCatalog(t, third.body, mainContext)

	// (d) The foreground child announces the names after its own prompt and
	// discovers TaskStop; the announcement stays in its first row.
	child := next(true)
	childContext := inner.desktopAgentDefinitionContext(auth, child.owner.host, "claude-opus-5")
	if gjson.GetBytes(child.body, "model").String() != "claude-opus-5" || !strings.Contains(gjson.GetBytes(child.body, "system.0.text").String(), "cc_is_subagent=true") {
		t.Fatalf("child plan: model=%q system=%s", gjson.GetBytes(child.body, "model").String(), gjson.GetBytes(child.body, "system.0.text").String())
	}
	if got := toolNames(child.body); strings.Join(got, ",") != "Agent,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("child request tools %v", got)
	}
	assertOwnedToolBytes(t, child.body, 1, claudetasks.PlaceholderDefinition())
	assertOwnedToolBytes(t, child.body, 2, claudetasks.ToolSearchDefinition(childContext))
	assertNativeToolCatalog(t, child.body, childContext)
	rows = gjson.GetBytes(child.body, "messages").Array()
	blocks = rows[len(rows)-1].Get("content").Array()
	if len(rows) != 1 || rows[0].Get("role").String() != "user" || len(blocks) != 2 || blocks[0].Get("text").String() != "inspect child state" || blocks[1].Get("text").String() != deferredToolsReminderText {
		t.Fatalf("child prompt did not merge the reminder: %s", gjson.GetBytes(child.body, "messages").Raw)
	}
	childNext := next(true)
	if childNext.owner.agentID != child.owner.agentID {
		t.Fatal("child continuation changed agent")
	}
	rows = gjson.GetBytes(childNext.body, "messages").Array()
	result = rows[len(rows)-1].Get("content.0")
	if len(rows) != 3 || result.Get("tool_use_id").String() != "tool_child_search" || !isToolReferenceResult(result.Get("content"), "TaskStop") {
		t.Fatalf("child ToolSearch result: %s", gjson.GetBytes(childNext.body, "messages").Raw)
	}
	if count, row := deferredReminderBlocks(childNext.body); count != 1 || row != 0 || rows[0].Raw != gjson.GetBytes(child.body, "messages.0").Raw {
		t.Fatalf("child continuation lost or repeated the announcement (count %d, row %d): %s", count, row, gjson.GetBytes(childNext.body, "messages").Raw)
	}
	if got := toolNames(childNext.body); strings.Join(got, ",") != "Agent,TaskStop,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("child continuation tools %v", got)
	}
	if raw := gjson.GetBytes(childNext.body, "tools.1").Raw; !strings.HasSuffix(raw, `,"defer_loading":true}`) {
		t.Fatalf("discovered TaskStop is not deferred: %s", raw[len(raw)-80:])
	}
	assertNativeToolCatalog(t, childNext.body, childContext)

	// The main continuation consumes the actual child result with the same
	// discovered set and the single announcement.
	continuation := next(false)
	if !strings.Contains(string(continuation.body), "actual child generation 2") {
		t.Fatal("foreground continuation used invented output")
	}
	if got := toolNames(continuation.body); strings.Join(got, ",") != "Agent,SendMessage,DeferredToolPlaceholder,ToolSearch" {
		t.Fatalf("continuation tools %v", got)
	}
	if count, row := deferredReminderBlocks(continuation.body); count != 1 || row != 0 {
		t.Fatalf("continuation announced %d times (row %d)", count, row)
	}
	assertNativeToolCatalog(t, continuation.body, mainContext)
	runtime.mu.Lock()
	actor := runtime.remoteInputs[value.QueryID]
	runtime.mu.Unlock()
	if actor == nil {
		t.Fatal("actual input actor missing")
	}
	deadline := time.After(10 * time.Second)
	for {
		states, err := actor.AgentTasks()
		if err != nil {
			t.Fatal(err)
		}
		if len(states) == 1 && states[0].Status == "completed" && states[0].PendingEvents == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child task did not finish", states)
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("unexpected extra request: %s", gjson.GetBytes(request.body, "messages").Raw)
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(20 * time.Millisecond):
	}
	if mainCalls.Load() != 4 || childCalls.Load() != 2 {
		t.Fatalf("model calls main=%d child=%d", mainCalls.Load(), childCalls.Load())
	}

	// (e) The delivered SDK event stream replays the native tool-search
	// telemetry of every owned request in emission order: the j1e decision
	// and (for a request carrying a new reminder) the XHr pool change precede
	// the tengu_api_query of that request; the ToolSearch outcome follows the
	// request whose tool_use produced it and precedes the next request.
	if err := inner.desktopTelemetry.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	const (
		decisionEvent   = "tengu_tool_search_mode_decision"
		poolChangeEvent = "tengu_deferred_tools_pool_change"
		outcomeEvent    = "tengu_tool_search_outcome"
		queryEvent      = "tengu_api_query"
	)
	stream := sdkDeliveredEventStream(t, sdkEvents, value.SDKSessionID, "claude-opus-5", decisionEvent, poolChangeEvent, outcomeEvent, queryEvent)
	wantOrder := []string{
		decisionEvent, poolChangeEvent, queryEvent, // main turn 1: nothing discovered, first announcement
		outcomeEvent,              // select:SendMessage
		decisionEvent, queryEvent, // main turn 2: SendMessage discovered, no new announcement
		outcomeEvent,              // keyword miss
		decisionEvent, queryEvent, // main turn 3: Agent tool_use
		decisionEvent, poolChangeEvent, queryEvent, // child generation 1: its own first announcement
		outcomeEvent,              // select:TaskStop
		decisionEvent, queryEvent, // child generation 2
		decisionEvent, queryEvent, // main continuation with the child result
	}
	gotOrder := make([]string, 0, len(stream))
	for _, event := range stream {
		gotOrder = append(gotOrder, event.name)
	}
	if strings.Join(gotOrder, "\n") != strings.Join(wantOrder, "\n") {
		t.Fatalf("owned tool-search telemetry order:\n%s\nwant:\n%s", strings.Join(gotOrder, "\n"), strings.Join(wantOrder, "\n"))
	}
	// The sampled tengu_attachment_compute_duration of every owned request
	// build sits between tengu_query_before_attachments and tengu_attachments
	// on continuation turns and before the tool-search decision on first turns.
	assertClaudeDesktopAttachmentComputeDurationStream(t, sdkEvents, value.SDKSessionID, "claude-opus-5", 6)
	enabled := map[string]any{"enabled": true, "mode": "tst", "reason": "tst_enabled", "checkedModel": "claude-opus-5", "mcpToolCount": float64(0), "mcpNonBlocking": true, "userType": "external"}
	announced := map[string]any{"addedCount": float64(3), "readdedCount": float64(0), "unlistedCount": float64(3), "removedCount": float64(0), "priorAnnouncedCount": float64(0), "messagesLength": float64(1), "attachmentCount": float64(0), "dtdCount": float64(0), "querySource": "sdk", "attachmentTypesSeen": ""}
	for index, name := range gotOrder {
		if name == decisionEvent {
			assertSDKEventFields(t, stream[index], enabled)
		}
	}
	mainAnnounced := map[string]any{"callSite": "attachments_main"}
	childAnnounced := map[string]any{"callSite": "attachments_subagent"}
	for key, value := range announced {
		mainAnnounced[key], childAnnounced[key] = value, value
	}
	assertSDKEventFields(t, stream[1], mainAnnounced)
	assertSDKEventFields(t, stream[10], childAnnounced)
	assertSDKEventFields(t, stream[3], map[string]any{"queryLength": float64(len("select:SendMessage")), "querySelectCount": float64(1), "queryType": "select", "matchCount": float64(1), "totalDeferredTools": float64(3), "maxResults": float64(5), "hasMatches": true, "mcpServersConfigured": float64(0), "mcpServersConnected": float64(0), "mcpServersCached": float64(0), "mcpServersPending": float64(0), "mcpToolsInPool": float64(0)})
	assertSDKEventFields(t, stream[6], map[string]any{"queryLength": float64(len("nothing here")), "querySelectCount": nil, "queryType": "keyword", "matchCount": float64(0), "totalDeferredTools": float64(3), "maxResults": float64(5), "hasMatches": false})
	assertSDKEventFields(t, stream[12], map[string]any{"queryLength": float64(len("select:TaskStop")), "querySelectCount": float64(1), "queryType": "select", "matchCount": float64(1), "totalDeferredTools": float64(3), "maxResults": float64(5), "hasMatches": true})
	// The main turn is one prompt: its decisions, pool change, outcomes and
	// queries share cc_prompt_id; the child generation runs under its own.
	mainPrompt, _ := stream[2].fields["cc_prompt_id"].(string)
	childPrompt, _ := stream[11].fields["cc_prompt_id"].(string)
	if mainPrompt == "" || childPrompt == "" || mainPrompt == childPrompt {
		t.Fatalf("prompt identities main=%q child=%q", mainPrompt, childPrompt)
	}
	for index, event := range stream {
		want := mainPrompt
		if index >= 9 && index <= 14 {
			want = childPrompt
		}
		if got, _ := event.fields["cc_prompt_id"].(string); got != want {
			t.Fatalf("event %d %s cc_prompt_id=%q want %q", index, event.name, got, want)
		}
	}
}

// The three tool-search gates come from the same evaluated host features as
// the catalog gates; the Desktop worker keeps the mode and fetch-rule zero.
func TestClaudeDesktopAgentDefinitionContextReadsToolSearchGates(t *testing.T) {
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	host := inner.desktopATIS.featureHosts.Warm()
	ctx := inner.desktopAgentDefinitionContext(auth, host, "claude-sonnet-5")
	if ctx.ToolSearchUnsupportedModels != nil || ctx.DeferredStubDisabled || ctx.NonDeferrableBuiltins != nil || ctx.ToolSearchDisabled || ctx.ToolSearchFetchRule {
		t.Fatalf("default tool-search gates %+v", ctx)
	}
	if !claudetasks.ToolSearchEnabled(ctx) {
		t.Fatal("Desktop defaults must enable tool search for the owned pool")
	}
	ticket, sink, err := inner.desktopATIS.bindFeatureHost(auth, host)
	if err != nil {
		t.Fatal(err)
	}
	seed := func(features string) {
		t.Helper()
		accepted, err := host.Service().Observe(ticket, []byte(`{"features":`+features+`}`), host.SessionID(), sink)
		if err != nil || !accepted {
			t.Fatalf("seed features: accepted=%v err=%v", accepted, err)
		}
	}
	seed(`{"tengu_tool_search_unsupported_models":{"value":["claude-3-5-haiku","claude-sonnet-5"]},"tengu_deferred_stub_tool":{"value":false},"tengu_non_deferrable_builtins":{"value":{"opus":["TaskStop"],"*":["TaskOutput","SendMessage"],"sonnet":["SendMessage"]}}}`)
	sonnet := inner.desktopAgentDefinitionContext(auth, host, "claude-sonnet-5")
	if strings.Join(sonnet.ToolSearchUnsupportedModels, ",") != "claude-3-5-haiku,claude-sonnet-5" || !sonnet.DeferredStubDisabled || strings.Join(sonnet.NonDeferrableBuiltins, ",") != "SendMessage" {
		t.Fatalf("sonnet tool-search gates %+v", sonnet)
	}
	if claudetasks.ToolSearchEnabled(sonnet) {
		t.Fatal("an unsupported model must disable tool search")
	}
	if opus := inner.desktopAgentDefinitionContext(auth, host, "claude-opus-5"); strings.Join(opus.NonDeferrableBuiltins, ",") != "TaskStop" || !claudetasks.ToolSearchEnabled(opus) {
		t.Fatalf("opus tool-search gates %+v", opus)
	}
	if haiku := inner.desktopAgentDefinitionContext(auth, host, "claude-haiku-4-5-20251001"); strings.Join(haiku.NonDeferrableBuiltins, ",") != "TaskOutput,SendMessage" {
		t.Fatalf("model without a keyed entry must take the * fallback: %+v", haiku)
	}
	seed(`{"tengu_tool_search_unsupported_models":{"value":[]},"tengu_deferred_stub_tool":{"value":true},"tengu_non_deferrable_builtins":{"value":["TaskOutput"]}}`)
	ctx = inner.desktopAgentDefinitionContext(auth, host, "claude-3-5-haiku-20241022")
	if ctx.ToolSearchUnsupportedModels == nil || len(ctx.ToolSearchUnsupportedModels) != 0 || ctx.DeferredStubDisabled || strings.Join(ctx.NonDeferrableBuiltins, ",") != "TaskOutput" {
		t.Fatalf("array-form tool-search gates %+v", ctx)
	}
	if !claudetasks.ToolSearchEnabled(ctx) {
		t.Fatal("an empty unsupported list replaces the SDK default")
	}
	if ctx.ToolSearchDisabled || ctx.ToolSearchFetchRule {
		t.Fatalf("Desktop defaults changed: %+v", ctx)
	}
}
