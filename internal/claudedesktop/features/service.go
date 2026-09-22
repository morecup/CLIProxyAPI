// Package features owns the native SDK-host feature cache and experiment reads.
// Durable evaluated facts and process-local exposure state have separate lives.
package features

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"sync"
)

var ErrStale = errors.New("Claude Desktop feature response belongs to a stale generation")

type Store interface {
	Load(scope string) ([]byte, string, error)
	Save(scope, previousRevision string, payload []byte) (string, error)
}

type Experiment struct {
	ID        string          `json:"id"`
	Variation json.RawMessage `json:"variation"`
	Value     json.RawMessage `json:"value"`
}

type Cache struct {
	Version     int                        `json:"version"`
	Values      map[string]json.RawMessage `json:"values"`
	Sources     map[string]json.RawMessage `json:"sources,omitempty"`
	Experiments map[string]Experiment      `json:"experiments,omitempty"`
}

// Exposure is owned by a real feature read, not a model HTTP outcome. It has
// deliberately no model, prompt, system, beta, process or caller-header fields.
type Exposure struct {
	SessionID    string
	FeatureID    string
	ExperimentID string
	VariationID  float64
}

// Sink synchronously acknowledges acceptance, not eventual HTTP delivery. It
// must not call back into Service. The first-party feature policy is evaluated
// inside Service, including its own recursive experiment exposure.
type Sink func(Exposure) (bool, error)

type Ticket struct {
	owner      *Service
	scope      string
	generation uint64
}

type Value struct {
	Raw    json.RawMessage
	Source string
}

type Service struct {
	mu                 sync.Mutex
	store              Store
	scope              string
	credentialRevision string // In-memory only; never written into the cache.
	generation         uint64
	cache              Cache
	disk               Cache
	revision           string
	fresh              bool
	logged             map[string]bool
	pending            map[string]Experiment
	pendingOrder       []string
	loadErr            error
	saveErr            error
	enabled            bool
	diskWhileDisabled  bool
	closed             bool
}

func New(store Store) *Service {
	return &Service{store: store, enabled: true, logged: make(map[string]bool), pending: make(map[string]Experiment)}
}

// Bind advances native auth generations without persisting token revisions or
// resurrecting logged exposures after process reconstruction. A host may keep
// its logged set across same-account token rotation, but not an identity change.
func (s *Service) Bind(scope, credentialRevision string) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || scope == "" {
		return Ticket{}, ErrStale
	}
	if s.scope == scope && s.credentialRevision == credentialRevision {
		return s.ticketLocked(), errors.Join(s.loadErr, s.saveErr)
	}
	sameScope := s.scope == scope
	s.scope, s.credentialRevision = scope, credentialRevision
	s.generation++
	s.fresh = false
	s.cache = Cache{Version: 1}
	if !sameScope {
		s.logged = make(map[string]bool)
		s.disk, s.revision, s.loadErr, s.saveErr = Cache{Version: 1}, "", nil, nil
		if s.store != nil {
			payload, revision, err := s.store.Load(scope)
			s.revision, s.loadErr = revision, err
			if err == nil && len(payload) > 0 {
				if json.Unmarshal(payload, &s.disk) != nil || !validCache(s.disk) {
					s.disk = Cache{Version: 1}
					s.loadErr = fmt.Errorf("Claude Desktop feature cache is invalid")
				}
			}
		}
	}
	return s.ticketLocked(), errors.Join(s.loadErr, s.saveErr)
}

func (s *Service) ticketLocked() Ticket {
	return Ticket{owner: s, scope: s.scope, generation: s.generation}
}

func (s *Service) currentLocked(t Ticket) bool {
	return !s.closed && t.owner == s && t.scope == s.scope && t.generation == s.generation
}

// ImportLegacy keeps earlier value-only caches as disk facts. It cannot invent
// experiment/source metadata and never replaces a current protected cache.
func (s *Service) ImportLegacy(t Ticket, values map[string]json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentLocked(t) || s.fresh || s.revision != "" || len(s.disk.Values) != 0 || s.loadErr != nil {
		return
	}
	s.disk.Values = cloneValues(values)
}

// Observe accepts only the generation that dispatched the actual evaluation.
// A valueless payload leaves the last accepted map intact. Deferred exposure
// precedes cache persistence; both precede the caller's refresh notification.
func (s *Service) Observe(t Ticket, payload []byte, session string, sink Sink) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentLocked(t) {
		return false, ErrStale
	}
	cache, err := normalize(payload)
	if err != nil {
		return false, err
	}
	if !s.enabled {
		return false, nil
	}
	if len(cache.Values) == 0 {
		return false, errors.Join(s.loadErr, s.saveErr)
	}
	s.cache, s.fresh = cache, true
	var joined error
	for i := 0; i < len(s.pendingOrder); i++ {
		name := s.pendingOrder[i]
		deferred, exists := s.pending[name]
		current, present := cache.Experiments[name]
		if exists && present && deferred.ID == current.ID && equalJSON(deferred.Variation, current.Variation) {
			joined = errors.Join(joined, s.exposeLocked(session, name, sink))
		}
	}
	s.pending, s.pendingOrder = make(map[string]Experiment), nil
	s.disk = cache
	if s.loadErr == nil && s.store != nil {
		encoded, errMarshal := json.Marshal(cache)
		if errMarshal != nil {
			s.saveErr = errMarshal
		} else {
			var revision string
			revision, s.saveErr = s.store.Save(s.scope, s.revision, encoded)
			if s.saveErr == nil {
				s.revision = revision
			}
		}
	}
	return true, errors.Join(joined, s.loadErr, s.saveErr)
}

// PersistenceError is independent of transient response or exposure failures.
// A healthy live evaluation cannot erase unreadable durable evidence.
func (s *Service) PersistenceError(t Ticket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentLocked(t) {
		return ErrStale
	}
	return errors.Join(s.loadErr, s.saveErr)
}

func (s *Service) Lookup(t Ticket, session, name string, fallback json.RawMessage, sink Sink) (Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentLocked(t) {
		return Value{Raw: bytes.Clone(fallback), Source: "stale"}, ErrStale
	}
	value, err := s.lookupLocked(session, name, fallback, sink)
	return value, errors.Join(err, s.loadErr, s.saveErr)
}

// Peek serves an unowned compatibility request without fabricating a native
// feature read, deferred exposure or acknowledgment on some other query.
func (s *Service) Peek(t Ticket, name string, fallback json.RawMessage) (Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentLocked(t) {
		return Value{Raw: bytes.Clone(fallback), Source: "stale"}, ErrStale
	}
	if !s.enabled && !s.diskWhileDisabled {
		return Value{Raw: bytes.Clone(fallback), Source: "disabled"}, nil
	}
	raw, exists := s.cache.Values[name]
	if !s.fresh || !exists {
		raw, exists = s.disk.Values[name]
	}
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = fallback
	}
	return Value{Raw: bytes.Clone(raw), Source: "unowned-cache"}, errors.Join(s.loadErr, s.saveErr)
}

func (s *Service) lookupLocked(session, name string, fallback json.RawMessage, sink Sink) (Value, error) {
	if !s.enabled && !s.diskWhileDisabled {
		return Value{Raw: bytes.Clone(fallback), Source: "disabled"}, nil
	}
	var err error
	if s.fresh {
		err = s.exposeLocked(session, name, sink)
	}
	raw, exists := s.cache.Values[name]
	source := "payload"
	if !s.fresh || !exists {
		raw, exists = s.disk.Values[name]
		source = "disk"
		if exists {
			if experiment, ok := s.disk.Experiments[name]; ok && equalJSON(experiment.Value, raw) {
				if _, exists := s.pending[name]; !exists {
					s.pendingOrder = append(s.pendingOrder, name)
				}
				s.pending[name] = cloneExperiment(experiment)
			}
		}
	}
	if exists {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			raw = fallback
		}
		return Value{Raw: bytes.Clone(raw), Source: source}, err
	}
	return Value{Raw: bytes.Clone(fallback), Source: "fallback"}, err
}

func (s *Service) exposeLocked(session, name string, sink Sink) error {
	experiment, exists := s.cache.Experiments[name]
	if !exists || s.logged[name] {
		return nil
	}
	// Mark before checking the first-party policy. If that policy is itself an
	// experiment, its recursive read terminates at the already claimed feature.
	s.logged[name] = true
	policy, errPolicy := s.lookupLocked(session, "tengu_frond_boric", json.RawMessage(`{}`), sink)
	var flags map[string]json.RawMessage
	_ = json.Unmarshal(policy.Raw, &flags)
	if bytes.Equal(bytes.TrimSpace(flags["firstParty"]), []byte("true")) {
		return errPolicy
	}
	if sink == nil {
		delete(s.logged, name)
		return errPolicy
	}
	variation, _ := number(experiment.Variation)
	accepted, err := sink(Exposure{SessionID: session, FeatureID: name, ExperimentID: experiment.ID, VariationID: variation})
	if !accepted {
		delete(s.logged, name)
	}
	return errors.Join(errPolicy, err)
}

func (s *Service) SetEnabled(enabled, diskWhileDisabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled, s.diskWhileDisabled = enabled, diskWhileDisabled
}

// Reset invalidates in-flight refreshes while independently preserving the
// native host's deferred and acknowledged reads. Disk facts have a separate
// lifetime and are not erased by an auth/client reset.
func (s *Service) Reset(preservePending, preserveLogged bool) Ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	s.fresh, s.cache = false, Cache{Version: 1}
	if !preservePending {
		s.pending, s.pendingOrder = make(map[string]Experiment), nil
	}
	if !preserveLogged {
		s.logged = make(map[string]bool)
	}
	return s.ticketLocked()
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.generation++
}

func Truthy(raw json.RawMessage) bool {
	value, err := decoded(raw)
	if err != nil || value == nil {
		return false
	}
	switch value := value.(type) {
	case bool:
		return value
	case string:
		return value != ""
	case float64:
		return value != 0 && !math.IsNaN(value)
	default:
		return true
	}
}

func normalize(payload []byte) (Cache, error) {
	if !json.Valid(payload) {
		return Cache{}, fmt.Errorf("Claude Desktop feature response is invalid")
	}
	cache := Cache{Version: 1, Values: make(map[string]json.RawMessage), Sources: make(map[string]json.RawMessage), Experiments: make(map[string]Experiment)}
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil || root == nil {
		return cache, nil
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(root["features"], &entries) != nil {
		var array []json.RawMessage
		if json.Unmarshal(root["features"], &array) != nil {
			return cache, nil
		}
		entries = make(map[string]json.RawMessage, len(array))
		for i, item := range array {
			entries[strconv.Itoa(i)] = item
		}
	}
	for name, raw := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			continue
		}
		value, exists := fields["value"]
		if !exists {
			value, exists = fields["defaultValue"]
		}
		if !exists {
			continue
		}
		cache.Values[name] = bytes.Clone(value)
		if source, exists := fields["source"]; exists {
			cache.Sources[name] = bytes.Clone(source)
		}
		var source string
		if json.Unmarshal(fields["source"], &source) != nil || source != "experiment" {
			continue
		}
		var experiment, result map[string]json.RawMessage
		if json.Unmarshal(fields["experiment"], &experiment) != nil || json.Unmarshal(fields["experimentResult"], &result) != nil {
			continue
		}
		var id string
		if len(experiment["key"]) == 0 || bytes.Equal(bytes.TrimSpace(experiment["key"]), []byte("null")) || json.Unmarshal(experiment["key"], &id) != nil {
			continue
		}
		if _, ok := number(result["variationId"]); ok {
			cache.Experiments[name] = Experiment{ID: id, Variation: bytes.Clone(result["variationId"]), Value: bytes.Clone(value)}
		}
	}
	return cache, nil
}

func validCache(cache Cache) bool {
	if cache.Version != 1 {
		return false
	}
	for _, value := range cache.Values {
		if !json.Valid(value) {
			return false
		}
	}
	for _, experiment := range cache.Experiments {
		if _, ok := number(experiment.Variation); !ok || !json.Valid(experiment.Value) {
			return false
		}
	}
	return true
}

func number(raw json.RawMessage) (float64, bool) {
	if !json.Valid(raw) {
		return 0, false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return 0, false
	}
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	valueFloat, err := strconv.ParseFloat(string(n), 64)
	return valueFloat, err == nil || errors.Is(err, strconv.ErrRange)
}

func decoded(raw json.RawMessage) (any, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid feature JSON value")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var normalizeNumber func(any) any
	normalizeNumber = func(value any) any {
		switch value := value.(type) {
		case json.Number:
			v, _ := strconv.ParseFloat(string(value), 64)
			return v
		case []any:
			for i := range value {
				value[i] = normalizeNumber(value[i])
			}
		case map[string]any:
			for key := range value {
				value[key] = normalizeNumber(value[key])
			}
		}
		return value
	}
	return normalizeNumber(value), nil
}

func equalJSON(a, b json.RawMessage) bool {
	left, errLeft := decoded(a)
	right, errRight := decoded(b)
	return errLeft == nil && errRight == nil && reflect.DeepEqual(left, right)
}

func cloneExperiment(value Experiment) Experiment {
	value.Value, value.Variation = bytes.Clone(value.Value), bytes.Clone(value.Variation)
	return value
}

func cloneValues(values map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		result[key] = bytes.Clone(value)
	}
	return result
}
