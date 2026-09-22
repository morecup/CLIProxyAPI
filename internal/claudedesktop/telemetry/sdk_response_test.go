package telemetry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

func TestSDKFastModeRejectionRequiresObservedResponseFacts(t *testing.T) {
	for _, tc := range []struct {
		name, speed, reason string
		role                claudeprofile.RequestRole
		status, want        int
	}{
		{"main-fast-rejection", "fast", "org_level_disabled", claudeprofile.RoleMain, 429, 1},
		{"success-header-is-not-rejection", "fast", "org_level_disabled", claudeprofile.RoleMain, 200, 0},
		{"server-failure-is-not-overage", "fast", "org_level_disabled", claudeprofile.RoleMain, 502, 0},
		{"normal-speed-429", "standard", "org_level_disabled", claudeprofile.RoleMain, 429, 0},
		{"beta-only-does-not-prove-fast", "", "org_level_disabled", claudeprofile.RoleMain, 429, 0},
		{"unknown-header-not-exported", "fast", "PRIVATE_ERROR", claudeprofile.RoleMain, 429, 0},
		{"missing-reason-not-inferred", "fast", "", claudeprofile.RoleMain, 429, 0},
		{"title-no-inheritance", "fast", "org_level_disabled", claudeprofile.RoleTitle, 429, 0},
		{"classifier-no-inheritance", "fast", "org_level_disabled", claudeprofile.RoleSecurityMonitor, 429, 0},
		{"count-tokens-no-inheritance", "fast", "org_level_disabled", claudeprofile.RoleCountTokens, 429, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)}
			doer := &testDoer{}
			manager := newSDKPreparationTestManager(t, clock, doer)
			facts := testRequestFacts("99999999-9999-4999-8999-999999999999")
			facts.Role = tc.role
			span := manager.BeginRequest(context.Background(), newTelemetryTestAuth(t, testAccountA, testOrgA, testDeviceA), facts)
			headers := http.Header{"aNtHrOpIc-Ratelimit-Unified-Overage-Disabled-Reason": {tc.reason}, "Set-Cookie": {"PRIVATE_COOKIE"}}
			// An observation made before the upstream request must not latch the
			// response or suppress the later real response.
			span.ObserveHTTPResponse(tc.status, headers)
			span.ObserveRequest([]byte(`{"model":"claude-opus-5","messages":[],"speed":"`+tc.speed+`"}`), http.Header{"Anthropic-Beta": {"fast-mode-2026-02-01"}})
			var group sync.WaitGroup
			for range 8 {
				group.Add(1)
				go func() { defer group.Done(); span.ObserveHTTPResponse(tc.status, headers) }()
			}
			group.Wait()
			span.FinishFailure(context.Background(), "rate_limit", errors.New("PRIVATE_ERROR"))
			span.ObserveHTTPResponse(tc.status, headers)
			if errFlush := manager.Flush(context.Background()); errFlush != nil {
				t.Fatal(errFlush)
			}
			got := 0
			for _, request := range doer.Requests() {
				if !strings.HasPrefix(request.URL, "https://api.anthropic.com/") {
					continue
				}
				names := capturedSDKEventNames(t, []recordedRequest{request})
				got += names["tengu_fast_mode_overage_rejected"]
				if names["tengu_api_retry"] != 0 {
					t.Fatal("overage rejection manufactured a scheduled retry")
				}
				if names["tengu_fast_mode_overage_rejected"] == 0 {
					continue
				}
				metadata := sdkMetadataForEvent(t, request.Body, "tengu_fast_mode_overage_rejected")
				if len(metadata) != 3 || metadata["cc_prompt_id"] != facts.PromptID || metadata["overage_disabled_reason"] != "org_level_disabled" {
					t.Fatalf("unexpected rejection metadata: %v", metadata)
				}
			}
			if got != tc.want {
				t.Fatalf("rejection count = %d, want %d", got, tc.want)
			}
		})
	}
}
