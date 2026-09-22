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

func TestSDKWireContentMatchesPinnedNativeStream(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-wire-content-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string
			Events   []json.RawMessage
			Expected json.RawMessage
		}
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Cases) != 9 {
		t.Fatal("invalid native wire vectors")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var response Response
			response.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_native","role":"assistant"}}`))
			last := -1
			for _, raw := range tc.Events {
				var event struct {
					Type  string
					Index int
				}
				_ = json.Unmarshal(raw, &event)
				if event.Type == "content_block_start" {
					if last >= 0 {
						response.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, last)))
					}
					last = event.Index
				}
				response.ObserveStreamLine(append([]byte("data: "), raw...))
			}
			response.ObserveStreamLine([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, last)))
			response.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
			want, wantKnown := sdkWireMessageFingerprint("assistant", tc.Expected)
			got, known, complete := response.SDKWireFingerprint()
			if !wantKnown || !known || !complete || want != got {
				t.Fatal("stream content differs from the pinned native reducer", got, want)
			}
			if response.sdkWireStream.blocks != nil {
				t.Fatal("completed response retained content")
			}
			messages, _ := response.SDKHistoryMessages()
			var blocks []json.RawMessage
			_ = json.Unmarshal(tc.Expected, &blocks)
			if len(messages) != len(blocks) {
				t.Fatal("native stream yield count differs")
			}
			for index, block := range blocks {
				n, known := sdkBlockTokens(block, 0)
				if messages[index].TokenEstimate != (SDKTokenEstimate{Tokens: n, Known: known}) {
					t.Fatal("assembled block estimate differs", tc.Name, messages[index].TokenEstimate, n, known)
				}
			}
		})
	}
}

func TestSDKWireContentIdentityPreservesMeaningfulFields(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		equal             bool
	}{
		{"text-cache", `[{"type":"text","text":"ab"}]`, `[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}]`, true},
		{"tool-key-order", `[{"type":"tool_use","id":"t","name":"Custom","input":{"a":1.0,"b":true}}]`, `[{"name":"Custom","input":{"b":true,"a":1},"id":"t","type":"tool_use","cache_control":{"type":"ephemeral"}}]`, true},
		{"tool-argument-cache", `[{"type":"tool_use","id":"t","name":"Custom","input":{"cache_control":1}}]`, `[{"type":"tool_use","id":"t","name":"Custom","input":{"cache_control":2}}]`, false},
		{"thinking-signature", `[{"type":"thinking","thinking":"same","signature":"a"}]`, `[{"type":"thinking","thinking":"same","signature":"b"}]`, false},
		{"media-bytes", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]`, `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQ=="}}]`, false},
		{"document-title", `[{"type":"document","title":"a","source":{"type":"text","media_type":"text/plain","data":"same"}}]`, `[{"type":"document","title":"b","source":{"type":"text","media_type":"text/plain","data":"same"}}]`, false},
		{"citations", `[{"type":"text","text":"same","citations":[]}]`, `[{"type":"text","text":"same","citations":[{"start_char_index":1}]}]`, false},
		{"tool-error", `[{"type":"tool_result","tool_use_id":"t","content":"same","is_error":false}]`, `[{"type":"tool_result","tool_use_id":"t","content":"same","is_error":true}]`, false},
		{"nested-text", `[{"type":"tool_result","tool_use_id":"t","content":"same"}]`, `[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"same","cache_control":{"type":"ephemeral"}}]}]`, true},
		{"nested-surrogate", `[{"type":"tool_use","id":"t","name":"Custom","input":{"value":"\ud800"}}]`, `[{"type":"tool_use","id":"t","name":"Custom","input":{"value":"\ud801"}}]`, false},
		{"nested-key-surrogate", `[{"type":"tool_use","id":"t","name":"Custom","input":{"\ud800":1}}]`, `[{"type":"tool_use","id":"t","name":"Custom","input":{"\ud801":1}}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, l := sdkWireMessageFingerprint("user", []byte(tc.left))
			right, r := sdkWireMessageFingerprint("user", []byte(tc.right))
			if !l || !r || (left == right) != tc.equal {
				t.Fatal("content identity mismatch", left, right, l, r)
			}
		})
	}
}

func TestSDKWireContentManyTextFragmentsPreserveOneLinearRun(t *testing.T) {
	const count = 5000
	text := strings.Repeat("synthetic", 8)
	fragments := make([]map[string]any, 0, count+1)
	for range count {
		fragments = append(fragments, map[string]any{"type": "text", "text": text})
	}
	media := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "synthetic"}}
	fragments = append(fragments, media)
	left, _ := json.Marshal(fragments)
	right, _ := json.Marshal([]map[string]any{{"type": "text", "text": strings.Repeat(text, count)}, media})
	a, known := sdkWireMessageFingerprint("user", left)
	b, otherKnown := sdkWireMessageFingerprint("user", right)
	if !known || !otherKnown || a != b {
		t.Fatal("large text-run segmentation changed content identity")
	}
	blocks, known := sdkWireContentBlocks(left)
	if !known || len(blocks) != 2 {
		t.Fatal("large text runs did not collapse at the media boundary")
	}
}

func TestSDKWireMediaRetryMatchesNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-wire-content-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Media []struct {
			Name           string
			Rows, Expected []json.RawMessage
		}
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Media) != 4 {
		t.Fatal("invalid native media vectors")
	}
	for _, tc := range fixture.Media {
		t.Run(tc.Name, func(t *testing.T) {
			before, _ := json.Marshal(tc.Rows)
			rows, err := StripSDKWireMedia(tc.Rows)
			if err != nil {
				t.Fatal(err)
			}
			actual, _ := json.Marshal(rows)
			expected, _ := json.Marshal(tc.Expected)
			var a, e any
			_ = json.Unmarshal(actual, &a)
			_ = json.Unmarshal(expected, &e)
			if !reflect.DeepEqual(a, e) {
				t.Fatal("native media stripping differs")
			}
			after, _ := json.Marshal(tc.Rows)
			if string(before) != string(after) {
				t.Fatal("summary retry mutated the original history")
			}
		})
	}
}

func TestSDKWireUserMergeMatchesNativeFlagIndependentVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-wire-content-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Merges []struct {
			Name                  string
			Left, Right, Expected json.RawMessage
			Known                 bool
		}
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Merges) != 9 {
		t.Fatal("invalid native merge vectors")
	}
	for _, tc := range fixture.Merges {
		t.Run(tc.Name, func(t *testing.T) {
			actual, err := MergeSDKWireUserContent(tc.Left, tc.Right)
			if !tc.Known {
				if err == nil {
					t.Fatal("missing native feature decision was guessed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var a, e any
			_ = json.Unmarshal(actual, &a)
			_ = json.Unmarshal(tc.Expected, &e)
			if !reflect.DeepEqual(a, e) {
				t.Fatal("native user content normalization differs", string(actual), string(tc.Expected))
			}
		})
	}
}

func TestSDKWireStreamInvalidationAndBound(t *testing.T) {
	for _, events := range [][]string{
		{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"wrong"}}`},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"Custom","id":"t","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\ud800"}}`},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"error","error":{"type":"overloaded_error"}}`},
	} {
		var response Response
		response.ObserveStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg","role":"assistant"}}`))
		for _, event := range events {
			response.ObserveStreamLine([]byte("data: " + event))
		}
		response.ObserveStreamLine([]byte(`data: {"type":"content_block_stop","index":0}`))
		response.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
		if _, known, _ := response.SDKWireFingerprint(); known || response.sdkWireStream.blocks != nil {
			t.Fatal("invalid stream retained a trusted fingerprint or content")
		}
	}
	var stream sdkWireStream
	stream.begin(true)
	index := 0
	stream.start(&index, []byte(`{"type":"text","text":""}`))
	stream.bytes = maxSDKCompactionViewBytes
	stream.delta(&index, []byte(`{"type":"text_delta","text":"overflow"}`))
	if !stream.invalid || stream.blocks != nil {
		t.Fatal("bounded observer retained overflow")
	}
}

func completeSDKWireContent(t *testing.T, r *Request, id, content, stop string, tools []string) {
	t.Helper()
	var response Response
	response.ObservePayloadAt([]byte(fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","content":%s,"stop_reason":%q,"usage":{"input_tokens":10,"output_tokens":2}}`, id, content, stop)), false, baseTime)
	r.ObserveSDKHistory(response.SDKHistoryMessages())
	r.ObserveSDKWireResponse(response.SDKWireFingerprint())
	r.FinishSuccess(baseTime.Add(time.Second), stop, tools)
}

func TestSDKWireToolMediaHistoryResolvesMergedAndSplitResults(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			var tracker Tracker
			i := input("wire-first")
			const user = `{"role":"user","content":[{"type":"text","text":"PRIVATE_PROMPT"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"PRIVATE_IMAGE"}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"PRIVATE_DOCUMENT"}}]}`
			const assistant = `[{"type":"thinking","thinking":"PRIVATE_THOUGHT","signature":"PRIVATE_SIGNATURE"},{"type":"tool_use","id":"tool-a","name":"Custom","input":{"path":"PRIVATE_A"}},{"type":"tool_use","id":"tool-b","name":"Custom","input":{"path":"PRIVATE_B"}}]`
			const resultA = `{"type":"tool_result","tool_use_id":"tool-a","content":"PRIVATE_RESULT_A"}`
			const resultB = `{"type":"tool_result","tool_use_id":"tool-b","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"PRIVATE_RESULT_B"}}]}`
			i.Body = []byte(`{"messages":[` + user + `]}`)
			first := tracker.Begin(i)
			first.ObserveSDKQuery(i.Body)
			completeSDKWireContent(t, first, "msg-wire", assistant, "tool_use", []string{"tool-a", "tool-b"})
			prefix := user + `,{"role":"assistant","content":` + assistant + `}`
			merged := `{"role":"user","content":[` + resultA + `,` + resultB + `]}`
			splitRows := `{"role":"user","content":[` + resultA + `]},{"role":"user","content":[` + resultB + `]}`
			results := merged
			if split {
				results = splitRows
			}
			i.ClientRequestID = "wire-results"
			i.Body = []byte(`{"messages":[` + prefix + `,` + results + `]}`)
			next := tracker.Begin(i)
			next.ObserveSDKQuery(i.Body)
			if next.Identity().PromptID != first.Identity().PromptID || !next.SDKHistory().OwnedMessagesKnown {
				t.Fatal("tool history lost its real owner", next.SDKHistory())
			}
			view, err := next.CompactionView(i.Body)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := view.Resolve(view.History().Messages)
			if err != nil {
				t.Fatal(err)
			}
			var expected struct{ Messages []json.RawMessage }
			_ = json.Unmarshal(i.Body, &expected)
			if !reflect.DeepEqual(rows, expected.Messages) {
				t.Fatal("non-text content changed during resolution")
			}
			view.Discard()
			completeSDKWireContent(t, next, "msg-done", `[{"type":"text","text":"PRIVATE_REPLY"}]`, "end_turn", nil)
			if split {
				results = merged
			} else {
				results = splitRows
			}
			i.ClientRequestID = "wire-next"
			i.Body = []byte(`{"messages":[` + prefix + `,` + results + `,{"role":"assistant","content":"PRIVATE_REPLY"},{"role":"user","content":"PRIVATE_NEXT"}]}`)
			last := tracker.Begin(i)
			last.ObserveSDKQuery(i.Body)
			view, err = last.CompactionView(i.Body)
			if err != nil {
				t.Fatal("row regrouping lost ownership", err, last.SDKHistory())
			}
			defer view.Discard()
			if strings.Contains(fmt.Sprintf("%+v %+v", last.SDKHistory(), last.call.state.sdk.history), "PRIVATE_") {
				t.Fatal("session retained raw content")
			}
		})
	}
}

func TestSDKWireToolContinuationCannotRewriteOwnedPast(t *testing.T) {
	for _, changed := range []string{
		`[{"type":"tool_use","id":"t","name":"Custom","input":{"value":2}}]`,
		`[{"type":"tool_use","id":"t","name":"Different","input":{"value":1}}]`,
		`[{"type":"thinking","thinking":"injected","signature":"injected"},{"type":"tool_use","id":"t","name":"Custom","input":{"value":1}}]`,
	} {
		var tracker Tracker
		i := input("untampered")
		first := tracker.Begin(i)
		first.ObserveSDKQuery(i.Body)
		completeSDKWireContent(t, first, "msg-source", `[{"type":"tool_use","id":"t","name":"Custom","input":{"value":1}}]`, "tool_use", []string{"t"})
		i.ClientRequestID = "tampered"
		i.Body = []byte(`{"messages":[{"role":"user","content":"synthetic prompt"},{"role":"assistant","content":` + changed + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"synthetic"}]}]}`)
		next := tracker.Begin(i)
		next.ObserveSDKQuery(i.Body)
		if h := next.SDKHistory(); h.OwnedMessagesKnown || h.IncompleteReason != "changed-sdk-caller-history" {
			t.Fatal("matching tool ID hid changed prior content", h.IncompleteReason)
		}
		if _, err := next.CompactionView(i.Body); err == nil {
			t.Fatal("changed caller past obtained owned content")
		}
	}
}
