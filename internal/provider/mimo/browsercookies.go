package mimo

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
)

var errBrowserCookieImportUnavailable = errors.New("browser cookie import unavailable")

type browserSession struct {
	Cookie      string
	SourceLabel string
}

type browserCookie struct {
	Name       string
	Value      string
	Domain     string
	Path       string
	Secure     bool
	ExpiresAt  time.Time
	LastAccess time.Time
}

var (
	importMimoBrowserSession = platformImportMimoBrowserSession
	hasMimoBrowserProfiles   = platformHasMimoBrowserProfiles
)

// configuredCDPPorts reads [providers.mimo] cdp_ports from config.
func configuredCDPPorts() []int {
	return config.Get().Providers["mimo"].CDPPorts
}

// hasConfiguredCDPPorts reports whether the user explicitly pointed vibeusage
// at a remote-debugging browser (env or config). The implicit 9222 default
// does not count, so the provider is not advertised as available on machines
// that never set anything up.
func hasConfiguredCDPPorts() bool {
	_, explicit := cdpFixedPorts(configuredCDPPorts())
	return explicit
}

func buildMimoCookieHeader(cookies []browserCookie, targetURL string, now time.Time) (string, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return "", fmt.Errorf("parse cookie target: %w", err)
	}
	if target.Hostname() == "" {
		return "", fmt.Errorf("cookie target has no host")
	}

	best := make(map[string]browserCookie)
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Value == "" || !cookieDomainMatches(cookie.Domain, target.Hostname()) ||
			!cookiePathMatches(cookie.Path, target.EscapedPath()) ||
			(cookie.Secure && target.Scheme != "https") ||
			(!cookie.ExpiresAt.IsZero() && !cookie.ExpiresAt.After(now)) {
			continue
		}

		current, ok := best[cookie.Name]
		if !ok || cookieMoreSpecific(cookie, current, target.Hostname()) {
			best[cookie.Name] = cookie
		}
	}

	if !hasMimoAuthCookies(best) {
		return "", fmt.Errorf("no authenticated Xiaomi MiMo session cookies found")
	}

	selected := make([]browserCookie, 0, len(best))
	for _, cookie := range best {
		selected = append(selected, cookie)
	}
	sort.Slice(selected, func(i, j int) bool {
		if len(selected[i].Path) != len(selected[j].Path) {
			return len(selected[i].Path) > len(selected[j].Path)
		}
		return selected[i].Name < selected[j].Name
	})

	pairs := make([]string, 0, len(selected))
	for _, cookie := range selected {
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(pairs, "; "), nil
}

func cookieDomainMatches(cookieDomain, requestHost string) bool {
	domain := strings.ToLower(strings.TrimSpace(cookieDomain))
	host := strings.ToLower(strings.TrimSpace(requestHost))
	if domain == "" || host == "" {
		return false
	}
	domain = strings.TrimPrefix(domain, ".")
	if host == domain || strings.HasSuffix(host, "."+domain) {
		return true
	}
	// Xiaomi authentication cookies are set across .xiaomimimo.com and .xiaomi.com
	if (strings.HasSuffix(host, "xiaomimimo.com") || host == "xiaomimimo.com") &&
		(domain == "xiaomi.com" || strings.HasSuffix(domain, "xiaomi.com")) {
		return true
	}
	return false
}

func cookiePathMatches(cookiePath, requestPath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if requestPath == "" {
		requestPath = "/"
	}
	if requestPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") ||
		(len(requestPath) > len(cookiePath) && requestPath[len(cookiePath)] == '/')
}

func cookieMoreSpecific(candidate, current browserCookie, requestHost string) bool {
	if len(candidate.Path) != len(current.Path) {
		return len(candidate.Path) > len(current.Path)
	}
	candidateExact := !strings.HasPrefix(candidate.Domain, ".") && strings.EqualFold(candidate.Domain, requestHost)
	currentExact := !strings.HasPrefix(current.Domain, ".") && strings.EqualFold(current.Domain, requestHost)
	if candidateExact != currentExact {
		return candidateExact
	}
	if !candidate.ExpiresAt.Equal(current.ExpiresAt) {
		return candidate.ExpiresAt.After(current.ExpiresAt)
	}
	return candidate.LastAccess.After(current.LastAccess)
}

func hasMimoAuthCookies(cookies map[string]browserCookie) bool {
	if _, ok := cookies["api-platform_serviceToken"]; !ok {
		return false
	}
	if _, ok := cookies["userId"]; ok {
		return true
	}
	if _, ok := cookies["cUserId"]; ok {
		return true
	}
	return false
}
