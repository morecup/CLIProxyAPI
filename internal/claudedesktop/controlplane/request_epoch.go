package controlplane

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// bindWorkerEpochLocked binds the payload after any bridge renewal, including
// a renewal caused by a 401/403. A struct assembled before renewal can contain
// the old epoch. Preserve JSON key order and nested event IDs while rebinding
// only this transport-owned field; never mutate the caller's object.
func (s *sessionRuntime) bindWorkerEpochLocked(body any) (any, error) {
	if body == nil {
		return nil, nil
	}
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal Claude Desktop worker request: %w", errMarshal)
	}
	if !gjson.GetBytes(encoded, "worker_epoch").Exists() {
		return nil, fmt.Errorf("Claude Desktop worker request has no worker_epoch")
	}
	epoch, errEpoch := s.workerEpochLocked()
	if errEpoch != nil {
		return nil, errEpoch
	}
	updated, errUpdate := sjson.SetBytes(encoded, "worker_epoch", epoch)
	if errUpdate != nil {
		return nil, fmt.Errorf("bind Claude Desktop worker request epoch: %w", errUpdate)
	}
	return json.RawMessage(updated), nil
}
