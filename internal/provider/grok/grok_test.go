package grok

import (
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

func TestParseBillingResponse(t *testing.T) {
	jsonResponse := []byte(`{
		"subscriptionTier": "SuperGrok Heavy",
		"config": {
			"creditUsagePercent": 42.5,
			"billingPeriodStart": "2026-08-01T00:00:00Z",
			"billingPeriodEnd": "2026-08-08T00:00:00Z",
			"currentPeriod": {
				"type": "WEEKLY",
				"start": "2026-08-01T00:00:00Z",
				"end": "2026-08-08T00:00:00Z"
			},
			"productUsage": [
				{ "product": "GrokChat", "usagePercent": 15.2 },
				{ "product": "GrokBuild", "usagePercent": 40.8 },
				{ "product": "GrokImagine", "usagePercent": 0 }
			]
		}
	}`)

	periods, err := parseBillingResponse(jsonResponse)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(periods) != 4 {
		t.Fatalf("expected 4 periods, got %d", len(periods))
	}

	// Primary period
	main := periods[0]
	if main.Name != "Weekly" {
		t.Errorf("expected name Weekly, got %s", main.Name)
	}
	if main.Utilization != 43 {
		t.Errorf("expected utilization 43, got %d", main.Utilization)
	}
	if main.PeriodType != models.PeriodWeekly {
		t.Errorf("expected PeriodWeekly, got %s", main.PeriodType)
	}
	if main.ResetsAt == nil || main.ResetsAt.Format(time.RFC3339) != "2026-08-08T00:00:00Z" {
		t.Errorf("expected reset at 2026-08-08T00:00:00Z, got %v", main.ResetsAt)
	}

	// Product breakdown
	expectedProducts := map[string]int{
		"Chat (Weekly)":       15,
		"Grok Build (Weekly)": 41,
		"Imagine (Weekly)":    0,
	}

	for _, p := range periods[1:] {
		expectedUtil, ok := expectedProducts[p.Name]
		if !ok {
			t.Errorf("unexpected product period name: %s", p.Name)
			continue
		}
		if p.Utilization != expectedUtil {
			t.Errorf("product %s expected util %d, got %d", p.Name, expectedUtil, p.Utilization)
		}
	}
}

func TestGrokProductDisplayName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"grokbuild", "Grok Build"},
		{"GrokAppBuilder", "App Builder"},
		{"chat", "Chat"},
		{"grok_imagine", "Imagine"},
		{"plugins", "Grok Plugins"},
		{"GrokVoice", "Voice"},
		{"grokapi", "API"},
		{"3rdParty", "3rd Party"},
		{"custom_feature", "custom_feature"},
	}

	for _, tt := range tests {
		got := grokProductDisplayName(tt.input)
		if got != tt.expected {
			t.Errorf("grokProductDisplayName(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}
