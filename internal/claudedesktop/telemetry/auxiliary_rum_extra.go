package telemetry

// Lane W8 rum-sentry: Datadog RUM `resource`, `long_task`, `telemetry` and the
// Sentry envelope item `attachment`.
//
// All four pairs were native-only boundaries in the historical capture. The
// captured contract (pinned by
// knowledge-kit/scripts/analysis/audit-desktop-telemetry-rum-source.mjs into
// testdata/auxiliary-telemetry-rum-native.json) shows that every one of them
// carries renderer- or Electron-process-measured fields that the gateway has no
// automatic source for. They now have closed, source-specific Desktop companion
// inputs; the table below retains the original native conditions and blocking
// fields so those inputs are not confused with automatic gateway measurements.

// auxiliaryRUMBoundary documents one endpoint/event pair that was a native-only
// boundary before the controlled Desktop companion source was added.
type auxiliaryRUMBoundary struct {
	EndpointRole string
	EventName    string
	// NativeCondition quotes when the native program emits the event.
	NativeCondition string
	// BlockingFields lists captured keys whose values vary and have no automatic
	// gateway source.
	BlockingFields []string
}

const (
	auxiliaryRUMSDKVersion    = "7.6.0"
	auxiliaryRUMTelemetryName = "telemetry"
	auxiliaryRUMResourceName  = "resource"
	auxiliaryRUMLongTaskName  = "long_task"
	sentryAttachmentItemName  = "attachment"
)

// auxiliaryRUMBoundaries returns the reviewed boundaries in golden order.
func auxiliaryRUMBoundaries() []auxiliaryRUMBoundary {
	return []auxiliaryRUMBoundary{
		{
			EndpointRole:    datadogRUMRole,
			EventName:       auxiliaryRUMTelemetryName,
			NativeCondition: "Datadog browser RUM SDK internal telemetry (service browser-rum-sdk): configuration once at SDK init and usage once per API feature per session, gated by the SDK telemetry session sample",
			BlockingFields:  []string{"version", "ddtags(version:)", "telemetry.connectivity.effective_type", "anonymous_id"},
		},
		{
			EndpointRole:    datadogRUMRole,
			EventName:       auxiliaryRUMResourceName,
			NativeCondition: "PerformanceResourceTiming / fetch instrumentation of the renderer (track_resources true): one event per completed renderer request",
			BlockingFields:  []string{"context.*", "display.viewport", "tab.id", "connectivity.effective_type", "version", "_dd.drift", "resource.response.headers", "_dd.span_id", "_dd.trace_id"},
		},
		{
			EndpointRole:    datadogRUMRole,
			EventName:       auxiliaryRUMLongTaskName,
			NativeCondition: "Long Animation Frames API entry (entry_type long-animation-frame, duration >= 50 ms) observed on the renderer main thread (track_long_task true)",
			BlockingFields:  []string{"long_task.blocking_duration", "long_task.render_start", "long_task.style_and_layout_start", "long_task.scripts", "long_task.first_ui_event_timestamp"},
		},
		{
			EndpointRole:    sentryRole,
			EventName:       sentryAttachmentItemName,
			NativeCondition: "sentry.javascript.electron 7.12.0 crash upload: after a native crash of the Desktop Electron process one envelope carries an event item followed by an attachment item with the Crashpad minidump (attachment_type event.minidump, filename <uuid>.dmp)",
			BlockingFields:  []string{"attachment payload (minidump bytes)", "filename", "length"},
		},
	}
}

// auxiliaryRUMBoundaryRegistered reports whether a reviewed historical boundary
// now has a controlled Desktop companion source registered as executable.
func auxiliaryRUMBoundaryRegistered(role, eventName string) bool {
	for _, name := range executableEvents[role] {
		if name == eventName {
			return true
		}
	}
	return false
}
