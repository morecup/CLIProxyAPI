package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func compactionViewFixture(t *testing.T, streamed bool) (*Request, []byte) {
	t.Helper()
	tracker := &Tracker{}
	i := input("view-first")
	i.Body = []byte(`{"messages":[{"role":"user","content":"PRIVATE_FIRST"}]}`)
	first := tracker.Begin(i)
	first.ObserveSDKQuery(i.Body)
	if streamed {
		var response Response
		response.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"role":"assistant","id":"msg_first"}}`))
		for index, text := range []string{"PRIVATE_", "REPLY"} {
			response.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":"ignored"}}`, index)))
			response.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text)))
			response.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index)))
		}
		response.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
		first.ObserveSDKHistory(response.SDKHistoryMessages())
		first.ObserveSDKWireResponse(response.SDKWireFingerprint())
		first.FinishSuccess(baseTime.Add(time.Second), "end_turn", nil)
	} else {
		completeSDKHistoryText(first, "msg_first", "PRIVATE_REPLY")
	}
	i.ClientRequestID = "view-second"
	i.Body = []byte(`{"messages":[{"role":"user","content":"PRIVATE_FIRST"},{"role":"assistant","content":[{"type":"text","text":"PRIVATE_REPLY"}]},{"role":"user","content":"PRIVATE_SECOND"}]}`)
	second := tracker.Begin(i)
	second.ObserveSDKQuery(i.Body)
	completeSDKHistoryText(second, "msg_second", "PRIVATE_SECOND_REPLY")
	i.ClientRequestID = "view-third"
	i.Body = []byte(`{"messages":[{"role":"user","content":"PRIVATE_FIRST"},{"role":"assistant","content":[{"type":"text","text":"PRIVATE_"},{"type":"text","text":"REPLY","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"PRIVATE_SECOND"},{"role":"assistant","content":"PRIVATE_SECOND_REPLY"},{"role":"user","content":"PRIVATE_THIRD"}]}`)
	third := tracker.Begin(i)
	third.ObserveSDKQuery(i.Body)
	return third, i.Body
}

func TestSDKCompactionViewResolvesWholeNativeGroups(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprint(streamed), func(t *testing.T) {
			owner, body := compactionViewFixture(t, streamed)
			view, err := owner.CompactionView(body)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Discard()
			var wire struct{ Messages []json.RawMessage }
			if json.Unmarshal(body, &wire) != nil {
				t.Fatal("invalid fixture")
			}
			queries := 0
			result, err := RunSDKReactiveCompaction(t.Context(), view.History(), nil, func(_ context.Context, plan SDKReactiveAttempt) (SDKReactiveQueryResult[bool], error) {
				queries++
				rows, resolveErr := view.Resolve(plan.Summarize)
				if resolveErr != nil || !reflect.DeepEqual(rows, wire.Messages[:3]) {
					t.Fatal("summary selection did not resolve the exact observed rows")
				}
				preserved, preserveErr := view.Resolve(plan.Preserve)
				if preserveErr != nil || !reflect.DeepEqual(preserved, wire.Messages[3:]) {
					t.Fatal("preserved native groups did not resolve")
				}
				if streamed && plan.MessagesToSummarize != 4 {
					t.Fatal("normalized HTTP rows replaced native message count")
				}
				rows[0][0] = 'x'
				again, againErr := view.Resolve(plan.Summarize)
				if againErr != nil || !reflect.DeepEqual(again, wire.Messages[:3]) {
					t.Fatal("returned content mutated the view")
				}
				return SDKReactiveQueryResult[bool]{Success: true, Payload: true}, nil
			})
			if err != nil || !result.ReadyToApply || queries != 1 {
				t.Fatalf("query=%d ready=%v err=%v", queries, result.ReadyToApply, err)
			}
			encoded, _ := json.Marshal(view)
			if string(encoded) != "{}" || strings.Contains(fmt.Sprintf("%+v", owner.call.state.sdk.history), "PRIVATE_") {
				t.Fatal("content escaped its request-local owner")
			}
		})
	}
}

func TestSDKCompactionViewRejectsWrongBodyAndPartialSelection(t *testing.T) {
	owner, body := compactionViewFixture(t, true)
	for _, changed := range [][]byte{
		[]byte(strings.ReplaceAll(string(body), "PRIVATE_THIRD", "changed")),
		[]byte(strings.ReplaceAll(string(body), `{"type":"text","text":"PRIVATE_"},{"type":"text","text":"REPLY","cache_control":{"type":"ephemeral"}}`, `{"type":"text","text":"PRIVATE_REPLY"}`)),
		[]byte(strings.Repeat("x", maxSDKCompactionViewBytes+1)),
	} {
		if _, err := owner.CompactionView(changed); !errors.Is(err, ErrSDKCompactionContentUnknown) {
			t.Fatal("different final wire body obtained a view")
		}
	}
	view, err := owner.CompactionView(body)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Discard()
	history := view.History()
	for _, selection := range [][]SDKHistoryMessage{
		history.Messages[1:2],
		{history.Messages[0], history.Messages[0]},
		{history.Messages[3], history.Messages[0]},
		{history.Messages[0], history.Messages[4], history.Messages[5]},
	} {
		if _, resolveErr := view.Resolve(selection); !errors.Is(resolveErr, ErrSDKCompactionContentUnknown) {
			t.Fatal("partial, repeated, reordered or skipped rows were resolved")
		}
	}
	history.Messages[0].TokenEstimate.Tokens++
	if _, resolveErr := view.Resolve(history.Messages); !errors.Is(resolveErr, ErrSDKCompactionContentUnknown) {
		t.Fatal("changed native metadata was accepted")
	}
}

func TestSDKCompactionViewRejectsStaleOwners(t *testing.T) {
	for _, mode := range []string{"finished", "partial-response", "concurrent-prompt", "pruned", "discarded"} {
		t.Run(mode, func(t *testing.T) {
			owner, body := compactionViewFixture(t, false)
			view, err := owner.CompactionView(body)
			if err != nil {
				t.Fatal(err)
			}
			history := view.History()
			switch mode {
			case "finished":
				owner.FinishFailure()
			case "partial-response":
				observeHistoryResponse(owner, "msg_partial", 1, baseTime)
			case "concurrent-prompt":
				i := input("concurrent")
				i.Body = body
				owner.tracker.Begin(i)
			case "pruned":
				delete(owner.tracker.prompts, owner.call.state.key)
			case "discarded":
				view.Discard()
			}
			if view.Current() {
				t.Fatal("stale content view remained current")
			}
			if _, err = view.Resolve(history.Messages); !errors.Is(err, ErrSDKCompactionViewStale) {
				t.Fatal("stale content view was resolved")
			}
			view.Discard()
		})
	}
}
