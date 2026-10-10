package oauth

import (
	"encoding/base64"
	"testing"
	"time"
)

func testJWT(payload string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(payload)) + ".sig"
}

func TestJWTExpiry(t *testing.T) {
	want := time.Date(2026, 10, 20, 22, 22, 42, 0, time.UTC)
	padded := base64.URLEncoding.EncodeToString([]byte(`{"exp":1792534962}`))
	for _, tc := range []struct {
		name  string
		token string
		ok    bool
	}{
		{"integer exp", testJWT(`{"exp":1792534962,"https://api.openai.com/auth":{"chatgpt_account_id":"acct"}}`), true},
		{"float exp", testJWT(`{"exp":1792534962.0}`), true},
		{"padded payload", "e30." + padded + ".sig", true},
		{"missing exp", testJWT(`{"iat":1792534962}`), false},
		{"zero exp", testJWT(`{"exp":0}`), false},
		{"not json", "e30." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".sig", false},
		{"opaque token", "stale", false},
		{"two segments", "a.b", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := JWTExpiry(tc.token)
			if ok != tc.ok {
				t.Fatalf("JWTExpiry ok = %v, want %v", ok, tc.ok)
			}
			if ok && !got.Equal(want) {
				t.Errorf("JWTExpiry = %s, want %s", got, want)
			}
		})
	}
}
