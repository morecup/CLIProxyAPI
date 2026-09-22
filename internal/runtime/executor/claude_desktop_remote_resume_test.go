package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudefeatures "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/features"
	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type remoteResumeFailingStore struct{ claudefeatures.Store }

func (s remoteResumeFailingStore) Save(string, string, []byte) (string, error) {
	return "", claudesessions.ErrUnavailable
}

func TestClaudeDesktopActualSavedRemoteResumeAfterStopAndRestart(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{})
}

func TestClaudeDesktopActualSavedRemoteWorkerRestoration(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{restoreWorker: true})
}

func TestClaudeDesktopActualSavedRemoteResumeWithUnavailableWorkerState(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{failedWorkerRead: true})
}

func TestClaudeDesktopActualSavedRemoteWorkerConflictRetiresQuery(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{failedWorkerRead: true, workerConflict: true})
}

func TestClaudeDesktopActualSavedRemoteHydrationUsesServerRevision(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{hydrateRemote: true})
}

func TestClaudeDesktopActualRemoteInitialHistoryThenStopAndRestart(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{hydrateRemote: true, hydrateInitial: true})
}

func TestClaudeDesktopActualRemoteHistorySelectsNativeChain(t *testing.T) {
	testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{hydrateRemote: true, hydrateInitial: true, branchedHistory: true})
}

func TestClaudeDesktopActualRemoteHistoryHonorsLastPrompt(t *testing.T) {
	for _, mode := range []string{"explicit-anchor", "explicit-clear"} {
		t.Run(mode, func(t *testing.T) {
			testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{hydrateRemote: true, hydrateInitial: true, historySelection: mode})
		})
	}
}

type remoteResumeCase struct {
	restoreWorker, failedWorkerRead, workerConflict                     bool
	hydrateRemote, hydrateInitial                                       bool
	branchedHistory                                                     bool
	historySelection                                                    string
	deltaEnabled, skipSubagentsOnDelta, rejectAnchor, nullAnchorEventID bool
}

func TestClaudeDesktopActualSavedRemoteHydrationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		delta, skip, reject, nullID bool
	}{
		{"full_with_skip_gate", false, true, false, false},
		{"delta_with_eager_agents", true, false, false, false},
		{"delta_with_skipped_agents", true, true, false, false},
		{"rejected_anchor_restores_eager_agents", true, true, true, false},
		{"null_event_id", true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testClaudeDesktopActualSavedRemoteResume(t, remoteResumeCase{hydrateRemote: true, hydrateInitial: true,
				deltaEnabled: tc.delta, skipSubagentsOnDelta: tc.skip, rejectAnchor: tc.reject, nullAnchorEventID: tc.nullID})
		})
	}
}

func testClaudeDesktopActualSavedRemoteResume(t *testing.T, options remoteResumeCase) {
	t.Helper()
	hydrateRemote, hydrateInitial := options.hydrateRemote, options.hydrateInitial
	extraHistory := int64(0)
	if hydrateInitial {
		extraHistory = 2
	}
	if options.historySelection == "explicit-clear" {
		extraHistory = 0
	}
	e, auths := newExecutionSessionAccountTest(t)
	auth := auths[0]
	for _, current := range auths {
		current.Metadata["access_token"] = current.Attributes[cliproxyauth.AttributeAPIKey]
		registry.GetGlobalRegistry().RegisterClient(current.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}, {ID: "claude-sonnet-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(current.ID) })
	}
	type stream struct {
		request *http.Request
		writer  *io.PipeWriter
	}
	streams := make(chan stream, 8)
	controls := make(chan struct{}, 8)
	var bridges, creates, archives, deltaReads, subagentReads atomic.Int32
	install := func(provider *ClaudeAccountExecutor) {
		manager := cliproxyauth.NewManager(nil, nil, nil)
		manager.RegisterExecutor(provider)
		provider.credentialManager = manager
		for _, current := range auths {
			if _, err := manager.Register(t.Context(), current); err != nil {
				t.Fatal(err)
			}
		}
		inner := accountRuntimeForAuth(t, provider, auth.ID).executor
		if options.deltaEnabled || options.skipSubagentsOnDelta {
			inner.desktopATIS.startFeatureHost = func(host *claudefeatures.Host) error {
				ticket, sink, err := inner.desktopATIS.bindFeatureHost(auth, host)
				if err != nil {
					return err
				}
				payload := fmt.Sprintf(`{"features":{"tengu_ccr_delta_rehydrate":{"value":%t},"tengu_ccr_subagent_skip_on_delta":{"value":%t}}}`, options.deltaEnabled, options.skipSubagentsOnDelta)
				_, err = host.Service().Observe(ticket, []byte(payload), host.SessionID(), sink)
				return err
			}
		}
		inner.desktopControlPlane.Close()
		inner.desktopControlPlane = claudecontrol.NewManager(claudecontrol.Options{StatePath: t.TempDir(), Bundle: inner.desktopProfile,
			DoerFactory: func(_ context.Context, _ string, current *cliproxyauth.Auth) (claudecontrol.HTTPDoer, error) {
				if current.ID != auth.ID {
					return nil, errors.New("foreign account")
				}
				return desktopControlQueryDoer(func(request *http.Request) (*http.Response, error) {
					body, status := `{}`, 200
					switch {
					case strings.HasSuffix(request.URL.Path, "/worker/internal-events"):
						body = `{"data":[]}`
						if request.URL.Query().Get("subagents") == "true" {
							subagentReads.Add(1)
						}
						anchor := request.URL.Query().Get("after_event_id")
						if anchor != "" {
							deltaReads.Add(1)
						}
						if hydrateRemote && (bridges.Load() > 1 || hydrateInitial) && request.URL.Query().Get("subagents") != "true" {
							if request.Header.Get("Authorization") != fmt.Sprintf("Bearer resume-worker-%d", bridges.Load()) {
								t.Error("hydration borrowed another worker credential")
							}
							list, err := provider.ListDesktopSessions(auth.ID)
							if err != nil || len(list) != 1 {
								return nil, errors.New("synthetic hydration lost owned session")
							}
							snapshot := inner.desktopPrompts.NativeContent(helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID), list[0].SDKSessionID)
							rows := make(map[string]any)
							for _, row := range snapshot.Messages {
								rows[row.UUID] = row
							}
							for _, row := range snapshot.ProjectedMessages {
								rows[row.UUID] = row
							}
							var events []map[string]any
							for _, id := range snapshot.ActiveUUIDs {
								events = append(events, map[string]any{"event_id": id, "payload": rows[id]})
							}
							if hydrateInitial && bridges.Load() == 1 {
								userID, assistantID := "72d63631-865d-4c32-96ad-762c02f44807", "6711681b-ee15-4fca-9fe9-5e339e9b0140"
								events = []map[string]any{
									{"event_id": userID, "payload": map[string]any{"type": "user", "uuid": userID, "sessionId": list[0].SDKSessionID, "timestamp": "2026-09-07T10:00:00.000Z", "parentUuid": nil, "message": map[string]any{"role": "user", "content": "REMOTE_BEFORE_LOCAL_USER"}}},
									{"event_id": assistantID, "payload": map[string]any{"type": "assistant", "uuid": assistantID, "sessionId": list[0].SDKSessionID, "timestamp": "2026-09-07T10:00:01.000Z", "parentUuid": userID, "message": map[string]any{"id": "msg_before_local", "role": "assistant", "content": []map[string]string{{"type": "text", "text": "REMOTE_BEFORE_LOCAL_REPLY"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 5, "output_tokens": 2}}}},
								}
							}
							if options.branchedHistory && bridges.Load() == 1 {
								// Plain-file selection retains the last foreground entry as its
								// leaf; the earlier sibling must not be flattened into that chain.
								inactive := map[string]any{"event_id": "df04096c-4e70-4e09-b95b-7db5cce807e5", "payload": map[string]any{
									"type": "assistant", "uuid": "df04096c-4e70-4e09-b95b-7db5cce807e5", "sessionId": list[0].SDKSessionID,
									"timestamp": "2026-09-07T10:00:00.500Z", "parentUuid": "72d63631-865d-4c32-96ad-762c02f44807",
									"message": map[string]any{"id": "msg_rejected_branch", "role": "assistant", "content": []map[string]string{{"type": "text", "text": "REJECTED_HISTORY_BRANCH"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 5, "output_tokens": 2}},
								}}
								events = []map[string]any{events[0], inactive, events[1]}
							}
							if options.historySelection != "" && bridges.Load() == 1 {
								var leaf any = "6711681b-ee15-4fca-9fe9-5e339e9b0140"
								if options.historySelection == "explicit-clear" {
									leaf = nil
								} else {
									events = append(events, map[string]any{"event_id": "75d50903-dcd6-44b4-a3e1-0da80736f4de", "payload": map[string]any{
										"type": "assistant", "uuid": "75d50903-dcd6-44b4-a3e1-0da80736f4de", "sessionId": list[0].SDKSessionID,
										"timestamp": "2026-09-07T10:00:02.000Z", "parentUuid": "72d63631-865d-4c32-96ad-762c02f44807",
										"message": map[string]any{"id": "msg_unselected_tip", "role": "assistant", "content": []map[string]string{{"type": "text", "text": "UNSELECTED_LATER_BRANCH"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 5, "output_tokens": 2}},
									}})
								}
								events = append(events, map[string]any{"event_id": "7f29e2fc-736d-4489-bd97-b35e530a6cff", "payload": map[string]any{
									"type": "last-prompt", "sessionId": list[0].SDKSessionID, "leafUuid": leaf, "explicit": true,
								}})
							}
							if options.deltaEnabled && bridges.Load() == 2 {
								parent := snapshot.ActiveUUIDs[len(snapshot.ActiveUUIDs)-1]
								userID, assistantID := "1a4a8de6-a859-41ce-99c9-3bb4e0fdfd0a", "266f0aeb-f32f-4417-9483-dc047b80cb85"
								events = append(events,
									map[string]any{"event_id": userID, "payload": map[string]any{"type": "user", "uuid": userID, "sessionId": list[0].SDKSessionID, "timestamp": "2026-09-07T10:01:00.000Z", "parentUuid": parent, "message": map[string]any{"role": "user", "content": "REMOTE_DELTA_USER"}}},
									map[string]any{"event_id": assistantID, "payload": map[string]any{"type": "assistant", "uuid": assistantID, "sessionId": list[0].SDKSessionID, "timestamp": "2026-09-07T10:01:01.000Z", "parentUuid": userID, "message": map[string]any{"id": "msg_delta", "role": "assistant", "content": []map[string]string{{"type": "text", "text": "REMOTE_DELTA_REPLY"}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 5, "output_tokens": 2}}}})
							}
							if anchor != "" && !(options.rejectAnchor && bridges.Load() == 2) {
								found := false
								for index, event := range events {
									if event["event_id"] == anchor {
										if options.nullAnchorEventID {
											events = events[index:]
											events[0] = map[string]any{"event_id": nil, "payload": event["payload"]}
										} else {
											events = events[index+1:]
										}
										found = true
										break
									}
								}
								if !found {
									return nil, errors.New("actual delta request lost its persisted anchor")
								}
							}
							encoded, _ := json.Marshal(map[string]any{"data": events})
							body = strings.ReplaceAll(string(encoded), "reply-1", "REMOTE_EDITED_REPLY")
							if anchor != "" && options.rejectAnchor && bridges.Load() == 2 {
								status, body = 400, `{"error":{"type":"invalid_request_error"}}`
							}
						}
					case request.URL.Path == "/v1/code/sessions":
						creates.Add(1)
						body = `{"session":{"id":"cse_unexpected"}}`
					case strings.HasSuffix(request.URL.Path, "/unarchive"):
						status = 409
					case strings.HasSuffix(request.URL.Path, "/archive"):
						archives.Add(1)
					case strings.HasSuffix(request.URL.Path, "/bridge"):
						epoch := bridges.Add(1)
						body = fmt.Sprintf(`{"api_base_url":"https://api.anthropic.com","expires_in":3600,"worker_epoch":"%d","worker_jwt":"resume-worker-%d"}`, epoch, epoch)
					case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/worker"):
						if options.failedWorkerRead && bridges.Load() > 1 {
							status = 400
							if options.workerConflict {
								status, body = 409, `{"error":{"reason":"superseded_by_worker"}}`
							}
						}
						if options.restoreWorker && bridges.Load() > 1 {
							model := "claude-opus-5"
							if bridges.Load() > 2 {
								model = "  DeFaUlT  "
							}
							body = fmt.Sprintf(`{"worker":{"external_metadata":{"model":%q,"system_prompt":"UNOWNED_WORKER_SYSTEM"},"internal_metadata":{"synthetic_unused":true}}}`, model)
						}
					case strings.HasSuffix(request.URL.Path, "/worker/events/stream"):
						reader, writer := io.Pipe()
						streams <- stream{request, writer}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &remoteExecutorPipe{reader, writer}}, nil
					case strings.HasSuffix(request.URL.Path, "/worker/events"):
						payload, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						for _, event := range gjson.GetBytes(payload, "events").Array() {
							if event.Get("payload.type").String() == "control_response" {
								controls <- struct{}{}
							}
						}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}), nil
			}})
	}
	install(e)
	requests := make(chan []byte, 8)
	var calls atomic.Int32
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
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
		call := calls.Add(1)
		requests <- body
		owner := request.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if owner.accountID != auth.ID || !owner.remoteInput {
			return nil, errors.New("lost resumed input ownership")
		}
		payload := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_resume_%d\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n", call)
		payload += "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
		payload += fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"reply-%d\"}}\n\n", call)
		payload += "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Request-Id": {fmt.Sprintf("req-resume-%d", call)}}, Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
	})))
	first, err := e.StartDesktopRemoteSession(ctx, auth.ID, cliproxyexecutor.ClaudeDesktopRemoteStart{RemoteSessionID: "cse_savedResume", Folder: `C:\code`, Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	wire := awaitRemoteExecutor(t, streams)
	send := func(sequence int, payload string) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintf(wire.writer, "id: %d\nevent: client_event\ndata: {\"event_id\":\"resume%d\",\"event_type\":\"user\",\"payload\":%s}\n\n", sequence, sequence, payload)
			done <- err
		}()
		if err := awaitRemoteExecutor(t, done); err != nil {
			t.Fatal(err)
		}
	}
	turn := func(provider *ClaudeAccountExecutor, sequence int, text string, completed int) []byte {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"type": "user", "uuid": fmt.Sprintf("resume-input-%d", sequence), "message": map[string]any{"role": "user", "content": text}})
		send(sequence, string(payload))
		body := awaitRemoteExecutor(t, requests)
		if options.branchedHistory && bytes.Contains(body, []byte("REJECTED_HISTORY_BRANCH")) {
			t.Fatal("actual Messages request included the inactive transcript branch")
		}
		if options.historySelection != "" && bytes.Contains(body, []byte("UNSELECTED_LATER_BRANCH")) {
			t.Fatal("actual Messages ignored the explicit last-prompt anchor")
		}
		if options.historySelection == "explicit-clear" && (bytes.Contains(body, []byte("REMOTE_BEFORE_LOCAL_USER")) || bytes.Contains(body, []byte("REMOTE_BEFORE_LOCAL_REPLY"))) {
			t.Fatal("actual Messages resurrected explicitly cleared remote history")
		}
		inner := accountRuntimeForAuth(t, provider, auth.ID).executor
		deadline := time.After(10 * time.Second)
		for inner.desktopControlPlane.Status().InboundCompleted != int64(completed) {
			select {
			case <-deadline:
				t.Fatal("remote completion missing", inner.desktopControlPlane.Status())
			case <-time.After(time.Millisecond):
			}
		}
		return body
	}
	initialBody := turn(e, 1, "saved-one", 1)
	if hydrateInitial && options.historySelection != "explicit-clear" && (!bytes.Contains(initialBody, []byte("REMOTE_BEFORE_LOCAL_REPLY")) || !bytes.Contains(initialBody, []byte("REMOTE_BEFORE_LOCAL_USER"))) {
		t.Fatal("first actual input ignored remote-only history")
	}
	send(2, `{"type":"control_request","request_id":"configure","request":{"subtype":"set_model","model":"claude-sonnet-5","system_prompt":"SAVED_SYSTEM"}}`)
	awaitRemoteExecutor(t, controls)
	turn(e, 3, "saved-two", 2)
	stop := func(provider *ClaudeAccountExecutor, value cliproxyexecutor.ClaudeDesktopSession) {
		t.Helper()
		if _, err := provider.StopDesktopSession(t.Context(), auth.ID, cliproxyexecutor.ClaudeDesktopSessionStop{SessionID: value.ID, ExpectedQueryID: value.QueryID}); err != nil {
			t.Fatal(err)
		}
	}
	stop(e, first)
	inner := accountRuntimeForAuth(t, e, auth.ID).executor
	accountScope := helps.ClaudeDesktopPromptAccountScope(auth, inner.desktopProfile.ProfileID)
	before := inner.desktopPrompts.NativeContent(accountScope, first.SDKSessionID)
	diagnosticScopeJSON, _ := json.Marshal([]string{accountScope, first.SDKSessionID})
	diagnosticScopeHash := sha256.Sum256(diagnosticScopeJSON)
	diagnosticState, _, diagnosticErr := helps.NewClaudeDesktopSDKSessionStore(inner.desktopDurableStatePath, e.cfg.ProxyURL).Load(hex.EncodeToString(diagnosticScopeHash[:]))
	if diagnosticErr != nil {
		t.Fatal(diagnosticErr)
	}
	t.Logf("saved structural history: issue=%q known=%t pending=%d messages=%d expected=%d", gjson.GetBytes(diagnosticState, "history.incompleteReason").String(), gjson.GetBytes(diagnosticState, "history.expectedTextKnown").Bool(), gjson.GetBytes(diagnosticState, "history.pendingReconciliations").Int(), gjson.GetBytes(diagnosticState, "history.messages.#").Int(), gjson.GetBytes(diagnosticState, "history.expectedText.#").Int())
	if len(before.Messages) != 4 {
		t.Fatal("initial native history was not complete", len(before.Messages), before.IncompleteReason)
	}
	// Fail the actual protected catalog commit. No attachment or inference may
	// begin, and the same observed durable generation remains recoverable.
	store := helps.NewClaudeDesktopSessionRecordStore(inner.desktopDurableStatePath, e.cfg.ProxyURL)
	inner.desktopATIS.desktopRecords = claudesessions.NewRegistry(remoteResumeFailingStore{store})
	operation := cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: first.ID, ExpectedGeneration: first.Generation}
	if value, err := e.ResumeDesktopSession(ctx, auth.ID, operation); err == nil || value.Running || value.Generation != first.Generation {
		t.Fatal("failed durable admission was published", value, err)
	}
	if bridges.Load() != 1 || calls.Load() != 2 {
		t.Fatal("failed admission emitted upstream work")
	}
	inner.desktopATIS.desktopRecords = claudesessions.NewRegistry(store)
	archivesBeforeResume := archives.Load()
	second, err := e.ResumeDesktopSession(ctx, auth.ID, operation)
	if options.workerConflict {
		var conflict *claudecontrol.WorkerEpochConflict
		if !errors.As(err, &conflict) || conflict.Reason != "superseded_by_worker" || second.Running || calls.Load() != 2 || archives.Load() != archivesBeforeResume {
			t.Fatal("conflicted actual query survived, inferred or archived its remote successor", err, second.Running, calls.Load(), archives.Load())
		}
		return
	}
	if err != nil {
		t.Fatal("actual stopped resume", err)
	}
	wire = awaitRemoteExecutor(t, streams)
	if second.ID != first.ID || second.SDKSessionID != first.SDKSessionID || second.QueryID == first.QueryID || second.Generation == first.Generation || calls.Load() != 2 {
		t.Fatal("resume changed history identity or invoked inference", second)
	}
	if options.failedWorkerRead {
		status := inner.desktopControlPlane.Status()
		if status.WorkerStateReadFailed != 1 || status.InitializationFailed != 0 {
			t.Fatal("actual resume hid unavailable worker state or mislabeled registration", status)
		}
	}
	if wire.request.Header.Get("Authorization") != "Bearer resume-worker-2" || wire.request.URL.Query().Get("from_sequence_num") != "3" {
		t.Fatal("resume lost fresh worker or durable cursor", wire.request.URL)
	}
	body := turn(e, 4, "saved-three", 3)
	if options.deltaEnabled {
		extraHistory += 2
		if !bytes.Contains(body, []byte("REMOTE_DELTA_USER")) || !bytes.Contains(body, []byte("REMOTE_DELTA_REPLY")) {
			t.Fatal("actual resumed inference ignored remote delta content")
		}
	}
	wantModel := "claude-sonnet-5"
	if options.restoreWorker {
		wantModel = "claude-opus-5"
	}
	if gjson.GetBytes(body, "messages.#").Int() != 6+extraHistory || gjson.GetBytes(body, "model").String() != wantModel || !bytes.Contains(body, []byte("SAVED_SYSTEM")) || bytes.Contains(body, []byte("UNOWNED_WORKER_SYSTEM")) {
		t.Fatal("stopped resume lost history/model/system")
	}
	grant, err := inner.desktopATIS.desktopRecords.Remote(accountRuntimeForAuth(t, e, auth.ID).recordOwner, second.ID, second.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	storedModel, storedDefault, storedSystem, err := grant.Configuration()
	if err != nil || storedModel != wantModel || storedDefault != "claude-opus-5" || storedSystem != "SAVED_SYSTEM" {
		t.Fatal("worker restoration did not reach durable configuration", err)
	}
	for _, text := range []string{"saved-one", "reply-1", "saved-two", "reply-2", "saved-three"} {
		if hydrateRemote && (!options.deltaEnabled || options.rejectAnchor) && text == "reply-1" {
			text = "REMOTE_EDITED_REPLY"
		}
		if !bytes.Contains(body, []byte(text)) {
			t.Fatal("history missing", text)
		}
	}
	if hydrateRemote && ((!options.deltaEnabled || options.rejectAnchor) && bytes.Contains(body, []byte("reply-1")) || inner.desktopControlPlane.Status().WorkerHydrationFailed != 0) {
		t.Fatal("server revision not used or hydration degraded")
	}
	after := inner.desktopPrompts.NativeContent(accountScope, first.SDKSessionID)
	if len(after.Messages) != 6 || after.IncompleteReason != "" || after.PersistenceError {
		t.Fatal("resumed wire poisoned tracker or replayed native rows", len(after.Messages), after.IncompleteReason, after.PersistenceError)
	}
	for i, row := range before.Messages {
		if after.Messages[i].UUID != row.UUID || !bytes.Equal(after.Messages[i].Message, row.Message) {
			t.Fatal("resume rewrote immutable original rows")
		}
	}
	stop(e, second)
	e.Close()
	restored := newClaudeAccountTestExecutor(e.cfg)
	t.Cleanup(restored.Close)
	prepareExecutionSessionAccountTest(t, restored, auths)
	install(restored)
	list, err := restored.ListDesktopSessions(auth.ID)
	if err != nil || len(list) != 1 || list[0].QueryID != "" || list[0].Generation != second.Generation || list[0].Running {
		t.Fatal("restart did not restore exact durable observation", list, err)
	}
	if _, err := restored.ResumeDesktopSession(ctx, auths[1].ID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: first.ID, ExpectedGeneration: second.Generation}); !errors.Is(err, claudesessions.ErrNotFound) {
		t.Fatal("foreign account resumed history", err)
	}
	if _, err := restored.ResumeDesktopSession(ctx, auth.ID, operation); !errors.Is(err, claudesessions.ErrStaleQuery) {
		t.Fatal("old page admitted after restart", err)
	}
	third, err := restored.ResumeDesktopSession(ctx, auth.ID, cliproxyexecutor.ClaudeDesktopSessionResume{SessionID: first.ID, ExpectedGeneration: second.Generation})
	if err != nil {
		// Structural diagnostics only; never log transcript text or credentials.
		failedInner := accountRuntimeForAuth(t, restored, auth.ID).executor
		failed := failedInner.desktopPrompts.NativeContent(accountScope, first.SDKSessionID)
		t.Logf("failed recovery: original=%d projected=%d active=%d issue=%q persistence=%t", len(failed.Messages), len(failed.ProjectedMessages), len(failed.ActiveUUIDs), failed.IncompleteReason, failed.PersistenceError)
		for _, row := range append(failed.Messages, failed.ProjectedMessages...) {
			parent := ""
			if row.ParentUUID != nil {
				parent = *row.ParentUUID
			}
			t.Logf("recovery row: type=%s uuid=%s parent=%s message_id=%s stop=%s", row.Type, row.UUID, parent, gjson.GetBytes(row.Message, "id").String(), gjson.GetBytes(row.Message, "stop_reason").String())
		}
		t.Logf("recovery active: %v", failed.ActiveUUIDs)
		t.Fatal("actual restart resume", err)
	}
	wire = awaitRemoteExecutor(t, streams)
	if third.SDKSessionID != first.SDKSessionID || third.QueryID == second.QueryID || calls.Load() != 3 || creates.Load() != 0 {
		t.Fatal("restart minted another session or invoked inference")
	}
	body = turn(restored, 5, "saved-four", 1)
	if options.deltaEnabled && (!bytes.Contains(body, []byte("REMOTE_DELTA_USER")) || !bytes.Contains(body, []byte("REMOTE_DELTA_REPLY"))) {
		t.Fatal("process reconstruction lost remote delta content")
	}
	if gjson.GetBytes(body, "messages.#").Int() != 8+extraHistory || gjson.GetBytes(body, "model").String() != wantModel || !bytes.Contains(body, []byte("reply-3")) || !bytes.Contains(body, []byte("SAVED_SYSTEM")) || bytes.Contains(body, []byte("UNOWNED_WORKER_SYSTEM")) {
		t.Fatal("restarted actor began with empty history")
	}
	stop(restored, third)
	final := accountRuntimeForAuth(t, restored, auth.ID).executor
	loaded, err := final.desktopPrompts.RestoreNativeContent(accountScope, first.SDKSessionID)
	if err != nil || int64(len(loaded.ActiveMessages)) != 8+extraHistory {
		t.Fatal("second-generation history did not persist", err)
	}
	// Independent protected reader: no raw transcript bytes leave the test.
	scopeJSON, _ := json.Marshal([]string{accountScope, first.SDKSessionID})
	scopeHash := sha256.Sum256(scopeJSON)
	index, err := helps.NewClaudeDesktopTranscriptStore(final.desktopDurableStatePath, e.cfg.ProxyURL).LoadTranscript(hex.EncodeToString(scopeHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	protected, err := os.ReadFile(index.Path)
	if err != nil || bytes.Contains(protected, []byte("saved-one")) || bytes.Contains(protected, []byte("SAVED_SYSTEM")) {
		t.Fatal("history leaked at rest", err)
	}
	wantDelta, wantAgents := int32(0), int32(3)
	if options.deltaEnabled {
		wantDelta = 2
		if options.skipSubagentsOnDelta {
			wantAgents = 1
			if options.rejectAnchor {
				wantAgents++
			}
		}
	}
	if deltaReads.Load() != wantDelta || subagentReads.Load() != wantAgents {
		t.Fatal("actual hydration ignored owned feature policy", deltaReads.Load(), subagentReads.Load(), wantDelta, wantAgents)
	}
}
