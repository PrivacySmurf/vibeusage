package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/auth/oauth"
	"github.com/joshuadavidthomas/vibeusage/internal/executil"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
)

// codexAuthFileMeta holds the non-secret auth.json fields diagnostics report.
type codexAuthFileMeta struct {
	AuthMode    string `json:"auth_mode"`
	LastRefresh string `json:"last_refresh"`
}

// describeCodexCredentials summarizes a Codex credential document without
// exposing token material. It returns the usable credentials, if any.
func describeCodexCredentials(data []byte) (string, *oauth.Credentials) {
	var cli CLICredentials
	var meta codexAuthFileMeta
	if json.Unmarshal(data, &cli) != nil || json.Unmarshal(data, &meta) != nil {
		return "invalid credential JSON", nil
	}
	mode := "unset"
	switch meta.AuthMode {
	case "":
	case "chatgpt", "apikey", "api_key":
		mode = meta.AuthMode
	default:
		mode = "other"
	}
	creds := withTokenExpiry(cli.EffectiveCredentials())
	if creds == nil {
		return "auth mode=" + mode + "; ignored (no usable access token)", nil
	}
	detail := fmt.Sprintf("auth mode=%s; access token present=true; refresh token present=%t", mode, creds.RefreshToken != "")
	if creds.ExpiresAt == "" {
		detail += "; expiry unknown"
	} else if expiry, err := time.Parse(time.RFC3339, creds.ExpiresAt); err == nil {
		detail += fmt.Sprintf("; expires=%s; expired=%t", expiry.UTC().Format(time.RFC3339), creds.IsExpired())
	} else {
		detail += "; expiry invalid"
	}
	if lastRefresh, err := time.Parse(time.RFC3339Nano, meta.LastRefresh); err == nil {
		detail += "; last refresh=" + lastRefresh.UTC().Format(time.RFC3339)
	}
	return detail, creds
}

type loginStatusOutput struct{ data []byte }

func (o *loginStatusOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 8192 - len(o.data); remaining > 0 {
		o.data = append(o.data, p[:min(remaining, n)]...)
	}
	return n, nil
}

// readCodexLoginStatus classifies `codex login status`; only the category
// escapes, never the raw output.
var readCodexLoginStatus = func(ctx context.Context, binary string) (provider.DiagnosticStatus, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "login", "status")
	output := &loginStatusOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = time.Second
	_ = cmd.Run()
	if ctx.Err() != nil {
		return provider.DiagWarn, "status command timed out or cancelled"
	}
	text := strings.ToLower(string(output.data))
	switch {
	case strings.Contains(text, "not logged in"):
		return provider.DiagFail, "not logged in; run `codex login`"
	case strings.Contains(text, "logged in using chatgpt"):
		return provider.DiagOK, "logged in with ChatGPT"
	case strings.Contains(text, "api key"):
		return provider.DiagWarn, "logged in with an API key; usage needs the ChatGPT login (`codex login`)"
	default:
		return provider.DiagWarn, "unrecognized status output"
	}
}

func codexAuthOverrides() []string {
	var names []string
	for _, name := range codexAuthOverrideNames {
		if os.Getenv(name) != "" {
			names = append(names, name)
		}
	}
	return names
}

// DiagnoseAuth never cleans orphan credentials, performs renewal, or writes
// coordination state. Raw credentials and subprocess output stay private.
func (c Codex) DiagnoseAuth(ctx context.Context) []provider.Diagnostic {
	home, _ := os.UserHomeDir()
	diags := []provider.Diagnostic{{Name: "CLI user environment", Status: provider.DiagInfo, Detail: "HOME=" + home + "; CODEX_HOME=" + codexHomeDir()}}
	if names := codexAuthOverrides(); len(names) > 0 {
		diags = append(diags, provider.Diagnostic{Name: "Auth environment overrides", Status: provider.DiagInfo, Detail: strings.Join(names, ", ") + " set; removed from the renewal subprocess so it renews the ChatGPT login"})
	}

	selected := ""
	s := &OAuthStrategy{}
	for _, path := range s.externalPaths() {
		diag := provider.Diagnostic{Name: "CLI credential file", Status: provider.DiagWarn, Detail: path + ": unavailable"}
		if data, err := os.ReadFile(path); err == nil {
			detail, creds := describeCodexCredentials(data)
			diag.Detail = path + ": " + detail
			if creds != nil {
				diag.Status = provider.DiagOK
				if selected == "" {
					selected = "CLI credential file; " + detail
				}
			}
		}
		diags = append(diags, diag)
	}
	keychainName := "Keychain (" + codexKeychainLabel + ", " + codexKeychainAccount() + ")"
	keychain := provider.Diagnostic{Name: keychainName, Status: provider.DiagInfo, Detail: "not present"}
	if secret, err := readKeychainSecret(codexKeychainLabel, codexKeychainAccount()); err == nil && secret != "" {
		detail, creds := describeCodexCredentials([]byte(secret))
		keychain.Detail = detail
		keychain.Status = provider.DiagWarn
		if creds != nil {
			keychain.Status = provider.DiagOK
			if selected == "" {
				selected = "Keychain; " + detail
			}
		}
	}
	diags = append(diags, keychain)

	selection := provider.Diagnostic{Name: "Selected OAuth source", Status: provider.DiagFail, Detail: "none; sign in with `codex login`"}
	if selected != "" {
		selection.Status = provider.DiagInfo
		selection.Detail = selected
	}
	diags = append(diags, selection)

	binary := executil.ResolveBinary("codex")
	if binary == "" {
		diags = append(diags, provider.Diagnostic{Name: "Codex CLI", Status: provider.DiagFail, Detail: "not found via PATH or common installation paths; renewal is unavailable"})
	} else {
		diags = append(diags, provider.Diagnostic{Name: "Codex CLI", Status: provider.DiagOK, Detail: binary})
		status, detail := readCodexLoginStatus(ctx, binary)
		diags = append(diags, provider.Diagnostic{Name: "CLI authentication", Status: status, Detail: detail})
	}

	dir := codexRefreshDir()
	diags = append(diags, provider.Diagnostic{Name: "Renewal coordination", Status: provider.DiagInfo, Detail: dir + "; shared per OS user with DSH Everything OAuth; one process at a time, 15s subprocess / 25s fetch budget; failure backoff 30s–5m"})
	outcome, err := oauth.ReadCLIRefreshOutcome(dir)
	diag := provider.Diagnostic{Name: "Last renewal attempt", Status: provider.DiagInfo, Detail: "none recorded"}
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
