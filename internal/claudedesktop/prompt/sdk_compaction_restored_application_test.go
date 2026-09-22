package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func restoredApplicationFixture(t *testing.T, payloads ...string) (SDKCompactionRestoration, SDKCompactHookOutcome) {
	t.Helper()
	var attachments []json.RawMessage
	for _, payload := range payloads {
		wrapped, err := WrapSDKAttachment([]byte(payload), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		attachments = append(attachments, wrapped)
	}
	// Synthetic owned operations are installed explicitly. These are not
	// production defaults and do not perform file reads or execute commands.
	ops := syntheticRestorationOps(func(stage string, many bool) ([]json.RawMessage, error) {
		if stage == "files" {
			return attachments, nil
		}
		return nil, nil
	})
	restored, err := RestoreSDKCompactionContext(t.Context(), ops, false, false)
	if err != nil {
		t.Fatal(err)
	}
	post, err := RunSDKPostCompactHooks(t.Context(), func(context.Context, SDKCompactHookInput) ([]SDKCompactHookExecution, error) {
		return []SDKCompactHookExecution{{Command: "synthetic", Output: "observed", Succeeded: true}}, nil
	}, "auto", "synthetic summary", false)
	if err != nil {
		t.Fatal(err)
	}
	return restored, post
}

func TestSDKRestoredApplicationReachesWireAndNextCompaction(t *testing.T) {
	owner, _, application, nextInput := stagedCompactionFixture(t)
	beforeHistory, beforeSDK, beforeTokens := owner.SDKHistory(), owner.Snapshot().SDK, application.PostTokens()
	beforeRows := application.Messages()
	restored, post := restoredApplicationFixture(t,
		`{"type":"compact_file_reference","filename":"C:/synthetic/old.txt"}`,
		`{"type":"already_read_file","filename":"C:/synthetic/kept.txt"}`,
		`{"type":"plan_file_reference","planFilePath":"C:/synthetic/plan.md","planContent":"PRIVATE_PLAN"}`,
		`{"type":"hook_additional_context","hookName":"SessionStart","content":["PRIVATE_HOOK"]}`)
	// SessionStart results follow the regular attachments, not completion order.
	restored.HookResults, restored.Attachments = restored.Attachments[3:], restored.Attachments[:3]
	if err := application.applyRestoration(restored, post, attachmentTestOptions()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeHistory, owner.SDKHistory()) || owner.Snapshot().SDK != beforeSDK {
		t.Fatal("staging changed shared prompt state")
	}
	rows := application.Messages()
	if len(rows) != len(beforeRows) || application.PostTokens() <= beforeTokens {
		t.Fatal("restoration did not merge into the retained user tail")
	}
	var last struct {
		Content []struct{ Type, Text string }
	}
	if json.Unmarshal(rows[len(rows)-1], &last) != nil || len(last.Content) != 4 ||
		!strings.HasPrefix(last.Content[1].Text, "<system-reminder>\nNote:") || !strings.Contains(last.Content[2].Text, "PRIVATE_PLAN") || !strings.Contains(last.Content[3].Text, "PRIVATE_HOOK") {
		t.Fatal("native reminder ordering or block boundaries differ")
	}
	nextInput.Body, _ = json.Marshal(map[string]any{"messages": rows})
	next, err := application.Commit(t.Context(), nextInput)
	if err != nil {
		t.Fatal(err)
	}
	history := next.SDKHistory()
	if !history.OwnedMessagesKnown || next.Snapshot().SDK.NumTurns != beforeSDK.NumTurns+1 {
		t.Fatal("native attachments became extra user submissions")
	}
	added := history.Messages[len(history.Messages)-4:]
	if added[0].WireParentUUID == "" || !added[1].NoWireContent || added[1].WireParentUUID != "" || added[1].TokenEstimate.Tokens != 0 || added[2].WireParentUUID != added[0].WireParentUUID {
		t.Fatal("native attachment ownership or no-wire accounting was lost")
	}
	view, err := next.CompactionView(nextInput.Body)
	if err != nil {
		t.Fatal("adopted attachment history cannot be compacted again", err)
	}
	defer view.Discard()
	resolved, err := view.Resolve(view.History().Messages)
	if err != nil {
		t.Fatal(err)
	}
	attachmentRowsEqual(t, resolved, rows)
	if _, err = view.Resolve([]SDKHistoryMessage{added[0]}); !errors.Is(err, ErrSDKCompactionContentUnknown) {
		t.Fatal("partial merged-row ownership was accepted")
	}
	encoded, _ := json.Marshal(history)
	if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), "C:/synthetic") {
		t.Fatal("restored content or paths entered structural history")
	}
	if err = application.applyRestoration(restored, post, attachmentTestOptions()); err == nil {
		t.Fatal("adopted restoration was appended twice")
	}
}

func TestSDKRestoredMediaApplicationRetainsOwnedAttachment(t *testing.T) {
	owner, _, application, nextInput := stagedCompactionFixture(t)
	before := owner.SDKHistory()
	restored, post := restoredApplicationFixture(t, `{"type":"file","filename":"synthetic","content":{"type":"image","file":{"base64":"c3ludGhldGlj","type":"image/png"}}}`)
	if err := application.applyRestoration(restored, post, attachmentTestOptions()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owner.SDKHistory(), before) {
		t.Fatal("draft mutated shared history")
	}
	nextInput.Body, _ = json.Marshal(map[string]any{"messages": application.Messages()})
	next, err := application.Commit(t.Context(), nextInput)
	if err != nil {
		t.Fatal(err)
	}
	view, err := next.CompactionView(nextInput.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Discard()
	rows, err := view.Resolve(view.History().Messages)
	if err != nil {
		t.Fatal(err)
	}
	attachmentRowsEqual(t, rows, application.Messages())
	if !strings.Contains(string(nextInput.Body), "c3ludGhldGlj") || strings.Contains(fmt.Sprint(next.SDKHistory()), "c3ludGhldGlj") {
		t.Fatal("media ownership or structural-only retention differs")
	}
}

func TestSDKRestoredApplicationRejectsIncompleteStagesAtomically(t *testing.T) {
	for _, mode := range []string{"missing-restoration", "missing-post", "reducer-only", "pre-not-post", "duplicate-uuid", "missing-uuid", "wrong-case", "unknown-attachment", "non-native-hook", "image-malformed", "stale-owner"} {
		t.Run(mode, func(t *testing.T) {
			owner, _, application, nextInput := stagedCompactionFixture(t)
			restored, post := restoredApplicationFixture(t, `{"type":"compact_file_reference","filename":"synthetic"}`)
			switch mode {
			case "missing-restoration":
				restored = SDKCompactionRestoration{}
			case "missing-post":
				post = SDKCompactHookOutcome{}
			case "reducer-only":
				post = ReduceSDKPostCompactHooks(nil)
			case "pre-not-post":
				post, _ = RunSDKPreCompactHooks(t.Context(), func(context.Context, SDKCompactHookInput) ([]SDKCompactHookExecution, error) { return nil, nil }, "auto", nil, false)
			case "duplicate-uuid":
				restored.Attachments = append(restored.Attachments, restored.Attachments[0])
			case "missing-uuid":
				restored.Attachments[0] = syntheticRestorationRow("already_read_file")
			case "wrong-case":
				restored.Attachments[0] = []byte(strings.Replace(string(restored.Attachments[0]), `"attachment":`, `"Attachment":`, 1))
			case "unknown-attachment":
				restored.Attachments[0], _ = WrapSDKAttachment([]byte(`{"type":"unknown"}`), nil, nil)
			case "non-native-hook":
				restored.HookResults = []json.RawMessage{[]byte(`{"role":"user","content":"unowned"}`)}
			case "image-malformed":
				restored.Attachments[0], _ = WrapSDKAttachment([]byte(`{"type":"file","filename":"synthetic","content":{"type":"image","file":{"base64":42,"type":"image/png"}}}`), nil, nil)
			case "stale-owner":
				nextInput.ClientRequestID = uuid.NewString()
				owner.tracker.Begin(nextInput)
			}
			beforeRows, beforeHistory, beforeTokens := application.Messages(), owner.SDKHistory(), application.PostTokens()
			if err := application.applyRestoration(restored, post, attachmentTestOptions()); err == nil {
				t.Fatal("incomplete restoration was adopted")
			}
			if !reflect.DeepEqual(beforeRows, application.Messages()) || !reflect.DeepEqual(beforeHistory, owner.SDKHistory()) || beforeTokens != application.PostTokens() || application.restored {
				t.Fatal("failed restoration partially changed the draft or owner")
			}
		})
	}
}

func TestSDKRestoreContextRejectsForeignOwnerOrSummaryBeforeOperations(t *testing.T) {
	for _, mode := range []string{"nil-owner", "foreign-owner", "different-selected-text", "different-summary", "cancelled", "missing-post"} {
		t.Run(mode, func(t *testing.T) {
			owner, _, application, _ := stagedCompactionFixture(t)
			var response SDKCompactionResponse
			response.ObserveJSON([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"<summary>PRIVATE_SUMMARY</summary>"}]}`))
			summary, known := response.TakeText("1.40609.0.0", "2.1.247")
			if !known {
				t.Fatal("unresolved fixture")
			}
			calls := 0
			params := SDKCompactionRestoreParams{Owner: owner, Summary: summary,
				Operations: syntheticRestorationOps(func(string, bool) ([]json.RawMessage, error) { calls++; return nil, nil }),
				PostHooks: func(context.Context, SDKCompactHookInput) ([]SDKCompactHookExecution, error) {
					calls++
					return nil, nil
				}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "nil-owner":
				params.Owner = nil
			case "foreign-owner":
				params.Owner, _, _, _ = stagedCompactionFixture(t)
			case "different-selected-text":
				params.Summary.selected += "\n"
			case "different-summary":
				params.Summary.normalized += "changed"
			case "cancelled":
				cancel()
			case "missing-post":
				params.PostHooks = nil
			}
			if _, err := application.RestoreContext(ctx, params); err == nil || calls != 0 || application.restored {
				t.Fatal("invalid restore ownership reached an operation", mode, err, calls)
			}
		})
	}
}
