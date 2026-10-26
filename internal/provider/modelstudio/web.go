package modelstudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/httpclient"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
	"github.com/joshuadavidthomas/vibeusage/internal/models"
)

const (
	modelStudioConsoleBaseURL  = "https://modelstudio.console.alibabacloud.com"
	modelStudioDashboardURL    = modelStudioConsoleBaseURL + "/ap-southeast-1/?tab=plan#/efm/subscription/token-plan/enterprise"
	modelStudioRegion          = "ap-southeast-1"
	modelStudioTeamProduct     = "sfm_tokenplanteams_dp_intl"
	bssProduct                 = "BssOpenAPI-V3"
	subscriptionSummaryAction  = "GetSubscriptionSummary"
	browserUserAgent           = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	modelStudioSessionCooldown = 30 * time.Minute
	// modelStudioSessionWarnFraction is the fraction of the minimum observed
	// session lifetime at which the panel warns to sign in soon. It is scaled
	// to the minimum recorded lifetime — never a fabricated absolute TTL —
	// because Alibaba Cloud invalidates console sessions server-side on an
	// unpredictable schedule, so only observed lifetimes bound the next expiry.
	modelStudioSessionWarnFraction = 0.75
	sessionLifetimesFilename       = "modelstudio-session-lifetimes.json"
	maxSessionLifetimes            = 10
)

var modelStudioNow = time.Now

type WebConsoleStrategy struct {
	HTTPTimeout    float64
	ConsoleBaseURL string
	DashboardURL   string
	Region         string
	ProductCode    string
}

type sessionCredentials struct {
	Cookie       string    `json:"cookie"`
	SecToken     string    `json:"sec_token,omitempty"`
	Source       string    `json:"source,omitempty"`
	BrowserLabel string    `json:"browser,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

const (
	sessionSourceEnvironment = "environment"
	sessionSourceManual      = "manual"
	sessionSourceBrowser     = "browser"
)

func (s *WebConsoleStrategy) IsAvailable() bool {
	if cookie := os.Getenv("MODELSTUDIO_COOKIE"); cookie != "" {
		return true
	}
	data, err := config.ReadCredential("modelstudio", "session")
	return (err == nil && len(data) > 0) || hasModelStudioBrowserProfiles()
}

func loadSessionCredential() (sessionCredentials, error) {
	if envCookie := strings.TrimSpace(os.Getenv("MODELSTUDIO_COOKIE")); envCookie != "" {
		return sessionCredentials{Cookie: envCookie, Source: sessionSourceEnvironment}, nil
	}

	data, err := config.ReadCredential("modelstudio", "session")
	if err != nil || len(data) == 0 {
		return sessionCredentials{}, err
	}

	var raw struct {
		Cookie       string `json:"cookie"`
		SecToken     string `json:"sec_token,omitempty"`
		Source       string `json:"source,omitempty"`
		BrowserLabel string `json:"browser,omitempty"`
		UpdatedAt    string `json:"updated_at,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err == nil && raw.Cookie != "" {
		creds := sessionCredentials{
			Cookie:       raw.Cookie,
			SecToken:     raw.SecToken,
			Source:       raw.Source,
			BrowserLabel: raw.BrowserLabel,
		}
		if creds.Source == "" {
			creds.Source = sessionSourceManual
		}
		if strings.TrimSpace(raw.UpdatedAt) != "" {
			var parsed time.Time
			if err := parsed.UnmarshalText([]byte(strings.TrimSpace(raw.UpdatedAt))); err == nil {
				creds.UpdatedAt = parsed
			}
		}
		return creds, nil
	}
	return sessionCredentials{Cookie: string(data), Source: sessionSourceManual}, nil
}

type sessionLifetimeEntry struct {
	BornAt          time.Time `json:"born_at"`
	DiedAt          time.Time `json:"died_at"`
	LifetimeSeconds int       `json:"lifetime_seconds"`
	Source          string    `json:"source"`
}

func (e sessionLifetimeEntry) isValid() bool {
	if e.BornAt.IsZero() || e.DiedAt.IsZero() || !e.DiedAt.After(e.BornAt) {
		return false
	}
	computed := int(e.DiedAt.UTC().Sub(e.BornAt.UTC()).Seconds())
	return computed > 0 && e.LifetimeSeconds == computed
}

func filterValidLifetimeEntries(entries []sessionLifetimeEntry) []sessionLifetimeEntry {
	valid := make([]sessionLifetimeEntry, 0, len(entries))
	for _, e := range entries {
		if e.isValid() {
			valid = append(valid, e)
		}
	}
	return valid
}

type sessionLifetimeFile struct {
	Entries []sessionLifetimeEntry `json:"entries"`
}

type sessionLifetimeStore struct {
	path string
}

func defaultSessionLifetimeStore() sessionLifetimeStore {
	return sessionLifetimeStore{
		path: filepath.Join(config.DataDir(), sessionLifetimesFilename),
	}
}

func (s sessionLifetimeStore) load() ([]sessionLifetimeEntry, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var file sessionLifetimeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	return file.Entries, nil
}

func (s sessionLifetimeStore) record(creds sessionCredentials, died time.Time) error {
	if creds.Source == sessionSourceEnvironment {
		return nil
	}
	if creds.UpdatedAt.IsZero() {
		return nil
	}
	born := creds.UpdatedAt.UTC()
	died = died.UTC()
	lifetime := died.Sub(born)
	if lifetime <= 0 {
		return nil
	}
	entries, err := s.load()
	if err != nil {
		return err
	}
	entries = filterValidLifetimeEntries(entries)
	for _, existing := range entries {
		if existing.BornAt.Equal(born) {
			return nil
		}
	}
	source := creds.Source
	if source == "" {
		source = sessionSourceManual
	}
	entries = append(entries, sessionLifetimeEntry{
		BornAt:          born,
		DiedAt:          died,
		LifetimeSeconds: int(lifetime.Seconds()),
		Source:          source,
	})
	if len(entries) > maxSessionLifetimes {
		entries = entries[len(entries)-maxSessionLifetimes:]
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	payload, err := json.Marshal(sessionLifetimeFile{Entries: entries})
	if err != nil {
		return err
	}
	if err := os.Chmod(s.path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return config.AtomicWriteFile(s.path, payload)
}

func (s sessionLifetimeStore) minLifetimeSeconds() int {
	entries, err := s.load()
	if err != nil {
		return 0
	}
	entries = filterValidLifetimeEntries(entries)
	if len(entries) == 0 {
		return 0
	}
	minSec := 0
	for _, e := range entries {
		if minSec == 0 || e.LifetimeSeconds < minSec {
			minSec = e.LifetimeSeconds
		}
	}
	return minSec
}

func (s sessionLifetimeStore) evaluateSession(creds sessionCredentials, now time.Time) *models.SessionInfo {
	if creds.Source == sessionSourceEnvironment || creds.UpdatedAt.IsZero() {
		return nil
	}
	born := creds.UpdatedAt.UTC()
	age := now.UTC().Sub(born)
	if age < 0 {
		age = 0
	}
	ageSec := int(age.Seconds())
	minSec := s.minLifetimeSeconds()
	warn := minSec > 0 && float64(ageSec) >= float64(minSec)*modelStudioSessionWarnFraction
	return &models.SessionInfo{
		BornAt:             born,
		AgeSeconds:         ageSec,
		MinLifetimeSeconds: minSec,
		WarnSignInSoon:     warn,
	}
}

// CurrentSessionInfoAt evaluates the current stored Model Studio session
// against the durable lifetime ledger at the given timestamp. It returns
// nil when credentials come from the environment, have no valid born-time,
// or cannot be read.
func CurrentSessionInfoAt(now time.Time) (*models.SessionInfo, error) {
	creds, err := loadSessionCredential()
	if err != nil {
		return nil, err
	}
	return defaultSessionLifetimeStore().evaluateSession(creds, now), nil
}

func (s *WebConsoleStrategy) Fetch(ctx context.Context) (fetch.FetchResult, error) {
	creds, err := loadSessionCredential()
	if err != nil {
		return fetch.ResultFail(fmt.Sprintf("Model Studio: reading stored browser session: %v", err)), nil
	}

	client := httpclient.NewFromConfig(s.HTTPTimeout)
	if creds.Cookie == "" {
		return s.fetchImportedBrowserSession(ctx, client, sessionCredentials{})
	}

	snapshot, err := s.fetchWithSession(ctx, client, creds)
	if err == nil {
		// A stored or environment session that works is a real recovery.
		clearLoginPark()
		snapshot.Session = defaultSessionLifetimeStore().evaluateSession(creds, modelStudioNow())
		return fetch.ResultOK(*snapshot), nil
	}
	if !errors.Is(err, errModelStudioSession) {
		return fetch.ResultFail(fmt.Sprintf("Model Studio web console API failed: %v", err)), nil
	}
	if recErr := defaultSessionLifetimeStore().record(creds, modelStudioNow()); recErr != nil {
		logging.FromContext(ctx).Warn("recording Model Studio session lifetime failed", "err", recErr)
	}
	if creds.Source == sessionSourceEnvironment {
		return fetch.ResultFatal(modelStudioSessionHint(creds, errBrowserCookieImportUnavailable)), nil
	}

	return s.fetchImportedBrowserSession(ctx, client, creds)
}

func (s *WebConsoleStrategy) fetchWithSession(
	ctx context.Context,
	client *httpclient.Client,
	creds sessionCredentials,
) (*models.UsageSnapshot, error) {
	consoleBaseURL := s.ConsoleBaseURL
	if consoleBaseURL == "" {
		consoleBaseURL = modelStudioConsoleBaseURL
	}
	dashboardURL := s.DashboardURL
	if dashboardURL == "" {
		dashboardURL = modelStudioDashboardURL
	}
	region := s.Region
	if region == "" {
		region = modelStudioRegion
	}
	productCode := s.ProductCode
	if productCode == "" {
		productCode = modelStudioTeamProduct
	}

	secToken := creds.SecToken
	if secToken == "" {
		secToken = extractCookieValue(creds.Cookie, "sec_token")
	}
	if secToken == "" {
		// The quota request does not always include sec_token in its Cookie
		// header. The console resolves it from this authenticated endpoint.
		secToken, _ = s.resolveSECToken(ctx, client, consoleBaseURL, creds.Cookie)
	}

	summaryData, err := s.callSubscriptionSummary(
		ctx,
		client,
		consoleBaseURL,
		dashboardURL,
		region,
		productCode,
		creds.Cookie,
		secToken,
	)
	if err != nil {
		return nil, err
	}

	snapshot, parseErr := parseModelStudioConsoleResponses(summaryData, nil, nil, nil, "web")
	if parseErr != nil {
		return nil, parseErr
	}
	return snapshot, nil
}

func (s *WebConsoleStrategy) fetchImportedBrowserSession(
	ctx context.Context,
	client *httpclient.Client,
	previous sessionCredentials,
) (fetch.FetchResult, error) {
	imported, importErr := importModelStudioBrowserSession(ctx)
	loginAttempted := false

	for {
		if importErr != nil {
			if !loginAttempted {
				loginAttempted = true
				session, loginErr := s.autoLogin(ctx)
				switch {
				case loginErr == nil:
					imported, importErr = session, nil
				case !errors.Is(loginErr, errAutoLoginUnavailable):
					importErr = fmt.Errorf("%w; %w", importErr, autoLoginFailure(loginErr))
				}
			}
			if importErr != nil {
				return fetch.ResultThrottled(modelStudioSessionHint(previous, importErr), time.Now().Add(modelStudioSessionCooldown)), nil
			}
		}

		now := modelStudioNow()
		creds := sessionCredentials{
			Cookie:       imported.Cookie,
			Source:       sessionSourceBrowser,
			BrowserLabel: imported.SourceLabel,
			UpdatedAt:    now,
		}
		snapshot, err := s.fetchWithSession(ctx, client, creds)
		if err == nil {
			// Cache only a browser-imported session that has completed an
			// authenticated quota request. The credentials store is
			// permission-restricted to the user.
			if data, marshalErr := json.Marshal(creds); marshalErr == nil {
				if config.WriteCredential("modelstudio", "session", data) == nil {
					clearLoginPark()
				}
			}
			snapshot.Session = defaultSessionLifetimeStore().evaluateSession(creds, now)
			return fetch.ResultOK(*snapshot), nil
		}
		if !errors.Is(err, errModelStudioSession) {
			return fetch.ResultFail(fmt.Sprintf("Model Studio web console API failed after importing %s cookies: %v", imported.SourceLabel, err)), nil
		}

		// The browser holds a session, but the console no longer accepts it.
		// Renew once and retry with the renewed session itself: its cookies
		// live only in the sign-in tab's context, so re-importing them after
		// the tab closes can miss them.
		if loginAttempted {
			return fetch.ResultThrottled(modelStudioSessionHint(creds, nil), time.Now().Add(modelStudioSessionCooldown)), nil
		}
		loginAttempted = true
		session, loginErr := s.autoLogin(ctx)
		if loginErr != nil {
			var hintErr error
			if !errors.Is(loginErr, errAutoLoginUnavailable) {
				hintErr = autoLoginFailure(loginErr)
			}
			return fetch.ResultThrottled(modelStudioSessionHint(creds, hintErr), time.Now().Add(modelStudioSessionCooldown)), nil
		}
		imported, importErr = session, nil
	}
}

// autoLogin renews the browser session through the configured login_command
// and headed CDP port and returns the renewed session captured from the
// sign-in tab. It returns errAutoLoginUnavailable when not configured.
func (s *WebConsoleStrategy) autoLogin(ctx context.Context) (browserSession, error) {
	command, port, err := autoLoginSettings()
	if err != nil {
		return browserSession{}, err
	}
	// Serialize automatic attempts across processes before any park or
	// cooldown read, so a concurrent attempt cannot overwrite or clear
	// another attempt's guard. Ownership is held until this function returns.
	release, lockErr := acquireModelStudioLoginLock()
	if lockErr != nil {
		if errors.Is(lockErr, errAutoLoginInProgress) {
			return browserSession{}, lockErr
		}
		return browserSession{}, fmt.Errorf("%w: cannot acquire park guard lock: %w", errAutoLoginGuardUnavailable, lockErr)
	}
	defer release()
	// A park outranks every time-based brake: refuse before the cooldown
	// marker is touched and before login_command can run.
	if park, parked := loadLoginPark(); parked {
		return browserSession{}, &loginParkedError{Kind: park.Kind, Port: port}
	}
	if last := lastLoginAttempt(); !last.IsZero() && time.Since(last) < loginCooldown {
		return browserSession{}, fmt.Errorf("%w (%s ago); next attempt after %s",
			errAutoLoginCoolingDown, time.Since(last).Round(time.Second), loginCooldown)
	}
	// Fail closed: record a provisional park guard before anything that can
	// reach Alibaba (login_command, the driver, even the attempt marker). If
	// the guard cannot be stored, a blocker (or a crash) could not be
	// remembered by the next process, so no automation starts at all.
	if err := writeLoginPark(loginBlockerPending, pendingGuardReason, time.Now()); err != nil {
		return browserSession{}, fmt.Errorf("%w: cannot record park guard: %w", errAutoLoginGuardUnavailable, err)
	}
	recordLoginAttempt(time.Now())
	creds, err := loadLoginCredentials(ctx, command)
	if err != nil {
		clearLoginPark() // a credential-command failure is not a blocker
		return browserSession{}, err
	}
	session, err := autoLoginModelStudio(ctx, port, creds)
	var blocked *loginBlockedError
	if errors.As(err, &blocked) {
		// Convert the provisional guard into the real park. If that write
		// fails the provisional guard still parks auto-login; say so.
		parked := &loginParkedError{Kind: blocked.Kind, Port: port}
		if writeErr := writeLoginPark(blocked.Kind, blocked.Reason, time.Now()); writeErr != nil {
			parked.MarkerErr = writeErr
		}
		return browserSession{}, parked
	}
	// Success, a form error, a timeout or any other non-blocker outcome:
	// release the guard. A panic or crash before this point leaves it in
	// place on purpose.
	clearLoginPark()
	return session, err
}

// autoLoginFailure words an automatic sign-in error for the session hint.
// A park reads as a state to act on, not as another failed attempt.
func autoLoginFailure(err error) error {
	if errors.Is(err, errAutoLoginParked) || errors.Is(err, errAutoLoginGuardUnavailable) || errors.Is(err, errAutoLoginInProgress) {
		return err
	}
	return fmt.Errorf("automatic sign-in failed: %w", err)
}

// ExtendTimeout gives Fetch room for a browser sign-in on top of the normal
// request budget, but only when automatic sign-in is configured.
func (s *WebConsoleStrategy) ExtendTimeout(base time.Duration) time.Duration {
	if _, _, err := autoLoginSettings(); err != nil {
		return base
	}
	if _, parked := loadLoginPark(); parked {
		return base // no sign-in will run, so no budget is needed
	}
	return base + loginOverallDeadline
}

// modelStudioSessionHint explains why no usable session exists and exactly
// where to sign in so the next run can pick the cookies up automatically.
// importErr, when present, carries the list of browsers that were probed.
func modelStudioSessionHint(creds sessionCredentials, importErr error) string {
	if creds.Source == sessionSourceEnvironment {
		return "Model Studio browser session expired. Update MODELSTUDIO_COOKIE with a fresh Cookie header."
	}

	var b strings.Builder
	switch {
	case creds.Source == sessionSourceBrowser && creds.BrowserLabel != "":
		fmt.Fprintf(&b, "Model Studio browser session expired (last imported from %s).", creds.BrowserLabel)
	case creds.Source == "" && importErr != nil:
		b.WriteString("No signed-in Model Studio browser session was found.")
	default:
		b.WriteString("Model Studio browser session expired.")
	}
	b.WriteString(" Sign in to the Singapore Team Token Plan page (")
	b.WriteString(modelStudioDashboardURL)
	b.WriteString(") in a browser vibeusage can read, then retry; the cookies are imported automatically.")
	if detail := browserImportDetail(importErr); detail != "" {
		b.WriteString(" Last import attempt: ")
		b.WriteString(detail)
		b.WriteString(".")
	}
	b.WriteString(" Readable browsers: any Chrome started with --remote-debugging-port (set cdp_ports under [providers.modelstudio] or VIBEUSAGE_CDP_PORTS), or run vibeusage auth modelstudio to paste the Cookie header manually.")
	return b.String()
}

// browserImportDetail strips the generic sentinel prefix from an import error
// so only the specific, user-actionable part is shown.
func browserImportDetail(importErr error) string {
	if importErr == nil {
		return ""
	}
	detail := strings.TrimSpace(importErr.Error())
	detail = strings.TrimPrefix(detail, errBrowserCookieImportUnavailable.Error())
	detail = strings.TrimLeft(detail, ":; ")
	return strings.TrimSuffix(detail, ".")
}

func (s *WebConsoleStrategy) resolveSECToken(
	ctx context.Context,
	client *httpclient.Client,
	consoleBaseURL string,
	cookie string,
) (string, error) {
	endpoint := strings.TrimRight(consoleBaseURL, "/") + "/tool/user/info.json"
	resp, err := client.DoCtx(
		ctx,
		http.MethodGet,
		endpoint,
		nil,
		httpclient.WithHeader("Accept", "application/json, text/plain, */*"),
		httpclient.WithHeader("Cookie", cookie),
		httpclient.WithHeader("Referer", strings.TrimRight(consoleBaseURL, "/")+"/"),
		httpclient.WithHeader("User-Agent", browserUserAgent),
		httpclient.WithSkipRawDump(),
	)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving sec_token: HTTP %d", resp.StatusCode)
	}

	secToken, err := parseConsoleSECToken(resp.Body)
	if err != nil {
		return "", fmt.Errorf("resolving sec_token: %w", err)
	}
	return secToken, nil
}

func (s *WebConsoleStrategy) callSubscriptionSummary(
	ctx context.Context,
	client *httpclient.Client,
	consoleBaseURL string,
	dashboardURL string,
	region string,
	productCode string,
	cookie string,
	secToken string,
) ([]byte, error) {
	endpoint, err := url.Parse(strings.TrimRight(consoleBaseURL, "/") + "/data/api.json")
	if err != nil {
		return nil, fmt.Errorf("building subscription summary URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("action", subscriptionSummaryAction)
	query.Set("product", bssProduct)
	query.Set("_tag", "")
	endpoint.RawQuery = query.Encode()

	form := url.Values{}
	form.Set("action", subscriptionSummaryAction)
	form.Set("product", bssProduct)
	form.Set("params", fmt.Sprintf(`{"ProductCode":"%s"}`, productCode))
	form.Set("region", region)
	if secToken != "" {
		form.Set("sec_token", secToken)
	}

	opts := []httpclient.RequestOption{
		httpclient.WithHeader("Accept", "*/*"),
		httpclient.WithHeader("Content-Type", "application/x-www-form-urlencoded"),
		httpclient.WithHeader("Cookie", cookie),
		httpclient.WithHeader("Origin", strings.TrimRight(consoleBaseURL, "/")),
		httpclient.WithHeader("Referer", dashboardURL),
		httpclient.WithHeader("User-Agent", browserUserAgent),
		httpclient.WithHeader("X-Requested-With", "XMLHttpRequest"),
	}
	if csrf := extractCookieValue(cookie, "login_aliyunid_csrf"); csrf != "" {
		opts = append(opts, httpclient.WithHeader("x-csrf-token", csrf), httpclient.WithHeader("x-xsrf-token", csrf))
	}

	resp, err := client.DoCtx(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()), opts...)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: HTTP %d", errModelStudioSession, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.URL != nil && isAlibabaLoginURL(resp.URL) {
		return nil, fmt.Errorf("%w: redirected to Alibaba login", errModelStudioSession)
	}
	if isLikelyLoginHTML(resp.Body) {
		return nil, fmt.Errorf("%w: Alibaba console returned a login page", errModelStudioSession)
	}

	return resp.Body, nil
}

func isAlibabaLoginURL(endpoint *url.URL) bool {
	if endpoint == nil {
		return false
	}
	normalized := strings.ToLower(endpoint.Hostname() + endpoint.Path)
	return strings.Contains(normalized, "passport") ||
		strings.Contains(normalized, "signin") ||
		strings.Contains(normalized, "login")
}

func extractCookieValue(cookieHeader, name string) string {
	parts := strings.Split(cookieHeader, ";")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, name+"=") {
			return strings.TrimPrefix(p, name+"=")
		}
	}
	return ""
}
