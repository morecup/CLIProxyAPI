package prompt

import "encoding/json"

// StripSDKWireMedia applies the pinned iPt user-content branch to resolved
// API rows. It replaces direct images/documents and media one level inside a
// tool result. Assistant content and all unrelated fields remain untouched.
// The original view is not mutated; this is only the summary retry payload.
func StripSDKWireMedia(rows []json.RawMessage) ([]json.RawMessage, error) {
	result := append([]json.RawMessage(nil), rows...)
	for index, raw := range rows {
		fields, err := sdkAttachmentObject(raw)
		if err != nil {
			return nil, ErrSDKCompactionContentUnknown
		}
		if sdkRestorationString(fields, "role") != "user" {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(fields["content"], &blocks) != nil {
			continue
		}
		changed := false
		for i, block := range blocks {
			value, err := sdkAttachmentObject(block)
			if err != nil {
				return nil, ErrSDKCompactionContentUnknown
			}
			switch sdkRestorationString(value, "type") {
			case "image":
				blocks[i], changed = json.RawMessage(`{"type":"text","text":"[image]"}`), true
			case "document":
				blocks[i], changed = json.RawMessage(`{"type":"text","text":"[document]"}`), true
			case "tool_result":
				var nested []json.RawMessage
				if json.Unmarshal(value["content"], &nested) != nil {
					continue
				}
				innerChanged := false
				for j, inner := range nested {
					var media struct{ Type string }
					if json.Unmarshal(inner, &media) != nil {
						continue
					}
					if media.Type == "image" {
						nested[j], innerChanged = json.RawMessage(`{"type":"text","text":"[image]"}`), true
					}
					if media.Type == "document" {
						nested[j], innerChanged = json.RawMessage(`{"type":"text","text":"[document]"}`), true
					}
				}
				if innerChanged {
					value["content"], _ = sdkAttachmentJSON(nested)
					blocks[i], err = sdkAttachmentJSON(value)
					if err != nil {
						return nil, err
					}
					changed = true
				}
			}
		}
		if changed {
			fields["content"], err = sdkAttachmentJSON(blocks)
			if err != nil {
				return nil, err
			}
			result[index], err = sdkAttachmentJSON(fields)
			if err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
