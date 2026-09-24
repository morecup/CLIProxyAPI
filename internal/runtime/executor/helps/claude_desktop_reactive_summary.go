package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudeprompt "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/prompt"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/sjson"
)

// ClaudeDesktopCompactionModel is the captured model used by Desktop's
// compaction helper independently of the model selected for the main query.
const ClaudeDesktopCompactionModel = "claude-opus-5"

type ClaudeDesktopSummaryExecutor interface {
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

type ClaudeDesktopReactiveSummaryParams struct {
	Auth               *cliproxyauth.Auth
	Bundle             *claudeprofile.Bundle
	View               *claudeprompt.SDKCompactionView
	ParentRequest      []byte
	SessionID          string
	CustomInstructions string
	InitialTokenGap    *int64
	// Called at the native iteration boundary, before preparing or dispatching
	// its helper. HTTP retries inside that helper do not invoke this callback.
	ObserveAttempt func(claudeprompt.SDKReactiveAttempt)
}

// A draft owns request-local content, not an application success. The caller
// must restore context/run PostCompact and atomically replace owned history
// before claiming adoption. Neither content field is serialized to telemetry.
type ClaudeDesktopSummaryDraft struct {
	Application       *claudeprompt.SDKCompactionApplication `json:"-"`
	Text              claudeprompt.SDKCompactionText         `json:"-"`
	Message           json.RawMessage                        `json:"-"`
	AssistantMessages int                                    `json:"assistant_messages"`
	ClientRequestID   string                                 `json:"client_request_id"`
	Usage             claudeprompt.SDKTokenUsage             `json:"usage"`
	UsageKnown        bool                                   `json:"usage_known"`
}

func (d *ClaudeDesktopSummaryDraft) Discard() {
	if d != nil {
		if d.Application != nil {
			d.Application.Discard()
		}
		*d = ClaudeDesktopSummaryDraft{}
	}
}

// RunClaudeDesktopReactiveSummary dispatches real compact-role stream queries
// through the provided executor, using the native attempt loop and an owned
// request-local content view. It does not install a trigger in a main request,
// write a transcript, execute a tool, or replace the caller's conversation.
func RunClaudeDesktopReactiveSummary(ctx context.Context, executor ClaudeDesktopSummaryExecutor, params ClaudeDesktopReactiveSummaryParams) (claudeprompt.SDKReactiveResult[ClaudeDesktopSummaryDraft], error) {
	var empty claudeprompt.SDKReactiveResult[ClaudeDesktopSummaryDraft]
	if executor == nil || params.Auth == nil || params.Bundle == nil || params.View == nil {
		return empty, errors.New("missing Desktop compaction query owner")
	}
	scope, _ := json.Marshal([]string{params.Auth.ID, params.Bundle.ProfileID, params.Auth.ProxyURL})
	if !params.View.MatchesScope(string(scope), params.SessionID) || !params.View.MatchesRequestBody(params.ParentRequest) {
		return empty, claudeprompt.ErrSDKCompactionViewStale
	}
	instruction, err := params.Bundle.CompactionInstruction(params.CustomInstructions)
	if err != nil {
		return empty, err
	}
	var parent struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(params.ParentRequest, &parent) != nil || parent.Model == "" {
		return empty, errors.New("missing Desktop compaction model")
	}
	if _, err = params.Bundle.Resolve(claudeprofile.RequestVariantKey{Model: ClaudeDesktopCompactionModel, LogicalModel: ClaudeDesktopCompactionModel, Role: claudeprofile.RoleCompaction, ThinkingDisplay: "omitted"}); err != nil {
		return empty, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var applications []*claudeprompt.SDKCompactionApplication
	outcome, errRun := claudeprompt.RunSDKReactiveCompaction(ctx, params.View.History(), params.InitialTokenGap,
		func(ctx context.Context, attempt claudeprompt.SDKReactiveAttempt) (claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft], error) {
			var result claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]
			if params.ObserveAttempt != nil {
				params.ObserveAttempt(attempt)
			}
			rows, errResolve := params.View.Resolve(attempt.Summarize)
			if errResolve != nil {
				return result, errResolve
			}
			if attempt.StrippedMedia {
				rows, errResolve = claudeprompt.StripSDKWireMedia(rows)
				if errResolve != nil {
					return result, errResolve
				}
			}
			rows, errResolve = appendClaudeDesktopSummaryInstruction(rows, instruction)
			if errResolve != nil {
				return result, errResolve
			}
			body, errMarshal := marshalClaudeDesktopSummaryJSON(struct {
				Model    string            `json:"model"`
				Messages []json.RawMessage `json:"messages"`
				Tools    json.RawMessage   `json:"tools,omitempty"`
			}{Model: ClaudeDesktopCompactionModel, Messages: rows, Tools: parent.Tools})
			if errMarshal != nil {
				return result, errMarshal
			}
			child, cancel := context.WithCancel(cliproxyexecutor.WithIndependentUpstreamAttempt(ctx, time.Now()))
			defer cancel()
			child = cliproxyexecutor.WithClaudeDesktopParentPromptID(child, params.View.ParentPromptID())
			child = cliproxyexecutor.WithClaudeDesktopSessionBinding(child, cliproxyexecutor.ClaudeDesktopSessionBinding{
				AccountID: params.Auth.ID, ProfileID: params.Bundle.ProfileID, Egress: params.Auth.ProxyURL, SessionID: params.SessionID})
			clientID := uuid.NewString()
			retained := false
			defer func() {
				if !retained {
					params.View.DiscardHelper(clientID)
				}
			}()
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude,
				Metadata: map[string]any{"claude_desktop_prompt_id": uuid.NewString(), "claude_desktop_client_request_id": clientID}}
			// Main request headers/system/thinking/diagnostics are not copied.
			// The existing compact planner owns their model-specific rendering.
			stream, errQuery := executor.ExecuteStream(child, params.Auth, cliproxyexecutor.Request{Model: ClaudeDesktopCompactionModel, Payload: body}, options)
			if errQuery != nil {
				return classifyClaudeDesktopSummaryError(ctx, errQuery), nil
			}
			if stream == nil || stream.Chunks == nil {
				return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: "error"}, nil
			}
			var summary claudeprompt.SDKCompactionResponse
			var observed claudeprompt.Response
			var forkUsage claudeprompt.SDKForkUsage
			defer summary.Discard()
			for {
				select {
				case <-ctx.Done():
					return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: "aborted"}, nil
				case chunk, open := <-stream.Chunks:
					if !open {
						if ctx.Err() != nil {
							return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: "aborted"}, nil
						}
						if !params.View.Current() {
							return result, claudeprompt.ErrSDKCompactionViewStale
						}
						text, known := summary.TakeText(params.Bundle.DesktopVersion, params.Bundle.CodeVersion)
						if !known {
							return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: "error"}, nil
						}
						wrapped, errWrap := text.Wrap(claudeprompt.SDKCompactionWrapOptions{SuppressFollowUpQuestions: true})
						if errWrap != nil {
							return result, errWrap
						}
						message, errMessage := marshalClaudeDesktopSummaryJSON(struct {
							Role    string `json:"role"`
							Content string `json:"content"`
						}{"user", wrapped})
						if errMessage != nil {
							return result, errMessage
						}
						history, _ := observed.SDKHistoryMessages()
						usage, usageKnown := forkUsage.Snapshot()
						application, errApplication := params.View.PrepareApplication(text,
							claudeprompt.SDKCompactionWrapOptions{SuppressFollowUpQuestions: true}, attempt.Preserve, clientID)
						if errApplication != nil {
							return result, errApplication
						}
						applications = append(applications, application)
						retained = true
						return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Success: true,
							Payload: ClaudeDesktopSummaryDraft{Application: application, Text: text, Message: message, ClientRequestID: clientID,
								AssistantMessages: len(history), Usage: usage, UsageKnown: usageKnown}}, nil
					}
					if chunk.Err != nil {
						return classifyClaudeDesktopSummaryError(ctx, chunk.Err), nil
					}
					// A bounded observer must not retain arbitrarily large events.
					if len(chunk.Payload) > 4*1024*1024 {
						return result, errors.New("Desktop compaction event exceeds observer limit")
					}
					for _, line := range bytes.Split(chunk.Payload, []byte{'\n'}) {
						summary.ObserveStreamLine(line)
						observed.ObserveStreamLine(line)
						forkUsage.ObserveStreamLine(line)
					}
				}
			}
		})
	for _, application := range applications {
		if errRun != nil || !outcome.ReadyToApply || outcome.Payload.Application != application {
			application.Discard()
		}
	}
	return outcome, errRun
}

func classifyClaudeDesktopSummaryError(ctx context.Context, err error) claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft] {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: "aborted"}
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() == 0 {
		return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: "error"}
	}
	failure := claudeprompt.ClassifySDKReactiveFailure(err.Error())
	return claudeprompt.SDKReactiveQueryResult[ClaudeDesktopSummaryDraft]{Reason: failure.Reason, TokenGap: failure.TokenGap}
}

func marshalClaudeDesktopSummaryJSON(value any) ([]byte, error) {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(data.Bytes(), []byte{'\n'}), nil
}

func appendClaudeDesktopSummaryInstruction(rows []json.RawMessage, instruction string) ([]json.RawMessage, error) {
	// Use the API's lower-case keys and the reviewed roo/AQs content merge.
	block, err := marshalClaudeDesktopSummaryJSON(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"text", instruction})
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		var last struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rows[len(rows)-1], &last) != nil {
			return nil, claudeprompt.ErrSDKCompactionContentUnknown
		}
		if last.Role == "user" {
			right := append(append(json.RawMessage{'['}, block...), ']')
			content, errContent := claudeprompt.MergeSDKWireUserContent(last.Content, right)
			if errContent != nil {
				return nil, errContent
			}
			updated, errSet := sjson.SetRawBytes(rows[len(rows)-1], "content", content)
			if errSet != nil {
				return nil, errSet
			}
			rows[len(rows)-1] = updated
			return rows, nil
		}
	}
	message, err := marshalClaudeDesktopSummaryJSON(struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}{"user", []json.RawMessage{block}})
	return append(rows, message), err
}
