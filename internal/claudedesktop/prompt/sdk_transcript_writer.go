package prompt

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// SDKTranscriptStore appends protected groups of JSONL, independently of the
// mutable recovery checkpoint. An index is restored from actual durable rows,
// not from the live content owner. Errors must not contain message content.
type SDKTranscriptStore interface {
	LoadTranscript(scope string) (SDKTranscriptIndex, error)
	AppendTranscript(scope, revision string, lines []byte) (string, error)
}

// SDKTranscriptReader visits independently authenticated append groups while
// holding the store's read boundary. The visitor must not reenter the store.
// All visited content is provisional until the complete read succeeds: a bad
// later frame invalidates the whole restoration, including its earlier prefix.
// Implementations and visitors must never include private content in errors.
type SDKTranscriptReader interface {
	ReadTranscript(scope string, visit func([]byte) error) (SDKTranscriptIndex, error)
}

type SDKTranscriptIndex struct {
	Path     string
	Revision string
	UUIDs    []string
}

const sdkTranscriptFlushInterval = 100 * time.Millisecond
const sdkTranscriptChunkLength = 104857600 // Native JavaScript UTF-16 length.

type sdkTranscriptItem struct {
	serialize func() ([]byte, error)
	done      chan struct{}
	onFailure func(error)
}

type sdkTranscriptScope struct {
	path          string
	revision      string
	known         map[string]bool
	loadErr       error
	err           error
	observe       func()
	retired       bool
	pendingBridge *SDKBridgeTranscriptRecord
	bridgeFailure func(error)
}

// One queue belongs to one runtime. The timer is cleared at callback entry;
// drainMu is the native drainChain equivalent, including after write failures.
// Queue completion means settled, not successful: native waiters also resolve
// after a failed append, while writer health records that separate outcome.
type sdkTranscriptWriter struct {
	mu         sync.Mutex
	drainMu    sync.Mutex
	store      SDKTranscriptStore
	queues     map[string][]sdkTranscriptItem
	order      []string
	scopes     map[string]*sdkTranscriptScope
	timer      *time.Timer
	sealed     bool
	chunkSize  int
	after      func(time.Duration, func()) *time.Timer
	atisStamps map[string]string
}

func newSDKTranscriptWriter(store SDKTranscriptStore) *sdkTranscriptWriter {
	return &sdkTranscriptWriter{store: store, queues: make(map[string][]sdkTranscriptItem),
		scopes: make(map[string]*sdkTranscriptScope), chunkSize: sdkTranscriptChunkLength, after: time.AfterFunc, atisStamps: make(map[string]string)}
}

func (w *sdkTranscriptWriter) load(scope string) *sdkTranscriptScope {
	w.mu.Lock()
	defer w.mu.Unlock()
	if state := w.scopes[scope]; state != nil {
		state.retired = false
		return state
	}
	index, err := w.store.LoadTranscript(scope)
	state := &sdkTranscriptScope{path: index.Path, revision: index.Revision, known: make(map[string]bool), loadErr: err, err: err}
	for _, id := range index.UUIDs {
		state.known[id] = true
	}
	w.scopes[scope] = state
	return state
}

func (w *sdkTranscriptWriter) enqueue(scope, id string, serialize func() ([]byte, error)) <-chan struct{} {
	return w.enqueueMessage(scope, id, serialize, false)
}

func (w *sdkTranscriptWriter) enqueueMessage(scope, id string, serialize func() ([]byte, error), sidechain bool) <-chan struct{} {
	done := make(chan struct{})
	w.mu.Lock()
	defer w.mu.Unlock()
	state := w.scopes[scope]
	if w.sealed || state == nil || state.loadErr != nil || (!sidechain && state.known[id]) {
		close(done)
		return done
	}
	if state.pendingBridge != nil {
		w.enqueueBridgeLocked(scope, *state.pendingBridge, state.bridgeFailure)
		state.pendingBridge, state.bridgeFailure = nil, nil
	}
	// Native main-transcript UUID deduplication occurs at enqueue, not after
	// disk success. A failure stays visible rather than inventing a redelivery.
	state.known[id] = true
	if _, exists := w.queues[scope]; !exists {
		w.order = append(w.order, scope)
	}
	w.queues[scope] = append(w.queues[scope], sdkTranscriptItem{serialize: serialize, done: done})
	w.scheduleLocked()
	return done
}

func (w *sdkTranscriptWriter) enqueueATIS(scope, sessionID string, latch *string) {
	if latch == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	state := w.scopes[scope]
	if w.sealed || state == nil || state.loadErr != nil {
		return
	}
	if state.path == "" {
		state.err = ErrSDKSessionUnavailable
		return
	}
	stamp := state.path + "\n" + *latch
	if previous, ok := w.atisStamps[scope]; ok && previous == stamp {
		return
	}
	// Native updates the stamp before append settles and never UUID-dedups an
	// atis-latch entry. Retain this process-local stamp across cache eviction,
	// including failures, but do not restore it from historical disk rows.
	w.atisStamps[scope] = stamp
	row := struct {
		Type      string `json:"type"`
		ATIS      string `json:"atis"`
		SessionID string `json:"sessionId"`
	}{"atis-latch", *latch, sessionID}
	if _, exists := w.queues[scope]; !exists {
		w.order = append(w.order, scope)
	}
	w.queues[scope] = append(w.queues[scope], sdkTranscriptItem{done: make(chan struct{}), serialize: func() ([]byte, error) {
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(row) != nil {
			return nil, ErrSDKSessionInvalid
		}
		return buffer.Bytes(), nil
	}})
	w.scheduleLocked()
}

func (w *sdkTranscriptWriter) scheduleLocked() {
	if w.timer != nil || w.sealed {
		return
	}
	w.timer = w.after(sdkTranscriptFlushInterval, func() {
		w.mu.Lock()
		w.timer = nil
		w.mu.Unlock()
		w.drain()
		w.mu.Lock()
		if len(w.queues) != 0 {
			w.scheduleLocked()
		}
		w.mu.Unlock()
	})
}

func (w *sdkTranscriptWriter) recordFailure(scope string, err error) {
	w.mu.Lock()
	state := w.scopes[scope]
	state.err = err
	observe := state.observe
	w.mu.Unlock()
	if observe != nil {
		observe()
	}
}

func (w *sdkTranscriptWriter) observeFailure(scope string, observe func()) {
	w.mu.Lock()
	state := w.scopes[scope]
	failed := state != nil && state.err != nil
	if state != nil {
		state.observe = observe
	}
	w.mu.Unlock()
	if failed && observe != nil {
		observe()
	}
}

func (w *sdkTranscriptWriter) failure(scope string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if state := w.scopes[scope]; state != nil {
		return state.err
	}
	return nil
}

func (w *sdkTranscriptWriter) drain() {
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	for position := 0; ; position++ {
		// Snapshot only this path's current queue, just as splice(0) does.
		// Native Map iteration visits newly added paths in this drain, while
		// new rows for an already visited path wait for the next one.
		w.mu.Lock()
		if position >= len(w.order) {
			w.mu.Unlock()
			break
		}
		scope := w.order[position]
		items := w.queues[scope]
		w.queues[scope] = nil
		state := w.scopes[scope]
		revision := state.revision
		w.mu.Unlock()
		settled := 0
		var chunk []byte
		units := 0
		flush := func(end int) error {
			if len(chunk) == 0 {
				return nil
			}
			next, err := w.store.AppendTranscript(scope, revision, chunk)
			if err != nil {
				return err
			}
			revision = next
			w.mu.Lock()
			state.revision = next
			w.mu.Unlock()
			for settled < end {
				close(items[settled].done)
				settled++
			}
			chunk, units = nil, 0
			return nil
		}
		var failure error
		for i, item := range items {
			line, err := item.serialize()
			if err != nil {
				failure = err
				break
			}
			length := sdkTranscriptUTF16Length(line)
			if units+length >= w.chunkSize && len(chunk) != 0 {
				if failure = flush(i); failure != nil {
					break
				}
			}
			chunk = append(chunk, line...)
			units += length
		}
		if failure == nil {
			failure = flush(len(items))
		}
		if failure != nil {
			w.recordFailure(scope, failure)
		}
		for settled < len(items) {
			if failure != nil && items[settled].onFailure != nil {
				items[settled].onFailure(failure)
			}
			close(items[settled].done)
			settled++
		}
	}
	w.mu.Lock()
	retained := w.order[:0]
	for _, scope := range w.order {
		if len(w.queues[scope]) == 0 {
			delete(w.queues, scope)
		} else {
			retained = append(retained, scope)
		}
	}
	w.order = retained
	for scope, state := range w.scopes {
		if _, queued := w.queues[scope]; !queued && state.retired && state.err == nil && state.pendingBridge == nil {
			delete(w.scopes, scope)
		}
	}
	w.mu.Unlock()
}

func sdkTranscriptUTF16Length(value []byte) int {
	length := 0
	for _, r := range string(value) {
		length++
		if r > 0xffff {
			length++
		}
	}
	return length
}

func (w *sdkTranscriptWriter) retireScope(scope string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if state := w.scopes[scope]; state != nil {
		state.retired = true
		if _, queued := w.queues[scope]; !queued && state.err == nil && state.pendingBridge == nil {
			delete(w.scopes, scope)
		}
	}
}

func (w *sdkTranscriptWriter) close() error {
	w.mu.Lock()
	w.sealed = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	w.drain()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, state := range w.scopes {
		if state.err != nil {
			return ErrSDKSessionUnavailable
		}
	}
	return nil
}

func (t *Tracker) loadNativeTranscriptLocked(scope string, n *sdkNativeContent) {
	if t.nativeOptions.TranscriptStore == nil {
		return
	}
	if t.transcript == nil {
		t.transcript = newSDKTranscriptWriter(t.nativeOptions.TranscriptStore)
	}
	state := t.transcript.load(scope)
	if state.loadErr != nil {
		n.unknown("sdk-transcript-unavailable")
		return
	}
	for _, row := range n.rows {
		if !state.known[row.UUID] {
			// An old checkpoint is not proof of a previous native disk append.
			// Never backfill historical rows with invented serialization timing.
			n.unknown("sdk-transcript-prefix-unavailable")
			return
		}
	}
	if len(state.known) != len(n.rows) {
		n.unknown("sdk-transcript-checkpoint-tail-unavailable")
		return
	}
	if reader, ok := t.nativeOptions.TranscriptStore.(SDKTranscriptReader); ok {
		if _, err := verifyNativeTranscript(reader, scope, n, state.revision, nil); err != nil {
			n.unknown("sdk-transcript-content-checkpoint-mismatch")
			n.persisted.err, n.persisted.loadFailed = err, true
		}
	}
}

func (t *Tracker) enqueueNativeTranscriptLocked(scope string, n *sdkNativeContent, id string) {
	if t.transcript == nil {
		return
	}
	t.transcript.enqueueATIS(scope, n.sessionID, n.atisLatch)
	t.transcript.enqueue(scope, id, func() ([]byte, error) {
		t.mu.Lock()
		index, exists := n.byUUID[id]
		if !exists {
			t.mu.Unlock()
			return nil, ErrSDKSessionInvalid
		}
		row := cloneNativeMessage(n.rows[index])
		t.mu.Unlock()
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(row) != nil {
			return nil, ErrSDKSessionInvalid
		}
		return buffer.Bytes(), nil
	})
}

// ObserveNativeTranscriptFailure connects late disk errors to the same durable
// SDK/Datadog health used during the request. It remains live after completion.
func (r *Request) ObserveNativeTranscriptFailure(observe func()) {
	if r == nil {
		return
	}
	r.tracker.mu.Lock()
	writer, scope := r.tracker.transcript, r.call.state.scope
	r.tracker.mu.Unlock()
	if writer != nil {
		writer.observeFailure(scope, observe)
	}
}

// FlushNativeTranscript is for explicit local flush/verification, never an
// upstream network deadline or an automatic per-message synchronous write.
func (t *Tracker) FlushNativeTranscript() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	writer := t.transcript
	t.mu.Unlock()
	if writer == nil {
		return nil
	}
	writer.drain()
	writer.mu.Lock()
	defer writer.mu.Unlock()
	for _, state := range writer.scopes {
		if state.err != nil {
			return ErrSDKSessionUnavailable
		}
	}
	return nil
}

// Close seals new appends and waits for already queued local writes. Checkpoint
// and transcript errors remain independent and cannot be cleared by a flush.
func (t *Tracker) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.closed = true
	writer := t.transcript
	children := make([]*SDKSidechain, 0, len(t.sidechains))
	for _, child := range t.sidechains {
		children = append(children, child)
	}
	t.mu.Unlock()
	for _, child := range children {
		_ = child.Close()
	}
	if writer != nil {
		return writer.close()
	}
	return nil
}

func (t *Tracker) nativeTranscriptErrorLocked(scope string) bool {
	return t.transcript != nil && t.transcript.failure(scope) != nil
}
