package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

type catalog struct {
	Events map[string]catalogEvent `json:"events"`
}

type catalogEvent struct {
	Count           int                   `json:"count"`
	EventDataSchema map[string]schemaNode `json:"event_data_schema"`
}

type schemaNode struct {
	Count    int                   `json:"count"`
	Types    json.RawMessage       `json:"types"`
	Children map[string]schemaNode `json:"children"`
}

type generatedBundle struct {
	SchemaVersion  int                          `json:"schema_version"`
	ProfileID      string                       `json:"profile_id"`
	SourceArtifact string                       `json:"source_artifact"`
	SourceSHA256   string                       `json:"source_sha256"`
	EventCount     int                          `json:"event_count"`
	Events         map[string]generatedContract `json:"events"`
}

type generatedContract struct {
	EventName     string                    `json:"event_name"`
	Source        string                    `json:"source"`
	PayloadField  string                    `json:"payload_field"`
	ObservedCount int                       `json:"observed_count"`
	Fields        map[string]generatedField `json:"fields"`
}

type generatedField struct {
	Types         []string `json:"types"`
	Required      bool     `json:"required"`
	ObservedCount int      `json:"observed_count"`
}

func main() {
	input := flag.String("input", "", "sanitized v140609 event-catalog.json")
	output := flag.String("output", "", "generated observed contract JSON")
	artifact := flag.String("artifact", "runtime/v140609/event-catalog.json", "published source artifact name")
	flag.Parse()
	if strings.TrimSpace(*input) == "" || strings.TrimSpace(*output) == "" {
		fatalf("-input and -output are required")
	}

	raw, errRead := os.ReadFile(*input)
	if errRead != nil {
		fatalf("read input: %v", errRead)
	}
	var source catalog
	if errDecode := json.Unmarshal(raw, &source); errDecode != nil {
		fatalf("decode input: %v", errDecode)
	}
	digest := sha256.Sum256(raw)
	generated := generatedBundle{
		SchemaVersion:  1,
		ProfileID:      "claude-desktop/windows-x64/1.40609.0.0",
		SourceArtifact: strings.TrimSpace(*artifact),
		SourceSHA256:   hex.EncodeToString(digest[:]),
		Events:         make(map[string]generatedContract),
	}
	for _, event := range claudeprofile.V140609ObservedExecutableEvents() {
		payloadField := payloadFieldForSource(event.Source)
		if payloadField == "" {
			continue
		}
		captured, ok := source.Events[event.EventName]
		if !ok {
			continue
		}
		container, ok := captured.EventDataSchema[payloadField]
		if !ok {
			fatalf("event %q has no %q schema", event.EventName, payloadField)
		}
		decoded, ok := container.Children["<decoded>"]
		if !ok {
			fatalf("event %q has no decoded %q schema", event.EventName, payloadField)
		}
		fields := make(map[string]generatedField, len(decoded.Children))
		for name, field := range decoded.Children {
			types := normalizeTypes(field.Types)
			if len(types) == 0 {
				fatalf("event %q field %q has no captured types", event.EventName, name)
			}
			fields[name] = generatedField{
				Types: types, Required: field.Count == captured.Count, ObservedCount: field.Count,
			}
		}
		generated.Events[event.Kind] = generatedContract{
			EventName: event.EventName, Source: event.Source, PayloadField: payloadField,
			ObservedCount: captured.Count, Fields: fields,
		}
	}
	generated.EventCount = len(generated.Events)
	if generated.EventCount != 99 {
		fatalf("generated %d contracts, want 99", generated.EventCount)
	}
	encoded, errEncode := json.MarshalIndent(generated, "", "  ")
	if errEncode != nil {
		fatalf("encode output: %v", errEncode)
	}
	encoded = append(encoded, '\n')
	if errWrite := os.WriteFile(*output, encoded, 0o644); errWrite != nil {
		fatalf("write output: %v", errWrite)
	}
}

func payloadFieldForSource(source string) string {
	switch source {
	case claudeprofile.ObservedEventSourceRenderer:
		return "properties"
	case claudeprofile.ObservedEventSourceMainProcess:
		return "metadata"
	case claudeprofile.ObservedEventSourceSDK:
		return "additional_metadata"
	default:
		return ""
	}
}

func normalizeTypes(raw json.RawMessage) []string {
	var value any
	if errDecode := json.Unmarshal(raw, &value); errDecode != nil {
		return nil
	}
	types := make(map[string]struct{})
	collectTypes(value, types)
	result := make([]string, 0, len(types))
	for value := range types {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func collectTypes(value any, result map[string]struct{}) {
	switch typed := value.(type) {
	case string:
		name := strings.TrimSpace(typed)
		parts := strings.Fields(name)
		if len(parts) > 1 && allDecimal(parts[len(parts)-1]) {
			name = strings.Join(parts[:len(parts)-1], " ")
		}
		if name != "" {
			result[name] = struct{}{}
		}
	case []any:
		if len(typed) == 2 {
			if name, okName := typed[0].(string); okName {
				if _, okCount := typed[1].(float64); okCount {
					result[strings.TrimSpace(name)] = struct{}{}
					return
				}
			}
		}
		for _, child := range typed {
			collectTypes(child, result)
		}
	}
}

func allDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func fatalf(format string, values ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
