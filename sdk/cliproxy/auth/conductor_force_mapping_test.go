package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type forceMappingExecutor struct {
	id string

	mu            sync.Mutex
	executeModels []string
	streamModels  []string
}

func (e *forceMappingExecutor) Identifier() string { return e.id }

func (e *forceMappingExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeModels = append(e.executeModels, req.Model)
	e.mu.Unlock()
	payload := `{"model":"` + req.Model + `","message":{"model":"` + req.Model + `"}}`
	return cliproxyexecutor.Response{Payload: []byte(payload)}, nil
}

func (e *forceMappingExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamModels = append(e.streamModels, req.Model)
	e.mu.Unlock()
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created","response":{"model":"` + req.Model + `"}}` + "\n\n")}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *forceMappingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *forceMappingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented"}
}

func (e *forceMappingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

func (e *forceMappingExecutor) ExecuteModels() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeModels))
	copy(out, e.executeModels)
	return out
}

func (e *forceMappingExecutor) StreamModels() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamModels))
	copy(out, e.streamModels)
	return out
}

func forceMappingPayloadLeaksUpstream(payload, upstreamModel string) bool {
	if upstreamModel == "" {
		return false
	}
	return strings.Contains(payload, `"model":"`+upstreamModel+`"`) ||
		strings.Contains(payload, `"model": "`+upstreamModel+`"`) ||
		strings.Contains(payload, `"modelVersion":"`+upstreamModel+`"`)
}

func setupForceMappingManager(t *testing.T, provider, upstreamModel, aliasModel string) (*Manager, *forceMappingExecutor) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	executor := &forceMappingExecutor{id: provider}
	manager.RegisterExecutor(executor)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		provider: {{
			Name:         upstreamModel,
			Alias:        aliasModel,
			Fork:         true,
			ForceMapping: true,
		}},
	})

	auth := &Auth{
		ID:       provider + "-force-mapping-auth",
		Provider: provider,
		Status:   StatusActive,
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: aliasModel}, {ID: upstreamModel}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	return manager, executor
}

func TestManagerExecute_OAuthAliasForceMappingRewritesNonStreamResponse(t *testing.T) {
	const (
		provider      = "claude"
		upstreamModel = "glm-5.2"
		aliasModel    = "claude-sonnet-latest"
	)

	manager, executor := setupForceMappingManager(t, provider, upstreamModel, aliasModel)
	resp, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want success", errExecute)
	}

	gotModels := executor.ExecuteModels()
	if len(gotModels) != 1 || gotModels[0] != upstreamModel {
		t.Fatalf("execute models = %v, want [%s]", gotModels, upstreamModel)
	}
	if got := string(resp.Payload); !strings.Contains(got, aliasModel) || forceMappingPayloadLeaksUpstream(got, upstreamModel) {
		t.Fatalf("response payload = %s, want alias %q without upstream %q", got, aliasModel, upstreamModel)
	}
}

func TestManagerExecuteStream_OAuthAliasForceMappingRewritesStreamResponse(t *testing.T) {
	const (
		provider      = "claude"
		upstreamModel = "glm-5.2"
		aliasModel    = "claude-sonnet-latest"
	)

	manager, executor := setupForceMappingManager(t, provider, upstreamModel, aliasModel)
	streamResult, errExecute := manager.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute stream error = %v, want success", errExecute)
	}

	gotModels := executor.StreamModels()
	if len(gotModels) != 1 || gotModels[0] != upstreamModel {
		t.Fatalf("stream models = %v, want [%s]", gotModels, upstreamModel)
	}

	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if got := string(payload); !strings.Contains(got, aliasModel) || forceMappingPayloadLeaksUpstream(got, upstreamModel) {
		t.Fatalf("stream payload = %s, want alias %q without upstream %q", got, aliasModel, upstreamModel)
	}
}

func setupAPIKeyForceMappingManager(t *testing.T, upstreamModel, aliasModel string) (*Manager, *forceMappingExecutor) {
	t.Helper()
	const provider = "claude"
	manager := NewManager(nil, nil, nil)
	executor := &forceMappingExecutor{id: provider}
	manager.RegisterExecutor(executor)

	apiKey := provider + "-key"
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: apiKey,
			Models: []internalconfig.ClaudeModel{{
				Name:         upstreamModel,
				Alias:        aliasModel,
				ForceMapping: true,
			}},
		}},
	}
	manager.SetConfig(cfg)

	auth := &Auth{
		ID:         provider + "-api-key-force-mapping-auth",
		Provider:   provider,
		Attributes: map[string]string{"api_key": apiKey},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: aliasModel}, {ID: upstreamModel}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	return manager, executor
}

func TestManagerExecute_APIKeyAliasForceMappingRewritesResponse(t *testing.T) {
	const (
		upstreamModel = "glm-5.2"
		aliasModel    = "claude-sonnet-latest"
	)

	manager, executor := setupAPIKeyForceMappingManager(t, upstreamModel, aliasModel)
	resp, errExecute := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want success", errExecute)
	}

	gotModels := executor.ExecuteModels()
	if len(gotModels) != 1 || gotModels[0] != upstreamModel {
		t.Fatalf("execute models = %v, want [%s]", gotModels, upstreamModel)
	}
	if got := string(resp.Payload); !strings.Contains(got, aliasModel) || forceMappingPayloadLeaksUpstream(got, upstreamModel) {
		t.Fatalf("response payload = %s, want alias %q without upstream %q", got, aliasModel, upstreamModel)
	}
}

func TestManagerExecuteStream_APIKeyAliasForceMappingRewritesResponse(t *testing.T) {
	const (
		upstreamModel = "glm-5.2"
		aliasModel    = "claude-sonnet-latest"
	)

	manager, executor := setupAPIKeyForceMappingManager(t, upstreamModel, aliasModel)
	streamResult, errExecute := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute stream error = %v, want success", errExecute)
	}

	gotModels := executor.StreamModels()
	if len(gotModels) != 1 || gotModels[0] != upstreamModel {
		t.Fatalf("stream models = %v, want [%s]", gotModels, upstreamModel)
	}

	var payload []byte
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if got := string(payload); !strings.Contains(got, aliasModel) || forceMappingPayloadLeaksUpstream(got, upstreamModel) {
		t.Fatalf("stream payload = %s, want alias %q without upstream %q", got, aliasModel, upstreamModel)
	}
}
