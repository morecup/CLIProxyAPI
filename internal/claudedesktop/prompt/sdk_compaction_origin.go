package prompt

import "encoding/json"

// SDKCompactionOrigin describes the decision made by the native context owner.
// It is not inferred from a helper's HTTP headers or its success status.
type SDKCompactionOrigin struct {
	Kind            string
	ThresholdSource string
}

func (o SDKCompactionOrigin) Valid() bool {
	switch o.Kind {
	case "manual", "reactive":
		return o.ThresholdSource == ""
	case "auto":
		switch o.ThresholdSource {
		case "env", "settings", "clientdata", "experiment", "model-default", "unknown-model":
			return true
		}
	}
	return false
}

func (o SDKCompactionOrigin) HookTrigger() string {
	if o.Kind == "manual" {
		return "manual"
	}
	return "auto"
}

// CompactionOwnedBy checks content ownership independently of the trigger.
// Preflight callers must additionally prove that no API response was observed.
func (v *SDKCompactionView) CompactionOwnedBy(owner *Request) bool {
	return v != nil && owner != nil && v.owner == owner && v.Current()
}

// PreservedTranscriptUUIDCount proves the simple-message case of native
// transcript filtering. Attachments and tool rows need the context owner's
// actual projection; counting every retained UUID is not equivalent to _Ue.
func (v *SDKCompactionView) PreservedTranscriptUUIDCount(preserve []SDKHistoryMessage) (int, bool) {
	rows, err := v.Resolve(preserve)
	if err != nil {
		return 0, false
	}
	for _, message := range preserve {
		if (message.Type != "user" && message.Type != "assistant") || message.WireToolResultID != "" {
			return 0, false
		}
	}
	for _, raw := range rows {
		var row struct{ Content json.RawMessage }
		if json.Unmarshal(raw, &row) != nil {
			return 0, false
		}
		var text string
		if json.Unmarshal(row.Content, &text) == nil {
			continue
		}
		var blocks []struct{ Type string }
		if json.Unmarshal(row.Content, &blocks) != nil {
			return 0, false
		}
		for _, block := range blocks {
			if block.Type != "text" && block.Type != "thinking" && block.Type != "redacted_thinking" {
				return 0, false
			}
		}
	}
	return len(preserve), true
}
