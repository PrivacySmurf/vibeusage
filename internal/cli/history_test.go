package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/display"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/history"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/prompt"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestHistoryProviderIDs(t *testing.T) {
	disabled := false
	cfg := config.DefaultConfig()
	cfg.Providers["disabled"] = config.ProviderConfig{Enabled: &disabled}
	providerMap := map[string][]fetch.Strategy{"zeta": nil, "alpha": nil, "disabled": nil}

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
				"claude": {Success: true, Snapshot: &models.UsageSnapshot{}},
				"codex":  {Success: false, Error: "request failed"},
			},
			attempted: 2,
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

func TestHistoryClearPromptsAndClearsOneProvider(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().Add(-time.Minute)
	if err := history.Append("claude", models.UsageSnapshot{FetchedAt: at}); err != nil {
		t.Fatalf("preparing Claude history: %v", err)
	}
	if err := history.Append("codex", models.UsageSnapshot{FetchedAt: at}); err != nil {
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

func TestHistoryClearDeclinedAndJSONBypassesPrompt(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().Add(-time.Minute)
	if err := history.Append("claude", models.UsageSnapshot{FetchedAt: at}); err != nil {
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
	if err := runHistoryClear(historyClearCmd, []string{"claude"}); err != nil {
		t.Fatalf("JSON clear error = %v", err)
	}
	if got := len(mock.ConfirmCalls); got != 1 {
		t.Errorf("confirmation calls = %d, want 1 because JSON bypasses prompt", got)
	}
	var result display.ActionResultJSON
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("clear JSON is invalid: %v", err)
	}
	if !result.Success || result.Provider != "claude" {
		t.Errorf("clear JSON = %#v", result)
	}
}

func TestHistoryClearForceAndQuietBypassPrompt(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	at := time.Now().Add(-time.Minute)
	if err := history.Append("claude", models.UsageSnapshot{FetchedAt: at}); err != nil {
		t.Fatalf("preparing Claude history: %v", err)
	}

	mock := &prompt.Mock{ConfirmFunc: func(prompt.ConfirmConfig) (bool, error) { return false, nil }}
	oldPrompt, oldForce, oldQuiet, oldJSON, oldWriter := prompt.Default, historyClearForce, quiet, jsonOutput, outWriter
	prompt.SetDefault(mock)
	historyClearForce, quiet, jsonOutput = true, false, false
	t.Cleanup(func() {
		prompt.SetDefault(oldPrompt)
		historyClearForce, quiet, jsonOutput, outWriter = oldForce, oldQuiet, oldJSON, oldWriter
	})

	if err := runHistoryClear(historyClearCmd, []string{"claude"}); err != nil {
		t.Fatalf("forced clear error = %v", err)
	}
	if records, err := history.Read("claude"); err != nil || len(records) != 0 {
		t.Errorf("Claude history after forced clear = %v, %v; want empty", records, err)
	}

	for _, providerID := range []string{"claude", "codex"} {
		if err := history.Append(providerID, models.UsageSnapshot{FetchedAt: at}); err != nil {
			t.Fatalf("preparing %s history: %v", providerID, err)
		}
	}
	historyClearForce, quiet = false, true
	if err := runHistoryClear(historyClearCmd, nil); err != nil {
		t.Fatalf("quiet clear-all error = %v", err)
	}
	for _, providerID := range []string{"claude", "codex"} {
		if records, err := history.Read(providerID); err != nil || len(records) != 0 {
			t.Errorf("%s history after quiet clear-all = %v, %v; want empty", providerID, records, err)
		}
	}
	if got := len(mock.ConfirmCalls); got != 0 {
		t.Errorf("confirmation calls = %d, want 0 for force and quiet bypasses", got)
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
