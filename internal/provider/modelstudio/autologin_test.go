package modelstudio

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestParseLoginCredentials(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    loginCredentials
		wantErr bool
	}{
		{
			name:  "json object",
			input: `{"username":"me@example.com","password":"s3cret"}`,
			want:  loginCredentials{Username: "me@example.com", Password: "s3cret"},
		},
		{
			name:  "json object with email alias",
			input: `{"email":"me@example.com","password":"s3cret","extra":1}`,
			want:  loginCredentials{Username: "me@example.com", Password: "s3cret"},
		},
		{
			name: "op item get fields array",
			input: `[{"id":"username","label":"username","value":"me@example.com"},
			         {"id":"password","label":"password","value":"s3cret","type":"CONCEALED"}]`,
			want: loginCredentials{Username: "me@example.com", Password: "s3cret"},
		},
		{name: "missing password", input: `{"username":"me@example.com"}`, wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "not json", input: "user:pass", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLoginCredentials([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadLoginCredentialsRunsCommand(t *testing.T) {
	creds, err := loadLoginCredentials(context.Background(), `printf '{"username":"me@example.com","password":"s3cret"}'`)
	if err != nil {
		t.Fatalf("loadLoginCredentials: %v", err)
	}
	if creds.Username != "me@example.com" || creds.Password != "s3cret" {
		t.Errorf("creds = %+v", creds)
	}

	_, err = loadLoginCredentials(context.Background(), `echo boom >&2; exit 3`)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected stderr in error, got %v", err)
	}
}

func TestAutoLoginSettings(t *testing.T) {
	t.Setenv(cdpPortsEnv, "")
	setProviderConfig := func(pc config.ProviderConfig) {
		cfg := config.DefaultConfig()
		cfg.Providers["modelstudio"] = pc
		config.SetGlobal(cfg)
	}
	t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

	setProviderConfig(config.ProviderConfig{})
	if _, _, err := autoLoginSettings(); !errors.Is(err, errAutoLoginUnavailable) {
		t.Errorf("no command: err = %v", err)
	}

	setProviderConfig(config.ProviderConfig{LoginCommand: "cat creds.json"})
	if _, _, err := autoLoginSettings(); !errors.Is(err, errAutoLoginUnavailable) {
		t.Errorf("command without port: err = %v", err)
	}

	setProviderConfig(config.ProviderConfig{LoginCommand: "cat creds.json", CDPPorts: []int{9444, 9222}})
	if cmd, port, err := autoLoginSettings(); err != nil || cmd != "cat creds.json" || port != "9444" {
		t.Errorf("first cdp port: cmd=%q port=%q err=%v", cmd, port, err)
	}

	setProviderConfig(config.ProviderConfig{LoginCommand: "cat creds.json", CDPPorts: []int{9222}, LoginPort: 9445})
	if _, port, err := autoLoginSettings(); err != nil || port != "9445" {
		t.Errorf("explicit login_port: port=%q err=%v", port, err)
	}
}

func TestWebConsoleFetchAutoLogsInWhenNoBrowserSession(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(string) string { return successfulSummaryPayload() })
	useAutoLoginConfig(t)

	var imports atomic.Int32
	stubImporter(t, func(context.Context) (browserSession, error) {
		imports.Add(1)
		return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome (CDP :9444)", errBrowserCookieImportUnavailable)
	})
	var logins atomic.Int32
	stubAutoLogin(t, func(_ context.Context, port string, creds loginCredentials) (browserSession, error) {
		logins.Add(1)
		if port != "9444" || creds.Username != "me@example.com" || creds.Password != "s3cret" {
			t.Errorf("autologin called with port=%q creds=%+v", port, creds)
		}
		return browserSession{Cookie: "login_aliyunid_ticket=fresh; login_current_pk=account", SourceLabel: "CDP auto-login"}, nil
	})

	result, err := newTestStrategy(server.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
	if logins.Load() != 1 || imports.Load() != 1 {
		t.Errorf("logins=%d imports=%d, want 1 and 1 (the renewed session replaces the re-import)", logins.Load(), imports.Load())
	}
	data, _ := config.ReadCredential("modelstudio", "session")
	if !strings.Contains(string(data), "CDP auto-login") {
		t.Errorf("session not persisted after auto-login: %s", data)
	}
}

func TestWebConsoleFetchAutoLogsInWhenBrowserSessionExpired(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(cookie string) string {
		if strings.Contains(cookie, "login_aliyunid_ticket=fresh") {
			return successfulSummaryPayload()
		}
		return expiredSessionPayload()
	})
	useAutoLoginConfig(t)

	stubImporter(t, func(context.Context) (browserSession, error) {
		return browserSession{Cookie: "login_aliyunid_ticket=stale; login_current_pk=account", SourceLabel: "Chrome (CDP :9444)"}, nil
	})
	var logins atomic.Int32
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		logins.Add(1)
		return browserSession{Cookie: "login_aliyunid_ticket=fresh; login_current_pk=account", SourceLabel: "CDP auto-login"}, nil
	})

	result, err := newTestStrategy(server.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success after auto-login, got %+v", result)
	}
	if logins.Load() != 1 {
		t.Errorf("logins = %d, want 1", logins.Load())
	}
}

func TestWebConsoleFetchReportsAutoLoginFailureOnce(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
	useAutoLoginConfig(t)

	stubImporter(t, func(context.Context) (browserSession, error) {
		return browserSession{Cookie: "login_aliyunid_ticket=stale; login_current_pk=account", SourceLabel: "Chrome (CDP :9444)"}, nil
	})
	var logins atomic.Int32
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		logins.Add(1)
		return browserSession{}, errors.New("sign-in blocked by a slider captcha; sign in manually once in this browser")
	})

	result, err := newTestStrategy(server.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if result.Success || result.RetryAfter == nil {
		t.Fatalf("expected throttled failure, got %+v", result)
	}
	if logins.Load() != 1 {
		t.Errorf("logins = %d, want exactly 1", logins.Load())
	}
	for _, want := range []string{"Chrome (CDP :9444)", "automatic sign-in failed", "slider captcha"} {
		if !strings.Contains(result.Error, want) {
			t.Errorf("error missing %q: %s", want, result.Error)
		}
	}
}

func TestWebConsoleFetchSkipsAutoLoginWhenUnconfigured(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(string) string { return successfulSummaryPayload() })
	config.SetGlobal(config.DefaultConfig())

	stubImporter(t, func(context.Context) (browserSession, error) {
		return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome (CDP :9444)", errBrowserCookieImportUnavailable)
	})
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		t.Error("autologin must not run without login_command")
		return browserSession{}, nil
	})

	result, _ := newTestStrategy(server.URL).Fetch(context.Background())
	if result.Success || strings.Contains(result.Error, "automatic sign-in") {
		t.Errorf("unexpected result: %+v", result)
	}
}

// TestPerformModelStudioAutoLoginAgainstFakeChrome drives the real login
// routine against a scripted DevTools endpoint: it must find the passport
// frame, type into both fields, click submit, and observe the redirect.
func TestPerformModelStudioAutoLoginAgainstFakeChrome(t *testing.T) {
	chrome := newFakeLoginChrome(t)

	session, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	if err != nil {
		t.Fatalf("performModelStudioAutoLogin: %v", err)
	}
	if !strings.Contains(session.Cookie, "login_aliyunid_ticket=") || session.SourceLabel != "CDP auto-login" {
		t.Errorf("session not captured from the sign-in tab: %+v", session)
	}

	chrome.mu.Lock()
	defer chrome.mu.Unlock()
	if chrome.opened != 1 || chrome.closed != 1 {
		t.Errorf("tabs opened=%d closed=%d, want 1 and 1", chrome.opened, chrome.closed)
	}
	if got := strings.Join(chrome.typed, "|"); got != "me@example.com|s3cret" {
		t.Errorf("typed = %q", got)
	}
	if !chrome.submitted {
		t.Error("submit was never clicked")
	}
}

// TestPerformModelStudioAutoLoginRetriesWhenFormIsWiped reproduces the
// observed field failure: the passport page re-initialises after our input,
// the first submit is a no-op, and the flow must fill again.
func TestPerformModelStudioAutoLoginRetriesWhenFormIsWiped(t *testing.T) {
	chrome := newFakeLoginChrome(t)
	chrome.wipeFirstSubmit = true

	_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	if err != nil {
		t.Fatalf("performModelStudioAutoLogin: %v", err)
	}
	chrome.mu.Lock()
	defer chrome.mu.Unlock()
	if chrome.submits != 2 {
		t.Errorf("submits = %d, want 2 (one wiped, one real)", chrome.submits)
	}
	if got := strings.Join(chrome.typed, "|"); got != "me@example.com|s3cret|me@example.com|s3cret" {
		t.Errorf("typed = %q", got)
	}
}

func TestPerformModelStudioAutoLoginDetectsCaptcha(t *testing.T) {
	chrome := newFakeLoginChrome(t)
	chrome.captcha = true

	_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	if err == nil || !strings.Contains(err.Error(), "slider captcha") {
		t.Fatalf("expected captcha error, got %v", err)
	}
	var blocked *loginBlockedError
	if !errors.As(err, &blocked) || blocked.Kind != loginBlockerCaptcha {
		t.Errorf("expected captcha-kind *loginBlockedError, got %T %v", err, err)
	}
	chrome.mu.Lock()
	defer chrome.mu.Unlock()
	if chrome.closed != 1 {
		t.Errorf("tab should be closed after failure, closed=%d", chrome.closed)
	}
}

func TestPerformModelStudioAutoLoginDetectsVerification(t *testing.T) {
	chrome := newFakeLoginChrome(t)
	chrome.verification = true

	_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	var blocked *loginBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected *loginBlockedError, got %T %v", err, err)
	}
	if blocked.Kind != loginBlockerVerification {
		t.Errorf("kind = %q, want verification", blocked.Kind)
	}
	if !strings.Contains(err.Error(), "account verification") || strings.Contains(err.Error(), "slider captcha") {
		t.Errorf("verification reason mislabelled: %v", err)
	}
	chrome.mu.Lock()
	defer chrome.mu.Unlock()
	if chrome.closed != 1 {
		t.Errorf("tab should be closed after failure, closed=%d", chrome.closed)
	}
}

// TestPerformModelStudioAutoLoginVerificationBeatsCaptchaSignals reproduces
// the incident: the SMS page also matched the captcha selector and was
// labelled "slider captcha".
func TestPerformModelStudioAutoLoginVerificationBeatsCaptchaSignals(t *testing.T) {
	chrome := newFakeLoginChrome(t)
	chrome.verification = true
	chrome.captchaSel = true
	chrome.captcha = true

	_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	var blocked *loginBlockedError
	if !errors.As(err, &blocked) || blocked.Kind != loginBlockerVerification {
		t.Fatalf("expected verification blocker, got %v", err)
	}
	if strings.Contains(err.Error(), "slider captcha") {
		t.Errorf("verification page reported as slider captcha: %v", err)
	}
}

func TestClassifyLoginBlocker(t *testing.T) {
	cases := []struct {
		name     string
		tmd      bool
		passport loginBlockerSignals
		topText  string
		want     loginBlockerKind
		wantErr  string
	}{
		{name: "verification text in passport frame", passport: loginBlockerSignals{Text: "To protect your account, please complete the verification"}, want: loginBlockerVerification},
		{name: "verification text case-insensitive", passport: loginBlockerSignals{Text: "ACCOUNT PROTECTION"}, want: loginBlockerVerification},
		{name: "identity verification text", passport: loginBlockerSignals{Text: "Identity verification required"}, want: loginBlockerVerification},
		{name: "security verification text", passport: loginBlockerSignals{Text: "Security Verification"}, want: loginBlockerVerification},
		{name: "verification text at top level", topText: "Please protect your account", want: loginBlockerVerification},
		{name: "checkcode visible", passport: loginBlockerSignals{Checkcode: true}, want: loginBlockerVerification},
		{name: "TMD punish frame", tmd: true, want: loginBlockerCaptcha},
		{name: "captcha selector visible", passport: loginBlockerSignals{Captcha: true}, want: loginBlockerCaptcha},
		{
			name: "verification text + TMD frame + captcha selector is verification",
			tmd:  true,
			passport: loginBlockerSignals{
				Captcha: true,
				Text:    "To protect your account, please complete the verification",
			},
			want: loginBlockerVerification,
		},
		{name: "checkcode + TMD frame is verification", tmd: true, passport: loginBlockerSignals{Checkcode: true, Captcha: true}, want: loginBlockerVerification},
		{name: "form error is untyped", passport: loginBlockerSignals{FormError: "Incorrect password"}, wantErr: "sign-in rejected: Incorrect password"},
		{name: "captcha beats form error", tmd: true, passport: loginBlockerSignals{FormError: "x"}, want: loginBlockerCaptcha},
		{name: "nothing shown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, reason := classifyLoginBlocker(tc.tmd, tc.passport, tc.topText)
			if kind != tc.want {
				t.Fatalf("kind = %q, want %q (reason %q)", kind, tc.want, reason)
			}
			switch tc.want {
			case loginBlockerVerification:
				if reason != verificationBlockedReason || strings.Contains(reason, "slider captcha") {
					t.Errorf("verification reason = %q", reason)
				}
			case loginBlockerCaptcha:
				if reason != captchaBlockedReason || !strings.Contains(reason, "slider captcha") {
					t.Errorf("captcha reason = %q", reason)
				}
			default:
				if reason != tc.wantErr {
					t.Errorf("reason = %q, want %q", reason, tc.wantErr)
				}
			}
		})
	}
	if !strings.Contains(verificationBlockedReason, "account verification") {
		t.Errorf("verification reason lacks wording: %q", verificationBlockedReason)
	}
}

// --- helpers -------------------------------------------------------------

func useAutoLoginConfig(t *testing.T) {
	t.Helper()
	t.Setenv(cdpPortsEnv, "")
	cfg := config.DefaultConfig()
	cfg.Providers["modelstudio"] = config.ProviderConfig{
		CDPPorts:     []int{9444, 9222},
		LoginPort:    9444,
		LoginCommand: `printf '{"username":"me@example.com","password":"s3cret"}'`,
	}
	config.SetGlobal(cfg)
	t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })
}

func stubImporter(t *testing.T, fn func(context.Context) (browserSession, error)) {
	t.Helper()
	old := importModelStudioBrowserSession
	t.Cleanup(func() { importModelStudioBrowserSession = old })
	importModelStudioBrowserSession = fn
}

func stubAutoLogin(t *testing.T, fn func(context.Context, string, loginCredentials) (browserSession, error)) {
	t.Helper()
	old := autoLoginModelStudio
	t.Cleanup(func() { autoLoginModelStudio = old })
	autoLoginModelStudio = fn
}

func newTestStrategy(serverURL string) *WebConsoleStrategy {
	return &WebConsoleStrategy{
		ConsoleBaseURL: serverURL,
		DashboardURL:   serverURL,
		Region:         modelStudioRegion,
		ProductCode:    modelStudioTeamProduct,
	}
}

// fakeLoginChrome emulates the subset of DevTools the auto-login uses. It
// starts on the sign-in page with a passport iframe and "redirects" to the
// console once the submit button has been clicked.
type fakeLoginChrome struct {
	port    string
	mu      sync.Mutex
	opened  int
	closed  int
	typed   []string
	values  map[string]string
	focused string
	// state
	submitted       bool
	submits         int
	captcha         bool // TMD punish frame appears after submit
	captchaSel      bool // a captcha-selector element is visible in the passport frame
	verification    bool // Alibaba account-protection page (checkcode + protect-your-account text)
	wipeFirstSubmit bool
	// The page never shows the username form: it opens straight on a blocker.
	initialVerification bool // account-protection page from the first poll, no form
	initialTMD          bool // TMD punish frame in the tree from the first poll, no form
	noForm              bool // the form never appears and nothing blocks
}

// blocked reports whether the sign-in page ends in an interactive blocker.
func (f *fakeLoginChrome) blocked() bool {
	return f.captcha || f.captchaSel || f.verification || f.initialVerification || f.initialTMD
}

func newFakeLoginChrome(t *testing.T) *fakeLoginChrome {
	t.Helper()
	oldSettle, oldRedirect, oldCookieWait := loginFormSettle, loginRedirectWait, loginCookieWait
	loginFormSettle, loginRedirectWait, loginCookieWait = 10*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { loginFormSettle, loginRedirectWait, loginCookieWait = oldSettle, oldRedirect, oldCookieWait })
	f := &fakeLoginChrome{values: map[string]string{}}
	const wsPath = "/devtools/page/fake-login-tab"

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/json/new":
			f.mu.Lock()
			f.opened++
			f.mu.Unlock()
			_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":                   "fake-login-tab",
				"webSocketDebuggerUrl": "ws://127.0.0.1:" + port + wsPath,
			})
		case strings.HasPrefix(r.URL.Path, "/json/close/"):
			f.mu.Lock()
			f.closed++
			f.mu.Unlock()
			_, _ = w.Write([]byte("Target is closing"))
		case r.URL.Path == wsPath:
			f.serveWS(t, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, f.port, _ = net.SplitHostPort(server.Listener.Addr().String())
	return f
}

func (f *fakeLoginChrome) serveWS(t *testing.T, w http.ResponseWriter, r *http.Request) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	h := sha1.New()
	h.Write([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(h.Sum(nil)))
	_ = rw.Flush()

	for {
		payload, err := readFakeWSFrame(bufio.NewReaderSize(rw.Reader, 1<<16))
		if err != nil {
			return
		}
		var msg struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		result := f.handle(msg.Method, msg.Params)
		reply, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
		_, _ = rw.Write(encodeFakeWSFrame(reply))
		_ = rw.Flush()
	}
}

func (f *fakeLoginChrome) handle(method string, raw json.RawMessage) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "Page.enable":
		return map[string]any{}
	case "Storage.getCookies":
		// The session cookies land only after the sign-in succeeds.
		cookies := []any{}
		if f.submitted && !f.blocked() {
			for _, c := range signedInCDPCookies() {
				cookies = append(cookies, map[string]any{
					"name": c.Name, "value": c.Value, "domain": c.Domain, "path": c.Path, "secure": c.Secure, "expires": c.Expires,
				})
			}
		}
		return map[string]any{"cookies": cookies}
	case "Page.getFrameTree":
		passport := map[string]any{
			"frame": map[string]string{"id": "passport", "url": "https://" + passportFrameHost + "/mini_login.htm"},
		}
		if f.captcha && f.submitted {
			// The real page nests the slider captcha as a TMD "punish" frame.
			passport["childFrames"] = []any{map[string]any{
				"frame": map[string]string{"id": "punish", "url": "https://" + passportFrameHost + "/newlogin/login.do" + loginCaptchaFramePath + "?x5secdata=abc"},
			}}
		}
		if f.initialTMD {
			// The punish frame is there before any form: the TMD page took over.
			passport["childFrames"] = []any{map[string]any{
				"frame": map[string]string{"id": "punish", "url": "https://" + passportFrameHost + "/newlogin/login.do" + loginCaptchaFramePath + "?x5secdata=abc"},
			}}
		}
		return map[string]any{"frameTree": map[string]any{
			"frame":       map[string]string{"id": "main", "url": alibabaLoginURL},
			"childFrames": []any{passport},
		}}
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": 7}
	case "Input.insertText":
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &p)
		f.typed = append(f.typed, p.Text)
		f.values[f.focused] = p.Text
		return map[string]any{}
	case "Runtime.evaluate":
		var p struct {
			Expression string `json:"expression"`
			ContextID  int    `json:"contextId"`
		}
		_ = json.Unmarshal(raw, &p)
		return map[string]any{"result": map[string]any{"value": f.evaluate(p.Expression, p.ContextID)}}
	}
	return map[string]any{}
}

// evaluate fakes just the expressions autologin.go emits.
func (f *fakeLoginChrome) evaluate(expr string, contextID int) any {
	switch {
	case expr == "location.href":
		if f.submitted && !f.blocked() {
			return modelStudioDashboardURL
		}
		return alibabaLoginURL
	case strings.HasPrefix(expr, "document.readyState"):
		// The form only exists in the passport world, and not at all when
		// the page opened on a blocker.
		return contextID == 7 && !f.initialVerification && !f.initialTMD && !f.noForm
	case strings.Contains(expr, "el.focus()"):
		for _, sel := range []string{loginUsernameSel, loginPasswordSel} {
			if strings.Contains(expr, sel) {
				f.focused = sel
				return true
			}
		}
		return false
	case strings.HasSuffix(expr, ").value"):
		for sel, value := range f.values {
			if strings.Contains(expr, sel) {
				return value
			}
		}
		return ""
	case strings.Contains(expr, loginSubmitSel) && strings.HasSuffix(expr, ".click()"):
		f.submits++
		if f.wipeFirstSubmit && f.submits == 1 {
			// Mimic the real page: its late initialisation cleared what we
			// typed, so the first submit goes out empty and nothing happens.
			f.values = map[string]string{}
			return nil
		}
		f.submitted = true
		return nil
	case strings.Contains(expr, "vibeusage-login-probe"):
		// The passport-frame probe returns raw signals; classification is
		// the Go side's job. Nothing shows until the form was submitted.
		signals := map[string]any{"checkcode": false, "captcha": false, "text": "", "err": ""}
		if f.submitted || f.initialVerification {
			if f.verification || f.initialVerification {
				signals["checkcode"] = true
				signals["text"] = "To protect your account, please complete the verification. Mobile 1-206****361 Get code"
			}
			if f.captchaSel {
				signals["captcha"] = true
			}
		}
		return signals
	case strings.Contains(expr, "vibeusage-page-text"):
		return "" // the top-level page shows no copy; the passport frame carries it
	}
	return nil
}

func TestWebConsoleFetchHonorsLoginCooldown(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	server := newModelStudioSessionServer(t, func(string) string { return successfulSummaryPayload() })
	useAutoLoginConfig(t)
	stubImporter(t, func(context.Context) (browserSession, error) {
		return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome (CDP :9444)", errBrowserCookieImportUnavailable)
	})
	var logins atomic.Int32
	stubAutoLogin(t, func(context.Context, string, loginCredentials) (browserSession, error) {
		logins.Add(1)
		return browserSession{}, errors.New("no redirect to the console within 45s")
	})

	strategy := newTestStrategy(server.URL)
	first, _ := strategy.Fetch(context.Background())
	second, _ := strategy.Fetch(context.Background())
	if logins.Load() != 1 {
		t.Fatalf("logins = %d, want 1 (second fetch must respect the cooldown)", logins.Load())
	}
	if !strings.Contains(first.Error, "no redirect") {
		t.Errorf("first error should carry the sign-in failure: %s", first.Error)
	}
	if !strings.Contains(second.Error, "attempted recently") {
		t.Errorf("second error should explain the cooldown: %s", second.Error)
	}

	recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
	_, _ = strategy.Fetch(context.Background())
	if logins.Load() != 2 {
		t.Errorf("logins = %d, want 2 once the cooldown has passed", logins.Load())
	}
}

func TestWebConsoleStrategyExtendsTimeoutOnlyWithAutoLogin(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	strategy := &WebConsoleStrategy{}

	config.SetGlobal(config.DefaultConfig())
	if got := strategy.ExtendTimeout(30 * time.Second); got != 30*time.Second {
		t.Errorf("unconfigured: ExtendTimeout = %v, want 30s", got)
	}

	useAutoLoginConfig(t)
	if got := strategy.ExtendTimeout(30 * time.Second); got != 30*time.Second+loginOverallDeadline {
		t.Errorf("configured: ExtendTimeout = %v", got)
	}
}
