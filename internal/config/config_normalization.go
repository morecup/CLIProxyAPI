package config

import (
	"sort"
	"strings"

	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
)

// NormalizePluginsConfig applies default plugin configuration values.
func (cfg *Config) NormalizePluginsConfig() {
	if cfg == nil {
		return
	}
	cfg.Plugins.Dir = strings.TrimSpace(cfg.Plugins.Dir)
	if cfg.Plugins.Dir == "" {
		cfg.Plugins.Dir = defaultPluginsDir
	}
	if len(cfg.Plugins.StoreSources) > 0 {
		sources := make([]string, 0, len(cfg.Plugins.StoreSources))
		for _, source := range cfg.Plugins.StoreSources {
			source = strings.TrimSpace(source)
			if source == "" {
				continue
			}
			sources = append(sources, source)
		}
		cfg.Plugins.StoreSources = sources
	}
	cfg.Plugins.StoreAuth = sdkpluginstore.NormalizeAuthConfigs(cfg.Plugins.StoreAuth)
	if cfg.Plugins.Configs == nil {
		cfg.Plugins.Configs = map[string]PluginInstanceConfig{}
	}
}

func (cfg *Config) SanitizeClaudeDesktop() {
	if cfg == nil {
		return
	}
	cfg.ClaudeDesktop.BundlePath = strings.TrimSpace(cfg.ClaudeDesktop.BundlePath)
	cfg.ClaudeDesktop.StatePath = strings.TrimSpace(cfg.ClaudeDesktop.StatePath)
	clean := make([]string, 0, len(cfg.ClaudeDesktop.RolloutAuthIDs))
	seen := make(map[string]struct{}, len(cfg.ClaudeDesktop.RolloutAuthIDs))
	for _, authID := range cfg.ClaudeDesktop.RolloutAuthIDs {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if _, exists := seen[authID]; exists {
			continue
		}
		seen[authID] = struct{}{}
		clean = append(clean, authID)
	}
	cfg.ClaudeDesktop.RolloutAuthIDs = clean

	profiles := make([]ClaudeDesktopMachineProfile, 0, len(cfg.ClaudeDesktop.MachineProfiles))
	profileIDs := make(map[string]struct{}, len(cfg.ClaudeDesktop.MachineProfiles))
	for _, profile := range cfg.ClaudeDesktop.MachineProfiles {
		profile.ID = strings.TrimSpace(profile.ID)
		if profile.ID == "" {
			continue
		}
		if _, exists := profileIDs[profile.ID]; exists {
			continue
		}
		profile.CPUModel = strings.TrimSpace(profile.CPUModel)
		profile.OSBuild = strings.TrimSpace(profile.OSBuild)
		profile.OSRelease = strings.TrimSpace(profile.OSRelease)
		profile.OSVersion = strings.TrimSpace(profile.OSVersion)
		if profile.TotalMemoryBytes > 0 && profile.AvailableMemoryBytes > profile.TotalMemoryBytes {
			profile.AvailableMemoryBytes = profile.TotalMemoryBytes
		}
		profileIDs[profile.ID] = struct{}{}
		profiles = append(profiles, profile)
	}
	cfg.ClaudeDesktop.MachineProfiles = profiles

	bindings := make(map[string]string, len(cfg.ClaudeDesktop.MachineProfileBindings))
	for authID, profileID := range cfg.ClaudeDesktop.MachineProfileBindings {
		authID = strings.TrimSpace(authID)
		profileID = strings.TrimSpace(profileID)
		if authID == "" || profileID == "" {
			continue
		}
		bindings[authID] = profileID
	}
	if len(bindings) == 0 {
		bindings = nil
	}
	cfg.ClaudeDesktop.MachineProfileBindings = bindings
}

// SanitizeOAuthModelAlias normalizes and deduplicates global OAuth model name aliases.
// It trims whitespace, normalizes channel keys to lower-case, drops empty entries,
// allows multiple aliases per upstream name, and ensures aliases are unique within each channel.
func (cfg *Config) SanitizeOAuthModelAlias() {
	if cfg == nil || len(cfg.OAuthModelAlias) == 0 {
		return
	}
	out := make(map[string][]OAuthModelAlias, len(cfg.OAuthModelAlias))
	for rawChannel, aliases := range cfg.OAuthModelAlias {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(aliases) == 0 {
			continue
		}
		seenAlias := make(map[string]struct{}, len(aliases))
		clean := make([]OAuthModelAlias, 0, len(aliases))
		for _, entry := range aliases {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, ok := seenAlias[aliasKey]; ok {
				continue
			}
			seenAlias[aliasKey] = struct{}{}
			clean = append(clean, OAuthModelAlias{
				Name:         name,
				Alias:        alias,
				Fork:         entry.Fork,
				DisplayName:  strings.TrimSpace(entry.DisplayName),
				ForceMapping: entry.ForceMapping,
			})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	cfg.OAuthModelAlias = out
}

// SanitizeOAuthRequestScopedErrors normalizes and validates global OAuth request-scoped error rules.
// It trims whitespace, normalizes channel keys to lower-case, validates status/action, and drops invalid rules.
func (cfg *Config) SanitizeOAuthRequestScopedErrors() {
	if cfg == nil || len(cfg.OAuthRequestScopedErrors) == 0 {
		return
	}
	out := make(map[string][]RequestScopedErrorRule, len(cfg.OAuthRequestScopedErrors))
	for rawChannel, rules := range cfg.OAuthRequestScopedErrors {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(rules) == 0 {
			continue
		}
		clean := make([]RequestScopedErrorRule, 0, len(rules))
		for _, r := range rules {
			action := strings.ToLower(strings.TrimSpace(r.Action))
			match := make([]string, 0, len(r.Match))
			for _, m := range r.Match {
				if tm := strings.TrimSpace(m); tm != "" {
					match = append(match, tm)
				}
			}
			matchRegexr := make([]string, 0, len(r.MatchRegexr))
			for _, re := range r.MatchRegexr {
				if tre := strings.TrimSpace(re); tre != "" {
					matchRegexr = append(matchRegexr, tre)
				}
			}
			if r.Status <= 0 || (len(match) == 0 && len(matchRegexr) == 0) || action == "" {
				continue
			}
			clean = append(clean, RequestScopedErrorRule{
				Status:      r.Status,
				Match:       match,
				MatchRegexr: matchRegexr,
				Action:      action,
			})
		}
		if len(clean) > 0 {
			out[channel] = clean
		}
	}
	if len(out) == 0 {
		cfg.OAuthRequestScopedErrors = nil
		return
	}
	cfg.OAuthRequestScopedErrors = out
}

// SanitizeClaudeKeys normalizes headers for Claude credentials.
func (cfg *Config) SanitizeClaudeKeys() {
	if cfg == nil || len(cfg.ClaudeKey) == 0 {
		return
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
	}
}

// FormatSortedHeaders serializes headers deterministically with null byte separators.
func FormatSortedHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(headers[k])
		b.WriteByte(0)
	}
	return b.String()
}

func normalizeModelPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

// NormalizeHeaders trims header keys and values and removes empty pairs.
func NormalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	clean := make(map[string]string, len(headers))
	for k, v := range headers {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// NormalizeExcludedModels trims, lowercases, and deduplicates model exclusion patterns.
// It preserves the order of first occurrences and drops empty entries.
func NormalizeExcludedModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeOAuthExcludedModels cleans provider -> excluded models mappings by normalizing provider keys
// and applying model exclusion normalization to each entry.
func NormalizeOAuthExcludedModels(entries map[string][]string) map[string][]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for provider, models := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		normalized := NormalizeExcludedModels(models)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
