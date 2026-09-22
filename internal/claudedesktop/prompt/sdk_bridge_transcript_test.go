package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func bridgeTranscriptFixture(t *testing.T) (*Tracker, Input, *sdkMemorySessionStore, *sdkMemorySessionStore, *sdkReadableTranscriptStore) {
	t.Helper()
	structural, content, transcript := &sdkMemorySessionStore{}, &sdkMemorySessionStore{}, &sdkReadableTranscriptStore{}
	input := sdkStateTestInput(`{"messages":[{"role":"user","content":"owned input"}]}`, time.Now().UTC().Truncate(time.Millisecond))
	input.SessionID = uuid.NewString()
	scope := digest(input.AccountID, input.SessionID)
	transcript.indices = map[string]SDKTranscriptIndex{scope: {Path: "C:/synthetic/session.jsonl"}}
	tracker := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript,
		Version: "2.1.247", Entrypoint: "claude-desktop", Cwd: "C:/synthetic"})
	tracker.transcript = pausedTranscriptWriter(t, transcript)
	return &tracker, input, structural, content, transcript
}

func finishBridgeTranscriptInput(t *testing.T, tracker *Tracker, input Input) {
	t.Helper()
	request := tracker.Begin(input)
	request.ObserveSDKQuery(input.Body)
	var response Response
	response.EnableNativeContent()
	response.ObservePayloadAt([]byte(`{"id":"msg_bridge","type":"message","role":"assistant","model":"synthetic","content":[{"type":"text","text":"owned reply"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), false, input.StartedAt.Add(time.Millisecond))
	observeNativeTestResponse(request, &response)
	request.FinishSuccess(input.StartedAt.Add(time.Millisecond), "end_turn", nil)
	request.RecordSDKAPISuccess(1)
}

func bridgeTranscriptRow(input Input, cursor int64) SDKBridgeTranscriptRecord {
	return SDKBridgeTranscriptRecord{Type: "bridge-session", SessionID: input.SessionID, BridgeSessionID: "cse_owned", LastSequenceNum: cursor,
		DeclaredDialogKinds: []string{"permission"}, SessionGroupingID: "group", NoHistoryBackfill: true,
		OwnerAccountUUID: "account", OwnerOrganizationUUID: "organization"}
}

func readBridgeTranscriptRows(t *testing.T, store *sdkReadableTranscriptStore, input Input) ([]string, []SDKBridgeTranscriptRecord) {
	t.Helper()
	var kinds []string
	var rows []SDKBridgeTranscriptRecord
	_, err := store.ReadTranscript(digest(input.AccountID, input.SessionID), func(lines []byte) error {
		for _, line := range bytes.Split(bytes.TrimSuffix(lines, []byte{'\n'}), []byte{'\n'}) {
			var header struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(line, &header); err != nil {
				return err
			}
			kinds = append(kinds, header.Type)
			if header.Type == "bridge-session" {
				row, err := ParseSDKBridgeTranscriptRecord(line)
				if err != nil {
					return err
				}
				rows = append(rows, row)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return kinds, rows
}

func TestBridgeTranscriptSeedDoesNotInventPromptOrFile(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "seed", true: "clear_before_file"}[clear], func(t *testing.T) {
			tracker, input, structural, content, transcript := bridgeTranscriptFixture(t)
			value := bridgeTranscriptRow(input, 41)
			if err := tracker.RecordNativeBridgeTranscript(input.AccountID, value, nil); err != nil {
				t.Fatal(err)
			}
			value.DeclaredDialogKinds[0] = "changed"
			if clear {
				if err := tracker.RecordNativeBridgeTranscript(input.AccountID, SDKBridgeTranscriptRecord{Type: "bridge-session", SessionID: input.SessionID}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := tracker.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			if len(transcript.appends) != 0 || len(tracker.prompts) != 0 || structural.saves != 0 || content.saves != 0 {
				t.Fatal("bridge metadata invented a transcript, prompt or content checkpoint")
			}
			finishBridgeTranscriptInput(t, tracker, input)
			if err := tracker.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			kinds, rows := readBridgeTranscriptRows(t, transcript, input)
			if clear {
				if len(rows) != 0 {
					t.Fatal("cleared pre-file metadata seeded a tombstone")
				}
			} else if len(rows) != 1 || kinds[0] != "bridge-session" || rows[0].DeclaredDialogKinds[0] != "permission" {
				t.Fatal("first input did not seed the captured immutable bridge metadata", kinds)
			}
			if len(tracker.NativeContent(input.AccountID, input.SessionID).Messages) != 2 {
				t.Fatal("metadata entered native message history")
			}
		})
	}
}

func TestBridgeTranscriptRepeatedCheckpointsRestoreLastWins(t *testing.T) {
	tracker, input, structural, content, transcript := bridgeTranscriptFixture(t)
	finishBridgeTranscriptInput(t, tracker, input)
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	before := tracker.NativeContent(input.AccountID, input.SessionID)
	for _, cursor := range []int64{0, 41, 41} {
		value := bridgeTranscriptRow(input, cursor)
		if err := tracker.RecordNativeBridgeTranscript(input.AccountID, value, nil); err != nil {
			t.Fatal(err)
		}
		value.DeclaredDialogKinds[0] = "mutated"
	}
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	_, rows := readBridgeTranscriptRows(t, transcript, input)
	if len(rows) != 3 || rows[1].LastSequenceNum != 41 || !reflect.DeepEqual(rows[1], rows[2]) || rows[2].DeclaredDialogKinds[0] != "permission" {
		t.Fatal("metadata was UUID/stamp-deduplicated or mutated", rows)
	}
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	resume, err := restarted.RestoreNativeContent(input.AccountID, input.SessionID)
	if err != nil || !reflect.DeepEqual(resume.Snapshot, before) || !reflect.DeepEqual(resume.TranscriptBridge, &rows[2]) {
		t.Fatal("protected last-wins restoration failed", err)
	}
	resume.TranscriptBridge.DeclaredDialogKinds[0] = "changed"
	clear := SDKBridgeTranscriptRecord{Type: "bridge-session", SessionID: input.SessionID}
	if err := restarted.RecordNativeBridgeTranscript(input.AccountID, clear, nil); err != nil {
		t.Fatal(err)
	}
	if err := restarted.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	resume, err = restarted.RestoreNativeContent(input.AccountID, input.SessionID)
	if err != nil || !reflect.DeepEqual(resume.TranscriptBridge, &clear) {
		t.Fatal("clear did not replace all prior bridge fields", err)
	}
}

func TestBridgeTranscriptDelayedFailureBelongsToQueuedProducer(t *testing.T) {
	tracker, input, _, _, transcript := bridgeTranscriptFixture(t)
	finishBridgeTranscriptInput(t, tracker, input)
	if err := tracker.FlushNativeTranscript(); err != nil {
		t.Fatal(err)
	}
	var first, second atomic.Int32
	if err := tracker.RecordNativeBridgeTranscript(input.AccountID, bridgeTranscriptRow(input, 1), func(error) { first.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := tracker.RecordNativeBridgeTranscript(input.AccountID, bridgeTranscriptRow(input, 2), func(error) { second.Add(1) }); err != nil {
		t.Fatal(err)
	}
	transcript.before = func(string, []byte) error { return errors.New("synthetic append failure") }
	if err := tracker.FlushNativeTranscript(); err == nil || first.Load() != 1 || second.Load() != 1 || !tracker.NativeContent(input.AccountID, input.SessionID).PersistenceError {
		t.Fatal("late failures were lost or rebound to a later query")
	}
	transcript.before = nil
	if err := tracker.RecordNativeBridgeTranscript(input.AccountID, bridgeTranscriptRow(input, 3), nil); err != nil {
		t.Fatal(err)
	}
	_ = tracker.FlushNativeTranscript()
	_, rows := readBridgeTranscriptRows(t, transcript, input)
	if len(rows) != 1 || rows[0].LastSequenceNum != 3 || first.Load() != 1 || second.Load() != 1 {
		t.Fatal("explicit later checkpoint replayed failed metadata")
	}
}

func TestBridgeTranscriptRejectsCorruptOrForeignRowsOnResume(t *testing.T) {
	for _, replacement := range []string{"foreign-session", "missing-sequence", "unknown-field", "truncated-tail"} {
		t.Run(replacement, func(t *testing.T) {
			tracker, input, _, _, transcript := bridgeTranscriptFixture(t)
			finishBridgeTranscriptInput(t, tracker, input)
			if err := tracker.RecordNativeBridgeTranscript(input.AccountID, bridgeTranscriptRow(input, 41), nil); err != nil {
				t.Fatal(err)
			}
			if err := tracker.FlushNativeTranscript(); err != nil {
				t.Fatal(err)
			}
			transcript.mutate = func(lines []byte) []byte {
				if !bytes.Contains(lines, []byte(`"bridge-session"`)) {
					return lines
				}
				switch replacement {
				case "foreign-session":
					return bytes.ReplaceAll(lines, []byte(input.SessionID), []byte(uuid.NewString()))
				case "missing-sequence":
					return bytes.ReplaceAll(lines, []byte(`"lastSequenceNum":41,`), nil)
				case "unknown-field":
					return bytes.ReplaceAll(lines, []byte(`"lastSequenceNum":41`), []byte(`"lastSequenceNum":41,"worker_jwt":"not_allowed"`))
				default:
					return lines[:len(lines)-1]
				}
			}
			if got, err := tracker.RestoreNativeContent(input.AccountID, input.SessionID); err == nil || got.TranscriptBridge != nil || len(got.ActiveMessages) != 0 {
				t.Fatal("partial or foreign metadata authorized restored history")
			}
		})
	}
}
