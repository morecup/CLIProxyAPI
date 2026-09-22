package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// SDKCompactionReadFile is an entry in the owned read-file snapshot, preserving
// insertion order for equal timestamps. It is not a request-controlled path to
// open on the proxy host. The owning runtime supplies all filesystem operations.
type SDKCompactionReadFile struct {
	Filename  string
	Timestamp float64
}

type SDKCompactionFileReadOptions struct {
	MaxTokens    int
	SuccessEvent string
	ErrorEvent   string
	Source       string
}

type SDKCompactionFileRestoreOps struct {
	NormalizePath    func(string) (string, error)
	MemoryPaths      func() ([]string, error)
	SessionReadPaths func(context.Context) ([]string, error)
	PlanPath         func() (string, error)
	ReadFile         func(context.Context, string, SDKCompactionFileReadOptions) (json.RawMessage, error)
	WrapAttachment   func(json.RawMessage) (json.RawMessage, error)
	WrappedTokens    func(json.RawMessage) (int64, error)
}

var ErrSDKCompactionFilesUnknown = errors.New("unresolved native compaction file state")

// SDKReadResultHasNoContent follows _584.$7, imported by iQo as BHe. These
// prefixes report deduplicated reads, not truncated file contents. Matching is
// case-sensitive and does not trim or search inside the tool result.
func SDKReadResultHasNoContent(text string) bool {
	return strings.HasPrefix(text, "File unchanged since last read. The content from the earlier Read tool_result in this conversation is still current — refer to that instead of re-reading.") ||
		strings.HasPrefix(text, "Wasted call — file unchanged since your last Read. Refer to that earlier tool_result instead.") ||
		strings.HasPrefix(text, "<system-reminder>This file is already in your context")
}

// SDKPreservedReadPaths follows iQo. A preserved Read call prevents redundant
// restoration except when its result was one of the no-content responses. It
// does not infer hidden reads or require that a result has already arrived.
func SDKPreservedReadPaths(messages []json.RawMessage, readToolName string, normalize func(string) (string, error)) (map[string]struct{}, error) {
	if readToolName == "" || normalize == nil {
		return nil, ErrSDKCompactionFilesUnknown
	}
	type message struct {
		kind    string
		content []map[string]json.RawMessage
	}
	rows := make([]message, len(messages))
	noContent := make(map[string]struct{})
	for index, raw := range messages {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(raw, &envelope) != nil {
			return nil, ErrSDKCompactionContentUnknown
		}
		rows[index].kind = sdkRestorationString(envelope, "type")
		if rows[index].kind != "user" && rows[index].kind != "assistant" {
			continue
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(envelope["message"], &payload) != nil || payload == nil {
			return nil, ErrSDKCompactionContentUnknown
		}
		if json.Unmarshal(payload["content"], &rows[index].content) != nil || rows[index].kind != "user" {
			continue
		}
		for _, block := range rows[index].content {
			var text string
			if sdkRestorationString(block, "type") == "tool_result" && json.Unmarshal(block["content"], &text) == nil && SDKReadResultHasNoContent(text) {
				noContent[sdkRestorationString(block, "tool_use_id")] = struct{}{}
			}
		}
	}
	paths := make(map[string]struct{})
	for _, row := range rows {
		if row.kind != "assistant" {
			continue
		}
		for _, block := range row.content {
			if _, absent := noContent[sdkRestorationString(block, "id")]; absent || sdkRestorationString(block, "type") != "tool_use" || sdkRestorationString(block, "name") != readToolName {
				continue
			}
			var input map[string]json.RawMessage
			var path *string
			if json.Unmarshal(block["input"], &input) != nil || json.Unmarshal(input["file_path"], &path) != nil || path == nil {
				continue
			}
			key, err := normalize(*path)
			if err != nil {
				return nil, err
			}
			paths[key] = struct{}{}
		}
	}
	return paths, nil
}

func sdkRestorationString(fields map[string]json.RawMessage, name string) string {
	var value string
	_ = json.Unmarshal(fields[name], &value)
	return value
}

// RestoreSDKCompactionFiles implements nQo: exclusions precede timestamp
// ordering, only the first five candidates are read, and the 50k token budget
// is applied to complete wrapped attachments after every selected read finishes.
// Null reads do not refill the candidate list. The real reader owns its success
// and error events; selecting a path never fabricates a successful file read.
func RestoreSDKCompactionFiles(ctx context.Context, files []SDKCompactionReadFile, preserved []json.RawMessage, readToolName string, excludeSessionReads bool, ops SDKCompactionFileRestoreOps) ([]json.RawMessage, error) {
	if ctx == nil || ops.NormalizePath == nil || ops.MemoryPaths == nil || ops.PlanPath == nil || ops.ReadFile == nil || ops.WrapAttachment == nil || ops.WrappedTokens == nil ||
		(excludeSessionReads && ops.SessionReadPaths == nil) {
		return nil, ErrSDKCompactionFilesUnknown
	}
	kept, err := SDKPreservedReadPaths(preserved, readToolName, ops.NormalizePath)
	if err != nil {
		return nil, err
	}
	normalizePaths := func(paths []string) (map[string]struct{}, error) {
		result := make(map[string]struct{})
		for _, path := range paths {
			key, errPath := ops.NormalizePath(path)
			if errPath != nil {
				return nil, errPath
			}
			result[key] = struct{}{}
		}
		return result, nil
	}
	session := make(map[string]struct{})
	if excludeSessionReads {
		paths, errPaths := ops.SessionReadPaths(ctx)
		if errPaths != nil {
			return nil, errPaths
		}
		session, err = normalizePaths(paths)
		if err != nil {
			return nil, err
		}
	}
	// lQo catches both memory-path discovery and normalization failures and
	// uses an empty set. In contrast, session-read discovery rejects nQo.
	memory := make(map[string]struct{})
	if paths, errPaths := ops.MemoryPaths(); errPaths == nil {
		if normalized, errNormalize := normalizePaths(paths); errNormalize == nil {
			memory = normalized
		}
	}
	var candidates []SDKCompactionReadFile
	for _, file := range files {
		key, errPath := ops.NormalizePath(file.Filename)
		if errPath != nil {
			return nil, errPath
		}
		if plan, errPlan := ops.PlanPath(); errPlan == nil {
			if normalized, errNormalize := ops.NormalizePath(plan); errNormalize == nil && key == normalized {
				continue
			}
		}
		if _, found := memory[key]; found {
			continue
		}
		if _, found := session[key]; found {
			continue
		}
		if _, found := kept[key]; found {
			continue
		}
		candidates = append(candidates, file)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Timestamp > candidates[j].Timestamp })
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	type completed struct {
		index   int
		message json.RawMessage
		err     error
	}
	done := make(chan completed, len(candidates))
	for index, file := range candidates {
		go func() {
			attachment, errRead := ops.ReadFile(ctx, file.Filename, SDKCompactionFileReadOptions{MaxTokens: 5000,
				SuccessEvent: "tengu_post_compact_file_restore_success", ErrorEvent: "tengu_post_compact_file_restore_error", Source: "compact"})
			var message json.RawMessage
			if errRead == nil && len(attachment) != 0 {
				message, errRead = ops.WrapAttachment(attachment)
			}
			done <- completed{index, append(json.RawMessage(nil), message...), errRead}
		}()
	}
	rows := make([]json.RawMessage, len(candidates))
	for range candidates {
		value := <-done
		if value.err != nil {
			return nil, value.err
		}
		rows[value.index] = value.message
	}
	var total int64
	var result []json.RawMessage
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		tokens, errTokens := ops.WrappedTokens(row)
		if errTokens != nil {
			return nil, errTokens
		}
		if tokens < 0 {
			return nil, ErrSDKCompactionFilesUnknown
		}
		if tokens <= 50000-total {
			total += tokens
			result = append(result, row)
		}
	}
	return result, nil
}
