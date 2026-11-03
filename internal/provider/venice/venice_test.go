package venice

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRateLimitsSnapshot_USDBalanceAndTier(t *testing.T) {
	resp := RateLimitsResponse{
		Data: RateLimitsData{
			AccessPermitted: true,
			APITier:         APITier{ID: "paid", IsCharged: true},
			Balances:        Balances{USD: 9.12, DIEM: 0, BundledCredits: 0},
		},
	}

	snapshot, err := parseRateLimitsSnapshot(resp)
	if err != nil {
		t.Fatalf("parseRateLimitsSnapshot() error = %v", err)
	}
	if snapshot.Provider != "venice" {
		t.Errorf("provider = %q, want %q", snapshot.Provider, "venice")
	}
	if snapshot.Source != "api_key" {
		t.Errorf("source = %q, want %q", snapshot.Source, "api_key")
	}
	if snapshot.Billing == nil || snapshot.Billing.Balance == nil {
		t.Fatal("expected billing balance")
	}
	if math.Abs(*snapshot.Billing.Balance-9.12) > 1e-9 {
		t.Errorf("balance = %v, want 9.12", *snapshot.Billing.Balance)
	}
	if snapshot.Billing.Credits != nil {
		t.Errorf("credits = %v, want nil when DIEM is 0", snapshot.Billing.Credits)
	}
	if snapshot.Identity == nil || snapshot.Identity.Plan != "paid" {
		t.Errorf("identity = %+v, want Plan=paid", snapshot.Identity)
	}
}

func TestParseRateLimitsSnapshot_IncludesBundledCreditsAndDIEM(t *testing.T) {
	resp := RateLimitsResponse{
		Data: RateLimitsData{
			AccessPermitted: true,
			APITier:         APITier{ID: "paid", IsCharged: true},
			Balances:        Balances{USD: 10.0, DIEM: 42.5, BundledCredits: 5.25},
		},
	}

	snapshot, err := parseRateLimitsSnapshot(resp)
	if err != nil {
		t.Fatalf("parseRateLimitsSnapshot() error = %v", err)
	}
	if snapshot.Billing == nil || snapshot.Billing.Balance == nil {
		t.Fatal("expected billing balance")
	}
	if math.Abs(*snapshot.Billing.Balance-15.25) > 1e-9 {
		t.Errorf("balance = %v, want 15.25", *snapshot.Billing.Balance)
	}
	if snapshot.Billing.Credits == nil || math.Abs(*snapshot.Billing.Credits-42.5) > 1e-9 {
		t.Errorf("credits = %v, want 42.5", snapshot.Billing.Credits)
	}
}

func TestFetchRateLimits_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer venice-test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer venice-test-key")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"data": {
				"accessPermitted": true,
				"apiTier": {"id": "paid", "isCharged": true},
				"balances": {"USD": 9.12196523, "DIEM": 0, "BUNDLED_CREDITS": 0}
			}
		}`))
	}))
	defer srv.Close()

	oldURL := rateLimitsURL
	rateLimitsURL = srv.URL
	defer func() { rateLimitsURL = oldURL }()

	result, err := fetchRateLimits(context.Background(), "venice-test-key", 5)
	if err != nil {
		t.Fatalf("fetchRateLimits() err = %v", err)
	}
	if !result.Success || result.Snapshot == nil {
		t.Fatalf("fetchRateLimits() failed: %+v", result)
	}
	if result.Snapshot.Billing == nil || result.Snapshot.Billing.Balance == nil {
		t.Fatal("expected billing balance in snapshot")
	}
	if math.Abs(*result.Snapshot.Billing.Balance-9.12196523) > 1e-9 {
		t.Errorf("balance = %v, want 9.12196523", *result.Snapshot.Billing.Balance)
	}
}

func TestFetchRateLimits_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Authentication failed"}`))
	}))
	defer srv.Close()

	oldURL := rateLimitsURL
	rateLimitsURL = srv.URL
	defer func() { rateLimitsURL = oldURL }()

	result, err := fetchRateLimits(context.Background(), "venice-bad", 5)
	if err != nil {
		t.Fatalf("fetchRateLimits() err = %v", err)
	}
	if result.ShouldFallback {
		t.Fatal("auth failure should be fatal")
	}
	if !strings.Contains(strings.ToLower(result.Error), "venice") {
		t.Errorf("error = %q, want auth hint mentioning venice", result.Error)
	}
}

func TestFetchRateLimits_InvalidJSONIncludesUnderlyingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":`))
	}))
	defer srv.Close()

	oldURL := rateLimitsURL
	rateLimitsURL = srv.URL
	defer func() { rateLimitsURL = oldURL }()

	result, err := fetchRateLimits(context.Background(), "venice-test", 5)
	if err != nil {
		t.Fatalf("fetchRateLimits() err = %v", err)
	}
	if result.Success {
		t.Fatal("expected failure on invalid JSON")
	}
	if !strings.Contains(result.Error, "Invalid response from Venice API:") {
		t.Errorf("error = %q, want prefix 'Invalid response from Venice API:'", result.Error)
	}
}
