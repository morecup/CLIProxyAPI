package executor

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	claudestartup "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/startup"
	claudetasks "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/tasks"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	claudeDesktopMachineBindingVersion = 1
	claudeDesktopRuntimeBindingVersion = 1
)

// ClaudeAccountExecutor routes each enrolled account through an independent
// logical Desktop runtime while preserving the provider-level scheduler API.
type ClaudeAccountExecutor struct {
	cfg                          *config.Config
	stateRoot                    string
	bundle                       *claudeprofile.Bundle
	bundleRevision               string
	bundleVersion                string
	bundleErr                    error
	machinePool                  claudeDesktopMachinePool
	mu                           sync.Mutex
	runtimes                     map[string]*claudeAccountRuntime
	stopping                     map[string]*claudeAccountRuntime
	closed                       bool
	executionSessions            helps.ClaudeDesktopExecutionSessions
	startupDoerFactory           claudestartup.DoerFactory
	telemetryEndpointDoerFactory claudetelemetry.EndpointDoerFactory
	credentialManager            *cliproxyauth.Manager
}

type claudeAccountRuntime struct {
	executor          *ClaudeExecutor
	accountKey        string
	authID            string
	revision          string
	machineProfileID  string
	stateDirectory    string
	lifecyclePath     string
	appSessionID      string
	lifecycleGen      uint64
	recoveredUnclean  bool
	recordOwner       string
	remoteInputs      map[string]*helps.ClaudeDesktopRemoteInput
	watchDemandMu     sync.Mutex
	watchSuppressions map[string]time.Time
	watchIdleTimers   map[string]*desktopSessionIdleTimer
	watchAfter        func(time.Duration, func()) *time.Timer

	mu          sync.Mutex
	active      int
	retiring    bool
	quarantined bool
	closing     bool
	closed      bool
}

type claudeDesktopMachinePool struct {
	profiles map[string]claudeDesktopMachineProfile
	ids      []string
	explicit map[string]string
	bound    map[string]string
}

type claudeDesktopMachineProfile struct {
	ID         string
	Revision   string
	Host       claudetelemetry.HostSnapshot
	SDKProcess claudetelemetry.SDKProcessSnapshot
}

type claudeDesktopMachineBinding struct {
	Version          int    `json:"version"`
	AccountKey       string `json:"account_key"`
	MachineProfileID string `json:"machine_profile_id"`
}

type claudeDesktopRuntimeBinding struct {
	Version          int                           `json:"version"`
	AuthIDHash       string                        `json:"auth_id_hash"`
	State            claudedesktop.EnrollmentState `json:"state"`
	ApprovedRevision string                        `json:"approved_revision"`
	ObservedRevision string                        `json:"observed_revision,omitempty"`
	MachineProfileID string                        `json:"machine_profile_id"`
	DesktopProfile   string                        `json:"desktop_profile"`
	PreviousRevision string                        `json:"previous_revision,omitempty"`
	PreviousMachine  string                        `json:"previous_machine_profile_id,omitempty"`
	PreviousDesktop  string                        `json:"previous_desktop_profile,omitempty"`
	ObservedMachine  string                        `json:"observed_machine_profile_id,omitempty"`
	ObservedDesktop  string                        `json:"observed_desktop_profile,omitempty"`
	QuarantineReason string                        `json:"quarantine_reason,omitempty"`
	UpdatedAt        string                        `json:"updated_at"`
}

// ClaudeAccountRuntimeStatus is a secret-free account runtime snapshot used
// by management diagnostics and H7.2 isolation tests.
type ClaudeAccountRuntimeStatus struct {
	AuthIDHash       string                        `json:"auth_id_hash"`
	State            claudedesktop.EnrollmentState `json:"state"`
	ApprovedRevision string                        `json:"approved_revision,omitempty"`
	ObservedRevision string                        `json:"observed_revision,omitempty"`
	MachineProfileID string                        `json:"machine_profile_id,omitempty"`
	DesktopProfile   string                        `json:"desktop_profile,omitempty"`
	PreviousRevision string                        `json:"previous_revision,omitempty"`
	PreviousMachine  string                        `json:"previous_machine_profile_id,omitempty"`
	PreviousDesktop  string                        `json:"previous_desktop_profile,omitempty"`
	ObservedMachine  string                        `json:"observed_machine_profile_id,omitempty"`
	ObservedDesktop  string                        `json:"observed_desktop_profile,omitempty"`
	QuarantineReason string                        `json:"quarantine_reason,omitempty"`
	CanPromote       bool                          `json:"can_promote"`
	CanRollback      bool                          `json:"can_rollback"`
	RuntimeLoaded    bool                          `json:"runtime_loaded"`
	RuntimeStopping  bool                          `json:"runtime_stopping"`
	LifecycleState   string                        `json:"lifecycle_state,omitempty"`
	LifecycleGen     uint64                        `json:"lifecycle_generation,omitempty"`
	AppSessionHash   string                        `json:"app_session_hash,omitempty"`
	PreviousExit     string                        `json:"previous_exit,omitempty"`
	RecoveredUnclean bool                          `json:"recovered_unclean_exit,omitempty"`
	StartedAt        string                        `json:"started_at,omitempty"`
	StoppedAt        string                        `json:"stopped_at,omitempty"`
	Startup          claudestartup.Status          `json:"startup"`
	ControlPlane     claudecontrol.Status          `json:"control_plane"`
	AgentTasks       *claudetasks.Health           `json:"agent_tasks,omitempty"`
}

// NewClaudeAccountExecutor creates the provider router used by the service.
func NewClaudeAccountExecutor(cfg *config.Config) *ClaudeAccountExecutor {
	return NewClaudeAccountExecutorWithOptions(cfg, ClaudeAccountExecutorOptions{})
}

// ClaudeAccountExecutorOptions provides dependency injection for account-local
// background senders. Production callers normally use NewClaudeAccountExecutor.
type ClaudeAccountExecutorOptions struct {
	StartupDoerFactory           claudestartup.DoerFactory
	TelemetryEndpointDoerFactory claudetelemetry.EndpointDoerFactory
	CredentialManager            *cliproxyauth.Manager
}

func NewClaudeAccountExecutorWithOptions(cfg *config.Config, options ClaudeAccountExecutorOptions) *ClaudeAccountExecutor {
	stateRoot := ""
	if cfg != nil {
		stateRoot = claudetelemetry.StatePath(cfg.ClaudeDesktop.StatePath, cfg.AuthDir)
	}
	if absolute, errAbs := filepath.Abs(stateRoot); errAbs == nil {
		stateRoot = absolute
	}
	machinePool := newClaudeDesktopMachinePool(cfg)
	machinePool.restoreBindings(stateRoot)
	bundle, bundleRevision, bundleVersion, errBundle := claudeDesktopBundleIdentity(cfg)
	return &ClaudeAccountExecutor{
		cfg:                          cfg,
		stateRoot:                    stateRoot,
		bundle:                       bundle,
		bundleRevision:               bundleRevision,
		bundleVersion:                bundleVersion,
		bundleErr:                    errBundle,
		machinePool:                  machinePool,
		runtimes:                     make(map[string]*claudeAccountRuntime),
		stopping:                     make(map[string]*claudeAccountRuntime),
		startupDoerFactory:           options.StartupDoerFactory,
		telemetryEndpointDoerFactory: options.TelemetryEndpointDoerFactory,
		credentialManager:            options.CredentialManager,
	}
}

func (e *ClaudeAccountExecutor) Identifier() string { return "claude" }

func (e *ClaudeAccountExecutor) RequestToFormat(cliproxyexecutor.Request, cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatClaude
}

func (e *ClaudeAccountExecutor) ShouldPrepareRequestAuth(*cliproxyauth.Auth) bool { return false }

func (e *ClaudeAccountExecutor) PrepareRequestAuth(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return nil, errRuntime
	}
	runtimeRef.release()
	return auth, nil
}

// UsesConfig reports whether this router belongs to the current immutable
// service config snapshot.
func (e *ClaudeAccountExecutor) UsesConfig(cfg *config.Config) bool {
	return e != nil && e.cfg == cfg
}

// Provision eagerly starts an account runtime so its application uptime begins
// when the credential enters the scheduler rather than on its first request.
func (e *ClaudeAccountExecutor) Provision(auth *cliproxyauth.Auth) error {
	if auth == nil {
		return newClaudeDesktopEligibilityError("auth is required")
	}
	enrollment, errEnrollment := claudedesktop.ValidateEnrollmentBinding(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return newClaudeDesktopEligibilityError(fmt.Sprintf("credential is not enrolled by Claude Desktop login: %v", errEnrollment))
	}
	if enrollment.State != claudedesktop.EnrollmentActive {
		return nil
	}
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return errRuntime
	}
	runtimeRef.release()
	return nil
}

// SyncAuth mirrors persisted credential lifecycle changes into account-scoped
// runtime resources. It is invoked by the auth manager after register/update.
func (e *ClaudeAccountExecutor) SyncAuth(auth *cliproxyauth.Auth) {
	if e == nil || auth == nil {
		return
	}
	enrollment, errEnrollment := claudedesktop.ValidateEnrollmentBinding(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		_ = e.quarantineAuth(auth.ID, "credential enrollment binding is invalid")
		return
	}
	if auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		e.CloseAuth(auth.ID)
		return
	}
	switch enrollment.State {
	case claudedesktop.EnrollmentActive:
		if errProvision := e.Provision(auth); errProvision != nil {
			if errors.Is(errProvision, errClaudeDesktopRuntimeDraining) {
				log.WithField("auth_id", auth.ID).Debug("claude desktop: account runtime enable is waiting for the previous app lifetime to stop")
				return
			}
			_ = e.quarantineAuth(auth.ID, "active account runtime synchronization failed")
			log.WithError(errProvision).WithField("auth_id", auth.ID).Warn("claude desktop: active account runtime could not be synchronized")
		}
	case claudedesktop.EnrollmentQuarantined:
		_ = e.quarantineAuth(auth.ID, enrollment.QuarantineReason)
	case claudedesktop.EnrollmentDisabled, claudedesktop.EnrollmentRetired, claudedesktop.EnrollmentProvisioning, claudedesktop.EnrollmentReady:
		e.CloseAuth(auth.ID)
	}
}

// CanScheduleAuth enforces the separation between the credential registry and
// scheduler candidates. Only active, binding-stable Desktop accounts pass.
func (e *ClaudeAccountExecutor) CanScheduleAuth(auth *cliproxyauth.Auth) error {
	if e == nil {
		return newClaudeDesktopEligibilityError("account runtime router is unavailable")
	}
	enrollment, errEnrollment := e.validateActiveRuntimeAuth(auth)
	if errEnrollment != nil {
		return errEnrollment
	}
	accountKey := claudeDesktopAccountKey(auth.ID)
	accountDirectory := filepath.Join(e.stateRoot, "accounts", accountKey)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return newClaudeDesktopEligibilityError("account runtime router is closed")
	}
	if draining := e.stoppingRuntimeLocked(auth.ID); draining != nil {
		e.mu.Unlock()
		return newClaudeDesktopRuntimeDrainingError()
	}
	profile, revision, errRevision := e.desiredRuntimeLocked(enrollment, auth, accountDirectory)
	if errRevision != nil {
		e.mu.Unlock()
		return errRevision
	}
	current := e.runtimes[auth.ID]
	binding, found, errBinding := readClaudeDesktopRuntimeBinding(accountDirectory, accountKey)
	if errBinding != nil {
		e.mu.Unlock()
		return newClaudeDesktopEligibilityError("runtime binding is unreadable")
	}
	if found && (binding.State == claudedesktop.EnrollmentQuarantined || binding.ApprovedRevision != revision) {
		if binding.ApprovedRevision != revision {
			binding.State = claudedesktop.EnrollmentQuarantined
			binding.ObservedRevision = revision
			binding.QuarantineReason = "runtime binding drift requires explicit promotion"
			binding.ObservedMachine = profile.ID
			binding.ObservedDesktop = e.bundleVersion
			binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if errPersist := persistClaudeDesktopRuntimeBinding(accountDirectory, binding); errPersist != nil {
				delete(e.runtimes, auth.ID)
				if current != nil {
					e.stopping[auth.ID] = current
				}
				e.mu.Unlock()
				if current != nil {
					current.quarantine()
				}
				return newClaudeDesktopEligibilityError("runtime binding drift could not be persisted")
			}
		}
		if current != nil {
			delete(e.runtimes, auth.ID)
			e.stopping[auth.ID] = current
		}
		e.mu.Unlock()
		if current != nil {
			current.quarantine()
		}
		return newClaudeDesktopEligibilityError("account runtime is quarantined: " + binding.QuarantineReason)
	}
	e.mu.Unlock()
	return nil
}

// PromoteAuth explicitly accepts the current proxy/profile/material binding.
// Drift is never promoted implicitly by ordinary requests or config reloads.
func (e *ClaudeAccountExecutor) PromoteAuth(auth *cliproxyauth.Auth) error {
	return e.activateRuntimeBinding(auth, false)
}

// RollbackAuth activates the previous approved revision after the operator has
// restored the corresponding bundle, proxy, machine profile, and materials.
// A hash alone is never enough to reconstruct secret-bearing old settings.
func (e *ClaudeAccountExecutor) RollbackAuth(auth *cliproxyauth.Auth) error {
	return e.activateRuntimeBinding(auth, true)
}

func (e *ClaudeAccountExecutor) activateRuntimeBinding(auth *cliproxyauth.Auth, rollback bool) error {
	if e == nil {
		return newClaudeDesktopEligibilityError("account runtime router is unavailable")
	}
	enrollment, errEnrollment := e.validateActiveRuntimeAuth(auth)
	if errEnrollment != nil {
		return errEnrollment
	}
	accountKey := claudeDesktopAccountKey(auth.ID)
	accountDirectory := filepath.Join(e.stateRoot, "accounts", accountKey)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return newClaudeDesktopEligibilityError("account runtime router is closed")
	}
	if draining := e.stoppingRuntimeLocked(auth.ID); draining != nil {
		e.mu.Unlock()
		return newClaudeDesktopRuntimeDrainingError()
	}
	profile, revision, errRevision := e.desiredRuntimeLocked(enrollment, auth, accountDirectory)
	if errRevision != nil {
		e.mu.Unlock()
		return errRevision
	}
	previousBinding, found, errBinding := readClaudeDesktopRuntimeBinding(accountDirectory, accountKey)
	if errBinding != nil {
		e.mu.Unlock()
		return newClaudeDesktopEligibilityError("runtime binding is unreadable")
	}
	if !found {
		e.mu.Unlock()
		return newClaudeDesktopEligibilityError("runtime binding has not been provisioned")
	}
	operation := "promote"
	if rollback {
		operation = "rollback"
		if previousBinding.PreviousRevision == "" {
			e.mu.Unlock()
			return newClaudeDesktopEligibilityError("runtime binding has no previous revision to roll back")
		}
		if revision != previousBinding.PreviousRevision {
			e.mu.Unlock()
			return newClaudeDesktopEligibilityError("restore the previous runtime configuration before rollback")
		}
	} else {
		if previousBinding.ApprovedRevision == revision && previousBinding.State == claudedesktop.EnrollmentActive {
			e.mu.Unlock()
			return e.Provision(auth)
		}
		if previousBinding.ObservedRevision != "" && previousBinding.ObservedRevision != revision {
			e.mu.Unlock()
			return newClaudeDesktopEligibilityError("observed runtime revision changed; inspect status before promotion")
		}
	}
	if current := e.runtimes[auth.ID]; current != nil {
		previousBinding.State = claudedesktop.EnrollmentQuarantined
		previousBinding.ObservedRevision = revision
		previousBinding.ObservedMachine = profile.ID
		previousBinding.ObservedDesktop = e.bundleVersion
		previousBinding.QuarantineReason = "runtime binding migration is draining the previous application lifetime"
		previousBinding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if errPersist := persistClaudeDesktopRuntimeBinding(accountDirectory, previousBinding); errPersist != nil {
			e.mu.Unlock()
			return fmt.Errorf("claude desktop account runtime: prepare %s: %w", operation, errPersist)
		}
		delete(e.runtimes, auth.ID)
		e.stopping[auth.ID] = current
		e.mu.Unlock()
		current.quarantine()
		if current.isClosed() {
			return e.activateRuntimeBinding(auth, rollback)
		}
		return newClaudeDesktopRuntimeDrainingError()
	}
	binding := newClaudeDesktopRuntimeBinding(accountKey, revision, profile.ID, e.bundleVersion)
	binding.PreviousRevision = previousBinding.ApprovedRevision
	binding.PreviousMachine = previousBinding.MachineProfileID
	binding.PreviousDesktop = previousBinding.DesktopProfile
	if errPersist := persistClaudeDesktopRuntimeBinding(accountDirectory, binding); errPersist != nil {
		e.mu.Unlock()
		return fmt.Errorf("claude desktop account runtime: %s binding: %w", operation, errPersist)
	}
	e.mu.Unlock()
	if errProvision := e.Provision(auth); errProvision != nil {
		e.mu.Lock()
		currentBinding, currentFound, _ := readClaudeDesktopRuntimeBinding(accountDirectory, accountKey)
		if currentFound && currentBinding.ApprovedRevision == binding.ApprovedRevision {
			_ = persistClaudeDesktopRuntimeBinding(accountDirectory, previousBinding)
		}
		e.mu.Unlock()
		return fmt.Errorf("claude desktop account runtime: %s activation: %w", operation, errProvision)
	}
	return nil
}

// AccountStatus returns a secret-free status for one stable AuthID.
func (e *ClaudeAccountExecutor) AccountStatus(authID string) ClaudeAccountRuntimeStatus {
	authID = strings.TrimSpace(authID)
	if e == nil || authID == "" {
		return ClaudeAccountRuntimeStatus{}
	}
	accountKey := claudeDesktopAccountKey(authID)
	accountDirectory := filepath.Join(e.stateRoot, "accounts", accountKey)
	e.mu.Lock()
	runtimeRef := e.runtimes[authID]
	runtimeLoaded := runtimeRef != nil
	stoppingRef := e.stoppingRuntimeLocked(authID)
	stopping := stoppingRef != nil
	binding, found, _ := readClaudeDesktopRuntimeBinding(accountDirectory, accountKey)
	e.mu.Unlock()
	status := ClaudeAccountRuntimeStatus{AuthIDHash: accountKey, RuntimeLoaded: runtimeLoaded, RuntimeStopping: stopping}
	if runtimeRef == nil {
		runtimeRef = stoppingRef
	}
	if runtimeRef != nil && runtimeRef.executor != nil {
		status.Startup = runtimeRef.executor.StartupStatus()
		status.ControlPlane = runtimeRef.executor.desktopControlPlane.Status()
		runtimeRef.mu.Lock()
		inputs := make([]*helps.ClaudeDesktopRemoteInput, 0, len(runtimeRef.remoteInputs))
		for _, input := range runtimeRef.remoteInputs {
			inputs = append(inputs, input)
		}
		runtimeRef.mu.Unlock()
		if len(inputs) != 0 {
			status.AgentTasks = &claudetasks.Health{}
			for _, input := range inputs {
				rows, err := input.AgentTasks()
				status.AgentTasks.Observe(rows, err)
			}
		}
	} else {
		status.Startup.State = "stopped"
	}
	if !found {
		return status
	}
	status.State = binding.State
	status.ApprovedRevision = binding.ApprovedRevision
	status.ObservedRevision = binding.ObservedRevision
	status.MachineProfileID = binding.MachineProfileID
	status.DesktopProfile = binding.DesktopProfile
	status.PreviousRevision = binding.PreviousRevision
	status.PreviousMachine = binding.PreviousMachine
	status.PreviousDesktop = binding.PreviousDesktop
	status.ObservedMachine = binding.ObservedMachine
	status.ObservedDesktop = binding.ObservedDesktop
	status.QuarantineReason = binding.QuarantineReason
	status.CanPromote = binding.State == claudedesktop.EnrollmentQuarantined && binding.ObservedRevision != "" && binding.ObservedRevision != binding.ApprovedRevision && !stopping
	status.CanRollback = binding.PreviousRevision != "" && binding.ObservedRevision == binding.PreviousRevision && !stopping
	lifecyclePath := filepath.Join(accountDirectory, "instances", claudeDesktopRevisionInstanceID(binding.ApprovedRevision), "app-lifecycle.json")
	lifecycle, lifecycleFound, _ := readClaudeDesktopAppLifecycle(lifecyclePath, accountKey)
	if lifecycleFound {
		status.LifecycleState = lifecycle.State
		status.LifecycleGen = lifecycle.Generation
		status.AppSessionHash = claudeDesktopAppSessionHash(lifecycle.AppSessionID)
		status.PreviousExit = lifecycle.PreviousExit
		status.RecoveredUnclean = lifecycle.RecoveredUnclean
		status.StartedAt = lifecycle.StartedAt
		status.StoppedAt = lifecycle.StoppedAt
		if runtimeRef == nil && lifecycle.State != claudeDesktopAppRunning && lifecycle.ControlPlane != nil {
			status.ControlPlane = *lifecycle.ControlPlane
		}
	}
	return status
}

func (e *ClaudeAccountExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx, release, errSession := e.beginExecutionSession(ctx, opts.Metadata, req.Metadata)
	if errSession != nil {
		return cliproxyexecutor.Response{}, errSession
	}
	defer release()
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return cliproxyexecutor.Response{}, errRuntime
	}
	defer runtimeRef.release()
	return runtimeRef.executor.Execute(ctx, auth, req, opts)
}

func (e *ClaudeAccountExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ctx, release, errSession := e.beginExecutionSession(ctx, opts.Metadata, req.Metadata)
	if errSession != nil {
		return nil, errSession
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return nil, errRuntime
	}
	result, errStream := runtimeRef.executor.ExecuteStream(ctx, auth, req, opts)
	if errStream != nil || result == nil || result.Chunks == nil {
		runtimeRef.release()
		return result, errStream
	}
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	handedOff = true
	go func() {
		defer close(out)
		defer runtimeRef.release()
		defer release()
		forward := true
		cancelForward := func() {
			forward = false
			// Preserve one observable cancellation even if the downstream
			// consumer stopped reading. Do not pin the account drain on it.
			select {
			case <-out:
			default:
			}
			cause := ctx.Err()
			if cancelled := newClaudeDesktopCancellationError(ctx, true, cause); cancelled != nil {
				cause = cancelled
			}
			out <- cliproxyexecutor.StreamChunk{Err: cause}
		}
		for chunk := range result.Chunks {
			if !forward {
				continue
			}
			if ctx.Err() != nil {
				cancelForward()
				continue
			}
			select {
			case <-ctx.Done():
				cancelForward()
			case out <- chunk:
			}
		}
		if forward && ctx.Err() != nil {
			cancelForward()
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}, nil
}

func (e *ClaudeAccountExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return nil, errRuntime
	}
	defer runtimeRef.release()
	return runtimeRef.executor.Refresh(ctx, auth)
}

func (e *ClaudeAccountExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx, release, errSession := e.beginExecutionSession(ctx, opts.Metadata, req.Metadata)
	if errSession != nil {
		return cliproxyexecutor.Response{}, errSession
	}
	defer release()
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return cliproxyexecutor.Response{}, errRuntime
	}
	defer runtimeRef.release()
	return runtimeRef.executor.CountTokens(ctx, auth, req, opts)
}

func (e *ClaudeAccountExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if ctx == nil && req != nil {
		ctx = req.Context()
	}
	ctx, release, errSession := e.beginExecutionSession(ctx)
	if errSession != nil {
		return nil, errSession
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	runtimeRef, errRuntime := e.acquireRuntime(auth)
	if errRuntime != nil {
		return nil, errRuntime
	}
	response, errRequest := runtimeRef.executor.HttpRequest(ctx, auth, req)
	if errRequest != nil || response == nil || response.Body == nil {
		runtimeRef.release()
		return response, errRequest
	}
	handedOff = true
	return helps.RetainClaudeDesktopResponseLifetime(response, func() {
		runtimeRef.release()
		release()
	}), nil
}

func (e *ClaudeAccountExecutor) beginExecutionSession(ctx context.Context, metadata ...map[string]any) (context.Context, func(), error) {
	if e == nil {
		return ctx, func() {}, newClaudeDesktopEligibilityError("account runtime router is unavailable")
	}
	bound, err := e.executionSessions.Bind(ctx, metadata...)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return bound, func() {}, newClaudeDesktopCancellationError(nil, true, err)
		}
		return bound, func() {}, newClaudeDesktopEligibilityError(err.Error())
	}
	bound, release := helps.BindClaudeDesktopQueryLifetime(bound, e.executionSessions.Lifetime(bound))
	if bound.Err() != nil {
		err = claudeDesktopRequestContextError(bound)
		release()
		return bound, func() {}, err
	}
	return bound, release, nil
}

type claudeAccountRuntimeResponseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *claudeAccountRuntimeResponseBody) Close() error {
	if b == nil || b.ReadCloser == nil {
		return nil
	}
	var errClose error
	b.once.Do(func() {
		errClose = b.ReadCloser.Close()
		if b.release != nil {
			b.release()
		}
	})
	return errClose
}

func (e *ClaudeAccountExecutor) acquireRuntime(auth *cliproxyauth.Auth) (*claudeAccountRuntime, error) {
	if e == nil {
		return nil, newClaudeDesktopEligibilityError("account runtime router is unavailable")
	}
	enrollment, errEnrollment := e.validateActiveRuntimeAuth(auth)
	if errEnrollment != nil {
		return nil, errEnrollment
	}
	accountKey := claudeDesktopAccountKey(auth.ID)
	accountDirectory := filepath.Join(e.stateRoot, "accounts", accountKey)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, newClaudeDesktopEligibilityError("account runtime router is closed")
	}
	if draining := e.stoppingRuntimeLocked(auth.ID); draining != nil {
		e.mu.Unlock()
		return nil, newClaudeDesktopRuntimeDrainingError()
	}
	profile, revision, errRevision := e.desiredRuntimeLocked(enrollment, auth, accountDirectory)
	if errRevision != nil {
		e.mu.Unlock()
		return nil, errRevision
	}
	binding, found, errBinding := readClaudeDesktopRuntimeBinding(accountDirectory, accountKey)
	if errBinding != nil {
		e.mu.Unlock()
		return nil, newClaudeDesktopEligibilityError("runtime binding is unreadable")
	}
	if found && (binding.State == claudedesktop.EnrollmentQuarantined || binding.ApprovedRevision != revision) {
		if binding.ApprovedRevision != revision {
			binding.State = claudedesktop.EnrollmentQuarantined
			binding.ObservedRevision = revision
			binding.QuarantineReason = "runtime binding drift requires explicit promotion"
			binding.ObservedMachine = profile.ID
			binding.ObservedDesktop = e.bundleVersion
			binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if errPersist := persistClaudeDesktopRuntimeBinding(accountDirectory, binding); errPersist != nil {
				current := e.runtimes[auth.ID]
				delete(e.runtimes, auth.ID)
				if current != nil {
					e.stopping[auth.ID] = current
				}
				e.mu.Unlock()
				if current != nil {
					current.quarantine()
				}
				return nil, newClaudeDesktopEligibilityError("runtime binding drift could not be persisted")
			}
		}
		current := e.runtimes[auth.ID]
		delete(e.runtimes, auth.ID)
		if current != nil {
			e.stopping[auth.ID] = current
		}
		e.mu.Unlock()
		if current != nil {
			current.quarantine()
		}
		return nil, newClaudeDesktopEligibilityError("account runtime is quarantined: " + binding.QuarantineReason)
	}
	if !found {
		binding = newClaudeDesktopRuntimeBinding(accountKey, revision, profile.ID, e.bundleVersion)
		if errPersist := persistClaudeDesktopRuntimeBinding(accountDirectory, binding); errPersist != nil {
			e.mu.Unlock()
			return nil, fmt.Errorf("claude desktop account runtime: persist binding: %w", errPersist)
		}
	}
	if current := e.runtimes[auth.ID]; current != nil && current.revision == revision {
		if current.acquire() {
			e.mu.Unlock()
			return current, nil
		}
	}

	instanceDirectory := filepath.Join(accountDirectory, "instances", claudeDesktopRevisionInstanceID(revision))
	if errMkdir := os.MkdirAll(instanceDirectory, 0o700); errMkdir != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("claude desktop account runtime: create state directory: %w", errMkdir)
	}
	durableStatePath := filepath.Join(accountDirectory, "durable")
	if errMigrate := migrateClaudeDesktopDurableState(accountDirectory, durableStatePath, binding); errMigrate != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("claude desktop account runtime: migrate durable state: %w", errMigrate)
	}
	lifecycle, errLifecycle := prepareClaudeDesktopAppLifecycle(instanceDirectory, accountKey, revision, time.Now())
	if errLifecycle != nil {
		e.mu.Unlock()
		return nil, newClaudeDesktopEligibilityError("application lifecycle is unavailable: " + errLifecycle.Error())
	}
	runtimeCfg := e.cfg.CloneForRuntime()
	if runtimeCfg == nil {
		runtimeCfg = &config.Config{}
	}
	runtimeCfg.ClaudeDesktop.StatePath = instanceDirectory
	profileSnapshot := profile
	runtimeOptions := claudeDesktopRuntimeOptions{
		executionSessions:            &e.executionSessions,
		statePath:                    instanceDirectory,
		durableStatePath:             durableStatePath,
		appSessionID:                 lifecycle.AppSessionID,
		machineProfileID:             profileSnapshot.ID,
		enableATIS:                   true,
		enableControlPlane:           true,
		enableStartup:                true,
		startupDoerFactory:           e.startupDoerFactory,
		telemetryEndpointDoerFactory: e.telemetryEndpointDoerFactory,
		hostSnapshot: func() claudetelemetry.HostSnapshot {
			return profileSnapshot.Host
		},
	}
	if !isZeroSDKProcessSnapshot(profileSnapshot.SDKProcess) {
		runtimeOptions.sdkProcessSnapshot = func() (claudetelemetry.SDKProcessSnapshot, bool) {
			return profileSnapshot.SDKProcess, true
		}
	}
	if e.credentialManager != nil {
		runtimeOptions.controlCredentials = &helps.ClaudeDesktopControlCredentials{
			Manager: e.credentialManager,
			Owner:   e,
			Acquire: (&ClaudeExecutor{cfg: runtimeCfg}).Refresh,
		}
	}
	inner := newClaudeExecutorWithRuntime(runtimeCfg, runtimeOptions)
	if errEligibility := inner.validateClaudeDesktopAuth(auth); errEligibility != nil {
		inner.Close()
		e.mu.Unlock()
		return nil, errEligibility
	}
	if errPersist := persistClaudeDesktopAppLifecycle(filepath.Join(instanceDirectory, "app-lifecycle.json"), lifecycle); errPersist != nil {
		inner.Quarantine()
		e.mu.Unlock()
		return nil, newClaudeDesktopEligibilityError("application lifecycle could not be persisted")
	}
	if errActivate := inner.Activate(auth); errActivate != nil {
		inner.Quarantine()
		_ = finishClaudeDesktopAppLifecycle(filepath.Join(instanceDirectory, "app-lifecycle.json"), accountKey, lifecycle.AppSessionID, claudeDesktopAppQuarantined, "activation failed", time.Now())
		binding.State = claudedesktop.EnrollmentQuarantined
		binding.QuarantineReason = "account runtime activation failed"
		binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_ = persistClaudeDesktopRuntimeBinding(accountDirectory, binding)
		e.mu.Unlock()
		return nil, newClaudeDesktopEligibilityError("account runtime activation failed")
	}
	next := &claudeAccountRuntime{
		executor:          inner,
		accountKey:        accountKey,
		authID:            auth.ID,
		revision:          revision,
		machineProfileID:  profileSnapshot.ID,
		stateDirectory:    instanceDirectory,
		lifecyclePath:     filepath.Join(instanceDirectory, "app-lifecycle.json"),
		appSessionID:      lifecycle.AppSessionID,
		lifecycleGen:      lifecycle.Generation,
		recoveredUnclean:  lifecycle.RecoveredUnclean,
		recordOwner:       inner.claudeDesktopRecordOwner(auth),
		watchSuppressions: make(map[string]time.Time),
		watchIdleTimers:   make(map[string]*desktopSessionIdleTimer),
	}
	if !next.acquire() {
		inner.Close()
		e.mu.Unlock()
		return nil, fmt.Errorf("claude desktop account runtime: failed to acquire new runtime")
	}
	previous := e.runtimes[auth.ID]
	e.runtimes[auth.ID] = next
	e.mu.Unlock()
	if previous != nil && previous != next {
		previous.quarantine()
	}
	return next, nil
}

func (e *ClaudeAccountExecutor) validateActiveRuntimeAuth(auth *cliproxyauth.Auth) (claudedesktop.Enrollment, error) {
	if auth == nil {
		return claudedesktop.Enrollment{}, newClaudeDesktopEligibilityError("auth is required")
	}
	if auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		return claudedesktop.Enrollment{}, newClaudeDesktopEligibilityError("credential is disabled")
	}
	validator := &ClaudeExecutor{
		cfg:               e.cfg,
		desktopOnly:       true,
		desktopProfile:    e.bundle,
		desktopProfileErr: e.bundleErr,
	}
	if errValidate := validator.validateClaudeDesktopAuth(auth); errValidate != nil {
		return claudedesktop.Enrollment{}, errValidate
	}
	enrollment, errEnrollment := claudedesktop.ValidateActiveEnrollment(auth.ID, auth.Metadata)
	if errEnrollment != nil {
		return claudedesktop.Enrollment{}, newClaudeDesktopEligibilityError(fmt.Sprintf("credential is not active: %v", errEnrollment))
	}
	if _, present := auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey]; present {
		if _, errMaterials := claudedesktop.TelemetryMaterialsFromMetadata(auth.Metadata); errMaterials != nil {
			return claudedesktop.Enrollment{}, newClaudeDesktopEligibilityError("telemetry enrollment materials are invalid")
		}
	}
	return enrollment, nil
}

func (e *ClaudeAccountExecutor) desiredRuntimeLocked(enrollment claudedesktop.Enrollment, auth *cliproxyauth.Auth, accountDirectory string) (claudeDesktopMachineProfile, string, error) {
	profile, errProfile := e.machinePool.resolve(auth.ID, claudeDesktopAccountKey(auth.ID), accountDirectory)
	if errProfile != nil {
		return claudeDesktopMachineProfile{}, "", newClaudeDesktopEligibilityError("machine profile is unavailable: " + errProfile.Error())
	}
	revision, errRevision := e.runtimeRevision(enrollment, auth, profile)
	if errRevision != nil {
		return claudeDesktopMachineProfile{}, "", errRevision
	}
	return profile, revision, nil
}

func (e *ClaudeAccountExecutor) runtimeRevision(enrollment claudedesktop.Enrollment, auth *cliproxyauth.Auth, profile claudeDesktopMachineProfile) (string, error) {
	effectiveProxy := ""
	if auth != nil {
		effectiveProxy = strings.TrimSpace(auth.ProxyURL)
	}
	if effectiveProxy == "" && e.cfg != nil {
		effectiveProxy = strings.TrimSpace(e.cfg.ProxyURL)
	}
	_, baseURL := claudeCreds(auth)
	materialRevision := ""
	if auth != nil && auth.Metadata != nil {
		if _, present := auth.Metadata[claudedesktop.MetadataTelemetryMaterialsKey]; present {
			materials, errMaterials := claudedesktop.TelemetryMaterialsFromMetadata(auth.Metadata)
			if errMaterials != nil {
				return "", newClaudeDesktopEligibilityError("telemetry enrollment materials are invalid")
			}
			encodedMaterials, _ := json.Marshal(materials)
			digestMaterials := sha256.Sum256(encodedMaterials)
			materialRevision = hex.EncodeToString(digestMaterials[:])
		}
	}
	trustedDeviceRevision := ""
	if auth != nil && auth.Metadata != nil {
		trustedDevice, _ := auth.Metadata[claudedesktop.MetadataTrustedDeviceTokenKey].(string)
		digestDevice := sha256.Sum256([]byte(strings.TrimSpace(trustedDevice)))
		trustedDeviceRevision = hex.EncodeToString(digestDevice[:])
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"claude-desktop-account-runtime-v2",
		auth.ID,
		enrollment.AccountUUID,
		enrollment.OrganizationUUID,
		enrollment.DeviceID,
		enrollment.ProfileVersion,
		effectiveProxy,
		strings.TrimSpace(baseURL),
		e.bundleRevision,
		profile.ID,
		profile.Revision,
		trustedDeviceRevision,
		materialRevision,
	}, "\x00")))
	return hex.EncodeToString(digest[:]), nil
}

func (r *claudeAccountRuntime) acquire() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retiring || r.closed {
		return false
	}
	r.active++
	return true
}

func (r *claudeAccountRuntime) release() {
	if r == nil {
		return
	}
	shouldClose := false
	joinInput := false
	r.mu.Lock()
	if r.active > 0 {
		r.active--
	}
	if r.retiring && r.active == 0 && !r.closed && !r.closing {
		r.closing = true
		shouldClose = true
		joinInput = len(r.remoteInputs) != 0
	}
	r.mu.Unlock()
	if shouldClose {
		// The final release may run on a query's response producer. Closing
		// joins that query's input actor, which can still be waiting for this
		// producer to close its channel. Drain from an independent goroutine;
		// the closing/closed flags still bracket the actual completed cleanup.
		if joinInput {
			go r.finishClose()
		} else {
			r.finishClose()
		}
	}
}

func (r *claudeAccountRuntime) retire() {
	if r == nil {
		return
	}
	shouldClose := false
	r.mu.Lock()
	r.retiring = true
	for _, input := range r.remoteInputs {
		input.Stop()
	}
	if r.active == 0 && !r.closed && !r.closing {
		r.closing = true
		shouldClose = true
	}
	r.mu.Unlock()
	r.cancelDesktopSessionIdleTimers()
	if shouldClose {
		r.finishClose()
	}
}

func (r *claudeAccountRuntime) quarantine() {
	if r == nil {
		return
	}
	shouldClose := false
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.retiring = true
	r.quarantined = true
	for _, input := range r.remoteInputs {
		input.Stop()
	}
	if r.active == 0 && !r.closed && !r.closing {
		r.closing = true
		shouldClose = true
	}
	closing := r.closing
	r.mu.Unlock()
	r.cancelDesktopSessionIdleTimers()
	// Signal all network owners before joining any of them. Another close may
	// already be waiting on bridge cleanup or the final telemetry flush.
	r.executor.prepareQuarantine()
	if shouldClose {
		r.finishClose()
	} else if closing {
		r.executor.Quarantine()
	}
}

// A runtime remains draining until remote cleanup and the final diagnostic
// checkpoint have completed. Starting Close is not evidence that it finished.
func (r *claudeAccountRuntime) finishClose() {
	r.mu.Lock()
	quarantined := r.quarantined
	r.mu.Unlock()
	if quarantined {
		r.executor.Quarantine()
	} else {
		r.executor.Close()
	}
	// Input actors never hold a runtime admission for their whole lifetime.
	// Retirement cancels them before active model requests begin draining.
	r.mu.Lock()
	inputs := r.remoteInputs
	r.remoteInputs = nil
	r.mu.Unlock()
	for _, input := range inputs {
		input.Stop()
		<-input.Done()
	}
	// Quarantine may arrive during graceful cleanup. Serialize its disposition
	// with the exact terminal checkpoint, not the stale close-entry snapshot.
	r.mu.Lock()
	defer r.mu.Unlock()
	state, reason := claudeDesktopAppStopped, "app_quit"
	if r.quarantined {
		state, reason = claudeDesktopAppQuarantined, "runtime quarantined"
	}
	if err := finishClaudeDesktopAppLifecycle(r.lifecyclePath, r.accountKey, r.appSessionID, state, reason, time.Now(), r.executor.desktopControlPlane.Status()); err != nil {
		log.WithError(err).Warn("claude desktop: final application diagnostics could not be persisted")
	}
	r.closed, r.closing = true, false
}

func (r *claudeAccountRuntime) isClosed() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	return closed
}

func (e *ClaudeAccountExecutor) stoppingRuntimeLocked(authID string) *claudeAccountRuntime {
	if e == nil {
		return nil
	}
	runtimeRef := e.stopping[strings.TrimSpace(authID)]
	if runtimeRef != nil && runtimeRef.isClosed() {
		delete(e.stopping, strings.TrimSpace(authID))
		return nil
	}
	return runtimeRef
}

// CloseAuth retires only the runtime owned by the removed credential.
func (e *ClaudeAccountExecutor) CloseAuth(authID string) {
	if e == nil || strings.TrimSpace(authID) == "" {
		return
	}
	authID = strings.TrimSpace(authID)
	e.mu.Lock()
	runtimeRef := e.runtimes[authID]
	delete(e.runtimes, authID)
	if runtimeRef != nil {
		e.stopping[authID] = runtimeRef
	}
	e.mu.Unlock()
	if runtimeRef != nil {
		runtimeRef.retire()
	}
}

// QuarantineAuth freezes one account's durable obligations and closes its
// control-plane and transport resources without synthesizing a normal quit.
func (e *ClaudeAccountExecutor) QuarantineAuth(authID string) {
	_ = e.quarantineAuth(authID, "account runtime quarantined")
}

func (e *ClaudeAccountExecutor) quarantineAuth(authID, reason string) error {
	if e == nil || strings.TrimSpace(authID) == "" {
		return nil
	}
	authID = strings.TrimSpace(authID)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "account runtime quarantined"
	}
	accountKey := claudeDesktopAccountKey(authID)
	accountDirectory := filepath.Join(e.stateRoot, "accounts", accountKey)
	e.mu.Lock()
	runtimeRef := e.runtimes[authID]
	if runtimeRef == nil {
		runtimeRef = e.stoppingRuntimeLocked(authID)
	}
	delete(e.runtimes, authID)
	if runtimeRef != nil {
		e.stopping[authID] = runtimeRef
	}
	binding, found, errBinding := readClaudeDesktopRuntimeBinding(accountDirectory, accountKey)
	if errBinding == nil && found {
		if binding.State == claudedesktop.EnrollmentQuarantined && strings.TrimSpace(binding.QuarantineReason) != "" {
			reason = binding.QuarantineReason
		}
		binding.State = claudedesktop.EnrollmentQuarantined
		binding.QuarantineReason = reason
		binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		errBinding = persistClaudeDesktopRuntimeBinding(accountDirectory, binding)
	}
	e.mu.Unlock()
	if runtimeRef != nil {
		runtimeRef.quarantine()
	}
	return errBinding
}

func (e *ClaudeAccountExecutor) CloseExecutionSession(sessionID string) {
	if e == nil {
		return
	}
	if strings.TrimSpace(sessionID) == cliproxyauth.CloseAllExecutionSessionsID {
		// Provider/account shutdown retains the accepted active-request drain.
		e.Close()
		return
	}
	e.executionSessions.Close(sessionID)
}

// Close retires every account runtime owned by this provider router.
func (e *ClaudeAccountExecutor) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.closed = true
	if e.stopping == nil {
		e.stopping = make(map[string]*claudeAccountRuntime)
	}
	for authID, runtimeRef := range e.runtimes {
		e.stopping[authID] = runtimeRef
	}
	e.runtimes = make(map[string]*claudeAccountRuntime)
	runtimes := make([]*claudeAccountRuntime, 0, len(e.stopping))
	for _, runtimeRef := range e.stopping {
		if runtimeRef != nil {
			runtimes = append(runtimes, runtimeRef)
		}
	}
	e.mu.Unlock()
	for _, runtimeRef := range runtimes {
		runtimeRef.retire()
	}
}

func newClaudeDesktopMachinePool(cfg *config.Config) claudeDesktopMachinePool {
	localHost := claudetelemetry.DetectHostSnapshot()
	pool := claudeDesktopMachinePool{
		profiles: make(map[string]claudeDesktopMachineProfile),
		explicit: make(map[string]string),
		bound:    make(map[string]string),
	}
	if cfg != nil {
		for authID, profileID := range cfg.ClaudeDesktop.MachineProfileBindings {
			pool.explicit[strings.TrimSpace(authID)] = strings.TrimSpace(profileID)
		}
		for _, configured := range cfg.ClaudeDesktop.MachineProfiles {
			profile := claudeDesktopMachineProfile{
				ID:       strings.TrimSpace(configured.ID),
				Revision: claudeDesktopConfiguredMachineProfileRevision(configured),
				Host: claudetelemetry.HostSnapshot{
					TotalMemoryBytes:     configured.TotalMemoryBytes,
					AvailableMemoryBytes: configured.AvailableMemoryBytes,
					CPUModel:             strings.TrimSpace(configured.CPUModel),
					OSBuild:              strings.TrimSpace(configured.OSBuild),
					OSRelease:            strings.TrimSpace(configured.OSRelease),
					OSVersion:            strings.TrimSpace(configured.OSVersion),
				},
				SDKProcess: claudetelemetry.SDKProcessSnapshot{
					UptimeSeconds:         configured.SDKProcess.UptimeSeconds,
					RSS:                   configured.SDKProcess.RSS,
					FootprintBytes:        configured.SDKProcess.FootprintBytes,
					CommitBytes:           configured.SDKProcess.CommitBytes,
					PeakFootprintBytes:    configured.SDKProcess.PeakFootprintBytes,
					MemorySampleAgeMS:     configured.SDKProcess.MemorySampleAgeMS,
					HeapTotal:             configured.SDKProcess.HeapTotal,
					HeapUsed:              configured.SDKProcess.HeapUsed,
					External:              configured.SDKProcess.External,
					ArrayBuffers:          configured.SDKProcess.ArrayBuffers,
					ConstrainedMemory:     configured.SDKProcess.ConstrainedMemory,
					CPUUserMicroseconds:   configured.SDKProcess.CPUUserMicroseconds,
					CPUSystemMicroseconds: configured.SDKProcess.CPUSystemMicroseconds,
				},
			}
			profile.Host = mergeClaudeDesktopHostSnapshot(profile.Host, localHost)
			if profile.ID != "" {
				pool.profiles[profile.ID] = profile
			}
		}
	}
	if len(pool.profiles) == 0 {
		pool.profiles["local-host"] = claudeDesktopMachineProfile{
			ID:       "local-host",
			Revision: claudeDesktopLocalMachineProfileRevision(localHost),
			Host:     localHost,
		}
	}
	pool.ids = make([]string, 0, len(pool.profiles))
	for id := range pool.profiles {
		pool.ids = append(pool.ids, id)
	}
	sort.Strings(pool.ids)
	return pool
}

func claudeDesktopConfiguredMachineProfileRevision(profile config.ClaudeDesktopMachineProfile) string {
	profile.ID = strings.TrimSpace(profile.ID)
	profile.CPUModel = strings.TrimSpace(profile.CPUModel)
	profile.OSBuild = strings.TrimSpace(profile.OSBuild)
	profile.OSRelease = strings.TrimSpace(profile.OSRelease)
	profile.OSVersion = strings.TrimSpace(profile.OSVersion)
	encoded, _ := json.Marshal(profile)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func claudeDesktopLocalMachineProfileRevision(host claudetelemetry.HostSnapshot) string {
	stable := struct {
		TotalMemoryBytes uint64 `json:"total_memory_bytes"`
		CPUModel         string `json:"cpu_model"`
		OSBuild          string `json:"os_build"`
		OSRelease        string `json:"os_release"`
		OSVersion        string `json:"os_version"`
	}{
		TotalMemoryBytes: host.TotalMemoryBytes,
		CPUModel:         strings.TrimSpace(host.CPUModel),
		OSBuild:          strings.TrimSpace(host.OSBuild),
		OSRelease:        strings.TrimSpace(host.OSRelease),
		OSVersion:        strings.TrimSpace(host.OSVersion),
	}
	encoded, _ := json.Marshal(stable)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (p *claudeDesktopMachinePool) resolve(authID, accountKey, accountDirectory string) (claudeDesktopMachineProfile, error) {
	if p == nil || len(p.ids) == 0 {
		return claudeDesktopMachineProfile{}, fmt.Errorf("machine profile pool is empty")
	}
	if explicit := strings.TrimSpace(p.explicit[authID]); explicit != "" {
		if profile, ok := p.profiles[explicit]; ok {
			p.bound[accountKey] = explicit
			if errPersist := persistClaudeDesktopMachineBinding(accountDirectory, accountKey, explicit); errPersist != nil {
				return claudeDesktopMachineProfile{}, errPersist
			}
			return profile, nil
		}
		log.WithFields(log.Fields{"auth_id": authID, "machine_profile": explicit}).Warn("claude desktop: configured machine profile binding is unavailable; using stable pool assignment")
	}
	if bound := p.bound[accountKey]; bound != "" {
		if profile, ok := p.profiles[bound]; ok {
			return profile, nil
		}
	}
	if persisted, okPersisted := readClaudeDesktopMachineBinding(accountDirectory, accountKey); okPersisted {
		if profile, ok := p.profiles[persisted]; ok {
			p.bound[accountKey] = persisted
			return profile, nil
		}
	}
	digest := sha256.Sum256([]byte("claude-desktop-machine-pool-v1\x00" + accountKey))
	start := int(binary.BigEndian.Uint64(digest[:8]) % uint64(len(p.ids)))
	usage := make(map[string]int, len(p.ids))
	for _, profileID := range p.bound {
		if _, ok := p.profiles[profileID]; ok {
			usage[profileID]++
		}
	}
	selected := p.ids[start]
	selectedUsage := usage[selected]
	for offset := 1; offset < len(p.ids); offset++ {
		candidate := p.ids[(start+offset)%len(p.ids)]
		if candidateUsage := usage[candidate]; candidateUsage < selectedUsage {
			selected = candidate
			selectedUsage = candidateUsage
			if selectedUsage == 0 {
				break
			}
		}
	}
	if errPersist := persistClaudeDesktopMachineBinding(accountDirectory, accountKey, selected); errPersist != nil {
		return claudeDesktopMachineProfile{}, errPersist
	}
	p.bound[accountKey] = selected
	return p.profiles[selected], nil
}

func (p *claudeDesktopMachinePool) restoreBindings(stateRoot string) {
	if p == nil || strings.TrimSpace(stateRoot) == "" || len(p.profiles) == 0 {
		return
	}
	accountDirectories, errReadDir := os.ReadDir(filepath.Join(stateRoot, "accounts"))
	if errReadDir != nil {
		return
	}
	for _, entry := range accountDirectories {
		if !entry.IsDir() {
			continue
		}
		payload, errRead := os.ReadFile(filepath.Join(stateRoot, "accounts", entry.Name(), "machine-binding.json"))
		if errRead != nil {
			continue
		}
		var binding claudeDesktopMachineBinding
		if errDecode := json.Unmarshal(payload, &binding); errDecode != nil || binding.Version != claudeDesktopMachineBindingVersion {
			continue
		}
		accountKey := strings.TrimSpace(binding.AccountKey)
		profileID := strings.TrimSpace(binding.MachineProfileID)
		if accountKey == "" || profileID == "" || accountKey != entry.Name() {
			continue
		}
		if _, ok := p.profiles[profileID]; ok {
			p.bound[accountKey] = profileID
		}
	}
}

func claudeDesktopAccountKey(authID string) string {
	digest := sha256.Sum256([]byte("claude-desktop-auth-partition-v1\x00" + strings.TrimSpace(authID)))
	return hex.EncodeToString(digest[:12])
}

func claudeDesktopBundleIdentity(cfg *config.Config) (*claudeprofile.Bundle, string, string, error) {
	bundlePath := ""
	if cfg != nil {
		bundlePath = strings.TrimSpace(cfg.ClaudeDesktop.BundlePath)
	}
	bundle, errLoad := claudeprofile.Load(bundlePath)
	if errLoad != nil {
		return nil, "", "", errLoad
	}
	encoded, errMarshal := json.Marshal(bundle)
	if errMarshal != nil {
		return nil, "", "", fmt.Errorf("marshal Claude Desktop profile identity: %w", errMarshal)
	}
	digest := sha256.Sum256(encoded)
	return bundle, hex.EncodeToString(digest[:]), strings.TrimSpace(bundle.DesktopVersion), nil
}

func newClaudeDesktopRuntimeBinding(authIDHash, revision, machineProfileID, desktopProfile string) claudeDesktopRuntimeBinding {
	return claudeDesktopRuntimeBinding{
		Version:          claudeDesktopRuntimeBindingVersion,
		AuthIDHash:       strings.TrimSpace(authIDHash),
		State:            claudedesktop.EnrollmentActive,
		ApprovedRevision: strings.TrimSpace(revision),
		MachineProfileID: strings.TrimSpace(machineProfileID),
		DesktopProfile:   strings.TrimSpace(desktopProfile),
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func claudeDesktopRevisionInstanceID(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) < 24 {
		return revision
	}
	return revision[:24]
}

func validClaudeDesktopRuntimeRevision(revision string) bool {
	revision = strings.TrimSpace(revision)
	if len(revision) != sha256.Size*2 {
		return false
	}
	decoded, errDecode := hex.DecodeString(revision)
	return errDecode == nil && len(decoded) == sha256.Size
}

func readClaudeDesktopRuntimeBinding(accountDirectory, authIDHash string) (claudeDesktopRuntimeBinding, bool, error) {
	path := filepath.Join(accountDirectory, "runtime-binding.json")
	payload, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return claudeDesktopRuntimeBinding{}, false, nil
		}
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("read Claude Desktop runtime binding: %w", errRead)
	}
	var binding claudeDesktopRuntimeBinding
	if errDecode := json.Unmarshal(payload, &binding); errDecode != nil {
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("decode Claude Desktop runtime binding: %w", errDecode)
	}
	if binding.Version != claudeDesktopRuntimeBindingVersion {
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("unsupported Claude Desktop runtime binding version %d", binding.Version)
	}
	if strings.TrimSpace(binding.AuthIDHash) == "" || binding.AuthIDHash != strings.TrimSpace(authIDHash) {
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("Claude Desktop runtime binding does not match account partition")
	}
	if !validClaudeDesktopRuntimeRevision(binding.ApprovedRevision) || strings.TrimSpace(binding.MachineProfileID) == "" || strings.TrimSpace(binding.DesktopProfile) == "" {
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("Claude Desktop runtime binding is incomplete")
	}
	if binding.ObservedRevision != "" && !validClaudeDesktopRuntimeRevision(binding.ObservedRevision) {
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("Claude Desktop observed runtime revision is invalid")
	}
	if binding.PreviousRevision != "" && !validClaudeDesktopRuntimeRevision(binding.PreviousRevision) {
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("Claude Desktop previous runtime revision is invalid")
	}
	switch binding.State {
	case claudedesktop.EnrollmentActive, claudedesktop.EnrollmentQuarantined:
	default:
		return claudeDesktopRuntimeBinding{}, false, fmt.Errorf("Claude Desktop runtime binding state is %q", binding.State)
	}
	return binding, true, nil
}

func persistClaudeDesktopRuntimeBinding(accountDirectory string, binding claudeDesktopRuntimeBinding) error {
	if errMkdir := os.MkdirAll(accountDirectory, 0o700); errMkdir != nil {
		return fmt.Errorf("create Claude Desktop account state directory: %w", errMkdir)
	}
	payload, errMarshal := json.MarshalIndent(binding, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop runtime binding: %w", errMarshal)
	}
	payload = append(payload, '\n')
	path := filepath.Join(accountDirectory, "runtime-binding.json")
	if existing, errRead := os.ReadFile(path); errRead == nil && string(existing) == string(payload) {
		return nil
	}
	if errWrite := helps.AtomicWriteFile(path, payload, 0o600); errWrite != nil {
		return fmt.Errorf("persist Claude Desktop runtime binding: %w", errWrite)
	}
	return nil
}

func mergeClaudeDesktopHostSnapshot(profile, local claudetelemetry.HostSnapshot) claudetelemetry.HostSnapshot {
	if profile.TotalMemoryBytes == 0 {
		profile.TotalMemoryBytes = local.TotalMemoryBytes
	}
	if profile.AvailableMemoryBytes == 0 {
		profile.AvailableMemoryBytes = local.AvailableMemoryBytes
	}
	if profile.TotalMemoryBytes > 0 && profile.AvailableMemoryBytes > profile.TotalMemoryBytes {
		profile.AvailableMemoryBytes = profile.TotalMemoryBytes
	}
	if profile.CPUModel == "" {
		profile.CPUModel = local.CPUModel
	}
	if profile.OSBuild == "" {
		profile.OSBuild = local.OSBuild
	}
	if profile.OSRelease == "" {
		profile.OSRelease = local.OSRelease
	}
	if profile.OSVersion == "" {
		profile.OSVersion = local.OSVersion
	}
	return profile
}

func isZeroSDKProcessSnapshot(snapshot claudetelemetry.SDKProcessSnapshot) bool {
	return snapshot.UptimeSeconds == 0 && snapshot.RSS == 0 && snapshot.FootprintBytes == 0 && snapshot.CommitBytes == 0 &&
		snapshot.PeakFootprintBytes == 0 && snapshot.MemorySampleAgeMS == 0 && snapshot.HeapTotal == 0 && snapshot.HeapUsed == 0 &&
		snapshot.External == 0 && snapshot.ArrayBuffers == 0 && snapshot.ConstrainedMemory == 0 &&
		snapshot.CPUUserMicroseconds == 0 && snapshot.CPUSystemMicroseconds == 0
}

func readClaudeDesktopMachineBinding(accountDirectory, accountKey string) (string, bool) {
	payload, errRead := os.ReadFile(filepath.Join(accountDirectory, "machine-binding.json"))
	if errRead != nil {
		return "", false
	}
	var binding claudeDesktopMachineBinding
	if errDecode := json.Unmarshal(payload, &binding); errDecode != nil || binding.Version != claudeDesktopMachineBindingVersion || binding.AccountKey != accountKey || strings.TrimSpace(binding.MachineProfileID) == "" {
		return "", false
	}
	return strings.TrimSpace(binding.MachineProfileID), true
}

func persistClaudeDesktopMachineBinding(accountDirectory, accountKey, profileID string) error {
	if errMkdir := os.MkdirAll(accountDirectory, 0o700); errMkdir != nil {
		return fmt.Errorf("create Claude Desktop account state directory: %w", errMkdir)
	}
	binding := claudeDesktopMachineBinding{
		Version:          claudeDesktopMachineBindingVersion,
		AccountKey:       accountKey,
		MachineProfileID: profileID,
	}
	payload, errMarshal := json.MarshalIndent(binding, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop machine binding: %w", errMarshal)
	}
	payload = append(payload, '\n')
	path := filepath.Join(accountDirectory, "machine-binding.json")
	if existing, errRead := os.ReadFile(path); errRead == nil && string(existing) == string(payload) {
		return nil
	}
	if errWrite := helps.AtomicWriteFile(path, payload, 0o600); errWrite != nil {
		return fmt.Errorf("persist Claude Desktop machine binding: %w", errWrite)
	}
	return nil
}
