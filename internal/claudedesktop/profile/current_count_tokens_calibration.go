package profile

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	CountTokensLayoutV2255313           = "v2255313-opus-5-5"
	currentCountTokensToolCatalogSHA256 = "dbf6dc100d0a558c38b47acd729618dd08e7ee466e3910b19c926a64cf82ed84"
)

//go:embed v2255313.count_tokens_tools.b64
var currentCountTokensToolCatalogBase64 string

type currentCountTokensToolRequest struct {
	Name  string            `json:"name"`
	Tools []json.RawMessage `json:"tools"`
}

type currentCountTokensToolCatalog struct {
	SchemaVersion  int                             `json:"schema_version"`
	DesktopVersion string                          `json:"desktop_version"`
	Source         string                          `json:"source"`
	ToolRequests   []currentCountTokensToolRequest `json:"tool_requests"`
}

var (
	currentCountTokensCatalogOnce sync.Once
	currentCountTokensCatalog     currentCountTokensToolCatalog
	currentCountTokensCatalogErr  error
)

func builtinCurrentCountTokensToolCatalog(catalogID, model string) (CountTokensCalibrationTools, error) {
	if catalogID != CountTokensLayoutV2255313 || strings.TrimSpace(model) != "claude-opus-5-5" {
		return CountTokensCalibrationTools{}, fmt.Errorf("claude desktop profile: no observed count_tokens calibration catalog %q for model %q", catalogID, model)
	}
	catalog, errCatalog := loadCurrentCountTokensToolCatalog()
	if errCatalog != nil {
		return CountTokensCalibrationTools{}, errCatalog
	}
	toolRequests := make([][]json.RawMessage, len(catalog.ToolRequests))
	for index, request := range catalog.ToolRequests {
		toolRequests[index] = cloneRawMessages(request.Tools)
	}
	return CountTokensCalibrationTools{
		Layout:           CountTokensLayoutV2255313,
		ExpectedRequests: 39,
		ToolRequests:     toolRequests,
	}, nil
}

func loadCurrentCountTokensToolCatalog() (currentCountTokensToolCatalog, error) {
	currentCountTokensCatalogOnce.Do(func() {
		compressed, errDecode := base64.StdEncoding.DecodeString(strings.TrimSpace(currentCountTokensToolCatalogBase64))
		if errDecode != nil {
			currentCountTokensCatalogErr = fmt.Errorf("claude desktop profile: decode current count_tokens calibration catalog: %w", errDecode)
			return
		}
		reader, errGzip := gzip.NewReader(bytes.NewReader(compressed))
		if errGzip != nil {
			currentCountTokensCatalogErr = fmt.Errorf("claude desktop profile: open current count_tokens calibration catalog: %w", errGzip)
			return
		}
		data, errRead := io.ReadAll(reader)
		errClose := reader.Close()
		if errRead != nil {
			currentCountTokensCatalogErr = fmt.Errorf("claude desktop profile: read current count_tokens calibration catalog: %w", errRead)
			return
		}
		if errClose != nil {
			currentCountTokensCatalogErr = fmt.Errorf("claude desktop profile: close current count_tokens calibration catalog: %w", errClose)
			return
		}
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != currentCountTokensToolCatalogSHA256 {
			currentCountTokensCatalogErr = fmt.Errorf("claude desktop profile: current count_tokens calibration catalog digest %s does not match %s", got, currentCountTokensToolCatalogSHA256)
			return
		}
		if errJSON := json.Unmarshal(data, &currentCountTokensCatalog); errJSON != nil {
			currentCountTokensCatalogErr = fmt.Errorf("claude desktop profile: decode current count_tokens calibration catalog JSON: %w", errJSON)
			return
		}
		currentCountTokensCatalogErr = validateCurrentCountTokensToolCatalog(currentCountTokensCatalog)
	})
	return currentCountTokensCatalog, currentCountTokensCatalogErr
}

func validateCurrentCountTokensToolCatalog(catalog currentCountTokensToolCatalog) error {
	if catalog.SchemaVersion != 2 || catalog.DesktopVersion != "2.2553.13" ||
		catalog.Source != "sanitized-recorder-label-2026-09-23-opus-5-5-count-token-tool-catalog" {
		return fmt.Errorf("claude desktop profile: current count_tokens calibration catalog identity is invalid")
	}
	wantNames := []string{
		"mcp-115", "skill-preload", "builtin-17",
		"single-ArtifactComments", "single-ArtifactData", "single-CronCreate", "single-CronDelete",
		"single-CronList", "single-DesignSync", "single-EnterPlanMode", "single-EnterWorktree",
		"single-ExitPlanMode", "single-ExitWorktree", "single-ListPlugins", "single-ListSkills",
		"single-Monitor", "single-NotebookEdit", "single-PushNotification", "single-RemoteTrigger",
		"single-SearchPlugins", "single-SearchSkills", "single-SendMessage", "single-SuggestPluginInstall",
		"single-TaskStop", "single-WebSearch", "single-WebFetch", "single-Skill",
	}
	if len(catalog.ToolRequests) != len(wantNames) {
		return fmt.Errorf("claude desktop profile: current count_tokens tool request count is %d, want %d", len(catalog.ToolRequests), len(wantNames))
	}
	for requestIndex, request := range catalog.ToolRequests {
		if request.Name != wantNames[requestIndex] {
			return fmt.Errorf("claude desktop profile: current count_tokens tool request %d is %q, want %q", requestIndex, request.Name, wantNames[requestIndex])
		}
		wantCount := 1
		switch requestIndex {
		case 0:
			wantCount = 115
		case 2:
			wantCount = 17
		}
		if len(request.Tools) != wantCount {
			return fmt.Errorf("claude desktop profile: current count_tokens tool request %q has %d tools, want %d", request.Name, len(request.Tools), wantCount)
		}
		for toolIndex, tool := range request.Tools {
			if !json.Valid(tool) || !strings.HasPrefix(strings.TrimSpace(string(tool)), "{") {
				return fmt.Errorf("claude desktop profile: current count_tokens request %q tool %d is invalid JSON", request.Name, toolIndex)
			}
			var shape struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"input_schema"`
			}
			if errTool := json.Unmarshal(tool, &shape); errTool != nil || strings.TrimSpace(shape.Name) == "" || !json.Valid(shape.InputSchema) {
				return fmt.Errorf("claude desktop profile: current count_tokens request %q tool %d has an invalid shape", request.Name, toolIndex)
			}
		}
	}
	return nil
}
