package config

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkpluginstore "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

// RequestScopedErrorRule configures custom classification and handling for upstream errors.
type RequestScopedErrorRule struct {
	// Status matches the HTTP status code of the upstream response (e.g. 400).
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// Match matches substrings in the upstream error body.
	Match []string `yaml:"match,omitempty" json:"match,omitempty"`
	// MatchRegexr matches regular expressions in the upstream error body.
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	// Action specifies the handling behavior: "stop", "stop-and-cooldown", "continue", "continue-and-cooldown".
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}

// PluginsConfig holds dynamic plugin system settings.
type PluginsConfig struct {
	// Enabled toggles dynamic plugin loading.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Dir is the plugin discovery directory.
	Dir string `yaml:"dir" json:"dir"`
	// StoreSources appends third-party plugin store registries to the built-in official source.
	StoreSources []string `yaml:"store-sources,omitempty" json:"store-sources,omitempty"`
	// StoreAuth defines optional auth rules for plugin store registry, metadata, and artifact requests.
	StoreAuth []sdkpluginstore.AuthConfig `yaml:"store-auth,omitempty" json:"store-auth,omitempty"`
	// AuthRevision changes when Home-managed plugin credentials change.
	AuthRevision int64 `yaml:"auth-revision,omitempty" json:"auth-revision,omitempty"`
	// Configs stores per-plugin instance configuration by plugin ID.
	Configs map[string]PluginInstanceConfig `yaml:"configs" json:"configs"`
}

// PluginInstanceConfig stores host-owned plugin settings and the original plugin YAML subtree.
type PluginInstanceConfig struct {
	// Enabled toggles this plugin instance. Nil is normalized to false during YAML parsing.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Priority controls plugin startup and routing order.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Raw preserves the full original plugin configuration YAML subtree.
	Raw yaml.Node `yaml:"-" json:"-"`
}

// UnmarshalYAML extracts host-owned fields while preserving the full original YAML node.
func (c *PluginInstanceConfig) UnmarshalYAML(value *yaml.Node) error {
	if c == nil {
		return nil
	}

	c.Priority = 0
	defaultEnabled := false
	c.Enabled = &defaultEnabled

	if value == nil || value.Kind == 0 {
		c.Raw = *defaultPluginInstanceConfigNode()
		return nil
	}

	c.Raw = *deepCopyNode(value)
	if value.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i]
		node := value.Content[i+1]
		if key == nil {
			continue
		}
		switch key.Value {
		case "enabled":
			var enabled bool
			if errDecodeEnabled := node.Decode(&enabled); errDecodeEnabled != nil {
				return fmt.Errorf("parse plugin enabled: %w", errDecodeEnabled)
			}
			c.Enabled = &enabled
		case "priority":
			var priority int
			if errDecodePriority := node.Decode(&priority); errDecodePriority != nil {
				return fmt.Errorf("parse plugin priority: %w", errDecodePriority)
			}
			c.Priority = priority
		}
	}

	return nil
}

// MarshalYAML returns the preserved raw plugin YAML subtree for lossless config output.
func (c PluginInstanceConfig) MarshalYAML() (any, error) {
	if c.Raw.Kind == 0 {
		return defaultPluginInstanceConfigNode(), nil
	}
	return deepCopyNode(&c.Raw), nil
}

func defaultPluginInstanceConfigNode() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: []*yaml.Node{},
	}
}

// ClaudeDesktopConfig selects the immutable Desktop profile and the directory
// reserved for account-scoped runtime state. The profile is shared read-only;
// mutable identity and session state must never be stored inside it.
type ClaudeDesktopConfig struct {
	BundlePath             string                        `yaml:"bundle-path,omitempty" json:"bundle-path,omitempty"`
	StatePath              string                        `yaml:"state-path,omitempty" json:"state-path,omitempty"`
	RolloutAuthIDs         []string                      `yaml:"rollout-auth-ids,omitempty" json:"rollout-auth-ids,omitempty"`
	EmergencyStop          bool                          `yaml:"emergency-stop,omitempty" json:"emergency-stop,omitempty"`
	MachineProfiles        []ClaudeDesktopMachineProfile `yaml:"machine-profiles,omitempty" json:"machine-profiles,omitempty"`
	MachineProfileBindings map[string]string             `yaml:"machine-profile-bindings,omitempty" json:"machine-profile-bindings,omitempty"`
}

// ClaudeDesktopMachineProfile describes a measured host assigned to one or
// more account runtimes. Zero-valued fields inherit the local host snapshot.
type ClaudeDesktopMachineProfile struct {
	ID                   string                         `yaml:"id" json:"id"`
	TotalMemoryBytes     uint64                         `yaml:"total-memory-bytes,omitempty" json:"total-memory-bytes,omitempty"`
	AvailableMemoryBytes uint64                         `yaml:"available-memory-bytes,omitempty" json:"available-memory-bytes,omitempty"`
	CPUModel             string                         `yaml:"cpu-model,omitempty" json:"cpu-model,omitempty"`
	OSBuild              string                         `yaml:"os-build,omitempty" json:"os-build,omitempty"`
	OSRelease            string                         `yaml:"os-release,omitempty" json:"os-release,omitempty"`
	OSVersion            string                         `yaml:"os-version,omitempty" json:"os-version,omitempty"`
	SDKProcess           ClaudeDesktopSDKProcessProfile `yaml:"sdk-process,omitempty" json:"sdk-process,omitempty"`
}

// ClaudeDesktopSDKProcessProfile contains an optional measured embedded-Node
// process snapshot. Values must come from the Desktop runtime, not Go MemStats.
type ClaudeDesktopSDKProcessProfile struct {
	UptimeSeconds         float64 `yaml:"uptime-seconds,omitempty" json:"uptime-seconds,omitempty"`
	RSS                   uint64  `yaml:"rss-bytes,omitempty" json:"rss-bytes,omitempty"`
	FootprintBytes        uint64  `yaml:"footprint-bytes,omitempty" json:"footprint-bytes,omitempty"`
	CommitBytes           uint64  `yaml:"commit-bytes,omitempty" json:"commit-bytes,omitempty"`
	PeakFootprintBytes    uint64  `yaml:"peak-footprint-bytes,omitempty" json:"peak-footprint-bytes,omitempty"`
	MemorySampleAgeMS     int64   `yaml:"memory-sample-age-ms,omitempty" json:"memory-sample-age-ms,omitempty"`
	HeapTotal             uint64  `yaml:"heap-total-bytes,omitempty" json:"heap-total-bytes,omitempty"`
	HeapUsed              uint64  `yaml:"heap-used-bytes,omitempty" json:"heap-used-bytes,omitempty"`
	External              uint64  `yaml:"external-bytes,omitempty" json:"external-bytes,omitempty"`
	ArrayBuffers          uint64  `yaml:"array-buffers-bytes,omitempty" json:"array-buffers-bytes,omitempty"`
	ConstrainedMemory     uint64  `yaml:"constrained-memory-bytes,omitempty" json:"constrained-memory-bytes,omitempty"`
	CPUUserMicroseconds   int64   `yaml:"cpu-user-microseconds,omitempty" json:"cpu-user-microseconds,omitempty"`
	CPUSystemMicroseconds int64   `yaml:"cpu-system-microseconds,omitempty" json:"cpu-system-microseconds,omitempty"`
}

// TLSConfig holds HTTPS server settings.
type TLSConfig struct {
	// Enable toggles HTTPS server mode.
	Enable bool `yaml:"enable" json:"enable"`
	// Cert is the path to the TLS certificate file.
	Cert string `yaml:"cert" json:"cert"`
	// Key is the path to the TLS private key file.
	Key string `yaml:"key" json:"key"`
}

// PprofConfig holds pprof HTTP server settings.
type PprofConfig struct {
	// Enable toggles the pprof HTTP debug server.
	Enable bool `yaml:"enable" json:"enable"`
	// Addr is the host:port address for the pprof HTTP server.
	Addr string `yaml:"addr" json:"addr"`
}

// RemoteManagement holds management API configuration under 'remote-management'.
type RemoteManagement struct {
	// AllowRemote toggles remote (non-localhost) access to management API.
	AllowRemote bool `yaml:"allow-remote"`
	// SecretKey is the management key (plaintext or bcrypt hashed). YAML key intentionally 'secret-key'.
	SecretKey string `yaml:"secret-key"`
	// DisableControlPanel skips serving and syncing the bundled management UI when true.
	DisableControlPanel bool `yaml:"disable-control-panel"`
	// DisableAutoUpdatePanel disables automatic periodic background updates of the management panel asset from GitHub.
	// When false (the default), the background updater remains enabled; when true, the panel is only downloaded on first access if missing.
	DisableAutoUpdatePanel bool `yaml:"disable-auto-update-panel"`
	// PanelGitHubRepository overrides the GitHub repository used to fetch the management panel asset.
	// Accepts either a repository URL (https://github.com/org/repo) or an API releases endpoint.
	PanelGitHubRepository string `yaml:"panel-github-repository"`
}

// RoutingConfig configures how credentials are selected for requests.
type RoutingConfig struct {
	// Strategy selects the credential selection strategy.
	// Supported values: "round-robin" (default), "weighted-round-robin", "fill-first".
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// SessionAffinity enables universal session-sticky routing for all clients.
	// Explicit Claude Code, Codex, OpenCode, and pi session headers are preferred,
	// followed by prompt_cache_key, Responses conversation IDs, legacy body IDs,
	// execution or derived session identity, and the existing message-content hash fallback.
	// Automatic failover is always enabled when bound auth becomes unavailable.
	SessionAffinity bool `yaml:"session-affinity,omitempty" json:"session-affinity,omitempty"`

	// SessionAffinityTTL specifies how long session-to-auth bindings are retained.
	// Default: 1h. Accepts duration strings like "30m", "1h", "2h30m".
	SessionAffinityTTL string `yaml:"session-affinity-ttl,omitempty" json:"session-affinity-ttl,omitempty"`
}

// OAuthModelAlias defines a model ID alias for a specific channel.
// It maps the upstream model name (Name) to the client-visible alias (Alias).
// When Fork is true, the alias is added as an additional model in listings while
// keeping the original model ID available.
type OAuthModelAlias struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
	Fork  bool   `yaml:"fork,omitempty" json:"fork,omitempty"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

// PayloadConfig defines default and override parameter rules applied to provider payloads.
type PayloadConfig struct {
	// Default defines rules that only set parameters when they are missing in the payload.
	Default []PayloadRule `yaml:"default" json:"default"`
	// DefaultRaw defines rules that set raw JSON values only when they are missing.
	DefaultRaw []PayloadRule `yaml:"default-raw" json:"default-raw"`
	// Override defines rules that always set parameters, overwriting any existing values.
	Override []PayloadRule `yaml:"override" json:"override"`
	// OverrideRaw defines rules that always set raw JSON values, overwriting any existing values.
	OverrideRaw []PayloadRule `yaml:"override-raw" json:"override-raw"`
	// Filter defines rules that remove parameters from the payload by JSON path.
	Filter []PayloadFilterRule `yaml:"filter" json:"filter"`
}

// PayloadFilterRule describes a rule to remove specific JSON paths from matching model payloads.
type PayloadFilterRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params lists JSON paths (gjson/sjson syntax) to remove from the payload.
	Params []string `yaml:"params" json:"params"`
}

// PayloadRule describes a single rule targeting a list of models with parameter updates.
type PayloadRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params maps JSON paths (gjson/sjson syntax) to values written into the payload.
	// For *-raw rules, values are treated as raw JSON fragments (strings are used as-is).
	Params map[string]any `yaml:"params" json:"params"`
}

// PayloadModelRule ties a model name pattern to a specific translator protocol.
type PayloadModelRule struct {
	// Name is the model name or wildcard pattern (e.g., "claude-*", "*-5").
	Name string `yaml:"name" json:"name"`
	// Protocol restricts the rule to a specific translator format (e.g., "claude", "responses").
	Protocol string `yaml:"protocol" json:"protocol"`
	// Headers restricts the rule to requests whose headers match all configured wildcard patterns.
	Headers map[string]string `yaml:"headers" json:"headers"`
	// FromProtocol restricts the rule to a specific source protocol (e.g., "claude", "responses").
	FromProtocol string `yaml:"from-protocol" json:"from-protocol"`
	// Match requires payload JSON paths to equal the configured values.
	Match []map[string]any `yaml:"match" json:"match"`
	// NotMatch requires payload JSON paths to not equal the configured values.
	NotMatch []map[string]any `yaml:"not-match" json:"not-match"`
	// Exist requires payload JSON paths to exist and not be null.
	Exist []string `yaml:"exist" json:"exist"`
	// NotExist requires payload JSON paths to be missing or null.
	NotExist []string `yaml:"not-exist" json:"not-exist"`
}

// ClaudeKey represents the configuration for a Claude API key,
// including the API key itself and an optional base URL for the API endpoint.
type ClaudeKey struct {
	// APIKey is the authentication key for accessing Claude API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/claude-sonnet-4").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Claude API endpoint.
	// If empty, the default Claude API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []ClaudeModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling overrides the global cooling policy for this credential when set.
	// True disables auth/model cooldowns; false explicitly enables them.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry optionally overrides the global request-retry for this credential.
	// Nil or a negative value means "use the global request-retry". 0 disables additional retry rounds.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

func (k ClaudeKey) GetAPIKey() string { return k.APIKey }

func (k ClaudeKey) GetBaseURL() string { return k.BaseURL }

func (k ClaudeKey) GetPrefix() string { return k.Prefix }

func (k ClaudeKey) GetProxyURL() string { return k.ProxyURL }

// ClaudeModel describes a mapping between an alias and the actual upstream model name.
type ClaudeModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// IsCompat preserves thinking blocks with empty signatures for compatible upstreams
	// and enables provider-aware signed-thinking replay for Claude-compatible API-key models.
	// Default false keeps the normal signature validation behavior.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// Thinking configures the thinking/reasoning capability for this model.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m ClaudeModel) GetName() string { return m.Name }

func (m ClaudeModel) GetAlias() string { return m.Alias }

func (m ClaudeModel) GetDisplayName() string   { return m.DisplayName }
func (m ClaudeModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m ClaudeModel) GetForceMapping() bool    { return m.ForceMapping }
func (m ClaudeModel) GetIsCompat() bool        { return m.IsCompat }

func (m ClaudeModel) GetThinking() *registry.ThinkingSupport { return m.Thinking }
