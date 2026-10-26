package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

type diagProviderWithoutDiagnoser struct {
	provider.Provider
}

func (diagProviderWithoutDiagnoser) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{}
}

type diagFakeProvider struct {
	provider.Provider
	diags []provider.Diagnostic
}

func (p diagFakeProvider) CredentialSources() provider.CredentialInfo {
	return provider.CredentialInfo{EnvVars: []string{"FAKE_DIAG_VAR"}}
}

func (p diagFakeProvider) DiagnoseAuth(context.Context) []provider.Diagnostic {
	return p.diags
}

func captureDiagnose(t *testing.T, fn func() error) string {
	t.Helper()
	var buf bytes.Buffer
	outWriter = &buf
	defer func() { outWriter = os.Stdout }()
	if err := fn(); err != nil {
		t.Fatalf("diagnose error: %v", err)
	}
	return buf.String()
}

func TestAuthDiagnoseRequiresProviderArg(t *testing.T) {
	testenv.ApplySameDir(t, t.TempDir())
	config.Override(t, config.DefaultConfig())

	if err := authCmd.Flags().Set("diagnose", "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authCmd.Flags().Set("diagnose", "false") }()

	err := authCmd.RunE(authCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "requires a provider argument") {
		t.Fatalf("want provider-required error, got %v", err)
	}
}

func TestAuthDiagnoseUnknownProvider(t *testing.T) {
	testenv.ApplySameDir(t, t.TempDir())
	config.Override(t, config.DefaultConfig())

	if err := authCmd.Flags().Set("diagnose", "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = authCmd.Flags().Set("diagnose", "false") }()

	err := authCmd.RunE(authCmd, []string{"no-such-provider"})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("want unknown-provider error, got %v", err)
	}
}

func TestAuthDiagnoseProviderWithoutDiagnoser(t *testing.T) {
	testenv.ApplySameDir(t, t.TempDir())
	config.Override(t, config.DefaultConfig())

	p := diagProviderWithoutDiagnoser{}

	output := captureDiagnose(t, func() error {
		return authDiagnoseProvider(context.Background(), "diag-no-diagnoser", p)
	})
	if !strings.Contains(output, "auth diagnostics") {
		t.Errorf("output missing title: %q", output)
	}
	if !strings.Contains(output, "Credential detected") {
		t.Errorf("output missing generic credential section: %q", output)
	}
	if !strings.Contains(output, "no provider-specific diagnostics available") {
		t.Errorf("output missing no-diagnostics note: %q", output)
	}
}

func TestAuthDiagnoseProviderWithDiagnoser(t *testing.T) {
	testenv.ApplySameDir(t, t.TempDir())
	config.Override(t, config.DefaultConfig())
	t.Setenv("FAKE_DIAG_VAR", "set")

	p := diagFakeProvider{diags: []provider.Diagnostic{
		{Name: "Stored session", Status: provider.DiagOK, Detail: "browser session, updated 1m0s ago"},
		{Name: "CDP :9999", Status: provider.DiagFail, Detail: "unreachable: connection refused"},
	}}

	output := captureDiagnose(t, func() error {
		return authDiagnoseProvider(context.Background(), "diag-fake", p)
	})
	for _, want := range []string{
		"diag-fake auth diagnostics",
		"FAKE_DIAG_VAR set",
		"ok",
		"Stored session",
		"browser session, updated 1m0s ago",
		"fail",
		"CDP :9999",
		"unreachable: connection refused",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunAuthDiagnose_PinsDiscoveryEnv(t *testing.T) {
	dirs := testenv.ApplyVibeusage(t, t.TempDir())
	config.Override(t, config.DefaultConfig())

	if dirs.CDPPort == "" {
		t.Fatal("dirs.CDPPort is empty")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+dirs.CDPPort, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected closed loopback port %s to fail dial", dirs.CDPPort)
	}
	if got := os.Getenv("HOME"); got != dirs.Home {
		t.Fatalf("HOME = %q, want %q", got, dirs.Home)
	}

	p := diagFakeProvider{diags: []provider.Diagnostic{
		{Name: "CDP :" + dirs.CDPPort, Status: provider.DiagFail, Detail: "unreachable: connection refused"},
	}}
	output := captureDiagnose(t, func() error {
		return authDiagnoseProvider(context.Background(), "diag-pinned", p)
	})
	if !strings.Contains(output, "CDP :"+dirs.CDPPort) {
		t.Errorf("output missing pinned CDP port %s in:\n%s", dirs.CDPPort, output)
	}
}
