package modelstudio

import (
	"strings"
	"testing"
	"time"
)

func TestCookieDomainMatches(t *testing.T) {
	tests := []struct {
		domain string
		host   string
		want   bool
	}{
		{domain: ".alibabacloud.com", host: "modelstudio.console.alibabacloud.com", want: true},
		{domain: "modelstudio.console.alibabacloud.com", host: "modelstudio.console.alibabacloud.com", want: true},
		{domain: "console.alibabacloud.com", host: "modelstudio.console.alibabacloud.com", want: false},
		{domain: ".evilalibabacloud.com", host: "modelstudio.console.alibabacloud.com", want: false},
		{domain: ".alibabacloud.com", host: "notalibabacloud.com", want: false},
	}
	for _, tt := range tests {
		if got := cookieDomainMatches(tt.domain, tt.host); got != tt.want {
			t.Errorf("cookieDomainMatches(%q, %q) = %v, want %v", tt.domain, tt.host, got, tt.want)
		}
	}
}

func TestCookiePathMatches(t *testing.T) {
	tests := []struct {
		cookiePath  string
		requestPath string
		want        bool
	}{
		{cookiePath: "/", requestPath: "/data/api.json", want: true},
		{cookiePath: "/data", requestPath: "/data/api.json", want: true},
		{cookiePath: "/data/", requestPath: "/data/api.json", want: true},
		{cookiePath: "/data", requestPath: "/database", want: false},
		{cookiePath: "/data/api.json", requestPath: "/data/api.json", want: true},
	}
	for _, tt := range tests {
		if got := cookiePathMatches(tt.cookiePath, tt.requestPath); got != tt.want {
			t.Errorf("cookiePathMatches(%q, %q) = %v, want %v", tt.cookiePath, tt.requestPath, got, tt.want)
		}
	}
}

func TestBuildModelStudioCookieHeader(t *testing.T) {
	now := time.Now().UTC()
	cookies := []browserCookie{
		{Name: "login_aliyunid_ticket", Value: "ticket", Domain: ".alibabacloud.com", Path: "/", Secure: true},
		{Name: "login_current_pk", Value: "account", Domain: ".alibabacloud.com", Path: "/", Secure: true},
		{Name: "login_aliyunid_csrf", Value: "broad", Domain: ".alibabacloud.com", Path: "/", Secure: true},
		{Name: "login_aliyunid_csrf", Value: "specific", Domain: "modelstudio.console.alibabacloud.com", Path: "/data", Secure: true},
		{Name: "expired", Value: "skip", Domain: ".alibabacloud.com", Path: "/", ExpiresAt: now.Add(-time.Minute)},
		{Name: "wrong_path", Value: "skip", Domain: ".alibabacloud.com", Path: "/other"},
		{Name: "wrong_domain", Value: "skip", Domain: ".evilalibabacloud.com", Path: "/"},
	}

	header, err := buildModelStudioCookieHeader(
		cookies,
		"https://modelstudio.console.alibabacloud.com/data/api.json",
		now,
	)
	if err != nil {
		t.Fatalf("buildModelStudioCookieHeader() error: %v", err)
	}
	for _, want := range []string{
		"login_aliyunid_ticket=ticket",
		"login_current_pk=account",
		"login_aliyunid_csrf=specific",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q", want)
		}
	}
	for _, unwanted := range []string{"broad", "expired", "wrong_path", "wrong_domain", "skip"} {
		if strings.Contains(header, unwanted) {
			t.Errorf("header unexpectedly contains %q", unwanted)
		}
	}
}

func TestBuildModelStudioCookieHeaderRequiresAuthenticatedPair(t *testing.T) {
	_, err := buildModelStudioCookieHeader(
		[]browserCookie{{
			Name: "login_aliyunid_ticket", Value: "ticket", Domain: ".alibabacloud.com", Path: "/",
		}},
		"https://modelstudio.console.alibabacloud.com/data/api.json",
		time.Now(),
	)
	if err == nil {
		t.Fatal("expected missing auth-cookie-pair error")
	}
}
