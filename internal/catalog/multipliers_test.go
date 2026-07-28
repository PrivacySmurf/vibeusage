package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
)

func TestParseMultipliersYAML(t *testing.T) {
	yaml := `# Comment
- name: Claude Sonnet 4.6
  multiplier_paid: 1
  multiplier_free: Not applicable

- name: GPT-4o
  multiplier_paid: 0
  multiplier_free: 1

- name: Claude Opus 4.6
  multiplier_paid: 3
  multiplier_free: Not applicable

- name: Goldeneye
  multiplier_paid: Not applicable
  multiplier_free: 1
`
	multipliers := parseMultipliersYAML(yaml)

	if len(multipliers) != 3 {
		t.Fatalf("expected 3 multipliers, got %d", len(multipliers))
	}
	if got := multipliers["Claude Sonnet 4.6"]; got != 1 {
		t.Errorf("Claude Sonnet 4.6 multiplier = %v, want 1", got)
	}
	if got := multipliers["GPT-4o"]; got != 0 {
		t.Errorf("GPT-4o multiplier = %v, want 0", got)
	}
	if got := multipliers["Claude Opus 4.6"]; got != 3 {
		t.Errorf("Claude Opus 4.6 multiplier = %v, want 3", got)
	}
	if _, ok := multipliers["Goldeneye"]; ok {
		t.Error("Goldeneye has no paid multiplier and should be absent")
	}
}

const goMultipliersFixture = `## Usage limits

- **Monthly limit** — $60 of usage

| Model                        | Input  | Output | Cached Read | Cached Write | Usage |
| ---------------------------- | ------ | ------ | ----------- | ------------ | ----- |
| Grok 4.5                     | $2.00  | $6.00  | $0.30       | -            | $15   |
| GLM-5.2                      | $1.40  | $4.40  | $0.26       | -            | $60   |
| Qwen3.7 Plus (≤ 256K tokens) | $0.40  | $1.60  | $0.04       | $0.50        | $15   |
`

func TestLookupMultiplierUsesProvider(t *testing.T) {
	goMultipliers := parseGoMultipliers(goMultipliersFixture)
	if goMultipliers == nil {
		t.Fatal("parseGoMultipliers() = nil")
	}
	if got := goMultipliers["Grok 4.5"]; got != 4 {
		t.Errorf("Grok 4.5 multiplier = %v, want 4", got)
	}
	if _, ok := goMultipliers["GLM-5.2"]; ok {
		t.Error("GLM-5.2 has the standard allowance and should not have a multiplier")
	}
	if got := goMultipliers["Qwen3.7 Plus"]; got != 4 {
		t.Errorf("parenthetical model multiplier = %v, want 4", got)
	}

	cleanup := SetMultipliersLoaderForTesting(func(context.Context) (multiplierCatalog, error) {
		return multiplierCatalog{
			copilotMultiplierProvider:  {"GPT-5": 2},
			opencodeMultiplierProvider: goMultipliers,
		}, nil
	})
	t.Cleanup(cleanup)

	if got := LookupMultiplier(copilotMultiplierProvider, "GPT-5"); got == nil || *got != 2 {
		t.Errorf("LookupMultiplier(copilot, GPT-5) = %v, want 2", got)
	}
	if got := LookupMultiplier(opencodeMultiplierProvider, "grok-4.5"); got == nil || *got != 4 {
		t.Errorf("LookupMultiplier(opencode, grok-4.5) = %v, want 4", got)
	}
	if got := LookupMultiplier(opencodeMultiplierProvider, "Qwen3.7 Plus"); got == nil || *got != 4 {
		t.Errorf("LookupMultiplier(opencode, Qwen3.7 Plus) = %v, want 4", got)
	}
	if got := LookupMultiplier("claude", "Grok 4.5"); got != nil {
		t.Errorf("LookupMultiplier(claude, Grok 4.5) = %v, want nil", got)
	}
}

func TestLookupMultiplierMatchesTrailingParenthetical(t *testing.T) {
	cleanup := SetMultipliersLoaderForTesting(func(context.Context) (multiplierCatalog, error) {
		return multiplierCatalog{
			opencodeMultiplierProvider: {"Kimi K3": 4},
		}, nil
	})
	t.Cleanup(cleanup)

	got := LookupMultiplier(opencodeMultiplierProvider, "Kimi K3 (2x usage)")
	if got == nil || *got != 4 {
		t.Errorf("LookupMultiplier() = %v, want 4", got)
	}
}

func TestLookupMultiplierPrefersDecoratedName(t *testing.T) {
	cleanup := SetMultipliersLoaderForTesting(func(context.Context) (multiplierCatalog, error) {
		return multiplierCatalog{
			opencodeMultiplierProvider: {
				"Kimi K3":            4,
				"Kimi-K3 (2x usage)": 2,
			},
		}, nil
	})
	t.Cleanup(cleanup)

	for range 100 {
		got := LookupMultiplier(opencodeMultiplierProvider, "Kimi K3 (2x usage)")
		if got == nil || *got != 2 {
			t.Fatalf("LookupMultiplier() = %v, want decorated-name multiplier 2", got)
		}
	}
}

func TestLookupMultiplierDoesNotStripParentheticalForOtherProviders(t *testing.T) {
	cleanup := SetMultipliersLoaderForTesting(func(context.Context) (multiplierCatalog, error) {
		return multiplierCatalog{
			copilotMultiplierProvider: {"Model": 4},
		}, nil
	})
	t.Cleanup(cleanup)

	if got := LookupMultiplier(copilotMultiplierProvider, "Model (variant)"); got != nil {
		t.Errorf("LookupMultiplier() = %v, want nil", got)
	}
}

func TestLookupMultiplierEmptyData(t *testing.T) {
	cleanup := SetMultipliersLoaderForTesting(func(context.Context) (multiplierCatalog, error) {
		return nil, nil
	})
	t.Cleanup(cleanup)

	if got := LookupMultiplier(opencodeMultiplierProvider, "Grok 4.5"); got != nil {
		t.Errorf("LookupMultiplier(opencode, Grok 4.5) = %v, want nil", got)
	}
}

func TestParseGoMultipliersRebasesMonthlyLimit(t *testing.T) {
	fixture := `- **Monthly limit** — $90 of usage

| Model    | Input | Output | Cached Read | Cached Write | Usage |
| -------- | ----- | ------ | ----------- | ------------ | ----- |
| Grok 4.5 | $2.00 | $6.00  | $0.30       | -            | $15   |
`
	multipliers := parseGoMultipliers(fixture)
	if multipliers == nil {
		t.Fatal("parseGoMultipliers() = nil")
	}
	if got := multipliers["Grok 4.5"]; got != 6 {
		t.Errorf("Grok 4.5 multiplier = %v, want 6", got)
	}
}

func TestParseGoMultipliersRejectsPartialTable(t *testing.T) {
	fixture := strings.TrimSpace(goMultipliersFixture) + "\n| Kimi K3 | $3.00 | $15.00 | $0.30 | - | $15\n"
	if got := parseGoMultipliers(fixture); got != nil {
		t.Errorf("parseGoMultipliers() = %v, want nil for malformed table", got)
	}
}

func TestMultiplierCacheBundlesProviders(t *testing.T) {
	t.Setenv("VIBEUSAGE_CACHE_DIR", t.TempDir())
	want := multiplierCatalog{
		copilotMultiplierProvider:  {"GPT-5": 2},
		opencodeMultiplierProvider: {"Grok 4.5": 4},
	}
	if err := writeMultiplierCache(config.MultipliersFile(), want); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(config.MultipliersFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"copilot"`) || !strings.Contains(string(raw), `"opencode"`) {
		t.Errorf("cache does not contain both providers: %s", raw)
	}

	got, ok := readMultiplierCache(config.MultipliersFile())
	if !ok {
		t.Fatal("readMultiplierCache() = false")
	}
	if got[copilotMultiplierProvider]["GPT-5"] != 2 || got[opencodeMultiplierProvider]["Grok 4.5"] != 4 {
		t.Errorf("cached multipliers = %#v, want %#v", got, want)
	}
}

func TestLoadMultipliersDoesNotCachePartialRefresh(t *testing.T) {
	server := partialMultiplierServer(t)
	defer server.Close()
	setMultiplierURLs(t, server.URL+"/copilot", server.URL+"/opencode")
	t.Setenv("VIBEUSAGE_CACHE_DIR", t.TempDir())

	catalog, err := loadMultipliers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog[copilotMultiplierProvider]["GPT-5"]; got != 2 {
		t.Errorf("Copilot multiplier = %v, want 2", got)
	}
	if _, err := os.Stat(config.MultipliersFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial refresh created a fresh cache: %v", err)
	}
}

func TestLoadMultipliersKeepsStaleCacheAfterPartialRefresh(t *testing.T) {
	server := partialMultiplierServer(t)
	defer server.Close()
	setMultiplierURLs(t, server.URL+"/copilot", server.URL+"/opencode")
	t.Setenv("VIBEUSAGE_CACHE_DIR", t.TempDir())

	staleCatalog := multiplierCatalog{
		copilotMultiplierProvider:  {"GPT-5": 1},
		opencodeMultiplierProvider: {"Grok 4.5": 4},
	}
	if err := writeMultiplierCache(config.MultipliersFile(), staleCatalog); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(config.MultipliersFile(), stale, stale); err != nil {
		t.Fatal(err)
	}

	catalog, err := loadMultipliers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog[copilotMultiplierProvider]["GPT-5"]; got != 2 {
		t.Errorf("Copilot multiplier = %v, want refreshed value 2", got)
	}
	if got := catalog[opencodeMultiplierProvider]["Grok 4.5"]; got != 4 {
		t.Errorf("OpenCode multiplier = %v, want stale value 4", got)
	}
	info, err := os.Stat(config.MultipliersFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(time.Now().Add(-24 * time.Hour)) {
		t.Errorf("partial refresh made the cache fresh: %s", info.ModTime())
	}
}

func partialMultiplierServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot":
			_, _ = w.Write([]byte("- name: GPT-5\n  multiplier_paid: 2\n"))
		case "/opencode":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
}

func setMultiplierURLs(t *testing.T, copilotURL string, opencodeURL string) {
	t.Helper()
	oldCopilotURL := multipliersURL
	oldOpenCodeURL := goMultipliersURL
	multipliersURL = copilotURL
	goMultipliersURL = opencodeURL
	t.Cleanup(func() {
		multipliersURL = oldCopilotURL
		goMultipliersURL = oldOpenCodeURL
	})
}
