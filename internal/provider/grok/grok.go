package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/auth/device"
	"github.com/joshuadavidthomas/vibeusage/internal/auth/oauth"
	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

type Grok struct{}

func (g Grok) Meta() provider.Metadata {
	return provider.Metadata{
		ID:           "grok",
		Name:         "Grok",
		Description:  "xAI Grok assistant",
		Homepage:     "https://grok.com",
		DashboardURL: "https://grok.com",
	}
}

func (g Grok) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{
		EnvVars:  []string{"GROK_OAUTH_TOKEN", "GROK_SESSION_COOKIE"},
		CLIPaths: externalOAuthPaths(),
	}
}

func (g Grok) FetchStrategies() []fetch.Strategy {
	timeout := config.Get().Fetch.Timeout
	return []fetch.Strategy{
		&OAuthStrategy{HTTPTimeout: timeout},
		&CookieStrategy{HTTPTimeout: timeout},
	}
}

func (g Grok) FetchStatus(_ context.Context) models.ProviderStatus {
	return models.ProviderStatus{Level: models.StatusUnknown}
}

// AcceptCredential stores a raw credential (session cookie or OAuth token).
func (g Grok) AcceptCredential(credential string) error {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return fmt.Errorf("credential cannot be empty")
	}

	if strings.HasPrefix(credential, "{") || strings.HasPrefix(credential, "ey") {
		var creds oauth.Credentials
		if strings.HasPrefix(credential, "{") {
			_ = json.Unmarshal([]byte(credential), &creds)
		}
		if creds.AccessToken == "" && strings.HasPrefix(credential, "ey") {
			creds.AccessToken = credential
		}
		if creds.AccessToken != "" {
			bytes, err := json.Marshal(creds)
			if err != nil {
				return fmt.Errorf("marshal grok oauth credentials: %w", err)
			}
			return config.WriteCredential("grok", "oauth", bytes)
		}
	}

	content, err := json.Marshal(map[string]string{"session_cookie": credential})
	if err != nil {
		return fmt.Errorf("marshal grok session cookie: %w", err)
	}
	return config.WriteCredential("grok", "session", content)
}

func (g Grok) Auth() provider.AuthFlow {
	return provider.DeviceAuthFlow{
		Config: device.Config{
			DeviceCodeURL: oauthDeviceCodeURL,
			DeviceCodeParams: map[string]string{
				"client_id": clientID,
				"scope":     "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write",
				"referrer":  "grok-build",
			},
			TokenURL: oauthTokenURL,
			TokenParams: map[string]string{
				"client_id":  clientID,
				"grant_type": "urn:ietf:params:oauth:grant-type:device_code",
			},
			HTTPOptions: []httpclient.RequestOption{
				httpclient.WithHeader("x-grok-client-version", "grld-macos-oauth"),
				httpclient.WithHeader("x-grok-client-surface", "ui"),
				httpclient.WithHeader("Accept", "application/json"),
			},
			HTTPTimeout:     config.Get().Fetch.Timeout,
			ProviderID:      "grok",
			CredType:        "oauth",
			ShowRefreshHint: true,
		},
	}
}

func init() {
	provider.Register(Grok{})
}

const (
	rateLimitsURL      = "https://grok.com/rest/rate-limits"
	billingURL         = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	oauthTokenURL      = "https://auth.x.ai/oauth2/token"
	oauthDeviceCodeURL = "https://auth.x.ai/oauth2/device/code"
	clientID           = "b1a00492-073a-47ea-816f-4c329264a828"
)

var grokOAuthCred = provider.APIKeySource{
	EnvVars:    []string{"GROK_OAUTH_TOKEN"},
	ProviderID: "grok",
	CredType:   "oauth",
	JSONKeys:   []string{"access_token", "accessToken", "token"},
}

var grokSessionCred = provider.APIKeySource{
	EnvVars:    []string{"GROK_SESSION_COOKIE"},
	ProviderID: "grok",
	CredType:   "session",
	JSONKeys:   []string{"session_cookie"},
}

func externalOAuthPaths() []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, "Library", "Application Support", "GRLD", "oidc.json"))
		paths = append(paths, filepath.Join(home, ".config", "grok", "oauth.json"))
		paths = append(paths, filepath.Join(home, ".grok", "oauth.json"))
	}
	return paths
}

// OAuthStrategy fetches Grok usage via the OAuth billing API endpoint.
type OAuthStrategy struct {
	HTTPTimeout float64
}

func (s *OAuthStrategy) IsAvailable() bool {
	if grokOAuthCred.Load() != "" {
		return true
	}
	for _, p := range externalOAuthPaths() {
		if fileExists(p) {
			return true
		}
	}
	return false
}

func (s *OAuthStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	token, sourceName, err := loadOrRefreshOAuthToken(ctx, s.HTTPTimeout)
	if err != nil || token == "" {
		return fetch.ResultFail("Grok: no valid OAuth token found"), nil
	}

	client := httpclient.NewFromConfig(s.HTTPTimeout)
	var billingResp BillingResponse
	resp, err := client.GetJSONCtx(ctx, billingURL, &billingResp,
		httpclient.WithHeader("Authorization", "Bearer "+token),
		httpclient.WithHeader("X-XAI-Token-Auth", "xai-grok-cli"),
		httpclient.WithHeader("Accept", "application/json"),
		httpclient.WithHeader("User-Agent", "GRLD-macOS/oauth"),
	)
	if err != nil {
		return fetch.ResultFail(fmt.Sprintf("Grok billing fetch failed: %v", err)), nil
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fetch.ResultFatal("Grok: unauthorized — OAuth token expired or invalid"), nil
	}
	if resp.StatusCode != 200 || resp.JSONErr != nil {
		return fetch.ResultFail(fmt.Sprintf("Grok billing returned HTTP %d", resp.StatusCode)), nil
	}

	periods, err := parseBillingResponse(resp.Body)
	if err != nil || len(periods) == 0 {
		return fetch.ResultFail("Grok: could not parse billing response"), nil
	}

	snapshot := models.UsageSnapshot{
		Provider:  "grok",
		FetchedAt: time.Now().UTC(),
		Periods:   periods,
		Source:    sourceName,
	}
	return fetch.ResultOK(snapshot), nil
}

func loadOrRefreshOAuthToken(ctx context.Context, timeout float64) (string, string, error) {
	// 1. Check stored vibeusage OAuth credentials
	data, _ := config.ReadCredential("grok", "oauth")
	if len(data) > 0 {
		var creds oauth.Credentials
		if err := json.Unmarshal(data, &creds); err == nil && creds.AccessToken != "" {
			if creds.NeedsRefresh() && creds.RefreshToken != "" {
				refreshed := oauth.Refresh(ctx, creds.RefreshToken, oauth.RefreshConfig{
					TokenURL: oauthTokenURL,
					FormFields: map[string]string{
						"client_id": clientID,
					},
					Headers: []httpclient.RequestOption{
						httpclient.WithHeader("x-grok-client-version", "grld-macos-oauth"),
						httpclient.WithHeader("x-grok-client-surface", "ui"),
						httpclient.WithHeader("Accept", "application/json"),
					},
					Save: func(c *oauth.Credentials) error {
						bytes, err := json.Marshal(c)
						if err != nil {
							return err
						}
						return config.WriteCredential("grok", "oauth", bytes)
					},
					HTTPTimeout: timeout,
				})
				if refreshed != nil {
					return refreshed.AccessToken, "oauth", nil
				}
			}
			return creds.AccessToken, "oauth", nil
		}
	}

	// 2. Check GROK_OAUTH_TOKEN env var
	if envToken := strings.TrimSpace(os.Getenv("GROK_OAUTH_TOKEN")); envToken != "" {
		return envToken, "env", nil
	}

	// 3. Check external CLI / GRLD files
	for _, p := range externalOAuthPaths() {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var grldStruct struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    string `json:"expiresAt"`
			Token        string `json:"access_token"`
		}
		if err := json.Unmarshal(b, &grldStruct); err == nil {
			tok := grldStruct.AccessToken
			if tok == "" {
				tok = grldStruct.Token
			}
			if tok != "" {
				creds := oauth.Credentials{
					AccessToken:  tok,
					RefreshToken: grldStruct.RefreshToken,
					ExpiresAt:    grldStruct.ExpiresAt,
				}
				if creds.NeedsRefresh() && creds.RefreshToken != "" {
					refreshed := oauth.Refresh(ctx, creds.RefreshToken, oauth.RefreshConfig{
						TokenURL: oauthTokenURL,
						FormFields: map[string]string{
							"client_id": clientID,
						},
						Headers: []httpclient.RequestOption{
							httpclient.WithHeader("x-grok-client-version", "grld-macos-oauth"),
							httpclient.WithHeader("x-grok-client-surface", "ui"),
							httpclient.WithHeader("Accept", "application/json"),
						},
						Save: func(c *oauth.Credentials) error {
							bytes, err := json.Marshal(c)
							if err != nil {
								return err
							}
							return config.WriteCredential("grok", "oauth", bytes)
						},
						HTTPTimeout: timeout,
					})
					if refreshed != nil {
						return refreshed.AccessToken, "grld", nil
					}
				}
				return tok, "grld", nil
			}
		}
	}

	return "", "", nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CookieStrategy fetches Grok usage using a browser session cookie.
type CookieStrategy struct {
	HTTPTimeout float64
}

func (s *CookieStrategy) IsAvailable() bool {
	return grokSessionCred.Load() != ""
}

// models to query in order — extend if xAI adds more rate-limited models.
var grokModels = []struct {
	key   string
	label string
}{
	{"grok-3", "Fast"},
	{"grok-3-reasoning", "Reasoning"},
}

func (s *CookieStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	cookie := grokSessionCred.Load()
	if cookie == "" {
		return fetch.ResultFail("No session cookie found"), nil
	}

	client := httpclient.NewFromConfig(s.HTTPTimeout)

	// Determine the cookie name. If the value looks like a full "key=value" pair,
	// use it verbatim as the Cookie header; otherwise default to "sct".
	cookieHeader := buildCookieHeader(cookie)

	var periods []models.UsagePeriod

	for _, m := range grokModels {
		body := RateLimitsRequest{
			RequestKind: "DEFAULT",
			ModelName:   m.key,
		}

		var rateLimits RateLimitsResponse
		resp, err := client.PostJSONCtx(ctx, rateLimitsURL, body, &rateLimits,
			httpclient.WithHeader("Cookie", cookieHeader),
			httpclient.WithHeader("Referer", "https://grok.com/"),
			httpclient.WithHeader("Origin", "https://grok.com"),
		)
		if err != nil {
			continue
		}
		if resp.StatusCode == 401 {
			return fetch.ResultFatal("Grok: unauthorized — session cookie may be expired"), nil
		}
		if resp.StatusCode != 200 || resp.JSONErr != nil {
			continue
		}
		if rateLimits.TotalQueries <= 0 {
			continue
		}

		period := models.UsagePeriod{
			Name:        periodName(m.label, m.key, rateLimits.WindowSizeSeconds),
			Utilization: rateLimits.Utilization(),
			PeriodType:  inferPeriodType(rateLimits.WindowType, rateLimits.WindowSizeSeconds),
			ResetsAt:    rateLimits.ResetsAt(),
			Model:       m.key,
		}
		used := rateLimits.TotalQueries - rateLimits.RemainingQueries
		if used < 0 {
			used = 0
		}
		period.Used = &used
		period.Limit = &rateLimits.TotalQueries
		periods = append(periods, period)
	}

	if len(periods) == 0 {
		return fetch.ResultFail("Grok: no rate-limit data returned — check your session cookie"), nil
	}

	now := time.Now().UTC()
	snapshot := models.UsageSnapshot{
		Provider:  "grok",
		FetchedAt: now,
		Periods:   periods,
		Source:    "cookie",
	}
	return fetch.ResultOK(snapshot), nil
}

// buildCookieHeader turns the stored credential into a Cookie header value.
// If the user stored a raw key=value pair (e.g. "sct=abc123"), use it directly.
// Otherwise wrap it as "sct=<value>".
func buildCookieHeader(value string) string {
	if strings.Contains(value, "=") {
		return value
	}
	return "sct=" + value
}

// periodName builds a human-readable name for a Grok usage period.
// Uses "(Nh)" suffix when window size is known so the frontend session parser
// can extract the duration from the name pattern.
func periodName(label, modelKey string, windowSecs int) string {
	if windowSecs > 0 {
		hours := windowSecs / 3600
		if hours > 0 {
			return fmt.Sprintf("%s (%dh)", label, hours)
		}
	}
	return label + " (" + modelKey + ")"
}

// inferPeriodType infers period type from the windowType string and/or windowSizeSeconds.
func inferPeriodType(windowType string, windowSecs int) models.PeriodType {
	switch strings.ToUpper(windowType) {
	case "DAY", "DAILY":
		return models.PeriodDaily
	case "WEEK", "WEEKLY":
		return models.PeriodWeekly
	case "MONTH", "MONTHLY":
		return models.PeriodMonthly
	}
	// Fall back to duration-based inference.
	switch {
	case windowSecs <= 0 || windowSecs <= 6*3600:
		return models.PeriodSession // ≤6h → session
	case windowSecs <= 25*3600:
		return models.PeriodDaily
	case windowSecs <= 8*24*3600:
		return models.PeriodWeekly
	default:
		return models.PeriodMonthly
	}
}

