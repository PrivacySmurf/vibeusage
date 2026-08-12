package grok

import (
	"context"
	"encoding/json"
	"strings"
	"time"

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
		EnvVars: []string{"GROK_SESSION_COOKIE"},
	}
}

func (g Grok) FetchStrategies() []fetch.Strategy {
	timeout := config.Get().Fetch.Timeout
	return []fetch.Strategy{&CookieStrategy{HTTPTimeout: timeout}}
}

func (g Grok) FetchStatus(_ context.Context) models.ProviderStatus {
	return models.ProviderStatus{Level: models.StatusUnknown}
}

func (g Grok) Auth() provider.AuthFlow {
	return provider.ManualKeyAuthFlow{
		Instructions: "Get your Grok session cookie:\n" +
			"  1. Open https://grok.com in your browser and sign in\n" +
			"  2. Open DevTools (F12 or Cmd+Option+I)\n" +
			"  3. Go to Application → Cookies → https://grok.com\n" +
			"  4. Find the 'sct' cookie (or 'auth_token' if sct is absent)\n" +
			"  5. Copy its value",
		Placeholder: "paste cookie value here",
		Validate:    provider.ValidateNotEmpty,
		ProviderID:  "grok",
		CredType:    "session",
		JSONKey:     "session_cookie",
		Save:        saveGrokCredential,
	}
}

func saveGrokCredential(value string) error {
	value = strings.TrimSpace(value)
	content, _ := json.Marshal(map[string]string{"session_cookie": value})
	return config.WriteCredential("grok", "session", content)
}

func init() {
	provider.Register(Grok{})
}

const (
	rateLimitsURL = "https://grok.com/rest/rate-limits"
)

// grokSessionCred loads the Grok session cookie from credentials or env.
var grokSessionCred = provider.APIKeySource{
	EnvVars:    []string{"GROK_SESSION_COOKIE"},
	ProviderID: "grok",
	CredType:   "session",
	JSONKeys:   []string{"session_cookie"},
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
			Name:        m.label + " (" + m.key + ")",
			Utilization: rateLimits.Utilization(),
			PeriodType:  inferPeriodType(rateLimits.WindowType),
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

// inferPeriodType maps Grok's windowType string to a vibeusage PeriodType.
func inferPeriodType(windowType string) models.PeriodType {
	switch strings.ToUpper(windowType) {
	case "HOUR", "2HOUR", "2H":
		return models.PeriodSession
	case "DAY", "DAILY":
		return models.PeriodDaily
	case "WEEK", "WEEKLY":
		return models.PeriodWeekly
	case "MONTH", "MONTHLY":
		return models.PeriodMonthly
	default:
		// Grok free plan uses 2-hour windows for fast queries.
		return models.PeriodSession
	}
}
