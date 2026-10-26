package httpclient

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/config"
	"github.com/joshuadavidthomas/vibeusage/internal/fetch"
	"github.com/joshuadavidthomas/vibeusage/internal/logging"
)

const (
	// DebugRawEnv enables writing raw HTTP responses to $VIBEUSAGE_CACHE_DIR/raw/
	// when set to "1" or "true".
	DebugRawEnv = "VIBEUSAGE_DEBUG_RAW"

	// RedactedValue replaces sensitive header and query parameter values in dumps.
	RedactedValue = "[REDACTED]"
)

var dumpSequence atomic.Uint64

func debugRawEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(DebugRawEnv)))
	return v == "1" || v == "true"
}

func maybeWriteRawDump(ctx context.Context, req *http.Request, resp *http.Response, respBody []byte) {
	if shouldSkipRawDump(ctx, req, resp) {
		return
	}
	provider := resolveProvider(ctx, req, resp)
	path, err := writeRawDump(provider, req, resp, respBody)
	if err != nil {
		logging.FromContext(ctx).Debug("writing raw response dump failed", "provider", provider, "err", err)
		return
	}
	logging.FromContext(ctx).Debug("wrote raw response dump", "provider", provider, "path", path)
}

func shouldSkipRawDump(ctx context.Context, req *http.Request, resp *http.Response) bool {
	if SkipRawDumpFromContext(ctx) {
		return true
	}
	if req != nil && SkipRawDumpFromContext(req.Context()) {
		return true
	}
	if resp != nil && resp.Request != nil && SkipRawDumpFromContext(resp.Request.Context()) {
		return true
	}

	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	if n == 0 {
		return false
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		for _, candidate := range []string{frame.Function, filepath.ToSlash(frame.File)} {
			if strings.Contains(candidate, "/internal/auth/oauth/") ||
				strings.Contains(candidate, "/internal/auth/device/") ||
				strings.Contains(candidate, ".exchangeCode") ||
				strings.Contains(candidate, ".resolveSECToken") {
				return true
			}
		}
		if !more {
			break
		}
	}
	return false
}

func writeRawDump(provider string, req *http.Request, resp *http.Response, respBody []byte) (string, error) {
	now := time.Now().UTC()
	seq := dumpSequence.Add(1)
	filename := fmt.Sprintf("%s-%s-%d.dump", now.Format("20060102T150405.000Z"), sanitizeFilenameToken(provider), seq)
	content := formatRawDump(now, provider, req, resp, respBody)
	return config.WriteRawDump(filename, content)
}

func formatRawDump(now time.Time, provider string, req *http.Request, resp *http.Response, respBody []byte) []byte {
	var buf bytes.Buffer

	method := http.MethodGet
	if resp != nil && resp.Request != nil && resp.Request.Method != "" {
		method = resp.Request.Method
	} else if req != nil && req.Method != "" {
		method = req.Method
	}

	var targetURL *url.URL
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		targetURL = resp.Request.URL
	} else if req != nil {
		targetURL = req.URL
	}

	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	statusLine := fmt.Sprintf("%d", statusCode)
	if statusText := http.StatusText(statusCode); statusText != "" {
		statusLine = fmt.Sprintf("%d %s", statusCode, statusText)
	}

	fmt.Fprintf(&buf, "Timestamp: %s\n", now.Format(time.RFC3339Nano))
	fmt.Fprintf(&buf, "Provider: %s\n", provider)
	fmt.Fprintf(&buf, "Method: %s\n", method)
	fmt.Fprintf(&buf, "URL: %s\n", redactURL(targetURL))
	fmt.Fprintf(&buf, "Status: %s\n", statusLine)

	headers := collectSelectedHeaders(req, resp)
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&buf, "%s: %s\n", name, headers[name])
	}

	buf.WriteByte('\n')
	buf.Write(respBody)
	return buf.Bytes()
}

func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	cloned := *u
	cloned.User = nil
	cloned.Fragment = ""

	if cloned.RawQuery != "" {
		parts := strings.Split(cloned.RawQuery, "&")
		for i, part := range parts {
			if part == "" {
				continue
			}
			if k, _, ok := strings.Cut(part, "="); ok {
				parts[i] = k + "=" + RedactedValue
			} else {
				parts[i] = RedactedValue
			}
		}
		cloned.RawQuery = strings.Join(parts, "&")
	} else if cloned.ForceQuery {
		cloned.ForceQuery = false
		cloned.RawQuery = RedactedValue
	}

	return cloned.String()
}

var (
	mandatoryRedactedHeaders = map[string]struct{}{
		"authorization":       {},
		"cookie":              {},
		"set-cookie":          {},
		"proxy-authorization": {},
		"www-authenticate":    {},
		"proxy-authenticate":  {},
		"x-api-key":           {},
		"x-goog-api-key":      {},
		"x-auth-token":        {},
		"x-access-token":      {},
		"x-csrf-token":        {},
		"x-xsrf-token":        {},
		"x-xai-token-auth":    {},
	}

	sensitiveHeaderKeywords = []string{
		"auth",
		"cookie",
		"token",
		"secret",
		"key",
		"password",
		"credential",
		"session",
		"csrf",
		"xsrf",
	}

	safeDiagnosticHeaders = map[string]struct{}{
		"accept":           {},
		"accept-language":  {},
		"anthropic-beta":   {},
		"cache-control":    {},
		"cf-ray":           {},
		"content-encoding": {},
		"content-length":   {},
		"content-type":     {},
		"date":             {},
		"retry-after":      {},
		"server":           {},
		"user-agent":       {},
		"vary":             {},
		"x-request-id":     {},
		"x-trace-id":       {},
	}

	safeDiagnosticPrefixes = []string{
		"x-ratelimit-",
		"ratelimit-",
		"anthropic-ratelimit-",
	}
)

func isSensitiveHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if _, ok := mandatoryRedactedHeaders[lower]; ok {
		return true
	}
	for _, kw := range sensitiveHeaderKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func isSafeDiagnosticHeader(name string) bool {
	if isSensitiveHeader(name) {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	if _, ok := safeDiagnosticHeaders[lower]; ok {
		return true
	}
	for _, prefix := range safeDiagnosticPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func collectSelectedHeaders(req *http.Request, resp *http.Response) map[string]string {
	out := make(map[string]string)

	var sources []http.Header
	if req != nil {
		sources = append(sources, req.Header)
	}
	if resp != nil {
		if resp.Request != nil {
			sources = append(sources, resp.Request.Header)
		}
		sources = append(sources, resp.Header)
	}

	for _, hdr := range sources {
		for rawName, vals := range hdr {
			if len(vals) == 0 {
				continue
			}
			canon := http.CanonicalHeaderKey(rawName)
			if isSensitiveHeader(canon) {
				out[canon] = RedactedValue
				continue
			}
			if isSafeDiagnosticHeader(canon) {
				joined := strings.Join(vals, ", ")
				joined = strings.ReplaceAll(joined, "\r", " ")
				joined = strings.ReplaceAll(joined, "\n", " ")
				out[canon] = joined
			}
		}
	}

	return out
}

func resolveProvider(ctx context.Context, req *http.Request, resp *http.Response) string {
	if req != nil {
		if p := fetch.ProviderFromContext(req.Context()); p != "" {
			return p
		}
	}
	if resp != nil && resp.Request != nil {
		if p := fetch.ProviderFromContext(resp.Request.Context()); p != "" {
			return p
		}
	}
	if p := fetch.ProviderFromContext(ctx); p != "" {
		return p
	}
	if p := providerFromCallStack(); p != "" {
		return p
	}
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		if host := resp.Request.URL.Hostname(); host != "" {
			return host
		}
	}
	if req != nil && req.URL != nil {
		if host := req.URL.Hostname(); host != "" {
			return host
		}
	}
	return "unknown"
}

func providerFromCallStack() string {
	var pcs [32]uintptr
	n := runtime.Callers(3, pcs[:])
	if n == 0 {
		return ""
	}
	frames := runtime.CallersFrames(pcs[:n])
	const marker = "/internal/provider/"
	for {
		frame, more := frames.Next()
		if filePath := filepath.ToSlash(frame.File); filePath != "" {
			if idx := strings.Index(filePath, marker); idx >= 0 {
				rest := filePath[idx+len(marker):]
				if slash := strings.IndexByte(rest, '/'); slash > 0 {
					dir := rest[:slash]
					if !strings.Contains(dir, ".") {
						return dir
					}
				}
			}
		}
		if idx := strings.Index(frame.Function, marker); idx >= 0 {
			rest := frame.Function[idx+len(marker):]
			if end := strings.IndexAny(rest, "/."); end > 0 {
				return rest[:end]
			}
		}
		if !more {
			break
		}
	}
	return ""
}

func sanitizeFilenameToken(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "unknown"
	}
	return res
}
