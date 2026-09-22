package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSDKTranscriptChainEventsUseOwnedRestorationDefaults(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001"} {
		t.Run(model, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 7, 14, 30, 0, 0, time.UTC)}
			doer := &testDoer{}
			m := bridgeTestManager(t, clock, doer)
			beforeCoverage := m.Status().LiveEmitterCoverage
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			auth.Metadata["subscription_type"] = "pro"
			session := uuid.NewString()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observe, err := m.SDKTranscriptChainObserver(ctx, auth, session, uuid.NewString(), model, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range []claudeprompt.SDKTranscriptChainEvent{
				{Kind: claudeprompt.SDKChainTimestamp, At: clock.Now()},
				{Kind: claudeprompt.SDKChainParallelResult, RecoveredCount: 3, At: clock.Now().Add(time.Millisecond)},
				{Kind: claudeprompt.SDKChainParentCycle, At: clock.Now().Add(2 * time.Millisecond)},
			} {
				observe(event)
			}
			cancel()
			observe(claudeprompt.SDKTranscriptChainEvent{Kind: claudeprompt.SDKChainTimestamp, At: clock.Now()})
			if err := m.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, request := range doer.Requests() {
				var batch struct {
					Events []sdkEventWrapper `json:"events"`
				}
				_ = json.Unmarshal(request.Body, &batch)
				for _, wrapper := range batch.Events {
					data := wrapper.EventData
					if !strings.HasPrefix(data.EventName, "tengu_chain_") {
						continue
					}
					names = append(names, data.EventName)
					betas, _ := m.sdkProfile.InputBetaHeader(model)
					if data.SessionID != session || data.Model != model || data.Betas != betas || data.Auth.AccountUUID != testAccountA {
						t.Fatal("borrowed restoration dimensions")
					}
					raw, err := base64.StdEncoding.DecodeString(data.AdditionalMetadata)
					if err != nil {
						t.Fatal(err)
					}
					var metadata map[string]any
					if json.Unmarshal(raw, &metadata) != nil || metadata["subscription_type"] != "pro" || metadata["cc_prompt_id"] != nil {
						t.Fatal("invented prompt or missing subscription", metadata)
					}
					want := 1
					if data.EventName == claudeprompt.SDKChainParallelResult {
						want = 2
						if metadata["recovered_count"] != float64(3) {
							t.Fatal(metadata)
						}
					}
					if len(metadata) != want {
						t.Fatal("unprofiled metadata", metadata)
					}
				}
			}
			if !slices.Equal(names, []string{claudeprompt.SDKChainTimestamp, claudeprompt.SDKChainParallelResult, claudeprompt.SDKChainParentCycle}) {
				t.Fatal("lost, duplicate or reordered observations", names)
			}
			coverage := m.Status().LiveEmitterCoverage
			if coverage.ObservableEndpointEventCount != 303 || coverage.LiveEndpointEventCount != beforeCoverage.LiveEndpointEventCount || coverage.LiveEventNameCount != beforeCoverage.LiveEventNameCount {
				t.Fatal("uncaptured source events inflated corpus acceptance", coverage.LiveEndpointEventCount, coverage.LiveEventNameCount)
			}
			for _, name := range []string{claudeprompt.SDKChainTimestamp, claudeprompt.SDKChainParallelResult, claudeprompt.SDKChainParentCycle} {
				found := false
				for _, pair := range coverage.UncapturedExecutableEndpointEvents {
					found = found || pair.EndpointRole == "sdk-event-logging" && pair.EventName == name && pair.Executable
				}
				if !found {
					t.Fatal("source-only mapping disappeared", name)
				}
			}
			t.Logf("uncaptured executable mappings: %+v", coverage.UncapturedExecutableEndpointEvents)
		})
	}
}

func TestSDKTranscriptChainSamplingKillAndCancellation(t *testing.T) {
	for _, mode := range []string{"zero", "one", "disabled", "string-disabled", "config-error", "retired-during-config", "queue-full", "unknown-model", "bad-event"} {
		t.Run(mode, func(t *testing.T) {
			clock := &testClock{now: time.Now()}
			doer := &testDoer{}
			m := bridgeTestManager(t, clock, doer)
			auth := newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var reads []string
			config := func(name string) (json.RawMessage, error) {
				reads = append(reads, name)
				if mode == "config-error" {
					return nil, errors.New("synthetic config read")
				}
				if mode == "retired-during-config" {
					cancel()
				}
				if name == "tengu_event_sampling_config" {
					rate := "1"
					if mode == "zero" {
						rate = "0"
					}
					return json.RawMessage(fmt.Sprintf(`{"tengu_chain_timestamp_fallback":{"sample_rate":%s}}`, rate)), nil
				}
				if mode == "disabled" {
					return json.RawMessage(`{"firstParty":true}`), nil
				}
				if mode == "string-disabled" {
					return json.RawMessage(`{"firstParty":"true"}`), nil
				}
				return json.RawMessage(`{}`), nil
			}
			model := "claude-opus-5"
			if mode == "unknown-model" {
				model = "unknown"
			}
			observe, err := m.SDKTranscriptChainObserver(ctx, auth, uuid.NewString(), uuid.NewString(), model, config)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := m.workerForDelivery(auth, m.sdkDelivery)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "queue-full" {
				worker.profile.batch.MaxPendingEvents = 0
			}
			event := claudeprompt.SDKTranscriptChainEvent{Kind: claudeprompt.SDKChainTimestamp, At: clock.Now()}
			if mode == "bad-event" {
				event.RecoveredCount = 9
			}
			observe(event)
			wantIssue := slices.Contains([]string{"config-error", "queue-full", "unknown-model", "bad-event"}, mode)
			if (worker.factIssueSnapshot() != nil) != wantIssue {
				t.Fatal("lost failure or intentional drop mislabeled")
			}
			if mode == "zero" && !slices.Equal(reads, []string{"tengu_event_sampling_config"}) {
				t.Fatal("sampled-out event read later gates", reads)
			}
			if err := m.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, request := range doer.Requests() {
				count += strings.Count(string(request.Body), claudeprompt.SDKChainTimestamp)
			}
			want := 0
			if mode == "one" || mode == "string-disabled" {
				want = 1
			}
			if count != want {
				t.Fatal("incorrect gate/delivery count", count, want)
			}
		})
	}
}

func TestSDKTranscriptChainAdmissionPrecedesWorkerAcquisition(t *testing.T) {
	for _, available := range []bool{true, false} {
		for _, mode := range []string{"sampled-out", "disabled", "retired-during-config", "accepted", "config-error"} {
			t.Run(fmt.Sprintf("worker-%t/%s", available, mode), func(t *testing.T) {
				clock := &testClock{now: time.Now()}
				doer := &testDoer{}
				m := bridgeTestManager(t, clock, doer)
				var auth *cliproxyauth.Auth
				if available {
					auth = newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var reads []string
				observe, err := m.SDKTranscriptChainObserver(ctx, auth, uuid.NewString(), uuid.NewString(), "claude-opus-5", func(name string) (json.RawMessage, error) {
					reads = append(reads, name)
					if mode == "config-error" {
						return nil, errors.New("synthetic admission config failure")
					}
					if mode == "retired-during-config" {
						cancel()
					}
					if mode == "sampled-out" && name == "tengu_event_sampling_config" {
						return json.RawMessage(`{"tengu_chain_timestamp_fallback":{"sample_rate":0}}`), nil
					}
					if mode == "disabled" && name == "tengu_frond_boric" {
						return json.RawMessage(`{"firstParty":true}`), nil
					}
					return json.RawMessage(`{}`), nil
				})
				if err != nil {
					t.Fatal(err)
				}
				observe(claudeprompt.SDKTranscriptChainEvent{Kind: claudeprompt.SDKChainTimestamp, At: clock.Now()})
				needsWorker := mode == "accepted" || mode == "config-error"
				m.mu.RLock()
				workerCount := len(m.workers)
				m.mu.RUnlock()
				wantWorkers := 0
				if available && needsWorker {
					wantWorkers = 1
				}
				if workerCount != wantWorkers {
					t.Errorf("worker acquisition preceded admission: got %d, want %d", workerCount, wantWorkers)
				}
				m.endpointMu.Lock()
				state := m.endpointStates[m.sdkDelivery.endpointRole]
				m.endpointMu.Unlock()
				if (state.Status != "ready") != (!available && needsWorker) {
					t.Errorf("intentional drop or unavailable admitted event misclassified: %+v", state)
				}
				if len(reads) == 0 || reads[0] != "tengu_event_sampling_config" {
					t.Errorf("worker failure bypassed native sampling: %v", reads)
				}
				if mode == "sampled-out" && len(reads) != 1 {
					t.Errorf("sampled-out event read a later gate: %v", reads)
				}
				if available && needsWorker {
					worker, err := m.workerForDelivery(auth, m.sdkDelivery)
					if err != nil {
						t.Fatal(err)
					}
					if (worker.factIssueSnapshot() != nil) != (mode == "config-error") {
						t.Fatal("admission failure was not retained")
					}
				}
				if err := m.Flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				delivered := 0
				for _, request := range doer.Requests() {
					delivered += strings.Count(string(request.Body), claudeprompt.SDKChainTimestamp)
				}
				wantDelivered := 0
				if available && mode == "accepted" {
					wantDelivered = 1
				}
				if delivered != wantDelivered {
					t.Errorf("incorrect admitted delivery count: got %d, want %d", delivered, wantDelivered)
				}
			})
		}
	}
}

func TestSDKTranscriptSampleRateMatchesNativeNumericPolicy(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		draw  float64
		want  string
		draws int
	}{
		{`{}`, 0, "null", 0}, {`{"e":null}`, 0, "null", 0}, {`{"e":{"sample_rate":null}}`, 0, "null", 0},
		{`{"e":{"sample_rate":"0.5"}}`, 0, "null", 0}, {`{"e":{"sample_rate":-1}}`, 0, "null", 0},
		{`{"e":{"sample_rate":2}}`, 0, "null", 0}, {`{"e":{"sample_rate":1}}`, 0, "null", 0},
		{`{"e":{"sample_rate":0}}`, 0, "0", 0}, {`{"e":{"sample_rate":0.5}}`, .499, "0.5", 1},
		{`{"e":{"sample_rate":0.5}}`, .5, "0", 1},
	} {
		t.Run(tc.raw+fmt.Sprint(tc.draw), func(t *testing.T) {
			draws := 0
			rate := sdkTranscriptSampleRate(json.RawMessage(tc.raw), "e", func() float64 { draws++; return tc.draw })
			encoded, _ := json.Marshal(rate)
			if string(encoded) != tc.want || draws != tc.draws {
				t.Fatal(string(encoded), draws)
			}
		})
	}
}
