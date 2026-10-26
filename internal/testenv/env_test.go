package testenv_test

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func TestApplyVibeusage_PinsDirsAndDiscoveryInputs(t *testing.T) {
	base := t.TempDir()
	dirs := testenv.ApplyVibeusage(t, base)

	if got := os.Getenv("VIBEUSAGE_CONFIG_DIR"); got != dirs.Config {
		t.Fatalf("VIBEUSAGE_CONFIG_DIR = %q, want %q", got, dirs.Config)
	}
	if got := os.Getenv("VIBEUSAGE_DATA_DIR"); got != dirs.Data {
		t.Fatalf("VIBEUSAGE_DATA_DIR = %q, want %q", got, dirs.Data)
	}
	if got := os.Getenv("VIBEUSAGE_CACHE_DIR"); got != dirs.Cache {
		t.Fatalf("VIBEUSAGE_CACHE_DIR = %q, want %q", got, dirs.Cache)
	}
	if got := os.Getenv("HOME"); got != dirs.Home {
		t.Fatalf("HOME = %q, want %q", got, dirs.Home)
	}
	if got := os.Getenv("USERPROFILE"); got != dirs.Home {
		t.Fatalf("USERPROFILE = %q, want %q", got, dirs.Home)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error: %v", err)
	}
	if homeDir != dirs.Home {
		t.Fatalf("os.UserHomeDir() = %q, want %q", homeDir, dirs.Home)
	}

	port := os.Getenv("VIBEUSAGE_CDP_PORTS")
	if port == "" || port != dirs.CDPPort {
		t.Fatalf("VIBEUSAGE_CDP_PORTS = %q, dirs.CDPPort = %q; want non-empty match", port, dirs.CDPPort)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		t.Fatalf("VIBEUSAGE_CDP_PORTS = %q, not a valid TCP port: %v", port, err)
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected closed loopback port %s to fail dial, but connection succeeded", port)
	}

	if got := os.Getenv("VIBEUSAGE_ALLOW_KEYCHAIN"); got != "0" {
		t.Fatalf("VIBEUSAGE_ALLOW_KEYCHAIN = %q, want \"0\"", got)
	}
}

func TestApplySameDir_PinsSameDirAndDiscoveryInputs(t *testing.T) {
	dir := t.TempDir()
	testenv.ApplySameDir(t, dir)

	for _, name := range []string{"VIBEUSAGE_CONFIG_DIR", "VIBEUSAGE_DATA_DIR", "VIBEUSAGE_CACHE_DIR", "HOME", "USERPROFILE"} {
		if got := os.Getenv(name); got != dir {
			t.Fatalf("%s = %q, want %q", name, got, dir)
		}
	}

	port := os.Getenv("VIBEUSAGE_CDP_PORTS")
	if port == "" {
		t.Fatal("VIBEUSAGE_CDP_PORTS is empty")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected closed loopback port %s to fail dial, but connection succeeded", port)
	}

	if got := os.Getenv("VIBEUSAGE_ALLOW_KEYCHAIN"); got != "0" {
		t.Fatalf("VIBEUSAGE_ALLOW_KEYCHAIN = %q, want \"0\"", got)
	}
}

func TestTargetedSetters(t *testing.T) {
	testenv.SetConfigDir(t.Setenv, "/tmp/cfg")
	if got := os.Getenv("VIBEUSAGE_CONFIG_DIR"); got != "/tmp/cfg" {
		t.Fatalf("VIBEUSAGE_CONFIG_DIR = %q, want /tmp/cfg", got)
	}

	testenv.SetDataDir(t.Setenv, "/tmp/data")
	if got := os.Getenv("VIBEUSAGE_DATA_DIR"); got != "/tmp/data" {
		t.Fatalf("VIBEUSAGE_DATA_DIR = %q, want /tmp/data", got)
	}

	testenv.SetCacheDir(t.Setenv, "/tmp/cache")
	if got := os.Getenv("VIBEUSAGE_CACHE_DIR"); got != "/tmp/cache" {
		t.Fatalf("VIBEUSAGE_CACHE_DIR = %q, want /tmp/cache", got)
	}

	testenv.SetCDPPorts(t.Setenv, "12345", "23456")
	if got := os.Getenv("VIBEUSAGE_CDP_PORTS"); got != "12345,23456" {
		t.Fatalf("VIBEUSAGE_CDP_PORTS = %q, want 12345,23456", got)
	}

	closedPort := testenv.SetClosedCDPPort(t)
	if got := os.Getenv("VIBEUSAGE_CDP_PORTS"); got != closedPort || closedPort == "" {
		t.Fatalf("VIBEUSAGE_CDP_PORTS = %q, closedPort = %q", got, closedPort)
	}

	testenv.SetAllowKeychain(t.Setenv, true)
	if got := os.Getenv("VIBEUSAGE_ALLOW_KEYCHAIN"); got != "1" {
		t.Fatalf("VIBEUSAGE_ALLOW_KEYCHAIN = %q, want 1", got)
	}

	testenv.SetAllowKeychain(t.Setenv, false)
	if got := os.Getenv("VIBEUSAGE_ALLOW_KEYCHAIN"); got != "0" {
		t.Fatalf("VIBEUSAGE_ALLOW_KEYCHAIN = %q, want 0", got)
	}
}

func TestApplyVibeusage_HoldsClosedLoopbackSocketUntilClose(t *testing.T) {
	dirs := testenv.ApplyVibeusage(t, t.TempDir())
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+dirs.CDPPort, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected closed loopback port %s to fail dial, but connection succeeded", dirs.CDPPort)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+dirs.CDPPort)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("expected closed loopback port %s to remain exclusively bound during fixture lifetime, but net.Listen succeeded", dirs.CDPPort)
	}

	dir := t.TempDir()
	testenv.ApplySameDir(t, dir)
	sameDirPort := os.Getenv("VIBEUSAGE_CDP_PORTS")
	if sameDirPort == "" {
		t.Fatal("VIBEUSAGE_CDP_PORTS is empty after ApplySameDir")
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:"+sameDirPort)
	if err == nil {
		_ = ln2.Close()
		t.Fatalf("expected ApplySameDir closed loopback port %s to remain exclusively bound during fixture lifetime, but net.Listen succeeded", sameDirPort)
	}
}

func TestApplyVibeusage_ClearsInheritedProviderConfigDirs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "outside-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "outside-codex"))

	testenv.ApplyVibeusage(t, t.TempDir())
	if got := os.Getenv("CLAUDE_CONFIG_DIR"); got != "" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want empty after ApplyVibeusage", got)
	}
	if got := os.Getenv("CODEX_HOME"); got != "" {
		t.Fatalf("CODEX_HOME = %q, want empty after ApplyVibeusage", got)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "outside-claude-2"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "outside-codex-2"))

	testenv.ApplySameDir(t, t.TempDir())
	if got := os.Getenv("CLAUDE_CONFIG_DIR"); got != "" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want empty after ApplySameDir", got)
	}
	if got := os.Getenv("CODEX_HOME"); got != "" {
		t.Fatalf("CODEX_HOME = %q, want empty after ApplySameDir", got)
	}
}
