//go:build !windows

package claudedesktop

import (
	"context"
	"fmt"
)

func acquireMagicLinkAttestation(context.Context, magicLinkCredentials) (magicLinkAttestation, error) {
	return magicLinkAttestation{}, fmt.Errorf("%w: WebView2 is available only on Windows", errMagicLinkAttestationUnavailable)
}

func RunMagicLinkAttestationHelper() bool { return false }
