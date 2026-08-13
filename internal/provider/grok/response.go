package grok

import "time"

// RateLimitsRequest is the body sent to POST /rest/rate-limits.
type RateLimitsRequest struct {
	RequestKind string `json:"requestKind"`
	ModelName   string `json:"modelName"`
}

// RateLimitsResponse is the JSON response from POST /rest/rate-limits.
type RateLimitsResponse struct {
	TotalQueries      int    `json:"totalQueries"`
	RemainingQueries  int    `json:"remainingQueries"`
	WindowSizeSeconds int    `json:"windowSizeSeconds,omitempty"`
	WindowType        string `json:"windowType,omitempty"`
	ResetTime         *int64 `json:"resetTime,omitempty"` // Unix milliseconds
}

// ResetsAt returns an approximate reset time. If the API provides an explicit
// reset timestamp it is used; otherwise we approximate from now + window size.
func (r RateLimitsResponse) ResetsAt() *time.Time {
	if r.ResetTime != nil && *r.ResetTime > 0 {
		ms := *r.ResetTime
		t := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
		return &t
	}
	if r.WindowSizeSeconds > 0 {
		t := time.Now().UTC().Add(time.Duration(r.WindowSizeSeconds) * time.Second)
		return &t
	}
	return nil
}

// Utilization returns the percentage of queries used (0–100).
func (r RateLimitsResponse) Utilization() int {
	if r.TotalQueries <= 0 {
		return 0
	}
	used := r.TotalQueries - r.RemainingQueries
	if used < 0 {
		used = 0
	}
	pct := used * 100 / r.TotalQueries
	if pct > 100 {
		pct = 100
	}
	return pct
}
