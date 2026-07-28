package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
)

const (
	defaultGoMonthlyLimit = 60.0

	copilotMultiplierProvider  = "copilot"
	opencodeMultiplierProvider = "opencode"
)

// multiplierCatalog holds per-model cost multipliers by vibeusage provider ID.
type multiplierCatalog map[string]map[string]float64

// lazyLoader loads one catalog data set once, shares the in-flight request with
// concurrent callers, and leaves canceled loads retryable.
type lazyLoader[T any] struct {
	mu      sync.Mutex
	loaded  bool
	loading chan struct{}
	value   T
	loader  func(context.Context) (T, error)
}

func (l *lazyLoader[T]) ensure(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("loading %s: %w", name, err)
		}

		l.mu.Lock()
		if l.loaded {
			l.mu.Unlock()
			return nil
		}
		if done := l.loading; done != nil {
			l.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return fmt.Errorf("waiting for %s: %w", name, ctx.Err())
			}
		}

		done := make(chan struct{})
		l.loading = done
		loader := l.loader
		l.mu.Unlock()

		data, err := loader(ctx)
		if err == nil && ctx.Err() != nil {
			err = fmt.Errorf("loading %s: %w", name, ctx.Err())
		}

		l.mu.Lock()
		if err == nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("loading %s: %w", name, ctx.Err())
			} else {
				l.value = data
				l.loaded = true
			}
		}
		l.loading = nil
		close(done)
		l.mu.Unlock()
		return err
	}
}

func (l *lazyLoader[T]) setLoaderForTesting(loader func(context.Context) (T, error), replace bool) func(context.Context) (T, error) {
	for {
		l.mu.Lock()
		if done := l.loading; done != nil {
			l.mu.Unlock()
			<-done
			continue
		}
		old := l.loader
		if replace {
			l.loader = loader
		}
		var zero T
		l.loaded = false
		l.value = zero
		l.mu.Unlock()
		return old
	}
}

var (
	multipliersURL   = "https://raw.githubusercontent.com/github/docs/main/data/tables/copilot/model-multipliers.yml"
	goMultipliersURL = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/go.mdx"

	multipliers = lazyLoader[multiplierCatalog]{loader: loadMultipliers}

	goMonthlyLimitPattern = regexp.MustCompile(`(?m)(?:\*\*)?Monthly limit(?:\*\*)?\s+—\s+\$([0-9][0-9,.]*)\s+of usage`)
	goPricingTableHeader  = []string{"Model", "Input", "Output", "Cached Read", "Cached Write", "Usage"}
)

func ensureMultipliersLoaded(ctx context.Context) error {
	return multipliers.ensure(ctx, "model multipliers")
}

// LookupMultiplier returns the cost multiplier for a model from a provider.
// It returns nil when the provider has no multiplier data for the model.
func LookupMultiplier(providerID string, modelName string) *float64 {
	_ = ensureMultipliersLoaded(context.Background())

	providerMultipliers := multipliers.value[providerID]
	if multiplier, ok := providerMultipliers[modelName]; ok {
		return &multiplier
	}

	key := normalizeName(modelName)
	for name, multiplier := range providerMultipliers {
		if normalizeName(name) == key {
			return &multiplier
		}
	}

	return nil
}

// ResetMultipliersForTesting clears cached multiplier data.
// Only use in serial tests.
func ResetMultipliersForTesting() {
	multipliers.setLoaderForTesting(nil, false)
}

// SetMultipliersLoaderForTesting overrides the multiplier loader for tests.
// It returns a cleanup function that restores the original loader.
func SetMultipliersLoaderForTesting(loader func(context.Context) (multiplierCatalog, error)) func() {
	old := multipliers.setLoaderForTesting(loader, true)
	return func() {
		multipliers.setLoaderForTesting(old, true)
	}
}

func loadMultipliers(ctx context.Context) (multiplierCatalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("loading model multipliers: %w", err)
	}

	path := config.MultipliersFile()
	if catalog, ok := readMultiplierCacheIfFresh(path); ok {
		return catalog, nil
	}

	catalog, _ := readMultiplierCache(path)
	if catalog == nil {
		catalog = make(multiplierCatalog)
	}

	copilotRefreshed := false
	if raw, err := fetchMultipliersYAML(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("loading model multipliers: %w", ctx.Err())
		}
	} else if providerMultipliers := parseMultipliersYAML(raw); providerMultipliers == nil {
		delete(catalog, copilotMultiplierProvider)
	} else {
		catalog[copilotMultiplierProvider] = providerMultipliers
		copilotRefreshed = true
	}

	goRefreshed := false
	if raw, err := fetchGoMultipliersMarkdown(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("loading model multipliers: %w", ctx.Err())
		}
	} else if providerMultipliers := parseGoMultipliers(raw); providerMultipliers == nil {
		delete(catalog, opencodeMultiplierProvider)
	} else {
		catalog[opencodeMultiplierProvider] = providerMultipliers
		goRefreshed = true
	}

	// A single cache timestamp applies to both providers. Only refresh it after
	// both sources succeed so a transient failure retries on the next command.
	if copilotRefreshed && goRefreshed {
		_ = writeMultiplierCache(path, catalog)
	}
	return catalog, nil
}

func readMultiplierCacheIfFresh(path string) (multiplierCatalog, bool) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > cacheTTL {
		return nil, false
	}
	return readMultiplierCache(path)
}

func readMultiplierCache(path string) (multiplierCatalog, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var catalog multiplierCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil || catalog == nil {
		return nil, false
	}
	return catalog, true
}

func writeMultiplierCache(path string, catalog multiplierCatalog) error {
	if err := os.MkdirAll(config.CacheDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func fetchMultipliersYAML(ctx context.Context) (string, error) {
	return fetchCatalogText(ctx, multipliersURL, "Copilot multipliers")
}

func fetchGoMultipliersMarkdown(ctx context.Context) (string, error) {
	return fetchCatalogText(ctx, goMultipliersURL, "OpenCode Go multipliers")
}

func fetchCatalogText(ctx context.Context, url string, name string) (string, error) {
	client := httpclient.NewWithTimeout(15 * time.Second)
	resp, err := client.DoCtx(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", name, err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetching %s: HTTP %d", name, resp.StatusCode)
	}
	return string(resp.Body), nil
}

// yamlMultiplierEntry is the raw YAML shape from github/docs. Paid is an
// interface{} because the source mixes bare numbers with "Not applicable".
type yamlMultiplierEntry struct {
	Name string      `yaml:"name"`
	Paid interface{} `yaml:"multiplier_paid"`
}

// parseMultipliersYAML parses Copilot's paid-plan multipliers.
func parseMultipliersYAML(raw string) map[string]float64 {
	var rows []yamlMultiplierEntry
	if err := yaml.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}

	multipliers := make(map[string]float64, len(rows))
	for _, row := range rows {
		if multiplier := convertYAMLMultiplier(row.Paid); multiplier != nil {
			multipliers[row.Name] = *multiplier
		}
	}
	return multipliers
}

// convertYAMLMultiplier converts a yaml.v3 scalar value (int, float64, or
// string) to a *float64. It returns nil for "Not applicable" or unrecognised
// values.
func convertYAMLMultiplier(value interface{}) *float64 {
	switch value := value.(type) {
	case int:
		multiplier := float64(value)
		return &multiplier
	case float64:
		return &value
	case string:
		return parseMultiplierValue(value)
	default:
		return nil
	}
}

func parseMultiplierValue(value string) *float64 {
	if strings.EqualFold(value, "not applicable") || value == "" {
		return nil
	}
	multiplier, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}
	return &multiplier
}

// parseGoMultipliers extracts OpenCode Go's per-model monthly allowance from
// the published pricing table. A malformed table returns nil so routing falls
// back to unadjusted workspace headroom rather than applying wrong costs.
func parseGoMultipliers(raw string) map[string]float64 {
	monthlyLimit := parseGoMonthlyLimit(raw)
	multipliers := make(map[string]float64)
	inTable := false
	rows := 0

	for _, line := range strings.Split(raw, "\n") {
		cells, ok := markdownTableCells(line)
		if !ok {
			if inTable && rows > 0 {
				if strings.Contains(line, "|") {
					return nil
				}
				break
			}
			continue
		}

		if !inTable {
			if sameCells(cells, goPricingTableHeader) {
				inTable = true
			}
			continue
		}

		if markdownTableSeparator(cells) {
			continue
		}
		if len(cells) != len(goPricingTableHeader) || cells[0] == "" {
			return nil
		}

		usage, ok := parseGoDollar(cells[len(cells)-1])
		if !ok || usage <= 0 {
			return nil
		}
		name := stripTrailingParenthetical(cells[0])
		if name == "" {
			return nil
		}
		if usage < monthlyLimit {
			multipliers[name] = monthlyLimit / usage
		}
		rows++
	}

	if !inTable || rows == 0 {
		return nil
	}
	return multipliers
}

func parseGoMonthlyLimit(raw string) float64 {
	match := goMonthlyLimitPattern.FindStringSubmatch(raw)
	if len(match) != 2 {
		return defaultGoMonthlyLimit
	}
	limit, ok := parseGoDollar("$" + match[1])
	if !ok || limit <= 0 {
		return defaultGoMonthlyLimit
	}
	return limit
}

func markdownTableCells(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil, false
	}

	cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells, true
}

func sameCells(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func markdownTableSeparator(cells []string) bool {
	for _, cell := range cells {
		if cell == "" || strings.Trim(cell, "-: ") != "" {
			return false
		}
	}
	return true
}

func parseGoDollar(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "$") {
		return 0, false
	}
	amount, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(value, "$"), ",", ""), 64)
	if err != nil {
		return 0, false
	}
	return amount, true
}

func stripTrailingParenthetical(name string) string {
	if start := strings.LastIndex(name, " ("); start >= 0 && strings.HasSuffix(name, ")") {
		return strings.TrimSpace(name[:start])
	}
	return strings.TrimSpace(name)
}
