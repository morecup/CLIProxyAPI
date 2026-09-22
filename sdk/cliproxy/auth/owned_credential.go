package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"time"
)

var ErrCredentialOwnerChanged = errors.New("credential owner changed")

var nextCredentialGeneration atomic.Uint64

// CredentialRefresh performs acquisition only. It must not enter or synchronize
// an application runtime: the caller may already be retiring that runtime.
type CredentialRefresh func(context.Context, *Auth) (*Auth, error)

// CurrentOwnedCredential resolves a live credential without letting an old
// application generation borrow a replacement executor's account or binding.
func (m *Manager) CurrentOwnedCredential(owner ProviderExecutor, expected *Auth) (*Auth, error) {
	if m == nil || owner == nil || expected == nil {
		return nil, ErrCredentialOwnerChanged
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentOwnedCredentialLocked(owner, expected)
}

func (m *Manager) currentOwnedCredentialLocked(owner ProviderExecutor, expected *Auth) (*Auth, error) {
	current := m.auths[expected.ID]
	if current == nil || m.executors[executorKeyFromAuth(current)] != owner || !sameCredentialRefreshOwner(current, expected) {
		return nil, ErrCredentialOwnerChanged
	}
	return cloneCredentialSnapshot(current)
}

// RefreshOwnedCredential shares the request/automatic refresh lock but does not
// call Update/SyncAuth, which can wait for the very runtime requesting cleanup.
// Only credential fields are committed. Persistence failure is returned, and a
// concurrent remove, disable, rebind or executor replacement cannot be undone.
func (m *Manager) RefreshOwnedCredential(ctx context.Context, owner ProviderExecutor, expected *Auth, refresh CredentialRefresh) (*Auth, error) {
	if m == nil || owner == nil || expected == nil || refresh == nil {
		return nil, ErrCredentialOwnerChanged
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lockValue, _ := m.refreshLocks.LoadOrStore(expected.ID, &authRefreshLock{})
	lock := lockValue.(*authRefreshLock)
	if err := lock.acquire(ctx); err != nil {
		return nil, err
	}
	defer lock.release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := m.CurrentOwnedCredential(owner, expected)
	if err != nil {
		return nil, err
	}
	if token := authAccessToken(current); token != "" && token != authAccessToken(expected) {
		return current, nil
	}
	homeAuthority := m.HomeEnabled()
	if !authHasRefreshCredential(current) && !homeAuthority {
		return nil, errors.New("credential refresh token is missing")
	}
	refreshInput, err := cloneCredentialSnapshot(current)
	if err != nil {
		return nil, err
	}
	updated, err := refresh(ctx, refreshInput)
	if err != nil {
		return nil, err
	}
	updated, err = bindOwnedRefreshResult(current, updated, homeAuthority)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.HomeEnabled() != homeAuthority {
		m.mu.Unlock()
		return nil, ErrCredentialOwnerChanged
	}
	latest, err := m.currentOwnedCredentialLocked(owner, current)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if authAccessToken(latest) != authAccessToken(current) || authMetadataString(latest, "refresh_token") != authMetadataString(current, "refresh_token") || authMetadataString(latest, "refreshToken") != authMetadataString(current, "refreshToken") {
		m.mu.Unlock()
		return nil, ErrCredentialOwnerChanged
	}
	for _, key := range refreshCredentialKeys {
		if value, ok := updated.Metadata[key]; ok {
			latest.Metadata[key] = value
		} else {
			delete(latest.Metadata, key)
		}
	}
	if previous := authAccessToken(current); previous != "" && latest.Attributes[AttributeAPIKey] == previous {
		latest.Attributes[AttributeAPIKey] = authAccessToken(updated)
	}
	latest.LastRefreshedAt = time.Now()
	latest.UpdatedAt = latest.LastRefreshedAt
	latest.NextRefreshAfter = time.Time{}
	// Serialize the durable write and publication with credential lifecycle
	// mutations. OAuth network acquisition above never holds the registry lock.
	if err = m.persist(ctx, latest); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.auths[latest.ID] = latest
	if m.scheduler != nil {
		m.scheduler.upsertAuth(latest.Clone())
	}
	m.mu.Unlock()
	m.queueRefreshReschedule(latest.ID)
	return latest.Clone(), nil
}

var refreshCredentialKeys = [...]string{"access_token", "accessToken", "refresh_token", "refreshToken", "expired", "expires_at", "last_refresh"}

func bindOwnedRefreshResult(current, updated *Auth, homeAuthority bool) (*Auth, error) {
	if updated == nil {
		return nil, errors.New("credential refresh returned no auth")
	}
	bound, err := cloneCredentialSnapshot(updated)
	if err != nil {
		return nil, err
	}
	// Home returns JSON, which cannot carry private registration generations or
	// json:"-" local fields. Restore only those omissions from this acquisition's
	// owner; the serialized identity and binding must still match in full.
	if bound.credentialGeneration == 0 {
		bound.credentialGeneration = current.credentialGeneration
		if bound.FileName == "" {
			bound.FileName = current.FileName
		}
		if bound.Index == "" {
			bound.Index = current.Index
		}
		if homeAuthority {
			// The remote store location is not this process's persistence target.
			// Preserve only storage-local annotations; proxy and protocol binding
			// attributes still have to match the acquired owner exactly.
			if bound.Attributes == nil && len(current.Attributes) != 0 {
				bound.Attributes = make(map[string]string)
			}
			for _, key := range []string{AttributePath, AttributeSource, AttributeSourceBackend} {
				if value, ok := current.Attributes[key]; ok {
					bound.Attributes[key] = value
				} else {
					delete(bound.Attributes, key)
				}
			}
			if len(bound.Attributes) == 0 && current.Attributes == nil {
				bound.Attributes = nil
			}
		}
	}
	if homeAuthority && !authHasRefreshCredential(bound) {
		if !authHasRefreshCredential(current) {
			return nil, errors.New("credential refresh returned no refresh credential")
		}
		if bound.Metadata == nil {
			bound.Metadata = make(map[string]any)
		}
		if token := authMetadataString(current, "refresh_token"); token != "" {
			bound.Metadata["refresh_token"] = token
		} else if token := authMetadataString(current, "refreshToken"); token != "" {
			bound.Metadata["refreshToken"] = token
		}
	}
	if !sameCredentialRefreshOwner(bound, current) || authAccessToken(bound) == "" {
		return nil, errors.New("credential refresh changed owner or returned no access token")
	}
	return bound, nil
}

func cloneCredentialSnapshot(auth *Auth) (*Auth, error) {
	cloned := auth.Clone()
	encoded, err := json.Marshal(auth.Metadata)
	if err != nil {
		return nil, errors.New("credential metadata cannot be snapshotted")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&cloned.Metadata); err != nil {
		return nil, errors.New("credential metadata snapshot is invalid")
	}
	return cloned, nil
}

func sameCredentialRefreshOwner(a, b *Auth) bool {
	if a == nil || b == nil || a.ID == "" || a.ID != b.ID || a.Index != b.Index || a.credentialGeneration != b.credentialGeneration || a.Provider != b.Provider || a.ProxyURL != b.ProxyURL || a.FileName != b.FileName || a.Disabled || b.Disabled || a.Status == StatusDisabled || b.Status == StatusDisabled {
		return false
	}
	attributes := func(auth *Auth) map[string]string {
		values := auth.Clone().Attributes
		if token := values[AttributeAPIKey]; token != "" && (token == authAccessToken(a) || token == authAccessToken(b)) {
			delete(values, AttributeAPIKey)
		}
		return values
	}
	if !reflect.DeepEqual(attributes(a), attributes(b)) {
		return false
	}
	project := func(auth *Auth) ([]byte, error) {
		// Normalize nested structs/maps to the same key order, preserving large
		// integer metadata exactly instead of round-tripping through float64.
		cloned, err := cloneCredentialSnapshot(auth)
		if err != nil {
			return nil, err
		}
		metadata := cloned.Metadata
		for _, key := range refreshCredentialKeys {
			delete(metadata, key)
		}
		// The hydrated Desktop fields below are compared, not their randomized
		// at-rest envelope, which changes on every successful durable write.
		if auth.Provider == "claude" && authMetadataString(auth, "claude_desktop_trusted_device_token") != "" {
			delete(metadata, "claude_desktop_credentials")
		}
		return json.Marshal(metadata)
	}
	one, errOne := project(a)
	two, errTwo := project(b)
	return errOne == nil && errTwo == nil && string(one) == string(two)
}
