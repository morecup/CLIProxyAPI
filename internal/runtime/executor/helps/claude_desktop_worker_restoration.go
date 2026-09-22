package helps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tidwall/gjson"
)

// RestoreWorker consumes the model at the native headless caller boundary:
// worker registration has completed, but history loading and inference have
// not. Unknown models do not replace the owned default. Other metadata cannot
// become a system prompt, request header or an inferred permission grant.
func (a *ClaudeDesktopRemoteInput) RestoreWorker(ctx context.Context, external json.RawMessage, epoch int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := errors.Join(ctx.Err(), a.ctx.Err()); err != nil {
		return err
	}
	if a.started || (a.workerRestored && a.restoredWorkerEpoch != epoch) {
		return errors.New("Claude Desktop worker restoration is not owned by the paused input actor")
	}
	if a.workerRestored {
		return nil
	}
	value := gjson.GetBytes(external, "model")
	if value.Type == gjson.String {
		model := value.String()
		if strings.EqualFold(strings.TrimSpace(model), "default") {
			model = a.defaultModel
		}
		// The native caller accepts only a currently recognized model. Without
		// its owned model resolver, external state has no admission authority.
		if a.resolveModel != nil {
			resolved, err := a.resolveModel(model)
			if err == nil && strings.TrimSpace(resolved) != "" && resolved != a.model {
				if a.persist == nil {
					return errors.New("Claude Desktop worker model persistence is unavailable")
				}
				if err := a.persist(ctx, resolved, a.system); err != nil {
					return err
				}
				a.model = resolved
			}
		}
	}
	a.restoredWorkerEpoch = epoch
	a.workerRestored = true
	return nil
}
