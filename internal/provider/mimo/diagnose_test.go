package mimo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/provider"
	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

func findDiag(t *testing.T, diags []provider.Diagnostic, name string) provider.Diagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no diagnostic named %q in %+v", name, diags)
	return provider.Diagnostic{}
}

func TestDiagnoseStoredSessionStates(t *testing.T) {
	t.Run("missing session fails", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		d := diagnoseMimoStoredSession()
		if d.Status != provider.DiagFail {
			t.Fatalf("want fail, got %q (%s)", d.Status, d.Detail)
		}
	})

	t.Run("signed-in session is ok and lists names", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		creds := sessionCredentials{
			Cookie:    "api-platform_serviceToken=secret-token; userId=secret-id; cna=other",
			Source:    sessionSourceBrowser,
			UpdatedAt: time.Now().Add(-30 * time.Minute),
		}
		data, err := json.Marshal(creds)
		if err != nil {
			t.Fatal(err)
		}
		if err := config.WriteCredential("mimo", "session", data); err != nil {
			t.Fatal(err)
		}

		d := diagnoseMimoStoredSession()
		if d.Status != provider.DiagOK {
			t.Fatalf("want ok, got %q (%s)", d.Status, d.Detail)
		}
		for _, want := range []string{"browser session", "api-platform_serviceToken", "userId", "30m0s ago"} {
			if !strings.Contains(d.Detail, want) {
				t.Errorf("detail %q missing %q", d.Detail, want)
			}
		}
		if strings.Contains(d.Detail, "secret-token") || strings.Contains(d.Detail, "secret-id") {
			t.Errorf("detail leaks cookie values: %q", d.Detail)
		}
	})

	t.Run("session without service token warns", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		data, err := json.Marshal(sessionCredentials{
			Cookie: "cna=other",
			Source: sessionSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := config.WriteCredential("mimo", "session", data); err != nil {
			t.Fatal(err)
		}

		d := diagnoseMimoStoredSession()
		if d.Status != provider.DiagWarn {
			t.Fatalf("want warn, got %q (%s)", d.Status, d.Detail)
		}
		if !strings.Contains(d.Detail, "missing api-platform_serviceToken") {
			t.Errorf("detail %q missing missing-cookie hint", d.Detail)
		}
	})
}

func TestDiagnoseCDPJars(t *testing.T) {
	t.Run("unreachable port fails", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		t.Setenv(cdpPortsEnv, "9499")
		diags := diagnoseMimoCDPJars(context.Background())
		if len(diags) != 1 {
			t.Fatalf("want 1 diagnostic, got %d: %+v", len(diags), diags)
		}
		if diags[0].Status != provider.DiagFail || !strings.Contains(diags[0].Detail, "unreachable") {
			t.Fatalf("want unreachable fail, got %+v", diags[0])
		}
	})

	t.Run("reachable jar lists names without values", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		future := float64(time.Now().Add(time.Hour).Unix())
		port := newFakeCDPServer(t, "Chrome/154.0", "Chrome/154.0", []cdpCookie{
			{Name: "api-platform_serviceToken", Value: "jar-secret-token", Domain: ".xiaomimimo.com", Expires: future},
			{Name: "userId", Value: "jar-secret-id", Domain: ".xiaomi.com", Expires: future},
			{Name: "unrelated", Value: "jar-unrelated", Domain: "example.com", Expires: future},
		})
		t.Setenv(cdpPortsEnv, port)

		diags := diagnoseMimoCDPJars(context.Background())
		if len(diags) != 1 {
			t.Fatalf("want 1 diagnostic, got %d: %+v", len(diags), diags)
		}
		d := diags[0]
		if d.Status != provider.DiagOK {
			t.Fatalf("want ok, got %q (%s)", d.Status, d.Detail)
		}
		for _, want := range []string{"3 cookies", "2 on Xiaomi domains", "api-platform_serviceToken", "signed-in session visible"} {
			if !strings.Contains(d.Detail, want) {
				t.Errorf("detail %q missing %q", d.Detail, want)
			}
		}
		for _, secret := range []string{"jar-secret-token", "jar-secret-id", "jar-unrelated"} {
			if strings.Contains(d.Detail, secret) {
				t.Errorf("detail leaks cookie value %q: %q", secret, d.Detail)
			}
		}
	})

	t.Run("browser-level rejection falls back to page target", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		port := newFakeCDPServerPageFallback(t, []cdpCookie{
			{Name: "api-platform_serviceToken", Value: "s", Domain: ".xiaomimimo.com"},
		})
		t.Setenv(cdpPortsEnv, port)

		diags := diagnoseMimoCDPJars(context.Background())
		if len(diags) != 1 {
			t.Fatalf("want 1 diagnostic, got %d: %+v", len(diags), diags)
		}
		if diags[0].Status != provider.DiagOK {
			t.Fatalf("want ok, got %q (%s)", diags[0].Status, diags[0].Detail)
		}
		if !strings.Contains(diags[0].Detail, "page target") {
			t.Errorf("detail %q missing page-target note", diags[0].Detail)
		}
	})

	t.Run("no ports probed warns", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		t.Setenv(cdpPortsEnv, "bogus")
		diags := diagnoseMimoCDPJars(context.Background())
		if len(diags) != 1 || diags[0].Status != provider.DiagWarn {
			t.Fatalf("want single warn, got %+v", diags)
		}
	})
}

func TestDiagnoseThrottleAndCooldown(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())

	d := diagnoseMimoThrottle()
	if d.Status != provider.DiagInfo || !strings.Contains(d.Detail, "no active throttle") {
		t.Fatalf("want no-throttle info, got %+v", d)
	}

	if err := (config.FileThrottleStore{}).Save("mimo", fetch.ThrottleMarker{
		RetryAt: time.Now().Add(10 * time.Minute),
		Reason:  "429",
	}); err != nil {
		t.Fatal(err)
	}
	d = diagnoseMimoThrottle()
	if d.Status != provider.DiagWarn || !strings.Contains(d.Detail, "throttled until") {
		t.Fatalf("want active-throttle warn, got %+v", d)
	}

	t.Run("cooldown reports remaining window", func(t *testing.T) {
		path := filepath.Join(config.ThrottlesDir(), "mimo-login.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		attempted := time.Now().Add(-1 * time.Minute)
		data, err := json.Marshal(struct {
			AttemptedAt time.Time `json:"attempted_at"`
		}{AttemptedAt: attempted})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		d := diagnoseMimoLoginCooldown()
		if d.Status != provider.DiagWarn {
			t.Fatalf("want cooling-down warn, got %+v", d)
		}
		if !strings.Contains(d.Detail, "cooling down") {
			t.Errorf("detail %q missing cooldown note", d.Detail)
		}
	})

	t.Run("expired cooldown is informational", func(t *testing.T) {
		path := filepath.Join(config.ThrottlesDir(), "mimo-login.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		attempted := time.Now().Add(-2 * mimoSessionCooldown)
		data, err := json.Marshal(struct {
			AttemptedAt time.Time `json:"attempted_at"`
		}{AttemptedAt: attempted})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		d := diagnoseMimoLoginCooldown()
		if d.Status != provider.DiagInfo {
			t.Fatalf("want info, got %+v", d)
		}
	})
}

func TestDiagnoseFullReport(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv(cdpPortsEnv, "9499")

	diags := (Mimo{}).DiagnoseAuth(context.Background())
	for _, want := range []string{
		"Environment cookie", "Stored session", "CDP :9499",
		"Browser profiles", "Throttle", "Auto-login cooldown",
	} {
		findDiag(t, diags, want)
	}
}

func TestMimoCookieHeaderNames(t *testing.T) {
	names := mimoCookieHeaderNames("a=1; b=2 ;;  c=3; novalue")
	if got := fmt.Sprint(names); got != "[a b c]" {
		t.Fatalf("unexpected names: %s", got)
	}
}
