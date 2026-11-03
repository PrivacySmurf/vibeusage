package venice

import (
	"context"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

type Venice struct{}

func (v Venice) Meta() provider.Metadata {
	return provider.Metadata{
		ID:           "venice",
		Name:         "Venice",
		Description:  "Venice AI privacy-preserving inference API",
		Homepage:     "https://venice.ai",
		StatusURL:    "https://veniceai-status.com",
		DashboardURL: "https://venice.ai/settings/api",
	}
}

func (v Venice) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{
		EnvVars: []string{"VENICE_API_KEY", "VENICE_AI_API_KEY"},
	}
}

func (v Venice) FetchStrategies() []fetch.Strategy {
	timeout := config.Get().Fetch.Timeout
	return []fetch.Strategy{&APIKeyStrategy{HTTPTimeout: timeout}}
}

func (v Venice) FetchStatus(ctx context.Context) models.ProviderStatus {
	return provider.FetchStatuspageStatus(ctx, "https://veniceai-status.com")
}

func (v Venice) Auth() provider.AuthFlow {
	return provider.ManualKeyAuthFlow{
		Instructions: "Get your Venice API key:\n" +
			"  1. Open https://venice.ai/settings/api\n" +
			"  2. Create or copy an API key",
		Placeholder: "VENICE_INFERENCE_KEY_...",
		Validate:    provider.ValidateNotEmpty,
		ProviderID:  "venice",
		CredType:    "apikey",
		JSONKey:     "api_key",
	}
}

func init() {
	provider.Register(Venice{})
}

var rateLimitsURL = "https://api.venice.ai/api/v1/api_keys/rate_limits"

// APIKeyStrategy fetches Venice balances and tier info from /api/v1/api_keys/rate_limits.
type APIKeyStrategy struct {
	HTTPTimeout float64
}

var veniceAPIKey = provider.APIKeySource{
	EnvVars:    []string{"VENICE_API_KEY", "VENICE_AI_API_KEY"},
	ProviderID: "venice",
	CredType:   "apikey",
	JSONKeys:   []string{"api_key"},
}

func (s *APIKeyStrategy) IsAvailable() bool {
	return veniceAPIKey.Load() != ""
}

func (s *APIKeyStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	token := veniceAPIKey.Load()
	if token == "" {
		return fetch.ResultFail("No API key found. Set VENICE_API_KEY or use 'vibeusage auth venice'"), nil
	}
	return fetchRateLimits(ctx, token, s.HTTPTimeout)
}

func fetchRateLimits(ctx context.Context, token string, httpTimeout float64) (fetch.FetchResult, error) {
	client := httpclient.NewFromConfig(httpTimeout)
	var parsed RateLimitsResponse
	resp, err := client.GetJSONCtx(ctx, rateLimitsURL, &parsed, httpclient.WithBearer(token))
	if err != nil {
		return fetch.ResultFail("Request failed: " + err.Error()), nil
	}

	if r := provider.CheckResponse(resp, "venice", "Venice"); r != nil {
		return *r, nil
	}

	snapshot, err := parseRateLimitsSnapshot(parsed)
	if err != nil {
		return fetch.ResultFail("Failed to parse Venice rate limits response: " + err.Error()), nil
	}
	return fetch.ResultOK(*snapshot), nil
}

func parseRateLimitsSnapshot(resp RateLimitsResponse) (*models.UsageSnapshot, error) {
	usdBalance := resp.Data.Balances.USD + resp.Data.Balances.BundledCredits
	billing := &models.BillingDetail{
		Balance: &usdBalance,
	}
	if resp.Data.Balances.DIEM > 0 {
		diem := resp.Data.Balances.DIEM
		billing.Credits = &diem
	}

	var identity *models.ProviderIdentity
	if tier := strings.TrimSpace(resp.Data.APITier.ID); tier != "" {
		identity = &models.ProviderIdentity{Plan: tier}
	}

	snapshot := &models.UsageSnapshot{
		Provider:  "venice",
		FetchedAt: time.Now().UTC(),
		Billing:   billing,
		Identity:  identity,
		Source:    "api_key",
	}
	return snapshot, nil
}
