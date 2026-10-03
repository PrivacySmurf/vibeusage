package fetch

import (
	"context"
	"fmt"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/logging"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

// ExecutePipeline tries each strategy in order until one succeeds.
// When enabled, a very short fresh-cache window deduplicates bursty repeat
// invocations before any live fetch is attempted. All configuration is
// provided via cfg rather than read from a global singleton.
func ExecutePipeline(ctx context.Context, providerID string, strategies []Strategy, useCache bool, cfg PipelineConfig) FetchOutcome {
	logger := logging.FromContext(ctx)
	anyAttempted := false
	lastErr := ""

	// Refuse to serve cache or fetch when the caller already cancelled;
	// a canceled run must not report stale usage as success.
	if ctx.Err() != nil {
		return contextCancelledOutcome(providerID)
	}

	// Honor a persisted rate-limit cooldown before any network attempt.
	// Within the window, serve cache if present; otherwise surface the
	// cooldown as the error so the user sees why nothing fetched.
	if useCache && cfg.Throttles != nil && hasAvailableStrategy(strategies) {
		marker, err := cfg.Throttles.Load(providerID)
		if ctx.Err() != nil {
			return contextCancelledOutcome(providerID)
		}
		if err != nil {
			logger.Warn("loading throttle marker failed", "provider", providerID, "err", err)
			marker = nil
		}
		if marker != nil {
			// When backoff is deep (≥5 consecutive failures), try a health
			// probe to detect early recovery before the full backoff expires.
			if marker.ConsecutiveFailures >= 5 {
				if probed := probeStrategies(ctx, strategies, cfg.Timeout); probed {
					logger.Info("health probe succeeded, clearing throttle", "provider", providerID, "consecutive_failures", marker.ConsecutiveFailures)
					if cfg.Throttles != nil {
						_ = cfg.Throttles.Clear(providerID)
					}
					marker = nil // fall through to normal fetch
				}
			}
			if marker != nil {
				if cfg.Cache != nil {
					cached, err := cfg.Cache.Load(providerID)
					if ctx.Err() != nil {
						return contextCancelledOutcome(providerID)
					}
					if err != nil {
						logger.Warn("loading cached snapshot failed", "provider", providerID, "err", err)
						cached = nil
					}
					if cachedSnapshotMatchesProvider(cached, providerID) {
						return FetchOutcome{
							ProviderID: providerID,
							Success:    true,
							Snapshot:   cached,
							Source:     "cache (throttled)",
							Cached:     true,
						}
					}
				}
				reason := marker.Reason
				if reason == "" {
					reason = "Rate limited"
				}
				return FetchOutcome{
					ProviderID: providerID,
					Success:    false,
					Error:      fmt.Sprintf("%s; retry after %s (%d consecutive failures)", reason, marker.RetryAt.Format(time.RFC3339), marker.ConsecutiveFailures),
				}
			}
		}
	}

	if useCache && cfg.Cache != nil && cfg.FreshCacheTTL > 0 && hasAvailableStrategy(strategies) {
		cached, err := cfg.Cache.Load(providerID)
		if ctx.Err() != nil {
			return contextCancelledOutcome(providerID)
		}
		if err != nil {
			logger.Warn("loading cached snapshot failed", "provider", providerID, "err", err)
			cached = nil
		}
		if cachedSnapshotMatchesProvider(cached, providerID) && isFreshSnapshot(cached, cfg.FreshCacheTTL) {
			return FetchOutcome{
				ProviderID: providerID,
				Success:    true,
				Snapshot:   cached,
				Source:     "cache",
				Cached:     true,
			}
		}
	}

	for _, strategy := range strategies {
		if !strategy.IsAvailable() {
			continue
		}

		anyAttempted = true

		timeout := cfg.Timeout
		if extender, ok := strategy.(TimeoutExtender); ok {
			if extended := extender.ExtendTimeout(timeout); extended > timeout {
				timeout = extended
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		result, fetchErr := strategy.Fetch(attemptCtx)
		attemptErr := attemptCtx.Err()
		cancel()

		if ctx.Err() != nil {
			return contextCancelledOutcome(providerID)
		}
		if attemptErr == context.DeadlineExceeded {
			lastErr = "Fetch timed out"
			continue
		}
		if fetchErr != nil {
			lastErr = fetchErr.Error()
			continue
		}

		if result.RetryAfter != nil && cfg.Throttles != nil {
			reason := result.Error
			if reason == "" {
				reason = "Rate limited"
			}
			if err := cfg.Throttles.Save(providerID, ThrottleMarker{RetryAt: *result.RetryAfter, Reason: reason}); err != nil {
				logger.Warn("saving throttle marker failed", "provider", providerID, "err", err)
			}
		}

		if result.Success && result.Snapshot != nil {
			if result.Snapshot.Provider != providerID {
				lastErr = fmt.Sprintf("Provider mismatch: expected %s, got %s", providerID, result.Snapshot.Provider)
				continue
			}
			if cfg.Cache != nil {
				if err := cfg.Cache.Save(*result.Snapshot); err != nil {
					logger.Warn("saving cached snapshot failed", "provider", providerID, "err", err)
				}
			}
			recorded := false
			recordingError := ""
			if cfg.Recorder != nil {
				var err error
				recorded, err = cfg.Recorder.Record(*result.Snapshot)
				if err != nil {
					logger.Warn("recording usage history failed", "provider", providerID, "err", err)
					recordingError = err.Error()
				}
			}
			if cfg.Throttles != nil {
				if err := cfg.Throttles.Clear(providerID); err != nil {
					logger.Warn("clearing throttle marker failed", "provider", providerID, "err", err)
				}
			}

			return FetchOutcome{
				ProviderID:     providerID,
				Success:        true,
				Snapshot:       result.Snapshot,
				Source:         StrategyName(strategy),
				RecordingError: recordingError,
				Recorded:       recorded,
			}
		}

		if !result.ShouldFallback {
			return FetchOutcome{
				ProviderID: providerID,
				Success:    false,
				Error:      result.Error,
			}
		}

		lastErr = result.Error
	}

	// All strategies failed — try cache fallback.
	// Only serve cache when credentials exist (anyAttempted=true) but
	// the API failed. This provides resilience when services are down
	// without misleading unconfigured users with old data.
	if useCache && cfg.Cache != nil {
		cached, err := cfg.Cache.Load(providerID)
		if ctx.Err() != nil {
			return contextCancelledOutcome(providerID)
		}
		if err != nil {
			logger.Warn("loading cached snapshot failed", "provider", providerID, "err", err)
			cached = nil
		}
		if cachedSnapshotMatchesProvider(cached, providerID) && anyAttempted {
			logger.Warn("live fetch failed, serving stale cache", "provider", providerID, "err", lastErr, "cache_age", time.Since(cached.FetchedAt))
			return FetchOutcome{
				ProviderID: providerID,
				Success:    true,
				Snapshot:   cached,
				Source:     "cache",
				Cached:     true,
			}
		}
	}

	if lastErr == "" {
		lastErr = "No strategies available"
	}

	return FetchOutcome{
		ProviderID: providerID,
		Success:    false,
		Error:      lastErr,
	}
}

// contextCancelledOutcome reports a fetch aborted by caller cancellation.
// Tests assert on the "Context cancelled" string — grep before renaming.
func contextCancelledOutcome(providerID string) FetchOutcome {
	return FetchOutcome{
		ProviderID: providerID,
		Success:    false,
		Error:      "Context cancelled",
	}
}

func hasAvailableStrategy(strategies []Strategy) bool {
	for _, strategy := range strategies {
		if strategy.IsAvailable() {
			return true
		}
	}
	return false
}

// probeStrategies checks if any available strategy implements HealthProber
// and runs the probe. Returns true if the health probe succeeds (indicating
// the rate limit has cleared).
func probeStrategies(ctx context.Context, strategies []Strategy, timeout time.Duration) bool {
	for _, strategy := range strategies {
		if !strategy.IsAvailable() {
			continue
		}
		prober, ok := strategy.(HealthProber)
		if !ok {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		result := prober.ProbeHealth(probeCtx)
		cancel()
		return result
	}
	return false
}

func cachedSnapshotMatchesProvider(snapshot *models.UsageSnapshot, providerID string) bool {
	return snapshot != nil && snapshot.Provider == providerID
}

func isFreshSnapshot(snapshot *models.UsageSnapshot, ttl time.Duration) bool {
	if snapshot == nil || snapshot.FetchedAt.IsZero() || ttl <= 0 {
		return false
	}
	return time.Since(snapshot.FetchedAt) <= ttl
}
