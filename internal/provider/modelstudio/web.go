package modelstudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

const (
	modelStudioConsoleBaseURL = "https://modelstudio.console.alibabacloud.com"
	modelStudioDashboardURL   = modelStudioConsoleBaseURL + "/ap-southeast-1/?tab=plan#/efm/subscription/token-plan/enterprise"
	modelStudioRegion         = "ap-southeast-1"
	modelStudioTeamProduct    = "sfm_tokenplanteams_dp_intl"
	bssProduct                = "BssOpenAPI-V3"
	subscriptionSummaryAction = "GetSubscriptionSummary"
	browserUserAgent          = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
)

type WebConsoleStrategy struct {
	HTTPTimeout    float64
	ConsoleBaseURL string
	DashboardURL   string
	Region         string
	ProductCode    string
}

type sessionCredentials struct {
	Cookie       string `json:"cookie"`
	SecToken     string `json:"sec_token,omitempty"`
	Source       string `json:"source,omitempty"`
	BrowserLabel string `json:"browser,omitempty"`
}

const (
	sessionSourceEnvironment = "environment"
	sessionSourceManual      = "manual"
	sessionSourceBrowser     = "browser"
)

func (s *WebConsoleStrategy) IsAvailable() bool {
	if cookie := os.Getenv("MODELSTUDIO_COOKIE"); cookie != "" {
		return true
	}
	data, err := config.ReadCredential("modelstudio", "session")
	return (err == nil && len(data) > 0) || hasModelStudioBrowserProfiles()
}

func loadSessionCredential() (sessionCredentials, error) {
	if envCookie := strings.TrimSpace(os.Getenv("MODELSTUDIO_COOKIE")); envCookie != "" {
		return sessionCredentials{Cookie: envCookie, Source: sessionSourceEnvironment}, nil
	}

	data, err := config.ReadCredential("modelstudio", "session")
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
	return sessionCredentials{Cookie: string(data), Source: sessionSourceManual}, nil
}

func (s *WebConsoleStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	creds, err := loadSessionCredential()
	if err != nil {
		return fetch.ResultFail(fmt.Sprintf("Model Studio: reading stored browser session: %v", err)), nil
	}

	client := httpclient.NewFromConfig(s.HTTPTimeout)
	if creds.Cookie == "" {
		return s.fetchImportedBrowserSession(ctx, client, sessionCredentials{})
	}

	snapshot, err := s.fetchWithSession(ctx, client, creds)
	if err == nil {
		return fetch.ResultOK(*snapshot), nil
	}
	if !errors.Is(err, errModelStudioSession) {
		return fetch.ResultFail(fmt.Sprintf("Model Studio web console API failed: %v", err)), nil
	}
	if creds.Source == sessionSourceEnvironment {
		return fetch.ResultFatal(modelStudioSessionHint(creds, errBrowserCookieImportUnavailable)), nil
	}

	return s.fetchImportedBrowserSession(ctx, client, creds)
}

func (s *WebConsoleStrategy) fetchWithSession(
	ctx context.Context,
	client *httpclient.Client,
	creds sessionCredentials,
) (*models.UsageSnapshot, error) {
	consoleBaseURL := s.ConsoleBaseURL
	if consoleBaseURL == "" {
		consoleBaseURL = modelStudioConsoleBaseURL
	}
	dashboardURL := s.DashboardURL
	if dashboardURL == "" {
		dashboardURL = modelStudioDashboardURL
	}
	region := s.Region
	if region == "" {
		region = modelStudioRegion
	}
	productCode := s.ProductCode
	if productCode == "" {
		productCode = modelStudioTeamProduct
	}

	secToken := creds.SecToken
	if secToken == "" {
		secToken = extractCookieValue(creds.Cookie, "sec_token")
	}
	if secToken == "" {
		// The quota request does not always include sec_token in its Cookie
		// header. The console resolves it from this authenticated endpoint.
		secToken, _ = s.resolveSECToken(ctx, client, consoleBaseURL, creds.Cookie)
	}

	summaryData, err := s.callSubscriptionSummary(
		ctx,
		client,
		consoleBaseURL,
		dashboardURL,
		region,
		productCode,
		creds.Cookie,
		secToken,
	)
	if err != nil {
		return nil, err
	}

	snapshot, parseErr := parseModelStudioConsoleResponses(summaryData, nil, nil, nil, "web")
	if parseErr != nil {
		return nil, parseErr
	}
	return snapshot, nil
}

func (s *WebConsoleStrategy) fetchImportedBrowserSession(
	ctx context.Context,
	client *httpclient.Client,
	previous sessionCredentials,
) (fetch.FetchResult, error) {
	imported, importErr := importModelStudioBrowserSession(ctx)
	if importErr != nil {
		return fetch.ResultFatal(modelStudioSessionHint(previous, importErr)), nil
	}

	creds := sessionCredentials{
		Cookie:       imported.Cookie,
		Source:       sessionSourceBrowser,
		BrowserLabel: imported.SourceLabel,
	}
	snapshot, err := s.fetchWithSession(ctx, client, creds)
	if err != nil {
		if errors.Is(err, errModelStudioSession) {
			return fetch.ResultFatal(modelStudioSessionHint(creds, nil)), nil
		}
		return fetch.ResultFail(fmt.Sprintf("Model Studio web console API failed after importing %s cookies: %v", imported.SourceLabel, err)), nil
	}

	// Cache only a browser-imported session that has completed an authenticated
	// quota request. The credentials store is permission-restricted to the user.
	if data, err := json.Marshal(creds); err == nil {
		_ = config.WriteCredential("modelstudio", "session", data)
	}
	return fetch.ResultOK(*snapshot), nil
}

func modelStudioSessionHint(creds sessionCredentials, importErr error) string {
	switch creds.Source {
	case sessionSourceEnvironment:
		return "Model Studio browser session expired. Update MODELSTUDIO_COOKIE with a fresh Cookie header."
	case sessionSourceManual:
		return "Model Studio browser session expired. Sign in to the Singapore Team Token Plan page in Chrome or the Codex in-app browser, then retry; vibeusage will import fresh cookies automatically. You can also run vibeusage auth modelstudio to replace the Cookie header manually."
	case sessionSourceBrowser:
		browser := creds.BrowserLabel
		if browser == "" {
			browser = "your browser"
		}
		return fmt.Sprintf("Model Studio browser session expired. Sign in again in %s, then retry; vibeusage will re-import the cookies automatically.", browser)
	}
	if importErr != nil {
		return "No signed-in Model Studio browser session was found. Sign in to the Singapore Team Token Plan page in Chrome or the Codex in-app browser, then retry. If automatic cookie import is unavailable, run vibeusage auth modelstudio to paste the Cookie header manually."
	}
	return "Model Studio browser session expired. Sign in to the Singapore Team Token Plan page, then retry."
}

func (s *WebConsoleStrategy) resolveSECToken(
	ctx context.Context,
	client *httpclient.Client,
	consoleBaseURL string,
	cookie string,
) (string, error) {
	endpoint := strings.TrimRight(consoleBaseURL, "/") + "/tool/user/info.json"
	resp, err := client.DoCtx(
		ctx,
		http.MethodGet,
		endpoint,
		nil,
		httpclient.WithHeader("Accept", "application/json, text/plain, */*"),
		httpclient.WithHeader("Cookie", cookie),
		httpclient.WithHeader("Referer", strings.TrimRight(consoleBaseURL, "/")+"/"),
		httpclient.WithHeader("User-Agent", browserUserAgent),
	)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving sec_token: HTTP %d", resp.StatusCode)
	}

	secToken, err := parseConsoleSECToken(resp.Body)
	if err != nil {
		return "", fmt.Errorf("resolving sec_token: %w", err)
	}
	return secToken, nil
}

func (s *WebConsoleStrategy) callSubscriptionSummary(
	ctx context.Context,
	client *httpclient.Client,
	consoleBaseURL string,
	dashboardURL string,
	region string,
	productCode string,
	cookie string,
	secToken string,
) ([]byte, error) {
	endpoint, err := url.Parse(strings.TrimRight(consoleBaseURL, "/") + "/data/api.json")
	if err != nil {
		return nil, fmt.Errorf("building subscription summary URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("action", subscriptionSummaryAction)
	query.Set("product", bssProduct)
	query.Set("_tag", "")
	endpoint.RawQuery = query.Encode()

	form := url.Values{}
	form.Set("action", subscriptionSummaryAction)
	form.Set("product", bssProduct)
	form.Set("params", fmt.Sprintf(`{"ProductCode":"%s"}`, productCode))
	form.Set("region", region)
	if secToken != "" {
		form.Set("sec_token", secToken)
	}

	opts := []httpclient.RequestOption{
		httpclient.WithHeader("Accept", "*/*"),
		httpclient.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		httpclient.WithHeader("Cookie", cookie),
		httpclient.WithHeader("Origin", strings.TrimRight(consoleBaseURL, "/")),
		httpclient.WithHeader("Referer", dashboardURL),
		httpclient.WithHeader("User-Agent", browserUserAgent),
		httpclient.WithHeader("X-Requested-With", "XMLHttpRequest"),
	}
	if csrf := extractCookieValue(cookie, "login_aliyunid_csrf"); csrf != "" {
		opts = append(opts, httpclient.WithHeader("x-csrf-token", csrf), httpclient.WithHeader("x-xsrf-token", csrf))
	}

	resp, err := client.DoCtx(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()), opts...)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: HTTP %d", errModelStudioSession, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.URL != nil && isAlibabaLoginURL(resp.URL) {
		return nil, fmt.Errorf("%w: redirected to Alibaba login", errModelStudioSession)
	}
	if isLikelyLoginHTML(resp.Body) {
		return nil, fmt.Errorf("%w: Alibaba console returned a login page", errModelStudioSession)
	}

	return resp.Body, nil
}

func isAlibabaLoginURL(endpoint *url.URL) bool {
	if endpoint == nil {
		return false
	}
	normalized := strings.ToLower(endpoint.Hostname() + endpoint.Path)
	return strings.Contains(normalized, "passport") ||
		strings.Contains(normalized, "signin") ||
		strings.Contains(normalized, "login")
}

func extractCookieValue(cookieHeader, name string) string {
	parts := strings.Split(cookieHeader, ";")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, name+"=") {
			return strings.TrimPrefix(p, name+"=")
		}
	}
	return ""
}
