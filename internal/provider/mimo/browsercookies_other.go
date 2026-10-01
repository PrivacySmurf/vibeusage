//go:build !darwin

package mimo

import "context"

// Non-macOS platforms have no cookie-database decryption path, but can still
// read cookies live from any Chromium exposing a remote-debugging port.

func platformCDPActivePortEndpoints() []cdpEndpoint {
	return nil
}

func platformImportMimoBrowserSession(ctx context.Context) (browserSession, error) {
	return importMimoCDPSession(ctx)
}

func platformHasMimoBrowserProfiles() bool {
	return hasConfiguredCDPPorts()
}
