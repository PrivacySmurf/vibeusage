package oauth

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCLIRefreshOutcomes(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		reason   CLIRefreshReason
		hint     CLIRefreshHint
		exit     int
	}{
		{"fresh", CLIRefreshOK, CLIHintUnknown, 0},
		{"success", CLIRefreshUnchanged, CLIHintUnknown, 0},
		{"login-required", CLIRefreshFailed, CLIHintLoginRequired, 7},
		{"network-error", CLIRefreshFailed, CLIHintNetwork, 8},
		{"rate-limit", CLIRefreshFailed, CLIHintRateLimited, 9},
		{"rejected", CLIRefreshFailed, CLIHintRefreshRejected, 10},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			_, credPath := setupFakeCLI(t, tc.scenario)
			creds, outcome := RefreshViaCLIWithOutcome(context.Background(), CLIRefreshConfig{BinaryName: "testcli", Args: []string{"refresh", "ARG_SECRET_SENTINEL"}, LoadCredentials: credLoader(credPath)})
			if outcome.Reason != tc.reason || outcome.Hint != tc.hint {
				t.Fatalf("outcome = %+v", outcome)
			}
			// A successful polling path may deliberately kill the process after
			// its write, so only failed/no-change exit codes are constrained.
			if tc.reason != CLIRefreshOK && outcome.ExitCode != tc.exit {
				t.Errorf("exit = %d, want %d", outcome.ExitCode, tc.exit)
			}
			if (creds != nil) != (tc.reason == CLIRefreshOK) {
				t.Fatal("credentials do not match outcome")
			}
			data, err := json.Marshal(outcome)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "SECRET_SENTINEL") {
				t.Fatal("outcome leaked CLI output or arguments")
			}
		})
	}
}

func TestCLIRefreshMissingStartTimeoutAndCancellation(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, outcome := RefreshViaCLIWithOutcome(context.Background(), CLIRefreshConfig{BinaryName: "definitely-nonexistent-cli", LoadCredentials: func() *Credentials { return nil }})
		if outcome.Reason != CLIRefreshBinaryMissing {
			t.Fatal(outcome)
		}
	})
	t.Run("start", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("invalid executable fixture is Unix-specific")
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "invalid-cli"), []byte("not an executable"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		_, outcome := RefreshViaCLIWithOutcome(context.Background(), CLIRefreshConfig{BinaryName: "invalid-cli", LoadCredentials: func() *Credentials { return nil }})
		if outcome.Reason != CLIRefreshStartFailed {
			t.Fatal(outcome)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		_, credPath := setupFakeCLI(t, "hang")
		_, outcome := RefreshViaCLIWithOutcome(context.Background(), CLIRefreshConfig{BinaryName: "testcli", Timeout: 100 * time.Millisecond, LoadCredentials: credLoader(credPath)})
		if outcome.Reason != CLIRefreshTimeout {
			t.Fatal(outcome)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, outcome := RefreshViaCLIWithOutcome(ctx, CLIRefreshConfig{})
		if outcome.Reason != CLIRefreshCancelled {
			t.Fatal(outcome)
		}
	})
}

func TestCLIRefreshCoordinationPersistsBackoffWithoutSecrets(t *testing.T) {
	_, credPath := setupFakeCLI(t, "login-required")
	dir := t.TempDir()
	initial := `{"access_token":"ACCESS_SECRET_SENTINEL","refresh_token":"REFRESH_SECRET_SENTINEL","expires_at":"2020-01-01T00:00:00Z"}`
	if err := os.WriteFile(credPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := CLIRefreshConfig{BinaryName: "testcli", LoadCredentials: credLoader(credPath), CoordinationDir: dir}
	_, first := RefreshViaCLIWithOutcome(context.Background(), cfg)
	if first.Reason != CLIRefreshFailed || first.RetryAt.IsZero() {
		t.Fatal(first)
	}
	_, second := RefreshViaCLIWithOutcome(context.Background(), cfg)
	if second.Reason != CLIRefreshCooldown || second.LastReason != first.Reason || !second.RetryAt.Equal(first.RetryAt) {
		t.Fatal(second)
	}
	data, err := os.ReadFile(filepath.Join(dir, "last-attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET_SENTINEL") {
		t.Fatal("coordination state contains secret values/output")
	}
	// Read-only diagnostics must not change the attempt.
	last, err := ReadCLIRefreshOutcome(dir)
	if err != nil || last.Reason != first.Reason {
		t.Fatalf("read outcome: %+v, %v", last, err)
	}
	info, err := os.Stat(filepath.Join(dir, "last-attempt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %o", info.Mode().Perm())
	}
	// A fresh CLI login with changed expiry must not inherit old backoff.
	if err := os.WriteFile(credPath, []byte(`{"access_token":"new-login","expires_at":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cliRefreshScenarioEnv, "success")
	_, third := RefreshViaCLIWithOutcome(context.Background(), cfg)
	if third.Reason != CLIRefreshUnchanged {
		t.Fatalf("new login should bypass prior cooldown: %+v", third)
	}
}

func TestCLIRefreshLockIsCrossProcessAndNonBlocking(t *testing.T) {
	binDir, _ := setupFakeCLI(t, "hold-lock")
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "refresh.lock")
	binary := filepath.Join(binDir, "testcli")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(), cliRefreshCredentialsEnv+"="+lockPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(lockPath + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire OS lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	_, outcome := RefreshViaCLIWithOutcome(context.Background(), CLIRefreshConfig{BinaryName: "testcli", CoordinationDir: dir, LoadCredentials: func() *Credentials { t.Error("busy caller should not load credentials or launch CLI"); return nil }})
	if outcome.Reason != CLIRefreshBusy || time.Since(started) > time.Second {
		t.Fatalf("expected prompt cross-process contention: %+v", outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "last-attempt.json")); !os.IsNotExist(err) {
		t.Fatal("contending caller should not overwrite owner state")
	}
}

func TestCLIRefreshCorruptStateFailsClosedAndBackoffIsCapped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "last-attempt.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, outcome := RefreshViaCLIWithOutcome(context.Background(), CLIRefreshConfig{CoordinationDir: dir, LoadCredentials: func() *Credentials { return nil }})
	if outcome.Reason != CLIRefreshStateError {
		t.Fatal(outcome)
	}
	for i := 1; i < 20; i++ {
		if delay := refreshBackoff(i); delay < 30*time.Second || delay > 5*time.Minute {
			t.Fatalf("backoff(%d) = %v", i, delay)
		}
	}
}

func TestCLIHintsBoundedSplitInput(t *testing.T) {
	hints := &cliHints{}
	_, _ = hints.Write([]byte(strings.Repeat("SECRET_SENTINEL", 10000) + "please run /lo"))
	_, _ = hints.Write([]byte("gin"))
	if hints.category() != CLIHintLoginRequired || len(hints.tail) > 128 {
		t.Fatal("stream classifier failed split marker or memory bound")
	}
}
