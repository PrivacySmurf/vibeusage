package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/auth/oauth"
	"github.com/joshuadavidthomas/vibeusage/internal/executil"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

type claudeCredentialSource struct {
	name      string
	detail    string
	status    provider.DiagnosticStatus
	creds     *oauth.Credentials
	canonical bool
}

func describeClaudeCredentials(data []byte) (string, *oauth.Credentials) {
	var cli ClaudeCLICredentials
	var standard oauth.Credentials
	if json.Unmarshal(data, &cli) != nil || json.Unmarshal(data, &standard) != nil {
		return "invalid credential JSON", nil
	}
	creds := parseClaudeCredentials(data)
	if cli.ClaudeAiOauth != nil {
		standard = cli.ClaudeAiOauth.ToOAuthCredentials()
	}
	detail := fmt.Sprintf("access token present=%t; refresh token present=%t", standard.AccessToken != "", standard.RefreshToken != "")
	if standard.ExpiresAt == "" {
		detail += "; expiry unknown"
	} else if expiry, err := time.Parse(time.RFC3339, standard.ExpiresAt); err == nil {
		detail += "; expires=" + expiry.Format(time.RFC3339)
	} else {
		detail += "; expiry invalid"
	}
	if creds != nil {
		detail += fmt.Sprintf("; expired=%t", creds.IsExpired())
	} else {
		detail += "; ignored (no usable access token)"
	}
	return detail, creds
}

func (s *OAuthStrategy) readCredentialSources() []claudeCredentialSource {
	var sources []claudeCredentialSource
	for _, path := range s.externalPaths() {
		source := claudeCredentialSource{name: "CLI credential file", status: provider.DiagWarn, detail: path + ": unavailable"}
		if data, err := os.ReadFile(path); err == nil {
			source.detail, source.creds = describeClaudeCredentials(data)
			source.detail = path + ": " + source.detail
			if source.creds != nil {
				source.status = provider.DiagOK
			}
		}
		sources = append(sources, source)
	}
	if !usesDefaultClaudeConfig() {
		return append(sources, claudeCredentialSource{name: "Keychain", status: provider.DiagInfo, detail: "custom CLAUDE_CONFIG_DIR: default-profile Keychain deliberately excluded"})
	}
	accounts := []string{}
	if username, err := currentUsername(); err == nil && username != "" {
		accounts = append(accounts, username)
	}
	accounts = append(accounts, "")
	for _, account := range accounts {
		name := "Keychain (current user)"
		if account == "" {
			name = "Keychain (legacy unscoped)"
		}
		source := claudeCredentialSource{name: name, status: provider.DiagWarn, detail: "unavailable", canonical: account != ""}
		if secret, err := readKeychainSecret(claudeKeychainSecret, account); err == nil && secret != "" {
			source.detail, source.creds = describeClaudeCredentials([]byte(secret))
			if source.creds != nil {
				source.status = provider.DiagOK
			}
		}
		sources = append(sources, source)
		if source.creds != nil {
			break
		} // Match the CLI-source fallback contract.
	}
	return sources
}

func selectClaudeCredential(sources []claudeCredentialSource) *claudeCredentialSource {
	var best *claudeCredentialSource
	for i := range sources {
		candidate := &sources[i]
		if candidate.creds == nil {
			continue
		}
		// The scoped macOS Keychain record is the current CLI-owned chain.
		// A stale file's optimistic expiry must not mask its renewal writes.
		if candidate.canonical {
			return candidate
		}
		if best == nil || (best.creds.IsExpired() && !candidate.creds.IsExpired()) {
			best = candidate
			continue
		}
		if !best.creds.IsExpired() && candidate.creds.IsExpired() {
			continue
		}
		if candidate.creds.ExpiresAt != "" && (best.creds.ExpiresAt == "" || candidate.creds.ExpiresAt > best.creds.ExpiresAt) {
			best = candidate
		}
	}
	return best
}

type claudeAuthStatus struct {
	LoggedIn    *bool  `json:"loggedIn"`
	AuthMethod  string `json:"authMethod"`
	APIProvider string `json:"apiProvider"`
}

type authStatusOutput struct{ data []byte }

func (o *authStatusOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 8192 - len(o.data)
	if remaining > 0 {
		o.data = append(o.data, p[:min(remaining, n)]...)
	}
	return n, nil
}

var readClaudeAuthStatus = func(ctx context.Context, binary string) (claudeAuthStatus, string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "auth", "status", "--json")
	output := &authStatusOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return claudeAuthStatus{}, "status command timed out or cancelled"
	}
	var status claudeAuthStatus
	// Some CLI versions exit nonzero when logged out but still emit status JSON.
	if json.Unmarshal(output.data, &status) == nil && status.LoggedIn != nil {
		return status, ""
	}
	if err != nil {
		return status, "status command failed (no valid status JSON)"
	}
	return status, "unrecognized status JSON"
}

var claudeAuthOverrideNames = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

func claudeAuthOverrides() []string {
	var names []string
	for _, name := range claudeAuthOverrideNames {
		if os.Getenv(name) != "" {
			names = append(names, name)
		}
	}
	return names
}

// DiagnoseAuth never cleans orphan credentials, performs generation/renewal, or
// writes coordination state. Raw credentials and subprocess output stay private.
func (c Claude) DiagnoseAuth(ctx context.Context) []provider.Diagnostic {
	home, _ := os.UserHomeDir()
	diags := []provider.Diagnostic{{Name: "CLI user environment", Status: provider.DiagInfo, Detail: "HOME=" + home + "; config=" + claudeConfigDir()}}
	if names := claudeAuthOverrides(); len(names) > 0 {
		diags = append(diags, provider.Diagnostic{Name: "Auth environment overrides", Status: provider.DiagWarn, Detail: strings.Join(names, ", ") + " set; CLI renewal is disabled to avoid refreshing another credential chain"})
	}
	s := &OAuthStrategy{}
	sources := s.readCredentialSources()
	for _, source := range sources {
		diags = append(diags, provider.Diagnostic{Name: source.name, Status: source.status, Detail: source.detail})
	}
	selected := selectClaudeCredential(sources)
	selection := provider.Diagnostic{Name: "Selected OAuth source", Status: provider.DiagFail, Detail: "none; sign in with 'claude auth login'"}
	if selected != nil {
		selection.Status = provider.DiagInfo
		selection.Detail = selected.name + "; " + selected.detail
	}
	diags = append(diags, selection)
	binary := executil.ResolveBinary("claude")
	if binary == "" {
		diags = append(diags, provider.Diagnostic{Name: "Claude CLI", Status: provider.DiagFail, Detail: "not found via PATH or common installation paths"})
	} else {
		diags = append(diags, provider.Diagnostic{Name: "Claude CLI", Status: provider.DiagOK, Detail: binary})
		status, statusError := readClaudeAuthStatus(ctx, binary)
		diag := provider.Diagnostic{Name: "CLI authentication", Status: provider.DiagWarn, Detail: statusError}
		if statusError == "" {
			method := "other"
			switch status.AuthMethod {
			case "none", "oauth", "api_key":
				method = status.AuthMethod
			}
			apiProvider := "other"
			if status.APIProvider == "firstParty" {
				apiProvider = "firstParty"
			}
			diag.Detail = fmt.Sprintf("loggedIn=%t; authMethod=%s; apiProvider=%s", *status.LoggedIn, method, apiProvider)
			if *status.LoggedIn {
				diag.Status = provider.DiagOK
			} else {
				diag.Status = provider.DiagFail
				diag.Detail += "; run 'claude auth login'"
			}
		}
		diags = append(diags, diag)
	}
	dir := claudeRefreshDir()
	diags = append(diags, provider.Diagnostic{Name: "Renewal coordination", Status: provider.DiagInfo, Detail: dir + "; shared per CLI user; one process at a time, 15s subprocess / 25s fetch budget; failure backoff 30s–5m"})
	outcome, err := oauth.ReadCLIRefreshOutcome(dir)
	diag := provider.Diagnostic{Name: "Last renewal attempt", Status: provider.DiagInfo, Detail: "none recorded; natural-expiry renewal is not yet verified"}
	if err != nil {
		diag.Status = provider.DiagWarn
		diag.Detail = "coordination state unreadable or invalid"
	} else if !outcome.At.IsZero() {
		diag.Detail = fmt.Sprintf("%s: %s; CLI hint=%s; exit=%d; duration=%dms", outcome.At.Format(time.RFC3339), outcome.Reason, outcome.Hint, outcome.ExitCode, outcome.DurationMillis)
		if outcome.RetryAt.After(time.Now()) {
			diag.Detail += "; retry after=" + outcome.RetryAt.Format(time.RFC3339)
		}
		if outcome.Reason != oauth.CLIRefreshOK {
			diag.Status = provider.DiagWarn
		}
	}
	return append(diags, diag)
}
