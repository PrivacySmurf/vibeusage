package grok

import "time"

// RateLimitsRequest is the body sent to POST /rest/rate-limits.
type RateLimitsRequest struct {
	RequestKind string `json:"requestKind"`
	ModelName   string `json:"modelName"`
}

// RateLimitsResponse is the JSON response from POST /rest/rate-limits.
type RateLimitsResponse struct {
	TotalQueries     int    `json:"totalQueries"`
	RemainingQueries int    `json:"remainingQueries"`
	WindowType       string `json:"windowType,omitempty"`
	ResetTime        *int64 `json:"resetTime,omitempty"` // Unix milliseconds
}

// ResetsAt converts the reset timestamp to a time.Time, if present.
func (r RateLimitsResponse) ResetsAt() *time.Time {
	if r.ResetTime == nil || *r.ResetTime <= 0 {
		return nil
	}
	ms := *r.ResetTime
	t := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
	return &t
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
