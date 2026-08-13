//go:build !darwin

package modelstudio

import "context"

func platformImportModelStudioBrowserSession(context.Context) (browserSession, error) {
	return browserSession{}, errBrowserCookieImportUnavailable
}

func platformHasModelStudioBrowserProfiles() bool {
	return false
}
