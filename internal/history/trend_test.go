package history

import (
	"reflect"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

func trendTime(day, hour int) time.Time {
	return time.Date(2026, time.July, day, hour, 0, 0, 0, time.UTC)
}

func trendRecord(at time.Time, period models.UsagePeriod) Record {
	return Record{Snapshot: models.UsageSnapshot{
		FetchedAt: at,
		Periods:   []models.UsagePeriod{period},
	}}
}

func trendCurrent(period models.UsagePeriod) models.UsageSnapshot {
	return models.UsageSnapshot{Periods: []models.UsagePeriod{period}}
}

func dailyPeriod(utilization int, reset time.Time) models.UsagePeriod {
	return models.UsagePeriod{
		Name:        "Daily",
		Utilization: utilization,
		PeriodType:  models.PeriodDaily,
		ResetsAt:    &reset,
	}
}

func TestTrendsBurnSlope(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(42, reset)
	records := []Record{
		trendRecord(trendTime(11, 2), dailyPeriod(10, reset)),
		trendRecord(trendTime(11, 8), dailyPeriod(22, reset)),
		trendRecord(trendTime(11, 12), dailyPeriod(30, reset)),
	}

	trends := Trends(records, trendCurrent(period), trendTime(11, 14))
	if len(trends) != 1 {
		t.Fatalf("trend count = %d, want 1", len(trends))
	}
	if got, want := trends[0].SamplesInPeriod, 3; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trends[0].BurnPerDay == nil {
		t.Fatal("burn per day = nil, want value")
	}
	if got, want := *trends[0].BurnPerDay, 48.0; got != want {
		t.Errorf("burn per day = %v, want %v", got, want)
	}
}

func TestTrendsOneSampleHasNoBurnRate(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(42, reset)
	records := []Record{trendRecord(trendTime(11, 2), dailyPeriod(10, reset))}

	trend := Trends(records, trendCurrent(period), trendTime(11, 14))[0]
	if trend.BurnPerDay != nil {
		t.Errorf("burn per day = %v, want nil", *trend.BurnPerDay)
	}
	if got, want := trend.SamplesInPeriod, 1; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
}

func TestTrendsSamePointUsesClosestElapsedRatio(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(50, reset)
	records := []Record{
		trendRecord(trendTime(10, 10), dailyPeriod(20, reset)),
		trendRecord(trendTime(10, 11), dailyPeriod(35, reset)),
	}

	trend := Trends(records, trendCurrent(period), trendTime(11, 10))[0]
	if trend.SamePointLastPeriod == nil || *trend.SamePointLastPeriod != 20 {
		t.Errorf("same point = %v, want 20", trend.SamePointLastPeriod)
	}
	if trend.SamePointDelta == nil || *trend.SamePointDelta != 30 {
		t.Errorf("delta = %v, want 30", trend.SamePointDelta)
	}
}

func TestTrendsIgnoresTwoWindowsAgo(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(50, reset)
	records := []Record{trendRecord(trendTime(8, 8), dailyPeriod(90, reset))}

	trend := Trends(records, trendCurrent(period), trendTime(11, 8))[0]
	if trend.SamePointLastPeriod != nil {
		t.Errorf("same point = %v, want nil", *trend.SamePointLastPeriod)
	}
	if trend.LastCompleteFinal != nil {
		t.Errorf("last complete final = %v, want nil", *trend.LastCompleteFinal)
	}
}

func TestTrendsWithoutResetUsesTrailingLookback(t *testing.T) {
	period := models.UsagePeriod{Name: "Credits", Utilization: 40, PeriodType: models.PeriodDaily}
	now := trendTime(11, 12)
	records := []Record{
		trendRecord(trendTime(10, 12), models.UsagePeriod{Name: "Credits", Utilization: 5, PeriodType: models.PeriodDaily}),
		trendRecord(trendTime(11, 0), models.UsagePeriod{Name: "Credits", Utilization: 20, PeriodType: models.PeriodDaily}),
		trendRecord(trendTime(9, 12), models.UsagePeriod{Name: "Credits", Utilization: 99, PeriodType: models.PeriodDaily}),
	}

	trend := Trends(records, trendCurrent(period), now)[0]
	if trend.SamePointLastPeriod != nil || trend.LastCompleteFinal != nil {
		t.Errorf("period without reset should have no Q1/Q2: %+v", trend)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 30 {
		t.Errorf("burn per day = %v, want 30", trend.BurnPerDay)
	}
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
}

func TestTrendsOnlyReportsCurrentIdentities(t *testing.T) {
	reset := trendTime(12, 0)
	records := []Record{trendRecord(trendTime(11, 2), dailyPeriod(20, reset))}
	current := trendCurrent(models.UsagePeriod{Name: "Weekly", Utilization: 10, PeriodType: models.PeriodWeekly, ResetsAt: &reset})

	trends := Trends(records, current, trendTime(11, 4))
	if len(trends) != 1 || trends[0].Identity.Name != "Weekly" {
		t.Errorf("trends = %+v, want only current Weekly period", trends)
	}
}

func TestTrendsMatchesPeriodsByTypeNameAndModel(t *testing.T) {
	reset := trendTime(12, 0)
	modelA := models.UsagePeriod{Name: "Models", Model: "a", Utilization: 30, PeriodType: models.PeriodDaily, ResetsAt: &reset}
	modelB := models.UsagePeriod{Name: "Models", Model: "b", Utilization: 99, PeriodType: models.PeriodDaily, ResetsAt: &reset}
	records := []Record{
		{Snapshot: models.UsageSnapshot{FetchedAt: trendTime(10, 12), Periods: []models.UsagePeriod{
			modelB,
			{Name: "Models", Model: "a", Utilization: 20, PeriodType: models.PeriodDaily, ResetsAt: &reset},
		}}},
		{Snapshot: models.UsageSnapshot{FetchedAt: trendTime(11, 2), Periods: []models.UsagePeriod{
			modelB,
			{Name: "Models", Model: "a", Utilization: 10, PeriodType: models.PeriodDaily, ResetsAt: &reset},
		}}},
		{Snapshot: models.UsageSnapshot{FetchedAt: trendTime(11, 8), Periods: []models.UsagePeriod{
			{Name: "Models", Model: "a", Utilization: 22, PeriodType: models.PeriodDaily, ResetsAt: &reset},
			modelB,
		}}},
	}

	trend := Trends(records, trendCurrent(modelA), trendTime(11, 12))[0]
	if got, want := trend.SamplesInPeriod, 2; got != want {
		t.Errorf("samples in period = %d, want %d", got, want)
	}
	if trend.SamePointLastPeriod == nil || *trend.SamePointLastPeriod != 20 {
		t.Errorf("same point = %v, want 20", trend.SamePointLastPeriod)
	}
	if trend.LastCompleteFinal == nil || *trend.LastCompleteFinal != 20 {
		t.Errorf("last complete final = %v, want 20", trend.LastCompleteFinal)
	}
	if trend.BurnPerDay == nil || *trend.BurnPerDay != 48 {
		t.Errorf("burn per day = %v, want 48", trend.BurnPerDay)
	}
}

func TestTrendsDeltaCanBeNegative(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(33, reset)
	records := []Record{trendRecord(trendTime(10, 21), dailyPeriod(40, reset))}

	trend := Trends(records, trendCurrent(period), trendTime(11, 21))[0]
	if trend.SamePointDelta == nil || *trend.SamePointDelta != -7 {
		t.Errorf("delta = %v, want -7", trend.SamePointDelta)
	}
}

func TestTrendsLastCompleteFinalUsesLatestTimestamp(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(50, reset)
	records := []Record{
		trendRecord(trendTime(10, 20), dailyPeriod(90, reset)),
		trendRecord(trendTime(10, 4), dailyPeriod(20, reset)),
	}

	trend := Trends(records, trendCurrent(period), trendTime(11, 4))[0]
	if trend.LastCompleteFinal == nil || *trend.LastCompleteFinal != 90 {
		t.Errorf("last complete final = %v, want 90", trend.LastCompleteFinal)
	}
}

func TestTrendsKeepsZeroValuesAndDoesNotMutateRecords(t *testing.T) {
	reset := trendTime(12, 0)
	period := dailyPeriod(0, reset)
	records := []Record{
		trendRecord(trendTime(11, 4), dailyPeriod(0, reset)),
		trendRecord(trendTime(10, 4), dailyPeriod(0, reset)),
	}
	original := append([]Record(nil), records...)

	trend := Trends(records, trendCurrent(period), trendTime(11, 4))[0]
	if trend.SamePointLastPeriod == nil || trend.SamePointDelta == nil {
		t.Errorf("zero Q1 values should be non-nil: %+v", trend)
	}
	if !reflect.DeepEqual(records, original) {
		t.Errorf("Trends mutated records: got %#v, want %#v", records, original)
	}
}
