package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type epochChangingDoer struct {
	recordingControlDoer
	gate     sync.Mutex
	bridges  int
	failNext bool
}

func (d *epochChangingDoer) Do(request *http.Request) (*http.Response, error) {
	response, err := d.recordingControlDoer.Do(request)
	if err != nil {
		return nil, err
	}
	d.gate.Lock()
	defer d.gate.Unlock()
	if strings.HasSuffix(request.URL.Path, "/bridge") {
		d.bridges++
		_ = response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(`{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"` + strconv.Itoa(d.bridges) + `","worker_jwt":"worker-` + strconv.Itoa(d.bridges) + `"}`))
	} else if d.failNext && request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/worker") {
		d.failNext = false
		response.StatusCode = http.StatusUnauthorized
	}
	return response, nil
}

func TestWorkerEpochRefreshRebindsBodiesAndInitializesNewWorker(t *testing.T) {
	bundle, err := claudeprofile.BuiltinV140609()
	if err != nil {
		t.Fatal(err)
	}
	doer := &epochChangingDoer{}
	manager := NewManager(Options{StatePath: t.TempDir(), Bundle: bundle, DisableLoops: true,
		DoerFactory: func(context.Context, string, *cliproxyauth.Auth) (HTTPDoer, error) { return doer, nil }})
	t.Cleanup(manager.Close)
	ctx := context.Background()
	if err := manager.EnsureSession(ctx, testDesktopAuth(t), "epoch-session", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	session := manager.sessions["epoch-session"]
	session.opMu.Lock()
	defer session.opMu.Unlock()
	facts := RequestFacts{Role: claudeprofile.RoleMain, Model: "claude-sonnet-5", PermissionMode: "default"}
	if err := session.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil {
		t.Fatal(err)
	}
	before := len(doer.snapshot())
	doer.gate.Lock()
	doer.failNext = true
	doer.gate.Unlock()
	if err := session.setWorkerRunningLocked(ctx, facts, nil); err != nil {
		t.Fatal(err)
	}
	if session.state.WorkerEpoch != "2" || session.requestInitSent {
		t.Fatal("new worker epoch retained the old initialization latch")
	}
	var updates []recordedControlRequest
	for _, request := range doer.snapshot()[before:] {
		if request.method == http.MethodPut && strings.HasSuffix(request.path, "/worker") {
			updates = append(updates, request)
		}
	}
	if len(updates) != 2 {
		t.Fatalf("worker update attempts=%d", len(updates))
	}
	for index, request := range updates {
		var body struct {
			Epoch int `json:"worker_epoch"`
		}
		if err := json.Unmarshal(request.body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Epoch != index+1 || request.header.Get("Authorization") != "Bearer worker-"+strconv.Itoa(index+1) {
			t.Fatal("worker body/credential epoch mismatch")
		}
	}
	if strings.Replace(string(updates[0].body), `"worker_epoch":1`, `"worker_epoch":2`, 1) != string(updates[1].body) {
		t.Fatal("epoch renewal changed unrelated fields or JSON key order")
	}
	if err := session.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil {
		t.Fatal(err)
	}
	if !session.requestInitSent {
		t.Fatal("new worker was not initialized")
	}
	before = len(doer.snapshot())
	if err := session.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil {
		t.Fatal(err)
	}
	if len(doer.snapshot()) != before {
		t.Fatal("new worker initialized twice")
	}
	// A proactive renewal must happen before deciding to omit initialization.
	session.state.WorkerJWTExpiresAt = time.Unix(1, 0).UTC().Format(time.RFC3339Nano)
	if err := session.postRequestEventsLocked(ctx, facts, nil, responseAccumulator{}); err != nil {
		t.Fatal(err)
	}
	if session.state.WorkerEpoch != "3" || !session.requestInitSent {
		t.Fatal("proactive epoch renewal skipped initialization")
	}
	last := doer.snapshot()[len(doer.snapshot())-1]
	if !strings.Contains(string(last.body), `"worker_epoch":3`) || !strings.Contains(string(last.body), `"subtype":"init"`) {
		t.Fatal("proactive renewal used stale event payload")
	}
}

func TestWorkerEpochBindingPreservesNestedFieldsAndCaller(t *testing.T) {
	session := &sessionRuntime{state: controlSessionState{WorkerEpoch: "12"}}
	body := json.RawMessage(`{"worker_status":"running","worker_epoch":1,"events":[{"payload":{"worker_epoch":7,"uuid":"event-id"}}]}`)
	updated, err := session.bindWorkerEpochLocked(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(updated.(json.RawMessage)) != strings.Replace(string(body), `"worker_epoch":1`, `"worker_epoch":12`, 1) {
		t.Fatal("nested fields or ordering changed")
	}
	if !strings.Contains(string(body), `"worker_epoch":1,`) {
		t.Fatal("caller body mutated")
	}
	if _, err := session.bindWorkerEpochLocked(map[string]any{"missing": true}); err == nil {
		t.Fatal("missing transport-owned epoch accepted")
	}
}
