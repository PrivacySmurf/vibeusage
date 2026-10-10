package oauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// JWTExpiry returns the exp claim of a JWT without verifying its signature.
// Callers use it only to schedule renewal of a token they already hold (and
// to agree on expiry with other tools that share a CLI's renewal state); it
// must never be used to decide whether a token is trustworthy.
func JWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil || *claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Exp), 0).UTC(), true
}
