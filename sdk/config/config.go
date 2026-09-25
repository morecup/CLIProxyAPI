// Package config provides the public SDK configuration API.
//
// It re-exports the server configuration types and helpers so external projects can
// embed CLIProxyAPI without importing internal packages.
package config

import internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"

type SDKConfig = internalconfig.SDKConfig

type Config = internalconfig.Config

type StreamingConfig = internalconfig.StreamingConfig
type ClaudeCodeConfig = internalconfig.ClaudeCodeConfig
type TLSConfig = internalconfig.TLSConfig
type RemoteManagement = internalconfig.RemoteManagement
type OAuthModelAlias = internalconfig.OAuthModelAlias
type PayloadConfig = internalconfig.PayloadConfig
type PayloadRule = internalconfig.PayloadRule
type PayloadFilterRule = internalconfig.PayloadFilterRule
type PayloadModelRule = internalconfig.PayloadModelRule

type ClaudeKey = internalconfig.ClaudeKey

type TLS = internalconfig.TLSConfig

const (
	DefaultPanelGitHubRepository = internalconfig.DefaultPanelGitHubRepository
)

func LoadConfig(configFile string) (*Config, error) { return internalconfig.LoadConfig(configFile) }

func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	return internalconfig.LoadConfigOptional(configFile, optional)
}

func ParseConfigBytes(data []byte) (*Config, error) { return internalconfig.ParseConfigBytes(data) }

func SaveConfigPreserveComments(configFile string, cfg *Config) error {
	return internalconfig.SaveConfigPreserveComments(configFile, cfg)
}

func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error {
	return internalconfig.SaveConfigPreserveCommentsUpdateNestedScalar(configFile, path, value)
}

func NormalizeCommentIndentation(data []byte) []byte {
	return internalconfig.NormalizeCommentIndentation(data)
}
