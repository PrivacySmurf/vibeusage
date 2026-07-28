package history

import (
	"math"
	"sort"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

// PeriodIdentity identifies a usage period across snapshots.
type PeriodIdentity struct {
	PeriodType models.PeriodType
	Name       string
	Model      string
}

// PeriodTrend summarizes a current period using matching history records.
type PeriodTrend struct {
	Identity            PeriodIdentity
	CurrentUtilization  int
	SamePointLastPeriod *int
	SamePointDelta      *int
	LastCompleteFinal   *int
	BurnPerDay          *float64
	SamplesInPeriod     int
	DaysRecorded        int
}

type observation struct {
	at          time.Time
	utilization int
}

// Trends calculates historical comparisons for periods in current. The now
// argument keeps the calculation deterministic for callers and tests.
func Trends(records []Record, current models.UsageSnapshot, now time.Time) []PeriodTrend {
	daysRecorded := recordedDays(records)
	trends := make([]PeriodTrend, 0, len(current.Periods))
	seen := make(map[PeriodIdentity]struct{}, len(current.Periods))

	for _, period := range current.Periods {
		identity := PeriodIdentity{
			PeriodType: period.PeriodType,
			Name:       period.Name,
			Model:      period.Model,
		}
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}

		trend := PeriodTrend{
			Identity:           identity,
			CurrentUtilization: period.Utilization,
			DaysRecorded:       daysRecorded,
		}
		duration := time.Duration(period.PeriodType.Hours() * float64(time.Hour))

		if period.ResetsAt == nil {
			observations := observationsIn(records, identity, now.Add(-duration), now)
			trend.setBurnRate(observations)
			trends = append(trends, trend)
			continue
		}

		currentEnd := *period.ResetsAt
		currentStart := currentEnd.Add(-duration)
		previousStart := currentStart.Add(-duration)
		previous := observationsIn(records, identity, previousStart, currentStart)
		currentObservations := observationsIn(records, identity, currentStart, currentEnd)

		trend.setSamePoint(previous, elapsedRatio(now, currentStart, duration), previousStart, duration)
		if len(previous) > 0 {
			last := previous[len(previous)-1].utilization
			trend.LastCompleteFinal = &last
		}
		trend.setBurnRate(currentObservations)
		trends = append(trends, trend)
	}

	return trends
}

func (trend *PeriodTrend) setSamePoint(previous []observation, currentRatio float64, previousStart time.Time, duration time.Duration) {
	if len(previous) == 0 {
		return
	}

	closest := previous[0]
	closestDistance := math.Abs(elapsedRatio(closest.at, previousStart, duration) - currentRatio)
	for _, candidate := range previous[1:] {
		distance := math.Abs(elapsedRatio(candidate.at, previousStart, duration) - currentRatio)
		if distance < closestDistance {
			closest = candidate
			closestDistance = distance
		}
	}

	value := closest.utilization
	delta := trend.CurrentUtilization - value
	trend.SamePointLastPeriod = &value
	trend.SamePointDelta = &delta
}

func (trend *PeriodTrend) setBurnRate(observations []observation) {
	trend.SamplesInPeriod = len(observations)
	if len(observations) < 2 {
		return
	}

	slopes := make([]float64, 0, len(observations)-1)
	for i := 1; i < len(observations); i++ {
		elapsed := observations[i].at.Sub(observations[i-1].at).Hours()
		if elapsed == 0 {
			continue
		}
		slopes = append(slopes, float64(observations[i].utilization-observations[i-1].utilization)/elapsed)
	}
	if len(slopes) == 0 {
		return
	}

	sort.Float64s(slopes)
	middle := len(slopes) / 2
	median := slopes[middle]
	if len(slopes)%2 == 0 {
		median = (slopes[middle-1] + slopes[middle]) / 2
	}
	burnPerDay := median * 24
	trend.BurnPerDay = &burnPerDay
}

func observationsIn(records []Record, identity PeriodIdentity, start, end time.Time) []observation {
	observations := make([]observation, 0)
	for _, record := range records {
		at := record.Snapshot.FetchedAt
		if at.Before(start) || at.After(end) {
			continue
		}
		for _, period := range record.Snapshot.Periods {
			if identityOf(period) == identity {
				observations = append(observations, observation{at: at, utilization: period.Utilization})
				break
			}
		}
	}
	sort.SliceStable(observations, func(i, j int) bool {
		return observations[i].at.Before(observations[j].at)
	})
	return observations
}

func identityOf(period models.UsagePeriod) PeriodIdentity {
	return PeriodIdentity{
		PeriodType: period.PeriodType,
		Name:       period.Name,
		Model:      period.Model,
	}
}

func elapsedRatio(at, start time.Time, duration time.Duration) float64 {
	if duration <= 0 {
		return 0
	}
	ratio := at.Sub(start).Seconds() / duration.Seconds()
	return math.Max(0, math.Min(ratio, 1))
}

func recordedDays(records []Record) int {
	days := make(map[time.Time]struct{})
	for _, record := range records {
		at := record.Snapshot.FetchedAt.UTC()
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		days[day] = struct{}{}
	}
	return len(days)
}
