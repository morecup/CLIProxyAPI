package executor

import (
	"context"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestClaudeExecutorEnrollmentValidationIsRaceFreeOnSharedCredential(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := newEnrolledClaudeDesktopAuth("claude-desktop-race.json", "sk-ant-oat-race-probe")
	ctx := context.Background()

	var wait sync.WaitGroup
	for index := 0; index < 64; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			prepared, errPrepare := executor.PrepareRequestAuth(ctx, auth)
			if errPrepare != nil || prepared != auth {
				t.Errorf("PrepareRequestAuth() = %#v, %v", prepared, errPrepare)
			}
		}()
	}
	wait.Wait()
}
