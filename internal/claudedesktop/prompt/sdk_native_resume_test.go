package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

type sdkReadableTranscriptStore struct {
	sdkTranscriptMemoryStore
	mutate func([]byte) []byte
	fail   error
}

func (s *sdkReadableTranscriptStore) ReadTranscript(scope string, visit func([]byte) error) (SDKTranscriptIndex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, append := range s.appends {
		if append.scope != scope {
			continue
		}
		lines := bytes.Clone(append.lines)
		if s.mutate != nil {
			lines = s.mutate(lines)
		}
		if err := visit(lines); err != nil {
			return SDKTranscriptIndex{}, err
		}
	}
	index := s.indices[scope]
	index.UUIDs = append([]string(nil), index.UUIDs...)
	return index, s.fail
}

func nativeResumeTestState(t *testing.T) (*Tracker, Input, *sdkMemorySessionStore, *sdkMemorySessionStore, *sdkReadableTranscriptStore) {
	t.Helper()
	structural, content, transcript := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}, &sdkReadableTranscriptStore{}
	tracker := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript,
		Version: "2.1.247", Entrypoint: "claude-desktop", Cwd: "C:/synthetic"})
	tracker.transcript = pausedTranscriptWriter(t, transcript)
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":[{"type":"text","text":"PRIVATE_INPUT"},{"type":"image","source":{"type":"base64","data":"PRIVATE_IMAGE"}}]}]}`, time.Now().UTC().Truncate(time.Millisecond))
	input.SessionID = uuid.NewString()
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"id":"msg_native_resume","type":"message","role":"assistant","model":"synthetic","content":[{"type":"text","text":"PRIVATE_REPLY"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`), false, input.StartedAt.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(input.StartedAt.Add(time.Millisecond), "end_turn", nil)
	request.RecordSDKAPISuccess(1)
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	return &tracker, input, structural, content, transcript
}

func TestNativeResumeRestoresWithoutInventingInputOrSaving(t *testing.T) {
	tracker, input, structural, content, transcript := nativeResumeTestState(t)
	want := tracker.NativeContent(input.AccountID, input.SessionID)
	structuralSaves, contentSaves, appends := structural.saves, content.saves, len(transcript.appends)
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	got, err := restarted.RestoreNativeContent(input.AccountID, input.SessionID)
	if err != nil || !reflect.DeepEqual(got.Snapshot, want) || len(got.ActiveMessages) != 2 || !got.HasCompletedTurns || got.TranscriptRevision == "" {
		t.Fatal("explicit native restore failed", err)
	}
	if len(restarted.prompts) != 1 || structural.saves != structuralSaves || content.saves != contentSaves || len(transcript.appends) != appends {
		t.Fatal("restore invented a request, checkpoint or append")
	}
	if raw, err := json.Marshal(got); err == nil || bytes.Contains(raw, []byte("PRIVATE")) {
		t.Fatal("sensitive restoration object can be exported as JSON")
	}
	got.ActiveMessages[0].Message[0] = 'X'
	*got.Snapshot.Messages[1].ParentUUID = "foreign"
	if !reflect.DeepEqual(restarted.NativeContent(input.AccountID, input.SessionID), want) {
		t.Fatal("restored snapshot mutated the owner")
	}
	for _, identity := range [][2]string{{"foreign", input.SessionID}, {input.AccountID, uuid.NewString()}} {
		if value, err := restarted.RestoreNativeContent(identity[0], identity[1]); err == nil || len(value.Snapshot.Messages) != 0 {
			t.Fatal("restore crossed account/session ownership")
		}
	}
}

func TestNativeResumeCheckpointRevisionsAreRevalidated(t *testing.T) {
	for _, kind := range []string{"structural", "content", "during-read"} {
		t.Run(kind, func(t *testing.T) {
			tracker, input, structural, content, transcript := nativeResumeTestState(t)
			defer tracker.Close()
			scope := digest(input.AccountID, input.SessionID)
			if _, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); err != nil {
				t.Fatal(err)
			}
			replace := func(store *sdkMemorySessionStore) {
				payload, revision, err := store.Load(scope)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Save(scope, revision, payload); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "structural":
				replace(structural)
			case "content":
				replace(content)
			case "during-read":
				transcript.mutate = func(lines []byte) []byte { replace(content); return lines }
			}
			got, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID)
			if !errors.Is(err, ErrSDKSessionStale) || len(got.ActiveMessages) != 0 {
				t.Fatal("cached checkpoints survived a new durable owner", err)
			}
		})
	}
}

func appendNativeResumeTestTurn(t *testing.T, tracker *Tracker, input Input) *Request {
	t.Helper()
	input.ClientRequestID, input.PromptID, input.StartedAt = uuid.NewString(), "", input.StartedAt.Add(time.Second)
	input.Body = []byte(`{"messages":[{"role":"user","content":"new owned input"}]}`)
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"id":"msg_resume_followup","type":"message","role":"assistant","model":"synthetic","content":[{"type":"text","text":"new owned reply"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":3}}`), false, input.StartedAt.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(input.StartedAt.Add(time.Millisecond), "end_turn", nil)
	return request
}

func TestNativeResumeSettlesLocalWritesAndRetainsCallbackOwnership(t *testing.T) {
	tracker, input, _, _, transcript := nativeResumeTestState(t)
	defer tracker.Close()
	request := appendNativeResumeTestTurn(t, tracker, input)
	before := len(transcript.appends)
	if _, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); !errors.Is(err, ErrSDKSessionActive) {
		t.Fatal("response completion retired a pending accounting callback", err)
	}
	if len(transcript.appends) != before {
		t.Fatal("active admission failure flushed the transcript")
	}
	request.RecordSDKAPISuccess(1)
	got, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID)
	if err != nil || len(got.ActiveMessages) != 4 || len(transcript.appends) != before+1 {
		t.Fatal("explicit restore did not settle the normal delayed append", err)
	}
	if got.Snapshot.IncompleteReason != "" || got.Snapshot.PersistenceError {
		t.Fatal("normal queued serialization was called corruption")
	}
	request.call.state.helperOwners++
	if _, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); !errors.Is(err, ErrSDKSessionActive) {
		t.Fatal("active helper was replaceable", err)
	}
	request.call.state.helperOwners--
}

func TestNativeResumeRechecksAdmissionAfterWaitingForFlush(t *testing.T) {
	tracker, input, _, _, transcript := nativeResumeTestState(t)
	defer tracker.Close()
	request := appendNativeResumeTestTurn(t, tracker, input)
	request.RecordSDKAPISuccess(1)
	entered, release := make(chan struct{}), make(chan struct{})
	transcript.before = func(string, []byte) error { close(entered); <-release; return nil }
	result := make(chan error, 1)
	go func() { _, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("restore never reached delayed local flush")
	}
	next := input
	next.ClientRequestID, next.PromptID, next.StartedAt = uuid.NewString(), "", input.StartedAt.Add(2*time.Second)
	next.Body = []byte(`{"messages":[{"role":"user","content":"new request while restore waits"}]}`)
	active := tracker.Begin(next)
	if active == nil {
		close(release)
		t.Fatal("flush retained the tracker lock")
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, ErrSDKSessionActive) {
			t.Fatal("restore bypassed the newer operation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restore did not finish after local flush")
	}
	transcript.before = nil
	if health := tracker.NativeContent(input.AccountID, input.SessionID); health.IncompleteReason != "" || health.PersistenceError {
		t.Fatal("admission race poisoned valid history")
	}
}

func TestNativeResumeRejectsMatchingUUIDWithDifferentContent(t *testing.T) {
	for _, scenario := range []string{"user", "assistant", "image", "parent", "order", "unknown-field", "duplicate-row", "missing-row", "late-read-error", "metadata-owner"} {
		t.Run(scenario, func(t *testing.T) {
			tracker, input, structural, content, transcript := nativeResumeTestState(t)
			if err := tracker.Close(); err != nil {
				t.Fatal(err)
			}
			transcript.mutate = func(lines []byte) []byte {
				switch scenario {
				case "user":
					return bytes.ReplaceAll(lines, []byte("PRIVATE_INPUT"), []byte("DIFFERENT_INPUT"))
				case "assistant":
					return bytes.ReplaceAll(lines, []byte("PRIVATE_REPLY"), []byte("DIFFERENT_REPLY"))
				case "image":
					return bytes.ReplaceAll(lines, []byte("PRIVATE_IMAGE"), []byte("DIFFERENT_IMAGE"))
				case "parent":
					return bytes.ReplaceAll(lines, []byte(`"parentUuid":null`), []byte(`"parentUuid":"`+uuid.NewString()+`"`))
				case "unknown-field":
					return bytes.ReplaceAll(lines, []byte(`"type":"user"`), []byte(`"unknown_native_field":true,"type":"user"`))
				case "metadata-owner":
					return append(lines, []byte(`{"type":"atis-latch","atis":"","sessionId":"foreign"}`+"\n")...)
				case "duplicate-row":
					return append(lines, lines...)
				case "missing-row":
					return nil
				case "order":
					rows := bytes.Split(bytes.TrimSuffix(lines, []byte{'\n'}), []byte{'\n'})
					if len(rows) != 2 {
						t.Fatal("unexpected test append shape")
					}
					return append(append(append(rows[1], '\n'), rows[0]...), '\n')
				}
				return lines
			}
			if scenario == "late-read-error" {
				transcript.fail = ErrSDKSessionUnavailable
			}
			restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
			defer restarted.Close()
			got, err := restarted.RestoreNativeContent(input.AccountID, input.SessionID)
			if err == nil || len(got.ActiveMessages) != 0 || len(got.Snapshot.Messages) != 0 {
				t.Fatal("unverified provisional content escaped restore", err)
			}
			health := restarted.NativeContent(input.AccountID, input.SessionID)
			if health.IncompleteReason != "sdk-transcript-content-checkpoint-mismatch" || !health.PersistenceError {
				t.Fatal("content mismatch did not degrade health")
			}
		})
	}
}

func TestNativeResumeAssistantDeltaAndLastDurableLatch(t *testing.T) {
	tracker, input, structural, content, transcript := nativeResumeTestState(t)
	defer tracker.Close()
	transcript.mutate = func(lines []byte) []byte {
		lines = bytes.ReplaceAll(lines, []byte(`"stop_reason":"end_turn"`), []byte(`"stop_reason":null`))
		lines = bytes.ReplaceAll(lines, []byte(`"output_tokens":2`), []byte(`"output_tokens":0`))
		return append(lines, []byte(`{"type":"atis-latch","atis":"old","sessionId":"`+input.SessionID+`"}`+"\n"+`{"type":"atis-latch","atis":"","sessionId":"`+input.SessionID+`"}`+"\n")...)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	got, err := restarted.RestoreNativeContent(input.AccountID, input.SessionID)
	if err != nil || got.TranscriptATISLatch == nil || *got.TranscriptATISLatch != "" || !bytes.Contains(got.ActiveMessages[1].Message, []byte(`"output_tokens":2`)) {
		t.Fatal("native immutable content was confused with mutable usage or defined-empty latch", err)
	}
}

func TestNativeResumeDetectsChangedOrMissingTranscriptAfterLoad(t *testing.T) {
	for _, mode := range []string{"revision", "content", "missing"} {
		t.Run(mode, func(t *testing.T) {
			tracker, input, _, _, transcript := nativeResumeTestState(t)
			defer tracker.Close()
			if _, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); err != nil {
				t.Fatal(err)
			}
			scope := digest(input.AccountID, input.SessionID)
			switch mode {
			case "revision":
				index := transcript.indices[scope]
				index.Revision = "stale-after-load"
				transcript.indices[scope] = index
			case "content":
				transcript.mutate = func(lines []byte) []byte {
					return bytes.ReplaceAll(lines, []byte("PRIVATE_REPLY"), []byte("changed-after-load"))
				}
			case "missing":
				transcript.appends = nil
				delete(transcript.indices, scope)
			}
			got, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID)
			if err == nil || len(got.ActiveMessages) != 0 {
				t.Fatal("cached index authorized a changed transcript")
			}
		})
	}
}

func TestNativeResumeActiveAndInterruptedAreNotCompletedTurns(t *testing.T) {
	tracker, input, structural, content, transcript := nativeResumeTestState(t)
	defer tracker.Close()
	next := input
	next.ClientRequestID, next.PromptID, next.StartedAt = uuid.NewString(), "", input.StartedAt.Add(time.Second)
	next.Body = []byte(`{"messages":[{"role":"user","content":"pending input"}]}`)
	request := tracker.Begin(next)
	request.ObserveSDKQuery(next.Body)
	if _, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); !errors.Is(err, ErrSDKSessionActive) {
		t.Fatal("live query was eligible for restoration", err)
	}
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	got, err := restarted.RestoreNativeContent(input.AccountID, input.SessionID)
	if err == nil || got.HasCompletedTurns || len(got.ActiveMessages) != 0 {
		t.Fatal("crash-interrupted accounting was silently accepted")
	}
}

func TestNativeResumeCheckpointParentMustPrecedeChild(t *testing.T) {
	for _, mode := range []string{"self", "future", "logical-future", "tool-user-parent"} {
		t.Run(mode, func(t *testing.T) {
			tracker, input, structural, content, transcript := nativeResumeTestState(t)
			defer tracker.Close()
			scope := digest(input.AccountID, input.SessionID)
			var record sdkNativeContentRecord
			if err := json.Unmarshal(content.data[scope], &record); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "self":
				record.Messages[0].ParentUUID = &record.Messages[0].UUID
			case "future":
				record.Messages[0].ParentUUID = &record.Messages[1].UUID
			case "logical-future":
				record.Messages[0].LogicalParentUUID = &record.Messages[1].UUID
			case "tool-user-parent":
				record.Messages[1].SourceToolAssistantUUID = record.Messages[0].UUID
			}
			content.data[scope], _ = json.Marshal(record)
			restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
			defer restarted.Close()
			if got, err := restarted.RestoreNativeContent(input.AccountID, input.SessionID); err == nil || len(got.ActiveMessages) != 0 {
				t.Fatal("invalid native ancestry accepted")
			}
		})
	}
}
