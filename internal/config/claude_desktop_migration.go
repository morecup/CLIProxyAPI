package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

var legacyClaudeTopLevelKeys = map[string]struct{}{
	"claude-header-defaults":    {},
	"disable-claude-cloak-mode": {},
}

var legacyClaudeCredentialKeys = map[string]struct{}{
	"cloak":                      {},
	"experimental-cch-signing":   {},
	"fingerprint-profile":        {},
	"rebuild-mid-system-message": {},
}

// ValidateClaudeDesktopMigrationYAML rejects settings that selected the old
// outbound Claude Code fingerprint. They cannot be reinterpreted as Desktop
// enrollment because those two identities have different trust boundaries.
func ValidateClaudeDesktopMigrationYAML(data []byte) error {
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal(data, &document); errUnmarshal != nil {
		return nil
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := document.Content[0]
	for index := 0; index+1 < len(root.Content); index += 2 {
		key := strings.ToLower(strings.TrimSpace(root.Content[index].Value))
		value := root.Content[index+1]
		if _, legacy := legacyClaudeTopLevelKeys[key]; legacy {
			return legacyClaudeMigrationError(key)
		}
		if key != "claude-api-key" || value.Kind != yaml.SequenceNode {
			continue
		}
		for credentialIndex, credential := range value.Content {
			if credential.Kind != yaml.MappingNode {
				continue
			}
			for fieldIndex := 0; fieldIndex+1 < len(credential.Content); fieldIndex += 2 {
				field := strings.ToLower(strings.TrimSpace(credential.Content[fieldIndex].Value))
				if _, legacy := legacyClaudeCredentialKeys[field]; legacy {
					return fmt.Errorf("claude-api-key[%d].%s: %w", credentialIndex, field, legacyClaudeMigrationError(field))
				}
			}
		}
	}
	return nil
}

func legacyClaudeMigrationError(field string) error {
	return fmt.Errorf("legacy Claude Code setting %q is no longer supported; remove it, keep API keys under anthropic-compatible, and enroll first-party OAuth separately through the claude Desktop provider", field)
}
