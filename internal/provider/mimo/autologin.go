package mimo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
)

// Automatic session refresh for Xiaomi MiMo via SSO.
//
// MiMo uses session cookies (expires=-1) that are set when the browser
// navigates to platform.xiaomimimo.com and Xiaomi SSO auto-completes using
// the existing account.xiaomi.com session. No form-filling is needed — just
// a navigation to trigger the SSO redirect.
//
// When no MiMo cookies are found in any CDP browser, vibeusage opens the
// MiMo platform in a new tab on the headed browser, waits for SSO to
// complete, then re-imports cookies.

const (
	mimoPlatformURL      = "https://platform.xiaomimimo.com/#/console/balance"
	mimoSSOWaitTimeout   = 15 * time.Second
	mimoSSOPollInterval  = 500 * time.Millisecond
	mimoSessionCooldown  = 5 * time.Minute
	mimoLoginOverallWait = 25 * time.Second
)

var (
	errMimoAutoLoginUnavailable = errors.New("no headed CDP port configured for MiMo auto-login")
	errMimoAutoLoginCoolingDown = errors.New("MiMo auto-login attempted recently")
	errMimoSSONotCompleted      = errors.New("SSO did not complete — Xiaomi account session may be expired")
)

// mimoLoginAttemptMarker records the last auto-login attempt for cooldown.
func mimoLoginAttemptMarker() string {
	return filepath.Join(config.ThrottlesDir(), "mimo-login.json")
}

func mimoLastLoginAttempt() time.Time {
	data, err := os.ReadFile(mimoLoginAttemptMarker())
	if err != nil {
		return time.Time{}
	}
	var marker struct {
		AttemptedAt time.Time `json:"attempted_at"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return time.Time{}
	}
	return marker.AttemptedAt
}

func mimoRecordLoginAttempt(at time.Time) {
	path := mimoLoginAttemptMarker()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(struct {
		AttemptedAt time.Time `json:"attempted_at"`
	}{AttemptedAt: at})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// mimoAutoLoginPort resolves the headed CDP port for MiMo auto-login.
// Uses the first configured cdp_ports entry (same pattern as ModelStudio).
func mimoAutoLoginPort() (string, error) {
	pc := config.Get().Providers["mimo"]
	if pc.LoginPort > 0 {
		if port := normalizeCDPPort(fmt.Sprintf("%d", pc.LoginPort)); port != "" {
			return port, nil
		}
	}
	if ports, explicit := cdpFixedPorts(pc.CDPPorts); explicit && len(ports) > 0 {
		return ports[0], nil
	}
	return "", errMimoAutoLoginUnavailable
}

// triggerMimoSSO opens the MiMo platform in a new tab on the headed browser,
// waits for Xiaomi SSO to auto-complete (setting MiMo session cookies), then
// imports the cookies while the tab is still open. The browser's existing
// Xiaomi account session handles authentication without any form-filling.
// Returns the imported cookie header on success.
func triggerMimoSSO(ctx context.Context, port string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, mimoLoginOverallWait)
	defer cancel()
	logger := logging.FromContext(ctx).With("provider", "mimo", "step", "auto-login")
	started := time.Now()
	elapsed := func() time.Duration { return time.Since(started).Round(time.Millisecond) }

	// Open MiMo platform in a new tab
	target, err := openCDPTarget(ctx, port, mimoPlatformURL)
	if err != nil {
		return "", fmt.Errorf("open MiMo tab on CDP :%s: %w", port, err)
	}
	defer closeCDPTarget(port, target.ID)
	logger.Debug("opened MiMo tab", "port", port, "elapsed", elapsed())

	page, err := dialCDP(ctx, port, target.wsPath())
	if err != nil {
		return "", fmt.Errorf("attach to MiMo tab: %w", err)
	}
	defer page.Close()

	if err := page.call("Page.enable", nil, nil); err != nil {
		return "", err
	}

	// Wait for SSO to complete — the page should land on the MiMo console
	// and the API should return valid data.
	deadline := time.Now().Add(mimoSSOWaitTimeout)
	for {
		var href string
		if err := page.evalInContext(0, "location.href", &href); err == nil {
			if parsed, err := url.Parse(href); err == nil && strings.Contains(parsed.Host, "xiaomimimo") {
				var apiResult string
				if err := page.evalInContext(0, `fetch('/api/v1/balance').then(r => r.json()).then(d => d.code === 0 ? 'ok' : 'fail').catch(() => 'error')`, &apiResult); err == nil && apiResult == "ok" {
					logger.Debug("SSO completed, importing cookies", "elapsed", elapsed())

					// Import cookies while the tab is still open
					cookieHeader, err := importCookiesFromPage(page, mimoTargetCookieURL)
					if err != nil {
						return "", fmt.Errorf("import cookies after SSO: %w", err)
					}
					return cookieHeader, nil
				}
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w within %s", errMimoSSONotCompleted, mimoSSOWaitTimeout)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(mimoSSOPollInterval):
		}
	}
}

// importCookiesFromPage reads cookies from the active page via Network.getCookies
// and builds the cookie header for the MiMo API.
func importCookiesFromPage(page *cdpConn, targetURL string) (string, error) {
	var res struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	if err := page.call("Network.getCookies", nil, &res); err != nil {
		return "", fmt.Errorf("get cookies: %w", err)
	}
	out := make([]browserCookie, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		var exp time.Time
		if c.Expires > 0 {
			exp = time.Unix(int64(c.Expires), 0)
		}
		out = append(out, browserCookie{
			Name:      c.Name,
			Value:     c.Value,
			Domain:    c.Domain,
			Path:      c.Path,
			Secure:    c.Secure,
			ExpiresAt: exp,
		})
	}

	header, err := buildMimoCookieHeader(out, targetURL, time.Now())
	if err != nil {
		return "", err
	}
	return header, nil
}

// autoLoginMimo orchestrates the MiMo session refresh: checks cooldown,
// resolves the headed port, triggers SSO, and returns the imported cookie header.
func autoLoginMimo(ctx context.Context) (string, error) {
	port, err := mimoAutoLoginPort()
	if err != nil {
		return "", err
	}

	if last := mimoLastLoginAttempt(); !last.IsZero() && time.Since(last) < mimoSessionCooldown {
		return "", fmt.Errorf("%w (%s ago); next attempt after %s",
			errMimoAutoLoginCoolingDown, time.Since(last).Round(time.Second), mimoSessionCooldown)
	}
	mimoRecordLoginAttempt(time.Now())

	return triggerMimoSSO(ctx, port)
}