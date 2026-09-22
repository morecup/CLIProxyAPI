package prompt

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestSDKTranscriptSelectionMatchesPinnedNative(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-transcript-selection.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Name     string            `json:"name"`
			Rows     []json.RawMessage `json:"rows"`
			Expected struct {
				Rows []SDKNativeMessage `json:"rows"`
			} `json:"expected"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Vectors) != 19 {
		t.Fatal("native fixture cases were lost")
	}
	for _, vector := range fixture.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			var raw []json.RawMessage
			var rows []SDKNativeMessage
			for _, value := range vector.Rows {
				var row SDKNativeMessage
				if json.Unmarshal(value, &row) != nil {
					t.Fatal("invalid fixture")
				}
				if row.Type == "user" || row.Type == "assistant" {
					raw, rows = append(raw, value), append(rows, row)
				}
			}
			leaf, cleared, err := sdkRemoteTranscriptSelection(vector.Rows, rows)
			if err != nil {
				t.Fatal(err)
			}
			var selected []SDKNativeMessage
			if !cleared {
				selected, err = sdkCompletedRemoteChain(raw, rows, leaf, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			var got, want []string
			for _, row := range selected {
				got = append(got, row.UUID)
			}
			for _, row := range vector.Expected.Rows {
				want = append(want, row.UUID)
			}
			if !reflect.DeepEqual(got, want) || cleared != (len(want) == 0) {
				t.Fatalf("selected %v (cleared=%t), native wants %v", got, cleared, want)
			}
		})
	}
}
