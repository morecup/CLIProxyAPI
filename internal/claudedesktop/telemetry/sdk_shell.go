package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	FactSDKBashToolCommandExecuted       = "bash_tool_command_executed"
	FactSDKPowerShellToolCommandExecuted = "powershell_tool_command_executed"
)

var sdkShellExecutedFacts = map[string]string{
	"tengu_bash_tool_command_executed":       FactSDKBashToolCommandExecuted,
	"tengu_powershell_tool_command_executed": FactSDKPowerShellToolCommandExecuted,
}

type sdkRawMetadata struct {
	raw json.RawMessage
}

func (m sdkRawMetadata) MarshalJSON() ([]byte, error) {
	return append([]byte(nil), m.raw...), nil
}

func init() {
	registerExecutableEvents("sdk-event-logging", map[string]string{
		FactSDKBashToolCommandExecuted:       "tengu_bash_tool_command_executed",
		FactSDKPowerShellToolCommandExecuted: "tengu_powershell_tool_command_executed",
	})
	registerExecutableEvents(datadogLogsRole, map[string]string{
		FactSDKBashToolCommandExecuted: "tengu_bash_tool_command_executed",
	})
}

// RecordSDKShellCommand accepts only an in-process owner's complete,
// content-free completion metadata. Request history cannot call this API and
// is never used to guess exit codes, shell editions, sandbox state or output.
func (m *Manager) RecordSDKShellCommand(ctx context.Context, auth *cliproxyauth.Auth, session, model, promptID, eventName string, metadata json.RawMessage) error {
	if m == nil || !m.Enabled() {
		return nil
	}
	fact, ok := sdkShellExecutedFacts[strings.TrimSpace(eventName)]
	if !ok {
		return fmt.Errorf("Claude Desktop shell telemetry event is not executable")
	}
	if err := validateSDKShellExecutedMetadata(fact, metadata); err != nil {
		return err
	}
	return m.recordSDKFact(ctx, auth, session, model, promptID, fact, func(subscription, promptID string) any {
		return sdkRawMetadata{raw: prefixSDKMetadata(metadata, subscription, promptID)}
	})
}

func prefixSDKMetadata(metadata json.RawMessage, subscription, promptID string) json.RawMessage {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	wrote := false
	for _, value := range [][2]string{{"subscription_type", subscription}, {"cc_prompt_id", promptID}} {
		if value[1] == "" {
			continue
		}
		if wrote {
			buffer.WriteByte(',')
		}
		key, _ := json.Marshal(value[0])
		text, _ := json.Marshal(value[1])
		buffer.Write(key)
		buffer.WriteByte(':')
		buffer.Write(text)
		wrote = true
	}
	trimmed := bytes.TrimSpace(metadata)
	if len(trimmed) > 2 {
		if wrote {
			buffer.WriteByte(',')
		}
		buffer.Write(trimmed[1 : len(trimmed)-1])
	}
	buffer.WriteByte('}')
	return append(json.RawMessage(nil), buffer.Bytes()...)
}

func validateSDKShellExecutedMetadata(fact string, metadata json.RawMessage) error {
	fields, err := decodeSDKShellMetadata(metadata)
	if err != nil {
		return err
	}
	common := []string{"command_type", "stdout_length", "stderr_length", "exit_code", "interrupted", "destructive_category", "destructive_target_scope", "permission_mode"}
	allowed := make(map[string]bool, len(common)+6)
	for _, key := range common {
		allowed[key] = true
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("Claude Desktop shell telemetry metadata is missing %q", key)
		}
	}
	for _, key := range []string{"command_type", "destructive_category", "destructive_target_scope", "permission_mode"} {
		if !jsonStringNonEmpty(fields[key]) {
			return fmt.Errorf("Claude Desktop shell telemetry field %q is invalid", key)
		}
	}
	for _, key := range []string{"stdout_length", "stderr_length"} {
		if value, ok := jsonInteger(fields[key]); !ok || value < 0 {
			return fmt.Errorf("Claude Desktop shell telemetry field %q is invalid", key)
		}
	}
	if _, ok := jsonInteger(fields["exit_code"]); !ok || !jsonBoolean(fields["interrupted"]) {
		return fmt.Errorf("Claude Desktop shell telemetry exit result is invalid")
	}
	if fact == FactSDKBashToolCommandExecuted {
		if err := validateSDKBashExecutedMetadata(fields, allowed); err != nil {
			return err
		}
	} else {
		allowed["powershell_edition"] = true
		if !jsonStringNonEmpty(fields["powershell_edition"]) {
			return fmt.Errorf("Claude Desktop PowerShell telemetry field %q is invalid", "powershell_edition")
		}
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("Claude Desktop shell telemetry metadata field %q is not allowed", key)
		}
	}
	return nil
}

func validateSDKBashExecutedMetadata(fields map[string]json.RawMessage, allowed map[string]bool) error {
	stringsRequired := []string{"executor_shell", "filesystem_policy", "call_origin", "git_destructive_target", "tool_use_id"}
	boolsRequired := []string{"executor_shell_overridden", "sandboxed", "sandbox_enabled", "had_sandbox_violation", "dangerously_disable_sandbox", "was_backgrounded"}
	for _, key := range stringsRequired {
		allowed[key] = true
		if !jsonStringNonEmpty(fields[key]) {
			return fmt.Errorf("Claude Desktop Bash telemetry field %q is invalid", key)
		}
	}
	for _, key := range boolsRequired {
		allowed[key] = true
		if !jsonBoolean(fields[key]) {
			return fmt.Errorf("Claude Desktop Bash telemetry field %q is invalid", key)
		}
	}
	return nil
}

func decodeSDKShellMetadata(metadata json.RawMessage) (map[string]json.RawMessage, error) {
	if !json.Valid(metadata) {
		return nil, fmt.Errorf("Claude Desktop shell telemetry metadata is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("Claude Desktop shell telemetry metadata is invalid")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, errToken := decoder.Token()
		key, ok := token.(string)
		if errToken != nil || !ok || key == "subscription_type" || key == "cc_prompt_id" {
			return nil, fmt.Errorf("Claude Desktop shell telemetry metadata key is invalid")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("Claude Desktop shell telemetry metadata key %q is duplicated", key)
		}
		if strings.Contains(strings.ToLower(key), "command") && key != "command_type" || key == "stdout" || key == "stderr" || key == "output" || key == "cwd" {
			return nil, fmt.Errorf("Claude Desktop shell telemetry metadata contains content field %q", key)
		}
		var value json.RawMessage
		if errDecode := decoder.Decode(&value); errDecode != nil {
			return nil, fmt.Errorf("Claude Desktop shell telemetry metadata is invalid")
		}
		fields[key] = append(json.RawMessage(nil), value...)
	}
	if _, err = decoder.Token(); err != nil || len(fields) == 0 || decoder.More() {
		return nil, fmt.Errorf("Claude Desktop shell telemetry metadata is invalid")
	}
	return fields, nil
}

func jsonStringNonEmpty(raw json.RawMessage) bool {
	var value string
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
}

func jsonBoolean(raw json.RawMessage) bool {
	var value bool
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil
}

func jsonInteger(raw json.RawMessage) (int64, bool) {
	var value int64
	return value, len(raw) > 0 && json.Unmarshal(raw, &value) == nil
}
