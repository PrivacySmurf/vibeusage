//go:build darwin

package keychain

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

// installFakeSecurity writes a fake `security` executable into a temp dir
// and prepends that dir to PATH so ReadGenericPassword runs it. The fake
// records its received arguments (one per line) to the file whose path is
// returned; body is appended to the script to control stdout and exit code.
func installFakeSecurity(t *testing.T, body string) (argsFile string) {
	t.Helper()

	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")

	script := "#!/bin/sh\nprintf '%s\n' \"$@\" > \"$KEYCHAIN_TEST_ARGS_FILE\"\n" + body
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake security: %v", err)
	}

	testenv.SetAllowKeychainValue(t.Setenv, "")
	t.Setenv("KEYCHAIN_TEST_ARGS_FILE", argsFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func readRecordedArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestReadGenericPassword_ServiceOnly(t *testing.T) {
	argsFile := installFakeSecurity(t, "printf 'secret\\n'\n")

	got, err := ReadGenericPassword("com.example.service", "")
	if err != nil {
		t.Fatalf("ReadGenericPassword: %v", err)
	}
	if got != "secret" {
		t.Errorf("got %q, want %q", got, "secret")
	}

	want := []string{"find-generic-password", "-s", "com.example.service", "-w"}
	if args := readRecordedArgs(t, argsFile); !slices.Equal(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestReadGenericPassword_ServiceAndAccount(t *testing.T) {
	argsFile := installFakeSecurity(t, "printf 'secret\\n'\n")

	got, err := ReadGenericPassword("com.example.service", "user@example.com")
	if err != nil {
		t.Fatalf("ReadGenericPassword: %v", err)
	}
	if got != "secret" {
		t.Errorf("got %q, want %q", got, "secret")
	}

	want := []string{
		"find-generic-password", "-s", "com.example.service",
		"-a", "user@example.com", "-w",
	}
	if args := readRecordedArgs(t, argsFile); !slices.Equal(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestReadGenericPassword_TrimsTrailingNewline(t *testing.T) {
	installFakeSecurity(t, "printf 'hunter2\\n\\n'\n")

	got, err := ReadGenericPassword("svc", "")
	if err != nil {
		t.Fatalf("ReadGenericPassword: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("got %q, want %q", got, "hunter2")
	}
}

func TestReadGenericPassword_NonZeroExit(t *testing.T) {
	installFakeSecurity(t, "exit 44\n")

	got, err := ReadGenericPassword("svc", "")
	if err == nil {
		t.Fatal("expected error when security exits non-zero")
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadGenericPassword_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow timeout test in -short mode")
	}
	installFakeSecurity(t, "sleep 5\n")

	got, err := ReadGenericPassword("svc", "")
	if err == nil {
		t.Fatal("expected error when security hangs past the timeout")
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestReadGenericPassword_KeychainGateDisabledVariants(t *testing.T) {
	for _, raw := range []string{"0", "false", "FALSE", " False ", "\t0\n"} {
		t.Run(strings.TrimSpace(raw)+"_"+raw, func(t *testing.T) {
			argsFile := installFakeSecurity(t, "printf 'should-not-run\\n'\n")
			testenv.SetAllowKeychainValue(t.Setenv, raw)

			got, err := ReadGenericPassword("com.example.service", "acct")
			if err == nil {
				t.Fatalf("ReadGenericPassword with raw=%q succeeded (%q), want disabled error", raw, got)
			}
			if !strings.Contains(err.Error(), "keychain access disabled") {
				t.Fatalf("ReadGenericPassword error = %q, want disabled message", err.Error())
			}
			if got != "" {
				t.Fatalf("got secret %q, want empty string", got)
			}
			if _, statErr := os.Stat(argsFile); !os.IsNotExist(statErr) {
				t.Fatalf("expected fake security not to run for raw=%q, but argsFile stat returned %v", raw, statErr)
			}
		})
	}
}

func TestReadGenericPassword_KeychainGateEnabledAndUnsetVariants(t *testing.T) {
	for _, raw := range []string{"", "1", "true"} {
		t.Run("raw_"+raw, func(t *testing.T) {
			argsFile := installFakeSecurity(t, "printf 'secret\\n'\n")
			testenv.SetAllowKeychainValue(t.Setenv, raw)

			got, err := ReadGenericPassword("com.example.service", "acct")
			if err != nil {
				t.Fatalf("ReadGenericPassword with raw=%q failed: %v", raw, err)
			}
			if got != "secret" {
				t.Fatalf("got %q, want %q", got, "secret")
			}
			want := []string{"find-generic-password", "-s", "com.example.service", "-a", "acct", "-w"}
			if args := readRecordedArgs(t, argsFile); !slices.Equal(args, want) {
				t.Fatalf("args = %v, want %v", args, want)
			}
		})
	}
}
