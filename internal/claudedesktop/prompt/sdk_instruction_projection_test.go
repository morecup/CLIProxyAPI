package prompt

import (
	"bytes"
	"testing"
	"time"
)

func TestOwnedInstructionCarrierProofChecksFinalWireWithoutInventingYield(t *testing.T) {
	before := []byte(`{"messages":[{"role":"user","content":"input"}],"system":"owned"}`)
	after := []byte(`{"messages":[{"role":"user","content":"input"},{"role":"system","content":[{"type":"text","text":"owned"}]}]}`)
	for _, kind := range []string{"exact", "cache-decoration", "unexpected-system", "changed-user", "unproven", "envelope-edit"} {
		t.Run(kind, func(t *testing.T) {
			tracker := NewTracker(nil)
			input := sdkStateTestInput(string(before), time.Now())
			request := tracker.Begin(input)
			if kind != "unproven" {
				request.ObserveOwnedInstructionCarrier(before, after)
			}
			wire := bytes.Clone(after)
			switch kind {
			case "cache-decoration":
				wire = bytes.ReplaceAll(wire, []byte(`"text":"owned"`), []byte(`"text":"owned","cache_control":{"type":"ephemeral"}`))
			case "unexpected-system":
				wire = bytes.ReplaceAll(wire, []byte(`"text":"owned"`), []byte(`"text":"foreign"`))
			case "changed-user":
				wire = bytes.ReplaceAll(wire, []byte(`"content":"input"`), []byte(`"content":"foreign"`))
			case "envelope-edit":
				wire = bytes.ReplaceAll(wire, []byte(`"role":"system"`), []byte(`"role":"system","unexpected":true`))
			}
			request.ObserveSDKQuery(wire)
			want := kind == "exact" || kind == "cache-decoration"
			if request.call.sdkWireInputKnown != want {
				t.Fatal("proof admitted unexpected edit or lost owned projection", request.call.sdkWireInputKnown)
			}
			if want && (len(request.call.sdkWireInputs) != 1 || len(request.call.state.sdk.history.messages) != 1) {
				t.Fatal("system carrier became a user yield")
			}
			if request.call.sdkWireRequestDigest != sdkCompactionHash(string(wire)) {
				t.Fatal("actual wire digest was replaced by native projection")
			}
		})
	}
}
