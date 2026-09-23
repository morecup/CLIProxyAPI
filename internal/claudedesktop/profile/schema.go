package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	SupportedSchemaVersion        = 11
	SupportedRequestProfileSchema = 1
)

type RequestRole string

const (
	RoleMain            RequestRole = "main"
	RoleTitle           RequestRole = "title"
	RoleLightHelper     RequestRole = "light-helper"
	RoleWebSearchHelper RequestRole = "web-search-helper"
	RoleCompaction      RequestRole = "compaction"
	RoleSubagent        RequestRole = "subagent"
	RoleSecurityMonitor RequestRole = "security-monitor"
	RoleCountTokens     RequestRole = "count-tokens"
)

type InstructionCarrier string

const (
	CarrierNone                  InstructionCarrier = "none"
	CarrierMidConversationSystem InstructionCarrier = "mid-conversation-system"
	CarrierUserSystemReminder    InstructionCarrier = "user-system-reminder"
)

type StreamPolicy string

const (
	StreamPolicyTransport    StreamPolicy = "transport"
	StreamPolicyRequiredTrue StreamPolicy = "required-true"
	StreamPolicyOmitFalse    StreamPolicy = "omit-false"
	StreamPolicyForbidden    StreamPolicy = "forbidden"
)

const (
	SystemPolicyBundleOwned      = "bundle-owned"
	SystemPolicyPreserveVerified = "preserve-verified"
	SystemPolicyForbidden        = "forbidden"
)

type HeaderProfile struct {
	Accept           string `json:"accept"`
	AcceptEncoding   string `json:"accept_encoding"`
	IncludeTimeout   bool   `json:"include_timeout"`
	Timeout          string `json:"timeout,omitempty"`
	IncludeAsync     bool   `json:"include_async"`
	IncludeClientID  bool   `json:"include_client_request_id"`
	IncludeSessionID bool   `json:"include_session_id"`
	ClientPlatform   string `json:"client_platform,omitempty"`
	ClientVersion    string `json:"client_version,omitempty"`
	RequestClass     string `json:"request_class,omitempty"`
}

type TransportProfile struct {
	Protocol            string         `json:"protocol"`
	ClientHelloPreset   string         `json:"client_hello_preset,omitempty"`
	JA3Hash             string         `json:"ja3_hash"`
	TLSLegacyVersion    uint16         `json:"tls_legacy_version"`
	CipherSuites        []uint16       `json:"cipher_suites"`
	ExtensionOrder      []uint16       `json:"extension_order"`
	SupportedGroups     []uint16       `json:"supported_groups"`
	SignatureAlgorithms []uint16       `json:"signature_algorithms"`
	SupportedVersions   []uint16       `json:"supported_versions"`
	KeyShareGroups      []uint16       `json:"key_share_groups"`
	ECPointFormats      []uint8        `json:"ec_point_formats"`
	ALPN                []string       `json:"alpn"`
	HeaderOrder         []string       `json:"header_order"`
	OptionalHeaders     []string       `json:"optional_headers,omitempty"`
	HTTP2Settings       []HTTP2Setting `json:"http2_settings,omitempty"`
	ConnectionWindow    uint32         `json:"connection_window_increment,omitempty"`
}

type HTTP2Setting struct {
	ID    uint16 `json:"id"`
	Value uint32 `json:"value"`
}

type TransportProfiles struct {
	Profiles         map[string]TransportProfile `json:"profiles"`
	RoleProfiles     map[RequestRole]string      `json:"role_profiles"`
	EndpointProfiles map[string]string           `json:"endpoint_profiles,omitempty"`
}

type BodyProfile struct {
	TopLevelOrder      []string        `json:"top_level_order"`
	MaxTokens          int64           `json:"max_tokens,omitempty"`
	Thinking           json.RawMessage `json:"thinking,omitempty"`
	ContextManagement  json.RawMessage `json:"context_management,omitempty"`
	OutputConfig       json.RawMessage `json:"output_config,omitempty"`
	Diagnostics        json.RawMessage `json:"diagnostics,omitempty"`
	Temperature        json.RawMessage `json:"temperature,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	EnsureTools        bool            `json:"ensure_tools"`
	RemoveUnlistedKeys bool            `json:"remove_unlisted_keys"`
}

type BodyProfiles struct {
	Profiles map[string]BodyProfile `json:"profiles"`
	Bindings map[string]string      `json:"bindings"`
}

type RequestVariantKey struct {
	Model           string      `json:"model"`
	LogicalModel    string      `json:"logical_model"`
	Role            RequestRole `json:"role"`
	Diagnostics     bool        `json:"diagnostics"`
	ThinkingDisplay string      `json:"thinking_display,omitempty"`
}

type CacheControl struct {
	Type  string `json:"type"`
	TTL   string `json:"ttl,omitempty"`
	Scope string `json:"scope,omitempty"`
}

type SystemBlockPlan struct {
	Kind         string        `json:"kind"`
	Artifact     string        `json:"artifact,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type TextArtifact struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Text   string `json:"text"`
}

type RequestVariant struct {
	Key                   RequestVariantKey   `json:"key"`
	StreamPolicy          StreamPolicy        `json:"stream_policy"`
	AnthropicBeta         []string            `json:"anthropic_beta"`
	AnthropicBetaVariants map[string][]string `json:"anthropic_beta_variants,omitempty"`
	SystemBlockCount      int                 `json:"system_block_count"`
	System                []SystemBlockPlan   `json:"system"`
	InstructionCarrier    InstructionCarrier  `json:"instruction_carrier"`
	TopLevelSystemPolicy  string              `json:"top_level_system_policy"`
	Headers               HeaderProfile       `json:"headers"`

	requestProfileID string
}

type SoftwareProfile struct {
	UserAgent      string `json:"user_agent"`
	PackageVersion string `json:"package_version"`
	RuntimeVersion string `json:"runtime_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Timeout        string `json:"timeout"`
}

type EnvironmentProfile struct {
	Platform          string            `json:"platform"`
	OSVersion         string            `json:"os_version"`
	DefaultWorkingDir string            `json:"default_working_dir"`
	ModelDisplayNames map[string]string `json:"model_display_names"`
}

type TelemetryHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type TelemetryBatchProfile struct {
	FlushIntervalMS  int     `json:"flush_interval_ms"`
	JitterMinimum    float64 `json:"jitter_minimum"`
	JitterMaximum    float64 `json:"jitter_maximum"`
	MaxEvents        int     `json:"max_events"`
	MaxBytes         int     `json:"max_bytes"`
	MaxRetries       int     `json:"max_retries"`
	InitialBackoffMS int     `json:"initial_backoff_ms"`
	MaxBackoffMS     int     `json:"max_backoff_ms"`
	LeaseTimeoutMS   int     `json:"lease_timeout_ms"`
	MaxPendingEvents int     `json:"max_pending_events"`
	MaxDeadLetters   int     `json:"max_dead_letters"`
}

type TelemetryEventProfile struct {
	EventName     string   `json:"event_name"`
	RequiredFacts []string `json:"required_facts,omitempty"`
}

type TelemetryUnsupportedEndpoint struct {
	Role   string `json:"role"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type TelemetryCaptureWindow struct {
	FirstCapturedAt string `json:"first_captured_at"`
	LastCapturedAt  string `json:"last_captured_at"`
}

type TelemetryCorpusEvidence struct {
	FlowCount               int `json:"flow_count"`
	HTTPScenarioCount       int `json:"http_scenario_count"`
	EligibleScenarioCount   int `json:"eligible_scenario_count"`
	TelemetryBatchFlowCount int `json:"telemetry_batch_flow_count"`
	EventCount              int `json:"event_count"`
	EventNameCount          int `json:"event_name_count"`
}

type TelemetryArtifactEvidence struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type TelemetryObservedEndpointEvidence struct {
	Role                 string `json:"role"`
	TelemetryClass       string `json:"telemetry_class"`
	Status               string `json:"status"`
	TransportProtocol    string `json:"transport_protocol"`
	BodyCoverage         string `json:"body_coverage"`
	FlowCount            int    `json:"flow_count"`
	ScenarioCount        int    `json:"scenario_count"`
	JSONBodyFlowCount    int    `json:"json_body_flow_count"`
	OpaqueBodyFlowCount  int    `json:"opaque_body_flow_count"`
	MissingBodyFlowCount int    `json:"missing_body_flow_count"`
	EventCount           int    `json:"event_count,omitempty"`
	MaximumBatchEvents   int    `json:"maximum_batch_events,omitempty"`
	MaximumBatchBytes    int    `json:"maximum_batch_bytes,omitempty"`
	Reason               string `json:"reason"`
}

// TelemetryEvidenceProfile contains aggregate, secret-free facts derived from
// the protected recorder corpus. It intentionally excludes destination query
// strings, credentials, cookies, and captured account identity values.
type TelemetryEvidenceProfile struct {
	SchemaVersion        int                                 `json:"schema_version"`
	SourceManifestSHA256 string                              `json:"source_manifest_sha256"`
	CaptureWindow        TelemetryCaptureWindow              `json:"capture_window"`
	Corpus               TelemetryCorpusEvidence             `json:"corpus"`
	Artifacts            []TelemetryArtifactEvidence         `json:"artifacts"`
	ObservedEndpoints    []TelemetryObservedEndpointEvidence `json:"observed_endpoints"`
	EmitterCoverage      TelemetryEmitterCoverageEvidence    `json:"emitter_coverage"`
}

// TelemetryEmitterCoverageEvidence pins the event-name universe observed in
// the sanitized event-state timeline. It lets runtime status distinguish the
// small fact-driven live subset from the much larger captured Desktop event
// surface instead of declaring completion against its own mappings.
type TelemetryEmitterCoverageEvidence struct {
	SourceArtifact           string                   `json:"source_artifact"`
	ObservableEventNames     []string                 `json:"observable_event_names"`
	ObservableEndpointEvents []TelemetryEndpointEvent `json:"observable_endpoint_events,omitempty"`
	ObservedScope            *TelemetryObservedScope  `json:"observed_scope,omitempty"`
}

type TelemetryEndpointEvent struct {
	EndpointRole string `json:"endpoint_role"`
	EventName    string `json:"event_name"`
}

type TelemetryTransportProfile struct {
	Revision          string         `json:"revision"`
	Protocol          string         `json:"protocol"`
	ClientHelloPreset string         `json:"client_hello_preset"`
	UserAgentPolicy   string         `json:"user_agent_policy"`
	EvidenceStatus    string         `json:"evidence_status"`
	HeaderOrder       []string       `json:"header_order"`
	HTTP2Settings     []HTTP2Setting `json:"http2_settings"`
	ConnectionWindow  uint32         `json:"connection_window_increment"`
	Reason            string         `json:"reason"`
}

type RendererRuntimeProfile struct {
	ChromiumVersion       string `json:"chromium_version"`
	ElectronVersion       string `json:"electron_version"`
	UserAgent             string `json:"user_agent"`
	AcceptEncoding        string `json:"accept_encoding"`
	AcceptLanguage        string `json:"accept_language"`
	ClientApp             string `json:"client_app"`
	ClientVersion         string `json:"client_version"`
	OSPlatform            string `json:"os_platform"`
	OSVersion             string `json:"os_version"`
	Platform              string `json:"platform"`
	DesktopTopbar         string `json:"desktop_topbar"`
	SecFetchDest          string `json:"sec_fetch_dest"`
	SecFetchMode          string `json:"sec_fetch_mode"`
	SecFetchSite          string `json:"sec_fetch_site"`
	Priority              string `json:"priority"`
	MemoryPolicy          string `json:"memory_policy"`
	FallbackTotalMemoryGB int    `json:"fallback_total_memory_gb"`
}

type TelemetrySentryProfile struct {
	Environment string `json:"environment"`
	Release     string `json:"release"`
	PublicKey   string `json:"public_key"`
	OrgID       string `json:"org_id"`
}

// TelemetryProfile is compiled only from secret-free source/corpus findings.
// Endpoint credentials, cookies, raw URLs, and captured identity values are
// deliberately not part of the immutable bundle.
type TelemetryProfile struct {
	SchemaVersion int                              `json:"schema_version"`
	Source        string                           `json:"source"`
	DesktopCommit string                           `json:"desktop_commit"`
	Required      bool                             `json:"required"`
	EndpointRole  string                           `json:"endpoint_role"`
	Endpoint      string                           `json:"endpoint"`
	EventType     string                           `json:"event_type"`
	Transport     TelemetryTransportProfile        `json:"transport"`
	Runtime       RendererRuntimeProfile           `json:"runtime"`
	Sentry        TelemetrySentryProfile           `json:"sentry"`
	Headers       []TelemetryHeader                `json:"headers"`
	Batch         TelemetryBatchProfile            `json:"batch"`
	BaseMetadata  map[string]any                   `json:"base_metadata"`
	Events        map[string]TelemetryEventProfile `json:"events"`
	Unsupported   []TelemetryUnsupportedEndpoint   `json:"unsupported_endpoints,omitempty"`
}

// SDKTelemetryProfile describes the Node/Agent SDK event logger embedded in
// Claude Desktop. It is intentionally separate from TelemetryProfile because
// the renderer and SDK senders use different origins, authentication, wire
// headers, schemas, batching, and connection pools.
type SDKTelemetryProfile struct {
	InputBetas       map[string][]string              `json:"input_betas,omitempty"`
	SchemaVersion    int                              `json:"schema_version"`
	Source           string                           `json:"source"`
	Required         bool                             `json:"required"`
	EndpointRole     string                           `json:"endpoint_role"`
	Endpoint         string                           `json:"endpoint"`
	EventType        string                           `json:"event_type"`
	TransportProfile string                           `json:"transport_profile"`
	AuthPolicy       string                           `json:"auth_policy"`
	HeaderOrder      []string                         `json:"header_order"`
	Headers          []TelemetryHeader                `json:"headers"`
	Batch            TelemetryBatchProfile            `json:"batch"`
	Environment      map[string]any                   `json:"environment"`
	Process          SDKTelemetryProcessProfile       `json:"process"`
	Events           map[string]TelemetryEventProfile `json:"events"`
}

type SDKTelemetryProcessProfile struct {
	ConstrainedMemory uint64 `json:"constrained_memory"`
}

// AuxiliaryTelemetryProfile describes one independently delivered Desktop
// telemetry stream. Runtime material names are references into the encrypted
// Claude Desktop enrollment; the immutable bundle never contains their values.
type AuxiliaryTelemetryProfile struct {
	SchemaVersion    int                              `json:"schema_version"`
	Source           string                           `json:"source"`
	Required         bool                             `json:"required"`
	TelemetryClass   string                           `json:"telemetry_class"`
	EndpointRole     string                           `json:"endpoint_role"`
	Endpoint         string                           `json:"endpoint"`
	BodyFormat       string                           `json:"body_format"`
	TransportProfile string                           `json:"transport_profile"`
	Protocol         string                           `json:"protocol"`
	UserAgentPolicy  string                           `json:"user_agent_policy"`
	AuthPolicy       string                           `json:"auth_policy"`
	HeaderOrder      []string                         `json:"header_order"`
	OptionalHeaders  []string                         `json:"optional_headers,omitempty"`
	Headers          []TelemetryHeader                `json:"headers"`
	RuntimeMaterials map[string]string                `json:"runtime_materials"`
	Batch            TelemetryBatchProfile            `json:"batch"`
	Events           map[string]TelemetryEventProfile `json:"events"`
}

type AuxiliaryTelemetryProfiles struct {
	Segment            AuxiliaryTelemetryProfile `json:"segment"`
	DatadogLogs        AuxiliaryTelemetryProfile `json:"datadog_logs"`
	DatadogLogsBrowser AuxiliaryTelemetryProfile `json:"datadog_logs_browser"`
	DatadogRUM         AuxiliaryTelemetryProfile `json:"datadog_rum"`
	Sentry             AuxiliaryTelemetryProfile `json:"sentry"`
}

func (p AuxiliaryTelemetryProfiles) All() []AuxiliaryTelemetryProfile {
	return []AuxiliaryTelemetryProfile{p.Segment, p.DatadogLogs, p.DatadogLogsBrowser, p.DatadogRUM, p.Sentry}
}

func (b *Bundle) IsTelemetryEndpointRole(role string) bool {
	if b == nil {
		return false
	}
	role = strings.TrimSpace(role)
	if role == b.Telemetry.EndpointRole || role == b.SDKTelemetry.EndpointRole {
		return true
	}
	for _, profile := range b.AuxiliaryTelemetry.All() {
		if role == profile.EndpointRole {
			return true
		}
	}
	return false
}

const (
	ControlEndpointCreateSession        = "create_session"
	ControlEndpointBridge               = "bridge"
	ControlEndpointWorkerStream         = "worker_stream"
	ControlEndpointWorkerRead           = "worker_read"
	ControlEndpointWorkerInternalEvents = "worker_internal_events"
	ControlEndpointWorkerUpdate         = "worker_update"
	ControlEndpointWorkerEvents         = "worker_events"
	ControlEndpointWorkerDelivery       = "worker_delivery"
	ControlEndpointWorkerHeartbeat      = "worker_heartbeat"
	ControlEndpointSessionRead          = "session_read"
	ControlEndpointSessionUpdate        = "session_update"
	ControlEndpointSessionArchive       = "session_archive"
	ControlEndpointSessionUnarchive     = "session_unarchive"
)

// ControlPlaneEndpointProfile describes one captured remote-control request
// class. Dynamic credentials, organization IDs, host, and content length are
// deliberately supplied at runtime and never embedded in the bundle.
type ControlPlaneEndpointProfile struct {
	EndpointRole string            `json:"endpoint_role"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	AuthPolicy   string            `json:"auth_policy"`
	HeaderOrder  []string          `json:"header_order"`
	Headers      []TelemetryHeader `json:"headers"`
}

// ControlPlaneWorkerProfile contains the secret-free, version-bound worker
// inventory emitted by Claude Desktop's embedded Agent SDK init event. Values
// that vary per request (model, effort, permission mode, session IDs, UUIDs,
// and rate-limit state) are deliberately supplied at runtime.
type ControlPlaneWorkerProfile struct {
	APIKeySource            string   `json:"api_key_source"`
	ClaudeCodeVersion       string   `json:"claude_code_version"`
	OutputStyle             string   `json:"output_style"`
	SlashCommands           []string `json:"slash_commands"`
	Agents                  []string `json:"agents"`
	Skills                  []string `json:"skills"`
	AnalyticsDisabled       bool     `json:"analytics_disabled"`
	ProductFeedbackDisabled bool     `json:"product_feedback_disabled"`
	FastModeState           string   `json:"fast_mode_state"`
	FastModeDisabledReason  string   `json:"fast_mode_disabled_reason"`
}

// ControlPlaneProfile captures the Claude Desktop remote-control session,
// worker, heartbeat, and archive protocol independently from message calls.
type ControlPlaneProfile struct {
	SchemaVersion                int                                    `json:"schema_version"`
	Source                       string                                 `json:"source"`
	Required                     bool                                   `json:"required"`
	BaseURL                      string                                 `json:"base_url"`
	HeartbeatIntervalSeconds     int                                    `json:"heartbeat_interval_seconds"`
	WorkerJWTRefreshLeadSeconds  int                                    `json:"worker_jwt_refresh_lead_seconds"`
	StreamReconnectBackoffMillis int                                    `json:"stream_reconnect_backoff_ms"`
	TeardownArchiveBudgetMillis  int                                    `json:"teardown_archive_budget_ms"`
	Worker                       ControlPlaneWorkerProfile              `json:"worker"`
	Endpoints                    map[string]ControlPlaneEndpointProfile `json:"endpoints"`
}

type StartupQueryParameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StartupHeaderProfile captures one observed Desktop sender family. Dynamic
// credentials and runtime identifiers are deliberately absent from Headers
// and are supplied by the account-local startup manager.
type StartupHeaderProfile struct {
	TransportProfile string            `json:"transport_profile"`
	Protocol         string            `json:"protocol"`
	HeaderOrder      []string          `json:"header_order"`
	Headers          []TelemetryHeader `json:"headers"`
}

type StartupEndpointProfile struct {
	EndpointRole  string                  `json:"endpoint_role"`
	Method        string                  `json:"method"`
	Endpoint      string                  `json:"endpoint"`
	HeaderProfile string                  `json:"header_profile"`
	AuthPolicy    string                  `json:"auth_policy"`
	ResponseMode  string                  `json:"response_mode"`
	Query         []StartupQueryParameter `json:"query,omitempty"`
	RequiredFacts []string                `json:"required_facts,omitempty"`
}

// StartupProfile is the evidence-backed application activation request set.
// It excludes browser assets, CDN traffic, challenge endpoints, advertising,
// and other noise that is not part of Claude Desktop's control surface.
type StartupProfile struct {
	SchemaVersion    int                             `json:"schema_version"`
	Source           string                          `json:"source"`
	Required         bool                            `json:"required"`
	RequestTimeoutMS int                             `json:"request_timeout_ms"`
	MaxResponseBytes int64                           `json:"max_response_bytes"`
	WebClientBuild   string                          `json:"web_client_build"`
	WebClientSHA     string                          `json:"web_client_sha"`
	HeaderProfiles   map[string]StartupHeaderProfile `json:"header_profiles"`
	Endpoints        []StartupEndpointProfile        `json:"endpoints"`
}

// RequestProfile is a request-only overlay for a newer Claude Desktop and
// Claude Code pair. It deliberately excludes telemetry and control-plane
// identity so a current Code request contract can coexist with an immutable
// historical Desktop telemetry bundle.
type RequestProfile struct {
	SchemaVersion      int                     `json:"schema_version"`
	ProfileID          string                  `json:"profile_id"`
	DesktopVersion     string                  `json:"desktop_version"`
	CodeVersion        string                  `json:"code_version"`
	AgentSDKVersion    string                  `json:"agent_sdk_version"`
	Software           SoftwareProfile         `json:"software"`
	Body               BodyProfiles            `json:"body"`
	Environment        EnvironmentProfile      `json:"environment"`
	Artifacts          map[string]TextArtifact `json:"artifacts"`
	Variants           []RequestVariant        `json:"variants"`
	CountTokensCatalog string                  `json:"count_tokens_catalog,omitempty"`
}

type Bundle struct {
	SchemaVersion      int                        `json:"schema_version"`
	ProfileID          string                     `json:"profile_id"`
	DesktopVersion     string                     `json:"desktop_version"`
	CodeVersion        string                     `json:"code_version"`
	AgentSDKVersion    string                     `json:"agent_sdk_version"`
	Software           SoftwareProfile            `json:"software"`
	Transport          TransportProfiles          `json:"transport"`
	Body               BodyProfiles               `json:"body"`
	Telemetry          TelemetryProfile           `json:"telemetry"`
	SDKTelemetry       SDKTelemetryProfile        `json:"sdk_telemetry"`
	AuxiliaryTelemetry AuxiliaryTelemetryProfiles `json:"auxiliary_telemetry"`
	Startup            StartupProfile             `json:"startup"`
	ControlPlane       ControlPlaneProfile        `json:"control_plane"`
	TelemetryEvidence  TelemetryEvidenceProfile   `json:"telemetry_evidence"`
	Environment        EnvironmentProfile         `json:"environment"`
	Artifacts          map[string]TextArtifact    `json:"artifacts"`
	Variants           []RequestVariant           `json:"variants"`
	RequestProfiles    []RequestProfile           `json:"request_profiles,omitempty"`

	byKey                   map[RequestVariantKey]RequestVariant
	overlayByKey            map[RequestVariantKey]RequestVariant
	requestProfileIndexByID map[string]int
}

var artifactPlaceholderPattern = regexp.MustCompile(`\{\{([A-Z0-9_]+)\}\}`)

var allowedArtifactPlaceholders = map[string]struct{}{
	"CURRENT_DATE_ISO":   {},
	"MEMORY_DIR":         {},
	"MODEL_DISPLAY_NAME": {},
	"OS_VERSION":         {},
	"SCRATCHPAD_DIR":     {},
	"USER_HOME":          {},
	"UUID":               {},
	"WORKING_DIR":        {},
}

func (b *Bundle) Validate() error {
	if b == nil {
		return fmt.Errorf("claude desktop profile: bundle is nil")
	}
	if b.SchemaVersion != SupportedSchemaVersion {
		return fmt.Errorf("claude desktop profile: unsupported schema version %d", b.SchemaVersion)
	}
	if strings.TrimSpace(b.ProfileID) == "" || strings.TrimSpace(b.DesktopVersion) == "" {
		return fmt.Errorf("claude desktop profile: profile_id and desktop_version are required")
	}
	if len(b.Variants) == 0 {
		return fmt.Errorf("claude desktop profile: no request variants")
	}
	if len(b.Artifacts) == 0 {
		return fmt.Errorf("claude desktop profile: no text artifacts")
	}
	if errTransport := validateTransportProfiles(b.Transport); errTransport != nil {
		return errTransport
	}
	if errBody := validateBodyProfiles(&b.Body); errBody != nil {
		return errBody
	}
	if errTelemetry := validateTelemetryProfile(b.Telemetry); errTelemetry != nil {
		return errTelemetry
	}
	if errTelemetry := validateSDKTelemetryProfile(b.SDKTelemetry, b.Transport); errTelemetry != nil {
		return errTelemetry
	}
	if errTelemetry := validateAuxiliaryTelemetryProfiles(b.AuxiliaryTelemetry, b.Transport); errTelemetry != nil {
		return errTelemetry
	}
	if errControlPlane := validateControlPlaneProfile(b.ControlPlane, b.Transport); errControlPlane != nil {
		return errControlPlane
	}
	if errStartup := validateStartupProfile(b.Startup, b.Transport); errStartup != nil {
		return errStartup
	}
	if errEvidence := validateTelemetryEvidenceProfile(b.TelemetryEvidence, b.Telemetry, b.SDKTelemetry); errEvidence != nil {
		return errEvidence
	}
	for name, artifact := range b.Artifacts {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("claude desktop profile: artifact name is empty")
		}
		if errArtifact := validateTextArtifact(name, artifact); errArtifact != nil {
			return errArtifact
		}
	}
	b.byKey = make(map[RequestVariantKey]RequestVariant, len(b.Variants))
	for index, variant := range b.Variants {
		variant.requestProfileID = ""
		variant.Key.Model = normalizeModel(variant.Key.Model)
		variant.Key.LogicalModel = normalizeModel(variant.Key.LogicalModel)
		if variant.Key.LogicalModel == "" {
			variant.Key.LogicalModel = variant.Key.Model
		}
		variant.Key.ThinkingDisplay = strings.ToLower(strings.TrimSpace(variant.Key.ThinkingDisplay))
		if variant.Key.Model == "" || variant.Key.Role == "" {
			return fmt.Errorf("claude desktop profile: variants[%d] has an incomplete key", index)
		}
		if len(variant.AnthropicBeta) == 0 {
			return fmt.Errorf("claude desktop profile: variants[%d] has no anthropic_beta values", index)
		}
		switch variant.StreamPolicy {
		case StreamPolicyTransport:
		case StreamPolicyRequiredTrue:
			if variant.Key.Role != RoleTitle && variant.Key.Role != RoleWebSearchHelper {
				return fmt.Errorf("claude desktop profile: variants[%d] requires streaming for unsupported role %q", index, variant.Key.Role)
			}
		case StreamPolicyOmitFalse:
			if variant.Key.Role != RoleLightHelper && variant.Key.Role != RoleSecurityMonitor {
				return fmt.Errorf("claude desktop profile: variants[%d] omits false stream for unsupported role %q", index, variant.Key.Role)
			}
		case StreamPolicyForbidden:
			if variant.Key.Role != RoleCountTokens {
				return fmt.Errorf("claude desktop profile: variants[%d] forbids stream for unsupported role %q", index, variant.Key.Role)
			}
		default:
			return fmt.Errorf("claude desktop profile: variants[%d] has unsupported stream policy %q", index, variant.StreamPolicy)
		}
		if variant.Key.Role == RoleCountTokens && variant.StreamPolicy != StreamPolicyForbidden {
			return fmt.Errorf("claude desktop profile: count-tokens variant %q must forbid stream", variant.Key.Model)
		}
		if errBetas := validateBetaList(variant.AnthropicBeta); errBetas != nil {
			return fmt.Errorf("claude desktop profile: variants[%d] anthropic_beta: %w", index, errBetas)
		}
		for name, betas := range variant.AnthropicBetaVariants {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("claude desktop profile: variants[%d] has an empty beta variant name", index)
			}
			if errBetas := validateBetaList(betas); errBetas != nil {
				return fmt.Errorf("claude desktop profile: variants[%d] beta variant %q: %w", index, name, errBetas)
			}
		}
		switch variant.TopLevelSystemPolicy {
		case SystemPolicyForbidden:
			if variant.SystemBlockCount != 0 || len(variant.System) != 0 {
				return fmt.Errorf("claude desktop profile: forbidden-system variant %q must omit top-level system", variant.Key.Model)
			}
		case SystemPolicyBundleOwned, SystemPolicyPreserveVerified:
			if variant.SystemBlockCount <= 0 || len(variant.System) != variant.SystemBlockCount {
				return fmt.Errorf("claude desktop profile: variants[%d] has invalid system block count", index)
			}
		default:
			return fmt.Errorf("claude desktop profile: variants[%d] has unsupported system policy %q", index, variant.TopLevelSystemPolicy)
		}
		if variant.Key.Role == RoleCountTokens && variant.TopLevelSystemPolicy != SystemPolicyForbidden {
			return fmt.Errorf("claude desktop profile: count-tokens variant %q must forbid top-level system", variant.Key.Model)
		}
		if variant.Key.Role != RoleCountTokens && variant.TopLevelSystemPolicy == SystemPolicyForbidden {
			return fmt.Errorf("claude desktop profile: variants[%d] has invalid system block count", index)
		}
		if variant.InstructionCarrier != CarrierNone && variant.InstructionCarrier != CarrierMidConversationSystem && variant.InstructionCarrier != CarrierUserSystemReminder {
			return fmt.Errorf("claude desktop profile: variants[%d] has unsupported instruction carrier %q", index, variant.InstructionCarrier)
		}
		if variant.TopLevelSystemPolicy == SystemPolicyPreserveVerified && variant.InstructionCarrier != CarrierNone {
			return fmt.Errorf("claude desktop profile: preserve-verified variant %+v cannot inherit caller system instructions", variant.Key)
		}
		if strings.TrimSpace(variant.Headers.Accept) == "" || strings.TrimSpace(variant.Headers.AcceptEncoding) == "" {
			return fmt.Errorf("claude desktop profile: variants[%d] has incomplete headers", index)
		}
		clientPlatform := strings.TrimSpace(variant.Headers.ClientPlatform)
		clientVersion := strings.TrimSpace(variant.Headers.ClientVersion)
		requestClass := strings.TrimSpace(variant.Headers.RequestClass)
		if clientPlatform != "" || clientVersion != "" || requestClass != "" {
			if clientPlatform == "" || clientVersion == "" || requestClass == "" {
				return fmt.Errorf("claude desktop profile: variants[%d] has incomplete client identity headers", index)
			}
			if requestClass != "main" && requestClass != "auxiliary" && requestClass != "subagent" {
				return fmt.Errorf("claude desktop profile: variants[%d] has unsupported request class %q", index, requestClass)
			}
		}
		if _, errTransport := b.TransportForRole(variant.Key.Role); errTransport != nil {
			return fmt.Errorf("claude desktop profile: variants[%d]: %w", index, errTransport)
		}
		if _, errBody := b.BodyForVariant(variant); errBody != nil {
			return fmt.Errorf("claude desktop profile: variants[%d]: %w", index, errBody)
		}
		if variant.Headers.IncludeTimeout && strings.TrimSpace(variant.Headers.Timeout) == "" && strings.TrimSpace(b.Software.Timeout) == "" {
			return fmt.Errorf("claude desktop profile: variants[%d] enables timeout without a value", index)
		}
		for blockIndex, block := range variant.System {
			switch block.Kind {
			case "billing":
				if blockIndex != 0 || strings.TrimSpace(block.Artifact) != "" {
					return fmt.Errorf("claude desktop profile: variants[%d] has invalid billing system block", index)
				}
			case "artifact":
				if _, ok := b.Artifacts[strings.TrimSpace(block.Artifact)]; !ok {
					return fmt.Errorf("claude desktop profile: variants[%d] references unknown artifact %q", index, block.Artifact)
				}
			default:
				return fmt.Errorf("claude desktop profile: variants[%d] has unsupported system block kind %q", index, block.Kind)
			}
			if block.CacheControl != nil && strings.TrimSpace(block.CacheControl.Type) == "" {
				return fmt.Errorf("claude desktop profile: variants[%d] system[%d] has incomplete cache control", index, blockIndex)
			}
		}
		if _, exists := b.byKey[variant.Key]; exists {
			return fmt.Errorf("claude desktop profile: duplicate request variant %+v", variant.Key)
		}
		b.Variants[index] = variant
		b.byKey[variant.Key] = variant
	}
	b.overlayByKey = make(map[RequestVariantKey]RequestVariant)
	b.requestProfileIndexByID = make(map[string]int, len(b.RequestProfiles))
	for index := range b.RequestProfiles {
		requestProfile := &b.RequestProfiles[index]
		requestProfile.ProfileID = strings.TrimSpace(requestProfile.ProfileID)
		if requestProfile.SchemaVersion != SupportedRequestProfileSchema {
			return fmt.Errorf("claude desktop profile: request_profiles[%d] has unsupported schema version %d", index, requestProfile.SchemaVersion)
		}
		if requestProfile.ProfileID == "" || strings.TrimSpace(requestProfile.DesktopVersion) == "" ||
			strings.TrimSpace(requestProfile.CodeVersion) == "" || strings.TrimSpace(requestProfile.AgentSDKVersion) == "" {
			return fmt.Errorf("claude desktop profile: request_profiles[%d] has incomplete identity", index)
		}
		if requestProfile.ProfileID == b.ProfileID {
			return fmt.Errorf("claude desktop profile: request profile %q duplicates the bundle profile id", requestProfile.ProfileID)
		}
		if _, exists := b.requestProfileIndexByID[requestProfile.ProfileID]; exists {
			return fmt.Errorf("claude desktop profile: duplicate request profile %q", requestProfile.ProfileID)
		}

		// Reuse the complete request-surface validation against the overlay's
		// own body, software, environment, artifacts, and variants. Telemetry,
		// control-plane, and transport data remain inherited from the immutable
		// historical bundle and are not copied into the overlay JSON.
		surface := *b
		surface.ProfileID = requestProfile.ProfileID
		surface.DesktopVersion = requestProfile.DesktopVersion
		surface.CodeVersion = requestProfile.CodeVersion
		surface.AgentSDKVersion = requestProfile.AgentSDKVersion
		surface.Software = requestProfile.Software
		surface.Body = requestProfile.Body
		surface.Environment = requestProfile.Environment
		surface.Artifacts = requestProfile.Artifacts
		surface.Variants = append([]RequestVariant(nil), requestProfile.Variants...)
		surface.RequestProfiles = nil
		surface.byKey = nil
		surface.overlayByKey = nil
		surface.requestProfileIndexByID = nil
		if errSurface := surface.Validate(); errSurface != nil {
			return fmt.Errorf("claude desktop profile: request profile %q: %w", requestProfile.ProfileID, errSurface)
		}
		requestProfile.Body = surface.Body
		requestProfile.Artifacts = surface.Artifacts
		requestProfile.Variants = surface.Variants
		for variantIndex, variant := range requestProfile.Variants {
			variant.requestProfileID = requestProfile.ProfileID
			if _, exists := b.overlayByKey[variant.Key]; exists {
				return fmt.Errorf("claude desktop profile: duplicate overlay request variant %+v", variant.Key)
			}
			requestProfile.Variants[variantIndex] = variant
			b.overlayByKey[variant.Key] = variant
		}
		b.requestProfileIndexByID[requestProfile.ProfileID] = index
	}
	return nil
}

func validateTelemetryProfile(telemetry TelemetryProfile) error {
	if telemetry.SchemaVersion != 2 {
		return fmt.Errorf("claude desktop profile: unsupported telemetry schema version %d", telemetry.SchemaVersion)
	}
	if strings.TrimSpace(telemetry.Source) == "" || strings.TrimSpace(telemetry.EndpointRole) == "" {
		return fmt.Errorf("claude desktop profile: telemetry source and endpoint_role are required")
	}
	if len(strings.TrimSpace(telemetry.DesktopCommit)) != 40 {
		return fmt.Errorf("claude desktop profile: telemetry desktop_commit is invalid")
	}
	endpoint, errParse := url.Parse(strings.TrimSpace(telemetry.Endpoint))
	if errParse != nil || endpoint.Scheme != "https" || !strings.EqualFold(endpoint.Hostname(), "claude.ai") || endpoint.Path != "/api/event_logging/v2/batch" {
		return fmt.Errorf("claude desktop profile: telemetry endpoint must be the captured Claude Desktop event endpoint")
	}
	if telemetry.EventType != "TelemetryEvent" {
		return fmt.Errorf("claude desktop profile: unsupported telemetry event type %q", telemetry.EventType)
	}
	transport := telemetry.Transport
	if strings.TrimSpace(transport.Revision) == "" || transport.Protocol != "http/2" ||
		transport.ClientHelloPreset != "chromium-148-v140609" || transport.UserAgentPolicy != "profile" ||
		transport.EvidenceStatus != "captured-current-wire" ||
		strings.TrimSpace(transport.Reason) == "" {
		return fmt.Errorf("claude desktop profile: telemetry transport declaration is invalid")
	}
	wantHeaderOrder := []string{
		":method", ":authority", ":scheme", ":path", "content-length",
		"accept-encoding", "accept-language", "anthropic-client-app",
		"anthropic-client-device-class", "anthropic-client-os-platform",
		"anthropic-client-os-version", "anthropic-client-platform",
		"anthropic-client-total-memory-gb", "anthropic-client-version",
		"anthropic-desktop-topbar", "baggage", "content-type", "sec-fetch-dest",
		"sec-fetch-mode", "sec-fetch-site", "sentry-trace", "user-agent",
		"x-service-name", "priority",
	}
	if !equalStringSlice(transport.HeaderOrder, wantHeaderOrder) {
		return fmt.Errorf("claude desktop profile: telemetry HTTP/2 header order does not match the captured sender")
	}
	wantSettings := []HTTP2Setting{{ID: 1, Value: 65536}, {ID: 2, Value: 0}, {ID: 4, Value: 6291456}, {ID: 6, Value: 262144}}
	if !equalHTTP2Settings(transport.HTTP2Settings, wantSettings) || transport.ConnectionWindow != 15663105 {
		return fmt.Errorf("claude desktop profile: telemetry HTTP/2 preface does not match the captured sender")
	}
	headers := make(map[string]string, len(telemetry.Headers))
	for _, header := range telemetry.Headers {
		name := strings.ToLower(strings.TrimSpace(header.Name))
		if name == "" || strings.TrimSpace(header.Value) == "" {
			return fmt.Errorf("claude desktop profile: telemetry contains an incomplete header")
		}
		if name == "authorization" || name == "cookie" || name == "set-cookie" {
			return fmt.Errorf("claude desktop profile: telemetry must not embed credential header %q", header.Name)
		}
		if name == "user-agent" || name == "content-length" || name == "baggage" || name == "sentry-trace" {
			return fmt.Errorf("claude desktop profile: telemetry dynamic/runtime header %q must not be source-owned", header.Name)
		}
		if _, exists := headers[name]; exists {
			return fmt.Errorf("claude desktop profile: telemetry repeats header %q", header.Name)
		}
		headers[name] = strings.TrimSpace(header.Value)
	}
	if len(headers) != 2 || !strings.EqualFold(headers["content-type"], "application/json") || headers["x-service-name"] != "claude_desktop" {
		return fmt.Errorf("claude desktop profile: telemetry headers do not match the captured Desktop sender")
	}
	runtime := telemetry.Runtime
	if runtime.ChromiumVersion != "148.0.7778.280" || runtime.ElectronVersion != "42.10.0" ||
		runtime.ClientApp != "com.anthropic.claudefordesktop" || runtime.ClientVersion != "1.40609.0" ||
		runtime.OSPlatform != "win32" || strings.TrimSpace(runtime.OSVersion) == "" || runtime.Platform != "desktop_app" ||
		runtime.DesktopTopbar != "1" || runtime.AcceptEncoding != "gzip, deflate, br, zstd" || runtime.AcceptLanguage != "en-US" ||
		runtime.SecFetchDest != "empty" || runtime.SecFetchMode != "no-cors" || runtime.SecFetchSite != "none" ||
		runtime.Priority != "u=4, i" || runtime.MemoryPolicy != "physical-gib-rounded-v1" || runtime.FallbackTotalMemoryGB <= 0 ||
		strings.TrimSpace(runtime.UserAgent) == "" || !strings.Contains(runtime.UserAgent, "Claude/"+runtime.ClientVersion) ||
		!strings.Contains(runtime.UserAgent, "Chrome/"+runtime.ChromiumVersion) || !strings.Contains(runtime.UserAgent, "Electron/"+runtime.ElectronVersion) {
		return fmt.Errorf("claude desktop profile: telemetry renderer runtime does not match the captured sender")
	}
	sentry := telemetry.Sentry
	if sentry.Environment != "production" || sentry.Release != "Claude%401.40609.0" || len(sentry.PublicKey) != 32 || sentry.OrgID != "1158394" {
		return fmt.Errorf("claude desktop profile: telemetry Sentry propagation profile is invalid")
	}
	batch := telemetry.Batch
	if batch.FlushIntervalMS <= 0 || batch.MaxEvents <= 0 || batch.MaxBytes <= 0 || batch.MaxRetries < 0 ||
		batch.InitialBackoffMS <= 0 || batch.MaxBackoffMS < batch.InitialBackoffMS || batch.LeaseTimeoutMS <= 0 ||
		batch.MaxPendingEvents < batch.MaxEvents || batch.MaxDeadLetters < 0 || batch.JitterMinimum <= 0 ||
		batch.JitterMaximum < batch.JitterMinimum {
		return fmt.Errorf("claude desktop profile: telemetry batch policy is invalid")
	}
	for _, key := range []string{"product_surface", "desktop_variant", "deployment_mode", "config_source", "config_source_remote", "platform", "arch", "installer_variant", "renderer_surface", "backend_kind"} {
		if _, ok := telemetry.BaseMetadata[key]; !ok {
			return fmt.Errorf("claude desktop profile: telemetry base metadata omits %q", key)
		}
	}
	for _, fact := range []string{
		"session_initialized",
		"session_stopped",
		"session_visibility_changed",
		"session_idle_timeout_started",
		"request_started",
		"request_succeeded",
		"request_failed",
	} {
		event, ok := telemetry.Events[fact]
		if !ok || strings.TrimSpace(event.EventName) == "" {
			return fmt.Errorf("claude desktop profile: telemetry event mapping %q is missing", fact)
		}
	}
	seenUnsupported := make(map[string]struct{}, len(telemetry.Unsupported))
	for _, endpoint := range telemetry.Unsupported {
		role := strings.TrimSpace(endpoint.Role)
		if role == "" || endpoint.Status != "excluded-non-claude-telemetry" || strings.TrimSpace(endpoint.Reason) == "" {
			return fmt.Errorf("claude desktop profile: invalid unsupported telemetry endpoint declaration")
		}
		if _, exists := seenUnsupported[role]; exists {
			return fmt.Errorf("claude desktop profile: duplicate unsupported telemetry endpoint %q", role)
		}
		seenUnsupported[role] = struct{}{}
	}
	return nil
}

func validateTelemetryEvidenceProfile(evidence TelemetryEvidenceProfile, renderer TelemetryProfile, sdk SDKTelemetryProfile) error {
	if evidence.SchemaVersion != 1 {
		return fmt.Errorf("claude desktop profile: unsupported telemetry evidence schema version %d", evidence.SchemaVersion)
	}
	if !validSHA256(evidence.SourceManifestSHA256) {
		return fmt.Errorf("claude desktop profile: telemetry evidence source manifest digest is invalid")
	}
	firstCapturedAt, errFirst := time.Parse(time.RFC3339Nano, evidence.CaptureWindow.FirstCapturedAt)
	lastCapturedAt, errLast := time.Parse(time.RFC3339Nano, evidence.CaptureWindow.LastCapturedAt)
	if errFirst != nil || errLast != nil || lastCapturedAt.Before(firstCapturedAt) {
		return fmt.Errorf("claude desktop profile: telemetry evidence capture window is invalid")
	}
	corpus := evidence.Corpus
	if corpus.FlowCount <= 0 || corpus.HTTPScenarioCount <= 0 || corpus.EligibleScenarioCount < corpus.HTTPScenarioCount ||
		corpus.TelemetryBatchFlowCount <= 0 || corpus.TelemetryBatchFlowCount > corpus.FlowCount || corpus.EventCount <= 0 ||
		corpus.EventNameCount <= 0 || corpus.EventNameCount > corpus.EventCount {
		return fmt.Errorf("claude desktop profile: telemetry evidence corpus counts are invalid")
	}
	if len(evidence.Artifacts) == 0 {
		return fmt.Errorf("claude desktop profile: telemetry evidence artifacts are missing")
	}
	seenArtifacts := make(map[string]struct{}, len(evidence.Artifacts))
	for _, artifact := range evidence.Artifacts {
		name := strings.TrimSpace(artifact.Name)
		if name == "" || !validSHA256(artifact.SHA256) {
			return fmt.Errorf("claude desktop profile: telemetry evidence artifact is invalid")
		}
		if _, exists := seenArtifacts[name]; exists {
			return fmt.Errorf("claude desktop profile: duplicate telemetry evidence artifact %q", name)
		}
		seenArtifacts[name] = struct{}{}
		if name == "corpus-manifest" && !strings.EqualFold(artifact.SHA256, evidence.SourceManifestSHA256) {
			return fmt.Errorf("claude desktop profile: telemetry evidence manifest digest mismatch")
		}
	}
	for _, required := range []string{"corpus-manifest", "event-catalog", "telemetry-profile", "telemetry-coverage", "event-state-transitions", "event-state-closure"} {
		if _, exists := seenArtifacts[required]; !exists {
			return fmt.Errorf("claude desktop profile: telemetry evidence omits artifact %q", required)
		}
	}
	if (evidence.EmitterCoverage.SourceArtifact != "event-state-transitions" && evidence.EmitterCoverage.SourceArtifact != "observed-event-union") || len(evidence.EmitterCoverage.ObservableEventNames) == 0 {
		return fmt.Errorf("claude desktop profile: telemetry emitter coverage evidence is invalid")
	}
	seenEventNames := make(map[string]struct{}, len(evidence.EmitterCoverage.ObservableEventNames))
	previousEventName := ""
	for _, eventName := range evidence.EmitterCoverage.ObservableEventNames {
		eventName = strings.TrimSpace(eventName)
		if eventName == "" || previousEventName >= eventName {
			return fmt.Errorf("claude desktop profile: telemetry emitter coverage names must be unique and sorted")
		}
		seenEventNames[eventName] = struct{}{}
		previousEventName = eventName
	}
	previousPair := ""
	for _, event := range evidence.EmitterCoverage.ObservableEndpointEvents {
		key := event.EndpointRole + "\x00" + event.EventName
		if _, ok := seenEventNames[event.EventName]; !ok || event.EndpointRole == "" || key <= previousPair {
			return fmt.Errorf("claude desktop profile: endpoint event coverage must reference observed names and be unique and sorted")
		}
		previousPair = key
	}
	if errScope := validateTelemetryObservedScope(evidence); errScope != nil {
		return errScope
	}
	if len(evidence.ObservedEndpoints) == 0 {
		return fmt.Errorf("claude desktop profile: telemetry observed endpoints are missing")
	}
	wantStatus := map[string]string{
		renderer.EndpointRole:  "captured-delivery-enabled",
		sdk.EndpointRole:       "captured-delivery-enabled",
		"segment":              "captured-delivery-enabled",
		"datadog-logs":         "captured-delivery-enabled",
		"datadog-logs-browser": "captured-delivery-enabled",
		"datadog-rum":          "captured-delivery-enabled",
		"sentry":               "captured-delivery-enabled",
	}
	wantProtocol := map[string]string{
		renderer.EndpointRole:  renderer.Transport.Protocol,
		sdk.EndpointRole:       "http/1.1",
		"segment":              "http/2",
		"datadog-logs":         "http/1.1",
		"datadog-logs-browser": "http/1.1",
		"datadog-rum":          "http/2",
		"sentry":               "http/2",
	}
	wantClass := map[string]string{
		renderer.EndpointRole:  "renderer",
		sdk.EndpointRole:       "sdk",
		"segment":              "segment",
		"datadog-logs":         "datadog-logs",
		"datadog-logs-browser": "datadog-logs",
		"datadog-rum":          "datadog-rum",
		"sentry":               "sentry",
	}
	allowedBodyCoverage := map[string]struct{}{
		"json-complete": {},
		"json-partial":  {},
		"wire-only":     {},
	}
	seenEndpoints := make(map[string]struct{}, len(evidence.ObservedEndpoints))
	for _, endpoint := range evidence.ObservedEndpoints {
		role := strings.TrimSpace(endpoint.Role)
		wantEndpointStatus, roleKnown := wantStatus[role]
		_, bodyCoverageKnown := allowedBodyCoverage[endpoint.BodyCoverage]
		if !roleKnown || endpoint.TelemetryClass != wantClass[role] || endpoint.Status != wantEndpointStatus || endpoint.TransportProtocol != wantProtocol[role] || !bodyCoverageKnown ||
			endpoint.FlowCount <= 0 || endpoint.ScenarioCount <= 0 || endpoint.ScenarioCount > corpus.HTTPScenarioCount ||
			endpoint.JSONBodyFlowCount < 0 || endpoint.OpaqueBodyFlowCount < 0 || endpoint.MissingBodyFlowCount < 0 ||
			endpoint.JSONBodyFlowCount+endpoint.OpaqueBodyFlowCount+endpoint.MissingBodyFlowCount != endpoint.FlowCount ||
			endpoint.EventCount < 0 || endpoint.MaximumBatchEvents < 0 || endpoint.MaximumBatchBytes < 0 || strings.TrimSpace(endpoint.Reason) == "" {
			return fmt.Errorf("claude desktop profile: invalid observed telemetry endpoint declaration for %q", role)
		}
		if strings.Contains(endpoint.Reason, "://") || strings.Contains(strings.ToLower(endpoint.Reason), "bearer ") ||
			strings.Contains(strings.ToLower(endpoint.Reason), "writekey") || strings.Contains(strings.ToLower(endpoint.Reason), "cookie=") {
			return fmt.Errorf("claude desktop profile: observed telemetry endpoint %q contains unsafe evidence text", role)
		}
		if _, exists := seenEndpoints[role]; exists {
			return fmt.Errorf("claude desktop profile: duplicate observed telemetry endpoint %q", role)
		}
		seenEndpoints[role] = struct{}{}
	}
	for role := range wantStatus {
		if _, exists := seenEndpoints[role]; !exists {
			return fmt.Errorf("claude desktop profile: telemetry evidence omits observed endpoint %q", role)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	_, errDecode := hex.DecodeString(value)
	return errDecode == nil
}

func validateSDKTelemetryProfile(telemetry SDKTelemetryProfile, transports TransportProfiles) error {
	if telemetry.SchemaVersion != 1 {
		return fmt.Errorf("claude desktop profile: unsupported SDK telemetry schema version %d", telemetry.SchemaVersion)
	}
	if strings.TrimSpace(telemetry.Source) == "" || telemetry.EndpointRole != "sdk-event-logging" {
		return fmt.Errorf("claude desktop profile: SDK telemetry source and endpoint_role are invalid")
	}
	endpoint, errParse := url.Parse(strings.TrimSpace(telemetry.Endpoint))
	if errParse != nil || endpoint.Scheme != "https" || !strings.EqualFold(endpoint.Hostname(), "api.anthropic.com") || endpoint.Path != "/api/event_logging/v2/batch" || endpoint.RawQuery != "" {
		return fmt.Errorf("claude desktop profile: SDK telemetry endpoint must be the captured Anthropic event endpoint")
	}
	if telemetry.EventType != "ClaudeCodeInternalEvent" {
		return fmt.Errorf("claude desktop profile: unsupported SDK telemetry event type %q", telemetry.EventType)
	}
	transportName := strings.TrimSpace(telemetry.TransportProfile)
	transport, ok := transports.Profiles[transportName]
	if !ok || transport.Protocol != "http/1.1" {
		return fmt.Errorf("claude desktop profile: SDK telemetry transport profile %q is invalid", transportName)
	}
	if telemetry.AuthPolicy != "oauth-bearer" {
		return fmt.Errorf("claude desktop profile: SDK telemetry auth policy %q is invalid", telemetry.AuthPolicy)
	}
	wantOrder := []string{"Accept", "Content-Type", "User-Agent", "x-service-name", "Authorization", "anthropic-beta", "Content-Length", "Accept-Encoding", "Host", "Connection"}
	if !equalStringSlice(telemetry.HeaderOrder, wantOrder) {
		return fmt.Errorf("claude desktop profile: SDK telemetry header order does not match the captured sender")
	}
	headers := make(map[string]string, len(telemetry.Headers))
	for _, header := range telemetry.Headers {
		name := strings.ToLower(strings.TrimSpace(header.Name))
		value := strings.TrimSpace(header.Value)
		if name == "" || value == "" {
			return fmt.Errorf("claude desktop profile: SDK telemetry contains an incomplete header")
		}
		if name == "authorization" || name == "cookie" || name == "set-cookie" || name == "content-length" || name == "host" {
			return fmt.Errorf("claude desktop profile: SDK telemetry must not embed dynamic header %q", header.Name)
		}
		if _, exists := headers[name]; exists {
			return fmt.Errorf("claude desktop profile: SDK telemetry repeats header %q", header.Name)
		}
		headers[name] = value
	}
	wantHeaders := map[string]string{
		"accept":          "application/json, text/plain, */*",
		"content-type":    "application/json",
		"user-agent":      "claude-code/2.1.247",
		"x-service-name":  "claude-code",
		"anthropic-beta":  "oauth-2025-04-20",
		"accept-encoding": "gzip, compress, deflate, br",
		"connection":      "close",
	}
	if len(headers) != len(wantHeaders) {
		return fmt.Errorf("claude desktop profile: SDK telemetry headers are incomplete")
	}
	for name, want := range wantHeaders {
		if headers[name] != want {
			return fmt.Errorf("claude desktop profile: SDK telemetry header %q does not match the captured sender", name)
		}
	}
	if errBatch := validateTelemetryBatchProfile(telemetry.Batch); errBatch != nil {
		return fmt.Errorf("claude desktop profile: SDK telemetry %w", errBatch)
	}
	for _, key := range []string{"platform", "node_version", "terminal", "package_managers", "runtimes", "is_running_with_bun", "is_ci", "is_claude_ai_auth", "version", "arch", "deployment_environment", "version_base", "build_time", "platform_raw", "shell"} {
		if _, ok := telemetry.Environment[key]; !ok {
			return fmt.Errorf("claude desktop profile: SDK telemetry environment omits %q", key)
		}
	}
	if telemetry.Process.ConstrainedMemory == 0 {
		return fmt.Errorf("claude desktop profile: SDK telemetry process constrained_memory is missing")
	}
	wantEvents := map[string]string{
		"cache_breakpoints":      "tengu_api_cache_breakpoints",
		"success":                "tengu_api_success",
		"retry":                  "tengu_api_retry",
		"runtime_started":        "tengu_started",
		"runtime_initialized":    "tengu_init",
		"sdk_init_handshake":     "tengu_sdk_init_handshake",
		"shutdown_pending_state": "tengu_shutdown_pending_state",
	}
	for fact, wantName := range wantEvents {
		event, okEvent := telemetry.Events[fact]
		if !okEvent || event.EventName != wantName {
			return fmt.Errorf("claude desktop profile: SDK telemetry event mapping %q is invalid", fact)
		}
	}
	if errInput := validateSDKInputProfile(telemetry); errInput != nil {
		return errInput
	}
	return nil
}

func validateAuxiliaryTelemetryProfiles(profiles AuxiliaryTelemetryProfiles, transports TransportProfiles) error {
	type expectation struct {
		class           string
		role            string
		host            string
		path            string
		format          string
		protocol        string
		transport       string
		userAgentPolicy string
		materials       map[string]string
		headers         map[string]string
		requiredEvents  map[string]string
	}
	expectations := []struct {
		profile AuxiliaryTelemetryProfile
		want    expectation
	}{
		{profiles.Segment, expectation{
			class: "segment",
			role:  "segment", host: "a-api.anthropic.com", path: "/v1/b",
			format: "segment-batch-json", protocol: "http/2", transport: "renderer-chromium-h2", userAgentPolicy: "profile",
			materials: map[string]string{"write_key": "segment_write_key"},
			headers: map[string]string{
				"accept": "*/*", "content-type": "text/plain", "origin": "https://claude.ai", "referer": "https://claude.ai/",
				"user-agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.40609.0 Chrome/148.0.7778.280 Electron/42.10.0 Safari/537.36 MSIX",
				"sec-ch-ua-platform": `"Windows"`, "sec-ch-ua": `"Not/A)Brand";v="99", "Chromium";v="148"`, "sec-ch-ua-mobile": "?0",
				"sec-fetch-site": "cross-site", "sec-fetch-mode": "cors", "sec-fetch-dest": "empty",
				"accept-encoding": "gzip, deflate, br, zstd", "accept-language": "en-US", "priority": "u=1, i",
			},
			requiredEvents: map[string]string{"runtime_started": "identify", "request_started": "claudeai.code.message.submitted", "first_byte": "claudeai.code.session.ttft"},
		}},
		{profiles.DatadogLogs, expectation{
			class: "datadog-logs",
			role:  "datadog-logs", host: "http-intake.logs.us5.datadoghq.com", path: "/api/v2/logs",
			format: "datadog-logs-json", protocol: "http/1.1", transport: "anthropic-node-http1", userAgentPolicy: "profile",
			materials: map[string]string{"api_key": "datadog_logs_api_key"},
			headers: map[string]string{
				"accept": "application/json, text/plain, */*", "content-type": "application/json", "user-agent": "axios/1.15.2",
				"accept-encoding": "gzip, compress, deflate, br", "connection": "close",
			},
			requiredEvents: map[string]string{"runtime_started": "tengu_started", "runtime_initialized": "tengu_init", "sdk_init_handshake": "tengu_sdk_init_handshake", "shutdown_pending_state": "tengu_shutdown_pending_state", "success": "tengu_api_success"},
		}},
		{profiles.DatadogLogsBrowser, expectation{
			class: "datadog-logs",
			role:  "datadog-logs-browser", host: "browser-intake-us5-datadoghq.com", path: "/api/v2/logs",
			format: "datadog-browser-logs-json", protocol: "http/1.1", transport: "anthropic-node-http1", userAgentPolicy: "profile",
			materials: map[string]string{"api_key": "datadog_logs_api_key"},
			headers: map[string]string{
				"accept": "application/json, text/plain, */*", "content-type": "application/json", "user-agent": "axios/1.15.2",
				"accept-encoding": "gzip, compress, deflate, br", "connection": "close",
			},
			requiredEvents: map[string]string{"retry": "error"},
		}},
		{profiles.DatadogRUM, expectation{
			class: "datadog-rum",
			role:  "datadog-rum", host: "browser-intake-us5-datadoghq.com", path: "/api/v2/rum",
			format: "datadog-rum-ndjson", protocol: "http/2", transport: "renderer-chromium-h2", userAgentPolicy: "profile",
			materials: map[string]string{"client_token": "datadog_rum_client_token", "application_id": "datadog_rum_application_id"},
			headers: map[string]string{
				"accept": "*/*", "content-type": "text/plain;charset=UTF-8", "origin": "https://claude.ai", "referer": "https://claude.ai/",
				"user-agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.40609.0 Chrome/148.0.7778.280 Electron/42.10.0 Safari/537.36 MSIX",
				"sec-ch-ua-platform": `"Windows"`, "sec-ch-ua": `"Not/A)Brand";v="99", "Chromium";v="148"`, "sec-ch-ua-mobile": "?0",
				"sec-fetch-site": "cross-site", "sec-fetch-mode": "no-cors", "sec-fetch-dest": "empty", "sec-fetch-storage-access": "active",
				"accept-encoding": "gzip, deflate, br, zstd", "accept-language": "en-US", "priority": "u=4, i",
			},
			requiredEvents: map[string]string{
				"session_initialized": "view", "request_started": "action", "first_byte": "action", "request_failed": "error", "session_stopped": "view",
			},
		}},
		{profiles.Sentry, expectation{
			class: "sentry",
			role:  "sentry", host: "o1158394.ingest.us.sentry.io", path: "/api/4507368973008896/envelope/",
			format: "sentry-envelope", protocol: "http/2", transport: "renderer-chromium-h2", userAgentPolicy: "profile",
			materials: map[string]string{"public_key": "sentry_public_key"},
			headers: map[string]string{
				"content-type": "application/x-sentry-envelope", "user-agent": "sentry.javascript.electron/7.12.0",
				"sec-fetch-site": "none", "sec-fetch-mode": "no-cors", "sec-fetch-dest": "empty",
				"accept-encoding": "gzip, deflate, br, zstd", "accept-language": "en-US", "priority": "u=4, i",
			},
			requiredEvents: map[string]string{"runtime_started": "session", "runtime_stopped": "session", "request_failed": "event"},
		}},
	}

	seenRoles := make(map[string]struct{}, len(expectations))
	for _, item := range expectations {
		profile, want := item.profile, item.want
		if profile.SchemaVersion != 1 || strings.TrimSpace(profile.Source) == "" || !profile.Required || profile.TelemetryClass != want.class || profile.EndpointRole != want.role ||
			profile.BodyFormat != want.format || profile.Protocol != want.protocol || profile.TransportProfile != want.transport ||
			profile.UserAgentPolicy != want.userAgentPolicy || profile.AuthPolicy != "runtime-material" {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry profile %q is invalid", want.role)
		}
		if _, exists := seenRoles[profile.EndpointRole]; exists {
			return fmt.Errorf("claude desktop profile: duplicate auxiliary telemetry role %q", profile.EndpointRole)
		}
		seenRoles[profile.EndpointRole] = struct{}{}
		endpoint, errParse := url.Parse(strings.TrimSpace(profile.Endpoint))
		if errParse != nil || endpoint.Scheme != "https" || !strings.EqualFold(endpoint.Hostname(), want.host) || endpoint.Path != want.path || endpoint.RawQuery != "" || endpoint.User != nil {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry endpoint %q is invalid", want.role)
		}
		if !equalStringMap(profile.RuntimeMaterials, want.materials) {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry runtime materials for %q are invalid", want.role)
		}
		headers := make(map[string]string, len(profile.Headers))
		for _, header := range profile.Headers {
			name := strings.ToLower(strings.TrimSpace(header.Name))
			value := strings.TrimSpace(header.Value)
			if name == "" || value == "" || name == "authorization" || name == "cookie" || name == "set-cookie" || name == "content-length" || name == "host" || name == "dd-api-key" {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q contains an invalid static header", want.role)
			}
			if _, exists := headers[name]; exists {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q repeats header %q", want.role, header.Name)
			}
			headers[name] = value
		}
		if !equalStringMap(headers, want.headers) {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry headers for %q do not match the captured sender", want.role)
		}
		if len(profile.HeaderOrder) == 0 {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry %q omits header order", want.role)
		}
		seenHeaders := make(map[string]struct{}, len(profile.HeaderOrder))
		for _, header := range profile.HeaderOrder {
			key := strings.ToLower(strings.TrimSpace(header))
			if key == "" {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q contains an empty header order item", want.role)
			}
			if _, exists := seenHeaders[key]; exists {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q repeats header order item %q", want.role, header)
			}
			seenHeaders[key] = struct{}{}
		}
		for header := range want.headers {
			if _, exists := seenHeaders[header]; !exists {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q header order omits %q", want.role, header)
			}
		}
		seenOptionalHeaders := make(map[string]struct{}, len(profile.OptionalHeaders))
		for _, header := range profile.OptionalHeaders {
			key := strings.ToLower(strings.TrimSpace(header))
			if key == "" {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q contains an empty optional header", want.role)
			}
			if _, exists := seenHeaders[key]; !exists {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q optional header %q is absent from header order", want.role, header)
			}
			if _, exists := seenOptionalHeaders[key]; exists {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q repeats optional header %q", want.role, header)
			}
			seenOptionalHeaders[key] = struct{}{}
		}
		if want.role == "sentry" {
			if len(seenOptionalHeaders) != 1 {
				return fmt.Errorf("claude desktop profile: Sentry optional headers do not match the captured sender")
			}
			if _, exists := seenOptionalHeaders["content-encoding"]; !exists {
				return fmt.Errorf("claude desktop profile: Sentry content-encoding must be optional")
			}
		} else if len(seenOptionalHeaders) != 0 {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry %q has unexpected optional headers", want.role)
		}
		if want.protocol == "http/1.1" {
			transport, exists := transports.Profiles[want.transport]
			if !exists || transport.Protocol != want.protocol {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry transport for %q is invalid", want.role)
			}
		}
		if errBatch := validateTelemetryBatchProfile(profile.Batch); errBatch != nil {
			return fmt.Errorf("claude desktop profile: auxiliary telemetry %q %w", want.role, errBatch)
		}
		for fact, eventName := range want.requiredEvents {
			event, exists := profile.Events[fact]
			if !exists || event.EventName != eventName {
				return fmt.Errorf("claude desktop profile: auxiliary telemetry %q event mapping %q is invalid", want.role, fact)
			}
		}
	}
	return nil
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func validateControlPlaneProfile(control ControlPlaneProfile, transports TransportProfiles) error {
	if control.SchemaVersion != 1 {
		return fmt.Errorf("claude desktop profile: unsupported control-plane schema version %d", control.SchemaVersion)
	}
	if strings.TrimSpace(control.Source) == "" || !control.Required {
		return fmt.Errorf("claude desktop profile: control-plane source and required policy are invalid")
	}
	baseURL, errBaseURL := url.Parse(strings.TrimSpace(control.BaseURL))
	if errBaseURL != nil || baseURL.Scheme != "https" || !strings.EqualFold(baseURL.Hostname(), "api.anthropic.com") || baseURL.Path != "" || baseURL.RawQuery != "" {
		return fmt.Errorf("claude desktop profile: control-plane base_url must be the captured Anthropic API origin")
	}
	if control.HeartbeatIntervalSeconds != 20 || control.WorkerJWTRefreshLeadSeconds <= 0 ||
		control.WorkerJWTRefreshLeadSeconds >= control.HeartbeatIntervalSeconds || control.StreamReconnectBackoffMillis <= 0 {
		return fmt.Errorf("claude desktop profile: control-plane timing policy is invalid")
	}
	if control.TeardownArchiveBudgetMillis < 500 || control.TeardownArchiveBudgetMillis > 2000 {
		return fmt.Errorf("claude desktop profile: control-plane teardown archive budget is invalid")
	}
	worker := control.Worker
	if strings.TrimSpace(worker.APIKeySource) == "" || strings.TrimSpace(worker.ClaudeCodeVersion) == "" ||
		strings.TrimSpace(worker.OutputStyle) == "" || strings.TrimSpace(worker.FastModeState) == "" ||
		strings.TrimSpace(worker.FastModeDisabledReason) == "" || len(worker.SlashCommands) == 0 ||
		len(worker.Agents) == 0 || len(worker.Skills) == 0 {
		return fmt.Errorf("claude desktop profile: control-plane worker init inventory is incomplete")
	}
	type endpointSpec struct {
		method     string
		path       string
		authPolicy string
	}
	want := map[string]endpointSpec{
		ControlEndpointCreateSession:        {method: http.MethodPost, path: "/v1/code/sessions", authPolicy: "oauth-bearer"},
		ControlEndpointBridge:               {method: http.MethodPost, path: "/v1/code/sessions/{session_id}/bridge", authPolicy: "oauth-bearer"},
		ControlEndpointWorkerStream:         {method: http.MethodGet, path: "/v1/code/sessions/{session_id}/worker/events/stream", authPolicy: "worker-jwt"},
		ControlEndpointWorkerRead:           {method: http.MethodGet, path: "/v1/code/sessions/{session_id}/worker", authPolicy: "worker-jwt"},
		ControlEndpointWorkerInternalEvents: {method: http.MethodGet, path: "/v1/code/sessions/{session_id}/worker/internal-events", authPolicy: "worker-jwt"},
		ControlEndpointWorkerUpdate:         {method: http.MethodPut, path: "/v1/code/sessions/{session_id}/worker", authPolicy: "worker-jwt"},
		ControlEndpointWorkerEvents:         {method: http.MethodPost, path: "/v1/code/sessions/{session_id}/worker/events", authPolicy: "worker-jwt"},
		ControlEndpointWorkerDelivery:       {method: http.MethodPost, path: "/v1/code/sessions/{session_id}/worker/events/delivery", authPolicy: "worker-jwt"},
		ControlEndpointWorkerHeartbeat:      {method: http.MethodPost, path: "/v1/code/sessions/{session_id}/worker/heartbeat", authPolicy: "worker-jwt"},
		ControlEndpointSessionRead:          {method: http.MethodGet, path: "/v1/sessions/{session_id}", authPolicy: "oauth-bearer"},
		ControlEndpointSessionUpdate:        {method: http.MethodPatch, path: "/v1/sessions/{session_id}", authPolicy: "oauth-bearer"},
		ControlEndpointSessionArchive:       {method: http.MethodPost, path: "/v1/sessions/{session_id}/archive", authPolicy: "oauth-bearer"},
		ControlEndpointSessionUnarchive:     {method: http.MethodPost, path: "/v1/sessions/{session_id}/unarchive", authPolicy: "oauth-bearer"},
	}
	if len(control.Endpoints) != len(want) {
		return fmt.Errorf("claude desktop profile: control-plane endpoint set is incomplete")
	}
	for name, spec := range want {
		endpoint, okEndpoint := control.Endpoints[name]
		if !okEndpoint || endpoint.Method != spec.method || endpoint.Path != spec.path || endpoint.AuthPolicy != spec.authPolicy {
			return fmt.Errorf("claude desktop profile: control-plane endpoint %q does not match the captured request class", name)
		}
		role := strings.TrimSpace(endpoint.EndpointRole)
		if role == "" || strings.TrimSpace(transports.EndpointProfiles[role]) == "" {
			return fmt.Errorf("claude desktop profile: control-plane endpoint %q has no transport binding", name)
		}
		if len(endpoint.HeaderOrder) == 0 {
			return fmt.Errorf("claude desktop profile: control-plane endpoint %q has no header order", name)
		}
		ordered := make(map[string]struct{}, len(endpoint.HeaderOrder))
		for _, header := range endpoint.HeaderOrder {
			key := strings.ToLower(strings.TrimSpace(header))
			if key == "" {
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q contains an empty header name", name)
			}
			if _, exists := ordered[key]; exists {
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q repeats header %q", name, header)
			}
			ordered[key] = struct{}{}
		}
		for _, required := range []string{"accept", "authorization", "user-agent", "anthropic-client-platform", "anthropic-version", "host", "accept-encoding", "connection"} {
			if _, exists := ordered[required]; !exists {
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q omits header %q", name, required)
			}
		}
		if spec.method == http.MethodPost || spec.method == http.MethodPut || spec.method == http.MethodPatch {
			for _, required := range []string{"content-type", "content-length"} {
				if _, exists := ordered[required]; !exists {
					return fmt.Errorf("claude desktop profile: control-plane endpoint %q omits header %q", name, required)
				}
			}
		}
		staticHeaders := make(map[string]string, len(endpoint.Headers))
		for _, header := range endpoint.Headers {
			key := strings.ToLower(strings.TrimSpace(header.Name))
			value := strings.TrimSpace(header.Value)
			if key == "" || value == "" {
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q contains an incomplete static header", name)
			}
			switch key {
			case "authorization", "x-organization-uuid", "host", "content-length", "cookie", "set-cookie":
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q embeds dynamic header %q", name, header.Name)
			}
			if _, exists := ordered[key]; !exists {
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q static header %q is absent from header_order", name, header.Name)
			}
			if _, exists := staticHeaders[key]; exists {
				return fmt.Errorf("claude desktop profile: control-plane endpoint %q repeats static header %q", name, header.Name)
			}
			staticHeaders[key] = value
		}
		if staticHeaders["user-agent"] == "" || staticHeaders["anthropic-client-platform"] != "claude_code_cli" || staticHeaders["anthropic-version"] != "2023-06-01" {
			return fmt.Errorf("claude desktop profile: control-plane endpoint %q static identity headers are invalid", name)
		}
	}
	return nil
}

func validateStartupProfile(startup StartupProfile, transports TransportProfiles) error {
	if startup.SchemaVersion != 1 {
		return fmt.Errorf("claude desktop profile: unsupported startup schema version %d", startup.SchemaVersion)
	}
	if strings.TrimSpace(startup.Source) == "" || !startup.Required || startup.RequestTimeoutMS <= 0 || startup.MaxResponseBytes <= 0 {
		return fmt.Errorf("claude desktop profile: startup declaration is incomplete")
	}
	if strings.TrimSpace(startup.WebClientBuild) == "" || len(strings.TrimSpace(startup.WebClientSHA)) != 40 {
		return fmt.Errorf("claude desktop profile: startup web client identity is invalid")
	}
	if len(startup.HeaderProfiles) == 0 || len(startup.Endpoints) == 0 {
		return fmt.Errorf("claude desktop profile: startup headers or endpoints are missing")
	}
	for name, headerProfile := range startup.HeaderProfiles {
		name = strings.TrimSpace(name)
		if name == "" || len(headerProfile.HeaderOrder) == 0 {
			return fmt.Errorf("claude desktop profile: startup header profile is invalid")
		}
		switch headerProfile.Protocol {
		case "http/1.1":
			transport, ok := transports.Profiles[strings.TrimSpace(headerProfile.TransportProfile)]
			if !ok || transport.Protocol != headerProfile.Protocol {
				return fmt.Errorf("claude desktop profile: startup header profile %q has an invalid transport", name)
			}
		case "http/2":
			if headerProfile.TransportProfile != "renderer-h2" {
				return fmt.Errorf("claude desktop profile: startup header profile %q has an invalid renderer transport", name)
			}
		default:
			return fmt.Errorf("claude desktop profile: startup header profile %q has unsupported protocol %q", name, headerProfile.Protocol)
		}
		ordered := make(map[string]struct{}, len(headerProfile.HeaderOrder))
		for _, wireName := range headerProfile.HeaderOrder {
			key := strings.ToLower(strings.TrimSpace(wireName))
			if key == "" {
				return fmt.Errorf("claude desktop profile: startup header profile %q contains an empty header", name)
			}
			if _, exists := ordered[key]; exists {
				return fmt.Errorf("claude desktop profile: startup header profile %q repeats header %q", name, wireName)
			}
			ordered[key] = struct{}{}
		}
		static := make(map[string]struct{}, len(headerProfile.Headers))
		for _, header := range headerProfile.Headers {
			key := strings.ToLower(strings.TrimSpace(header.Name))
			if key == "" || strings.TrimSpace(header.Value) == "" {
				return fmt.Errorf("claude desktop profile: startup header profile %q contains an incomplete static header", name)
			}
			switch key {
			case "authorization", "cookie", "host", "content-length", "x-organization-uuid", "sentry-trace", "baggage", "traceparent", "x-datadog-parent-id", "x-datadog-trace-id", "x-activity-session-id", "anthropic-anonymous-id", "anthropic-device-id", "if-none-match":
				return fmt.Errorf("claude desktop profile: startup header profile %q embeds dynamic header %q", name, header.Name)
			}
			if _, exists := ordered[key]; !exists {
				return fmt.Errorf("claude desktop profile: startup header profile %q static header %q is absent from header_order", name, header.Name)
			}
			if _, exists := static[key]; exists {
				return fmt.Errorf("claude desktop profile: startup header profile %q repeats static header %q", name, header.Name)
			}
			static[key] = struct{}{}
		}
	}

	wantRoles := map[string]struct{}{
		"startup-bootstrap": {}, "startup-grove": {}, "startup-penguin": {}, "startup-account-settings": {},
		"startup-ultrareview-quota": {}, "startup-sdk-eval": {}, "startup-update-head": {}, "startup-update-get": {},
		"startup-desktop-features": {}, "startup-organization": {}, "startup-remote-devices": {}, "startup-dxt-blocklist": {},
		"startup-marketplaces": {}, "startup-plugins": {}, "startup-projects": {}, "startup-usage": {},
		"startup-code-sessions": {}, "startup-code-sessions-watch": {}, "startup-environments": {},
	}
	seenRoles := make(map[string]struct{}, len(startup.Endpoints))
	for _, endpoint := range startup.Endpoints {
		role := strings.TrimSpace(endpoint.EndpointRole)
		if _, wanted := wantRoles[role]; !wanted {
			return fmt.Errorf("claude desktop profile: unexpected startup endpoint role %q", role)
		}
		if _, duplicate := seenRoles[role]; duplicate {
			return fmt.Errorf("claude desktop profile: duplicate startup endpoint role %q", role)
		}
		seenRoles[role] = struct{}{}
		if _, ok := startup.HeaderProfiles[strings.TrimSpace(endpoint.HeaderProfile)]; !ok {
			return fmt.Errorf("claude desktop profile: startup endpoint %q references an unknown header profile", role)
		}
		method := strings.ToUpper(strings.TrimSpace(endpoint.Method))
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodPost {
			return fmt.Errorf("claude desktop profile: startup endpoint %q has unsupported method %q", role, endpoint.Method)
		}
		parsed, errParse := url.Parse(strings.TrimSpace(endpoint.Endpoint))
		if errParse != nil || parsed.Scheme != "https" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || parsed.Path == "" ||
			(!strings.EqualFold(parsed.Hostname(), "api.anthropic.com") && !strings.EqualFold(parsed.Hostname(), "claude.ai")) {
			return fmt.Errorf("claude desktop profile: startup endpoint %q URL is invalid", role)
		}
		switch endpoint.AuthPolicy {
		case "oauth-bearer", "session-cookie", "none":
		default:
			return fmt.Errorf("claude desktop profile: startup endpoint %q has unsupported auth policy %q", role, endpoint.AuthPolicy)
		}
		if endpoint.ResponseMode != "bounded" && endpoint.ResponseMode != "stream" {
			return fmt.Errorf("claude desktop profile: startup endpoint %q has unsupported response mode %q", role, endpoint.ResponseMode)
		}
		for _, query := range endpoint.Query {
			if strings.TrimSpace(query.Name) == "" || strings.TrimSpace(query.Value) == "" || strings.ContainsAny(query.Name, "&=") {
				return fmt.Errorf("claude desktop profile: startup endpoint %q contains an invalid query parameter", role)
			}
		}
	}
	if len(seenRoles) != len(wantRoles) {
		return fmt.Errorf("claude desktop profile: startup endpoint set is incomplete")
	}
	return nil
}

func validateTelemetryBatchProfile(batch TelemetryBatchProfile) error {
	if batch.FlushIntervalMS <= 0 || batch.MaxEvents <= 0 || batch.MaxBytes <= 0 || batch.MaxRetries < 0 ||
		batch.InitialBackoffMS <= 0 || batch.MaxBackoffMS < batch.InitialBackoffMS || batch.LeaseTimeoutMS <= 0 ||
		batch.MaxPendingEvents < batch.MaxEvents || batch.MaxDeadLetters < 0 || batch.JitterMinimum <= 0 ||
		batch.JitterMaximum < batch.JitterMinimum {
		return fmt.Errorf("batch policy is invalid")
	}
	return nil
}

func equalStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalHTTP2Settings(left, right []HTTP2Setting) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TransportForEndpointRole returns an isolated captured transport for a
// Desktop background endpoint. Named endpoint profiles cover auxiliary SDK
// calls such as bootstrap; telemetry retains its sender-specific overrides.
func (b *Bundle) TransportForEndpointRole(role string) (TransportProfile, error) {
	if b == nil {
		return TransportProfile{}, fmt.Errorf("claude desktop profile: bundle is nil")
	}
	if role == b.Telemetry.EndpointRole {
		return TransportProfile{
			Protocol:          b.Telemetry.Transport.Protocol,
			ClientHelloPreset: b.Telemetry.Transport.ClientHelloPreset,
			HeaderOrder:       append([]string(nil), b.Telemetry.Transport.HeaderOrder...),
			HTTP2Settings:     append([]HTTP2Setting(nil), b.Telemetry.Transport.HTTP2Settings...),
			ConnectionWindow:  b.Telemetry.Transport.ConnectionWindow,
		}, nil
	}
	for _, auxiliary := range b.AuxiliaryTelemetry.All() {
		if role != auxiliary.EndpointRole {
			continue
		}
		if auxiliary.Protocol == "http/2" {
			return TransportProfile{
				Protocol:          b.Telemetry.Transport.Protocol,
				ClientHelloPreset: b.Telemetry.Transport.ClientHelloPreset,
				HeaderOrder:       append([]string(nil), auxiliary.HeaderOrder...),
				OptionalHeaders:   append([]string(nil), auxiliary.OptionalHeaders...),
				HTTP2Settings:     append([]HTTP2Setting(nil), b.Telemetry.Transport.HTTP2Settings...),
				ConnectionWindow:  b.Telemetry.Transport.ConnectionWindow,
			}, nil
		}
		transport, ok := b.Transport.Profiles[auxiliary.TransportProfile]
		if !ok {
			return TransportProfile{}, fmt.Errorf("unknown transport profile %q for endpoint role %q", auxiliary.TransportProfile, role)
		}
		transport.HeaderOrder = append([]string(nil), auxiliary.HeaderOrder...)
		transport.OptionalHeaders = nil
		return transport, nil
	}
	for _, endpoint := range b.Startup.Endpoints {
		if role != endpoint.EndpointRole {
			continue
		}
		headers, okHeaders := b.Startup.HeaderProfiles[endpoint.HeaderProfile]
		if !okHeaders {
			return TransportProfile{}, fmt.Errorf("startup endpoint role %q references an unknown header profile", role)
		}
		if headers.Protocol == "http/2" {
			return TransportProfile{
				Protocol:          b.Telemetry.Transport.Protocol,
				ClientHelloPreset: b.Telemetry.Transport.ClientHelloPreset,
				HeaderOrder:       append([]string(nil), headers.HeaderOrder...),
				HTTP2Settings:     append([]HTTP2Setting(nil), b.Telemetry.Transport.HTTP2Settings...),
				ConnectionWindow:  b.Telemetry.Transport.ConnectionWindow,
			}, nil
		}
		transport, okTransport := b.Transport.Profiles[headers.TransportProfile]
		if !okTransport {
			return TransportProfile{}, fmt.Errorf("startup endpoint role %q has no transport", role)
		}
		transport.HeaderOrder = append([]string(nil), headers.HeaderOrder...)
		transport.OptionalHeaders = nil
		return transport, nil
	}
	if name := strings.TrimSpace(b.Transport.EndpointProfiles[role]); name != "" {
		transport, ok := b.Transport.Profiles[name]
		if !ok {
			return TransportProfile{}, fmt.Errorf("unknown transport profile %q for endpoint role %q", name, role)
		}
		for _, endpoint := range b.ControlPlane.Endpoints {
			if endpoint.EndpointRole == role {
				transport.HeaderOrder = append([]string(nil), endpoint.HeaderOrder...)
				transport.OptionalHeaders = nil
				return transport, nil
			}
		}
		transport.HeaderOrder = append([]string(nil), transport.HeaderOrder...)
		transport.OptionalHeaders = append([]string(nil), transport.OptionalHeaders...)
		return transport, nil
	}
	if role != b.SDKTelemetry.EndpointRole {
		return TransportProfile{}, fmt.Errorf("no transport profile for endpoint role %q", role)
	}
	transport, ok := b.Transport.Profiles[b.SDKTelemetry.TransportProfile]
	if !ok {
		return TransportProfile{}, fmt.Errorf("unknown transport profile %q", b.SDKTelemetry.TransportProfile)
	}
	transport.HeaderOrder = append([]string(nil), b.SDKTelemetry.HeaderOrder...)
	transport.OptionalHeaders = nil
	return transport, nil
}

func (b *Bundle) TransportForRole(role RequestRole) (TransportProfile, error) {
	if b == nil {
		return TransportProfile{}, fmt.Errorf("claude desktop profile: bundle is nil")
	}
	name := strings.TrimSpace(b.Transport.RoleProfiles[role])
	if name == "" {
		return TransportProfile{}, fmt.Errorf("no transport profile for role %q", role)
	}
	transport, ok := b.Transport.Profiles[name]
	if !ok {
		return TransportProfile{}, fmt.Errorf("transport profile %q for role %q does not exist", name, role)
	}
	transport.HeaderOrder = append([]string(nil), transport.HeaderOrder...)
	transport.OptionalHeaders = append([]string(nil), transport.OptionalHeaders...)
	return transport, nil
}

func (b *Bundle) BodyForVariant(variant RequestVariant) (BodyProfile, error) {
	if b == nil {
		return BodyProfile{}, fmt.Errorf("claude desktop profile: bundle is nil")
	}
	requestProfile, errProfile := b.RequestProfileForVariant(variant)
	if errProfile != nil {
		return BodyProfile{}, errProfile
	}
	return bodyForVariant(requestProfile.Body, variant)
}

func bodyForVariant(profiles BodyProfiles, variant RequestVariant) (BodyProfile, error) {
	role := variant.Key.Role
	model := normalizeModel(variant.Key.Model)
	name := strings.TrimSpace(profiles.Bindings[string(role)+"|"+model])
	if name == "" {
		name = strings.TrimSpace(profiles.Bindings[string(role)+"|*"])
	}
	if name == "" {
		return BodyProfile{}, fmt.Errorf("no body profile for role=%q model=%q", role, model)
	}
	body, ok := profiles.Profiles[name]
	if !ok {
		return BodyProfile{}, fmt.Errorf("body profile %q for role=%q model=%q does not exist", name, role, model)
	}
	return body, nil
}

// RequestProfileForVariant returns the request identity and artifacts that own
// a resolved variant. Historical variants read the bundle's immutable request
// surface; current variants read their request-only overlay.
func (b *Bundle) RequestProfileForVariant(variant RequestVariant) (RequestProfile, error) {
	if b == nil {
		return RequestProfile{}, fmt.Errorf("claude desktop profile: bundle is nil")
	}
	profileID := strings.TrimSpace(variant.requestProfileID)
	if profileID == "" {
		return RequestProfile{
			SchemaVersion:   SupportedRequestProfileSchema,
			ProfileID:       b.ProfileID,
			DesktopVersion:  b.DesktopVersion,
			CodeVersion:     b.CodeVersion,
			AgentSDKVersion: b.AgentSDKVersion,
			Software:        b.Software,
			Body:            b.Body,
			Environment:     b.Environment,
			Artifacts:       b.Artifacts,
			Variants:        b.Variants,
		}, nil
	}
	if b.requestProfileIndexByID == nil {
		if errValidate := b.Validate(); errValidate != nil {
			return RequestProfile{}, errValidate
		}
	}
	index, ok := b.requestProfileIndexByID[profileID]
	if !ok || index < 0 || index >= len(b.RequestProfiles) {
		return RequestProfile{}, fmt.Errorf("claude desktop profile %q has no request profile %q", b.ProfileID, profileID)
	}
	return b.RequestProfiles[index], nil
}

func (b *Bundle) Resolve(key RequestVariantKey) (RequestVariant, error) {
	if b == nil {
		return RequestVariant{}, fmt.Errorf("claude desktop profile: bundle is nil")
	}
	if b.byKey == nil {
		if errValidate := b.Validate(); errValidate != nil {
			return RequestVariant{}, errValidate
		}
	}
	key.Model = normalizeModel(key.Model)
	key.LogicalModel = normalizeModel(key.LogicalModel)
	if key.LogicalModel == "" {
		key.LogicalModel = key.Model
	}
	key.ThinkingDisplay = strings.ToLower(strings.TrimSpace(key.ThinkingDisplay))
	variant, ok := b.overlayByKey[key]
	if !ok {
		variant, ok = b.byKey[key]
	}
	if !ok {
		return RequestVariant{}, fmt.Errorf("claude desktop profile %q has no exact variant for model=%q role=%q diagnostics=%t", b.ProfileID, key.Model, key.Role, key.Diagnostics)
	}
	return variant, nil
}

func (b *Bundle) Artifact(name string) (TextArtifact, error) {
	if b == nil {
		return TextArtifact{}, fmt.Errorf("claude desktop profile: bundle is nil")
	}
	artifact, ok := b.Artifacts[strings.TrimSpace(name)]
	if !ok {
		return TextArtifact{}, fmt.Errorf("claude desktop profile %q has no artifact %q", b.ProfileID, name)
	}
	return artifact, nil
}

func (b *Bundle) ArtifactForVariant(variant RequestVariant, name string) (TextArtifact, error) {
	requestProfile, errProfile := b.RequestProfileForVariant(variant)
	if errProfile != nil {
		return TextArtifact{}, errProfile
	}
	artifact, ok := requestProfile.Artifacts[strings.TrimSpace(name)]
	if !ok {
		return TextArtifact{}, fmt.Errorf("claude desktop request profile %q has no artifact %q", requestProfile.ProfileID, name)
	}
	return artifact, nil
}

func (a TextArtifact) Render(values map[string]string) (string, error) {
	var missing string
	rendered := artifactPlaceholderPattern.ReplaceAllStringFunc(a.Text, func(token string) string {
		name := token[2 : len(token)-2]
		value, ok := values[name]
		if !ok && missing == "" {
			missing = name
		}
		return value
	})
	if missing != "" {
		return "", fmt.Errorf("claude desktop artifact is missing runtime value %q", missing)
	}
	return rendered, nil
}

func validateTextArtifact(name string, artifact TextArtifact) error {
	if artifact.Bytes != len([]byte(artifact.Text)) {
		return fmt.Errorf("claude desktop profile: artifact %q byte length mismatch", name)
	}
	sum := sha256.Sum256([]byte(artifact.Text))
	if !strings.EqualFold(strings.TrimSpace(artifact.SHA256), hex.EncodeToString(sum[:])) {
		return fmt.Errorf("claude desktop profile: artifact %q digest mismatch", name)
	}
	for _, match := range artifactPlaceholderPattern.FindAllStringSubmatch(artifact.Text, -1) {
		if _, ok := allowedArtifactPlaceholders[match[1]]; !ok {
			return fmt.Errorf("claude desktop profile: artifact %q has unsupported placeholder %q", name, match[1])
		}
	}
	for _, marker := range []string{"Bearer eyJ", "sk-ant-", "access_token\"", "refresh_token\""} {
		if strings.Contains(artifact.Text, marker) {
			return fmt.Errorf("claude desktop profile: artifact %q contains a credential-shaped value", name)
		}
	}
	return nil
}

func validateBetaList(betas []string) error {
	if len(betas) == 0 {
		return fmt.Errorf("list is empty")
	}
	seen := make(map[string]struct{}, len(betas))
	for _, beta := range betas {
		beta = strings.TrimSpace(beta)
		if beta == "" {
			return fmt.Errorf("list contains an empty value")
		}
		if _, ok := seen[beta]; ok {
			return fmt.Errorf("list contains duplicate %q", beta)
		}
		seen[beta] = struct{}{}
	}
	return nil
}

func validateTransportProfiles(profiles TransportProfiles) error {
	if len(profiles.Profiles) == 0 {
		return fmt.Errorf("claude desktop profile: no transport profiles")
	}
	if len(profiles.RoleProfiles) == 0 {
		return fmt.Errorf("claude desktop profile: no role transport bindings")
	}
	for name, transport := range profiles.Profiles {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("claude desktop profile: transport profile name is empty")
		}
		if transport.Protocol != "http/1.1" {
			return fmt.Errorf("claude desktop profile: transport %q has unsupported protocol %q", name, transport.Protocol)
		}
		if transport.TLSLegacyVersion == 0 || len(transport.CipherSuites) == 0 || len(transport.ExtensionOrder) == 0 ||
			len(transport.SupportedGroups) == 0 || len(transport.SignatureAlgorithms) == 0 || len(transport.SupportedVersions) == 0 ||
			len(transport.KeyShareGroups) == 0 || len(transport.ALPN) == 0 {
			return fmt.Errorf("claude desktop profile: transport %q has an incomplete TLS profile", name)
		}
		if len(transport.JA3Hash) != 32 {
			return fmt.Errorf("claude desktop profile: transport %q has an invalid JA3 hash", name)
		}
		seenHeaders := make(map[string]struct{}, len(transport.HeaderOrder))
		for _, header := range transport.HeaderOrder {
			header = strings.TrimSpace(header)
			if header == "" {
				return fmt.Errorf("claude desktop profile: transport %q has an empty header name", name)
			}
			key := strings.ToLower(header)
			if _, exists := seenHeaders[key]; exists {
				return fmt.Errorf("claude desktop profile: transport %q repeats header %q", name, header)
			}
			seenHeaders[key] = struct{}{}
		}
		optionalHeaders := make(map[string]struct{}, len(transport.OptionalHeaders))
		for _, header := range transport.OptionalHeaders {
			header = strings.TrimSpace(header)
			key := strings.ToLower(header)
			if header == "" {
				return fmt.Errorf("claude desktop profile: transport %q has an empty optional header name", name)
			}
			if _, ok := seenHeaders[key]; !ok {
				return fmt.Errorf("claude desktop profile: transport %q optional header %q is absent from header_order", name, header)
			}
			if _, exists := optionalHeaders[key]; exists {
				return fmt.Errorf("claude desktop profile: transport %q repeats optional header %q", name, header)
			}
			optionalHeaders[key] = struct{}{}
		}
		for _, required := range []string{"accept", "authorization", "content-type", "user-agent", "connection", "host", "accept-encoding"} {
			if _, ok := seenHeaders[required]; !ok {
				return fmt.Errorf("claude desktop profile: transport %q omits required header %q", name, required)
			}
			if _, optional := optionalHeaders[required]; optional {
				return fmt.Errorf("claude desktop profile: transport %q marks required header %q optional", name, required)
			}
		}
	}
	for role, name := range profiles.RoleProfiles {
		if role == "" || strings.TrimSpace(name) == "" {
			return fmt.Errorf("claude desktop profile: invalid role transport binding")
		}
		if _, ok := profiles.Profiles[name]; !ok {
			return fmt.Errorf("claude desktop profile: role %q references unknown transport %q", role, name)
		}
	}
	for role, name := range profiles.EndpointProfiles {
		if strings.TrimSpace(role) == "" || strings.TrimSpace(name) == "" {
			return fmt.Errorf("claude desktop profile: invalid endpoint transport binding")
		}
		if _, ok := profiles.Profiles[name]; !ok {
			return fmt.Errorf("claude desktop profile: endpoint %q references unknown transport %q", role, name)
		}
	}
	return nil
}

func validateBodyProfiles(profiles *BodyProfiles) error {
	if profiles == nil || len(profiles.Profiles) == 0 || len(profiles.Bindings) == 0 {
		return fmt.Errorf("claude desktop profile: body profiles and bindings are required")
	}
	for name, body := range profiles.Profiles {
		name = strings.TrimSpace(name)
		if name == "" || len(body.TopLevelOrder) == 0 {
			return fmt.Errorf("claude desktop profile: body profile has an incomplete name or key order")
		}
		seen := make(map[string]struct{}, len(body.TopLevelOrder))
		for _, key := range body.TopLevelOrder {
			key = strings.TrimSpace(key)
			if key == "" {
				return fmt.Errorf("claude desktop profile: body profile %q has an empty top-level key", name)
			}
			if _, ok := seen[key]; ok {
				return fmt.Errorf("claude desktop profile: body profile %q repeats top-level key %q", name, key)
			}
			seen[key] = struct{}{}
		}
		for _, required := range []string{"model", "messages"} {
			if _, ok := seen[required]; !ok {
				return fmt.Errorf("claude desktop profile: body profile %q omits required key %q", name, required)
			}
		}
		fields := []struct {
			name  string
			value *json.RawMessage
		}{
			{name: "thinking", value: &body.Thinking},
			{name: "context_management", value: &body.ContextManagement},
			{name: "output_config", value: &body.OutputConfig},
			{name: "diagnostics", value: &body.Diagnostics},
			{name: "temperature", value: &body.Temperature},
			{name: "tool_choice", value: &body.ToolChoice},
		}
		for _, field := range fields {
			value := *field.value
			if len(value) > 0 && !json.Valid(value) {
				return fmt.Errorf("claude desktop profile: body profile %q has invalid %s JSON", name, field.name)
			}
			if len(value) > 0 {
				var compacted bytes.Buffer
				compacted.Grow(len(value))
				if errCompact := json.Compact(&compacted, value); errCompact != nil {
					return fmt.Errorf("claude desktop profile: compact body profile %q %s: %w", name, field.name, errCompact)
				}
				*field.value = append(json.RawMessage(nil), compacted.Bytes()...)
			}
		}
		profiles.Profiles[name] = body
	}
	for binding, name := range profiles.Bindings {
		if strings.TrimSpace(binding) == "" || strings.TrimSpace(name) == "" {
			return fmt.Errorf("claude desktop profile: body profile binding is incomplete")
		}
		if _, ok := profiles.Profiles[name]; !ok {
			return fmt.Errorf("claude desktop profile: body binding %q references unknown profile %q", binding, name)
		}
	}
	return nil
}

func normalizeModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	return model
}
