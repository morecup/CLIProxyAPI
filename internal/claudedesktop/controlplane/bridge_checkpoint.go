package controlplane

import claudesessions "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/sessions"

// BridgeTranscriptSink is an owned asynchronous transcript producer. A disk
// failure must remain observable without vetoing an established remote bridge.
// The failure observer belongs to this exact query, including delayed appends.
type BridgeTranscriptSink func(claudesessions.BridgeState, func(error)) error

// The pinned SDK saves on attachment and host cleanup (and on dialog-kind
// declarations when that consumer runs). Stream frames only advance the live
// cursor. This metadata lives in the Desktop record, not an obsolete query file.
func (s *sessionRuntime) checkpointBridgeLocked() error {
	if s.bridgeRecord == nil {
		return nil
	}
	if s.manager.credentials != nil {
		current, err := s.manager.credentials.Current(s.auth.Clone())
		if err != nil {
			return err
		}
		if err := s.useCredentialLocked(current); err != nil {
			return err
		}
	}
	value := claudesessions.BridgeState{}
	if s.bridgeCheckpoint != nil {
		value = *s.bridgeCheckpoint
	}
	value.SessionID, value.LastSequenceNum = s.state.RemoteSessionID, s.lastSequence.Load()
	value.OwnerAccountUUID, value.OwnerOrganizationUUID = s.enrollment.AccountUUID, s.enrollment.OrganizationUUID
	if err := s.bridgeRecord.Save(value); err != nil {
		return err
	}
	s.bridgeCheckpoint = &value
	if s.bridgeTranscript != nil {
		observe := func(err error) {
			if err != nil {
				s.bridgeTranscriptFailed.Store(true)
			}
		}
		observe(s.bridgeTranscript(value, observe))
	}
	return nil
}
