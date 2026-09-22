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

const countTokensToolCatalogSHA256 = "333af6db5e3b3ebcbbc182c1aee7dd52c12bd2e415862e214eb6219bf7d403e8"

//go:embed v140609.count_tokens_tools.b64
var countTokensToolCatalogBase64 string

type countTokensToolCatalog struct {
	SchemaVersion         int               `json:"schema_version"`
	DesktopVersion        string            `json:"desktop_version"`
	Source                string            `json:"source"`
	BuiltinTools          []json.RawMessage `json:"builtin_tools"`
	MCPTools              []json.RawMessage `json:"mcp_tools"`
	SingleTools           []json.RawMessage `json:"single_tools"`
	HaikuExtraSingleTools []json.RawMessage `json:"haiku_extra_single_tools"`
}

// CountTokensCalibrationTools contains only Desktop-owned, capture-compiled
// tool definitions. It deliberately has no input path for downstream MCP or
// caller tool schemas.
type CountTokensCalibrationTools struct {
	Layout           string
	ExpectedRequests int
	BuiltinTools     []json.RawMessage
	MCPTools         []json.RawMessage
	SingleTools      []json.RawMessage
	ToolRequests     [][]json.RawMessage
}

var (
	countTokensCatalogOnce sync.Once
	countTokensCatalog     countTokensToolCatalog
	countTokensCatalogErr  error
)

func (b *Bundle) CountTokensCalibrationTools(model string) (CountTokensCalibrationTools, error) {
	if b == nil {
		return CountTokensCalibrationTools{}, fmt.Errorf("claude desktop profile: no count_tokens calibration catalog for Desktop %q", bundleDesktopVersion(b))
	}
	variant, errVariant := b.Resolve(RequestVariantKey{Model: model, LogicalModel: model, Role: RoleCountTokens})
	if errVariant != nil {
		return CountTokensCalibrationTools{}, errVariant
	}
	requestProfile, errProfile := b.RequestProfileForVariant(variant)
	if errProfile != nil {
		return CountTokensCalibrationTools{}, errProfile
	}
	if catalogID := strings.TrimSpace(requestProfile.CountTokensCatalog); catalogID != "" {
		return builtinCurrentCountTokensToolCatalog(catalogID, model)
	}
	if strings.TrimSpace(requestProfile.DesktopVersion) != "1.40609.0.0" {
		return CountTokensCalibrationTools{}, fmt.Errorf("claude desktop profile: no count_tokens calibration catalog for Desktop %q", requestProfile.DesktopVersion)
	}
	catalog, errCatalog := builtinCountTokensToolCatalog()
	if errCatalog != nil {
		return CountTokensCalibrationTools{}, errCatalog
	}
	singles := catalog.SingleTools
	switch strings.TrimSpace(model) {
	case "claude-haiku-4-5-20251001", "claude-opus-4-6", "claude-opus-4-7", "claude-sonnet-4-6":
		singles = countTokensSinglesWithTaskManagement(catalog.SingleTools, catalog.HaikuExtraSingleTools)
	case "claude-opus-4-8", "claude-opus-5", "claude-sonnet-5":
	default:
		return CountTokensCalibrationTools{}, fmt.Errorf("claude desktop profile: no observed count_tokens calibration catalog for model %q", model)
	}
	return CountTokensCalibrationTools{
		Layout: "v140609",
		ExpectedRequests: map[string]int{
			"claude-opus-4-6": 46, "claude-opus-4-7": 46, "claude-opus-4-8": 38,
			"claude-opus-5": 41, "claude-sonnet-4-6": 46, "claude-sonnet-5": 42,
			"claude-haiku-4-5-20251001": 46,
		}[strings.TrimSpace(model)],
		BuiltinTools: cloneRawMessages(catalog.BuiltinTools),
		MCPTools:     cloneRawMessages(catalog.MCPTools),
		SingleTools:  cloneRawMessages(singles),
	}, nil
}

func builtinCountTokensToolCatalog() (countTokensToolCatalog, error) {
	countTokensCatalogOnce.Do(func() {
		encoded := strings.TrimSpace(countTokensToolCatalogBase64)
		compressed, errDecode := base64.StdEncoding.DecodeString(encoded)
		if errDecode != nil {
			countTokensCatalogErr = fmt.Errorf("claude desktop profile: decode count_tokens calibration catalog: %w", errDecode)
			return
		}
		reader, errGzip := gzip.NewReader(bytes.NewReader(compressed))
		if errGzip != nil {
			countTokensCatalogErr = fmt.Errorf("claude desktop profile: open count_tokens calibration catalog: %w", errGzip)
			return
		}
		data, errRead := io.ReadAll(reader)
		errClose := reader.Close()
		if errRead != nil {
			countTokensCatalogErr = fmt.Errorf("claude desktop profile: read count_tokens calibration catalog: %w", errRead)
			return
		}
		if errClose != nil {
			countTokensCatalogErr = fmt.Errorf("claude desktop profile: close count_tokens calibration catalog: %w", errClose)
			return
		}
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != countTokensToolCatalogSHA256 {
			countTokensCatalogErr = fmt.Errorf("claude desktop profile: count_tokens calibration catalog digest %s does not match %s", got, countTokensToolCatalogSHA256)
			return
		}
		if errJSON := json.Unmarshal(data, &countTokensCatalog); errJSON != nil {
			countTokensCatalogErr = fmt.Errorf("claude desktop profile: decode count_tokens calibration catalog JSON: %w", errJSON)
			return
		}
		countTokensCatalogErr = validateCountTokensToolCatalog(countTokensCatalog)
	})
	return countTokensCatalog, countTokensCatalogErr
}

func validateCountTokensToolCatalog(catalog countTokensToolCatalog) error {
	if catalog.SchemaVersion != 1 || catalog.DesktopVersion != "1.40609.0.0" || catalog.Source != "sanitized-v140609-count-token-tool-catalog" {
		return fmt.Errorf("claude desktop profile: count_tokens calibration catalog identity is invalid")
	}
	if len(catalog.BuiltinTools) != 17 || len(catalog.MCPTools) != 66 || len(catalog.SingleTools) != 24 || len(catalog.HaikuExtraSingleTools) != 4 {
		return fmt.Errorf("claude desktop profile: count_tokens calibration tool counts are invalid")
	}
	for family, tools := range map[string][]json.RawMessage{
		"builtin": catalog.BuiltinTools, "mcp": catalog.MCPTools,
		"single": catalog.SingleTools, "haiku-extra": catalog.HaikuExtraSingleTools,
	} {
		for index, tool := range tools {
			if !json.Valid(tool) || !strings.HasPrefix(strings.TrimSpace(string(tool)), "{") {
				return fmt.Errorf("claude desktop profile: %s count_tokens tool %d is invalid JSON", family, index)
			}
			var shape struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"input_schema"`
			}
			if errTool := json.Unmarshal(tool, &shape); errTool != nil || strings.TrimSpace(shape.Name) == "" || !json.Valid(shape.InputSchema) {
				return fmt.Errorf("claude desktop profile: %s count_tokens tool %d has an invalid shape", family, index)
			}
		}
	}
	return nil
}

func countTokensSinglesWithTaskManagement(base, extras []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(base)+len(extras))
	extraByName := make(map[string]json.RawMessage, len(extras))
	for _, tool := range extras {
		extraByName[rawToolName(tool)] = tool
	}
	for _, tool := range base {
		name := rawToolName(tool)
		if name == "TaskOutput" {
			for _, extraName := range []string{"TaskCreate", "TaskGet", "TaskList"} {
				result = append(result, extraByName[extraName])
			}
		}
		result = append(result, tool)
		if name == "TaskStop" {
			result = append(result, extraByName["TaskUpdate"])
		}
	}
	return result
}

func rawToolName(tool json.RawMessage) string {
	var shape struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(tool, &shape)
	return shape.Name
}

func cloneRawMessages(values []json.RawMessage) []json.RawMessage {
	cloned := make([]json.RawMessage, len(values))
	for index, value := range values {
		cloned[index] = append(json.RawMessage(nil), value...)
	}
	return cloned
}

func bundleDesktopVersion(bundle *Bundle) string {
	if bundle == nil {
		return ""
	}
	return bundle.DesktopVersion
}
