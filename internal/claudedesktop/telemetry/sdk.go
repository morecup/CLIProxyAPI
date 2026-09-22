package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	FactSDKCacheBreakpoints = "cache_breakpoints"
	FactSDKSuccess          = "success"
	FactSDKRetry            = "retry"
)

const sdkLifecycleBetas = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management"

type sdkEventWrapper struct {
	EventType string       `json:"event_type"`
	EventData sdkEventData `json:"event_data"`
}

type sdkEventData struct {
	SkillName          *string        `json:"skill_name,omitempty"`
	EventName          string         `json:"event_name"`
	ClientTimestamp    string         `json:"client_timestamp"`
	Model              string         `json:"model"`
	SessionID          string         `json:"session_id"`
	UserType           string         `json:"user_type"`
	Betas              string         `json:"betas"`
	Environment        sdkEnvironment `json:"env"`
	Entrypoint         string         `json:"entrypoint"`
	AgentSDKVersion    string         `json:"agent_sdk_version"`
	IsInteractive      bool           `json:"is_interactive"`
	ClientType         string         `json:"client_type"`
	Process            string         `json:"process,omitempty"`
	AdditionalMetadata string         `json:"additional_metadata"`
	Auth               sdkEventAuth   `json:"auth"`
	EventID            string         `json:"event_id"`
	DeviceID           string         `json:"device_id"`
}

type sdkEnvironment struct {
	Platform              string `json:"platform"`
	NodeVersion           string `json:"node_version"`
	Terminal              string `json:"terminal"`
	PackageManagers       string `json:"package_managers"`
	Runtimes              string `json:"runtimes"`
	IsRunningWithBun      bool   `json:"is_running_with_bun"`
	IsCI                  bool   `json:"is_ci"`
	IsClaubbit            bool   `json:"is_claubbit"`
	IsGitHubAction        bool   `json:"is_github_action"`
	IsClaudeCodeAction    bool   `json:"is_claude_code_action"`
	IsClaudeAIAuth        bool   `json:"is_claude_ai_auth"`
	Version               string `json:"version"`
	Arch                  string `json:"arch"`
	IsClaudeCodeRemote    bool   `json:"is_claude_code_remote"`
	DeploymentEnvironment string `json:"deployment_environment"`
	IsConductor           bool   `json:"is_conductor"`
	VersionBase           string `json:"version_base"`
	BuildTime             string `json:"build_time"`
	IsLocalAgentMode      bool   `json:"is_local_agent_mode"`
	PlatformRaw           string `json:"platform_raw"`
	Shell                 string `json:"shell"`
}

type sdkEventAuth struct {
	OrganizationUUID string `json:"organization_uuid"`
	AccountUUID      string `json:"account_uuid"`
}

type sdkProcess struct {
	Uptime            float64     `json:"uptime"`
	RSS               uint64      `json:"rss"`
	HeapTotal         uint64      `json:"heapTotal"`
	HeapUsed          uint64      `json:"heapUsed"`
	External          uint64      `json:"external"`
	ArrayBuffers      uint64      `json:"arrayBuffers"`
	ConstrainedMemory uint64      `json:"constrainedMemory"`
	CPUUsage          sdkCPUUsage `json:"cpuUsage"`
}

type sdkCPUUsage struct {
	User   int64 `json:"user"`
	System int64 `json:"system"`
}

type sdkCacheBreakpointsMetadata struct {
	SubscriptionType  string `json:"subscription_type"`
	PromptID          string `json:"cc_prompt_id,omitempty"`
	TotalMessageCount int    `json:"totalMessageCount"`
	CachingEnabled    bool   `json:"cachingEnabled"`
	SkipCacheWrite    bool   `json:"skipCacheWrite"`
	ForkPointPinned   bool   `json:"forkPointPinned"`
	MarkerCount       int    `json:"markerCount"`
}

type sdkRetryMetadata struct {
	SubscriptionType  string `json:"subscription_type"`
	PromptID          string `json:"cc_prompt_id,omitempty"`
	Attempt           int    `json:"attempt"`
	AttemptDurationMS int64  `json:"attempt_duration_ms"`
	DelayMS           int64  `json:"delayMs"`
	Error             string `json:"error"`
	Provider          string `json:"provider"`
	Status            int    `json:"status,omitempty"`
}

// RecordScheduledRetry emits tengu_api_retry only after the conductor has
// committed to another upstream attempt. A terminal executor failure alone is
// insufficient because request-scoped failures and exhausted retry budgets do
// not produce this Desktop SDK event.
func (s *RequestSpan) RecordScheduledRetry(ctx context.Context, attempt int, delay time.Duration, err error) {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	if s.retryRecorded || !s.requestObserved {
		s.mu.Unlock()
		return
	}
	s.retryRecorded = true
	facts := s.facts
	s.mu.Unlock()
	eventAttempt := facts.Attempt
	if eventAttempt < 1 {
		eventAttempt = attempt
	}
	if eventAttempt < 1 {
		eventAttempt = 1
	}
	if delay < 0 {
		delay = 0
	}
	duration := s.manager.now().Sub(facts.StartedAt)
	if duration < 0 {
		duration = 0
	}
	status := 0
	var statusError interface{ StatusCode() int }
	if errors.As(err, &statusError) && statusError != nil {
		status = statusError.StatusCode()
	}
	if facts.Prompt != nil {
		facts.Prompt.ObserveSDKRetry(status)
	}
	metadata := sdkRetryMetadata{
		SubscriptionType:  subscriptionType(s.sdkWorker.authSnapshot()),
		PromptID:          facts.PromptID,
		Attempt:           eventAttempt,
		AttemptDurationMS: duration.Milliseconds(),
		DelayMS:           delay.Milliseconds(),
		Error:             sdkRetryError(err, status),
		Provider:          "firstParty",
		Status:            status,
	}
	if errEnqueue := s.manager.enqueueSDKEvent(s.sdkWorker, FactSDKRetry, facts, metadata); errEnqueue != nil {
		s.sdkWorker.recordQueueFailure(errEnqueue)
		if ctx == nil {
			ctx = context.Background()
		}
		log.WithContext(ctx).WithError(errEnqueue).Warn("claude desktop SDK telemetry: retry event was not persisted")
	}
	if worker := s.auxiliaryWorkers[datadogLogsBrowserRole]; worker != nil {
		payload, eventName, errProject := s.manager.projectDatadogBrowserError(worker, facts, duration, err, status)
		s.manager.enqueueAuxiliary(ctx, worker, FactSDKRetry, eventName, facts, payload, errProject)
	}
}

func sdkRetryError(err error, status int) string {
	if err == nil {
		return ""
	}
	var urlError *url.Error
	var networkError net.Error
	if errors.As(err, &urlError) || errors.As(err, &networkError) {
		return "Connection error."
	}
	class := safeErrorClass(err)
	if status > 0 {
		return fmt.Sprintf("API error: type=%s status=%d", class, status)
	}
	return fmt.Sprintf("API error: type=%s", class)
}

type sdkSuccessMetadata struct {
	SubscriptionType           string  `json:"subscription_type"`
	PromptID                   string  `json:"cc_prompt_id"`
	Model                      string  `json:"model"`
	Betas                      string  `json:"betas"`
	MessageCount               int     `json:"messageCount"`
	MessageTokens              int64   `json:"messageTokens"`
	InputTokens                int64   `json:"inputTokens"`
	OutputTokens               int64   `json:"outputTokens"`
	CachedInputTokens          int64   `json:"cachedInputTokens"`
	UncachedInputTokens        int64   `json:"uncachedInputTokens"`
	DurationMS                 int64   `json:"durationMs"`
	DurationMSIncludingRetries int64   `json:"durationMsIncludingRetries"`
	Attempt                    int     `json:"attempt"`
	TTFTMS                     int64   `json:"ttftMs"`
	BuildAgeMins               int64   `json:"buildAgeMins"`
	Provider                   string  `json:"provider"`
	RequestID                  string  `json:"requestId"`
	StopReason                 string  `json:"stop_reason"`
	CostUSD                    float64 `json:"costUSD"`
	DidFallBackToNonStreaming  bool    `json:"didFallBackToNonStreaming"`
	IsNonInteractiveSession    bool    `json:"isNonInteractiveSession"`
	Print                      bool    `json:"print"`
	IsTTY                      bool    `json:"isTTY"`
	QuerySource                string  `json:"querySource"`
	PermissionMode             string  `json:"permissionMode"`
	EffortLevel                string  `json:"effort_level,omitempty"`
	PreviousRequestID          string  `json:"previousRequestId,omitempty"`
	MessageClientPlatform      string  `json:"messageClientPlatform,omitempty"`
	GlobalCacheStrategy        string  `json:"globalCacheStrategy"`
	PromptCacheTTL             string  `json:"prompt_cache_ttl"`
	PromptCacheTTLReason       string  `json:"prompt_cache_ttl_reason"`
	TextContentLength          int     `json:"textContentLength"`
	ThinkingContentLength      *int    `json:"thinkingContentLength,omitempty"`
	ToolUseContentLengths      string  `json:"toolUseContentLengths,omitempty"`
	NarrationBlockCount        int     `json:"narrationBlockCount"`
	ImageBlockCount            int     `json:"imageBlockCount"`
	ImageTotalPixels           int64   `json:"imageTotalPixels"`
	ImageTotalBytes            int64   `json:"imageTotalBytes"`
	DocumentBlockCount         int     `json:"documentBlockCount"`
	DocumentTotalBytes         int64   `json:"documentTotalBytes"`
	InputTextCharLength        int     `json:"inputTextCharLength"`
	EstimatedInputTokens       int64   `json:"estimatedInputTokens"`
	RequestBodyEncoding        string  `json:"requestBodyEncoding"`
	RequestBodyChars           int     `json:"requestBodyChars"`
	GzipSkipReason             string  `json:"gzipSkipReason"`
	FastMode                   bool    `json:"fastMode"`
	BaseURL                    string  `json:"baseUrl"`
	DefaultModel               *string `json:"default_model,omitempty"`
	IsDefaultModel             *bool   `json:"is_default_model,omitempty"`
	DefaultEffortLevel         *string `json:"default_effort_level,omitempty"`
	IsDefaultEffort            *bool   `json:"is_default_effort,omitempty"`
	QueryChainID               string  `json:"queryChainId,omitempty"`
	QueryDepth                 *int    `json:"queryDepth,omitempty"`
	TimeSinceLastAPICallMS     *int64  `json:"timeSinceLastApiCallMs,omitempty"`
}

type sdkClassifierSuccessMetadata struct {
	CachedInputTokens          int64  `json:"cachedInputTokens"`
	PromptID                   string `json:"cc_prompt_id"`
	DurationMSIncludingRetries int64  `json:"durationMsIncludingRetries"`
	InputTokens                int64  `json:"inputTokens"`
	Model                      string `json:"model"`
	OutputTokens               int64  `json:"outputTokens"`
	QuerySource                string `json:"querySource"`
	RequestID                  string `json:"requestId"`
	StopReason                 string `json:"stop_reason"`
	SubscriptionType           string `json:"subscription_type"`
	TimeSinceLastAPICallMS     *int64 `json:"timeSinceLastApiCallMs,omitempty"`
	UncachedInputTokens        int64  `json:"uncachedInputTokens"`
}

func (s *RequestSpan) ObserveRequest(body []byte, headers http.Header) {
	if !s.Active() {
		return
	}
	s.mu.Lock()
	if s.finished || s.requestObserved {
		s.mu.Unlock()
		return
	}
	s.requestObserved = true
	observeSDKRequestFacts(&s.facts, body, headers)
	if s.facts.Role == claudeprofile.RoleCompaction {
		s.compactionInput = claudeprompt.ObserveSDKCompactionInput(body)
	}
	s.cacheDiagnosisRequest = sdkCacheDiagnosisRequestFromBody(body)
	s.requestBodyChars = javascriptUTF16Length(string(body))
	s.inputTextMetrics(body)
	if s.sdkWorker != nil {
		s.timeSinceLastAPIMS = s.sdkWorker.observeAPICall(s.facts.StartedAt)
	}
	emitAuxiliary := !s.auxiliaryStarted
	s.auxiliaryStarted = true
	facts := s.facts
	s.mu.Unlock()

	s.observeSDKParent(facts)
	s.emitSDKInput(facts)
	if facts.Prompt != nil {
		facts.Prompt.ObserveSDKQuery(body)
	}
	s.emitSDKPreparation(facts, body)
	if emitAuxiliary {
		s.manager.emitAuxiliaryRequestStarted(context.Background(), s)
	}
}

func (s *RequestSpan) inputTextMetrics(body []byte) {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return
	}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			typeName, _ := typed["type"].(string)
			switch typeName {
			case "text":
				if text, _ := typed["text"].(string); text != "" {
					s.inputTextCharLength += javascriptUTF16Length(text)
				}
			case "image":
				s.imageBlockCount++
				bytes, pixels := sdkBase64SourceMetrics(typed, true)
				s.imageTotalBytes += bytes
				s.imageTotalPixels += pixels
			case "document":
				s.documentBlockCount++
				bytes, _ := sdkBase64SourceMetrics(typed, false)
				s.documentTotalBytes += bytes
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
}

type sdkCountingReader struct {
	reader io.Reader
	count  int64
}

func (r *sdkCountingReader) Read(destination []byte) (int, error) {
	n, errRead := r.reader.Read(destination)
	r.count += int64(n)
	return n, errRead
}

func sdkBase64SourceMetrics(block map[string]any, inspectImage bool) (decodedBytes, pixels int64) {
	source, _ := block["source"].(map[string]any)
	if source == nil || !strings.EqualFold(strings.TrimSpace(stringValue(source["type"])), "base64") {
		return 0, 0
	}
	data := strings.TrimSpace(stringValue(source["data"]))
	if data == "" {
		return 0, 0
	}
	counter := &sdkCountingReader{reader: base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))}
	if inspectImage {
		if config, _, errConfig := image.DecodeConfig(counter); errConfig == nil && config.Width > 0 && config.Height > 0 {
			pixels = int64(config.Width) * int64(config.Height)
		}
	}
	if _, errRead := io.Copy(io.Discard, counter); errRead != nil {
		return 0, 0
	}
	return counter.count, pixels
}

func observeSDKRequestFacts(facts *RequestFacts, body []byte, headers http.Header) {
	if facts == nil {
		return
	}
	var root map[string]any
	if json.Unmarshal(body, &root) == nil {
		if model, _ := root["model"].(string); strings.TrimSpace(model) != "" {
			facts.Model = strings.TrimSpace(model)
		}
		if messages, ok := root["messages"].([]any); ok {
			facts.MessageCount = len(messages)
		}
		facts.MarkerCount = countJSONKey(root, "cache_control")
		facts.CachingEnabled = facts.MarkerCount > 0
		if facts.MarkerCount == 0 {
			facts.MarkerCount = 1
		}
		facts.EffortLevel = strings.TrimSpace(jsonStringAt(root, "output_config", "effort"))
		facts.FastMode = strings.EqualFold(strings.TrimSpace(jsonStringAt(root, "speed")), "fast")
	}
	for name, values := range headers {
		if strings.EqualFold(name, "anthropic-beta") && len(values) > 0 {
			facts.Betas = strings.TrimSpace(values[0])
			break
		}
	}
	if facts.QuerySource == "" {
		facts.QuerySource = sdkQuerySource(facts.Role)
	}
}

func countJSONKey(value any, target string) int {
	count := 0
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == target {
				count++
			}
			count += countJSONKey(child, target)
		}
	case []any:
		for _, child := range typed {
			count += countJSONKey(child, target)
		}
	}
	return count
}

func sdkQuerySource(role claudeprofile.RequestRole) string {
	switch role {
	case claudeprofile.RoleTitle:
		return "generate_session_title"
	case claudeprofile.RoleSecurityMonitor:
		return "agent_classifier"
	case claudeprofile.RoleCompaction:
		return "compact"
	case claudeprofile.RoleWebSearchHelper:
		return "web_search_tool"
	case claudeprofile.RoleLightHelper:
		return "prompt_suggestion"
	default:
		return "sdk"
	}
}

func (s *RequestSpan) ObserveResponseMetrics(requestID, stopReason string, textContentLength int) {
	s.ObserveResponseContentMetrics(requestID, stopReason, textContentLength, nil, nil)
}

func (s *RequestSpan) ObserveResponseContentMetrics(requestID, stopReason string, textContentLength int, thinkingContentLength *int, toolUseContentLengths map[string]int) {
	s.ObserveResponse(requestID, stopReason)
	if !s.Active() || textContentLength < 0 {
		return
	}
	s.mu.Lock()
	if !s.finished {
		s.textContentLength = textContentLength
		if thinkingContentLength != nil {
			value := *thinkingContentLength
			if value < 0 {
				value = 0
			}
			s.thinkingContentLength = &value
		}
		if len(toolUseContentLengths) > 0 {
			s.toolUseContentLengths = make(map[string]int, len(toolUseContentLengths))
			for name, length := range toolUseContentLengths {
				if name = strings.TrimSpace(name); name != "" {
					s.toolUseContentLengths[name] += max(length, 0)
				}
			}
		}
	}
	s.mu.Unlock()
}

func (m *Manager) finishSDKRequest(span *RequestSpan, category, errorClass string, duration time.Duration) error {
	if span == nil || span.sdkWorker == nil || !span.requestObserved {
		return nil
	}
	span.mu.Lock()
	facts := span.facts
	usage := span.usage
	compaction := claudeprompt.CompletedSDKCompaction(span.compactionInput, span.compactionSummary)
	requestID := span.requestID
	stopReason := span.stopReason
	firstByte := span.firstByte
	requestBodyChars := span.requestBodyChars
	inputTextCharLength := span.inputTextCharLength
	textContentLength := span.textContentLength
	thinkingContentLength := span.thinkingContentLength
	toolUseContentLengths := span.toolUseContentLengths
	imageBlockCount := span.imageBlockCount
	imageTotalPixels := span.imageTotalPixels
	imageTotalBytes := span.imageTotalBytes
	documentBlockCount := span.documentBlockCount
	documentTotalBytes := span.documentTotalBytes
	timeSinceLastAPIMS := span.timeSinceLastAPIMS
	cacheDiagnosis := span.cacheDiagnosis
	span.mu.Unlock()
	durationIncludingRetries := duration
	if !facts.ChainStartedAt.IsZero() && !facts.StartedAt.Before(facts.ChainStartedAt) {
		durationIncludingRetries += facts.StartedAt.Sub(facts.ChainStartedAt)
	}
	toolUseContentLengthsJSON := ""
	if len(toolUseContentLengths) > 0 {
		if encoded, errEncode := json.Marshal(toolUseContentLengths); errEncode == nil {
			toolUseContentLengthsJSON = string(encoded)
		}
	}

	if category != "" || errorClass != "" {
		// tengu_api_retry describes a retry that was actually scheduled. The
		// executor span only observes this attempt's terminal result; the outer
		// conductor decides later whether another attempt will run and what its
		// delay will be. Do not manufacture attempt=1/delayMs=0 from a failure.
		// Renderer outcome telemetry still records the normalized failure.
		return nil
	}
	if cacheDiagnosis != nil {
		if errDiagnosis := m.enqueueSDKEvent(span.sdkWorker, FactSDKCacheDiagnosis, facts, *cacheDiagnosis); errDiagnosis != nil {
			span.sdkWorker.recordQueueFailure(errDiagnosis)
			log.WithError(errDiagnosis).Warn("claude desktop SDK telemetry: cache diagnosis event was not persisted")
		}
	}
	if requestID == "" {
		requestID = "unknown"
	}
	if stopReason == "" {
		stopReason = "unknown"
	}
	ttft := int64(0)
	if !firstByte.IsZero() {
		ttft = firstByte.Sub(facts.StartedAt).Milliseconds()
		if ttft < 0 {
			ttft = 0
		}
	}
	if facts.Role == claudeprofile.RoleSecurityMonitor {
		metadata := sdkClassifierSuccessMetadata{
			CachedInputTokens:          usage.CacheReadInputTokens,
			PromptID:                   facts.PromptID,
			DurationMSIncludingRetries: durationIncludingRetries.Milliseconds(),
			InputTokens:                usage.InputTokens,
			Model:                      facts.Model,
			OutputTokens:               usage.OutputTokens,
			QuerySource:                facts.QuerySource,
			RequestID:                  requestID,
			StopReason:                 stopReason,
			SubscriptionType:           subscriptionType(span.sdkWorker.authSnapshot()),
			TimeSinceLastAPICallMS:     timeSinceLastAPIMS,
			UncachedInputTokens:        usage.CacheCreationInputTokens,
		}
		return m.enqueueSDKEvent(span.sdkWorker, FactSDKSuccess, facts, metadata)
	}
	cacheStrategy, promptCacheTTL, promptCacheTTLReason := sdkCacheProfile(facts.Role)
	messageTokens := usage.InputTokens
	if facts.Role == claudeprofile.RoleTitle {
		messageTokens = 0
	}
	queryChainID, queryDepth := sdkQueryLineage(span.sdkWorker, facts)
	var defaultModel *string
	var isDefaultModel *bool
	var defaultEffortLevel *string
	var isDefaultEffort *bool
	if facts.Role == claudeprofile.RoleMain {
		defaultModelValue := "claude-sonnet-5"
		isDefaultModelValue := facts.Model == defaultModelValue
		defaultModel = &defaultModelValue
		isDefaultModel = &isDefaultModelValue
		if strings.TrimSpace(facts.EffortLevel) != "" {
			defaultEffortValue := "high"
			isDefaultEffortValue := strings.EqualFold(strings.TrimSpace(facts.EffortLevel), defaultEffortValue)
			defaultEffortLevel = &defaultEffortValue
			isDefaultEffort = &isDefaultEffortValue
		}
	}
	metadata := sdkSuccessMetadata{
		SubscriptionType:           subscriptionType(span.sdkWorker.authSnapshot()),
		PromptID:                   facts.PromptID,
		Model:                      facts.Model,
		Betas:                      facts.Betas,
		MessageCount:               facts.MessageCount,
		MessageTokens:              messageTokens,
		InputTokens:                usage.InputTokens,
		OutputTokens:               usage.OutputTokens,
		CachedInputTokens:          usage.CacheReadInputTokens,
		UncachedInputTokens:        usage.CacheCreationInputTokens,
		DurationMS:                 sdkRoundedDurationMS(duration),
		DurationMSIncludingRetries: sdkRoundedDurationMS(durationIncludingRetries),
		Attempt:                    facts.Attempt,
		TTFTMS:                     ttft,
		BuildAgeMins:               m.sdkBuildAgeMinutes(),
		Provider:                   "firstParty",
		RequestID:                  requestID,
		StopReason:                 stopReason,
		CostUSD:                    sdkRequestCostUSD(facts.Model, usage),
		DidFallBackToNonStreaming:  false,
		IsNonInteractiveSession:    true,
		Print:                      false,
		IsTTY:                      false,
		QuerySource:                facts.QuerySource,
		PermissionMode:             defaultString(facts.PermissionMode, "default"),
		EffortLevel:                facts.EffortLevel,
		PreviousRequestID:          facts.PreviousRequestID,
		MessageClientPlatform:      sdkMessageClientPlatform(facts.Role),
		GlobalCacheStrategy:        cacheStrategy,
		PromptCacheTTL:             promptCacheTTL,
		PromptCacheTTLReason:       promptCacheTTLReason,
		TextContentLength:          textContentLength,
		ThinkingContentLength:      thinkingContentLength,
		ToolUseContentLengths:      toolUseContentLengthsJSON,
		NarrationBlockCount:        0,
		ImageBlockCount:            imageBlockCount,
		ImageTotalPixels:           imageTotalPixels,
		ImageTotalBytes:            imageTotalBytes,
		DocumentBlockCount:         documentBlockCount,
		DocumentTotalBytes:         documentTotalBytes,
		InputTextCharLength:        inputTextCharLength,
		EstimatedInputTokens:       sdkEstimatedInputTokens(inputTextCharLength),
		RequestBodyEncoding:        "identity",
		RequestBodyChars:           requestBodyChars,
		GzipSkipReason:             sdkGzipSkipReason(span.sdkWorker.binding.EgressProxyURL),
		FastMode:                   facts.FastMode,
		BaseURL:                    claudedesktop.DefaultAPIHost,
		DefaultModel:               defaultModel,
		IsDefaultModel:             isDefaultModel,
		DefaultEffortLevel:         defaultEffortLevel,
		IsDefaultEffort:            isDefaultEffort,
		QueryChainID:               queryChainID,
		QueryDepth:                 queryDepth,
		TimeSinceLastAPICallMS:     timeSinceLastAPIMS,
	}
	if facts.Role == claudeprofile.RoleTitle {
		metadata.PromptID = ""
		if parent := span.sdkTitleParent(); parent != nil {
			metadata.PromptID = parent.promptID
		}
	}
	// Accounting records the observed callback, not successful queue storage.
	// A full/unwritable telemetry queue must not erase the API completion fact.
	if !facts.ExternalSDKAccounting {
		if facts.Prompt != nil && facts.Role == claudeprofile.RoleMain {
			facts.Prompt.RecordSDKAPISuccess(sdkRoundedDurationMS(durationIncludingRetries))
		} else if parent := span.sdkTitleParent(); parent != nil && parent.owner != nil {
			switch facts.Role {
			case claudeprofile.RoleTitle, claudeprofile.RoleLightHelper:
				parent.owner.RecordSDKHelperSuccess(facts.QuerySource, facts.ClientRequestID, sdkRoundedDurationMS(durationIncludingRetries), facts.Attempt > 1)
			case claudeprofile.RoleCompaction:
				parent.owner.RecordSDKCompactionSuccess(facts.QuerySource, facts.ClientRequestID, sdkRoundedDurationMS(durationIncludingRetries), compaction)
			default:
				parent.owner.ObserveSDKUnmodeledHelper()
			}
		}
	}
	return m.enqueueSDKEvent(span.sdkWorker, FactSDKSuccess, facts, metadata)
}

func jsonStringAt(root map[string]any, path ...string) string {
	var current any = root
	for _, key := range path {
		object, _ := current.(map[string]any)
		if object == nil {
			return ""
		}
		current = object[key]
	}
	value, _ := current.(string)
	return value
}

func sdkMessageClientPlatform(role claudeprofile.RequestRole) string {
	switch role {
	case claudeprofile.RoleMain, claudeprofile.RoleCompaction, claudeprofile.RoleSubagent, claudeprofile.RoleLightHelper:
		return "desktop_app"
	default:
		return ""
	}
}

func (m *Manager) enqueueSDKEvent(worker *accountWorker, fact string, facts RequestFacts, metadata any) error {
	return m.enqueueSDKEventAt(worker, fact, facts, metadata, m.now())
}

func (m *Manager) enqueueSDKEventAt(worker *accountWorker, fact string, facts RequestFacts, metadata any, at time.Time) error {
	if worker == nil {
		return nil
	}
	profile, ok := m.sdkProfile.Events[fact]
	if !ok || profile.EventName == "" {
		return fmt.Errorf("SDK telemetry fact %q is not mapped by profile", fact)
	}
	var skillName *string
	if values, okValues := metadata.(interface{ sdkEventSkillName() *string }); okValues {
		skillName = values.sdkEventSkillName()
	}
	additionalMetadata, errMetadata := json.Marshal(metadata)
	if errMetadata != nil {
		return fmt.Errorf("marshal SDK telemetry metadata: %w", errMetadata)
	}
	process := ""
	if snapshot, okSnapshot := m.sdkProcessSnapshot(); okSnapshot {
		processJSON, errProcess := json.Marshal(snapshot)
		if errProcess != nil {
			return fmt.Errorf("marshal SDK telemetry process snapshot: %w", errProcess)
		}
		process = base64.StdEncoding.EncodeToString(processJSON)
	}
	timestamp := at.UTC()
	eventID := uuid.New().String()
	payload, errPayload := json.Marshal(sdkEventWrapper{
		EventType: m.sdkProfile.EventType,
		EventData: sdkEventData{
			SkillName:          skillName,
			EventName:          profile.EventName,
			ClientTimestamp:    timestamp.Format("2006-01-02T15:04:05.000Z"),
			Model:              facts.Model,
			SessionID:          facts.SessionID,
			UserType:           "external",
			Betas:              facts.Betas,
			Environment:        m.sdkEnvironment(),
			Entrypoint:         "claude-desktop",
			AgentSDKVersion:    m.bundle.AgentSDKVersion,
			IsInteractive:      false,
			ClientType:         "claude-desktop",
			Process:            process,
			AdditionalMetadata: base64.StdEncoding.EncodeToString(additionalMetadata),
			Auth: sdkEventAuth{
				OrganizationUUID: worker.binding.OrganizationUUID,
				AccountUUID:      worker.binding.AccountUUID,
			},
			EventID:  eventID,
			DeviceID: claudedesktop.RequestDeviceID(worker.binding.DeviceID),
		},
	})
	if errPayload != nil {
		return fmt.Errorf("marshal SDK telemetry event: %w", errPayload)
	}
	if errEnqueue := worker.enqueue(Envelope{
		Version:         1,
		EventUUID:       eventID,
		EndpointRole:    m.sdkProfile.EndpointRole,
		CatalogFact:     fact,
		CatalogEvent:    profile.EventName,
		OccurredAt:      timestamp.Format(time.RFC3339Nano),
		Binding:         worker.binding,
		SessionID:       facts.SessionID,
		ClientRequestID: facts.ClientRequestID,
		Payload:         payload,
	}); errEnqueue != nil {
		return errEnqueue
	}
	if errDatadog := m.enqueueDatadogLog(worker.authSnapshot(), fact, facts, profile.EventName, metadata); errDatadog != nil {
		log.WithError(errDatadog).Debug("claude desktop Datadog logs: SDK event mirror was not persisted")
	}
	return nil
}

func (m *Manager) ensureSDKRuntimeStarted(worker *accountWorker) error {
	if worker == nil || !m.beginActivationEvent(m.sdkDelivery.endpointRole) {
		return nil
	}
	facts := m.sdkLifecycleFacts()
	auth := worker.authSnapshot()
	subscription := subscriptionType(auth)
	events := []sdkStartupEmission{
		{FactRuntimeStarted, 100, map[string]any{
			"subscription_type": subscription,
			"in_tmux_worktree":  false,
			"tmux_flag":         false,
			"worktree_flag":     false,
		}},
		{FactRuntimeInitialized, 200, map[string]any{
			"subscription_type":                     subscription,
			"allowDangerouslySkipPermissionsPassed": false,
			"apiKeySource":                          "none",
			"autoUpdatesChannel":                    "latest",
			"bootstrap_entry":                       "cli",
			"dangerouslySkipPermissionsPassed":      false,
			"debug":                                 false,
			"debugToStderr":                         false,
			"entrypoint":                            "claude",
			"githubActionInputsPresent":             false,
			"hasInitialPrompt":                      false,
			"hasStdin":                              true,
			"has_mcp_localhost":                     false,
			"has_mcp_managed":                       false,
			"has_mcp_other":                         false,
			"has_mcp_private_network":               false,
			"has_mcp_public":                        false,
			"has_mcp_stdio":                         false,
			"has_remote":                            false,
			"inProtectedNamespace":                  false,
			"inputFormat":                           "stream-json",
			"is_git":                                false,
			"mcpClientCount":                        0,
			"modeIsBypass":                          false,
			"numAllowedTools":                       0,
			"numDisallowedTools":                    0,
			"outputFormat":                          "stream-json",
			"permissionMode":                        "default",
			"print":                                 false,
			"remote_host_class":                     "none",
			"rendererEntryPath":                     "gb_on",
			"thinkingType":                          "adaptive",
			"verbose":                               true,
			"worktree":                              false,
		}},
		{FactSDKInitHandshake, 300, map[string]any{
			"subscription_type": subscription,
			"mcpNonBlocking":    true,
			"mcp_client_count":  0,
			"mcp_pending_count": 0,
			"session_mirror":    false,
			"uptime_ms":         maxInt64(0, m.now().Sub(m.startedAt).Milliseconds()),
		}},
	}
	// Topic files register further native startup events (sdk_emit.go); they
	// are merged by their native order relative to the three core events.
	events = m.withRegisteredStartupEvents(events, subscription)
	for _, event := range events {
		if errEnqueue := m.enqueueSDKEvent(worker, event.fact, facts, event.metadata); errEnqueue != nil {
			m.clearActivationEvent(m.sdkDelivery.endpointRole)
			return errEnqueue
		}
	}
	select {
	case worker.wake <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) emitSDKShutdownPending() {
	if m == nil || !m.activationEventEmitted(m.sdkDelivery.endpointRole) {
		return
	}
	m.mu.RLock()
	var worker *accountWorker
	for _, candidate := range m.workers {
		if candidate.profile.endpointRole == m.sdkDelivery.endpointRole {
			worker = candidate
			break
		}
	}
	m.mu.RUnlock()
	if worker == nil {
		return
	}
	hadLiveTurn := false
	lastPromptID := ""
	m.lifecycleMu.Lock()
	for _, session := range m.sessions {
		if session == nil {
			continue
		}
		if session.pendingRequests > 0 || !session.lastActivityAt.IsZero() {
			hadLiveTurn = true
		}
		if session.facts.PromptID != "" {
			lastPromptID = session.facts.PromptID
		}
	}
	m.lifecycleMu.Unlock()
	metadata := map[string]any{
		"subscription_type":             subscriptionType(worker.authSnapshot()),
		"exiting":                       true,
		"had_live_turn":                 hadLiveTurn,
		"internal_events_pending":       0,
		"pending_human_requests":        0,
		"replies_left_for_next_process": 0,
		"worker_status":                 "idle",
	}
	if lastPromptID != "" {
		metadata["cc_prompt_id"] = lastPromptID
	}
	if errEnqueue := m.enqueueSDKEvent(worker, FactShutdownPending, m.sdkLifecycleFacts(), metadata); errEnqueue != nil {
		worker.recordQueueFailure(errEnqueue)
		log.WithError(errEnqueue).Warn("claude desktop SDK telemetry: shutdown state was not persisted")
	}
}

func (m *Manager) sdkLifecycleFacts() RequestFacts {
	model := "claude-sonnet-5"
	for _, endpoint := range m.bundle.Startup.Endpoints {
		if endpoint.EndpointRole != "startup-bootstrap" {
			continue
		}
		for _, query := range endpoint.Query {
			if query.Name == "model" && strings.TrimSpace(query.Value) != "" {
				model = strings.TrimSpace(query.Value)
			}
		}
	}
	return RequestFacts{
		SessionID:       m.appSessionID,
		ClientRequestID: uuid.New().String(),
		Model:           model,
		Betas:           sdkLifecycleBetas,
		StartedAt:       m.startedAt,
	}
}

func (m *Manager) beginActivationEvent(role string) bool {
	m.activationMu.Lock()
	defer m.activationMu.Unlock()
	if m.activationEmitted[role] {
		return false
	}
	m.activationEmitted[role] = true
	return true
}

func (m *Manager) clearActivationEvent(role string) {
	m.activationMu.Lock()
	delete(m.activationEmitted, role)
	m.activationMu.Unlock()
}

func (m *Manager) activationEventEmitted(role string) bool {
	m.activationMu.Lock()
	defer m.activationMu.Unlock()
	return m.activationEmitted[role]
}

func (m *Manager) sdkEnvironment() sdkEnvironment {
	value := func(key string) string {
		result, _ := m.sdkProfile.Environment[key].(string)
		return result
	}
	boolean := func(key string) bool {
		result, _ := m.sdkProfile.Environment[key].(bool)
		return result
	}
	return sdkEnvironment{
		Platform:              value("platform"),
		NodeVersion:           value("node_version"),
		Terminal:              value("terminal"),
		PackageManagers:       value("package_managers"),
		Runtimes:              value("runtimes"),
		IsRunningWithBun:      boolean("is_running_with_bun"),
		IsCI:                  boolean("is_ci"),
		IsClaubbit:            boolean("is_claubbit"),
		IsGitHubAction:        boolean("is_github_action"),
		IsClaudeCodeAction:    boolean("is_claude_code_action"),
		IsClaudeAIAuth:        boolean("is_claude_ai_auth"),
		Version:               value("version"),
		Arch:                  value("arch"),
		IsClaudeCodeRemote:    boolean("is_claude_code_remote"),
		DeploymentEnvironment: value("deployment_environment"),
		IsConductor:           boolean("is_conductor"),
		VersionBase:           value("version_base"),
		BuildTime:             value("build_time"),
		IsLocalAgentMode:      boolean("is_local_agent_mode"),
		PlatformRaw:           value("platform_raw"),
		Shell:                 value("shell"),
	}
}

func (m *Manager) sdkProcessSnapshot() (sdkProcess, bool) {
	if m == nil || m.sdkProcessProvider == nil {
		return sdkProcess{}, false
	}
	snapshot, ok := m.sdkProcessProvider()
	if !ok {
		return sdkProcess{}, false
	}
	// A configured process snapshot is a measured baseline, not a frozen value.
	// Advance it with this account runtime's own clock so independently
	// provisioned accounts cannot accidentally report one shared process uptime.
	uptime := snapshot.UptimeSeconds + m.now().Sub(m.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	constrainedMemory := snapshot.ConstrainedMemory
	if constrainedMemory == 0 {
		constrainedMemory = m.sdkProfile.Process.ConstrainedMemory
	}
	return sdkProcess{
		Uptime:            uptime,
		RSS:               snapshot.RSS,
		HeapTotal:         snapshot.HeapTotal,
		HeapUsed:          snapshot.HeapUsed,
		External:          snapshot.External,
		ArrayBuffers:      snapshot.ArrayBuffers,
		ConstrainedMemory: constrainedMemory,
		CPUUsage: sdkCPUUsage{
			User:   snapshot.CPUUserMicroseconds,
			System: snapshot.CPUSystemMicroseconds,
		},
	}, true
}

func (m *Manager) sdkBuildAgeMinutes() int64 {
	buildTime, errBuildTime := time.Parse(time.RFC3339, stringValue(m.sdkProfile.Environment["build_time"]))
	if errBuildTime != nil {
		return 0
	}
	age := m.now().Sub(buildTime)
	if age < 0 {
		return 0
	}
	return int64(age / time.Minute)
}

func (w *accountWorker) authSnapshot() *cliproxyauth.Auth {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.auth
}

func subscriptionType(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Metadata != nil {
		for _, key := range []string{"subscription_type", "subscriptionType", "billing_type"} {
			if value, _ := auth.Metadata[key].(string); strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return "pro"
}

func sdkErrorLabel(category, errorClass string) string {
	switch normalizeErrorCategory(defaultString(errorClass, category)) {
	case "network_error":
		return "Connection error."
	case "timeout":
		return "Request timed out."
	case "cancelled":
		return "Request cancelled."
	case "rate_limit":
		return "Rate limit exceeded."
	default:
		return "API request failed."
	}
}

func sdkCacheProfile(role claudeprofile.RequestRole) (strategy, ttl, reason string) {
	switch role {
	case claudeprofile.RoleTitle, claudeprofile.RoleWebSearchHelper:
		return "system_prompt", "5m", "default"
	case claudeprofile.RoleCompaction:
		return "none", "5m", "default"
	}
	return "none", "1h", "subscriber"
}

func sdkRequestCostUSD(model string, usage Usage) float64 {
	type prices struct {
		input, output, cacheRead, cacheWrite float64
	}
	normalized := strings.ToLower(strings.TrimSpace(model))
	price := prices{}
	switch {
	case strings.Contains(normalized, "haiku-4-5"):
		price = prices{input: 1, output: 5, cacheRead: 0.1, cacheWrite: 2}
	case strings.Contains(normalized, "sonnet-5"):
		price = prices{input: 2, output: 10, cacheRead: 0.2, cacheWrite: 4}
	case strings.Contains(normalized, "sonnet-4-6"):
		price = prices{input: 3, output: 15, cacheRead: 0.3, cacheWrite: 6}
	case strings.Contains(normalized, "opus-4-6"), strings.Contains(normalized, "opus-4-7"), strings.Contains(normalized, "opus-4-8"), strings.Contains(normalized, "opus-5"):
		price = prices{input: 5, output: 25, cacheRead: 0.5, cacheWrite: 10}
	default:
		return 0
	}
	return (float64(usage.InputTokens)*price.input +
		float64(usage.OutputTokens)*price.output +
		float64(usage.CacheReadInputTokens)*price.cacheRead +
		float64(usage.CacheCreationInputTokens)*price.cacheWrite) / 1_000_000
}

func sdkGzipSkipReason(proxyURL string) string {
	if strings.TrimSpace(proxyURL) != "" {
		return "proxy"
	}
	return "below_min_size"
}

func sdkEstimatedInputTokens(inputTextCharLength int) int64 {
	if inputTextCharLength <= 0 {
		return 0
	}
	return int64((inputTextCharLength + 2) / 3)
}

func javascriptUTF16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func sdkQueryLineage(worker *accountWorker, facts RequestFacts) (string, *int) {
	switch facts.Role {
	case claudeprofile.RoleMain, claudeprofile.RoleSubagent, claudeprofile.RoleLightHelper, claudeprofile.RoleCompaction:
	default:
		return "", nil
	}
	if facts.QueryDepth != nil && *facts.QueryDepth >= 0 {
		if _, errParse := uuid.Parse(facts.QueryChainID); errParse == nil {
			depth := *facts.QueryDepth
			return facts.QueryChainID, &depth
		}
	}
	// A standalone main call starts its own prompt. Helper/subagent depth is
	// unknown without an observed parent; role=helper does not imply depth=1.
	if facts.Role != claudeprofile.RoleMain || facts.PromptID == "" {
		return "", nil
	}
	depth := 0
	material := facts.SessionID + "\x00" + facts.PromptID
	if worker != nil {
		material = worker.binding.BindingRevision + "\x00" + material
	}
	queryChainID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("claude-desktop-query-chain-v2\x00"+material)).String()
	return queryChainID, &depth
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
