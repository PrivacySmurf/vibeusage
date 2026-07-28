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
	cfg := config.Get()
	providerMap := buildProviderMap()
	providerIDs, err := historyProviderIDs(providerMap, cfg, args)
	if err != nil {
		return err
	}

	filteredMap := make(map[string][]fetch.Strategy, len(providerIDs))
	for _, providerID := range providerIDs {
		filteredMap[providerID] = providerMap[providerID]
	}
	outcomes := fetch.FetchAllProviders(cmd.Context(), filteredMap, !noCache, orchestratorConfigFromConfig(cfg), nil)

	report := display.HistoryJSON{Providers: make(map[string]display.HistoryProviderJSON)}
	now := time.Now()
	for _, providerID := range providerIDs {
		outcome := outcomes[providerID]
		if !outcome.Success || outcome.Snapshot == nil {
			if len(args) == 1 {
				if outcome.Error == "" {
					return fmt.Errorf("fetching %s history failed", providerID)
				}
				return fmt.Errorf("fetching %s history: %s", providerID, outcome.Error)
			}
			continue
		}

		records, err := history.Read(providerID)
		if err != nil {
			return err
		}
		trends := history.Trends(records, *outcome.Snapshot, now)
		report.Providers[providerID] = historyProviderJSON(records, trends)
	}

	if jsonOutput {
		return display.OutputJSON(outWriter, report)
	}
	return renderHistoryReport(report, providerIDs)
}

func runHistoryRecord(cmd *cobra.Command, args []string) error {
	cfg := config.Get()
	providerMap := buildProviderMap()
	providerIDs, err := historyProviderIDs(providerMap, cfg, nil)
	if err != nil {
		return err
	}

	filteredMap := make(map[string][]fetch.Strategy, len(providerIDs))
	attempted := 0
	for _, providerID := range providerIDs {
		strategies := providerMap[providerID]
		filteredMap[providerID] = strategies
		if hasAvailableStrategy(strategies) {
			attempted++
		}
	}

	outcomes := fetch.FetchAllProviders(cmd.Context(), filteredMap, !noCache, orchestratorConfigFromConfig(cfg), nil)
	return historyRecordResult(outcomes, attempted)
}

func historyRecordResult(outcomes map[string]fetch.FetchOutcome, attempted int) error {
	if attempted == 0 {
		return fmt.Errorf("history record: no enabled providers are authenticated. Run `vibeusage auth` to enable a provider")
	}

	for _, outcome := range outcomes {
		if outcome.Success && outcome.Snapshot != nil {
			return nil
		}
	}
	return fmt.Errorf("history record: all %d providers failed", attempted)
}

func hasAvailableStrategy(strategies []fetch.Strategy) bool {
	for _, strategy := range strategies {
		if strategy.IsAvailable() {
			return true
		}
	}
	return false
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
		return []string{providerID}, nil
	}

	providerIDs := make([]string, 0, len(providerMap))
	for providerID := range providerMap {
		if cfg.IsProviderEnabled(providerID) {
			providerIDs = append(providerIDs, providerID)
		}
	}
	sort.Strings(providerIDs)
	if len(providerIDs) == 0 {
		return nil, fmt.Errorf("no providers are enabled. Run `vibeusage auth` to enable a provider")
	}
	return providerIDs, nil
}

func historyProviderJSON(records []history.Record, trends []history.PeriodTrend) display.HistoryProviderJSON {
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
		Periods:      periods,
	}
}

func renderHistoryReport(report display.HistoryJSON, providerIDs []string) error {
	for index, providerID := range providerIDs {
		providerReport, ok := report.Providers[providerID]
		if !ok {
			continue
		}
		if index > 0 {
			_, _ = fmt.Fprintln(outWriter)
		}
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
			[]string{"Period", "Type", "Now", "Last period", "Δ", "Burn/day", "Samples", "Last complete"},
			rows,
			display.TableOptions{Title: provider.DisplayName(providerID), NoColor: noColor},
		))
		_, _ = fmt.Fprintf(outWriter, "%d samples, %d days recorded\n", providerReport.Samples, providerReport.DaysRecorded)
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

	if !historyClearForce && !quiet && !jsonOutput {
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
	_, _ = fmt.Fprintln(outWriter, message)
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
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		days[day] = struct{}{}
	}
	return len(days)
}
