package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

func resolveIncomingClaudeHeaders(ctx context.Context, incoming http.Header) http.Header {
	resolved := make(http.Header)
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		resolved = ginCtx.Request.Header.Clone()
	}
	for key, values := range incoming {
		resolved[key] = append([]string(nil), values...)
	}
	return resolved
}

const (
	claudeTokenCountingBeta = "token-counting-2024-11-01"
	claudeFastModeBeta      = "fast-mode-2026-02-01"
)

// claudeRequestUsesFastMode reports whether the request selects the fast service
// tier. Anthropic rejects the body's speed field with "Extra inputs are not
// permitted" unless fast-mode-2026-02-01 is declared, so the beta has to follow
// the body. Deriving it here rather than at the call sites is deliberate: the
// streaming and non-streaming paths previously disagreed and streaming silently
// dropped the beta, turning every fast request into a 400.
func claudeRequestUsesFastMode(body []byte, requested map[string]bool) bool {
	if requested[claudeFastModeBeta] {
		return true
	}
	speed := gjson.GetBytes(body, "speed")
	return speed.Type == gjson.String && strings.EqualFold(strings.TrimSpace(speed.String()), "fast")
}

// claudeEntitlementError marks an upstream refusal that is a property of the
// request shape combined with the account's entitlements, not of the credential's
// health. The auth manager must neither rotate nor cool down on these.
type claudeEntitlementError struct {
	statusErr
}

func (claudeEntitlementError) IsRequestScoped() bool {
	return true
}

func (claudeEntitlementError) IsCredentialScoped() bool {
	return false
}

type claudeRateLimitError struct {
	statusErr
	credentialScoped bool
}

func (e claudeRateLimitError) IsCredentialScoped() bool {
	return e.credentialScoped
}

func (e claudeRateLimitError) IsRequestScoped() bool {
	return false
}

// classifyClaudeUpstreamError promotes upstream refusals that no other credential
// can satisfy into request-scoped errors.
//
// Anthropic answers a fast-mode request from an account without the matching
// usage credits with 429 rate_limit_error "Usage credits are required for fast
// mode". The generic pipeline reads 429 as quota exhaustion: it marks the
// credential Quota.Exceeded, applies an exponential cooldown and rotates to the
// next one, which returns the same 429. A single speed:"fast" request would walk
// the whole Claude pool and cool down every credential, all of which remain
// perfectly healthy for ordinary traffic. The refusal belongs to the request.
func classifyClaudeUpstreamError(statusCode int, headers http.Header, body []byte) error {
	var retryAfter *time.Duration
	if statusCode == http.StatusTooManyRequests || (statusCode >= 400 && statusCode < 600) {
		retryAfter = helps.ParseClaudeRateLimitReset(headers, time.Now())
	}
	err := statusErr{code: statusCode, msg: string(body), retryAfter: retryAfter}
	if statusCode == http.StatusTooManyRequests {
		if helps.ClaudeHeadersIndicateUnifiedRateLimitRejection(headers) {
			return claudeRateLimitError{statusErr: err, credentialScoped: true}
		}
		if claudeBodyIndicatesFastModeCredits(body) {
			return claudeEntitlementError{err}
		}
		// Ordinary model-level Claude 429 (not a unified 5h/7d rejection)
		return claudeRateLimitError{statusErr: err, credentialScoped: false}
	}
	return err
}

// claudeBodyIndicatesFastModeCredits matches Anthropic's fast-mode entitlement
// refusal without matching a genuine rate limit, which never mentions fast mode.
func claudeBodyIndicatesFastModeCredits(body []byte) bool {
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.ToLower(string(body))
	}
	return strings.Contains(message, "fast request rejected") ||
		(strings.Contains(message, "fast") &&
			(strings.Contains(message, "usage credits") || strings.Contains(message, "credits are required")))
}

// claudeRequestedBetas collects every beta the caller asked for, from the
// Anthropic-Beta header and from betas lifted out of the request body.
func claudeRequestedBetas(incomingBetas string, extraBetas []string) map[string]bool {
	requested := make(map[string]bool)
	for _, beta := range strings.Split(incomingBetas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	for _, beta := range extraBetas {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	return requested
}

// isAnthropicUpstreamURL reports whether a resolved request targets Anthropic's
// first-party API.
//
// Every rule that reconstructs Claude Code's identity must key on this rather
// than on the cloaked flag. Kimi rewrites base_url to api.kimi.com and custom
// gateways set their own host, yet both delegate to ClaudeExecutor and are
// therefore cloaked; a cloak-keyed rule silently rewrites their traffic too.
func isAnthropicUpstreamURL(u *url.URL) bool {
	return helps.IsAnthropicUpstreamURL(u)
}

// isAnthropicUpstreamBase reports whether a configured base URL targets Anthropic's
// first-party API. Used before the outgoing request exists.
func isAnthropicUpstreamBase(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return isAnthropicUpstreamURL(parsed)
}

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

const (
	claudeOpus55ModelID          = "claude-opus-5-5"
	claudeOpus55ComputerToolType = "computer_toolset_20260801"
)

type claudeRequestValidationError struct {
	statusErr
}

func (claudeRequestValidationError) IsRequestScoped() bool { return true }

// validateClaudeOpus55Request rejects request shapes the model cannot accept.
// It runs both before generic compatibility rewrites and against the finished
// wire body so caller intent is not silently changed and late payload rules
// cannot reintroduce an unsupported tool declaration. Computer tool versions
// are API-platform specific, so that gate applies only to Anthropic's endpoint.
func validateClaudeOpus55Request(body []byte, directAnthropic bool) error {
	if claudetasks.CanonicalModelID(gjson.GetBytes(body, "model").String()) != claudeOpus55ModelID {
		return nil
	}

	toolChoiceType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "tool_choice.type").String()))
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		return claudeRequestValidationError{statusErr{code: http.StatusBadRequest, msg: "claude-opus-5-5 adaptive thinking does not support tool_choice type \"any\" or \"tool\"; use \"auto\" or \"none\""}}
	}
	if !directAnthropic {
		return nil
	}

	for index, tool := range gjson.GetBytes(body, "tools").Array() {
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if !strings.HasPrefix(toolType, "computer_") || toolType == claudeOpus55ComputerToolType {
			continue
		}
		return claudeRequestValidationError{statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("claude-opus-5-5 does not support tools[%d].type %q; use %q", index, toolType, claudeOpus55ComputerToolType)}}
	}
	return nil
}

func claudeRequestUsesComputerToolset(body []byte) bool {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), claudeOpus55ComputerToolType) {
			return true
		}
	}
	return false
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
		// Adaptive thinking may also set output_config.effort; remove it to avoid
		// leaking thinking controls when tool_choice forces tool use.
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		if oc := gjson.GetBytes(body, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "output_config")
		}
	}
	return body
}

// normalizeClaudeSamplingForUpstream keeps Anthropic message requests valid.
//
// Translated compatibility callers keep the conservative normalization: their
// sampling knobs come from a protocol that was not written for Anthropic, and
// Anthropic rejects several combinations outright, so neither temperature nor
// top_p is worth forwarding.
//
// A Desktop request owns its profiled sampling shape. The measured structured
// Haiku helper sends "temperature":1, so stripping it would emit an unprofiled
// shape. Keep what the caller
// sent and drop only what Anthropic actually rejects (verified live):
//   - thinking active: temperature must be 1, top_p must be >= 0.95, top_k unset
//   - otherwise: temperature and top_p cannot both be specified
func normalizeClaudeSamplingForUpstream(body []byte, desktopOwned bool) []byte {
	thinkingActive := false
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "enabled", "adaptive", "auto":
		thinkingActive = true
	}

	if !desktopOwned {
		body, _ = sjson.DeleteBytes(body, "temperature")
		body, _ = sjson.DeleteBytes(body, "top_p")
		if thinkingActive {
			body, _ = sjson.DeleteBytes(body, "top_k")
		}
		return body
	}

	if thinkingActive {
		if temperature := gjson.GetBytes(body, "temperature"); temperature.Exists() && temperature.Num != 1 {
			body, _ = sjson.DeleteBytes(body, "temperature")
		}
		if topP := gjson.GetBytes(body, "top_p"); topP.Exists() && topP.Num < 0.95 {
			body, _ = sjson.DeleteBytes(body, "top_p")
		}
		body, _ = sjson.DeleteBytes(body, "top_k")
		return body
	}
	// Anthropic accepts either one but not both; temperature is the knob native
	// Claude Code actually sends, so top_p is the one that gives way.
	if gjson.GetBytes(body, "temperature").Exists() && gjson.GetBytes(body, "top_p").Exists() {
		body, _ = sjson.DeleteBytes(body, "top_p")
	}
	return body
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// peekableBody wraps a bufio.Reader around the original ReadCloser so that
// magic bytes can be inspected without consuming them from the stream.
type peekableBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableBody) Close() error {
	return p.closer.Close()
}

func claudeResponseContentEncoding(header http.Header) string {
	return strings.Join(header.Values("Content-Encoding"), ",")
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		// No Content-Encoding header.  Attempt best-effort magic-byte detection to
		// handle misbehaving upstreams that compress without setting the header.
		// Only gzip (1f 8b) and zstd (28 b5 2f fd) have reliable magic sequences;
		// br and deflate have none and are left as-is.
		// The bufio wrapper preserves unread bytes so callers always see the full
		// stream regardless of whether decompression was applied.
		pb := &peekableBody{Reader: bufio.NewReader(body), closer: body}
		magic, peekErr := pb.Peek(4)
		if peekErr == nil || (peekErr == io.EOF && len(magic) >= 2) {
			switch {
			case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
				gzipReader, gzErr := gzip.NewReader(pb)
				if gzErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte gzip: failed to create reader: %w", gzErr)
				}
				return &compositeReadCloser{
					Reader: gzipReader,
					closers: []func() error{
						gzipReader.Close,
						pb.Close,
					},
				}, nil
			case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
				decoder, zdErr := zstd.NewReader(pb)
				if zdErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte zstd: failed to create reader: %w", zdErr)
				}
				return &compositeReadCloser{
					Reader: decoder,
					closers: []func() error{
						func() error { decoder.Close(); return nil },
						pb.Close,
					},
				}, nil
			}
		}
		return pb, nil
	}
	encodings := strings.Split(contentEncoding, ",")
	reader := io.Reader(body)
	decoderClosers := make([]func() error, 0, len(encodings))
	cleanup := func() {
		for i := len(decoderClosers) - 1; i >= 0; i-- {
			_ = decoderClosers[i]()
		}
		_ = body.Close()
	}
	for index := len(encodings) - 1; index >= 0; index-- {
		encoding := strings.TrimSpace(strings.ToLower(encodings[index]))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, errGzip := gzip.NewReader(reader)
			if errGzip != nil {
				cleanup()
				return nil, fmt.Errorf("failed to create gzip reader: %w", errGzip)
			}
			reader = gzipReader
			decoderClosers = append(decoderClosers, gzipReader.Close)
		case "deflate":
			deflateReader, errDeflate := newClaudeDeflateReader(reader)
			if errDeflate != nil {
				cleanup()
				return nil, errDeflate
			}
			reader = deflateReader
			decoderClosers = append(decoderClosers, deflateReader.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, errZstd := zstd.NewReader(reader)
			if errZstd != nil {
				cleanup()
				return nil, fmt.Errorf("failed to create zstd reader: %w", errZstd)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, func() error {
				decoder.Close()
				return nil
			})
		default:
			cleanup()
			return nil, fmt.Errorf("unsupported content encoding %q", encoding)
		}
	}
	if len(decoderClosers) == 0 && reader == body {
		return body, nil
	}
	closers := make([]func() error, 0, len(decoderClosers)+1)
	for index := len(decoderClosers) - 1; index >= 0; index-- {
		closers = append(closers, decoderClosers[index])
	}
	closers = append(closers, body.Close)
	return &compositeReadCloser{Reader: reader, closers: closers}, nil
}

func newClaudeDeflateReader(reader io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	header, errPeek := buffered.Peek(2)
	if errPeek == nil && isZlibHeader(header) {
		zlibReader, errZlib := zlib.NewReader(buffered)
		if errZlib != nil {
			return nil, fmt.Errorf("failed to create zlib deflate reader: %w", errZlib)
		}
		return zlibReader, nil
	}
	return flate.NewReader(buffered), nil
}

func isZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

// claudeCredentialUsesOAuth classifies the selected upstream credential. It is the
// single authority for every decision that has to agree with the OAuth beta
// profile, including the extended-cache-ttl beta and the matching body cache ttl.
func claudeCredentialUsesOAuth(auth *cliproxyauth.Auth, apiKey string) bool {
	if isClaudeOAuthToken(apiKey) {
		return true
	}
	if auth != nil && auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		return false
	}
	hasAPIKeyAttr := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	return !hasAPIKeyAttr
}

func copyClaudeCallerFingerprintHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for name, values := range src {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if lowerName != "accept" && lowerName != "accept-encoding" && lowerName != "user-agent" &&
			lowerName != "x-app" && lowerName != "x-client-request-id" &&
			!strings.HasPrefix(lowerName, "anthropic-") &&
			!strings.HasPrefix(lowerName, "x-stainless-") &&
			!strings.HasPrefix(lowerName, "x-claude-code-") &&
			!strings.HasPrefix(lowerName, "x-claude-remote-") &&
			lowerName != "x-client-app" &&
			lowerName != "x-anthropic-additional-protection" {
			continue
		}
		dst.Del(name)
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, body []byte, incomingHeaders http.Header) error {
	return applyAnthropicCompatibleHeaders(r, auth, apiKey, stream, extraBetas, body, incomingHeaders)
}

func (e *ClaudeExecutor) applyClaudeHeadersWithProfile(
	r *http.Request,
	auth *cliproxyauth.Auth,
	apiKey string,
	stream bool,
	extraBetas []string,
	body []byte,
	desktopPlan claudeDesktopRequestPlan,
	incomingHeaders http.Header,
	sessionIDs ...string,
) error {
	if e == nil || !e.desktopOnly {
		return applyAnthropicCompatibleHeaders(r, auth, apiKey, stream, extraBetas, body, incomingHeaders)
	}
	_ = stream
	_ = extraBetas
	_ = body
	_ = incomingHeaders
	if r == nil {
		return nil
	}
	if e.desktopProfile == nil {
		return claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: "claude desktop profile is unavailable"}}
	}
	// Desktop owns the complete first-party header set. Rebuild from an empty map
	// so caller/config custom headers cannot create an unobserved hybrid profile.
	r.Header = make(http.Header)
	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	requestProfile, errProfile := e.desktopProfile.RequestProfileForVariant(desktopPlan.Variant)
	if errProfile != nil {
		return claudeDesktopPlanningError{statusErr{code: http.StatusServiceUnavailable, msg: errProfile.Error()}}
	}
	software := requestProfile.Software
	headers := desktopPlan.Variant.Headers
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", software.UserAgent)
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	r.Header.Set("X-App", "cli")
	r.Header.Set("X-Stainless-Retry-Count", "0")
	r.Header.Set("X-Stainless-Runtime", "node")
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("X-Stainless-Package-Version", software.PackageVersion)
	r.Header.Set("X-Stainless-Runtime-Version", software.RuntimeVersion)
	r.Header.Set("X-Stainless-OS", software.OS)
	r.Header.Set("X-Stainless-Arch", software.Arch)
	if value := strings.TrimSpace(headers.ClientPlatform); value != "" {
		r.Header.Set("Anthropic-Client-Platform", value)
	} else {
		r.Header.Del("Anthropic-Client-Platform")
	}
	if value := strings.TrimSpace(headers.ClientVersion); value != "" {
		r.Header.Set("Anthropic-Client-Version", value)
	} else {
		r.Header.Del("Anthropic-Client-Version")
	}
	if value := strings.TrimSpace(headers.DispatchID); value != "" {
		r.Header.Set("Anthropic-Dispatch-Id", value)
	} else {
		r.Header.Del("Anthropic-Dispatch-Id")
	}
	if value := strings.TrimSpace(headers.RequestClass); value != "" {
		r.Header.Set("X-Claude-Code-Request-Class", value)
	} else {
		r.Header.Del("X-Claude-Code-Request-Class")
	}
	if headers.IncludeTimeout {
		timeout := strings.TrimSpace(headers.Timeout)
		if timeout == "" {
			timeout = strings.TrimSpace(software.Timeout)
		}
		r.Header.Set("X-Stainless-Timeout", timeout)
	} else {
		r.Header.Del("X-Stainless-Timeout")
	}
	r.Header.Set("Accept", headers.Accept)
	r.Header.Set("Accept-Encoding", headers.AcceptEncoding)
	r.Header.Set("Connection", "keep-alive")
	if betas := desktopPlan.anthropicBeta(); betas != "" {
		r.Header.Set("Anthropic-Beta", betas)
	} else {
		r.Header.Del("Anthropic-Beta")
	}
	if desktopPlan.Variant.Key.Role == claudeprofile.RoleMain && helps.CanStripDefaultClaudeDesktopCreditBeta(r.Header, body) {
		owner, _ := r.Context().Value(claudeDesktopQueryContextKey{}).(claudeDesktopQueryContext)
		if host := e.claudeDesktopCreditBetaHost(r.Context(), auth, owner.session); host != nil && host.BetaRejectedForSession(owner.session, helps.ClaudeDesktopFallbackCreditBeta) {
			helps.StripClaudeDesktopFallbackCreditBeta(r.Header)
		}
	}
	if headers.IncludeAsync {
		r.Header.Set("X-Stainless-Async", "async")
	} else {
		r.Header.Del("X-Stainless-Async")
	}
	sessionID := ""
	for _, candidate := range sessionIDs {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			sessionID = candidate
			break
		}
	}
	if headers.IncludeSessionID {
		if sessionID == "" {
			return claudeDesktopPlanningError{statusErr{code: http.StatusInternalServerError, msg: "claude desktop request has no bound session id"}}
		}
		r.Header.Set("X-Claude-Code-Session-Id", sessionID)
	} else {
		r.Header.Del("X-Claude-Code-Session-Id")
	}
	if headers.IncludeClientID {
		clientRequestID := strings.TrimSpace(desktopPlan.ClientRequestID)
		if clientRequestID == "" {
			clientRequestID = uuid.New().String()
		}
		r.Header.Set("X-Client-Request-Id", clientRequestID)
	} else {
		r.Header.Del("X-Client-Request-Id")
	}
	if desktopPlan.Variant.Key.Role == claudeprofile.RoleCompaction && strings.EqualFold(strings.TrimSpace(headers.RequestClass), string(claudeprofile.RoleCompaction)) {
		kind := strings.TrimSpace(desktopPlan.CompactionRequestKind)
		if kind == "" {
			kind = "manual"
		}
		switch kind {
		case "manual", "auto", "reactive":
		default:
			return claudeDesktopPlanningError{statusErr{code: http.StatusBadRequest, msg: "invalid Claude Desktop compaction trigger"}}
		}
		r.Header.Set("X-CC-Compaction-Request", kind)
		r.Header.Set("X-Claude-Code-Compaction", kind)
	} else {
		r.Header.Del("X-CC-Compaction-Request")
		r.Header.Del("X-Claude-Code-Compaction")
	}
	atis := ""
	if e.desktopATIS != nil {
		var errATIS error
		atis, errATIS = e.desktopATIS.Assignment(
			r.Context(),
			auth,
			sessionID,
			desktopPlan.Variant.Key.LogicalModel,
			desktopPlan.Variant.Key.Model,
			desktopPlan.Variant.Key.Role,
		)
		desktopPlan.NativePrompt.BindNativeATISLatch(e.desktopATIS.Latch(sessionID))
		if errATIS != nil {
			helps.LogWithRequestID(r.Context()).WithFields(log.Fields{
				"role":  desktopPlan.Variant.Key.Role,
				"model": desktopPlan.Variant.Key.LogicalModel,
			}).WithError(errATIS).Warn("claude desktop: ATIS bootstrap degraded; omitting unavailable assignment")
		}
	}
	if atis != "" {
		r.Header.Set("x-cc-atis", atis)
	} else {
		r.Header.Del("x-cc-atis")
	}
	for _, name := range []string{
		"X-Claude-Code-Agent-Id",
		"X-Claude-Code-Parent-Agent-Id",
		"X-Claude-Remote-Container-Id",
		"X-Claude-Remote-Session-Id",
		"X-Client-App",
		"X-Anthropic-Additional-Protection",
	} {
		r.Header.Del(name)
	}
	return nil
}

func applyAnthropicCompatibleHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, body []byte, incomingHeaders http.Header) error {
	if r == nil {
		return nil
	}
	if incomingHeaders == nil {
		if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			incomingHeaders = ginCtx.Request.Header
		}
	}
	copyClaudeCallerFingerprintHeaders(r.Header, incomingHeaders)

	useAPIKey := auth != nil && (auth.AuthKind() == cliproxyauth.AuthKindAPIKey || (auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""))
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		if isAnthropicUpstreamURL(r.URL) && useAPIKey {
			r.Header.Del("Authorization")
			r.Header.Set("x-api-key", apiKey)
		} else {
			r.Header.Del("x-api-key")
			r.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		r.Header.Del("Authorization")
		r.Header.Del("x-api-key")
	}
	r.Header.Set("Content-Type", "application/json")

	defaultAccept := "application/json"
	defaultAcceptEncoding := "gzip, deflate, br, zstd"
	if stream && !isAnthropicUpstreamURL(r.URL) {
		defaultAccept = "text/event-stream"
		defaultAcceptEncoding = "identity"
	}
	misc.EnsureHeader(r.Header, incomingHeaders, "Anthropic-Version", "2023-06-01")
	misc.EnsureHeader(r.Header, incomingHeaders, "Accept", defaultAccept)
	misc.EnsureHeader(r.Header, incomingHeaders, "Accept-Encoding", defaultAcceptEncoding)
	misc.EnsureHeader(r.Header, incomingHeaders, "User-Agent", "CLIProxyAPI/"+buildinfo.Version)

	betas := strings.TrimSpace(strings.Join(incomingHeaders.Values("Anthropic-Beta"), ","))
	seen := make(map[string]bool)
	for _, beta := range strings.Split(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			seen[beta] = true
		}
	}
	appendBeta := func(beta string) {
		beta = strings.TrimSpace(beta)
		if beta == "" || seen[beta] {
			return
		}
		if betas == "" {
			betas = beta
		} else {
			betas += "," + beta
		}
		seen[beta] = true
	}
	for _, beta := range extraBetas {
		appendBeta(beta)
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "speed").String()), "fast") {
		appendBeta(claudeFastModeBeta)
	}
	if betas == "" {
		r.Header.Del("Anthropic-Beta")
	} else {
		r.Header.Set("Anthropic-Beta", betas)
	}

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, incomingHeaders)
	return nil
}

func stripClaudeDesktopIdentityHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	for name := range headers {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		if lowerName == "x-app" || lowerName == "x-client-request-id" ||
			lowerName == "x-client-app" || lowerName == "x-anthropic-additional-protection" ||
			lowerName == "anthropic-dangerous-direct-browser-access" ||
			strings.HasPrefix(lowerName, "x-stainless-") ||
			strings.HasPrefix(lowerName, "x-claude-code-") ||
			strings.HasPrefix(lowerName, "x-claude-remote-") {
			headers.Del(name)
		}
	}
}

// doClaudeUpstreamRequest is the single send boundary for every Claude upstream
// call. Folding the wire-casing pass in here makes it structurally impossible
// for one of the three request paths to drift away from the others, which is
// exactly how the streaming and non-streaming beta sets diverged before.
func doClaudeUpstreamRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	applyClaudeWireHeaderCasing(req)
	return client.Do(req)
}

func (e *ClaudeExecutor) doClaudeUpstreamRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	if e != nil && e.desktopOnly {
		applyClaudeWireHeaderCasing(req)
	}
	return client.Do(req)
}

// claudeWireHeaderCasing maps Go's canonical header name to the Desktop profile
// casing. Only the names that differ are listed;
// the other twelve already survive canonicalisation unchanged.
var claudeWireHeaderCasing = map[string]string{
	"X-Stainless-Os":      "X-Stainless-OS",
	"Anthropic-Beta":      "anthropic-beta",
	"Anthropic-Version":   "anthropic-version",
	"X-App":               "x-app",
	"X-Client-Request-Id": "x-client-request-id",

	"Anthropic-Dangerous-Direct-Browser-Access": "anthropic-dangerous-direct-browser-access",
}

// applyClaudeWireHeaderCasing restores the header name casing of the real client.
//
// CPA negotiates ALPN http/1.1 with Anthropic, so header names reach the server
// verbatim rather than lowercased by HPACK, which makes casing observable. Go
// canonicalises every name passed through Header.Set, turning the client's
// anthropic-beta and x-app into Anthropic-Beta and X-App. Writing the map keys
// directly is the only way to keep the original casing.
//
// This also fixes ordering for free: Go sorts header names bytewise when it
// serialises them, and the real client's order is exactly that same bytewise
// sort, so correct casing reproduces the correct order. Host, User-Agent and
// Content-Length remain misplaced because Go writes them ahead of the sorted
// block; that needs transport-level surgery and is out of scope here.
//
// Call this immediately before handing the request to the client and nowhere
// else. The rewritten keys are unreachable through Header.Get, which
// canonicalises its argument, so running it any earlier would silently hide
// these headers from the rest of the pipeline.
func applyClaudeWireHeaderCasing(r *http.Request) {
	if r == nil || r.Header == nil || !isAnthropicUpstreamURL(r.URL) {
		return
	}
	for canonical, wire := range claudeWireHeaderCasing {
		values, ok := r.Header[canonical]
		if !ok {
			continue
		}
		delete(r.Header, canonical)
		r.Header[wire] = values
	}
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" {
		apiKey = claudeauth.ReadMetadataString(&a.Metadata, "access_token")
	}
	return
}

// claudePayloadHasMidSystemMessage reports whether the caller placed a
// {"role":"system"} turn inside messages.
func claudePayloadHasMidSystemMessage(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	found := false
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			found = true
			return false
		}
		return true
	})
	return found
}

func rebuildMidSystemMessagesToTopLevel(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var movedSystemParts []string
	keptMessages := make([]string, 0, int(messages.Get("#").Int()))
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			movedSystemParts = append(movedSystemParts, claudeSystemTextParts(message.Get("content"))...)
			return true
		}
		keptMessages = append(keptMessages, message.Raw)
		return true
	})
	if len(movedSystemParts) == 0 {
		return payload
	}

	systemParts := claudeSystemTextParts(gjson.GetBytes(payload, "system"))
	systemParts = append(systemParts, movedSystemParts...)
	if len(systemParts) > 0 {
		if updated, errSetSystem := sjson.SetRawBytes(payload, "system", rawJSONArray(systemParts)); errSetSystem == nil {
			payload = updated
		}
	}
	if updated, errSetMessages := sjson.SetRawBytes(payload, "messages", rawJSONArray(keptMessages)); errSetMessages == nil {
		payload = updated
	}
	return payload
}

func claudeSystemTextParts(content gjson.Result) []string {
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return []string{string(block)}
	}
	if !content.IsArray() {
		return nil
	}

	var parts []string
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			text := item.String()
			if strings.TrimSpace(text) != "" {
				block := []byte(`{"type":"text","text":""}`)
				block, _ = sjson.SetBytes(block, "text", text)
				parts = append(parts, string(block))
			}
			return true
		}
		if item.IsObject() && item.Get("type").String() == "text" && strings.TrimSpace(item.Get("text").String()) != "" {
			parts = append(parts, item.Raw)
		}
		return true
	})
	return parts
}

func rawJSONArray(items []string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return []byte(builder.String())
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

type claudeMCPAliasOptions struct {
	secret string
	// firstParty names are declared by the owned Agent runtime byte-exact to
	// the SDK, which sends them unaliased; downstream client tools never
	// qualify because only owned requests populate this set.
	firstParty map[string]bool
}

func resolveClaudeMCPAliasOptions(ctx context.Context) claudeMCPAliasOptions {
	// Alias identity belongs to the downstream caller, not to the selected
	// upstream credential. This keeps names stable across OAuth refresh and auth
	// failover while giving one caller a shared virtual MCP server component.
	secret := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if secret == "" {
		secret = "cpa-claude-mcp-default-caller"
	}
	options := claudeMCPAliasOptions{secret: secret}
	if claudetasks.IsOwnedRequest(ctx) {
		options.firstParty = claudetasks.FirstPartyToolNames()
	}
	return options
}

// prepareClaudeDesktopToolNamesForUpstream applies one request-local MCP symbol
// table across every Claude Desktop request path.
func prepareClaudeDesktopToolNamesForUpstream(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string) {
	return remapOAuthToolNamesWithOptions(body, mcpAliases)
}

func restoreClaudeDesktopToolNamesFromResponse(body []byte, reverseMap map[string]string) ([]byte, error) {
	return reverseRemapOAuthToolNames(body, reverseMap)
}

func restoreClaudeDesktopToolNamesFromStreamLine(line []byte, reverseMap map[string]string) ([]byte, error) {
	return reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
}

// remapOAuthToolNames represents every declared third-party client tool as a
// semantic Claude Code MCP extension. Existing valid MCP names and explicit
// typed Anthropic tools remain unchanged.
//
// It operates on tools[].name, tool_choice.name, and all declared
// tool_use/tool_reference references in messages.
//
// The returned map is keyed on the upstream name and maps to the client-supplied
// original name. Callers MUST pass this map to the reverse
// functions so only aliases allocated for this request are restored on the
// response. A global reverse map would mix symbols from unrelated callers.
func remapOAuthToolNames(body []byte) ([]byte, map[string]string) {
	return remapOAuthToolNamesWithOptions(body, claudeMCPAliasOptions{secret: "cpa-claude-mcp-default-caller"})
}

type claudeRawJSONEdit struct {
	start       int
	end         int
	replacement string
}

func remapOAuthToolNamesWithOptions(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string) {
	remapped, reverseMap, ok := remapOAuthToolNamesWithBatchedEdits(body, mcpAliases)
	if ok {
		return remapped, reverseMap
	}
	return remapOAuthToolNamesWithOptionsLegacy(body, mcpAliases)
}

// remapOAuthToolNamesWithBatchedEdits records offsets from the original JSON
// and applies every rename in one copy. Repeated sjson.SetBytes calls copy most
// of the request for every historical tool reference, turning this path into
// O(body size * reference count) allocation growth.
func remapOAuthToolNamesWithBatchedEdits(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string, bool) {
	if !gjson.ValidBytes(body) {
		return nil, nil, false
	}

	reverseMap := make(map[string]string)
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// Build one request-specific forward map from declarations. Every client
	// tool, including typed custom declarations and names resembling Claude
	// built-ins, gets an MCP alias. Historical references use this same map.
	tools := gjson.GetBytes(body, "tools")
	forwardMap := make(map[string]string)
	protectedNames := make(map[string]bool)
	reservedNames := helps.AugmentClaudeBuiltinToolRegistry(body, nil)
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			name := tool.Get("name").String()
			if name != "" {
				reservedNames[name] = true
			}
			if helps.IsClaudeServerToolType(tool.Get("type").String()) {
				protectedNames[name] = true
			}
			return true
		})
		for name := range mcpAliases.firstParty {
			protectedNames[name] = true
		}
		passthroughMCPTools := make([]string, 0, 4)
		tools.ForEach(func(_, tool gjson.Result) bool {
			if helps.IsClaudeServerToolType(tool.Get("type").String()) {
				return true
			}
			name := tool.Get("name").String()
			if name == "" || mcpAliases.firstParty[name] {
				return true
			}
			if helps.IsClaudeMCPToolName(name) {
				passthroughMCPTools = append(passthroughMCPTools, name)
				return true
			}
			if _, exists := forwardMap[name]; exists {
				return true
			}
			alias, allocated := helps.AllocateClaudeMCPToolAlias(mcpAliases.secret, name, reservedNames)
			if !allocated {
				log.Warnf("claude oauth mcp alias: no free alias left for tool %q, forwarding the original name", name)
				return true
			}
			forwardMap[name] = alias
			reservedNames[alias] = true
			return true
		})
		recordPassthroughMCPTools(recordRename, forwardMap, passthroughMCPTools)
	}

	rewriteName := func(name string) (string, bool) {
		if name == "" || protectedNames[name] || helps.IsClaudeMCPToolName(name) {
			return name, false
		}
		if newName, ok := forwardMap[name]; ok && newName != name {
			return newName, true
		}
		return name, false
	}

	edits := make([]claudeRawJSONEdit, 0, len(forwardMap)+1)
	appendRawEdit := func(result gjson.Result, replacement string) bool {
		start := result.Index
		end := start + len(result.Raw)
		if result.Raw == "" || start < 0 || end < start || end > len(body) || !bytes.Equal(body[start:end], []byte(result.Raw)) {
			return false
		}
		edits = append(edits, claudeRawJSONEdit{start: start, end: end, replacement: replacement})
		return true
	}
	appendStringEdit := func(result gjson.Result, replacement string) bool {
		// Generated aliases only emit [A-Za-z0-9_-], so adding quotes is
		// byte-identical to sjson's encoding without another allocation.
		return appendRawEdit(result, `"`+replacement+`"`)
	}

	// 1. Rebuild typed custom tools exactly as before, but replace the original
	// tools array only after all offsets have been collected.
	toolsNeedRewrite := false
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			toolType := tool.Get("type").String()
			if helps.IsClaudeServerToolType(toolType) {
				return true
			}
			if strings.TrimSpace(toolType) != "" {
				toolsNeedRewrite = true
				return false
			}
			name := tool.Get("name").String()
			_, toolsNeedRewrite = rewriteName(name)
			return !toolsNeedRewrite
		})
	}
	if toolsNeedRewrite {
		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			if helps.IsClaudeServerToolType(tool.Get("type").String()) {
				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(tool.Raw)
				toolCount++
				return true
			}

			name := tool.Get("name").String()
			toolJSON := tool.Raw
			if strings.TrimSpace(tool.Get("type").String()) != "" {
				if updatedTool, errDelete := sjson.Delete(toolJSON, "type"); errDelete == nil {
					toolJSON = updatedTool
				}
			}
			if newName, renamed := rewriteName(name); renamed {
				updatedTool, err := sjson.Set(toolJSON, "name", newName)
				if err == nil {
					toolJSON = updatedTool
					recordRename(name, newName)
				}
			}

			if toolCount > 0 {
				toolsJSON.WriteByte(',')
			}
			toolsJSON.WriteString(toolJSON)
			toolCount++
			return true
		})
		toolsJSON.WriteByte(']')
		if !appendRawEdit(tools, toolsJSON.String()) {
			return nil, nil, false
		}
	}

	// 2. Rename tool_choice if it references a declared client tool.
	toolChoice := gjson.GetBytes(body, "tool_choice")
	if toolChoice.Get("type").String() == "tool" {
		nameResult := toolChoice.Get("name")
		tcName := nameResult.String()
		if newName, renamed := rewriteName(tcName); renamed {
			if !appendStringEdit(nameResult, newName) {
				return nil, nil, false
			}
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages while every Result.Index still
	// points into the original request bytes.
	messages := gjson.GetBytes(body, "messages")
	validOffsets := true
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "tool_use":
					nameResult := part.Get("name")
					name := nameResult.String()
					if newName, renamed := rewriteName(name); renamed {
						if !appendStringEdit(nameResult, newName) {
							validOffsets = false
							return false
						}
						recordRename(name, newName)
					}
				case "tool_reference":
					nameResult := part.Get("tool_name")
					toolName := nameResult.String()
					if newName, renamed := rewriteName(toolName); renamed {
						if !appendStringEdit(nameResult, newName) {
							validOffsets = false
							return false
						}
						recordRename(toolName, newName)
					}
				case "tool_result":
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(_, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() != "tool_reference" {
								return true
							}
							nameResult := nestedPart.Get("tool_name")
							nestedToolName := nameResult.String()
							if newName, renamed := rewriteName(nestedToolName); renamed {
								if !appendStringEdit(nameResult, newName) {
									validOffsets = false
									return false
								}
								recordRename(nestedToolName, newName)
							}
							return true
						})
					}
				case "tool_search_tool_result":
					toolRefs := part.Get("content.tool_references")
					if toolRefs.Exists() && toolRefs.IsArray() {
						toolRefs.ForEach(func(_, refPart gjson.Result) bool {
							if refPart.Get("type").String() != "tool_reference" {
								return true
							}
							nameResult := refPart.Get("tool_name")
							refToolName := nameResult.String()
							if newName, renamed := rewriteName(refToolName); renamed {
								if !appendStringEdit(nameResult, newName) {
									validOffsets = false
									return false
								}
								recordRename(refToolName, newName)
							}
							return true
						})
					}
				}
				return validOffsets
			})
			return validOffsets
		})
	}
	if !validOffsets {
		return nil, nil, false
	}

	remapped, ok := applyClaudeRawJSONEdits(body, edits)
	if !ok {
		return nil, nil, false
	}
	return remapped, reverseMap, true
}

func applyClaudeRawJSONEdits(body []byte, edits []claudeRawJSONEdit) ([]byte, bool) {
	if len(edits) == 0 {
		return body, true
	}
	sort.Slice(edits, func(i, j int) bool {
		return edits[i].start < edits[j].start
	})

	finalSize := len(body)
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor || edit.start < 0 || edit.end < edit.start || edit.end > len(body) {
			return nil, false
		}
		finalSize += len(edit.replacement) - (edit.end - edit.start)
		if finalSize < 0 {
			return nil, false
		}
		cursor = edit.end
	}

	out := make([]byte, 0, finalSize)
	cursor = 0
	for _, edit := range edits {
		out = append(out, body[cursor:edit.start]...)
		out = append(out, edit.replacement...)
		cursor = edit.end
	}
	out = append(out, body[cursor:]...)
	return out, true
}

// remapOAuthToolNamesWithOptionsLegacy is the byte-for-byte compatibility
// fallback for malformed JSON or an unexpected GJSON offset. Keep it available
// as a differential-test oracle for the batched implementation.
func remapOAuthToolNamesWithOptionsLegacy(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string) {
	reverseMap := make(map[string]string)
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// Build one request-specific forward map from declarations. Every client
	// tool, including typed custom declarations and names resembling Claude
	// built-ins, gets an MCP alias. Historical references use this same map.
	tools := gjson.GetBytes(body, "tools")
	forwardMap := make(map[string]string)
	protectedNames := make(map[string]bool)
	reservedNames := helps.AugmentClaudeBuiltinToolRegistry(body, nil)
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			name := tool.Get("name").String()
			if name != "" {
				reservedNames[name] = true
			}
			if helps.IsClaudeServerToolType(tool.Get("type").String()) {
				protectedNames[name] = true
			}
			return true
		})
		for name := range mcpAliases.firstParty {
			protectedNames[name] = true
		}
		passthroughMCPTools := make([]string, 0, 4)
		tools.ForEach(func(_, tool gjson.Result) bool {
			if helps.IsClaudeServerToolType(tool.Get("type").String()) {
				return true
			}
			name := tool.Get("name").String()
			if name == "" || mcpAliases.firstParty[name] {
				return true
			}
			if helps.IsClaudeMCPToolName(name) {
				passthroughMCPTools = append(passthroughMCPTools, name)
				return true
			}
			if _, exists := forwardMap[name]; exists {
				return true
			}
			alias, allocated := helps.AllocateClaudeMCPToolAlias(mcpAliases.secret, name, reservedNames)
			if !allocated {
				log.Warnf("claude oauth mcp alias: no free alias left for tool %q, forwarding the original name", name)
				return true
			}
			forwardMap[name] = alias
			reservedNames[alias] = true
			return true
		})
		recordPassthroughMCPTools(recordRename, forwardMap, passthroughMCPTools)
	}

	rewriteName := func(name string) (string, bool) {
		if name == "" || protectedNames[name] || helps.IsClaudeMCPToolName(name) {
			return name, false
		}
		if newName, ok := forwardMap[name]; ok && newName != name {
			return newName, true
		}
		return name, false
	}

	// 1. Rewrite the tools array without rebuilding from a stale gjson snapshot.
	toolsNeedRewrite := false
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			toolType := tool.Get("type").String()
			if helps.IsClaudeServerToolType(toolType) {
				return true
			}
			if strings.TrimSpace(toolType) != "" {
				toolsNeedRewrite = true
				return false
			}
			name := tool.Get("name").String()
			_, toolsNeedRewrite = rewriteName(name)
			return !toolsNeedRewrite
		})
	}
	if toolsNeedRewrite {
		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			if helps.IsClaudeServerToolType(tool.Get("type").String()) {
				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(tool.Raw)
				toolCount++
				return true
			}

			name := tool.Get("name").String()
			toolJSON := tool.Raw
			if strings.TrimSpace(tool.Get("type").String()) != "" {
				if updatedTool, errDelete := sjson.Delete(toolJSON, "type"); errDelete == nil {
					toolJSON = updatedTool
				}
			}
			if newName, renamed := rewriteName(name); renamed {
				updatedTool, err := sjson.Set(toolJSON, "name", newName)
				if err == nil {
					toolJSON = updatedTool
					recordRename(name, newName)
				}
			}

			if toolCount > 0 {
				toolsJSON.WriteByte(',')
			}
			toolsJSON.WriteString(toolJSON)
			toolCount++
			return true
		})
		toolsJSON.WriteByte(']')
		body, _ = sjson.SetRawBytes(body, "tools", []byte(toolsJSON.String()))
	}

	// 2. Rename tool_choice if it references a declared client tool.
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	if toolChoiceType == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if newName, renamed := rewriteName(tcName); renamed {
			body, _ = sjson.SetBytes(body, "tool_choice.name", newName)
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if newName, renamed := rewriteName(name); renamed {
						path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(name, newName)
					}
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if newName, renamed := rewriteName(toolName); renamed {
						path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(toolName, newName)
					}
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					toolID := part.Get("tool_use_id").String()
					_ = toolID // tool_use_id stays as-is
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if newName, renamed := rewriteName(nestedToolName); renamed {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, newName)
									recordRename(nestedToolName, newName)
								}
							}
							return true
						})
					}
				case "tool_search_tool_result":
					toolRefs := part.Get("content.tool_references")
					if toolRefs.Exists() && toolRefs.IsArray() {
						toolRefs.ForEach(func(refIndex, refPart gjson.Result) bool {
							if refPart.Get("type").String() == "tool_reference" {
								refToolName := refPart.Get("tool_name").String()
								if newName, renamed := rewriteName(refToolName); renamed {
									refPath := fmt.Sprintf("messages.%d.content.%d.content.tool_references.%d.tool_name", msgIndex.Int(), contentIndex.Int(), refIndex.Int())
									body, _ = sjson.SetBytes(body, refPath, newName)
									recordRename(refToolName, newName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body, reverseMap
}

type claudeMCPAliasParts struct {
	server   string
	toolID   string
	semantic string
}

type claudeMCPAliasEntry struct {
	alias    string
	original string
	parts    claudeMCPAliasParts
}

type claudeMCPAliasResolver struct {
	exact   map[string]string
	aliases []claudeMCPAliasEntry
	servers map[string]struct{}
}

type claudeMCPAliasRestoreError struct {
	error
}

func (e claudeMCPAliasRestoreError) Unwrap() error {
	return e.error
}

func (claudeMCPAliasRestoreError) IsRequestScoped() bool {
	return true
}

func newClaudeMCPAliasResolver(reverseMap map[string]string) claudeMCPAliasResolver {
	resolver := claudeMCPAliasResolver{
		exact:   reverseMap,
		aliases: make([]claudeMCPAliasEntry, 0, len(reverseMap)),
		servers: make(map[string]struct{}),
	}
	for alias, original := range reverseMap {
		if alias == original {
			// Caller-owned MCP tool recorded for exact passthrough only. It must not
			// register a virtual server or take part in fuzzy alias recovery.
			continue
		}
		parts, ok := parseClaudeMCPAlias(alias)
		if !ok {
			continue
		}
		resolver.aliases = append(resolver.aliases, claudeMCPAliasEntry{
			alias:    alias,
			original: original,
			parts:    parts,
		})
		resolver.servers[parts.server] = struct{}{}
	}
	return resolver
}

func parseClaudeMCPAlias(name string) (claudeMCPAliasParts, bool) {
	if !helps.IsClaudeMCPToolName(name) {
		return claudeMCPAliasParts{}, false
	}
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return claudeMCPAliasParts{}, false
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" {
		return claudeMCPAliasParts{}, false
	}
	toolID, semantic, ok := strings.Cut(tool, "_")
	if !ok || toolID == "" || semantic == "" {
		return claudeMCPAliasParts{}, false
	}
	return claudeMCPAliasParts{server: server, toolID: toolID, semantic: semantic}, true
}

func claudeMCPAliasServer(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

// recordPassthroughMCPTools remembers caller-owned MCP tool names that were left
// untouched. Without this the response resolver would treat such a name as a
// drifted alias whenever the derived two-word virtual server happens to equal a
// real MCP server name, and would either restore the wrong tool or fail the
// request. Recording is skipped when nothing was aliased so an untouched request
// keeps an empty reverse map and the restore path stays a no-op.
func recordPassthroughMCPTools(recordRename func(original, renamed string), forwardMap map[string]string, passthrough []string) {
	if len(forwardMap) == 0 {
		return
	}
	for _, name := range passthrough {
		recordRename(name, name)
	}
}

func (resolver claudeMCPAliasResolver) resolve(name string) (string, bool, error) {
	if original, ok := resolver.exact[name]; ok {
		if original == name {
			// Caller-owned MCP tool: forward it exactly as the client declared it.
			return "", false, nil
		}
		return original, true, nil
	}

	server := claudeMCPAliasServer(name)
	if _, known := resolver.servers[server]; !known {
		return "", false, nil
	}

	canonicalServerPrefix := "mcp__" + server + "__"
	normalizedName := name
	suffix := strings.TrimPrefix(name, canonicalServerPrefix)
	for {
		strippedSuffix, repeatedServer := strings.CutPrefix(suffix, server+"__")
		if !repeatedServer {
			break
		}
		suffix = strippedSuffix
		normalizedName = canonicalServerPrefix + suffix
		if original, exact := resolver.exact[normalizedName]; exact {
			return original, true, nil
		}
	}

	matchedOriginal := ""
	matchCount := 0
	for _, entry := range resolver.aliases {
		if entry.parts.server == server && strings.HasSuffix(name, entry.alias) {
			matchedOriginal = entry.original
			matchCount++
		}
	}
	if matchCount == 1 {
		return matchedOriginal, true, nil
	}
	if matchCount > 1 {
		return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: matched multiple declared aliases", name)}
	}

	parts, validAlias := parseClaudeMCPAlias(normalizedName)
	if validAlias {
		for _, entry := range resolver.aliases {
			if entry.parts.server == parts.server && entry.parts.semantic == parts.semantic {
				matchedOriginal = entry.original
				matchCount++
			}
		}
	}
	// Extra words in the tool component still parse, but the semantic field
	// is then wrong. Fall through to an unambiguous suffix match so word-level
	// repeats do not become restore 500s.
	if matchCount == 0 {
		var suffixMatches []claudeMCPAliasEntry
		for _, entry := range resolver.aliases {
			if entry.parts.server == server && strings.HasSuffix(normalizedName, "_"+entry.parts.semantic) {
				suffixMatches = append(suffixMatches, entry)
			}
		}
		if len(suffixMatches) == 1 {
			matchedOriginal = suffixMatches[0].original
			matchCount = 1
		} else if len(suffixMatches) > 1 {
			// If multiple candidates match (e.g. "_file" and "_read_file"),
			// choose the strictly longest semantic match when unambiguous.
			longest := suffixMatches[0]
			tie := false
			for _, candidate := range suffixMatches[1:] {
				if len(candidate.parts.semantic) > len(longest.parts.semantic) {
					longest = candidate
					tie = false
				} else if len(candidate.parts.semantic) == len(longest.parts.semantic) {
					tie = true
				}
			}
			if !tie {
				matchedOriginal = longest.original
				matchCount = 1
			} else {
				matchCount = len(suffixMatches)
			}
		}
		if matchCount == 1 {
			// This path guesses instead of failing, so leave a trace: it is the only
			// way to tell a silent wrong-tool restore from a healthy request.
			log.Debugf("claude oauth mcp alias: recovered drifted tool name %q as %q via semantic suffix", name, matchedOriginal)
		}
	}
	if matchCount == 1 {
		return matchedOriginal, true, nil
	}
	if matchCount > 1 {
		return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: semantic suffix matches multiple declared tools", name)}
	}

	return "", false, claudeMCPAliasRestoreError{fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: no unique request-local match", name)}
}

// reverseRemapOAuthToolNames reverses the tool name mapping for non-stream responses
// using the per-request map produced by remapOAuthToolNames. Names outside the
// request-local generated MCP server are passed through unchanged.
func reverseRemapOAuthToolNames(body []byte, reverseMap map[string]string) ([]byte, error) {
	if len(reverseMap) == 0 {
		return body, nil
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body, nil
	}
	resolver := newClaudeMCPAliasResolver(reverseMap)
	var resolveErr error
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			origName, matched, errResolve := resolver.resolve(name)
			if errResolve != nil {
				resolveErr = errResolve
				return false
			}
			if matched {
				path := fmt.Sprintf("content.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			origName, matched, errResolve := resolver.resolve(toolName)
			if errResolve != nil {
				resolveErr = errResolve
				return false
			}
			if matched {
				path := fmt.Sprintf("content.%d.tool_name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_result":
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() != "tool_reference" {
						return true
					}
					toolName := nestedPart.Get("tool_name").String()
					origName, matched, errResolve := resolver.resolve(toolName)
					if errResolve != nil {
						resolveErr = errResolve
						return false
					}
					if matched {
						path := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
						body, _ = sjson.SetBytes(body, path, origName)
					}
					return true
				})
			}
		case "tool_search_tool_result":
			toolRefs := part.Get("content.tool_references")
			if toolRefs.Exists() && toolRefs.IsArray() {
				toolRefs.ForEach(func(refIndex, refPart gjson.Result) bool {
					if refPart.Get("type").String() != "tool_reference" {
						return true
					}
					toolName := refPart.Get("tool_name").String()
					origName, matched, errResolve := resolver.resolve(toolName)
					if errResolve != nil {
						resolveErr = errResolve
						return false
					}
					if matched {
						path := fmt.Sprintf("content.%d.content.tool_references.%d.tool_name", index.Int(), refIndex.Int())
						body, _ = sjson.SetBytes(body, path, origName)
					}
					return true
				})
			}
		}
		return resolveErr == nil
	})
	return body, resolveErr
}

// reverseRemapOAuthToolNamesFromStreamLine reverses the tool name mapping for SSE
// stream lines, using the per-request reverseMap produced by remapOAuthToolNames.
func reverseRemapOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) ([]byte, error) {
	if len(reverseMap) == 0 {
		return line, nil
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line, nil
	}

	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line, nil
	}

	resolver := newClaudeMCPAliasResolver(reverseMap)
	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		origName, matched, errResolve := resolver.resolve(name)
		if errResolve != nil {
			return line, errResolve
		}
		if !matched {
			return line, nil
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", origName)
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		origName, matched, errResolve := resolver.resolve(toolName)
		if errResolve != nil {
			return line, errResolve
		}
		if !matched {
			return line, nil
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", origName)
	case "tool_search_tool_result":
		toolRefs := contentBlock.Get("content.tool_references")
		if !toolRefs.Exists() || !toolRefs.IsArray() {
			return line, nil
		}
		updatedPayload := payload
		var resolveErr error
		hasChange := false
		toolRefs.ForEach(func(refIndex, refPart gjson.Result) bool {
			if refPart.Get("type").String() != "tool_reference" {
				return true
			}
			toolName := refPart.Get("tool_name").String()
			origName, matched, errResolve := resolver.resolve(toolName)
			if errResolve != nil {
				resolveErr = errResolve
				return false
			}
			if matched {
				path := fmt.Sprintf("content_block.content.tool_references.%d.tool_name", refIndex.Int())
				updatedPayload, err = sjson.SetBytes(updatedPayload, path, origName)
				if err != nil {
					return false
				}
				hasChange = true
			}
			return true
		})
		if resolveErr != nil {
			return line, resolveErr
		}
		if err != nil {
			return line, fmt.Errorf("rewrite Claude OAuth MCP tool alias: %w", err)
		}
		if !hasChange {
			return line, nil
		}
		updated = updatedPayload
	default:
		return line, nil
	}
	if err != nil {
		return line, fmt.Errorf("rewrite Claude OAuth MCP tool alias: %w", err)
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...), nil
	}
	return updated, nil
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	// Collect built-in tool names from the authoritative fallback seed list and
	// augment it with any typed built-ins present in the current request body.
	builtinTools := helps.AugmentClaudeBuiltinToolRegistry(body, nil)

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			// Skip built-in tools (web_search, code_execution, etc.) which have
			// a "type" field and require their name to remain unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) || helps.IsClaudeMCPToolName(name) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] && !helps.IsClaudeMCPToolName(name) {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] || helps.IsClaudeMCPToolName(name) {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] || helps.IsClaudeMCPToolName(toolName) {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] && !helps.IsClaudeMCPToolName(nestedToolName) {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if !strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if !strings.HasPrefix(toolName, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.tool_name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
		case "tool_result":
			// Handle nested tool_reference blocks inside tool_result.content[]
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if strings.HasPrefix(nestedToolName, prefix) {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
							body, _ = sjson.SetBytes(body, nestedPath, strings.TrimPrefix(nestedToolName, prefix))
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}
