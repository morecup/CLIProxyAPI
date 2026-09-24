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
			for index := range tc.Attempts {
				// The 2.1.280 automatic path adds these dimensions to the
				// otherwise unchanged round-selection vectors captured at 2.1.247.
				tc.Attempts[index].SplitKind = "round"
				tc.Attempts[index].HeadTruncations = 0
			}
			var attempts []SDKReactiveAttempt
			history := SDKHistorySnapshot{Messages: tc.Messages, OwnedMessagesKnown: true}
			// These captured 2.1.247 vectors isolate the round ladder. The public
			// 2.1.280 path enables summarize_all and is covered separately below.
			result, err := runSDKReactiveCompaction(t.Context(), history, tc.InitialTokenGap, false, func(ctx context.Context, plan SDKReactiveAttempt) (SDKReactiveQueryResult[int], error) {
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
			if result.ReadyToApply && (result.SplitKind != "round" || result.HeadTruncations != 0) {
				t.Fatalf("automatic split metadata=%q/%d", result.SplitKind, result.HeadTruncations)
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

func TestSDKReactiveCompactionSummarizeAllKeepsExactTrailingUser(t *testing.T) {
	history := SDKHistorySnapshot{OwnedMessagesKnown: true, Messages: []SDKHistoryMessage{
		{Type: "user", UUID: "opening-user", TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}},
		{Type: "assistant", UUID: "assistant", MessageID: "assistant", TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}},
		{Type: "user", UUID: "trailing-user", TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}},
	}}
	calls := 0
	result, err := RunSDKReactiveCompaction(t.Context(), history, nil, func(_ context.Context, attempt SDKReactiveAttempt) (SDKReactiveQueryResult[string], error) {
		calls++
		if !ValidateSDKReactiveAttempt(history, attempt) || attempt.SplitKind != "summarize_all" || attempt.HeadTruncations != 0 ||
			attempt.GroupsToSummarize != 2 || attempt.GroupsToPreserve != 0 || len(attempt.Preserve) != 1 || attempt.Preserve[0].UUID != "trailing-user" {
			t.Fatalf("summarize_all attempt=%+v", attempt)
		}
		if calls == 1 {
			attempt.Summarize[0].UUID = "mutated"
			attempt.Preserve[0].UUID = "mutated"
			return SDKReactiveQueryResult[string]{Reason: "media_too_large"}, nil
		}
		if !attempt.StrippedMedia || attempt.Attempt != 1 || attempt.Summarize[0].UUID != "opening-user" || attempt.Preserve[0].UUID != "trailing-user" {
			t.Fatal("callback mutation changed the repeated fallback attempt")
		}
		return SDKReactiveQueryResult[string]{Success: true, Payload: "summary"}, nil
	})
	if err != nil || !result.ReadyToApply || result.Payload != "summary" || result.Attempts != 1 || calls != 2 ||
		result.SplitKind != "summarize_all" || result.HeadTruncations != 0 || result.GroupsPreserved != 0 ||
		len(result.Preserve) != 1 || result.Preserve[0].UUID != "trailing-user" {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
}

func sdkReactiveLongHistory(assistantCount int) SDKHistorySnapshot {
	messages := []SDKHistoryMessage{{Type: "user", UUID: "opening", TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}}}
	for index := range assistantCount {
		id := string(rune('a' + index))
		messages = append(messages, SDKHistoryMessage{Type: "assistant", UUID: id, MessageID: id, TokenEstimate: SDKTokenEstimate{Tokens: 10, Known: true}})
	}
	return SDKHistorySnapshot{Messages: messages, OwnedMessagesKnown: true}
}

func TestSDKReactiveHeadTruncationUsesGroupsAndTokenGap(t *testing.T) {
	history := sdkReactiveLongHistory(10)
	active := sdkReactiveFlatten(GroupSDKHistory(history.Messages))
	tests := []struct {
		name string
		gap  *int64
		want string
	}{
		{name: "twenty-percent", want: "b"},
		{name: "gap-guided", gap: func() *int64 { value := int64(35); return &value }(), want: "d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			truncated, err := sdkReactiveTruncateHead(active, tc.gap)
			if err != nil || len(truncated) < 2 || !sdkReactiveIsHeadMarker(truncated[0]) || truncated[1].UUID != tc.want {
				t.Fatalf("truncated=%+v err=%v", truncated, err)
			}
			again, err := sdkReactiveTruncateHead(truncated, nil)
			if err != nil || len(again) >= len(truncated) || !sdkReactiveIsHeadMarker(again[0]) {
				t.Fatalf("repeated truncation=%+v err=%v", again, err)
			}
		})
	}
}

func TestSDKReactiveCompactionStopsAfterThreeHeadTruncations(t *testing.T) {
	history := sdkReactiveLongHistory(10)
	var fallbackHeads []int
	var fallbackLengths []int
	result, err := RunSDKReactiveCompaction(t.Context(), history, nil, func(_ context.Context, attempt SDKReactiveAttempt) (SDKReactiveQueryResult[bool], error) {
		if !ValidateSDKReactiveAttempt(history, attempt) {
			t.Fatalf("invalid attempt=%+v", attempt)
		}
		if attempt.SplitKind == "summarize_all" {
			fallbackHeads = append(fallbackHeads, attempt.HeadTruncations)
			fallbackLengths = append(fallbackLengths, len(attempt.Summarize))
			if !sdkReactiveIsHeadMarker(attempt.Summarize[0]) {
				t.Fatal("assistant-leading truncation omitted its temporary marker")
			}
		}
		return SDKReactiveQueryResult[bool]{Reason: "prompt_too_long"}, nil
	})
	if err != nil || result.ReadyToApply || result.Reason != "exhausted" || result.SplitKind != "summarize_all" || result.HeadTruncations != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(fallbackHeads, []int{1, 2, 3}) || len(fallbackLengths) != 3 ||
		!(fallbackLengths[0] > fallbackLengths[1] && fallbackLengths[1] > fallbackLengths[2]) {
		t.Fatalf("heads=%v lengths=%v", fallbackHeads, fallbackLengths)
	}
}
