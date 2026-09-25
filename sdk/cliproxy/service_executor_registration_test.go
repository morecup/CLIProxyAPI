package cliproxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type serviceTestPluginExecutor struct{}
type serviceTestSDKExecutor struct{ serviceTestPluginExecutor }

func (serviceTestSDKExecutor) Identifier() string { return "sdk-provider" }

func (serviceTestPluginExecutor) Identifier() string {
	return "plugin-provider"
}

func (serviceTestPluginExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serviceTestPluginExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (serviceTestPluginExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (serviceTestPluginExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serviceTestPluginExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRegisterAvailableExecutors(t *testing.T) {
	oldRegisterPluginExecutors := registerPluginExecutors
	pluginRegisterCalls := 0
	var expectedPluginHost *pluginhost.Host
	var expectedManager *coreauth.Manager
	registerPluginExecutors = func(host *pluginhost.Host, manager *coreauth.Manager) {
		pluginRegisterCalls++
		if host != expectedPluginHost {
			t.Fatalf("plugin executor registration host = %p, want %p", host, expectedPluginHost)
		}
		if manager != expectedManager {
			t.Fatalf("plugin executor registration manager = %p, want %p", manager, expectedManager)
		}
		manager.RegisterExecutor(serviceTestPluginExecutor{})
	}
	t.Cleanup(func() {
		registerPluginExecutors = oldRegisterPluginExecutors
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  pluginhost.New(),
	}
	expectedPluginHost = service.pluginHost
	expectedManager = service.coreManager

	service.registerAvailableExecutors(nil, executorRegistrationOptions{
		includeBaseline: true,
		includePlugins:  true,
	})

	if pluginRegisterCalls != 1 {
		t.Fatalf("plugin executor registration calls = %d, want 1", pluginRegisterCalls)
	}

	providers := []string{
		"claude",
		"anthropic-compatible",
		"plugin-provider",
	}
	for _, provider := range providers {
		resolved, ok := service.coreManager.Executor(provider)
		if !ok || resolved == nil {
			t.Fatalf("expected executor for provider %s after registration", provider)
		}
	}

	resolved, _ := service.coreManager.Executor("plugin-provider")
	if _, isPlugin := resolved.(serviceTestPluginExecutor); !isPlugin {
		t.Fatalf("executor type = %T, want serviceTestPluginExecutor", resolved)
	}
}

func TestClaudeExecutorRegistrationReusesRouterForConfigSnapshot(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	firstConfig := &config.Config{}
	firstConfig.ClaudeDesktop.StatePath = t.TempDir()
	service := &Service{cfg: firstConfig, coreManager: manager}
	authA := &coreauth.Auth{ID: "claude-a", Provider: "claude"}
	authB := &coreauth.Auth{ID: "claude-b", Provider: "claude"}

	service.registerExecutorForAuth(authA, true)
	firstExecutor, okFirst := manager.Executor("claude")
	firstRouter, isFirstRouter := firstExecutor.(*runtimeexecutor.ClaudeAccountExecutor)
	if !okFirst || !isFirstRouter {
		t.Fatalf("first Claude executor = %T, want *executor.ClaudeAccountExecutor", firstExecutor)
	}
	service.registerExecutorForAuth(authB, true)
	secondExecutor, _ := manager.Executor("claude")
	if secondExecutor != firstRouter {
		t.Fatal("force registration replaced the Claude router within one config snapshot")
	}

	secondConfig := firstConfig.CloneForRuntime()
	secondConfig.ClaudeDesktop.StatePath = t.TempDir()
	service.cfgMu.Lock()
	service.cfg = secondConfig
	service.cfgMu.Unlock()
	service.registerExecutorForAuth(authA, true)
	reloadedExecutor, _ := manager.Executor("claude")
	reloadedRouter, isReloadedRouter := reloadedExecutor.(*runtimeexecutor.ClaudeAccountExecutor)
	if !isReloadedRouter || reloadedRouter == firstRouter || !reloadedRouter.UsesConfig(secondConfig) {
		t.Fatalf("reloaded Claude executor = %T (%p), want one router for new config", reloadedExecutor, reloadedRouter)
	}
	service.registerExecutorForAuth(authB, true)
	afterSecondAuth, _ := manager.Executor("claude")
	if afterSecondAuth != reloadedRouter {
		t.Fatal("second auth created another Claude router during config reload")
	}
	t.Cleanup(reloadedRouter.Close)
}

func TestSyncPluginModelRuntimePreservesSDKExecutorUnlessForced(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	custom := serviceTestSDKExecutor{}
	manager.RegisterExecutor(custom)
	auth := &coreauth.Auth{ID: "private-auth", Provider: custom.Identifier()}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}

	service.syncPluginModelRuntime(context.Background())
	got, ok := manager.Executor(custom.Identifier())
	if !ok || got != custom {
		t.Fatalf("plugin model sync replaced SDK executor with %T", got)
	}

	service.registerExecutorForAuth(auth, true)
	got, ok = manager.Executor(custom.Identifier())
	if !ok || got != custom {
		t.Fatal("forced registration removed the SDK executor for an unknown provider")
	}
}
