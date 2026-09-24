package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type summaryExecutorFunc func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)

func (f summaryExecutorFunc) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, request cliproxyexecutor.Request, options cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return f(ctx, auth, request, options)
}

type summaryStatusError string

func (e summaryStatusError) Error() string { return string(e) }
func (summaryStatusError) StatusCode() int { return 400 }

func summaryParamsFixture(t *testing.T) (ClaudeDesktopReactiveSummaryParams, *claudeprompt.Request) {
	t.Helper()
	bundle, err := claudeprofile.BuiltinCurrent()
	if err != nil {
		t.Fatal(err)
	}
	params := ClaudeDesktopReactiveSummaryParams{Auth: &cliproxyauth.Auth{ID: "synthetic-account"}, Bundle: bundle, SessionID: uuid.NewString()}
	var tracker claudeprompt.Tracker
	var messages []map[string]any
	var owner *claudeprompt.Request
	for n := 0; n < 3; n++ {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("PRIVATE_INPUT_%d", n)})
		params.ParentRequest, err = json.Marshal(map[string]any{"model": "claude-opus-5-5", "messages": messages})
		if err != nil {
			t.Fatal(err)
		}
		owner = BeginClaudeDesktopPrompt(&tracker, t.Context(), params.Auth, bundle.ProfileID, "main", params.SessionID, uuid.NewString(), uuid.NewString(), params.ParentRequest)
		owner.ObserveSDKQuery(params.ParentRequest)
		if n == 2 {
			break
		}
		var response claudeprompt.Response
		response.ObservePayloadAt([]byte(fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","content":[{"type":"text","text":"PRIVATE_REPLY"}],"stop_reason":"end_turn"}`, n)), false, time.Now())
		owner.ObserveSDKHistory(response.SDKHistoryMessages())
		owner.ObserveSDKWireResponse(response.SDKWireFingerprint())
		owner.FinishSuccess(time.Now(), "end_turn", nil)
		messages = append(messages, map[string]any{"role": "assistant", "content": "PRIVATE_REPLY"})
	}
	params.View, err = owner.CompactionView(params.ParentRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(params.View.Discard)
	return params, owner
}

func summarySyntheticStream() *cliproxyexecutor.StreamResult {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_summary\",\"role\":\"assistant\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"<summary>PRIVATE_SUMMARY</summary>\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}
}

func TestClaudeDesktopSummaryScopeAndStaleInputDoNotDispatch(t *testing.T) {
	for _, mode := range []string{"account", "profile", "egress", "session", "body", "finished", "discarded"} {
		t.Run(mode, func(t *testing.T) {
			params, owner := summaryParamsFixture(t)
			switch mode {
			case "account":
				params.Auth.ID = "foreign"
			case "profile":
				params.Bundle.ProfileID = "foreign"
			case "egress":
				params.Auth.ProxyURL = "http://synthetic.invalid:8080"
			case "session":
				params.SessionID = uuid.NewString()
			case "body":
				params.ParentRequest = append(params.ParentRequest, ' ')
			case "finished":
				owner.FinishFailure()
			case "discarded":
				params.View.Discard()
			}
			executor := summaryExecutorFunc(func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				t.Fatal("foreign or stale summary dispatched")
				return nil, nil
			})
			result, err := RunClaudeDesktopReactiveSummary(t.Context(), executor, params)
			if result.ReadyToApply || !errors.Is(err, claudeprompt.ErrSDKCompactionViewStale) {
				t.Fatal("ownership check did not fail closed")
			}
		})
	}
}

func TestClaudeDesktopSummaryCancellationAndLateResult(t *testing.T) {
	for _, mode := range []string{"cancel-before", "cancel-during", "stale-during", "success"} {
		t.Run(mode, func(t *testing.T) {
			params, owner := summaryParamsFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel-before" {
				cancel()
			}
			var child context.Context
			executor := summaryExecutorFunc(func(c context.Context, a *cliproxyauth.Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				child = c
				var body struct {
					Model string `json:"model"`
				}
				if errBody := json.Unmarshal(r.Payload, &body); errBody != nil || r.Model != "claude-opus-5-5" || body.Model != "claude-opus-5-5" {
					t.Fatalf("compaction helper model = request %q payload %q, want claude-opus-5-5 (decode error: %v)", r.Model, body.Model, errBody)
				}
				if ClaudeDesktopSessionUUID(c, a, params.Bundle.ProfileID, "wrong") != params.SessionID {
					t.Fatal("bound session lost")
				}
				if len(o.Headers) != 0 || strings.Contains(string(r.Payload), "diagnostics") {
					t.Fatal("main request decoration inherited")
				}
				if kind, errKind := ClaudeDesktopCompactionRequestKind(c, nil); kind != "reactive" || errKind != nil {
					t.Fatalf("PTL helper origin=%q err=%v", kind, errKind)
				}
				if kind, errKind := ClaudeDesktopCompactionRequestKind(ctx, nil); kind != "manual" || errKind != nil {
					t.Fatal("helper origin leaked to parent context")
				}
				if mode == "cancel-during" {
					cancel()
				}
				if mode == "stale-during" {
					owner.FinishFailure()
				}
				return summarySyntheticStream(), nil
			})
			result, err := RunClaudeDesktopReactiveSummary(ctx, executor, params)
			defer result.Payload.Discard()
			if mode == "stale-during" {
				if !errors.Is(err, claudeprompt.ErrSDKCompactionViewStale) || result.ReadyToApply {
					t.Fatal("late summary accepted")
				}
			} else if mode == "success" {
				if err != nil || !result.ReadyToApply || ctx.Err() != nil {
					t.Fatal("valid summary did not remain request-local")
				}
				encoded, _ := json.Marshal(result.Payload)
				if strings.Contains(string(encoded), "PRIVATE_") {
					t.Fatal("summary plaintext serialized")
				}
			} else if err != nil || result.Reason != "aborted" || result.ReadyToApply {
				t.Fatal("cancelled summary accepted")
			}
			if child != nil && child.Err() == nil {
				t.Fatal("summary child cancellation was not released")
			}
			if owner.Snapshot().SDK.SawCompact {
				t.Fatal("draft generation applied history")
			}
		})
	}
}

func TestClaudeDesktopSummaryFallbackBuildsMarkerAndRecentWrapper(t *testing.T) {
	params, _ := summaryParamsFixture(t)
	var attempts []claudeprompt.SDKReactiveAttempt
	params.ObserveAttempt = func(attempt claudeprompt.SDKReactiveAttempt) {
		attempt.Summarize, attempt.Preserve = nil, nil
		attempts = append(attempts, attempt)
	}
	calls := 0
	executor := summaryExecutorFunc(func(_ context.Context, _ *cliproxyauth.Auth, request cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		calls++
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(request.Payload, &body); err != nil || len(body.Messages) == 0 {
			t.Fatalf("invalid summary request: %v", err)
		}
		if calls == 1 {
			return nil, summaryStatusError("prompt is too long")
		}
		var marker string
		if body.Messages[0].Role != "user" || json.Unmarshal(body.Messages[0].Content, &marker) != nil || marker != "[earlier conversation truncated for compaction retry]" {
			t.Fatalf("fallback marker=%q row=%+v", marker, body.Messages[0])
		}
		return summarySyntheticStream(), nil
	})
	result, err := RunClaudeDesktopReactiveSummary(t.Context(), executor, params)
	defer result.Payload.Discard()
	if err != nil || !result.ReadyToApply || calls != 2 || result.SplitKind != "summarize_all" || result.HeadTruncations != 1 ||
		result.GroupsPreserved != 0 || len(result.Preserve) != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
	}
	if len(attempts) != 2 || attempts[0].SplitKind != "round" || attempts[1].SplitKind != "summarize_all" ||
		attempts[1].HeadTruncations != 1 || attempts[1].GroupsToPreserve != 0 {
		t.Fatalf("attempts=%+v", attempts)
	}
	var draft struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(result.Payload.Message, &draft) != nil || !strings.Contains(draft.Content, "Recent messages are preserved verbatim.") {
		t.Fatal("draft wrapper omitted the head-truncation preservation notice")
	}
	rows := result.Payload.Application.Messages()
	var applied struct {
		Content string `json:"content"`
	}
	if len(rows) != 2 || json.Unmarshal(rows[0], &applied) != nil || !strings.Contains(applied.Content, "Recent messages are preserved verbatim.") {
		t.Fatal("application wrapper omitted the head-truncation preservation notice")
	}
}

func TestClaudeDesktopInternalSessionBindingDoesNotCrossScope(t *testing.T) {
	params, _ := summaryParamsFixture(t)
	binding := cliproxyexecutor.ClaudeDesktopSessionBinding{AccountID: params.Auth.ID, ProfileID: params.Bundle.ProfileID, SessionID: params.SessionID}
	for _, mode := range []string{"account", "profile", "egress", "invalid", "nil", "matching"} {
		b := binding
		switch mode {
		case "account":
			b.AccountID = "other"
		case "profile":
			b.ProfileID = "other"
		case "egress":
			b.Egress = "other"
		case "invalid":
			b.SessionID = "not UUID"
		}
		ctx := cliproxyexecutor.WithClaudeDesktopSessionBinding(t.Context(), b)
		if mode == "nil" {
			ctx = nil
		}
		want := "fallback"
		if mode == "matching" {
			want = params.SessionID
		}
		if got := ClaudeDesktopSessionUUID(ctx, params.Auth, params.Bundle.ProfileID, "fallback"); got != want {
			t.Fatalf("binding guard failed: %s", mode)
		}
	}
}

func TestClaudeDesktopSummaryInstructionMatchesNativeTextMerge(t *testing.T) {
	data, err := os.ReadFile("../../../claudedesktop/prompt/testdata/sdk-summary-dependencies-native.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Merges []struct {
			Name                  string
			Left, Right, Expected json.RawMessage
		}
	}
	if json.Unmarshal(data, &vectors) != nil || len(vectors.Merges) != 5 {
		t.Fatal("invalid native merge vectors")
	}
	for _, v := range vectors.Merges {
		t.Run(v.Name, func(t *testing.T) {
			row := json.RawMessage(`{"role":"user","content":` + string(v.Left) + `}`)
			rows, err := appendClaudeDesktopSummaryInstruction([]json.RawMessage{row}, "instruction")
			var actual struct{ Content any }
			var want any
			if err != nil || len(rows) != 1 || json.Unmarshal(rows[0], &actual) != nil || json.Unmarshal(v.Expected, &want) != nil || !reflect.DeepEqual(actual.Content, want) {
				t.Fatal("native text merge differs")
			}
		})
	}
}
