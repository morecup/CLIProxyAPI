package prompt

import "strings"

const defaultSDKTranscriptRetentionDays = 30

// SDKTranscriptLeasePass is a content-free inspection of the protected
// transcript indices for durable Desktop sessions. It deliberately reports
// only aggregate health: neither transcript paths nor transcript rows leave
// the prompt package.
type SDKTranscriptLeasePass struct {
	Candidates      int
	Renewed         int
	Fresh           int
	Missing         int
	Errors          int
	Skipped         string
	RetentionDays   int
	RetentionSource string
}

// TranscriptLeasePass checks the stored transcript index for each owned SDK
// session. Scope hashing stays here so callers cannot substitute a raw
// transcript scope or learn the store's on-disk layout. The gateway has no
// mutable lease primitive, so Renewed remains zero; a readable nonempty index
// is reported as Fresh and an absent index as Missing.
func (t *Tracker) TranscriptLeasePass(accountScope string, sessionIDs []string) SDKTranscriptLeasePass {
	pass := SDKTranscriptLeasePass{RetentionDays: defaultSDKTranscriptRetentionDays, RetentionSource: "default"}
	if t == nil || strings.TrimSpace(accountScope) == "" {
		pass.Skipped = "policy_unavailable"
		return pass
	}
	t.mu.Lock()
	store := t.nativeOptions.TranscriptStore
	t.mu.Unlock()
	if store == nil {
		pass.Skipped = "policy_unavailable"
		return pass
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, rawID := range sessionIDs {
		sessionID := strings.TrimSpace(rawID)
		if sessionID == "" {
			continue
		}
		if _, duplicate := seen[sessionID]; duplicate {
			continue
		}
		seen[sessionID] = struct{}{}
		pass.Candidates++
		index, err := store.LoadTranscript(digest(accountScope, sessionID))
		if err != nil {
			pass.Errors++
			continue
		}
		if strings.TrimSpace(index.Revision) == "" && len(index.UUIDs) == 0 {
			pass.Missing++
			continue
		}
		pass.Fresh++
	}
	return pass
}
