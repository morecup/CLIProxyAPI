package prompt

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

type SDKCompactionRestoreParams struct {
	Owner         *Request
	Trigger       string
	Summary       SDKCompactionText
	Operations    SDKCompactionRestorationOps
	PostHooks     SDKCompactHookRunner
	Normalize     SDKAttachmentNormalizeOptions
	Isolated      bool
	RemoteEnabled bool
}

// RestoreContext runs restoration and awaited PostCompact for this exact
// summary's owner. Callers cannot reuse a completed hook/result from another
// request. Operations must already be bound to that owner's native context.
func (a *SDKCompactionApplication) RestoreContext(ctx context.Context, params SDKCompactionRestoreParams) (SDKCompactionRestoration, error) {
	if ctx == nil || a == nil || a.view == nil || a.view.owner != params.Owner || !a.view.Current() || a.restored || params.Summary.Fingerprint() != a.summary || sdkCompactionHash(params.Summary.SelectedText()) != a.selectedHash {
		return SDKCompactionRestoration{}, ErrSDKCompactionViewStale
	}
	if err := ctx.Err(); err != nil {
		return SDKCompactionRestoration{}, err
	}
	if !params.Isolated && params.PostHooks == nil {
		return SDKCompactionRestoration{}, ErrSDKCompactionHooksUnknown
	}
	trigger := params.Trigger
	if trigger == "" {
		trigger = "auto"
	}
	if trigger != "auto" && trigger != "manual" {
		return SDKCompactionRestoration{}, ErrSDKCompactionHooksUnknown
	}
	restored, err := RestoreSDKCompactionContext(ctx, params.Operations, params.Isolated, params.RemoteEnabled)
	if err != nil {
		return restored, err
	}
	post, err := RunSDKPostCompactHooks(ctx, params.PostHooks, trigger, params.Summary.SelectedText(), params.Isolated)
	if err != nil {
		return restored, err
	}
	if err = ctx.Err(); err != nil {
		return restored, err
	}
	return restored, a.applyRestoration(restored, post, params.Normalize)
}

// applyRestoration stages the real s1e/PZo result after an awaited bV. It is a
// synchronous request-owner operation, like PrepareApplication and Discard;
// do not run it concurrently with Commit. No shared history changes until
// Commit validates the final wire body and acquires the tracker lock.
// A zero-value result or a pure hook reducer cannot impersonate executed work.
func (a *SDKCompactionApplication) applyRestoration(restoration SDKCompactionRestoration, post SDKCompactHookOutcome, options SDKAttachmentNormalizeOptions) error {
	if a == nil || a.view == nil || !a.view.Current() || a.restored {
		return ErrSDKCompactionViewStale
	}
	if !restoration.resolved {
		return ErrSDKCompactionRestorationUnknown
	}
	if post.completedEvent != "PostCompact" {
		return ErrSDKCompactionHooksUnknown
	}
	rows := a.Messages()
	messages := append([]SDKHistoryMessage(nil), a.messages...)
	postTokens := a.postTokens
	stagedNativeRows := append([]SDKNativeMessage(nil), a.nativeRows...)
	seen := make(map[string]bool, len(messages))
	anchor := ""
	for _, message := range messages {
		seen[message.UUID] = true
		if message.Type == "user" {
			anchor = message.UUID
		}
	}
	if anchor == "" || len(rows) == 0 {
		return ErrSDKCompactionContentUnknown
	}
	// yV orders all restoration attachments before all SessionStart outputs.
	for _, nativeRows := range [][]json.RawMessage{restoration.Attachments, restoration.HookResults} {
		for _, raw := range nativeRows {
			envelope, err := sdkAttachmentObject(raw)
			if err != nil || sdkRestorationString(envelope, "type") != "attachment" {
				return ErrSDKAttachmentUnknown
			}
			id := sdkRestorationString(envelope, "uuid")
			if parsed, errUUID := uuid.Parse(id); errUUID != nil || parsed == uuid.Nil || seen[id] {
				return ErrSDKAttachmentUnknown
			}
			if !sdkAttachmentStrings(envelope, "timestamp") || sdkRestorationString(envelope, "timestamp") == "" {
				return ErrSDKAttachmentUnknown
			}
			attachment, err := sdkAttachmentObject(envelope["attachment"])
			if err != nil {
				return err
			}
			normalized, err := NormalizeSDKAttachment(envelope["attachment"], options)
			if err != nil {
				return err
			}
			message := SDKHistoryMessage{Type: "attachment", UUID: id, Subtype: sdkRestorationString(attachment, "type"),
				TokenEstimate: SDKTokenEstimate{Known: true}, NoWireContent: len(normalized) == 0}
			if len(normalized) != 0 {
				message.WireParentUUID = anchor
			}
			for _, row := range normalized {
				fields, errFields := sdkAttachmentObject(row)
				if errFields != nil || sdkRestorationString(fields, "role") != "user" {
					return ErrSDKAttachmentUnknown
				}
				if _, known := sdkWireMessageFingerprint("user", fields["content"]); !known {
					return ErrSDKCompactionContentUnknown
				}
				estimate := EstimateSDKContent(fields["content"])
				if !estimate.Known {
					return ErrSDKReactiveEstimateUnknown
				}
				message.TokenEstimate.Tokens += estimate.Tokens
				rows[len(rows)-1], err = mergeSDKRestorationUserRow(rows[len(rows)-1], fields["content"])
				if err != nil {
					return err
				}
			}
			seen[id] = true
			messages = append(messages, message)
			stagedNativeRows = append(stagedNativeRows, SDKNativeMessage{Type: "attachment", UUID: id, Timestamp: sdkRestorationString(envelope, "timestamp"), Attachment: append(json.RawMessage(nil), envelope["attachment"]...)})
			postTokens += message.TokenEstimate.Tokens
		}
	}
	if len(messages) > maxSDKHistoryMessages {
		return ErrSDKCompactionContentUnknown
	}
	body, err := sdkAttachmentJSON(struct {
		Messages []json.RawMessage `json:"messages"`
	}{rows})
	if err != nil || len(body) > maxSDKCompactionViewBytes {
		return ErrSDKCompactionContentUnknown
	}
	fingerprints, known := sdkWireRequestFingerprints(body)
	if !known {
		return ErrSDKCompactionContentUnknown
	}
	if !a.view.Current() {
		return ErrSDKCompactionViewStale
	}
	a.rows, a.messages, a.fingerprints, a.postTokens, a.restored = rows, messages, fingerprints, postTokens, true
	a.nativeRows = stagedNativeRows
	return nil
}

// nZe output goes through roo, not the human-turn WQe merge. Use only reviewed
// normalization whose outcome does not require an unavailable feature state.
func mergeSDKRestorationUserRow(previous json.RawMessage, content json.RawMessage) (json.RawMessage, error) {
	fields, err := sdkAttachmentObject(previous)
	if err != nil || sdkRestorationString(fields, "role") != "user" {
		return nil, ErrSDKCompactionContentUnknown
	}
	fields["content"], err = MergeSDKWireUserContent(fields["content"], content)
	if err != nil {
		return nil, err
	}
	return sdkAttachmentJSON(fields)
}
