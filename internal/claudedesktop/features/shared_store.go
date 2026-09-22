package features

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// SharedStore serializes cache updates from hosts in one account runtime. Each
// view retains its own CAS revision, while writes advance the runtime's known
// disk revision. A write from an external runtime still fails the underlying
// CAS; this is not an unconditional overwrite or cross-account cache.
type SharedStore struct {
	mu     sync.Mutex
	base   Store
	states map[string]sharedCacheState
}

type sharedCacheState struct {
	payload  []byte
	revision string
}

type sharedStoreView struct {
	owner     *SharedStore
	revisions map[string]string
}

func NewSharedStore(base Store) *SharedStore {
	return &SharedStore{base: base, states: make(map[string]sharedCacheState)}
}

func (s *SharedStore) View() Store {
	return &sharedStoreView{owner: s, revisions: make(map[string]string)}
}

func (v *sharedStoreView) Load(scope string) ([]byte, string, error) {
	s := v.owner
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.states[scope]
	if s.base != nil {
		payload, revision, err := s.base.Load(scope)
		if err != nil {
			return nil, revision, err
		}
		// Once a runtime has observed a disk revision, a later external change
		// cannot silently authorize this older runtime's existing host writers.
		if known, ok := s.states[scope]; ok && known.revision != revision {
			return nil, revision, fmt.Errorf("Claude Desktop feature cache changed outside its runtime")
		}
		current = sharedCacheState{payload: bytes.Clone(payload), revision: revision}
		s.states[scope] = current
	}
	v.revisions[scope] = current.revision
	return bytes.Clone(current.payload), current.revision, nil
}

func (v *sharedStoreView) Save(scope, previous string, payload []byte) (string, error) {
	s := v.owner
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision, loaded := v.revisions[scope]; !loaded || revision != previous {
		return "", fmt.Errorf("Claude Desktop feature cache view is stale")
	}
	current := s.states[scope]
	var revision string
	if s.base != nil {
		var err error
		revision, err = s.base.Save(scope, current.revision, payload)
		if err != nil {
			return "", err
		}
	} else {
		digest := sha256.Sum256(payload)
		revision = hex.EncodeToString(digest[:])
	}
	s.states[scope] = sharedCacheState{payload: bytes.Clone(payload), revision: revision}
	v.revisions[scope] = revision
	return revision, nil
}
