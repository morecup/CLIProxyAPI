package claudedesktop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMagicLinkChromiumBooleanSettings(t *testing.T) {
	tests := []struct {
		name         string
		environment  string
		value        string
		defaultValue bool
		want         bool
		wantError    bool
	}{
		{name: "default true", environment: "CLIPROXY_TEST_CHROMIUM_BOOL_TRUE", defaultValue: true, want: true},
		{name: "default false", environment: "CLIPROXY_TEST_CHROMIUM_BOOL_FALSE", defaultValue: false, want: false},
		{name: "enabled", environment: "CLIPROXY_TEST_CHROMIUM_BOOL_ENABLED", value: " on ", want: true},
		{name: "disabled", environment: "CLIPROXY_TEST_CHROMIUM_BOOL_DISABLED", value: "OFF", defaultValue: true, want: false},
		{name: "invalid", environment: "CLIPROXY_TEST_CHROMIUM_BOOL_INVALID", value: "sometimes", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(test.environment, test.value)
			got, err := magicLinkChromiumBoolEnv(test.environment, test.defaultValue)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), test.environment) {
					t.Fatalf("magicLinkChromiumBoolEnv() error = %v, want environment-specific validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("magicLinkChromiumBoolEnv() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("magicLinkChromiumBoolEnv() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestChromiumAttestationErrorIncludesCause(t *testing.T) {
	cause := errors.New("chrome sandbox startup failed")
	err := chromiumAttestationError(context.Background(), nil, "start isolated Chromium", cause)
	if !errors.Is(err, errMagicLinkAttestationUnavailable) {
		t.Fatalf("chromiumAttestationError() = %v, want unavailable sentinel", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("chromiumAttestationError() = %v, want underlying cause", err)
	}
}
