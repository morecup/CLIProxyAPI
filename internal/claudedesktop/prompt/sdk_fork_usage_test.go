package prompt

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSDKForkUsageMatchesPinnedNativeAccumulator(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-summary-dependencies-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Name     string
			Events   []json.RawMessage
			Expected SDKTokenUsage
		}
	}
	if json.Unmarshal(data, &vectors) != nil || len(vectors.Cases) != 8 {
		t.Fatal("invalid native fork usage vectors")
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			var usage SDKForkUsage
			for _, event := range vector.Events {
				usage.ObserveStreamLine(append([]byte("data: "), event...))
			}
			usage.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
			got, known := usage.Snapshot()
			if !known || got != vector.Expected {
				t.Fatalf("native fork usage mismatch: %+v want %+v known=%v", got, vector.Expected, known)
			}
		})
	}
}

func TestSDKForkUsageIsDeltaSumNotParentOrResponseUsage(t *testing.T) {
	var usage SDKForkUsage
	for _, event := range []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":900,"output_tokens":0}}}`,
		`{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}`,
		`{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":2,"cache_creation":{"ephemeral_1h_input_tokens":7,"ephemeral_5m_input_tokens":8}}}`,
	} {
		usage.ObserveStreamLine([]byte("data: " + event))
	}
	if _, known := usage.Snapshot(); known {
		t.Fatal("unfinished fork supplied complete usage")
	}
	usage.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
	got, known := usage.Snapshot()
	if !known || got != (SDKTokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 20, CacheCreationInputTokens: 20}) {
		t.Fatalf("independent native fork total=%+v known=%v", got, known)
	}
}

func TestSDKForkUsageZeroAndInvalidAreDifferent(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		var usage SDKForkUsage
		if invalid {
			usage.ObserveStreamLine([]byte(`data: {"type":"message_delta","usage":{"input_tokens":-1}}`))
		}
		usage.ObserveStreamLine([]byte(`data: {"type":"message_stop"}`))
		got, known := usage.Snapshot()
		if got != (SDKTokenUsage{}) || known == invalid {
			t.Fatal("native zero fork total was confused with invalid usage")
		}
	}
}
