// Package testenv isolates Vibeusage directories (VIBEUSAGE_CONFIG_DIR,
// VIBEUSAGE_DATA_DIR, VIBEUSAGE_CACHE_DIR) and pins all three browser and
// credential discovery inputs so tests cannot reach the operator's real
// environment:
//  1. CDP ports (VIBEUSAGE_CDP_PORTS set to an exclusively bound, non-listening
//     loopback TCP socket retained for the test lifetime so connections are
//     refused and no other listener can bind the port)
//  2. native Chromium profile discovery (HOME and USERPROFILE set to a fresh
//     temp directory, and CLI profile overrides CLAUDE_CONFIG_DIR and
//     CODEX_HOME cleared)
//  3. macOS Keychain (VIBEUSAGE_ALLOW_KEYCHAIN set to "0", keeping the keychain
//     gate off)
package testenv

import (
	"os"
	"path/filepath"
	"strings"
)

// TB is the subset of testing.TB needed to set environment variables and
// register cleanup for reserved closed loopback sockets without importing
// testing in non-test code.
type TB interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
	Setenv(key, value string)
}

// Dirs contains isolated directories and discovery settings for vibeusage in tests.
type Dirs struct {
	Base    string
	Config  string
	Data    string
	Cache   string
	Home    string
	CDPPort string
	closeFn func() error
}

// Close releases any held resources (such as the bound closed loopback socket)
// associated with this isolated test environment.
func (d Dirs) Close() error {
	if d.closeFn != nil {
		return d.closeFn()
	}
	return nil
}

func reserveClosedLoopback(tb TB) (string, func() error) {
	tb.Helper()
	port, closeFn, err := bindClosedLoopback()
	if err != nil {
		tb.Fatalf("testenv: reserve closed loopback port: %v", err)
	}
	tb.Cleanup(func() {
		_ = closeFn()
	})
	return port, closeFn
}

// VibeusageDirs returns conventional test directories rooted at base.
func VibeusageDirs(base string) Dirs {
	return Dirs{
		Base:   base,
		Config: filepath.Join(base, "config"),
		Data:   filepath.Join(base, "data"),
		Cache:  filepath.Join(base, "cache"),
		Home:   filepath.Join(base, "home"),
	}
}

// ReserveClosedPort binds an exclusive non-listening loopback TCP socket for
// the lifetime of tb and returns its port string.
func ReserveClosedPort(tb TB) string {
	tb.Helper()
	port, _ := reserveClosedLoopback(tb)
	return port
}

// ApplyVibeusage sets VIBEUSAGE_* env vars to isolated test directories and
// pins browser/credential discovery inputs (CDP ports, native Chromium profile
// discovery via HOME/USERPROFILE, CLI profile overrides CLAUDE_CONFIG_DIR and
// CODEX_HOME, and macOS Keychain) for the lifetime of tb.
func ApplyVibeusage(tb TB, base string) Dirs {
	tb.Helper()
	d := VibeusageDirs(base)
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		tb.Fatalf("testenv: create isolated home dir: %v", err)
	}
	d.CDPPort, d.closeFn = reserveClosedLoopback(tb)
	tb.Setenv("VIBEUSAGE_CONFIG_DIR", d.Config)
	tb.Setenv("VIBEUSAGE_DATA_DIR", d.Data)
	tb.Setenv("VIBEUSAGE_CACHE_DIR", d.Cache)
	tb.Setenv("HOME", d.Home)
	tb.Setenv("USERPROFILE", d.Home)
	tb.Setenv("CLAUDE_CONFIG_DIR", "")
	tb.Setenv("CODEX_HOME", "")
	tb.Setenv("VIBEUSAGE_CDP_PORTS", d.CDPPort)
	tb.Setenv("VIBEUSAGE_ALLOW_KEYCHAIN", "0")
	return d
}

// ApplySameDir points config/data/cache and HOME/USERPROFILE to the same
// directory, clears inherited CLI profile overrides, and pins CDP ports and
// macOS Keychain discovery for the lifetime of tb.
// Useful in tests that expect ConfigDir() to exactly match a temp dir path.
func ApplySameDir(tb TB, dir string) {
	tb.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatalf("testenv: create isolated dir: %v", err)
	}
	port, _ := reserveClosedLoopback(tb)
	tb.Setenv("VIBEUSAGE_CONFIG_DIR", dir)
	tb.Setenv("VIBEUSAGE_DATA_DIR", dir)
	tb.Setenv("VIBEUSAGE_CACHE_DIR", dir)
	tb.Setenv("HOME", dir)
	tb.Setenv("USERPROFILE", dir)
	tb.Setenv("CLAUDE_CONFIG_DIR", "")
	tb.Setenv("CODEX_HOME", "")
	tb.Setenv("VIBEUSAGE_CDP_PORTS", port)
	tb.Setenv("VIBEUSAGE_ALLOW_KEYCHAIN", "0")
}

// SetConfigDir sets VIBEUSAGE_CONFIG_DIR in tests.
func SetConfigDir(setenv func(string, string), dir string) {
	setenv("VIBEUSAGE_CONFIG_DIR", dir)
}

// SetDataDir sets VIBEUSAGE_DATA_DIR in tests.
func SetDataDir(setenv func(string, string), dir string) {
	setenv("VIBEUSAGE_DATA_DIR", dir)
}

// SetCacheDir sets VIBEUSAGE_CACHE_DIR in tests.
func SetCacheDir(setenv func(string, string), dir string) {
	setenv("VIBEUSAGE_CACHE_DIR", dir)
}

// EnsureCacheDir sets VIBEUSAGE_CACHE_DIR to fallbackDir() when it is not
// already set in the environment.
func EnsureCacheDir(setenv func(string, string), fallbackDir func() string) {
	if os.Getenv("VIBEUSAGE_CACHE_DIR") == "" {
		setenv("VIBEUSAGE_CACHE_DIR", fallbackDir())
	}
}

// SetCDPPorts sets VIBEUSAGE_CDP_PORTS to the comma-joined port list in tests.
func SetCDPPorts(setenv func(string, string), ports ...string) {
	setenv("VIBEUSAGE_CDP_PORTS", strings.Join(ports, ","))
}

// SetClosedCDPPort allocates a closed loopback port owned by tb, sets
// VIBEUSAGE_CDP_PORTS to it, and returns the port string.
func SetClosedCDPPort(tb TB) string {
	tb.Helper()
	port := ReserveClosedPort(tb)
	tb.Setenv("VIBEUSAGE_CDP_PORTS", port)
	return port
}

// SetAllowKeychain sets VIBEUSAGE_ALLOW_KEYCHAIN to "1" when allow is true and
// "0" when allow is false.
func SetAllowKeychain(setenv func(string, string), allow bool) {
	if allow {
		setenv("VIBEUSAGE_ALLOW_KEYCHAIN", "1")
		return
	}
	setenv("VIBEUSAGE_ALLOW_KEYCHAIN", "0")
}

// SetAllowKeychainValue sets VIBEUSAGE_ALLOW_KEYCHAIN to the exact raw value in tests.
func SetAllowKeychainValue(setenv func(string, string), raw string) {
	setenv("VIBEUSAGE_ALLOW_KEYCHAIN", raw)
}
