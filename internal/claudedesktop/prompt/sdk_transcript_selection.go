package prompt

import (
	"bytes"
	"encoding/json"
)

// The ordinary-file vao lane keeps last-prompt markers separate from content.
// A passive marker follows a later descendant, whereas an explicit marker can
// select an earlier branch. A new foreground row retires an earlier clear.
// Compaction-preserved relinking is a separate transform, not inferred here.
func sdkRemoteTranscriptSelection(journal []json.RawMessage, rows []SDKNativeMessage) (string, bool, error) {
	byID := make(map[string]SDKNativeMessage, len(rows))
	for _, row := range rows {
		byID[row.UUID] = row
	}
	var foreground, anchor string
	var explicit, cleared bool
	for _, raw := range journal {
		var entry struct {
			Type     string          `json:"type"`
			UUID     string          `json:"uuid"`
			LeafUUID json.RawMessage `json:"leafUuid"`
			Explicit json.RawMessage `json:"explicit"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			return "", false, ErrSDKSessionInvalid
		}
		if row, ok := byID[entry.UUID]; ok && row.Type == entry.Type {
			if !row.IsSidechain {
				foreground, explicit, cleared = row.UUID, false, false
			}
			if row.Type == "system" && row.Subtype == "compact_boundary" {
				anchor, explicit = "", false
			}
			continue
		}
		if entry.Type != "last-prompt" {
			continue
		}
		isExplicit := bytes.Equal(bytes.TrimSpace(entry.Explicit), []byte("true"))
		var leaf string
		if json.Unmarshal(entry.LeafUUID, &leaf) == nil && leaf != "" {
			explicit = isExplicit || explicit && leaf == anchor
			anchor, cleared = leaf, false
		} else if bytes.Equal(bytes.TrimSpace(entry.LeafUUID), []byte("null")) && isExplicit {
			anchor, explicit, cleared = "", false, true
		}
	}
	if cleared {
		return "", true, nil
	}
	leaf := anchor
	if _, ok := byID[leaf]; !ok {
		leaf = foreground
	} else if !explicit && foreground != "" && foreground != leaf {
		seen := make(map[string]bool)
		for current := foreground; current != "" && !seen[current]; {
			if current == leaf {
				leaf = foreground
				break
			}
			seen[current] = true
			row, exists := byID[current]
			if !exists || row.ParentUUID == nil {
				break
			}
			current = *row.ParentUUID
		}
	}
	seen := make(map[string]bool)
	for leaf != "" && !seen[leaf] {
		seen[leaf] = true
		row, ok := byID[leaf]
		if !ok || row.IsSidechain {
			break
		}
		if row.Type == "user" || row.Type == "assistant" {
			return leaf, false, nil
		}
		if row.ParentUUID == nil {
			break
		}
		leaf = *row.ParentUUID
	}
	return "", false, ErrSDKResumeReconstructionRequired
}
