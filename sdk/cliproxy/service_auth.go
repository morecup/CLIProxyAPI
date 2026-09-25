package cliproxy

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

// newDefaultAuthManager creates a default authentication manager with supported OAuth providers.
func newDefaultAuthManager() *sdkAuth.Manager {
	return sdkAuth.NewManager(
		sdkAuth.GetTokenStore(),
		sdkAuth.NewClaudeAuthenticator(),
	)
}

func (s *Service) ensureAuthUpdateQueue(ctx context.Context) {
	if s == nil {
		return
	}
	if s.authUpdates == nil {
		s.authUpdates = make(chan watcher.AuthUpdate, 256)
	}
	if s.authQueueStop != nil {
		return
	}
	queueCtx, cancel := context.WithCancel(ctx)
	s.authQueueStop = cancel
	go s.consumeAuthUpdates(queueCtx)
}

func (s *Service) consumeAuthUpdates(ctx context.Context) {
	ctx = coreauth.WithSkipPersist(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-s.authUpdates:
			if !ok {
				return
			}
			updates := []watcher.AuthUpdate{update}
		labelDrain:
			for {
				select {
				case nextUpdate := <-s.authUpdates:
					updates = append(updates, nextUpdate)
				default:
					break labelDrain
				}
			}
			s.handleAuthUpdates(ctx, updates)
		}
	}
}

func (s *Service) emitAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.watcher != nil && s.watcher.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
			log.Debugf("auth update queue saturated, applying inline action=%v id=%s", update.Action, update.ID)
		}
	}
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) handleAuthUpdate(ctx context.Context, update watcher.AuthUpdate) {
	s.handleAuthUpdates(ctx, []watcher.AuthUpdate{update})
}

func (s *Service) handleAuthUpdates(ctx context.Context, updates []watcher.AuthUpdate) {
	if s == nil {
		return
	}
	updates = coalesceAuthUpdates(updates)
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || s.coreManager == nil {
		return
	}

	registrationCtx := coreauth.WithDeferredAPIKeyModelAliasRebuild(ctx)
	tasks := make([]modelRegistrationTask, 0, len(updates))
	needsPluginSync := false
	needsAliasRebuild := false
	for _, update := range updates {
		switch update.Action {
		case watcher.AuthUpdateActionAdd, watcher.AuthUpdateActionModify:
			if update.Auth == nil || update.Auth.ID == "" {
				continue
			}
			auth := s.prepareCoreAuthForModelRegistration(registrationCtx, update.Auth)
			if auth == nil {
				continue
			}
			needsAliasRebuild = true
			authForRegistration := auth
			tasks = append(tasks, modelRegistrationTask{
				phase:    modelRegistrationPhase(authForRegistration),
				category: modelRegistrationCategory(authForRegistration),
				run: func() {
					s.completeModelRegistrationForAuth(registrationCtx, authForRegistration)
				},
			})
			needsPluginSync = true
		case watcher.AuthUpdateActionDelete:
			id := update.ID
			if id == "" && update.Auth != nil {
				id = update.Auth.ID
			}
			if id == "" {
				continue
			}
			s.applyCoreAuthRemoval(registrationCtx, id)
			needsAliasRebuild = true
		default:
			log.Debugf("received unknown auth update action: %v", update.Action)
		}
	}

	if needsAliasRebuild {
		s.coreManager.RefreshAPIKeyModelAlias()
	}
	s.runModelRegistrationTasks(registrationCtx, tasks)
	if needsPluginSync {
		s.syncPluginRuntime(registrationCtx)
	}
}

func coalesceAuthUpdates(updates []watcher.AuthUpdate) []watcher.AuthUpdate {
	if len(updates) <= 1 {
		return updates
	}
	order := make([]string, 0, len(updates))
	byID := make(map[string]watcher.AuthUpdate, len(updates))
	unkeyed := make([]watcher.AuthUpdate, 0)
	for _, update := range updates {
		id := authUpdateID(update)
		if id == "" {
			unkeyed = append(unkeyed, update)
			continue
		}
		if _, exists := byID[id]; !exists {
			order = append(order, id)
		}
		byID[id] = update
	}
	if len(byID) == 0 {
		return unkeyed
	}
	out := make([]watcher.AuthUpdate, 0, len(byID)+len(unkeyed))
	for _, id := range order {
		out = append(out, byID[id])
	}
	out = append(out, unkeyed...)
	return out
}

func authUpdateID(update watcher.AuthUpdate) string {
	if strings.TrimSpace(update.ID) != "" {
		return strings.TrimSpace(update.ID)
	}
	if update.Auth != nil {
		return strings.TrimSpace(update.Auth.ID)
	}
	return ""
}

func (s *Service) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) {
	auth = s.prepareCoreAuthForModelRegistration(ctx, auth)
	if auth == nil {
		return
	}
	s.completeModelRegistrationForAuth(ctx, auth)
	s.syncPluginRuntime(ctx)
}

func (s *Service) prepareCoreAuthForModelRegistration(ctx context.Context, auth *coreauth.Auth) *coreauth.Auth {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" {
		return nil
	}
	auth = auth.Clone()
	s.ensureExecutorsForAuthWithContext(ctx, auth, false)

	// IMPORTANT: Update coreManager FIRST, before model registration.
	// This ensures that configuration changes (proxy_url, prefix, etc.) take effect
	// immediately for API calls, rather than waiting for model registration to complete.
	op := "register"
	var err error
	if existing, ok := s.coreManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		if !existing.Disabled && existing.Status != coreauth.StatusDisabled && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			auth.LastRefreshedAt = existing.LastRefreshedAt
			auth.NextRefreshAfter = existing.NextRefreshAfter
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
		op = "update"
		_, err = s.coreManager.Update(ctx, auth)
	} else {
		_, err = s.coreManager.Register(ctx, auth)
	}
	if err != nil {
		log.Errorf("failed to %s auth %s: %v", op, auth.ID, err)
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok || current.Disabled {
			GlobalModelRegistry().UnregisterClient(auth.ID)
			return nil
		}
		auth = current
	}
	return auth
}

func (s *Service) completeModelRegistrationForAuth(ctx context.Context, auth *coreauth.Auth) {
	if s == nil || s.coreManager == nil || auth == nil || auth.ID == "" {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	s.registerModelsForAuth(ctx, auth)
	if ctx != nil && ctx.Err() != nil {
		return
	}
	s.coreManager.ReconcileRegistryModelStates(ctx, auth.ID)

	// Refresh the scheduler entry so that the auth's supportedModelSet is rebuilt
	// from the now-populated global model registry. Without this, newly added auths
	// have an empty supportedModelSet (because Register/Update upserts into the
	// scheduler before registerModelsForAuth runs) and are invisible to the scheduler.
	s.coreManager.RefreshSchedulerEntry(auth.ID)
}

func (s *Service) applyCoreAuthRemoval(ctx context.Context, id string) {
	if s == nil || id == "" {
		return
	}
	if s.coreManager == nil {
		return
	}
	id = strings.TrimSpace(id)
	GlobalModelRegistry().UnregisterClient(id)
	s.coreManager.Remove(ctx, id)
	s.syncPluginRuntime(ctx)
}

func (s *Service) applyRetryConfig(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	maxInterval := time.Duration(cfg.MaxRetryInterval) * time.Second
	s.coreManager.SetRetryConfig(cfg.RequestRetry, maxInterval, cfg.MaxRetryCredentials)
	coreauth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
}

func (s *Service) configureCooldownStateStore(cfg *config.Config) {
	_ = s.configureCooldownStateStoreContext(context.Background(), cfg, false)
}

func (s *Service) configureCooldownStateStoreContext(ctx context.Context, cfg *config.Config, persistOld bool) bool {
	if s == nil || s.coreManager == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	return s.coreManager.SwapCooldownStateStore(ctx, s.resolveCooldownStateStore(cfg), persistOld)
}

func (s *Service) resolveCooldownStateStore(cfg *config.Config) coreauth.CooldownStateStore {
	if cfg == nil || !cfg.SaveCooldownStatus || cfg.Home.Enabled {
		return nil
	}
	if s != nil && s.cooldownStateStore != nil {
		return s.cooldownStateStore
	}
	authDir, errResolve := resolveCooldownStateAuthDir(cfg)
	if errResolve != nil {
		log.Warnf("failed to resolve cooldown state directory: %v", errResolve)
		return nil
	}
	if authDir == "" {
		return nil
	}
	return coreauth.NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
}

func resolveCooldownStateAuthDir(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}
	authDir, errAuthDir := util.ResolveAuthDir(cfg.AuthDir)
	if errAuthDir != nil {
		return "", errAuthDir
	}
	return authDir, nil
}
