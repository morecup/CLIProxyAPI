package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSDKHistoryGroupingMatchesNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-history-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string
			Messages []SDKHistoryMessage
			Groups   [][]string
		}
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Cases) != 10 {
		t.Fatal("invalid native history vectors")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var ids [][]string
			groups := GroupSDKHistory(tc.Messages)
			for _, group := range groups {
				var values []string
				for _, message := range group {
					values = append(values, message.UUID)
				}
				ids = append(ids, values)
			}
			if !reflect.DeepEqual(ids, tc.Groups) {
				t.Fatalf("groups=%v want=%v", ids, tc.Groups)
			}
			if len(groups) != 0 {
				groups[0][0].UUID = "mutated"
			}
			for _, message := range tc.Messages {
				if message.UUID == "mutated" {
					t.Fatal("grouping returned a mutable input slice")
				}
			}
		})
	}
}

func observeHistoryResponse(r *Request, id string, blocks int, at time.Time) *Response {
	response := &Response{}
	response.ObserveStreamLineAt([]byte(fmt.Sprintf(`data: {"type":"message_start","message":{"role":"assistant","id":%q}}`, id)), at)
	for index := range blocks {
		response.ObserveStreamLineAt([]byte(fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":"PRIVATE_TEXT"}}`, index)), at)
		response.ObserveStreamLineAt([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index)), at)
		r.ObserveSDKHistory(response.SDKHistoryMessages())
		r.ObserveSDKHistory(response.SDKHistoryMessages())
	}
	return response
}

func TestSDKHistoryMaterializesBlocksAndToolResultUsers(t *testing.T) {
	var tracker Tracker
	i := input("history")
	r := tracker.Begin(i)
	r.ObserveSDKQuery(i.Body)
	response := observeHistoryResponse(r, "msg-one", 3, baseTime)
	first := r.SDKHistory()
	if !first.OwnedMessagesKnown || len(first.Messages) != 4 || len(first.Groups) != 2 || len(first.Groups[1]) != 3 {
		t.Fatalf("native block history=%+v", first)
	}
	if strings.Contains(fmt.Sprintf("%+v", first), "PRIVATE_TEXT") {
		t.Fatal("history retained content")
	}
	first.Messages[0].UUID, first.Groups[0][0].UUID = "mutated", "mutated"
	if r.SDKHistory().Messages[0].UUID == "mutated" {
		t.Fatal("history snapshot mutated state")
	}
	response.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
	r.ObserveSDKWireResponse(response.SDKWireFingerprint())
	r.FinishSuccess(baseTime.Add(time.Second), "tool_use", []string{"tool"})
	i = continuation("history-next", "tool")
	i.Body = []byte(`{"messages":[{"role":"user","content":"synthetic prompt"},{"role":"assistant","content":""},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool","content":"synthetic result"}]}]}`)
	next := tracker.Begin(i)
	next.ObserveSDKQuery(i.Body)
	observeHistoryResponse(next, "msg-two", 1, baseTime.Add(2*time.Second))
	r.ObserveSDKHistory(response.SDKHistoryMessages())
	history := next.SDKHistory()
	if !history.OwnedMessagesKnown || len(history.Messages) != 6 || len(history.Groups) != 3 {
		t.Fatalf("tool result history=%+v", history)
	}
	if history.Messages[4].Type != "user" || history.Messages[0].UUID == history.Messages[4].UUID {
		t.Fatal("tool result did not get its own native user object")
	}
}

func TestSDKHistoryRetryIsolationAndUnknownRetraction(t *testing.T) {
	for _, blocks := range []int{0, 1} {
		t.Run(fmt.Sprint(blocks), func(t *testing.T) {
			var tracker Tracker
			i := input("retry-history")
			first := tracker.Begin(i)
			response := observeHistoryResponse(first, "old", blocks, baseTime)
			first.FinishFailure()
			i.Attempt = 2
			retry := tracker.Begin(i)
			first.ObserveSDKHistory(response.SDKHistoryMessages())
			observeHistoryResponse(retry, "new", 1, baseTime.Add(time.Second))
			history := retry.SDKHistory()
			if len(history.Messages) != 2+blocks || history.OwnedMessagesKnown != (blocks == 0) {
				t.Fatalf("retry history=%+v", history)
			}
			if blocks > 0 && history.IncompleteReason != "unobserved-sdk-retry-retraction" {
				t.Fatal("retraction gap hidden")
			}
		})
	}
}

func TestSDKHistoryUnknownPastBoundsAndCompactionBoundary(t *testing.T) {
	var tracker Tracker
	unknown := tracker.Begin(continuation("unknown", "unowned"))
	if unknown.SDKHistory().OwnedMessagesKnown {
		t.Fatal("unobserved initial history was declared known")
	}
	history := &sdkHistory{}
	for range maxSDKHistoryMessages + 1 {
		history.append(SDKHistoryMessage{Type: "user"})
	}
	if len(history.messages) != maxSDKHistoryMessages || history.incompleteReason != "sdk-history-retention-limit" {
		t.Fatal("history bound was silent or ineffective")
	}
	history.boundary()
	history.append(SDKHistoryMessage{Type: "user"})
	snapshot := history.snapshot()
	if len(snapshot.Messages) != 2 || snapshot.Messages[0].Subtype != "compact_boundary" || snapshot.OwnedMessagesKnown {
		t.Fatal("boundary failed to retain its own unknown-history guard")
	}
}

func TestSDKHistoryIsAccountSessionIsolatedAndOldSnapshotsAreStable(t *testing.T) {
	var tracker Tracker
	i := input("history-a")
	a := tracker.Begin(i)
	before := a.SDKHistory()
	i.AccountID = "other-account"
	b := tracker.Begin(i)
	observeHistoryResponse(b, "msg-b", 1, baseTime)
	if len(a.SDKHistory().Messages) != 1 || len(before.Messages) != 1 || len(b.SDKHistory().Messages) != 2 {
		t.Fatal("history crossed accounts or mutated an old snapshot")
	}
	if len(tracker.SDKHistory(i.AccountID, i.SessionID).Messages) != 2 {
		t.Fatal("session history lookup missed its owner")
	}
	i.SessionID = "other-session"
	if tracker.SDKHistory(i.AccountID, i.SessionID).OwnedMessagesKnown {
		t.Fatal("unknown session inherited history")
	}
}
