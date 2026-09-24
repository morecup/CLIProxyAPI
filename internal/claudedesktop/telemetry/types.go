package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	FactSessionInitialized = "session_initialized"
	FactSessionStopped     = "session_stopped"
	FactSessionVisibility  = "session_visibility_changed"
	FactSessionIdleTimeout = "session_idle_timeout_started"
	FactRequestStarted     = "request_started"
	FactFirstByte          = "first_byte"
	FactRequestSucceeded   = "request_succeeded"
	FactRequestFailed      = "request_failed"
	FactRuntimeStarted     = "runtime_started"
	FactRuntimeInitialized = "runtime_initialized"
	FactSDKInitHandshake   = "sdk_init_handshake"
	FactShutdownPending    = "shutdown_pending_state"
	FactRuntimeStopped     = "runtime_stopped"
	FactUpdateCheckStarted = "update_check_started"
	FactUpdateNotAvailable = "update_not_available"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type HTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f HTTPDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

type DoerFactory func(effectiveProxyURL string) HTTPDoer

type EndpointDoerFactory func(effectiveProxyURL, endpointRole string, auth *cliproxyauth.Auth) HTTPDoer

// RendererRuntimeSnapshot contains Electron-owned process metrics. The proxy
// cannot derive these values from the Go runtime; a Desktop companion must
// provide an actual Electron snapshot when these fields are required.
type RendererRuntimeSnapshot struct {
	ProcessFootprintSampleAgeMS            int64
	ProcessGPURSSBytes                     uint64
	ProcessMainCommitBytes                 uint64
	ProcessMainCPUPct                      float64
	ProcessMainExternalBytes               uint64
	ProcessMainFootprintBytes              uint64
	ProcessMainHeapLimitBytes              uint64
	ProcessMainHeapTotalBytes              uint64
	ProcessMainHeapUsedBytes               uint64
	ProcessMainRSSBytes                    uint64
	ProcessRendererCPUSumPct               float64
	ProcessRendererFootprintSumBytes       uint64
	ProcessRendererMainViewBlinkBytes      uint64
	ProcessRendererMainViewHeapLimitBytes  uint64
	ProcessRendererMainViewHeapSampleAgeMS int64
	ProcessRendererMainViewHeapTotalBytes  uint64
	ProcessRendererMainViewHeapUsedBytes   uint64
	ProcessRendererRSSSumBytes             uint64
	ProcessUtilityRSSSumBytes              uint64
	WindowCount                            int
}

type RendererRuntimeSnapshotProvider func() (RendererRuntimeSnapshot, bool)

// HostSnapshot contains stable machine-level fields used by renderer telemetry.
// A logical Desktop runtime receives one snapshot for its lifetime.
type HostSnapshot struct {
	TotalMemoryBytes     uint64
	AvailableMemoryBytes uint64
	CPUModel             string
	OSBuild              string
	OSRelease            string
	OSVersion            string
}

type HostSnapshotProvider func() HostSnapshot

// SDKProcessSnapshot contains metrics measured from the embedded Node process.
// The proxy cannot derive these values from the Go runtime. A Desktop companion
// or enrollment-bound runtime snapshot must provide them explicitly.
type SDKProcessSnapshot struct {
	UptimeSeconds         float64
	RSS                   uint64
	FootprintBytes        uint64
	CommitBytes           uint64
	PeakFootprintBytes    uint64
	MemorySampleAgeMS     int64
	HeapTotal             uint64
	HeapUsed              uint64
	External              uint64
	ArrayBuffers          uint64
	ConstrainedMemory     uint64
	CPUUserMicroseconds   int64
	CPUSystemMicroseconds int64
}

type SDKProcessSnapshotProvider func() (SDKProcessSnapshot, bool)

type deliveryProfile struct {
	endpointRole      string
	endpoint          string
	bodyFormat        string
	headers           []claudeprofile.TelemetryHeader
	headerOrder       []string
	runtimeMaterials  map[string]string
	rendererRuntime   *claudeprofile.RendererRuntimeProfile
	sentry            *claudeprofile.TelemetrySentryProfile
	batch             claudeprofile.TelemetryBatchProfile
	protocol          string
	userAgentPolicy   string
	transportRevision string
	authPolicy        string
	queueNamespace    string
}

type Options struct {
	StatePath                       string
	Bundle                          *claudeprofile.Bundle
	ApplicationSessionID            string
	DoerFactory                     DoerFactory
	EndpointDoerFactory             EndpointDoerFactory
	RendererRuntimeSnapshotProvider RendererRuntimeSnapshotProvider
	HostSnapshotProvider            HostSnapshotProvider
	SDKProcessSnapshotProvider      SDKProcessSnapshotProvider
	MachineProfileID                string
	GlobalProxyURL                  string
	Now                             func() time.Time
	RandomFloat                     func() float64
}

type RequestFacts struct {
	// ExternalSDKAccounting means the executor owns the ledger independently
	// of optional telemetry workers. Standalone observers retain their fallback.
	ExternalSDKAccounting bool
	Input                 claudeprompt.Submission
	Prompt                *claudeprompt.Request
	Role                  claudeprofile.RequestRole
	SessionID             string
	// DesktopSessionID belongs to the durable app record, not the SDK transcript.
	DesktopSessionID  string
	QueryID           string
	QueryLifetime     context.Context
	PromptID          string
	ParentPromptID    string
	ClientRequestID   string
	PreviousRequestID string
	Model             string
	DesktopVersion    string
	CodeVersion       string
	AgentSDKVersion   string
	PermissionMode    string
	MCPServerCount    int
	Betas             string
	MessageCount      int
	CachingEnabled    bool
	SkipCacheWrite    bool
	ForkPointPinned   bool
	MarkerCount       int
	QuerySource       string
	// Query lineage is a prompt's agent-loop position, not a fixed request-role depth.
	QueryChainID   string
	QueryDepth     *int
	EffortLevel    string
	FastMode       bool
	Attempt        int
	ChainStartedAt time.Time
	StartedAt      time.Time
	TranscriptSize *int64
}

type Usage struct {
	InputTokens              int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	OutputTokens             int64
}

type Binding struct {
	AuthID              string `json:"auth_id"`
	AccountUUID         string `json:"account_uuid"`
	OrganizationUUID    string `json:"organization_uuid"`
	DeviceID            string `json:"device_id"`
	ProfileID           string `json:"profile_id"`
	DesktopVersion      string `json:"desktop_version"`
	EgressProxyURL      string `json:"egress_proxy_url,omitempty"`
	EgressRevision      string `json:"egress_revision"`
	DestinationRevision string `json:"destination_revision"`
	TransportRevision   string `json:"transport_revision"`
	BindingRevision     string `json:"binding_revision"`
	RuntimeUUID         string `json:"runtime_uuid"`
}

type Envelope struct {
	Version         int             `json:"version"`
	EventUUID       string          `json:"event_uuid"`
	Sequence        uint64          `json:"sequence"`
	EndpointRole    string          `json:"endpoint_role"`
	CatalogFact     string          `json:"catalog_fact"`
	CatalogEvent    string          `json:"catalog_event"`
	OccurredAt      string          `json:"occurred_at"`
	Binding         Binding         `json:"binding"`
	SessionID       string          `json:"session_id,omitempty"`
	ClientRequestID string          `json:"client_request_id,omitempty"`
	Payload         json.RawMessage `json:"payload"`
	PayloadEncoding string          `json:"payload_encoding,omitempty"`
	ItemHeaders     map[string]any  `json:"item_headers,omitempty"`
}

type EndpointStatus struct {
	Role   string `json:"role"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type DeliveryEndpointStatus struct {
	Role              string `json:"role"`
	TelemetryClass    string `json:"telemetry_class"`
	Required          bool   `json:"required"`
	Status            string `json:"status"`
	Reason            string `json:"reason,omitempty"`
	TransportRevision string `json:"transport_revision"`
	TransportProtocol string `json:"transport_protocol"`
	TransportEvidence string `json:"transport_evidence,omitempty"`
	UserAgentPolicy   string `json:"user_agent_policy,omitempty"`
	AuthPolicy        string `json:"auth_policy,omitempty"`
}

type EvidenceCaptureWindowStatus struct {
	FirstCapturedAt string `json:"first_captured_at"`
	LastCapturedAt  string `json:"last_captured_at"`
}

type EvidenceCorpusStatus struct {
	FlowCount               int `json:"flow_count"`
	HTTPScenarioCount       int `json:"http_scenario_count"`
	EligibleScenarioCount   int `json:"eligible_scenario_count"`
	TelemetryBatchFlowCount int `json:"telemetry_batch_flow_count"`
	EventCount              int `json:"event_count"`
	EventNameCount          int `json:"event_name_count"`
}

type EvidenceArtifactStatus struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type EvidenceStatus struct {
	SourceManifestSHA256 string                      `json:"source_manifest_sha256"`
	CaptureWindow        EvidenceCaptureWindowStatus `json:"capture_window"`
	Corpus               EvidenceCorpusStatus        `json:"corpus"`
	Artifacts            []EvidenceArtifactStatus    `json:"artifacts"`
	ObservedScope        *ObservedScopeStatus        `json:"observed_scope,omitempty"`
}

// ObservedScopeStatus exposes provenance without repeating overlapping source
// memberships in every account snapshot. Corpus statistics remain baseline-only.
type ObservedScopeStatus struct {
	SchemaVersion                  int                    `json:"schema_version"`
	Policy                         string                 `json:"policy"`
	UnionSHA256                    string                 `json:"union_sha256"`
	BaselineEndpointEventCount     int                    `json:"baseline_endpoint_event_count"`
	BaselineEventNameCount         int                    `json:"baseline_event_name_count"`
	SupplementalEndpointEventCount int                    `json:"supplemental_endpoint_event_count"`
	SupplementalEventNameCount     int                    `json:"supplemental_event_name_count"`
	Sources                        []ObservedSourceStatus `json:"sources"`
}

type ObservedSourceStatus struct {
	Kind               string `json:"kind"`
	Artifact           string `json:"artifact"`
	SHA256             string `json:"sha256"`
	EndpointEventCount int    `json:"endpoint_event_count"`
}

// Executable means a matching production trigger exists, not field/timing or
// live A/B fidelity acceptance. Observed endpoint pairs remain the denominator.
type EndpointEventCoverageStatus struct {
	EndpointRole string `json:"endpoint_role"`
	EventName    string `json:"event_name"`
	Executable   bool   `json:"executable"`
}

type ObservedEndpointStatus struct {
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

type LiveEmitterCoverageStatus struct {
	Status                                string                        `json:"status"`
	CoveragePolicy                        string                        `json:"coverage_policy"`
	CapturedEventNameCount                int                           `json:"captured_event_name_count"`
	ObservableEventNameCount              int                           `json:"observable_event_name_count"`
	UnmodeledCapturedEventCount           int                           `json:"unmodeled_captured_event_count"`
	LiveEventNameCount                    int                           `json:"live_event_name_count"`
	LiveEventNames                        []string                      `json:"live_event_names"`
	RendererRuntimeMetricsSource          string                        `json:"renderer_runtime_metrics_source"`
	SDKProcessMetricsSource               string                        `json:"sdk_process_metrics_source"`
	TranscriptSizeSource                  string                        `json:"transcript_size_source"`
	PayloadContractStatus                 string                        `json:"payload_contract_status"`
	ObservedCompanionEventCount           int                           `json:"observed_companion_event_count"`
	CapturedPayloadContractEventCount     int                           `json:"captured_payload_contract_event_count"`
	SpecializedPayloadContractEventCount  int                           `json:"specialized_payload_contract_event_count"`
	EndpointOnlyPayloadContractEventCount int                           `json:"endpoint_only_payload_contract_event_count"`
	UncapturedPayloadContractEventCount   int                           `json:"uncaptured_payload_contract_event_count"`
	PayloadContractArtifact               string                        `json:"payload_contract_artifact,omitempty"`
	PayloadContractSHA256                 string                        `json:"payload_contract_sha256,omitempty"`
	SpecializedPayloadContractArtifact    string                        `json:"specialized_payload_contract_artifact,omitempty"`
	SpecializedPayloadContractSHA256      string                        `json:"specialized_payload_contract_sha256,omitempty"`
	DeclaredEventNameCount                int                           `json:"declared_event_name_count"`
	ObservableEndpointEventCount          int                           `json:"observable_endpoint_event_count"`
	LiveEndpointEventCount                int                           `json:"live_endpoint_event_count"`
	UnmodeledEndpointEventCount           int                           `json:"unmodeled_endpoint_event_count"`
	UnverifiedDeclaredEventNames          []string                      `json:"unverified_declared_event_names"`
	EndpointEvents                        []EndpointEventCoverageStatus `json:"endpoint_events,omitempty"`
	UncapturedExecutableEndpointEvents    []EndpointEventCoverageStatus `json:"uncaptured_executable_endpoint_events,omitempty"`
}

type TransportFidelityStatus struct {
	ClientHelloStatus       string `json:"client_hello_status"`
	ClientHelloPreset       string `json:"client_hello_preset"`
	ObservedChromiumVersion string `json:"observed_chromium_version"`
	HTTP2StreamMode         string `json:"http2_stream_mode"`
}

type AccountStatus struct {
	FactIssues          *FactIssueStatus `json:"fact_issues,omitempty"`
	AuthIDHash          string           `json:"auth_id_hash"`
	ProfileID           string           `json:"profile_id"`
	DesktopVersion      string           `json:"desktop_version"`
	EndpointRole        string           `json:"endpoint_role"`
	Health              string           `json:"health"`
	Pending             int              `json:"pending"`
	Sending             int              `json:"sending"`
	DeadLetters         int              `json:"dead_letters"`
	ConsecutiveFailures int              `json:"consecutive_failures"`
	LastSuccessAt       *time.Time       `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time       `json:"last_failure_at,omitempty"`
	LastError           string           `json:"last_error,omitempty"`
	NextAttemptAt       *time.Time       `json:"next_attempt_at,omitempty"`
	QueueWritable       bool             `json:"queue_writable"`
}

type Status struct {
	Enabled              bool                      `json:"enabled"`
	Required             bool                      `json:"required"`
	ProfileID            string                    `json:"profile_id,omitempty"`
	DesktopVersion       string                    `json:"desktop_version,omitempty"`
	TransportRevision    string                    `json:"transport_revision,omitempty"`
	TransportProtocol    string                    `json:"transport_protocol,omitempty"`
	TransportEvidence    string                    `json:"transport_evidence,omitempty"`
	UserAgentPolicy      string                    `json:"user_agent_policy,omitempty"`
	StatePathConfigured  bool                      `json:"state_path_configured"`
	MachineProfileID     string                    `json:"machine_profile_id,omitempty"`
	AppSessionIDHash     string                    `json:"app_session_id_hash,omitempty"`
	RuntimeStartedAt     *time.Time                `json:"runtime_started_at,omitempty"`
	Accounts             []AccountStatus           `json:"accounts"`
	DeliveryEndpoints    []DeliveryEndpointStatus  `json:"delivery_endpoints"`
	TelemetryEvidence    *EvidenceStatus           `json:"telemetry_evidence,omitempty"`
	ObservedEndpoints    []ObservedEndpointStatus  `json:"observed_endpoints"`
	UnsupportedEndpoints []EndpointStatus          `json:"unsupported_endpoints"`
	LiveEmitterCoverage  LiveEmitterCoverageStatus `json:"live_emitter_coverage"`
	TransportFidelity    TransportFidelityStatus   `json:"transport_fidelity"`
}

type RequestSpan struct {
	reactiveCompaction     *SDKReactiveCompactionSpan
	compactionResponse     claudeprompt.SDKCompactionResponse
	compactionInput        claudeprompt.SDKCompactionInput
	compactionSummary      claudeprompt.SDKCompactionSummary
	mu                     sync.Mutex
	manager                *Manager
	worker                 *accountWorker
	sdkWorker              *accountWorker
	auxiliaryWorkers       map[string]*accountWorker
	facts                  RequestFacts
	firstTurn              bool
	firstByte              time.Time
	usage                  Usage
	requestID              string
	stopReason             string
	requestObserved        bool
	responseObserved       bool
	responseStatus         int
	cacheDiagnosisRequest  sdkCacheDiagnosisRequest
	cacheDiagnosisRecorded bool
	cacheDiagnosis         *sdkCacheDiagnosisMetadata
	titleResponse          sdkTitleResponse
	titleParent            *sdkPromptParent
	titleAttemptFailed     bool
	titleOutcomeFinished   bool
	requestBodyChars       int
	inputTextCharLength    int
	textContentLength      int
	thinkingContentLength  *int
	toolUseContentLengths  map[string]int
	imageBlockCount        int
	imageTotalPixels       int64
	imageTotalBytes        int64
	documentBlockCount     int
	documentTotalBytes     int64
	timeSinceLastAPIMS     *int64
	retryRecorded          bool
	auxiliaryStarted       bool
	finished               bool
	promptFailureFinished  bool
	sdkPromptFinished      bool
}

// LineageToken reserves one monotonic request position in an account-bound
// Desktop session. The token remains valid across executor layers, while the
// underlying state is persisted by the account worker before the request is
// allowed to proceed.
type LineageToken struct {
	worker    *accountWorker
	sessionID string
	sequence  uint64
}

func (t *LineageToken) Active() bool {
	return t != nil && t.worker != nil && t.sessionID != "" && t.sequence != 0
}

// Commit records the Anthropic request-id returned for this reserved request.
// Older completions cannot overwrite a newer committed request lineage.
func (t *LineageToken) Commit(requestID string) error {
	if !t.Active() {
		return nil
	}
	return t.worker.commitLineage(t.sessionID, t.sequence, requestID)
}

func (s *RequestSpan) Active() bool {
	return s != nil && s.manager != nil && (s.worker != nil || s.sdkWorker != nil || len(s.auxiliaryWorkers) > 0)
}

func (s *RequestSpan) ObserveFirstByte(at time.Time) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	if s.finished || !s.firstByte.IsZero() {
		s.mu.Unlock()
		return
	}
	if at.IsZero() {
		at = s.manager.now()
	}
	s.firstByte = at
	s.mu.Unlock()
	if s.facts.Prompt != nil && !s.facts.Prompt.ObserveFirstByte(at) {
		return
	}
	s.manager.observeRendererFirstByte(s)
	s.manager.observeAuxiliaryFirstByte(s)
}

func (s *RequestSpan) ObserveUsage(usage Usage) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if usage.InputTokens > 0 {
		s.usage.InputTokens = usage.InputTokens
	}
	if usage.CacheCreationInputTokens > 0 {
		s.usage.CacheCreationInputTokens = usage.CacheCreationInputTokens
	}
	if usage.CacheReadInputTokens > 0 {
		s.usage.CacheReadInputTokens = usage.CacheReadInputTokens
	}
	if usage.OutputTokens > 0 {
		s.usage.OutputTokens = usage.OutputTokens
	}
}

func (s *RequestSpan) ObserveResponse(requestID, stopReason string) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		s.requestID = requestID
	}
	if stopReason = strings.TrimSpace(stopReason); stopReason != "" {
		s.stopReason = stopReason
	}
}

func (s *RequestSpan) FinishSuccess(ctx context.Context) {
	if !s.Active() {
		return
	}
	s.FinishSuccessAt(ctx, s.manager.now())
}

// FinishSuccessAt shares the executor's completion clock with API accounting.
func (s *RequestSpan) FinishSuccessAt(ctx context.Context, at time.Time) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.titleResponse.text = nil
	if s.facts.Role == claudeprofile.RoleCompaction && s.manager.bundle != nil {
		s.compactionSummary = s.compactionResponse.TakeSummary(s.manager.bundle.DesktopVersion, s.manager.bundle.CodeVersion)
	} else {
		s.compactionResponse.Discard()
	}
	s.mu.Unlock()
	if at.IsZero() {
		at = s.manager.now()
	}
	s.manager.finishRequestAt(ctx, s, "", "", at)
}

func (s *RequestSpan) FinishFailure(ctx context.Context, category string, err error) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.titleAttemptFailed = true
	s.titleResponse.text = nil
	s.compactionResponse.Discard()
	s.mu.Unlock()
	s.manager.finishRequest(ctx, s, category, safeErrorClass(err))
	if key := s.TitleFinalizerKey(); key != "" {
		if errPending := s.sdkWorker.setFactIssue(factIssueTitle, s.facts.SessionID, key, true); errPending != nil {
			s.sdkWorker.recordQueueFailure(errPending)
		}
	}
}
