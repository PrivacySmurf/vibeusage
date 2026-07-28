package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

func testSnapshot(providerID, name string, fetchedAt time.Time) models.UsageSnapshot {
	return models.UsageSnapshot{
		Provider:  providerID,
		FetchedAt: fetchedAt.UTC(),
		Periods: []models.UsagePeriod{{
			Name:        name,
			Utilization: 42,
			PeriodType:  models.PeriodDaily,
		}},
	}
}

func writeRecords(t *testing.T, providerID string, records ...Record) {
	t.Helper()
	if err := os.MkdirAll(config.HistoryDir(), 0o755); err != nil {
		t.Fatalf("creating history directory: %v", err)
	}

	var data bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("encoding history record: %v", err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(historyPath(providerID), data.Bytes(), 0o600); err != nil {
		t.Fatalf("writing history: %v", err)
	}
}

func TestAppendCreatesPrivateHistoryFile(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	if err := Append("claude", testSnapshot("claude", "daily", time.Now())); err != nil {
		t.Fatalf("appending history: %v", err)
	}

	info, err := os.Stat(historyPath("claude"))
	if err != nil {
		t.Fatalf("stating history file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Errorf("history file mode = %o, want %o", got, want)
		}
	}
	if got, want := filepath.Dir(historyPath("claude")), config.HistoryDir(); got != want {
		t.Errorf("history directory = %q, want %q", got, want)
	}
}

func TestAppendAndReadPreservesOrder(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	first := testSnapshot("claude", "first", time.Now().Add(-2*DedupFloor))
	second := testSnapshot("claude", "second", time.Now())
	for _, snap := range []models.UsageSnapshot{first, second} {
		if err := Append("claude", snap); err != nil {
			t.Fatalf("appending history: %v", err)
		}
	}

	records, err := Read("claude")
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	if got, want := records[0].Snapshot, first; !reflect.DeepEqual(got, want) {
		t.Errorf("first snapshot = %#v, want %#v", got, want)
	}
	if got, want := records[1].Snapshot, second; !reflect.DeepEqual(got, want) {
		t.Errorf("second snapshot = %#v, want %#v", got, want)
	}
}

func TestReadSkipsMalformedLines(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	first := Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "first", time.Now().Add(-2*time.Minute))}
	second := Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "second", time.Now())}
	firstLine, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("encoding first record: %v", err)
	}
	secondLine, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("encoding second record: %v", err)
	}
	if err := os.MkdirAll(config.HistoryDir(), 0o755); err != nil {
		t.Fatalf("creating history directory: %v", err)
	}
	data := bytes.Join([][]byte{firstLine, []byte("not json"), secondLine, []byte(`{"v":`)}, []byte("\n"))
	if err := os.WriteFile(historyPath("claude"), data, 0o600); err != nil {
		t.Fatalf("writing history: %v", err)
	}

	records, err := Read("claude")
	if err != nil {
		t.Fatalf("reading history: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(records[0], first) || !reflect.DeepEqual(records[1], second) {
		t.Errorf("records = %#v, want %#v then %#v", records, first, second)
	}
}

func TestAppendDeduplicatesRecentHistory(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	if err := Append("claude", testSnapshot("claude", "first", time.Now())); err != nil {
		t.Fatalf("appending first history record: %v", err)
	}
	if err := Append("claude", testSnapshot("claude", "duplicate", time.Now())); err != nil {
		t.Fatalf("appending duplicate history record: %v", err)
	}
	records, err := Read("claude")
	if err != nil {
		t.Fatalf("reading deduplicated history: %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("record count after duplicate = %d, want %d", got, want)
	}

	writeRecords(t, "claude", Record{
		V:        CurrentRecordVersion,
		Snapshot: testSnapshot("claude", "older", time.Now().Add(-DedupFloor-time.Second)),
	})
	if err := Append("claude", testSnapshot("claude", "after-floor", time.Now())); err != nil {
		t.Fatalf("appending history after dedup floor: %v", err)
	}
	records, err = Read("claude")
	if err != nil {
		t.Fatalf("reading history after dedup floor: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Errorf("record count after dedup floor = %d, want %d", got, want)
	}
}

func TestAppendDeduplicatesConcurrentHistory(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	const writers = 64
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- Append("claude", testSnapshot("claude", "same-sample", time.Now()))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Append() error: %v", err)
		}
	}

	records, err := Read("claude")
	if err != nil {
		t.Fatalf("reading concurrent history: %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("record count after concurrent duplicate appends = %d, want %d", got, want)
	}
}

func TestAppendCompactsExpiredRecords(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	writeRecords(t, "claude",
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "expired", time.Now().AddDate(0, 0, -MaxAgeDays-1))},
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "current", time.Now().Add(-DedupFloor-time.Second))},
	)
	if err := Append("claude", testSnapshot("claude", "new", time.Now())); err != nil {
		t.Fatalf("appending history: %v", err)
	}
	records, err := Read("claude")
	if err != nil {
		t.Fatalf("reading compacted history: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	for _, record := range records {
		if record.Snapshot.Periods[0].Name == "expired" {
			t.Error("expired record remained after compaction")
		}
	}
}

func TestAppendCompactsOversizedHistory(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	writeRecords(t, "claude",
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", strings.Repeat("x", MaxFileBytes), time.Now().Add(-2*time.Hour))},
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "current", time.Now().Add(-DedupFloor-time.Second))},
	)
	if err := Append("claude", testSnapshot("claude", "new", time.Now())); err != nil {
		t.Fatalf("appending history: %v", err)
	}

	info, err := os.Stat(historyPath("claude"))
	if err != nil {
		t.Fatalf("stating compacted history: %v", err)
	}
	if info.Size() > MaxFileBytes {
		t.Errorf("history file size = %d, exceeds %d", info.Size(), MaxFileBytes)
	}
	records, err := Read("claude")
	if err != nil {
		t.Fatalf("reading compacted history: %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	for _, record := range records {
		if record.Snapshot.Periods[0].Name != "current" && record.Snapshot.Periods[0].Name != "new" {
			t.Errorf("unexpected record after size compaction: %q", record.Snapshot.Periods[0].Name)
		}
	}
}

func TestAppendCompactionPreservesPrivateMode(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	writeRecords(t, "claude", Record{
		V:        CurrentRecordVersion,
		Snapshot: testSnapshot("claude", "current", time.Now().Add(-DedupFloor-time.Second)),
	})
	file, err := os.OpenFile(historyPath("claude"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening history for malformed line: %v", err)
	}
	if _, err := file.WriteString("not json\n"); err != nil {
		_ = file.Close()
		t.Fatalf("writing malformed history line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing history after malformed line: %v", err)
	}
	oldMtime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(historyPath("claude"), oldMtime, oldMtime); err != nil {
		t.Fatalf("aging history file: %v", err)
	}
	if err := Append("claude", testSnapshot("claude", "new", time.Now())); err != nil {
		t.Fatalf("appending history: %v", err)
	}

	info, err := os.Stat(historyPath("claude"))
	if err != nil {
		t.Fatalf("stating compacted history: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Errorf("history file mode = %o, want %o", got, want)
		}
	}
	data, err := os.ReadFile(historyPath("claude"))
	if err != nil {
		t.Fatalf("reading compacted history: %v", err)
	}
	if bytes.Contains(data, []byte("not json")) {
		t.Error("stale-file compaction did not discard malformed line")
	}
}

func TestReadWaitsForActiveAppendLock(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	writeRecords(t, "claude", Record{
		V:        CurrentRecordVersion,
		Snapshot: testSnapshot("claude", "daily", time.Now()),
	})

	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withHistoryLock("claude", func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	readStarted := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		close(readStarted)
		_, err := Read("claude")
		readDone <- err
	}()
	<-readStarted

	returnedWhileLocked := false
	select {
	case <-readDone:
		returnedWhileLocked = true
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("holding history lock: %v", err)
	}
	if !returnedWhileLocked {
		if err := <-readDone; err != nil {
			t.Fatalf("Read() error: %v", err)
		}
	}
	if returnedWhileLocked {
		t.Error("Read() returned while an append lock was active")
	}
}

func TestClearWaitsForActiveAppendLock(t *testing.T) {
	for _, providerID := range []string{"claude", ""} {
		name := providerID
		if name == "" {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
			if err := os.MkdirAll(config.HistoryDir(), 0o755); err != nil {
				t.Fatalf("creating history directory: %v", err)
			}

			locked := make(chan struct{})
			release := make(chan struct{})
			holderDone := make(chan error, 1)
			go func() {
				holderDone <- withHistoryLock("claude", func() error {
					close(locked)
					<-release
					return nil
				})
			}()
			<-locked

			clearDone := make(chan error, 1)
			go func() { clearDone <- Clear(providerID) }()

			returnedWhileLocked := false
			select {
			case <-clearDone:
				returnedWhileLocked = true
			case <-time.After(50 * time.Millisecond):
			}
			close(release)
			if err := <-holderDone; err != nil {
				t.Fatalf("holding history lock: %v", err)
			}
			if !returnedWhileLocked {
				if err := <-clearDone; err != nil {
					t.Fatalf("Clear() error: %v", err)
				}
			}
			if returnedWhileLocked {
				t.Error("Clear() returned while an append lock was active")
			}
		})
	}
}

func TestClear(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())

	for _, providerID := range []string{"claude", "codex"} {
		if err := Append(providerID, testSnapshot(providerID, "daily", time.Now())); err != nil {
			t.Fatalf("appending %s history: %v", providerID, err)
		}
	}
	if err := Clear("claude"); err != nil {
		t.Fatalf("clearing provider history: %v", err)
	}
	if _, err := os.Stat(historyPath("claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cleared history still exists: %v", err)
	}
	if _, err := os.Stat(historyPath("codex")); err != nil {
		t.Errorf("other provider history missing: %v", err)
	}

	if err := Clear(""); err != nil {
		t.Fatalf("clearing all history: %v", err)
	}
	if _, err := os.Stat(config.HistoryDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("history directory still exists: %v", err)
	}
}
