package venice

// RateLimitsResponse represents the response from the Venice rate limits and
// balances endpoint (GET /api/v1/api_keys/rate_limits).
type RateLimitsResponse struct {
	Data RateLimitsData `json:"data"`
}

// RateLimitsData contains API key access status, tier, remaining balances, and
// per-model rate limits.
type RateLimitsData struct {
	AccessPermitted bool            `json:"accessPermitted"`
	APITier         APITier         `json:"apiTier"`
	Balances        Balances        `json:"balances"`
	KeyExpiration   *string         `json:"keyExpiration"`
	NextEpochBegins *string         `json:"nextEpochBegins"`
	RateLimits      []ModelRateItem `json:"rateLimits,omitempty"`
}

// APITier describes the billing tier of the Venice API key.
type APITier struct {
	ID        string `json:"id"`
	IsCharged bool   `json:"isCharged"`
}

// Balances holds remaining USD, DIEM, and bundled credit balances.
type Balances struct {
	USD            float64 `json:"USD"`
	DIEM           float64 `json:"DIEM"`
	BundledCredits float64 `json:"BUNDLED_CREDITS"`
}

// ModelRateItem describes the rate limits for one model.
type ModelRateItem struct {
	APIModelID string      `json:"apiModelId"`
	RateLimits []RateLimit `json:"rateLimits,omitempty"`
}

// RateLimit describes a single RPM, RPD, or TPM limit.
type RateLimit struct {
	Amount float64 `json:"amount"`
	Type   string  `json:"type"`
}
