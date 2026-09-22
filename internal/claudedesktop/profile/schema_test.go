package profile

import "testing"

func TestBuiltinV140609ResolvesExactVariants(t *testing.T) {
	bundle, errLoad := BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("BuiltinV140609() error = %v", errLoad)
	}
	opus, errResolve := bundle.Resolve(RequestVariantKey{Model: "claude-opus-5", Role: RoleMain, ThinkingDisplay: "updates"})
	if errResolve != nil {
		t.Fatalf("Resolve(opus main) error = %v", errResolve)
	}
	sonnet, errResolve := bundle.Resolve(RequestVariantKey{Model: "claude-sonnet-5", Role: RoleMain, ThinkingDisplay: "updates"})
	if errResolve != nil {
		t.Fatalf("Resolve(sonnet main) error = %v", errResolve)
	}
	if len(opus.AnthropicBeta) != 14 || len(sonnet.AnthropicBeta) != 13 {
		t.Fatalf("beta counts = opus:%d sonnet:%d, want 14 and 13", len(opus.AnthropicBeta), len(sonnet.AnthropicBeta))
	}
	if _, errMissing := bundle.Resolve(RequestVariantKey{Model: "claude-opus-5-latest", Role: RoleMain, ThinkingDisplay: "updates"}); errMissing == nil {
		t.Fatal("Resolve() accepted an uncompiled nearest-model alias")
	}
}

func TestBuiltinV140609UsesModelSpecificInstructionCarriers(t *testing.T) {
	bundle, errLoad := BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("BuiltinV140609() error = %v", errLoad)
	}
	opus, _ := bundle.Resolve(RequestVariantKey{Model: "claude-opus-5", Role: RoleMain, ThinkingDisplay: "updates"})
	haiku, _ := bundle.Resolve(RequestVariantKey{Model: "claude-haiku-4-5-20251001", Role: RoleMain, ThinkingDisplay: "updates"})
	if opus.InstructionCarrier != CarrierMidConversationSystem {
		t.Fatalf("opus carrier = %q", opus.InstructionCarrier)
	}
	if haiku.InstructionCarrier != CarrierUserSystemReminder {
		t.Fatalf("haiku carrier = %q", haiku.InstructionCarrier)
	}
}

func TestBuiltinV140609UsesRoleSpecificStreamPolicies(t *testing.T) {
	bundle, errLoad := BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("BuiltinV140609() error = %v", errLoad)
	}
	tests := []struct {
		key  RequestVariantKey
		want StreamPolicy
	}{
		{key: RequestVariantKey{Model: "claude-opus-5", Role: RoleMain, ThinkingDisplay: "updates"}, want: StreamPolicyTransport},
		{key: RequestVariantKey{Model: "claude-opus-5", Role: RoleCompaction, ThinkingDisplay: "omitted"}, want: StreamPolicyTransport},
		{key: RequestVariantKey{Model: "claude-opus-5", Role: RoleSubagent, Diagnostics: true, ThinkingDisplay: "omitted"}, want: StreamPolicyTransport},
		{key: RequestVariantKey{Model: "claude-sonnet-5", Role: RoleSubagent, Diagnostics: true, ThinkingDisplay: "updates"}, want: StreamPolicyTransport},
		{key: RequestVariantKey{Model: "claude-haiku-4-5-20251001", Role: RoleTitle}, want: StreamPolicyRequiredTrue},
		{key: RequestVariantKey{Model: "claude-haiku-4-5-20251001", Role: RoleWebSearchHelper}, want: StreamPolicyRequiredTrue},
		{key: RequestVariantKey{Model: "claude-haiku-4-5-20251001", Role: RoleLightHelper}, want: StreamPolicyOmitFalse},
		{key: RequestVariantKey{Model: "claude-sonnet-5", Role: RoleSecurityMonitor}, want: StreamPolicyOmitFalse},
		{key: RequestVariantKey{Model: "claude-opus-5", Role: RoleCountTokens}, want: StreamPolicyForbidden},
	}
	for _, test := range tests {
		variant, errResolve := bundle.Resolve(test.key)
		if errResolve != nil {
			t.Fatalf("Resolve(%+v) error = %v", test.key, errResolve)
		}
		if variant.StreamPolicy != test.want {
			t.Fatalf("Resolve(%+v) stream policy = %q, want %q", test.key, variant.StreamPolicy, test.want)
		}
	}
}

func TestBuiltinV140609PinsCapturedMessagesTransport(t *testing.T) {
	bundle, errLoad := BuiltinV140609()
	if errLoad != nil {
		t.Fatalf("BuiltinV140609() error = %v", errLoad)
	}
	transport, errTransport := bundle.TransportForRole(RoleMain)
	if errTransport != nil {
		t.Fatalf("TransportForRole(main) error = %v", errTransport)
	}
	if transport.Protocol != "http/1.1" || transport.JA3Hash != "d871d02cecbde59abbf8f4806134addf" {
		t.Fatalf("transport = protocol:%q ja3:%q", transport.Protocol, transport.JA3Hash)
	}
	wantOrder := []string{
		"Accept", "Authorization", "Content-Type", "User-Agent", "X-Claude-Code-Session-Id",
		"X-Stainless-Arch", "X-Stainless-Lang", "X-Stainless-OS", "X-Stainless-Package-Version",
		"X-Stainless-Retry-Count", "X-Stainless-Runtime", "X-Stainless-Runtime-Version",
		"X-Stainless-Timeout", "anthropic-beta", "anthropic-client-platform", "anthropic-client-version",
		"anthropic-dangerous-direct-browser-access", "anthropic-version", "x-app", "x-cc-atis",
		"x-claude-code-request-class", "x-client-request-id", "Connection", "Host", "Accept-Encoding",
		"Content-Length",
	}
	if len(transport.HeaderOrder) != len(wantOrder) {
		t.Fatalf("header order length = %d, want %d", len(transport.HeaderOrder), len(wantOrder))
	}
	for index := range wantOrder {
		if transport.HeaderOrder[index] != wantOrder[index] {
			t.Fatalf("header order[%d] = %q, want %q", index, transport.HeaderOrder[index], wantOrder[index])
		}
	}
	if len(transport.OptionalHeaders) != 1 || transport.OptionalHeaders[0] != "x-cc-atis" {
		t.Fatalf("optional headers = %#v, want x-cc-atis", transport.OptionalHeaders)
	}
	bootstrap, errBootstrap := bundle.TransportForEndpointRole("atis-bootstrap")
	if errBootstrap != nil {
		t.Fatalf("TransportForEndpointRole(atis-bootstrap) error = %v", errBootstrap)
	}
	wantBootstrapOrder := []string{"Accept", "Content-Type", "User-Agent", "anthropic-client-platform", "anthropic-client-version", "Authorization", "anthropic-beta", "Accept-Encoding", "Host", "Connection"}
	if len(bootstrap.HeaderOrder) != len(wantBootstrapOrder) {
		t.Fatalf("bootstrap header order = %#v", bootstrap.HeaderOrder)
	}
	for index := range wantBootstrapOrder {
		if bootstrap.HeaderOrder[index] != wantBootstrapOrder[index] {
			t.Fatalf("bootstrap header order[%d] = %q, want %q", index, bootstrap.HeaderOrder[index], wantBootstrapOrder[index])
		}
	}
}

func TestBuiltinV140609PinsCapturedControlPlaneTransports(t *testing.T) {
	bundle, errLoad := BuiltinV140609()
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	tests := []struct {
		endpoint string
		order    []string
	}{
		{
			endpoint: ControlEndpointCreateSession,
			order:    []string{"Accept", "Content-Type", "Authorization", "anthropic-version", "anthropic-client-platform", "User-Agent", "Content-Length", "Accept-Encoding", "Host", "Connection"},
		},
		{
			endpoint: ControlEndpointWorkerStream,
			order:    []string{"Accept", "Authorization", "User-Agent", "anthropic-client-platform", "anthropic-version", "Connection", "Host", "Accept-Encoding"},
		},
		{
			endpoint: ControlEndpointWorkerEvents,
			order:    []string{"Authorization", "Content-Type", "User-Agent", "anthropic-client-platform", "anthropic-version", "Connection", "Accept", "Host", "Accept-Encoding", "Content-Length"},
		},
		{
			endpoint: ControlEndpointSessionArchive,
			order:    []string{"Accept", "Content-Type", "Authorization", "anthropic-version", "anthropic-client-platform", "User-Agent", "anthropic-beta", "x-organization-uuid", "Content-Length", "Accept-Encoding", "Host", "Connection"},
		},
	}
	for _, test := range tests {
		endpoint := bundle.ControlPlane.Endpoints[test.endpoint]
		transport, errTransport := bundle.TransportForEndpointRole(endpoint.EndpointRole)
		if errTransport != nil {
			t.Fatalf("TransportForEndpointRole(%s) error = %v", endpoint.EndpointRole, errTransport)
		}
		if len(transport.HeaderOrder) != len(test.order) {
			t.Fatalf("%s header order = %#v", test.endpoint, transport.HeaderOrder)
		}
		for index := range test.order {
			if transport.HeaderOrder[index] != test.order[index] {
				t.Fatalf("%s header[%d] = %q, want %q", test.endpoint, index, transport.HeaderOrder[index], test.order[index])
			}
		}
	}
}
