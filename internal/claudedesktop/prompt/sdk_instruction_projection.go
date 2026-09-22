package prompt

import (
	"encoding/json"
	"slices"
)

// A projection proves the exact message edit made by the owned instruction
// carrier producer. Only hashes survive preparation; no caller flag, request
// header or generic role=system filter can construct this proof.
type sdkInstructionProjection struct {
	before, after []string
}

// ObserveOwnedInstructionCarrier must be called at the actual transformation,
// before ObserveSDKQuery. Caller system settings are not SDK user yields. The
// outgoing body is still checked against the entire transformed message list,
// so an unexpected edit cannot disappear from history reconciliation.
func (r *Request) ObserveOwnedInstructionCarrier(before, after []byte) {
	if r == nil {
		return
	}
	prior, known := sdkWireRequestFingerprints(before)
	next, projected := sdkInstructionEnvelopeFingerprints(after)
	if !known || !projected {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.finished || r.attempt != r.call.attempt || r.call.sdkQueryObserved {
		return
	}
	r.call.sdkInstructionProjection = &sdkInstructionProjection{before: prior, after: next}
}

func sdkInstructionEnvelopeFingerprints(body []byte) ([]string, bool) {
	var root struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Messages) == 0 || len(root.Messages) > maxSDKHistoryMessages {
		return nil, false
	}
	result := make([]string, 0, len(root.Messages))
	for _, row := range root.Messages {
		// Content normalization removes only already-reviewed cache decoration;
		// the remaining envelope is hashed too, including its original role.
		fingerprint, known := sdkWireMessageFingerprint("user", row["content"])
		if !known {
			return nil, false
		}
		delete(row, "content")
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, false
		}
		result = append(result, digest(string(encoded), fingerprint))
	}
	return result, true
}

func (p *sdkInstructionProjection) resolve(body []byte) ([]string, bool) {
	actual, known := sdkInstructionEnvelopeFingerprints(body)
	if !known || !slices.Equal(actual, p.after) {
		return nil, false
	}
	return slices.Clone(p.before), true
}
