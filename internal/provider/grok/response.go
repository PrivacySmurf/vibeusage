package grok

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

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

// BillingResponse is the response shape from GET https://cli-chat-proxy.grok.com/v1/billing?format=credits.
type BillingResponse struct {
	SubscriptionTier  string        `json:"subscriptionTier"`
	Subscription_Tier string        `json:"subscription_tier"`
	Config            BillingConfig `json:"config"`
}

type BillingConfig struct {
	CreditUsagePercent float64          `json:"creditUsagePercent"`
	BillingPeriodStart string           `json:"billingPeriodStart"`
	BillingPeriodEnd   string           `json:"billingPeriodEnd"`
	CurrentPeriod      BillingPeriod    `json:"currentPeriod"`
	ProductUsage       []BillingProduct `json:"productUsage"`
}

type BillingPeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type BillingProduct struct {
	Product      string  `json:"product"`
	UsagePercent float64 `json:"usagePercent"`
}

func parseBillingResponse(data []byte) ([]models.UsagePeriod, error) {
	var resp BillingResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing grok billing response: %w", err)
	}

	periodType := models.PeriodWeekly
	if strings.Contains(strings.ToUpper(resp.Config.CurrentPeriod.Type), "MONTH") {
		periodType = models.PeriodMonthly
	}

	resetsAtStr := resp.Config.CurrentPeriod.End
	if resetsAtStr == "" {
		resetsAtStr = resp.Config.BillingPeriodEnd
	}

	var resetsAt *time.Time
	if resetsAtStr != "" {
		if t, err := time.Parse(time.RFC3339, resetsAtStr); err == nil {
			resetsAt = &t
		} else if t, err := time.Parse("2006-01-02T15:04:05Z07:00", resetsAtStr); err == nil {
			resetsAt = &t
		}
	}

	overallPct := int(math.Round(resp.Config.CreditUsagePercent))
	if overallPct < 0 {
		overallPct = 0
	} else if overallPct > 100 {
		overallPct = 100
	}

	periodLabel := "Weekly"
	if periodType == models.PeriodMonthly {
		periodLabel = "Monthly"
	}

	var periods []models.UsagePeriod

	// Primary overall period
	periods = append(periods, models.UsagePeriod{
		Name:        periodLabel,
		Utilization: overallPct,
		PeriodType:  periodType,
		ResetsAt:    resetsAt,
	})

	// Add product breakdown categories
	for _, p := range resp.Config.ProductUsage {
		pct := int(math.Round(p.UsagePercent))
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		displayName := grokProductDisplayName(p.Product)
		periods = append(periods, models.UsagePeriod{
			Name:        displayName + " (" + periodLabel + ")",
			Utilization: pct,
			PeriodType:  periodType,
			ResetsAt:    resetsAt,
		})
	}

	return periods, nil
}

func grokProductDisplayName(raw string) string {
	s := strings.ToLower(raw)
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")

	switch s {
	case "grokbuild", "build":
		return "Grok Build"
	case "grokappbuilder", "appbuilder":
		return "App Builder"
	case "grokchat", "chat":
		return "Chat"
	case "grokimagine", "imagine", "image", "images":
		return "Imagine"
	case "grokplugins", "plugins", "plugin":
		return "Grok Plugins"
	case "voice", "grokvoice":
		return "Voice"
	case "api", "grokapi":
		return "API"
	case "thirdparty", "3rdparty", "third":
		return "3rd Party"
	}

	if strings.Contains(s, "appbuilder") {
		return "App Builder"
	}
	if strings.Contains(s, "third") || strings.Contains(s, "3rd") {
		return "3rd Party"
	}
	if strings.Contains(s, "plugin") {
		return "Grok Plugins"
	}
	if strings.Contains(s, "imagine") || strings.Contains(s, "image") {
		return "Imagine"
	}
	if strings.Contains(s, "voice") {
		return "Voice"
	}
	if strings.Contains(s, "chat") {
		return "Chat"
	}
	if strings.Contains(s, "api") {
		return "API"
	}
	if strings.Contains(s, "build") {
		return "Grok Build"
	}

	return raw
}
