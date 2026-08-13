package modelstudio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
