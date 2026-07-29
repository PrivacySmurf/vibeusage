package history

import (
	"reflect"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

func trendTime(month time.Month, day, hour int) time.Time {
	return time.Date(2026, month, day, hour, 0, 0, 0, time.UTC)
}

func trendRecord(at time.Time, period models.UsagePeriod) Record {
	return Record{V: CurrentRecordVersion, Snapshot: models.UsageSnapshot{
		Provider:  "test",
		FetchedAt: at,
		Periods:   []models.UsagePeriod{period},
	}}
}

func trendCurrent(at time.Time, period models.UsagePeriod) models.UsageSnapshot {
	return models.UsageSnapshot{Provider: "test", FetchedAt: at, Periods: []models.UsagePeriod{period}}
}

func dailyPeriod(utilization int, reset time.Time) models.UsagePeriod {
	return models.UsagePeriod{
		Name:        "Daily",
		Utilization: utilization,
		PeriodType:  models.PeriodDaily,
		ResetsAt:    &reset,
	}
}

func TestTrendsBurnUsesNetChangeThroughCurrentSnapshot(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	asOf := trendTime(time.July, 11, 14)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 2), dailyPeriod(10, reset)),
		trendRecord(trendTime(time.July, 11, 8), dailyPeriod(22, reset)),
		trendRecord(trendTime(time.July, 11, 12), dailyPeriod(30, reset)),
	}

	trend := Trends(records, trendCurrent(asOf, dailyPeriod(42, reset)))[0]
	if got, want := trend.SamplesInPeriod, 4; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 64 {
		t.Errorf("burn per day = %v, want 64", trend.BurnPerDay)
	}
}

func TestTrendsCurrentSnapshotAloneHasNoBurnRate(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	trend := Trends(nil, trendCurrent(trendTime(time.July, 11, 14), dailyPeriod(42, reset)))[0]
	if trend.BurnPerDay != nil {
		t.Errorf("burn per day = %v, want nil", *trend.BurnPerDay)
	}
	if got, want := trend.SamplesInPeriod, 1; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
}

func TestTrendsSamePointUsesClosestElapsedRatio(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	previousReset := trendTime(time.July, 11, 0)
	records := []Record{
		trendRecord(trendTime(time.July, 10, 10), dailyPeriod(20, previousReset)),
		trendRecord(trendTime(time.July, 10, 11), dailyPeriod(35, previousReset)),
	}

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 10), dailyPeriod(50, reset)))[0]
	if trend.SamePointLastPeriod == nil || *trend.SamePointLastPeriod != 20 {
		t.Errorf("same point = %v, want 20", trend.SamePointLastPeriod)
	}
	if trend.SamePointDelta == nil || *trend.SamePointDelta != 30 {
		t.Errorf("delta = %v, want 30", trend.SamePointDelta)
	}
}

func TestTrendsUsesHalfOpenResetWindows(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	currentStart := trendTime(time.July, 11, 0)
	previousReset := currentStart
	records := []Record{
		trendRecord(trendTime(time.July, 10, 23), dailyPeriod(80, previousReset)),
		trendRecord(currentStart, dailyPeriod(0, reset)),
		trendRecord(trendTime(time.July, 11, 1), dailyPeriod(10, reset)),
		trendRecord(reset, dailyPeriod(0, reset.Add(24*time.Hour))),
	}

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 2), dailyPeriod(20, reset)))[0]
	if trend.LastCompleteFinal == nil || *trend.LastCompleteFinal != 80 {
		t.Errorf("last complete final = %v, want 80", trend.LastCompleteFinal)
	}
	if got, want := trend.SamplesInPeriod, 3; got != want {
		t.Errorf("current samples = %d, want %d", got, want)
	}
}

func TestTrendsAcceptsSmallResetEstimateJitter(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	jitteredReset := reset.Add(30 * time.Second)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 2), dailyPeriod(10, jitteredReset)),
	}

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 8), dailyPeriod(40, reset)))[0]
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trend.BurnPerDay == nil {
		t.Fatal("burn per day = nil, want value")
	}
}

func TestTrendsAcceptsResetJitterAtWindowStart(t *testing.T) {
	reset := trendTime(time.July, 12, 0).Add(30 * time.Second)
	historicalReset := reset.Add(-30 * time.Second)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 0).Add(10*time.Second), dailyPeriod(0, historicalReset)),
	}

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 0).Add(time.Minute), dailyPeriod(10, reset)))[0]
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
}

func TestTrendsRejectsMismatchedHistoricalReset(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	wrongReset := trendTime(time.July, 13, 0)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 2), dailyPeriod(90, wrongReset)),
	}

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 4), dailyPeriod(20, reset)))[0]
	if trend.BurnPerDay != nil || trend.SamplesInPeriod != 1 {
		t.Errorf("mismatched reset affected trend: %+v", trend)
	}
}

func TestTrendsIgnoresFutureRecords(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	asOf := trendTime(time.July, 11, 8)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 2), dailyPeriod(10, reset)),
		trendRecord(trendTime(time.July, 11, 10), dailyPeriod(90, reset)),
	}

	trend := Trends(records, trendCurrent(asOf, dailyPeriod(30, reset)))[0]
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 80 {
		t.Errorf("burn per day = %v, want 80", trend.BurnPerDay)
	}
}

func TestTrendsWithoutResetUsesTrailingLookback(t *testing.T) {
	period := models.UsagePeriod{Name: "Credits", Utilization: 40, PeriodType: models.PeriodDaily}
	asOf := trendTime(time.July, 11, 12)
	records := []Record{
		trendRecord(trendTime(time.July, 10, 12), models.UsagePeriod{Name: "Credits", Utilization: 5, PeriodType: models.PeriodDaily}),
		trendRecord(trendTime(time.July, 11, 0), models.UsagePeriod{Name: "Credits", Utilization: 20, PeriodType: models.PeriodDaily}),
		trendRecord(trendTime(time.July, 9, 12), models.UsagePeriod{Name: "Credits", Utilization: 99, PeriodType: models.PeriodDaily}),
	}

	trend := Trends(records, trendCurrent(asOf, period))[0]
	if trend.SamePointLastPeriod != nil || trend.LastCompleteFinal != nil {
		t.Errorf("period without reset should have no comparisons: %+v", trend)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 35 {
		t.Errorf("burn per day = %v, want 35", trend.BurnPerDay)
	}
	if got, want := trend.SamplesInPeriod, 3; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
}

func TestTrendsOnlyReportsCurrentIdentities(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	records := []Record{trendRecord(trendTime(time.July, 11, 2), dailyPeriod(20, reset))}
	current := trendCurrent(trendTime(time.July, 11, 4), models.UsagePeriod{Name: "Weekly", Utilization: 10, PeriodType: models.PeriodWeekly, ResetsAt: &reset})

	trends := Trends(records, current)
	if len(trends) != 1 || trends[0].Identity.Name != "Weekly" {
		t.Errorf("trends = %+v, want only current Weekly period", trends)
	}
}

func TestTrendsMatchesPeriodsByTypeNameAndModel(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	previousReset := trendTime(time.July, 11, 0)
	modelA := models.UsagePeriod{Name: "Models", Model: "a", Utilization: 30, PeriodType: models.PeriodDaily, ResetsAt: &reset}
	modelB := models.UsagePeriod{Name: "Models", Model: "b", Utilization: 99, PeriodType: models.PeriodDaily, ResetsAt: &reset}
	records := []Record{
		{V: CurrentRecordVersion, Snapshot: models.UsageSnapshot{Provider: "test", FetchedAt: trendTime(time.July, 10, 12), Periods: []models.UsagePeriod{
			{Name: "Models", Model: "b", Utilization: 99, PeriodType: models.PeriodDaily, ResetsAt: &previousReset},
			{Name: "Models", Model: "a", Utilization: 20, PeriodType: models.PeriodDaily, ResetsAt: &previousReset},
		}}},
		{V: CurrentRecordVersion, Snapshot: models.UsageSnapshot{Provider: "test", FetchedAt: trendTime(time.July, 11, 2), Periods: []models.UsagePeriod{
			modelB,
			{Name: "Models", Model: "a", Utilization: 10, PeriodType: models.PeriodDaily, ResetsAt: &reset},
		}}},
	}

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 8), modelA))[0]
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trend.SamePointLastPeriod == nil || *trend.SamePointLastPeriod != 20 {
		t.Errorf("same point = %v, want 20", trend.SamePointLastPeriod)
	}
	if trend.LastCompleteFinal == nil || *trend.LastCompleteFinal != 20 {
		t.Errorf("last complete final = %v, want 20", trend.LastCompleteFinal)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 80 {
		t.Errorf("burn per day = %v, want 80", trend.BurnPerDay)
	}
}

func TestTrendsDeduplicatesCurrentTimestampAndUsesCurrentValue(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	asOf := trendTime(time.July, 11, 8)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 2), dailyPeriod(10, reset)),
		trendRecord(asOf, dailyPeriod(25, reset)),
		trendRecord(asOf, dailyPeriod(28, reset)),
	}

	trend := Trends(records, trendCurrent(asOf, dailyPeriod(40, reset)))[0]
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 120 {
		t.Errorf("burn per day = %v, want 120", trend.BurnPerDay)
	}
}

func TestTrendsIrregularCadenceUsesNetChange(t *testing.T) {
	period := models.UsagePeriod{Name: "Credits", Utilization: 33, PeriodType: models.PeriodDaily}
	asOf := trendTime(time.July, 11, 0)
	records := []Record{
		trendRecord(trendTime(time.July, 10, 0), models.UsagePeriod{Name: "Credits", Utilization: 0, PeriodType: models.PeriodDaily}),
		trendRecord(trendTime(time.July, 10, 1), models.UsagePeriod{Name: "Credits", Utilization: 10, PeriodType: models.PeriodDaily}),
	}

	trend := Trends(records, trendCurrent(asOf, period))[0]
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 33 {
		t.Errorf("burn per day = %v, want 33", trend.BurnPerDay)
	}
}

func TestMonthlyPeriodStartUsesFixedThirtyDays(t *testing.T) {
	marchEnd := time.Date(2026, time.March, 31, 0, 0, 0, 0, time.UTC)
	start, ok := periodStart(marchEnd, models.PeriodMonthly)
	if !ok || !start.Equal(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly boundary = %s, want 2026-03-01", start)
	}
}

func TestTrendsResetlessMonthlyUsesThirtyDayLookback(t *testing.T) {
	asOf := trendTime(time.July, 31, 0)
	period := models.UsagePeriod{Name: "Monthly", Utilization: 40, PeriodType: models.PeriodMonthly}
	records := []Record{trendRecord(asOf.Add(-30*24*time.Hour), models.UsagePeriod{
		Name: "Monthly", Utilization: 10, PeriodType: models.PeriodMonthly,
	})}

	trend := Trends(records, trendCurrent(asOf, period))[0]
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 1 {
		t.Errorf("burn per day = %v, want 1", trend.BurnPerDay)
	}
}

func TestTrendsUnknownPeriodDoesNotFabricateMetrics(t *testing.T) {
	period := models.UsagePeriod{Name: "Unknown", Utilization: 50, PeriodType: models.PeriodType("hourly")}
	trend := Trends(nil, trendCurrent(trendTime(time.July, 11, 0), period))[0]
	if trend.BurnPerDay != nil || trend.SamplesInPeriod != 0 {
		t.Errorf("unknown period produced metrics: %+v", trend)
	}
}

func TestTrendsKeepsZeroValuesAndDoesNotMutateRecords(t *testing.T) {
	reset := trendTime(time.July, 12, 0)
	previousReset := trendTime(time.July, 11, 0)
	records := []Record{
		trendRecord(trendTime(time.July, 11, 4), dailyPeriod(0, reset)),
		trendRecord(trendTime(time.July, 10, 4), dailyPeriod(0, previousReset)),
	}
	original := append([]Record(nil), records...)

	trend := Trends(records, trendCurrent(trendTime(time.July, 11, 4), dailyPeriod(0, reset)))[0]
	if trend.SamePointLastPeriod == nil || trend.SamePointDelta == nil {
		t.Errorf("zero comparison values should be non-nil: %+v", trend)
	}
	if !reflect.DeepEqual(records, original) {
		t.Errorf("Trends mutated records: got %#v, want %#v", records, original)
	}
}
