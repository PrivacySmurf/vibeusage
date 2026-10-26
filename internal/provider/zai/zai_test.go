package zai

import (
	"context"
	"testing"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestAPIKeyStrategy_IsAvailable_EnvVar(t *testing.T) {
	dirs := testenv.ApplyVibeusage(t, t.TempDir())
	t.Cleanup(func() { _ = dirs.Close() })
	t.Setenv("ZAI_API_KEY", "test-key")
	s := &APIKeyStrategy{}
	if !s.IsAvailable() {
		t.Error("IsAvailable() = false, want true when ZAI_API_KEY is set")
	}
}

func TestAPIKeyStrategy_IsAvailable_NoEnvVar(t *testing.T) {
	dirs := testenv.ApplyVibeusage(t, t.TempDir())
	t.Cleanup(func() { _ = dirs.Close() })
	t.Setenv("ZAI_API_KEY", "")
	s := &APIKeyStrategy{}
	if got := zaiAPIKey.Load(); got != "" {
		t.Fatalf("zaiAPIKey.Load() = %q, want empty in isolated fixture", got)
	}
	if s.IsAvailable() {
		t.Error("IsAvailable() = true, want false when no token available")
	}
}

func TestAPIKeyStrategy_Fetch_NoToken(t *testing.T) {
	dirs := testenv.ApplyVibeusage(t, t.TempDir())
	t.Cleanup(func() { _ = dirs.Close() })
	t.Setenv("ZAI_API_KEY", "")
	s := &APIKeyStrategy{}
	if got := zaiAPIKey.Load(); got != "" {
		t.Fatalf("zaiAPIKey.Load() = %q, want empty in isolated fixture", got)
	}

	result, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected non-successful result when no token available")
	}
}

func TestParseQuotaResponse_Source(t *testing.T) {
	resp := QuotaResponse{
		Code:    200,
		Success: true,
		Data: &QuotaData{
			Level: "pro",
			Limits: []QuotaLimit{
				{Type: "TOKENS_LIMIT", Unit: 3, Number: 5, Percentage: 10, NextResetTime: 1771661559241},
			},
		},
	}

	snapshot := parseQuotaResponse(resp)
	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Source != "api_key" {
		t.Errorf("Source = %q, want %q", snapshot.Source, "api_key")
	}
	if snapshot.Provider != "zai" {
		t.Errorf("Provider = %q, want %q", snapshot.Provider, "zai")
	}
}

func TestParseQuotaResponse_PeriodFields(t *testing.T) {
	resp := QuotaResponse{
		Code:    200,
		Success: true,
		Data: &QuotaData{
			Limits: []QuotaLimit{
				{Type: "TOKENS_LIMIT", Unit: 3, Number: 5, Percentage: 75, NextResetTime: 1771661559241},
			},
		},
	}

	snapshot := parseQuotaResponse(resp)
	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snapshot.Periods) != 1 {
		t.Fatalf("expected 1 period, got %d", len(snapshot.Periods))
	}

	p := snapshot.Periods[0]
	if p.Name != "Session (5h)" {
		t.Errorf("Name = %q, want %q", p.Name, "Session (5h)")
	}
	if p.Utilization != 75 {
		t.Errorf("Utilization = %d, want 75", p.Utilization)
	}
	if p.PeriodType != models.PeriodSession {
		t.Errorf("PeriodType = %q, want %q", p.PeriodType, models.PeriodSession)
	}
}

// TestParseQuotaResponse_CreditsPlan pins the credits-based plan shape
// (CREDIT_LIMIT windows) observed live from api.z.ai on 2026-10-21: usage is
// the window's credit allowance, currentValue the consumed credits.
func TestParseQuotaResponse_CreditsPlan(t *testing.T) {
	resp := QuotaResponse{
		Code:    200,
		Msg:     "Operation successful",
		Success: true,
		Data: &QuotaData{
			Level: "max",
			Limits: []QuotaLimit{
				{Type: "CREDIT_LIMIT", Unit: 3, Number: 5, Usage: 28000, CurrentValue: 15, Remaining: 27984, Percentage: 1, NextResetTime: 1792632041991},
				{Type: "CREDIT_LIMIT", Unit: 6, Number: 1, Usage: 140000, CurrentValue: 15, Remaining: 139984, Percentage: 1, NextResetTime: 1793218703984},
			},
		},
	}

	snapshot := parseQuotaResponse(resp)
	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Identity == nil || snapshot.Identity.Plan != "Max" {
		t.Errorf("Identity.Plan = %+v, want Max", snapshot.Identity)
	}
	if len(snapshot.Periods) != 2 {
		t.Fatalf("expected 2 periods, got %d", len(snapshot.Periods))
	}

	session := snapshot.Periods[0]
	if session.Name != "Session (5h) Credits" {
		t.Errorf("session Name = %q, want %q", session.Name, "Session (5h) Credits")
	}
	if session.PeriodType != models.PeriodSession {
		t.Errorf("session PeriodType = %q, want %q", session.PeriodType, models.PeriodSession)
	}
	if session.Used == nil || *session.Used != 15 {
		t.Errorf("session Used = %v, want 15", session.Used)
	}
	if session.Limit == nil || *session.Limit != 28000 {
		t.Errorf("session Limit = %v, want 28000", session.Limit)
	}

	weekly := snapshot.Periods[1]
	if weekly.Name != "Weekly Credits" {
		t.Errorf("weekly Name = %q, want %q", weekly.Name, "Weekly Credits")
	}
	if weekly.PeriodType != models.PeriodWeekly {
		t.Errorf("weekly PeriodType = %q, want %q", weekly.PeriodType, models.PeriodWeekly)
	}
	if weekly.Used == nil || *weekly.Used != 15 {
		t.Errorf("weekly Used = %v, want 15", weekly.Used)
	}
	if weekly.Limit == nil || *weekly.Limit != 140000 {
		t.Errorf("weekly Limit = %v, want 140000", weekly.Limit)
	}
}

func TestZai_Meta(t *testing.T) {
	z := Zai{}
	meta := z.Meta()
	if meta.ID != "zai" {
		t.Errorf("ID = %q, want %q", meta.ID, "zai")
	}
	if meta.Name != "Z.ai" {
		t.Errorf("Name = %q, want %q", meta.Name, "Z.ai")
	}
}
