// Package registry provides Claude model definitions and lookup helpers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude []*ModelInfo `json:"claude"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

// GetStaticModelDefinitionsByChannel returns the built-in model catalog for a
// channel name, or nil when the channel is unknown.
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	if strings.EqualFold(strings.TrimSpace(channel), "claude") {
		return GetClaudeModels()
	}
	return nil
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	for _, m := range getModels().Claude {
		if m != nil && m.ID == modelID {
			return cloneModelInfo(m)
		}
	}

	return nil
}
