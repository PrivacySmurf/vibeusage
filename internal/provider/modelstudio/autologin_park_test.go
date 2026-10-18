package modelstudio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

const parkTestCookie = "login_aliyunid_ticket=imported; login_current_pk=account"

// isolateParkFixture keeps our own new fixtures off native Chrome discovery:
// HOME scratch, MODELSTUDIO_COOKIE/VIBEUSAGE_ALLOW_KEYCHAIN/cdpPortsEnv
// cleared, plus testenv.ApplyVibeusage.
func isolateParkFixture(t *testing.T) {
	t.Helper()
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODELSTUDIO_COOKIE", "")
	t.Setenv("VIBEUSAGE_ALLOW_KEYCHAIN", "")
	t.Setenv(cdpPortsEnv, "")
}

// parkRig wires a strategy against fakes only: a session server whose health
// can be flipped, a stub importer, a stub login driver that always reports an
// interactive blocker, and a login_command that merely appends a line to a
// counter file inside t.TempDir() and prints fake credentials.
type parkRig struct {
	t         *testing.T
	strategy  *WebConsoleStrategy
	healthy   atomic.Bool
	importOK  atomic.Bool
	logins    atomic.Int32
	counter   string
	blockKind loginBlockerKind
}

func newParkRig(t *testing.T, kind loginBlockerKind) *parkRig {
	t.Helper()
	isolateParkFixture(t)
	rig := &parkRig{t: t, blockKind: kind, counter: filepath.Join(t.TempDir(), "login-command-runs")}

	server := newModelStudioSessionServer(t, func(string) string {
		if rig.healthy.Load() {
			return successfulSummaryPayload()
		}
		return expiredSessionPayload()
	})
	rig.strategy = newTestStrategy(server.URL)

	t.Setenv(cdpPortsEnv, "")
	cfg := config.DefaultConfig()
	cfg.Providers["modelstudio"] = config.ProviderConfig{
		CDPPorts:  []int{9444, 9222},
		LoginPort: 9444,
		// Never a real credential command: it only records that it ran.
		LoginCommand: fmt.Sprintf(`echo run >> '%s'; printf '{"username":"fake-user","password":"fake-pass"}'`, rig.counter),
	}
	config.SetGlobal(cfg)
	t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

	stubImporter(t, func(context.Context) (browserSession, error) {
		if rig.importOK.Load() {
			return browserSession{Cookie: parkTestCookie, SourceLabel: "Chrome (CDP :9444)"}, nil
		}
		return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome (CDP :9444)", errBrowserCookieImportUnavailable)
	})
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		rig.logins.Add(1)
		reason := verificationBlockedReason
		if rig.blockKind == loginBlockerCaptcha {
			reason = captchaBlockedReason
		}
		return browserSession{}, fmt.Errorf("sign-in did not complete: %w", &loginBlockedError{Kind: rig.blockKind, Reason: reason})
	})
	return rig
}

// commandRuns counts how many times login_command ran.
func (r *parkRig) commandRuns() int {
	data, err := os.ReadFile(r.counter)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "run\n")
}

func (r *parkRig) fetch() (string, bool) {
	r.t.Helper()
	result, err := r.strategy.Fetch(context.Background())
	if err != nil {
		r.t.Fatalf("Fetch() error: %v", err)
	}
	return result.Error, result.Success
}

func wantParkedWording(kind loginBlockerKind) string {
	if kind == loginBlockerCaptcha {
		return "slider captcha"
	}
	return "account verification"
}

func assertParkedMessage(t *testing.T, kind loginBlockerKind, msg string) {
	t.Helper()
	for _, want := range []string{
		"automatic sign-in is parked pending manual sign-in",
		modelStudioDashboardURL,
		"9444",
		wantParkedWording(kind),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
	if kind == loginBlockerVerification && strings.Contains(msg, "slider captcha") {
		t.Errorf("verification park reported as slider captcha: %s", msg)
	}
}

func TestParkOnVerificationBlocksFurtherAutoLogin(t *testing.T) {
	runParkBlocksFurtherAutoLogin(t, loginBlockerVerification)
}

func TestParkOnCaptchaBlocksFurtherAutoLogin(t *testing.T) {
	runParkBlocksFurtherAutoLogin(t, loginBlockerCaptcha)
}

func runParkBlocksFurtherAutoLogin(t *testing.T, kind loginBlockerKind) {
	modes := []struct {
		name  string
		setup func(*parkRig)
	}{
		{name: "no browser session", setup: func(*parkRig) {}},
		{name: "expired imported session", setup: func(r *parkRig) { r.importOK.Store(true) }},
		{name: "expired stored session", setup: func(r *parkRig) {
			storeModelStudioTestSession(r.t, sessionCredentials{
				Cookie: "login_aliyunid_ticket=stale; login_current_pk=account", Source: sessionSourceBrowser, BrowserLabel: "Chrome (CDP :9444)",
			})
		}},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			rig := newParkRig(t, kind)
			mode.setup(rig)

			first, ok := rig.fetch()
			if ok {
				t.Fatal("first fetch unexpectedly succeeded")
			}
			if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
				t.Fatalf("first fetch: driver calls=%d login_command runs=%d, want 1 and 1", rig.logins.Load(), rig.commandRuns())
			}
			assertParkedMessage(t, kind, first)
			if strings.Contains(first, "automatic sign-in failed") {
				t.Errorf("park should read as a state, not a failed attempt: %s", first)
			}

			park, parked := loadLoginPark()
			if !parked || park.Kind != kind || park.ParkedAt.IsZero() || park.Reason == "" {
				t.Fatalf("park marker = %+v parked=%v", park, parked)
			}
			info, err := os.Stat(loginParkMarker())
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("marker stat err=%v mode=%v, want 0600", err, info)
			}
			if dir, err := os.Stat(filepath.Dir(loginParkMarker())); err != nil || dir.Mode().Perm() != 0o700 {
				t.Errorf("marker dir stat err=%v mode=%v, want 0700", err, dir)
			}

			// Second Fetch: the cooldown has not elapsed, but the park — not
			// the cooldown — must be what answers.
			second, ok := rig.fetch()
			if ok {
				t.Fatal("second fetch unexpectedly succeeded")
			}
			assertParkedMessage(t, kind, second)
			if strings.Contains(second, "attempted recently") {
				t.Errorf("second fetch answered with the cooldown, not the park: %s", second)
			}
			if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
				t.Errorf("second fetch: driver calls=%d login_command runs=%d, want still 1 and 1", rig.logins.Load(), rig.commandRuns())
			}
		})
	}
}

func TestParkNotClearedByCooldownExpiry(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)
	if _, ok := rig.fetch(); ok {
		t.Fatal("first fetch unexpectedly succeeded")
	}
	before, _ := loadLoginPark()

	// Both time brakes elapse: the login-attempt cooldown (backdated marker)
	// and, by construction, the 30-minute session throttle (each Fetch here is
	// a fresh process-equivalent call).
	recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
	for i := 0; i < 2; i++ {
		msg, ok := rig.fetch()
		if ok {
			t.Fatal("fetch unexpectedly succeeded")
		}
		assertParkedMessage(t, loginBlockerVerification, msg)
	}
	if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
		t.Errorf("driver calls=%d login_command runs=%d after cooldown expiry, want 1 and 1", rig.logins.Load(), rig.commandRuns())
	}
	after, parked := loadLoginPark()
	if !parked || !after.ParkedAt.Equal(before.ParkedAt) {
		t.Errorf("park changed across cooldown expiry: before=%+v after=%+v parked=%v", before, after, parked)
	}
}

func TestParkedMarkerNeverRunsLoginCommand(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"verification marker": func(t *testing.T) {
			if err := writeLoginPark(loginBlockerVerification, verificationBlockedReason, time.Now()); err != nil {
				t.Fatal(err)
			}
		},
		"captcha marker": func(t *testing.T) {
			if err := writeLoginPark(loginBlockerCaptcha, captchaBlockedReason, time.Now()); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt marker": func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(loginParkMarker()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(loginParkMarker(), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newParkRig(t, loginBlockerVerification)
			write(t)
			for _, importOK := range []bool{false, true} {
				rig.importOK.Store(importOK)
				msg, ok := rig.fetch()
				if ok {
					t.Fatal("fetch unexpectedly succeeded")
				}
				if !strings.Contains(msg, "automatic sign-in is parked pending manual sign-in") || !strings.Contains(msg, modelStudioDashboardURL) {
					t.Errorf("message lacks park guidance: %s", msg)
				}
			}
			if rig.logins.Load() != 0 {
				t.Errorf("driver calls = %d, want 0", rig.logins.Load())
			}
			if _, err := os.Stat(rig.counter); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("login_command ran while parked (counter stat err=%v)", err)
			}
			if !lastLoginAttempt().IsZero() {
				t.Error("a parked fetch must not record a login attempt")
			}
		})
	}
}

func TestParkClearedBySessionRecovery(t *testing.T) {
	park := func(t *testing.T, rig *parkRig) {
		t.Helper()
		if err := writeLoginPark(rig.blockKind, verificationBlockedReason, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// afterRecovery proves auto-login is allowed again: with the cooldown
	// marker backdated and the session failing, the driver runs exactly once.
	afterRecovery := func(t *testing.T, rig *parkRig) {
		t.Helper()
		if _, parked := loadLoginPark(); parked {
			t.Fatal("park should be cleared after recovery")
		}
		if rig.logins.Load() != 0 {
			t.Fatalf("recovery must not need the driver, calls=%d", rig.logins.Load())
		}
		rig.healthy.Store(false)
		rig.importOK.Store(false)
		recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
		if _, ok := rig.fetch(); ok {
			t.Fatal("fetch unexpectedly succeeded")
		}
		if rig.logins.Load() != 1 {
			t.Errorf("driver calls after unpark = %d, want exactly 1", rig.logins.Load())
		}
		if _, parked := loadLoginPark(); !parked {
			t.Error("a fresh blocker should park again")
		}
	}

	t.Run("AcceptCredential session write", func(t *testing.T) {
		rig := newParkRig(t, loginBlockerVerification)
		park(t, rig)
		if err := (ModelStudio{}).AcceptCredential("login_aliyunid_ticket=pasted; sec_token=tok"); err != nil {
			t.Fatal(err)
		}
		rig.healthy.Store(true)
		afterRecoveryFetch := func() {
			if _, ok := rig.fetch(); !ok {
				t.Fatal("stored manual session should fetch")
			}
		}
		afterRecoveryFetch()
		afterRecovery(t, rig)
	})

	t.Run("AcceptCredential does not clear for OpenAPI key", func(t *testing.T) {
		rig := newParkRig(t, loginBlockerVerification)
		park(t, rig)
		if err := (ModelStudio{}).AcceptCredential("key-id:key-secret"); err != nil {
			t.Fatal(err)
		}
		if _, parked := loadLoginPark(); !parked {
			t.Error("an OpenAPI key is not a signed-in session and must not unpark")
		}
	})

	t.Run("AcceptCredential session write clears immediately", func(t *testing.T) {
		rig := newParkRig(t, loginBlockerCaptcha)
		park(t, rig)
		if err := (ModelStudio{}).AcceptCredential(`{"cookie":"login_aliyunid_ticket=pasted"}`); err != nil {
			t.Fatal(err)
		}
		if _, parked := loadLoginPark(); parked {
			t.Error("a saved session should unpark")
		}
	})

	t.Run("import and fetch success", func(t *testing.T) {
		rig := newParkRig(t, loginBlockerCaptcha)
		park(t, rig)
		rig.importOK.Store(true)
		rig.healthy.Store(true)
		if _, ok := rig.fetch(); !ok {
			t.Fatal("imported session should fetch")
		}
		afterRecovery(t, rig)
	})

	t.Run("stored session fetch success", func(t *testing.T) {
		rig := newParkRig(t, loginBlockerVerification)
		park(t, rig)
		storeModelStudioTestSession(t, sessionCredentials{Cookie: "login_aliyunid_ticket=good", Source: sessionSourceManual})
		rig.healthy.Store(true)
		if _, ok := rig.fetch(); !ok {
			t.Fatal("stored session should fetch")
		}
		afterRecovery(t, rig)
	})

	t.Run("failed import fetch does not clear", func(t *testing.T) {
		rig := newParkRig(t, loginBlockerVerification)
		park(t, rig)
		rig.importOK.Store(true) // imports a session the console still rejects
		if _, ok := rig.fetch(); ok {
			t.Fatal("fetch unexpectedly succeeded")
		}
		if _, parked := loadLoginPark(); !parked {
			t.Error("park must survive a session the console rejects")
		}
		if rig.logins.Load() != 0 {
			t.Errorf("driver calls = %d, want 0", rig.logins.Load())
		}
	})
}

func TestAutoLoginFormErrorDoesNotPark(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		rig.logins.Add(1)
		return browserSession{}, errors.New("sign-in rejected: Incorrect password")
	})
	msg, ok := rig.fetch()
	if ok || !strings.Contains(msg, "automatic sign-in failed") || !strings.Contains(msg, "sign-in rejected") {
		t.Errorf("unexpected result: ok=%v msg=%s", ok, msg)
	}
	if _, parked := loadLoginPark(); parked {
		t.Error("a form error is not an interactive blocker and must not park")
	}
	if msg, _ := rig.fetch(); !strings.Contains(msg, "attempted recently") {
		t.Errorf("second fetch should hit the plain cooldown: %s", msg)
	}
}

// stubParkWrite swaps the park-marker write seam for the test and restores it
// on cleanup. The previous (real) writer is returned so a stub can delegate.
func stubParkWrite(t *testing.T, fn func(path string, data []byte) error) (real func(path string, data []byte) error) {
	t.Helper()
	old := writeLoginParkFile
	t.Cleanup(func() { writeLoginParkFile = old })
	writeLoginParkFile = fn
	return old
}

// TestParkGuardWriteFailureBlocksAutoLogin: when the provisional guard cannot
// be recorded, no automation may start, however often Fetch is retried.
func TestParkGuardWriteFailureBlocksAutoLogin(t *testing.T) {
	modes := []struct {
		name  string
		setup func(*parkRig)
	}{
		{name: "no browser session", setup: func(*parkRig) {}},
		{name: "expired imported session", setup: func(r *parkRig) { r.importOK.Store(true) }},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			rig := newParkRig(t, loginBlockerVerification)
			mode.setup(rig)
			var writes atomic.Int32
			stubParkWrite(t, func(string, []byte) error {
				writes.Add(1)
				return errors.New("injected disk failure")
			})

			for i := 1; i <= 2; i++ {
				msg, ok := rig.fetch()
				if ok {
					t.Fatalf("fetch %d unexpectedly succeeded", i)
				}
				for _, want := range []string{"automatic sign-in not attempted", "park guard", "injected disk failure"} {
					if !strings.Contains(msg, want) {
						t.Errorf("fetch %d: message missing %q: %s", i, want, msg)
					}
				}
				if strings.Contains(msg, "is parked") || strings.Contains(msg, "automatic sign-in failed") {
					t.Errorf("fetch %d: message claims a park or a failed attempt: %s", i, msg)
				}
				if rig.logins.Load() != 0 || rig.commandRuns() != 0 {
					t.Fatalf("fetch %d: driver calls=%d login_command runs=%d, want 0 and 0", i, rig.logins.Load(), rig.commandRuns())
				}
				if _, err := os.Stat(rig.counter); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("fetch %d: login_command ran (counter stat err=%v)", i, err)
				}
				if !lastLoginAttempt().IsZero() {
					t.Errorf("fetch %d: a login attempt was recorded without a guard", i)
				}
			}
			if writes.Load() != 2 {
				t.Errorf("guard write attempts = %d, want 2 (one per Fetch)", writes.Load())
			}
			if _, parked := loadLoginPark(); parked {
				t.Error("no marker was written, so nothing is parked")
			}
			d := diagnoseModelStudioAutoLogin()
			if d.Status != provider.DiagInfo || !strings.Contains(d.Detail, "not parked") {
				t.Errorf("diagnostic = %+v, want truthful not-parked", d)
			}
		})
	}
}

// TestParkGuardConversionFailureStaysParked: a typed blocker whose park
// update cannot be written must leave the provisional guard in force.
func TestParkGuardConversionFailureStaysParked(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)
	var writes atomic.Int32
	real := stubParkWrite(t, nil)
	writeLoginParkFile = func(path string, data []byte) error {
		if writes.Add(1) == 1 {
			return real(path, data) // the provisional guard
		}
		return errors.New("injected conversion failure")
	}

	first, ok := rig.fetch()
	if ok {
		t.Fatal("first fetch unexpectedly succeeded")
	}
	for _, want := range []string{
		"automatic sign-in is parked pending manual sign-in",
		"park marker update failed", "injected conversion failure", "provisional guard",
		modelStudioDashboardURL, "9444",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first message missing %q: %s", want, first)
		}
	}
	if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
		t.Fatalf("first fetch: driver calls=%d login_command runs=%d, want 1 and 1", rig.logins.Load(), rig.commandRuns())
	}
	park, parked := loadLoginPark()
	if !parked || park.Kind != loginBlockerPending || park.Reason == "" || park.ParkedAt.IsZero() {
		t.Fatalf("marker = %+v parked=%v, want the pending guard", park, parked)
	}

	// The store recovers and the cooldown elapses; only the guard holds.
	writeLoginParkFile = real
	recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
	second, ok := rig.fetch()
	if ok || !strings.Contains(second, "automatic sign-in is parked pending manual sign-in") ||
		!strings.Contains(second, "did not finish") {
		t.Errorf("second fetch should report the pending park: ok=%v %s", ok, second)
	}
	if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
		t.Errorf("second fetch: driver calls=%d login_command runs=%d, want still 1 and 1", rig.logins.Load(), rig.commandRuns())
	}

	d := diagnoseModelStudioAutoLogin()
	for _, want := range []string{"parked pending manual sign-in", "did not finish", pendingGuardReason, "modelstudio-login-park.json", modelStudioDashboardURL, "9444"} {
		if d.Status != provider.DiagWarn || !strings.Contains(d.Detail, want) {
			t.Errorf("diagnostic %+v missing %q", d, want)
		}
	}
}

// TestParkGuardReleasedOnNonBlockedOutcome: every outcome that is not an
// interactive blocker must release the provisional guard again.
func TestParkGuardReleasedOnNonBlockedOutcome(t *testing.T) {
	cases := []struct {
		name    string
		command string // login_command override; empty keeps the rig's
		driver  func() (browserSession, error)
		wantOK  bool
		wantErr string
	}{
		{
			name: "form error",
			driver: func() (browserSession, error) {
				return browserSession{}, errors.New("sign-in rejected: Incorrect password")
			},
			wantErr: "sign-in rejected",
		},
		{
			name: "form never appeared without blocker",
			driver: func() (browserSession, error) {
				return browserSession{}, errors.New("sign-in form did not appear: no frame")
			},
			wantErr: "sign-in form did not appear",
		},
		{
			name:    "context deadline",
			driver:  func() (browserSession, error) { return browserSession{}, context.DeadlineExceeded },
			wantErr: "deadline exceeded",
		},
		{
			name:    "credential command failure",
			command: `echo boom >&2; exit 3`,
			driver: func() (browserSession, error) {
				return browserSession{}, errors.New("driver must not run after a credential failure")
			},
			wantErr: "login_command failed: boom",
		},
		{
			name: "success",
			driver: func() (browserSession, error) {
				return browserSession{Cookie: parkTestCookie, SourceLabel: "CDP auto-login"}, nil
			},
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newParkRig(t, loginBlockerVerification)
			rig.healthy.Store(true) // a renewed session works
			if tc.command != "" {
				cfg := config.DefaultConfig()
				cfg.Providers["modelstudio"] = config.ProviderConfig{CDPPorts: []int{9444, 9222}, LoginPort: 9444, LoginCommand: tc.command}
				config.SetGlobal(cfg)
			}
			stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
				rig.logins.Add(1)
				if park, parked := loadLoginPark(); !parked || park.Kind != loginBlockerPending {
					t.Errorf("guard during the attempt = %+v parked=%v, want pending", park, parked)
				}
				return tc.driver()
			})

			msg, ok := rig.fetch()
			if ok != tc.wantOK {
				t.Fatalf("success=%v, want %v (%s)", ok, tc.wantOK, msg)
			}
			if tc.wantErr != "" && !strings.Contains(msg, tc.wantErr) {
				t.Errorf("message missing %q: %s", tc.wantErr, msg)
			}
			if strings.Contains(msg, "is parked") {
				t.Errorf("a non-blocker outcome must not read as a park: %s", msg)
			}
			if park, parked := loadLoginPark(); parked {
				t.Errorf("guard left behind after a non-blocked outcome: %+v", park)
			}
			if _, err := os.Stat(loginParkMarker()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("marker file still present (stat err=%v)", err)
			}
		})
	}
}

// stubLoginLock swaps the login-lock acquisition seam and restores it on
// cleanup. The real acquisition is returned so a stub can wrap it.
func stubLoginLock(t *testing.T, fn func() (func(), error)) (real func() (func(), error)) {
	t.Helper()
	old := acquireModelStudioLoginLock
	t.Cleanup(func() { acquireModelStudioLoginLock = old })
	acquireModelStudioLoginLock = fn
	return old
}

// TestParkConcurrentAutoLoginSerialized: while one attempt owns the lock
// (before its park/cooldown reads), a second attempt must not start, and the
// first attempt's real park must survive.
func TestParkConcurrentAutoLoginSerialized(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)

	var calls atomic.Int32
	acquired := make(chan struct{})
	proceed := make(chan struct{})
	real := stubLoginLock(t, nil)
	acquireModelStudioLoginLock = func() (func(), error) {
		release, err := real()
		if err == nil && calls.Add(1) == 1 {
			close(acquired)
			<-proceed // hold the real lock before autoLogin's park/cooldown reads
		}
		return release, err
	}
	released := false
	releaseFirst := func() {
		if !released {
			released = true
			close(proceed)
		}
	}

	type fetchOutcome struct {
		msg string
		ok  bool
		err error
	}
	firstDone := make(chan fetchOutcome, 1)
	firstExited := make(chan struct{})
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	go func() {
		defer close(firstExited)
		result, err := rig.strategy.Fetch(firstCtx)
		firstDone <- fetchOutcome{msg: result.Error, ok: result.Success, err: err}
	}()
	// Cancel, release and join the first Fetch before the driver/importer/config
	// fakes are restored, so an early failure can never let it reach the real driver.
	t.Cleanup(func() {
		cancelFirst()
		releaseFirst()
		<-firstExited
	})
	select {
	case <-acquired:
	case <-time.After(10 * time.Second):
		t.Fatal("first attempt never acquired the lock")
	}

	assertNothingStarted := func(label string) {
		t.Helper()
		if rig.logins.Load() != 0 || rig.commandRuns() != 0 {
			t.Errorf("%s: driver calls=%d login_command runs=%d, want 0 and 0", label, rig.logins.Load(), rig.commandRuns())
		}
		if !lastLoginAttempt().IsZero() {
			t.Errorf("%s: a login attempt was recorded", label)
		}
		if _, parked := loadLoginPark(); parked {
			t.Errorf("%s: a park marker was written by the contending attempt", label)
		}
	}

	// Direct strategy call.
	if _, err := rig.strategy.autoLogin(context.Background()); !errors.Is(err, errAutoLoginInProgress) {
		t.Fatalf("second autoLogin error = %v, want errAutoLoginInProgress", err)
	}
	assertNothingStarted("second autoLogin")

	// Full Fetch.
	msg, ok := rig.fetch()
	if ok {
		t.Fatal("contending fetch unexpectedly succeeded")
	}
	if !strings.Contains(msg, "another automatic sign-in is in progress") {
		t.Errorf("contending message missing in-progress wording: %s", msg)
	}
	if strings.Contains(msg, "is parked") || strings.Contains(msg, "automatic sign-in failed") {
		t.Errorf("contention must read as not attempted, not a park or failure: %s", msg)
	}
	assertNothingStarted("second fetch")

	// Let the first owner proceed into a typed verification blocker.
	releaseFirst()
	var first fetchOutcome
	select {
	case first = <-firstDone:
	case <-time.After(30 * time.Second):
		t.Fatal("first attempt never finished")
	}
	if first.err != nil || first.ok {
		t.Fatalf("first fetch ok=%v err=%v", first.ok, first.err)
	}
	assertParkedMessage(t, loginBlockerVerification, first.msg)
	if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
		t.Fatalf("after first attempt: driver calls=%d login_command runs=%d, want 1 and 1", rig.logins.Load(), rig.commandRuns())
	}
	park, parked := loadLoginPark()
	if !parked || park.Kind != loginBlockerVerification {
		t.Fatalf("park = %+v parked=%v, want a real verification park", park, parked)
	}

	// The park, not the cooldown, keeps further attempts out.
	recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
	after, ok := rig.fetch()
	if ok {
		t.Fatal("post-park fetch unexpectedly succeeded")
	}
	assertParkedMessage(t, loginBlockerVerification, after)
	if rig.logins.Load() != 1 || rig.commandRuns() != 1 {
		t.Errorf("after backdated cooldown: driver calls=%d login_command runs=%d, want still 1 and 1", rig.logins.Load(), rig.commandRuns())
	}
	if park, parked := loadLoginPark(); !parked || park.Kind != loginBlockerVerification {
		t.Errorf("park did not survive: %+v parked=%v", park, parked)
	}
}

// TestAutoLoginOrdinaryFailureReleasesGuardAndLock: a non-blocker failure
// releases both the provisional guard and the lock for a later attempt.
func TestAutoLoginOrdinaryFailureReleasesGuardAndLock(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		rig.logins.Add(1)
		return browserSession{}, errors.New("sign-in rejected: Incorrect password")
	})

	msg, ok := rig.fetch()
	if ok || !strings.Contains(msg, "sign-in rejected") {
		t.Fatalf("unexpected first result: ok=%v msg=%s", ok, msg)
	}
	if _, parked := loadLoginPark(); parked {
		t.Fatal("guard left behind after an ordinary failure")
	}
	release, err := acquireAutoLoginLock()
	if err != nil {
		t.Fatalf("lock not released after ordinary failure: %v", err)
	}
	release()

	recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
	msg, ok = rig.fetch()
	if ok || !strings.Contains(msg, "sign-in rejected") {
		t.Fatalf("unexpected second result: ok=%v msg=%s", ok, msg)
	}
	if rig.logins.Load() != 2 || rig.commandRuns() != 2 {
		t.Errorf("driver calls=%d login_command runs=%d, want 2 and 2", rig.logins.Load(), rig.commandRuns())
	}
	if _, parked := loadLoginPark(); parked {
		t.Error("guard left behind after the second ordinary failure")
	}
}

// TestParkLockAcquisitionFailureRunsNoAutomation: when the lock cannot be
// acquired for an I/O reason, nothing runs, nothing is written and nothing is
// reported as parked.
func TestParkLockAcquisitionFailureRunsNoAutomation(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)
	stubLoginLock(t, func() (func(), error) { return nil, errors.New("injected lock failure") })

	for i := 1; i <= 2; i++ {
		msg, ok := rig.fetch()
		if ok {
			t.Fatalf("fetch %d unexpectedly succeeded", i)
		}
		for _, want := range []string{"cannot acquire park guard lock", "not attempted", "injected lock failure"} {
			if !strings.Contains(msg, want) {
				t.Errorf("fetch %d: message missing %q: %s", i, want, msg)
			}
		}
		if strings.Contains(msg, "is parked") || strings.Contains(msg, "automatic sign-in failed") {
			t.Errorf("fetch %d: message claims a park or a failed attempt: %s", i, msg)
		}
		if rig.logins.Load() != 0 || rig.commandRuns() != 0 {
			t.Fatalf("fetch %d: driver calls=%d login_command runs=%d, want 0 and 0", i, rig.logins.Load(), rig.commandRuns())
		}
		if !lastLoginAttempt().IsZero() {
			t.Errorf("fetch %d: a login attempt was recorded", i)
		}
		if _, err := os.Stat(loginParkMarker()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("fetch %d: marker written (stat err=%v)", i, err)
		}
	}
	d := diagnoseModelStudioAutoLogin()
	if d.Status != provider.DiagInfo || !strings.Contains(d.Detail, "not parked") {
		t.Errorf("diagnostic = %+v, want truthful not-parked", d)
	}
}

const (
	parkLockHelperEnv   = "VIBEUSAGE_PARK_LOCK_HELPER"
	parkLockHelperReady = "PARK_LOCK_HELPER_READY"
)

// TestParkLockHelperProcess is the child half of TestParkLockCrossProcess. It
// only acquires and holds the real lock until stdin closes; it never runs
// Fetch, a command or a driver. It is a no-op in normal test runs.
func TestParkLockHelperProcess(t *testing.T) {
	if os.Getenv(parkLockHelperEnv) != "1" {
		return
	}
	release, err := acquireAutoLoginLock()
	if err != nil {
		fmt.Fprintf(os.Stdout, "PARK_LOCK_HELPER_ERROR %v\n", err)
		t.Fatalf("helper cannot acquire lock: %v", err)
	}
	fmt.Fprintln(os.Stdout, parkLockHelperReady)
	_, _ = bufio.NewReader(os.Stdin).ReadString(0) // blocks until the parent closes stdin
	release()
}

func TestParkLockCrossProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based contention is exercised on unix; the windows lock is compile-checked only")
	}
	rig := newParkRig(t, loginBlockerVerification)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestParkLockHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), parkLockHelperEnv+"=1") // includes the scratch VIBEUSAGE_* dirs
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	ready := make(chan struct{})
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		signalled := false
		for sc.Scan() {
			line := sc.Text()
			select {
			case lines <- line:
			default:
			}
			if line == parkLockHelperReady && !signalled {
				signalled = true
				close(ready)
			}
		}
		waited <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
	})

	select {
	case <-ready:
	case err := <-waited:
		t.Fatalf("helper exited before signalling readiness: %v stderr=%s", err, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("helper never signalled readiness; stderr=%s", stderr.String())
	}

	// (i) cross-process contention.
	if release, err := acquireAutoLoginLock(); !errors.Is(err, errAutoLoginInProgress) {
		if err == nil {
			release()
		}
		t.Fatalf("acquire while child holds lock = %v, want errAutoLoginInProgress", err)
	}

	// (ii) child exit permits reacquisition.
	_ = stdin.Close()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("helper exit: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not exit after stdin closed")
	}
	release, err := acquireAutoLoginLock()
	if err != nil {
		t.Fatalf("reacquire after child exit: %v", err)
	}
	release()
	release, err = acquireAutoLoginLock()
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release()

	// (iii) a free lock does not unpark an existing pending marker.
	if err := writeLoginPark(loginBlockerPending, pendingGuardReason, time.Now()); err != nil {
		t.Fatal(err)
	}
	msg, ok := rig.fetch()
	if ok {
		t.Fatal("fetch unexpectedly succeeded")
	}
	if !strings.Contains(msg, "automatic sign-in is parked pending manual sign-in") || !strings.Contains(msg, "did not finish") {
		t.Errorf("expected the pending park message: %s", msg)
	}
	if rig.logins.Load() != 0 || rig.commandRuns() != 0 {
		t.Errorf("driver calls=%d login_command runs=%d, want 0 and 0", rig.logins.Load(), rig.commandRuns())
	}
	if park, parked := loadLoginPark(); !parked || park.Kind != loginBlockerPending {
		t.Errorf("pending marker lost: %+v parked=%v", park, parked)
	}
	lock := filepath.Join(config.ThrottlesDir(), "modelstudio-login.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("lock file should persist in the scratch throttles dir: %v", err)
	}
}

func TestParkExtendTimeoutReturnsBaseWhileParked(t *testing.T) {
	rig := newParkRig(t, loginBlockerVerification)
	base := 30 * time.Second
	if got := rig.strategy.ExtendTimeout(base); got != base+loginOverallDeadline {
		t.Fatalf("unparked ExtendTimeout = %v, want %v", got, base+loginOverallDeadline)
	}
	if err := writeLoginPark(loginBlockerVerification, verificationBlockedReason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := rig.strategy.ExtendTimeout(base); got != base {
		t.Errorf("parked ExtendTimeout = %v, want %v", got, base)
	}
}

func TestParkMarkerCorruptCountsAsParked(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	if _, parked := loadLoginPark(); parked {
		t.Fatal("no marker should not be parked")
	}
	if err := os.MkdirAll(filepath.Dir(loginParkMarker()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loginParkMarker(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	park, parked := loadLoginPark()
	if !parked || park.Kind != loginBlockerUnknown {
		t.Errorf("corrupt marker = %+v parked=%v, want parked kind unknown", park, parked)
	}
	clearLoginPark()
	if _, parked := loadLoginPark(); parked {
		t.Error("clear should remove a corrupt marker")
	}
	clearLoginPark() // missing marker is fine
}

func TestParkGuidancePort(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv(cdpPortsEnv, "")
	t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })
	set := func(pc config.ProviderConfig) {
		cfg := config.DefaultConfig()
		cfg.Providers["modelstudio"] = pc
		config.SetGlobal(cfg)
	}

	set(config.ProviderConfig{LoginPort: 9445, CDPPorts: []int{9444}})
	if got := loginGuidancePort(); got != "9445" {
		t.Errorf("login_port: %q", got)
	}
	set(config.ProviderConfig{CDPPorts: []int{9444, 9222}})
	if got := loginGuidancePort(); got != "9444" {
		t.Errorf("first cdp_ports: %q", got)
	}
	set(config.ProviderConfig{})
	if got := loginGuidancePort(); got != "login_port" {
		t.Errorf("fallback: %q", got)
	}

	err := error(&loginParkedError{Kind: loginBlockerCaptcha, Port: "9444"})
	if !errors.Is(err, errAutoLoginParked) {
		t.Error("parked error should match errAutoLoginParked")
	}
	assertParkedMessage(t, loginBlockerCaptcha, err.Error())
}

func TestDiagnoseAutoLoginParked(t *testing.T) {
	const canaryCookie = "canary-cookie-value-9d3f"
	const canaryPassword = "canary-password-value-7a1c"

	for _, kind := range []loginBlockerKind{loginBlockerVerification, loginBlockerCaptcha} {
		t.Run(string(kind), func(t *testing.T) {
			rig := newParkRig(t, kind)
			storeModelStudioTestSession(t, sessionCredentials{Cookie: "login_aliyunid_ticket=" + canaryCookie, Source: sessionSourceBrowser})
			t.Setenv("MODELSTUDIO_COOKIE", "")
			rig.blockKind = kind
			reason := verificationBlockedReason
			if kind == loginBlockerCaptcha {
				reason = captchaBlockedReason
			}
			parkedAt := time.Now().Add(-90 * time.Minute)
			if err := writeLoginPark(kind, reason, parkedAt); err != nil {
				t.Fatal(err)
			}

			d := diagnoseModelStudioAutoLogin()
			if d.Name != "Auto-login" || d.Status != provider.DiagWarn {
				t.Fatalf("diagnostic = %+v, want Auto-login warn", d)
			}
			for _, want := range []string{
				"parked pending manual sign-in",
				modelStudioDashboardURL,
				"9444",
				wantParkedWording(kind),
				"clears automatically once a signed-in session is imported or saved",
				"modelstudio-login-park.json",
				parkedAt.Format(time.RFC3339),
			} {
				if !strings.Contains(d.Detail, want) {
					t.Errorf("detail missing %q: %s", want, d.Detail)
				}
			}
			if kind == loginBlockerVerification && strings.Contains(d.Detail, "slider captcha") {
				t.Errorf("verification park reported as slider captcha: %s", d.Detail)
			}
			for _, secret := range []string{canaryCookie, canaryPassword, "fake-pass", "fake-user"} {
				if strings.Contains(d.Detail, secret) {
					t.Errorf("detail leaks %q: %s", secret, d.Detail)
				}
			}
		})
	}
}

func TestDiagnoseAutoLoginNotParked(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		newParkRig(t, loginBlockerVerification)
		d := diagnoseModelStudioAutoLogin()
		if d.Status != provider.DiagInfo || !strings.Contains(d.Detail, "not parked") {
			t.Errorf("diagnostic = %+v", d)
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		config.SetGlobal(config.DefaultConfig())
		d := diagnoseModelStudioAutoLogin()
		if d.Status != provider.DiagInfo || !strings.Contains(d.Detail, "not configured") {
			t.Errorf("diagnostic = %+v", d)
		}
	})
}

// shortFormWait lets tests that never show the sign-in form fail the form
// wait in a few hundred milliseconds instead of twenty seconds.
func shortFormWait(t *testing.T) {
	t.Helper()
	oldWait, oldSettle := loginFormWait, loginFormSettle
	loginFormWait, loginFormSettle = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { loginFormWait, loginFormSettle = oldWait, oldSettle })
}

func (f *fakeLoginChrome) tabsOpened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

// TestAutoLoginInitialBlockerParksBeforeForm: when the first page is a
// blocker instead of the username form, the failed form wait must classify
// it and return the typed error that web.go parks on.
func TestAutoLoginInitialBlockerParksBeforeForm(t *testing.T) {
	cases := []struct {
		name string
		kind loginBlockerKind
		set  func(*fakeLoginChrome)
	}{
		{"verification", loginBlockerVerification, func(c *fakeLoginChrome) { c.initialVerification = true }},
		{"captcha", loginBlockerCaptcha, func(c *fakeLoginChrome) { c.initialTMD = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortFormWait(t)
			chrome := newFakeLoginChrome(t)
			tc.set(chrome)

			_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "fake-user", Password: "fake-pass"})
			var blocked *loginBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("expected *loginBlockedError, got %T %v", err, err)
			}
			if blocked.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", blocked.Kind, tc.kind)
			}
			if tc.kind == loginBlockerVerification && strings.Contains(err.Error(), "slider captcha") {
				t.Errorf("verification reason mentions slider captcha: %v", err)
			}
			chrome.mu.Lock()
			defer chrome.mu.Unlock()
			if len(chrome.typed) != 0 || chrome.submits != 0 {
				t.Errorf("typed=%v submits=%d, want nothing typed or submitted", chrome.typed, chrome.submits)
			}
			if chrome.closed != 1 {
				t.Errorf("tab should be closed, closed=%d", chrome.closed)
			}
		})
	}
}

// TestAutoLoginMissingFormWithoutBlockerStaysUntyped: a form that never
// appears with no blocker on screen must not park.
func TestAutoLoginMissingFormWithoutBlockerStaysUntyped(t *testing.T) {
	shortFormWait(t)
	chrome := newFakeLoginChrome(t)
	chrome.noForm = true

	_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "fake-user", Password: "fake-pass"})
	if err == nil || !strings.Contains(err.Error(), "sign-in form did not appear") {
		t.Fatalf("expected form-wait error, got %v", err)
	}
	var blocked *loginBlockedError
	if errors.As(err, &blocked) {
		t.Errorf("a missing form with no blocker must not classify, got %+v", blocked)
	}
}

// TestParkInitialBlockerFetchNeverRetries drives the real
// performModelStudioAutoLogin against the fake chrome through Fetch: the
// first Fetch parks, and a second one — even with the cooldown backdated —
// adds no driver call, no login_command run and no new tab.
func TestParkInitialBlockerFetchNeverRetries(t *testing.T) {
	cases := []struct {
		name string
		kind loginBlockerKind
		set  func(*fakeLoginChrome)
	}{
		{"verification", loginBlockerVerification, func(c *fakeLoginChrome) { c.initialVerification = true }},
		{"captcha", loginBlockerCaptcha, func(c *fakeLoginChrome) { c.initialTMD = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortFormWait(t)
			isolateParkFixture(t)
			chrome := newFakeLoginChrome(t)
			tc.set(chrome)
			port, err := strconv.Atoi(chrome.port)
			if err != nil {
				t.Fatal(err)
			}

			counter := filepath.Join(t.TempDir(), "login-command-runs")
			t.Setenv(cdpPortsEnv, "")
			cfg := config.DefaultConfig()
			cfg.Providers["modelstudio"] = config.ProviderConfig{
				CDPPorts:  []int{port},
				LoginPort: port,
				// Never a real credential command: it only records that it ran.
				LoginCommand: fmt.Sprintf(`echo run >> '%s'; printf '{"username":"fake-user","password":"fake-pass"}'`, counter),
			}
			config.SetGlobal(cfg)
			t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

			server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
			stubImporter(t, func(context.Context) (browserSession, error) {
				return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in fake Chrome", errBrowserCookieImportUnavailable)
			})
			var drivers atomic.Int32
			stubAutoLogin(t, func(ctx context.Context, p string, creds loginCredentials) (browserSession, error) {
				drivers.Add(1)
				return performModelStudioAutoLogin(ctx, p, creds)
			})
			runs := func() int {
				data, _ := os.ReadFile(counter)
				return strings.Count(string(data), "run\n")
			}

			strategy := newTestStrategy(server.URL)
			result, err := strategy.Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch() error: %v", err)
			}
			if result.Success {
				t.Fatal("first fetch unexpectedly succeeded")
			}
			park, parked := loadLoginPark()
			if !parked || park.Kind != tc.kind || park.Reason == "" {
				t.Fatalf("park marker = %+v parked=%v, want kind %q", park, parked, tc.kind)
			}
			if tc.kind == loginBlockerVerification && strings.Contains(park.Reason+result.Error, "slider captcha") {
				t.Errorf("verification park reported as slider captcha: %q / %q", park.Reason, result.Error)
			}
			if !strings.Contains(result.Error, "automatic sign-in is parked pending manual sign-in") || !strings.Contains(result.Error, wantParkedWording(tc.kind)) {
				t.Errorf("first fetch should report the park: %s", result.Error)
			}
			if drivers.Load() != 1 || runs() != 1 || chrome.tabsOpened() != 1 {
				t.Fatalf("first fetch: drivers=%d login_command runs=%d tabs=%d, want 1/1/1", drivers.Load(), runs(), chrome.tabsOpened())
			}

			// Cooldown elapsed: only the park may still hold the line.
			recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
			result, err = strategy.Fetch(context.Background())
			if err != nil {
				t.Fatalf("second Fetch() error: %v", err)
			}
			if result.Success || !strings.Contains(result.Error, "automatic sign-in is parked pending manual sign-in") {
				t.Errorf("second fetch should still report the park: success=%v %s", result.Success, result.Error)
			}
			if drivers.Load() != 1 || runs() != 1 || chrome.tabsOpened() != 1 {
				t.Errorf("second fetch: drivers=%d login_command runs=%d tabs=%d, want still 1/1/1", drivers.Load(), runs(), chrome.tabsOpened())
			}
		})
	}
}
