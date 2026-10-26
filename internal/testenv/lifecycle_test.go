package testenv_test

import (
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func invokeApplyVibeusage(t *testing.T, base string) testenv.Dirs {
	t.Helper()
	fn := reflect.ValueOf(testenv.ApplyVibeusage)
	arg0 := reflect.ValueOf(t)
	if fn.Type().In(0).Kind() == reflect.Func {
		arg0 = reflect.ValueOf(t.Setenv)
	}
	out := fn.Call([]reflect.Value{arg0, reflect.ValueOf(base)})
	return out[0].Interface().(testenv.Dirs)
}

func invokeApplySameDir(t *testing.T, dir string) {
	t.Helper()
	fn := reflect.ValueOf(testenv.ApplySameDir)
	arg0 := reflect.ValueOf(t)
	if fn.Type().In(0).Kind() == reflect.Func {
		arg0 = reflect.ValueOf(t.Setenv)
	}
	fn.Call([]reflect.Value{arg0, reflect.ValueOf(dir)})
}

func invokeSetClosedCDPPort(t *testing.T) string {
	t.Helper()
	fn := reflect.ValueOf(testenv.SetClosedCDPPort)
	arg0 := reflect.ValueOf(t)
	if fn.Type().In(0).Kind() == reflect.Func {
		arg0 = reflect.ValueOf(t.Setenv)
	}
	out := fn.Call([]reflect.Value{arg0})
	return out[0].String()
}

func TestReserveClosedPort_AndDirsClose(t *testing.T) {
	var port string
	t.Run("reserve", func(t *testing.T) {
		port = testenv.ReserveClosedPort(t)
		if port == "" {
			t.Fatal("ReserveClosedPort returned empty port")
		}
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("expected ReserveClosedPort(%s) to refuse connections", port)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err == nil {
			_ = ln.Close()
			t.Fatalf("expected ReserveClosedPort(%s) to prevent rebinding while active", port)
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("ReserveClosedPort leaked bound socket %s after subtest cleanup: %v", port, err)
	}
	_ = ln.Close()

	t.Run("explicit-close", func(t *testing.T) {
		dirs := invokeApplyVibeusage(t, t.TempDir())
		if err := dirs.Close(); err != nil {
			t.Fatalf("dirs.Close() error: %v", err)
		}
		if err := dirs.Close(); err != nil {
			t.Fatalf("idempotent dirs.Close() error: %v", err)
		}
	})

	testenv.SetAllowKeychainValue(t.Setenv, " False ")
	if got := os.Getenv("VIBEUSAGE_ALLOW_KEYCHAIN"); got != " False " {
		t.Fatalf("VIBEUSAGE_ALLOW_KEYCHAIN = %q, want %q", got, " False ")
	}
}

func TestApplyCleanup_ReleasesSocketOnSubtestExit(t *testing.T) {
	t.Run("ApplyVibeusage", func(t *testing.T) {
		var base, port string
		t.Run("fixture", func(t *testing.T) {
			base = t.TempDir()
			dirs := invokeApplyVibeusage(t, base)
			port = dirs.CDPPort
			if ln, err := net.Listen("tcp", "127.0.0.1:"+port); err == nil {
				_ = ln.Close()
				t.Fatalf("port %s should be bound during fixture lifetime", port)
			}
		})
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatalf("expected temp dir %s to be removed after subtest exit, got err=%v", base, err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatalf("fixture cleanup removed temp dir %s but leaked bound socket %s: %v", base, port, err)
		}
		_ = ln.Close()
	})

	t.Run("ApplySameDir", func(t *testing.T) {
		var base, port string
		t.Run("fixture", func(t *testing.T) {
			base = t.TempDir()
			invokeApplySameDir(t, base)
			port = os.Getenv("VIBEUSAGE_CDP_PORTS")
			if ln, err := net.Listen("tcp", "127.0.0.1:"+port); err == nil {
				_ = ln.Close()
				t.Fatalf("port %s should be bound during fixture lifetime", port)
			}
		})
		if _, err := os.Stat(base); !os.IsNotExist(err) {
			t.Fatalf("expected temp dir %s to be removed after subtest exit, got err=%v", base, err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatalf("ApplySameDir cleanup removed temp dir %s but leaked bound socket %s: %v", base, port, err)
		}
		_ = ln.Close()
	})
}

func TestClosedSetterCleanup_ReleasesSocketOnSubtestExit(t *testing.T) {
	var port string
	t.Run("fixture", func(t *testing.T) {
		port = invokeSetClosedCDPPort(t)
		if ln, err := net.Listen("tcp", "127.0.0.1:"+port); err == nil {
			_ = ln.Close()
			t.Fatalf("port %s should be bound during fixture lifetime", port)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("SetClosedCDPPort leaked bound socket %s after subtest cleanup: %v", port, err)
	}
	_ = ln.Close()
}
