package oauth

import (
	"context"
	"os/exec"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/executil"
)

// CLIRefreshConfig holds parameters for a CLI-owned token refresh.
type CLIRefreshConfig struct {
	BinaryName      string
	Args            []string
	LoadCredentials func() *Credentials
	// Timeout bounds the subprocess; zero uses 15 seconds.
	Timeout time.Duration
	// CoordinationDir opts into cross-process exclusion and bounded failure
	// backoff. Callers sharing a CLI credential chain must use the same path.
	CoordinationDir string
}

// RefreshViaCLI is the compatibility API for callers that only need credentials.
func RefreshViaCLI(ctx context.Context, cfg CLIRefreshConfig) *Credentials {
	creds, _ := RefreshViaCLIWithOutcome(ctx, cfg)
	return creds
}

func runCLIRefresh(ctx context.Context, cfg CLIRefreshConfig) (creds *Credentials, outcome CLIRefreshOutcome) {
	started := time.Now()
	outcome = CLIRefreshOutcome{At: started.UTC(), ExitCode: -1, Hint: CLIHintUnknown}
	defer func() { outcome.DurationMillis = time.Since(started).Milliseconds() }()
	if ctx.Err() != nil {
		outcome.Reason = CLIRefreshCancelled
		return nil, outcome
	}
	if cfg.LoadCredentials == nil {
		outcome.Reason = CLIRefreshStateError
		return nil, outcome
	}
	initial := cfg.LoadCredentials()
	var initialToken string
	if initial != nil {
		initialToken = initial.AccessToken
	}
	fresh := func() *Credentials {
		c := cfg.LoadCredentials()
		if c == nil || c.AccessToken == "" || c.AccessToken == initialToken || c.NeedsRefresh() {
			return nil
		}
		return c
	}

	outcome.BinaryPath = executil.ResolveBinary(cfg.BinaryName)
	if outcome.BinaryPath == "" {
		outcome.Reason = CLIRefreshBinaryMissing
		return nil, outcome
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, outcome.BinaryPath, cfg.Args...)
	hints := &cliHints{}
	cmd.Stdin = nil
	cmd.Stdout = hints
	cmd.Stderr = hints
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		outcome.Reason = CLIRefreshStartFailed
		if ctx.Err() != nil {
			outcome.Reason = CLIRefreshCancelled
		}
		return nil, outcome
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finish := func(stop bool) {
		if stop {
			_ = cmd.Process.Kill()
		}
		<-done // Always reap, including the success/polling path.
		outcome.ExitCode = cmd.ProcessState.ExitCode()
		outcome.Hint = hints.category()
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c := fresh(); c != nil {
			finish(true)
			outcome.Reason = CLIRefreshOK
			return c, outcome
		}
		select {
		case err := <-done:
			outcome.ExitCode = cmd.ProcessState.ExitCode()
			outcome.Hint = hints.category()
			if c := fresh(); c != nil {
				outcome.Reason = CLIRefreshOK
				return c, outcome
			}
			outcome.Reason = CLIRefreshUnchanged
			if err != nil {
				outcome.Reason = CLIRefreshFailed
			}
			if tctx.Err() != nil {
				outcome.Reason = CLIRefreshTimeout
				if ctx.Err() != nil {
					outcome.Reason = CLIRefreshCancelled
				}
			}
			return nil, outcome
		case <-tctx.Done():
			finish(true)
			if c := fresh(); c != nil {
				outcome.Reason = CLIRefreshOK
				return c, outcome
			}
			outcome.Reason = CLIRefreshTimeout
			if ctx.Err() != nil {
				outcome.Reason = CLIRefreshCancelled
			}
			return nil, outcome
		case <-ticker.C:
		}
	}
}
