package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joshuadavidthomas/vibeusage/internal/provider"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

// codexTestJWT builds an unsigned JWT; vibeusage only reads its exp claim.
func codexTestJWT(subject string, exp int64) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"sub": subject, "exp": exp})
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

func setupCodexRenewalEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setUserHome(t, home)
	t.Setenv("CODEX_HOME", "")
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	stubCodexKeychainEmpty(t)
	return home
}

func readCodexRefreshState(t *testing.T, home string) (map[string]any, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".cache", "vibeusage", "codex-refresh", "last-attempt.json"))
	if err != nil {
		t.Fatalf("read shared coordination state: %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return state, string(data)
}

func TestFetch_RenewsThroughSharedCoordinationDir(t *testing.T) {
	home := setupCodexRenewalEnv(t)
	fresh := codexTestJWT("fresh", 4102444800) // 2100-01-01
	writeCodexAuth(t, home, `{"auth_mode":"chatgpt","tokens":{"access_token":"stale","refresh_token":"ref","account_id":"acct"}}`)
	t.Setenv("OPENAI_API_KEY", "OPENAI_SENTINEL")
	t.Setenv("CODEX_API_KEY", "CODEX_SENTINEL")
	prependFakeCodex(t, "#!/usr/bin/env sh\n"+
		"{ printf 'args=%s\\n' \"$*\"; printf 'cwd=%s\\n' \"$(pwd -P)\"; printf 'OPENAI_API_KEY=%s\\n' \"$OPENAI_API_KEY\"; printf 'CODEX_API_KEY=%s\\n' \"$CODEX_API_KEY\"; } > \"$HOME/invocation.txt\"\n"+
		"cat > \"$HOME/.codex/auth.json\" <<'JSON'\n"+
		`{"auth_mode":"chatgpt","tokens":{"access_token":"`+fresh+`","refresh_token":"ref2","account_id":"acct"}}`+"\nJSON\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer stale":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case "Bearer " + fresh:
			_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":10}}}`))
		default:
			http.Error(w, "unexpected token", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	writeCodexConfig(t, home, server.URL)

	result, err := (&OAuthStrategy{HTTPTimeout: 2}).Fetch(context.Background())
	if err != nil || !result.Success {
		t.Fatalf("Fetch() = %+v, %v; want success after CLI renewal", result, err)
	}

	report, err := os.ReadFile(filepath.Join(home, "invocation.txt"))
	if err != nil {
		t.Fatalf("fake codex was not invoked: %v", err)
	}
	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(report)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		lines[name] = value
	}
	if want := strings.Join(codexRenewalArgs, " "); lines["args"] != want {
		t.Errorf("args = %q, want %q", lines["args"], want)
	}
	if lines["OPENAI_API_KEY"] != "" || lines["CODEX_API_KEY"] != "" {
		t.Errorf("API-key overrides reached the renewal subprocess: %q", report)
	}
	wantDir, _ := filepath.EvalSymlinks(os.TempDir())
	if gotDir, _ := filepath.EvalSymlinks(lines["cwd"]); gotDir != wantDir {
		t.Errorf("cwd = %q, want neutral temp dir %q", lines["cwd"], wantDir)
	}

	state, raw := readCodexRefreshState(t, home)
	outcome, _ := state["outcome"].(map[string]any)
	if outcome["reason"] != "refreshed" {
		t.Errorf("outcome reason = %v, want refreshed", outcome["reason"])
	}
	// The renewed chain's JWT expiry is recorded, matching DSH's format.
	if state["credential_expires_at"] != "2100-01-01T00:00:00Z" {
		t.Errorf("credential_expires_at = %v, want 2100-01-01T00:00:00Z", state["credential_expires_at"])
	}
	for _, secret := range []string{fresh, "stale", "ref2", "OPENAI_SENTINEL", "CODEX_SENTINEL"} {
		if strings.Contains(raw, secret) {
			t.Errorf("coordination state leaked %q", secret)
		}
	}
}

func TestFetch_RejectedRenewalIsFatalAndCoolsDown(t *testing.T) {
	home := setupCodexRenewalEnv(t)
	writeCodexAuth(t, home, `{"tokens":{"access_token":"stale","refresh_token":"dead"}}`)
	prependFakeCodex(t, "#!/usr/bin/env sh\n"+
		"echo x >> \"$HOME/spawns.txt\"\n"+
		"echo 'Error: Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.' >&2\n"+
		"exit 1\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	writeCodexConfig(t, home, server.URL)

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := (&OAuthStrategy{HTTPTimeout: 2}).Fetch(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: Fetch() err = %v", attempt, err)
		}
		if result.Success || result.ShouldFallback || result.RetryAfter != nil {
			t.Fatalf("attempt %d: result = %+v, want fatal (login required)", attempt, result)
		}
		if !strings.Contains(result.Error, "refresh_rejected") || !strings.Contains(result.Error, "codex login") {
			t.Errorf("attempt %d: error = %q, want refresh_rejected hint and `codex login` advice", attempt, result.Error)
		}
	}
	spawns, err := os.ReadFile(filepath.Join(home, "spawns.txt"))
	if err != nil {
		t.Fatalf("fake codex was not invoked: %v", err)
	}
	if got := strings.Count(string(spawns), "x"); got != 1 {
		t.Errorf("codex spawned %d times, want 1 (second fetch must hit the shared cooldown)", got)
	}
	state, _ := readCodexRefreshState(t, home)
	if outcome, _ := state["outcome"].(map[string]any); outcome["hint"] != "refresh_rejected" {
		t.Errorf("persisted hint = %v, want refresh_rejected", outcome["hint"])
	}
}

func TestLoadCredentials_DerivesExpiryFromAccessTokenJWT(t *testing.T) {
	home := setupCodexRenewalEnv(t)
	token := codexTestJWT("acct", 1792534962)
	writeCodexAuth(t, home, `{"tokens":{"access_token":"`+token+`","refresh_token":"ref"}}`)

	creds := (&OAuthStrategy{}).loadCredentials()
	if creds == nil {
		t.Fatal("loadCredentials() = nil")
	}
	if creds.ExpiresAt != "2026-10-20T22:22:42Z" {
		t.Errorf("ExpiresAt = %q, want JWT exp 2026-10-20T22:22:42Z", creds.ExpiresAt)
	}
}

func TestLoadCredentials_FollowsCodexHome(t *testing.T) {
	home := setupCodexRenewalEnv(t)
	writeCodexAuth(t, home, `{"tokens":{"access_token":"default-profile","refresh_token":"ref"}}`)
	custom := t.TempDir()
	if err := os.WriteFile(filepath.Join(custom, "auth.json"), []byte(`{"tokens":{"access_token":"custom-profile","refresh_token":"ref"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", custom)

	creds := (&OAuthStrategy{}).loadCredentials()
	if creds == nil || creds.AccessToken != "custom-profile" {
		t.Fatalf("loadCredentials() = %+v, want the CODEX_HOME profile", creds)
	}
}

func TestDiagnoseAuth_SecretFreeAndReadOnly(t *testing.T) {
	home := setupCodexRenewalEnv(t)
	access := codexTestJWT("ACCESS_SECRET_SENTINEL", 1792534962)
	writeCodexAuth(t, home, `{"auth_mode":"chatgpt","last_refresh":"2026-10-10T22:22:43.259667Z","tokens":{"access_token":"`+access+`","refresh_token":"REFRESH_SECRET_SENTINEL","account_id":"ACCOUNT_SENTINEL"}}`)
	t.Setenv("OPENAI_API_KEY", "OPENAI_SECRET_SENTINEL")
	prependFakeCodex(t, "#!/usr/bin/env sh\necho 'should not run' > \"$HOME/ran.txt\"\n")
	old := readCodexLoginStatus
	t.Cleanup(func() { readCodexLoginStatus = old })
	readCodexLoginStatus = func(context.Context, string) (provider.DiagnosticStatus, string) {
		return provider.DiagOK, "logged in with ChatGPT"
	}

	diags := Codex{}.DiagnoseAuth(context.Background())
	byName := map[string]provider.Diagnostic{}
	for _, d := range diags {
		byName[d.Name] = d
		for _, secret := range []string{access, "ACCESS_SECRET_SENTINEL", "REFRESH_SECRET_SENTINEL", "ACCOUNT_SENTINEL", "OPENAI_SECRET_SENTINEL"} {
			if strings.Contains(d.Detail, secret) || strings.Contains(d.Name, secret) {
				t.Errorf("diagnostic %q leaked %q", d.Name, secret)
			}
		}
	}
	file := byName["CLI credential file"]
	if file.Status != provider.DiagOK || !strings.Contains(file.Detail, "auth mode=chatgpt") || !strings.Contains(file.Detail, "expires=2026-10-20T22:22:42Z") || !strings.Contains(file.Detail, "last refresh=2026-10-10T22:22:43Z") {
		t.Errorf("credential file diagnostic = %+v", file)
	}
	if got := byName["Selected OAuth source"]; got.Status != provider.DiagInfo || !strings.HasPrefix(got.Detail, "CLI credential file") {
		t.Errorf("selected source = %+v", got)
	}
	if got := byName["Auth environment overrides"]; !strings.Contains(got.Detail, "OPENAI_API_KEY") {
		t.Errorf("override diagnostic = %+v, want OPENAI_API_KEY named", got)
	}
	if got := byName["CLI authentication"]; got.Status != provider.DiagOK {
		t.Errorf("CLI authentication = %+v", got)
	}
	if got := byName["Last renewal attempt"]; got.Detail != "none recorded" {
		t.Errorf("last renewal = %+v, want none recorded", got)
	}
	if _, err := os.Stat(filepath.Join(home, "ran.txt")); err == nil {
		t.Error("diagnostics launched the Codex CLI for renewal")
	}
	if _, err := os.Stat(filepath.Join(home, ".cache", "vibeusage", "codex-refresh")); err == nil {
		t.Error("diagnostics created coordination state")
	}
}
