package modelstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

// modelStudioAuthCookieNames are the SSO cookies that prove a signed-in
// Alibaba Cloud session. login_aliyunid_ticket is required; the identity
// cookies accompany it in a known-good jar.
var modelStudioRequiredCookie = "login_aliyunid_ticket"

var modelStudioIdentityCookies = []string{
	"login_aliyunid_pk", "login_current_pk", "login_aliyunid",
}

// diagCookieNameLimit caps how many cookie names a single diagnostic line
// lists so the report stays one-glance readable.
const diagCookieNameLimit = 12

// DiagnoseAuth reports the provider's credential and session state without
// mutating anything: stored session, CDP endpoint reachability, live cookie
// jar contents (names and partition keys only, never values), browser
// profile discoverability, and throttle markers. It backs
// `vibeusage auth modelstudio --diagnose`.
func (m ModelStudio) DiagnoseAuth(ctx context.Context) []provider.Diagnostic {
	diags := []provider.Diagnostic{
		diagnoseModelStudioEnvCookie(),
		diagnoseModelStudioStoredSession(),
	}
	diags = append(diags, diagnoseModelStudioCDPJars(ctx)...)
	diags = append(diags,
		diagnoseModelStudioBrowserProfiles(),
		diagnoseModelStudioThrottle(),
	)
	return diags
}

func diagnoseModelStudioEnvCookie() provider.Diagnostic {
	if cookie := strings.TrimSpace(os.Getenv("MODELSTUDIO_COOKIE")); cookie != "" {
		return provider.Diagnostic{
			Name:   "Environment cookie",
			Status: provider.DiagInfo,
			Detail: "MODELSTUDIO_COOKIE is set and takes precedence over the stored session",
		}
	}
	return provider.Diagnostic{
		Name:   "Environment cookie",
		Status: provider.DiagInfo,
		Detail: "MODELSTUDIO_COOKIE not set",
	}
}

func diagnoseModelStudioStoredSession() provider.Diagnostic {
	creds, err := loadSessionCredential()
	if err != nil {
		return provider.Diagnostic{
			Name:   "Stored session",
			Status: provider.DiagFail,
			Detail: fmt.Sprintf("reading stored session failed: %v", err),
		}
	}
	if creds.Cookie == "" {
		return provider.Diagnostic{
			Name:   "Stored session",
			Status: provider.DiagFail,
			Detail: "no session stored; run 'vibeusage auth modelstudio' or rely on CDP auto-login",
		}
	}

	names := cookieHeaderNames(creds.Cookie)
	nameSet := make(map[string]bool, len(names))
	for _, name := range names {
		nameSet[name] = true
	}

	var present []string
	if nameSet[modelStudioRequiredCookie] {
		present = append(present, modelStudioRequiredCookie)
	}
	for _, name := range modelStudioIdentityCookies {
		if nameSet[name] {
			present = append(present, name)
		}
	}

	status := provider.DiagWarn
	detail := fmt.Sprintf("source %s", modelStudioSourceLabel(creds.Source))
	if creds.BrowserLabel != "" {
		detail += " via " + creds.BrowserLabel
	}
	if !creds.UpdatedAt.IsZero() {
		detail += fmt.Sprintf(", updated %s ago", time.Since(creds.UpdatedAt).Round(time.Second))
	} else {
		detail += ", age unknown (stored before updated_at was recorded)"
	}
	if len(present) > 0 {
		detail += fmt.Sprintf("; auth cookies present: %s", strings.Join(present, ", "))
		status = provider.DiagOK
	} else {
		detail += fmt.Sprintf("; missing %s cookie — session will not authenticate",
			modelStudioRequiredCookie)
	}
	return provider.Diagnostic{
		Name:   "Stored session",
		Status: status,
		Detail: detail,
	}
}

// diagnoseModelStudioCDPJars probes every configured CDP port and reports the
// live cookie jar of each reachable browser: cookie names, partition keys,
// and expiries only. This is the state a renewal failure hides, surfaced in
// one command instead of an ad-hoc watch.
func diagnoseModelStudioCDPJars(ctx context.Context) []provider.Diagnostic {
	ports, explicit := cdpFixedPorts(configuredCDPPorts())
	var diags []provider.Diagnostic

	seen := make(map[string]bool, len(ports))
	for _, port := range ports {
		seen[port] = true
		endpoint, err := probeCDPPort(ctx, port)
		if err != nil {
			diags = append(diags, provider.Diagnostic{
				Name:   fmt.Sprintf("CDP :%s", port),
				Status: provider.DiagFail,
				Detail: fmt.Sprintf("unreachable: %v", err),
			})
			continue
		}
		diags = append(diags, diagnoseModelStudioJar(ctx, endpoint))
	}

	for _, endpoint := range platformCDPActivePortEndpoints() {
		if seen[endpoint.Port] {
			continue
		}
		seen[endpoint.Port] = true
		diags = append(diags, diagnoseModelStudioJar(ctx, endpoint))
	}

	if len(diags) == 0 {
		label := "defaulted ports 9222"
		if explicit {
			label = "configured ports"
		}
		diags = append(diags, provider.Diagnostic{
			Name:   "CDP endpoints",
			Status: provider.DiagWarn,
			Detail: fmt.Sprintf("no CDP port probed (%s) — passive import and auto-login are unavailable", label),
		})
	}
	return diags
}

func diagnoseModelStudioJar(ctx context.Context, endpoint cdpEndpoint) provider.Diagnostic {
	cookies, viaFallback, browserErr, err := diagnoseModelStudioCookies(ctx, endpoint)
	if err != nil {
		return provider.Diagnostic{
			Name:   endpoint.Label,
			Status: provider.DiagFail,
			Detail: fmt.Sprintf("cookie jar unreadable: %v", err),
		}
	}

	relevant := splitModelStudioCookies(cookies)
	detail := fmt.Sprintf("%d cookies (%d on Alibaba domains)", len(cookies), len(relevant))
	if viaFallback {
		detail += fmt.Sprintf("; browser-level Storage.getCookies rejected (%v), read via page target", browserErr)
	}
	if names := relevantNames(relevant); len(names) > 0 {
		detail += ": " + strings.Join(names, ", ")
	}
	if auth := hasModelStudioAuthCookies(cookieMap(relevant)); auth {
		detail += " — signed-in session visible"
	} else {
		detail += " — no signed-in session visible"
	}
	return provider.Diagnostic{
		Name:   endpoint.Label,
		Status: provider.DiagOK,
		Detail: detail,
	}
}

// diagnoseModelStudioCookies reads the jar through the browser-level
// endpoint first, recording whether the page-target fallback had to run.
func diagnoseModelStudioCookies(ctx context.Context, endpoint cdpEndpoint) (cookies []cdpCookie, viaFallback bool, browserErr error, err error) {
	conn, dialErr := dialCDP(ctx, endpoint.Port, endpoint.Path)
	if dialErr == nil {
		var res struct {
			Cookies []cdpCookie `json:"cookies"`
		}
		callErr := conn.call("Storage.getCookies", nil, &res)
		conn.Close()
		if callErr == nil {
			return res.Cookies, false, nil, nil
		}
		browserErr = callErr
	} else {
		browserErr = dialErr
	}

	fallbackCtx, cancel := fallbackCDPContext(ctx)
	defer cancel()
	cookies, err = diagnoseCookiesViaPageTarget(fallbackCtx, endpoint.Port)
	return cookies, true, browserErr, err
}

// diagnoseCookiesViaPageTarget mirrors fetchCookiesViaPageTarget but keeps
// the raw cdpCookie entries so partition keys survive for the report.
func diagnoseCookiesViaPageTarget(ctx context.Context, port string) ([]cdpCookie, error) {
	targets, err := listCDPPageTargets(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("listing page targets: %w", err)
	}
	for _, target := range targets {
		conn, dialErr := dialCDP(ctx, port, target.WebSocketDebuggerURL)
		if dialErr != nil {
			continue
		}
		var res struct {
			Cookies []cdpCookie `json:"cookies"`
		}
		callErr := conn.call("Storage.getCookies", nil, &res)
		conn.Close()
		if callErr == nil {
			return res.Cookies, nil
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no page targets to read the jar from")
	}
	return nil, fmt.Errorf("no page target served the cookie jar")
}

func diagnoseModelStudioBrowserProfiles() provider.Diagnostic {
	if hasModelStudioBrowserProfiles() {
		return provider.Diagnostic{
			Name:   "Browser profiles",
			Status: provider.DiagInfo,
			Detail: "native Chromium profiles discoverable for passive cookie import",
		}
	}
	return provider.Diagnostic{
		Name:   "Browser profiles",
		Status: provider.DiagInfo,
		Detail: "no native Chromium profiles discoverable",
	}
}

func diagnoseModelStudioThrottle() provider.Diagnostic {
	marker, err := (config.FileThrottleStore{}).Load("modelstudio")
	if err != nil {
		return provider.Diagnostic{
			Name:   "Throttle",
			Status: provider.DiagWarn,
			Detail: fmt.Sprintf("reading throttle marker failed: %v", err),
		}
	}
	if marker != nil && marker.RetryAt.After(time.Now()) {
		return provider.Diagnostic{
			Name:   "Throttle",
			Status: provider.DiagWarn,
			Detail: fmt.Sprintf("fetches throttled until %s (%s)", marker.RetryAt.Format(time.RFC3339), marker.Reason),
		}
	}
	return provider.Diagnostic{
		Name:   "Throttle",
		Status: provider.DiagInfo,
		Detail: "no active throttle marker",
	}
}

// modelStudioSourceLabel humanizes the stored session source constant.
func modelStudioSourceLabel(source string) string {
	switch source {
	case sessionSourceEnvironment:
		return "environment"
	case sessionSourceBrowser:
		return "browser session"
	case sessionSourceManual:
		return "manual paste"
	default:
		if source == "" {
			return "manual paste"
		}
		return source
	}
}

// cookieHeaderNames extracts cookie names from a "name=value; ..." header.
// Values are discarded and never reported.
func cookieHeaderNames(header string) []string {
	var names []string
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if name, _, found := strings.Cut(part, "="); found {
			names = append(names, strings.TrimSpace(name))
		}
	}
	return names
}

// cookieRelevant reports whether a cookie belongs to the provider's
// authentication domains.
func modelStudioCookieRelevant(domain string) bool {
	d := strings.ToLower(domain)
	return strings.Contains(d, "alibaba") || strings.Contains(d, "aliyun")
}

func splitModelStudioCookies(cookies []cdpCookie) (relevant []cdpCookie) {
	for _, c := range cookies {
		if modelStudioCookieRelevant(c.Domain) {
			relevant = append(relevant, c)
		}
	}
	return relevant
}

// relevantNames lists cookie names (with expiry when known), longest-expiring
// first, capped at diagCookieNameLimit. Values are never included.
func relevantNames(cookies []cdpCookie) []string {
	sorted := make([]cdpCookie, len(cookies))
	copy(sorted, cookies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Expires > sorted[j].Expires
	})
	names := make([]string, 0, len(sorted))
	for i, c := range sorted {
		if i == diagCookieNameLimit {
			names = append(names, fmt.Sprintf("… and %d more", len(sorted)-i))
			break
		}
		names = append(names, diagCookieSummary(c))
	}
	return names
}

func diagCookieSummary(c cdpCookie) string {
	summary := c.Name
	switch {
	case c.Expires > 0:
		summary += fmt.Sprintf(" (expires %s)", time.Unix(int64(c.Expires), 0).Format("15:04 MST"))
	default:
		summary += " (session cookie)"
	}
	if len(c.PartitionKey) > 0 {
		summary += fmt.Sprintf(" partition=%s", compactJSON(c.PartitionKey))
	}
	return summary
}

func compactJSON(raw json.RawMessage) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

func cookieMap(cookies []cdpCookie) map[string]browserCookie {
	m := make(map[string]browserCookie, len(cookies))
	for _, c := range cookies {
		expires := time.Time{}
		if c.Expires > 0 {
			expires = time.Unix(int64(c.Expires), 0)
		}
		m[c.Name] = browserCookie{
			Name:      c.Name,
			Value:     c.Value,
			Domain:    c.Domain,
			Path:      c.Path,
			Secure:    c.Secure,
			ExpiresAt: expires,
		}
	}
	return m
}
