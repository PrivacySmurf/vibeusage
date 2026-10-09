package oauth

import (
	"context"
	"strings"
	"sync"
	"time"
)

// CLIRefreshReason describes the mechanism, not whether a refresh token was
// revoked. Only a recognized CLI hint can provide evidence of that distinction.
type CLIRefreshReason string

const (
	CLIRefreshOK            CLIRefreshReason = "refreshed"
	CLIRefreshBinaryMissing CLIRefreshReason = "binary_missing"
	CLIRefreshStartFailed   CLIRefreshReason = "start_failed"
	CLIRefreshFailed        CLIRefreshReason = "cli_failed"
	CLIRefreshUnchanged     CLIRefreshReason = "credentials_unchanged"
	CLIRefreshTimeout       CLIRefreshReason = "timeout"
	CLIRefreshCancelled     CLIRefreshReason = "cancelled"
	CLIRefreshBusy          CLIRefreshReason = "refresh_in_progress"
	CLIRefreshCooldown      CLIRefreshReason = "cooldown"
	CLIRefreshStateError    CLIRefreshReason = "coordination_state_error"
	CLIRefreshInterrupted   CLIRefreshReason = "interrupted"
	CLIRefreshAuthOverride  CLIRefreshReason = "auth_environment_override"
)

type CLIRefreshHint string

const (
	CLIHintUnknown         CLIRefreshHint = "unclassified"
	CLIHintLoginRequired   CLIRefreshHint = "login_required"
	CLIHintRefreshRejected CLIRefreshHint = "refresh_rejected"
	CLIHintRateLimited     CLIRefreshHint = "rate_limited"
	CLIHintNetwork         CLIRefreshHint = "network_error"
)

// CLIRefreshOutcome is safe to persist or log. It never contains credentials,
// arguments, raw subprocess output, or error strings from the subprocess.
type CLIRefreshOutcome struct {
	At             time.Time        `json:"at"`
	Reason         CLIRefreshReason `json:"reason"`
	Hint           CLIRefreshHint   `json:"hint"`
	BinaryPath     string           `json:"binary_path,omitempty"`
	ExitCode       int              `json:"exit_code"` // -1 when no normal exit was observed
	DurationMillis int64            `json:"duration_ms"`
	RetryAt        time.Time        `json:"retry_at,omitempty"`
	LastReason     CLIRefreshReason `json:"last_reason,omitempty"`
}

// RefreshViaCLIWithOutcome preserves the CLI's exclusive ownership of the
// rotating token chain and returns an actionable, secret-free failure reason.
func RefreshViaCLIWithOutcome(ctx context.Context, cfg CLIRefreshConfig) (*Credentials, CLIRefreshOutcome) {
	if cfg.CoordinationDir != "" {
		return coordinatedCLIRefresh(ctx, cfg)
	}
	return runCLIRefresh(ctx, cfg)
}

// cliHints is a bounded streaming classifier, not a captured transcript. Only
// allowlisted categories escape this object; even unknown output is discarded.
type cliHints struct {
	mu   sync.Mutex
	tail string
	hint CLIRefreshHint
}

func (h *cliHints) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		size := min(len(p), 1024)
		text := h.tail + strings.ToLower(string(p[:size]))
		p = p[size:]
		switch {
		case strings.Contains(text, "invalid refresh token"), strings.Contains(text, "refresh token has been revoked"), strings.Contains(text, "refresh token is invalid"), strings.Contains(text, "refresh token expired"):
			h.hint = CLIHintRefreshRejected
		case strings.Contains(text, "not logged in"), strings.Contains(text, "please run /login"), strings.Contains(text, "please run claude auth login"), strings.Contains(text, "login required"):
			if h.hint != CLIHintRefreshRejected {
				h.hint = CLIHintLoginRequired
			}
		case strings.Contains(text, "rate limit"), strings.Contains(text, "too many requests"):
			if h.hint == "" || h.hint == CLIHintNetwork {
				h.hint = CLIHintRateLimited
			}
		case strings.Contains(text, "econnrefused"), strings.Contains(text, "enotfound"), strings.Contains(text, "network error"):
			if h.hint == "" {
				h.hint = CLIHintNetwork
			}
		}
		h.tail = text[max(0, len(text)-128):]
	}
	return n, nil
}

func (h *cliHints) category() CLIRefreshHint {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hint == "" {
		return CLIHintUnknown
	}
	return h.hint
}
