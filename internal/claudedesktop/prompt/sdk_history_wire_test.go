package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func completeSDKHistoryText(r *Request, messageID, text string) {
	var response Response
	response.ObservePayloadAt([]byte(fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}`, messageID, text)), false, baseTime)
	r.ObserveSDKHistory(response.SDKHistoryMessages())
	r.ObserveSDKWireResponse(response.SDKWireFingerprint())
	r.FinishSuccess(baseTime.Add(time.Second), "end_turn", nil)
}

func TestSDKHistoryReconcilesOrdinaryTextAcrossPromptsWithoutRetainingContent(t *testing.T) {
	var tracker Tracker
	i := input("text-one")
	i.Body = []byte(`{"messages":[{"role":"user","content":"PRIVATE_USER_PROMPT"}]}`)
	first := tracker.Begin(i)
	first.ObserveSDKQuery(i.Body)
	completeSDKHistoryText(first, "msg-first", "PRIVATE_ASSISTANT_REPLY")
	i.ClientRequestID, i.StartedAt = "text-two", baseTime.Add(2*time.Second)
	i.Body = []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"PRIVATE_USER_PROMPT","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":[{"type":"text","text":"PRIVATE_ASSISTANT_"},{"type":"text","text":"REPLY"}]},{"role":"user","content":"PRIVATE_NEXT_PROMPT"}]}`)
	next := tracker.Begin(i)
	if next.SDKHistory().IncompleteReason != "awaiting-sdk-history-reconciliation" {
		t.Fatal("unchecked history was reported known")
	}
	next.ObserveSDKQuery(i.Body)
	completeSDKHistoryText(next, "msg-second", "PRIVATE_NEXT_REPLY")
	history := next.SDKHistory()
	if !history.OwnedMessagesKnown || len(history.Messages) != 4 || len(history.Groups) != 3 {
		t.Fatalf("reconciled history=%+v", history)
	}
	if strings.Contains(fmt.Sprintf("%+v %+v %+v", history, next.call.sdkWireInputs, next.call.state.sdk.history), "PRIVATE_") {
		t.Fatal("history retained content")
	}
}

func TestSDKHistoryRejectsChangedOrUnsupportedCallerPast(t *testing.T) {
	for _, tc := range []struct{ name, body, reason string }{
		{"changed-user", `{"messages":[{"role":"user","content":"changed"},{"role":"assistant","content":"reply"},{"role":"user","content":"next"}]}`, "changed-sdk-caller-history"},
		{"changed-assistant", `{"messages":[{"role":"user","content":"synthetic prompt"},{"role":"assistant","content":"changed"},{"role":"user","content":"next"}]}`, "changed-sdk-caller-history"},
		{"dropped-past", `{"messages":[{"role":"user","content":"next"}]}`, "changed-sdk-caller-history"},
		{"unobserved-thinking-past", `{"messages":[{"role":"user","content":"synthetic prompt"},{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"reply"}]},{"role":"user","content":"next"}]}`, "changed-sdk-caller-history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tracker Tracker
			i := input("one")
			first := tracker.Begin(i)
			first.ObserveSDKQuery(i.Body)
			completeSDKHistoryText(first, "msg-first", "reply")
			i.ClientRequestID, i.Body = "two", []byte(tc.body)
			next := tracker.Begin(i)
			next.ObserveSDKQuery(i.Body)
			if got := next.SDKHistory(); got.OwnedMessagesKnown || got.IncompleteReason != tc.reason {
				t.Fatalf("caller history guard=%+v", got)
			}
		})
	}
}

func TestSDKHistoryStreamingAndBufferedTextHaveSameFingerprint(t *testing.T) {
	var streamed, buffered Response
	buffered.ObservePayloadAt([]byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"a😀b"}],"stop_reason":"end_turn"}`), false, baseTime)
	for _, event := range []string{
		`{"type":"message_start","message":{"id":"msg","role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a😀"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"b"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	} {
		streamed.ObserveStreamLineAt([]byte("data: "+event), baseTime)
	}
	want, wantKnown, _ := buffered.SDKWireFingerprint()
	got, known, complete := streamed.SDKWireFingerprint()
	if got != want || !known || !wantKnown || !complete {
		t.Fatal("stream segmentation changed text fingerprint")
	}
	if messages, _ := streamed.SDKHistoryMessages(); len(messages) != 2 {
		t.Fatal("content fingerprint collapsed native assistant objects")
	}
}

func TestSDKHistoryPartialNonTextAndUnmatchedDeltasCannotReconcile(t *testing.T) {
	for _, suffix := range []string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"unowned"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	} {
		var response Response
		response.ObserveStreamLineAt([]byte(`data: {"type":"message_start","message":{"id":"msg","role":"assistant"}}`), baseTime)
		response.ObserveStreamLineAt([]byte(suffix), baseTime)
		response.ObserveStreamLineAt([]byte(`data: {"type":"message_stop"}`), baseTime)
		if _, known, _ := response.SDKWireFingerprint(); known {
			t.Fatal("unsupported/incomplete response received a trusted history fingerprint")
		}
	}
}
