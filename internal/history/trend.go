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
}

type observation struct {
	at          time.Time
	utilization int
}

// Trends calculates historical comparisons for periods in current. The
// snapshot's fetch time is the point at which its current values were observed.
func Trends(records []Record, current models.UsageSnapshot) []PeriodTrend {
	trends := make([]PeriodTrend, 0, len(current.Periods))
	seen := make(map[PeriodIdentity]struct{}, len(current.Periods))

	for _, period := range current.Periods {
		identity := identityOf(period)
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}

		trend := PeriodTrend{
			Identity:           identity,
			CurrentUtilization: period.Utilization,
		}
		asOf := current.FetchedAt
		if asOf.IsZero() {
			trends = append(trends, trend)
			continue
		}

		if period.ResetsAt == nil {
			duration, ok := periodDuration(period.PeriodType)
			if !ok {
				trends = append(trends, trend)
				continue
			}
			observations := observationsIn(records, identity, asOf.Add(-duration), asOf, nil, false, false)
			observations = addObservation(observations, observation{at: asOf, utilization: period.Utilization})
			trend.setBurnRate(observations)
			trends = append(trends, trend)
			continue
		}

		currentEnd := period.ResetsAt.UTC()
		currentStart, ok := periodStart(currentEnd, period.PeriodType)
		if !ok || asOf.Before(currentStart) || !asOf.Before(currentEnd) {
			trends = append(trends, trend)
			continue
		}
		previousStart, ok := periodStart(currentStart, period.PeriodType)
		if !ok {
			trends = append(trends, trend)
			continue
		}

		previous := observationsIn(records, identity, previousStart, currentStart, &currentStart, true, true)
		currentObservations := observationsIn(records, identity, currentStart, asOf, &currentEnd, true, false)
		currentObservations = addObservation(currentObservations, observation{at: asOf, utilization: period.Utilization})

		trend.setSamePoint(
			previous,
			elapsedRatio(asOf, currentStart, currentEnd.Sub(currentStart)),
			previousStart,
			currentStart.Sub(previousStart),
		)
		if len(previous) > 0 {
			last := previous[len(previous)-1].utilization
			trend.LastCompleteFinal = &last
		}
		trend.setBurnRate(currentObservations)
		trends = append(trends, trend)
	}

	return trends
}

func periodDuration(periodType models.PeriodType) (time.Duration, bool) {
	switch periodType {
	case models.PeriodSession:
		return 5 * time.Hour, true
	case models.PeriodDaily:
		return 24 * time.Hour, true
	case models.PeriodWeekly:
		return 7 * 24 * time.Hour, true
	case models.PeriodMonthly:
		return 30 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func periodStart(end time.Time, periodType models.PeriodType) (time.Time, bool) {
	duration, ok := periodDuration(periodType)
	if !ok {
		return time.Time{}, false
	}
	return end.Add(-duration), true
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
	observations = normalizeObservations(observations)
	trend.SamplesInPeriod = len(observations)
	if len(observations) < 2 {
		return
	}

	first := observations[0]
	last := observations[len(observations)-1]
	elapsedHours := last.at.Sub(first.at).Hours()
	if elapsedHours <= 0 {
		return
	}
	burnPerDay := float64(last.utilization-first.utilization) / elapsedHours * 24
	trend.BurnPerDay = &burnPerDay
}

func observationsIn(records []Record, identity PeriodIdentity, start, end time.Time, expectedReset *time.Time, tolerateStart, tolerateEnd bool) []observation {
	if tolerateStart {
		start = start.Add(-resetTolerance)
	}
	if tolerateEnd {
		end = end.Add(resetTolerance)
	}
	observations := make([]observation, 0)
	for _, record := range records {
		at := record.Snapshot.FetchedAt
		if at.Before(start) || !at.Before(end) {
			continue
		}
		for _, period := range record.Snapshot.Periods {
			if identityOf(period) == identity && resetsAt(period.ResetsAt, expectedReset) {
				observations = append(observations, observation{at: at, utilization: period.Utilization})
				break
			}
		}
	}
	return normalizeObservations(observations)
}

const resetTolerance = time.Minute

func resetsAt(actual, expected *time.Time) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	delta := actual.Sub(*expected)
	return delta >= -resetTolerance && delta <= resetTolerance
}

func addObservation(observations []observation, current observation) []observation {
	return normalizeObservations(append(observations, current))
}

func normalizeObservations(observations []observation) []observation {
	sort.SliceStable(observations, func(i, j int) bool {
		return observations[i].at.Before(observations[j].at)
	})
	if len(observations) < 2 {
		return observations
	}

	normalized := observations[:0]
	for _, candidate := range observations {
		last := len(normalized) - 1
		if last >= 0 && normalized[last].at.Equal(candidate.at) {
			normalized[last] = candidate
			continue
		}
		normalized = append(normalized, candidate)
	}
	return normalized
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
