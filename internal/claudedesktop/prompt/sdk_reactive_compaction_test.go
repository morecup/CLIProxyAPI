package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestSDKReactiveCompactionAttemptsMatchNativeVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/sdk-reactive-attempts-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string
			Messages        []SDKHistoryMessage
			InitialTokenGap *int64 `json:"initial_token_gap"`
			Outcomes        []struct {
				Reason             string
				TokenGap           *int64
				ViaCreditsBoundary bool
			}
			Attempts []SDKReactiveAttempt
			Expected struct {
				ReadyToApply    bool `json:"ready_to_apply"`
				Reason          string
				Attempts        int
				TotalGroups     int      `json:"total_groups"`
				GroupsPreserved int      `json:"groups_preserved"`
				PreservedUUIDs  []string `json:"preserved_uuids"`
				CreditsRescue   bool     `json:"credits_rescue"`
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 18 {
		t.Fatal("incomplete native compaction vectors")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var attempts []SDKReactiveAttempt
			history := SDKHistorySnapshot{Messages: tc.Messages, OwnedMessagesKnown: true}
			result, err := RunSDKReactiveCompaction(t.Context(), history, tc.InitialTokenGap, func(ctx context.Context, plan SDKReactiveAttempt) (SDKReactiveQueryResult[int], error) {
				index := len(attempts)
				if len(plan.Summarize) != plan.MessagesToSummarize {
					t.Fatal("message count differs from planned input")
				}
				plan.Summarize, plan.Preserve = nil, nil
				attempts = append(attempts, plan)
				if index < len(tc.Outcomes) {
					out := tc.Outcomes[index]
					return SDKReactiveQueryResult[int]{Reason: out.Reason, TokenGap: out.TokenGap, ViaCreditsBoundary: out.ViaCreditsBoundary}, nil
				}
				return SDKReactiveQueryResult[int]{Success: true, Payload: 123}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// JSON ignores absent optional metadata and non-telemetry message lists.
			got, _ := json.Marshal(attempts)
			want, _ := json.Marshal(tc.Attempts)
			if len(attempts) != 0 && string(got) != string(want) || len(attempts) != len(tc.Attempts) {
				t.Fatalf("attempts=%s want=%s", got, want)
			}
			e := tc.Expected
			if result.ReadyToApply != e.ReadyToApply || result.Reason != e.Reason || result.Attempts != e.Attempts || result.TotalGroups != e.TotalGroups || result.GroupsPreserved != e.GroupsPreserved || result.CreditsRescue != e.CreditsRescue {
				t.Fatalf("result=%+v expected=%+v", result, e)
			}
			ids := make([]string, 0, len(result.Preserve))
			for _, m := range result.Preserve {
				ids = append(ids, m.UUID)
			}
			if !reflect.DeepEqual(ids, e.PreservedUUIDs) {
				t.Fatalf("preserved=%v expected=%v", ids, e.PreservedUUIDs)
			}
			if result.ReadyToApply && result.Payload != 123 {
				t.Fatal("summary payload lost")
			}
		})
	}
}

func sdkReactiveTestHistory() SDKHistorySnapshot {
	messages := []SDKHistoryMessage{{Type: "user", UUID: "user", TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}}}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		messages = append(messages, SDKHistoryMessage{Type: "assistant", UUID: id, MessageID: id, TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}})
	}
	return SDKHistorySnapshot{Messages: messages, OwnedMessagesKnown: true}
}

func TestSDKReactiveCompactionNoInventedWork(t *testing.T) {
	calls := 0
	query := func(context.Context, SDKReactiveAttempt) (SDKReactiveQueryResult[int], error) {
		calls++
		return SDKReactiveQueryResult[int]{Success: true}, nil
	}
	if _, err := RunSDKReactiveCompaction(t.Context(), SDKHistorySnapshot{}, nil, query); !errors.Is(err, ErrSDKReactiveHistoryUnknown) {
		t.Fatal(err)
	}
	h := sdkReactiveTestHistory()
	h.Messages[1].TokenEstimate.Known = false
	gap := int64(25)
	if _, err := RunSDKReactiveCompaction(t.Context(), h, &gap, query); !errors.Is(err, ErrSDKReactiveEstimateUnknown) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := RunSDKReactiveCompaction(ctx, sdkReactiveTestHistory(), nil, query); err != nil || result.Reason != "aborted" || result.Attempts != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if calls != 0 {
		t.Fatal("query ran with unknown history, unknown seed estimate or cancelled owner")
	}
}

func TestSDKReactiveCompactionCallbacksAreIsolated(t *testing.T) {
	h := sdkReactiveTestHistory()
	original := append([]SDKHistoryMessage(nil), h.Messages...)
	gap := int64(25)
	calls := 0
	result, err := RunSDKReactiveCompaction(t.Context(), h, &gap, func(ctx context.Context, plan SDKReactiveAttempt) (SDKReactiveQueryResult[string], error) {
		calls++
		for _, m := range plan.Summarize {
			if m.UUID == "mutated" {
				t.Fatal("previous query mutated future history")
			}
		}
		if calls == 1 {
			plan.Summarize[0].UUID = "mutated"
			plan.Preserve[0].UUID = "mutated"
			if plan.TokenGap != nil {
				*plan.TokenGap = -999
			}
			if plan.StepSize != nil {
				*plan.StepSize = -999
			}
			return SDKReactiveQueryResult[string]{Reason: "media_too_large"}, nil
		}
		if plan.Attempt != 1 || !plan.StrippedMedia || plan.TokenGap == nil || *plan.TokenGap != 25 || *plan.StepSize != 2 {
			t.Fatalf("repeated iteration=%+v", plan)
		}
		return SDKReactiveQueryResult[string]{Success: true, Payload: "private summary"}, nil
	})
	if err != nil || !result.ReadyToApply || result.Attempts != 1 || result.Payload != "private summary" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(h.Messages, original) || gap != 25 {
		t.Fatal("caller history mutated")
	}
	for _, m := range result.Preserve {
		if m.UUID == "mutated" {
			t.Fatal("query mutated preserved application history")
		}
	}
}

func TestSDKReactiveCompactionCancellationAndQueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	result, err := RunSDKReactiveCompaction(ctx, sdkReactiveTestHistory(), nil, func(context.Context, SDKReactiveAttempt) (SDKReactiveQueryResult[int], error) {
		cancel()
		return SDKReactiveQueryResult[int]{Success: true, Payload: 123}, nil
	})
	if err != nil || result.ReadyToApply || result.Reason != "aborted" || result.Attempts != 1 || result.Payload != 0 {
		t.Fatalf("cancelled result=%+v err=%v", result, err)
	}
	sentinel := errors.New("synthetic query failure")
	result, err = RunSDKReactiveCompaction(t.Context(), sdkReactiveTestHistory(), nil, func(context.Context, SDKReactiveAttempt) (SDKReactiveQueryResult[int], error) {
		return SDKReactiveQueryResult[int]{}, sentinel
	})
	if !errors.Is(err, sentinel) || result.ReadyToApply || result.Attempts != 1 {
		t.Fatalf("failed result=%+v err=%v", result, err)
	}
}
