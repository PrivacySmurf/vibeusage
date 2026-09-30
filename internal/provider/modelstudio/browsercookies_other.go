//go:build !darwin

package modelstudio

import "context"

// Non-macOS platforms have no cookie-database decryption path, but can still
// read cookies live from any Chromium exposing a remote-debugging port.

func platformCDPActivePortEndpoints() []cdpEndpoint {
	return nil
}

func platformImportModelStudioBrowserSession(ctx context.Context) (browserSession, error) {
	return importModelStudioCDPSession(ctx)
}

func platformHasModelStudioBrowserProfiles() bool {
	return hasConfiguredCDPPorts()
}
