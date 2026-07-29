package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/display"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/history"
	"github.com/joshuadavidthomas/vibeusage/internal/prompt"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

var historyCmd = &cobra.Command{
	Use:   "history [provider]",
	Short: "Show usage history and trends",
	Long:  "Show current usage alongside matching historical usage trends.",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runHistory,
}

var historyClearCmd = &cobra.Command{
	Use:   "clear [provider]",
	Short: "Delete recorded usage history",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runHistoryClear,
}

var historyRecordCmd = &cobra.Command{
	Use:   "record",
	Short: "Record usage history for scheduled collection",
	Long:  "Fetch usage from every enabled provider and record it locally. Use this command with cron, systemd timers, or launchd agents; see the README for scheduling examples.",
	Args:  cobra.NoArgs,
	RunE:  runHistoryRecord,
}

var historyClearForce bool

func init() {
	historyClearCmd.Flags().BoolVarP(&historyClearForce, "force", "f", false, "Clear without confirmation")
	historyCmd.AddCommand(historyClearCmd, historyRecordCmd)
}

func runHistory(cmd *cobra.Command, args []string) error {
	return runHistoryWith(cmd, args, config.Get(), buildProviderMap())
}

func runHistoryWith(cmd *cobra.Command, args []string, cfg config.Config, providerMap map[string][]fetch.Strategy) error {
	providerIDs, err := historyProviderIDs(providerMap, cfg, args)
	if err != nil {
		return err
	}

	filteredMap := make(map[string][]fetch.Strategy, len(providerIDs))
	for _, providerID := range providerIDs {
		filteredMap[providerID] = providerMap[providerID]
	}
	outcomes := fetch.FetchAllProviders(cmd.Context(), filteredMap, !noCache, orchestratorConfigFromConfig(cfg), nil)

	report := display.HistoryJSON{
		Providers: make(map[string]display.HistoryProviderJSON),
		Errors:    make(map[string]string),
	}
	for _, providerID := range providerIDs {
		outcome := outcomes[providerID]
		if !outcome.Success || outcome.Snapshot == nil {
			errMsg := outcome.Error
			if errMsg == "" {
				errMsg = "fetch failed"
			}
			if len(args) == 1 {
				return fmt.Errorf("fetching %s history: %s", providerID, errMsg)
			}
			report.Errors[providerID] = errMsg
			continue
		}
		if len(outcome.Snapshot.Periods) == 0 {
			errMsg := "no usage periods available"
			if len(args) == 1 {
				return fmt.Errorf("fetching %s history: %s", providerID, errMsg)
			}
			report.Errors[providerID] = errMsg
			continue
		}

		records, err := history.Read(providerID)
		if err != nil {
			return err
		}
		trends := history.Trends(records, *outcome.Snapshot)
		report.Providers[providerID] = historyProviderJSON(records, trends, outcome)
	}

	if len(report.Providers) == 0 {
		return historyFetchError(report.Errors)
	}
	if jsonOutput {
		return display.OutputJSON(outWriter, report)
	}
	return renderHistoryReport(report, providerIDs)
}

func runHistoryRecord(cmd *cobra.Command, args []string) error {
	return runHistoryRecordWith(cmd, config.Get(), buildProviderMap())
}

func runHistoryRecordWith(cmd *cobra.Command, cfg config.Config, providerMap map[string][]fetch.Strategy) error {
	if !cfg.History.Enabled {
		return fmt.Errorf("history record: recording is disabled in config")
	}
	providerIDs, err := historyProviderIDs(providerMap, cfg, nil)
	if err != nil {
		return fmt.Errorf("history record: %w", err)
	}

	filteredMap := make(map[string][]fetch.Strategy, len(providerIDs))
	for _, providerID := range providerIDs {
		filteredMap[providerID] = providerMap[providerID]
	}

	orchestratorConfig := orchestratorConfigFromConfig(cfg)
	orchestratorConfig.Pipeline.FreshCacheTTL = 0
	outcomes := fetch.FetchAllProviders(cmd.Context(), filteredMap, !noCache, orchestratorConfig, nil)
	return historyRecordResult(outcomes, len(providerIDs))
}

func historyFetchError(errorsByProvider map[string]string) error {
	details := make([]string, 0, len(errorsByProvider))
	for providerID, errMsg := range errorsByProvider {
		details = append(details, providerID+": "+errMsg)
	}
	sort.Strings(details)
	if len(details) == 0 {
		return fmt.Errorf("fetching history failed for all providers")
	}
	return fmt.Errorf("fetching history failed for all providers: %s", strings.Join(details, "; "))
}

func historyRecordResult(outcomes map[string]fetch.FetchOutcome, attempted int) error {
	if attempted == 0 {
		return fmt.Errorf("history record: no enabled providers are authenticated. Run `vibeusage auth` to enable a provider")
	}

	recordingFailures := make([]string, 0)
	for providerID, outcome := range outcomes {
		if outcome.RecordingError != "" {
			recordingFailures = append(recordingFailures, providerID+": "+outcome.RecordingError)
		}
	}
	if len(recordingFailures) > 0 {
		sort.Strings(recordingFailures)
		return fmt.Errorf("history record: recording failed: %s", strings.Join(recordingFailures, "; "))
	}

	details := make([]string, 0, len(outcomes))
	for providerID, outcome := range outcomes {
		if outcome.Success && outcome.Snapshot != nil && outcome.Recorded {
			return nil
		}
		detail := outcome.Error
		if outcome.Cached {
			detail = "cached data only"
		} else if outcome.Success && outcome.Snapshot != nil {
			detail = "no usage periods to record"
		} else if detail == "" {
			detail = "fetch failed"
		}
		details = append(details, providerID+": "+detail)
	}
	sort.Strings(details)
	if len(details) == 0 {
		return fmt.Errorf("history record: all %d providers failed", attempted)
	}
	return fmt.Errorf("history record: all %d providers failed: %s", attempted, strings.Join(details, "; "))
}

func historyProviderIDs(providerMap map[string][]fetch.Strategy, cfg config.Config, args []string) ([]string, error) {
	if len(args) == 1 {
		providerID := args[0]
		if _, ok := provider.Get(providerID); !ok {
			return nil, fmt.Errorf("unknown provider: %s", providerID)
		}
		if !cfg.IsProviderEnabled(providerID) {
			return nil, fmt.Errorf("%s is disabled. Run `vibeusage auth` to re-enable it", provider.DisplayName(providerID))
		}
		if !hasAvailableHistoryStrategy(providerMap[providerID]) {
			return nil, fmt.Errorf("%s is not authenticated. Run `vibeusage auth %s` to connect it", provider.DisplayName(providerID), providerID)
		}
		return []string{providerID}, nil
	}

	providerIDs := make([]string, 0, len(providerMap))
	enabled := 0
	for providerID, strategies := range providerMap {
		if !cfg.IsProviderEnabled(providerID) {
			continue
		}
		enabled++
		if hasAvailableHistoryStrategy(strategies) {
			providerIDs = append(providerIDs, providerID)
		}
	}
	sort.Strings(providerIDs)
	if len(providerIDs) == 0 {
		if enabled == 0 {
			return nil, fmt.Errorf("no providers are enabled. Run `vibeusage auth` to enable a provider")
		}
		return nil, fmt.Errorf("no enabled providers are authenticated. Run `vibeusage auth` to connect a provider")
	}
	return providerIDs, nil
}

func hasAvailableHistoryStrategy(strategies []fetch.Strategy) bool {
	for _, strategy := range strategies {
		if strategy.IsAvailable() {
			return true
		}
	}
	return false
}

func historyProviderJSON(records []history.Record, trends []history.PeriodTrend, outcome fetch.FetchOutcome) display.HistoryProviderJSON {
	periods := make([]display.HistoryPeriodJSON, 0, len(trends))
	for _, trend := range trends {
		periods = append(periods, display.HistoryPeriodJSON{
			Name:                    trend.Identity.Name,
			PeriodType:              string(trend.Identity.PeriodType),
			Model:                   trend.Identity.Model,
			CurrentUtilization:      trend.CurrentUtilization,
			SamePointLastPeriod:     trend.SamePointLastPeriod,
			Delta:                   trend.SamePointDelta,
			BurnPerDay:              trend.BurnPerDay,
			SamplesInPeriod:         trend.SamplesInPeriod,
			LastCompletePeriodFinal: trend.LastCompleteFinal,
		})
	}

	return display.HistoryProviderJSON{
		DaysRecorded: recordedDays(records),
		Samples:      len(records),
		FetchedAt:    outcome.Snapshot.FetchedAt,
		Cached:       outcome.Cached,
		Source:       outcome.Source,
		Periods:      periods,
	}
}

func renderHistoryReport(report display.HistoryJSON, providerIDs []string) error {
	rendered := false
	for _, providerID := range providerIDs {
		providerReport, ok := report.Providers[providerID]
		if !ok {
			continue
		}
		if rendered {
			_, _ = fmt.Fprintln(outWriter)
		}
		rendered = true
		rows := make([][]string, 0, len(providerReport.Periods))
		for _, period := range providerReport.Periods {
			name := period.Name
			if period.Model != "" {
				name += " (" + period.Model + ")"
			}
			rows = append(rows, []string{
				name,
				period.PeriodType,
				formatPercent(period.CurrentUtilization),
				formatOptionalPercent(period.SamePointLastPeriod),
				formatDelta(period.Delta),
				formatBurnPerDay(period.BurnPerDay),
				strconv.Itoa(period.SamplesInPeriod),
				formatOptionalPercent(period.LastCompletePeriodFinal),
			})
		}
		_, _ = fmt.Fprintln(outWriter, display.NewTableWithOptions(
			[]string{"Period", "Type", "Now", "Last period", "Δ", "Burn/day", "Points", "Last complete"},
			rows,
			display.TableOptions{Title: provider.DisplayName(providerID), NoColor: noColor},
		))
		_, _ = fmt.Fprintf(outWriter, "%d samples, %d days recorded\n", providerReport.Samples, providerReport.DaysRecorded)
		if providerReport.Cached {
			_, _ = fmt.Fprintf(outWriter, "Current snapshot is cached from %s\n", providerReport.FetchedAt.Format(time.RFC3339))
		}
	}
	for _, providerID := range providerIDs {
		errMsg, ok := report.Errors[providerID]
		if !ok {
			continue
		}
		if rendered {
			_, _ = fmt.Fprintln(outWriter)
		}
		rendered = true
		_, _ = fmt.Fprintf(outWriter, "%s: %s\n", provider.DisplayName(providerID), errMsg)
	}
	return nil
}

func runHistoryClear(cmd *cobra.Command, args []string) error {
	providerID := ""
	if len(args) == 1 {
		providerID = args[0]
		if _, ok := provider.Get(providerID); !ok {
			return fmt.Errorf("unknown provider: %s", providerID)
		}
	}

	if !historyClearForce {
		if quiet || jsonOutput {
			return fmt.Errorf("history clear requires --force with --quiet or --json")
		}
		target := "all providers"
		if providerID != "" {
			target = provider.DisplayName(providerID)
		}
		confirmed, err := prompt.Default.Confirm(prompt.ConfirmConfig{
			Title:       "Clear usage history?",
			Description: fmt.Sprintf("This permanently deletes recorded history for %s.", target),
			Affirmative: "Clear",
			Negative:    "Cancel",
			Default:     false,
		})
		if err != nil {
			return fmt.Errorf("confirming history clear: %w", err)
		}
		if !confirmed {
			return nil
		}
	}

	if err := history.Clear(providerID); err != nil {
		return err
	}
	message := "Cleared usage history"
	if providerID != "" {
		message += " for " + provider.DisplayName(providerID)
	}
	if jsonOutput {
		return display.OutputJSON(outWriter, display.ActionResultJSON{
			Success:  true,
			Message:  message,
			Provider: providerID,
		})
	}
	if !quiet {
		_, _ = fmt.Fprintln(outWriter, message)
	}
	return nil
}

func formatPercent(value int) string {
	return strconv.Itoa(value) + "%"
}

func formatOptionalPercent(value *int) string {
	if value == nil {
		return "—"
	}
	return formatPercent(*value)
}

func formatDelta(value *int) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%+d pp", *value)
}

func formatBurnPerDay(value *float64) string {
	if value == nil {
		return "—"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", *value), "0"), ".") + " pp/day"
}

func recordedDays(records []history.Record) int {
	days := make(map[time.Time]struct{})
	for _, record := range records {
		at := record.Snapshot.FetchedAt.UTC()
		if at.IsZero() {
			continue
		}
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		days[day] = struct{}{}
	}
	return len(days)
}
