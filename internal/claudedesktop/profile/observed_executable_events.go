package profile

import "strings"

const (
	ObservedEventSourceRenderer    = "renderer"
	ObservedEventSourceMainProcess = "main_process"
	ObservedEventSourceSDK         = "sdk"
	ObservedEventSourcePerformance = "performance"
	ObservedEventSourceCrash       = "crash"

	ObservedRendererCallTrack = "track"
	ObservedRendererCallPage  = "page"
)

// ObservedExecutableEvent is the version-bound, closed catalog accepted from
// the Desktop companion. Kind is the public discriminator; callers never
// choose an endpoint role or an event name.
type ObservedExecutableEvent struct {
	Kind          string
	Family        string
	EventName     string
	EndpointRoles []string
	Source        string
	RendererCall  string
}

func (e ObservedExecutableEvent) Fact() string {
	return "observed_" + e.Kind
}

var v140609ObservedExecutableEvents = []ObservedExecutableEvent{
	{Kind: "login", Family: "ui", EventName: "/login", EndpointRoles: []string{"segment"}},
	{Kind: "upgrade", Family: "ui", EventName: "/upgrade", EndpointRoles: []string{"segment"}},
	{Kind: "upgrade_max_from_existing", Family: "ui", EventName: "/upgrade/max/from-existing", EndpointRoles: []string{"segment"}},
	{Kind: "attachment", Family: "observability", EventName: "attachment", EndpointRoles: []string{"sentry"}},
	{Kind: "chorus_ideas_suggestions_shown", Family: "ui", EventName: "chorus.ideas.suggestions_shown", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "chrome_bridge_transport_selected", Family: "mcp", EventName: "chrome_bridge_transport_selected", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "claudeai_code_auto_mode_notice_printed", Family: "ui", EventName: "claudeai.code.auto_mode_notice.printed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_code_try_auto_coach_dismissed", Family: "ui", EventName: "claudeai.code.try_auto_coach.dismissed", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "claudeai_code_try_auto_coach_shown", Family: "ui", EventName: "claudeai.code.try_auto_coach.shown", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "claudeai_cowork_model_update_banner_displayed", Family: "ui", EventName: "claudeai.cowork_model_update_banner.displayed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_cowork_model_update_composer_warning_displayed", Family: "ui", EventName: "claudeai.cowork_model_update_composer_warning.displayed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_cumulative_error_count", Family: "observability", EventName: "claudeai.cumulative_error_count", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "claudeai_desktop_code_landing_image_added", Family: "ui", EventName: "claudeai.desktop.code.landing.image_added", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "claudeai_desktop_code_landing_model_selected", Family: "ui", EventName: "claudeai.desktop.code.landing.model_selected", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_desktop_code_prompt_suggestion_shown", Family: "ui", EventName: "claudeai.desktop.code.prompt_suggestion.shown", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_desktop_sidebar_design_entry_shown", Family: "ui", EventName: "claudeai.desktop.sidebar.design_entry_shown", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_desktop_sidebar_mode_pill_selected", Family: "ui", EventName: "claudeai.desktop.sidebar.mode_pill_selected", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_desktop_sidebar_more_flyout_opened", Family: "ui", EventName: "claudeai.desktop.sidebar.more_flyout_opened", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_desktop_sidebar_nav_item_clicked", Family: "ui", EventName: "claudeai.desktop.sidebar.nav_item_clicked", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_desktop_sidebar_profile_menu_opened", Family: "ui", EventName: "claudeai.desktop.sidebar.profile_menu_opened", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_epitaxy_clipboard", Family: "ui", EventName: "claudeai.epitaxy.clipboard", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_epitaxy_page_viewed", Family: "ui", EventName: "claudeai.epitaxy.page.viewed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_epitaxy_sidebar_group_new_session_clicked", Family: "ui", EventName: "claudeai.epitaxy.sidebar.group_new_session_clicked", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_feature_ok", Family: "ui", EventName: "claudeai.feature_ok", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_mcp_bootstrap_stream_completed", Family: "mcp", EventName: "claudeai.mcp.bootstrap_stream.completed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_model_selector_model_selected", Family: "ui", EventName: "claudeai.model_selector.model_selected", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_model_selector_opened", Family: "ui", EventName: "claudeai.model_selector.opened", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_perf_interaction_histogram", Family: "observability", EventName: "claudeai.perf.interaction_histogram", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_pricing_max_plan_toggled", Family: "ui", EventName: "claudeai.pricing.max_plan_toggled", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_sidebar_pin_census", Family: "ui", EventName: "claudeai.sidebar.pin_census", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_sidebar_state_set", Family: "ui", EventName: "claudeai.sidebar.state_set", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_upgrade_plans_page_viewed", Family: "ui", EventName: "claudeai.upgrade.plans_page_viewed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_user_facing_error_shown", Family: "ui", EventName: "claudeai.user_facing_error.shown", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_user_menu_item_clicked", Family: "ui", EventName: "claudeai.user_menu.item_clicked", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_yukon_gold_activation_checklist_v2_in_viewport", Family: "ui", EventName: "claudeai.yukon_gold.activation_checklist_v2_in_viewport", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_yukon_gold_activation_checklist_v2_shown", Family: "ui", EventName: "claudeai.yukon_gold.activation_checklist_v2_shown", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "claudeai_yukon_gold_enabled", Family: "ui", EventName: "claudeai.yukon_gold.enabled", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "cowork_remote_attestation", Family: "platform", EventName: "cowork_remote_attestation", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "cowork_scheduled_tasks_inventory", Family: "platform", EventName: "cowork_scheduled_tasks_inventory", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_app_input_ready", Family: "platform", EventName: "desktop_app_input_ready", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_app_sidebar_painted", Family: "platform", EventName: "desktop_app_sidebar_painted", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_app_startup_perf", Family: "observability", EventName: "desktop_app_startup_perf", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_composer_inp", Family: "observability", EventName: "desktop_ccd_composer_inp", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_config_reparse", Family: "platform", EventName: "desktop_ccd_config_reparse", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_governor_soft_cap_exceeded", Family: "platform", EventName: "desktop_ccd_governor_soft_cap_exceeded", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_governor_would_evict", Family: "platform", EventName: "desktop_ccd_governor_would_evict", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_governor_yielded", Family: "platform", EventName: "desktop_ccd_governor_yielded", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_remote_control_auto_enable", Family: "platform", EventName: "desktop_ccd_remote_control_auto_enable", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_remote_managed_settings_fetch", Family: "platform", EventName: "desktop_ccd_remote_managed_settings_fetch", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_stream_render", Family: "observability", EventName: "desktop_ccd_stream_render", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_terminal_spawned", Family: "platform", EventName: "desktop_ccd_terminal_spawned", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_ccd_trust_check_miss", Family: "platform", EventName: "desktop_ccd_trust_check_miss", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_feature_exposure", Family: "platform", EventName: "desktop_feature_exposure", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_hardware_buddy_status", Family: "platform", EventName: "desktop_hardware_buddy_status", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_main_event_loop_stall", Family: "observability", EventName: "desktop_main_event_loop_stall", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_main_view_paint_probe", Family: "observability", EventName: "desktop_main_view_paint_probe", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_notification_displayed", Family: "ui", EventName: "desktop_notification_displayed", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_notification_reachability", Family: "ui", EventName: "desktop_notification_reachability", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_notification_shown", Family: "ui", EventName: "desktop_notification_shown", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_notification_suppressed", Family: "ui", EventName: "desktop_notification_suppressed", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "desktop_process_memory_sample", Family: "observability", EventName: "desktop_process_memory_sample", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "device_registry_no_tpm_probe_code", Family: "platform", EventName: "device_registry_no_tpm_probe_code", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_bridge_device_reload", Family: "mcp", EventName: "lam_bridge_device_reload", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_cowork_root_info", Family: "platform", EventName: "lam_cowork_root_info", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_feature_support_evaluated", Family: "platform", EventName: "lam_feature_support_evaluated", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_hipaa_pref_synced", Family: "platform", EventName: "lam_hipaa_pref_synced", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_internal_mcp_server_created", Family: "mcp", EventName: "lam_internal_mcp_server_created", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_mcp_servers_setup_summary", Family: "mcp", EventName: "lam_mcp_servers_setup_summary", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_remote_get_device_info", Family: "platform", EventName: "lam_remote_get_device_info", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "lam_remote_tools_device_state", Family: "platform", EventName: "lam_remote_tools_device_state", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "long_task", Family: "observability", EventName: "long_task", EndpointRoles: []string{"datadog-rum"}},
	{Kind: "marketplace_plugin_account_sync_result", Family: "mcp", EventName: "marketplace_plugin_account_sync_result", EndpointRoles: []string{"desktop-event-logging"}},
	{Kind: "mcp_prompts_listed", Family: "mcp", EventName: "mcp.prompts.listed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "mcp_resources_listed", Family: "mcp", EventName: "mcp.resources.listed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "mcp_servers_connected", Family: "mcp", EventName: "mcp.servers.connected", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "mcp_servers_listed", Family: "mcp", EventName: "mcp.servers.listed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "mcp_tools_listed", Family: "mcp", EventName: "mcp.tools.listed", EndpointRoles: []string{"desktop-event-logging", "segment"}},
	{Kind: "resource", Family: "observability", EventName: "resource", EndpointRoles: []string{"datadog-rum"}},
	{Kind: "telemetry", Family: "observability", EventName: "telemetry", EndpointRoles: []string{"datadog-rum"}},
	{Kind: "tengu_auto_mode_decision", Family: "sdk", EventName: "tengu_auto_mode_decision", EndpointRoles: []string{"datadog-logs", "sdk-event-logging"}},
	{Kind: "tengu_auto_mode_outcome", Family: "sdk", EventName: "tengu_auto_mode_outcome", EndpointRoles: []string{"datadog-logs", "sdk-event-logging"}},
	{Kind: "tengu_bg_classify", Family: "sdk", EventName: "tengu_bg_classify", EndpointRoles: []string{"datadog-logs", "sdk-event-logging"}},
	{Kind: "tengu_bridge_repl_skipped", Family: "mcp", EventName: "tengu_bridge_repl_skipped", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_bridge_repl_ws_closed", Family: "mcp", EventName: "tengu_bridge_repl_ws_closed", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_bridge_repl_ws_connected", Family: "mcp", EventName: "tengu_bridge_repl_ws_connected", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_cli_flags", Family: "sdk", EventName: "tengu_cli_flags", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_client_data_cache_key", Family: "sdk", EventName: "tengu_client_data_cache_key", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_config_cache_stats", Family: "sdk", EventName: "tengu_config_cache_stats", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_config_fallback_write", Family: "sdk", EventName: "tengu_config_fallback_write", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_config_lock_contention", Family: "sdk", EventName: "tengu_config_lock_contention", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_config_stale_write", Family: "sdk", EventName: "tengu_config_stale_write", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_context_size", Family: "sdk", EventName: "tengu_context_size", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_dir_search", Family: "sdk", EventName: "tengu_dir_search", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_event_loop_stall", Family: "observability", EventName: "tengu_event_loop_stall", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_file_activity", Family: "sdk", EventName: "tengu_file_activity", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_file_changed", Family: "sdk", EventName: "tengu_file_changed", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_file_operation", Family: "sdk", EventName: "tengu_file_operation", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_fork_agent_query", Family: "sdk", EventName: "tengu_fork_agent_query", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_gzip_request_body_skipped", Family: "sdk", EventName: "tengu_gzip_request_body_skipped", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_headless_latency", Family: "observability", EventName: "tengu_headless_latency", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_heron_brook_applied", Family: "sdk", EventName: "tengu_heron_brook_applied", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_mcp_instructions_pool_change", Family: "mcp", EventName: "tengu_mcp_instructions_pool_change", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_mcp_reconcile", Family: "mcp", EventName: "tengu_mcp_reconcile", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_mcp_registry_fetch", Family: "mcp", EventName: "tengu_mcp_registry_fetch", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_mcp_sdk_generation", Family: "mcp", EventName: "tengu_mcp_sdk_generation", EndpointRoles: []string{"datadog-logs", "sdk-event-logging"}},
	{Kind: "tengu_mcp_tools_listed", Family: "mcp", EventName: "tengu_mcp_tools_listed", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_memdir_loaded", Family: "sdk", EventName: "tengu_memdir_loaded", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_org_memory_connected_mode", Family: "sdk", EventName: "tengu_org_memory_connected_mode", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_org_memory_decision", Family: "sdk", EventName: "tengu_org_memory_decision", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_org_penguin_mode_fetch_failed", Family: "sdk", EventName: "tengu_org_penguin_mode_fetch_failed", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_plugin_enabled_for_session", Family: "mcp", EventName: "tengu_plugin_enabled_for_session", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_plugin_name_collision", Family: "mcp", EventName: "tengu_plugin_name_collision", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_prompt_suggestion", Family: "sdk", EventName: "tengu_prompt_suggestion", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_prompt_suggestion_init", Family: "sdk", EventName: "tengu_prompt_suggestion_init", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_push_reachability", Family: "sdk", EventName: "tengu_push_reachability", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_reactive_compact_succeeded", Family: "sdk", EventName: "tengu_reactive_compact_succeeded", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_retention_sweep", Family: "sdk", EventName: "tengu_retention_sweep", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_ripgrep_availability", Family: "sdk", EventName: "tengu_ripgrep_availability", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_session_file_read", Family: "sdk", EventName: "tengu_session_file_read", EndpointRoles: []string{"datadog-logs", "sdk-event-logging"}},
	{Kind: "tengu_session_start", Family: "sdk", EventName: "tengu_session_start", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_startup_perf", Family: "observability", EventName: "tengu_startup_perf", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_uds_startup_bind", Family: "sdk", EventName: "tengu_uds_startup_bind", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_worker_permission_mode_restore", Family: "sdk", EventName: "tengu_worker_permission_mode_restore", EndpointRoles: []string{"sdk-event-logging"}},
	{Kind: "tengu_write_tool_not_read_hypothetical", Family: "sdk", EventName: "tengu_write_tool_not_read_hypothetical", EndpointRoles: []string{"sdk-event-logging"}},
}

func V140609ObservedExecutableEvents() []ObservedExecutableEvent {
	result := make([]ObservedExecutableEvent, 0, len(v140609ObservedExecutableEvents))
	for _, event := range v140609ObservedExecutableEvents {
		event.EndpointRoles = append([]string(nil), event.EndpointRoles...)
		event.Source, event.RendererCall = observedEventSource(event), observedRendererCall(event)
		result = append(result, event)
	}
	return result
}

func V140609ObservedExecutableEvent(kind string) (ObservedExecutableEvent, bool) {
	kind = strings.TrimSpace(kind)
	for _, event := range V140609ObservedExecutableEvents() {
		if event.Kind == kind {
			return event, true
		}
	}
	return ObservedExecutableEvent{}, false
}

func observedEventSource(event ObservedExecutableEvent) string {
	for _, role := range event.EndpointRoles {
		switch role {
		case "sdk-event-logging":
			return ObservedEventSourceSDK
		case "datadog-rum":
			return ObservedEventSourcePerformance
		case "sentry":
			return ObservedEventSourceCrash
		case "segment":
			return ObservedEventSourceRenderer
		}
	}
	if strings.HasPrefix(event.EventName, "claudeai.") || strings.HasPrefix(event.EventName, "chorus.") || strings.HasPrefix(event.EventName, "mcp.") {
		return ObservedEventSourceRenderer
	}
	return ObservedEventSourceMainProcess
}

func observedRendererCall(event ObservedExecutableEvent) string {
	if observedEventSource(event) != ObservedEventSourceRenderer {
		return ""
	}
	if strings.HasPrefix(event.EventName, "/") {
		return ObservedRendererCallPage
	}
	return ObservedRendererCallTrack
}

func installV140609ObservedExecutableMappings(bundle *Bundle) {
	if bundle == nil || bundle.ProfileID != "claude-desktop/windows-x64/1.40609.0.0" || bundle.DesktopVersion != "1.40609.0.0" {
		return
	}
	for _, event := range V140609ObservedExecutableEvents() {
		mapping := TelemetryEventProfile{EventName: event.EventName}
		fact := event.Fact()
		for _, role := range event.EndpointRoles {
			switch role {
			case bundle.Telemetry.EndpointRole:
				bundle.Telemetry.Events[fact] = mapping
			case bundle.SDKTelemetry.EndpointRole:
				bundle.SDKTelemetry.Events[fact] = mapping
			default:
				installAuxiliaryObservedMapping(&bundle.AuxiliaryTelemetry, role, fact, mapping)
			}
		}
	}
}

func installAuxiliaryObservedMapping(profiles *AuxiliaryTelemetryProfiles, role, fact string, mapping TelemetryEventProfile) {
	if profiles == nil {
		return
	}
	for _, profile := range []*AuxiliaryTelemetryProfile{
		&profiles.Segment, &profiles.DatadogLogs, &profiles.DatadogLogsBrowser, &profiles.DatadogRUM, &profiles.Sentry,
	} {
		if profile.EndpointRole == role {
			profile.Events[fact] = mapping
			return
		}
	}
}
