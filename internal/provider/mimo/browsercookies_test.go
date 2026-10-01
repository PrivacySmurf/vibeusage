package mimo

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
		{domain: ".xiaomimimo.com", host: "platform.xiaomimimo.com", want: true},
		{domain: "platform.xiaomimimo.com", host: "platform.xiaomimimo.com", want: true},
		{domain: "console.xiaomimimo.com", host: "platform.xiaomimimo.com", want: false},
		{domain: ".evilxiaomimimo.com", host: "platform.xiaomimimo.com", want: false},
		{domain: ".xiaomimimo.com", host: "notxiaomimimo.com", want: false},
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
		{cookiePath: "/", requestPath: "/api/v1/balance", want: true},
		{cookiePath: "/api", requestPath: "/api/v1/balance", want: true},
		{cookiePath: "/api/", requestPath: "/api/v1/balance", want: true},
		{cookiePath: "/console", requestPath: "/api/v1/balance", want: false},
		{cookiePath: "/api/v1/balance", requestPath: "/api/v1/balance", want: true},
	}
	for _, tt := range tests {
		if got := cookiePathMatches(tt.cookiePath, tt.requestPath); got != tt.want {
			t.Errorf("cookiePathMatches(%q, %q) = %v, want %v", tt.cookiePath, tt.requestPath, got, tt.want)
		}
	}
}

func TestBuildMimoCookieHeader(t *testing.T) {
	now := time.Now().UTC()
	cookies := []browserCookie{
		{Name: "api-platform_serviceToken", Value: "token123", Domain: ".xiaomimimo.com", Path: "/", Secure: true},
		{Name: "userId", Value: "user456", Domain: ".xiaomimimo.com", Path: "/", Secure: true},
		{Name: "other_cookie", Value: "broad", Domain: ".xiaomimimo.com", Path: "/", Secure: true},
		{Name: "other_cookie", Value: "specific", Domain: "platform.xiaomimimo.com", Path: "/api", Secure: true},
		{Name: "expired", Value: "skip", Domain: ".xiaomimimo.com", Path: "/", ExpiresAt: now.Add(-time.Minute)},
		{Name: "wrong_path", Value: "skip", Domain: ".xiaomimimo.com", Path: "/other"},
		{Name: "wrong_domain", Value: "skip", Domain: ".evilxiaomimimo.com", Path: "/"},
	}

	header, err := buildMimoCookieHeader(
		cookies,
		"https://platform.xiaomimimo.com/api/v1/balance",
		now,
	)
	if err != nil {
		t.Fatalf("buildMimoCookieHeader() error: %v", err)
	}
	for _, want := range []string{
		"api-platform_serviceToken=token123",
		"userId=user456",
		"other_cookie=specific",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q in %q", want, header)
		}
	}
	for _, unwanted := range []string{"broad", "expired", "wrong_path", "wrong_domain", "skip"} {
		if strings.Contains(header, unwanted) {
			t.Errorf("header unexpectedly contains %q", unwanted)
		}
	}
}

func TestBuildMimoCookieHeaderRequiresAuthenticatedPair(t *testing.T) {
	// Missing userId
	_, err := buildMimoCookieHeader(
		[]browserCookie{{
			Name: "api-platform_serviceToken", Value: "token123", Domain: ".xiaomimimo.com", Path: "/",
		}},
		"https://platform.xiaomimimo.com/api/v1/balance",
		time.Now(),
	)
	if err == nil {
		t.Fatal("expected missing auth cookies error")
	}

	// With cUserId instead of userId
	header, err := buildMimoCookieHeader(
		[]browserCookie{
			{Name: "api-platform_serviceToken", Value: "token123", Domain: ".xiaomimimo.com", Path: "/"},
			{Name: "cUserId", Value: "cuser456", Domain: ".xiaomimimo.com", Path: "/"},
		},
		"https://platform.xiaomimimo.com/api/v1/balance",
		time.Now(),
	)
	if err != nil {
		t.Fatalf("unexpected error with cUserId: %v", err)
	}
	if !strings.Contains(header, "cUserId=cuser456") {
		t.Errorf("expected header to contain cUserId")
	}
}
