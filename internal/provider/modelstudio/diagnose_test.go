package modelstudio

import (
	"context"
	"encoding/json"
	"fmt"
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
		d := diagnoseModelStudioStoredSession()
		if d.Status != provider.DiagFail {
			t.Fatalf("want fail, got %q (%s)", d.Status, d.Detail)
		}
	})

	t.Run("signed-in session with updated_at is ok and lists names", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		creds := sessionCredentials{
			Cookie:    "login_aliyunid_ticket=secret-ticket; login_current_pk=secret-pk; cna=other",
			Source:    sessionSourceBrowser,
			UpdatedAt: time.Now().Add(-2 * time.Hour),
		}
		data, err := json.Marshal(creds)
		if err != nil {
			t.Fatal(err)
		}
		if err := config.WriteCredential("modelstudio", "session", data); err != nil {
			t.Fatal(err)
		}

		d := diagnoseModelStudioStoredSession()
		if d.Status != provider.DiagOK {
			t.Fatalf("want ok, got %q (%s)", d.Status, d.Detail)
		}
		for _, want := range []string{"browser session", "login_aliyunid_ticket", "login_current_pk", "updated 2h0m0s ago"} {
			if !strings.Contains(d.Detail, want) {
				t.Errorf("detail %q missing %q", d.Detail, want)
			}
		}
		if strings.Contains(d.Detail, "secret-ticket") || strings.Contains(d.Detail, "secret-pk") {
			t.Errorf("detail leaks cookie values: %q", d.Detail)
		}
	})

	t.Run("session without auth cookies warns", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		data, err := json.Marshal(sessionCredentials{
			Cookie: "cna=other; tfstk=other2",
			Source: sessionSourceBrowser,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := config.WriteCredential("modelstudio", "session", data); err != nil {
			t.Fatal(err)
		}

		d := diagnoseModelStudioStoredSession()
		if d.Status != provider.DiagWarn {
			t.Fatalf("want warn, got %q (%s)", d.Status, d.Detail)
		}
		if !strings.Contains(d.Detail, "missing login_aliyunid_ticket") {
			t.Errorf("detail %q missing missing-cookie hint", d.Detail)
		}
	})

	t.Run("pre-upgrade session reports unknown age", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		data, err := json.Marshal(sessionCredentials{
			Cookie: "login_aliyunid_ticket=t; login_current_pk=p",
			Source: sessionSourceBrowser,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := config.WriteCredential("modelstudio", "session", data); err != nil {
			t.Fatal(err)
		}

		d := diagnoseModelStudioStoredSession()
		if d.Status != provider.DiagOK {
			t.Fatalf("want ok, got %q (%s)", d.Status, d.Detail)
		}
		if !strings.Contains(d.Detail, "age unknown") {
			t.Errorf("detail %q missing age-unknown note", d.Detail)
		}
	})
}

func TestDiagnoseCDPJars(t *testing.T) {
	t.Run("unreachable port fails", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		t.Setenv(cdpPortsEnv, "9499")
		diags := diagnoseModelStudioCDPJars(context.Background())
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
			{Name: "login_aliyunid_ticket", Value: "jar-secret-ticket", Domain: ".alibabacloud.com", Expires: future},
			{Name: "login_current_pk", Value: "jar-secret-pk", Domain: ".alibabacloud.com", Expires: future},
			{Name: "cna", Value: "jar-other", Domain: ".alibaba.com", Expires: future},
			{Name: "unrelated", Value: "jar-unrelated", Domain: "example.com", Expires: future},
		})
		t.Setenv(cdpPortsEnv, port)

		diags := diagnoseModelStudioCDPJars(context.Background())
		if len(diags) != 1 {
			t.Fatalf("want 1 diagnostic, got %d: %+v", len(diags), diags)
		}
		d := diags[0]
		if d.Status != provider.DiagOK {
			t.Fatalf("want ok, got %q (%s)", d.Status, d.Detail)
		}
		for _, want := range []string{"4 cookies", "3 on Alibaba domains", "login_aliyunid_ticket", "signed-in session visible"} {
			if !strings.Contains(d.Detail, want) {
				t.Errorf("detail %q missing %q", d.Detail, want)
			}
		}
		for _, secret := range []string{"jar-secret-ticket", "jar-secret-pk", "jar-other", "jar-unrelated"} {
			if strings.Contains(d.Detail, secret) {
				t.Errorf("detail leaks cookie value %q: %q", secret, d.Detail)
			}
		}
	})

	t.Run("browser-level rejection falls back to page target", func(t *testing.T) {
		testenv.ApplyVibeusage(t.Setenv, t.TempDir())
		port := newFakeCDPServerPageFallback(t, []cdpCookie{
			{Name: "login_aliyunid_ticket", Value: "s", Domain: ".alibabacloud.com"},
		})
		t.Setenv(cdpPortsEnv, port)

		diags := diagnoseModelStudioCDPJars(context.Background())
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
		// An explicitly configured port list that normalizes to nothing must
		// produce the no-ports warning, independent of any live CDP fleet.
		t.Setenv(cdpPortsEnv, "bogus")
		diags := diagnoseModelStudioCDPJars(context.Background())
		if len(diags) != 1 || diags[0].Status != provider.DiagWarn {
			t.Fatalf("want single warn, got %+v", diags)
		}
	})
}

func TestDiagnoseThrottle(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())

	d := diagnoseModelStudioThrottle()
	if d.Status != provider.DiagInfo || !strings.Contains(d.Detail, "no active throttle") {
		t.Fatalf("want no-throttle info, got %+v", d)
	}

	if err := (config.FileThrottleStore{}).Save("modelstudio", fetch.ThrottleMarker{
		RetryAt: time.Now().Add(20 * time.Minute),
		Reason:  "429 too many requests",
	}); err != nil {
		t.Fatal(err)
	}
	d = diagnoseModelStudioThrottle()
	if d.Status != provider.DiagWarn || !strings.Contains(d.Detail, "429 too many requests") {
		t.Fatalf("want active-throttle warn, got %+v", d)
	}
}

func TestDiagnoseFullReport(t *testing.T) {
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv(cdpPortsEnv, "9499")

	diags := (ModelStudio{}).DiagnoseAuth(context.Background())
	for _, want := range []string{
		"Environment cookie", "Stored session", "CDP :9499",
		"Browser profiles", "Throttle",
	} {
		findDiag(t, diags, want)
	}
}

func TestCookieHeaderNames(t *testing.T) {
	names := cookieHeaderNames("a=1; b=2 ;;  c=3; novalue")
	if got := fmt.Sprint(names); got != "[a b c]" {
		t.Fatalf("unexpected names: %s", got)
	}
}
