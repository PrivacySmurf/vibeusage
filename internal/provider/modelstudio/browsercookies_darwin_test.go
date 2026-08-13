//go:build darwin

package modelstudio

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDecryptChromiumCookieV24(t *testing.T) {
	const (
		host     = ".alibabacloud.com"
		password = "safe-storage-password"
		value    = "synthetic-cookie-value"
	)

	encrypted := encryptChromiumCookieForTest(t, host, password, value, 24)
	decrypted, err := decryptChromiumCookie(encrypted, host, password, 24)
	if err != nil {
		t.Fatalf("decryptChromiumCookie() error: %v", err)
	}
	if string(decrypted) != value {
		t.Errorf("decrypted = %q, want %q", decrypted, value)
	}
	if _, err := decryptChromiumCookie(encrypted, ".evilalibabacloud.com", password, 24); err == nil {
		t.Fatal("expected host-digest mismatch")
	}
}

func TestChromiumCookieProfilesIncludesCodexAndChrome(t *testing.T) {
	home := t.TempDir()
	oldHome := browserCookieHomeDir
	t.Cleanup(func() { browserCookieHomeDir = oldHome })
	browserCookieHomeDir = func() (string, error) { return home, nil }

	codexDB := filepath.Join(home, "Library", "Application Support", "Codex", "Default", "Partitions", "codex-browser-app", "Cookies")
	chromeDB := filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Profile 2", "Network", "Cookies")
	for _, path := range []string{codexDB, chromeDB} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	profiles := chromiumCookieProfiles()
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v, want Codex and Chrome", profiles)
	}
	if profiles[0].Label != "Codex in-app browser" || profiles[0].CookieDB != codexDB {
		t.Errorf("first profile = %+v, want Codex", profiles[0])
	}
	if profiles[1].Label != "Chrome Profile 2" || profiles[1].CookieDB != chromeDB {
		t.Errorf("second profile = %+v, want Chrome Profile 2", profiles[1])
	}
}

func TestReadChromiumCookiesFromPrivateSnapshot(t *testing.T) {
	const (
		host     = ".alibabacloud.com"
		password = "safe-storage-password"
	)
	database := filepath.Join(t.TempDir(), "Cookies")
	future := chromiumEpochOffsetMicroseconds + time.Now().Add(time.Hour).UnixMicro()

	rows := map[string]string{
		"login_aliyunid_ticket": "ticket-value",
		"login_current_pk":      "account-value",
	}
	statements := []string{
		`CREATE TABLE meta (key LONGVARCHAR NOT NULL UNIQUE PRIMARY KEY, value LONGVARCHAR);`,
		`INSERT INTO meta(key, value) VALUES('version', '24');`,
		`CREATE TABLE cookies (host_key TEXT, path TEXT, name TEXT, value TEXT, encrypted_value BLOB, expires_utc INTEGER, is_secure INTEGER, last_access_utc INTEGER);`,
	}
	for name, value := range rows {
		encrypted := encryptChromiumCookieForTest(t, host, password, value, 24)
		statements = append(statements,
			"INSERT INTO cookies VALUES('"+host+"','/','"+name+"','',X'"+hex.EncodeToString(encrypted)+"',"+
				formatTestInt(future)+",1,"+formatTestInt(future)+");")
	}
	cmd := exec.Command(chromiumCookieSQLitePath, database, strings.Join(statements, "\n"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create SQLite fixture: %v: %s", err, output)
	}

	cookies, err := readChromiumCookies(context.Background(), database, password)
	if err != nil {
		t.Fatalf("readChromiumCookies() error: %v", err)
	}
	header, err := buildModelStudioCookieHeader(
		cookies,
		"https://modelstudio.console.alibabacloud.com/data/api.json",
		time.Now(),
	)
	if err != nil {
		t.Fatalf("buildModelStudioCookieHeader() error: %v", err)
	}
	for name, value := range rows {
		if !strings.Contains(header, name+"="+value) {
			t.Errorf("header missing synthetic cookie %s", name)
		}
	}
}

func encryptChromiumCookieForTest(t *testing.T, host, password, value string, dbVersion int) []byte {
	t.Helper()
	plaintext := []byte(value)
	if dbVersion >= 24 {
		digest := sha256.Sum256([]byte(host))
		plaintext = append(digest[:], plaintext...)
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	plaintext = append(plaintext, bytesOf(byte(padding), padding)...)

	key, err := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), 1003, 16)
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	ciphertext := make([]byte, len(plaintext))
	cipher.NewCBCEncrypter(block, []byte("                ")).CryptBlocks(ciphertext, plaintext)
	return append([]byte("v10"), ciphertext...)
}

func bytesOf(value byte, count int) []byte {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}
	return result
}

func formatTestInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
