package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strings"
)

// This is a local observer bound, not a native tool-input or request limit.
const maxSDKTokenBlockBytes = 2 * 1024 * 1024

type sdkTokenBlock struct {
	textHash        hash.Hash
	usageExcluded   bool
	kind            string
	units           int64
	nameUnits       int64
	input           []byte
	estimate        SDKTokenEstimate
	closed, invalid bool
}

type sdkResponseTokens struct {
	bufferedInputBytes                  int
	usage                               SDKTokenUsage
	inputSeen, outputSeen, invalidUsage bool
	synthetic                           bool
	blocks                              map[int]*sdkTokenBlock
}

// Native Aj keeps a previous positive input/cache value when a delta supplies
// zero, while output_tokens (including zero) replaces it. Cache creation can
// be supplied through the ephemeral 1h/5m breakdown instead of its total.
func (r *sdkResponseTokens) observeUsage(raw json.RawMessage) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return
	}
	var value struct {
		Input   *int64 `json:"input_tokens"`
		Output  *int64 `json:"output_tokens"`
		Created *int64 `json:"cache_creation_input_tokens"`
		Read    *int64 `json:"cache_read_input_tokens"`
		Cache   struct {
			Hour *int64 `json:"ephemeral_1h_input_tokens"`
			Five *int64 `json:"ephemeral_5m_input_tokens"`
		} `json:"cache_creation"`
	}
	if json.Unmarshal(raw, &value) != nil {
		r.invalidUsage = true
		return
	}
	for _, n := range []*int64{value.Input, value.Output, value.Created, value.Read, value.Cache.Hour, value.Cache.Five} {
		if n != nil && (*n < 0 || *n > 1<<50) {
			r.invalidUsage = true
			return
		}
	}
	if value.Input != nil {
		r.inputSeen = true
		if *value.Input > 0 {
			r.usage.InputTokens = *value.Input
		}
	}
	if value.Output != nil {
		r.usage.OutputTokens, r.outputSeen = *value.Output, true
	}
	var created int64
	if value.Cache.Hour != nil {
		created += *value.Cache.Hour
	}
	if value.Cache.Five != nil {
		created += *value.Cache.Five
	}
	if value.Created != nil && *value.Created > 0 {
		r.usage.CacheCreationInputTokens = *value.Created
	} else if created > 0 {
		r.usage.CacheCreationInputTokens = created
	}
	if value.Read != nil && *value.Read > 0 {
		r.usage.CacheReadInputTokens = *value.Read
	}
}

func (r *sdkResponseTokens) knownUsage() bool { return r.inputSeen && r.outputSeen && !r.invalidUsage }

func (r *sdkResponseTokens) start(index int, raw json.RawMessage) {
	if index < 0 || index >= 4096 {
		return
	}
	if r.blocks == nil {
		r.blocks = make(map[int]*sdkTokenBlock)
	}
	if r.blocks[index] != nil {
		r.blocks[index].invalid = true
		return
	}
	var value struct{ Type, Name string }
	if json.Unmarshal(raw, &value) != nil {
		return
	}
	b := &sdkTokenBlock{kind: value.Type}
	switch value.Type {
	case "text", "thinking":
		// Native stream initialization deliberately discards initial text.
		if value.Type == "text" {
			b.textHash = sha256.New()
		}
	case "tool_use":
		b.nameUnits = sdkTextUnits(value.Name)
	default:
		n, ok := sdkBlockTokens(raw, 0)
		b.estimate = SDKTokenEstimate{Tokens: n, Known: ok}
	}
	r.blocks[index] = b
}

func (r *sdkResponseTokens) delta(index int, raw json.RawMessage) {
	b := r.blocks[index]
	if b == nil || b.closed || b.invalid {
		return
	}
	var value struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
		Input    string `json:"partial_json"`
	}
	if json.Unmarshal(raw, &value) != nil {
		b.invalid = true
		r.bufferedInputBytes -= len(b.input)
		b.input = nil
		return
	}
	switch value.Type {
	case "text_delta", "thinking_delta", "signature_delta", "input_json_delta":
	case "citations_delta":
		return
	default:
		// The pinned query parser ignores unknown delta kinds.
		return
	}
	switch {
	case b.kind == "text" && value.Type == "text_delta":
		b.units += sdkTextUnits(value.Text)
		if b.textHash != nil {
			_, _ = b.textHash.Write([]byte(value.Text))
		}
	case b.kind == "thinking" && value.Type == "thinking_delta":
		b.units += sdkTextUnits(value.Thinking)
	case b.kind == "thinking" && value.Type == "signature_delta":
	case b.kind == "redacted_thinking" && value.Type == "thinking_delta":
	case b.kind == "tool_use" && value.Type == "input_json_delta":
		if len(value.Input) > maxSDKTokenBlockBytes-r.bufferedInputBytes {
			r.bufferedInputBytes -= len(b.input)
			b.invalid, b.input = true, nil
			return
		}
		b.input = append(b.input, value.Input...)
		r.bufferedInputBytes += len(value.Input)
	default:
		r.bufferedInputBytes -= len(b.input)
		b.invalid, b.input = true, nil
	}
}

func (r *sdkResponseTokens) close(index int) SDKTokenEstimate {
	b := r.blocks[index]
	if b == nil || b.closed {
		return SDKTokenEstimate{}
	}
	b.closed = true
	if b.textHash != nil {
		b.usageExcluded = sdkUsageSentinelHash(hex.EncodeToString(b.textHash.Sum(nil)))
		b.textHash = nil
	}
	defer func() { r.bufferedInputBytes -= len(b.input); b.input = nil }()
	if b.invalid {
		return SDKTokenEstimate{}
	}
	switch b.kind {
	case "text", "thinking":
		return SDKTokenEstimate{Tokens: sdkRoundedTokens(b.units), Known: true}
	case "tool_use":
		input := b.input
		if len(input) == 0 || bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
			input = []byte(`{}`)
		}
		if !json.Valid(input) {
			return SDKTokenEstimate{}
		}
		n, ok := sdkJSONUnits(input, 0)
		return SDKTokenEstimate{Tokens: sdkRoundedTokens(n + b.nameUnits), Known: ok && b.nameUnits != 0}
	default:
		return b.estimate
	}
}

func sdkUsageSentinelHash(hash string) bool {
	switch hash {
	case "537b0a1dcbd6cd3aca54b52cc75b25a994abc303bd7afe8ef665bb545ef013b6",
		"7c43783e9e0ece33ff98eb4956ec1db1f51850098d921627c439921679e46005",
		"e7b2f2e48ceee0f6783bfaabca3e3721587b92fc816118de5a27ae336613cdda",
		"8621ef054998d150ed119a1752d9599c7eb404ae98eb50396268a99a055d019c",
		"85caba2f4e18966971684fe6e699d5ae2415abee297b0901c721711030e77396":
		return true
	}
	return false
}

func sdkUsageContentExcluded(raw json.RawMessage) bool {
	var content []struct{ Type, Text string }
	if json.Unmarshal(raw, &content) != nil || len(content) == 0 || content[0].Type != "text" {
		return false
	}
	sum := sha256.Sum256([]byte(content[0].Text))
	return sdkUsageSentinelHash(hex.EncodeToString(sum[:]))
}

func (c *call) observeSDKUserTokens(body []byte) {
	h := c.state.sdk.history
	if h == nil || len(c.sdkUserHistoryOffsets) == 0 {
		return
	}
	var root struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if json.Unmarshal(body, &root) != nil || len(root.Messages) == 0 {
		return
	}
	last := root.Messages[len(root.Messages)-1]
	if last.Role != "user" {
		return
	}
	var estimates []SDKTokenEstimate
	if len(c.resultIDs) == 0 {
		estimates = []SDKTokenEstimate{EstimateSDKContent(last.Content)}
	} else {
		start := len(root.Messages) - 1
		for start > 0 && root.Messages[start-1].Role == "user" {
			start--
		}
		for _, row := range root.Messages[start:] {
			var blocks []json.RawMessage
			if json.Unmarshal(row.Content, &blocks) != nil {
				return
			}
			for _, block := range blocks {
				var value struct {
					Type string
					ID   string `json:"tool_use_id"`
				}
				if json.Unmarshal(block, &value) != nil || value.Type != "tool_result" {
					return
				}
				if len(estimates) >= len(c.resultIDs) || digest(strings.TrimSpace(value.ID)) != c.resultIDs[len(estimates)] {
					return
				}
				n, ok := sdkBlockTokens(block, 0)
				estimates = append(estimates, SDKTokenEstimate{Tokens: n, Known: ok})
			}
		}
	}
	if len(estimates) != len(c.sdkUserHistoryOffsets) {
		return
	}
	for i, offset := range c.sdkUserHistoryOffsets {
		if offset < 0 || offset >= len(h.messages) || h.messages[offset].Type != "user" {
			return
		}
		h.messages[offset].TokenEstimate = estimates[i]
	}
}
