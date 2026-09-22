package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claudedesktop"
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// CredentialSource is the live account owner, not a request token snapshot.
// Refresh must serialize with inference/automatic refresh and durably commit
// before returning. It must not acquire the retiring application runtime.
type CredentialSource interface {
	Current(*cliproxyauth.Auth) (*cliproxyauth.Auth, error)
	Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
}

func (s *sessionRuntime) useCredentialLocked(auth *cliproxyauth.Auth) error {
	if auth == nil || s.auth == nil || auth.ID != s.auth.ID || auth.Provider != s.auth.Provider || auth.ProxyURL != s.auth.ProxyURL || auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		return cliproxyauth.ErrCredentialOwnerChanged
	}
	enrollment, err := claudedesktop.ValidateTrustedDeviceEnrollment(auth.ID, auth.Metadata)
	if err != nil || enrollment.AccountUUID != s.enrollment.AccountUUID || enrollment.OrganizationUUID != s.enrollment.OrganizationUUID || enrollment.DeviceID != s.enrollment.DeviceID {
		return cliproxyauth.ErrCredentialOwnerChanged
	}
	if enrollment.State != claudedesktop.EnrollmentActive {
		return cliproxyauth.ErrCredentialOwnerChanged
	}
	s.auth = auth.Clone()
	s.enrollment = enrollment
	return nil
}

func (s *sessionRuntime) archiveOnTeardownLocked(ctx context.Context) error {
	_, err := s.archiveOnTeardownObservedLocked(ctx)
	return err
}

func (s *sessionRuntime) archiveOnTeardownObservedLocked(ctx context.Context) (BridgeArchiveOutcome, error) {
	source := s.manager.credentials
	// Native eligibility uses a wall-clock budget, independently of whether an
	// HTTP attempt is canceled. Do not install a network deadline here.
	started := s.manager.now().UnixMilli()
	remaining := func() int64 {
		return int64(s.manager.profile.TeardownArchiveBudgetMillis) - (s.manager.now().UnixMilli() - started)
	}
	code := 0
	err := s.doJSONObserved(ctx, claudeprofile.ControlEndpointSessionArchive, s.state.ArchiveSessionID, struct{}{}, nil, &code)
	outcome := bridgeArchiveOutcome(code, err)
	var status *statusError
	if source == nil || !errors.As(err, &status) || status.code != http.StatusUnauthorized || ctx.Err() != nil || remaining() < 200 {
		return outcome, err
	}
	// Native teardown retries once only for a current credential source. A
	// standalone/pinned snapshot has no source and cannot refresh another owner.
	refreshed, errRefresh := source.Refresh(ctx, s.auth.Clone())
	if errRefresh != nil {
		return outcome, errors.Join(err, errRefresh)
	}
	if errRefresh = s.useCredentialLocked(refreshed); errRefresh != nil {
		return outcome, errors.Join(err, errRefresh)
	}
	if errRefresh = ctx.Err(); errRefresh != nil {
		return outcome, errors.Join(err, errRefresh)
	}
	code = 0
	err = s.doJSONObserved(ctx, claudeprofile.ControlEndpointSessionArchive, s.state.ArchiveSessionID, struct{}{}, nil, &code)
	return bridgeArchiveOutcome(code, err), err
}
