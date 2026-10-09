package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var errRefreshLockBusy = errors.New("CLI refresh lock is busy")

type cliRefreshState struct {
	Outcome             CLIRefreshOutcome `json:"outcome"`
	Failures            int               `json:"failures"`
	CredentialExpiresAt string            `json:"credential_expires_at,omitempty"`
}

// ReadCLIRefreshOutcome is read-only; it does not create files or acquire locks.
func ReadCLIRefreshOutcome(dir string) (CLIRefreshOutcome, error) {
	state, err := readCLIRefreshState(dir)
	return state.Outcome, err
}

func readCLIRefreshState(dir string) (cliRefreshState, error) {
	var state cliRefreshState
	data, err := os.ReadFile(filepath.Join(dir, "last-attempt.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func writeCLIRefreshState(dir string, state cliRefreshState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".attempt-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, "last-attempt.json"))
}

func safeCredentialExpiry(creds *Credentials) string {
	if creds == nil || creds.ExpiresAt == "" {
		return ""
	}
	expiry, err := time.Parse(time.RFC3339, creds.ExpiresAt)
	if err != nil {
		return "invalid"
	}
	return expiry.UTC().Format(time.RFC3339)
}

func refreshBackoff(failures int) time.Duration {
	failures = max(1, min(failures, 5))
	return min(30*time.Second*time.Duration(1<<(failures-1)), 5*time.Minute)
}

func coordinatedCLIRefresh(ctx context.Context, cfg CLIRefreshConfig) (*Credentials, CLIRefreshOutcome) {
	outcome := CLIRefreshOutcome{At: time.Now().UTC(), ExitCode: -1, Hint: CLIHintUnknown}
	if ctx.Err() != nil {
		outcome.Reason = CLIRefreshCancelled
		return nil, outcome
	}
	if cfg.LoadCredentials == nil {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	if err := os.MkdirAll(cfg.CoordinationDir, 0o700); err != nil {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	path := filepath.Join(cfg.CoordinationDir, "refresh.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	defer func() { _ = file.Close() }()
	if err = tryRefreshLock(file); err != nil {
		outcome.Reason = CLIRefreshStateError
		if errors.Is(err, errRefreshLockBusy) {
			outcome.Reason = CLIRefreshBusy
			outcome.RetryAt = time.Now().UTC().Add(5 * time.Second)
		}
		return nil, outcome
	}
	defer func() { _ = unlockRefreshFile(file) }()
	state, err := readCLIRefreshState(cfg.CoordinationDir)
	if err != nil {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	initial := cfg.LoadCredentials()
	expiresAt := safeCredentialExpiry(initial)
	if state.CredentialExpiresAt != expiresAt {
		state = cliRefreshState{}
	}
	if state.Outcome.RetryAt.After(time.Now()) {
		outcome.Reason = CLIRefreshCooldown
		outcome.Hint = state.Outcome.Hint
		outcome.LastReason = state.Outcome.Reason
		outcome.RetryAt = state.Outcome.RetryAt
		return nil, outcome
	}
	// Record a bounded recovery delay before launch. If this process crashes,
	// the OS releases the lock but another caller will not immediately retry.
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	pending := cliRefreshState{Failures: state.Failures, CredentialExpiresAt: expiresAt, Outcome: outcome}
	pending.Outcome.Reason = CLIRefreshInterrupted
	pending.Outcome.RetryAt = time.Now().UTC().Add(timeout + refreshBackoff(state.Failures+1))
	if err = writeCLIRefreshState(cfg.CoordinationDir, pending); err != nil {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	creds, outcome := runCLIRefresh(ctx, cfg)
	state.Outcome = outcome
	state.CredentialExpiresAt = expiresAt
	if creds != nil {
		state.Failures = 0
		state.CredentialExpiresAt = safeCredentialExpiry(creds)
	} else {
		state.Failures = min(state.Failures+1, 5)
		state.Outcome.RetryAt = time.Now().UTC().Add(refreshBackoff(state.Failures))
		outcome.RetryAt = state.Outcome.RetryAt
	}
	if err = writeCLIRefreshState(cfg.CoordinationDir, state); err != nil {
		// Never discard credentials already refreshed by their owner, but make
		// the coordination failure observable and fail closed next invocation.
		outcome.Reason = CLIRefreshStateError
	}
	return creds, outcome
}
