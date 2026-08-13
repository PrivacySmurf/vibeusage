package models

import (
	"strings"
	"time"
)

// ParseRFC3339Ptr parses an RFC 3339 (or ISO8601) timestamp and returns a
// pointer to the resulting time. Supports timestamps with or without fractional
// seconds and common offset formats. Returns nil if empty or unparseable.
func ParseRFC3339Ptr(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &t
	}
	layouts := []string{
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05Z07:00",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

// ClampPct clamps an integer percentage to the range [0, 100].
func ClampPct(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
