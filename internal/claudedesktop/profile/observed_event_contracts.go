package profile

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const (
	ObservedPayloadContractCaptured     = "captured-contract"
	ObservedPayloadContractSpecialized  = "specialized-contract"
	ObservedPayloadContractEndpointOnly = "endpoint-only"

	ObservedFieldOwnerProgram = "program"
)

//go:embed v140609.observed-contracts.json
var v140609ObservedContractsJSON []byte

//go:embed v140609.specialized-contracts.json
var v140609SpecializedContractsJSON []byte

// ObservedEventFieldContract describes one JSON value accepted from the
// Desktop companion. Nested fields, array items and object variants are used by
// reviewed contracts that could not be generated from the flat capture catalog.
type ObservedEventFieldContract struct {
	Types                []string                              `json:"types"`
	Required             bool                                  `json:"required"`
	ObservedCount        int                                   `json:"observed_count,omitempty"`
	Owner                string                                `json:"owner,omitempty"`
	NonEmpty             bool                                  `json:"non_empty,omitempty"`
	Enum                 []string                              `json:"enum,omitempty"`
	Const                json.RawMessage                       `json:"const,omitempty"`
	Fields               map[string]ObservedEventFieldContract `json:"fields,omitempty"`
	AdditionalProperties bool                                  `json:"additional_properties,omitempty"`
	Items                *ObservedEventFieldContract           `json:"items,omitempty"`
	Variants             []ObservedEventObjectContract         `json:"variants,omitempty"`
}

type ObservedEventObjectContract struct {
	Name                 string                                `json:"name,omitempty"`
	Fields               map[string]ObservedEventFieldContract `json:"fields"`
	AdditionalProperties bool                                  `json:"additional_properties,omitempty"`
}

type ObservedEventBinaryContract struct {
	FilenameShape string `json:"filename_shape"`
	MagicASCII    string `json:"magic_ascii"`
	MinimumBytes  int    `json:"minimum_bytes"`
	MaximumBytes  int    `json:"maximum_bytes"`
}

type ObservedEventContract struct {
	EventName            string                                `json:"event_name"`
	Source               string                                `json:"source"`
	PayloadField         string                                `json:"payload_field"`
	ObservedCount        int                                   `json:"observed_count"`
	Fields               map[string]ObservedEventFieldContract `json:"fields,omitempty"`
	EventFields          map[string]ObservedEventFieldContract `json:"event_fields,omitempty"`
	FieldOrder           []string                              `json:"field_order,omitempty"`
	AdditionalProperties bool                                  `json:"additional_properties,omitempty"`
	Variants             []ObservedEventObjectContract         `json:"variants,omitempty"`
	Binary               *ObservedEventBinaryContract          `json:"binary,omitempty"`
}

type ObservedEventContractSummary struct {
	SchemaVersion  int
	ProfileID      string
	SourceArtifact string
	SourceSHA256   string
	EventCount     int
}

type ObservedEventContractArtifact struct {
	Artifact string `json:"artifact"`
	SHA256   string `json:"sha256"`
}

type ObservedEventPayloadContract struct {
	Classification string
	Contract       ObservedEventContract
}

type ObservedEventPayloadContractSummary struct {
	ProfileID                     string
	ObservedCompanionEventCount   int
	CapturedContractEventCount    int
	SpecializedContractEventCount int
	EndpointOnlyEventCount        int
	CapturedSourceArtifact        string
	CapturedSourceSHA256          string
	SpecializedSourceArtifact     string
	SpecializedSourceSHA256       string
}

type observedEventContractBundle struct {
	SchemaVersion  int                              `json:"schema_version"`
	ProfileID      string                           `json:"profile_id"`
	SourceArtifact string                           `json:"source_artifact"`
	SourceSHA256   string                           `json:"source_sha256"`
	EventCount     int                              `json:"event_count"`
	Events         map[string]ObservedEventContract `json:"events"`
}

type observedSpecializedContractBundle struct {
	SchemaVersion   int                              `json:"schema_version"`
	ProfileID       string                           `json:"profile_id"`
	SourceArtifacts []ObservedEventContractArtifact  `json:"source_artifacts"`
	EventCount      int                              `json:"event_count"`
	Events          map[string]ObservedEventContract `json:"events"`
}

var (
	v140609ObservedContractsOnce sync.Once
	v140609ObservedContracts     observedEventContractBundle
	v140609ObservedContractsErr  error

	v140609SpecializedContractsOnce sync.Once
	v140609SpecializedContracts     observedSpecializedContractBundle
	v140609SpecializedContractsErr  error
)

// V140609ObservedEventContract retains the original generated, captured-only
// lookup and provenance. New callers that need all three coverage classes use
// V140609ObservedEventPayloadContract.
func V140609ObservedEventContract(kind string) (ObservedEventContract, bool, error) {
	bundle, errBundle := loadV140609ObservedEventContracts()
	if errBundle != nil {
		return ObservedEventContract{}, false, errBundle
	}
	contract, ok := bundle.Events[strings.TrimSpace(kind)]
	if !ok {
		return ObservedEventContract{}, false, nil
	}
	return cloneObservedEventContract(contract), true, nil
}

func V140609ObservedEventPayloadContract(kind string) (ObservedEventPayloadContract, bool, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return ObservedEventPayloadContract{}, false, nil
	}
	captured, errCaptured := loadV140609ObservedEventContracts()
	if errCaptured != nil {
		return ObservedEventPayloadContract{}, false, errCaptured
	}
	if contract, ok := captured.Events[kind]; ok {
		return ObservedEventPayloadContract{Classification: ObservedPayloadContractCaptured, Contract: cloneObservedEventContract(contract)}, true, nil
	}
	specialized, errSpecialized := loadV140609SpecializedEventContracts()
	if errSpecialized != nil {
		return ObservedEventPayloadContract{}, false, errSpecialized
	}
	if contract, ok := specialized.Events[kind]; ok {
		return ObservedEventPayloadContract{Classification: ObservedPayloadContractSpecialized, Contract: cloneObservedEventContract(contract)}, true, nil
	}
	if _, ok := V140609ObservedExecutableEvent(kind); ok {
		return ObservedEventPayloadContract{Classification: ObservedPayloadContractEndpointOnly}, true, nil
	}
	return ObservedEventPayloadContract{}, false, nil
}

func V140609ObservedEventContracts() (map[string]ObservedEventContract, error) {
	bundle, errBundle := loadV140609ObservedEventContracts()
	if errBundle != nil {
		return nil, errBundle
	}
	result := make(map[string]ObservedEventContract, len(bundle.Events))
	for kind, contract := range bundle.Events {
		result[kind] = cloneObservedEventContract(contract)
	}
	return result, nil
}

func V140609SpecializedEventContracts() (map[string]ObservedEventContract, error) {
	bundle, errBundle := loadV140609SpecializedEventContracts()
	if errBundle != nil {
		return nil, errBundle
	}
	result := make(map[string]ObservedEventContract, len(bundle.Events))
	for kind, contract := range bundle.Events {
		result[kind] = cloneObservedEventContract(contract)
	}
	return result, nil
}

func V140609ObservedEventContractSummaryValue() (ObservedEventContractSummary, error) {
	bundle, errBundle := loadV140609ObservedEventContracts()
	if errBundle != nil {
		return ObservedEventContractSummary{}, errBundle
	}
	return ObservedEventContractSummary{
		SchemaVersion: bundle.SchemaVersion, ProfileID: bundle.ProfileID,
		SourceArtifact: bundle.SourceArtifact, SourceSHA256: bundle.SourceSHA256,
		EventCount: bundle.EventCount,
	}, nil
}

func V140609ObservedEventPayloadContractSummaryValue() (ObservedEventPayloadContractSummary, error) {
	captured, errCaptured := loadV140609ObservedEventContracts()
	if errCaptured != nil {
		return ObservedEventPayloadContractSummary{}, errCaptured
	}
	specialized, errSpecialized := loadV140609SpecializedEventContracts()
	if errSpecialized != nil {
		return ObservedEventPayloadContractSummary{}, errSpecialized
	}
	total := len(V140609ObservedExecutableEvents())
	return ObservedEventPayloadContractSummary{
		ProfileID:                     captured.ProfileID,
		ObservedCompanionEventCount:   total,
		CapturedContractEventCount:    captured.EventCount,
		SpecializedContractEventCount: specialized.EventCount,
		EndpointOnlyEventCount:        total - captured.EventCount - specialized.EventCount,
		CapturedSourceArtifact:        captured.SourceArtifact,
		CapturedSourceSHA256:          captured.SourceSHA256,
		SpecializedSourceArtifact:     "profile/v140609.specialized-contracts.json",
		SpecializedSourceSHA256:       sha256Hex(v140609SpecializedContractsJSON),
	}, nil
}

func loadV140609ObservedEventContracts() (*observedEventContractBundle, error) {
	v140609ObservedContractsOnce.Do(func() {
		if errDecode := json.Unmarshal(v140609ObservedContractsJSON, &v140609ObservedContracts); errDecode != nil {
			v140609ObservedContractsErr = fmt.Errorf("decode Claude Desktop observed event contracts: %w", errDecode)
			return
		}
		if v140609ObservedContracts.SchemaVersion != 1 || v140609ObservedContracts.ProfileID != "claude-desktop/windows-x64/1.40609.0.0" {
			v140609ObservedContractsErr = fmt.Errorf("Claude Desktop observed event contract identity is invalid")
			return
		}
		if v140609ObservedContracts.EventCount != len(v140609ObservedContracts.Events) || v140609ObservedContracts.EventCount != 99 {
			v140609ObservedContractsErr = fmt.Errorf("Claude Desktop observed event contract count is %d/%d, want 99", v140609ObservedContracts.EventCount, len(v140609ObservedContracts.Events))
		}
	})
	if v140609ObservedContractsErr != nil {
		return nil, v140609ObservedContractsErr
	}
	return &v140609ObservedContracts, nil
}

func loadV140609SpecializedEventContracts() (*observedSpecializedContractBundle, error) {
	v140609SpecializedContractsOnce.Do(func() {
		if errDecode := json.Unmarshal(v140609SpecializedContractsJSON, &v140609SpecializedContracts); errDecode != nil {
			v140609SpecializedContractsErr = fmt.Errorf("decode Claude Desktop specialized event contracts: %w", errDecode)
			return
		}
		if v140609SpecializedContracts.SchemaVersion != 1 || v140609SpecializedContracts.ProfileID != "claude-desktop/windows-x64/1.40609.0.0" {
			v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract identity is invalid")
			return
		}
		if v140609SpecializedContracts.EventCount != len(v140609SpecializedContracts.Events) || v140609SpecializedContracts.EventCount != 25 {
			v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract count is %d/%d, want 25", v140609SpecializedContracts.EventCount, len(v140609SpecializedContracts.Events))
			return
		}
		captured, errCaptured := loadV140609ObservedEventContracts()
		if errCaptured != nil {
			v140609SpecializedContractsErr = errCaptured
			return
		}
		for kind, contract := range v140609SpecializedContracts.Events {
			if _, overlaps := captured.Events[kind]; overlaps {
				v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract %q overlaps captured catalog", kind)
				return
			}
			event, ok := V140609ObservedExecutableEvent(kind)
			if !ok || contract.EventName != event.EventName || contract.Source != event.Source || contract.PayloadField == "" || contract.ObservedCount <= 0 {
				v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract %q is invalid", kind)
				return
			}
			if len(contract.FieldOrder) > 0 {
				ordered := make(map[string]struct{}, len(contract.FieldOrder))
				for _, name := range contract.FieldOrder {
					if _, duplicate := ordered[name]; duplicate {
						v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract %q repeats field-order entry %q", kind, name)
						return
					}
					if _, exists := contract.Fields[name]; !exists {
						v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract %q orders unknown field %q", kind, name)
						return
					}
					ordered[name] = struct{}{}
				}
				if len(ordered) != len(contract.Fields) {
					v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract %q field order is incomplete", kind)
					return
				}
			}
		}
		for _, artifact := range v140609SpecializedContracts.SourceArtifacts {
			if strings.TrimSpace(artifact.Artifact) == "" || len(artifact.SHA256) != 64 {
				v140609SpecializedContractsErr = fmt.Errorf("Claude Desktop specialized event contract provenance is invalid")
				return
			}
		}
	})
	if v140609SpecializedContractsErr != nil {
		return nil, v140609SpecializedContractsErr
	}
	return &v140609SpecializedContracts, nil
}

func cloneObservedEventContract(contract ObservedEventContract) ObservedEventContract {
	result := contract
	result.Fields = cloneObservedFieldContracts(contract.Fields)
	result.EventFields = cloneObservedFieldContracts(contract.EventFields)
	result.FieldOrder = append([]string(nil), contract.FieldOrder...)
	result.Variants = cloneObservedObjectContracts(contract.Variants)
	if contract.Binary != nil {
		binary := *contract.Binary
		result.Binary = &binary
	}
	return result
}

func cloneObservedFieldContracts(fields map[string]ObservedEventFieldContract) map[string]ObservedEventFieldContract {
	if fields == nil {
		return nil
	}
	result := make(map[string]ObservedEventFieldContract, len(fields))
	for name, field := range fields {
		result[name] = cloneObservedEventFieldContract(field)
	}
	return result
}

func cloneObservedEventFieldContract(field ObservedEventFieldContract) ObservedEventFieldContract {
	field.Types = append([]string(nil), field.Types...)
	field.Enum = append([]string(nil), field.Enum...)
	field.Const = append(json.RawMessage(nil), field.Const...)
	field.Fields = cloneObservedFieldContracts(field.Fields)
	field.Variants = cloneObservedObjectContracts(field.Variants)
	if field.Items != nil {
		item := cloneObservedEventFieldContract(*field.Items)
		field.Items = &item
	}
	return field
}

func cloneObservedObjectContracts(variants []ObservedEventObjectContract) []ObservedEventObjectContract {
	result := make([]ObservedEventObjectContract, len(variants))
	for index, variant := range variants {
		result[index] = variant
		result[index].Fields = cloneObservedFieldContracts(variant.Fields)
	}
	return result
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
