package display

import (
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

// SnapshotErrorJSON represents a failed fetch outcome.
type SnapshotErrorJSON struct {
	Error ErrorDetailJSON `json:"error"`
}

// ErrorDetailJSON is the nested error detail within SnapshotErrorJSON.
type ErrorDetailJSON struct {
	Message  string `json:"message"`
	Provider string `json:"provider"`
}

// multiProviderJSON is the top-level response for multi-provider fetches.
type multiProviderJSON struct {
	Providers map[string]models.UsageSnapshot `json:"providers"`
	Errors    map[string]string               `json:"errors"`
	FetchedAt string                          `json:"fetched_at"`
}

// StatusEntryJSON represents a single provider's status.
type StatusEntryJSON struct {
	Level       string `json:"level"`
	Description string `json:"description"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// AuthStatusEntryJSON represents a single provider's auth status.
type AuthStatusEntryJSON struct {
	Authenticated bool   `json:"authenticated"`
	Source        string `json:"source"`
	Disabled      bool   `json:"disabled,omitempty"`
}

// ConfigShowJSON represents the config show JSON output.
type ConfigShowJSON struct {
	Fetch       ConfigFetchJSON       `json:"fetch"`
	Display     ConfigDisplayJSON     `json:"display"`
	Credentials ConfigCredentialsJSON `json:"credentials"`
	History     ConfigHistoryJSON     `json:"history"`
	Roles       any                   `json:"roles"`
	Path        string                `json:"path"`
}

// ConfigFetchJSON represents the fetch section of config.
type ConfigFetchJSON struct {
	Timeout       float64 `json:"timeout"`
	MaxConcurrent int     `json:"max_concurrent"`
}

// ConfigDisplayJSON represents the display section of config.
type ConfigDisplayJSON struct {
	ShowRemaining bool   `json:"show_remaining"`
	ResetFormat   string `json:"reset_format"`
}

// ConfigCredentialsJSON represents the credentials section of config.
type ConfigCredentialsJSON struct {
	UseKeyring bool `json:"use_keyring"`
}

// ConfigHistoryJSON represents the history section of config.
type ConfigHistoryJSON struct {
	Enabled bool `json:"enabled"`
}

// ActionResultJSON is a generic success/message response used by
// config reset, cache clear, and similar operations.
type ActionResultJSON struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Reset    bool   `json:"reset,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// HistoryJSON represents historical usage trends grouped by provider.
type HistoryJSON struct {
	Providers map[string]HistoryProviderJSON `json:"providers"`
	Errors    map[string]string              `json:"errors"`
}

// HistoryProviderJSON represents a provider's history summary.
type HistoryProviderJSON struct {
	DaysRecorded int                 `json:"days_recorded"`
	Samples      int                 `json:"samples"`
	FetchedAt    time.Time           `json:"fetched_at"`
	Cached       bool                `json:"cached"`
	Source       string              `json:"source,omitempty"`
	Periods      []HistoryPeriodJSON `json:"periods"`
}

// HistoryPeriodJSON represents a period's current usage and trends.
type HistoryPeriodJSON struct {
	Name                    string   `json:"name"`
	PeriodType              string   `json:"period_type"`
	Model                   string   `json:"model,omitempty"`
	CurrentUtilization      int      `json:"current_utilization"`
	SamePointLastPeriod     *int     `json:"same_point_last_period,omitempty"`
	Delta                   *int     `json:"delta,omitempty"`
	BurnPerDay              *float64 `json:"burn_per_day,omitempty"`
	SamplesInPeriod         int      `json:"samples_in_period"`
	LastCompletePeriodFinal *int     `json:"last_complete_period_final,omitempty"`
}

// UpdateStatusJSON represents update check/apply output.
type UpdateStatusJSON struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	TargetVersion   string `json:"target_version"`
	UpdateAvailable bool   `json:"update_available"`
	IsDowngrade     bool   `json:"is_downgrade"`
	Asset           string `json:"asset,omitempty"`
	Applied         bool   `json:"applied,omitempty"`
	Pending         bool   `json:"pending,omitempty"`
}

// StatuslineJSON represents a single provider's condensed usage data.
type StatuslineJSON struct {
	Provider string                 `json:"provider"`
	Periods  []StatuslinePeriodJSON `json:"periods"`
	Overage  *StatuslineOverageJSON `json:"overage,omitempty"`
	Error    string                 `json:"error,omitempty"`
}

// StatuslinePeriodJSON represents a single period's condensed data.
type StatuslinePeriodJSON struct {
	Name        string `json:"name"`
	Utilization int    `json:"utilization"`
	PeriodType  string `json:"period_type"`
}

// StatuslineOverageJSON represents condensed overage data.
type StatuslineOverageJSON struct {
	Used        float64 `json:"used"`
	Limit       float64 `json:"limit"`
	Currency    string  `json:"currency"`
	Utilization int     `json:"utilization"`
}
