package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestNativeUsageMatchesPinnedSource(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk-native-usage.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string            `json:"name"`
			Updates  []json.RawMessage `json:"updates"`
			Expected any               `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Cases) != 7 {
		t.Fatal("native usage fixture missing", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			value := nativeUsageMerge(nil, nil)
			for _, update := range tc.Updates {
				value = nativeUsageMerge(value, update)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var actual any
			_ = json.Unmarshal(encoded, &actual)
			if !reflect.DeepEqual(actual, tc.Expected) {
				t.Fatalf("native usage mismatch: got %s", encoded)
			}
		})
	}
}

func nativeContentTestTracker(structural, content SDKSessionStore) Tracker {
	return NewTracker(structural, SDKNativeContentOptions{Store: content, Version: "2.1.247", Entrypoint: "claude-desktop", Cwd: "C:/synthetic/owned"})
}

func observeNativeTestResponse(request *Request, response *Response) {
	request.ObserveSDKHistory(response.SDKHistoryMessages())
	request.ObserveNativeContent(response.NativeContentMessages())
	request.ObserveSDKWireResponse(response.SDKWireFingerprint())
}

func TestNativeContentOwnedInputResponseAndRestart(t *testing.T) {
	structural, content := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
	tracker := nativeContentTestTracker(structural, content)
	at := time.Now().UTC().Truncate(time.Millisecond)
	input := sdkStateTestInput(`{"model":"synthetic","metadata":{"host_path":"C:/not-opened"},"messages":[{"role":"user","content":[{"type":"text","text":"PRIVATE_INPUT"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"PRIVATE_IMAGE"}}]}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	response.EnableNativeContent()
	response.SetNativeRequestID("req_synthetic_native")
	response.ObservePayloadAt([]byte(`{"type":"message","id":"msg_native","model":"synthetic","role":"assistant","content":[{"type":"text","text":"PRIVATE_REPLY"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`), false, at.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(at.Add(time.Millisecond), "end_turn", nil)
	request.RecordSDKAPISuccess(1)
	snapshot := tracker.NativeContent(input.AccountID, input.SessionID)
	if snapshot.IncompleteReason != "" || snapshot.PersistenceError || len(snapshot.Messages) != 2 || len(snapshot.ActiveUUIDs) != 2 {
		t.Fatalf("content ownership failed: count=%d issue=%q persisted=%v", len(snapshot.Messages), snapshot.IncompleteReason, snapshot.PersistenceError)
	}
	if snapshot.Messages[0].UUID != request.SDKHistory().Messages[0].UUID || snapshot.Messages[0].Timestamp != nativeContentTimestamp(at) ||
		snapshot.Messages[0].ParentUUID != nil || snapshot.Messages[0].PromptID != request.Identity().PromptID ||
		snapshot.Messages[1].ParentUUID == nil || *snapshot.Messages[1].ParentUUID != snapshot.Messages[0].UUID ||
		snapshot.Messages[1].Timestamp != nativeContentTimestamp(at.Add(time.Millisecond)) || snapshot.Messages[1].RequestID != "req_synthetic_native" {
		t.Fatal("native identity, parent or observation time was reconstructed incorrectly")
	}
	if !bytes.Contains(snapshot.Messages[0].Message, []byte("PRIVATE_IMAGE")) || !bytes.Contains(snapshot.Messages[1].Message, []byte("PRIVATE_REPLY")) {
		t.Fatal("actual native content was discarded")
	}
	scope := digest(input.AccountID, input.SessionID)
	if bytes.Contains(structural.data[scope], []byte("PRIVATE_")) || bytes.Contains(content.data[scope], []byte("host_path")) || bytes.Contains(content.data[scope], []byte("C:/not-opened")) {
		t.Fatal("message ownership retained the request envelope or contaminated structural storage")
	}
	copySnapshot := tracker.NativeContent(input.AccountID, input.SessionID)
	copySnapshot.Messages[0].Message[0] = 'X'
	*copySnapshot.Messages[1].ParentUUID = "changed"
	if !reflect.DeepEqual(snapshot, tracker.NativeContent(input.AccountID, input.SessionID)) {
		t.Fatal("snapshot mutated the owner")
	}
	restarted := nativeContentTestTracker(structural, content)
	next := input
	next.ClientRequestID, next.PromptID = "new-request", ""
	next.StartedAt = at.Add(2 * time.Hour)
	next.Body = []byte(`{"messages":[{"role":"user","content":"new input"}]}`)
	restarted.Begin(next)
	got := restarted.NativeContent(input.AccountID, input.SessionID)
	if got.IncompleteReason != "" || got.PersistenceError || len(got.Messages) != 3 || !reflect.DeepEqual(got.Messages[:2], snapshot.Messages) {
		t.Fatal("restart failed to preserve exact owned content, timestamps or parents")
	}
	if foreign := restarted.NativeContent("different-account", input.SessionID); len(foreign.Messages) != 0 {
		t.Fatal("native content crossed account boundaries")
	}
}

func TestNativeContentStreamYieldsMutableUsageAndToolParent(t *testing.T) {
	tracker := nativeContentTestTracker(&sdkMemorySessionStore{}, &sdkMemorySessionStore{})
	at := time.Now().UTC().Truncate(time.Millisecond)
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"run synthetic tool"}]}`, at)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	response.EnableNativeContent()
	lines := []string{
		`data: {"type":"message_start","message":{"id":"msg_tools","type":"message","role":"assistant","model":"synthetic","usage":{"input_tokens":20,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool_a","name":"Read","input":{"discarded":true}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"C:/synthetic/not-opened\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"discarded"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"synthetic final text"}}`,
		`data: {"type":"content_block_stop","index":1}`,
	}
	for i, line := range lines {
		response.ObserveStreamLineAt([]byte(line), at.Add(time.Duration(i+1)*time.Millisecond))
		observeNativeTestResponse(request, &response)
	}
	before := tracker.NativeContent(input.AccountID, input.SessionID)
	if len(before.Messages) != 3 || before.Messages[1].Timestamp == before.Messages[2].Timestamp ||
		!bytes.Contains(before.Messages[1].Message, []byte("C:/synthetic/not-opened")) || bytes.Contains(before.Messages[1].Message, []byte("discarded")) {
		t.Fatal("stream block ownership did not follow the native reducer")
	}
	response.ObserveStreamLineAt([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_details":{"kind":"synthetic","fallback_credit_token":"not-retained"}},"usage":{"input_tokens":0,"output_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":4},"server_tool_use":{"web_search_requests":2}}}`), at.Add(8*time.Millisecond))
	response.ObserveStreamLineAt([]byte(`data: {"type":"message_stop"}`), at.Add(9*time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(at.Add(9*time.Millisecond), "tool_use", []string{"tool_a"})
	request.RecordSDKAPISuccess(9)
	after := tracker.NativeContent(input.AccountID, input.SessionID)
	for i := 1; i < len(after.Messages); i++ {
		var value struct {
			Usage      struct{ InputTokens, OutputTokens, CacheCreationInputTokens int }
			StopReason string `json:"stop_reason"`
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(after.Messages[i].Message, &fields)
		var usage map[string]int
		_ = json.Unmarshal(fields["usage"], &usage)
		_ = json.Unmarshal(after.Messages[i].Message, &value)
		if usage["input_tokens"] != 20 || usage["output_tokens"] != 7 || usage["cache_creation_input_tokens"] != 4 || value.StopReason != "tool_use" || bytes.Contains(after.Messages[i].Message, []byte("not-retained")) ||
			after.Messages[i].UUID != before.Messages[i].UUID || after.Messages[i].Timestamp != before.Messages[i].Timestamp {
			t.Fatal("native message_delta fields or immutable yield identity were lost")
		}
	}
	next := input
	next.ClientRequestID, next.StartedAt = "tool-continuation", at.Add(time.Second)
	next.Body = []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_a","content":"synthetic result"}]}]}`)
	continuation := tracker.Begin(next)
	got := tracker.NativeContent(input.AccountID, input.SessionID)
	result := got.Messages[len(got.Messages)-1]
	if continuation.Identity().StartsPrompt || result.SourceToolAssistantUUID != before.Messages[1].UUID || result.ParentUUID == nil || *result.ParentUUID != before.Messages[1].UUID {
		t.Fatal("tool result parent was incorrectly set to the last assistant block")
	}
}

func TestNativeContentStaleCancellationAndCorruption(t *testing.T) {
	for _, scenario := range []string{"late-attempt", "save-failure", "corrupt", "missing-content", "stale-writer"} {
		t.Run(scenario, func(t *testing.T) {
			structural, content := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
			tracker := nativeContentTestTracker(structural, content)
			input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic input"}]}`, time.Now())
			request := tracker.Begin(input)
			request.ObserveSDKQuery(input.Body)
			scope := digest(input.AccountID, input.SessionID)
			if scenario == "late-attempt" {
				next := input
				next.Attempt = 2
				current := tracker.Begin(next)
				request.ObserveNativeContent([]SDKNativeMessage{{UUID: "foreign"}}, "late-error")
				if current.SDKSessionStateError() != nil || len(tracker.NativeContent(input.AccountID, input.SessionID).Messages) != 1 {
					t.Fatal("obsolete attempt changed content state")
				}
				current.FinalizeCancellation(input.StartedAt.Add(time.Second))
				current.FinalizeCancellation(input.StartedAt.Add(2 * time.Second))
				if snapshot := tracker.NativeContent(input.AccountID, input.SessionID); len(snapshot.Messages) != 2 || !bytes.Contains(snapshot.Messages[1].Message, []byte("[Request interrupted by user]")) {
					t.Fatal("owned cancellation marker missing or duplicated")
				}
				return
			}
			if scenario == "save-failure" {
				content.failSave = errors.New("synthetic write failure")
				request.FinalizeCancellation(time.Now())
				if request.SDKSessionStateError() == nil {
					t.Fatal("native content write failure stayed healthy")
				}
				return
			}
			request.FinalizeCancellation(time.Now())
			if scenario == "corrupt" {
				content.data[scope] = []byte("synthetic corrupt content")
			} else if scenario == "missing-content" {
				delete(content.data, scope)
				delete(content.revisions, scope)
			} else {
				content.revisions[scope] = "newer-owner"
				tracker.nativeContent[scope].dirty = true
				if request.CheckpointSDKSessionState() == nil {
					t.Fatal("stale native content writer overwrote the newer owner")
				}
				return
			}
			original := bytes.Clone(content.data[scope])
			restarted := nativeContentTestTracker(structural, content)
			next := input
			next.ClientRequestID, next.StartedAt = "next-request", time.Now().Add(time.Second)
			newRequest := restarted.Begin(next)
			if newRequest.SDKSessionStateError() == nil || !bytes.Equal(original, content.data[scope]) {
				t.Fatal("corrupt/missing native content silently reconstructed or overwritten")
			}
		})
	}
}
