package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestSDKCompactionHooksNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-restoration-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Hooks []struct {
			Name            string                    `json:"name"`
			Isolated        bool                      `json:"isolated"`
			Results         []SDKCompactHookExecution `json:"results"`
			Pre             SDKCompactHookOutcome     `json:"pre"`
			Post            SDKCompactHookOutcome     `json:"post"`
			InvocationCount int                       `json:"invocationCount"`
		} `json:"hooks"`
	}
	if err = json.Unmarshal(data, &vectors); err != nil || len(vectors.Hooks) != 18 {
		t.Fatalf("invalid native hook vectors: %v", err)
	}
	for _, vector := range vectors.Hooks {
		name := vector.Name
		if vector.Isolated {
			name += "/isolated"
		}
		t.Run(name, func(t *testing.T) {
			var calls []SDKCompactHookInput
			runner := func(ctx context.Context, input SDKCompactHookInput) ([]SDKCompactHookExecution, error) {
				calls = append(calls, input)
				return vector.Results, nil
			}
			pre, errPre := RunSDKPreCompactHooks(context.Background(), runner, "auto", nil, vector.Isolated)
			post, errPost := RunSDKPostCompactHooks(context.Background(), runner, "auto", "synthetic-summary", vector.Isolated)
			vector.Pre.completedEvent = "PreCompact"
			vector.Post.completedEvent = "PostCompact"
			if errPre != nil || errPost != nil || pre != vector.Pre || post != vector.Post || len(calls) != vector.InvocationCount {
				t.Fatalf("native hook mismatch: pre=%#v post=%#v calls=%d errors=%v/%v", pre, post, len(calls), errPre, errPost)
			}
			if calls[0].Event != "PreCompact" || calls[0].Trigger != "auto" || calls[0].CustomInstructions != nil || calls[0].CompactSummary != "" {
				t.Fatal("incorrect PreCompact input")
			}
			if len(calls) == 2 && (calls[1].Event != "PostCompact" || calls[1].CompactSummary != "synthetic-summary" || calls[1].CustomInstructions != nil) {
				t.Fatal("incorrect PostCompact input")
			}
		})
	}
}

func TestSDKCompactionHookRunnerRequiredAndErrorsPropagated(t *testing.T) {
	if _, err := RunSDKPreCompactHooks(context.Background(), nil, "auto", nil, false); !errors.Is(err, ErrSDKCompactionHooksUnknown) {
		t.Fatal("missing PreCompact runner was treated as no hooks")
	}
	if _, err := RunSDKPreCompactHooks(context.Background(), nil, "auto", nil, true); !errors.Is(err, ErrSDKCompactionHooksUnknown) {
		t.Fatal("isolated PreCompact still requires an owned runner")
	}
	if _, err := RunSDKPostCompactHooks(context.Background(), nil, "auto", "summary", false); !errors.Is(err, ErrSDKCompactionHooksUnknown) {
		t.Fatal("missing PostCompact runner was treated as no hooks")
	}
	if _, err := RunSDKPostCompactHooks(context.Background(), nil, "auto", "summary", true); err != nil {
		t.Fatal("isolated PostCompact must skip the runner")
	}
	want := errors.New("synthetic-runner-error")
	runner := func(context.Context, SDKCompactHookInput) ([]SDKCompactHookExecution, error) { return nil, want }
	if _, err := RunSDKPreCompactHooks(context.Background(), runner, "auto", nil, false); !errors.Is(err, want) {
		t.Fatal("PreCompact runner exception lost")
	}
	if _, err := RunSDKPostCompactHooks(context.Background(), runner, "auto", "summary", false); !errors.Is(err, want) {
		t.Fatal("PostCompact runner exception lost")
	}
}

func TestSDKCompactionHookOwnedContextAndInput(t *testing.T) {
	type marker struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), marker{}, "owned"))
	cancel()
	instructions := "unchanged custom instruction"
	runner := func(actual context.Context, input SDKCompactHookInput) ([]SDKCompactHookExecution, error) {
		if actual != ctx || actual.Value(marker{}) != "owned" || actual.Err() != context.Canceled || !reflect.DeepEqual(input.CustomInstructions, &instructions) || input.Trigger != "manual" {
			t.Fatal("hook cancellation, owner or input was replaced")
		}
		return nil, actual.Err()
	}
	if _, err := RunSDKPreCompactHooks(ctx, runner, "manual", &instructions, false); err != context.Canceled {
		t.Fatal("hook cancellation not returned")
	}
}
