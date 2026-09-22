package telemetry

import claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"

func observedScopeStatus(scope *claudeprofile.TelemetryObservedScope) *ObservedScopeStatus {
	if scope == nil {
		return nil
	}
	status := &ObservedScopeStatus{
		SchemaVersion: scope.SchemaVersion, Policy: scope.Policy, UnionSHA256: scope.UnionSHA256,
		BaselineEndpointEventCount:     scope.BaselineEndpointEventCount,
		BaselineEventNameCount:         scope.BaselineEventNameCount,
		SupplementalEndpointEventCount: scope.SupplementalEndpointEventCount,
		SupplementalEventNameCount:     scope.SupplementalEventNameCount,
		Sources:                        make([]ObservedSourceStatus, 0, len(scope.Sources)),
	}
	for _, source := range scope.Sources {
		status.Sources = append(status.Sources, ObservedSourceStatus{
			Kind: source.Kind, Artifact: source.Artifact, SHA256: source.SHA256,
			EndpointEventCount: len(source.EndpointEvents),
		})
	}
	return status
}

// executableEvents is the set of event contracts with production call sites.
// A profile entry alone is not an emitter. Keep this list paired with delivery
// tests; names are checked as well as facts so a renamed timer cannot silently
// count as a supported lifecycle event.
var executableEvents = map[string]map[string]string{
	"desktop-event-logging": {
		FactUpdateCheckStarted:           "desktop_update_check_started",
		FactUpdateNotAvailable:           "desktop_update_not_available",
		FactSessionInitialized:           "desktop_ccd_session_initialized",
		FactRequestStarted:               "desktop_ccd_message_cycle_start",
		FactRequestSucceeded:             "desktop_ccd_message_cycle_outcome",
		FactRequestFailed:                "desktop_ccd_message_cycle_outcome",
		FactSessionVisibility:            "desktop_ccd_session_visibility_changed",
		FactSessionIdleTimeout:           "desktop_ccd_session_idle_timeout_started",
		FactSessionStopped:               "desktop_ccd_session_stopped",
		FactSessionResumeBringHomeTiming: "desktop_ccd_resume_bring_home_timing",
		FactSessionIdlePauseDeclined:     "desktop_ccd_session_idle_pause_declined",
		FactSessionIdleTimeoutCancelled:  "desktop_ccd_session_idle_timeout_cancelled",
		FactSessionIdleWarmStart:         "desktop_ccd_session_idle_warm_start",
		FactSessionIdleWarmComplete:      "desktop_ccd_session_idle_warm_complete",
		FactSessionPauseBlockedByRC:      "desktop_ccd_session_pause_blocked_by_rc",
		FactTranscriptLeasePass:          "desktop_ccd_transcript_lease_pass",
	},
	"sdk-event-logging": {
		FactSDKInput:       "tengu_input_prompt",
		FactRuntimeStarted: "tengu_started", FactRuntimeInitialized: "tengu_init",
		FactSDKInitHandshake: "tengu_sdk_init_handshake", FactShutdownPending: "tengu_shutdown_pending_state",
		FactSDKCacheBreakpoints: "tengu_api_cache_breakpoints", FactSDKRetry: "tengu_api_retry",
		FactSDKSuccess: "tengu_api_success", FactSDKQuery: "tengu_api_query", FactSDKSystemBlock: "tengu_sysprompt_block",
		FactSDKTurnEnd:                  "tengu_turn_end",
		FactSDKTTFT:                     "tengu_sdk_ttft",
		FactSDKResult:                   "tengu_sdk_result",
		FactSDKAfterNormalize:           "tengu_api_after_normalize",
		FactSDKSystemBoundary:           "tengu_sysprompt_boundary_found",
		FactSDKSystemMissingBoundary:    "tengu_sysprompt_missing_boundary_marker",
		FactSDKFastModeOverageRejected:  "tengu_fast_mode_overage_rejected",
		FactSDKCacheDiagnosis:           "tengu_prompt_cache_diagnosis_received",
		FactSDKTitleGenerated:           "tengu_session_title_generated",
		FactSDKReactiveCompactTriggered: "tengu_reactive_compact_triggered",
		FactSDKReactiveCompactAttempt:   "tengu_reactive_compact_attempt",
		FactSDKBridgePlaceholderUsed:    "tengu_bridge_placeholder_used_session",
		FactSDKBridgeTeardown:           "tengu_bridge_repl_teardown",
		FactSDKChainTimestamp:           "tengu_chain_timestamp_fallback",
		FactSDKChainParallelResult:      "tengu_chain_parallel_tr_recovered",
		FactSDKChainParentCycle:         "tengu_chain_parent_cycle",
		FactSDKToolSearchModeDecision:   "tengu_tool_search_mode_decision",
		FactSDKDeferredToolsPoolChange:  "tengu_deferred_tools_pool_change",
		FactSDKToolSearchOutcome:        "tengu_tool_search_outcome",
	},
	segmentRole: {
		FactRuntimeStarted: "identify", FactRequestStarted: "claudeai.code.message.submitted", FactFirstByte: "claudeai.code.session.ttft",
	},
	datadogLogsRole: {
		FactRuntimeStarted: "tengu_started", FactRuntimeInitialized: "tengu_init",
		FactSDKInitHandshake: "tengu_sdk_init_handshake", FactShutdownPending: "tengu_shutdown_pending_state",
		FactSDKSuccess: "tengu_api_success",
		FactSDKTTFT:    "tengu_sdk_ttft", FactSDKResult: "tengu_sdk_result",
	},
	datadogLogsBrowserRole: {FactSDKRetry: "error"},
	datadogRUMRole:         {FactSessionInitialized: "view", FactSessionStopped: "view", FactRequestStarted: "action", FactFirstByte: "action", FactRequestFailed: "error"},
	sentryRole:             {FactRuntimeStarted: "session", FactRuntimeStopped: "session", FactRequestFailed: "event"},
}

func (m *Manager) executableEndpointEvents() map[string]claudeprofile.TelemetryEndpointEvent {
	result := make(map[string]claudeprofile.TelemetryEndpointEvent)
	appendEvents := func(role string, events map[string]claudeprofile.TelemetryEventProfile) {
		for fact, event := range events {
			if expected := executableEvents[role][fact]; expected != "" && event.EventName == expected {
				result[role+"\x00"+expected] = claudeprofile.TelemetryEndpointEvent{EndpointRole: role, EventName: expected}
			}
		}
	}
	appendEvents(m.profile.EndpointRole, m.profile.Events)
	appendEvents(m.sdkProfile.EndpointRole, m.sdkProfile.Events)
	for _, profile := range m.auxiliaryProfiles {
		appendEvents(profile.EndpointRole, profile.Events)
	}
	return result
}
