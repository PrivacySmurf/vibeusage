package display

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

func TestOutputJSON_WritesToWriter(t *testing.T) {
	var buf bytes.Buffer
	if err := OutputJSON(&buf, map[string]string{"key": "value"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"key"`) {
		t.Errorf("expected key in output, got: %s", output)
	}
	if !strings.Contains(output, `"value"`) {
		t.Errorf("expected value in output, got: %s", output)
	}
}

func TestOutputJSON_PrettyPrints(t *testing.T) {
	var buf bytes.Buffer
	if err := OutputJSON(&buf, map[string]string{"a": "1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "  ") {
		t.Errorf("expected indented output, got: %s", output)
	}
}

func TestOutputJSON_ReturnsErrorOnMarshalFailure(t *testing.T) {
	var buf bytes.Buffer
	// Channels cannot be marshaled to JSON.
	err := OutputJSON(&buf, map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("expected error for unmarshalable type, got nil")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestOutputJSON_ReturnsErrorOnWriteFailure(t *testing.T) {
	err := OutputJSON(failWriter{}, map[string]string{"a": "1"})
	if err == nil {
		t.Fatal("expected error for failed writer, got nil")
	}
}

func TestOutputStatusJSON_WritesToWriter(t *testing.T) {
	var buf bytes.Buffer
	statuses := map[string]models.ProviderStatus{
		"claude": {
			Level: models.StatusOperational,
		},
	}

	if err := OutputStatusJSON(&buf, statuses); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "claude") {
		t.Errorf("expected provider in output, got: %s", output)
	}
	if !strings.Contains(output, "operational") {
		t.Errorf("expected level in output, got: %s", output)
	}
}

func TestOutputMultiProviderJSON_WritesToWriter(t *testing.T) {
	var buf bytes.Buffer

	now := time.Now()
	outcomes := map[string]fetch.FetchOutcome{
		"claude": {
			ProviderID: "claude",
			Success:    true,
			Snapshot: &models.UsageSnapshot{
				Provider:  "claude",
				FetchedAt: now,
			},
		},
	}

	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "claude") {
		t.Errorf("expected provider in output, got: %s", output)
	}
	if !strings.Contains(output, "providers") {
		t.Errorf("expected providers key in output, got: %s", output)
	}
}

func TestOutputMultiProviderJSON_IncludesErrors(t *testing.T) {
	var buf bytes.Buffer

	outcomes := map[string]fetch.FetchOutcome{
		"cursor": {
			ProviderID: "cursor",
			Success:    false,
			Error:      "auth failed",
		},
	}

	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "errors") {
		t.Errorf("expected errors key in output, got: %s", output)
	}
	if !strings.Contains(output, "auth failed") {
		t.Errorf("expected error message in output, got: %s", output)
	}
}

// SnapshotToJSON structural tests

func TestSnapshotToJSON_FailedOutcome(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    false,
		Error:      "token expired",
	}

	result := SnapshotToJSON(outcome)

	errResult, ok := result.(SnapshotErrorJSON)
	if !ok {
		t.Fatalf("expected SnapshotErrorJSON, got %T", result)
	}
	if errResult.Error.Message != "token expired" {
		t.Errorf("error.message = %q, want %q", errResult.Error.Message, "token expired")
	}
	if errResult.Error.Provider != "claude" {
		t.Errorf("error.provider = %q, want %q", errResult.Error.Provider, "claude")
	}
}

func TestSnapshotToJSON_NilSnapshot(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot:   nil,
	}

	result := SnapshotToJSON(outcome)
	if _, ok := result.(SnapshotErrorJSON); !ok {
		t.Fatalf("expected SnapshotErrorJSON when snapshot is nil, got %T", result)
	}
}

func TestSnapshotToJSON_SuccessBaseFields(t *testing.T) {
	now := time.Now()
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider:  "claude",
			FetchedAt: now,
			Periods:   []models.UsagePeriod{},
			Source:    "oauth",
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	if data["provider"] != "claude" {
		t.Errorf("provider = %q, want %q", data["provider"], "claude")
	}
	if data["source"] != "oauth" {
		t.Errorf("source = %q, want %q", data["source"], "oauth")
	}
}

func TestSnapshotToJSON_WithIdentity(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "claude",
			Identity: &models.ProviderIdentity{
				Email:        "user@example.com",
				Organization: "Acme Corp",
				Plan:         "pro",
			},
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	identity, ok := data["identity"].(map[string]any)
	if !ok {
		t.Fatal("expected identity to be set")
	}
	if identity["email"] != "user@example.com" {
		t.Errorf("identity.email = %q, want %q", identity["email"], "user@example.com")
	}
	if identity["organization"] != "Acme Corp" {
		t.Errorf("identity.organization = %q, want %q", identity["organization"], "Acme Corp")
	}
	if identity["plan"] != "pro" {
		t.Errorf("identity.plan = %q, want %q", identity["plan"], "pro")
	}
}

func TestSnapshotToJSON_NoIdentity(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "claude",
			Identity: nil,
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	if data["identity"] != nil {
		t.Error("identity should be nil when not provided")
	}
}

func TestSnapshotToJSON_Periods(t *testing.T) {
	reset := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "claude",
			Periods: []models.UsagePeriod{
				{
					Name:        "Monthly",
					Utilization: 75,
					PeriodType:  models.PeriodMonthly,
					ResetsAt:    &reset,
				},
				{
					Name:        "Sonnet Daily",
					Utilization: 30,
					PeriodType:  models.PeriodDaily,
					Model:       "claude-3-sonnet",
				},
			},
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	periods, ok := data["periods"].([]any)
	if !ok {
		t.Fatal("expected periods to be an array")
	}
	if len(periods) != 2 {
		t.Fatalf("expected 2 periods, got %d", len(periods))
	}

	p0 := periods[0].(map[string]any)
	if p0["name"] != "Monthly" {
		t.Errorf("period[0].name = %q, want %q", p0["name"], "Monthly")
	}
	if p0["utilization"] != float64(75) {
		t.Errorf("period[0].utilization = %v, want 75", p0["utilization"])
	}
	if p0["period_type"] != "monthly" {
		t.Errorf("period[0].period_type = %q, want %q", p0["period_type"], "monthly")
	}
	if _, ok := p0["resets_at"]; !ok {
		t.Error("period[0] should have resets_at")
	}

	p1 := periods[1].(map[string]any)
	if p1["model"] != "claude-3-sonnet" {
		t.Errorf("period[1].model = %q, want %q", p1["model"], "claude-3-sonnet")
	}
}

func TestSnapshotToJSON_WithResetCredits(t *testing.T) {
	expires := time.Date(2026, time.July, 17, 0, 0, 0, 0, time.UTC)
	lastReset := time.Date(2026, time.July, 21, 16, 47, 15, 0, time.UTC)
	outcome := fetch.FetchOutcome{
		ProviderID: "codex",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "codex",
			UsageLimitResets: &models.UsageLimitResets{
				AvailableCount: 2,
				Resets: []models.UsageLimitReset{{
					Title:     "Full reset",
					ExpiresAt: &expires,
				}},
				Activity: &models.UsageLimitResetActivity{
					LastResetAt:         lastReset,
					AverageIntervalDays: 8.8,
					Source:              "https://codex-resets.com",
				},
			},
		},
	}

	data := marshalSnapshotToJSON(t, outcome)
	resets, ok := data["usage_limit_resets"].(map[string]any)
	if !ok {
		t.Fatal("expected usage_limit_resets to be set")
	}
	if resets["available_count"] != float64(2) {
		t.Errorf("usage_limit_resets.available_count = %v, want 2", resets["available_count"])
	}
	details, ok := resets["resets"].([]any)
	if !ok || len(details) != 1 {
		t.Fatalf("usage_limit_resets.resets = %v, want one reset", resets["resets"])
	}
	reset := details[0].(map[string]any)
	if reset["title"] != "Full reset" {
		t.Errorf("reset.title = %q, want Full reset", reset["title"])
	}
	if _, ok := reset["expires_at"]; !ok {
		t.Error("reset should include expires_at")
	}
	activity, ok := resets["activity"].(map[string]any)
	if !ok {
		t.Fatal("expected usage_limit_resets.activity to be set")
	}
	if activity["average_interval_days"] != 8.8 {
		t.Errorf("activity.average_interval_days = %v, want 8.8", activity["average_interval_days"])
	}
	if activity["source"] != "https://codex-resets.com" {
		t.Errorf("activity.source = %q, want codex-resets.com", activity["source"])
	}
	if _, ok := activity["last_reset_at"]; !ok {
		t.Error("activity should include last_reset_at")
	}
}

func TestSnapshotToJSON_WithOverage(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "claude",
			Overage: &models.OverageUsage{
				Used:      15.50,
				Limit:     100.00,
				Currency:  "USD",
				IsEnabled: true,
			},
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	overage, ok := data["overage"].(map[string]any)
	if !ok {
		t.Fatal("expected overage to be set")
	}
	if overage["used"] != 15.50 {
		t.Errorf("overage.used = %v, want 15.50", overage["used"])
	}
	if overage["limit"] != 100.00 {
		t.Errorf("overage.limit = %v, want 100.00", overage["limit"])
	}
	if overage["currency"] != "USD" {
		t.Errorf("overage.currency = %q, want %q", overage["currency"], "USD")
	}
}

func TestSnapshotToJSON_OverageDisabled(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "claude",
			Overage: &models.OverageUsage{
				Used:      5.0,
				Limit:     100.0,
				IsEnabled: false,
			},
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	if data["overage"] != nil {
		t.Error("overage should be nil when disabled")
	}
}

func TestSnapshotToJSON_OverageNil(t *testing.T) {
	outcome := fetch.FetchOutcome{
		ProviderID: "claude",
		Success:    true,
		Snapshot: &models.UsageSnapshot{
			Provider: "claude",
			Overage:  nil,
		},
	}

	data := marshalSnapshotToJSON(t, outcome)

	if data["overage"] != nil {
		t.Error("overage should be nil when nil")
	}
}

// marshalSnapshotToJSON is a test helper that converts a successful
// FetchOutcome through SnapshotToJSON and returns the unmarshaled map.
func marshalSnapshotToJSON(t *testing.T, outcome fetch.FetchOutcome) map[string]any {
	t.Helper()
	result := SnapshotToJSON(outcome)
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal SnapshotToJSON result: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("failed to unmarshal SnapshotToJSON result: %v", err)
	}
	return data
}

// OutputMultiProviderJSON structural tests

func TestOutputMultiProviderJSON_Structure(t *testing.T) {
	var buf bytes.Buffer

	now := time.Now()
	outcomes := map[string]fetch.FetchOutcome{
		"claude": {
			ProviderID: "claude",
			Success:    true,
			Source:     "oauth",
			Snapshot: &models.UsageSnapshot{
				Provider:  "claude",
				FetchedAt: now,
				Periods:   []models.UsagePeriod{{Name: "Monthly", Utilization: 50, PeriodType: models.PeriodMonthly}},
			},
		},
		"cursor": {
			ProviderID: "cursor",
			Success:    false,
			Error:      "auth failed",
		},
	}

	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result struct {
		Providers map[string]any    `json:"providers"`
		Errors    map[string]string `json:"errors"`
		FetchedAt string            `json:"fetched_at"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result.FetchedAt == "" {
		t.Error("missing fetched_at")
	}

	if _, ok := result.Providers["claude"]; !ok {
		t.Error("missing 'claude' in providers")
	}

	if errMsg, ok := result.Errors["cursor"]; !ok {
		t.Error("missing 'cursor' in errors")
	} else if errMsg != "auth failed" {
		t.Errorf("errors.cursor = %q, want %q", errMsg, "auth failed")
	}
}

func TestOutputMultiProviderJSON_EmptyOutcomes(t *testing.T) {
	var buf bytes.Buffer
	if err := OutputMultiProviderJSON(&buf, map[string]fetch.FetchOutcome{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result struct {
		Providers map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(result.Providers) != 0 {
		t.Errorf("expected empty providers, got %d", len(result.Providers))
	}
}

func TestOutputMultiProviderJSON_ErrorWithEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	outcomes := map[string]fetch.FetchOutcome{
		"broken": {
			ProviderID: "broken",
			Success:    false,
			Error:      "",
		},
	}

	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result struct {
		Errors map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result.Errors["broken"] != "Unknown error" {
		t.Errorf("expected 'Unknown error' for empty message, got: %v", result.Errors["broken"])
	}
}

func TestOutputMultiProviderJSON_FreshnessAndStaleness(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	oneDayAgo := now.Add(-24 * time.Hour)

	outcomes := map[string]fetch.FetchOutcome{
		"live_provider": {
			ProviderID: "live_provider",
			Success:    true,
			Source:     "oauth",
			Cached:     false,
			Snapshot: &models.UsageSnapshot{
				Provider:  "live_provider",
				FetchedAt: now,
				Periods:   []models.UsagePeriod{{Name: "Session", Utilization: 20}},
			},
		},
		"fresh_cached_provider": {
			ProviderID: "fresh_cached_provider",
			Success:    true,
			Source:     "cache",
			Cached:     true,
			Snapshot: &models.UsageSnapshot{
				Provider:  "fresh_cached_provider",
				FetchedAt: now.Add(-60 * time.Second),
				Periods:   []models.UsagePeriod{{Name: "Session", Utilization: 30}},
			},
		},
		"stale_cached_provider": {
			ProviderID: "stale_cached_provider",
			Success:    true,
			Source:     "cache (throttled)",
			Cached:     true,
			Snapshot: &models.UsageSnapshot{
				Provider:  "stale_cached_provider",
				FetchedAt: oneDayAgo,
				Periods:   []models.UsagePeriod{{Name: "Session", Utilization: 40}},
			},
		},
	}

	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result struct {
		Providers map[string]models.UsageSnapshot `json:"providers"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	live := result.Providers["live_provider"]
	if live.Freshness != "live" || live.Stale || live.Cached {
		t.Errorf("live_provider: freshness=%q, is_stale=%v, cached=%v; want live, false, false", live.Freshness, live.Stale, live.Cached)
	}

	freshCached := result.Providers["fresh_cached_provider"]
	if freshCached.Freshness != "cached_fresh" || freshCached.Stale || !freshCached.Cached {
		t.Errorf("fresh_cached_provider: freshness=%q, is_stale=%v, cached=%v; want cached_fresh, false, true", freshCached.Freshness, freshCached.Stale, freshCached.Cached)
	}

	staleCached := result.Providers["stale_cached_provider"]
	if staleCached.Freshness != "fallback_stale" || !staleCached.Stale || !staleCached.Cached || staleCached.Source != "cache (throttled)" {
		t.Errorf("stale_cached_provider: freshness=%q, is_stale=%v, cached=%v, source=%q; want fallback_stale, true, true, cache (throttled)", staleCached.Freshness, staleCached.Stale, staleCached.Cached, staleCached.Source)
	}
	if staleCached.DataAgeSeconds < 86000 {
		t.Errorf("stale_cached_provider: data_age_seconds=%d, want >= 86000", staleCached.DataAgeSeconds)
	}
}

// OutputStatusJSON structural tests

func TestOutputStatusJSON_Structure(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	statuses := map[string]models.ProviderStatus{
		"claude": {
			Level:     models.StatusOperational,
			UpdatedAt: &now,
		},
		"copilot": {
			Level:       models.StatusDegraded,
			Description: "Partial issues",
			UpdatedAt:   nil,
		},
	}

	if err := OutputStatusJSON(&buf, statuses); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]StatusEntryJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	claude, ok := result["claude"]
	if !ok {
		t.Fatal("missing 'claude' key")
	}
	if claude.Level != "operational" {
		t.Errorf("claude.level = %q, want %q", claude.Level, "operational")
	}
	// DisplayDescription() returns canonical description even when Description field is empty
	if claude.Description != "All systems operational" {
		t.Errorf("claude.description = %q, want %q", claude.Description, "All systems operational")
	}
	if claude.UpdatedAt == "" {
		t.Error("claude should have updated_at")
	}

	copilot, ok := result["copilot"]
	if !ok {
		t.Fatal("missing 'copilot' key")
	}
	// Incident-specific description overrides canonical
	if copilot.Description != "Partial issues" {
		t.Errorf("copilot.description = %q, want %q", copilot.Description, "Partial issues")
	}
	if copilot.UpdatedAt != "" {
		t.Errorf("copilot should not have updated_at, got %q", copilot.UpdatedAt)
	}
}

func decodeRawProviders(t *testing.T, data []byte) map[string]map[string]any {
	t.Helper()
	var raw struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return raw.Providers
}

func TestOutputMultiProviderJSON_ThrottledCacheOverwritesSourceAndAddsMetadata(t *testing.T) {
	now := time.Now()
	retryAt := now.Add(15 * time.Minute).UTC().Truncate(time.Second)

	// Two providers (one not modelstudio): stored snapshots keep an older
	// strategy source ("web"/"oauth") that must not leak through.
	cases := map[string]struct {
		storedSource string
		reason       string
	}{
		"modelstudio": {storedSource: "web", reason: "Rate limited by Alibaba"},
		"claude":      {storedSource: "oauth", reason: "Rate limited"},
	}

	outcomes := map[string]fetch.FetchOutcome{}
	for pid, tc := range cases {
		until := retryAt
		outcomes[pid] = fetch.FetchOutcome{
			ProviderID:     pid,
			Success:        true,
			Source:         fetch.SourceCacheThrottled,
			Cached:         true,
			ThrottledUntil: &until,
			ThrottleReason: tc.reason,
			Snapshot: &models.UsageSnapshot{
				Provider:  pid,
				FetchedAt: now.Add(-2 * time.Hour),
				Periods:   []models.UsagePeriod{{Name: "Session", Utilization: 10}},
				Source:    tc.storedSource,
			},
		}
	}

	var buf bytes.Buffer
	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := decodeRawProviders(t, buf.Bytes())

	for pid, tc := range cases {
		p, ok := raw[pid]
		if !ok {
			t.Fatalf("provider %q missing from output", pid)
		}
		if p["source"] != fetch.SourceCacheThrottled {
			t.Errorf("%s: source=%v, want %q", pid, p["source"], fetch.SourceCacheThrottled)
		}
		if p["freshness"] != "fallback_stale" || p["is_stale"] != true || p["cached"] != true {
			t.Errorf("%s: freshness=%v is_stale=%v cached=%v; want fallback_stale, true, true", pid, p["freshness"], p["is_stale"], p["cached"])
		}
		gotUntil, _ := p["throttled_until"].(string)
		parsed, err := time.Parse(time.RFC3339, gotUntil)
		if err != nil {
			t.Fatalf("%s: throttled_until %q is not RFC3339: %v", pid, gotUntil, err)
		}
		if !parsed.Equal(retryAt) {
			t.Errorf("%s: throttled_until=%s, want %s", pid, parsed, retryAt)
		}
		if p["throttle_reason"] != tc.reason {
			t.Errorf("%s: throttle_reason=%v, want %q", pid, p["throttle_reason"], tc.reason)
		}
		if age, _ := p["data_age_seconds"].(float64); age < 7000 {
			t.Errorf("%s: data_age_seconds=%v, want >= 7000", pid, p["data_age_seconds"])
		}
	}
}

func TestOutputMultiProviderJSON_NonThrottledOmitsThrottleKeysAndKeepsSource(t *testing.T) {
	now := time.Now()
	stale := now.Add(time.Hour)
	outcomes := map[string]fetch.FetchOutcome{
		"live": {
			ProviderID: "live",
			Success:    true,
			Source:     "web",
			Snapshot: &models.UsageSnapshot{
				Provider:  "live",
				FetchedAt: now,
				Periods:   []models.UsagePeriod{{Name: "Session", Utilization: 1}},
				Source:    "web",
			},
		},
		"fresh_cache": {
			ProviderID: "fresh_cache",
			Success:    true,
			Source:     "cache",
			Cached:     true,
			Snapshot: &models.UsageSnapshot{
				Provider:  "fresh_cache",
				FetchedAt: now.Add(-30 * time.Second),
				Periods:   []models.UsagePeriod{{Name: "Session", Utilization: 2}},
				Source:    "web",
			},
		},
		// A stored snapshot carrying stale throttle data must never leak into
		// a non-throttled outcome.
		"leaky_stored": {
			ProviderID: "leaky_stored",
			Success:    true,
			Source:     "cache",
			Cached:     true,
			Snapshot: &models.UsageSnapshot{
				Provider:       "leaky_stored",
				FetchedAt:      now.Add(-30 * time.Second),
				Periods:        []models.UsagePeriod{{Name: "Session", Utilization: 3}},
				Source:         "web",
				ThrottledUntil: &stale,
				ThrottleReason: "old",
			},
		},
	}

	var buf bytes.Buffer
	if err := OutputMultiProviderJSON(&buf, outcomes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := decodeRawProviders(t, buf.Bytes())

	for _, pid := range []string{"live", "fresh_cache", "leaky_stored"} {
		p := raw[pid]
		if p["source"] != "web" {
			t.Errorf("%s: source=%v, want web", pid, p["source"])
		}
		for _, key := range []string{"throttled_until", "throttle_reason"} {
			if _, present := p[key]; present {
				t.Errorf("%s: unexpected key %q in JSON", pid, key)
			}
		}
	}
	if raw["live"]["freshness"] != "live" || raw["fresh_cache"]["freshness"] != "cached_fresh" {
		t.Errorf("freshness changed: live=%v fresh_cache=%v", raw["live"]["freshness"], raw["fresh_cache"]["freshness"])
	}
}

func TestSnapshotToJSON_ThrottledCacheSingleProvider(t *testing.T) {
	retryAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	outcome := fetch.FetchOutcome{
		ProviderID:     "mimo",
		Success:        true,
		Source:         fetch.SourceCacheThrottled,
		Cached:         true,
		ThrottledUntil: &retryAt,
		ThrottleReason: "Rate limited",
		Snapshot: &models.UsageSnapshot{
			Provider:  "mimo",
			FetchedAt: time.Now().Add(-time.Hour),
			Periods:   []models.UsagePeriod{{Name: "Monthly", Utilization: 5}},
			Source:    "web",
		},
	}

	data, err := json.Marshal(SnapshotToJSON(outcome))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["source"] != fetch.SourceCacheThrottled {
		t.Errorf("source=%v, want %q", raw["source"], fetch.SourceCacheThrottled)
	}
	if raw["freshness"] != "fallback_stale" || raw["is_stale"] != true {
		t.Errorf("freshness=%v is_stale=%v; want fallback_stale, true", raw["freshness"], raw["is_stale"])
	}
	if got, _ := raw["throttled_until"].(string); got != retryAt.Format(time.RFC3339) {
		t.Errorf("throttled_until=%v, want %s", raw["throttled_until"], retryAt.Format(time.RFC3339))
	}
	if raw["throttle_reason"] != "Rate limited" {
		t.Errorf("throttle_reason=%v", raw["throttle_reason"])
	}

	// Same snapshot, non-throttled outcome: keep source, no throttle keys.
	live := outcome
	live.Source = "web"
	live.Cached = false
	live.ThrottledUntil = nil
	live.ThrottleReason = ""
	data, _ = json.Marshal(SnapshotToJSON(live))
	raw = map[string]any{}
	_ = json.Unmarshal(data, &raw)
	if raw["source"] != "web" {
		t.Errorf("live source=%v, want web", raw["source"])
	}
	if _, ok := raw["throttled_until"]; ok {
		t.Error("live outcome must omit throttled_until")
	}
	if _, ok := raw["throttle_reason"]; ok {
		t.Error("live outcome must omit throttle_reason")
	}
}
