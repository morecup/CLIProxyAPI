package telemetry

import (
	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
)

// Only a successful, observed compact response may supply summary content.
// The response observer retains the bounded native-selected text until completion;
// FinishSuccess reduces it to a version-bound fingerprint and erases the text.
func (s *RequestSpan) observeSDKCompactionResponse(payload []byte) {
	if s == nil || s.manager == nil || s.sdkWorker == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || !s.requestObserved || !s.responseObserved || s.facts.Role != claudeprofile.RoleCompaction || s.responseStatus < 200 || s.responseStatus >= 300 {
		return
	}
	s.compactionResponse.ObserveJSON(payload)
}
