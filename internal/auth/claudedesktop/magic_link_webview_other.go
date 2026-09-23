//go:build !windows

package claudedesktop

import "context"

func acquireMagicLinkAttestation(ctx context.Context, credentials magicLinkCredentials, options magicLinkAttestationOptions) (magicLinkAttestation, error) {
	return acquireMagicLinkAttestationWithChromium(ctx, credentials, options)
}

func RunMagicLinkAttestationHelper() bool { return false }
