package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	cliRefreshHelperEnv      = "GO_WANT_CLI_REFRESH_HELPER"
	cliRefreshScenarioEnv    = "CLI_REFRESH_HELPER_SCENARIO"
	cliRefreshCredentialsEnv = "CRED_PATH"
)

func TestMain(m *testing.M) {
	if os.Getenv(cliRefreshHelperEnv) == "1" {
		os.Exit(runCLIRefreshHelper(os.Getenv(cliRefreshScenarioEnv), os.Getenv(cliRefreshCredentialsEnv)))
	}
	os.Exit(m.Run())
}

func TestRefreshViaCLI_ReturnsFreshCredentials(t *testing.T) {
	binDir, credPath := setupFakeCLI(t, "fresh")

	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName:      "testcli",
		Args:            []string{"refresh"},
		LoadCredentials: credLoader(credPath),
	})
	_ = binDir
	if got == nil {
		t.Fatal("RefreshViaCLI() = nil, want credentials")
	}
	if got.AccessToken != "fresh" {
		t.Errorf("access_token = %q, want %q", got.AccessToken, "fresh")
	}
}

func TestRefreshViaCLI_ReturnsQuicklyWhenCLIHangs(t *testing.T) {
	binDir, credPath := setupFakeCLI(t, "write-then-hang")

	start := time.Now()
	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName:      "testcli",
		Args:            []string{"refresh"},
		LoadCredentials: credLoader(credPath),
	})
	elapsed := time.Since(start)
	_ = binDir

	if got == nil {
		t.Fatal("RefreshViaCLI() = nil, want credentials")
	}
	if elapsed >= 2*time.Second {
		t.Errorf("took %v, want < 2s", elapsed)
	}
}

func TestRefreshViaCLI_ReturnsNilWhenCredentialsStayExpired(t *testing.T) {
	binDir, _ := setupFakeCLI(t, "success")

	expired := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName: "testcli",
		Args:       []string{"refresh"},
		LoadCredentials: func() *Credentials {
			return &Credentials{
				AccessToken:  "stale",
				RefreshToken: "ref",
				ExpiresAt:    expired,
			}
		},
	})
	_ = binDir

	if got != nil {
		t.Errorf("RefreshViaCLI() = %+v, want nil", got)
	}
}

func TestRefreshViaCLI_ReturnsNilWhenBinaryNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName: "nonexistent-cli",
		Args:       []string{"refresh"},
		LoadCredentials: func() *Credentials {
			return nil
		},
	})

	if got != nil {
		t.Errorf("RefreshViaCLI() = %+v, want nil", got)
	}
}

func TestRefreshViaCLI_RespectsContextCancellation(t *testing.T) {
	binDir, _ := setupFakeCLI(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	got := RefreshViaCLI(ctx, CLIRefreshConfig{
		BinaryName: "testcli",
		Args:       []string{"refresh"},
		LoadCredentials: func() *Credentials {
			return nil
		},
	})
	elapsed := time.Since(start)
	_ = binDir

	if got != nil {
		t.Errorf("RefreshViaCLI() = %+v, want nil", got)
	}
	if elapsed >= 1*time.Second {
		t.Errorf("took %v, want fast return on cancelled context", elapsed)
	}
}

func TestRefreshViaCLI_DoesNotShortCircuitWhenCredsHaveNoExpiry(t *testing.T) {
	// Regression: providers like Codex store creds without expires_at, so
	// NeedsRefresh() returns false even for stale tokens. RefreshViaCLI must
	// not accept the pre-refresh creds on the first poll just because they
	// look "non-expiring".
	binDir, credPath := setupFakeCLI(t, "delayed-no-expiry")

	// Pre-write the existing creds file with the *old* token (no expires_at).
	initial := `{"access_token":"stale","refresh_token":"oldref"}`
	if err := os.WriteFile(credPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName:      "testcli",
		Args:            []string{"refresh"},
		LoadCredentials: credLoader(credPath),
	})
	_ = binDir

	if got == nil {
		t.Fatal("RefreshViaCLI() = nil, want refreshed creds")
	}
	if got.AccessToken != "refreshed" {
		t.Errorf("access_token = %q, want %q (must wait for CLI to write new token)", got.AccessToken, "refreshed")
	}
}

func TestRefreshViaCLI_RejectsChangedButExpiredToken(t *testing.T) {
	// A changed access token still has to satisfy !NeedsRefresh(). If the
	// CLI rotates to a token that's already expired, treat it as a failed
	// refresh — we don't want the caller to immediately re-enter the
	// refresh path.
	binDir, credPath := setupFakeCLI(t, "delayed-expired")

	initial := `{"access_token":"stale","refresh_token":"r","expires_at":"2020-01-01T00:00:00Z"}`
	if err := os.WriteFile(credPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName:      "testcli",
		Args:            []string{"refresh"},
		LoadCredentials: credLoader(credPath),
	})
	_ = binDir

	if got != nil {
		t.Errorf("RefreshViaCLI() = %+v, want nil for a token that was rotated to an already-expired value", got)
	}
}

func TestRefreshViaCLI_ReturnsNilWhenCLIDoesNotChangeToken(t *testing.T) {
	// If the CLI exits without rotating the token, refresh has effectively
	// failed — we must return nil rather than the unchanged creds.
	binDir, credPath := setupFakeCLI(t, "success")

	initial := `{"access_token":"unchanged","refresh_token":"ref"}`
	if err := os.WriteFile(credPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName:      "testcli",
		Args:            []string{"refresh"},
		LoadCredentials: credLoader(credPath),
	})
	_ = binDir

	if got != nil {
		t.Errorf("RefreshViaCLI() = %+v, want nil when token did not change", got)
	}
}

func TestRefreshViaCLI_PollsMultipleTimes(t *testing.T) {
	binDir, credPath := setupFakeCLI(t, "polling")

	var calls atomic.Int32
	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName: "testcli",
		Args:       []string{"refresh"},
		LoadCredentials: func() *Credentials {
			calls.Add(1)
			return credLoader(credPath)()
		},
	})
	_ = binDir

	if got == nil {
		t.Fatal("RefreshViaCLI() = nil, want credentials")
	}
	if got.AccessToken != "delayed" {
		t.Errorf("access_token = %q, want %q", got.AccessToken, "delayed")
	}
	if n := calls.Load(); n < 2 {
		t.Errorf("LoadCredentials called %d times, want >= 2 (proving polling)", n)
	}
}

// setupFakeCLI copies the current test binary into PATH as "testcli". TestMain
// detects the helper environment and runs the requested CLI behavior in Go.
func setupFakeCLI(t *testing.T, scenario string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(dir, "creds.json")

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binaryName := "testcli"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binPath := filepath.Join(binDir, binaryName)
	copyTestBinary(t, testBinary, binPath)

	// A killed executable can remain locked for a moment on Windows. Remove it
	// before TempDir's cleanup runs so the test does not race process teardown.
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.Remove(binPath)
			if err == nil || os.IsNotExist(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove helper executable: %v", err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Setenv(cliRefreshHelperEnv, "1")
	t.Setenv(cliRefreshScenarioEnv, scenario)
	t.Setenv(cliRefreshCredentialsEnv, credPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return binDir, credPath
}

func copyTestBinary(t *testing.T, sourcePath, targetPath string) {
	t.Helper()
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatalf("open test binary: %v", err)
	}
	defer func() { _ = source.Close() }()

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create helper executable: %v", err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatalf("copy helper executable: %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("close helper executable: %v", err)
	}
	if err := os.Chmod(targetPath, 0o755); err != nil {
		t.Fatalf("chmod helper executable: %v", err)
	}
}

func runCLIRefreshHelper(scenario, credPath string) int {
	writeCredentials := func(body string) int {
		if credPath == "" {
			_, _ = fmt.Fprintln(os.Stderr, "credential path is empty")
			return 2
		}
		if err := os.MkdirAll(filepath.Dir(credPath), 0o755); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "create credential directory: %v\n", err)
			return 2
		}
		if err := os.WriteFile(credPath, []byte(body), 0o600); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "write credentials: %v\n", err)
			return 2
		}
		return 0
	}

	switch scenario {
	case "fresh":
		return writeCredentials(`{"access_token":"fresh","refresh_token":"ref","expires_at":"2099-01-01T00:00:00Z"}`)
	case "write-then-hang":
		if code := writeCredentials(`{"access_token":"fresh","refresh_token":"ref","expires_at":"2099-01-01T00:00:00Z"}`); code != 0 {
			return code
		}
		time.Sleep(30 * time.Second)
		return 0
	case "login-required":
		_, _ = fmt.Fprintln(os.Stderr, "SECRET_SENTINEL access_token=SECRET_SENTINEL: Not logged in. Please run /login")
		return 7
	case "network-error":
		_, _ = fmt.Fprintln(os.Stderr, "SECRET_SENTINEL network error ECONNREFUSED")
		return 8
	case "rate-limit":
		_, _ = fmt.Fprintln(os.Stdout, "SECRET_SENTINEL too many requests")
		return 9
	case "rejected":
		_, _ = fmt.Fprintln(os.Stderr, "SECRET_SENTINEL invalid refresh token")
		return 10
	case "codex-reused":
		_, _ = fmt.Fprintln(os.Stderr, "SECRET_SENTINEL Error: unexpected status 401 Unauthorized: Your access token could not be refreshed because your refresh token was already used. Please log out and sign in again.")
		return 1
	case "codex-logged-out":
		_, _ = fmt.Fprintln(os.Stderr, "SECRET_SENTINEL Your access token could not be refreshed because you have since logged out or signed in to another account.")
		return 1
	case "codex-revoked":
		_, _ = fmt.Fprintln(os.Stderr, `SECRET_SENTINEL {"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"}}`)
		return 1
	case "codex-login":
		_, _ = fmt.Fprintln(os.Stderr, "SECRET_SENTINEL Authentication is required; run `codex login`.")
		return 1
	case "report-env":
		cwd, err := os.Getwd()
		if err != nil {
			return 2
		}
		report := fmt.Sprintf("cwd=%s\nOPENAI_API_KEY=%s\nCODEX_API_KEY=%s\nKEEP_ME=%s\n", cwd, os.Getenv("OPENAI_API_KEY"), os.Getenv("CODEX_API_KEY"), os.Getenv("KEEP_ME"))
		if err := os.WriteFile(credPath+".env", []byte(report), 0o600); err != nil {
			return 2
		}
		return writeCredentials(`{"access_token":"fresh","refresh_token":"ref","expires_at":"2099-01-01T00:00:00Z"}`)
	case "hold-lock":
		file, err := os.OpenFile(credPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return 2
		}
		defer func() { _ = file.Close() }()
		if tryRefreshLock(file) != nil {
			return 3
		}
		defer func() { _ = unlockRefreshFile(file) }()
		if os.WriteFile(credPath+".ready", []byte("ready"), 0o600) != nil {
			return 4
		}
		time.Sleep(30 * time.Second)
		return 0
	case "success":
		return 0
	case "hang":
		time.Sleep(30 * time.Second)
		return 0
	case "delayed-no-expiry":
		time.Sleep(50 * time.Millisecond)
		return writeCredentials(`{"access_token":"refreshed","refresh_token":"newref"}`)
	case "delayed-expired":
		time.Sleep(50 * time.Millisecond)
		return writeCredentials(`{"access_token":"changed","refresh_token":"r","expires_at":"2020-01-01T00:00:00Z"}`)
	case "polling":
		time.Sleep(100 * time.Millisecond)
		return writeCredentials(`{"access_token":"delayed","refresh_token":"ref","expires_at":"2099-01-01T00:00:00Z"}`)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown CLI refresh helper scenario %q\n", scenario)
		return 2
	}
}

// credLoader returns a LoadCredentials function that reads from a JSON file.
func credLoader(path string) func() *Credentials {
	return func() *Credentials {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var creds Credentials
		if err := json.Unmarshal(data, &creds); err != nil {
			return nil
		}
		if creds.AccessToken == "" {
			return nil
		}
		return &creds
	}
}

func TestRefreshViaCLI_UnsetEnvAndDir(t *testing.T) {
	_, credPath := setupFakeCLI(t, "report-env")
	t.Setenv("OPENAI_API_KEY", "OPENAI_SENTINEL")
	t.Setenv("CODEX_API_KEY", "CODEX_SENTINEL")
	t.Setenv("KEEP_ME", "kept")
	workDir := t.TempDir()

	got := RefreshViaCLI(context.Background(), CLIRefreshConfig{
		BinaryName:      "testcli",
		Args:            []string{"refresh"},
		LoadCredentials: credLoader(credPath),
		UnsetEnv:        []string{"OPENAI_API_KEY", "CODEX_API_KEY"},
		Dir:             workDir,
	})
	if got == nil {
		t.Fatal("RefreshViaCLI() = nil, want credentials")
	}
	// The poller may stop the helper right after its credential write, but the
	// report is written first.
	report, err := os.ReadFile(credPath + ".env")
	if err != nil {
		t.Fatalf("read env report: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(report)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		lines[name] = value
	}
	if gotDir, _ := filepath.EvalSymlinks(lines["cwd"]); gotDir != wantDir {
		t.Errorf("cwd = %q, want %q", lines["cwd"], wantDir)
	}
	if lines["OPENAI_API_KEY"] != "" || lines["CODEX_API_KEY"] != "" {
		t.Errorf("auth overrides leaked into subprocess: %q", report)
	}
	if lines["KEEP_ME"] != "kept" {
		t.Errorf("KEEP_ME = %q, want kept (only named variables are removed)", lines["KEEP_ME"])
	}
}
