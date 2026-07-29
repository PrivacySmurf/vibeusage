package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/display"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/history"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/prompt"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

type historyTestStrategy struct {
	available bool
	result    fetch.FetchResult
	err       error
}

func (s historyTestStrategy) IsAvailable() bool { return s.available }
func (s historyTestStrategy) Fetch(context.Context) (fetch.FetchResult, error) {
	return s.result, s.err
}

func setHistoryTestContext(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetContext(context.Background()) })
}

func storedHistorySnapshot(providerID string, at time.Time) models.UsageSnapshot {
	return models.UsageSnapshot{
		Provider:  providerID,
		FetchedAt: at,
		Periods:   []models.UsagePeriod{{Name: "Daily", PeriodType: models.PeriodDaily}},
	}
}

func TestHistoryProviderIDs(t *testing.T) {
	disabled := false
	cfg := config.DefaultConfig()
	cfg.Providers["disabled"] = config.ProviderConfig{Enabled: &disabled}
	available := []fetch.Strategy{historyTestStrategy{available: true}}
	providerMap := map[string][]fetch.Strategy{"zeta": available, "alpha": available, "disabled": available}

	ids, err := historyProviderIDs(providerMap, cfg, nil)
	if err != nil {
		t.Fatalf("historyProviderIDs() error = %v", err)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("historyProviderIDs() = %v, want %v", ids, want)
	}
}

func TestHistoryProviderIDsRejectsUnknownAndDisabledProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	if _, err := historyProviderIDs(nil, cfg, []string{"unknown"}); err == nil {
		t.Fatal("expected unknown provider error")
	}

	disabled := false
	cfg.Providers["claude"] = config.ProviderConfig{Enabled: &disabled}
	if _, err := historyProviderIDs(nil, cfg, []string{"claude"}); err == nil {
		t.Fatal("expected disabled provider error")
	}

	cfg = config.DefaultConfig()
	if _, err := historyProviderIDs(map[string][]fetch.Strategy{"claude": nil}, cfg, []string{"claude"}); err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("unavailable provider error = %v", err)
	}
	if _, err := historyProviderIDs(map[string][]fetch.Strategy{"claude": nil}, cfg, nil); err == nil || !strings.Contains(err.Error(), "no enabled providers are authenticated") {
		t.Fatalf("no-auth providers error = %v", err)
	}
}

func TestHistoryProviderJSONAndTable(t *testing.T) {
	zero := 0
	burn := 2.5
	report := display.HistoryJSON{Providers: map[string]display.HistoryProviderJSON{
		"claude": {
			DaysRecorded: 2,
			Samples:      3,
			Periods: []display.HistoryPeriodJSON{{
				Name:                "All Models",
				PeriodType:          "weekly",
				Model:               "claude-sonnet",
				CurrentUtilization:  10,
				SamePointLastPeriod: &zero,
				Delta:               &zero,
				BurnPerDay:          &burn,
				SamplesInPeriod:     2,
			}},
		},
	}}

	var output bytes.Buffer
	oldWriter, oldNoColor := outWriter, noColor
	outWriter, noColor = &output, true
	t.Cleanup(func() {
		outWriter, noColor = oldWriter, oldNoColor
	})

	if err := renderHistoryReport(report, []string{"claude"}); err != nil {
		t.Fatalf("renderHistoryReport() error = %v", err)
	}
	got := output.String()
	for _, text := range []string{"All Models (claude-sonnet)", "0%", "+0 pp", "2.5 pp/day", "3 samples, 2 days recorded", "—"} {
		if !strings.Contains(got, text) {
			t.Errorf("output missing %q:\n%s", text, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("no-color output contains ANSI escapes: %q", got)
	}

	var jsonOutput bytes.Buffer
	if err := display.OutputJSON(&jsonOutput, report); err != nil {
		t.Fatalf("OutputJSON() error = %v", err)
	}
	var decoded display.HistoryJSON
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatalf("history JSON is invalid: %v", err)
	}
	period := decoded.Providers["claude"].Periods[0]
	if period.SamePointLastPeriod == nil || period.Delta == nil {
		t.Errorf("zero optional fields were omitted: %#v", period)
	}
}

func TestHistoryRecordResult(t *testing.T) {
	tests := []struct {
		name      string
		outcomes  map[string]fetch.FetchOutcome
		attempted int
		wantErr   string
	}{
		{
			name: "some providers succeed",
			outcomes: map[string]fetch.FetchOutcome{
				"claude": {Success: true, Snapshot: &models.UsageSnapshot{}, Recorded: true},
				"codex":  {Success: false, Error: "request failed"},
			},
			attempted: 2,
		},
		{
			name: "recording failure",
			outcomes: map[string]fetch.FetchOutcome{
				"claude": {
					Success:        true,
					Snapshot:       &models.UsageSnapshot{},
					RecordingError: "history write failed",
				},
				"codex": {Success: true, Snapshot: &models.UsageSnapshot{}},
			},
			attempted: 2,
			wantErr:   "history record: recording failed: claude: history write failed",
		},
		{
			name: "live fetch has no usage periods",
			outcomes: map[string]fetch.FetchOutcome{
				"opencode": {Success: true, Snapshot: &models.UsageSnapshot{}},
			},
			attempted: 1,
			wantErr:   "opencode: no usage periods to record",
		},
		{
			name: "all attempted providers fail",
			outcomes: map[string]fetch.FetchOutcome{
				"claude": {Success: false, Error: "request failed"},
				"codex":  {Success: false, Error: "request failed"},
			},
			attempted: 2,
			wantErr:   "history record: all 2 providers failed",
		},
		{
			name:      "no enabled providers are authenticated",
			attempted: 0,
			wantErr:   "history record: no enabled providers are authenticated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := historyRecordResult(tt.outcomes, tt.attempted)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("historyRecordResult() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("historyRecordResult() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunHistoryRecordsLiveFetchAndOutputsJSON(t *testing.T) {
	setHistoryTestContext(t, historyCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().UTC().Add(-time.Second)
	reset := at.Add(24 * time.Hour)
	snapshot := models.UsageSnapshot{
		Provider:  "claude",
		FetchedAt: at,
		Periods: []models.UsagePeriod{{
			Name:        "Daily",
			Utilization: 42,
			PeriodType:  models.PeriodDaily,
			ResetsAt:    &reset,
		}},
	}
	providerMap := map[string][]fetch.Strategy{
		"claude": {historyTestStrategy{available: true, result: fetch.ResultOK(snapshot)}},
	}

	var output bytes.Buffer
	oldWriter, oldJSON, oldNoCache, oldNoColor := outWriter, jsonOutput, noCache, noColor
	outWriter, jsonOutput, noCache, noColor = &output, true, true, true
	t.Cleanup(func() {
		outWriter, jsonOutput, noCache, noColor = oldWriter, oldJSON, oldNoCache, oldNoColor
	})

	if err := runHistoryWith(historyCmd, []string{"claude"}, config.DefaultConfig(), providerMap); err != nil {
		t.Fatalf("runHistoryWith() error = %v", err)
	}
	var report display.HistoryJSON
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("history JSON is invalid: %v\n%s", err, output.String())
	}
	providerReport := report.Providers["claude"]
	if providerReport.Cached || !providerReport.FetchedAt.Equal(at) {
		t.Errorf("provider provenance = %#v", providerReport)
	}
	if providerReport.Samples != 1 || len(providerReport.Periods) != 1 || providerReport.Periods[0].SamplesInPeriod != 1 {
		t.Errorf("provider report = %#v", providerReport)
	}
	records, err := history.Read("claude")
	if err != nil || len(records) != 1 {
		t.Fatalf("recorded history = %v, %v; want one sample", records, err)
	}
}

func TestRunHistoryReportsPartialFetchFailure(t *testing.T) {
	setHistoryTestContext(t, historyCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().UTC().Add(-time.Second)
	providerMap := map[string][]fetch.Strategy{
		"claude": {historyTestStrategy{available: true, result: fetch.ResultOK(models.UsageSnapshot{
			Provider:  "claude",
			FetchedAt: at,
			Periods:   []models.UsagePeriod{{Name: "Daily", PeriodType: models.PeriodDaily}},
		})}},
		"codex": {historyTestStrategy{available: true, result: fetch.ResultFail("service unavailable")}},
	}

	var output bytes.Buffer
	oldWriter, oldJSON, oldNoCache, oldNoColor := outWriter, jsonOutput, noCache, noColor
	outWriter, jsonOutput, noCache, noColor = &output, false, true, true
	t.Cleanup(func() {
		outWriter, jsonOutput, noCache, noColor = oldWriter, oldJSON, oldNoCache, oldNoColor
	})

	if err := runHistoryWith(historyCmd, nil, config.DefaultConfig(), providerMap); err != nil {
		t.Fatalf("runHistoryWith() error = %v", err)
	}
	for _, want := range []string{"Claude", "Codex: service unavailable"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("history output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunHistoryRejectsPeriodlessSnapshotInsteadOfRenderingEmptyTable(t *testing.T) {
	setHistoryTestContext(t, historyCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	providerMap := map[string][]fetch.Strategy{
		"opencode": {historyTestStrategy{available: true, result: fetch.ResultOK(models.UsageSnapshot{
			Provider: "opencode", FetchedAt: time.Now().UTC().Add(-time.Second),
		})}},
	}

	var output bytes.Buffer
	oldWriter, oldJSON, oldNoCache := outWriter, jsonOutput, noCache
	outWriter, jsonOutput, noCache = &output, false, true
	t.Cleanup(func() {
		outWriter, jsonOutput, noCache = oldWriter, oldJSON, oldNoCache
	})

	err := runHistoryWith(historyCmd, []string{"opencode"}, config.DefaultConfig(), providerMap)
	if err == nil || !strings.Contains(err.Error(), "no usage periods available") {
		t.Fatalf("periodless history error = %v", err)
	}
	if output.Len() != 0 {
		t.Errorf("periodless history output = %q, want no empty table", output.String())
	}
}

func TestRunHistoryFailsWhenEveryProviderFetchFails(t *testing.T) {
	setHistoryTestContext(t, historyCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	providerMap := map[string][]fetch.Strategy{
		"claude": {historyTestStrategy{available: true, result: fetch.ResultFail("timeout")}},
		"codex":  {historyTestStrategy{available: true, result: fetch.ResultFail("service unavailable")}},
	}

	var output bytes.Buffer
	oldWriter, oldJSON, oldNoCache := outWriter, jsonOutput, noCache
	outWriter, jsonOutput, noCache = &output, true, true
	t.Cleanup(func() {
		outWriter, jsonOutput, noCache = oldWriter, oldJSON, oldNoCache
	})

	err := runHistoryWith(historyCmd, nil, config.DefaultConfig(), providerMap)
	if err == nil {
		t.Fatal("runHistoryWith() succeeded, want aggregate failure")
	}
	for _, want := range []string{"claude: timeout", "codex: service unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("history error missing %q: %v", want, err)
		}
	}
	if output.Len() != 0 {
		t.Errorf("all-failed JSON output = %q, want empty", output.String())
	}
}

func TestRunHistoryMarksFreshCachedSnapshot(t *testing.T) {
	setHistoryTestContext(t, historyCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().UTC().Add(-10 * time.Second)
	snapshot := models.UsageSnapshot{
		Provider:  "claude",
		FetchedAt: at,
		Periods:   []models.UsagePeriod{{Name: "Daily", PeriodType: models.PeriodDaily}},
	}
	if err := (config.FileCache{}).Save(snapshot); err != nil {
		t.Fatalf("saving fresh cache: %v", err)
	}
	providerMap := map[string][]fetch.Strategy{
		"claude": {historyTestStrategy{available: true, result: fetch.ResultFatal("should not fetch")}},
	}

	var output bytes.Buffer
	oldWriter, oldJSON, oldNoCache := outWriter, jsonOutput, noCache
	outWriter, jsonOutput, noCache = &output, true, false
	t.Cleanup(func() {
		outWriter, jsonOutput, noCache = oldWriter, oldJSON, oldNoCache
	})

	if err := runHistoryWith(historyCmd, []string{"claude"}, config.DefaultConfig(), providerMap); err != nil {
		t.Fatalf("runHistoryWith() error = %v", err)
	}
	var report display.HistoryJSON
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("history JSON is invalid: %v", err)
	}
	providerReport := report.Providers["claude"]
	if !providerReport.Cached || !providerReport.FetchedAt.Equal(at) || providerReport.Source != "cache" {
		t.Errorf("cached provider report = %#v", providerReport)
	}
	if records, err := history.Read("claude"); err != nil || len(records) != 0 {
		t.Errorf("cached history read = %v, %v; want no recorded sample", records, err)
	}
}

func TestRunHistoryRecordBypassesFreshCacheAndWritesLiveSample(t *testing.T) {
	setHistoryTestContext(t, historyRecordCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	cachedAt := time.Now().UTC().Add(-10 * time.Second)
	if err := (config.FileCache{}).Save(models.UsageSnapshot{Provider: "claude", FetchedAt: cachedAt}); err != nil {
		t.Fatalf("saving fresh cache: %v", err)
	}
	liveAt := time.Now().UTC().Add(-time.Second)
	providerMap := map[string][]fetch.Strategy{
		"claude": {historyTestStrategy{available: true, result: fetch.ResultOK(models.UsageSnapshot{
			Provider:  "claude",
			FetchedAt: liveAt,
			Periods:   []models.UsagePeriod{{Name: "Daily", PeriodType: models.PeriodDaily}},
		})}},
	}
	oldNoCache := noCache
	noCache = false
	t.Cleanup(func() { noCache = oldNoCache })

	if err := runHistoryRecordWith(historyRecordCmd, config.DefaultConfig(), providerMap); err != nil {
		t.Fatalf("runHistoryRecordWith() error = %v", err)
	}
	records, err := history.Read("claude")
	if err != nil || len(records) != 1 {
		t.Fatalf("recorded history = %v, %v; want one sample", records, err)
	}
	if !records[0].Snapshot.FetchedAt.Equal(liveAt) {
		t.Errorf("recorded fetched_at = %s, want live %s", records[0].Snapshot.FetchedAt, liveAt)
	}
}

func TestRunHistoryRecordRejectsDisabledAndCachedOnlyRuns(t *testing.T) {
	setHistoryTestContext(t, historyRecordCmd)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	providerMap := map[string][]fetch.Strategy{
		"claude": {historyTestStrategy{available: true, result: fetch.ResultFatal("should not fetch")}},
	}
	disabled := config.DefaultConfig()
	disabled.History.Enabled = false
	if err := runHistoryRecordWith(historyRecordCmd, disabled, providerMap); err == nil || !strings.Contains(err.Error(), "recording is disabled") {
		t.Fatalf("disabled history error = %v", err)
	}

	cached := models.UsageSnapshot{Provider: "claude", FetchedAt: time.Now().UTC().Add(-time.Minute)}
	if err := (config.FileCache{}).Save(cached); err != nil {
		t.Fatalf("saving cache: %v", err)
	}
	if err := (config.FileThrottleStore{}).Save("claude", fetch.ThrottleMarker{
		RetryAt: time.Now().Add(time.Hour), Reason: "rate limited",
	}); err != nil {
		t.Fatalf("saving throttle: %v", err)
	}
	oldNoCache := noCache
	noCache = false
	t.Cleanup(func() { noCache = oldNoCache })

	if err := runHistoryRecordWith(historyRecordCmd, config.DefaultConfig(), providerMap); err == nil || !strings.Contains(err.Error(), "cached data only") {
		t.Fatalf("cached-only history record error = %v", err)
	}
	if records, err := history.Read("claude"); err != nil || len(records) != 0 {
		t.Errorf("cached-only history read = %v, %v; want no samples", records, err)
	}
}

func TestHistoryClearPromptsAndClearsOneProvider(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().Add(-time.Minute)
	if err := history.Append("claude", storedHistorySnapshot("claude", at)); err != nil {
		t.Fatalf("preparing Claude history: %v", err)
	}
	if err := history.Append("codex", storedHistorySnapshot("codex", at)); err != nil {
		t.Fatalf("preparing Codex history: %v", err)
	}

	mock := &prompt.Mock{ConfirmFunc: func(prompt.ConfirmConfig) (bool, error) { return true, nil }}
	oldPrompt, oldForce, oldQuiet, oldJSON, oldWriter := prompt.Default, historyClearForce, quiet, jsonOutput, outWriter
	var output bytes.Buffer
	prompt.SetDefault(mock)
	historyClearForce, quiet, jsonOutput, outWriter = false, false, false, &output
	t.Cleanup(func() {
		prompt.SetDefault(oldPrompt)
		historyClearForce, quiet, jsonOutput, outWriter = oldForce, oldQuiet, oldJSON, oldWriter
	})

	if err := runHistoryClear(historyClearCmd, []string{"claude"}); err != nil {
		t.Fatalf("runHistoryClear() error = %v", err)
	}
	if got := len(mock.ConfirmCalls); got != 1 {
		t.Errorf("confirmation calls = %d, want 1", got)
	}
	if records, err := history.Read("claude"); err != nil || len(records) != 0 {
		t.Errorf("Claude history after clear = %v, %v; want empty", records, err)
	}
	if records, err := history.Read("codex"); err != nil || len(records) != 1 {
		t.Errorf("Codex history after clear = %v, %v; want one record", records, err)
	}
	if !strings.Contains(output.String(), "Cleared usage history for Claude") {
		t.Errorf("clear output = %q", output.String())
	}
}

func TestHistoryClearDeclinedAndJSONRequiresForce(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().Add(-time.Minute)
	if err := history.Append("claude", storedHistorySnapshot("claude", at)); err != nil {
		t.Fatalf("preparing history: %v", err)
	}

	mock := &prompt.Mock{ConfirmFunc: func(prompt.ConfirmConfig) (bool, error) { return false, nil }}
	oldPrompt, oldForce, oldQuiet, oldJSON, oldWriter := prompt.Default, historyClearForce, quiet, jsonOutput, outWriter
	prompt.SetDefault(mock)
	historyClearForce, quiet, jsonOutput = false, false, false
	t.Cleanup(func() {
		prompt.SetDefault(oldPrompt)
		historyClearForce, quiet, jsonOutput, outWriter = oldForce, oldQuiet, oldJSON, oldWriter
	})

	if err := runHistoryClear(historyClearCmd, []string{"claude"}); err != nil {
		t.Fatalf("declined clear error = %v", err)
	}
	if records, err := history.Read("claude"); err != nil || len(records) != 1 {
		t.Errorf("history after declined clear = %v, %v; want one record", records, err)
	}

	var output bytes.Buffer
	outWriter, jsonOutput = &output, true
	if err := runHistoryClear(historyClearCmd, []string{"claude"}); err == nil || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("JSON clear without force error = %v", err)
	}
	if records, err := history.Read("claude"); err != nil || len(records) != 1 {
		t.Errorf("history after rejected JSON clear = %v, %v; want one record", records, err)
	}

	historyClearForce = true
	if err := runHistoryClear(historyClearCmd, []string{"claude"}); err != nil {
		t.Fatalf("forced JSON clear error = %v", err)
	}
	var result display.ActionResultJSON
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("clear JSON is invalid: %v", err)
	}
	if !result.Success || result.Provider != "claude" {
		t.Errorf("clear JSON = %#v", result)
	}
}

func TestHistoryClearQuietRequiresForceAndPrintsNothing(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().Add(-time.Minute)
	for _, providerID := range []string{"claude", "codex"} {
		if err := history.Append(providerID, storedHistorySnapshot(providerID, at)); err != nil {
			t.Fatalf("preparing %s history: %v", providerID, err)
		}
	}

	mock := &prompt.Mock{ConfirmFunc: func(prompt.ConfirmConfig) (bool, error) { return false, nil }}
	oldPrompt, oldForce, oldQuiet, oldJSON, oldWriter := prompt.Default, historyClearForce, quiet, jsonOutput, outWriter
	var output bytes.Buffer
	prompt.SetDefault(mock)
	historyClearForce, quiet, jsonOutput, outWriter = false, true, false, &output
	t.Cleanup(func() {
		prompt.SetDefault(oldPrompt)
		historyClearForce, quiet, jsonOutput, outWriter = oldForce, oldQuiet, oldJSON, oldWriter
	})

	if err := runHistoryClear(historyClearCmd, nil); err == nil || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("quiet clear without force error = %v", err)
	}
	for _, providerID := range []string{"claude", "codex"} {
		if records, err := history.Read(providerID); err != nil || len(records) != 1 {
			t.Errorf("%s history after rejected quiet clear = %v, %v; want one record", providerID, records, err)
		}
	}

	historyClearForce = true
	if err := runHistoryClear(historyClearCmd, nil); err != nil {
		t.Fatalf("forced quiet clear-all error = %v", err)
	}
	for _, providerID := range []string{"claude", "codex"} {
		if records, err := history.Read(providerID); err != nil || len(records) != 0 {
			t.Errorf("%s history after forced quiet clear-all = %v, %v; want empty", providerID, records, err)
		}
	}
	if output.Len() != 0 {
		t.Errorf("quiet clear output = %q, want empty", output.String())
	}
	if got := len(mock.ConfirmCalls); got != 0 {
		t.Errorf("confirmation calls = %d, want 0", got)
	}
}

func TestHistoryCommandRegistered(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() != "history" {
			continue
		}
		for _, subcommand := range cmd.Commands() {
			if subcommand.Name() == "record" {
				return
			}
		}
		t.Error("history command does not register record")
		return
	}
	t.Error("root command does not register history")
}
