package telemetry

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	"github.com/tidwall/gjson"
)

func TestSDKCompactionOriginsHaveDistinctNativeLifecycle(t *testing.T) {
	for _, kind := range []string{"manual", "auto", "reactive"} {
		for _, terminal := range []string{"success", "failure", "close"} {
			t.Run(kind+"/"+terminal, func(t *testing.T) {
				f, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 4)
				origin := claudeprompt.SDKCompactionOrigin{Kind: kind}
				if kind == "reactive" {
					if !view.ClaimReactiveFailure() {
						t.Fatal("missing recovery claim")
					}
				} else {
					span.responseObserved, span.responseStatus = false, 0
					if kind == "auto" {
						origin.ThresholdSource = "model-default"
					}
				}
				operation := span.BeginSDKCompaction(view, origin)
				if operation == nil {
					t.Fatal("native origin was not admitted")
				}
				defer operation.Close()
				if got := span.BeginSDKCompaction(view, origin); got != operation {
					t.Fatal("duplicate lifecycle")
				}
				if got := span.BeginSDKCompaction(view, claudeprompt.SDKCompactionOrigin{Kind: "invalid"}); got != nil {
					t.Fatal("changed origin reused an operation")
				}
				result, err := claudeprompt.RunSDKReactiveCompaction(t.Context(), view.History(), nil,
					func(_ context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[bool], error) {
						operation.ObserveAttempt(attempt)
						return claudeprompt.SDKReactiveQueryResult[bool]{Success: true}, nil
					})
				if err != nil {
					t.Fatal(err)
				}
				pre, post := int64(993721), int64(54128)
				zero, no := 0, false
				breakdown, err := claudeprompt.ObserveSDKCompactionBreakdown([]json.RawMessage{json.RawMessage(`{"role":"user","content":"synthetic"}`)}, []string{"environment"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				f.clock.Advance(14351 * time.Millisecond)
				success := SDKReactiveCompactionSuccess{Attempts: result.Attempts, GroupsPreserved: result.GroupsPreserved, TotalGroups: result.TotalGroups,
					SplitKind: result.SplitKind, HeadTruncations: result.HeadTruncations, PreservedUUIDCount: len(result.Preserve), PreservedMessageCount: len(result.Preserve),
					ForkAssistantMessageCount: 1, RestoredItemCount: 8, PreCompactTokens: &pre, PostCompactTokens: &post,
					UsageKnown: true, Usage: claudeprompt.SDKTokenUsage{InputTokens: 90390, OutputTokens: 1557, CacheReadInputTokens: 41851, CacheCreationInputTokens: 813822},
					KeptThinkingBlockCount: &zero, KeptThinkingStripped: &no, CacheCold: &no, Breakdown: breakdown}
				switch terminal {
				case "success":
					operation.RecordSuccess(success)
				case "failure":
					status := 503
					operation.RecordFailure(SDKReactiveCompactionFailure{Reason: "error", Attempts: 1, TotalGroups: result.TotalGroups, CacheCold: &no, Status: &status})
				case "close":
					operation.Close()
					operation.RecordSuccess(success)
				}
				events := f.events(t)
				var want []string
				if kind == "auto" {
					want = append(want, "tengu_auto_compact_routed_reactive")
				}
				if kind != "manual" {
					want = append(want, "tengu_reactive_compact_triggered")
				}
				want = append(want, "tengu_reactive_compact_attempt")
				if terminal == "success" {
					want = append(want, "tengu_reactive_compact_succeeded")
					if kind == "auto" {
						want = append(want, "tengu_auto_compact_succeeded")
					}
				}
				if terminal == "failure" {
					data := events["tengu_reactive_compact_failed"][0]
					if data["status"] != float64(503) {
						t.Fatal("lost observed summary failure status", data)
					}
					if _, present := data["precomputedKind"]; present != (kind == "manual") {
						t.Fatal("manual direct-path precompute decision differs", data)
					}
				}
				if terminal == "failure" {
					want = append(want, "tengu_reactive_compact_failed")
				}
				var names []string
				for _, request := range f.doer.Requests() {
					for _, event := range gjson.GetBytes(request.Body, "events").Array() {
						name := event.Get("event_data.event_name").String()
						if name == "tengu_auto_compact_routed_reactive" || name == "tengu_auto_compact_succeeded" || name == "tengu_reactive_compact_triggered" || name == "tengu_reactive_compact_attempt" || name == "tengu_reactive_compact_succeeded" || name == "tengu_reactive_compact_failed" {
							names = append(names, name)
						}
					}
				}
				if !reflect.DeepEqual(names, want) {
					t.Fatalf("sequence %v want %v", names, want)
				}
				for _, name := range []string{"tengu_reactive_compact_succeeded", "tengu_reactive_compact_failed"} {
					for _, data := range events[name] {
						if data["trigger"] != origin.HookTrigger() {
							t.Fatal("incorrect native trigger", data)
						}
						_, threshold := data["thresholdSource"]
						_, source := data["querySource"]
						_, reuse := data["manualPrecomputeReuse"]
						if threshold != (kind == "auto") || source != (kind != "manual") || reuse != (kind == "manual") {
							t.Fatal("origin fields crossed paths", data)
						}
					}
				}
				if terminal == "success" {
					data := events["tengu_reactive_compact_succeeded"][0]
					if data["keptThinkingBlockCount"] != float64(0) || data["keptThinkingStripped"] != false || data["cacheCold"] != false || data["total_tokens"] != float64(2) || data["compactionTotalTokens"] != float64(947620) {
						t.Fatal("lost native diagnostics", data)
					}
					if _, exists := data["keptThinkingStripDecidedBy"]; exists {
						t.Fatal("zero thinking fabricated a policy decision")
					}
					if kind == "auto" {
						auto := events["tengu_auto_compact_succeeded"][0]
						if auto["originalMessageCount"] != float64(len(view.History().Messages)) || auto["compactedMessageCount"] != float64(9) || auto["routedThroughReactive"] != true {
							t.Fatal("incorrect outer counts", auto)
						}
						for _, absent := range []string{"postCompactTokenCount", "truePostCompactTokenCount", "querySource", "trigger", "cacheHitRate"} {
							if _, exists := auto[absent]; exists {
								t.Fatal("unexpected outer field", absent)
							}
						}
					}
				}
			})
		}
	}
}

func TestSDKCompactionPreflightRequiresNativeOwnership(t *testing.T) {
	for _, mode := range []string{"response", "no-view", "foreign", "bad-threshold", "manual-threshold", "finished"} {
		t.Run(mode, func(t *testing.T) {
			_, span, view := reactiveTelemetryFixture(t, "claude-opus-5", 3)
			span.responseObserved, span.responseStatus = false, 0
			origin := claudeprompt.SDKCompactionOrigin{Kind: "auto", ThresholdSource: "model-default"}
			switch mode {
			case "response":
				span.responseObserved, span.responseStatus = true, 400
			case "no-view":
				view = nil
			case "foreign":
				_, _, view = reactiveTelemetryFixture(t, "claude-opus-5", 3)
			case "bad-threshold":
				origin.ThresholdSource = "PRIVATE_VALUE"
			case "manual-threshold":
				origin.Kind = "manual"
			case "finished":
				span.finished = true
			}
			if operation := span.BeginSDKCompaction(view, origin); operation != nil {
				t.Fatal("unowned preflight emitted telemetry")
			}
		})
	}
}
