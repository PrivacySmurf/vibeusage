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
	"sort"
	"strings"
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
	CurrentRecordVersion = 2
	legacyRecordVersion  = 1
	MaxAgeDays           = 90
	MaxFileBytes         = 8 << 20
	DedupFloor           = time.Minute
	compactionInterval   = 24 * time.Hour
)

func historyPath(providerID string) string {
	return filepath.Join(config.HistoryDir(), providerID+".jsonl")
}

func validateProviderID(providerID string) error {
	if providerID == "" {
		return fmt.Errorf("history provider ID is empty")
	}
	for _, char := range providerID {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return fmt.Errorf("invalid history provider ID %q", providerID)
	}
	return nil
}

func historySnapshot(providerID string, snap models.UsageSnapshot) models.UsageSnapshot {
	return models.UsageSnapshot{
		Provider:  providerID,
		FetchedAt: snap.FetchedAt.UTC(),
		Periods:   snap.Periods,
	}
}

// Append records a snapshot unless the newest stored record is less than a
// minute old. It compacts history lazily to enforce age and size limits.
func Append(providerID string, snap models.UsageSnapshot) error {
	if err := validateProviderID(providerID); err != nil {
		return err
	}
	if snap.Provider != providerID {
		return fmt.Errorf("recording history for %s: snapshot provider is %q", providerID, snap.Provider)
	}
	if snap.FetchedAt.IsZero() {
		return fmt.Errorf("recording history for %s: fetched_at is missing", providerID)
	}
	now := time.Now()
	if snap.FetchedAt.After(now.Add(DedupFloor)) {
		return fmt.Errorf("recording history for %s: fetched_at is in the future", providerID)
	}
	if snap.FetchedAt.Before(now.AddDate(0, 0, -MaxAgeDays)) {
		return fmt.Errorf("recording history for %s: fetched_at is older than the retention window", providerID)
	}
	snap = historySnapshot(providerID, snap)

	return withHistoryLock(providerID, func() error {
		if err := os.MkdirAll(config.HistoryDir(), 0o700); err != nil {
			return fmt.Errorf("creating history directory for %s: %w", providerID, err)
		}
		if err := os.Chmod(config.HistoryDir(), 0o700); err != nil {
			return fmt.Errorf("securing history directory for %s: %w", providerID, err)
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

	records, err := readUnlocked(providerID)
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -MaxAgeDays)
	needsRewrite, err := historyFileNeedsRewrite(providerID)
	if err != nil {
		return err
	}
	needsCompaction := needsRewrite || statErr == nil && (time.Since(info.ModTime()) > compactionInterval || info.Size() > MaxFileBytes)
	if !needsCompaction {
		futureCutoff := time.Now().Add(DedupFloor)
		for _, record := range records {
			if record.V != CurrentRecordVersion ||
				record.Snapshot.FetchedAt.Before(cutoff) ||
				record.Snapshot.FetchedAt.After(futureCutoff) {
				needsCompaction = true
				break
			}
		}
	}
	if needsCompaction {
		if err := compact(providerID); err != nil {
			return err
		}
		records, err = readUnlocked(providerID)
		if err != nil {
			return err
		}
	}

	for _, record := range records {
		delta := snap.FetchedAt.Sub(record.Snapshot.FetchedAt)
		if delta > -DedupFloor && delta < DedupFloor {
			return nil
		}
	}

	data, err := json.Marshal(Record{V: CurrentRecordVersion, Snapshot: snap})
	if err != nil {
		return fmt.Errorf("encoding history for %s: %w", providerID, err)
	}
	data = append(data, '\n')
	if len(data) > MaxFileBytes {
		return fmt.Errorf("encoding history for %s: record exceeds %d bytes", providerID, MaxFileBytes)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening history for %s: %w", providerID, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("securing history for %s: %w", providerID, err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stating history for %s: %w", providerID, err)
	}
	if fileInfo.Size() > 0 {
		lastByte := []byte{0}
		if _, err := file.ReadAt(lastByte, fileInfo.Size()-1); err != nil {
			_ = file.Close()
			return fmt.Errorf("reading history tail for %s: %w", providerID, err)
		}
		if lastByte[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
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
	if info.Size() > MaxFileBytes {
		return compact(providerID)
	}
	return nil
}

// Read returns valid history records in oldest-first order. Empty and malformed
// lines are skipped so torn or interleaved appends do not make the history unreadable.
func Read(providerID string) (records []Record, err error) {
	if err := validateProviderID(providerID); err != nil {
		return nil, err
	}
	needsRewrite := false
	err = withHistoryReadLock(providerID, func() error {
		records, err = readUnlocked(providerID)
		if err != nil {
			return err
		}
		needsRewrite, err = historyFileNeedsRewrite(providerID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !needsRewrite {
		return readableRecords(records), nil
	}

	err = withHistoryLock(providerID, func() error {
		if err := compact(providerID); err != nil {
			return err
		}
		records, err = readUnlocked(providerID)
		return err
	})
	return readableRecords(records), err
}

// Migrate removes private fields and invalid rows from every history file,
// including files for providers that are disabled or no longer authenticated.
func Migrate() error {
	entries, err := os.ReadDir(config.HistoryDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading history directory for migration: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if validateProviderID(providerID) != nil {
			continue
		}
		if err := withHistoryLock(providerID, func() error {
			needsRewrite, err := historyFileNeedsRewrite(providerID)
			if err != nil || !needsRewrite {
				return err
			}
			return compact(providerID)
		}); err != nil {
			return err
		}
	}
	return nil
}

func historyFileNeedsRewrite(providerID string) (bool, error) {
	file, err := os.Open(historyPath(providerID))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading history for %s migration: %w", providerID, err)
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		var version struct {
			V int `json:"v"`
		}
		trimmed := bytes.TrimSpace(line)
		if json.Unmarshal(trimmed, &version) == nil {
			if version.V == legacyRecordVersion {
				return true, nil
			}
			if version.V == CurrentRecordVersion {
				var record Record
				if json.Unmarshal(trimmed, &record) == nil &&
					(record.Snapshot.Provider != providerID ||
						record.Snapshot.FetchedAt.IsZero() ||
						len(record.Snapshot.Periods) == 0 ||
						record.Snapshot.FetchedAt.After(time.Now().Add(DedupFloor))) {
					return true, nil
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil {
			return false, fmt.Errorf("reading history for %s migration: %w", providerID, readErr)
		}
	}
}

func readableRecords(records []Record) []Record {
	futureCutoff := time.Now().Add(DedupFloor)
	readable := records[:0]
	for _, record := range records {
		if !record.Snapshot.FetchedAt.After(futureCutoff) {
			readable = append(readable, record)
		}
	}
	return readable
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
			var version struct {
				V int `json:"v"`
			}
			if err := json.Unmarshal(line, &version); err == nil {
				if version.V > CurrentRecordVersion {
					return nil, fmt.Errorf("reading history for %s: unsupported record version %d", providerID, version.V)
				}
				if version.V >= legacyRecordVersion && version.V <= CurrentRecordVersion {
					var record Record
					if err := json.Unmarshal(line, &record); err == nil &&
						record.Snapshot.Provider == providerID &&
						!record.Snapshot.FetchedAt.IsZero() &&
						len(record.Snapshot.Periods) > 0 {
						record.Snapshot = historySnapshot(providerID, record.Snapshot)
						records = append(records, record)
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("reading history for %s: %w", providerID, readErr)
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Snapshot.FetchedAt.Before(records[j].Snapshot.FetchedAt)
	})
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
	if err := validateProviderID(providerID); err != nil {
		return err
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
	futureCutoff := time.Now().Add(DedupFloor)
	kept := make([]Record, 0, len(records))
	for _, record := range records {
		if !record.Snapshot.FetchedAt.Before(cutoff) && !record.Snapshot.FetchedAt.After(futureCutoff) {
			kept = append(kept, record)
		}
	}

	lines := make([][]byte, len(kept))
	total := 0
	for i, record := range kept {
		record.V = CurrentRecordVersion
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
