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

func TestPipelineRecorderSkipsSnapshotsWithoutUsagePeriods(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	recorder := PipelineRecorder{}
	periodless := models.UsageSnapshot{Provider: "opencode", FetchedAt: time.Now()}

	recorded, err := recorder.Record(periodless)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if recorded {
		t.Fatal("Record() = true for periodless snapshot")
	}
	if records, err := Read("opencode"); err != nil || len(records) != 0 {
		t.Errorf("periodless history = %v, %v; want empty", records, err)
	}

	recorded, err = recorder.Record(testSnapshot("opencode", "Monthly", time.Now()))
	if err != nil {
		t.Fatalf("Record() with usage error = %v", err)
	}
	if !recorded {
		t.Fatal("Record() = false for snapshot with usage period")
	}
}

func TestReadPurgesExistingPeriodlessRecords(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	writeRecords(t, "opencode", Record{V: CurrentRecordVersion, Snapshot: models.UsageSnapshot{
		Provider: "opencode", FetchedAt: time.Now(),
	}})

	records, err := Read("opencode")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %v, want empty", records)
	}
	data, err := os.ReadFile(historyPath("opencode"))
	if err != nil {
		t.Fatalf("reading compacted history: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("periodless history file = %s, want empty", data)
	}
}

func TestAppendStoresOnlyHistoryFields(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	balance := 12.5
	autoReload := true
	snap := testSnapshot("claude", "daily", time.Now())
	snap.Identity = &models.ProviderIdentity{Email: "private@example.com", Organization: "Secret Org", Plan: "Enterprise"}
	snap.Billing = &models.BillingDetail{Balance: &balance, AutoReload: &autoReload}
	snap.Source = "oauth"

	if err := Append("claude", snap); err != nil {
		t.Fatalf("appending history: %v", err)
	}
	data, err := os.ReadFile(historyPath("claude"))
	if err != nil {
		t.Fatalf("reading history file: %v", err)
	}
	for _, secret := range []string{"private@example.com", "Secret Org", "Enterprise", "oauth", "identity", "billing"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("history contains private snapshot field %q: %s", secret, data)
		}
	}
	if !strings.Contains(string(data), `"utilization":42`) {
		t.Errorf("history omitted utilization: %s", data)
	}
}

func TestHistoryRejectsInvalidProviderAndSnapshotIdentity(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	at := time.Now()
	for _, providerID := range []string{"", "../escape", `..\\escape`, "Claude", "z.ai", "/absolute"} {
		snap := testSnapshot(providerID, "daily", at)
		if err := Append(providerID, snap); err == nil {
			t.Errorf("Append(%q) succeeded, want error", providerID)
		}
	}
	if err := Append("claude", testSnapshot("codex", "daily", at)); err == nil || !strings.Contains(err.Error(), "snapshot provider") {
		t.Fatalf("mismatched snapshot error = %v", err)
	}
	if _, err := Read("../escape"); err == nil {
		t.Fatal("Read() accepted path traversal provider")
	}
	if err := Clear("../escape"); err == nil {
		t.Fatal("Clear() accepted path traversal provider")
	}
}

func TestAppendRejectsInvalidTimestamps(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	tests := []struct {
		name string
		at   time.Time
	}{
		{name: "missing"},
		{name: "future", at: time.Now().Add(DedupFloor + time.Minute)},
		{name: "expired", at: time.Now().AddDate(0, 0, -MaxAgeDays-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Append("claude", testSnapshot("claude", "daily", tt.at)); err == nil {
				t.Fatal("Append() succeeded, want timestamp error")
			}
		})
	}
}

func TestReadSortsRecordsByFetchedAt(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	first := time.Now().Add(-3 * time.Hour)
	second := first.Add(time.Hour)
	third := second.Add(time.Hour)
	writeRecords(t, "claude",
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "third", third)},
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "first", first)},
		Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "second", second)},
	)

	records, err := Read("claude")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	for i, want := range []string{"first", "second", "third"} {
		if got := records[i].Snapshot.Periods[0].Name; got != want {
			t.Errorf("record %d = %q, want %q", i, got, want)
		}
	}
}

func TestReadRefusesUnsupportedRecordVersion(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	writeRecords(t, "claude", Record{V: CurrentRecordVersion + 1, Snapshot: testSnapshot("claude", "future", time.Now())})

	if _, err := Read("claude"); err == nil || !strings.Contains(err.Error(), "unsupported record version") {
		t.Fatalf("Read() error = %v", err)
	}
	if err := Append("claude", testSnapshot("claude", "current", time.Now())); err == nil || !strings.Contains(err.Error(), "unsupported record version") {
		t.Fatalf("Append() error = %v", err)
	}
}

func TestReadMigratesLegacyRecordsWithoutPrivateFields(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	legacy := testSnapshot("claude", "legacy", time.Now().Add(-2*DedupFloor))
	legacy.Identity = &models.ProviderIdentity{Email: "private@example.com"}
	writeRecords(t, "claude", Record{V: legacyRecordVersion, Snapshot: legacy})

	records, err := Read("claude")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	data, err := os.ReadFile(historyPath("claude"))
	if err != nil {
		t.Fatalf("reading migrated history: %v", err)
	}
	if strings.Contains(string(data), "private@example.com") || strings.Contains(string(data), `"identity"`) {
		t.Errorf("migrated history retained private fields: %s", data)
	}
	for lineNumber, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decoding migrated line %d: %v", lineNumber+1, err)
		}
		if record.V != CurrentRecordVersion {
			t.Errorf("migrated line %d version = %d, want %d", lineNumber+1, record.V, CurrentRecordVersion)
		}
	}
}

func TestMigrateSanitizesEveryLegacyFileAndDropsInvalidRows(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	claude := testSnapshot("claude", "legacy", time.Now().Add(-time.Hour))
	claude.Identity = &models.ProviderIdentity{Email: "claude-private@example.com"}
	codex := testSnapshot("wrong-provider", "invalid", time.Now().Add(-time.Hour))
	codex.Identity = &models.ProviderIdentity{Email: "codex-private@example.com"}
	writeRecords(t, "claude", Record{V: legacyRecordVersion, Snapshot: claude})
	writeRecords(t, "codex", Record{V: legacyRecordVersion, Snapshot: codex})

	if err := Migrate(); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	for _, providerID := range []string{"claude", "codex"} {
		data, err := os.ReadFile(historyPath(providerID))
		if err != nil {
			t.Fatalf("reading %s history: %v", providerID, err)
		}
		if strings.Contains(string(data), "private@example.com") || strings.Contains(string(data), `"identity"`) {
			t.Errorf("%s history retained private fields: %s", providerID, data)
		}
	}

	records, err := Read("claude")
	if err != nil || len(records) != 1 || records[0].V != CurrentRecordVersion {
		t.Errorf("migrated Claude records = %v, %v", records, err)
	}
	records, err = Read("codex")
	if err != nil || len(records) != 0 {
		t.Errorf("migrated Codex records = %v, %v; want invalid row dropped", records, err)
	}
}

func TestReadRemovesFutureDatedLegacyPrivateRecord(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	legacy := testSnapshot("claude", "future", time.Now().Add(24*time.Hour))
	legacy.Identity = &models.ProviderIdentity{Email: "private@example.com"}
	writeRecords(t, "claude", Record{V: legacyRecordVersion, Snapshot: legacy})

	records, err := Read("claude")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %v, want empty", records)
	}
	data, err := os.ReadFile(historyPath("claude"))
	if err != nil {
		t.Fatalf("reading migrated history: %v", err)
	}
	if strings.Contains(string(data), "private@example.com") {
		t.Errorf("future legacy history retained private fields: %s", data)
	}
}

func TestAppendRepairsTornFinalLine(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	first := time.Now().Add(-2 * DedupFloor)
	writeRecords(t, "claude", Record{V: CurrentRecordVersion, Snapshot: testSnapshot("claude", "first", first)})
	file, err := os.OpenFile(historyPath("claude"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening history tail: %v", err)
	}
	if _, err := file.WriteString(`{"v":1`); err != nil {
		_ = file.Close()
		t.Fatalf("writing torn tail: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing torn history: %v", err)
	}

	if err := Append("claude", testSnapshot("claude", "second", time.Now())); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	records, err := Read("claude")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got, want := len(records), 2; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	if got := records[1].Snapshot.Periods[0].Name; got != "second" {
		t.Errorf("last record = %q, want second", got)
	}
}

func TestFutureStoredTimestampIsHiddenAndDoesNotBlockCurrentAppend(t *testing.T) {
	t.Setenv("VIBEUSAGE_DATA_DIR", t.TempDir())
	writeRecords(t, "claude", Record{
		V:        CurrentRecordVersion,
		Snapshot: testSnapshot("claude", "future", time.Now().Add(24*time.Hour)),
	})
	if err := Append("claude", testSnapshot("claude", "current", time.Now())); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	records, err := Read("claude")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got, want := len(records), 1; got != want {
		t.Fatalf("record count = %d, want %d", got, want)
	}
	if got := records[0].Snapshot.Periods[0].Name; got != "current" {
		t.Errorf("visible record = %q, want current", got)
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
