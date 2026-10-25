package modelstudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
)

// Automatic sign-in for an expired Model Studio browser session.
//
// The Alibaba Cloud console session cookie expires on its own schedule. When
// no readable browser holds a live session and the user has configured a
// login_command, vibeusage drives the configured headed Chrome through the
// sign-in form over CDP and then re-imports the cookies. Credentials only
// ever travel from the command's stdout into the browser page.

const (
	alibabaLoginURL   = "https://account.alibabacloud.com/login/login.htm"
	passportFrameHost = "passport.alibabacloud.com"
	loginUsernameSel  = "#fm-login-id"
	loginPasswordSel  = "#fm-login-password"
	loginSubmitSel    = "#fm-login-submit"
	loginCheckcodeSel = "#fm-login-checkcode"
	loginGetCodeSel   = "#J_GetCode, .send-btn, button.send-code, a.send-code"
	loginCaptchaSel   = ".nc-container, #nc_1_n1z, .nc_wrapper"
	// Alibaba's anti-bot layer ("TMD") injects the slider captcha as a nested
	// iframe under this path; its presence is the reliable signal.
	loginCaptchaFramePath = "/_____tmd_____/punish"
	captchaBlockedReason  = "sign-in blocked by a slider captcha (Alibaba risk control after repeated logins); sign in manually once in this browser"
	// verificationBlockedReason labels Alibaba's account-protection page
	// ("To protect your account, please complete the verification"), which
	// wants a code texted to the account's phone. It is not a slider.
	verificationBlockedReason = "sign-in blocked by Alibaba account verification (SMS / account-protection code required); sign in manually once in this browser"
	loginErrorSel             = ".fm-error, .error-message, .next-form-item-help"
	assistedLoginEnv          = "VIBEUSAGE_MODELSTUDIO_ASSIST"
	verificationCodeFilename  = "modelstudio-verification-code"
	loginMaxAttempts          = 3
	loginPollInterval         = 500 * time.Millisecond
	loginCommandTimeout       = 30 * time.Second
	loginOverallDeadline      = 90 * time.Second
	loginDiagnosticsHeadroom  = 2 * time.Second
	// loginCooldown is the minimum gap between automatic sign-in attempts,
	// whatever their outcome. Repeated logins are what trip Alibaba's risk
	// control (slider captcha), so a broken flow must fail quietly, not loop.
	loginCooldown = 10 * time.Minute
	// loginParkTTL is how long a verification/captcha/unknown park blocks
	// automatic sign-in. Alibaba's interactive challenge is often temporary;
	// a permanent park left today's session stale for hours after a one-time
	// SMS wall, even though a later password fill succeeded without SMS.
	// The 10-minute loginCooldown still spaces retries after the park expires.
	loginParkTTL = 4 * time.Hour
	// loginParkPendingTTL is how long a provisional "did not finish" guard
	// blocks automatic sign-in after a crash or kill mid-attempt. Shorter than
	// loginParkTTL so a dead process cannot strand the park all day.
	loginParkPendingTTL = 30 * time.Minute
)

// Tunable so tests can run the retry path quickly.
var (
	loginFormWait         = 20 * time.Second
	loginFormSettle       = 750 * time.Millisecond
	loginRedirectWait     = 15 * time.Second
	loginCookieWait       = 15 * time.Second
	loginVerificationWait = loginOverallDeadline
)

var (
	errAutoLoginUnavailable = errors.New("automatic sign-in not configured")
	errAutoLoginCoolingDown = errors.New("automatic sign-in attempted recently")
	errAutoLoginParked      = errors.New("automatic sign-in is parked pending manual sign-in")
	// errAutoLoginGuardUnavailable means the provisional park guard could not
	// be recorded, so no sign-in was started. It is deliberately not a park
	// and not "unavailable": nothing is parked and sign-in is configured.
	errAutoLoginGuardUnavailable = errors.New("automatic sign-in not attempted")
	// errAutoLoginInProgress means another process (or goroutine) owns the
	// automatic sign-in lock, so this attempt did not start.
	errAutoLoginInProgress   = errors.New("automatic sign-in not attempted: another automatic sign-in is in progress")
	errLoginNoRedirect       = errors.New("no redirect to the console after submitting")
	errLoginNoSessionCookies = errors.New("console session cookies did not appear after sign-in")
)

type loginCredentials struct {
	Username string
	Password string
}

// autoLoginModelStudio is swapped out in tests.
var autoLoginModelStudio = performModelStudioAutoLogin

// autoLoginSettings resolves the configured command and headed port.
func autoLoginSettings() (command string, port string, err error) {
	pc := config.Get().Providers["modelstudio"]
	command = strings.TrimSpace(pc.LoginCommand)
	if command == "" {
		return "", "", errAutoLoginUnavailable
	}
	if pc.LoginPort > 0 {
		port = normalizeCDPPort(strconv.Itoa(pc.LoginPort))
	} else if ports, explicit := cdpFixedPorts(pc.CDPPorts); explicit && len(ports) > 0 {
		port = ports[0]
	}
	if port == "" {
		return "", "", fmt.Errorf("%w: login_command is set but no login_port or cdp_ports to sign in on", errAutoLoginUnavailable)
	}
	return command, port, nil
}

// loginAttemptMarker records when the last automatic sign-in started so
// the cooldown survives across processes (each poll is a new process).
func loginAttemptMarker() string {
	return filepath.Join(config.ThrottlesDir(), "modelstudio-login.json")
}

func lastLoginAttempt() time.Time {
	data, err := os.ReadFile(loginAttemptMarker())
	if err != nil {
		return time.Time{}
	}
	var marker struct {
		AttemptedAt time.Time `json:"attempted_at"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return time.Time{}
	}
	return marker.AttemptedAt
}

func recordLoginAttempt(at time.Time) {
	path := loginAttemptMarker()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(struct {
		AttemptedAt time.Time `json:"attempted_at"`
	}{AttemptedAt: at})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// loginBlockerKind classifies why Alibaba refused an automatic sign-in.
type loginBlockerKind string

const (
	loginBlockerVerification loginBlockerKind = "verification"
	loginBlockerCaptcha      loginBlockerKind = "captcha"
	// loginBlockerUnknown is only used for a park marker that cannot be read.
	loginBlockerUnknown loginBlockerKind = "unknown"
	// loginBlockerPending marks the provisional guard written before an
	// automatic sign-in starts. It is converted to a real blocker kind when
	// the attempt reports one and removed when the attempt ends without a
	// blocker; a guard that survives means the attempt never finished, which
	// is treated as a possible blocker (fail closed).
	loginBlockerPending loginBlockerKind = "pending"
)

// pendingGuardReason is the reason stored in the provisional park guard.
const pendingGuardReason = "an automatic sign-in attempt started and did not finish"

// loginBlockedError reports an interactive blocker (account verification or
// captcha) that a scripted password login cannot get past. Callers park
// automatic sign-in when they see one.
type loginBlockedError struct {
	Kind   loginBlockerKind
	Reason string
}

func (e *loginBlockedError) Error() string { return e.Reason }

type attemptRedactedError struct {
	cause error
	text  string
}

func (e attemptRedactedError) Error() string { return e.text }
func (e attemptRedactedError) Unwrap() error { return e.cause }

// loginBlockerSignals is what the passport-frame probe observed.
type loginBlockerSignals struct {
	Checkcode bool   `json:"checkcode"`
	Captcha   bool   `json:"captcha"`
	Text      string `json:"text"`
	FormError string `json:"err"`
}

// verificationTextMarkers are lower-case fragments of Alibaba's
// account-protection copy.
var verificationTextMarkers = []string{
	"protect your account",
	"complete the verification",
	"account protection",
	"identity verification",
	"security verification",
}

func looksLikeVerificationText(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range verificationTextMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// classifyLoginBlocker decides what the sign-in page is showing. Verification
// evidence wins over captcha evidence: the account-protection page can sit
// next to elements that match the captcha selector without any slider.
// Form errors come back untyped (kind "") with a "sign-in rejected" reason;
// they are not interactive blockers and never park auto-login.
func classifyLoginBlocker(tmdFrame bool, passport loginBlockerSignals, topText string) (loginBlockerKind, string) {
	if passport.Checkcode || looksLikeVerificationText(passport.Text) || looksLikeVerificationText(topText) {
		return loginBlockerVerification, verificationBlockedReason
	}
	if tmdFrame || passport.Captcha {
		return loginBlockerCaptcha, captchaBlockedReason
	}
	if passport.FormError != "" {
		return "", "sign-in rejected: " + passport.FormError
	}
	return "", ""
}

// The park marker survives across processes (each poll is a new process).
// While it is younger than its TTL, automatic sign-in is refused outright:
// repeating scripted password logins against an interactive blocker only
// feeds Alibaba's risk control. A usable session still clears it immediately
// (see clearLoginPark callers). The attempt cooldown alone never clears it;
// only a successful session or the park TTL does.
func loginParkMarker() string {
	return filepath.Join(config.ThrottlesDir(), "modelstudio-login-park.json")
}

type loginPark struct {
	Kind     loginBlockerKind `json:"kind"`
	Reason   string           `json:"reason"`
	ParkedAt time.Time        `json:"parked_at"`
}

// loginParkMaxAge is how long a park of the given kind stays in force.
func loginParkMaxAge(kind loginBlockerKind) time.Duration {
	if kind == loginBlockerPending {
		return loginParkPendingTTL
	}
	return loginParkTTL
}

// loadLoginPark reports whether auto-login is parked. A marker that exists
// but cannot be read or parsed still counts as parked (kind "unknown"): the
// brake must fail closed while it is younger than the park TTL. Once the TTL
// elapses the marker is removed so automatic sign-in can try again (still
// subject to loginCooldown).
func loadLoginPark() (loginPark, bool) {
	path := loginParkMarker()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loginPark{}, false
		}
		return loginPark{Kind: loginBlockerUnknown, Reason: "park marker unreadable: " + err.Error()}, true
	}
	var park loginPark
	if err := json.Unmarshal(data, &park); err != nil {
		// Corrupt markers still park, aged from the file mtime so a stuck
		// unreadable file cannot block forever past the TTL.
		park = loginPark{Kind: loginBlockerUnknown, Reason: "park marker unparseable"}
		if info, statErr := os.Stat(path); statErr == nil {
			park.ParkedAt = info.ModTime()
		}
	}
	if park.Kind == "" {
		park.Kind = loginBlockerUnknown
	}
	ageAt := park.ParkedAt
	if ageAt.IsZero() {
		if info, statErr := os.Stat(path); statErr == nil {
			ageAt = info.ModTime()
			park.ParkedAt = ageAt
		}
	}
	if !ageAt.IsZero() && time.Since(ageAt) >= loginParkMaxAge(park.Kind) {
		clearLoginPark()
		return loginPark{}, false
	}
	return park, true
}

// acquireAutoLoginLock takes the cross-process lock that serializes automatic
// sign-in attempts. The lock file is persistent and never removed, so every
// process locks the same inode. The OS releases the lock if the holder dies.
func acquireAutoLoginLock() (func(), error) {
	path := filepath.Join(config.ThrottlesDir(), "modelstudio-login.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryAutoLoginLock(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unlockAutoLoginFile(file)
		_ = file.Close()
	}, nil
}

// acquireModelStudioLoginLock is swapped out in tests.
var acquireModelStudioLoginLock = acquireAutoLoginLock

// writeLoginParkFile persists the park marker. It is a seam so tests can
// inject write failures without relying on file permissions.
var writeLoginParkFile = func(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func writeLoginPark(kind loginBlockerKind, reason string, at time.Time) error {
	data, err := json.Marshal(loginPark{Kind: kind, Reason: reason, ParkedAt: at})
	if err != nil {
		return err
	}
	return writeLoginParkFile(loginParkMarker(), data)
}

// clearLoginPark removes the marker; a missing marker is not an error.
func clearLoginPark() {
	if err := os.Remove(loginParkMarker()); err != nil && !errors.Is(err, os.ErrNotExist) {
		logging.FromContext(context.Background()).Debug("clearing modelstudio login park failed", "err", err)
	}
}

func verificationCodePath() string {
	return filepath.Join(config.DataDir(), verificationCodeFilename)
}

func assistedLoginEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(assistedLoginEnv))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

func validateVerificationCode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	if len(code) < 4 || len(code) > 8 {
		return "", errors.New("verification code must be 4-8 digits")
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return "", errors.New("verification code must be 4-8 digits")
		}
	}
	return code, nil
}

// DeliverVerificationCode validates a one-time SMS verification code and writes
// it atomically with mode 0600 so an in-flight assisted Model Studio sign-in can
// consume it exactly once. The code is never included in errors or logs.
func DeliverVerificationCode(raw string) error {
	code, err := validateVerificationCode(raw)
	if err != nil {
		return err
	}
	path := verificationCodePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create verification code directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".modelstudio-code.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary verification code file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("chmod temporary verification code file: %w", err)
	}
	if _, err := tmp.WriteString(code); err != nil {
		return fmt.Errorf("write verification code file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync verification code file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close verification code file: %w", err)
	}
	if err := replaceVerificationCodeFile(tmpPath, path); err != nil {
		return fmt.Errorf("install verification code file: %w", err)
	}
	cleanupTmp = false
	_ = os.Chmod(path, 0o600)
	return nil
}

// consumeVerificationCode atomically claims, reads, zeroes, and deletes the
// one-use verification code file. Even if the file contains invalid bytes, it
// is zeroed and removed on the first read attempt.
func consumeVerificationCode() (string, bool) {
	path := verificationCodePath()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	claimPath := filepath.Join(
		filepath.Dir(path),
		fmt.Sprintf(".modelstudio-code.claim-%d-%d", os.Getpid(), time.Now().UnixNano()),
	)
	if err := replaceVerificationCodeFile(path, claimPath); err != nil {
		return "", false
	}
	defer func() {
		zeroSize := int(info.Size())
		if zeroSize < 64 {
			zeroSize = 64
		}
		if f, openErr := os.OpenFile(claimPath, os.O_WRONLY, 0o600); openErr == nil {
			_, _ = f.Write(make([]byte, zeroSize))
			_ = f.Sync()
			_ = f.Close()
		}
		_ = os.Remove(claimPath)
	}()

	data, err := os.ReadFile(claimPath)
	if err != nil {
		return "", false
	}
	code, validErr := validateVerificationCode(string(data))
	for i := range data {
		data[i] = 0
	}
	if validErr != nil {
		return "", false
	}
	if time.Since(info.ModTime()) > loginOverallDeadline {
		return "", false
	}
	return code, true
}

func purgeStaleVerificationCode() {
	info, err := os.Lstat(verificationCodePath())
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if info.Size() == 0 || time.Since(info.ModTime()) > loginOverallDeadline {
		_, _ = consumeVerificationCode()
	}
}

// loginGuidancePort names the headed-Chrome port an operator should sign in
// on: login_port, else the first cdp_ports entry, else the literal setting
// name so the message still tells them what to configure.
func loginGuidancePort() string {
	pc := config.Get().Providers["modelstudio"]
	if pc.LoginPort > 0 {
		if port := normalizeCDPPort(strconv.Itoa(pc.LoginPort)); port != "" {
			return port
		}
	}
	if ports, explicit := cdpFixedPorts(pc.CDPPorts); explicit && len(ports) > 0 {
		return ports[0]
	}
	return "login_port"
}

// classWording is the operator-facing name of a blocker kind.
func (k loginBlockerKind) classWording() string {
	switch k {
	case loginBlockerVerification:
		return "Alibaba account verification (SMS / account-protection code)"
	case loginBlockerCaptcha:
		return "a slider captcha (Alibaba risk control)"
	case loginBlockerPending:
		return "an automatic sign-in attempt that did not finish (possible Alibaba blocker)"
	default:
		return "an Alibaba sign-in blocker"
	}
}

// loginParkedError is returned instead of attempting a sign-in while parked.
type loginParkedError struct {
	Kind loginBlockerKind
	Port string
	// MarkerErr is set when converting the provisional guard into the real
	// park failed; the provisional guard still keeps auto-login parked.
	MarkerErr error
}

func (e *loginParkedError) Error() string {
	msg := fmt.Sprintf("%s after %s: sign in manually at %s in the Chrome on port %s, then retry; it clears automatically once a signed-in session is imported or saved",
		errAutoLoginParked.Error(), e.Kind.classWording(), modelStudioDashboardURL, e.Port)
	if e.MarkerErr != nil {
		msg += fmt.Sprintf(" (park marker update failed: %v; a provisional guard keeps auto-login parked)", e.MarkerErr)
	}
	return msg
}

func (e *loginParkedError) Unwrap() error { return errAutoLoginParked }

// loadLoginCredentials runs the configured command and parses its output.
// Accepted shapes: a JSON object with username/password (aliases: email,
// login, user), or the array `op item get --fields ... --format json` prints.
func loadLoginCredentials(ctx context.Context, command string) (loginCredentials, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, loginCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "/bin/sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return loginCredentials{}, fmt.Errorf("login_command failed: %s", detail)
	}
	creds, err := parseLoginCredentials(stdout.Bytes())
	if err != nil {
		return loginCredentials{}, fmt.Errorf("login_command output: %w", err)
	}
	return creds, nil
}

func parseLoginCredentials(data []byte) (loginCredentials, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return loginCredentials{}, errors.New("empty output")
	}

	values := make(map[string]string)
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return loginCredentials{}, fmt.Errorf("invalid JSON: %w", err)
		}
		for key, raw := range obj {
			var value string
			if err := json.Unmarshal(raw, &value); err == nil {
				values[strings.ToLower(key)] = value
			}
		}
	case '[':
		var fields []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			return loginCredentials{}, fmt.Errorf("invalid JSON: %w", err)
		}
		for _, field := range fields {
			if field.Label != "" {
				values[strings.ToLower(field.Label)] = field.Value
			}
			if field.ID != "" {
				if _, taken := values[strings.ToLower(field.ID)]; !taken {
					values[strings.ToLower(field.ID)] = field.Value
				}
			}
		}
	default:
		return loginCredentials{}, errors.New("expected a JSON object or array")
	}

	creds := loginCredentials{
		Username: firstNonEmpty(values, "username", "email", "login", "user"),
		Password: firstNonEmpty(values, "password"),
	}
	if creds.Username == "" || creds.Password == "" {
		return loginCredentials{}, errors.New("missing username or password")
	}
	return creds, nil
}

func firstNonEmpty(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

// performModelStudioAutoLogin opens the sign-in page in a new tab of the
// headed browser on port, fills the passport form, submits, waits for the
// console redirect, and closes the tab. The browser's cookie jar keeps the
// session, so a normal CDP import afterwards picks it up.
func performModelStudioAutoLogin(ctx context.Context, port string, creds loginCredentials) (session browserSession, err error) {
	ctx, cancel := context.WithTimeout(ctx, loginOverallDeadline)
	defer cancel()
	logger := logging.FromContext(ctx).With("provider", "modelstudio", "step", "auto-login")
	started := time.Now()
	elapsed := func() time.Duration { return time.Since(started).Round(time.Millisecond) }

	loginURL := alibabaLoginURL + "?oauth_callback=" + url.QueryEscape(modelStudioDashboardURL)
	target, err := openCDPTarget(ctx, port, loginURL)
	if err != nil {
		return browserSession{}, fmt.Errorf("open sign-in tab on CDP :%s: %w", port, err)
	}
	defer closeCDPTarget(port, target.ID)
	logger.Debug("opened sign-in tab", "port", port, "elapsed", elapsed())

	page, err := dialCDP(ctx, port, target.wsPath())
	if err != nil {
		return browserSession{}, fmt.Errorf("attach to sign-in tab: %w", err)
	}
	defer page.Close()
	page.rememberSecret(creds.Password)
	defer func() {
		if err != nil {
			var blocked *loginBlockedError
			if errors.As(err, &blocked) {
				blocked.Reason = page.redactSecrets(blocked.Reason)
			}
			err = attemptRedactedError{
				cause: err,
				text:  page.redactSecrets(err.Error()),
			}
		}
	}()

	if err := page.call("Page.enable", nil, nil); err != nil {
		return browserSession{}, err
	}

	// The passport form is (re)initialised by its own scripts shortly after
	// it appears, which can wipe values typed too early. Fill, verify right
	// before submitting, and retry when the page neither redirects nor
	// reports a problem.
	var lastErr error
	for attempt := 1; attempt <= loginMaxAttempts; attempt++ {
		formCtx, err := page.waitForFrameWorld(ctx, passportFrameHost, loginUsernameSel, loginFormWait)
		if err != nil {
			// The form never showed up. An interactive blocker (account
			// verification page, TMD punish frame) can replace it before
			// any credentials are typed; classify only now that the wait
			// has failed so a normal sign-in page is never mistaken for one.
			kind, reason := page.loginBlockerReason()
			if kind == loginBlockerVerification && assistedLoginEnabled() {
				session, assistErr := page.completeAssistedVerification(ctx, elapsed)
				if assistErr == nil {
					return session, nil
				}
				var assistBlocked *loginBlockedError
				if errors.As(assistErr, &assistBlocked) {
					assistBlocked.Reason = page.redactSecrets(assistBlocked.Reason)
				}
				diag := page.failureDiagnostics()
				logger.Debug("sign-in form did not appear", "elapsed", elapsed(), "err", page.redactSecrets(assistErr.Error()), "blocker", string(kind),
					"url", diag.URL, "frames", diag.Frames, "form_text", diag.FormText, "screenshot", diag.Screenshot)
				return browserSession{}, fmt.Errorf("sign-in form did not appear: %w", assistErr)
			}
			diag := page.failureDiagnostics()
			logger.Debug("sign-in form did not appear", "elapsed", elapsed(), "err", page.redactSecrets(err.Error()), "blocker", string(kind),
				"url", diag.URL, "frames", diag.Frames, "form_text", diag.FormText, "screenshot", diag.Screenshot)
			if kind != "" {
				return browserSession{}, fmt.Errorf("sign-in form did not appear: %w", &loginBlockedError{Kind: kind, Reason: page.redactSecrets(reason)})
			}
			return browserSession{}, fmt.Errorf("sign-in form did not appear: %w", err)
		}
		if attempt == 1 {
			logger.Debug("sign-in form ready", "elapsed", elapsed())
		}

		if err := page.fillAndSubmit(formCtx, creds); err != nil {
			// A stale execution context means the frame reloaded under us;
			// just go around and re-acquire it.
			lastErr = err
			logger.Debug("filling sign-in form failed", "attempt", attempt, "err", page.redactSecrets(err.Error()))
			continue
		}
		logger.Debug("submitted sign-in form", "attempt", attempt, "elapsed", elapsed())

		err = page.waitForLoginOutcome(ctx, loginRedirectWait)
		if err == nil {
			// The redirect to the console only starts the SSO ticket
			// handshake that mints the session cookies, and those cookies
			// are scoped to this tab's context and disappear once it
			// closes — so capture the Cookie header before returning.
			session, captureErr := page.captureSession(ctx, loginCookieWait)
			if captureErr != nil {
				return browserSession{}, captureErr
			}
			logger.Debug("signed in", "elapsed", elapsed())
			return session, nil
		}
		var blocked *loginBlockedError
		if errors.As(err, &blocked) && blocked.Kind == loginBlockerVerification && assistedLoginEnabled() {
			session, assistErr := page.completeAssistedVerification(ctx, elapsed)
			if assistErr == nil {
				return session, nil
			}
			lastErr = assistErr
			break
		}
		lastErr = err
		if !errors.Is(err, errLoginNoRedirect) {
			break
		}
		logger.Debug("no redirect after submit; retrying", "attempt", attempt, "elapsed", elapsed())
	}

	var lastBlocked *loginBlockedError
	if errors.As(lastErr, &lastBlocked) {
		lastBlocked.Reason = page.redactSecrets(lastBlocked.Reason)
	}
	diag := page.failureDiagnostics()
	errText := ""
	if lastErr != nil {
		errText = page.redactSecrets(lastErr.Error())
	}
	logger.Debug("sign-in did not complete", "elapsed", elapsed(), "err", errText,
		"url", diag.URL, "frames", diag.Frames, "form_text", diag.FormText, "screenshot", diag.Screenshot)
	if diag.FormText != "" {
		return browserSession{}, fmt.Errorf("%w (sign-in page showed: %s)", lastErr, diag.FormText)
	}
	return browserSession{}, lastErr
}

// fillAndSubmit types both credentials, confirms the form still holds them,
// and clicks submit.
func (c *cdpConn) fillAndSubmit(formCtx int, creds loginCredentials) error {
	if err := c.typeInto(formCtx, loginUsernameSel, creds.Username); err != nil {
		return fmt.Errorf("enter username: %w", err)
	}
	if err := c.typeInto(formCtx, loginPasswordSel, creds.Password); err != nil {
		return fmt.Errorf("enter password: %w", err)
	}
	// Re-read both just before submitting: the page may have reset the form
	// between the two inserts.
	for _, field := range []struct{ sel, want string }{{loginUsernameSel, creds.Username}, {loginPasswordSel, creds.Password}} {
		var value string
		if err := c.evalInContext(formCtx, "document.querySelector("+strconv.Quote(field.sel)+").value", &value); err != nil {
			return err
		}
		if value != field.want {
			return fmt.Errorf("%s was cleared before submit", field.sel)
		}
	}
	return c.evalInContext(formCtx, "document.querySelector("+strconv.Quote(loginSubmitSel)+").click()", nil)
}

type cdpTarget struct {
	ID                   string `json:"id"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func (t cdpTarget) wsPath() string {
	if parsed, err := url.Parse(t.WebSocketDebuggerURL); err == nil && parsed.Path != "" {
		return parsed.Path
	}
	return "/devtools/page/" + t.ID
}

func openCDPTarget(ctx context.Context, port, pageURL string) (cdpTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://127.0.0.1:"+port+"/json/new?"+pageURL, nil)
	if err != nil {
		return cdpTarget{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cdpTarget{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cdpTarget{}, fmt.Errorf("/json/new status %d", resp.StatusCode)
	}
	var target cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		return cdpTarget{}, err
	}
	if target.ID == "" {
		return cdpTarget{}, errors.New("/json/new returned no target id")
	}
	return target, nil
}

func closeCDPTarget(port, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/json/close/"+id, nil)
	if err != nil {
		return
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

type cdpFrame struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type cdpFrameTree struct {
	Frame       cdpFrame       `json:"frame"`
	ChildFrames []cdpFrameTree `json:"childFrames"`
}

// any reports whether some frame in the tree satisfies match.
func (t cdpFrameTree) any(match func(cdpFrame) bool) bool {
	if match(t.Frame) {
		return true
	}
	for _, child := range t.ChildFrames {
		if child.any(match) {
			return true
		}
	}
	return false
}

func (t cdpFrameTree) find(host string) (cdpFrame, bool) {
	if parsed, err := url.Parse(t.Frame.URL); err == nil && parsed.Hostname() == host {
		return t.Frame, true
	}
	for _, child := range t.ChildFrames {
		if frame, ok := child.find(host); ok {
			return frame, true
		}
	}
	return cdpFrame{}, false
}

// waitForFrameWorld polls until a frame on frameHost contains selector, then
// returns an isolated-world execution context for that frame. The passport
// form is a cross-origin iframe, so page-level evaluation cannot reach it.
func (c *cdpConn) waitForFrameWorld(ctx context.Context, frameHost, selector string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		var tree struct {
			FrameTree cdpFrameTree `json:"frameTree"`
		}
		if err := c.call("Page.getFrameTree", nil, &tree); err != nil {
			return 0, err
		}
		if frame, ok := tree.FrameTree.find(frameHost); ok {
			var world struct {
				ExecutionContextID int `json:"executionContextId"`
			}
			params := map[string]any{"frameId": frame.ID, "worldName": "vibeusage"}
			if err := c.call("Page.createIsolatedWorld", params, &world); err == nil {
				var ready bool
				expr := "document.readyState === \"complete\" && !!document.querySelector(" + strconv.Quote(selector) + ")"
				if err := c.evalInContext(world.ExecutionContextID, expr, &ready); err == nil && ready {
					// Give the frame's own scripts a moment to finish
					// (re)building the form, then confirm it is still there.
					select {
					case <-ctx.Done():
						return 0, ctx.Err()
					case <-time.After(loginFormSettle):
					}
					var still bool
					if err := c.evalInContext(world.ExecutionContextID, expr, &still); err == nil && still {
						return world.ExecutionContextID, nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no %s frame with %s within %s", frameHost, selector, timeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

// evalInContext runs expression in executionContextId (0 = page default)
// and decodes its by-value result into out.
func (c *cdpConn) evalInContext(contextID int, expression string, out any) error {
	params := map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}
	if contextID != 0 {
		params["contextId"] = contextID
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call("Runtime.evaluate", params, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("page script error: %s", res.ExceptionDetails.Text)
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}

// typeInto focuses selector inside contextID and inserts text as keyboard
// input, then verifies the field holds it.
func (c *cdpConn) typeInto(contextID int, selector, text string) error {
	focus := "(() => { const el = document.querySelector(" + strconv.Quote(selector) + "); if (!el) return false; el.focus(); el.select && el.select(); return document.activeElement === el; })()"
	var focused bool
	if err := c.evalInContext(contextID, focus, &focused); err != nil {
		return err
	}
	if !focused {
		return fmt.Errorf("could not focus %s", selector)
	}
	if err := c.call("Input.insertText", map[string]any{"text": text}, nil); err != nil {
		return err
	}
	var value string
	if err := c.evalInContext(contextID, "document.querySelector("+strconv.Quote(selector)+").value", &value); err != nil {
		return err
	}
	if value != text {
		return fmt.Errorf("%s did not accept input", selector)
	}
	return nil
}

// waitForLoginOutcome polls the tab until it lands on the console (success)
// or shows a captcha / error on the sign-in form (failure).
func (c *cdpConn) waitForLoginOutcome(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	consoleHost, _ := url.Parse(modelStudioConsoleBaseURL)
	for {
		var href string
		if err := c.evalInContext(0, "location.href", &href); err == nil {
			if parsed, err := url.Parse(href); err == nil && parsed.Hostname() == consoleHost.Hostname() {
				return nil
			}
		}

		if kind, reason := c.loginBlockerReason(); kind != "" {
			return &loginBlockedError{Kind: kind, Reason: reason}
		} else if reason != "" {
			return errors.New(reason)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w within %s", errLoginNoRedirect, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

// captureSession polls the browser jar until the console's authentication
// cookies have landed, then builds the Cookie header while the sign-in tab is
// still open. The SSO ticket cookies are scoped to this tab's browsing
// context and disappear once it closes, so they cannot be re-imported later.
func (c *cdpConn) captureSession(ctx context.Context, timeout time.Duration) (browserSession, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		var res struct {
			Cookies []cdpCookie `json:"cookies"`
		}
		if callErr := c.call("Storage.getCookies", nil, &res); callErr != nil {
			lastErr = callErr
		} else if header, buildErr := buildModelStudioCookieHeader(
			toBrowserCookies(res.Cookies),
			modelStudioConsoleBaseURL+"/data/api.json",
			time.Now(),
		); buildErr != nil {
			lastErr = buildErr
		} else {
			return browserSession{Cookie: header, SourceLabel: "CDP auto-login"}, nil
		}

		if time.Now().After(deadline) {
			lastText := "<nil>"
			if lastErr != nil {
				lastText = c.redactSecrets(lastErr.Error())
			}
			return browserSession{}, fmt.Errorf("%w within %s (last: %s)", errLoginNoSessionCookies, timeout, lastText)
		}
		select {
		case <-ctx.Done():
			return browserSession{}, ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

// loginPageTextExpr reads the top-level page's visible text. The marker
// comment lets the test fake recognise it.
const loginPageTextExpr = `(() => { /* vibeusage-page-text */ return (document.body && document.body.innerText || "").replace(/\s+/g, " ").trim().slice(0, 600); })()`

const loginClickGetCodeExpr = `(() => { /* vibeusage-click-get-code */
	const visible = el => !!el && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== "hidden";
	const direct = [...document.querySelectorAll(` + `"` + loginGetCodeSel + `"` + `)].find(visible);
	if (direct) { direct.click(); return true; }
	const candidates = [...document.querySelectorAll("button, a, input[type=button]")].filter(visible);
	for (const el of candidates) {
		const text = ((el.innerText || el.value || "").replace(/\s+/g, " ").trim()).toLowerCase();
		if (text.includes("get code") || text.includes("send code") || text.includes("获取验证码")) {
			el.click();
			return true;
		}
	}
	return false;
})()`

func (c *cdpConn) passportWorld() (int, error) {
	var tree struct {
		FrameTree cdpFrameTree `json:"frameTree"`
	}
	if err := c.call("Page.getFrameTree", nil, &tree); err != nil {
		return 0, err
	}
	frame, ok := tree.FrameTree.find(passportFrameHost)
	if !ok {
		return 0, fmt.Errorf("no %s frame", passportFrameHost)
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := c.call("Page.createIsolatedWorld", map[string]any{"frameId": frame.ID, "worldName": "vibeusage"}, &world); err != nil {
		return 0, err
	}
	return world.ExecutionContextID, nil
}

func (c *cdpConn) rememberSecret(secret string) {
	if s := strings.TrimSpace(secret); s != "" {
		c.redactedSecrets = append(c.redactedSecrets, s)
	}
	if secret != "" && secret != strings.TrimSpace(secret) {
		c.redactedSecrets = append(c.redactedSecrets, secret)
	}
}

func (c *cdpConn) redactSecrets(text string) string {
	if text == "" {
		return ""
	}
	out := text
	for _, secret := range c.redactedSecrets {
		if secret != "" {
			out = strings.ReplaceAll(out, secret, "[REDACTED]")
		}
	}
	return out
}

func boundedLoginWait(ctx context.Context, requested time.Duration) time.Duration {
	wait := requested
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		headroom := loginDiagnosticsHeadroom
		if maxHeadroom := remaining / 3; headroom > maxHeadroom {
			headroom = maxHeadroom
		}
		if maxWait := remaining - headroom; maxWait < wait {
			if maxWait < 0 {
				maxWait = 0
			}
			wait = maxWait
		}
	}
	return wait
}

// completeAssistedVerification clicks "Get code" at most once and waits up to
// loginVerificationWait (bounded by ctx / loginOverallDeadline) for a one-use
// verification code file delivered via DeliverVerificationCode.
func (c *cdpConn) completeAssistedVerification(ctx context.Context, elapsed func() time.Duration) (browserSession, error) {
	logger := logging.FromContext(ctx).With("provider", "modelstudio", "step", "auto-login")
	waitCtx, cancel := context.WithTimeout(ctx, boundedLoginWait(ctx, loginVerificationWait))
	defer cancel()
	if waitCtx.Err() != nil {
		return browserSession{}, &loginBlockedError{
			Kind:   loginBlockerVerification,
			Reason: verificationBlockedReason,
		}
	}
	purgeStaleVerificationCode()

	clickedGetCode := false
	for {
		if waitCtx.Err() != nil {
			return browserSession{}, &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason,
			}
		}
		if !clickedGetCode {
			clickedGetCode = true
			if worldID, err := c.passportWorld(); err == nil {
				if waitCtx.Err() != nil {
					return browserSession{}, &loginBlockedError{
						Kind:   loginBlockerVerification,
						Reason: verificationBlockedReason,
					}
				}
				var clicked bool
				_ = c.evalInContext(worldID, loginClickGetCodeExpr, &clicked)
				logger.Debug("requested verification code", "clicked", clicked, "elapsed", elapsed())
			}
		}
		if waitCtx.Err() != nil {
			return browserSession{}, &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason,
			}
		}
		if code, ok := consumeVerificationCode(); ok {
			c.rememberSecret(code)
			if waitCtx.Err() != nil {
				return browserSession{}, &loginBlockedError{
					Kind:   loginBlockerVerification,
					Reason: verificationBlockedReason,
				}
			}
			if err := c.submitVerificationCode(code); err != nil {
				return browserSession{}, &loginBlockedError{
					Kind:   loginBlockerVerification,
					Reason: fmt.Sprintf("%s (%s)", verificationBlockedReason, c.redactSecrets(err.Error())),
				}
			}
			logger.Debug("submitted assisted verification code", "elapsed", elapsed())
			if err := c.waitForVerificationOutcome(ctx, loginRedirectWait); err != nil {
				var blocked *loginBlockedError
				if errors.As(err, &blocked) {
					blocked.Reason = c.redactSecrets(blocked.Reason)
				}
				return browserSession{}, err
			}
			session, err := c.captureSession(ctx, loginCookieWait)
			if err != nil {
				return browserSession{}, err
			}
			logger.Debug("signed in after assisted verification", "elapsed", elapsed())
			return session, nil
		}

		select {
		case <-waitCtx.Done():
			return browserSession{}, &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason,
			}
		case <-time.After(loginPollInterval):
		}
	}
}

func (c *cdpConn) submitVerificationCode(code string) error {
	c.rememberSecret(code)
	worldID, err := c.passportWorld()
	if err != nil {
		return err
	}
	if err := c.typeInto(worldID, loginCheckcodeSel, code); err != nil {
		return fmt.Errorf("enter verification code: %w", err)
	}
	return c.evalInContext(worldID, "document.querySelector("+strconv.Quote(loginSubmitSel)+").click()", nil)
}

func (c *cdpConn) waitForVerificationOutcome(ctx context.Context, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, boundedLoginWait(ctx, loginOverallDeadline))
	defer cancel()
	deadline := time.Now().Add(timeout)
	consoleHost, _ := url.Parse(modelStudioConsoleBaseURL)
	for {
		if waitCtx.Err() != nil {
			return &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason,
			}
		}
		var href string
		if err := c.evalInContext(0, "location.href", &href); err == nil {
			if parsed, err := url.Parse(href); err == nil && parsed.Hostname() == consoleHost.Hostname() {
				return nil
			}
		}

		tmdFrame, signals, topText := c.readLoginBlockerSignals()
		if signals.FormError != "" {
			_, formReason := classifyLoginBlocker(false, loginBlockerSignals{FormError: signals.FormError}, "")
			formReason = c.redactSecrets(formReason)
			return &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason + " (" + formReason + ")",
			}
		}
		if kind, reason := classifyLoginBlocker(tmdFrame, signals, topText); kind == loginBlockerCaptcha {
			return &loginBlockedError{
				Kind:   loginBlockerCaptcha,
				Reason: c.redactSecrets(reason),
			}
		}

		if time.Now().After(deadline) {
			return &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason,
			}
		}
		select {
		case <-waitCtx.Done():
			return &loginBlockedError{
				Kind:   loginBlockerVerification,
				Reason: verificationBlockedReason,
			}
		case <-time.After(loginPollInterval):
		}
	}
}

func (c *cdpConn) readLoginBlockerSignals() (bool, loginBlockerSignals, string) {
	var tree struct {
		FrameTree cdpFrameTree `json:"frameTree"`
	}
	if err := c.call("Page.getFrameTree", nil, &tree); err != nil {
		return false, loginBlockerSignals{}, ""
	}
	tmdFrame := tree.FrameTree.any(func(f cdpFrame) bool { return strings.Contains(f.URL, loginCaptchaFramePath) })

	var signals loginBlockerSignals
	if frame, ok := tree.FrameTree.find(passportFrameHost); ok {
		var world struct {
			ExecutionContextID int `json:"executionContextId"`
		}
		if err := c.call("Page.createIsolatedWorld", map[string]any{"frameId": frame.ID, "worldName": "vibeusage"}, &world); err == nil {
			expr := `(() => { /* vibeusage-login-probe */
		const visible = el => !!el && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== "hidden";
		const q = s => [...document.querySelectorAll(s)];
		const err = q(` + strconv.Quote(loginErrorSel) + `).filter(visible).map(e => e.textContent.trim()).filter(Boolean)[0];
		return {
			checkcode: visible(document.querySelector(` + strconv.Quote(loginCheckcodeSel) + `)),
			captcha: q(` + strconv.Quote(loginCaptchaSel) + `).some(visible),
			text: (document.body && document.body.innerText || "").replace(/\s+/g, " ").trim().slice(0, 600),
			err: err ? err.slice(0, 120) : ""
		};
	})()`
			// A failed probe leaves the zero signals; the frame tree and
			// top-level text can still classify the page.
			_ = c.evalInContext(world.ExecutionContextID, expr, &signals)
		}
	}

	var topText string
	_ = c.evalInContext(0, loginPageTextExpr, &topText)

	return tmdFrame, signals, topText
}

// loginBlockerReason inspects the sign-in tab for an interactive blocker
// (account verification or captcha) or a form error. It returns a zero
// kind with an empty reason when nothing blocking is shown, and a zero kind
// with a reason for a plain form error.
func (c *cdpConn) loginBlockerReason() (loginBlockerKind, string) {
	tmdFrame, signals, topText := c.readLoginBlockerSignals()
	return classifyLoginBlocker(tmdFrame, signals, topText)
}

type loginDiagnostics struct {
	URL        string
	Frames     []string
	FormText   string
	Screenshot string
}

// failureDiagnostics captures what the sign-in tab looked like when the
// flow gave up: URL, frame URLs, the passport form's visible text, and a
// screenshot in the cache directory. Best-effort; never fails.
func (c *cdpConn) failureDiagnostics() loginDiagnostics {
	var diag loginDiagnostics
	_ = c.evalInContext(0, "location.href", &diag.URL)

	var tree struct {
		FrameTree cdpFrameTree `json:"frameTree"`
	}
	if err := c.call("Page.getFrameTree", nil, &tree); err == nil {
		var walk func(cdpFrameTree)
		walk = func(node cdpFrameTree) {
			if u, err := url.Parse(node.Frame.URL); err == nil && u.Host != "" {
				diag.Frames = append(diag.Frames, u.Host+u.Path)
			}
			for _, child := range node.ChildFrames {
				walk(child)
			}
		}
		walk(tree.FrameTree)
		if frame, ok := tree.FrameTree.find(passportFrameHost); ok {
			var world struct {
				ExecutionContextID int `json:"executionContextId"`
			}
			if err := c.call("Page.createIsolatedWorld", map[string]any{"frameId": frame.ID, "worldName": "vibeusage"}, &world); err == nil {
				expr := `(() => { const t = (document.body && document.body.innerText || "").replace(/\s+/g, " ").trim(); return t.slice(0, 240); })()`
				_ = c.evalInContext(world.ExecutionContextID, expr, &diag.FormText)
			}
		}
	}

	var shot struct {
		Data string `json:"data"`
	}
	if err := c.call("Page.captureScreenshot", map[string]any{"format": "png"}, &shot); err == nil && shot.Data != "" {
		if png, err := base64.StdEncoding.DecodeString(shot.Data); err == nil {
			path := filepath.Join(config.CacheDir(), "modelstudio-login-failure.png")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil && os.WriteFile(path, png, 0o600) == nil {
				diag.Screenshot = path
			}
		}
	}
	diag.URL = c.redactSecrets(diag.URL)
	for i := range diag.Frames {
		diag.Frames[i] = c.redactSecrets(diag.Frames[i])
	}
	diag.FormText = c.redactSecrets(diag.FormText)
	return diag
}
