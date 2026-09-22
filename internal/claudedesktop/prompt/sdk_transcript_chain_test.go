package prompt

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestSDKTranscriptChainMatchesPinnedNative(t *testing.T) {
	payload, err := os.ReadFile("testdata/sdk-transcript-chain.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Name, Leaf string
			Rows       []json.RawMessage
			Expected   struct {
				Rows   []json.RawMessage
				Events []sdkResumeEvent
			}
		}
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			before, _ := json.Marshal(vector.Rows)
			result, err := sdkReconstructTranscriptChain(vector.Rows, vector.Leaf)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.rows) != len(vector.Expected.Rows) || len(result.events) != len(vector.Expected.Events) {
				t.Fatalf("chain/event counts: got %d/%d, want %d/%d", len(result.rows), len(result.events), len(vector.Expected.Rows), len(vector.Expected.Events))
			}
			normalized := func(value any) any {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var plain any
				if json.Unmarshal(encoded, &plain) != nil {
					t.Fatal("invalid chain fixture")
				}
				return plain
			}
			if !reflect.DeepEqual(normalized(result.rows), normalized(vector.Expected.Rows)) {
				t.Fatal("chain differs from pinned native rows")
			}
			for index, event := range result.events {
				if !reflect.DeepEqual(normalized(event), normalized(vector.Expected.Events[index])) {
					t.Fatal("native chain diagnostic differs", index)
				}
			}
			// No mutation of input fields, links or shared message objects.
			if len(result.rows) != 0 {
				result.rows[0][0] = ' '
			}
			after, _ := json.Marshal(vector.Rows)
			if !bytes.Equal(before, after) {
				t.Fatal("chain reconstruction mutated original transcript")
			}
		})
	}
}

func TestSDKTranscriptChainInvalidInput(t *testing.T) {
	row := json.RawMessage(`{"type":"user","uuid":"u","message":{"role":"user","content":"x"}}`)
	for _, rows := range [][]json.RawMessage{nil, {row, row}, {json.RawMessage(`null`)}, {json.RawMessage(`{"type":"user"}`)}} {
		if _, err := sdkReconstructTranscriptChain(rows, "u"); err == nil {
			t.Fatal("invalid graph accepted")
		}
	}
	if _, err := sdkReconstructTranscriptChain([]json.RawMessage{row}, "missing"); err == nil {
		t.Fatal("unknown leaf accepted")
	}
}
