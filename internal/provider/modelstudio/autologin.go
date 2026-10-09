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
	loginCaptchaSel   = ".nc-container, #nc_1_n1z, .nc_wrapper"
	// Alibaba's anti-bot layer ("TMD") injects the slider captcha as a nested
	// iframe under this path; its presence is the reliable signal.
	loginCaptchaFramePath = "/_____tmd_____/punish"
	captchaBlockedReason  = "sign-in blocked by a slider captcha (Alibaba risk control after repeated logins); sign in manually once in this browser"
	loginErrorSel         = ".fm-error, .error-message, .next-form-item-help"
	loginFormWait         = 20 * time.Second
	loginMaxAttempts      = 3
	loginPollInterval     = 500 * time.Millisecond
	loginCommandTimeout   = 30 * time.Second
	loginOverallDeadline  = 90 * time.Second
	// loginCooldown is the minimum gap between automatic sign-in attempts,
	// whatever their outcome. Repeated logins are what trip Alibaba's risk
	// control (slider captcha), so a broken flow must fail quietly, not loop.
	loginCooldown = 10 * time.Minute
)

// Tunable so tests can run the retry path quickly.
var (
	loginFormSettle   = 750 * time.Millisecond
	loginRedirectWait = 15 * time.Second
	loginCookieWait   = 15 * time.Second
)

var (
	errAutoLoginUnavailable  = errors.New("automatic sign-in not configured")
	errAutoLoginCoolingDown  = errors.New("automatic sign-in attempted recently")
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
func performModelStudioAutoLogin(ctx context.Context, port string, creds loginCredentials) (browserSession, error) {
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
			return browserSession{}, fmt.Errorf("sign-in form did not appear: %w", err)
		}
		if attempt == 1 {
			logger.Debug("sign-in form ready", "elapsed", elapsed())
		}

		if err := page.fillAndSubmit(formCtx, creds); err != nil {
			// A stale execution context means the frame reloaded under us;
			// just go around and re-acquire it.
			lastErr = err
			logger.Debug("filling sign-in form failed", "attempt", attempt, "err", err)
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
		lastErr = err
		if !errors.Is(err, errLoginNoRedirect) {
			break
		}
		logger.Debug("no redirect after submit; retrying", "attempt", attempt, "elapsed", elapsed())
	}

	diag := page.failureDiagnostics()
	logger.Debug("sign-in did not complete", "elapsed", elapsed(), "err", lastErr,
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

		if reason := c.loginBlockerReason(); reason != "" {
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
			return browserSession{}, fmt.Errorf("%w within %s (last: %v)", errLoginNoSessionCookies, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return browserSession{}, ctx.Err()
		case <-time.After(loginPollInterval):
		}
	}
}

// loginBlockerReason inspects the passport frame for a visible captcha or
// error message. It returns "" when nothing blocking is shown.
func (c *cdpConn) loginBlockerReason() string {
	var tree struct {
		FrameTree cdpFrameTree `json:"frameTree"`
	}
	if err := c.call("Page.getFrameTree", nil, &tree); err != nil {
		return ""
	}
	if tree.FrameTree.any(func(f cdpFrame) bool { return strings.Contains(f.URL, loginCaptchaFramePath) }) {
		return captchaBlockedReason
	}
	frame, ok := tree.FrameTree.find(passportFrameHost)
	if !ok {
		return ""
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := c.call("Page.createIsolatedWorld", map[string]any{"frameId": frame.ID, "worldName": "vibeusage"}, &world); err != nil {
		return ""
	}
	expr := `(() => {
		const visible = el => !!el && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== "hidden";
		const q = s => [...document.querySelectorAll(s)];
		if (q(` + strconv.Quote(loginCaptchaSel) + `).some(visible)) return ` + strconv.Quote(captchaBlockedReason) + `;
		if (visible(document.querySelector(` + strconv.Quote(loginCheckcodeSel) + `))) return "sign-in requires a verification code; sign in manually once in this browser";
		const err = q(` + strconv.Quote(loginErrorSel) + `).filter(visible).map(e => e.textContent.trim()).filter(Boolean)[0];
		if (err) return "sign-in rejected: " + err.slice(0, 120);
		return "";
	})()`
	var reason string
	if err := c.evalInContext(world.ExecutionContextID, expr, &reason); err != nil {
		return ""
	}
	return reason
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
	return diag
}
