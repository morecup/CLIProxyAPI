package profile

import (
	"strings"
	"testing"
)

func TestSDKInputBetaPolicyIsIndependentAndExact(t *testing.T) {
	p := SDKTelemetryProfile{Events: map[string]TelemetryEventProfile{"input_prompt": {EventName: "tengu_input_prompt"}}, InputBetas: map[string][]string{"model-a": {"alpha-20260101"}}}
	if err := validateSDKInputProfile(p); err != nil {
		t.Fatal(err)
	}
	if got, ok := p.InputBetaHeader("model-a"); !ok || got != "alpha-20260101" {
		t.Fatal("exact beta policy missing")
	}
	for _, model := range []string{"", "model-a-extra", "MODEL-A", "model-b"} {
		if _, ok := p.InputBetaHeader(model); ok {
			t.Fatal("unknown model inherited betas")
		}
	}
	for _, betas := range [][]string{nil, {"alpha", "alpha"}, {"alpha,beta"}, {"alpha\r\nbeta"}, {" "}, {strings.Repeat("x", 102)}} {
		p.InputBetas["model-a"] = betas
		if validateSDKInputProfile(p) == nil {
			t.Fatalf("invalid policy accepted: %q", betas)
		}
	}
	p.InputBetas = map[string][]string{"model-a": {"alpha"}}
	delete(p.Events, "input_prompt")
	if validateSDKInputProfile(p) == nil {
		t.Fatal("policy without input event mapping")
	}
	if validateSDKInputProfile(SDKTelemetryProfile{}) != nil {
		t.Fatal("older bundle without producer became invalid")
	}
}
