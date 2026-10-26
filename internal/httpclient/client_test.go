package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestNew_DefaultTimeout(t *testing.T) {
	c := New()
	if c.http.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", c.http.Timeout)
	}
}

func TestNewWithTimeout(t *testing.T) {
	c := NewWithTimeout(10 * time.Second)
	if c.http.Timeout != 10*time.Second {
		t.Errorf("expected timeout 10s, got %v", c.http.Timeout)
	}
}

func TestGetJSON_Success(t *testing.T) {
	type resp struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp{Name: "test", Value: 42})
	}))
	defer srv.Close()

	c := New()
	var out resp
	httpResp, err := c.GetJSON(srv.URL, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", httpResp.StatusCode)
	}
	if out.Name != "test" || out.Value != 42 {
		t.Errorf("unexpected response: %+v", out)
	}
}

func TestGetJSON_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := New()
	var out map[string]string
	httpResp, err := c.GetJSON(srv.URL, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", httpResp.StatusCode)
	}
	// Body should still be readable from Response
	if len(httpResp.Body) == 0 {
		t.Error("expected body to be captured even on non-200")
	}
}

func TestGetJSON_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := New()
	var out map[string]string
	httpResp, err := c.GetJSON(srv.URL, &out)
	if err != nil {
		t.Fatalf("unexpected network error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", httpResp.StatusCode)
	}
	if httpResp.JSONErr == nil {
		t.Error("expected JSONErr to be set for invalid JSON")
	}
}

func TestGetJSON_NilOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"key":"val"}`))
	}))
	defer srv.Close()

	c := New()
	httpResp, err := c.GetJSON(srv.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
}

func TestGetJSON_NetworkError(t *testing.T) {
	c := New()
	_, err := c.GetJSON("http://127.0.0.1:1", nil)
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestPostJSON_WithBody(t *testing.T) {
	type reqBody struct {
		Input string `json:"input"`
	}
	type respBody struct {
		Output string `json:"output"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		var in reqBody
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(respBody{Output: "echo:" + in.Input})
	}))
	defer srv.Close()

	c := New()
	var out respBody
	httpResp, err := c.PostJSON(srv.URL, reqBody{Input: "hello"}, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
	if out.Output != "echo:hello" {
		t.Errorf("unexpected output: %s", out.Output)
	}
}

func TestPostJSON_NilBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		// Body should be empty when nil is passed
		body := make([]byte, 1)
		n, _ := r.Body.Read(body)
		if n != 0 {
			t.Errorf("expected empty body, got %d bytes", n)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := New()
	var out map[string]string
	httpResp, err := c.PostJSON(srv.URL, nil, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
	if out["status"] != "ok" {
		t.Errorf("unexpected response: %v", out)
	}
}

func TestPostForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if ct != "application/x-www-form-urlencoded" {
			t.Errorf("expected form Content-Type, got %s", ct)
		}
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("unexpected grant_type: %s", r.FormValue("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new-token"})
	}))
	defer srv.Close()

	c := New()
	var out map[string]string
	form := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "old-token",
	}
	httpResp, err := c.PostForm(srv.URL, form, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
	if out["access_token"] != "new-token" {
		t.Errorf("unexpected token: %s", out["access_token"])
	}
}

func TestNewFromConfig(t *testing.T) {
	c := NewFromConfig(15.0)
	if c.http.Timeout != 15*time.Second {
		t.Errorf("expected timeout 15s, got %v", c.http.Timeout)
	}
}

func TestNewFromConfig_Zero(t *testing.T) {
	c := NewFromConfig(0)
	if c.http.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s for zero config, got %v", c.http.Timeout)
	}
}

func TestDo_CustomMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	c := New()
	httpResp, err := c.Do(http.MethodPatch, srv.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 204 {
		t.Errorf("expected 204, got %d", httpResp.StatusCode)
	}
}

// Context-aware method tests

func TestDoCtx_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	c := New()
	_, err := c.DoCtx(ctx, http.MethodGet, srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestDoCtx_ContextDeadlineExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	c := NewWithTimeout(30 * time.Second) // Long client timeout; short context timeout
	_, err := c.DoCtx(ctx, http.MethodGet, srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for context deadline exceeded")
	}
}

func TestDoCtx_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`OK`))
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New()
	resp, err := c.DoCtx(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestDoCtx_WithOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ctx-token" {
			t.Errorf("expected bearer token, got %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New()
	resp, err := c.DoCtx(ctx, http.MethodGet, srv.URL, nil, WithBearer("ctx-token"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestDoCtx_RecordsFinalURLAfterRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := New().DoCtx(context.Background(), http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.URL == nil {
		t.Fatal("expected final response URL")
	}
	if resp.URL.Path != "/final" {
		t.Errorf("final URL path = %q, want /final", resp.URL.Path)
	}
}

func TestGetJSONCtx_Success(t *testing.T) {
	type resp struct {
		Name string `json:"name"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(resp{Name: "ctx-test"})
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New()
	var out resp
	httpResp, err := c.GetJSONCtx(ctx, srv.URL, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
	if out.Name != "ctx-test" {
		t.Errorf("expected name ctx-test, got %s", out.Name)
	}
}

func TestGetJSONCtx_Cancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := New()
	_, err := c.GetJSONCtx(ctx, srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestPostJSONCtx_Success(t *testing.T) {
	type reqBody struct {
		Input string `json:"input"`
	}
	type respBody struct {
		Output string `json:"output"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var in reqBody
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(respBody{Output: "echo:" + in.Input})
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New()
	var out respBody
	httpResp, err := c.PostJSONCtx(ctx, srv.URL, reqBody{Input: "hello"}, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
	if out.Output != "echo:hello" {
		t.Errorf("unexpected output: %s", out.Output)
	}
}

func TestPostJSONCtx_Cancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := New()
	_, err := c.PostJSONCtx(ctx, srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestPostFormCtx_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("unexpected grant_type: %s", r.FormValue("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "new"})
	}))
	defer srv.Close()

	ctx := context.Background()
	c := New()
	var out map[string]string
	httpResp, err := c.PostFormCtx(ctx, srv.URL, map[string]string{"grant_type": "refresh_token"}, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if httpResp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", httpResp.StatusCode)
	}
	if out["access_token"] != "new" {
		t.Errorf("unexpected token: %s", out["access_token"])
	}
}

func TestPostFormCtx_Cancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := New()
	_, err := c.PostFormCtx(ctx, srv.URL, map[string]string{"k": "v"}, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestDoCtx_CancelMidFlight(t *testing.T) {
	// Verify that cancelling mid-flight interrupts the request
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Block until the client disconnects
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := New()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.DoCtx(ctx, http.MethodGet, srv.URL, nil)
		errCh <- err
	}()

	// Wait for the handler to start, then cancel
	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after mid-flight cancellation")
	}
}

func TestDoCtx_DebugRawDump_WritesStatusURLRedactedHeadersAndExactBody(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	testenv.SetCacheDir(t.Setenv, cacheDir)
	t.Setenv(DebugRawEnv, "1")

	exactBody := []byte("{\"code\":200,\"data\":{\"limits\":[{\"type\":\"CREDIT_LIMIT\",\"usage\":42.5}]}}\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Set-Cookie", "sid=server-cookie-secret-999; Path=/; HttpOnly")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(exactBody)
	}))
	defer srv.Close()

	ctx := fetch.WithProvider(context.Background(), "zai")
	c := New()
	resp, err := c.DoCtx(
		ctx,
		http.MethodGet,
		srv.URL+"/api/monitor/usage/quota/limit?apiKey=query-secret-123&mode=full",
		nil,
		WithBearer("bearer-secret-456"),
		WithCookie("token", "cookie-secret-789"),
		WithHeader("Accept", "application/json"),
	)
	if err != nil {
		t.Fatalf("DoCtx() unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(resp.Body, exactBody) {
		t.Fatalf("resp.Body = %q, want %q", resp.Body, exactBody)
	}

	rawDir := config.RawDumpsDir()
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error: %v", rawDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 dump file in %q, got %d", rawDir, len(entries))
	}
	if !strings.Contains(entries[0].Name(), "-zai-") || !strings.HasSuffix(entries[0].Name(), ".dump") {
		t.Errorf("unexpected dump filename %q", entries[0].Name())
	}

	dumpPath := filepath.Join(rawDir, entries[0].Name())
	dumpBytes, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error: %v", dumpPath, err)
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(rawDir)
		if err != nil {
			t.Fatalf("Stat(%q) error: %v", rawDir, err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("raw dir perm = %o, want 0700", perm)
		}
		fileInfo, err := os.Stat(dumpPath)
		if err != nil {
			t.Fatalf("Stat(%q) error: %v", dumpPath, err)
		}
		if perm := fileInfo.Mode().Perm(); perm != 0o600 {
			t.Errorf("dump file perm = %o, want 0600", perm)
		}
	}

	parts := bytes.SplitN(dumpBytes, []byte("\n\n"), 2)
	if len(parts) != 2 {
		t.Fatalf("dump missing header/body separator: %q", string(dumpBytes))
	}
	headerSection := string(parts[0])
	bodySection := parts[1]

	for _, wantLine := range []string{
		"Timestamp: ",
		"Provider: zai",
		"Method: GET",
		"URL: " + srv.URL + "/api/monitor/usage/quota/limit?apiKey=[REDACTED]&mode=[REDACTED]",
		"Status: 200 OK",
		"Accept: application/json",
		"Authorization: [REDACTED]",
		"Content-Type: application/json; charset=utf-8",
		"Cookie: [REDACTED]",
		"Retry-After: 120",
		"Set-Cookie: [REDACTED]",
	} {
		if !strings.Contains(headerSection, wantLine) {
			t.Errorf("dump header section missing %q; got:\n%s", wantLine, headerSection)
		}
	}

	if !bytes.Equal(bodySection, exactBody) {
		t.Errorf("dump body = %q, want exact %q", bodySection, exactBody)
	}
}

func TestDoCtx_DebugRawDump_MandatoryRedactionOfAuthCookieAndSetCookie(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	testenv.SetCacheDir(t.Setenv, cacheDir)
	t.Setenv(DebugRawEnv, "true")

	const (
		authSecret      = "Bearer sk-ant-oat01-super-secret-token-value"
		cookieSecret    = "sessionKey=sk-ant-sid01-ultra-secret-cookie"
		setCookieSecret = "__Secure-next-auth.session-token=resp-cookie-secret"
		apiKeySecret    = "AIzaSySecretGoogKeyValue"
		querySecret     = "secret_query_token_value"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", setCookieSecret)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := New()
	_, err := c.DoCtx(
		context.Background(),
		http.MethodPost,
		srv.URL+"/v1/usage?access_token="+querySecret,
		strings.NewReader(`{}`),
		WithProvider("claude"),
		WithHeader("Authorization", authSecret),
		WithHeader("Cookie", cookieSecret),
		WithHeader("x-goog-api-key", apiKeySecret),
	)
	if err != nil {
		t.Fatalf("DoCtx() unexpected error: %v", err)
	}

	entries, err := os.ReadDir(config.RawDumpsDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 dump file, got entries=%v err=%v", entries, err)
	}
	rawBytes, err := os.ReadFile(filepath.Join(config.RawDumpsDir(), entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	for _, secret := range []string{authSecret, "sk-ant-oat01-super-secret-token-value", cookieSecret, setCookieSecret, apiKeySecret, querySecret} {
		if bytes.Contains(rawBytes, []byte(secret)) {
			t.Fatalf("dump leaked secret %q in:\n%s", secret, string(rawBytes))
		}
	}

	dumpText := string(rawBytes)
	for _, required := range []string{
		"Authorization: [REDACTED]",
		"Cookie: [REDACTED]",
		"Set-Cookie: [REDACTED]",
		"X-Goog-Api-Key: [REDACTED]",
		"access_token=[REDACTED]",
	} {
		if !strings.Contains(dumpText, required) {
			t.Errorf("dump missing redacted marker %q in:\n%s", required, dumpText)
		}
	}
}

func TestDoCtx_DebugRawDump_DisabledByDefault(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	testenv.SetCacheDir(t.Setenv, cacheDir)
	t.Setenv(DebugRawEnv, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New()
	resp, err := c.DoCtx(fetch.WithProvider(context.Background(), "zai"), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("DoCtx() error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}

	if _, err := os.Stat(config.RawDumpsDir()); !os.IsNotExist(err) {
		t.Errorf("expected %q not to exist when %s is unset, stat err = %v", config.RawDumpsDir(), DebugRawEnv, err)
	}
}

func TestDoCtx_DebugRawDump_BestEffortFailureLogsVerboseAndSucceeds(t *testing.T) {
	blockingFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blockingFile, []byte("block"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error: %v", blockingFile, err)
	}
	testenv.SetCacheDir(t.Setenv, blockingFile)
	t.Setenv(DebugRawEnv, "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := New()

	// Default logger (WarnLevel): fetch succeeds and dump error stays quiet.
	defaultCtx, defaultBuf := logging.NewTestContext(logging.Flags{})
	defaultCtx = fetch.WithProvider(defaultCtx, "zai")
	resp, err := c.DoCtx(defaultCtx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("DoCtx() with default logger failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"status":"ok"}` {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode, string(resp.Body))
	}
	if defaultBuf.Len() != 0 {
		t.Errorf("expected no log output at default level, got %q", defaultBuf.String())
	}

	// Verbose logger (DebugLevel): fetch still succeeds and logs dump failure.
	verboseCtx, verboseBuf := logging.NewTestContext(logging.Flags{Verbose: true})
	verboseCtx = fetch.WithProvider(verboseCtx, "zai")
	resp, err = c.DoCtx(verboseCtx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("DoCtx() with verbose logger failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(verboseBuf.String(), "writing raw response dump failed") {
		t.Errorf("expected verbose log to mention dump failure, got %q", verboseBuf.String())
	}
}

func TestDoCtx_DebugRawDump_RejectsSymlinkRawDirAndFetchStillSucceeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission semantics are Unix-specific")
	}

	root := t.TempDir()
	externalDir := filepath.Join(root, "external")
	if err := os.MkdirAll(externalDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(externalDir): %v", err)
	}
	if err := os.Chmod(externalDir, 0o755); err != nil {
		t.Fatalf("Chmod(externalDir): %v", err)
	}

	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(cacheDir): %v", err)
	}
	testenv.SetCacheDir(t.Setenv, cacheDir)
	t.Setenv(DebugRawEnv, "1")

	if err := os.Symlink(externalDir, config.RawDumpsDir()); err != nil {
		t.Fatalf("Symlink(externalDir, RawDumpsDir): %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := New()
	verboseCtx, verboseBuf := logging.NewTestContext(logging.Flags{Verbose: true})
	verboseCtx = fetch.WithProvider(verboseCtx, "zai")
	resp, err := c.DoCtx(verboseCtx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("DoCtx() failed when raw dir was symlink: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"status":"ok"}` {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode, string(resp.Body))
	}
	if !strings.Contains(verboseBuf.String(), "writing raw response dump failed") {
		t.Errorf("expected verbose log to mention dump failure, got %q", verboseBuf.String())
	}

	entries, err := os.ReadDir(externalDir)
	if err != nil {
		t.Fatalf("ReadDir(externalDir): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("externalDir entries = %d, want 0", len(entries))
	}
	extInfo, err := os.Stat(externalDir)
	if err != nil {
		t.Fatalf("Stat(externalDir): %v", err)
	}
	if perm := extInfo.Mode().Perm(); perm != 0o755 {
		t.Errorf("externalDir permissions = %o, want unchanged 0755", perm)
	}
}

func TestR1_4_DoCtx_PartialBodyReadFailureStillWritesDump(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	testenv.SetCacheDir(t.Setenv, cacheDir)
	t.Setenv(DebugRawEnv, "1")

	partialBody := []byte(`{"partial":1`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "sid=r1-4-cookie-secret")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(partialBody)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()

	c := New()
	resp, err := c.DoCtx(
		fetch.WithProvider(context.Background(), "zai"),
		http.MethodGet,
		srv.URL+"/v1/quota?token=r1-4-query-secret",
		nil,
		WithBearer("r1-4-bearer-secret"),
	)
	if err == nil {
		t.Fatalf("DoCtx() succeeded with resp=%+v, want error on truncated body", resp)
	}
	if resp != nil {
		t.Fatalf("DoCtx() resp = %+v, want nil on error", resp)
	}

	entries, readDirErr := os.ReadDir(config.RawDumpsDir())
	if readDirErr != nil {
		t.Fatalf("ReadDir(RawDumpsDir): %v", readDirErr)
	}
	if len(entries) != 1 {
		t.Fatalf("raw dump file count = %d, want 1", len(entries))
	}

	dumpBytes, readErr := os.ReadFile(filepath.Join(config.RawDumpsDir(), entries[0].Name()))
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v", entries[0].Name(), readErr)
	}
	headerBlock, gotBody, ok := bytes.Cut(dumpBytes, []byte("\n\n"))
	if !ok {
		t.Fatalf("dump missing header/body separator: %q", string(dumpBytes))
	}
	headersText := string(headerBlock) + "\n"
	for _, want := range []string{
		"Provider: zai\n",
		"Status: 200 OK\n",
		"Authorization: [REDACTED]\n",
		"Set-Cookie: [REDACTED]\n",
		"token=[REDACTED]",
	} {
		if !strings.Contains(headersText, want) {
			t.Errorf("dump headers missing %q in:\n%s", want, headersText)
		}
	}
	for _, secret := range []string{
		"r1-4-cookie-secret",
		"r1-4-query-secret",
		"r1-4-bearer-secret",
	} {
		if bytes.Contains(dumpBytes, []byte(secret)) {
			t.Errorf("dump leaked secret %q", secret)
		}
	}
	if !bytes.Equal(gotBody, partialBody) {
		t.Errorf("dump partial body = %q, want %q", string(gotBody), string(partialBody))
	}
}
