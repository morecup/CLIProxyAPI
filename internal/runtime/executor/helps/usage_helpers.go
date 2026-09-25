package helps

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/clienterror"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

type UsageReporter struct {
	provider        string
	executorType    string
	model           string
	alias           string
	authID          string
	authIndex       string
	authMu          sync.RWMutex
	accessTokenHash string
	authType        string
	apiKey          string
	source          string
	reasoning       string
	serviceTier     string
	generate        bool
	requestedAt     time.Time
	ttftMu          sync.RWMutex
	ttft            time.Duration
	ttftStart       time.Time
	ttftSet         bool
	once            sync.Once
}

type usageExecutor interface {
	Identifier() string
}

func NewExecutorUsageReporter(ctx context.Context, executor usageExecutor, model string, auth *cliproxyauth.Auth) *UsageReporter {
	provider := ""
	if executor != nil {
		provider = executor.Identifier()
	}
	reporter := NewUsageReporter(ctx, provider, model, auth)
	reporter.executorType = ExecutorTypeName(executor)
	return reporter
}

func NewUsageReporter(ctx context.Context, provider, model string, auth *cliproxyauth.Auth) *UsageReporter {
	apiKey := APIKeyFromContext(ctx)
	alias := usage.RequestedModelAliasFromContext(ctx)
	if alias == "" {
		alias = model
	}
	reporter := &UsageReporter{
		provider:    provider,
		model:       model,
		alias:       strings.TrimSpace(alias),
		requestedAt: time.Now(),
		apiKey:      apiKey,
		source:      resolveUsageSource(auth, apiKey),
		authType:    resolveUsageAuthType(auth),
		reasoning:   usage.ReasoningEffortFromContext(ctx),
		serviceTier: usage.ServiceTierFromContext(ctx),
		generate:    usage.GenerateFromContext(ctx),
	}
	if auth != nil {
		reporter.authID = auth.ID
		reporter.authIndex = auth.EnsureIndex()
		reporter.accessTokenHash = authAccessTokenSHA256(auth)
	}
	return reporter
}

// UpdateAccessTokenFingerprint records the token version actually used upstream.
func (r *UsageReporter) UpdateAccessTokenFingerprint(auth *cliproxyauth.Auth) {
	if r == nil {
		return
	}
	r.authMu.Lock()
	r.accessTokenHash = authAccessTokenSHA256(auth)
	r.authMu.Unlock()
}

func (r *UsageReporter) accessTokenFingerprint() string {
	if r == nil {
		return ""
	}
	r.authMu.RLock()
	defer r.authMu.RUnlock()
	return r.accessTokenHash
}

func ExecutorTypeName(executor any) string {
	if executor == nil {
		return ""
	}
	executorType := reflect.TypeOf(executor)
	for executorType.Kind() == reflect.Pointer {
		executorType = executorType.Elem()
	}
	return strings.TrimSpace(executorType.Name())
}

func (r *UsageReporter) Publish(ctx context.Context, detail usage.Detail) {
	r.publishWithOutcome(ctx, detail, false, usage.Failure{})
}

func (r *UsageReporter) PublishAdditionalModel(ctx context.Context, model string, detail usage.Detail) {
	record, ok := r.buildAdditionalModelRecord(model, detail)
	if !ok {
		return
	}
	r.publishRecord(ctx, record)
}

func (r *UsageReporter) SetTranslatedReasoningEffort(payload []byte, format string) {
	if r == nil {
		return
	}
	r.reasoning = thinking.ExtractTranslatedReasoningEffort(payload, format)
}

func (r *UsageReporter) TrackHTTPClient(client *http.Client) *http.Client {
	if r == nil || client == nil {
		return client
	}
	tracked := *client
	transport := tracked.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	tracked.Transport = usageTTFTRoundTripper{
		base:     transport,
		reporter: r,
	}
	return &tracked
}

func (r *UsageReporter) ObserveResponse(resp *http.Response) {
	if r == nil || resp == nil || resp.Body == nil {
		return
	}
	r.StartResponseTTFT()
	resp.Body = &usageTTFTReadCloser{
		ReadCloser: resp.Body,
		mark: func() {
			r.MarkFirstResponseByte()
		},
	}
}

func (r *UsageReporter) StartResponseTTFT() {
	if r == nil {
		return
	}
	r.ttftMu.Lock()
	if !r.ttftSet && r.ttftStart.IsZero() {
		r.ttftStart = time.Now()
	}
	r.ttftMu.Unlock()
}

func (r *UsageReporter) MarkFirstResponseByte() {
	if r == nil {
		return
	}
	r.ttftMu.Lock()
	if r.ttftSet {
		r.ttftMu.Unlock()
		return
	}
	start := r.ttftStart
	r.ttftStart = time.Time{}
	r.ttftMu.Unlock()
	if start.IsZero() {
		return
	}
	r.setTTFT(time.Since(start))
}

func (r *UsageReporter) buildAdditionalModelRecord(model string, detail usage.Detail) (usage.Record, bool) {
	if r == nil {
		return usage.Record{}, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return usage.Record{}, false
	}
	detail = normalizeUsageDetailTotal(detail, r.provider, r.executorType)
	if !hasNonZeroTokenUsage(detail) {
		return usage.Record{}, false
	}
	return r.buildRecordForModel(model, detail, false, usage.Failure{}), true
}

func (r *UsageReporter) PublishFailure(ctx context.Context, errs ...error) {
	r.publishWithOutcome(ctx, usage.Detail{}, true, failFromErrors(errs...))
}

func (r *UsageReporter) PublishFailureWithDetail(ctx context.Context, detail usage.Detail, errs ...error) {
	r.publishWithOutcome(ctx, detail, true, failFromErrors(errs...))
}

func (r *UsageReporter) TrackFailure(ctx context.Context, errPtr *error) {
	if r == nil || errPtr == nil {
		return
	}
	if *errPtr != nil {
		r.PublishFailure(ctx, *errPtr)
	}
}

func (r *UsageReporter) publishWithOutcome(ctx context.Context, detail usage.Detail, failed bool, fail usage.Failure) {
	if r == nil {
		return
	}
	detail = normalizeUsageDetailTotal(detail, r.provider, r.executorType)
	r.once.Do(func() {
		r.publishRecord(ctx, r.buildRecord(detail, failed, fail))
	})
}

func normalizeUsageDetailTotal(detail usage.Detail, provider, executorType string) usage.Detail {
	return usage.EnsureTokenBreakdownForProvider(detail, provider, executorType)
}

func hasNonZeroTokenUsage(detail usage.Detail) bool {
	return detail.InputTokens != 0 ||
		detail.OutputTokens != 0 ||
		detail.ReasoningTokens != 0 ||
		detail.CachedTokens != 0 ||
		detail.CacheReadTokens != 0 ||
		detail.CacheCreationTokens != 0 ||
		detail.TotalTokens != 0 ||
		detail.TokenBreakdown.TotalTokens != 0
}

// ensurePublished guarantees that a usage record is emitted exactly once.
// It is safe to call multiple times; only the first call wins due to once.Do.
// This is used to ensure request counting even when upstream responses do not
// include any usage fields (tokens), especially for streaming paths.
func (r *UsageReporter) EnsurePublished(ctx context.Context) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.publishRecord(ctx, r.buildRecord(usage.Detail{}, false, usage.Failure{}))
	})
}

func (r *UsageReporter) publishRecord(ctx context.Context, record usage.Record) {
	record.ResponseHeaders = internallogging.GetResponseHeaders(ctx)
	usage.PublishRecord(ctx, record)
}

func (r *UsageReporter) buildRecord(detail usage.Detail, failed bool, failures ...usage.Failure) usage.Record {
	var fail usage.Failure
	if len(failures) > 0 {
		fail = failures[0]
	}
	if r == nil {
		return usage.Record{Detail: detail, Failed: failed, Fail: fail, Generate: usage.GenerateFlag(true)}
	}
	return r.buildRecordForModel(r.model, detail, failed, fail)
}

func (r *UsageReporter) buildRecordForModel(model string, detail usage.Detail, failed bool, fail usage.Failure) usage.Record {
	if r == nil {
		return usage.Record{Model: model, Detail: detail, Failed: failed, Fail: fail, Generate: usage.GenerateFlag(true)}
	}
	return usage.Record{
		Provider:            r.provider,
		ExecutorType:        r.executorType,
		Model:               model,
		Alias:               r.alias,
		Source:              r.source,
		APIKey:              r.apiKey,
		AuthID:              r.authID,
		AuthIndex:           r.authIndex,
		AccessTokenSHA256:   r.accessTokenFingerprint(),
		AuthType:            r.authType,
		ReasoningEffort:     r.reasoning,
		ServiceTier:         r.serviceTier,
		ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
		Generate:            usage.GenerateFlag(r.generate),
		RequestedAt:         r.requestedAt,
		Latency:             r.latency(),
		TTFT:                r.ttftDuration(),
		Failed:              failed,
		Fail:                fail,
		Detail:              detail,
	}
}

func failFromErrors(errs ...error) usage.Failure {
	for _, err := range errs {
		if err == nil {
			continue
		}
		return usage.Failure{
			Body:       strings.TrimSpace(err.Error()),
			StatusCode: clienterror.HTTPStatusFromError(err),
		}
	}
	return usage.Failure{}
}

func (r *UsageReporter) latency() time.Duration {
	if r == nil || r.requestedAt.IsZero() {
		return 0
	}
	latency := time.Since(r.requestedAt)
	if latency < 0 {
		return 0
	}
	return latency
}

func (r *UsageReporter) setTTFT(ttft time.Duration) {
	if r == nil {
		return
	}
	if ttft < 0 {
		ttft = 0
	}
	r.ttftMu.Lock()
	if r.ttftSet {
		r.ttftMu.Unlock()
		return
	}
	r.ttft = ttft
	r.ttftSet = true
	r.ttftStart = time.Time{}
	r.ttftMu.Unlock()
}

func (r *UsageReporter) ttftDuration() time.Duration {
	if r == nil {
		return 0
	}
	r.ttftMu.RLock()
	defer r.ttftMu.RUnlock()
	return r.ttft
}

type usageTTFTRoundTripper struct {
	base     http.RoundTripper
	reporter *UsageReporter
}

func (t usageTTFTRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.reporter.StartResponseTTFT()
	resp, errRoundTrip := t.base.RoundTrip(req)
	if errRoundTrip != nil {
		return resp, errRoundTrip
	}
	t.reporter.ObserveResponse(resp)
	return resp, nil
}

type usageTTFTReadCloser struct {
	io.ReadCloser
	once sync.Once
	mark func()
}

func (r *usageTTFTReadCloser) Read(p []byte) (int, error) {
	if r == nil || r.ReadCloser == nil {
		return 0, io.ErrClosedPipe
	}
	n, errRead := r.ReadCloser.Read(p)
	if n > 0 && r.mark != nil {
		r.once.Do(r.mark)
	}
	return n, errRead
}

func APIKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	if v, exists := ginCtx.Get("userApiKey"); exists {
		switch value := v.(type) {
		case string:
			return value
		case fmt.Stringer:
			return value.String()
		default:
			return fmt.Sprintf("%v", value)
		}
	}
	return ""
}

func resolveUsageSource(auth *cliproxyauth.Auth, ctxAPIKey string) string {
	if auth != nil {
		if _, value := auth.AccountInfo(); value != "" {
			return strings.TrimSpace(value)
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return key
			}
		}
	}
	if trimmed := strings.TrimSpace(ctxAPIKey); trimmed != "" {
		return trimmed
	}
	return ""
}

func resolveUsageAuthType(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.AuthKind()
}

// StreamUsageBuffer keeps the latest usage detail observed in a stream.
type StreamUsageBuffer struct {
	detail usage.Detail
	ok     bool
}

var (
	openAIStreamUsageMarker       = []byte(`"usage"`)
	openAIStreamServiceTierMarker = []byte(`"service_tier"`)
)

// Observe records detail when ok is true, allowing the final stream usage to win.
func (b *StreamUsageBuffer) Observe(detail usage.Detail, ok bool) {
	if b == nil || !ok {
		return
	}
	responseServiceTier := strings.TrimSpace(detail.ResponseServiceTier)
	if responseServiceTier == "" || hasNonZeroTokenUsage(detail) {
		preservedTier := b.detail.ResponseServiceTier
		b.detail = detail
		if b.detail.ResponseServiceTier == "" {
			b.detail.ResponseServiceTier = preservedTier
		}
	} else {
		b.detail.ResponseServiceTier = responseServiceTier
	}
	b.ok = true
}

// ObserveOpenAIStream records response-tier state and the latest usage from an
// OpenAI-style stream while avoiding JSON parsing for irrelevant chunks.
func (b *StreamUsageBuffer) ObserveOpenAIStream(line []byte) {
	if b == nil {
		return
	}
	payload := jsonPayload(line)
	if len(payload) == 0 {
		return
	}

	hasUsageCandidate := bytes.Contains(payload, openAIStreamUsageMarker)
	needTier := b.detail.ResponseServiceTier == "" || hasUsageCandidate
	hasTierCandidate := needTier && bytes.Contains(payload, openAIStreamServiceTierMarker)
	if !hasUsageCandidate && !hasTierCandidate {
		return
	}
	if !gjson.ValidBytes(payload) {
		return
	}

	detail := usage.Detail{}
	usageOK := false
	if hasUsageCandidate {
		usageNode := gjson.GetBytes(payload, "usage")
		if hasOpenAIStyleUsageTokenFields(usageNode) {
			detail = parseOpenAIStyleUsageNode(usageNode)
			usageOK = true
		}
	}
	if hasTierCandidate {
		detail.ResponseServiceTier = extractResponseServiceTierFromValidJSON(payload)
	}
	b.Observe(detail, usageOK || detail.ResponseServiceTier != "")
}

// Publish emits the latest observed usage detail, if any.
func (b *StreamUsageBuffer) Publish(ctx context.Context, reporter *UsageReporter) bool {
	if b == nil || !b.ok || reporter == nil {
		return false
	}
	reporter.Publish(ctx, b.detail)
	return true
}

// PublishFailure emits the latest observed usage detail together with failure details.
func (b *StreamUsageBuffer) PublishFailure(ctx context.Context, reporter *UsageReporter, errs ...error) bool {
	if b == nil || reporter == nil {
		return false
	}
	reporter.PublishFailureWithDetail(ctx, b.detail, errs...)
	return true
}

// Detail returns the latest observed usage detail.
func (b *StreamUsageBuffer) Detail() (usage.Detail, bool) {
	if b == nil || !b.ok {
		return usage.Detail{}, false
	}
	return b.detail, true
}

func ParseOpenAIUsage(data []byte) usage.Detail {
	responseServiceTier := extractResponseServiceTier(data)
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !hasOpenAIStyleUsageTokenFields(usageNode) {
		return usage.Detail{ResponseServiceTier: responseServiceTier}
	}
	detail := parseOpenAIStyleUsageNode(usageNode)
	detail.ResponseServiceTier = responseServiceTier
	return detail
}

func hasOpenAIStyleUsageTokenFields(usageNode gjson.Result) bool {
	if !usageNode.Exists() || !usageNode.IsObject() {
		return false
	}
	return usageNode.Get("total_tokens").Exists() || hasOpenAIStyleUsageBucketFields(usageNode)
}

func hasOpenAIStyleUsageBucketFields(usageNode gjson.Result) bool {
	return usageNode.Get("prompt_tokens").Exists() ||
		usageNode.Get("input_tokens").Exists() ||
		usageNode.Get("completion_tokens").Exists() ||
		usageNode.Get("output_tokens").Exists() ||
		usageNode.Get("prompt_tokens_details.cached_tokens").Exists() ||
		usageNode.Get("input_tokens_details.cached_tokens").Exists() ||
		usageNode.Get("prompt_tokens_details.cache_write_tokens").Exists() ||
		usageNode.Get("prompt_tokens_details.cache_creation_tokens").Exists() ||
		usageNode.Get("input_tokens_details.cache_write_tokens").Exists() ||
		usageNode.Get("input_tokens_details.cache_creation_tokens").Exists() ||
		usageNode.Get("completion_tokens_details.reasoning_tokens").Exists() ||
		usageNode.Get("output_tokens_details.reasoning_tokens").Exists()
}

func parseOpenAIStyleUsageNode(usageNode gjson.Result) usage.Detail {
	inputNode := usageNode.Get("prompt_tokens")
	if !inputNode.Exists() {
		inputNode = usageNode.Get("input_tokens")
	}
	outputNode := usageNode.Get("completion_tokens")
	if !outputNode.Exists() {
		outputNode = usageNode.Get("output_tokens")
	}
	detail := usage.Detail{
		InputTokens:  inputNode.Int(),
		OutputTokens: outputNode.Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	cached := usageNode.Get("prompt_tokens_details.cached_tokens")
	if !cached.Exists() {
		cached = usageNode.Get("input_tokens_details.cached_tokens")
	}
	if cached.Exists() {
		detail.CachedTokens = cached.Int()
		detail.CacheReadTokens = cached.Int()
	}
	cacheCreation := firstExistingUsageNode(
		usageNode,
		"input_tokens_details.cache_creation_tokens",
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_write_tokens",
	)
	if cacheCreation.Exists() {
		detail.CacheCreationTokens = cacheCreation.Int()
	}
	reasoning := usageNode.Get("completion_tokens_details.reasoning_tokens")
	if !reasoning.Exists() {
		reasoning = usageNode.Get("output_tokens_details.reasoning_tokens")
	}
	if reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	if hasOpenAIStyleUsageBucketFields(usageNode) {
		if inputNode.Exists() && outputNode.Exists() {
			detail.TokenBreakdown = usage.NewSubsetTokenBreakdown(
				detail.InputTokens,
				detail.CacheReadTokens,
				detail.CacheCreationTokens,
				detail.OutputTokens,
				detail.ReasoningTokens,
				detail.TotalTokens,
			)
		} else {
			cacheReadTokens := detail.CacheReadTokens
			cacheCreationTokens := detail.CacheCreationTokens
			if !inputNode.Exists() {
				cacheReadTokens = 0
				cacheCreationTokens = 0
			}
			reasoningTokens := detail.ReasoningTokens
			if !outputNode.Exists() {
				reasoningTokens = 0
			}
			detail.TokenBreakdown = usage.NewPartialSubsetTokenBreakdown(
				detail.InputTokens,
				cacheReadTokens,
				cacheCreationTokens,
				detail.OutputTokens,
				reasoningTokens,
				detail.TotalTokens,
			)
		}
	} else {
		detail.TokenBreakdown = usage.NewUnclassifiedTokenBreakdown(detail.TotalTokens)
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.TokenBreakdown.TotalTokens
	}
	return detail
}

func ParseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	responseServiceTier := extractResponseServiceTier(payload)
	usageNode := gjson.GetBytes(payload, "usage")
	if !hasOpenAIStyleUsageTokenFields(usageNode) {
		if responseServiceTier == "" {
			return usage.Detail{}, false
		}
		return usage.Detail{ResponseServiceTier: responseServiceTier}, true
	}
	detail := parseOpenAIStyleUsageNode(usageNode)
	detail.ResponseServiceTier = responseServiceTier
	return detail, true
}

func ParseClaudeUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return parseClaudeUsageNode(usageNode)
}

func ParseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	return parseClaudeUsageNode(usageNode), true
}

func parseClaudeUsageNode(usageNode gjson.Result) usage.Detail {
	cacheReadTokens := usageNode.Get("cache_read_input_tokens").Int()
	cacheCreationTokens := usageNode.Get("cache_creation_input_tokens").Int()
	rawOutputTokens := usageNode.Get("output_tokens").Int()
	// Anthropic reports thinking as a subset of output_tokens. Prefer the official
	// nested field, then fall back to legacy aliases used by some gateways.
	reasoningNode := firstExistingUsageNode(
		usageNode,
		"output_tokens_details.thinking_tokens",
		"output_tokens_details.reasoning_tokens",
		"thinking_tokens",
	)
	reasoningTokens := reasoningNode.Int()
	if reasoningTokens < 0 {
		reasoningTokens = 0
	}
	nonReasoningOutput := rawOutputTokens
	if reasoningTokens > 0 && reasoningTokens <= rawOutputTokens {
		nonReasoningOutput = rawOutputTokens - reasoningTokens
	} else if reasoningTokens > rawOutputTokens {
		// Keep Detail.OutputTokens authoritative for keeper subset checks and
		// avoid inventing extra non-reasoning output when the upstream payload
		// is inconsistent.
		nonReasoningOutput = 0
	}
	detail := usage.Detail{
		InputTokens:         usageNode.Get("input_tokens").Int(),
		OutputTokens:        rawOutputTokens,
		ReasoningTokens:     reasoningTokens,
		CachedTokens:        cacheReadTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheCreationTokens
	}
	// raw output_tokens already includes thinking; cache fields are independent
	// from input_tokens in the Messages API.
	detail.TotalTokens = detail.InputTokens + rawOutputTokens + detail.CacheReadTokens + detail.CacheCreationTokens
	detail.TokenBreakdown = usage.NewIndependentTokenBreakdown(
		detail.InputTokens,
		detail.CacheReadTokens,
		detail.CacheCreationTokens,
		nonReasoningOutput,
		detail.ReasoningTokens,
		detail.TotalTokens,
	)
	return detail
}

func extractResponseServiceTier(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	return extractResponseServiceTierFromValidJSON(payload)
}

func extractResponseServiceTierFromValidJSON(payload []byte) string {
	for _, path := range []string{"response.service_tier", "service_tier"} {
		if tier := strings.TrimSpace(gjson.GetBytes(payload, path).String()); tier != "" {
			return tier
		}
	}
	return ""
}

func firstExistingUsageNode(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		node := root.Get(path)
		if node.Exists() {
			return node
		}
	}
	return gjson.Result{}
}

func JSONPayload(line []byte) []byte {
	return jsonPayload(line)
}

func jsonPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	return trimmed
}
