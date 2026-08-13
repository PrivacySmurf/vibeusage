package modelstudio

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
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
	importModelStudioBrowserSession = platformImportModelStudioBrowserSession
	hasModelStudioBrowserProfiles   = platformHasModelStudioBrowserProfiles
)

func buildModelStudioCookieHeader(cookies []browserCookie, targetURL string, now time.Time) (string, error) {
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

	if !hasModelStudioAuthCookies(best) {
		return "", fmt.Errorf("no authenticated Alibaba session cookies found")
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
	if !strings.HasPrefix(domain, ".") {
		return domain == host
	}
	domain = strings.TrimPrefix(domain, ".")
	return host == domain || strings.HasSuffix(host, "."+domain)
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

func hasModelStudioAuthCookies(cookies map[string]browserCookie) bool {
	if _, ok := cookies["login_aliyunid_ticket"]; !ok {
		return false
	}
	for _, name := range []string{"login_aliyunid_pk", "login_current_pk", "login_aliyunid"} {
		if _, ok := cookies[name]; ok {
			return true
		}
	}
	return false
}
