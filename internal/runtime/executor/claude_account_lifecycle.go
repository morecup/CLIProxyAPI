package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	claudecontrol "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/controlplane"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

const (
	claudeDesktopAppLifecycleVersion = 1
	claudeDesktopAppRunning          = "running"
	claudeDesktopAppStopped          = "stopped"
	claudeDesktopAppQuarantined      = "quarantined"
	claudeDesktopExitClean           = "clean"
	claudeDesktopExitUnclean         = "unclean"
)

var errClaudeDesktopRuntimeDraining = errors.New("previous Claude Desktop application lifetime is still draining")

type claudeDesktopAppLifecycle struct {
	Version              int                   `json:"version"`
	AuthIDHash           string                `json:"auth_id_hash"`
	State                string                `json:"state"`
	Revision             string                `json:"revision"`
	AppSessionID         string                `json:"app_session_id"`
	Generation           uint64                `json:"generation"`
	StartedAt            string                `json:"started_at"`
	StoppedAt            string                `json:"stopped_at,omitempty"`
	StopReason           string                `json:"stop_reason,omitempty"`
	PreviousState        string                `json:"previous_state,omitempty"`
	PreviousRevision     string                `json:"previous_revision,omitempty"`
	PreviousAppSessionID string                `json:"previous_app_session_id,omitempty"`
	PreviousExit         string                `json:"previous_exit,omitempty"`
	RecoveredUnclean     bool                  `json:"recovered_unclean_exit,omitempty"`
	UpdatedAt            string                `json:"updated_at"`
	ControlPlane         *claudecontrol.Status `json:"control_plane,omitempty"`
}

func prepareClaudeDesktopAppLifecycle(instanceDirectory, authIDHash, revision string, now time.Time) (claudeDesktopAppLifecycle, error) {
	path := filepath.Join(instanceDirectory, "app-lifecycle.json")
	previous, found, errRead := readClaudeDesktopAppLifecycle(path, authIDHash)
	if errRead != nil {
		return claudeDesktopAppLifecycle{}, errRead
	}
	generation := uint64(1)
	previousExit := ""
	recoveredUnclean := false
	if found {
		generation = previous.Generation + 1
		switch previous.State {
		case claudeDesktopAppStopped:
			previousExit = claudeDesktopExitClean
		case claudeDesktopAppRunning:
			previousExit = claudeDesktopExitUnclean
			recoveredUnclean = true
		case claudeDesktopAppQuarantined:
			previousExit = claudeDesktopAppQuarantined
		}
	}
	startedAt := now.UTC().Format(time.RFC3339Nano)
	return claudeDesktopAppLifecycle{
		Version:              claudeDesktopAppLifecycleVersion,
		AuthIDHash:           strings.TrimSpace(authIDHash),
		State:                claudeDesktopAppRunning,
		Revision:             strings.TrimSpace(revision),
		AppSessionID:         uuid.NewString(),
		Generation:           generation,
		StartedAt:            startedAt,
		PreviousState:        previous.State,
		PreviousRevision:     previous.Revision,
		PreviousAppSessionID: previous.AppSessionID,
		PreviousExit:         previousExit,
		RecoveredUnclean:     recoveredUnclean,
		UpdatedAt:            startedAt,
	}, nil
}

func readClaudeDesktopAppLifecycle(path, authIDHash string) (claudeDesktopAppLifecycle, bool, error) {
	payload, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return claudeDesktopAppLifecycle{}, false, nil
		}
		return claudeDesktopAppLifecycle{}, false, fmt.Errorf("read Claude Desktop app lifecycle: %w", errRead)
	}
	var lifecycle claudeDesktopAppLifecycle
	if errDecode := json.Unmarshal(payload, &lifecycle); errDecode != nil {
		return claudeDesktopAppLifecycle{}, false, fmt.Errorf("decode Claude Desktop app lifecycle: %w", errDecode)
	}
	if lifecycle.Version != claudeDesktopAppLifecycleVersion {
		return claudeDesktopAppLifecycle{}, false, fmt.Errorf("unsupported Claude Desktop app lifecycle version %d", lifecycle.Version)
	}
	if lifecycle.AuthIDHash != strings.TrimSpace(authIDHash) {
		return claudeDesktopAppLifecycle{}, false, fmt.Errorf("Claude Desktop app lifecycle does not match account partition")
	}
	if strings.TrimSpace(lifecycle.Revision) == "" || strings.TrimSpace(lifecycle.AppSessionID) == "" || lifecycle.Generation == 0 {
		return claudeDesktopAppLifecycle{}, false, fmt.Errorf("Claude Desktop app lifecycle is incomplete")
	}
	switch lifecycle.State {
	case claudeDesktopAppRunning, claudeDesktopAppStopped, claudeDesktopAppQuarantined:
	default:
		return claudeDesktopAppLifecycle{}, false, fmt.Errorf("Claude Desktop app lifecycle state is %q", lifecycle.State)
	}
	return lifecycle, true, nil
}

func persistClaudeDesktopAppLifecycle(path string, lifecycle claudeDesktopAppLifecycle) error {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create Claude Desktop app lifecycle directory: %w", errMkdir)
	}
	payload, errMarshal := json.MarshalIndent(lifecycle, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal Claude Desktop app lifecycle: %w", errMarshal)
	}
	payload = append(payload, '\n')
	if existing, errRead := os.ReadFile(path); errRead == nil && string(existing) == string(payload) {
		return nil
	}
	if errWrite := helps.AtomicWriteFile(path, payload, 0o600); errWrite != nil {
		return fmt.Errorf("persist Claude Desktop app lifecycle: %w", errWrite)
	}
	return nil
}

func finishClaudeDesktopAppLifecycle(path, authIDHash, appSessionID, state, reason string, now time.Time, control ...claudecontrol.Status) error {
	lifecycle, found, errRead := readClaudeDesktopAppLifecycle(path, authIDHash)
	if errRead != nil || !found {
		return errRead
	}
	if lifecycle.AppSessionID != strings.TrimSpace(appSessionID) {
		return nil
	}
	switch state {
	case claudeDesktopAppStopped, claudeDesktopAppQuarantined:
	default:
		return fmt.Errorf("unsupported Claude Desktop app terminal state %q", state)
	}
	stoppedAt := now.UTC().Format(time.RFC3339Nano)
	lifecycle.State = state
	lifecycle.StoppedAt = stoppedAt
	lifecycle.StopReason = strings.TrimSpace(reason)
	lifecycle.UpdatedAt = stoppedAt
	if len(control) != 0 {
		// Only the joined application's final, secret-free observation is
		// persisted. Never merge counters from a different query or generation.
		final := control[0]
		lifecycle.ControlPlane = &final
	}
	return persistClaudeDesktopAppLifecycle(path, lifecycle)
}

func claudeDesktopAppSessionHash(appSessionID string) string {
	digest := sha256.Sum256([]byte("claude-desktop-app-session-v1\x00" + strings.TrimSpace(appSessionID)))
	return hex.EncodeToString(digest[:12])
}
