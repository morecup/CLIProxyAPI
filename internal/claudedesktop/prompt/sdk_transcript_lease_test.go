package prompt

import (
	"errors"
	"testing"
)

type sdkTranscriptLeaseStore struct {
	indices map[string]SDKTranscriptIndex
	errs    map[string]error
}

func (s sdkTranscriptLeaseStore) LoadTranscript(scope string) (SDKTranscriptIndex, error) {
	if err := s.errs[scope]; err != nil {
		return SDKTranscriptIndex{}, err
	}
	return s.indices[scope], nil
}

func (sdkTranscriptLeaseStore) AppendTranscript(string, string, []byte) (string, error) {
	return "", errors.New("not implemented")
}

func TestSDKTranscriptLeasePassUsesOwnedScopesAndOnlyAggregateHealth(t *testing.T) {
	account, fresh, missing, unavailable := "owned-account", "fresh-session", "missing-session", "unavailable-session"
	store := sdkTranscriptLeaseStore{
		indices: map[string]SDKTranscriptIndex{digest(account, fresh): {Revision: "revision", UUIDs: []string{"row"}}},
		errs:    map[string]error{digest(account, unavailable): errors.New("synthetic storage failure")},
	}
	tracker := NewTracker(nil, SDKNativeContentOptions{TranscriptStore: store})
	pass := tracker.TranscriptLeasePass(account, []string{fresh, missing, unavailable, fresh, ""})
	if pass.Candidates != 3 || pass.Renewed != 0 || pass.Fresh != 1 || pass.Missing != 1 || pass.Errors != 1 {
		t.Fatalf("lease pass = %+v", pass)
	}
	if pass.Skipped != "" || pass.RetentionDays != 30 || pass.RetentionSource != "default" {
		t.Fatalf("lease policy = %+v", pass)
	}
}

func TestSDKTranscriptLeasePassReportsUnavailablePolicyWithoutScanning(t *testing.T) {
	tracker := NewTracker(nil)
	pass := tracker.TranscriptLeasePass("owned-account", []string{"session"})
	if pass.Skipped != "policy_unavailable" || pass.Candidates != 0 || pass.Fresh != 0 || pass.Missing != 0 || pass.Errors != 0 {
		t.Fatalf("unavailable policy pass = %+v", pass)
	}
}
