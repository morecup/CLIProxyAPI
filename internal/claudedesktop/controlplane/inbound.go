package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// InboundConsumer is a query-owned execution port. Its callbacks receive only
// the admitted payload; envelope headers cannot become inference headers.
type InboundConsumer struct {
	User     func(context.Context, json.RawMessage) error
	Control  func(context.Context, json.RawMessage) (any, error)
	Response func(context.Context, json.RawMessage) error
	Policy   func() (AttestationPolicy, error)
	// RestoreWorker runs once after worker registration, before publishing the
	// bridge checkpoint or allowing the paused input actor to start.
	RestoreWorker func(context.Context, *WorkerRestoration) error
}

type AttestationPolicy struct {
	Enforce        bool
	AcceptLevel    string
	AcceptStatuses map[string]bool
}

// EnforcedAttestationPolicy matches the native default and invalid-config
// disposition. Unknown fields do not widen admission.
func EnforcedAttestationPolicy(raw json.RawMessage) AttestationPolicy {
	closed := AttestationPolicy{Enforce: true, AcceptLevel: "VERIFIED"}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return closed
	}
	policy := closed
	if v, ok := fields["accept_level"]; ok {
		if json.Unmarshal(v, &policy.AcceptLevel) != nil || attestationRank(policy.AcceptLevel) < 0 {
			return closed
		}
	}
	if v, ok := fields["accept_statuses"]; ok {
		var statuses []string
		if string(v) == "null" || json.Unmarshal(v, &statuses) != nil {
			return closed
		}
		policy.AcceptStatuses = make(map[string]bool)
		for _, status := range statuses {
			switch status {
			case "UNSPECIFIED", "ABSENT", "INVALID", "UNCHECKED":
				policy.AcceptStatuses[status] = true
			default:
				return closed
			}
		}
	}
	return policy
}

func attestationRank(value string) int {
	switch value {
	case "VERIFIED":
		return 0
	case "VERIFIED_KEYLESS_DEVICE":
		return 1
	case "VERIFIED_BY_GATE":
		return 2
	}
	return -1
}

func normalizedAttestation(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		value = strings.TrimPrefix(value, "DEVICE_ATTESTATION_STATUS_")
		switch value {
		case "UNSPECIFIED", "ABSENT", "VERIFIED", "VERIFIED_BY_GATE", "INVALID", "UNCHECKED", "VERIFIED_KEYLESS_DEVICE", "SERVICE_VOUCHED":
			return value
		}
	}
	var number int
	if len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &number) == nil && number >= 0 && number < 6 {
		return []string{"UNSPECIFIED", "ABSENT", "VERIFIED", "VERIFIED_BY_GATE", "INVALID", "UNCHECKED"}[number]
	}
	return "UNSPECIFIED"
}

func (p AttestationPolicy) admits(status string) bool {
	if !p.Enforce || status == "SERVICE_VOUCHED" || p.AcceptStatuses[status] {
		return true
	}
	level := p.AcceptLevel
	if level == "" {
		level = "VERIFIED"
	}
	rank := attestationRank(status)
	return rank >= 0 && rank <= attestationRank(level)
}

// Native UUID rings are process/query-local, not durable event-ID ledgers.
// Repeated additions do not move a UUID to the newest position.
type uuidRing struct {
	slots []string
	seen  map[string]bool
	next  int
}

func (r *uuidRing) has(id string) bool { return r.seen[id] }
func (r *uuidRing) add(id string) {
	if id == "" || r.has(id) {
		return
	}
	if r.slots == nil {
		r.slots = make([]string, 2000)
		r.seen = make(map[string]bool)
	}
	delete(r.seen, r.slots[r.next])
	r.slots[r.next] = id
	r.seen[id] = true
	r.next = (r.next + 1) % len(r.slots)
}

type workerFrame struct {
	event, id, data string
	comment         bool
}

func readWorkerFrames(reader *bufio.Reader, consume func(workerFrame) error) error {
	var frame workerFrame
	var line strings.Builder
	pendingCR := false
	if prefix, _ := reader.Peek(3); string(prefix) == "\xef\xbb\xbf" {
		_, _ = reader.Discard(3)
	}
	applyLine := func() error {
		value := line.String()
		line.Reset()
		if value == "" {
			if frame.data != "" || frame.comment {
				if err := consume(frame); err != nil {
					return err
				}
			}
			frame = workerFrame{}
			return nil
		}
		if strings.HasPrefix(value, ":") {
			frame.comment = true
			return nil
		}
		key, val, ok := strings.Cut(value, ":")
		if !ok {
			return nil
		}
		val = strings.TrimPrefix(val, " ")
		switch key {
		case "event":
			frame.event = val
		case "id":
			frame.id = val
		case "data":
			if frame.data != "" {
				frame.data += "\n"
			}
			frame.data += val
		}
		return nil
	}
	for {
		ch, err := reader.ReadByte()
		if err != nil {
			if err != io.EOF {
				return err
			}
			// The native transport does not call the parser's flush method at
			// EOF. An unfinished frame is retried, not dispatched prematurely.
			return nil
		}
		if pendingCR {
			pendingCR = false
			if err := applyLine(); err != nil {
				return err
			}
			if ch == '\n' {
				continue
			}
		}
		if ch == '\r' {
			pendingCR = true
		} else if ch == '\n' {
			if err := applyLine(); err != nil {
				return err
			}
		} else {
			line.WriteByte(ch)
		}
	}
}

type inboundEnvelope struct {
	EventID     string          `json:"event_id"`
	EventType   string          `json:"event_type"`
	Source      string          `json:"source"`
	Attestation json.RawMessage `json:"device_attestation_status"`
	Payload     json.RawMessage `json:"payload"`
}

func normalizeRequestIDs(payload map[string]json.RawMessage) {
	if old, ok := payload["requestId"]; ok {
		if _, exists := payload["request_id"]; !exists {
			payload["request_id"] = old
			delete(payload, "requestId")
		}
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(payload["response"], &response) == nil && response != nil {
		if old, ok := response["requestId"]; ok {
			if _, exists := response["request_id"]; !exists {
				response["request_id"] = old
				delete(response, "requestId")
				payload["response"], _ = json.Marshal(response)
			}
		}
	}
}

func rawString(value json.RawMessage) string {
	var text string
	_ = json.Unmarshal(value, &text)
	return text
}

func (s *sessionRuntime) consumeInboundFrame(ctx context.Context, frame workerFrame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cursor, err := strconv.ParseInt(sequencePrefix(frame.id), 10, 64); err == nil && frame.event != "ephemeral_event" {
		for previous := s.lastSequence.Load(); cursor > previous; previous = s.lastSequence.Load() {
			if s.lastSequence.CompareAndSwap(previous, cursor) {
				break
			}
		}
	}
	if frame.event == "ephemeral_event" {
		var event struct {
			Type string `json:"event_type"`
		}
		if json.Unmarshal([]byte(frame.data), &event) == nil && event.Type == "heartbeat_probe" {
			s.opMu.Lock()
			err := s.heartbeatLocked(ctx)
			s.opMu.Unlock()
			if err != nil {
				s.inboundFailed.Add(1)
			}
		}
		return nil
	}
	if frame.event != "client_event" || frame.data == "" {
		return nil
	}
	var envelope inboundEnvelope
	if json.Unmarshal([]byte(frame.data), &envelope) != nil {
		s.inboundFailed.Add(1)
		return nil
	}
	defer func() {
		s.enqueueDelivery(envelope.EventID, "received")
		s.enqueueDelivery(envelope.EventID, "processed")
	}()
	var payload map[string]json.RawMessage
	if json.Unmarshal(envelope.Payload, &payload) != nil || payload == nil {
		return nil
	}
	typeName := rawString(payload["type"])
	if typeName == "" {
		return nil
	}
	s.opMu.Lock()
	consumer := s.inbound
	s.opMu.Unlock()
	if consumer != nil && consumer.Policy != nil {
		policy, err := consumer.Policy()
		if err != nil {
			s.inboundFailed.Add(1)
			return nil
		}
		if !policy.admits(normalizedAttestation(envelope.Attestation)) {
			return nil
		}
	}
	if (typeName == "workflow_launch" || typeName == "queued_notification" || typeName == "session_notice") && envelope.EventType != typeName {
		return nil
	}
	if typeName == "control_request" && envelope.Source == "worker" {
		return nil
	}
	normalizeRequestIDs(payload)
	admitted, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if typeName == "control_request" {
		requestID := rawString(payload["request_id"])
		if requestID == "" || len(payload["request"]) == 0 || string(payload["request"]) == "null" {
			return nil
		}
		var value any
		var controlErr error
		if consumer == nil || consumer.Control == nil {
			s.inboundUnhandled.Add(1)
			controlErr = errors.New("Claude Desktop query has no inbound control consumer")
		} else {
			value, controlErr = consumer.Control(ctx, admitted)
		}
		response := map[string]any{"subtype": "success", "request_id": requestID}
		if controlErr != nil {
			s.inboundFailed.Add(1)
			response["subtype"] = "error"
			response["error"] = controlErr.Error()
		} else if value != nil {
			response["response"] = value
		}
		s.opMu.Lock()
		err = s.postWorkerEventLocked(ctx, map[string]any{"type": "control_response", "response": response, "session_id": s.state.RemoteSessionID})
		s.opMu.Unlock()
		if err != nil {
			s.inboundFailed.Add(1)
		}
		return nil
	}
	if typeName == "control_response" {
		if consumer == nil || consumer.Response == nil {
			s.inboundUnhandled.Add(1)
		} else if err := consumer.Response(ctx, admitted); err != nil {
			s.inboundFailed.Add(1)
		}
		return nil
	}
	if typeName != "user" {
		return nil
	}
	id := rawString(payload["uuid"])
	s.ingressMu.Lock()
	if id != "" && (s.inboundSeen.has(id) || s.outboundSeen.has(id)) {
		s.ingressMu.Unlock()
		return nil
	}
	if string(payload["isReplay"]) == "true" {
		s.ingressMu.Unlock()
		return nil
	}
	s.inboundSeen.add(id)
	s.ingressMu.Unlock()
	if consumer == nil || consumer.User == nil {
		s.inboundUnhandled.Add(1)
		return nil
	}
	if err := consumer.User(ctx, admitted); err != nil {
		s.inboundFailed.Add(1)
	} else {
		s.inboundDispatched.Add(1)
	}
	return nil
}

func sequencePrefix(value string) string {
	value = strings.TrimSpace(value)
	end := 0
	if len(value) > 0 && (value[0] == '+' || value[0] == '-') {
		end++
	}
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	return value[:end]
}

func (s *sessionRuntime) rememberOutbound(value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	var item struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(raw, &item) != nil || item.UUID == "" {
		return
	}
	s.ingressMu.Lock()
	s.outboundSeen.add(item.UUID)
	s.ingressMu.Unlock()
}
