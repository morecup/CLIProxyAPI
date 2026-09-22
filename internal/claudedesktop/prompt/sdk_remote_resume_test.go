package prompt

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestRemoteWireResumeRestoresOwnedHistoryWithoutWrites(t *testing.T) {
	tracker, input, structural, content, transcript := nativeResumeTestState(t)
	if err := tracker.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewTracker(structural, SDKNativeContentOptions{Store: content, TranscriptStore: transcript})
	defer restarted.Close()
	counts := []int{structural.saves, content.saves, len(transcript.appends)}
	prepared, err := restarted.PrepareRemoteWireResume(input.AccountID, input.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	before := restarted.NativeContent(input.AccountID, input.SessionID)
	rows, err := prepared.Read()
	if err != nil || len(rows) != 2 || !bytes.Contains(rows[0], []byte("PRIVATE_IMAGE")) || !bytes.Contains(rows[1], []byte("PRIVATE_REPLY")) {
		t.Fatal("history missing", err)
	}
	if raw, err := json.Marshal(prepared); err == nil || len(raw) != 0 {
		t.Fatal("private capability escaped")
	}
	rows[0][0] = 'X'
	if !reflect.DeepEqual(before, restarted.NativeContent(input.AccountID, input.SessionID)) || !reflect.DeepEqual(counts, []int{structural.saves, content.saves, len(transcript.appends)}) {
		t.Fatal("read mutated protected rows or counters")
	}
	rows, err = prepared.Read()
	if err != nil || !json.Valid(rows[0]) {
		t.Fatal("actor projection aliased reader")
	}
	payload, rev, _ := content.Load(digest(input.AccountID, input.SessionID))
	_, _ = content.Save(digest(input.AccountID, input.SessionID), rev, payload)
	if _, err := prepared.Read(); !errors.Is(err, ErrSDKSessionStale) {
		t.Fatal("stale capability remained valid", err)
	}
}

func TestRemoteWireResumeDoesNotHideUnreconciledHistory(t *testing.T) {
	for _, kind := range []string{"unknown-fingerprint", "changed-wire", "active"} {
		t.Run(kind, func(t *testing.T) {
			tracker, input, _, _, _ := nativeResumeTestState(t)
			defer tracker.Close()
			scope := digest(input.AccountID, input.SessionID)
			for _, state := range tracker.prompts {
				if state.scope == scope {
					switch kind {
					case "unknown-fingerprint":
						state.sdk.history.expectedTextKnown = false
					case "changed-wire":
						state.sdk.history.expectedText[0] = digest("changed")
					case "active":
						state.helperOwners++
					}
				}
			}
			if kind != "active" {
				tracker.saveSDKSessionLocked(scope)
			}
			if prepared, err := tracker.PrepareRemoteWireResume(input.AccountID, input.SessionID); err == nil || prepared != nil {
				t.Fatal("unknown/active history admitted")
			}
		})
	}
}

func TestRemoteWireProjectionReassemblesBlocksButNeverFlattensAttachments(t *testing.T) {
	input := []SDKNativeMessage{
		{Type: "user", Message: json.RawMessage(`{"role":"user","content":"input"}`)},
		{Type: "assistant", Message: json.RawMessage(`{"id":"m","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"signed"}]}`)},
		{Type: "assistant", Message: json.RawMessage(`{"id":"m","role":"assistant","content":[{"type":"text","text":"reply"}]}`)},
	}
	rows, err := committedNativeWireRows(input)
	if err != nil || len(rows) != 2 || !bytes.Contains(rows[1], []byte(`"signature":"signed"`)) {
		t.Fatal("native blocks lost", err)
	}
	input = append(input, SDKNativeMessage{Type: "attachment", Attachment: json.RawMessage(`{"type":"hook_additional_context"}`)})
	if _, err := committedNativeWireRows(input); !errors.Is(err, ErrSDKResumeReconstructionRequired) {
		t.Fatal("missing attachment semantics silently ignored", err)
	}
}
