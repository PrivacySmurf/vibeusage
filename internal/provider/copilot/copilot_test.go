package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/auth/oauth"
	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestCredentialSources_ListsUsableEnvironmentTokens(t *testing.T) {
	info := (Copilot{}).CredentialSources()
	want := []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
	if len(info.EnvVars) != len(want) {
		t.Fatalf("EnvVars = %v, want %v", info.EnvVars, want)
	}
	for i := range want {
		if info.EnvVars[i] != want[i] {
			t.Fatalf("EnvVars = %v, want %v", info.EnvVars, want)
		}
	}
	if len(info.CLIPaths) != 0 {
		t.Fatalf("CLIPaths = %v, want none", info.CLIPaths)
	}
}

func TestDeviceFlowStrategy_LoadsStoredCredentialThenEnvironmentTokensInOrder(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	for _, envVar := range copilotTokenEnvVars {
		t.Setenv(envVar, "")
	}

	strategy := &DeviceFlowStrategy{}
	if strategy.IsAvailable() {
		t.Fatal("strategy should be unavailable without credentials")
	}

	stored := &oauth.Credentials{AccessToken: "stored-token"}
	content, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("Marshal stored credentials: %v", err)
	}
	if err := config.WriteCredential("copilot", "oauth", content); err != nil {
		t.Fatalf("WriteCredential: %v", err)
	}
	got, source := strategy.loadCredentials()
	if got == nil || got.AccessToken != "stored-token" || source != "device_flow" {
		t.Fatalf("stored credentials = %#v from %q, want stored-token from device_flow", got, source)
	}
	config.DeleteCredential("copilot", "oauth")

	t.Setenv("GITHUB_TOKEN", " github-token ")
	got, source = strategy.loadCredentials()
	if got == nil || got.AccessToken != "github-token" || source != "github_token" {
		t.Fatalf("GITHUB_TOKEN credentials = %#v from %q, want github-token from github_token", got, source)
	}

	t.Setenv("GH_TOKEN", "gh-token")
	got, source = strategy.loadCredentials()
	if got == nil || got.AccessToken != "gh-token" || source != "github_token" {
		t.Fatalf("GH_TOKEN credentials = %#v from %q, want gh-token from github_token", got, source)
	}

	t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-token")
	got, source = strategy.loadCredentials()
	if got == nil || got.AccessToken != "copilot-token" || source != "github_token" {
		t.Fatalf("COPILOT_GITHUB_TOKEN credentials = %#v from %q, want copilot-token from github_token", got, source)
	}
}

func TestFetch_UsesEnvironmentTokenAndLabelsSnapshot(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	for _, envVar := range copilotTokenEnvVars {
		t.Setenv(envVar, "")
	}
	t.Setenv("GITHUB_TOKEN", "github-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer github-token" {
			t.Errorf("Authorization = %q, want Bearer github-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"copilot_plan":"pro","quota_snapshots":{"chat":{"entitlement":100,"remaining":50}}}`))
	}))
	defer server.Close()

	oldUsageURL := copilotUsageURL
	copilotUsageURL = server.URL
	t.Cleanup(func() { copilotUsageURL = oldUsageURL })

	result, err := (&DeviceFlowStrategy{HTTPTimeout: 1}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !result.Success || result.Snapshot == nil {
		t.Fatalf("Fetch result = %#v, want successful snapshot", result)
	}
	if result.Snapshot.Source != "github_token" {
		t.Errorf("snapshot source = %q, want github_token", result.Snapshot.Source)
	}
}

func TestParseTypedUsageResponse_FullResponse(t *testing.T) {
	resp := UserResponse{
		Login:             "testuser",
		QuotaResetDateUTC: "2025-03-01T00:00:00Z",
		CopilotPlan:       "pro",
		QuotaSnapshots: &QuotaSnapshots{
			PremiumInteractions: &Quota{Entitlement: 100, Remaining: 60, Unlimited: false},
			Chat:                &Quota{Entitlement: 500, Remaining: 300, Unlimited: false},
			Completions:         &Quota{Entitlement: 0, Remaining: 0, Unlimited: true},
		},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Provider != "copilot" {
		t.Errorf("provider = %q, want %q", snapshot.Provider, "copilot")
	}
	if snapshot.Source != "device_flow" {
		t.Errorf("source = %q, want %q", snapshot.Source, "device_flow")
	}

	if len(snapshot.Periods) != 3 {
		t.Fatalf("len(periods) = %d, want 3", len(snapshot.Periods))
	}

	premium := snapshot.Periods[0]
	if premium.Name != "Monthly (Premium)" {
		t.Errorf("period[0].name = %q, want %q", premium.Name, "Monthly (Premium)")
	}
	if premium.Utilization != 40 {
		t.Errorf("premium utilization = %d, want 40", premium.Utilization)
	}
	if premium.PeriodType != models.PeriodMonthly {
		t.Errorf("premium period_type = %q, want %q", premium.PeriodType, models.PeriodMonthly)
	}
	if premium.ResetsAt == nil {
		t.Fatal("expected resets_at")
	}
	expectedReset := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	if !premium.ResetsAt.Equal(expectedReset) {
		t.Errorf("resets_at = %v, want %v", premium.ResetsAt, expectedReset)
	}

	chat := snapshot.Periods[1]
	if chat.Name != "Monthly (Chat)" {
		t.Errorf("period[1].name = %q, want %q", chat.Name, "Monthly (Chat)")
	}
	if chat.Utilization != 40 {
		t.Errorf("chat utilization = %d, want 40", chat.Utilization)
	}

	completions := snapshot.Periods[2]
	if completions.Name != "Monthly (Completions)" {
		t.Errorf("period[2].name = %q, want %q", completions.Name, "Monthly (Completions)")
	}
	if completions.Utilization != 0 {
		t.Errorf("completions utilization = %d, want 0", completions.Utilization)
	}

	if snapshot.Identity == nil {
		t.Fatal("expected identity")
	}
	if snapshot.Identity.Plan != "pro" {
		t.Errorf("plan = %q, want %q", snapshot.Identity.Plan, "pro")
	}
	if snapshot.Identity.Email != "testuser" {
		t.Errorf("email = %q, want %q", snapshot.Identity.Email, "testuser")
	}
}

func TestParseTypedUsageResponse_AdditionalQuotaSnapshotWithQuotaID(t *testing.T) {
	resp := UserResponse{
		QuotaResetDateUTC: "2025-03-01T00:00:00Z",
		QuotaSnapshots: &QuotaSnapshots{
			Additional: map[string]*Quota{
				"some_key": {QuotaID: "custom_feature", Entitlement: 100, Remaining: 25},
			},
		},
	}

	snapshot := (&DeviceFlowStrategy{}).parseTypedUsageResponse(resp)
	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snapshot.Periods) != 1 {
		t.Fatalf("len(periods) = %d, want 1", len(snapshot.Periods))
	}
	period := snapshot.Periods[0]
	if period.Name != "Monthly (Custom Feature)" {
		t.Errorf("period name = %q, want Monthly (Custom Feature)", period.Name)
	}
	if period.Utilization != 75 {
		t.Errorf("period utilization = %d, want 75", period.Utilization)
	}
}

func TestParseTypedUsageResponse_SuppressesUnidentifiedAdditionalQuotaSnapshot(t *testing.T) {
	resp := UserResponse{
		QuotaSnapshots: &QuotaSnapshots{
			Additional: map[string]*Quota{
				"custom_feature": {Entitlement: 100, Remaining: 25},
			},
		},
	}

	if snapshot := (&DeviceFlowStrategy{}).parseTypedUsageResponse(resp); snapshot != nil {
		t.Fatalf("expected nil snapshot for unidentified additional quota, got %#v", snapshot)
	}
}

func TestParseTypedUsageResponse_NoQuotas(t *testing.T) {
	resp := UserResponse{
		CopilotPlan: "free",
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot != nil {
		t.Error("expected nil snapshot when no quotas")
	}
}

func TestParseTypedUsageResponse_EmptyQuotas(t *testing.T) {
	resp := UserResponse{
		QuotaSnapshots: &QuotaSnapshots{},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot != nil {
		t.Error("expected nil snapshot when all quotas nil")
	}
}

func TestParseTypedUsageResponse_ZeroEntitlementNotUnlimited(t *testing.T) {
	resp := UserResponse{
		QuotaSnapshots: &QuotaSnapshots{
			PremiumInteractions: &Quota{Entitlement: 0, Remaining: 0, Unlimited: false},
			Chat:                &Quota{Entitlement: 100, Remaining: 50, Unlimited: false},
		},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	// Only chat should appear (premium has no usage)
	if len(snapshot.Periods) != 1 {
		t.Fatalf("len(periods) = %d, want 1", len(snapshot.Periods))
	}
	if snapshot.Periods[0].Name != "Monthly (Chat)" {
		t.Errorf("period name = %q, want %q", snapshot.Periods[0].Name, "Monthly (Chat)")
	}
}

func TestParseTypedUsageResponse_NoPlan(t *testing.T) {
	resp := UserResponse{
		QuotaSnapshots: &QuotaSnapshots{
			Chat: &Quota{Entitlement: 100, Remaining: 50, Unlimited: false},
		},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Identity != nil {
		t.Error("expected nil identity when no plan or login")
	}
}

func TestParseTypedUsageResponse_LoginOnly(t *testing.T) {
	resp := UserResponse{
		Login: "testuser",
		QuotaSnapshots: &QuotaSnapshots{
			Chat: &Quota{Entitlement: 100, Remaining: 50, Unlimited: false},
		},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Identity == nil {
		t.Fatal("expected identity when login is present")
	}
	if snapshot.Identity.Email != "testuser" {
		t.Errorf("email = %q, want %q", snapshot.Identity.Email, "testuser")
	}
	if snapshot.Identity.Plan != "" {
		t.Errorf("plan = %q, want empty", snapshot.Identity.Plan)
	}
}

func TestParseTypedUsageResponse_ResetDateWithZ(t *testing.T) {
	resp := UserResponse{
		QuotaResetDateUTC: "2025-03-01T00:00:00Z",
		QuotaSnapshots: &QuotaSnapshots{
			Chat: &Quota{Entitlement: 100, Remaining: 50, Unlimited: false},
		},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Periods[0].ResetsAt == nil {
		t.Fatal("expected resets_at")
	}
}

func TestParseTypedUsageResponse_InvalidResetDate(t *testing.T) {
	resp := UserResponse{
		QuotaResetDateUTC: "not-a-date",
		QuotaSnapshots: &QuotaSnapshots{
			Chat: &Quota{Entitlement: 100, Remaining: 50, Unlimited: false},
		},
	}

	s := DeviceFlowStrategy{}
	snapshot := s.parseTypedUsageResponse(resp)

	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snapshot.Periods[0].ResetsAt != nil {
		t.Error("expected nil resets_at for invalid date")
	}
}

func TestSaveCopilotCredentials_PersistsToSlot(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())

	in := &oauth.Credentials{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAt:    "2099-01-01T00:00:00Z",
	}
	if err := saveCopilotCredentials(in); err != nil {
		t.Fatalf("saveCopilotCredentials: %v", err)
	}

	data, err := config.ReadCredential("copilot", "oauth")
	if err != nil || data == nil {
		t.Fatalf("ReadCredential after save: data=%q err=%v", data, err)
	}
	var got oauth.Credentials
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != *in {
		t.Errorf("persisted creds = %+v, want %+v", got, *in)
	}
}
