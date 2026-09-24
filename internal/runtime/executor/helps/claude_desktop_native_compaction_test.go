package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClaudeDesktopNativeCompactionRunsEachOriginThroughHooksAndCommit(t *testing.T) {
	for _, kind := range []string{"manual", "auto", "reactive"} {
		for _, mode := range []string{"success", "blocked", "summary-error", "post-error", "no-commit", "foreign-context"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				summary, owner := summaryParamsFixture(t)
				summary.Origin = claudeprompt.SDKCompactionOrigin{Kind: kind}
				summary.CustomInstructions = "synthetic command instructions"
				summary.ObserveAttempt = func(attempt claudeprompt.SDKReactiveAttempt) {
					if len(attempt.Summarize) > 0 {
						attempt.Summarize[0].UUID = "observer-must-not-change-query"
					}
				}
				if kind == "auto" {
					summary.Origin.ThresholdSource = "settings"
				}
				if kind == "reactive" && !summary.View.ClaimReactiveFailure() {
					t.Fatal("missing claim")
				}
				var stages []string
				native := ClaudeDesktopRecoveryContext{
					Binding: cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: summary.Auth.ID, ProfileID: summary.Bundle.ProfileID, SessionID: summary.SessionID},
					Hooks: func(_ context.Context, input claudeprompt.SDKCompactHookInput) ([]claudeprompt.SDKCompactHookExecution, error) {
						stages = append(stages, input.Event)
						if input.Trigger != summary.Origin.HookTrigger() {
							t.Fatal("wrong hook trigger", input.Trigger)
						}
						if input.Event == "PreCompact" && (input.CustomInstructions == nil || *input.CustomInstructions != "synthetic command instructions") {
							t.Fatal("command instructions lost before PreCompact")
						}
						if input.Event == "PostCompact" && mode == "post-error" {
							return nil, errors.New("synthetic post failure")
						}
						return []claudeprompt.SDKCompactHookExecution{{Command: "synthetic", Succeeded: true, Blocked: mode == "blocked", Output: "synthetic hook instructions"}}, nil
					},
					SnapshotAndReset: func(context.Context, []claudeprompt.SDKHistoryMessage) (claudeprompt.SDKCompactionRestorationOps, error) {
						stages = append(stages, "reset")
						rows := func(context.Context) ([]json.RawMessage, error) { return nil, nil }
						row := func(context.Context) (json.RawMessage, error) { return nil, nil }
						return claudeprompt.SDKCompactionRestorationOps{ReadFiles: rows, LocalTasks: rows, PlanFile: row, PlanMode: row, InvokedSkills: row,
							DerivedContext: rows, SessionStart: rows, WrapAttachment: func(raw json.RawMessage) (json.RawMessage, error) {
								return claudeprompt.WrapSDKAttachment(raw, nil, nil)
							}}, nil
					},
				}
				if mode == "foreign-context" {
					native.Binding.AccountID = "foreign"
				}
				ctx := WithClaudeDesktopRecoveryContext(t.Context(), native)
				executor := summaryExecutorFunc(func(child context.Context, _ *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					stages = append(stages, "summary")
					if !bytes.Contains(request.Payload, []byte(`synthetic command instructions\n\nsynthetic hook instructions`)) {
						t.Fatal("command and hook instructions were not combined")
					}
					got, err := ClaudeDesktopCompactionRequestKind(child, http.Header{"X-Cc-Compaction-Request": {"wrong-wire-hint"}})
					if err != nil || got != kind {
						t.Fatal("owned origin lost before helper dispatch", got, err)
					}
					if mode == "summary-error" {
						return nil, errors.New("synthetic summary failure")
					}
					var response claudeprompt.SDKCompactionResponse
					response.ObserveJSON([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"<summary>PRIVATE_SUMMARY</summary>"}]}`))
					text, known := response.TakeText(summary.Bundle.DesktopVersion, summary.Bundle.CodeVersion)
					if !known {
						t.Fatal("unknown test summary")
					}
					owner.RecordSDKCompactionSuccess("compact", options.Metadata["claude_desktop_client_request_id"].(string), 1,
						claudeprompt.CompletedSDKCompaction(claudeprompt.ObserveSDKCompactionInput(request.Payload), text.Fingerprint()))
					return summarySyntheticStream(), nil
				})
				result, err := RunClaudeDesktopNativeCompaction(ctx, executor, ClaudeDesktopNativeCompactionParams{
					Summary: summary, Owner: owner,
					Apply: func(application *claudeprompt.SDKCompactionApplication) error {
						stages = append(stages, "commit")
						if mode == "no-commit" {
							return nil
						}
						body, _ := json.Marshal(map[string]any{"messages": application.Messages()})
						scope, _ := json.Marshal([]string{summary.Auth.ID, summary.Bundle.ProfileID, summary.Auth.ProxyURL})
						_, errCommit := application.Commit(ctx, claudeprompt.Input{AccountID: string(scope), SessionID: summary.SessionID,
							Role: "main", PromptID: owner.Identity().PromptID, ClientRequestID: uuid.NewString(), Body: body})
						return errCommit
					},
				})
				if result.Applied != (mode == "success") {
					t.Fatalf("result=%+v err=%v stages=%v", result, err, stages)
				}
				want := []string{"PreCompact", "summary", "reset", "PostCompact", "commit"}
				switch mode {
				case "success":
					if err != nil || !owner.Snapshot().SDK.SawCompact {
						t.Fatal("native application not adopted", err)
					}
				case "blocked":
					want = want[:1]
					if result.Reason != "hook_blocked" {
						t.Fatal(result)
					}
				case "summary-error":
					want = want[:2]
					if result.Reason != "error" {
						t.Fatal(result)
					}
				case "post-error":
					want = want[:4]
					if err == nil {
						t.Fatal("missing post failure")
					}
				case "no-commit":
					if err == nil {
						t.Fatal("callback manufactured a commit")
					}
				case "foreign-context":
					want = nil
					if err == nil {
						t.Fatal("foreign context accepted")
					}
				}
				if !reflect.DeepEqual(stages, want) {
					t.Fatalf("stages=%v want=%v", stages, want)
				}
			})
		}
	}
}
