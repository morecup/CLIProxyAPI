package synthesizer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ConfigSynthesizer generates Auth entries from configuration API keys.
// Only the Claude provider remains in this build.
type ConfigSynthesizer struct{}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

func addWeightToAttrs(weight *int, attrs map[string]string) {
	if weight == nil {
		return
	}
	normalized := *weight
	if normalized <= 0 {
		normalized = 0
	}
	attrs[coreauth.AttributeWeight] = strconv.Itoa(normalized)
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}
	if errValidate := ctx.Config.ValidateCredentialWeights(); errValidate != nil {
		return nil, fmt.Errorf("synthesize config API key auths: %w", errValidate)
	}

	// Claude API Keys
	out = append(out, s.synthesizeClaudeKeys(ctx)...)

	return out, nil
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		base := strings.TrimSpace(ck.BaseURL)
		if key == "" && base == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		id, token := idGen.Next("claude:apikey", key, base, proxyURL, prefix, config.FormatSortedHeaders(ck.Headers))
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:claude[%s]", token),
			"config_index": strconv.Itoa(i),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		metadata := map[string]any{}
		if ck.DisableCooling != nil {
			metadata["disable_cooling"] = *ck.DisableCooling
		}
		addRequestRetryToMetadata(ck.RequestRetry, metadata)
		addRequestScopedErrorsToMetadata(ck.RequestScopedErrors, metadata)
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		addWeightToAttrs(ck.Weight, attrs)
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeClaudeModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "anthropic-compatible",
			Label:      "anthropic-compatible-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}
