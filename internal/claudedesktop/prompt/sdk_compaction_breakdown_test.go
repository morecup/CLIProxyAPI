package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSDKCompactionBreakdownUsesSerializedBlocksAndPrivateReadAggregation(t *testing.T) {
	rows := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"abcdefgh"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{"file_path":"PRIVATE_PATH"}},{"type":"tool_use","id":"b","name":"Read","input":{"file_path":"PRIVATE_PATH"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"PRIVATE_TEXT"},{"type":"tool_result","tool_use_id":"b","content":"PRIVATE_TEXT"}]}`),
	}
	got, err := ObserveSDKCompactionBreakdown(rows, []string{"environment", "hook_success", "hook_success"}, func(name string) string { return name })
	if err != nil {
		t.Fatal(err)
	}
	m := got.Metadata()
	if m["human_message_tokens"] != 2 || m["attachment_hook_success_count"] != 2 || m["duplicate_read_file_count"] != 1 || m["duplicate_read_tokens"] != m["tool_result_Read_tokens"]/2 || m["tool_request_Read_tokens"] == 0 {
		t.Fatal(m)
	}
	if m["total_tokens"] != m["human_message_tokens"]+m["tool_request_Read_tokens"]+m["tool_result_Read_tokens"] {
		t.Fatal("incorrect total", m)
	}
	data, _ := json.Marshal(m)
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("raw content leaked")
	}
	m["total_tokens"] = -1
	if got.Metadata()["total_tokens"] < 0 {
		t.Fatal("diagnostics mutated by consumer")
	}
	block, err := ObserveSDKCompactionBreakdown([]json.RawMessage{json.RawMessage(`{"role":"user","content":[{"type":"text","text":"abcdefgh"}]}`)}, nil, nil)
	if err != nil || block.Metadata()["human_message_tokens"] != 8 {
		t.Fatal("block framing was not estimated", err, block.Metadata())
	}
}

func TestSDKCompactionBreakdownRejectsPostCacheOrUnreviewedInput(t *testing.T) {
	for _, raw := range []string{`{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}`, `{"role":"system","content":"x"}`, `{"role":"user","content":[{"type":"future"}]}`} {
		if _, err := ObserveSDKCompactionBreakdown([]json.RawMessage{json.RawMessage(raw)}, nil, nil); err == nil {
			t.Fatal("unreviewed input accepted")
		}
	}
	zero, err := ObserveSDKCompactionBreakdown(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := zero.Metadata()["human_message_percent"]; present {
		t.Fatal("zero denominator emitted a percent")
	}
}
