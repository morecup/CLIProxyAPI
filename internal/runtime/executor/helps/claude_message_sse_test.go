package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func claudeSyntheticSSE(events ...string) []byte {
	return []byte("data: " + strings.Join(events, "\n\ndata: ") + "\n\n")
}

func TestCollectClaudeMessagePreservesBlocksAndUsage(t *testing.T) {
	data := claudeSyntheticSSE(
		`{"type":"message_start","message":{"type":"message","id":"msg_synthetic","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":7,"cache_read_input_tokens":9},"stop_reason":null}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"synthetic thought"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"synthetic-signature"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"part ","citations":[]}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"two"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"page_location","start_page_number":1,"end_page_number":2}}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_synthetic","name":"Read","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"synthetic\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":11,"server_tool_use":{"web_search_requests":1}}}`,
		`{"type":"message_stop"}`,
	)
	message, err := CollectClaudeMessageSSE(data)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"type": "message", "id": "msg_synthetic", "content.0.thinking": "synthetic thought", "content.0.signature": "synthetic-signature", "content.1.text": "part two", "content.2.id": "tool_synthetic", "content.2.input.path": "synthetic", "stop_reason": "tool_use"} {
		if gjson.GetBytes(message, key).String() != want {
			t.Fatalf("%s not preserved", key)
		}
	}
	for key, want := range map[string]int64{"content.#": 3, "content.1.citations.#": 1, "usage.input_tokens": 7, "usage.cache_read_input_tokens": 9, "usage.output_tokens": 11, "usage.server_tool_use.web_search_requests": 1} {
		if gjson.GetBytes(message, key).Int() != want {
			t.Fatalf("%s not preserved", key)
		}
	}
}

func TestCollectClaudeMessageRejectsUnsettledOrUnknownDeltas(t *testing.T) {
	start := `{"type":"message_start","message":{"type":"message","content":[],"stop_reason":null}}`
	block := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	for _, events := range [][]string{
		{start, block},
		{start, block, `{"type":"message_stop"}`},
		{start, block, `{"type":"content_block_delta","index":0,"delta":{"type":"future_delta","text":"PRIVATE_VALUE"}}`},
		{start, block, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		{start, block, `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		{start, `{"type":"content_block_stop","index":0}`},
		{start, `{"type":"error","error":{"message":"PRIVATE_VALUE"}}`},
		{block},
	} {
		result, err := CollectClaudeMessageSSE(claudeSyntheticSSE(events...))
		if err == nil || result != nil {
			t.Fatal("invalid stream accepted")
		}
		if strings.Contains(err.Error(), "PRIVATE_VALUE") {
			t.Fatal("response content in error")
		}
	}
}
