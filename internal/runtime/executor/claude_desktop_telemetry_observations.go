package executor

import (
	"context"

	claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"
	claudetelemetry "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/telemetry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func (e *ClaudeAccountExecutor) ObserveDesktopRendererTelemetry(ctx context.Context, authID string, observation cliproxyexecutor.ClaudeDesktopRendererTelemetryObservation) error {
	return e.withDesktopTelemetryRuntime(authID, func(runtime *claudeAccountRuntime, auth *cliproxyauth.Auth) error {
		return runtime.executor.desktopTelemetry.RecordObservedRendererEvent(ctx, auth, claudetelemetry.ObservedRendererEvent{
			Kind: observation.Kind, Route: string(observation.Route), SessionID: observation.SessionID, PromptID: observation.PromptID,
			ClientRequestID: observation.ClientRequestID, Properties: observation.Properties,
		})
	})
}

func (e *ClaudeAccountExecutor) ObserveDesktopMainProcessTelemetry(ctx context.Context, authID string, observation cliproxyexecutor.ClaudeDesktopMainProcessTelemetryObservation) error {
	return e.withDesktopTelemetryRuntime(authID, func(runtime *claudeAccountRuntime, auth *cliproxyauth.Auth) error {
		return runtime.executor.desktopTelemetry.RecordObservedMainProcessEvent(ctx, auth, claudetelemetry.ObservedMainProcessEvent{
			Kind: observation.Kind, SessionID: observation.SessionID, ClientRequestID: observation.ClientRequestID, Metadata: observation.Metadata,
		})
	})
}

func (e *ClaudeAccountExecutor) ObserveDesktopSDKTelemetry(ctx context.Context, authID string, observation cliproxyexecutor.ClaudeDesktopSDKTelemetryObservation) error {
	return e.withDesktopTelemetryRuntime(authID, func(runtime *claudeAccountRuntime, auth *cliproxyauth.Auth) error {
		return runtime.executor.desktopTelemetry.RecordObservedSDKEvent(ctx, auth, claudetelemetry.ObservedSDKEvent{
			Kind: observation.Kind, SessionID: observation.SessionID, Model: observation.Model, PromptID: observation.PromptID,
			ClientRequestID: observation.ClientRequestID, SkillName: observation.SkillName, Metadata: observation.Metadata,
		})
	})
}

func (e *ClaudeAccountExecutor) ObserveDesktopPerformanceTelemetry(ctx context.Context, authID string, observation cliproxyexecutor.ClaudeDesktopPerformanceTelemetryObservation) error {
	return e.withDesktopTelemetryRuntime(authID, func(runtime *claudeAccountRuntime, auth *cliproxyauth.Auth) error {
		return runtime.executor.desktopTelemetry.RecordObservedPerformanceEvent(ctx, auth, claudetelemetry.ObservedPerformanceEvent{
			Kind: observation.Kind, SessionID: observation.SessionID, ClientRequestID: observation.ClientRequestID, Data: observation.Data,
		})
	})
}

func (e *ClaudeAccountExecutor) ObserveDesktopCrashTelemetry(ctx context.Context, authID string, observation cliproxyexecutor.ClaudeDesktopCrashTelemetryObservation) error {
	return e.withDesktopTelemetryRuntime(authID, func(runtime *claudeAccountRuntime, auth *cliproxyauth.Auth) error {
		return runtime.executor.desktopTelemetry.RecordObservedCrashAttachment(ctx, auth, claudetelemetry.ObservedCrashAttachment{
			Filename: observation.Filename, Data: observation.Data, Metadata: observation.Metadata,
		})
	})
}

func (e *ClaudeAccountExecutor) withDesktopTelemetryRuntime(authID string, observe func(*claudeAccountRuntime, *cliproxyauth.Auth) error) error {
	if e == nil || observe == nil {
		return claudesessions.ErrUnavailable
	}
	auth, errAuth := e.desktopRemoteAuth(authID)
	if errAuth != nil {
		return errAuth
	}
	runtime, errRuntime := e.acquireDesktopSessionRuntime(auth.ID)
	if errRuntime != nil {
		return errRuntime
	}
	defer runtime.release()
	if runtime.executor == nil || runtime.executor.desktopTelemetry == nil {
		return claudesessions.ErrUnavailable
	}
	return observe(runtime, auth)
}
