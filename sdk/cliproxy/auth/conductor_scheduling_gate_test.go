package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type schedulingGateTestExecutor struct {
	provider string
	denied   map[string]bool

	mu     sync.Mutex
	synced []string
}

func (e *schedulingGateTestExecutor) Identifier() string { return e.provider }

func (*schedulingGateTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (*schedulingGateTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (*schedulingGateTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*schedulingGateTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (*schedulingGateTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *schedulingGateTestExecutor) CanScheduleAuth(auth *Auth) error {
	if auth == nil || e.denied[auth.ID] {
		return errors.New("runtime gate denied auth")
	}
	return nil
}

func (e *schedulingGateTestExecutor) SyncAuth(auth *Auth) {
	if auth == nil {
		return
	}
	e.mu.Lock()
	e.synced = append(e.synced, auth.ID)
	e.mu.Unlock()
}

func (e *schedulingGateTestExecutor) synchronizedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.synced...)
}

func TestManagerSchedulingGateExcludesAuthFromAllLocalSelectionPaths(t *testing.T) {
	ctx := context.Background()
	executor := &schedulingGateTestExecutor{
		provider: "runtime-gated-provider",
		denied:   map[string]bool{"blocked": true},
	}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(ctx, &Auth{ID: "blocked", Provider: executor.provider, Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}

	if manager.HasProviderAuth(executor.provider) {
		t.Fatal("HasProviderAuth included a runtime-gated credential")
	}
	if providers := manager.AvailableProviders(); len(providers) != 0 {
		t.Fatalf("AvailableProviders() = %v, want no gated provider", providers)
	}
	if selected, _, errPick := manager.pickNext(ctx, executor.provider, "", cliproxyexecutor.Options{}, nil); errPick == nil || selected != nil {
		t.Fatalf("pickNext(blocked only) = %#v, %v", selected, errPick)
	}
	if manager.retryAllowed(0, []string{executor.provider}, "", authSelectionEligibility{}, "", 1) {
		t.Fatal("retryAllowed accepted a runtime-gated credential")
	}

	if _, errRegister := manager.Register(ctx, &Auth{ID: "allowed", Provider: executor.provider, Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	selected, _, errPick := manager.pickNext(ctx, executor.provider, "", cliproxyexecutor.Options{}, nil)
	if errPick != nil || selected == nil || selected.ID != "allowed" {
		t.Fatalf("pickNext() = %#v, %v; want allowed", selected, errPick)
	}
	if !manager.HasProviderAuth(executor.provider) {
		t.Fatal("HasProviderAuth excluded the allowed runtime credential")
	}
	if !manager.retryAllowed(0, []string{executor.provider}, "", authSelectionEligibility{}, "", 1) {
		t.Fatal("retryAllowed excluded the allowed runtime credential")
	}
}

func TestManagerSynchronizesExistingRegisteredAndUpdatedAuthLifecycle(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(ctx, &Auth{ID: "existing", Provider: "lifecycle-provider", Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	executor := &schedulingGateTestExecutor{provider: "lifecycle-provider", denied: map[string]bool{}}
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(ctx, &Auth{ID: "new", Provider: executor.provider, Status: StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	updated, ok := manager.GetByID("new")
	if !ok {
		t.Fatal("registered auth is missing")
	}
	updated.StatusMessage = "updated"
	if _, errUpdate := manager.Update(ctx, updated); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	got := executor.synchronizedIDs()
	want := []string{"existing", "new", "new"}
	if len(got) != len(want) {
		t.Fatalf("SyncAuth calls = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("SyncAuth calls = %v, want %v", got, want)
		}
	}
}
