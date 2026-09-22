package prompt

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// SDKTranscriptChainEvent contains only facts produced by the owned graph
// traversal. It never carries message IDs, transcript paths or content.
type SDKTranscriptChainEvent struct {
	Kind           string
	RecoveredCount int
	At             time.Time
}

const (
	SDKChainParentCycle    = "tengu_chain_parent_cycle"
	SDKChainTimestamp      = "tengu_chain_timestamp_fallback"
	SDKChainParallelResult = "tengu_chain_parallel_tr_recovered"
)

// The graph is separate from original transcript storage and live checkpoints.
// These transforms follow native k$/ati/lti/sti after the loader chooses a leaf.
// Loader tombstones, compaction relinking and fork contexts remain separate.
type sdkTranscriptChainRow struct {
	raw                     json.RawMessage
	row                     *sdkResumeRow
	uuid, parent, timestamp string
	at                      time.Time
	validTime               bool
}

type sdkTranscriptChainResult struct {
	rows   []json.RawMessage
	events []sdkResumeEvent
}

func sdkReconstructTranscriptChain(raw []json.RawMessage, leaf string) (sdkTranscriptChainResult, error) {
	return sdkReconstructTranscriptChainObserved(raw, leaf, nil)
}

func sdkReconstructTranscriptChainObserved(raw []json.RawMessage, leaf string, observe func(SDKTranscriptChainEvent)) (sdkTranscriptChainResult, error) {
	var result sdkTranscriptChainResult
	emit := func(kind string, count int) {
		attributes := map[string]any{}
		if kind == SDKChainParallelResult {
			attributes["recovered_count"] = count
		}
		result.events = append(result.events, sdkResumeEvent{kind, attributes})
		if observe != nil {
			observe(SDKTranscriptChainEvent{Kind: kind, RecoveredCount: count, At: time.Now()})
		}
	}
	rows := make([]*sdkTranscriptChainRow, 0, len(raw))
	byID := make(map[string]*sdkTranscriptChainRow, len(raw))
	for _, value := range raw {
		parsed, err := parseSDKResumeRow(value)
		if err != nil {
			return result, err
		}
		row := &sdkTranscriptChainRow{raw: bytes.Clone(value), row: parsed,
			uuid: sdkResumeString(parsed.fields, "uuid"), parent: sdkResumeString(parsed.fields, "parentUuid"),
			timestamp: sdkResumeString(parsed.fields, "timestamp")}
		if row.uuid == "" || byID[row.uuid] != nil {
			return result, ErrSDKSessionInvalid
		}
		// The owned writer/remote admission uses canonical ISO timestamps.
		// Invalid values cannot become timestamp-fallback candidates.
		row.at, err = time.Parse(time.RFC3339Nano, row.timestamp)
		row.validTime = err == nil
		byID[row.uuid] = row
		rows = append(rows, row)
	}
	current := byID[leaf]
	if current == nil {
		return result, ErrSDKResumeReconstructionRequired
	}
	seen := make(map[string]bool, len(rows))
	var chain []*sdkTranscriptChainRow
	for current != nil {
		if seen[current.uuid] {
			emit(SDKChainParentCycle, 0)
			break
		}
		seen[current.uuid] = true
		chain = append(chain, current)
		if current.parent == "" {
			break
		}
		parent := byID[current.parent]
		if parent == nil || seen[parent.uuid] {
			parent = nil
			var distance time.Duration
			if current.validTime {
				for _, candidate := range rows {
					if seen[candidate.uuid] || !candidate.validTime ||
						!bytes.Equal(bytes.TrimSpace(candidate.row.fields["isSidechain"]), bytes.TrimSpace(current.row.fields["isSidechain"])) {
						continue
					}
					delta := current.at.Sub(candidate.at)
					if delta >= 0 && delta <= 5*time.Second && (parent == nil || delta < distance) {
						parent, distance = candidate, delta
					}
				}
			}
			if parent != nil {
				emit(SDKChainTimestamp, 0)
			}
		}
		current = parent
	}
	slices.Reverse(chain)
	chain, recovered := sdkTranscriptParallelResults(rows, chain, seen)
	if recovered > 0 {
		emit(SDKChainParallelResult, recovered)
	}
	// Native trailing metadata follows descendants of the originally selected
	// leaf, not the last recovered assistant/tool-result sibling.
	children := make(map[string][]*sdkTranscriptChainRow)
	for _, row := range rows {
		if row.parent != "" && row.row.kind() != "user" && row.row.kind() != "assistant" {
			children[row.parent] = append(children[row.parent], row)
		}
	}
	queue := []string{leaf}
	var trailing []*sdkTranscriptChainRow
	for head := 0; head < len(queue); head++ {
		for _, child := range children[queue[head]] {
			if !seen[child.uuid] {
				seen[child.uuid] = true
				trailing = append(trailing, child)
				queue = append(queue, child.uuid)
			}
		}
	}
	slices.SortStableFunc(trailing, sdkTranscriptTimestampOrder)
	for _, row := range append(chain, trailing...) {
		result.rows = append(result.rows, bytes.Clone(row.raw))
	}
	return result, nil
}

func sdkTranscriptTimestampOrder(a, b *sdkTranscriptChainRow) int {
	// Canonical ISO strings have the same order under native localeCompare.
	return strings.Compare(a.timestamp, b.timestamp)
}

func sdkTranscriptParallelResults(rows, chain []*sdkTranscriptChainRow, seen map[string]bool) ([]*sdkTranscriptChainRow, int) {
	last := make(map[string]*sdkTranscriptChainRow)
	var assistants []*sdkTranscriptChainRow
	for _, row := range chain {
		if row.row.kind() == "assistant" {
			assistants = append(assistants, row)
			if id := row.row.id(); id != "" {
				last[id] = row
			}
		}
	}
	siblings := make(map[string][]*sdkTranscriptChainRow)
	toolResults := make(map[string][]*sdkTranscriptChainRow)
	for _, row := range rows {
		if row.row.kind() == "assistant" && row.row.id() != "" {
			siblings[row.row.id()] = append(siblings[row.row.id()], row)
		} else if row.row.kind() == "user" && row.parent != "" {
			for _, block := range row.row.blocks {
				if sdkResumeString(block, "type") == "tool_result" {
					toolResults[row.parent] = append(toolResults[row.parent], row)
					break
				}
			}
		}
	}
	processed := make(map[string]bool)
	after := make(map[string][]*sdkTranscriptChainRow)
	recovered := 0
	for _, assistant := range assistants {
		id := assistant.row.id()
		if id == "" || processed[id] {
			continue
		}
		processed[id] = true
		var missing, results []*sdkTranscriptChainRow
		for _, sibling := range siblings[id] {
			if !seen[sibling.uuid] {
				missing = append(missing, sibling)
			}
			for _, row := range toolResults[sibling.uuid] {
				if !seen[row.uuid] {
					results = append(results, row)
				}
			}
		}
		slices.SortStableFunc(missing, sdkTranscriptTimestampOrder)
		slices.SortStableFunc(results, sdkTranscriptTimestampOrder)
		restored := append(missing, results...)
		for _, row := range restored {
			seen[row.uuid] = true
		}
		recovered += len(restored)
		after[last[id].uuid] = restored
	}
	if recovered == 0 {
		return chain, 0
	}
	var result []*sdkTranscriptChainRow
	for _, row := range chain {
		result = append(result, row)
		result = append(result, after[row.uuid]...)
	}
	return result, recovered
}

// The existing completed-content lane has no tombstone/fork/hook adoption.
// Select the last foreground entry (native vao's plain-file lane), then k$,
// instead of assuming that every serialized sibling belongs to that history.
func sdkCompletedRemoteChain(raw []json.RawMessage, rows []SDKNativeMessage, leaf string, observe func(SDKTranscriptChainEvent)) ([]SDKNativeMessage, error) {
	start := 0
	for index, row := range rows {
		if row.Type == "system" && row.Subtype == "compact_boundary" {
			start = index
		}
	}
	selected, err := sdkReconstructTranscriptChainObserved(raw[start:], leaf, observe)
	if err != nil {
		return nil, err
	}
	var result []SDKNativeMessage
	for _, raw := range selected.rows {
		var row SDKNativeMessage
		if json.Unmarshal(raw, &row) != nil || row.IsSidechain {
			return nil, ErrSDKResumeReconstructionRequired
		}
		result = append(result, row)
	}
	return result, nil
}
