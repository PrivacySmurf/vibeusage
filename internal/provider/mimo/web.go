package mimo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

const (
	defaultBaseURL   = "https://platform.xiaomimimo.com/api/v1"
	browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
)

type WebConsoleStrategy struct {
	HTTPTimeout float64
	BaseURL     string
}

type sessionCredentials struct {
	Cookie       string `json:"cookie"`
	Source       string `json:"source,omitempty"`
	BrowserLabel string `json:"browser,omitempty"`
}

const (
	sessionSourceEnvironment = "environment"
	sessionSourceManual      = "manual"
	sessionSourceBrowser     = "browser"
)

func getAPIBaseURL(override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if env := os.Getenv("MIMO_API_URL"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return defaultBaseURL
}

func loadSessionCredential() (sessionCredentials, error) {
	if envCookie := strings.TrimSpace(os.Getenv("MIMO_COOKIE")); envCookie != "" {
		return sessionCredentials{Cookie: envCookie, Source: sessionSourceEnvironment}, nil
	}

	data, err := config.ReadCredential("mimo", "session")
	if err != nil || len(data) == 0 {
		return sessionCredentials{}, err
	}

	var creds sessionCredentials
	if err := json.Unmarshal(data, &creds); err == nil && creds.Cookie != "" {
		if creds.Source == "" {
			creds.Source = sessionSourceManual
		}
		return creds, nil
	}
	return sessionCredentials{Cookie: strings.TrimSpace(string(data)), Source: sessionSourceManual}, nil
}

func (s *WebConsoleStrategy) IsAvailable() bool {
	if cookie := os.Getenv("MIMO_COOKIE"); cookie != "" {
		return true
	}
	data, err := config.ReadCredential("mimo", "session")
	return (err == nil && len(data) > 0) || hasMimoBrowserProfiles()
}

func (s *WebConsoleStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	creds, err := loadSessionCredential()
	if err != nil {
		return fetch.ResultFail(fmt.Sprintf("Xiaomi MiMo: reading stored browser session: %v", err)), nil
	}

	timeout := s.HTTPTimeout
	if timeout <= 0 {
		timeout = config.Get().Fetch.Timeout
	}
	if timeout <= 0 {
		timeout = 15.0
	}
	client := httpclient.NewFromConfig(timeout)

	if creds.Cookie == "" {
		return s.fetchImportedBrowserSession(ctx, client, sessionCredentials{})
	}

	snapshot, err := s.fetchWithSession(ctx, client, creds.Cookie)
	if err == nil {
		return fetch.ResultOK(*snapshot), nil
	}
	if !errors.Is(err, errSessionExpired) {
		return fetch.ResultFail(fmt.Sprintf("Xiaomi MiMo web console API failed: %v", err)), nil
	}
	if creds.Source == sessionSourceEnvironment {
		return fetch.ResultFatal(mimoSessionHint(creds, errBrowserCookieImportUnavailable)), nil
	}

	return s.fetchImportedBrowserSession(ctx, client, creds)
}

func (s *WebConsoleStrategy) fetchWithSession(
	ctx context.Context,
	client *httpclient.Client,
	cookie string,
) (*models.UsageSnapshot, error) {
	baseURL := getAPIBaseURL(s.BaseURL)

	reqHeaders := []httpclient.RequestOption{
		httpclient.WithHeader("Accept", "application/json, text/plain, */*"),
		httpclient.WithHeader("Cookie", cookie),
		httpclient.WithHeader("Accept-Language", "en-US,en;q=0.9"),
		httpclient.WithHeader("x-timeZone", "UTC"),
		httpclient.WithHeader("Origin", "https://platform.xiaomimimo.com"),
		httpclient.WithHeader("Referer", "https://platform.xiaomimimo.com/#/console/balance"),
		httpclient.WithHeader("User-Agent", browserUserAgent),
	}

	// 1. Fetch balance
	balanceURL := baseURL + "/balance"
	balResp, err := client.DoCtx(ctx, http.MethodGet, balanceURL, nil, reqHeaders...)
	if err != nil {
		return nil, fmt.Errorf("balance request failed: %w", err)
	}

	if balResp.StatusCode == http.StatusUnauthorized || balResp.StatusCode == http.StatusForbidden {
		return nil, errSessionExpired
	}
	if balResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("balance request returned HTTP %d", balResp.StatusCode)
	}

	balanceBytes := balResp.Body

	// 2. Fetch token plan detail (optional, non-fatal)
	detailURL := baseURL + "/tokenPlan/detail"
	var detailBytes []byte
	if detailResp, err := client.DoCtx(ctx, http.MethodGet, detailURL, nil, reqHeaders...); err == nil && detailResp.StatusCode == http.StatusOK {
		detailBytes = detailResp.Body
	}

	// 3. Fetch token plan usage (optional, non-fatal)
	usageURL := baseURL + "/tokenPlan/usage"
	var usageBytes []byte
	if usageResp, err := client.DoCtx(ctx, http.MethodGet, usageURL, nil, reqHeaders...); err == nil && usageResp.StatusCode == http.StatusOK {
		usageBytes = usageResp.Body
	}

	return parseSnapshot(balanceBytes, detailBytes, usageBytes, time.Now())
}

func (s *WebConsoleStrategy) fetchImportedBrowserSession(
	ctx context.Context,
	client *httpclient.Client,
	previous sessionCredentials,
) (fetch.FetchResult, error) {
	imported, importErr := importMimoBrowserSession(ctx)
	if importErr != nil {
		if cookieHeader, loginErr := autoLoginMimo(ctx); loginErr == nil && cookieHeader != "" {
			// Auto-login succeeded and returned cookies directly
			creds := sessionCredentials{
				Cookie:       cookieHeader,
				Source:       sessionSourceBrowser,
				BrowserLabel: "CDP auto-login",
			}
			snapshot, err := s.fetchWithSession(ctx, client, creds.Cookie)
			if err == nil {
				if data, err := json.Marshal(creds); err == nil {
					_ = config.WriteCredential("mimo", "session", data)
				}
				return fetch.ResultOK(*snapshot), nil
			}
			// If the direct cookie import failed, fall through to normal hint
			importErr = fmt.Errorf("auto-login succeeded but API call failed: %w", err)
		} else if loginErr != nil && !errors.Is(loginErr, errMimoAutoLoginUnavailable) {
			importErr = fmt.Errorf("%w; auto-login failed: %v", importErr, loginErr)
		}
		return fetch.ResultFatal(mimoSessionHint(previous, importErr)), nil
	}

	creds := sessionCredentials{
		Cookie:       imported.Cookie,
		Source:       sessionSourceBrowser,
		BrowserLabel: imported.SourceLabel,
	}

	snapshot, err := s.fetchWithSession(ctx, client, creds.Cookie)
	if err != nil {
		if !errors.Is(err, errSessionExpired) {
			return fetch.ResultFail(fmt.Sprintf("Xiaomi MiMo web console API failed after importing %s cookies: %v", imported.SourceLabel, err)), nil
		}
		// Session expired — try auto-login to refresh SSO cookies
		if cookieHeader, loginErr := autoLoginMimo(ctx); loginErr == nil && cookieHeader != "" {
			creds.Cookie = cookieHeader
			creds.BrowserLabel = "CDP auto-login"
			if retrySnapshot, retryErr := s.fetchWithSession(ctx, client, cookieHeader); retryErr == nil {
				if data, err := json.Marshal(creds); err == nil {
					_ = config.WriteCredential("mimo", "session", data)
				}
				return fetch.ResultOK(*retrySnapshot), nil
			}
		}
		return fetch.ResultFatal(mimoSessionHint(creds, err)), nil
	}

	// Cache successfully authenticated browser-imported session
	if data, err := json.Marshal(creds); err == nil {
		_ = config.WriteCredential("mimo", "session", data)
	}

	return fetch.ResultOK(*snapshot), nil
}

// ExtendTimeout gives Fetch room for an SSO navigation on top of the normal
// request budget.
func (s *WebConsoleStrategy) ExtendTimeout(base time.Duration) time.Duration {
	if _, err := mimoAutoLoginPort(); err != nil {
		return base
	}
	return base + mimoLoginOverallWait
}

func mimoSessionHint(creds sessionCredentials, reason error) string {
	var sb strings.Builder
	switch creds.Source {
	case sessionSourceEnvironment:
		sb.WriteString("Xiaomi MiMo: MIMO_COOKIE session expired or is invalid.")
	case sessionSourceBrowser:
		if creds.BrowserLabel != "" {
			fmt.Fprintf(&sb, "Xiaomi MiMo: session imported from %s session expired or is invalid.", creds.BrowserLabel)
		} else {
			sb.WriteString("Xiaomi MiMo: browser session expired or is invalid.")
		}
	default:
		sb.WriteString("Xiaomi MiMo: session expired or no authenticated session found.")
	}

	// The bare import-unavailable sentinel adds nothing beyond the hint text;
	// every other reason (including import details and "; auto-login failed")
	// is the only clue the user gets, so show it.
	if reason != nil && reason != errBrowserCookieImportUnavailable {
		sb.WriteString(" (")
		sb.WriteString(reason.Error())
		sb.WriteString(")")
	}

	sb.WriteString("\nSign in to https://platform.xiaomimimo.com/console/balance in your browser (or CDP fleet on port 9444), or update credentials with 'vibeusage auth mimo'.")
	return sb.String()
}
