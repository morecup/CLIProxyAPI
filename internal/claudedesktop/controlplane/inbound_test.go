package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:1]) }

func TestWorkerSSEFramingBoundariesAndUnfinishedEOF(t *testing.T) {
	for _, test := range []struct {
		name, data string
		count      int
		text       string
	}{
		{"lf", "event: client_event\nid: 7\ndata: one\ndata: two\n\n", 1, "one\ntwo"},
		{"bom-crlf", "\xef\xbb\xbfevent: client_event\r\nid: 7\r\ndata: value\r\n\r\n", 1, "value"},
		{"no-final-blank", "event: client_event\ndata: value\n", 0, ""},
		{"pending-cr", "event: client_event\rdata: value\r\r", 0, ""},
		{"cr-lookahead", "event: client_event\rdata: value\r\r:", 1, "value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var frames []workerFrame
			err := readWorkerFrames(bufio.NewReader(oneByteReader{strings.NewReader(test.data)}), func(frame workerFrame) error { frames = append(frames, frame); return nil })
			if err != nil || len(frames) != test.count {
				t.Fatal("frame boundary", len(frames), err)
			}
			if len(frames) != 0 && (frames[0].event != "client_event" || frames[0].data != test.text) {
				t.Fatal("frame value", frames)
			}
		})
	}
}

func TestWorkerIngressPolicyEchoReplayControlAndCursor(t *testing.T) {
	m := newQueryControlManager(t, &queryControlDoer{})
	facts := queryFacts("ingress", t.Context())
	var users, controls, responses int
	var admittedControl json.RawMessage
	policy := AttestationPolicy{AcceptLevel: "VERIFIED"}
	var policyErr error
	facts.Inbound = &InboundConsumer{
		User: func(context.Context, json.RawMessage) error { users++; return nil },
		Control: func(_ context.Context, raw json.RawMessage) (any, error) {
			controls++
			admittedControl = raw
			return map[string]any{"still_queued": []string{}}, nil
		},
		Response: func(context.Context, json.RawMessage) error { responses++; return nil },
		Policy:   func() (AttestationPolicy, error) { return policy, policyErr },
	}
	span, err := m.BeginRequest(t.Context(), testDesktopAuth(t), facts)
	if err != nil {
		t.Fatal(err)
	}
	s := span.session
	send := func(sequence, attestation, payload string) {
		t.Helper()
		raw, _ := json.Marshal(inboundEnvelope{EventID: sequence, EventType: "user", Attestation: json.RawMessage(attestation), Payload: json.RawMessage(payload)})
		if err := s.consumeInboundFrame(t.Context(), workerFrame{event: "client_event", id: sequence, data: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	send("7", `"ABSENT"`, `{"type":"user","uuid":"one"}`)
	send("7", `"ABSENT"`, `{"type":"user","uuid":"one"}`)
	send("7", `"ABSENT"`, `{"type":"user","uuid":"two"}`)
	send("8", `"ABSENT"`, `{"type":"user","uuid":"replay","isReplay":true}`)
	send("9", `"ABSENT"`, `{"type":"user","uuid":"replay"}`)
	s.rememberOutbound(map[string]any{"uuid": "outbound"})
	send("10", `"ABSENT"`, `{"type":"user","uuid":"outbound"}`)
	send("11", `"ABSENT"`, `{"type":"control_request","uuid":"one","requestId":"legacy","request":{"subtype":"interrupt"}}`)
	send("12", `"ABSENT"`, `{"type":"control_response","uuid":"one","response":{"requestId":"legacy"}}`)
	if users != 3 || controls != 1 || responses != 1 || !strings.Contains(string(admittedControl), `"request_id":"legacy"`) {
		t.Fatal("dispatch/dedup", users, controls, responses, string(admittedControl))
	}
	policy = EnforcedAttestationPolicy(json.RawMessage(`{}`))
	send("13", `"ABSENT"`, `{"type":"user","uuid":"blocked"}`)
	send("14", `"SERVICE_VOUCHED"`, `{"type":"user","uuid":"service"}`)
	policyErr = errors.New("policy unavailable")
	send("15", `"VERIFIED"`, `{"type":"user","uuid":"policy-error"}`)
	if err := s.consumeInboundFrame(t.Context(), workerFrame{event: "ephemeral_event", id: "999", data: `{"event_type":"unrelated"}`}); err != nil {
		t.Fatal(err)
	}
	if users != 4 || s.lastSequence.Load() != 15 || m.Status().InboundFailed != 1 || m.Status().InboundCompleted != 0 {
		t.Fatal("policy/cursor/completion", users, m.Status())
	}
	s.deliveryMu.Lock()
	count := len(s.deliveryPending) + len(s.deliveryWaiting)
	s.deliveryMu.Unlock()
	if count != 22 {
		t.Fatal("transport acks were confused with model completion", count)
	}
	m.RecordInboundOutcome(facts.DesktopSessionID, facts.QueryID, context.Canceled)
	m.RecordInboundOutcome(facts.DesktopSessionID, facts.QueryID, nil)
	if status := m.Status(); status.InboundCompleted != 1 || status.InboundCanceled != 1 || status.InboundExecutionFailed != 0 {
		t.Fatal("explicit cancellation was counted as inference failure", status)
	}
}

func TestAttestationPolicyValidationAndUUIDRing(t *testing.T) {
	for _, raw := range []string{`null`, `{`, `{"accept_level":"UNKNOWN"}`, `{"accept_statuses":null}`, `{"accept_statuses":["VERIFIED"]}`} {
		p := EnforcedAttestationPolicy(json.RawMessage(raw))
		if !p.Enforce || !p.admits("VERIFIED") || p.admits("ABSENT") || p.admits("VERIFIED_BY_GATE") {
			t.Fatal("malformed policy widened admission", raw, p)
		}
	}
	p := EnforcedAttestationPolicy(json.RawMessage(`{"accept_level":"VERIFIED_BY_GATE","accept_statuses":["ABSENT"]}`))
	if !p.admits("VERIFIED_KEYLESS_DEVICE") || !p.admits("ABSENT") || p.admits("INVALID") {
		t.Fatal("rank/exception policy", p)
	}
	r := uuidRing{slots: make([]string, 2), seen: make(map[string]bool)}
	r.add("a")
	r.add("b")
	r.add("a")
	r.add("c")
	if r.has("a") || !r.has("b") || !r.has("c") {
		t.Fatal("duplicate refreshed ring position")
	}
}
