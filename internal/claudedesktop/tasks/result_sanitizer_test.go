package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func readTaskResultVectors(t *testing.T, target any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/task-results-native.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTaskResultSanitizer(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Input      string          `json:"input"`
			Provenance bool            `json:"provenance"`
			Prepend    bool            `json:"prepend"`
			Expected   sanitizedResult `json:"expected"`
		} `json:"sanitizer_cases"`
	}
	readTaskResultVectors(t, &fixture)
	if len(fixture.Cases) != 68 {
		t.Fatalf("missing pinned vectors: %d", len(fixture.Cases))
	}
	for i, row := range fixture.Cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			actual, err := sanitizeResult(row.Input, row.Provenance, row.Prepend)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, row.Expected) {
				t.Fatalf("got %#v; want %#v", actual, row.Expected)
			}
		})
	}
}
