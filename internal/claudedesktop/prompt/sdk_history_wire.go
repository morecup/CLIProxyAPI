package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
)

// Keep the original pure-text hash for existing durable session records.
func sdkTextMessageHash(role string) hash.Hash {
	h := sha256.New()
	_, _ = h.Write([]byte(role + "\x00"))
	return h
}

func sdkTextMessageFingerprint(role string, content json.RawMessage) (string, bool) {
	if role != "user" && role != "assistant" {
		return "", false
	}
	h := sdkTextMessageHash(role)
	var text string
	if json.Unmarshal(content, &text) == nil {
		_, _ = h.Write([]byte(text))
	} else {
		var blocks []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil || blocks == nil {
			return "", false
		}
		for _, block := range blocks {
			if block.Type != "text" || block.Text == nil {
				return "", false
			}
			_, _ = h.Write([]byte(*block.Text))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func sdkWireRequestFingerprints(body []byte) ([]string, bool) {
	return sdkWireHistoryFingerprints(body, true)
}

func sdkWireHistoryFingerprints(body []byte, requireInput bool) ([]string, bool) {
	var root struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Messages) == 0 || len(root.Messages) > maxSDKHistoryMessages || (requireInput && root.Messages[len(root.Messages)-1].Role != "user") {
		return nil, false
	}
	result := make([]string, 0, len(root.Messages))
	for _, message := range root.Messages {
		contents := []json.RawMessage{message.Content}
		if blocks, pureResults := sdkWireToolResults(message.Content); message.Role == "user" && pureResults {
			// Each owned tool result is one native user yield. The HTTP
			// normalizer can merge or split their rows without changing it.
			contents = make([]json.RawMessage, len(blocks))
			for index, block := range blocks {
				contents[index] = append(append(json.RawMessage{'['}, block...), ']')
			}
		}
		for _, content := range contents {
			fingerprint, known := sdkWireMessageFingerprint(message.Role, content)
			if !known || len(result) >= maxSDKHistoryMessages {
				return nil, false
			}
			result = append(result, fingerprint)
		}
	}
	return result, true
}

func (c *call) observeSDKWireHistory(body []byte) {
	c.sdkWireInputs, c.sdkWireInputKnown = sdkWireRequestFingerprints(body)
	if projection := c.sdkInstructionProjection; projection != nil {
		c.sdkWireInputs, c.sdkWireInputKnown = projection.resolve(body)
		c.sdkInstructionProjection = nil
	}
	sum := sha256.Sum256(body)
	c.sdkWireRequestDigest = hex.EncodeToString(sum[:])
	if !c.reconcileHistory {
		return
	}
	c.reconcileHistory = false
	h := c.state.sdk.history
	if h == nil {
		return
	}
	h.pendingReconciliations--
	if !c.sdkWireInputKnown || !h.expectedTextKnown {
		h.unknown("unreconciled-sdk-caller-history")
		return
	}
	added := len(c.sdkUserHistoryOffsets)
	if added == 0 || len(c.sdkWireInputs) != len(h.expectedText)+added {
		h.unknown("changed-sdk-caller-history")
		return
	}
	for index, expected := range h.expectedText {
		if c.sdkWireInputs[index] != expected {
			h.unknown("changed-sdk-caller-history")
			return
		}
	}
}

func (r *Request) ObserveSDKWireResponse(fingerprint string, known, complete bool) {
	if r == nil || !complete {
		return
	}
	r.tracker.mu.Lock()
	defer r.tracker.mu.Unlock()
	if r.finished || r.attempt != r.call.attempt {
		return
	}
	r.sdkWireResponse, r.sdkWireResponseKnown = fingerprint, known && len(fingerprint) == sha256.Size*2
}

func (r *Request) commitSDKWireHistory() {
	h := r.call.state.sdk.history
	if h == nil {
		return
	}
	h.expectedTextKnown = r.call.sdkWireInputKnown && r.sdkWireResponseKnown
	h.expectedText = nil
	if h.expectedTextKnown {
		h.expectedText = append(append([]string(nil), r.call.sdkWireInputs...), r.sdkWireResponse)
	}
}
