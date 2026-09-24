package prompt

import (
	"encoding/json"
	"testing"
)

func TestSDKCompactionTextWrapMatchesNativeOptions(t *testing.T) {
	vectors := readSDKCompactVectors(t)
	for _, vector := range vectors.Wrappers {
		var response SDKCompactionResponse
		response.ObserveJSON(compactResponseJSON(t, vectors.ResponseText))
		text, known := response.TakeText("1.40609.0.0", "2.1.247")
		options := SDKCompactionWrapOptions{RecentMessagesPreserved: vector.Variant&1 != 0,
			REPLStateCleared: vector.Variant&2 != 0, SuppressFollowUpQuestions: vector.Variant&4 != 0}
		if vector.Variant&8 != 0 {
			options.TranscriptPath = `C:\synthetic\session.jsonl`
		}
		got, err := text.Wrap(options)
		if !known || err != nil || got != vector.Text || !text.Fingerprint().matches(got) {
			t.Fatalf("native wrapper option %d differs", vector.Variant)
		}
		if text.SelectedText() != vectors.ResponseText {
			t.Fatal("selected pre-normalization text was lost")
		}
		encoded, _ := json.Marshal(text)
		if string(encoded) != "{}" || response.text != nil || response.blocks != nil {
			t.Fatal("operation content escaped into JSON or the observer")
		}
	}
}

func TestSDKCompactionSelectedEmptyAndNormalizedEmptyAreDifferent(t *testing.T) {
	for _, selected := range []string{"", "  ", "<analysis>synthetic</analysis>"} {
		var response SDKCompactionResponse
		response.ObserveJSON(compactResponseJSON(t, selected))
		text, known := response.TakeText("1.40609.0.0", "2.1.247")
		want := selected == "<analysis>synthetic</analysis>"
		if known != want {
			t.Fatal("native selection gate was moved after normalization")
		}
		wrapped, err := text.Wrap(SDKCompactionWrapOptions{SuppressFollowUpQuestions: true})
		if want {
			if err != nil || !text.Fingerprint().matches(wrapped) || text.Fingerprint().bytes != 0 {
				t.Fatal("known empty normalized summary could not be wrapped or adopted")
			}
		} else if err == nil || wrapped != "" {
			t.Fatal("missing selected summary was wrapped")
		}
	}
}

func TestSDKCompactionTextAcceptsCurrentDesktopCodeIdentity(t *testing.T) {
	var response SDKCompactionResponse
	response.ObserveJSON(compactResponseJSON(t, "<summary>current automatic compaction</summary>"))
	text, known := response.TakeText("2.7032.0", "2.1.280")
	wrapped, err := text.Wrap(SDKCompactionWrapOptions{SuppressFollowUpQuestions: true})
	if !known || err != nil || wrapped == "" || !text.Fingerprint().matches(wrapped) {
		t.Fatalf("current compaction response was not accepted: known=%v err=%v", known, err)
	}
}
