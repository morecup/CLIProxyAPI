package telemetry

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

const rendererGiB = uint64(1 << 30)

const (
	rendererSentryTraceID = "00000000000000000000000000000000"
	rendererSentrySpanID  = "0000000000000000"
)

var rendererPhysicalMemoryBytes = detectRendererPhysicalMemoryBytes
var rendererHostSnapshotNow = detectRendererHostSnapshot

type rendererHostSnapshot = HostSnapshot

// The fallback supports legacy direct observers; ordinary Desktop execution
// supplies the independently persisted record ID before telemetry is optional.
func rendererRecordID(facts RequestFacts) string {
	if facts.DesktopSessionID != "" {
		return facts.DesktopSessionID
	}
	return rendererLocalSessionID(facts.SessionID)
}

func rendererRecordKey(facts RequestFacts) string {
	if facts.DesktopSessionID != "" {
		return facts.DesktopSessionID
	}
	return facts.SessionID
}

func rendererLocalSessionID(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if strings.HasPrefix(sessionID, "local_") {
		if _, errParse := uuid.Parse(strings.TrimPrefix(sessionID, "local_")); errParse == nil {
			return sessionID
		}
	}
	if parsed, errParse := uuid.Parse(sessionID); errParse == nil {
		return "local_" + parsed.String()
	}
	return "local_" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(sessionID)).String()
}

func rendererRuntimeMetadata(snapshotProvider RendererRuntimeSnapshotProvider, hostProviders ...HostSnapshotProvider) map[string]any {
	host := rendererHostSnapshotNow()
	var hostProvider HostSnapshotProvider
	if len(hostProviders) > 0 {
		hostProvider = hostProviders[0]
	}
	if hostProvider != nil {
		host = hostProvider()
	}
	metadata := make(map[string]any, 28)
	if host.TotalMemoryBytes > 0 {
		metadata["total_memory"] = host.TotalMemoryBytes
		metadata["device_class"] = rendererDeviceClass(host.TotalMemoryBytes)
	}
	if host.AvailableMemoryBytes > 0 {
		metadata["available_memory"] = host.AvailableMemoryBytes
		metadata["free_memory"] = host.AvailableMemoryBytes
	}
	if host.CPUModel != "" {
		metadata["cpu_model"] = host.CPUModel
	}
	if host.OSBuild != "" {
		metadata["os_build"] = host.OSBuild
	}
	if host.OSRelease != "" {
		metadata["os_release"] = host.OSRelease
	}
	if host.OSVersion != "" {
		metadata["os_version"] = host.OSVersion
	}
	if snapshotProvider == nil {
		return metadata
	}
	snapshot, ok := snapshotProvider()
	if !ok {
		return metadata
	}
	metadata["process_footprint_sample_age_ms"] = snapshot.ProcessFootprintSampleAgeMS
	metadata["process_gpu_rss_bytes"] = snapshot.ProcessGPURSSBytes
	metadata["process_main_commit_bytes"] = snapshot.ProcessMainCommitBytes
	metadata["process_main_cpu_pct"] = snapshot.ProcessMainCPUPct
	metadata["process_main_external_bytes"] = snapshot.ProcessMainExternalBytes
	metadata["process_main_footprint_bytes"] = snapshot.ProcessMainFootprintBytes
	metadata["process_main_heap_limit_bytes"] = snapshot.ProcessMainHeapLimitBytes
	metadata["process_main_heap_total_bytes"] = snapshot.ProcessMainHeapTotalBytes
	metadata["process_main_heap_used_bytes"] = snapshot.ProcessMainHeapUsedBytes
	metadata["process_main_rss_bytes"] = snapshot.ProcessMainRSSBytes
	metadata["process_renderer_cpu_sum_pct"] = snapshot.ProcessRendererCPUSumPct
	metadata["process_renderer_footprint_sum_bytes"] = snapshot.ProcessRendererFootprintSumBytes
	metadata["process_renderer_main_view_blink_bytes"] = snapshot.ProcessRendererMainViewBlinkBytes
	metadata["process_renderer_main_view_heap_limit_bytes"] = snapshot.ProcessRendererMainViewHeapLimitBytes
	metadata["process_renderer_main_view_heap_sample_age_ms"] = snapshot.ProcessRendererMainViewHeapSampleAgeMS
	metadata["process_renderer_main_view_heap_total_bytes"] = snapshot.ProcessRendererMainViewHeapTotalBytes
	metadata["process_renderer_main_view_heap_used_bytes"] = snapshot.ProcessRendererMainViewHeapUsedBytes
	metadata["process_renderer_rss_sum_bytes"] = snapshot.ProcessRendererRSSSumBytes
	metadata["process_utility_rss_sum_bytes"] = snapshot.ProcessUtilityRSSSumBytes
	metadata["window_count"] = snapshot.WindowCount
	return metadata
}

func applyRendererRequestHeaders(request *http.Request, runtime *claudeprofile.RendererRuntimeProfile, sentry *claudeprofile.TelemetrySentryProfile, hostProviders ...HostSnapshotProvider) error {
	if request == nil || runtime == nil || sentry == nil {
		return fmt.Errorf("renderer telemetry profile is incomplete")
	}
	totalBytes := rendererPhysicalMemoryBytes()
	var hostProvider HostSnapshotProvider
	if len(hostProviders) > 0 {
		hostProvider = hostProviders[0]
	}
	if hostProvider != nil {
		totalBytes = hostProvider().TotalMemoryBytes
	}
	if totalBytes == 0 {
		totalBytes = uint64(runtime.FallbackTotalMemoryGB) * rendererGiB
	}
	totalMemoryGB := (totalBytes + rendererGiB/2) / rendererGiB
	if totalMemoryGB == 0 {
		totalMemoryGB = 1
	}

	request.Header.Set("Accept-Encoding", runtime.AcceptEncoding)
	request.Header.Set("Accept-Language", runtime.AcceptLanguage)
	request.Header.Set("anthropic-client-app", runtime.ClientApp)
	request.Header.Set("anthropic-client-device-class", rendererDeviceClass(totalBytes))
	request.Header.Set("anthropic-client-os-platform", runtime.OSPlatform)
	request.Header.Set("anthropic-client-os-version", runtime.OSVersion)
	request.Header.Set("anthropic-client-platform", runtime.Platform)
	request.Header.Set("anthropic-client-total-memory-gb", strconv.FormatUint(totalMemoryGB, 10))
	request.Header.Set("anthropic-client-version", runtime.ClientVersion)
	request.Header.Set("anthropic-desktop-topbar", runtime.DesktopTopbar)
	request.Header.Set("sec-fetch-dest", runtime.SecFetchDest)
	request.Header.Set("sec-fetch-mode", runtime.SecFetchMode)
	request.Header.Set("sec-fetch-site", runtime.SecFetchSite)
	request.Header.Set("User-Agent", runtime.UserAgent)
	request.Header.Set("Priority", runtime.Priority)

	request.Header.Set("sentry-trace", rendererSentryTraceID+"-"+rendererSentrySpanID)
	request.Header.Set("baggage", strings.Join([]string{
		"sentry-environment=" + sentry.Environment,
		"sentry-release=" + sentry.Release,
		"sentry-public_key=" + sentry.PublicKey,
		"sentry-trace_id=" + rendererSentryTraceID,
		"sentry-org_id=" + sentry.OrgID,
	}, ","))
	return nil
}

// DetectHostSnapshot captures the local machine fields used by renderer telemetry.
func DetectHostSnapshot() HostSnapshot {
	return rendererHostSnapshotNow()
}

func rendererDeviceClass(totalBytes uint64) string {
	switch {
	case totalBytes < 6*rendererGiB:
		return "le4"
	case totalBytes < 12*rendererGiB:
		return "8"
	case totalBytes < 20*rendererGiB:
		return "16"
	default:
		return "gt16"
	}
}
