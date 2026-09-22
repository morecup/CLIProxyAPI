package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type claudeDesktopCalibrationRequest struct {
	body []byte
}

func (e *ClaudeExecutor) runClaudeDesktopCountTokensCalibration(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	role claudeprofile.RequestRole,
	sessionID string,
	mainBody []byte,
) {
	if e == nil || !e.desktopOnly || (role != claudeprofile.RoleMain && role != claudeprofile.RoleCompaction) {
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(mainBody, "model").String())
	if model == "" || strings.TrimSpace(sessionID) == "" || auth == nil {
		return
	}
	if !e.beginClaudeDesktopCalibration(auth.ID, sessionID, model) {
		return
	}
	requests, errBuild := e.buildClaudeDesktopCalibrationRequests(model, mainBody)
	if errBuild != nil {
		helpersLogClaudeDesktopCalibrationFailure(ctx, model, len(requests), errBuild)
		return
	}
	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	plan, errPlan := e.planClaudeDesktopRequestWithHints(requests[0].body, claudeprofile.RoleCountTokens, model, nil)
	if errPlan != nil {
		helpersLogClaudeDesktopCalibrationFailure(ctx, model, 0, errPlan)
		return
	}
	client, errClient := e.newClaudeUpstreamHTTPClient(ctx, auth, plan)
	if errClient != nil {
		helpersLogClaudeDesktopCalibrationFailure(ctx, model, 0, errClient)
		return
	}

	var wait sync.WaitGroup
	var failuresMu sync.Mutex
	var firstFailure error
	failures := 0
	for _, calibrationRequest := range requests {
		calibrationRequest := calibrationRequest
		wait.Add(1)
		go func() {
			defer wait.Done()
			if errSend := e.sendClaudeDesktopCalibrationRequest(ctx, client, auth, apiKey, sessionID, url, model, calibrationRequest.body); errSend != nil {
				failuresMu.Lock()
				failures++
				if firstFailure == nil {
					firstFailure = errSend
				}
				failuresMu.Unlock()
			}
		}()
	}
	wait.Wait()
	if firstFailure != nil {
		helpersLogClaudeDesktopCalibrationFailure(ctx, model, failures, firstFailure)
	}
}

func (e *ClaudeExecutor) beginClaudeDesktopCalibration(authID, sessionID, model string) bool {
	key := strings.TrimSpace(authID) + "\x00" + strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(model)
	if key == "\x00\x00" {
		return false
	}
	e.desktopCalibrationMu.Lock()
	defer e.desktopCalibrationMu.Unlock()
	if e.desktopCalibrated == nil {
		e.desktopCalibrated = make(map[string]struct{})
	}
	if _, exists := e.desktopCalibrated[key]; exists {
		return false
	}
	e.desktopCalibrated[key] = struct{}{}
	return true
}

func (e *ClaudeExecutor) buildClaudeDesktopCalibrationRequests(model string, mainBody []byte) ([]claudeDesktopCalibrationRequest, error) {
	if e == nil || e.desktopProfile == nil {
		return nil, fmt.Errorf("claude desktop calibration profile is unavailable")
	}
	tools, errTools := e.desktopProfile.CountTokensCalibrationTools(model)
	if errTools != nil {
		return nil, errTools
	}
	sections, errSections := claudeDesktopCalibrationSystemSections(model, mainBody)
	if errSections != nil {
		return nil, errSections
	}
	requests := make([]claudeDesktopCalibrationRequest, 0, len(sections)+2+len(tools.SingleTools))
	for _, section := range sections {
		body, errBody := marshalClaudeDesktopCalibrationBody(model, section, nil)
		if errBody != nil {
			return nil, errBody
		}
		requests = append(requests, claudeDesktopCalibrationRequest{body: body})
	}
	for _, toolSet := range [][]json.RawMessage{tools.BuiltinTools, tools.MCPTools} {
		body, errBody := marshalClaudeDesktopCalibrationBody(model, "foo", toolSet)
		if errBody != nil {
			return nil, errBody
		}
		requests = append(requests, claudeDesktopCalibrationRequest{body: body})
	}
	for _, tool := range tools.SingleTools {
		body, errBody := marshalClaudeDesktopCalibrationBody(model, "foo", []json.RawMessage{tool})
		if errBody != nil {
			return nil, errBody
		}
		requests = append(requests, claudeDesktopCalibrationRequest{body: body})
	}
	want := map[string]int{
		"claude-opus-4-6": 46, "claude-opus-4-7": 46, "claude-opus-4-8": 38,
		"claude-opus-5": 41, "claude-sonnet-4-6": 46, "claude-sonnet-5": 42,
		"claude-haiku-4-5-20251001": 46,
	}[model]
	if want == 0 || len(requests) != want {
		return nil, fmt.Errorf("claude desktop calibration request count for %q is %d, want %d", model, len(requests), want)
	}
	return requests, nil
}

func claudeDesktopCalibrationSystemSections(model string, mainBody []byte) ([]string, error) {
	system := gjson.GetBytes(mainBody, "system")
	if !system.IsArray() || len(system.Array()) != 4 {
		return nil, fmt.Errorf("claude desktop calibration requires the four-block main system for %q", model)
	}
	introMarkers, sessionMarkers, errMarkers := claudeDesktopCalibrationSystemMarkers(model)
	if errMarkers != nil {
		return nil, errMarkers
	}
	introSections, errIntro := splitClaudeDesktopCalibrationSystemBlock(system.Array()[2].Get("text").String(), introMarkers)
	if errIntro != nil {
		return nil, fmt.Errorf("claude desktop calibration intro for %q: %w", model, errIntro)
	}
	sessionSections, errSession := splitClaudeDesktopCalibrationSystemBlock(system.Array()[3].Get("text").String(), sessionMarkers)
	if errSession != nil {
		return nil, fmt.Errorf("claude desktop calibration session for %q: %w", model, errSession)
	}
	sections := append(introSections, sessionSections...)
	want := map[string]int{
		"claude-opus-4-6": 16, "claude-opus-4-7": 16, "claude-opus-4-8": 12,
		"claude-opus-5": 15, "claude-sonnet-4-6": 16, "claude-sonnet-5": 16,
		"claude-haiku-4-5-20251001": 16,
	}[model]
	if want == 0 || len(sections) != want {
		return nil, fmt.Errorf("claude desktop calibration system section count for %q is %d, want %d", model, len(sections), want)
	}
	return sections, nil
}

func claudeDesktopCalibrationSystemMarkers(model string) ([]string, []string, error) {
	historicalIntro := []string{
		"\nYou are an interactive agent that helps users with software engineering tasks.",
		"# System",
		"# Doing tasks",
		"# Executing actions with care",
		"# Using your tools",
		"# Tone and style",
	}
	opusIntro := []string{
		"\nYou are an interactive agent that helps users with software engineering tasks.",
	}
	historicalSession := []string{
		"# Text output (does not apply to tool calls)",
		"When you use a pronoun for someone",
		"# Session-specific guidance",
		"# auto memory",
		"# Environment",
		"# Scratchpad Directory",
		"# Context management",
		"When you have enough information to act, act.",
		"<total_tokens>",
		"\n\nWhen referencing files in your responses",
	}
	opusSession := []string{
		"Write code that reads like the surrounding code:",
		"When you use a pronoun for someone",
		"For actions that are hard to reverse or outward-facing",
		"# Session-specific guidance",
		"# Memory",
		"# Environment",
		"# Scratchpad Directory",
		"# Context management",
		"When you have enough information to act, act.",
	}
	switch strings.TrimSpace(model) {
	case "claude-opus-5":
		return opusIntro, append(opusSession,
			"# Delivering work",
			"# Corrections",
			"Do not call the AgentTool unless the user requested it",
			"<total_tokens>",
			"\n\nWhen referencing files in your responses",
		), nil
	case "claude-opus-4-8":
		return opusIntro, append(opusSession,
			"<total_tokens>",
			"\n\nWhen referencing files in your responses",
		), nil
	case "claude-opus-4-6", "claude-opus-4-7", "claude-sonnet-4-6", "claude-sonnet-5", "claude-haiku-4-5-20251001":
		return historicalIntro, historicalSession, nil
	default:
		return nil, nil, fmt.Errorf("claude desktop calibration has no observed system layout for model %q", model)
	}
}

func splitClaudeDesktopCalibrationSystemBlock(text string, markers []string) ([]string, error) {
	if text == "" {
		return nil, fmt.Errorf("system block is empty")
	}
	if len(markers) == 0 || !strings.HasPrefix(text, markers[0]) {
		return nil, fmt.Errorf("system block does not start with the captured first section")
	}
	sections := make([]string, 0, len(markers))
	sectionStart := 0
	searchStart := len(markers[0])
	for _, marker := range markers[1:] {
		separatorAndMarker := "\n\n" + marker
		relative := strings.Index(text[searchStart:], separatorAndMarker)
		if relative < 0 {
			return nil, fmt.Errorf("system block is missing captured section marker %q", marker)
		}
		separatorStart := searchStart + relative
		if separatorStart <= sectionStart {
			return nil, fmt.Errorf("captured section marker %q is out of order", marker)
		}
		sections = append(sections, text[sectionStart:separatorStart])
		sectionStart = separatorStart + 2
		searchStart = sectionStart + len(marker)
	}
	sections = append(sections, text[sectionStart:])
	return sections, nil
}

func marshalClaudeDesktopCalibrationBody(model, content string, tools []json.RawMessage) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	writeJSON := func(value any) error {
		before := encoded.Len()
		if errEncode := encoder.Encode(value); errEncode != nil {
			return errEncode
		}
		encoded.Truncate(encoded.Len() - 1)
		if encoded.Len() == before {
			return fmt.Errorf("empty JSON encoding")
		}
		return nil
	}
	encoded.WriteString(`{"model":`)
	if errModel := writeJSON(model); errModel != nil {
		return nil, fmt.Errorf("encode Claude Desktop calibration model: %w", errModel)
	}
	encoded.WriteString(`,"messages":[{"role":"user","content":`)
	if errContent := writeJSON(content); errContent != nil {
		return nil, fmt.Errorf("encode Claude Desktop calibration content: %w", errContent)
	}
	encoded.WriteString(`}],"tools":[`)
	for index, tool := range tools {
		if !json.Valid(tool) {
			return nil, fmt.Errorf("Claude Desktop calibration tool %d is invalid JSON", index)
		}
		if index > 0 {
			encoded.WriteByte(',')
		}
		encoded.Write(tool)
	}
	encoded.WriteString(`]}`)
	return encoded.Bytes(), nil
}

func (e *ClaudeExecutor) sendClaudeDesktopCalibrationRequest(
	ctx context.Context,
	client *http.Client,
	auth *cliproxyauth.Auth,
	apiKey, sessionID, url, model string,
	body []byte,
) error {
	plan, errPlan := e.planClaudeDesktopRequestWithHints(body, claudeprofile.RoleCountTokens, model, nil)
	if errPlan != nil {
		return errPlan
	}
	plan.ClientRequestID = uuid.NewString()
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errRequest != nil {
		return errRequest
	}
	if errHeaders := e.applyClaudeHeadersWithProfile(request, auth, apiKey, false, nil, body, plan, nil, sessionID); errHeaders != nil {
		return errHeaders
	}
	response, errSend := e.doClaudeUpstreamRequest(client, request)
	if errSend != nil {
		return errSend
	}
	if response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		if errClose := response.Body.Close(); errClose != nil {
			return errClose
		}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("count_tokens calibration returned HTTP %d", response.StatusCode)
	}
	return nil
}

func helpersLogClaudeDesktopCalibrationFailure(ctx context.Context, model string, failures int, err error) {
	fields := log.Fields{"model": model}
	if failures > 0 {
		fields["failed_requests"] = failures
	}
	helps.LogWithRequestID(ctx).WithFields(fields).WithError(err).Warn("claude desktop: count_tokens calibration burst did not complete")
}
