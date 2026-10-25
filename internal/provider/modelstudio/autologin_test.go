package modelstudio

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
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
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "1")
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
	if chrome.getCodeClicks != 0 {
		t.Errorf("captcha must never trigger assisted Get code click, got %d", chrome.getCodeClicks)
	}
	if chrome.closed != 1 {
		t.Errorf("tab should be closed after failure, closed=%d", chrome.closed)
	}
}

func TestPerformModelStudioAutoLoginDetectsVerification(t *testing.T) {
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "")
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
	if chrome.getCodeClicks != 0 {
		t.Errorf("unassisted verification must not click Get code, got %d", chrome.getCodeClicks)
	}
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

type fakeMouseEvent struct {
	Type       string  `json:"type"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button"`
	Buttons    int     `json:"buttons"`
	ClickCount int     `json:"clickCount"`
}

type fakeKeyEvent struct {
	Type                  string `json:"type"`
	Key                   string `json:"key"`
	Code                  string `json:"code"`
	Text                  string `json:"text"`
	UnmodifiedText        string `json:"unmodifiedText"`
	Modifiers             int    `json:"modifiers"`
	WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode"`
	NativeVirtualKeyCode  int    `json:"nativeVirtualKeyCode"`
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
	// Assisted verification state
	getCodeClicks        int
	onGetCodeClick       func()
	checkcodeSubmitted   bool
	rejectCheckcode      string
	onAfterCodeProbe     func()
	onBlockerProbe       func()
	onPassportWorld      func()
	storageGetCookiesErr error
	// Humanized input tracking & test hooks
	mouseEvents       []fakeMouseEvent
	keyEvents         []fakeKeyEvent
	insertTextCalls   int
	ignoreKeyDownText bool
	jsSubmitClicks    int
	missingCoords     map[string]bool
	rejectFieldInput  map[string]bool
	clearBeforeSubmit map[string]bool
	lastMouseMovedX   float64
	lastMouseMovedY   float64
	hasMouseMoved     bool
	lastMousePressedX float64
	lastMousePressedY float64
	hasMousePressed   bool
}

// blocked reports whether the sign-in page ends in an interactive blocker.
func (f *fakeLoginChrome) blocked() bool {
	verificationBlocked := (f.verification || f.initialVerification) && (!f.checkcodeSubmitted || f.rejectCheckcode != "")
	formErrorBlocked := !f.verification && !f.initialVerification && f.rejectCheckcode != ""
	return f.captcha || f.captchaSel || verificationBlocked || formErrorBlocked || f.initialTMD
}

func newFakeLoginChrome(t *testing.T) *fakeLoginChrome {
	t.Helper()
	if os.Getenv("VIBEUSAGE_CACHE_DIR") == "" {
		t.Setenv("VIBEUSAGE_CACHE_DIR", t.TempDir())
	}
	oldSettle, oldRedirect, oldCookieWait, oldVerifyWait, oldSleep := loginFormSettle, loginRedirectWait, loginCookieWait, loginVerificationWait, loginSleep
	loginFormSettle, loginRedirectWait, loginCookieWait, loginVerificationWait = 10*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() {
		loginFormSettle, loginRedirectWait, loginCookieWait, loginVerificationWait, loginSleep = oldSettle, oldRedirect, oldCookieWait, oldVerifyWait, oldSleep
	})
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
		result, rpcErr := f.handle(msg.Method, msg.Params)
		replyObj := map[string]any{"id": msg.ID, "result": result}
		if rpcErr != nil {
			replyObj = map[string]any{"id": msg.ID, "error": map[string]string{"message": rpcErr.Error()}}
		}
		reply, _ := json.Marshal(replyObj)
		_, _ = rw.Write(encodeFakeWSFrame(reply))
		_ = rw.Flush()
	}
}

func (f *fakeLoginChrome) handle(method string, raw json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "Page.enable":
		return map[string]any{}, nil
	case "Storage.getCookies":
		if f.storageGetCookiesErr != nil {
			return nil, f.storageGetCookiesErr
		}
		// The session cookies land only after the sign-in succeeds.
		cookies := []any{}
		if f.submitted && !f.blocked() {
			for _, c := range signedInCDPCookies() {
				cookies = append(cookies, map[string]any{
					"name": c.Name, "value": c.Value, "domain": c.Domain, "path": c.Path, "secure": c.Secure, "expires": c.Expires,
				})
			}
		}
		return map[string]any{"cookies": cookies}, nil
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
		}}, nil
	case "Page.createIsolatedWorld":
		cb := f.onPassportWorld
		if cb != nil {
			f.mu.Unlock()
			cb()
			f.mu.Lock()
		}
		return map[string]any{"executionContextId": 7}, nil
	case "Input.insertText":
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &p)
		f.insertTextCalls++
		if f.focused == loginCheckcodeSel {
			f.typed = append(f.typed, p.Text)
		}
		f.values[f.focused] = p.Text
		return map[string]any{}, nil
	case "Input.dispatchMouseEvent":
		var p fakeMouseEvent
		_ = json.Unmarshal(raw, &p)
		f.mouseEvents = append(f.mouseEvents, p)
		switch p.Type {
		case "mouseMoved":
			f.lastMouseMovedX, f.lastMouseMovedY, f.hasMouseMoved = p.X, p.Y, true
		case "mousePressed":
			f.lastMousePressedX, f.lastMousePressedY, f.hasMousePressed = p.X, p.Y, true
		case "mouseReleased":
			if f.hasMouseMoved && f.hasMousePressed &&
				f.lastMouseMovedX == p.X && f.lastMouseMovedY == p.Y &&
				f.lastMousePressedX == p.X && f.lastMousePressedY == p.Y {
				switch {
				case p.X == 130 && p.Y == 100:
					f.focused = loginUsernameSel
				case p.X == 130 && p.Y == 160:
					f.focused = loginPasswordSel
				case p.X == 130 && p.Y == 190:
					f.focused = loginCheckcodeSel
				case p.X == 130 && p.Y == 220:
					if f.values[loginCheckcodeSel] != "" {
						f.typed = append(f.typed, f.values[loginCheckcodeSel])
						f.checkcodeSubmitted = true
					} else {
						f.typed = append(f.typed, f.values[loginUsernameSel], f.values[loginPasswordSel])
					}
					f.submits++
					if f.wipeFirstSubmit && f.submits == 1 {
						f.values = map[string]string{}
					} else {
						f.submitted = true
					}
				}
			}
			f.hasMousePressed = false
		}
		return map[string]any{}, nil
	case "Input.dispatchKeyEvent":
		var p fakeKeyEvent
		_ = json.Unmarshal(raw, &p)
		f.keyEvents = append(f.keyEvents, p)
		if (p.Type == "char" || (!f.ignoreKeyDownText && p.Type == "keyDown" && p.Text != "")) && f.focused != "" && !f.rejectFieldInput[f.focused] {
			f.values[f.focused] += p.Text
		}
		return map[string]any{}, nil
	case "Page.captureScreenshot":
		return map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("fake-png-screenshot"))}, nil
	case "Runtime.evaluate":
		var p struct {
			Expression string `json:"expression"`
			ContextID  int    `json:"contextId"`
		}
		_ = json.Unmarshal(raw, &p)
		val, cb := f.evaluate(p.Expression, p.ContextID)
		if cb != nil {
			f.mu.Unlock()
			cb()
			f.mu.Lock()
		}
		return map[string]any{"result": map[string]any{"value": val}}, nil
	}
	return map[string]any{}, nil
}

// evaluate fakes just the expressions autologin.go emits.
func (f *fakeLoginChrome) evaluate(expr string, contextID int) (any, func()) {
	switch {
	case expr == "location.href":
		if f.submitted && !f.blocked() {
			return modelStudioDashboardURL, nil
		}
		return alibabaLoginURL, nil
	case strings.HasPrefix(expr, "document.readyState"):
		// The form only exists in the passport world, and not at all when
		// the page opened on a blocker.
		return contextID == 7 && !f.initialVerification && !f.initialTMD && !f.noForm, nil
	case strings.Contains(expr, "vibeusage-element-rect"):
		for _, item := range []struct {
			sel  string
			x, y float64
		}{
			{loginUsernameSel, 120, 80},
			{loginPasswordSel, 120, 140},
			{loginCheckcodeSel, 120, 170},
			{loginSubmitSel, 120, 200},
		} {
			if strings.Contains(expr, item.sel) {
				if f.missingCoords[item.sel] {
					return map[string]any{"x": 0.0, "y": 0.0, "ok": false}, nil
				}
				return map[string]any{"x": item.x, "y": item.y, "ok": true}, nil
			}
		}
		return map[string]any{"x": 0.0, "y": 0.0, "ok": false}, nil
	case strings.Contains(expr, "vibeusage-frame-offset"):
		return map[string]any{"x": 10.0, "y": 20.0}, nil
	case strings.Contains(expr, "vibeusage-clear-field"):
		for _, sel := range []string{loginUsernameSel, loginPasswordSel, loginCheckcodeSel} {
			if strings.Contains(expr, sel) {
				f.values[sel] = ""
				return true, nil
			}
		}
		return false, nil
	case strings.Contains(expr, "el.focus()"):
		for _, sel := range []string{loginUsernameSel, loginPasswordSel, loginCheckcodeSel} {
			if strings.Contains(expr, sel) {
				f.focused = sel
				if strings.Contains(expr, "vibeusage-js-focus") {
					f.values[sel] = ""
				}
				return true, nil
			}
		}
		return false, nil
	case strings.HasSuffix(expr, ").value"):
		for sel, value := range f.values {
			if strings.Contains(expr, sel) {
				if f.clearBeforeSubmit[sel] {
					delete(f.clearBeforeSubmit, sel)
					f.values[sel] = ""
					return value, nil
				}
				return value, nil
			}
		}
		return "", nil
	case strings.Contains(expr, loginSubmitSel) && strings.HasSuffix(expr, ".click()"):
		f.jsSubmitClicks++
		if f.values[loginCheckcodeSel] == "" {
			f.typed = append(f.typed, f.values[loginUsernameSel], f.values[loginPasswordSel])
		}
		f.submits++
		if f.wipeFirstSubmit && f.submits == 1 {
			// Mimic the real page: its late initialisation cleared what we
			// typed, so the first submit goes out empty and nothing happens.
			f.values = map[string]string{}
			return nil, nil
		}
		if f.values[loginCheckcodeSel] != "" {
			f.checkcodeSubmitted = true
		}
		f.submitted = true
		return nil, nil
	case strings.Contains(expr, "vibeusage-click-get-code"):
		f.getCodeClicks++
		return true, f.onGetCodeClick
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
			if (f.checkcodeSubmitted || (!f.verification && !f.initialVerification)) && f.rejectCheckcode != "" {
				signals["err"] = f.rejectCheckcode
			}
		}
		if f.checkcodeSubmitted && f.onAfterCodeProbe != nil {
			cb := f.onAfterCodeProbe
			f.onAfterCodeProbe = nil
			return signals, cb
		}
		return signals, f.onBlockerProbe
	case strings.Contains(expr, "document.body.innerText") && strings.Contains(expr, "slice(0, 240)"):
		if f.checkcodeSubmitted && f.rejectCheckcode != "" {
			return "To protect your account, please complete the verification. " + f.rejectCheckcode, nil
		}
		return "", nil
	case strings.Contains(expr, "vibeusage-page-text"):
		return "", nil // the top-level page shows no copy; the passport frame carries it
	}
	return nil, nil
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

func TestAssistedVerificationDeliverCodeCompletesSignIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial bool
	}{
		{name: "post_submit_verification", initial: false},
		{name: "initial_verification_panel", initial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateParkFixture(t)
			t.Setenv(assistedLoginEnv, "1")

			chrome := newFakeLoginChrome(t)
			loginVerificationWait = 1200 * time.Millisecond
			if tc.initial {
				chrome.initialVerification = true
				oldFormWait := loginFormWait
				loginFormWait = 20 * time.Millisecond
				t.Cleanup(func() { loginFormWait = oldFormWait })
			} else {
				chrome.verification = true
			}
			var delivered atomic.Bool
			chrome.onGetCodeClick = func() {
				if delivered.CompareAndSwap(false, true) {
					go func() {
						time.Sleep(25 * time.Millisecond)
						if err := DeliverVerificationCode("654321"); err != nil {
							t.Errorf("DeliverVerificationCode: %v", err)
						}
					}()
				}
			}

			port, err := strconv.Atoi(chrome.port)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.DefaultConfig()
			cfg.Providers["modelstudio"] = config.ProviderConfig{
				CDPPorts:     []int{port},
				LoginPort:    port,
				LoginCommand: `printf '{"username":"me@example.com","password":"s3cret"}'`,
			}
			config.SetGlobal(cfg)
			t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

			server := newModelStudioSessionServer(t, func(cookie string) string {
				if strings.Contains(cookie, "login_aliyunid_ticket=ticket") {
					return successfulSummaryPayload()
				}
				return expiredSessionPayload()
			})
			stubImporter(t, func(context.Context) (browserSession, error) {
				return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome", errBrowserCookieImportUnavailable)
			})
			stubAutoLogin(t, performModelStudioAutoLogin)

			result, err := newTestStrategy(server.URL).Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch() error: %v", err)
			}
			if !result.Success {
				t.Fatalf("expected assisted sign-in to succeed, got %+v", result)
			}

			chrome.mu.Lock()
			clicks := chrome.getCodeClicks
			checkcodeSubmitted := chrome.checkcodeSubmitted
			chrome.mu.Unlock()
			if clicks != 1 {
				t.Errorf("getCodeClicks = %d, want 1", clicks)
			}
			if !checkcodeSubmitted {
				t.Error("expected checkcode to be submitted")
			}

			if _, statErr := os.Stat(verificationCodePath()); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("verification code file should be removed after consumption, statErr=%v", statErr)
			}
			if code, ok := consumeVerificationCode(); ok || code != "" {
				t.Errorf("second consumeVerificationCode() = (%q, %v), want (\"\", false)", code, ok)
			}

			saved, readErr := config.ReadCredential("modelstudio", "session")
			if readErr != nil || !strings.Contains(string(saved), "CDP auto-login") {
				t.Errorf("expected cached session credential, got %q err=%v", saved, readErr)
			}
			if park, parked := loadLoginPark(); parked {
				t.Errorf("expected login park to be cleared after assisted sign-in, got %+v", park)
			}
		})
	}
}

func TestAssistedVerificationTimeoutParksByteForByte(t *testing.T) {
	runScenario := func(t *testing.T, assist string) (string, bool, loginPark, []byte) {
		t.Helper()
		isolateParkFixture(t)
		t.Setenv(assistedLoginEnv, assist)

		chrome := newFakeLoginChrome(t)
		chrome.verification = true
		oldVerifyWait := loginVerificationWait
		loginVerificationWait = loginOverallDeadline
		t.Cleanup(func() { loginVerificationWait = oldVerifyWait })

		port, err := strconv.Atoi(chrome.port)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.DefaultConfig()
		cfg.Providers["modelstudio"] = config.ProviderConfig{
			CDPPorts:     []int{port},
			LoginPort:    port,
			LoginCommand: `printf '{"username":"me@example.com","password":"s3cret"}'`,
		}
		config.SetGlobal(cfg)
		t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

		server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
		stubImporter(t, func(context.Context) (browserSession, error) {
			return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome", errBrowserCookieImportUnavailable)
		})
		stubAutoLogin(t, performModelStudioAutoLogin)

		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Millisecond)
		defer cancel()
		res, err := newTestStrategy(server.URL).Fetch(ctx)
		if err != nil {
			t.Fatalf("Fetch() error: %v", err)
		}
		if res.Success || res.RetryAfter == nil || time.Until(*res.RetryAfter) <= 0 {
			t.Fatalf("expected throttled result with %s cooldown, got %+v", modelStudioSessionCooldown, res)
		}
		if assist == "1" {
			chrome.mu.Lock()
			clicks := chrome.getCodeClicks
			chrome.mu.Unlock()
			if clicks != 1 {
				t.Errorf("assisted getCodeClicks = %d, want 1", clicks)
			}
		}
		park, parked := loadLoginPark()
		if !parked {
			t.Fatal("expected verification park marker to be written")
		}
		shotPath := filepath.Join(config.CacheDir(), "modelstudio-login-failure.png")
		shotBytes, shotErr := os.ReadFile(shotPath)
		if shotErr != nil || len(shotBytes) == 0 {
			t.Fatalf("expected failure screenshot at %s, err=%v", shotPath, shotErr)
		}
		// Normalize ephemeral test port in error message so we can compare byte-for-byte.
		normalizedErr := strings.ReplaceAll(res.Error, chrome.port, "9444")
		return normalizedErr, res.Success, park, shotBytes
	}

	unassistedErr, unassistedOK, unassistedPark, unassistedShot := runScenario(t, "")
	assistedErr, assistedOK, assistedPark, assistedShot := runScenario(t, "1")

	if assistedOK != unassistedOK || assistedErr != unassistedErr {
		t.Errorf("assisted Fetch result mismatch:\n got: %q\nwant: %q", assistedErr, unassistedErr)
	}
	if assistedPark.Kind != unassistedPark.Kind || assistedPark.Reason != unassistedPark.Reason {
		t.Errorf("park mismatch: got (%q, %q), want (%q, %q)",
			assistedPark.Kind, assistedPark.Reason, unassistedPark.Kind, unassistedPark.Reason)
	}
	if string(assistedShot) != string(unassistedShot) {
		t.Errorf("screenshot mismatch: got %d bytes, want %d bytes", len(assistedShot), len(unassistedShot))
	}
}

func TestAssistedVerificationRejectedCodeParksWithoutRetry(t *testing.T) {
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "1")

	chrome := newFakeLoginChrome(t)
	chrome.verification = true
	chrome.rejectCheckcode = "Verification code 999999 is incorrect"
	chrome.onGetCodeClick = func() {
		if err := DeliverVerificationCode("999999"); err != nil {
			t.Errorf("DeliverVerificationCode: %v", err)
		}
	}

	port, err := strconv.Atoi(chrome.port)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Providers["modelstudio"] = config.ProviderConfig{
		CDPPorts:     []int{port},
		LoginPort:    port,
		LoginCommand: `printf '{"username":"me@example.com","password":"s3cret"}'`,
	}
	config.SetGlobal(cfg)
	t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

	server := newModelStudioSessionServer(t, func(string) string { return expiredSessionPayload() })
	stubImporter(t, func(context.Context) (browserSession, error) {
		return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome", errBrowserCookieImportUnavailable)
	})
	stubAutoLogin(t, performModelStudioAutoLogin)

	var logBuf bytes.Buffer
	logger := logging.NewLogger(&logBuf)
	logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
	res, err := newTestStrategy(server.URL).Fetch(logging.WithLogger(context.Background(), logger))
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}
	if res.Success {
		t.Fatalf("expected failure when verification code is rejected, got %+v", res)
	}

	chrome.mu.Lock()
	clicks := chrome.getCodeClicks
	submits := chrome.submits
	chrome.mu.Unlock()
	if clicks != 1 {
		t.Errorf("getCodeClicks = %d, want 1 (must never request a second SMS)", clicks)
	}
	if submits != 2 {
		t.Errorf("submits = %d, want 2 (1 password submit + 1 verification submit, no retry loop)", submits)
	}

	park, parked := loadLoginPark()
	if !parked || park.Kind != loginBlockerVerification {
		t.Fatalf("expected verification park, got parked=%v park=%+v", parked, park)
	}
	if !strings.Contains(park.Reason, "sign-in rejected") || !strings.Contains(park.Reason, "is incorrect") {
		t.Errorf("expected park reason to surface panel error, got %q", park.Reason)
	}
	if strings.Contains(res.Error, "999999") || strings.Contains(logBuf.String(), "999999") || strings.Contains(park.Reason, "999999") {
		t.Errorf("submitted code leaked into error=%q, logs=%q, or park=%q", res.Error, logBuf.String(), park.Reason)
	}
	shotPath := filepath.Join(config.CacheDir(), "modelstudio-login-failure.png")
	if _, statErr := os.Stat(shotPath); statErr != nil {
		t.Errorf("expected failure screenshot at %s, err=%v", shotPath, statErr)
	}
}

func TestVerificationCodeFileOneUseAndPermissions(t *testing.T) {
	isolateParkFixture(t)

	for _, bad := range []string{"", "   ", "123", "123456789", "12ab56", "my-secret-code"} {
		err := DeliverVerificationCode(bad)
		if err == nil {
			t.Fatalf("DeliverVerificationCode(%q) unexpectedly succeeded", bad)
		}
		if bad != "" && strings.TrimSpace(bad) != "" && strings.Contains(err.Error(), strings.TrimSpace(bad)) {
			t.Fatalf("error %q leaked input %q", err.Error(), bad)
		}
	}

	if err := DeliverVerificationCode(" 482910 \n"); err != nil {
		t.Fatalf("DeliverVerificationCode valid: %v", err)
	}
	path := verificationCodePath()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected regular file, mode=%v", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600", perm)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "482910") {
			t.Errorf("directory listing leaked code in filename: %s", entry.Name())
		}
		if entry.Name() != verificationCodeFilename {
			t.Errorf("unexpected extra file in data dir: %s", entry.Name())
		}
	}

	code, ok := consumeVerificationCode()
	if !ok || code != "482910" {
		t.Fatalf("first consumeVerificationCode() = (%q, %v), want (\"482910\", true)", code, ok)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("code file still exists after consume: %v", statErr)
	}
	entriesAfter, _ := os.ReadDir(filepath.Dir(path))
	if len(entriesAfter) != 0 {
		t.Errorf("expected empty data dir after consume, got %d entries", len(entriesAfter))
	}

	if code2, ok2 := consumeVerificationCode(); ok2 || code2 != "" {
		t.Errorf("second consumeVerificationCode() = (%q, %v), want (\"\", false)", code2, ok2)
	}
}

func TestAssistedVerificationConcurrencyUnderLock(t *testing.T) {
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "1")

	chrome := newFakeLoginChrome(t)
	chrome.verification = true

	port, err := strconv.Atoi(chrome.port)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Providers["modelstudio"] = config.ProviderConfig{
		CDPPorts:     []int{port},
		LoginPort:    port,
		LoginCommand: `printf '{"username":"me@example.com","password":"s3cret"}'`,
	}
	config.SetGlobal(cfg)
	t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

	server := newModelStudioSessionServer(t, func(cookie string) string {
		if strings.Contains(cookie, "login_aliyunid_ticket=ticket") {
			return successfulSummaryPayload()
		}
		return expiredSessionPayload()
	})
	stubImporter(t, func(context.Context) (browserSession, error) {
		return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome", errBrowserCookieImportUnavailable)
	})
	stubAutoLogin(t, performModelStudioAutoLogin)

	strategy := newTestStrategy(server.URL)
	var secondErr error
	chrome.onGetCodeClick = func() {
		// A second concurrent call to autoLogin while the first is waiting inside
		// completeAssistedVerification must hit the real login lock and return
		// errAutoLoginInProgress immediately without opening a tab or clicking "Get code".
		_, secondErr = strategy.autoLogin(context.Background())

		if err := DeliverVerificationCode("135790"); err != nil {
			t.Errorf("DeliverVerificationCode: %v", err)
		}
	}

	res, err := strategy.Fetch(context.Background())
	if err != nil || !res.Success {
		t.Fatalf("first Fetch() = %+v, err=%v", res, err)
	}
	if !errors.Is(secondErr, errAutoLoginInProgress) {
		t.Errorf("concurrent autoLogin err = %v, want %v", secondErr, errAutoLoginInProgress)
	}

	chrome.mu.Lock()
	clicks := chrome.getCodeClicks
	opened := chrome.opened
	chrome.mu.Unlock()
	if clicks != 1 || opened != 1 {
		t.Errorf("clicks=%d opened=%d, want 1 and 1", clicks, opened)
	}
}

func TestR1_1_StaleOrEmptyCodeFileDoesNotAuthorizeUnattendedSMS(t *testing.T) {
	for _, envVal := range []string{"", "0", "false"} {
		for _, tc := range []struct {
			name    string
			content string
			aged    bool
		}{
			{name: "aged_valid_digits", content: "135790", aged: true},
			{name: "fresh_valid_digits", content: "135790", aged: false},
			{name: "aged_empty_file", content: "", aged: true},
		} {
			t.Run(fmt.Sprintf("env_%q_%s", envVal, tc.name), func(t *testing.T) {
				isolateParkFixture(t)
				t.Setenv(assistedLoginEnv, envVal)
				if err := os.MkdirAll(config.DataDir(), 0o700); err != nil {
					t.Fatal(err)
				}
				codePath := verificationCodePath()
				if err := os.WriteFile(codePath, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.aged {
					old := time.Unix(100000, 0)
					if err := os.Chtimes(codePath, old, old); err != nil {
						t.Fatal(err)
					}
				}
				if assistedLoginEnabled() {
					t.Fatalf("assistedLoginEnabled() = true with %s=%q and file %s", assistedLoginEnv, envVal, tc.name)
				}

				chrome := newFakeLoginChrome(t)
				chrome.verification = true
				chrome.rejectCheckcode = "Code rejected"

				_, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
				var blocked *loginBlockedError
				if !errors.As(err, &blocked) || blocked.Kind != loginBlockerVerification {
					t.Fatalf("expected verification blocker, got %v", err)
				}
				chrome.mu.Lock()
				clicks := chrome.getCodeClicks
				chrome.mu.Unlock()
				if clicks != 0 {
					t.Fatalf("unattended run with %s=%q (%s) triggered %d SMS Get-code clicks, want 0", assistedLoginEnv, envVal, tc.name, clicks)
				}
			})
		}
	}

	t.Run("stale_file_rejected_by_consume_even_when_opted_in", func(t *testing.T) {
		isolateParkFixture(t)
		t.Setenv(assistedLoginEnv, "1")
		if err := DeliverVerificationCode("135790"); err != nil {
			t.Fatal(err)
		}
		staleTime := time.Now().Add(-2 * loginOverallDeadline)
		if err := os.Chtimes(verificationCodePath(), staleTime, staleTime); err != nil {
			t.Fatal(err)
		}
		if code, ok := consumeVerificationCode(); ok || code != "" {
			t.Fatalf("consumeVerificationCode() accepted stale file: (%q, %v), want (\"\", false)", code, ok)
		}
		if _, err := os.Lstat(verificationCodePath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected stale verification code file to be removed, stat err=%v", err)
		}
	})
}

func TestR1_2_RejectedPanelTextRedactsSubmittedCodeAcrossAllSinks(t *testing.T) {
	const secretCode = "246802"
	const secretPassword = "s3cret-password-991"

	for _, tc := range []struct {
		name    string
		initial bool
	}{
		{name: "post_submit_verification", initial: false},
		{name: "initial_verification_panel", initial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateParkFixture(t)
			t.Setenv(assistedLoginEnv, "1")

			chrome := newFakeLoginChrome(t)
			if tc.initial {
				chrome.initialVerification = true
				oldFormWait := loginFormWait
				loginFormWait = 20 * time.Millisecond
				t.Cleanup(func() { loginFormWait = oldFormWait })
			} else {
				chrome.verification = true
			}
			chrome.rejectCheckcode = "Verification code " + secretCode + " and password " + secretPassword + " rejected"
			chrome.onGetCodeClick = func() {
				if err := DeliverVerificationCode(secretCode); err != nil {
					t.Errorf("DeliverVerificationCode: %v", err)
				}
			}

			var logBuf bytes.Buffer
			logger := logging.NewLogger(&logBuf)
			logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
			ctx := logging.WithLogger(context.Background(), logger)

			_, err := performModelStudioAutoLogin(ctx, chrome.port, loginCredentials{Username: "me@example.com", Password: secretPassword})
			if err == nil {
				t.Fatal("expected error when verification code is rejected")
			}
			var blocked *loginBlockedError
			if !errors.As(err, &blocked) || blocked.Kind != loginBlockerVerification {
				t.Fatalf("expected verification *loginBlockedError, got %T: %v", err, err)
			}
			if writeErr := writeLoginPark(blocked.Kind, blocked.Reason, time.Now()); writeErr != nil {
				t.Fatal(writeErr)
			}
			parkRaw, readErr := os.ReadFile(loginParkMarker())
			if readErr != nil {
				t.Fatal(readErr)
			}

			for _, secret := range []string{secretCode, secretPassword} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("returned error leaked secret %q: %s", secret, err.Error())
				}
				if strings.Contains(logBuf.String(), secret) {
					t.Errorf("verbose log leaked secret %q: %s", secret, logBuf.String())
				}
				if strings.Contains(string(parkRaw), secret) {
					t.Errorf("persisted login park leaked secret %q: %s", secret, string(parkRaw))
				}
			}
			if !strings.Contains(err.Error(), "[REDACTED]") {
				t.Errorf("expected [REDACTED] in returned error, got %q", err.Error())
			}
			if !strings.Contains(blocked.Reason, "sign-in rejected") {
				t.Errorf("expected preserved rejection reason prefix, got %q", blocked.Reason)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		initial  bool
		assisted bool
	}{
		{name: "post_submit_capture_error", initial: false, assisted: true},
		{name: "initial_panel_capture_error", initial: true, assisted: true},
		{name: "normal_post_submit_capture_error", initial: false, assisted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateParkFixture(t)
			if tc.assisted {
				t.Setenv(assistedLoginEnv, "1")
			}

			chrome := newFakeLoginChrome(t)
			oldCookieWait := loginCookieWait
			loginCookieWait = 10 * time.Millisecond
			t.Cleanup(func() { loginCookieWait = oldCookieWait })

			if tc.assisted {
				if tc.initial {
					chrome.initialVerification = true
					oldFormWait := loginFormWait
					loginFormWait = 20 * time.Millisecond
					t.Cleanup(func() { loginFormWait = oldFormWait })
				} else {
					chrome.verification = true
				}
				chrome.onGetCodeClick = func() {
					if err := DeliverVerificationCode(secretCode); err != nil {
						t.Errorf("DeliverVerificationCode: %v", err)
					}
				}
			}
			chrome.storageGetCookiesErr = fmt.Errorf("cookie capture failed after code=%s pass=%s", secretCode, secretPassword)

			var logBuf bytes.Buffer
			logger := logging.NewLogger(&logBuf)
			logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
			ctx := logging.WithLogger(context.Background(), logger)

			_, err := performModelStudioAutoLogin(ctx, chrome.port, loginCredentials{Username: "me@example.com", Password: secretPassword})
			if err == nil {
				t.Fatal("expected cookie capture error")
			}
			if !errors.Is(err, errLoginNoSessionCookies) {
				t.Fatalf("expected errors.Is(err, errLoginNoSessionCookies) == true, got %T: %v", err, err)
			}
			secrets := []string{secretPassword}
			if tc.assisted {
				secrets = append(secrets, secretCode)
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("returned capture error leaked secret %q: %s", secret, err.Error())
				}
				if strings.Contains(logBuf.String(), secret) {
					t.Errorf("verbose log leaked secret %q: %s", secret, logBuf.String())
				}
			}
			if !strings.Contains(err.Error(), "[REDACTED]") {
				t.Errorf("expected [REDACTED] in returned capture error, got %q", err.Error())
			}

			// Also verify through WebConsoleStrategy.Fetch.
			clearLoginPark()
			recordLoginAttempt(time.Now().Add(-loginCooldown - time.Minute))
			chrome.mu.Lock()
			chrome.getCodeClicks = 0
			chrome.submitted = false
			chrome.checkcodeSubmitted = false
			chrome.values = map[string]string{}
			chrome.mu.Unlock()

			port, convErr := strconv.Atoi(chrome.port)
			if convErr != nil {
				t.Fatal(convErr)
			}
			cfg := config.DefaultConfig()
			cfg.Providers["modelstudio"] = config.ProviderConfig{
				CDPPorts:     []int{port},
				LoginPort:    port,
				LoginCommand: fmt.Sprintf(`printf '{"username":"me@example.com","password":%q}'`, secretPassword),
			}
			config.SetGlobal(cfg)
			t.Cleanup(func() { config.SetGlobal(config.DefaultConfig()) })

			server := newModelStudioSessionServer(t, func(string) string {
				return expiredSessionPayload()
			})
			stubImporter(t, func(context.Context) (browserSession, error) {
				return browserSession{}, fmt.Errorf("%w: no signed-in Alibaba Cloud session in Chrome", errBrowserCookieImportUnavailable)
			})
			stubAutoLogin(t, performModelStudioAutoLogin)

			fetchOut, fetchErr := newTestStrategy(server.URL).Fetch(ctx)
			if fetchErr != nil {
				t.Fatalf("Fetch returned unexpected error: %v", fetchErr)
			}
			for _, secret := range secrets {
				if strings.Contains(fetchOut.Error, secret) {
					t.Errorf("Fetch outcome leaked secret %q: %s", secret, fetchOut.Error)
				}
				if strings.Contains(logBuf.String(), secret) {
					t.Errorf("verbose log after Fetch leaked secret %q: %s", secret, logBuf.String())
				}
			}
			if !strings.Contains(fetchOut.Error, "[REDACTED]") {
				t.Errorf("expected [REDACTED] in Fetch outcome error, got %q", fetchOut.Error)
			}
		})
	}

	t.Run("return_boundary_untyped_form_error", func(t *testing.T) {
		isolateParkFixture(t)
		chrome := newFakeLoginChrome(t)
		chrome.rejectCheckcode = "Password " + secretPassword + " was rejected by passport"

		var logBuf bytes.Buffer
		logger := logging.NewLogger(&logBuf)
		logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
		ctx := logging.WithLogger(context.Background(), logger)

		_, err := performModelStudioAutoLogin(ctx, chrome.port, loginCredentials{Username: "me@example.com", Password: secretPassword})
		if err == nil {
			t.Fatal("expected error from untyped form rejection")
		}
		if strings.Contains(err.Error(), secretPassword) {
			t.Errorf("return boundary leaked password %q: %s", secretPassword, err.Error())
		}
		if strings.Contains(logBuf.String(), secretPassword) {
			t.Errorf("verbose log leaked password %q: %s", secretPassword, logBuf.String())
		}
		if !strings.Contains(err.Error(), "[REDACTED]") {
			t.Errorf("expected [REDACTED] in returned error, got %q", err.Error())
		}
	})
}

func TestR2_R1_2_CaptureSessionErrorRedactsSecretsAndPreservesUnwrap(t *testing.T) {
	TestR1_2_RejectedPanelTextRedactsSubmittedCodeAcrossAllSinks(t)
}

func TestR2_1_CanceledParentBeforeGetCodeNeverRequestsSMS(t *testing.T) {
	for _, tc := range []struct {
		name             string
		initial          bool
		cancelAfterWorld bool
	}{
		{name: "initial_panel_canceled_on_blocker_probe", initial: true, cancelAfterWorld: false},
		{name: "post_submit_canceled_on_blocker_probe", initial: false, cancelAfterWorld: false},
		{name: "canceled_after_passport_world_before_get_code_eval", initial: false, cancelAfterWorld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateParkFixture(t)
			t.Setenv(assistedLoginEnv, "1")

			if !tc.cancelAfterWorld {
				if err := DeliverVerificationCode("135790"); err != nil {
					t.Fatal(err)
				}
			}

			chrome := newFakeLoginChrome(t)
			if tc.initial {
				chrome.initialVerification = true
				oldFormWait := loginFormWait
				loginFormWait = 15 * time.Millisecond
				t.Cleanup(func() { loginFormWait = oldFormWait })
			} else {
				chrome.verification = true
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tc.cancelAfterWorld {
				chrome.onBlockerProbe = func() {
					chrome.mu.Lock()
					chrome.onPassportWorld = cancel
					chrome.mu.Unlock()
				}
			} else {
				chrome.onBlockerProbe = cancel
			}

			_, err := performModelStudioAutoLogin(ctx, chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})

			chrome.mu.Lock()
			clicks := chrome.getCodeClicks
			checkcodeSubmitted := chrome.checkcodeSubmitted
			chrome.mu.Unlock()
			if clicks != 0 {
				t.Errorf("getCodeClicks = %d, want 0 when parent ctx is canceled before Get code", clicks)
			}
			if checkcodeSubmitted {
				t.Error("expected no verification code submission after parent ctx cancellation")
			}

			if !tc.cancelAfterWorld {
				raw, statErr := os.ReadFile(verificationCodePath())
				if statErr != nil || string(raw) != "135790" {
					t.Errorf("expected pre-staged code file to remain untouched when canceled before purge/get-code, got content=%q err=%v", string(raw), statErr)
				}
			}

			if err == nil {
				t.Fatal("expected error when parent context is canceled before Get code")
			}
			var blocked *loginBlockedError
			if !errors.As(err, &blocked) || blocked.Kind != loginBlockerVerification {
				t.Fatalf("expected *loginBlockedError with Kind=%q, got %T: %v", loginBlockerVerification, err, err)
			}
			if blocked.Reason != verificationBlockedReason {
				t.Errorf("blocked.Reason = %q, want %q", blocked.Reason, verificationBlockedReason)
			}
		})
	}
}

func TestR2_1_CanceledParentBeforeAssistedGetCodeNeverClicks(t *testing.T) {
	TestR2_1_CanceledParentBeforeGetCodeNeverRequestsSMS(t)
}

func TestR1_4_AssistedVerificationRealParentDeadlineCapturesScreenshot(t *testing.T) {
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "1")

	chrome := newFakeLoginChrome(t)
	chrome.verification = true
	oldVerifyWait := loginVerificationWait
	loginVerificationWait = loginOverallDeadline
	t.Cleanup(func() { loginVerificationWait = oldVerifyWait })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	_, err := performModelStudioAutoLogin(ctx, chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	var blocked *loginBlockedError
	if !errors.As(err, &blocked) || blocked.Kind != loginBlockerVerification {
		t.Fatalf("expected verification blocker on timeout, got %v", err)
	}
	shotPath := filepath.Join(config.CacheDir(), "modelstudio-login-failure.png")
	shotBytes, shotErr := os.ReadFile(shotPath)
	if shotErr != nil || len(shotBytes) == 0 {
		t.Fatalf("real parent deadline expiration did not capture failure screenshot at %s: err=%v len=%d", shotPath, shotErr, len(shotBytes))
	}
}

func TestR1_6_AssistedVerificationMixedSignalTransitionCompletesSignIn(t *testing.T) {
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "1")

	chrome := newFakeLoginChrome(t)
	chrome.verification = true
	chrome.captchaSel = true
	chrome.onGetCodeClick = func() {
		if err := DeliverVerificationCode("123456"); err != nil {
			t.Errorf("DeliverVerificationCode: %v", err)
		}
	}
	chrome.onAfterCodeProbe = func() {
		chrome.mu.Lock()
		defer chrome.mu.Unlock()
		chrome.verification = false
		chrome.captchaSel = false
	}

	session, err := performModelStudioAutoLogin(context.Background(), chrome.port, loginCredentials{Username: "me@example.com", Password: "s3cret"})
	if err != nil {
		t.Fatalf("expected mixed-signal verification transition to complete sign-in, got error: %v", err)
	}
	if !strings.Contains(session.Cookie, "login_aliyunid_ticket=") {
		t.Fatalf("expected captured session cookie after redirect, got %+v", session)
	}
	chrome.mu.Lock()
	clicks := chrome.getCodeClicks
	chrome.mu.Unlock()
	if clicks != 1 {
		t.Errorf("getCodeClicks = %d, want 1", clicks)
	}
}

func TestHumanizedLoginDispatchesOnlyTrustedMouseAndKeyEvents(t *testing.T) {
	t.Run("trusted_mouse_focus_key_typing_and_mouse_submit", func(t *testing.T) {
		chrome := newFakeLoginChrome(t)
		creds := loginCredentials{
			Username: "op_user@example.com",
			Password: "Op!Pass#42",
		}

		session, err := performModelStudioAutoLogin(context.Background(), chrome.port, creds)
		if err != nil {
			t.Fatalf("performModelStudioAutoLogin: %v", err)
		}
		if !strings.Contains(session.Cookie, "login_aliyunid_ticket=") {
			t.Fatalf("expected session cookie, got %+v", session)
		}

		chrome.mu.Lock()
		defer chrome.mu.Unlock()

		if chrome.insertTextCalls != 0 {
			t.Errorf("insertTextCalls = %d, want 0 (credential fill must never use Input.insertText)", chrome.insertTextCalls)
		}
		if chrome.jsSubmitClicks != 0 {
			t.Errorf("jsSubmitClicks = %d, want 0 (submit must never use JS .click())", chrome.jsSubmitClicks)
		}

		// Expect 3 trusted mouse click sequences: username (130, 100), password (130, 160), submit (130, 220).
		wantClicks := []struct {
			name string
			x, y float64
		}{
			{"username focus", 130, 100},
			{"password focus", 130, 160},
			{"submit click", 130, 220},
		}
		if len(chrome.mouseEvents) != len(wantClicks)*3 {
			t.Fatalf("len(mouseEvents) = %d, want %d: %+v", len(chrome.mouseEvents), len(wantClicks)*3, chrome.mouseEvents)
		}
		for i, wc := range wantClicks {
			moved := chrome.mouseEvents[i*3]
			pressed := chrome.mouseEvents[i*3+1]
			released := chrome.mouseEvents[i*3+2]
			if moved.Type != "mouseMoved" || moved.X != wc.x || moved.Y != wc.y {
				t.Errorf("%s mouseMoved = %+v, want at (%v, %v)", wc.name, moved, wc.x, wc.y)
			}
			if pressed.Type != "mousePressed" || pressed.X != wc.x || pressed.Y != wc.y || pressed.Button != "left" || pressed.Buttons != 1 || pressed.ClickCount != 1 {
				t.Errorf("%s mousePressed = %+v, want left button click at (%v, %v)", wc.name, pressed, wc.x, wc.y)
			}
			if released.Type != "mouseReleased" || released.X != wc.x || released.Y != wc.y || released.Button != "left" || released.Buttons != 0 || released.ClickCount != 1 {
				t.Errorf("%s mouseReleased = %+v, want left button release at (%v, %v)", wc.name, released, wc.x, wc.y)
			}
		}

		allRunes := []rune(creds.Username + creds.Password)
		if len(chrome.keyEvents) != len(allRunes)*3 {
			t.Fatalf("len(keyEvents) = %d, want %d (3 events per rune)", len(chrome.keyEvents), len(allRunes)*3)
		}
		var typedChars strings.Builder
		for i, r := range allRunes {
			down := chrome.keyEvents[i*3]
			ch := chrome.keyEvents[i*3+1]
			up := chrome.keyEvents[i*3+2]
			wantStr := string(r)
			if down.Type != "rawKeyDown" || down.Text != "" || down.UnmodifiedText != "" || down.Key != wantStr || down.Code == "" {
				t.Errorf("rune %d rawKeyDown = %+v", i, down)
			}
			if ch.Type != "char" || ch.Text != wantStr || ch.UnmodifiedText != wantStr {
				t.Errorf("rune %d char = %+v", i, ch)
			}
			if up.Type != "keyUp" || up.Key != wantStr || up.Text != "" {
				t.Errorf("rune %d keyUp = %+v", i, up)
			}
			typedChars.WriteString(ch.Text)
		}
		if got := typedChars.String(); got != creds.Username+creds.Password {
			t.Errorf("reassembled char events = %q, want %q", got, creds.Username+creds.Password)
		}
		if got := strings.Join(chrome.typed, "|"); got != creds.Username+"|"+creds.Password {
			t.Errorf("submitted values = %q, want %q", got, creds.Username+"|"+creds.Password)
		}
	})

	t.Run("missing_coordinates_falls_back_to_js_focus_with_debug_log", func(t *testing.T) {
		chrome := newFakeLoginChrome(t)
		chrome.missingCoords = map[string]bool{loginUsernameSel: true}

		var logBuf bytes.Buffer
		logger := logging.NewLogger(&logBuf)
		logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
		ctx := logging.WithLogger(context.Background(), logger)

		creds := loginCredentials{Username: "fallback@example.com", Password: "Pw9!"}
		if _, err := performModelStudioAutoLogin(ctx, chrome.port, creds); err != nil {
			t.Fatalf("performModelStudioAutoLogin with missing username coords: %v", err)
		}

		if !strings.Contains(logBuf.String(), "field coordinates unavailable; falling back to JS focus") ||
			!strings.Contains(logBuf.String(), loginUsernameSel) {
			t.Errorf("expected JS-focus fallback debug log for %s, got log:\n%s", loginUsernameSel, logBuf.String())
		}

		chrome.mu.Lock()
		defer chrome.mu.Unlock()
		if chrome.insertTextCalls != 0 {
			t.Errorf("insertTextCalls = %d, want 0 even when focus falls back to JS", chrome.insertTextCalls)
		}
		if chrome.jsSubmitClicks != 0 {
			t.Errorf("jsSubmitClicks = %d, want 0", chrome.jsSubmitClicks)
		}
	})
}

func TestHumanizedLoginKeyPacingWithinBounds(t *testing.T) {
	runDeterministicPacingCheck(t)
}

func TestHumanizedLoginCredentialsNeverReachLogsDiagnosticsOrErrors(t *testing.T) {
	const (
		secretUser = "user-secret-login-id@example.com"
		secretPass = "pw-secret-val-98765!"
	)
	creds := loginCredentials{Username: secretUser, Password: secretPass}

	assertNoLeak := func(t *testing.T, label, text string) {
		t.Helper()
		for _, secret := range []string{secretUser, secretPass} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaked credential %q: %s", label, secret, text)
			}
		}
	}

	t.Run("verbose_success_and_diagnostics", func(t *testing.T) {
		chrome := newFakeLoginChrome(t)
		var logBuf bytes.Buffer
		logger := logging.NewLogger(&logBuf)
		logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
		ctx := logging.WithLogger(context.Background(), logger)

		if _, err := performModelStudioAutoLogin(ctx, chrome.port, creds); err != nil {
			t.Fatalf("performModelStudioAutoLogin: %v", err)
		}
		assertNoLeak(t, "verbose log on success", logBuf.String())
	})

	t.Run("field_rejected_error_quotes_selector_only", func(t *testing.T) {
		chrome := newFakeLoginChrome(t)
		chrome.rejectFieldInput = map[string]bool{loginPasswordSel: true}
		oldRedirect := loginRedirectWait
		loginRedirectWait = 20 * time.Millisecond
		t.Cleanup(func() { loginRedirectWait = oldRedirect })

		var logBuf bytes.Buffer
		logger := logging.NewLogger(&logBuf)
		logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
		ctx := logging.WithLogger(context.Background(), logger)

		_, err := performModelStudioAutoLogin(ctx, chrome.port, creds)
		if err == nil {
			t.Fatal("expected error when password field rejects input")
		}
		if err.Error() != "enter password: "+loginPasswordSel+" did not accept input" {
			t.Errorf("expected exact selector-only error for %s, got %q", loginPasswordSel, err.Error())
		}
		assertNoLeak(t, "field-rejected error string", err.Error())
		assertNoLeak(t, "field-rejected verbose log", logBuf.String())

		page, dialErr := dialCDP(ctx, chrome.port, "/devtools/page/fake-login-tab")
		if dialErr != nil {
			t.Fatalf("dialCDP: %v", dialErr)
		}
		defer page.Close()
		rawErr := page.typeCredentialInto(ctx, logger, 7, loginPasswordSel, secretPass)
		if rawErr == nil {
			t.Fatal("expected direct typeCredentialInto error")
		}
		if rawErr.Error() != loginPasswordSel+" did not accept input" {
			t.Errorf("raw helper error = %q, want %q (must quote selector only, never value)", rawErr.Error(), loginPasswordSel+" did not accept input")
		}
		assertNoLeak(t, "direct typeCredentialInto error", rawErr.Error())
	})

	t.Run("cleared_before_submit_and_failure_diagnostics", func(t *testing.T) {
		chrome := newFakeLoginChrome(t)
		chrome.clearBeforeSubmit = map[string]bool{loginUsernameSel: true}
		chrome.rejectCheckcode = "Account " + secretUser + " or password " + secretPass + " invalid"

		var logBuf bytes.Buffer
		logger := logging.NewLogger(&logBuf)
		logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})
		ctx := logging.WithLogger(context.Background(), logger)

		_, err := performModelStudioAutoLogin(ctx, chrome.port, creds)
		if err == nil {
			t.Fatal("expected error when form is rejected")
		}
		assertNoLeak(t, "returned error", err.Error())
		assertNoLeak(t, "verbose log with failureDiagnostics", logBuf.String())
	})
}

type r1ScriptedCDPConn struct {
	mu                    sync.Mutex
	pending               bytes.Buffer
	chrome                *fakeLoginChrome
	deadlines             []time.Time
	calls                 int
	closed                bool
	noReply               bool
	blockRead             chan struct{}
	abortRead             chan struct{}
	abortReadClosed       bool
	readEntered           chan struct{}
	readEnteredClosed     bool
	closedSig             chan struct{}
	closedSigClosed       bool
	interruptedByDeadline bool
	interruptedByClose    bool
	releasedByAbort       bool
	ignoreKeyDownText     bool
	intercept             func(method string, raw json.RawMessage) (errMsg string, intercept bool)
}

func newR1ScriptedPage(ignoreKeyDownText bool) (*cdpConn, *r1ScriptedCDPConn) {
	transport := &r1ScriptedCDPConn{
		chrome:            &fakeLoginChrome{values: map[string]string{}},
		ignoreKeyDownText: ignoreKeyDownText,
	}
	return &cdpConn{conn: transport, reader: bufio.NewReader(transport)}, transport
}

func (c *r1ScriptedCDPConn) releaseAbort() {
	c.mu.Lock()
	var abortCh chan struct{}
	if c.abortRead != nil && !c.abortReadClosed {
		c.abortReadClosed = true
		abortCh = c.abortRead
	}
	c.mu.Unlock()
	if abortCh != nil {
		close(abortCh)
	}
}

func (c *r1ScriptedCDPConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	blockCh := c.blockRead
	abortCh := c.abortRead
	var enteredCh chan struct{}
	if blockCh != nil && c.readEntered != nil && !c.readEnteredClosed {
		c.readEnteredClosed = true
		enteredCh = c.readEntered
	}
	noReply := c.noReply
	closed := c.closed
	c.mu.Unlock()

	if enteredCh != nil {
		close(enteredCh)
	}

	if blockCh != nil {
		select {
		case <-blockCh:
			return 0, net.ErrClosed
		case <-abortCh:
			c.mu.Lock()
			c.releasedByAbort = true
			c.mu.Unlock()
			return 0, errors.New("scripted read released by failure watchdog abort")
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || closed {
		return 0, net.ErrClosed
	}
	if c.noReply || noReply {
		return 0, errors.New("scripted no-reply read timeout")
	}
	return c.pending.Read(p)
}

func (c *r1ScriptedCDPConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.mu.Unlock()

	raw, err := readFakeWSFrame(bufio.NewReader(bytes.NewReader(p)))
	if err != nil {
		return 0, err
	}
	var msg struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return 0, err
	}

	c.mu.Lock()
	c.calls++
	intercept := c.intercept
	c.mu.Unlock()

	if intercept != nil {
		if errMsg, ok := intercept(msg.Method, msg.Params); ok {
			if errMsg == "BLOCK_READ" {
				_, _ = c.chrome.handle(msg.Method, msg.Params)
				c.mu.Lock()
				if c.blockRead == nil {
					c.blockRead = make(chan struct{})
				}
				c.mu.Unlock()
				return len(p), nil
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if errMsg == "NO_REPLY" {
				c.noReply = true
				return len(p), nil
			}
			reply, _ := json.Marshal(map[string]any{
				"id":    msg.ID,
				"error": map[string]string{"message": errMsg},
			})
			c.pending.Write(encodeFakeWSFrame(reply))
			return len(p), nil
		}
	}

	paramsToHandle := msg.Params
	if c.ignoreKeyDownText && msg.Method == "Input.dispatchKeyEvent" {
		var kv map[string]any
		if json.Unmarshal(msg.Params, &kv) == nil && kv["type"] == "keyDown" {
			delete(kv, "text")
			delete(kv, "unmodifiedText")
			paramsToHandle, _ = json.Marshal(kv)
		}
	}

	result, rpcErr := c.chrome.handle(msg.Method, paramsToHandle)

	replyObj := map[string]any{"id": msg.ID, "result": result}
	if rpcErr != nil {
		replyObj = map[string]any{"id": msg.ID, "error": map[string]string{"message": rpcErr.Error()}}
	}
	reply, _ := json.Marshal(replyObj)

	c.mu.Lock()
	c.pending.Write(encodeFakeWSFrame(reply))
	c.mu.Unlock()
	return len(p), nil
}

func (c *r1ScriptedCDPConn) Close() error {
	c.mu.Lock()
	wasClosed := c.closed
	c.closed = true
	c.interruptedByClose = true
	blockCh := c.blockRead
	c.blockRead = nil
	var sigCh chan struct{}
	if c.closedSig != nil && !c.closedSigClosed {
		c.closedSigClosed = true
		sigCh = c.closedSig
	}
	c.mu.Unlock()
	if !wasClosed && blockCh != nil {
		close(blockCh)
	}
	if sigCh != nil {
		close(sigCh)
	}
	return nil
}

func (c *r1ScriptedCDPConn) LocalAddr() net.Addr  { return nil }
func (c *r1ScriptedCDPConn) RemoteAddr() net.Addr { return nil }

func (c *r1ScriptedCDPConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, d)
	var blockCh chan struct{}
	if !d.IsZero() && d.Year() <= 1971 {
		c.interruptedByDeadline = true
		blockCh = c.blockRead
		c.blockRead = nil
	}
	c.mu.Unlock()
	if blockCh != nil {
		close(blockCh)
	}
	return nil
}

func (c *r1ScriptedCDPConn) SetReadDeadline(d time.Time) error  { return c.SetDeadline(d) }
func (c *r1ScriptedCDPConn) SetWriteDeadline(d time.Time) error { return c.SetDeadline(d) }

func TestR1_1_AssistedVerificationUsesTrustedMouseAndKeyEventsWithoutInsertTextOrJSClick(t *testing.T) {
	isolateParkFixture(t)
	t.Setenv(assistedLoginEnv, "1")

	oldSleep := loginSleep
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { loginSleep = oldSleep })

	oldVerifyWait := loginVerificationWait
	loginVerificationWait = 2 * time.Second
	t.Cleanup(func() { loginVerificationWait = oldVerifyWait })

	chrome := newFakeLoginChrome(t)
	chrome.ignoreKeyDownText = true
	chrome.verification = true
	const smsCode = "654321"
	var delivered atomic.Bool
	chrome.onGetCodeClick = func() {
		if delivered.CompareAndSwap(false, true) {
			if err := DeliverVerificationCode(smsCode); err != nil {
				t.Errorf("DeliverVerificationCode: %v", err)
			}
		}
	}

	creds := loginCredentials{Username: "assist@example.com", Password: "Pw#123"}
	session, err := performModelStudioAutoLogin(context.Background(), chrome.port, creds)
	if err != nil {
		t.Fatalf("performModelStudioAutoLogin assisted: %v", err)
	}
	if !strings.Contains(session.Cookie, "login_aliyunid_ticket=ticket") {
		t.Fatalf("unexpected session cookie: %q", session.Cookie)
	}

	chrome.mu.Lock()
	defer chrome.mu.Unlock()

	if chrome.insertTextCalls != 0 {
		t.Errorf("insertTextCalls = %d, want 0 across both credential and assisted checkcode entry", chrome.insertTextCalls)
	}
	if chrome.jsSubmitClicks != 0 {
		t.Errorf("jsSubmitClicks = %d, want 0 across both credential and assisted checkcode submit", chrome.jsSubmitClicks)
	}

	// 5 trusted clicks: username (130, 100), password (130, 160), submit (130, 220),
	// checkcode (130, 190), submit (130, 220).
	wantClicks := []struct {
		name string
		x, y float64
	}{
		{"username focus", 130, 100},
		{"password focus", 130, 160},
		{"credential submit", 130, 220},
		{"checkcode focus", 130, 190},
		{"checkcode submit", 130, 220},
	}
	if len(chrome.mouseEvents) != len(wantClicks)*3 {
		t.Fatalf("len(mouseEvents) = %d, want %d (5 trusted clicks): %+v", len(chrome.mouseEvents), len(wantClicks)*3, chrome.mouseEvents)
	}
	for i, wc := range wantClicks {
		moved := chrome.mouseEvents[i*3]
		pressed := chrome.mouseEvents[i*3+1]
		released := chrome.mouseEvents[i*3+2]
		if moved.Type != "mouseMoved" || moved.X != wc.x || moved.Y != wc.y {
			t.Errorf("%s mouseMoved = %+v, want at (%v, %v)", wc.name, moved, wc.x, wc.y)
		}
		if pressed.Type != "mousePressed" || pressed.X != wc.x || pressed.Y != wc.y || pressed.Button != "left" {
			t.Errorf("%s mousePressed = %+v", wc.name, pressed)
		}
		if released.Type != "mouseReleased" || released.X != wc.x || released.Y != wc.y || released.Button != "left" {
			t.Errorf("%s mouseReleased = %+v", wc.name, released)
		}
	}

	allRunes := []rune(creds.Username + creds.Password + smsCode)
	if len(chrome.keyEvents) != len(allRunes)*3 {
		t.Fatalf("len(keyEvents) = %d, want %d (3 key events per rune including checkcode)", len(chrome.keyEvents), len(allRunes)*3)
	}
	for i, r := range allRunes {
		down := chrome.keyEvents[i*3]
		ch := chrome.keyEvents[i*3+1]
		up := chrome.keyEvents[i*3+2]
		wantStr := string(r)
		if down.Type != "rawKeyDown" || down.Text != "" || down.UnmodifiedText != "" || down.Key != wantStr {
			t.Errorf("rune %d rawKeyDown = %+v", i, down)
		}
		if ch.Type != "char" || ch.Text != wantStr || ch.UnmodifiedText != wantStr {
			t.Errorf("rune %d char = %+v", i, ch)
		}
		if up.Type != "keyUp" || up.Text != "" || up.Key != wantStr {
			t.Errorf("rune %d keyUp = %+v", i, up)
		}
	}
}

func TestR1_2_KeySequenceUsesRawKeyDownWithoutDoubleInsertion(t *testing.T) {
	oldSleep := loginSleep
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { loginSleep = oldSleep })

	chrome := newFakeLoginChrome(t)
	creds := loginCredentials{Username: "nodouble@example.com", Password: "Secret!42"}

	if _, err := performModelStudioAutoLogin(context.Background(), chrome.port, creds); err != nil {
		t.Fatalf("performModelStudioAutoLogin failed (text-bearing keyDown would double-insert and fail value re-read): %v", err)
	}

	chrome.mu.Lock()
	defer chrome.mu.Unlock()

	if got := chrome.values[loginUsernameSel]; got != creds.Username {
		t.Errorf("username field value = %q, want %q", got, creds.Username)
	}
	if got := chrome.values[loginPasswordSel]; got != creds.Password {
		t.Errorf("password field value = %q, want %q", got, creds.Password)
	}

	allRunes := []rune(creds.Username + creds.Password)
	if len(chrome.keyEvents) != len(allRunes)*3 {
		t.Fatalf("len(keyEvents) = %d, want %d", len(chrome.keyEvents), len(allRunes)*3)
	}
	for i, r := range allRunes {
		down := chrome.keyEvents[i*3]
		ch := chrome.keyEvents[i*3+1]
		up := chrome.keyEvents[i*3+2]
		wantStr := string(r)
		if down.Type != "rawKeyDown" || down.Text != "" || down.UnmodifiedText != "" {
			t.Errorf("rune %d (%q) expected rawKeyDown with empty text/unmodifiedText, got %+v", i, wantStr, down)
		}
		if ch.Type != "char" || ch.Text != wantStr || ch.UnmodifiedText != wantStr {
			t.Errorf("rune %d (%q) expected char with text=%q, got %+v", i, wantStr, wantStr, ch)
		}
		if up.Type != "keyUp" || up.Text != "" || up.UnmodifiedText != "" {
			t.Errorf("rune %d (%q) expected keyUp with empty text, got %+v", i, wantStr, up)
		}
	}
}

func TestR1_3_FillAndSubmitBoundsCDPIODeadlineCancelsAndRestores(t *testing.T) {
	t0 := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	withInjectedModelStudioClock(t, func() time.Time { return t0 })

	oldSleep := loginSleep
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { loginSleep = oldSleep })

	overall := t0.Add(loginOverallDeadline)
	wantFillDeadline := t0.Add(loginFillSubmitTimeout)

	for _, stage := range []string{"geometry", "mouse", "keyDown", "submit"} {
		t.Run("stalled_"+stage, func(t *testing.T) {
			page, transport := newR1ScriptedPage(true)
			outerCtx, outerCancel := context.WithDeadline(context.Background(), overall)
			defer outerCancel()

			transport.intercept = func(method string, raw json.RawMessage) (string, bool) {
				s := string(raw)
				match := (stage == "geometry" && method == "Runtime.evaluate" && strings.Contains(s, "vibeusage-element-rect")) ||
					(stage == "mouse" && method == "Input.dispatchMouseEvent") ||
					(stage == "keyDown" && method == "Input.dispatchKeyEvent") ||
					(stage == "submit" && method == "Runtime.evaluate" && strings.Contains(s, "vibeusage-element-rect") && strings.Contains(s, loginSubmitSel))
				if match {
					return "NO_REPLY", true
				}
				return "", false
			}

			err := page.fillAndSubmit(outerCtx, nil, 7, loginCredentials{Username: "u", Password: "p"})
			if err == nil {
				t.Fatalf("stage %s: expected stalled read error", stage)
			}

			transport.mu.Lock()
			deadlines := append([]time.Time(nil), transport.deadlines...)
			closed := transport.closed
			transport.mu.Unlock()

			if len(deadlines) == 0 {
				t.Fatalf("stage %s: expected fillAndSubmit to set transport deadline to %v, but 0 deadlines were set", stage, wantFillDeadline)
			}
			lastDeadline := deadlines[len(deadlines)-1]
			if !lastDeadline.Equal(wantFillDeadline) {
				t.Fatalf("stage %s: active transport deadline at stalled I/O = %v, want %v (overall=%v)", stage, lastDeadline, wantFillDeadline, overall)
			}
			if !closed {
				t.Errorf("stage %s: expected transport to be closed after stalled frame read so desynchronized stream cannot be reused", stage)
			}
			if reuseErr := page.call("Page.enable", nil, nil); reuseErr == nil {
				t.Errorf("stage %s: expected subsequent call on broken cdpConn to fail instead of reusing desynchronized stream", stage)
			}
		})
	}

	t.Run("cancellation_mid_keydown_interrupts_read_and_skips_remaining_events", func(t *testing.T) {
		runMidKeyDownCancellationCheck(t)
	})

	t.Run("successful_fill_restores_overall_deadline", func(t *testing.T) {
		page, transport := newR1ScriptedPage(true)
		outerCtx, outerCancel := context.WithDeadline(context.Background(), overall)
		defer outerCancel()

		if err := page.fillAndSubmit(outerCtx, nil, 7, loginCredentials{Username: "u", Password: "p"}); err != nil {
			t.Fatalf("fillAndSubmit: %v", err)
		}

		transport.mu.Lock()
		deadlines := append([]time.Time(nil), transport.deadlines...)
		transport.mu.Unlock()

		if len(deadlines) < 2 {
			t.Fatalf("expected fill deadlines followed by restored overall deadline, got %v", deadlines)
		}
		for i, dl := range deadlines[:len(deadlines)-1] {
			if !dl.Equal(wantFillDeadline) {
				t.Fatalf("call %d deadline during fillAndSubmit = %v, want %v", i, dl, wantFillDeadline)
			}
		}
		restored := deadlines[len(deadlines)-1]
		if !restored.Equal(overall) {
			t.Fatalf("restored deadline after fillAndSubmit = %v, want overall deadline %v", restored, overall)
		}
	})
}

func TestR1_4_KeyDispatchErrorNeverLeaksEventPayloadFragments(t *testing.T) {
	oldSleep := loginSleep
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { loginSleep = oldSleep })

	creds := loginCredentials{
		Username: "review_account@example.invalid",
		Password: "Z_Synthetic_Pass_42!",
	}
	const fragment = `params={"type":"rawKeyDown","text":"Z","unmodifiedText":"Z","key":"Z","code":"KeyZ","windowsVirtualKeyCode":90}`
	forbiddenFragments := []string{
		fragment,
		`"text":"Z"`,
		`"unmodifiedText":"Z"`,
		`"code":"KeyZ"`,
		`"windowsVirtualKeyCode":90`,
		"Invalid parameters",
		creds.Username,
		creds.Password,
	}

	for _, eventPhase := range []string{"KeyDown", "char", "keyUp"} {
		t.Run("phase_"+eventPhase, func(t *testing.T) {
			page, transport := newR1ScriptedPage(true)
			var logBuf bytes.Buffer
			logger := logging.NewLogger(&logBuf)
			logging.Configure(logger, logging.Flags{Verbose: true, NoColor: true})

			transport.intercept = func(method string, raw json.RawMessage) (string, bool) {
				if method != "Input.dispatchKeyEvent" || transport.chrome.focused != loginPasswordSel {
					return "", false
				}
				s := string(raw)
				match := (eventPhase == "KeyDown" && (strings.Contains(s, `"rawKeyDown"`) || strings.Contains(s, `"keyDown"`))) ||
					(eventPhase == "char" && strings.Contains(s, `"char"`)) ||
					(eventPhase == "keyUp" && strings.Contains(s, `"keyUp"`))
				if match {
					return "Invalid parameters: " + fragment, true
				}
				return "", false
			}

			rawErr := page.typeCredentialInto(context.Background(), logger, 7, loginPasswordSel, creds.Password)
			if rawErr == nil {
				t.Fatalf("phase %s: expected key dispatch error", eventPhase)
			}
			wantDirect := "dispatch key event on " + loginPasswordSel
			if rawErr.Error() != wantDirect {
				t.Errorf("phase %s: direct typeCredentialInto error = %q, want %q", eventPhase, rawErr.Error(), wantDirect)
			}
			if unwrapped := errors.Unwrap(rawErr); unwrapped != nil {
				t.Errorf("phase %s: expected nil unwrap on controlled key dispatch error, got %v", eventPhase, unwrapped)
			}

			page2, transport2 := newR1ScriptedPage(true)
			transport2.intercept = transport.intercept
			fillErr := page2.fillAndSubmit(context.Background(), logger, 7, creds)
			if fillErr == nil {
				t.Fatalf("phase %s: expected fillAndSubmit error", eventPhase)
			}
			logger.Debug("filling sign-in form failed", "err", page2.redactSecrets(fillErr.Error()))
			finalErr := attemptRedactedError{cause: fillErr, text: page2.redactSecrets(fillErr.Error())}

			for _, forbidden := range forbiddenFragments {
				if strings.Contains(rawErr.Error(), forbidden) {
					t.Errorf("phase %s: direct error leaked %q: %s", eventPhase, forbidden, rawErr.Error())
				}
				if strings.Contains(fillErr.Error(), forbidden) {
					t.Errorf("phase %s: fillAndSubmit error leaked %q: %s", eventPhase, forbidden, fillErr.Error())
				}
				if strings.Contains(finalErr.Error(), forbidden) {
					t.Errorf("phase %s: final redacted error leaked %q: %s", eventPhase, forbidden, finalErr.Error())
				}
				if strings.Contains(logBuf.String(), forbidden) {
					t.Errorf("phase %s: verbose log leaked %q: %s", eventPhase, forbidden, logBuf.String())
				}
			}
		})
	}
}

func runDeterministicPacingCheck(t *testing.T) {
	t.Helper()

	t0 := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	virtualNow := t0
	withInjectedModelStudioClock(t, func() time.Time { return virtualNow })

	var recordedDelays []time.Duration
	var observedDeadlines []time.Time
	var remainingAtStep []time.Duration

	oldSleep := loginSleep
	loginSleep = func(ctx context.Context, d time.Duration) error {
		recordedDelays = append(recordedDelays, d)
		virtualNow = virtualNow.Add(d)
		if dl, ok := ctx.Deadline(); ok {
			observedDeadlines = append(observedDeadlines, dl)
			remainingAtStep = append(remainingAtStep, dl.Sub(virtualNow))
		}
		return ctx.Err()
	}
	t.Cleanup(func() { loginSleep = oldSleep })

	creds := loginCredentials{
		Username: "pacing_operator@example.com",
		Password: "Pacing#Secret!2026",
	}
	overall := t0.Add(loginOverallDeadline)
	wantFillDeadline := t0.Add(loginFillSubmitTimeout)

	page, transport := newR1ScriptedPage(true)
	outerCtx, outerCancel := context.WithDeadline(context.Background(), overall)
	defer outerCancel()

	if err := page.fillAndSubmit(outerCtx, nil, 7, creds); err != nil {
		t.Fatalf("fillAndSubmit over in-memory scripted transport: %v", err)
	}

	wantDelayCount := len([]rune(creds.Username)) + len([]rune(creds.Password))
	if len(recordedDelays) != wantDelayCount {
		t.Fatalf("len(recordedDelays) = %d, want %d", len(recordedDelays), wantDelayCount)
	}
	var sumDelays time.Duration
	for i, d := range recordedDelays {
		if d < loginKeyDelayMin || d > loginKeyDelayMax {
			t.Errorf("delay[%d] = %v, outside humanized bounds [%v, %v]", i, d, loginKeyDelayMin, loginKeyDelayMax)
		}
		sumDelays += d
	}
	if gotAdvance := virtualNow.Sub(t0); gotAdvance != sumDelays {
		t.Errorf("virtual clock advance = %v, want exact sum of delays %v", gotAdvance, sumDelays)
	}
	if sumDelays <= 0 || sumDelays >= loginFillSubmitTimeout {
		t.Errorf("accumulated virtual delay = %v, want within (0, %v)", sumDelays, loginFillSubmitTimeout)
	}

	if len(observedDeadlines) != wantDelayCount {
		t.Fatalf("len(observedDeadlines) = %d, want %d", len(observedDeadlines), wantDelayCount)
	}
	for i, dl := range observedDeadlines {
		if !dl.Equal(wantFillDeadline) {
			t.Fatalf("step %d fillCtx deadline = %v, want injected-clock deadline %v", i, dl, wantFillDeadline)
		}
		if remainingAtStep[i] <= 0 || remainingAtStep[i] > loginFillSubmitTimeout {
			t.Errorf("step %d remaining virtual budget = %v, want within (0, %v]", i, remainingAtStep[i], loginFillSubmitTimeout)
		}
	}

	transport.mu.Lock()
	deadlines := append([]time.Time(nil), transport.deadlines...)
	transport.mu.Unlock()
	if len(deadlines) < 2 {
		t.Fatalf("expected transport deadlines to be set during fillAndSubmit and restored afterward, got %v", deadlines)
	}
	for i, dl := range deadlines[:len(deadlines)-1] {
		if !dl.Equal(wantFillDeadline) {
			t.Fatalf("transport deadline[%d] = %v, want %v", i, dl, wantFillDeadline)
		}
	}
	if restored := deadlines[len(deadlines)-1]; !restored.Equal(overall) {
		t.Fatalf("restored transport deadline = %v, want %v", restored, overall)
	}

	if loginFillSubmitTimeout > 30*time.Second || loginFillSubmitTimeout >= loginOverallDeadline {
		t.Errorf("loginFillSubmitTimeout = %v, must be <= 30s and < loginOverallDeadline (%v)", loginFillSubmitTimeout, loginOverallDeadline)
	}
}

func TestR1_5_DeterministicClockAndInMemoryTransportPacing(t *testing.T) {
	runDeterministicPacingCheck(t)

	t.Run("virtual_clock_exceeding_fill_deadline_aborts_deterministically", func(t *testing.T) {
		t0 := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
		virtualNow := t0
		withInjectedModelStudioClock(t, func() time.Time { return virtualNow })

		oldSleep := loginSleep
		loginSleep = func(ctx context.Context, _ time.Duration) error {
			virtualNow = virtualNow.Add(12 * time.Second)
			return ctx.Err()
		}
		t.Cleanup(func() { loginSleep = oldSleep })

		page, _ := newR1ScriptedPage(true)
		err := page.fillAndSubmit(context.Background(), nil, 7, loginCredentials{Username: "abcd", Password: "efgh"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded when virtual clock crosses 30s fill deadline, got %v", err)
		}
		if gotAdvance := virtualNow.Sub(t0); gotAdvance != 36*time.Second {
			t.Errorf("virtual clock advanced %v before aborting, want 36s (3 steps of 12s)", gotAdvance)
		}
	})
}

func runMidKeyDownCancellationCheck(t *testing.T) {
	t.Helper()

	page, transport := newR1ScriptedPage(true)
	transport.abortRead = make(chan struct{})
	transport.readEntered = make(chan struct{})
	transport.closedSig = make(chan struct{})
	t.Cleanup(func() { transport.releaseAbort() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	transport.intercept = func(method string, raw json.RawMessage) (string, bool) {
		if method == "Input.dispatchKeyEvent" && strings.Contains(string(raw), "rawKeyDown") {
			return "BLOCK_READ", true
		}
		return "", false
	}

	done := make(chan error, 1)
	go func() {
		done <- page.typeCredentialInto(ctx, nil, 7, loginUsernameSel, "X")
	}()

	watchdog := time.NewTimer(2 * time.Second)
	defer watchdog.Stop()

	select {
	case <-transport.readEntered:
	case <-watchdog.C:
		transport.releaseAbort()
		<-done
		t.Fatalf("timed out waiting for Read to block in-flight")
	}

	cancel()

	var err error
	select {
	case err = <-done:
	case <-watchdog.C:
		transport.releaseAbort()
		<-done
		t.Fatalf("cancellation did not interrupt blocked in-flight Read via SetDeadline/Close")
	}

	transport.mu.Lock()
	interruptedByDeadline := transport.interruptedByDeadline
	interruptedByClose := transport.interruptedByClose
	closed := transport.closed
	releasedByAbort := transport.releasedByAbort
	transport.mu.Unlock()

	if releasedByAbort {
		t.Fatalf("in-flight Read was released by failure watchdog abort instead of forced transport interruption")
	}
	if !interruptedByDeadline || !interruptedByClose || !closed {
		t.Fatalf("expected forced transport interruption before accepting result (interruptedByDeadline=%v, interruptedByClose=%v, closed=%v)",
			interruptedByDeadline, interruptedByClose, closed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on mid-keyDown cancellation, got %v", err)
	}
	transport.chrome.mu.Lock()
	keyCount := len(transport.chrome.keyEvents)
	transport.chrome.mu.Unlock()
	if keyCount != 1 {
		t.Fatalf("expected cancellation during rawKeyDown to stop after 1 key event (before char/keyUp), got %d", keyCount)
	}
	if reuseErr := page.call("Page.enable", nil, nil); reuseErr == nil {
		t.Fatalf("expected subsequent call on canceled/interrupted cdpConn to fail")
	}

	page2, transport2 := newR1ScriptedPage(true)
	transport2.abortRead = make(chan struct{})
	transport2.closedSig = make(chan struct{})
	t.Cleanup(func() { transport2.releaseAbort() })

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	transport2.pending.Write(encodeFakeWSFrame([]byte(`{"id":1,"result":{}}`)))
	if _, peekErr := page2.reader.Peek(1); peekErr != nil {
		t.Fatalf("failed to pre-buffer reply frame in page2.reader: %v", peekErr)
	}

	watchdog2 := time.NewTimer(2 * time.Second)
	defer watchdog2.Stop()

	transport2.intercept = func(method string, _ json.RawMessage) (string, bool) {
		if method == "Input.dispatchKeyEvent" {
			cancel2()
			select {
			case <-transport2.closedSig:
			case <-watchdog2.C:
				transport2.releaseAbort()
				t.Errorf("timed out waiting for AfterFunc to interrupt/close transport")
			}
			return "NO_REPLY", true
		}
		return "", false
	}

	err2 := page2.callContext(ctx2, "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "X"}, nil)

	transport2.mu.Lock()
	interruptedByDeadline2 := transport2.interruptedByDeadline
	interruptedByClose2 := transport2.interruptedByClose
	closed2 := transport2.closed
	releasedByAbort2 := transport2.releasedByAbort
	transport2.mu.Unlock()

	if releasedByAbort2 {
		t.Fatalf("pre-buffered cancellation check was released by watchdog abort instead of forced transport interruption")
	}
	if !interruptedByDeadline2 || !interruptedByClose2 || !closed2 {
		t.Fatalf("expected AfterFunc to force transport deadline+close on cancellation (interruptedByDeadline=%v, interruptedByClose=%v, closed=%v)",
			interruptedByDeadline2, interruptedByClose2, closed2)
	}
	if !errors.Is(err2, context.Canceled) {
		t.Fatalf("expected callContext to reject pre-buffered reply after forced transport interruption and return context.Canceled, got %v", err2)
	}
}

func TestR2_1_CancellationRequiresForcedTransportInterruptionBeforeResult(t *testing.T) {
	t0 := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)
	withInjectedModelStudioClock(t, func() time.Time { return t0 })

	oldSleep := loginSleep
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { loginSleep = oldSleep })

	runMidKeyDownCancellationCheck(t)
}
