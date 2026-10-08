package mimo

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hash-route URL (#/console/balance) redirects to the public /token-plan
// marketing page, which never runs the Xiaomi SSO handshake, so auto-login
// times out with "SSO did not complete" even with a valid account session
// (regression introduced in 8598798). Only the path-route console URL
// triggers the account.xiaomi.com SSO redirect that sets api-platform cookies.
func TestMimoPlatformURLTriggersSSOHandshake(t *testing.T) {
	parsed, err := url.Parse(mimoPlatformURL)
	require.NoError(t, err)
	assert.Empty(t, parsed.Fragment, "hash-route console URLs bypass the SSO handshake")
	assert.Equal(t, "/console/balance", parsed.Path)
}

// The auto-login failure reason must survive into the user-facing hint;
// folding it into the suppressed import-unavailable sentinel hid the only
// clue ("auto-login failed: ...") behind a generic "session expired" line.
func TestMimoSessionHintIncludesAutoLoginFailure(t *testing.T) {
	creds := sessionCredentials{Source: sessionSourceBrowser, BrowserLabel: "Chrome (CDP :9444)"}
	reason := fmt.Errorf("%w; auto-login failed: %v", errBrowserCookieImportUnavailable, errMimoSSONotCompleted)
	hint := mimoSessionHint(creds, reason)
	assert.True(t, strings.Contains(hint, "auto-login failed"),
		"hint must show why auto-login failed, got: %q", hint)
}

// A bare import-unavailable sentinel carries no information beyond the hint
// text itself and stays suppressed.
func TestMimoSessionHintSuppressesBareSentinel(t *testing.T) {
	creds := sessionCredentials{Source: sessionSourceBrowser, BrowserLabel: "Chrome (CDP :9444)"}
	hint := mimoSessionHint(creds, errBrowserCookieImportUnavailable)
	assert.False(t, strings.Contains(hint, "browser cookie import unavailable"),
		"bare sentinel adds nothing: %q", hint)
}
