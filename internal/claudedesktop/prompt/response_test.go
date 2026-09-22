package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSDKAssistantObservedAtFirstCompletedBlock(t *testing.T) {
	var response Response
	observe := func(seconds int, event string) {
		response.ObserveStreamLineAt([]byte("data: "+event), baseTime.Add(time.Duration(seconds)*time.Second))
	}
	observe(1, `{"type":"message_start","message":{"role":"assistant"}}`)
	observe(2, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"private thought"}}`)
	observe(3, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"private delta"}}`)
	if at, _ := response.SDKAssistantMessage(); !at.IsZero() {
		t.Fatal("message start or a partial delta set assistant time")
	}
	observe(4, `{"type":"content_block_stop","index":0}`)
	if at, tools := response.SDKAssistantMessage(); at != baseTime.Add(4*time.Second) || len(tools) != 0 {
		t.Fatalf("first completed thinking block: %v %#v", at, tools)
	}
	if _, _, complete := response.Outcome(); complete {
		t.Fatal("one completed content block completed the response")
	}
	observe(5, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"private text"}}`)
	observe(6, `{"type":"content_block_stop","index":1}`)
	observe(7, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
	observe(8, `{"type":"message_stop"}`)
	if at, _ := response.SDKAssistantMessage(); at != baseTime.Add(4*time.Second) {
		t.Fatal("later content or response completion replaced the first assistant clock")
	}
	if stop, _, complete := response.Outcome(); stop != "end_turn" || !complete {
		t.Fatal("response completion was lost")
	}
	if strings.Contains(fmt.Sprintf("%+v", &response), "private") {
		t.Fatal("response accounting retained model content")
	}
}

func TestSDKToolMetadataRequiresClosedBlock(t *testing.T) {
	var response Response
	for _, line := range []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-a","name":"Read","input":{"private":"arguments"}}}`,
	} {
		response.ObserveStreamLineAt([]byte("data: "+line), baseTime)
	}
	if at, tools := response.SDKAssistantMessage(); !at.IsZero() || len(tools) != 0 {
		t.Fatal("unassembled tool block was counted as an assistant object")
	}
	response.ObserveStreamLineAt([]byte(`data: {"type":"content_block_stop","index":0}`), baseTime.Add(time.Second))
	at, tools := response.SDKAssistantMessage()
	if at.IsZero() || len(tools) != 1 || tools[0] != (ToolObservation{ID: "tool-a", Name: "Read"}) {
		t.Fatalf("closed tool observation: %v %#v", at, tools)
	}
	tools[0].Name = "mutated"
	response.ObserveStreamLineAt([]byte(`data: {"type":"content_block_stop","index":0}`), baseTime.Add(2*time.Second))
	if _, tools = response.SDKAssistantMessage(); len(tools) != 1 || tools[0].Name != "Read" {
		t.Fatal("duplicate stop or returned slice changed accounting")
	}
}

func TestSDKAssistantIgnoresUnmatchedAndInvalidBlockStops(t *testing.T) {
	for _, index := range []string{"null", "-1", "4096", "0.5", `"0"`, "0"} {
		t.Run(index, func(t *testing.T) {
			var response Response
			response.ObserveStreamLineAt([]byte(`data: {"type":"message_start","message":{"role":"assistant"}}`), baseTime)
			if index != "0" {
				response.ObserveStreamLineAt([]byte(fmt.Sprintf(`data: {"type":"content_block_start","index":%s,"content_block":{"type":"text"}}`, index)), baseTime)
			}
			response.ObserveStreamLineAt([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%s}`, index)), baseTime.Add(time.Second))
			if at, _ := response.SDKAssistantMessage(); !at.IsZero() {
				t.Fatal("unmatched/invalid stop generated an assistant object")
			}
		})
	}
}

func TestSDKAssistantJSONUsesArrivalTimeOnlyForAssistant(t *testing.T) {
	for _, role := range []string{"assistant", "user", ""} {
		var response Response
		response.ObservePayloadAt([]byte(fmt.Sprintf(`{"type":"message","role":%q,"content":[{"type":"text","text":"private"}],"stop_reason":"end_turn"}`, role)), false, baseTime)
		at, _ := response.SDKAssistantMessage()
		if (role == "assistant") != !at.IsZero() {
			t.Fatalf("unexpected observation for role %q", role)
		}
		response.ObservePayloadAt([]byte(`{"type":"message","role":"assistant"}`), false, baseTime.Add(time.Hour))
		if later, _ := response.SDKAssistantMessage(); later != at {
			t.Fatal("repeated payload changed the observation")
		}
	}
}
