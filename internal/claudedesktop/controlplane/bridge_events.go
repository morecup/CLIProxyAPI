package controlplane

import (
	"errors"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type BridgeEventKind string

const (
	BridgePlaceholderUsed BridgeEventKind = "bridge_placeholder_used"
	BridgeTeardown        BridgeEventKind = "bridge_teardown"
	// BridgeStarted is delivered once per Host generation when the bridge
	// credential is minted and the worker stream is running. It is routed to
	// RequestFacts.BridgeStartObserver only, never to the lifecycle observer.
	BridgeStarted BridgeEventKind = "bridge_started"
)

// BridgeOwner identifies one Host generation, not a remote session or an HTTP
// attempt. Its observer outlives query cancellation until worker cleanup joins.
type BridgeOwner struct {
	DesktopSessionID string
	QueryID          string
	SDKSessionID     string
}

type BridgeArchiveOutcome struct {
	Status     string `json:"archive_status"`
	Credential string `json:"archive_credential,omitempty"`
	OK         bool   `json:"archive_ok"`
	HTTPStatus *int   `json:"archive_http_status,omitempty"`
	Timeout    bool   `json:"archive_timeout"`
	NoToken    bool   `json:"archive_no_token"`
}

// These are observed control outcomes only. No request body, token, remote ID,
// arbitrary event name or caller-supplied telemetry metadata crosses this API.
type BridgeEvent struct {
	Kind     BridgeEventKind
	Owner    BridgeOwner
	Model    string
	PromptID string
	At       time.Time
	Archive  *BridgeArchiveOutcome
	// Start-only dimensions: the minted credential's expires_in seconds and
	// whether the grant resumes a bridge that already carries history.
	ExpiresIn          int
	HasInitialMessages bool
}

type BridgeObserver func(BridgeEvent) error

var errMissingOAuthAccessToken = errors.New("Claude Desktop control-plane OAuth access token is missing")

func bridgeArchiveOutcome(status int, err error) BridgeArchiveOutcome {
	outcome := BridgeArchiveOutcome{Status: "network_error", Credential: "current"}
	if errors.Is(err, cliproxyauth.ErrCredentialOwnerChanged) && status == 0 {
		outcome.Status, outcome.Credential = "skipped_owner_changed", ""
	} else if errors.Is(err, errMissingOAuthAccessToken) {
		outcome.Status, outcome.NoToken = "skipped_no_token", true
	} else if status > 0 {
		outcome.HTTPStatus = &status
		outcome.OK = status < 400
		var failure *statusError
		switch {
		case errors.As(err, &failure) && failure.untrustedDevice:
			outcome.Status = "server_403_untrusted"
		case status >= 500:
			outcome.Status = "server_5xx"
		case status >= 400:
			outcome.Status = "server_4xx"
		default:
			outcome.Status = "ok"
		}
	}
	// The native timeout flag denotes the native archive budget, not arbitrary
	// context cancellation. No such production deadline is installed here.
	return outcome
}

// observeBridgeStartedLocked reports the bridge start to the dedicated start
// observer. has_initial_messages is the real grant state: a resumed bridge
// checkpoint whose sequence already advanced carries history to backfill.
func (s *sessionRuntime) observeBridgeStartedLocked() error {
	if s.bridgeStartObserver == nil || s.manager.abort.Load() {
		return nil
	}
	return s.bridgeStartObserver(BridgeEvent{
		Kind: BridgeStarted, Owner: BridgeOwner{s.desktopID, s.queryID, s.state.LocalSessionID},
		Model: s.bridgeModel, PromptID: s.bridgePromptID, At: s.manager.now().UTC(),
		ExpiresIn: s.bridgeExpiresIn, HasInitialMessages: s.bridgeCheckpoint != nil && s.bridgeCheckpoint.LastSequenceNum > 0,
	})
}

func (s *sessionRuntime) observeBridgeLocked(kind BridgeEventKind, archive *BridgeArchiveOutcome) error {
	if s.bridgeObserver == nil || s.manager.abort.Load() {
		return nil
	}
	return s.bridgeObserver(BridgeEvent{
		Kind: kind, Owner: BridgeOwner{s.desktopID, s.queryID, s.state.LocalSessionID},
		Model: s.bridgeModel, PromptID: s.bridgePromptID, At: s.manager.now().UTC(), Archive: archive,
	})
}
