// Package history stores usage snapshots as per-provider JSONL files.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

// Record is one line in a provider's JSONL history file.
type Record struct {
	V        int                  `json:"v"`
	Snapshot models.UsageSnapshot `json:"snapshot"`
}

const (
	CurrentRecordVersion = 1
	MaxAgeDays           = 90
	MaxFileBytes         = 8 << 20
	DedupFloor           = time.Minute
	compactionInterval   = 24 * time.Hour
)

func historyPath(providerID string) string {
	return filepath.Join(config.HistoryDir(), providerID+".jsonl")
}

// Append records a snapshot unless the newest stored record is less than a
// minute old. It compacts history lazily to enforce age and size limits.
func Append(providerID string, snap models.UsageSnapshot) error {
	return withHistoryLock(providerID, func() error {
		if err := os.MkdirAll(config.HistoryDir(), 0o755); err != nil {
			return fmt.Errorf("creating history directory for %s: %w", providerID, err)
		}
		return appendLocked(providerID, snap)
	})
}

func appendLocked(providerID string, snap models.UsageSnapshot) error {
	path := historyPath(providerID)
	info, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("stating history for %s: %w", providerID, statErr)
	}
	stale := statErr == nil && time.Since(info.ModTime()) > compactionInterval

	records, err := readUnlocked(providerID)
	if err != nil {
		return err
	}
	if len(records) > 0 && time.Since(records[len(records)-1].Snapshot.FetchedAt) < DedupFloor {
		return nil
	}

	data, err := json.Marshal(Record{V: CurrentRecordVersion, Snapshot: snap})
	if err != nil {
		return fmt.Errorf("encoding history for %s: %w", providerID, err)
	}
	data = append(data, '\n')

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening history for %s: %w", providerID, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing history for %s: %w", providerID, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing history for %s: %w", providerID, err)
	}

	info, err = os.Stat(path)
	if err != nil {
		return fmt.Errorf("stating history for %s: %w", providerID, err)
	}
	cutoff := time.Now().AddDate(0, 0, -MaxAgeDays)
	hasExpiredRecords := snap.FetchedAt.Before(cutoff)
	for _, record := range records {
		if record.Snapshot.FetchedAt.Before(cutoff) {
			hasExpiredRecords = true
			break
		}
	}
	if info.Size() > MaxFileBytes || stale || hasExpiredRecords {
		if err := compact(providerID); err != nil {
			return err
		}
	}
	return nil
}

// Read returns valid history records in oldest-first order. Empty and malformed
// lines are skipped so torn or interleaved appends do not make the history unreadable.
func Read(providerID string) (records []Record, err error) {
	err = withHistoryReadLock(providerID, func() error {
		records, err = readUnlocked(providerID)
		return err
	})
	return records, err
}

func readUnlocked(providerID string) ([]Record, error) {
	file, err := os.Open(historyPath(providerID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading history for %s: %w", providerID, err)
	}
	defer func() { _ = file.Close() }()

	var records []Record
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			var record Record
			if err := json.Unmarshal(line, &record); err == nil {
				records = append(records, record)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("reading history for %s: %w", providerID, readErr)
		}
	}
	return records, nil
}

// Clear removes one provider's history. An empty provider ID removes all history.
func Clear(providerID string) error {
	if providerID == "" {
		return withAllHistoryLock(func() error {
			if err := os.RemoveAll(config.HistoryDir()); err != nil {
				return fmt.Errorf("clearing history: %w", err)
			}
			return nil
		})
	}
	return withHistoryLock(providerID, func() error {
		if err := os.Remove(historyPath(providerID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clearing history for %s: %w", providerID, err)
		}
		return nil
	})
}

func compact(providerID string) error {
	records, err := readUnlocked(providerID)
	if err != nil {
		return err
	}

	cutoff := time.Now().AddDate(0, 0, -MaxAgeDays)
	kept := make([]Record, 0, len(records))
	for _, record := range records {
		if !record.Snapshot.FetchedAt.Before(cutoff) {
			kept = append(kept, record)
		}
	}

	lines := make([][]byte, len(kept))
	total := 0
	for i, record := range kept {
		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encoding history for %s: %w", providerID, err)
		}
		lines[i] = append(line, '\n')
		total += len(lines[i])
	}
	first := 0
	for total > MaxFileBytes && first < len(lines) {
		total -= len(lines[first])
		first++
	}

	data := make([]byte, 0, total)
	for _, line := range lines[first:] {
		data = append(data, line...)
	}
	if err := config.AtomicWriteFile(historyPath(providerID), data); err != nil {
		return fmt.Errorf("compacting history for %s: %w", providerID, err)
	}
	return nil
}
