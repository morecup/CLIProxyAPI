package prompt

import (
	"bytes"
	"encoding/json"
)

// SDKForkUsage follows ow's independent totalUsage accumulator: each
// message_delta usage is merged with fresh zeroes, then added. message_start
// usage and the parent's session ledger are not inputs to this accumulator.
type SDKForkUsage struct {
	total             SDKTokenUsage
	complete, invalid bool
}

func (u *SDKForkUsage) ObserveStreamLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) || u.complete {
		return
	}
	var event struct {
		Type  string          `json:"type"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &event) != nil {
		u.invalid = true
		return
	}
	if event.Type == "message_stop" {
		u.complete = true
	}
	if event.Type != "message_delta" || len(event.Usage) == 0 || bytes.Equal(bytes.TrimSpace(event.Usage), []byte("null")) {
		return
	}
	var delta sdkResponseTokens
	delta.observeUsage(event.Usage)
	if delta.invalidUsage {
		u.invalid = true
		return
	}
	for _, field := range []struct {
		total *int64
		delta int64
	}{
		{&u.total.InputTokens, delta.usage.InputTokens},
		{&u.total.OutputTokens, delta.usage.OutputTokens},
		{&u.total.CacheReadInputTokens, delta.usage.CacheReadInputTokens},
		{&u.total.CacheCreationInputTokens, delta.usage.CacheCreationInputTokens},
	} {
		if *field.total > 1<<53-1-field.delta {
			u.invalid = true
			return
		}
		*field.total += field.delta
	}
}

func (u *SDKForkUsage) Snapshot() (SDKTokenUsage, bool) {
	if u == nil || !u.complete || u.invalid {
		return SDKTokenUsage{}, false
	}
	return u.total, true
}
