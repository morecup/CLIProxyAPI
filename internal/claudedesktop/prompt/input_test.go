package prompt

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSubmissionFirstAndLastTextAreDifferentFacts(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"continue"},{"type":"image"},{"type":"text","text":"🙂中文"}]}]}`)
	got := ObserveSubmission(body, time.Now())
	if !got.Known || !got.FreshConversation || !got.IsKeepGoing || got.Length != 4 || got.IsNegative || got.IsWakeup {
		t.Fatalf("first/last UTF16 facts: %+v", got)
	}
	for i := range body {
		body[i] = 'x'
	}
	if got.Length != 4 || !got.IsKeepGoing {
		t.Fatal("submission retained mutable body")
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "continue") || strings.Contains(string(encoded), "中文") {
		t.Fatal("submission retains input text")
	}
}

func TestSubmissionUnknownDoesNotGuessNativeDispatch(t *testing.T) {
	for _, body := range []string{
		`not-json`, `{"messages":[]}`, `{"messages":[{"role":"assistant","content":"hello"}]}`,
		`{"messages":[{"role":"user","content":" "}]}`, `{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":"/synthetic-command"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"synthetic"},{"type":"text","text":"hello"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":17}]}]}`,
		`{"messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}]}`,
	} {
		if got := ObserveSubmission([]byte(body), time.Now()); got.Known {
			t.Fatalf("unknown became ordinary: %s", body)
		}
	}
}

func TestSubmissionClassifiers(t *testing.T) {
	for _, tc := range []struct {
		text                 string
		negative, keep, wake bool
	}{
		{"WHAT THE HELL", true, false, false}, {"this is wrong", false, false, false},
		{"continue", false, true, false}, {"continue now", false, false, false},
		{"Please keep going!", false, true, false}, {"keep goingx", false, false, false},
		{"hello", false, false, true}, {"hİ", false, false, false},
		{"\uFEFFcontinue\uFEFF", false, true, false}, {"\u0085continue\u0085", false, false, false},
		{"¿在吗？！", false, false, true}, {"...", false, false, true}, {".", false, false, false},
		{"!", false, false, false}, {"hola", false, false, true}, {"hello " + strings.Repeat("x", 41), false, false, false},
	} {
		body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": tc.text}}})
		got := ObserveSubmission(body, time.Now())
		if !got.Known || got.IsNegative != tc.negative || got.IsKeepGoing != tc.keep || got.IsWakeup != tc.wake {
			t.Errorf("%q: %+v", tc.text, got)
		}
	}
}

func TestSubmissionMatchesPinnedNativeScalarFixtures(t *testing.T) {
	encoded, errRead := os.ReadFile("testdata/sdk-input-native.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	var fixture struct {
		Cases []struct {
			Input                json.RawMessage `json:"input"`
			Negative, Keep, Wake bool
			Length               int
		} `json:"cases"`
	}
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 34 {
		t.Fatal("native fixture coverage changed")
	}
	for index, tc := range fixture.Cases {
		body := append([]byte(`{"messages":[{"role":"user","content":`), tc.Input...)
		body = append(body, []byte(`}]}`)...)
		got := ObserveSubmission(body, time.Now())
		if !got.Known || got.IsNegative != tc.Negative || got.IsKeepGoing != tc.Keep || got.IsWakeup != tc.Wake || got.Length != tc.Length {
			t.Errorf("native fixture %d differs: %+v", index, got)
		}
	}
}

func TestSDKInputClaimDoesNotReuseControlOrRetryOwnership(t *testing.T) {
	var tracker Tracker
	in := Input{AccountID: "a", SessionID: "s", ClientRequestID: "r", Role: "main", Body: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	first := tracker.Begin(in)
	if !first.ClaimSDKInput() || first.ClaimSDKInput() || !first.ClaimControlInput() {
		t.Fatal("input/control claim boundary")
	}
	first.FinishFailure()
	in.Attempt = 2
	retry := tracker.Begin(in)
	if retry.ClaimSDKInput() {
		t.Fatal("retry emitted new submission")
	}
	in.AccountID = "b"
	if !tracker.Begin(in).ClaimSDKInput() {
		t.Fatal("another account inherited claim")
	}
}
