package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/transcript"
)

// recordingTranscript captures the owned sidechain rows a task appends so the
// native attachment/meta pairing can be asserted without a real journal.
type recordingTranscript struct {
	mu   sync.Mutex
	rows []transcriptRow
}

type transcriptRow struct {
	Kind     string
	Content  json.RawMessage
	Origin   json.RawMessage
	UUID     string
	PromptID string
}

func (f *recordingTranscript) append(row transcriptRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, row)
}
func (f *recordingTranscript) AppendInput(content json.RawMessage, promptID string, _ time.Time) error {
	f.append(transcriptRow{Kind: "input", Content: bytes.Clone(content), PromptID: promptID})
	return nil
}
func (f *recordingTranscript) AppendMetaInput(content, origin json.RawMessage, uuid, promptID string, _ time.Time) error {
	f.append(transcriptRow{Kind: "meta", Content: bytes.Clone(content), Origin: bytes.Clone(origin), UUID: uuid, PromptID: promptID})
	return nil
}
func (f *recordingTranscript) AppendAttachment(attachment json.RawMessage, _ time.Time) error {
	f.append(transcriptRow{Kind: "attachment", Content: bytes.Clone(attachment)})
	return nil
}
func (f *recordingTranscript) Observe([]transcript.Message, string) error { return nil }
func (f *recordingTranscript) VerifyResponse([]byte) error                { return nil }
func (f *recordingTranscript) Leaf() string                               { return "" }
func (f *recordingTranscript) Failure() error                             { return nil }
func (f *recordingTranscript) Flush() error                               { return nil }
func (f *recordingTranscript) Path() string                               { return "" }
func (f *recordingTranscript) ReadTail(int64) (string, error)             { return "", nil }
func (f *recordingTranscript) Close() error                               { return nil }
func (f *recordingTranscript) snapshot() []transcriptRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transcriptRow(nil), f.rows...)
}

// sendHarness routes synthetic model responses per agent so sibling tasks can
// be driven independently through their tool rounds.
type sendHarness struct {
	mu          sync.Mutex
	invocations chan Invocation
	responses   map[string]chan []byte
	transcripts map[string]*recordingTranscript
	main        chan MainDelivery
}

func newSendHarness() *sendHarness {
	return &sendHarness{invocations: make(chan Invocation, 16), responses: map[string]chan []byte{}, transcripts: map[string]*recordingTranscript{}, main: make(chan MainDelivery, 4)}
}

func (h *sendHarness) channel(id string) chan []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.responses[id] == nil {
		h.responses[id] = make(chan []byte, 8)
	}
	return h.responses[id]
}

func (h *sendHarness) transcript(id string) *recordingTranscript {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transcripts[id] == nil {
		h.transcripts[id] = &recordingTranscript{}
	}
	return h.transcripts[id]
}

func (h *sendHarness) options(store Store) Options {
	return Options{Store: store, MainAgentID: "11111111-2222-4333-8444-555555555555",
		Execute: func(ctx context.Context, invocation Invocation) ([]byte, error) {
			h.invocations <- invocation
			select {
			case response := <-h.channel(invocation.AgentID):
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		MessageMain:    func(delivery MainDelivery) error { h.main <- delivery; return nil },
		OpenTranscript: func(id, _ string) (Transcript, error) { return h.transcript(id), nil },
	}
}

func lastUserText(t *testing.T, messages []Message) string {
	t.Helper()
	last := messages[len(messages)-1]
	var text string
	if last.Role != "user" || json.Unmarshal(last.Content, &text) != nil {
		t.Fatalf("last message is not a plain user text: %s", last.Content)
	}
	return text
}

const toolRound = `{"role":"assistant","content":[{"type":"tool_use","id":"probe","name":"UnownedShell","input":{}}],"stop_reason":"tool_use"}`

func TestSendMessageRuntimeDeliversNativeProvenance(t *testing.T) {
	h := newSendHarness()
	store := &memoryStore{}
	r := newRuntime(t, h.options(store))
	owner := caller()
	worker := launch(t, r, owner, "launch-worker", `,"name":"worker"`)
	workerRun := await(t, h.invocations)
	reviewer := launch(t, r, owner, "launch-reviewer", `,"name":"reviewer"`)
	reviewerRun := await(t, h.invocations)
	workerCaller := Caller{AgentID: worker, PromptID: workerRun.PromptID, Model: workerRun.Model, Depth: 1}
	workerRef := candidateHash("subagent", worker)[:refLength]
	reviewerRef := candidateHash("subagent", reviewer)[:refLength]

	// Coordinator (main) -> live child: queued with the conversation pin.
	got := send(t, r, owner, "worker", "queued message")
	want := `{"success":true,"message":"Message queued for delivery to worker at its next tool round.","pin":{"id":"` + worker + `","name":"worker","ref":"` + workerRef + `"}}`
	if string(got) != want {
		t.Fatalf("live coordinator send: %s", got)
	}
	// Peer (worker) -> live sibling: same lane, sender identity is the task name.
	if got := send(t, r, workerCaller, "reviewer", "peer <note>"); string(got) != `{"success":true,"message":"Message queued for delivery to reviewer at its next tool round.","pin":{"id":"`+reviewer+`","name":"reviewer","ref":"`+reviewerRef+`"}}` {
		t.Fatalf("live peer send: %s", got)
	}
	// Peer -> main: wrapped text and origin reach the main conversation queue.
	if got := send(t, r, workerCaller, "main", "report to main"); string(got) != `{"success":true,"message":"`+textQueuedMain+`"}` {
		t.Fatalf("main send: %s", got)
	}
	delivery := await(t, h.main)
	wrapped, origin := peerDelivery("worker", worker, "report to main")
	if delivery.Text != wrapped || delivery.Text != "<agent-message from=\"worker\">\nreport to main\n</agent-message>" || string(delivery.Origin) != `{"kind":"peer","from":"worker","senderTaskId":"`+worker+`","name":"worker","body":"report to main"}` {
		t.Fatalf("main delivery: %q %s", delivery.Text, delivery.Origin)
	}
	if FreshTurnWire(delivery) != freshTurnProjection(wrapped, origin) || !strings.HasPrefix(FreshTurnWire(delivery), peerPrefixFresh+"\n"+wrapped+"\n\n"+peerTrailer) {
		t.Fatalf("fresh-turn wire: %q", FreshTurnWire(delivery))
	}
	// Main addressing itself is refused with the native text.
	if got := send(t, r, owner, "main", "self"); string(got) != `{"success":false,"message":"`+strings.ReplaceAll(textMainSelf, `"`, `\"`)+`"}` {
		t.Fatalf("main self send: %s", got)
	}
	if states := r.Snapshots(); states[0].Queued != 1 || states[1].Queued != 1 {
		t.Fatal("queued counts", states)
	}
	raw, _, _ := store.Load("")
	if !bytes.Contains(raw, []byte(`"send_message_pins":{"reviewer":{"id":"`+reviewer+`","name":"reviewer","ref":"`+reviewerRef+`"},"worker":{"id":"`+worker+`","name":"worker","ref":"`+workerRef+`"}}`)) {
		t.Fatalf("pins not persisted: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"pending_messages":[{"text":"queued message","origin":{"kind":"coordinator"},"isMeta":true}]`)) {
		t.Fatalf("coordinator pending entry: %s", raw)
	}

	// The worker's next tool round carries the system-reminder wrapped
	// coordinator projection after the tool results.
	h.channel(worker) <- []byte(toolRound)
	second := await(t, h.invocations)
	wire := lastUserText(t, second.Messages)
	if wire != queuedDeliveryWire("queued message", coordinatorOrigin) || wire != "<system-reminder>\nThe coordinator sent a message while you were working:\nqueued message\n\nAddress this before completing your current task.\n</system-reminder>" {
		t.Fatalf("coordinator wire: %q", wire)
	}
	if !bytes.Contains(second.Messages[len(second.Messages)-2].Content, []byte(`"tool_result"`)) {
		t.Fatal("queued delivery overtook the tool results")
	}
	rows := h.transcript(worker).snapshot()
	if len(rows) != 4 || rows[0].Kind != "input" || rows[1].Kind != "input" || rows[2].Kind != "attachment" || rows[3].Kind != "meta" {
		t.Fatalf("worker transcript rows: %+v", rows)
	}
	var attachment struct {
		Type       string          `json:"type"`
		Prompt     string          `json:"prompt"`
		SourceUUID string          `json:"source_uuid"`
		Origin     json.RawMessage `json:"origin"`
		IsMeta     bool            `json:"isMeta"`
	}
	if json.Unmarshal(rows[2].Content, &attachment) != nil || attachment.Type != "queued_command" || attachment.Prompt != "queued message" || !attachment.IsMeta || string(attachment.Origin) != `{"kind":"coordinator"}` || !validPromptID(attachment.SourceUUID) {
		t.Fatalf("queued_command attachment: %s", rows[2].Content)
	}
	var metaText string
	if json.Unmarshal(rows[3].Content, &metaText) != nil || metaText != wire || rows[3].UUID != attachment.SourceUUID || string(rows[3].Origin) != `{"kind":"coordinator"}` || rows[3].PromptID != workerRun.PromptID {
		t.Fatalf("meta row: %+v", rows[3])
	}

	// The reviewer receives the peer wrapper with the sender identity and the
	// mid-turn peer projection inside the system reminder.
	h.channel(reviewer) <- []byte(toolRound)
	reviewerSecond := await(t, h.invocations)
	peerWrapped, peerOrigin := peerDelivery("worker", worker, "peer <note>")
	peerWire := lastUserText(t, reviewerSecond.Messages)
	if peerWire != queuedDeliveryWire(peerWrapped, peerOrigin) || !strings.HasPrefix(peerWire, "<system-reminder>\n"+peerPrefixMidTurn+"\n<agent-message from=\"worker\">\npeer <note>\n</agent-message>\n\n"+peerTrailer+peerMidTurnTail+"\n</system-reminder>") {
		t.Fatalf("peer wire: %q", peerWire)
	}
	reviewerRows := h.transcript(reviewer).snapshot()
	if len(reviewerRows) != 4 || string(reviewerRows[3].Origin) != `{"kind":"peer","from":"worker","senderTaskId":"`+worker+`","name":"worker","body":"peer <note>"}` {
		t.Fatalf("reviewer meta origin: %+v", reviewerRows)
	}
	if reviewerRun.AgentID != reviewer || reviewerSecond.PromptID != reviewerRun.PromptID {
		t.Fatal("reviewer continuation changed identity")
	}

	// Name reuse: the pinned spelling now reaches another agent and is refused
	// until the caller addresses the new agent by its ref.
	replacement := launch(t, r, owner, "launch-replacement", `,"name":"worker"`)
	await(t, h.invocations)
	replacementRef := candidateHash("subagent", replacement)[:refLength]
	got = send(t, r, owner, "worker", "which worker?")
	var rebound struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Display string `json:"display"`
	}
	if json.Unmarshal(got, &rebound) != nil || rebound.Success || !strings.HasPrefix(rebound.Message, "'worker' now resolves to a different agent than it did earlier in this conversation: earlier sends went to ["+workerRef+"], which this name no longer reaches. Nothing was sent.\nIt now resolves to:\n  worker ["+replacementRef+"] \u2014 subagent, in this session, started ") || !strings.Contains(rebound.Message, "{\"to\": \"worker ["+replacementRef+"]\", ...}") || rebound.Display != "Not sent \u2014 'worker' now means a different agent than it did earlier in this conversation; asked Claude to confirm which one it wants." {
		t.Fatalf("rebound guard: %s", got)
	}
	if got := send(t, r, owner, "worker ["+replacementRef+"]", "explicit ref"); string(got) != `{"success":true,"message":"Message queued for delivery to worker at its next tool round.","pin":{"id":"`+replacement+`","name":"worker","ref":"`+replacementRef+`"}}` {
		t.Fatalf("explicit ref send: %s", got)
	}
	if got := send(t, r, owner, "worker", "plain again"); !bytes.Contains(got, []byte(`"pin":{"id":"`+replacement+`"`)) {
		t.Fatalf("re-pinned spelling: %s", got)
	}
	// The earlier worker stays reachable only by its agent ID: the registry
	// lists one candidate per name, so its old ref is no longer an address.
	if got := send(t, r, owner, worker, "by id"); string(got) != `{"success":true,"message":"Message queued for delivery to `+worker+` at its next tool round.","pin":{"id":"`+worker+`","name":"`+worker+`","ref":"`+workerRef+`"}}` {
		t.Fatalf("agent ID send: %s", got)
	}
	if got := send(t, r, owner, "worker ["+workerRef+"]", "by old ref"); string(got) != `{"success":false,"message":"No agent named 'worker [`+workerRef+`]' is reachable. Did you mean: worker [`+replacementRef+`]?\n`+textNotFoundHint+`","display":"Not sent `+"\u2014"+` no agent named 'worker [`+workerRef+`]' is reachable. Did you mean: worker?"}` {
		t.Fatalf("old ref send: %s", got)
	}

	// Restart: pins reload from the store and a killed task resumes with the
	// mid-turn coordinator projection as its resume prompt and meta row.
	r.Close()
	restored := newRuntime(t, h.options(store))
	if restored.pins["worker"].ID != replacement || restored.pins["reviewer"].ID != reviewer || restored.pins[worker].ID != worker {
		t.Fatalf("pins not restored: %+v", restored.pins)
	}
	resumeCaller := caller()
	got = send(t, restored, resumeCaller, worker, "resume after restart")
	if string(got) != `{"success":true,"message":"Resuming agent `+resumeDisplayName(worker)+`","resumedAgentId":"`+worker+`","pin":{"id":"`+worker+`","name":"`+worker+`","ref":"`+workerRef+`"}}` {
		t.Fatalf("resume result: %s", got)
	}
	resumed := await(t, h.invocations)
	if resumed.AgentID != worker || resumed.Kind != "resume" || resumed.ParentPromptID != resumeCaller.PromptID {
		t.Fatal("resume identity", resumed)
	}
	if prompt := lastUserText(t, resumed.Messages); prompt != midTurnProjection("resume after restart", coordinatorOrigin) || prompt != "The coordinator sent a message while you were working:\nresume after restart\n\nAddress this before completing your current task." {
		t.Fatalf("resume prompt: %q", prompt)
	}
	resumedRows := h.transcript(worker).snapshot()
	last := resumedRows[len(resumedRows)-1]
	var resumeText string
	if last.Kind != "meta" || json.Unmarshal(last.Content, &resumeText) != nil || resumeText != midTurnProjection("resume after restart", coordinatorOrigin) || string(last.Origin) != `{"kind":"coordinator"}` || last.PromptID != resumed.PromptID {
		t.Fatalf("resume meta row: %+v", last)
	}
	h.channel(worker) <- reply("resumed")
	eventually(t, func() bool { return restored.Snapshots()[0].Status == "completed" })
}
