package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestSDKTokensMatchNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-tokens-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Content []struct {
			Name    string
			Content json.RawMessage
			Raw     string
			Tokens  int64
		}
		History []struct {
			Name     string
			Tokens   int64
			Messages []SDKHistoryMessage
		}
		Usage []struct {
			Name     string
			Updates  []json.RawMessage
			Expected SDKTokenUsage
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Content) != 36 || len(fixture.History) != 11 || len(fixture.Usage) != 5 {
		t.Fatal("incomplete native token fixture")
	}
	for _, tc := range fixture.Content {
		t.Run("content/"+tc.Name, func(t *testing.T) {
			raw := tc.Content
			if tc.Raw != "" {
				raw = json.RawMessage(tc.Raw)
			}
			got := EstimateSDKContent(raw)
			if !got.Known || got.Tokens != tc.Tokens {
				t.Fatalf("got=%+v want=%d", got, tc.Tokens)
			}
		})
	}
	for _, tc := range fixture.History {
		t.Run("history/"+tc.Name, func(t *testing.T) {
			got := EstimateSDKHistory(tc.Messages)
			if !got.Known || got.Tokens != tc.Tokens {
				t.Fatalf("got=%+v want=%d", got, tc.Tokens)
			}
		})
	}
	for _, tc := range fixture.Usage {
		t.Run("usage/"+tc.Name, func(t *testing.T) {
			var observer sdkResponseTokens
			for _, update := range tc.Updates {
				observer.observeUsage(update)
			}
			if !observer.knownUsage() || observer.usage != tc.Expected {
				t.Fatalf("got=%+v want=%+v", observer, tc.Expected)
			}
		})
	}
}

func TestSDKTokenStreamingUsageUpdatesAllYieldsOnce(t *testing.T) {
	var tracker Tracker
	r := tracker.Begin(input("tokens"))
	r.ObserveSDKQuery([]byte(`{"messages":[{"role":"user","content":"abcd"}]}`))
	response := &Response{}
	observe := func(event string) {
		response.ObserveStreamLine([]byte("data: " + event))
		r.ObserveSDKHistory(response.SDKHistoryMessages())
	}
	observe(`{"type":"message_start","message":{"id":"same","role":"assistant","model":"real","usage":{"input_tokens":100,"cache_read_input_tokens":20,"output_tokens":0}}}`)
	for index, text := range []string{"abc", "de", "中文😀"} {
		observe(fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":"ignored initial content"}}`, index))
		observe(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text))
		observe(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
	}
	observe(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":9}}`)
	observe(`{"type":"message_stop"}`)
	r.ObserveSDKHistory(response.SDKHistoryMessages())
	history := r.SDKHistory()
	if len(history.Messages) != 4 || !history.TokenEstimate.Known || history.TokenEstimate.Tokens != 131 {
		t.Fatalf("history=%+v", history)
	}
	for _, m := range history.Messages[1:] {
		if !m.UsageKnown || m.Usage.InputTokens != 100 || m.Usage.OutputTokens != 9 || m.TokenEstimate.Tokens != 1 {
			t.Fatalf("yield=%+v", m)
		}
	}
	if history.Messages[0].TokenEstimate.Tokens != 1 {
		t.Fatal("user estimate missing")
	}
	if strings.Contains(fmt.Sprintf("%+v", history), "ignored initial content") {
		t.Fatal("retained text")
	}
}

func TestSDKTokenUnknownAndExcludedFacts(t *testing.T) {
	for _, raw := range []string{`garbage`, `[{"type":"tool_use","name":"x","input":`, strings.Repeat(`[`, 130) + `"x"` + strings.Repeat(`]`, 130)} {
		if got := EstimateSDKContent([]byte(raw)); got.Known {
			t.Fatalf("malformed/oversized nesting known: %+v", got)
		}
	}
	for _, raw := range []string{`{"input_tokens":1,"output_tokens":-1}`, `{"input_tokens":1.1,"output_tokens":0}`, `{"input_tokens":1,"output_tokens":1125899906842625}`} {
		var r sdkResponseTokens
		r.observeUsage([]byte(raw))
		if r.knownUsage() {
			t.Fatal("invalid usage known")
		}
	}
	for _, text := range []string{"No response requested.", "[Request interrupted by user]", "[Request interrupted by user for tool use]"} {
		var r Response
		r.ObservePayload([]byte(fmt.Sprintf(`{"type":"message","role":"assistant","id":"id","content":[{"type":"text","text":%q}],"usage":{"input_tokens":999,"output_tokens":0}}`, text)), false)
		messages, _ := r.SDKHistoryMessages()
		if len(messages) != 1 || !messages[0].UsageExcluded || EstimateSDKHistory(messages).Tokens == 999 {
			t.Fatal("sentinel used as usage anchor")
		}
	}
	if got := EstimateSDKHistory([]SDKHistoryMessage{{Type: "user"}}); got.Known {
		t.Fatal("missing content became zero tokens")
	}
}

func TestSDKTokenToolAndThinkingStreamEstimates(t *testing.T) {
	var r Response
	r.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"role":"assistant","id":"id"}}`))
	for index, tc := range []struct {
		start, delta string
		want         int64
	}{
		{`{"type":"thinking","thinking":"ignored"}`, `{"type":"thinking_delta","thinking":"abcdef"}`, 2},
		{`{"type":"tool_use","name":"test","input":{}}`, `{"type":"input_json_delta","partial_json":"{\"x\":1}"}`, 3},
		{`{"type":"redacted_thinking","data":"abcdef"}`, ``, 2},
	} {
		r.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":%s}`, index, tc.start)))
		if tc.delta != "" {
			r.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":%s}`, index, tc.delta)))
		}
		r.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index)))
		messages, _ := r.SDKHistoryMessages()
		got := messages[len(messages)-1].TokenEstimate
		if !got.Known || got.Tokens != tc.want {
			t.Fatalf("block %d: %+v want %d", index, got, tc.want)
		}
	}
	if r.sdkTokens.blocks[1].input != nil || r.sdkTokens.bufferedInputBytes != 0 {
		t.Fatal("tool metadata retained after estimate")
	}
}

func TestSDKTokenStreamInputBoundIsPerResponse(t *testing.T) {
	var r sdkResponseTokens
	r.start(0, []byte(`{"type":"tool_use","name":"one"}`))
	r.start(1, []byte(`{"type":"tool_use","name":"two"}`))
	delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": strings.Repeat("x", maxSDKTokenBlockBytes/2+1)})
	r.delta(0, delta)
	r.delta(1, delta)
	if r.bufferedInputBytes != maxSDKTokenBlockBytes/2+1 || !r.blocks[1].invalid || r.blocks[1].input != nil {
		t.Fatal("multiple open blocks bypassed response bound")
	}
	if r.close(0).Known || r.close(1).Known || r.bufferedInputBytes != 0 {
		t.Fatal("invalid/oversized block retained input or became known")
	}
	// An unknown malformed block must not poison later independently valid input.
	r.start(2, []byte(`{"type":"tool_use","name":"three"}`))
	r.delta(2, []byte(`{"type":"input_json_delta","partial_json":"{}"}`))
	if !r.close(2).Known || r.bufferedInputBytes != 0 {
		t.Fatal("bounded buffer was not released")
	}
}

func TestSDKTokenSummaryAndInterruptionUserYields(t *testing.T) {
	var tracker Tracker
	_, in := compactAdoptionFixture(t, &tracker)
	next := tracker.Begin(in)
	next.ObserveSDKQuery(in.Body)
	history := next.SDKHistory()
	if len(history.Messages) != 2 || history.Messages[1].Type != "user" || !history.Messages[1].TokenEstimate.Known || history.Messages[1].TokenEstimate.Tokens == 0 {
		t.Fatalf("summary user tokens=%+v", history)
	}
	next.FinalizeCancellation(baseTime)
	history = next.SDKHistory()
	if len(history.Messages) != 3 || !history.Messages[2].TokenEstimate.Known || history.Messages[2].TokenEstimate.Tokens != 7 {
		t.Fatalf("interruption tokens=%+v", history)
	}
}
