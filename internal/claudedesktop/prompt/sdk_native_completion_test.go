package prompt

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSDKSidechainCompletionUsesNativeReducer(t *testing.T) {
	tracker, _ := sidechainTestOwner(t)
	s, err := tracker.OpenSidechain("a", "s", "a12345678", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.AppendInput(json.RawMessage(`"input"`), uuid.NewString(), time.Now()) != nil {
		t.Fatal("initial input failed")
	}
	var response Response
	response.EnableNativeContent()
	lines := []string{
		`{"type":"message_start","message":{"id":"msg_native","type":"message","role":"assistant","model":"sonnet","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"DISCARD_INITIAL"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"native text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","document_index":1}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"DISCARD_INITIAL","signature":"DISCARD_INITIAL"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"actual thinking"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"actual signature"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_details":{"fallback_credit_token":"DO_NOT_FORWARD","reason":"native"}},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}
	for _, line := range lines {
		response.ObserveStreamLine([]byte("data: " + line))
		if s.Observe(response.NativeContentMessages()) != nil {
			t.Fatal("per-yield observation failed")
		}
	}
	raw, err := response.NativeCompletedMessage()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("DISCARD_INITIAL")) || bytes.Contains(raw, []byte("citations")) || bytes.Contains(raw, []byte("DO_NOT_FORWARD")) {
		t.Fatal("generic collector semantics leaked into native task consumer")
	}
	var value struct {
		ID      string
		Content []map[string]any
		Usage   struct {
			OutputTokens int `json:"output_tokens"`
		}
	}
	if json.Unmarshal(raw, &value) != nil || value.ID != "msg_native" || len(value.Content) != 2 || value.Content[0]["text"] != "native text" || value.Content[1]["thinking"] != "actual thinking" || value.Content[1]["signature"] != "actual signature" || value.Usage.OutputTokens != 9 {
		t.Fatal("native completion lost actual content/usage")
	}
	if s.VerifyResponse(raw) != nil {
		t.Fatal("observed per-yield content differs from actual consumer")
	}
	rows := sidechainRows(t, s)
	if len(rows) != 3 || rows[1].UUID == rows[2].UUID {
		t.Fatal("native streaming yields were aggregated in transcript")
	}
	var incomplete Response
	incomplete.EnableNativeContent()
	incomplete.ObserveStreamLine([]byte("data: " + lines[0]))
	if _, err := incomplete.NativeCompletedMessage(); err == nil {
		t.Fatal("incomplete response accepted")
	}
}

func TestSDKSidechainRejectsUnobservedCompletedReport(t *testing.T) {
	for _, mode := range []string{"no-response", "different-id", "different-content"} {
		t.Run(mode, func(t *testing.T) {
			tracker, _ := sidechainTestOwner(t)
			s, err := tracker.OpenSidechain("a", "s", "a12345678", "")
			if err != nil {
				t.Fatal(err)
			}
			if s.AppendInput(json.RawMessage(`"input"`), uuid.NewString(), time.Now()) != nil {
				t.Fatal("input failed")
			}
			raw := []byte(`{"id":"msg_one","role":"assistant","content":[{"type":"text","text":"observed"}],"stop_reason":"end_turn"}`)
			if mode != "no-response" {
				var response Response
				response.EnableNativeContent()
				response.ObservePayload(raw, false)
				if s.Observe(response.NativeContentMessages()) != nil {
					t.Fatal("observation failed")
				}
			}
			if mode == "different-id" {
				raw = bytes.ReplaceAll(raw, []byte("msg_one"), []byte("msg_other"))
			}
			if mode == "different-content" {
				raw = bytes.ReplaceAll(raw, []byte("observed"), []byte("not observed"))
			}
			if s.VerifyResponse(raw) == nil || s.Path() != "" {
				t.Fatal("unobserved report advertised as retrievable")
			}
		})
	}
}

func TestSDKSidechainEmptyCompletionDoesNotInventContent(t *testing.T) {
	tracker, _ := sidechainTestOwner(t)
	s, err := tracker.OpenSidechain("a", "s", "a12345678", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.AppendInput(json.RawMessage(`"actual input"`), uuid.NewString(), time.Now()) != nil {
		t.Fatal("input failed")
	}
	var response Response
	response.EnableNativeContent()
	for _, line := range []string{
		`{"type":"message_start","message":{"id":"msg_empty","role":"assistant","type":"message","model":"sonnet","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
		`{"type":"message_stop"}`,
	} {
		response.ObserveStreamLine([]byte("data: " + line))
		if s.Observe(response.NativeContentMessages()) != nil {
			t.Fatal("empty observation rejected")
		}
	}
	raw, err := response.NativeCompletedMessage()
	if err != nil || s.VerifyResponse(raw) != nil {
		t.Fatal("valid empty completion rejected", err)
	}
	if !bytes.Contains(raw, []byte(`"content":[]`)) || !bytes.Contains(raw, []byte(`"id":"msg_empty"`)) {
		t.Fatal("actual empty response header lost")
	}
	if len(sidechainRows(t, s)) != 1 {
		t.Fatal("empty completion invented an assistant transcript row")
	}
}
