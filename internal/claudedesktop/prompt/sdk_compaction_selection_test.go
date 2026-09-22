package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestSDKCompactionSelectionMatchesNativeAssistantVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-compaction-selection-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name               string
			Content            []json.RawMessage
			Buffered, Streamed *string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 12 {
		t.Fatal("incomplete selection fixture")
	}
	for _, tc := range fixture.Cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.Name, stream), func(t *testing.T) {
				var response SDKCompactionResponse
				want := tc.Buffered
				if !stream {
					response.ObserveJSON(sdkCompactJSON(t, map[string]any{"type": "message", "role": "assistant", "stop_reason": "end_turn", "content": tc.Content}))
				} else {
					want = tc.Streamed
					response.ObserveJSON([]byte(`{"type":"message_start","message":{"role":"assistant"}}`))
					for index, raw := range tc.Content {
						var block struct{ Type, Text string }
						if json.Unmarshal(raw, &block) != nil {
							t.Fatal("invalid block fixture")
						}
						response.ObserveJSON(sdkCompactJSON(t, map[string]any{"type": "content_block_start", "index": index, "content_block": raw}))
						if block.Type == "text" {
							response.ObserveJSON(sdkCompactJSON(t, map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": block.Text}}))
						}
						response.ObserveJSON(sdkCompactJSON(t, map[string]any{"type": "content_block_stop", "index": index}))
					}
					response.ObserveJSON([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
					response.ObserveJSON([]byte(`{"type":"message_stop"}`))
				}
				got := response.TakeSummary("1.40609.0.0", "2.1.247")
				if want == nil {
					if got.reviewed {
						t.Fatal("native absent summary became known")
					}
				} else if !got.reviewed || got.bytes != len(*want) || got.sha256 != sdkCompactionHash(*want) {
					t.Fatal("selected summary differs from native")
				}
				if response.text != nil || response.blocks != nil {
					t.Fatal("response content retained")
				}
			})
		}
	}
}

func TestSDKCompactionSelectionDoesNotInventStopReasonGate(t *testing.T) {
	for _, reason := range []string{"end_turn", "max_tokens", "refusal", "tool_use"} {
		t.Run(reason, func(t *testing.T) {
			var response SDKCompactionResponse
			response.ObserveJSON(sdkCompactJSON(t, map[string]any{"type": "message", "role": "assistant", "stop_reason": reason, "content": []map[string]any{{"type": "text", "text": "<summary>synthetic</summary>"}}}))
			if !response.TakeSummary("1.40609.0.0", "2.1.247").reviewed {
				t.Fatal("stop reason replaced native text selection")
			}
		})
	}
}
