package httpclient

import (
	"context"
	"net/http"

	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
)

type skipRawDumpContextKey struct{}

// WithSkipRawDumpCtx marks a context as carrying a credential-exchange request
// whose response body must never be written to raw debug dumps.
func WithSkipRawDumpCtx(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipRawDumpContextKey{}, true)
}

// SkipRawDumpFromContext reports whether raw response dumping is suppressed for ctx.
func SkipRawDumpFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(skipRawDumpContextKey{}).(bool)
	return ok && v
}

// WithSkipRawDump marks a request as a credential exchange whose response body
// must never be written to raw debug dumps.
func WithSkipRawDump() RequestOption {
	return func(r *http.Request) {
		*r = *r.WithContext(WithSkipRawDumpCtx(r.Context()))
	}
}

// WithHeader sets a request header.
func WithHeader(key, value string) RequestOption {
	return func(r *http.Request) {
		r.Header.Set(key, value)
	}
}

// WithBearer sets the Authorization header to "Bearer <token>".
func WithBearer(token string) RequestOption {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// WithCookie adds a cookie to the request.
func WithCookie(name, value string) RequestOption {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
}

// WithProvider attaches a provider ID to the request context for debug dumps.
func WithProvider(providerID string) RequestOption {
	return func(r *http.Request) {
		if providerID == "" {
			return
		}
		*r = *r.WithContext(fetch.WithProvider(r.Context(), providerID))
	}
}
