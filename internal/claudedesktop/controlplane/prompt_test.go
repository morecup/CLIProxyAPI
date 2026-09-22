package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestManagedPromptControlPlaneDoesNotCompleteOnToolUse(t *testing.T) {
	bundle, errBundle := claudeprofile.BuiltinV140609()
	if errBundle != nil {
		t.Fatal(errBundle)
	}
	doer := &recordingControlDoer{}
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true, DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }})
	t.Cleanup(manager.Close)
	auth := testDesktopAuth(t)
	var tracker claudeprompt.Tracker
	input := claudeprompt.Input{AccountID: auth.ID, SessionID: "managed-control", PromptID: "11111111-1111-4111-8111-111111111111", ClientRequestID: "first", Role: "main", Body: []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`), StartedAt: time.Now()}
	firstToken := tracker.Begin(input)
	facts := RequestFacts{Role: claudeprofile.RoleMain, LocalSessionID: input.SessionID, PromptID: firstToken.Identity().PromptID, Model: "claude-sonnet-5", Body: input.Body, Prompt: firstToken, Attempt: 1}
	first, errBegin := manager.BeginRequest(context.Background(), auth, facts)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	baseline := len(doer.snapshot())
	first.ObserveHTTPResponse(http.Header{"Request-Id": {"req_tool"}})
	first.ObserveResponsePayload([]byte(`{"type":"message","id":"msg_tool","model":"claude-sonnet-5","content":[{"type":"tool_use","id":"tool","name":"Read","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`), false)
	firstToken.FinishSuccess(time.Now(), "tool_use", []string{"tool"})
	first.FinishSuccess(context.Background())
	count := func(kind string) int {
		n := 0
		for _, request := range doer.snapshot()[baseline:] {
			if !strings.HasSuffix(request.path, "/worker/events") {
				continue
			}
			var batch struct {
				Events []struct {
					Payload struct {
						Type    string `json:"type"`
						Subtype string `json:"subtype"`
					} `json:"payload"`
				} `json:"events"`
			}
			if json.Unmarshal(request.body, &batch) == nil {
				for _, event := range batch.Events {
					if event.Payload.Type == kind || event.Payload.Type+":"+event.Payload.Subtype == kind {
						n++
					}
				}
			}
		}
		return n
	}
	if count("result") != 0 {
		t.Fatal("tool response posted a terminal worker result")
	}
	input.ClientRequestID = "second"
	input.Body = []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool","content":"synthetic result"}]}]}`)
	secondToken := tracker.Begin(input)
	facts.Prompt, facts.PromptID, facts.Body = secondToken, secondToken.Identity().PromptID, input.Body
	second, errSecond := manager.BeginRequest(context.Background(), auth, facts)
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	second.ObserveHTTPResponse(http.Header{"Request-Id": {"req_done"}})
	second.ObserveResponsePayload([]byte(`{"type":"message","id":"msg_done","model":"claude-sonnet-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), false)
	secondToken.FinishSuccess(time.Now(), "end_turn", nil)
	second.FinishSuccess(context.Background())
	if count("user") != 1 || count("result") != 1 {
		t.Fatalf("user=%d result=%d; expected one per logical prompt", count("user"), count("result"))
	}
	if count("system:init") != 1 || count("system:status") != 1 || count("system:background_tasks_changed") != 1 {
		t.Fatal("tool continuation repeated worker initialization")
	}
	// A later human prompt still uses the existing worker, not a fresh init.
	input.ClientRequestID = "third"
	input.Body = []byte(`{"messages":[{"role":"user","content":"next prompt"}]}`)
	thirdToken := tracker.Begin(input)
	facts.Prompt, facts.PromptID, facts.Body = thirdToken, thirdToken.Identity().PromptID, input.Body
	third, errThird := manager.BeginRequest(context.Background(), auth, facts)
	if errThird != nil {
		t.Fatal(errThird)
	}
	third.ObserveHTTPResponse(http.Header{"Request-Id": {"req_next"}})
	third.ObserveResponsePayload([]byte(`{"type":"message","id":"msg_next","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":22}}`), false)
	thirdToken.FinishSuccess(time.Now(), "end_turn", nil)
	third.FinishSuccess(context.Background())
	if count("system:init") != 1 || count("result") != 2 || count("user") != 2 {
		t.Fatal("later human prompt did not reuse the initialized worker")
	}
	for _, request := range doer.snapshot()[baseline:] {
		if !strings.HasSuffix(request.path, "/worker/events") {
			continue
		}
		var batch struct {
			Events []struct {
				Payload zeroTurnResultPayload `json:"payload"`
			} `json:"events"`
		}
		if json.Unmarshal(request.body, &batch) != nil {
			continue
		}
		for _, event := range batch.Events {
			v := event.Payload
			if v.Type == "result" && (v.DurationMS != 0 || v.DurationAPIMS != 0 || v.NumTurns != 0 || v.Usage.InputTokens != 0 || v.Usage.OutputTokens != 0 || v.Result != "" || v.StopReason != nil || len(v.ModelUsage) != 0) {
				t.Fatal("worker sentinel was replaced with SDK/usage aggregates")
			}
		}
	}
}

type initFailingDoer struct {
	recordingControlDoer
	gate   sync.Mutex
	failed bool
}

func (d *initFailingDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := d.recordingControlDoer.Do(request)
	if err != nil {
		return nil, err
	}
	d.gate.Lock()
	defer d.gate.Unlock()
	requests := d.snapshot()
	if !d.failed && strings.Contains(string(requests[len(requests)-1].body), `"subtype":"init"`) {
		d.failed = true
		_ = response.Body.Close()
		response.StatusCode = http.StatusBadGateway
		response.Body = io.NopCloser(strings.NewReader(`{}`))
	}
	return response, nil
}

func TestWorkerInitializationRetriesFailedBatchAndIsSessionScoped(t *testing.T) {
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	doer := &initFailingDoer{}
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }})
	t.Cleanup(manager.Close)
	ctx := context.Background()
	auth := testDesktopAuth(t)
	for _, id := range []string{"first", "second"} {
		if err := manager.EnsureSession(ctx, auth, id, "claude-sonnet-5"); err != nil {
			t.Fatal(err)
		}
	}
	first := manager.sessions["first"]
	facts := RequestFacts{Role: claudeprofile.RoleMain, Model: "claude-sonnet-5"}
	func() {
		first.opMu.Lock()
		defer first.opMu.Unlock()
		if err := first.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err == nil || first.requestInitSent {
			t.Fatal("failed init batch was marked as delivered")
		}
		if err := first.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil || !first.requestInitSent {
			t.Fatalf("initialization did not retry: %v", err)
		}
		before := len(doer.snapshot())
		if err := first.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil {
			t.Fatal(err)
		}
		if len(doer.snapshot()) != before {
			t.Fatal("empty continuation emitted initialization or an empty batch")
		}
		facts.PermissionMode = "default"
		if err := first.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil {
			t.Fatal(err)
		}
		updates := doer.snapshot()[before:]
		if len(updates) != 1 {
			t.Fatalf("config change requests = %d", len(updates))
		}
		var batch struct {
			Events []struct {
				Payload struct {
					Type           string `json:"type"`
					Subtype        string `json:"subtype"`
					PermissionMode string `json:"permissionMode"`
				} `json:"payload"`
			} `json:"events"`
		}
		if err := json.Unmarshal(updates[0].body, &batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.Events) != 1 || batch.Events[0].Payload.Subtype != "init" || batch.Events[0].Payload.PermissionMode != "default" {
			t.Fatal("config change replayed full startup batch instead of init-only update")
		}
	}()
	second := manager.sessions["second"]
	second.opMu.Lock()
	defer second.opMu.Unlock()
	if second.requestInitSent {
		t.Fatal("other session inherited worker initialization")
	}
	if err := second.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil || !second.requestInitSent {
		t.Fatalf("second worker init: %v", err)
	}
	second.resetRemoteLocked()
	if second.requestInitSent {
		t.Fatal("remote worker reset retained initialization latch")
	}
}
