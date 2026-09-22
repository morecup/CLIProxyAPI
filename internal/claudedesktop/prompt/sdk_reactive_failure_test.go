package prompt

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestSDKReactiveFailuresMatchNativeHelpers(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-reactive-failure-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Text string `json:"text"`
			SDKReactiveFailure
		}
	}
	if json.Unmarshal(data, &vectors) != nil || len(vectors.Cases) != 25 {
		t.Fatal("invalid native failure vectors")
	}
	for index, tc := range vectors.Cases {
		if got := ClassifySDKReactiveFailure(tc.Text); !reflect.DeepEqual(got, tc.SDKReactiveFailure) {
			t.Fatalf("native failure vector %d differs: got=%+v want=%+v", index, got, tc.SDKReactiveFailure)
		}
	}
	for _, text := range []string{"prompt is too long: 999999999999999999999999 tokens > 1", "prompt is too long: 9007199254740992 tokens > 1"} {
		if got := ClassifySDKReactiveFailure(text); got.Reason != "prompt_too_long" || got.TokenGap != nil {
			t.Fatal("out-of-bound token gap was reported as exact")
		}
	}
}
