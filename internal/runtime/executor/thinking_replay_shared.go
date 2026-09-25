package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// statusErr is a lightweight upstream status error carrying an optional Retry-After hint.
type statusErr struct {
	code       int
	msg        string
	retryAfter *time.Duration
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }

func sourceFormatEqual(from, want sdktranslator.Format) bool {
	return strings.EqualFold(strings.TrimSpace(from.String()), want.String())
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func headerValueCaseInsensitive(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// thinkingReplayScope carries the bounded replay state for one Claude-compatible request.
type thinkingReplayScope struct {
	modelFamily   string
	sessionKey    string
	snapshot      internalcache.ClaudeThinkingReplaySnapshot
	cacheReady    bool
	replayApplied bool
}

func (s thinkingReplayScope) valid() bool {
	return strings.TrimSpace(s.modelFamily) != "" && strings.TrimSpace(s.sessionKey) != ""
}

func thinkingReplaySessionKey(ctx context.Context, from sdktranslator.Format, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte) string {
	if ctx == nil {
		ctx = context.Background()
	}
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		if sessionKey, _ := helps.ClaudeCodeExecutionScope(ctx, req.Payload, opts.Headers); sessionKey != "" {
			return sessionKey
		}
	}
	if value := metadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return "execution:" + value
	}
	if value := metadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return "execution:" + value
	}
	if value := thinkingReplaySessionKeyFromPayload(body); value != "" {
		return value
	}
	if value := thinkingReplaySessionKeyFromPayload(req.Payload); value != "" {
		return value
	}
	if value := thinkingReplaySessionKeyFromHeaders(opts.Headers); value != "" {
		return value
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		if value := thinkingReplaySessionKeyFromHeaders(ginCtx.Request.Header); value != "" {
			return value
		}
	}
	return ""
}

func thinkingReplaySessionKeyFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String()); promptCacheKey != "" {
		return "prompt-cache:" + promptCacheKey
	}
	return ""
}

func thinkingReplaySessionKeyFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, headerName := range []string{"Session_id", "session_id", "Session-Id"} {
		if value := strings.TrimSpace(headerValueCaseInsensitive(headers, headerName)); value != "" {
			return "session-id:" + value
		}
	}
	if conversationID := strings.TrimSpace(headerValueCaseInsensitive(headers, "Conversation_id")); conversationID != "" {
		return "conversation_id:" + conversationID
	}
	return ""
}

// thinkingReplayIsolateSessionKey namespaces client-controlled session keys by the
// downstream API key so two callers cannot share cached reasoning by reusing
// prompt_cache_key or session headers. Trusted execution session keys keep their
// existing form. Sessions without a caller API key are disabled rather than shared.
func thinkingReplayIsolateSessionKey(ctx context.Context, sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	if strings.HasPrefix(sessionKey, "execution:") {
		return sessionKey
	}
	apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "caller:" + hex.EncodeToString(sum[:8]) + ":" + sessionKey
}

func shouldClearThinkingReplayAfterError(err error) bool {
	if err == nil {
		return false
	}
	var upstreamStatus statusErr
	if !errors.As(err, &upstreamStatus) {
		return false
	}
	statusCode := upstreamStatus.StatusCode()
	return statusCode == 400 || statusCode == 422
}

func thinkingReplayContentIsReplayable(content []byte) bool {
	root := gjson.ParseBytes(content)
	if !root.IsArray() {
		return false
	}
	hasSignedThinking := false
	hasToolUse := false
	for _, part := range root.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking":
			if strings.TrimSpace(part.Get("signature").String()) != "" {
				hasSignedThinking = true
			}
		case "tool_use":
			if strings.TrimSpace(part.Get("id").String()) != "" {
				hasToolUse = true
			}
		}
	}
	return hasSignedThinking && hasToolUse
}

func restoreThinkingReplayContent(body, cachedContent []byte) ([]byte, bool) {
	cachedParts, cachedOK := thinkingReplayNonThinkingContentParts(gjson.ParseBytes(cachedContent))
	if !cachedOK {
		return body, false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, false
	}
	messageItems := messages.Array()
	for index := len(messageItems) - 1; index >= 0; index-- {
		message := messageItems[index]
		if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "assistant") {
			continue
		}
		currentContent := message.Get("content")
		if thinkingReplayJSONEqual([]byte(currentContent.Raw), cachedContent) {
			return body, false
		}
		if thinkingReplayContentHasThinking(currentContent) {
			continue
		}
		currentParts, currentOK := thinkingReplayNonThinkingContentParts(currentContent)
		if !currentOK || !thinkingReplayCanonicalPartsEqual(currentParts, cachedParts) {
			continue
		}
		updated, errSet := sjson.SetRawBytes(body, fmt.Sprintf("messages.%d.content", index), cachedContent)
		if errSet != nil {
			return body, false
		}
		return updated, true
	}
	return body, false
}

func thinkingReplayContentHasThinking(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			return true
		}
	}
	return false
}

func thinkingReplayNonThinkingContentParts(content gjson.Result) ([][]byte, bool) {
	if !content.IsArray() {
		return nil, false
	}
	parts := make([][]byte, 0, len(content.Array()))
	hasToolUse := false
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use":
			if strings.TrimSpace(part.Get("id").String()) == "" {
				return nil, false
			}
			hasToolUse = true
		}
		canonical, ok := thinkingReplayCanonicalJSON([]byte(part.Raw))
		if !ok {
			return nil, false
		}
		parts = append(parts, canonical)
	}
	return parts, hasToolUse
}

func thinkingReplayCanonicalPartsEqual(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			return false
		}
	}
	return true
}

func thinkingReplayJSONEqual(left, right []byte) bool {
	canonicalLeft, leftOK := thinkingReplayCanonicalJSON(left)
	canonicalRight, rightOK := thinkingReplayCanonicalJSON(right)
	return leftOK && rightOK && bytes.Equal(canonicalLeft, canonicalRight)
}

func thinkingReplayCanonicalJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, false
	}
	canonical, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	return canonical, true
}

type thinkingReplayStreamBlock struct {
	raw                  []byte
	text                 strings.Builder
	thinking             strings.Builder
	signature            strings.Builder
	input                strings.Builder
	textInitialized      bool
	thinkingInitialized  bool
	signatureInitialized bool
	hasInputDelta        bool
	finished             bool
}

type thinkingReplayStreamAccumulator struct {
	blocks        map[int]*thinkingReplayStreamBlock
	observed      bool
	complete      bool
	upstreamError bool
	abandoned     bool
	bytesUsed     int
}

func newThinkingReplayStreamAccumulator() *thinkingReplayStreamAccumulator {
	return &thinkingReplayStreamAccumulator{blocks: make(map[int]*thinkingReplayStreamBlock)}
}

func (a *thinkingReplayStreamAccumulator) observe(chunk []byte) {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(payload) {
			a.abandon()
			continue
		}
		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "message_start":
			a.observed = true
		case "content_block_start":
			if !a.abandoned {
				a.observeBlockStart(root)
			}
		case "content_block_delta":
			if !a.abandoned {
				a.observeBlockDelta(root)
			}
		case "content_block_stop":
			if !a.abandoned {
				a.finishBlock(int(root.Get("index").Int()))
			}
		case "message_stop":
			a.complete = true
		case "error":
			a.upstreamError = true
			a.abandon()
		}
	}
}

func (a *thinkingReplayStreamAccumulator) observeBlockStart(root gjson.Result) {
	index := int(root.Get("index").Int())
	block := root.Get("content_block")
	if !block.IsObject() || len(a.blocks) >= internalcache.ClaudeThinkingReplayCacheMaxBlocksPerTurn {
		a.abandon()
		return
	}
	if _, exists := a.blocks[index]; exists {
		a.abandon()
		return
	}
	raw := []byte(block.Raw)
	if !a.reserveBytes(len(raw)) {
		return
	}
	a.blocks[index] = &thinkingReplayStreamBlock{raw: append([]byte(nil), raw...)}
}

func (a *thinkingReplayStreamAccumulator) observeBlockDelta(root gjson.Result) {
	index := int(root.Get("index").Int())
	block, ok := a.blocks[index]
	if !ok {
		a.abandon()
		return
	}
	delta := root.Get("delta")
	switch delta.Get("type").String() {
	case "text_delta":
		a.appendBlockText(block, &block.text, &block.textInitialized, "text", delta.Get("text").String())
	case "thinking_delta":
		a.appendBlockText(block, &block.thinking, &block.thinkingInitialized, "thinking", delta.Get("thinking").String())
	case "signature_delta":
		a.appendBlockText(block, &block.signature, &block.signatureInitialized, "signature", delta.Get("signature").String())
	case "input_json_delta":
		suffix := delta.Get("partial_json").String()
		if a.reserveBytes(len(suffix)) {
			block.input.WriteString(suffix)
			block.hasInputDelta = true
		}
	default:
		a.abandon()
	}
}

func (a *thinkingReplayStreamAccumulator) appendBlockText(block *thinkingReplayStreamBlock, builder *strings.Builder, initialized *bool, path, suffix string) {
	if !*initialized {
		initial := gjson.GetBytes(block.raw, path).String()
		if !a.reserveBytes(len(initial)) {
			return
		}
		builder.WriteString(initial)
		*initialized = true
	}
	if a.reserveBytes(len(suffix)) {
		builder.WriteString(suffix)
	}
}

func (a *thinkingReplayStreamAccumulator) finishBlock(index int) {
	block, ok := a.blocks[index]
	if !ok {
		a.abandon()
		return
	}
	if block.hasInputDelta && !gjson.Valid(block.input.String()) {
		a.abandon()
		return
	}
	block.finished = true
}

func (a *thinkingReplayStreamAccumulator) reserveBytes(count int) bool {
	if count < 0 || a.bytesUsed > internalcache.ClaudeThinkingReplayCacheMaxBytesPerSession-count {
		a.abandon()
		return false
	}
	a.bytesUsed += count
	return true
}

func (a *thinkingReplayStreamAccumulator) abandon() {
	a.abandoned = true
	a.blocks = nil
	a.bytesUsed = 0
}

func (a *thinkingReplayStreamAccumulator) content() ([]byte, bool) {
	if !a.observed || !a.complete || a.upstreamError || a.abandoned {
		return nil, false
	}
	indexes := make([]int, 0, len(a.blocks))
	for index := range a.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	parts := make([][]byte, 0, len(indexes))
	for _, index := range indexes {
		block := a.blocks[index]
		if !block.finished {
			a.abandon()
			return nil, false
		}
		raw := append([]byte(nil), block.raw...)
		var errSet error
		if block.textInitialized {
			raw, errSet = sjson.SetBytes(raw, "text", block.text.String())
		}
		if errSet == nil && block.thinkingInitialized {
			raw, errSet = sjson.SetBytes(raw, "thinking", block.thinking.String())
		}
		if errSet == nil && block.signatureInitialized {
			raw, errSet = sjson.SetBytes(raw, "signature", block.signature.String())
		}
		if errSet == nil && block.hasInputDelta {
			raw, errSet = sjson.SetRawBytes(raw, "input", []byte(block.input.String()))
		}
		if errSet != nil {
			a.abandon()
			return nil, false
		}
		parts = append(parts, raw)
	}
	content := helps.JoinRawJSONArray(parts)
	if len(content) > internalcache.ClaudeThinkingReplayCacheMaxBytesPerSession {
		a.abandon()
		return nil, false
	}
	return content, true
}

type thinkingReplayContentCacheFunc func(context.Context, thinkingReplayScope, []byte)
type thinkingReplayContentClearFunc func(context.Context, thinkingReplayScope)

func wrapThinkingReplayStream(ctx context.Context, result *cliproxyexecutor.StreamResult, scope thinkingReplayScope, cacheContent thinkingReplayContentCacheFunc, clearContent thinkingReplayContentClearFunc) *cliproxyexecutor.StreamResult {
	if result == nil || !scope.valid() {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		accumulator := newThinkingReplayStreamAccumulator()
		hasError := false
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				hasError = true
			} else {
				accumulator.observe(chunk.Payload)
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
		if hasError {
			return
		}
		if content, completed := accumulator.content(); completed {
			cacheContent(ctx, scope, content)
			return
		}
		if accumulator.upstreamError && scope.replayApplied {
			clearContent(ctx, scope)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers.Clone(), Chunks: out}
}
