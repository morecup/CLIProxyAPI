package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	claudeprofile "github.com/router-for-me/CLIProxyAPI/v7/internal/claudedesktop/profile"
	log "github.com/sirupsen/logrus"
)

const (
	placeholderStartDelay = 15 * time.Second
	placeholderMinimumAge = 5 * time.Minute
	placeholderMaximumAge = 30 * 24 * time.Hour
	placeholderLimit      = 20
)

var placeholderID = regexp.MustCompile(`^(session|cse)_[A-Za-z0-9_-]+$`)

type processIdentity struct {
	Start string
	Dead  bool
}

type placeholderRecord struct {
	PID          int    `json:"pid"`
	ProcessStart string `json:"procStart,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

type placeholderState struct {
	Version int                          `json:"version"`
	Records map[string]placeholderRecord `json:"records"`
	Order   []string                     `json:"order,omitempty"`
}

func normalizePlaceholder(id string) string {
	if strings.HasPrefix(id, "session_") {
		return strings.TrimPrefix(id, "session_")
	}
	return strings.TrimPrefix(id, "cse_")
}

func (m *Manager) placeholderPath() string {
	return filepath.Join(m.root, "control-plane", "bridge-placeholders.json")
}

// Mutations serialize the shared account file, while each live query owns its
// own one-shot sweep. Corruption is never replaced with an empty journal.
func (m *Manager) loadPlaceholdersLocked() (placeholderState, error) {
	state := placeholderState{Version: 1, Records: make(map[string]placeholderRecord)}
	err := readProtectedState(m.placeholderPath(), &state)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil && (state.Version != 1 || state.Records == nil) {
		err = fmt.Errorf("Claude Desktop placeholder journal is invalid")
	}
	m.placeholderStoreFailed.Store(err != nil)
	if err == nil {
		state.Order = placeholderOrder(state)
		m.placeholderPending.Store(int64(len(state.Records)))
	}
	return state, err
}

func (m *Manager) savePlaceholdersLocked(state placeholderState) error {
	state.Order = placeholderOrder(state)
	err := writeProtectedState(m.placeholderPath(), state)
	m.placeholderStoreFailed.Store(err != nil)
	if err == nil {
		m.placeholderPending.Store(int64(len(state.Records)))
	}
	return err
}

// Preserve native object insertion order, including equal-millisecond cap
// ties. A legacy journal without order has no claim to an unknown ordering.
func placeholderOrder(state placeholderState) []string {
	ids := make([]string, 0, len(state.Records))
	seen := make(map[string]bool, len(state.Records))
	for _, id := range state.Order {
		if _, ok := state.Records[id]; ok && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	var missing []string
	for id := range state.Records {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return append(ids, missing...)
}

func (m *Manager) registerPlaceholder(id string) error {
	if !placeholderID.MatchString(id) {
		return fmt.Errorf("Claude Desktop placeholder session ID is invalid")
	}
	pid := os.Getpid()
	owner, err := m.processProbe(pid)
	if err != nil || owner.Dead || owner.Start == "" {
		m.placeholderStoreFailed.Store(true)
		return fmt.Errorf("Claude Desktop placeholder process ownership is unavailable")
	}
	m.placeholderMu.Lock()
	defer m.placeholderMu.Unlock()
	state, err := m.loadPlaceholdersLocked()
	if err != nil {
		return err
	}
	for previous := range state.Records {
		if normalizePlaceholder(previous) == normalizePlaceholder(id) {
			delete(state.Records, previous)
		}
	}
	state.Order = placeholderOrder(state)
	state.Records[id] = placeholderRecord{PID: pid, ProcessStart: owner.Start, CreatedAt: m.now().UnixMilli()}
	ids := append(state.Order, id)
	sort.SliceStable(ids, func(i, j int) bool {
		return state.Records[ids[i]].CreatedAt > state.Records[ids[j]].CreatedAt
	})
	for i := placeholderLimit; i < len(ids); i++ {
		delete(state.Records, ids[i])
	}
	state.Order = ids
	return m.savePlaceholdersLocked(state)
}

func (m *Manager) removePlaceholder(id string, expected *placeholderRecord) error {
	return m.removePlaceholders(map[string]*placeholderRecord{id: expected})
}

func (m *Manager) removePlaceholders(removals map[string]*placeholderRecord) error {
	m.placeholderMu.Lock()
	defer m.placeholderMu.Unlock()
	state, err := m.loadPlaceholdersLocked()
	if err != nil {
		return err
	}
	changed := false
	for id, expected := range removals {
		for previous, record := range state.Records {
			if normalizePlaceholder(previous) == normalizePlaceholder(id) && (expected == nil || *expected == record) {
				delete(state.Records, previous)
				changed = true
			}
		}
	}
	if changed {
		return m.savePlaceholdersLocked(state)
	}
	return nil
}

func (s *sessionRuntime) preparePlaceholderLocked() {
	if s.placeholderGate == nil || s.ctx.Err() != nil {
		return
	}
	if !s.placeholderRegistered {
		enabled, err := s.placeholderGate()
		if err == nil && enabled {
			err = s.manager.registerPlaceholder(s.state.RemoteSessionID)
		}
		s.placeholderRegistered = err == nil
		if err != nil {
			s.manager.placeholderStoreFailed.Store(true)
			log.WithError(err).Warn("claude desktop control-plane: placeholder registration incomplete")
		}
	}
	if !s.placeholderSweepStarted && !s.manager.disableLoops {
		s.placeholderSweepStarted = true
		s.loops.Add(1)
		go func() {
			defer s.loops.Done()
			timer := time.NewTimer(placeholderStartDelay)
			defer timer.Stop()
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
			}
			if err := s.sweepPlaceholders(); err != nil && !errors.Is(err, context.Canceled) {
				s.manager.placeholderSweepFailed.Store(true)
				log.WithError(err).Warn("claude desktop control-plane: placeholder sweep incomplete")
			}
		}()
	}
}

func (s *sessionRuntime) sweepPlaceholders() error {
	m := s.manager
	// Serialize scans to avoid duplicate GET/archive calls from several Hosts.
	// Registration and ordinary inference never acquire this lock.
	m.sweepMu.Lock()
	defer m.sweepMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.opMu.Lock()
	gate := s.placeholderGate
	current := s.state.RemoteSessionID
	client := &sessionRuntime{manager: m, auth: s.auth, enrollment: s.enrollment}
	s.opMu.Unlock()
	if gate == nil {
		return nil
	}
	enabled, err := gate()
	if err != nil || !enabled {
		return err
	}
	m.placeholderMu.Lock()
	state, err := m.loadPlaceholdersLocked()
	m.placeholderMu.Unlock()
	if err != nil {
		return err
	}
	var errs []error
	removals := make(map[string]*placeholderRecord)
	for _, id := range state.Order {
		record := state.Records[id]
		if err := s.ctx.Err(); err != nil {
			return err
		}
		age := m.now().Sub(time.UnixMilli(record.CreatedAt))
		if (age > -placeholderMinimumAge && age < placeholderMinimumAge) || normalizePlaceholder(id) == normalizePlaceholder(current) {
			continue
		}
		remove := !placeholderID.MatchString(id)
		if !remove {
			var errCheck error
			var used bool
			remove, used, errCheck = client.checkPlaceholder(s.ctx, id, record)
			if used {
				// Resolve the scanning Host's current main context at occurrence,
				// not the orphan's identity or a stale sweep-start prompt snapshot.
				s.opMu.Lock()
				errEvent := s.observeBridgeLocked(BridgePlaceholderUsed, nil)
				s.opMu.Unlock()
				errCheck = errors.Join(errCheck, errEvent)
			}
			if errCheck != nil {
				errs = append(errs, errCheck)
			}
			// Expiry removes local bookkeeping only, never authorizes archive.
			remove = remove || age > placeholderMaximumAge
		}
		if remove {
			removals[id] = &record
		}
	}
	if len(removals) > 0 {
		if errRemove := m.removePlaceholders(removals); errRemove != nil {
			errs = append(errs, errRemove)
		}
	}
	err = errors.Join(errs...)
	m.placeholderSweepFailed.Store(err != nil)
	return err
}

func (s *sessionRuntime) checkPlaceholder(ctx context.Context, id string, record placeholderRecord) (remove, used bool, err error) {
	if record.PID <= 1 {
		return false, false, nil
	}
	owner, err := s.manager.processProbe(record.PID)
	if err != nil {
		return false, false, err
	}
	if !owner.Dead && (record.ProcessStart == "" || owner.Start == "" || owner.Start == record.ProcessStart) {
		return false, false, nil
	}
	// Use the pinned compatibility CRUD route; never infer a route from an ID.
	archiveID := "session_" + normalizePlaceholder(id)
	var response struct {
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	err = s.doJSON(ctx, claudeprofile.ControlEndpointSessionRead, archiveID, nil, &response)
	var status *statusError
	if errors.As(err, &status) && status.code == 404 {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if response.CreatedAt == "" || response.UpdatedAt == "" {
		return false, false, nil
	}
	if response.CreatedAt != response.UpdatedAt {
		s.manager.placeholderUsed.Add(1)
		return true, true, nil
	}
	err = s.doJSON(ctx, claudeprofile.ControlEndpointSessionArchive, archiveID, struct{}{}, nil)
	if archiveTerminal(err) {
		return true, false, nil
	}
	return false, false, err
}

func archiveTerminal(err error) bool {
	if err == nil {
		return true
	}
	var status *statusError
	return errors.As(err, &status) && !status.untrustedDevice && status.code > 0 && status.code < 500 && status.code != 401 && status.code != 408 && status.code != 429
}

func isUntrustedDevice(payload []byte) bool {
	var response map[string]json.RawMessage
	if json.Unmarshal(payload, &response) != nil {
		return false
	}
	var detail map[string]json.RawMessage
	_ = json.Unmarshal(response["error"], &detail)
	if raw, exists := detail["resource"]; exists {
		var resource string
		return json.Unmarshal(raw, &resource) == nil && resource == "untrusted_device"
	}
	var message string
	if json.Unmarshal(response["message"], &message) == nil && string(response["message"]) != "null" {
		return strings.Contains(message, "trusted device")
	}
	return json.Unmarshal(detail["message"], &message) == nil && strings.Contains(message, "trusted device")
}
