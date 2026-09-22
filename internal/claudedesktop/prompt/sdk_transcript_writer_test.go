package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

type sdkTranscriptMemoryStore struct {
	mu      sync.Mutex
	indices map[string]SDKTranscriptIndex
	appends []struct {
		scope string
		lines []byte
	}
	before func(string, []byte) error
}

func (s *sdkTranscriptMemoryStore) LoadTranscript(scope string) (SDKTranscriptIndex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.indices[scope]
	value.UUIDs = append([]string(nil), value.UUIDs...)
	return value, nil
}

func (s *sdkTranscriptMemoryStore) AppendTranscript(scope, revision string, lines []byte) (string, error) {
	if s.before != nil {
		if err := s.before(scope, lines); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indices == nil {
		s.indices = make(map[string]SDKTranscriptIndex)
	}
	if revision != s.indices[scope].Revision {
		return "", ErrSDKSessionStale
	}
	s.appends = append(s.appends, struct {
		scope string
		lines []byte
	}{scope, bytes.Clone(lines)})
	next := fmt.Sprint(len(s.appends))
	index := s.indices[scope]
	index.Revision = next
	for _, line := range bytes.Split(bytes.TrimSpace(lines), []byte{'\n'}) {
		var row struct {
			UUID string `json:"uuid"`
		}
		_ = json.Unmarshal(line, &row)
		if row.UUID != "" {
			index.UUIDs = append(index.UUIDs, row.UUID)
		}
	}
	s.indices[scope] = index
	return next, nil
}

func pausedTranscriptWriter(t *testing.T, s SDKTranscriptStore) *sdkTranscriptWriter {
	t.Helper()
	w := newSDKTranscriptWriter(s)
	w.after = func(_ time.Duration, f func()) *time.Timer {
		timer := time.AfterFunc(time.Hour, f)
		timer.Stop()
		return timer
	}
	t.Cleanup(func() { _ = w.close() })
	return w
}

func transcriptJSON(value string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(value + "\n"), nil }
}

func assertTranscriptSettled(t *testing.T, ch <-chan struct{}, settled bool) {
	t.Helper()
	select {
	case <-ch:
		if !settled {
			t.Fatal("queue settled before append completed")
		}
	default:
		if settled {
			t.Fatal("queue waiter was not settled")
		}
	}
}

func TestSDKTranscriptDeferredSerializationTimerAndDedup(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	w := pausedTranscriptWriter(t, store)
	w.load("main")
	var callback func()
	count := 0
	w.after = func(delay time.Duration, f func()) *time.Timer {
		if delay != 100*time.Millisecond {
			t.Error("wrong native flush interval", delay)
		}
		count++
		callback = f
		timer := time.AfterFunc(time.Hour, f)
		timer.Stop()
		return timer
	}
	value := `{"usage":{"output_tokens":0}}`
	first := w.enqueue("main", "one", func() ([]byte, error) { return []byte(value + "\n"), nil })
	w.enqueue("main", "two", transcriptJSON(`{"text":"second"}`))
	duplicate := w.enqueue("main", "one", transcriptJSON(`{"text":"duplicate"}`))
	if count != 1 || len(store.appends) != 0 {
		t.Fatal("enqueue wrote eagerly or scheduled duplicate timers")
	}
	assertTranscriptSettled(t, first, false)
	assertTranscriptSettled(t, duplicate, true)
	value = `{"usage":{"output_tokens":7}}`
	callback()
	assertTranscriptSettled(t, first, true)
	value = `{"usage":{"output_tokens":9}}`
	if len(store.appends) != 1 || string(store.appends[0].lines) != `{"usage":{"output_tokens":7}}`+"\n"+`{"text":"second"}`+"\n" {
		t.Fatal("disk rows did not snapshot at drain")
	}
	if w.timer != nil {
		t.Fatal("fired timer was not cleared")
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	assertTranscriptSettled(t, w.enqueue("main", "late", transcriptJSON(`{}`)), true)
	if len(store.appends) != 1 {
		t.Fatal("shutdown accepted a new append")
	}
}

func TestSDKTranscriptChunkUsesUTF16AndPathInsertionOrder(t *testing.T) {
	line := `{"text":"😀"}` + "\n"
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			store := &sdkTranscriptMemoryStore{}
			w := pausedTranscriptWriter(t, store)
			w.load("a")
			w.load("b")
			w.chunkSize = 2 * len(utf16.Encode([]rune(line)))
			if !exact {
				w.chunkSize++
			}
			w.enqueue("a", "1", transcriptJSON(strings.TrimSuffix(line, "\n")))
			w.enqueue("b", "2", transcriptJSON(`{}`))
			w.enqueue("a", "3", transcriptJSON(strings.TrimSuffix(line, "\n")))
			w.drain()
			want := []string{"a", "b"}
			if exact {
				want = []string{"a", "a", "b"}
			}
			var actual []string
			for _, a := range store.appends {
				actual = append(actual, a.scope)
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatal("native chunk/path order differs", actual, want)
			}
		})
	}
}

func TestSDKTranscriptFailureSettlesButRemainsVisible(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	w := pausedTranscriptWriter(t, store)
	w.load("main")
	fail := true
	store.before = func(string, []byte) error {
		if fail {
			return errors.New("synthetic failure")
		}
		return nil
	}
	observed := 0
	w.observeFailure("main", func() { observed++ })
	done := w.enqueue("main", "1", transcriptJSON(`{}`))
	w.drain()
	assertTranscriptSettled(t, done, true)
	if observed != 1 || w.failure("main") == nil || len(store.appends) != 0 {
		t.Fatal("failure became a successful append")
	}
	w.observeFailure("main", func() { observed++ })
	if observed != 2 {
		t.Fatal("late observer missed completed failure")
	}
	fail = false
	w.enqueue("main", "1", transcriptJSON(`{"duplicate":true}`))
	w.enqueue("main", "2", transcriptJSON(`{"next":true}`))
	w.drain()
	if len(store.appends) != 1 || string(store.appends[0].lines) != `{"next":true}`+"\n" || w.failure("main") == nil {
		t.Fatal("failed append was silently retried or health cleared")
	}
}

func TestSDKTranscriptNewPathDuringAppendIsVisitedInSameDrain(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	w := pausedTranscriptWriter(t, store)
	w.load("a")
	w.load("b")
	var late <-chan struct{}
	store.before = func(scope string, _ []byte) error {
		if scope == "a" {
			late = w.enqueue("b", "2", transcriptJSON(`{"second":true}`))
		}
		return nil
	}
	w.enqueue("a", "1", transcriptJSON(`{"first":true}`))
	w.drain()
	if len(store.appends) != 2 || store.appends[0].scope != "a" || store.appends[1].scope != "b" {
		t.Fatal("native Map insertion order was reduced to a stale path snapshot")
	}
	assertTranscriptSettled(t, late, true)
}

func TestSDKTranscriptDrainSerializesLateWorkAndClose(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	w := pausedTranscriptWriter(t, store)
	w.load("main")
	started, release := make(chan struct{}), make(chan struct{})
	count := 0
	store.before = func(string, []byte) error {
		count++
		if count == 1 {
			close(started)
			<-release
		}
		return nil
	}
	first := w.enqueue("main", "1", transcriptJSON(`{"first":true}`))
	drained := make(chan struct{})
	go func() { w.drain(); close(drained) }()
	<-started
	second := w.enqueue("main", "2", transcriptJSON(`{"second":true}`))
	assertTranscriptSettled(t, first, false)
	assertTranscriptSettled(t, second, false)
	closed := make(chan struct{})
	go func() { _ = w.close(); close(closed) }()
	assertTranscriptSettled(t, closed, false)
	close(release)
	<-drained
	<-closed
	assertTranscriptSettled(t, first, true)
	assertTranscriptSettled(t, second, true)
	if len(store.appends) != 2 || string(store.appends[0].lines) != `{"first":true}`+"\n" || string(store.appends[1].lines) != `{"second":true}`+"\n" {
		t.Fatal("late entry was not drained separately")
	}
}

func TestSDKTranscriptSerializerFailureSettlesRemainingGroup(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	w := pausedTranscriptWriter(t, store)
	w.load("main")
	first := w.enqueue("main", "1", transcriptJSON(`{}`))
	second := w.enqueue("main", "2", func() ([]byte, error) { return nil, ErrSDKSessionInvalid })
	third := w.enqueue("main", "3", transcriptJSON(`{}`))
	w.drain()
	for _, done := range []<-chan struct{}{first, second, third} {
		assertTranscriptSettled(t, done, true)
	}
	if len(store.appends) != 0 || w.failure("main") == nil {
		t.Fatal("serialization failure was silently ignored")
	}
}

func TestSDKTranscriptRetiredScopeDrainsBeforeReleasingIndex(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	w := pausedTranscriptWriter(t, store)
	w.load("main")
	done := w.enqueue("main", "one", transcriptJSON(`{}`))
	w.retireScope("main")
	if w.scopes["main"] == nil {
		t.Fatal("retirement discarded pending native writes")
	}
	w.drain()
	assertTranscriptSettled(t, done, true)
	if w.scopes["main"] != nil {
		t.Fatal("inactive scope retained its index and observer")
	}
	w.load("main")
	if w.scopes["main"].revision != "1" {
		t.Fatal("scope reload did not use durable index")
	}
	w.recordFailure("main", ErrSDKSessionUnavailable)
	w.retireScope("main")
	if w.scopes["main"] == nil || w.failure("main") == nil {
		t.Fatal("retirement erased failed writer health")
	}
}

func TestSDKTranscriptLiveContentNotCheckpointOrRenderer(t *testing.T) {
	store := &sdkTranscriptMemoryStore{}
	tracker := NewTracker(&sdkMemorySessionStore{}, SDKNativeContentOptions{Store: &sdkMemorySessionStore{}, TranscriptStore: store})
	tracker.transcript = pausedTranscriptWriter(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"PRIVATE_LIVE_CONTENT"}]}`, at)
	request := tracker.Begin(input)
	t.Cleanup(func() { _ = tracker.Close() })
	var response Response
	response.EnableNativeContent()
	for _, line := range []string{
		`data: {"type":"message_start","message":{"id":"msg_live","model":"synthetic","role":"assistant","usage":{"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"reply"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	} {
		response.ObserveStreamLineAt([]byte(line), at)
		observeNativeTestResponse(request, &response)
	}
	response.ObserveStreamLineAt([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`), at.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	if len(store.appends) != 1 || !bytes.Contains(store.appends[0].lines, []byte("PRIVATE_LIVE_CONTENT")) || bytes.Contains(store.appends[0].lines, []byte("active_uuids")) || !bytes.Contains(store.appends[0].lines, []byte(`"output_tokens":7`)) {
		t.Fatal("transcript did not use real messages independently of checkpoint")
	}
	written := bytes.Clone(store.appends[0].lines)
	response.ObserveStreamLineAt([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`), at.Add(2*time.Millisecond))
	observeNativeTestResponse(request, &response)
	if !bytes.Contains(tracker.NativeContent(input.AccountID, input.SessionID).Messages[1].Message, []byte(`"output_tokens":9`)) || !bytes.Equal(store.appends[0].lines, written) {
		t.Fatal("later live usage did not remain distinct from the already written row")
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	if tracker.Begin(input) != nil {
		t.Fatal("closed tracker accepted new native work")
	}
}

func TestSDKTranscriptRestartDoesNotInventLostHistory(t *testing.T) {
	for _, loss := range []string{"prefix", "checkpoint-tail"} {
		t.Run(loss, func(t *testing.T) {
			structural, content := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}
			store := &sdkTranscriptMemoryStore{}
			options := SDKNativeContentOptions{Store: content, TranscriptStore: store}
			tracker := NewTracker(structural, options)
			tracker.transcript = pausedTranscriptWriter(t, store)
			input := sdkStateTestInput(`{"messages":[{"role":"user","content":"synthetic previous"}]}`, time.Now())
			request := tracker.Begin(input)
			request.FinalizeCancellation(time.Now())
			if err := tracker.Close(); err != nil {
				t.Fatal(err)
			}
			scope := digest(input.AccountID, input.SessionID)
			store.mu.Lock()
			index := store.indices[scope]
			if loss == "prefix" {
				index.UUIDs = index.UUIDs[1:]
			} else {
				index.UUIDs = append(index.UUIDs, "synthetic-uncheckpointed-tail")
			}
			store.indices[scope] = index
			store.mu.Unlock()
			restarted := NewTracker(structural, options)
			restarted.transcript = pausedTranscriptWriter(t, store)
			next := input
			next.ClientRequestID = "new-request"
			next.PromptID = ""
			next.Body = []byte(`{"messages":[{"role":"user","content":"synthetic next"}]}`)
			if restarted.Begin(next) == nil {
				t.Fatal("missing transcript incorrectly rejected model request")
			}
			snapshot := restarted.NativeContent(input.AccountID, input.SessionID)
			if !strings.Contains(snapshot.IncompleteReason, "sdk-transcript-") || restarted.SDKSessionStateError(input.AccountID, input.SessionID) == nil {
				t.Fatal("missing transcript history became healthy")
			}
			if err := restarted.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			if len(store.appends) != 2 || bytes.Contains(store.appends[1].lines, []byte("synthetic previous")) {
				t.Fatal("historical content was backfilled at an invented time")
			}
		})
	}
}
