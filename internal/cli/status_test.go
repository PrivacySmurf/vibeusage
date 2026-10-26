package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
	"github.com/joshuadavidthomas/vibeusage/internal/provider/claude"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestDisplayStatusTable_ContainsProviderData(t *testing.T) {
	now := time.Now()
	statuses := map[string]models.ProviderStatus{
		"claude": {
			Level:     models.StatusOperational,
			UpdatedAt: &now,
		},
		"cursor": {
			Level:       models.StatusDegraded,
			Description: "Slow responses",
			UpdatedAt:   &now,
		},
	}

	var buf bytes.Buffer
	outWriter = &buf
	defer func() { outWriter = os.Stdout }()

	oldNoColor := noColor
	noColor = true
	defer func() { noColor = oldNoColor }()

	displayStatusTable(context.Background(), statuses, 100)

	output := buf.String()

	for _, want := range []string{"claude", "cursor", "All systems operational", "Slow responses"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q\n\nGot:\n%s", want, output)
		}
	}
}

func TestDisplayStatusTable_HasTableBorders(t *testing.T) {
	statuses := map[string]models.ProviderStatus{
		"claude": {
			Level: models.StatusOperational,
		},
	}

	var buf bytes.Buffer
	outWriter = &buf
	defer func() { outWriter = os.Stdout }()

	oldNoColor := noColor
	noColor = false
	defer func() { noColor = oldNoColor }()

	oldQuiet := quiet
	quiet = false
	defer func() { quiet = oldQuiet }()

	displayStatusTable(context.Background(), statuses, 0)

	output := buf.String()

	// Should use lipgloss table borders (rounded)
	if !strings.Contains(output, "╭") {
		t.Errorf("expected lipgloss rounded border, got:\n%s", output)
	}
}

func TestDisplayStatusTable_QuietMode(t *testing.T) {
	now := time.Now()
	statuses := map[string]models.ProviderStatus{
		"claude": {
			Level:     models.StatusOperational,
			UpdatedAt: &now,
		},
	}

	var buf bytes.Buffer
	outWriter = &buf
	defer func() { outWriter = os.Stdout }()

	oldQuiet := quiet
	quiet = true
	defer func() { quiet = oldQuiet }()

	displayStatusTable(context.Background(), statuses, 0)

	output := buf.String()

	// Quiet mode should not use table borders
	if strings.Contains(output, "╭") {
		t.Error("quiet mode should not use table borders")
	}
	if !strings.Contains(output, "claude") {
		t.Error("quiet mode should still show provider names")
	}
}

func TestDisplayStatusTable_VerboseShowsDuration(t *testing.T) {
	statuses := map[string]models.ProviderStatus{
		"claude": {Level: models.StatusOperational},
	}

	// Capture logger output via context injection
	ctx, logBuf := logging.NewTestContext(logging.Flags{Verbose: true})

	var buf bytes.Buffer
	outWriter = &buf
	defer func() { outWriter = os.Stdout }()

	oldQuiet := quiet
	quiet = false
	defer func() { quiet = oldQuiet }()

	displayStatusTable(ctx, statuses, 250)

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "250") {
		t.Errorf("verbose mode should log duration, got:\n%s", logOutput)
	}
}

func TestDisplayStatusTable_Headers(t *testing.T) {
	statuses := map[string]models.ProviderStatus{
		"claude": {Level: models.StatusOperational},
	}

	var buf bytes.Buffer
	outWriter = &buf
	defer func() { outWriter = os.Stdout }()

	oldNoColor := noColor
	noColor = true
	defer func() { noColor = oldNoColor }()

	oldQuiet := quiet
	quiet = false
	defer func() { quiet = oldQuiet }()

	displayStatusTable(context.Background(), statuses, 0)

	output := buf.String()
	for _, header := range []string{"Provider", "Status", "Description", "Updated"} {
		if !strings.Contains(output, header) {
			t.Errorf("output missing header %q\n\nGot:\n%s", header, output)
		}
	}
}

func TestFetchAllStatuses_ThreadsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	providers := map[string]provider.Provider{
		"slow": &statusStubProvider{
			id: "slow",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				// If context is threaded, this select should hit the cancelled case
				select {
				case <-ctx.Done():
					return models.ProviderStatus{Level: models.StatusUnknown, Description: "cancelled"}
				case <-time.After(5 * time.Second):
					return models.ProviderStatus{Level: models.StatusOperational}
				}
			},
		},
	}

	statuses := fetchAllStatuses(ctx, providers, 5, nil)

	if statuses["slow"].Level != models.StatusUnknown {
		t.Errorf("expected StatusUnknown from cancelled context, got %v", statuses["slow"].Level)
	}
	if statuses["slow"].Description != "cancelled" {
		t.Errorf("expected 'cancelled' description, got %q", statuses["slow"].Description)
	}
}

func TestFetchAllStatuses_BoundsConcurrency(t *testing.T) {
	const maxConcurrent = 2
	const numProviders = 6

	var concurrent atomic.Int32
	var maxSeen atomic.Int32

	providers := make(map[string]provider.Provider, numProviders)
	for i := range numProviders {
		id := string(rune('a' + i))
		providers[id] = &statusStubProvider{
			id: id,
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				cur := concurrent.Add(1)
				// Track max concurrent
				for {
					old := maxSeen.Load()
					if cur <= old || maxSeen.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
				concurrent.Add(-1)
				return models.ProviderStatus{Level: models.StatusOperational}
			},
		}
	}

	statuses := fetchAllStatuses(context.Background(), providers, maxConcurrent, nil)

	if len(statuses) != numProviders {
		t.Errorf("expected %d results, got %d", numProviders, len(statuses))
	}

	if peak := maxSeen.Load(); peak > int32(maxConcurrent) {
		t.Errorf("concurrency exceeded bound: peak=%d, max=%d", peak, maxConcurrent)
	}
}

func TestFetchAllStatuses_CollectsAllResults(t *testing.T) {
	providers := map[string]provider.Provider{
		"ok": &statusStubProvider{
			id: "ok",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				return models.ProviderStatus{Level: models.StatusOperational}
			},
		},
		"down": &statusStubProvider{
			id: "down",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				return models.ProviderStatus{Level: models.StatusMajorOutage, Description: "Everything broken"}
			},
		},
	}

	statuses := fetchAllStatuses(context.Background(), providers, 5, nil)

	if len(statuses) != 2 {
		t.Fatalf("expected 2 results, got %d", len(statuses))
	}
	if statuses["ok"].Level != models.StatusOperational {
		t.Errorf("expected StatusOperational for 'ok', got %v", statuses["ok"].Level)
	}
	if statuses["down"].Level != models.StatusMajorOutage {
		t.Errorf("expected StatusMajorOutage for 'down', got %v", statuses["down"].Level)
	}
}

func TestFetchAllStatuses_CallbackInvoked(t *testing.T) {
	var called []string
	var mu sync.Mutex

	providers := map[string]provider.Provider{
		"a": &statusStubProvider{
			id: "a",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				return models.ProviderStatus{Level: models.StatusOperational}
			},
		},
		"b": &statusStubProvider{
			id: "b",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				return models.ProviderStatus{Level: models.StatusDegraded}
			},
		},
	}

	callback := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		called = append(called, id)
	}

	fetchAllStatuses(context.Background(), providers, 5, callback)

	mu.Lock()
	defer mu.Unlock()

	if len(called) != 2 {
		t.Errorf("expected callback invoked 2 times, got %d", len(called))
	}

	// Sort to make comparison deterministic
	sort.Strings(called)
	if called[0] != "a" || called[1] != "b" {
		t.Errorf("expected callbacks for 'a' and 'b', got %v", called)
	}
}

func TestFetchAllStatuses_CallbackNil(t *testing.T) {
	// Ensure no panic when callback is nil
	providers := map[string]provider.Provider{
		"test": &statusStubProvider{
			id: "test",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				return models.ProviderStatus{Level: models.StatusOperational}
			},
		},
	}

	// Should not panic
	statuses := fetchAllStatuses(context.Background(), providers, 5, nil)

	if len(statuses) != 1 {
		t.Errorf("expected 1 result, got %d", len(statuses))
	}
}

// statusStubProvider implements provider.Provider with a configurable FetchStatus.
type statusStubProvider struct {
	id          string
	fetchStatus func(ctx context.Context) models.ProviderStatus
}

func (s *statusStubProvider) Meta() provider.Metadata {
	return provider.Metadata{ID: s.id, Name: s.id}
}

func (s *statusStubProvider) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{}
}

func (s *statusStubProvider) FetchStrategies() []fetch.Strategy {
	return nil
}

func (s *statusStubProvider) FetchStatus(ctx context.Context) models.ProviderStatus {
	if s.fetchStatus != nil {
		return s.fetchStatus(ctx)
	}
	return models.ProviderStatus{}
}

type statusRedirectRoundTripper struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt statusRedirectRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	u := *cloned.URL
	u.Scheme = rt.target.Scheme
	u.Host = rt.target.Host
	cloned.URL = &u
	cloned.Host = rt.target.Host
	return rt.base.RoundTrip(cloned)
}

func TestR1_3_FetchStatus_PropagatesProviderToRawDump(t *testing.T) {
	testenv.ApplyVibeusage(t, t.TempDir())
	t.Setenv("VIBEUSAGE_DEBUG_RAW", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":{"indicator":"none","description":"All Systems Operational"}}`))
	}))
	defer srv.Close()

	// 1. CLI status dispatch propagates provider ID on context.
	statuses := fetchAllStatuses(context.Background(), map[string]provider.Provider{
		"claude": &statusStubProvider{
			id: "claude",
			fetchStatus: func(ctx context.Context) models.ProviderStatus {
				if got := fetch.ProviderFromContext(ctx); got != "claude" {
					t.Errorf("ProviderFromContext(ctx) = %q, want %q", got, "claude")
				}
				return provider.FetchStatuspageStatus(ctx, srv.URL)
			},
		},
	}, 1, nil)
	if statuses["claude"].Level != models.StatusOperational {
		t.Fatalf("statuses[claude].Level = %v, want %v", statuses["claude"].Level, models.StatusOperational)
	}

	// 2. Direct provider FetchStatus without explicit context still resolves "claude"
	// via call-stack fallback instead of shared "status" from internal/provider/status.go.
	targetURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	origTransport := http.DefaultTransport
	http.DefaultTransport = statusRedirectRoundTripper{target: targetURL, base: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	directStatus := (claude.Claude{}).FetchStatus(context.Background())
	if directStatus.Level != models.StatusOperational {
		t.Fatalf("direct Claude.FetchStatus() = %v, want %v", directStatus.Level, models.StatusOperational)
	}

	entries, err := os.ReadDir(config.RawDumpsDir())
	if err != nil {
		t.Fatalf("ReadDir(RawDumpsDir): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("raw dump file count = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "-status-") {
			t.Errorf("dump filename %q erroneously used shared helper name 'status' as provider", entry.Name())
		}
		if !strings.Contains(entry.Name(), "-claude-") {
			t.Errorf("dump filename %q missing '-claude-' provider token", entry.Name())
		}
		dumpBytes, readErr := os.ReadFile(filepath.Join(config.RawDumpsDir(), entry.Name()))
		if readErr != nil {
			t.Fatalf("ReadFile(%s): %v", entry.Name(), readErr)
		}
		if !strings.Contains(string(dumpBytes), "Provider: claude\n") {
			t.Errorf("dump %s missing 'Provider: claude':\n%s", entry.Name(), string(dumpBytes))
		}
	}
}
