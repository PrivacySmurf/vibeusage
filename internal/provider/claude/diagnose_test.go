package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joshuadavidthomas/vibeusage/internal/auth/oauth"
	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestDiagnoseAuthReadOnlyAndTokenSafe(t *testing.T) {
	home := t.TempDir()
	setUserHome(t, home)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	writeClaudeAuth(t, home, `{"claudeAiOauth":{"accessToken":"FILE_SECRET_SENTINEL","refreshToken":"REFRESH_SECRET_SENTINEL","expiresAt":1}}`)
	if err := config.WriteCredential("claude", "oauth", []byte(`{"access_token":"ORPHAN_SECRET_SENTINEL"}`)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.CredentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	oldRead, oldUsername, oldStatus := readKeychainSecret, currentUsername, readClaudeAuthStatus
	t.Cleanup(func() { readKeychainSecret = oldRead; currentUsername = oldUsername; readClaudeAuthStatus = oldStatus })
	currentUsername = func() (string, error) { return "test-user", nil }
	readKeychainSecret = func(_ string, account string) (string, error) {
		if account != "" {
			return `{"claudeAiOauth":{"accessToken":"","expiresAt":0}}`, nil
		}
		return `{"mcpOAuth":{"plugin":"MCP_SECRET_SENTINEL"}}`, nil
	}
	loggedIn := false
	readClaudeAuthStatus = func(context.Context, string) (claudeAuthStatus, string) {
		return claudeAuthStatus{LoggedIn: &loggedIn, AuthMethod: "AUTH_SECRET_SENTINEL", APIProvider: "PROVIDER_SECRET_SENTINEL"}, ""
	}
	prependFakeClaude(t, "#!/usr/bin/env sh\nexit 99\n")
	var output strings.Builder
	for _, diag := range (Claude{}).DiagnoseAuth(context.Background()) {
		output.WriteString(diag.Name + " " + diag.Detail + "\n")
	}
	text := output.String()
	if strings.Contains(text, "SECRET_SENTINEL") {
		t.Fatal("diagnostics leaked credential or uncontrolled status values")
	}
	if !strings.Contains(text, "ignored (no usable access token)") || !strings.Contains(text, "loggedIn=false") || !strings.Contains(text, "Selected OAuth source CLI credential file") {
		t.Errorf("missing actionable diagnostics: %s", text)
	}
	after, err := os.ReadFile(config.CredentialsFile())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only diagnostics mutated orphan credentials")
	}
	if _, err := os.Stat(claudeRefreshDir()); !os.IsNotExist(err) {
		t.Fatal("diagnostics should not create coordination directory")
	}
}

func TestDescribeClaudeCredentialsDoesNotPrintInvalidExpiry(t *testing.T) {
	detail, _ := describeClaudeCredentials([]byte(`{"access_token":"ACCESS_SECRET_SENTINEL","expires_at":"EXPIRY_SECRET_SENTINEL"}`))
	if strings.Contains(detail, "SECRET_SENTINEL") || !strings.Contains(detail, "expiry invalid") {
		t.Fatal("invalid expiry must not be echoed")
	}
}

func TestCustomClaudeConfigExcludesDefaultKeychain(t *testing.T) {
	home := t.TempDir()
	setUserHome(t, home)
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	custom := filepath.Join(home, "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"custom-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := readKeychainSecret
	t.Cleanup(func() { readKeychainSecret = old })
	readKeychainSecret = func(string, string) (string, error) {
		t.Fatal("must not read another profile's Keychain")
		return "", nil
	}
	creds := (&OAuthStrategy{}).loadCredentials()
	if creds == nil || creds.AccessToken != "custom-token" {
		t.Fatal("custom CLI config not selected")
	}
	if path := (Claude{}).CredentialSources().CLIPaths[0]; path != filepath.Join(custom, ".credentials.json") {
		t.Fatal("generic diagnostics disagree with selected profile")
	}
}

func TestAuthOverridePreventsWrongChainRenewal(t *testing.T) {
	setUserHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "OVERRIDE_SECRET_SENTINEL")
	creds, outcome := (&OAuthStrategy{}).refreshViaCLI(context.Background())
	if creds != nil || outcome.Reason != oauth.CLIRefreshAuthOverride {
		t.Fatal("auth override should prevent CLI generation/renewal")
	}
	if _, err := os.Stat(claudeRefreshDir()); !os.IsNotExist(err) {
		t.Fatal("override path should not launch or create coordination state")
	}
}

func TestClaudeRefreshFailureSeparatesReauthFromTransientFailure(t *testing.T) {
	for _, hint := range []oauth.CLIRefreshHint{oauth.CLIHintUnknown, oauth.CLIHintNetwork, oauth.CLIHintRateLimited, oauth.CLIHintLoginRequired, oauth.CLIHintRefreshRejected} {
		result := claudeRefreshFailure(oauth.CLIRefreshOutcome{Reason: oauth.CLIRefreshFailed, Hint: hint})
		needsLogin := hint == oauth.CLIHintLoginRequired || hint == oauth.CLIHintRefreshRejected
		if strings.Contains(result.Error, "Sign in") != needsLogin || result.ShouldFallback == needsLogin {
			t.Errorf("wrong recovery policy for hint %s", hint)
		}
		if !needsLogin && result.RetryAfter == nil {
			t.Errorf("missing transient backoff for %s", hint)
		}
	}
}

func TestProbeHealthUsesControlledFetchNotGeneration(t *testing.T) {
	setUserHome(t, t.TempDir())
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	stubKeychainEmpty(t)
	if (&OAuthStrategy{}).ProbeHealth(context.Background()) {
		t.Fatal("without an OAuth chain, probe must not launch a Claude generation")
	}
}
