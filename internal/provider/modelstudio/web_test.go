package modelstudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestResolveSECToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/tool/user/info.json" {
			t.Errorf("path = %s, want /tool/user/info.json", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "login_aliyunid_ticket=test-ticket" {
			t.Errorf("Cookie = %q", got)
		}
		if got := r.Header.Get("Referer"); got != serverURL(r)+"/" {
			t.Errorf("Referer = %q, want %q", got, serverURL(r)+"/")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"code":"200",
			"data":{"secToken":"resolved-token"},
			"httpStatusCode":"200",
			"successResponse":true
		}`)
	}))
	t.Cleanup(server.Close)

	strategy := &WebConsoleStrategy{}
	token, err := strategy.resolveSECToken(
		context.Background(),
		httpclient.New(),
		server.URL,
		"login_aliyunid_ticket=test-ticket",
	)
	if err != nil {
		t.Fatalf("resolveSECToken() error: %v", err)
	}
	if token != "resolved-token" {
		t.Errorf("token = %q, want resolved-token", token)
	}
}

func TestCallSubscriptionSummary(t *testing.T) {
	t.Parallel()

	const (
		cookie        = "login_aliyunid_ticket=test-ticket; login_aliyunid_csrf=test-csrf"
		secToken      = "resolved-token"
		region        = "ap-southeast-1"
		productCode   = "sfm_tokenplanteams_dp_intl"
		dashboardPath = "/ap-southeast-1/?tab=plan#/efm/subscription/token-plan/enterprise"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/data/api.json" {
			t.Errorf("path = %s, want /data/api.json", r.URL.Path)
		}
		if got := r.URL.Query().Get("action"); got != subscriptionSummaryAction {
			t.Errorf("query action = %q", got)
		}
		if got := r.URL.Query().Get("product"); got != bssProduct {
			t.Errorf("query product = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error: %v", err)
		}
		checks := map[string]string{
			"action":    subscriptionSummaryAction,
			"product":   bssProduct,
			"params":    `{"ProductCode":"` + productCode + `"}`,
			"region":    region,
			"sec_token": secToken,
		}
		for key, want := range checks {
			if got := r.Form.Get(key); got != want {
				t.Errorf("form %s = %q, want %q", key, got, want)
			}
		}
		if got := r.Header.Get("Cookie"); got != cookie {
			t.Errorf("Cookie = %q", got)
		}
		if got := r.Header.Get("Origin"); got != serverURL(r) {
			t.Errorf("Origin = %q, want %q", got, serverURL(r))
		}
		if got := r.Header.Get("Referer"); got != serverURL(r)+dashboardPath {
			t.Errorf("Referer = %q, want %q", got, serverURL(r)+dashboardPath)
		}
		if got := r.Header.Get("x-csrf-token"); got != "test-csrf" {
			t.Errorf("x-csrf-token = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"code":"200",
			"data":{
				"Code":"200",
				"Data":"{\"TotalCount\":1,\"TotalValue\":100,\"TotalSurplusValue\":75}",
				"Success":true
			},
			"httpStatusCode":"200",
			"successResponse":true
		}`)
	}))
	t.Cleanup(server.Close)

	strategy := &WebConsoleStrategy{}
	body, err := strategy.callSubscriptionSummary(
		context.Background(),
		httpclient.New(),
		server.URL,
		server.URL+dashboardPath,
		region,
		productCode,
		cookie,
		secToken,
	)
	if err != nil {
		t.Fatalf("callSubscriptionSummary() error: %v", err)
	}
	if _, err := parseModelStudioConsoleResponses(body, nil, nil, nil, "test"); err != nil {
		t.Fatalf("parse response: %v", err)
	}
}

func TestWebConsoleFetchRetriesExpiredManualSessionWithBrowserCookies(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	storeModelStudioTestSession(t, sessionCredentials{
		Cookie: "login_aliyunid_ticket=stale-ticket; login_current_pk=account",
		Source: sessionSourceManual,
	})

	server := newModelStudioSessionServer(t, func(cookie string) string {
		if strings.Contains(cookie, "fresh-ticket") {
			return successfulSummaryPayload()
		}
		return expiredSessionPayload()
	})

	oldImporter := importModelStudioBrowserSession
	t.Cleanup(func() { importModelStudioBrowserSession = oldImporter })
	var imports atomic.Int32
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		imports.Add(1)
		return browserSession{
			Cookie:      "login_aliyunid_ticket=fresh-ticket; login_current_pk=account",
			SourceLabel: "Chrome Default",
		}, nil
	}

	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL + "/ap-southeast-1/?tab=plan",
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}
	result, err := strategy.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if !result.Success {
		t.Fatalf("Fetch() result: %+v", result)
	}
	if imports.Load() != 1 {
		t.Errorf("browser imports = %d, want 1", imports.Load())
	}

	data, err := config.ReadCredential("modelstudio", "session")
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}
	var cached sessionCredentials
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("decode cached credential: %v", err)
	}
	if cached.Source != sessionSourceBrowser || cached.BrowserLabel != "Chrome Default" {
		t.Errorf("cached source = %+v, want browser/Chrome Default", cached)
	}
}

func TestWebConsoleFetchDoesNotOverrideExpiredEnvironmentCookie(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv("MODELSTUDIO_COOKIE", "login_aliyunid_ticket=stale-ticket; login_current_pk=account")
	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })

	oldImporter := importModelStudioBrowserSession
	t.Cleanup(func() { importModelStudioBrowserSession = oldImporter })
	var imports atomic.Int32
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		imports.Add(1)
		return browserSession{}, fmt.Errorf("must not run")
	}

	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}
	result, err := strategy.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if result.Success || !strings.Contains(result.Error, "Update MODELSTUDIO_COOKIE") {
		t.Fatalf("Fetch() result: %+v", result)
	}
	if imports.Load() != 0 {
		t.Errorf("browser imports = %d, want 0", imports.Load())
	}
}

func TestWebConsoleFetchDoesNotRetryWorkspacePermissionError(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	storeModelStudioTestSession(t, sessionCredentials{
		Cookie: "login_aliyunid_ticket=valid-ticket; login_current_pk=account",
		Source: sessionSourceManual,
	})
	server := newModelStudioSessionServer(t, func(string) string {
		return `{
			"code":"200",
			"data":{"Code":"BailianGateway.Workspace.NotAuthorised","Message":"Workspace access denied","Success":false},
			"httpStatusCode":"200",
			"successResponse":false
		}`
	})

	oldImporter := importModelStudioBrowserSession
	t.Cleanup(func() { importModelStudioBrowserSession = oldImporter })
	var imports atomic.Int32
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		imports.Add(1)
		return browserSession{}, nil
	}

	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}
	result, err := strategy.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if result.Success || !strings.Contains(result.Error, "Workspace.NotAuthorised") {
		t.Fatalf("Fetch() result: %+v", result)
	}
	if imports.Load() != 0 {
		t.Errorf("browser imports = %d, want 0", imports.Load())
	}
}

func TestWebConsoleFetchThrottlesOnBrowserImportFailure(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(string) string { return successfulSummaryPayload() })

	oldImporter := importModelStudioBrowserSession
	t.Cleanup(func() { importModelStudioBrowserSession = oldImporter })
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		return browserSession{}, fmt.Errorf("no browser session found")
	}

	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}
	result, err := strategy.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected failure, got success: %+v", result)
	}
	if result.RetryAfter == nil {
		t.Fatalf("expected RetryAfter to be set, got nil")
	}
	if !result.ShouldFallback {
		t.Errorf("expected ShouldFallback = true for throttled result")
	}
	if time.Until(*result.RetryAfter) <= 0 || time.Until(*result.RetryAfter) > 31*time.Minute {
		t.Errorf("unexpected RetryAfter: %v", result.RetryAfter)
	}
	if !strings.Contains(result.Error, "No signed-in Model Studio browser session") {
		t.Errorf("unexpected Error: %q", result.Error)
	}
}

func TestWebConsoleFetchThrottlesWhenImportedSessionExpired(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })

	oldImporter := importModelStudioBrowserSession
	t.Cleanup(func() { importModelStudioBrowserSession = oldImporter })
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		return browserSession{
			Cookie:      "login_aliyunid_ticket=expired-ticket; login_current_pk=account",
			SourceLabel: "Chrome Profile 1",
		}, nil
	}

	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}
	result, err := strategy.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected failure, got success: %+v", result)
	}
	if result.RetryAfter == nil {
		t.Fatalf("expected RetryAfter to be set, got nil")
	}
	if !result.ShouldFallback {
		t.Errorf("expected ShouldFallback = true for throttled result")
	}
	if time.Until(*result.RetryAfter) <= 0 || time.Until(*result.RetryAfter) > 31*time.Minute {
		t.Errorf("unexpected RetryAfter: %v", result.RetryAfter)
	}
	if !strings.Contains(result.Error, "Chrome Profile 1") {
		t.Errorf("unexpected Error (expected mention of Chrome Profile 1): %q", result.Error)
	}
}

func storeModelStudioTestSession(t *testing.T, creds sessionCredentials) {
	t.Helper()
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := config.WriteCredential("modelstudio", "session", data); err != nil {
		t.Fatalf("WriteCredential: %v", err)
	}
}

func newModelStudioSessionServer(t *testing.T, quotaResponse func(cookie string) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tool/user/info.json":
			_, _ = fmt.Fprint(w, `{"code":"200","data":{"secToken":"synthetic-sec-token"},"httpStatusCode":"200","successResponse":true}`)
		case "/data/api.json":
			_, _ = fmt.Fprint(w, quotaResponse(r.Header.Get("Cookie")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func successfulSummaryPayload() string {
	return `{
		"code":"200",
		"data":{"Code":"200","Data":"{\"TotalCount\":1,\"TotalValue\":100,\"TotalSurplusValue\":75}","Success":true},
		"httpStatusCode":"200",
		"successResponse":true
	}`
}

func expiredSessionPayload() string {
	return `{
		"code":"ConsoleNeedLogin",
		"message":"You need to log in.",
		"httpStatusCode":"200",
		"successResponse":false
	}`
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func withInjectedModelStudioClock(t *testing.T, nowFn func() time.Time) {
	t.Helper()
	prev := modelStudioNow
	modelStudioNow = nowFn
	t.Cleanup(func() { modelStudioNow = prev })
}

func disableBrowserFallbackForTest(t *testing.T) {
	t.Helper()
	oldImporter := importModelStudioBrowserSession
	t.Cleanup(func() {
		importModelStudioBrowserSession = oldImporter
	})
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		return browserSession{}, errBrowserCookieImportUnavailable
	}
}

func TestSessionLifetime_RecordsOnExpiryWithInjectedClock(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	disableBrowserFallbackForTest(t)

	born := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	died := born.Add(48 * time.Hour)
	withInjectedModelStudioClock(t, func() time.Time { return died })

	storeModelStudioTestSession(t, sessionCredentials{
		Cookie:       "login_aliyunid_ticket=secret-ticket-123; login_current_pk=account",
		SecToken:     "secret-sec-token-456",
		Source:       sessionSourceBrowser,
		BrowserLabel: "Chrome Default",
		UpdatedAt:    born,
	})

	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}

	result, err := strategy.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if result.Success {
		t.Fatalf("expected expiry failure, got success: %+v", result)
	}

	lifetimePath := filepath.Join(config.DataDir(), sessionLifetimesFilename)
	info, err := os.Stat(lifetimePath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", lifetimePath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}

	rawBytes, err := os.ReadFile(lifetimePath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", lifetimePath, err)
	}
	rawJSON := string(rawBytes)
	for _, forbidden := range []string{"secret-ticket-123", "secret-sec-token-456", "login_aliyunid_ticket", "cookie"} {
		if strings.Contains(rawJSON, forbidden) {
			t.Errorf("lifetime file leaked forbidden material %q: %s", forbidden, rawJSON)
		}
	}

	entries, err := defaultSessionLifetimeStore().load()
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	got := entries[0]
	if !got.BornAt.Equal(born) {
		t.Errorf("BornAt = %v, want %v", got.BornAt, born)
	}
	if !got.DiedAt.Equal(died) {
		t.Errorf("DiedAt = %v, want %v", got.DiedAt, died)
	}
	if got.LifetimeSeconds != int((48 * time.Hour).Seconds()) {
		t.Errorf("LifetimeSeconds = %d, want %d", got.LifetimeSeconds, int((48 * time.Hour).Seconds()))
	}
	if got.Source != sessionSourceBrowser {
		t.Errorf("Source = %q, want %q", got.Source, sessionSourceBrowser)
	}
}

func TestSessionLifetime_NoDuplicateEntryForSameGeneration(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	disableBrowserFallbackForTest(t)

	born := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	currentNow := born.Add(36 * time.Hour)
	withInjectedModelStudioClock(t, func() time.Time { return currentNow })

	storeModelStudioTestSession(t, sessionCredentials{
		Cookie:    "login_aliyunid_ticket=stale-gen-1; login_current_pk=account",
		Source:    sessionSourceManual,
		UpdatedAt: born,
	})

	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}

	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch() error: %v", err)
	}

	// Second Fetch at +37h while a login park is active AND browser import
	// returns an expired browser session (hitting the second errModelStudioSession
	// check inside fetchImportedBrowserSession at web.go:358).
	currentNow = born.Add(37 * time.Hour)
	if err := writeLoginPark(loginBlockerVerification, "SMS verification required", time.Now()); err != nil {
		t.Fatalf("writeLoginPark: %v", err)
	}
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		return browserSession{
			Cookie:      "login_aliyunid_ticket=stale-browser-ticket; login_current_pk=account",
			SourceLabel: "Chrome Default",
		}, nil
	}
	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch() error: %v", err)
	}

	// Third Fetch at +38h while still parked and browser import unavailable.
	currentNow = born.Add(38 * time.Hour)
	importModelStudioBrowserSession = func(context.Context) (browserSession, error) {
		return browserSession{}, errBrowserCookieImportUnavailable
	}
	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("third Fetch() error: %v", err)
	}

	entries, err := defaultSessionLifetimeStore().load()
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (no duplicate for same BornAt)", len(entries))
	}
	if !entries[0].DiedAt.Equal(born.Add(36 * time.Hour)) {
		t.Errorf("DiedAt = %v, want %v", entries[0].DiedAt, born.Add(36*time.Hour))
	}
	if entries[0].LifetimeSeconds != int((36 * time.Hour).Seconds()) {
		t.Errorf("LifetimeSeconds = %d, want %d", entries[0].LifetimeSeconds, int((36 * time.Hour).Seconds()))
	}
}

func TestSessionLifetime_ExcludesEnvironmentAndUnparsableUpdatedAt(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	disableBrowserFallbackForTest(t)

	born := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	died := born.Add(24 * time.Hour)
	withInjectedModelStudioClock(t, func() time.Time { return died })

	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}

	// 1. Stored credential with Source == sessionSourceEnvironment and a valid non-zero UpdatedAt.
	// This directly exercises the creds.Source == sessionSourceEnvironment guard in record().
	storeModelStudioTestSession(t, sessionCredentials{
		Cookie:    "login_aliyunid_ticket=env-with-born; login_current_pk=account",
		Source:    sessionSourceEnvironment,
		UpdatedAt: born,
	})
	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch(stored environment source) error: %v", err)
	}

	// 2. MODELSTUDIO_COOKIE environment variable.
	t.Setenv("MODELSTUDIO_COOKIE", "login_aliyunid_ticket=env-cookie; login_current_pk=account")
	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch(env var) error: %v", err)
	}
	t.Setenv("MODELSTUDIO_COOKIE", "")

	// 3. Stored JSON credential with missing updated_at.
	if err := config.WriteCredential("modelstudio", "session", []byte(`{"cookie":"login_aliyunid_ticket=missing-ts; login_current_pk=account","source":"manual"}`)); err != nil {
		t.Fatalf("WriteCredential(missing updated_at): %v", err)
	}
	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch(missing updated_at) error: %v", err)
	}

	// 4. Stored JSON credential with unparsable updated_at.
	if err := config.WriteCredential("modelstudio", "session", []byte(`{"cookie":"login_aliyunid_ticket=bad-ts; login_current_pk=account","source":"browser","updated_at":"not-a-timestamp"}`)); err != nil {
		t.Fatalf("WriteCredential(unparsable updated_at): %v", err)
	}
	loaded, err := loadSessionCredential()
	if err != nil {
		t.Fatalf("loadSessionCredential(unparsable updated_at): %v", err)
	}
	if loaded.Cookie != "login_aliyunid_ticket=bad-ts; login_current_pk=account" {
		t.Errorf("loaded.Cookie = %q, want parsed cookie field", loaded.Cookie)
	}
	if !loaded.UpdatedAt.IsZero() {
		t.Errorf("loaded.UpdatedAt = %v, want zero time", loaded.UpdatedAt)
	}
	if _, err := strategy.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch(unparsable updated_at) error: %v", err)
	}

	entries, err := defaultSessionLifetimeStore().load()
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0; got %+v", len(entries), entries)
	}
}

func TestSessionLifetime_RetentionCapLast10(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())

	store := defaultSessionLifetimeStore()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := 1; i <= 12; i++ {
		born := base.Add(time.Duration(i) * 24 * time.Hour)
		died := born.Add(time.Duration(20+i) * time.Hour)
		err := store.record(sessionCredentials{
			Cookie:    fmt.Sprintf("ticket-%d", i),
			Source:    sessionSourceBrowser,
			UpdatedAt: born,
		}, died)
		if err != nil {
			t.Fatalf("record(%d): %v", i, err)
		}
	}

	entries, err := store.load()
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if len(entries) != 10 {
		t.Fatalf("len(entries) = %d, want 10", len(entries))
	}
	for idx := 0; idx < 10; idx++ {
		gen := idx + 3
		wantBorn := base.Add(time.Duration(gen) * 24 * time.Hour)
		wantLifetimeSec := int((time.Duration(20+gen) * time.Hour).Seconds())
		if !entries[idx].BornAt.Equal(wantBorn) {
			t.Errorf("entries[%d].BornAt = %v, want %v", idx, entries[idx].BornAt, wantBorn)
		}
		if entries[idx].LifetimeSeconds != wantLifetimeSec {
			t.Errorf("entries[%d].LifetimeSeconds = %d, want %d", idx, entries[idx].LifetimeSeconds, wantLifetimeSec)
		}
	}
}

func TestSessionLifetime_ZeroRecordedLifetimesShowsAgeNeverWarns(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	disableBrowserFallbackForTest(t)

	born := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	currentNow := born.Add(100 * time.Hour)
	withInjectedModelStudioClock(t, func() time.Time { return currentNow })

	storeModelStudioTestSession(t, sessionCredentials{
		Cookie:    "login_aliyunid_ticket=valid-ticket; login_current_pk=account",
		Source:    sessionSourceBrowser,
		UpdatedAt: born,
	})

	server := newModelStudioSessionServer(t, func(string) string { return successfulSummaryPayload() })
	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}

	lifetimePath := filepath.Join(config.DataDir(), sessionLifetimesFilename)

	// Case A: lifetime file absent on disk.
	if _, err := os.Stat(lifetimePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected lifetime file not to exist before Fetch, got err=%v", err)
	}
	resAbsent, err := strategy.Fetch(context.Background())
	if err != nil || !resAbsent.Success || resAbsent.Snapshot == nil {
		t.Fatalf("Fetch() with absent lifetime file failed: res=%+v err=%v", resAbsent, err)
	}
	if resAbsent.Snapshot.Session == nil {
		t.Fatalf("expected Snapshot.Session != nil when lifetime file is absent")
	}
	if resAbsent.Snapshot.Session.AgeSeconds != 360000 {
		t.Errorf("AgeSeconds = %d, want 360000", resAbsent.Snapshot.Session.AgeSeconds)
	}
	if resAbsent.Snapshot.Session.MinLifetimeSeconds != 0 {
		t.Errorf("MinLifetimeSeconds = %d, want 0", resAbsent.Snapshot.Session.MinLifetimeSeconds)
	}
	if resAbsent.Snapshot.Session.WarnSignInSoon {
		t.Errorf("WarnSignInSoon = true, want false when lifetime file is absent")
	}

	// Case B: lifetime file exists on disk with empty entries ({"entries":[]}).
	if err := os.MkdirAll(filepath.Dir(lifetimePath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(lifetimePath, []byte(`{"entries":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile empty entries: %v", err)
	}
	currentNow = born.Add(200 * time.Hour)
	resEmpty, err := strategy.Fetch(context.Background())
	if err != nil || !resEmpty.Success || resEmpty.Snapshot == nil {
		t.Fatalf("Fetch() with empty lifetime file failed: res=%+v err=%v", resEmpty, err)
	}
	if resEmpty.Snapshot.Session == nil {
		t.Fatalf("expected Snapshot.Session != nil when lifetime file has empty entries")
	}
	if resEmpty.Snapshot.Session.AgeSeconds != 720000 {
		t.Errorf("AgeSeconds = %d, want 720000", resEmpty.Snapshot.Session.AgeSeconds)
	}
	if resEmpty.Snapshot.Session.MinLifetimeSeconds != 0 {
		t.Errorf("MinLifetimeSeconds = %d, want 0", resEmpty.Snapshot.Session.MinLifetimeSeconds)
	}
	if resEmpty.Snapshot.Session.WarnSignInSoon {
		t.Errorf("WarnSignInSoon = true, want false when lifetime file has empty entries")
	}
}

func TestSessionLifetime_ReloadsInFreshStoreAndEvaluatesThreshold(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	disableBrowserFallbackForTest(t)

	born0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	currentNow := born0.Add(100 * time.Hour)
	withInjectedModelStudioClock(t, func() time.Time { return currentNow })

	storeModelStudioTestSession(t, sessionCredentials{
		Cookie:    "login_aliyunid_ticket=valid-ticket; login_current_pk=account",
		Source:    sessionSourceBrowser,
		UpdatedAt: born0,
	})

	server := newModelStudioSessionServer(t, func(string) string { return successfulSummaryPayload() })
	strategy := &WebConsoleStrategy{
		ConsoleBaseURL: server.URL,
		DashboardURL:   server.URL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}

	// With 0 recorded lifetimes: session age is populated, warning never triggers.
	res0, err := strategy.Fetch(context.Background())
	if err != nil || !res0.Success || res0.Snapshot == nil {
		t.Fatalf("Fetch() with 0 lifetimes failed: res=%+v err=%v", res0, err)
	}
	if res0.Snapshot.Session == nil {
		t.Fatalf("expected Snapshot.Session to be populated")
	}
	if res0.Snapshot.Session.WarnSignInSoon {
		t.Errorf("expected WarnSignInSoon=false with 0 recorded lifetimes")
	}

	// Record 3 prior lifetimes (48h, 40h, 60h -> min is 40h, 75% threshold is 30h).
	writerStore := defaultSessionLifetimeStore()
	priorHours := []int{48, 40, 60}
	for i, hrs := range priorHours {
		b := time.Date(2026, 9, 10+i*3, 0, 0, 0, 0, time.UTC)
		d := b.Add(time.Duration(hrs) * time.Hour)
		if err := writerStore.record(sessionCredentials{
			Cookie:    "prior",
			Source:    sessionSourceBrowser,
			UpdatedAt: b,
		}, d); err != nil {
			t.Fatalf("record prior %d: %v", i, err)
		}
	}

	// Reload in a fresh store instance to verify persistence across restarts.
	reloadedStore := sessionLifetimeStore{
		path: filepath.Join(config.DataDir(), sessionLifetimesFilename),
	}
	if gotMin := reloadedStore.minLifetimeSeconds(); gotMin != int((40 * time.Hour).Seconds()) {
		t.Fatalf("reloaded minLifetimeSeconds() = %d, want %d", gotMin, int((40 * time.Hour).Seconds()))
	}

	bornActive := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	storeModelStudioTestSession(t, sessionCredentials{
		Cookie:    "login_aliyunid_ticket=valid-ticket; login_current_pk=account",
		Source:    sessionSourceBrowser,
		UpdatedAt: bornActive,
	})

	// At 29h59m (below 75% of 40h = 30h): age shows, no warning.
	currentNow = bornActive.Add(29*time.Hour + 59*time.Minute)
	resBelow, err := strategy.Fetch(context.Background())
	if err != nil || !resBelow.Success || resBelow.Snapshot == nil {
		t.Fatalf("Fetch() below threshold failed: res=%+v err=%v", resBelow, err)
	}
	if resBelow.Snapshot.Session == nil || resBelow.Snapshot.Session.WarnSignInSoon {
		t.Fatalf("expected WarnSignInSoon=false at 29h59m, got %+v", resBelow.Snapshot.Session)
	}

	// At 30h (exactly at 75% of 40h): age and "sign in soon" warning both show.
	currentNow = bornActive.Add(30 * time.Hour)
	resAt, err := strategy.Fetch(context.Background())
	if err != nil || !resAt.Success || resAt.Snapshot == nil {
		t.Fatalf("Fetch() at threshold failed: res=%+v err=%v", resAt, err)
	}
	if resAt.Snapshot.Session == nil || !resAt.Snapshot.Session.WarnSignInSoon {
		t.Fatalf("expected WarnSignInSoon=true at 30h, got %+v", resAt.Snapshot.Session)
	}
	minSec := int((40 * time.Hour).Seconds())
	if resAt.Snapshot.Session.AgeSeconds != int(float64(minSec)*modelStudioSessionWarnFraction) {
		t.Fatalf("AgeSeconds at 30h = %d, want %d", resAt.Snapshot.Session.AgeSeconds, int(float64(minSec)*modelStudioSessionWarnFraction))
	}

	// At 31h (77.5% of 40h, strictly between 75% = 30h and 80% = 32h):
	// age and "sign in soon" warning both show.
	currentNow = bornActive.Add(31 * time.Hour)
	resBetween, err := strategy.Fetch(context.Background())
	if err != nil || !resBetween.Success || resBetween.Snapshot == nil {
		t.Fatalf("Fetch() at 31h failed: res=%+v err=%v", resBetween, err)
	}
	if resBetween.Snapshot.Session == nil || !resBetween.Snapshot.Session.WarnSignInSoon {
		t.Fatalf("expected WarnSignInSoon=true at 31h (77.5%% of 40h), got %+v", resBetween.Snapshot.Session)
	}

	if modelStudioSessionWarnFraction != 0.75 {
		t.Fatalf("modelStudioSessionWarnFraction = %v, want 0.75", modelStudioSessionWarnFraction)
	}
}

func TestSessionLifetime_RecordPreservesLedgerOnLoadOrWriteFailure(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())

	store := defaultSessionLifetimeStore()
	born1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	died1 := born1.Add(40 * time.Hour)
	if err := store.record(sessionCredentials{
		Cookie:    "t=1",
		Source:    sessionSourceBrowser,
		UpdatedAt: born1,
	}, died1); err != nil {
		t.Fatalf("initial record(): %v", err)
	}

	initialBytes, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile(initial): %v", err)
	}

	// Subcase A: replacement failure in a read-only parent directory preserves previous ledger bytes.
	parentDir := filepath.Dir(store.path)
	if err := os.Chmod(parentDir, 0o500); err != nil {
		t.Fatalf("Chmod(parentDir, 0500): %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parentDir, 0o700) })

	born2 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	err = store.record(sessionCredentials{
		Cookie:    "t=2",
		Source:    sessionSourceBrowser,
		UpdatedAt: born2,
	}, born2.Add(42*time.Hour))
	if err == nil {
		t.Fatalf("expected record() in read-only parent dir to return an error, got nil")
	}
	if err := os.Chmod(parentDir, 0o700); err != nil {
		t.Fatalf("Chmod(parentDir, 0700): %v", err)
	}
	afterWriteFailBytes, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile(after write failure): %v", err)
	}
	if string(afterWriteFailBytes) != string(initialBytes) {
		t.Fatalf("ledger modified on failed replacement:\ngot:  %s\nwant: %s", string(afterWriteFailBytes), string(initialBytes))
	}

	// Subcase B: corrupt existing ledger returns load error and is not overwritten with a single-entry file.
	corruptPayload := []byte("{corrupt-json")
	if err := os.WriteFile(store.path, corruptPayload, 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt): %v", err)
	}
	born3 := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	err = store.record(sessionCredentials{
		Cookie:    "t=3",
		Source:    sessionSourceBrowser,
		UpdatedAt: born3,
	}, born3.Add(44*time.Hour))
	if err == nil {
		t.Fatalf("expected record() on corrupt ledger to return a load error, got nil")
	}
	afterCorruptBytes, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile(after corrupt load): %v", err)
	}
	if string(afterCorruptBytes) != string(corruptPayload) {
		t.Fatalf("corrupt ledger was overwritten on load error:\ngot:  %s\nwant: %s", string(afterCorruptBytes), string(corruptPayload))
	}
}

func TestSessionLifetime_IgnoresMalformedEntriesInMinimumAndDedupe(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())

	store := defaultSessionLifetimeStore()
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Write a ledger mixing malformed entries (missing/zero timestamps, reversed timestamps,
	// mismatched lifetime_seconds) with one valid 40h (144000s) observation.
	rawLedger := `{
  "entries": [
    {"lifetime_seconds": 1, "source": "browser"},
    {"born_at": "0001-01-01T00:00:00Z", "died_at": "0001-01-01T00:00:00Z", "lifetime_seconds": 1, "source": "browser"},
    {"born_at": "2026-10-03T10:00:00Z", "died_at": "2026-10-03T09:00:00Z", "lifetime_seconds": 10, "source": "browser"},
    {"born_at": "2026-10-10T00:00:00Z", "died_at": "2026-10-11T16:00:00Z", "lifetime_seconds": 1, "source": "browser"},
    {"born_at": "2026-10-05T00:00:00Z", "died_at": "2026-10-06T16:00:00Z", "lifetime_seconds": 144000, "source": "browser", "extra_field": "ignored"}
  ]
}`
	if err := os.WriteFile(store.path, []byte(rawLedger), 0o600); err != nil {
		t.Fatalf("WriteFile(rawLedger): %v", err)
	}

	if gotMin := store.minLifetimeSeconds(); gotMin != 144000 {
		t.Fatalf("minLifetimeSeconds() = %d, want 144000 (40h valid entry, ignoring malformed entries)", gotMin)
	}

	activeBorn := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	info := store.evaluateSession(sessionCredentials{
		Cookie:    "active",
		Source:    sessionSourceBrowser,
		UpdatedAt: activeBorn,
	}, activeBorn.Add(1*time.Hour))
	if info == nil {
		t.Fatalf("evaluateSession() returned nil")
	}
	if info.MinLifetimeSeconds != 144000 {
		t.Errorf("info.MinLifetimeSeconds = %d, want 144000", info.MinLifetimeSeconds)
	}
	if info.WarnSignInSoon {
		t.Errorf("info.WarnSignInSoon = true at 1h age, want false (malformed 1s/10s entries must not trigger premature warning)")
	}

	// Recording a valid observation for the generation that had a mismatched entry
	// (2026-10-10T00:00:00Z) must succeed and prune malformed entries on save.
	bornBad := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if err := store.record(sessionCredentials{
		Cookie:    "re-record",
		Source:    sessionSourceBrowser,
		UpdatedAt: bornBad,
	}, bornBad.Add(36*time.Hour)); err != nil {
		t.Fatalf("record(bornBad): %v", err)
	}

	entries, err := store.load()
	if err != nil {
		t.Fatalf("load(): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) after pruning malformed = %d, want 2; got %+v", len(entries), entries)
	}
	if entries[0].LifetimeSeconds != 144000 || entries[1].LifetimeSeconds != 129600 {
		t.Errorf("entries lifetimes = [%d, %d], want [144000, 129600]", entries[0].LifetimeSeconds, entries[1].LifetimeSeconds)
	}
}
