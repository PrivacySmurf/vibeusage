//go:build darwin

package modelstudio

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joshuadavidthomas/vibeusage/internal/keychain"
)

const chromiumEpochOffsetMicroseconds = int64(11644473600000000)

type chromiumCookieProfile struct {
	Label           string
	CookieDB        string
	KeychainService string
	KeychainAccount string
}

type chromiumCookieRow struct {
	HostKey      string `json:"host_key"`
	Path         string `json:"path"`
	Name         string `json:"name"`
	ValueHex     string `json:"value_hex"`
	EncryptedHex string `json:"encrypted_hex"`
	ExpiresUTC   int64  `json:"expires_utc"`
	IsSecure     int    `json:"is_secure"`
	LastAccess   int64  `json:"last_access_utc"`
	DBVersion    int    `json:"db_version"`
}

var (
	browserCookieHomeDir     = os.UserHomeDir
	readBrowserSafeStorage   = keychain.ReadGenericPassword
	chromiumCookieSQLitePath = "/usr/bin/sqlite3"
)

type cdpCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Secure  bool    `json:"secure"`
	Expires float64 `json:"expires"`
}

type cdpResponse struct {
	ID     int `json:"id"`
	Result struct {
		Cookies []cdpCookie `json:"cookies"`
	} `json:"result"`
}

func cdpActivePortFiles() []string {
	home, err := browserCookieHomeDir()
	if err != nil || home == "" {
		return nil
	}
	appSupport := filepath.Join(home, "Library", "Application Support")
	candidates := []string{
		filepath.Join(appSupport, "Google", "Chrome", "DevToolsActivePort"),
		filepath.Join(appSupport, "Google", "Chrome Beta", "DevToolsActivePort"),
		filepath.Join(appSupport, "Google", "Chrome Canary", "DevToolsActivePort"),
		filepath.Join(appSupport, "Chromium", "DevToolsActivePort"),
		filepath.Join(appSupport, "BraveSoftware", "Brave-Browser", "DevToolsActivePort"),
		filepath.Join(appSupport, "Microsoft Edge", "DevToolsActivePort"),
	}
	var found []string
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			found = append(found, path)
		}
	}
	return found
}

func parseDevToolsActivePort(filePath string) (string, string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("invalid DevToolsActivePort in %s", filePath)
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}

func approveChromeRemoteDebugging() {
	script := `
using terms from application "System Events"
  on clickAllow(nodeRef)
    try
      if (role of nodeRef as text) is "AXButton" and (description of nodeRef as text) is "Allow" then
        perform action "AXPress" of nodeRef
        return true
      end if
    end try
    try
      repeat with childRef in UI elements of nodeRef
        if my clickAllow(childRef) then return true
      end repeat
    end try
    return false
  end clickAllow
end using terms from

tell application "System Events"
  if not (exists process "Google Chrome") then return "no-chrome"
  tell process "Google Chrome"
    repeat with w in windows
      try
        repeat with s in sheets of w
          if (name of s as text) is "Allow remote debugging?" then
            if my clickAllow(s) then return "approved"
            return "sheet-without-allow"
          end if
        end repeat
      end try
    end repeat
  end tell
end tell
return "no-sheet"
`
	for i := 0; i < 5; i++ {
		time.Sleep(300 * time.Millisecond)
		out, err := exec.Command("osascript", "-e", script).Output()
		if err == nil && strings.TrimSpace(string(out)) == "approved" {
			return
		}
	}
}

func fetchCookiesFromCDPEndpoint(ctx context.Context, port, path string) ([]browserCookie, error) {
	go approveChromeRemoteDebugging()

	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}

	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	secKey := base64.StdEncoding.EncodeToString(keyBytes)

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, port, secKey)
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, fmt.Errorf("ws handshake: %w", err)
	}
	if resp.StatusCode != 101 {
		return nil, fmt.Errorf("ws handshake status %d", resp.StatusCode)
	}

	h := sha1.New()
	h.Write([]byte(secKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expectedAccept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if resp.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		return nil, fmt.Errorf("invalid Sec-WebSocket-Accept")
	}

	payload := []byte(`{"id":1,"method":"Storage.getCookies"}`)
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, 0x81)
	maskKey := []byte{0x12, 0x34, 0x56, 0x78}
	length := len(payload)
	if length < 126 {
		frame = append(frame, byte(0x80|length))
	} else if length <= 65535 {
		frame = append(frame, 0x80|126, byte(length>>8), byte(length))
	}
	frame = append(frame, maskKey...)
	for i, b := range payload {
		frame = append(frame, b^maskKey[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}

	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(reader, header); err != nil {
			return nil, err
		}
		isMasked := (header[1] & 0x80) != 0
		payloadLen := int64(header[1] & 0x7f)
		switch payloadLen {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(reader, ext); err != nil {
				return nil, err
			}
			payloadLen = int64(ext[0])<<8 | int64(ext[1])
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(reader, ext); err != nil {
				return nil, err
			}
			payloadLen = int64(ext[0])<<56 | int64(ext[1])<<48 | int64(ext[2])<<40 | int64(ext[3])<<32 |
				int64(ext[4])<<24 | int64(ext[5])<<16 | int64(ext[6])<<8 | int64(ext[7])
		}

		var mask [4]byte
		if isMasked {
			if _, err := io.ReadFull(reader, mask[:]); err != nil {
				return nil, err
			}
		}

		buf := make([]byte, payloadLen)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return nil, err
		}
		if isMasked {
			for i := range buf {
				buf[i] ^= mask[i%4]
			}
		}

		var res cdpResponse
		if err := json.Unmarshal(buf, &res); err == nil && res.ID == 1 {
			var out []browserCookie
			for _, c := range res.Result.Cookies {
				var exp time.Time
				if c.Expires > 0 {
					exp = time.Unix(int64(c.Expires), 0)
				}
				out = append(out, browserCookie{
					Name:       c.Name,
					Value:      c.Value,
					Domain:     c.Domain,
					Path:       c.Path,
					Secure:     c.Secure,
					ExpiresAt:  exp,
					LastAccess: time.Now(),
				})
			}
			return out, nil
		}
	}
}

func importModelStudioCDPSession(ctx context.Context) (browserSession, error) {
	portFiles := cdpActivePortFiles()
	if len(portFiles) == 0 {
		return browserSession{}, fmt.Errorf("no DevToolsActivePort found")
	}

	cdpCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	for _, file := range portFiles {
		port, path, err := parseDevToolsActivePort(file)
		if err != nil {
			continue
		}
		cookies, err := fetchCookiesFromCDPEndpoint(cdpCtx, port, path)
		if err != nil {
			continue
		}
		header, err := buildModelStudioCookieHeader(
			cookies,
			modelStudioConsoleBaseURL+"/data/api.json",
			time.Now(),
		)
		if err != nil {
			continue
		}
		label := "Chrome (CDP)"
		if strings.Contains(file, "Chromium") {
			label = "Chromium (CDP)"
		} else if strings.Contains(file, "Brave") {
			label = "Brave (CDP)"
		} else if strings.Contains(file, "Edge") {
			label = "Edge (CDP)"
		}
		return browserSession{
			Cookie:      header,
			SourceLabel: label,
		}, nil
	}

	return browserSession{}, fmt.Errorf("no authenticated Alibaba session found via CDP")
}

func platformHasModelStudioBrowserProfiles() bool {
	if len(cdpActivePortFiles()) > 0 {
		return true
	}
	return len(chromiumCookieProfiles()) > 0
}

func platformImportModelStudioBrowserSession(ctx context.Context) (browserSession, error) {
	// 1. Prefer Chrome DevTools Protocol (CDP) live extraction.
	// This retrieves active cookies directly from Chrome memory via WebSocket,
	// completely avoiding SQLite disk locks and macOS Keychain access dialogs.
	if session, err := importModelStudioCDPSession(ctx); err == nil {
		return session, nil
	}

	// 2. Keychain Safe Storage is disabled by default for unattended background runs
	// because /usr/bin/security triggers an interactive macOS password dialog.
	if os.Getenv("VIBEUSAGE_ALLOW_KEYCHAIN") != "1" {
		return browserSession{}, fmt.Errorf("%w: Chrome CDP unavailable and macOS Keychain access disabled to prevent login popups (set VIBEUSAGE_ALLOW_KEYCHAIN=1 to allow)", errBrowserCookieImportUnavailable)
	}

	profiles := chromiumCookieProfiles()
	if len(profiles) == 0 {
		return browserSession{}, fmt.Errorf("%w: no supported Chrome, Chromium, or Codex cookie profile found", errBrowserCookieImportUnavailable)
	}

	type safeStorageKey struct {
		service string
		account string
	}
	passwords := make(map[safeStorageKey]string)
	passwordErrors := make(map[safeStorageKey]error)
	var importErrors []string
	for _, profile := range profiles {
		key := safeStorageKey{service: profile.KeychainService, account: profile.KeychainAccount}
		password, lookedUp := passwords[key]
		if !lookedUp {
			var err error
			password, err = readBrowserSafeStorage(key.service, key.account)
			passwords[key] = password
			passwordErrors[key] = err
		}
		if err := passwordErrors[key]; err != nil || password == "" {
			importErrors = append(importErrors, profile.Label+": Safe Storage Keychain access unavailable")
			continue
		}

		cookies, err := readChromiumCookies(ctx, profile.CookieDB, password)
		if err != nil {
			importErrors = append(importErrors, profile.Label+": "+redactBrowserImportError(err))
			continue
		}
		header, err := buildModelStudioCookieHeader(
			cookies,
			modelStudioConsoleBaseURL+"/data/api.json",
			time.Now(),
		)
		if err != nil {
			importErrors = append(importErrors, profile.Label+": no signed-in Alibaba session")
			continue
		}
		return browserSession{Cookie: header, SourceLabel: profile.Label}, nil
	}

	if len(importErrors) == 0 {
		return browserSession{}, fmt.Errorf("%w: no authenticated Alibaba browser session found", errBrowserCookieImportUnavailable)
	}
	return browserSession{}, fmt.Errorf("%w: %s", errBrowserCookieImportUnavailable, strings.Join(importErrors, "; "))
}

func chromiumCookieProfiles() []chromiumCookieProfile {
	home, err := browserCookieHomeDir()
	if err != nil || home == "" {
		return nil
	}

	applicationSupport := filepath.Join(home, "Library", "Application Support")
	profiles := []chromiumCookieProfile{
		{
			Label:           "Codex in-app browser",
			CookieDB:        filepath.Join(applicationSupport, "Codex", "Default", "Partitions", "codex-browser-app", "Cookies"),
			KeychainService: "Chromium Safe Storage",
			KeychainAccount: "Chromium",
		},
	}
	profiles = append(profiles, discoverChromiumProfiles(
		filepath.Join(applicationSupport, "Google", "Chrome"),
		"Chrome",
		"Chrome Safe Storage",
		"Chrome",
	)...)
	profiles = append(profiles, discoverChromiumProfiles(
		filepath.Join(applicationSupport, "Chromium"),
		"Chromium",
		"Chromium Safe Storage",
		"Chromium",
	)...)

	seen := make(map[string]bool)
	available := make([]chromiumCookieProfile, 0, len(profiles))
	for _, profile := range profiles {
		if seen[profile.CookieDB] {
			continue
		}
		info, err := os.Stat(profile.CookieDB)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		seen[profile.CookieDB] = true
		available = append(available, profile)
	}
	return available
}

func discoverChromiumProfiles(root, browser, service, account string) []chromiumCookieProfile {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var profiles []chromiumCookieProfile
	for _, entry := range entries {
		if !entry.IsDir() || (entry.Name() != "Default" && !strings.HasPrefix(entry.Name(), "Profile ")) {
			continue
		}
		for _, relative := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
			path := filepath.Join(root, entry.Name(), relative)
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				profiles = append(profiles, chromiumCookieProfile{
					Label:           browser + " " + entry.Name(),
					CookieDB:        path,
					KeychainService: service,
					KeychainAccount: account,
				})
				break
			}
		}
	}
	return profiles
}

func readChromiumCookies(ctx context.Context, cookieDB, safeStoragePassword string) ([]browserCookie, error) {
	tempDir, err := os.MkdirTemp("", "vibeusage-modelstudio-cookies-*")
	if err != nil {
		return nil, fmt.Errorf("create private cookie snapshot: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure cookie snapshot directory: %w", err)
	}

	snapshot := filepath.Join(tempDir, "Cookies")
	if err := copyCookieDatabaseFile(cookieDB, snapshot); err != nil {
		return nil, fmt.Errorf("copy cookie database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(cookieDB + suffix); err == nil {
			if err := copyCookieDatabaseFile(cookieDB+suffix, snapshot+suffix); err != nil {
				return nil, fmt.Errorf("copy cookie database sidecar: %w", err)
			}
		}
	}

	query := `SELECT host_key AS host_key, path AS path, name AS name,
hex(CAST(value AS BLOB)) AS value_hex, hex(encrypted_value) AS encrypted_hex,
CAST(expires_utc AS INTEGER) AS expires_utc, CAST(is_secure AS INTEGER) AS is_secure,
CAST(last_access_utc AS INTEGER) AS last_access_utc,
COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'version'), 0) AS db_version
FROM cookies WHERE lower(host_key) LIKE '%alibabacloud.com'`
	cmd := exec.CommandContext(ctx, chromiumCookieSQLitePath, "-readonly", "-json", snapshot, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, errors.New("query cookie database")
	}

	var rows []chromiumCookieRow
	if len(output) != 0 {
		if err := json.Unmarshal(output, &rows); err != nil {
			return nil, fmt.Errorf("decode cookie database rows: %w", err)
		}
	}

	cookies := make([]browserCookie, 0, len(rows))
	for _, row := range rows {
		if !cookieDomainMatches(row.HostKey, "modelstudio.console.alibabacloud.com") {
			continue
		}
		value, err := decodeChromiumCookieValue(row, safeStoragePassword)
		if err != nil || value == "" || !validCookiePair(row.Name, value) {
			continue
		}
		cookies = append(cookies, browserCookie{
			Name:       row.Name,
			Value:      value,
			Domain:     row.HostKey,
			Path:       row.Path,
			Secure:     row.IsSecure != 0,
			ExpiresAt:  chromiumTime(row.ExpiresUTC),
			LastAccess: chromiumTime(row.LastAccess),
		})
	}
	return cookies, nil
}

func copyCookieDatabaseFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func decodeChromiumCookieValue(row chromiumCookieRow, safeStoragePassword string) (string, error) {
	if row.ValueHex != "" {
		plaintext, err := hex.DecodeString(row.ValueHex)
		if err != nil {
			return "", err
		}
		return string(plaintext), nil
	}

	encrypted, err := hex.DecodeString(row.EncryptedHex)
	if err != nil {
		return "", err
	}
	plaintext, err := decryptChromiumCookie(encrypted, row.HostKey, safeStoragePassword, row.DBVersion)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func decryptChromiumCookie(encrypted []byte, hostKey, safeStoragePassword string, dbVersion int) ([]byte, error) {
	if len(encrypted) < 3 || (string(encrypted[:3]) != "v10" && string(encrypted[:3]) != "v11") {
		return nil, errors.New("unsupported Chromium cookie encryption")
	}
	ciphertext := encrypted[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("invalid Chromium cookie ciphertext")
	}

	key, err := pbkdf2.Key(sha1.New, safeStoragePassword, []byte("saltysalt"), 1003, 16)
	if err != nil {
		return nil, fmt.Errorf("derive Chromium cookie key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize Chromium cookie cipher: %w", err)
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, []byte("                ")).CryptBlocks(plaintext, ciphertext)
	plaintext, err = unpadPKCS7(plaintext, aes.BlockSize)
	if err != nil {
		return nil, err
	}

	if dbVersion >= 24 {
		if len(plaintext) < sha256.Size {
			return nil, errors.New("chromium cookie host digest missing")
		}
		digest := sha256.Sum256([]byte(hostKey))
		if subtle.ConstantTimeCompare(plaintext[:sha256.Size], digest[:]) != 1 {
			return nil, errors.New("chromium cookie host digest mismatch")
		}
		plaintext = plaintext[sha256.Size:]
	}
	if !utf8.Valid(plaintext) {
		return nil, errors.New("chromium cookie value is not UTF-8")
	}
	return plaintext, nil
}

func unpadPKCS7(value []byte, blockSize int) ([]byte, error) {
	if len(value) == 0 || len(value)%blockSize != 0 {
		return nil, errors.New("invalid Chromium cookie padding")
	}
	padding := int(value[len(value)-1])
	if padding == 0 || padding > blockSize || padding > len(value) {
		return nil, errors.New("invalid Chromium cookie padding")
	}
	for _, b := range value[len(value)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid Chromium cookie padding")
		}
	}
	return value[:len(value)-padding], nil
}

func chromiumTime(microseconds int64) time.Time {
	if microseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(microseconds - chromiumEpochOffsetMicroseconds).UTC()
}

func validCookiePair(name, value string) bool {
	return (&http.Cookie{Name: name, Value: value}).Valid() == nil
}

func redactBrowserImportError(err error) string {
	if err == nil {
		return "cookie import failed"
	}
	message := err.Error()
	for _, safe := range []string{
		"create private cookie snapshot",
		"secure cookie snapshot directory",
		"copy cookie database",
		"copy cookie database sidecar",
		"query cookie database",
		"decode cookie database rows",
	} {
		if strings.Contains(message, safe) {
			return safe
		}
	}
	return "cookie import failed"
}
