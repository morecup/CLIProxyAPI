package cliproxy

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// pluginHostHasAuthProvider is overridable in tests to avoid loading real plugins.
var pluginHostHasAuthProvider = func(host *pluginhost.Host, provider string) bool {
	return host != nil && host.HasAuthProvider(provider)
}

func (s *Service) ensureExecutorsForAuth(a *coreauth.Auth) {
	s.ensureExecutorsForAuthWithContext(context.Background(), a, false)
}

func (s *Service) ensureExecutorsForAuthWithMode(a *coreauth.Auth, forceReplace bool) {
	s.ensureExecutorsForAuthWithContext(context.Background(), a, forceReplace)
}

func (s *Service) ensureExecutorsForAuthWithContext(ctx context.Context, a *coreauth.Auth, forceReplace bool) {
	if a == nil || (ctx != nil && ctx.Err() != nil) {
		return
	}
	s.registerAvailableExecutors(ctx, executorRegistrationOptions{
		auths:             []*coreauth.Auth{a},
		forceReplaceAuths: forceReplace,
	})
}

func (s *Service) registerAvailableExecutors(ctx context.Context, opts executorRegistrationOptions) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.executorRegistrationMu.Lock()
	defer s.executorRegistrationMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	// Keep all Service-owned executor registration paths here so native, Home,
	// auth-derived, and plugin executors stay in the same binding order.
	if opts.includeBaseline {
		s.registerExecutorsForAuths(baselineExecutorAuths(), opts.forceReplaceAuths)
	}
	if len(opts.auths) > 0 {
		s.registerExecutorsForAuths(opts.auths, opts.forceReplaceAuths)
	}
	if opts.includePlugins && s.pluginHost != nil {
		registerPluginExecutors(s.pluginHost, s.coreManager)
	}
}

func baselineExecutorAuths() []*coreauth.Auth {
	providers := []string{
		"claude",
		constant.AnthropicCompatible,
	}
	auths := make([]*coreauth.Auth, 0, len(providers))
	for _, provider := range providers {
		auths = append(auths, &coreauth.Auth{
			ID:       provider,
			Provider: provider,
		})
	}
	return auths
}

func (s *Service) registerExecutorsForAuths(auths []*coreauth.Auth, forceReplace bool) {
	for _, auth := range auths {
		s.registerExecutorForAuth(auth, forceReplace)
	}
}

func (s *Service) registerExecutorForAuth(a *coreauth.Auth, forceReplace bool) {
	if s == nil || s.coreManager == nil || a == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	// Skip disabled auth entries when (re)binding executors.
	// Disabled auths can linger during config reloads and must not override active
	// provider executors.
	if a.Disabled {
		return
	}
	switch strings.ToLower(a.Provider) {
	case "claude":
		// A config reload forces replacement of the old provider router, but every
		// Claude auth in the same registration batch must converge on the one
		// router created for the new immutable config snapshot.
		if existing, okExisting := s.coreManager.Executor("claude"); okExisting {
			if accountExecutor, okAccount := existing.(*executor.ClaudeAccountExecutor); okAccount && accountExecutor.UsesConfig(cfg) {
				if len(a.Metadata) > 0 {
					if errProvision := accountExecutor.Provision(a); errProvision != nil {
						log.WithError(errProvision).WithField("auth_id", a.ID).Warn("claude desktop: account runtime could not be provisioned")
					}
				}
				return
			}
		}
		accountExecutor := executor.NewClaudeAccountExecutorWithOptions(cfg, executor.ClaudeAccountExecutorOptions{CredentialManager: s.coreManager})
		s.coreManager.RegisterExecutor(accountExecutor)
		if len(a.Metadata) > 0 {
			if errProvision := accountExecutor.Provision(a); errProvision != nil {
				log.WithError(errProvision).WithField("auth_id", a.ID).Warn("claude desktop: account runtime could not be provisioned")
			}
		}
	case constant.AnthropicCompatible:
		s.coreManager.RegisterExecutor(executor.NewAnthropicCompatibleExecutor(cfg))
	}
}

func (s *Service) registerResolvedModelsForAuth(a *coreauth.Auth, providerKey string, models []*ModelInfo) {
	if a == nil || a.ID == "" {
		return
	}
	providerKey = strings.ToLower(strings.TrimSpace(providerKey))
	if providerKey == "" {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	normalizedModels := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		clone := *model
		clone.ID = modelID
		normalizedModels = append(normalizedModels, &clone)
	}
	if len(normalizedModels) == 0 {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	GlobalModelRegistry().RegisterClient(a.ID, providerKey, normalizedModels)
}

func (s *Service) pluginModelsForProvider(providerKey string) []*ModelInfo {
	if s == nil || s.pluginHost == nil {
		return nil
	}
	return s.pluginHost.ModelsForProvider(providerKey)
}

func (s *Service) appendPluginModels(providerKey string, models []*ModelInfo) []*ModelInfo {
	pluginModels := s.pluginModelsForProvider(providerKey)
	if len(pluginModels) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models)+len(pluginModels))
	seen := make(map[string]struct{}, len(models)+len(pluginModels))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID != "" {
			seen[modelID] = struct{}{}
		}
		out = append(out, model)
	}
	for _, model := range pluginModels {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		out = append(out, model)
	}
	return out
}

func (s *Service) tryRegisterPluginModelsForAuth(ctx context.Context, a *coreauth.Auth, provider, authKind string, excluded []string) bool {
	if s == nil || s.pluginHost == nil || a == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	result := s.pluginHost.ModelsForAuth(ctx, a)
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if !result.Handled {
		return false
	}
	if result.Err != nil {
		return true
	}
	activeAuth := a
	providerKey := strings.ToLower(strings.TrimSpace(result.Provider))
	if providerKey == "" {
		providerKey = strings.ToLower(strings.TrimSpace(provider))
	}
	if result.Auth != nil && s.coreManager != nil {
		result.Auth.ID = a.ID
		if result.Auth.Provider == "" {
			result.Auth.Provider = a.Provider
		}
		if result.Auth.FileName == "" {
			result.Auth.FileName = a.FileName
		}
		if result.Auth.Attributes == nil {
			result.Auth.Attributes = make(map[string]string)
		}
		for key, value := range a.Attributes {
			if _, exists := result.Auth.Attributes[key]; !exists {
				result.Auth.Attributes[key] = value
			}
		}
		if updated, errUpdate := s.coreManager.Update(ctx, result.Auth); errUpdate == nil && updated != nil {
			activeAuth = updated.Clone()
		}
	}
	if activeAuth == nil {
		activeAuth = a
	}
	if activeProvider := strings.ToLower(strings.TrimSpace(activeAuth.Provider)); activeProvider != "" {
		providerKey = activeProvider
	}
	if providerKey == "" {
		providerKey = strings.ToLower(strings.TrimSpace(provider))
	}
	activeAuthKind := activeAuth.AuthKind()
	activeExcluded := s.oauthExcludedModels(providerKey, activeAuthKind)
	if a == activeAuth && len(activeExcluded) == 0 {
		activeExcluded = excluded
	}
	if activeAuth.Attributes != nil {
		if val, ok := activeAuth.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			activeExcluded = strings.Split(val, ",")
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	models := applyExcludedModels(result.Models, activeExcluded)
	models = applyOAuthModelAliasForAuth(s.cfg, providerKey, activeAuthKind, activeAuth.Attributes, models)
	if len(models) > 0 {
		s.registerResolvedModelsForAuth(activeAuth, providerKey, applyModelPrefixes(models, activeAuth.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
		return true
	}
	GlobalModelRegistry().UnregisterClient(activeAuth.ID)
	return true
}
