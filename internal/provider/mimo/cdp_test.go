package mimo

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joshuadavidthomas/vibeusage/internal/testenv"
)

// newFakeCDPServer serves /json/version over HTTP and answers a single
// Storage.getCookies command over a minimal WebSocket on the same listener,
// mimicking what Chrome exposes on --remote-debugging-port.
func newFakeCDPServer(t *testing.T, browser, userAgent string, cookies []cdpCookie) (port string) {
	t.Helper()
	const wsPath = "/devtools/browser/fake-browser-id"

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_, portOnly, _ := net.SplitHostPort(server.Listener.Addr().String())
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"Browser":              browser,
				"User-Agent":           userAgent,
				"webSocketDebuggerUrl": "ws://127.0.0.1:" + portOnly + wsPath,
			})
		case wsPath:
			serveFakeCDPWebSocket(t, w, r, cookies)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	return port
}

func serveFakeCDPWebSocket(t *testing.T, w http.ResponseWriter, r *http.Request, cookies []cdpCookie) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	defer func() { _ = conn.Close() }()

	h := sha1.New()
	h.Write([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	_ = rw.Flush()

	// Read one masked client frame (the command).
	if _, err := readFakeWSFrame(rw.Reader); err != nil {
		t.Errorf("read client frame: %v", err)
		return
	}

	var reply struct {
		ID     int `json:"id"`
		Result struct {
			Cookies []cdpCookie `json:"cookies"`
		} `json:"result"`
	}
	reply.ID = 1
	reply.Result.Cookies = cookies
	payload, _ := json.Marshal(reply)
	_, _ = rw.Write(encodeFakeWSFrame(payload))
	_ = rw.Flush()
}

func readFakeWSFrame(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(reader, ext); err != nil {
			return nil, err
		}
		length = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(reader, ext); err != nil {
			return nil, err
		}
		length = 0
		for _, b := range ext {
			length = length<<8 | int64(b)
		}
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return nil, err
	}
	if masked {
		for i := range buf {
			buf[i] ^= mask[i%4]
		}
	}
	return buf, nil
}

func encodeFakeWSFrame(payload []byte) []byte {
	frame := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		frame = append(frame, byte(n))
	case n <= 65535:
		frame = append(frame, 126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 127)
		for shift := 56; shift >= 0; shift -= 8 {
			frame = append(frame, byte(uint64(n)>>uint(shift)))
		}
	}
	return append(frame, payload...)
}

func signedInMimoCDPCookies() []cdpCookie {
	future := float64(time.Now().Add(time.Hour).Unix())
	return []cdpCookie{
		{Name: "api-platform_serviceToken", Value: "token123", Domain: ".xiaomimimo.com", Path: "/", Secure: true, Expires: future},
		{Name: "userId", Value: "user456", Domain: ".xiaomimimo.com", Path: "/", Secure: true, Expires: future},
		{Name: "unrelated", Value: "x", Domain: ".example.com", Path: "/", Expires: future},
	}
}

func TestCDPFixedPortsPrecedence(t *testing.T) {
	t.Setenv(cdpPortsEnv, "")
	ports, explicit := cdpFixedPorts(nil)
	if explicit || len(ports) != 1 || ports[0] != cdpDefaultPort {
		t.Errorf("default: ports=%v explicit=%v", ports, explicit)
	}

	ports, explicit = cdpFixedPorts([]int{9444, 9222, 9444, 0, 70000})
	if !explicit || strings.Join(ports, ",") != "9444,9222" {
		t.Errorf("config: ports=%v explicit=%v", ports, explicit)
	}

	t.Setenv(cdpPortsEnv, " 9333 , bogus, 9333 ,9222")
	ports, explicit = cdpFixedPorts([]int{9444})
	if !explicit || strings.Join(ports, ",") != "9333,9222" {
		t.Errorf("env override: ports=%v explicit=%v", ports, explicit)
	}
}

func TestCDPBrowserLabel(t *testing.T) {
	cases := []struct{ browser, ua, want string }{
		{"Chrome/153.0.8010.54", "Mozilla/5.0 ... Chrome/153.0.0.0", "Chrome (CDP :9444)"},
		{"Chrome/153.0.8010.54", "Mozilla/5.0 ... HeadlessChrome/153.0.0.0", "Chrome headless (CDP :9444)"},
		{"HeadlessChrome/120.0", "", "Chrome headless (CDP :9444)"},
		{"Edg/120.0", "", "Edge (CDP :9444)"},
		{"", "", "Chrome (CDP :9444)"},
	}
	for _, tc := range cases {
		if got := cdpBrowserLabel(tc.browser, tc.ua, "9444"); got != tc.want {
			t.Errorf("cdpBrowserLabel(%q, %q) = %q, want %q", tc.browser, tc.ua, got, tc.want)
		}
	}
}

func TestDiscoverFixedPortCDPEndpointsSkipsUnreachable(t *testing.T) {
	port := newFakeCDPServer(t, "Chrome/153.0", "Mozilla/5.0 Chrome/153", nil)
	closed := reserveClosedPort(t)

	endpoints := discoverFixedPortCDPEndpoints(context.Background(), []string{closed, port})
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want exactly the live server", endpoints)
	}
	if endpoints[0].Port != port || endpoints[0].Path != "/devtools/browser/fake-browser-id" {
		t.Errorf("endpoint = %+v", endpoints[0])
	}
	if endpoints[0].Label != "Chrome (CDP :"+port+")" {
		t.Errorf("label = %q", endpoints[0].Label)
	}
}

func isolateCDPDiscovery(t *testing.T) {
	t.Helper()
	testenv.ApplyVibeusage(t.Setenv, t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestImportMimoCDPSessionFindsSignedInBrowser(t *testing.T) {
	isolateCDPDiscovery(t)
	emptyPort := newFakeCDPServer(t, "Chrome/153.0", "HeadlessChrome/153", nil)
	signedInPort := newFakeCDPServer(t, "Chrome/153.0", "Chrome/153", signedInMimoCDPCookies())
	t.Setenv(cdpPortsEnv, emptyPort+","+signedInPort)

	session, err := importMimoCDPSession(context.Background())
	if err != nil {
		t.Fatalf("importMimoCDPSession() error: %v", err)
	}
	if session.SourceLabel != "Chrome (CDP :"+signedInPort+")" {
		t.Errorf("SourceLabel = %q", session.SourceLabel)
	}
	for _, want := range []string{"api-platform_serviceToken=token123", "userId=user456"} {
		if !strings.Contains(session.Cookie, want) {
			t.Errorf("cookie header missing %q: %q", want, session.Cookie)
		}
	}
	if strings.Contains(session.Cookie, "unrelated=") {
		t.Errorf("cookie header leaked a foreign-domain cookie: %q", session.Cookie)
	}
}

func TestImportMimoCDPSessionReportsEveryBrowserChecked(t *testing.T) {
	isolateCDPDiscovery(t)
	headlessPort := newFakeCDPServer(t, "Chrome/153.0", "HeadlessChrome/153", nil)
	headedPort := newFakeCDPServer(t, "Chrome/153.0", "Chrome/153", nil)
	closed := reserveClosedPort(t)
	t.Setenv(cdpPortsEnv, headlessPort+","+headedPort+","+closed)

	_, err := importMimoCDPSession(context.Background())
	if err == nil {
		t.Fatal("expected error when no browser is signed in")
	}
	msg := err.Error()
	for _, want := range []string{
		"no signed-in Xiaomi MiMo session",
		"Chrome headless (CDP :" + headlessPort + ")",
		"Chrome (CDP :" + headedPort + ")",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, closed) {
		t.Errorf("unreachable port should not be listed as checked: %q", msg)
	}
}

func TestImportMimoCDPSessionNoEndpoints(t *testing.T) {
	isolateCDPDiscovery(t)
	closed := reserveClosedPort(t)
	t.Setenv(cdpPortsEnv, closed)

	_, err := importMimoCDPSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), "probed ports "+closed) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func reserveClosedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	return port
}

// newFakeCDPServerPageFallback mimics Chrome 154+ headed profiles: the
// browser-level endpoint rejects Storage.getCookies ("Browser context
// management is not supported") while a page-level target serves the same
// cookie jar.
func newFakeCDPServerPageFallback(t *testing.T, cookies []cdpCookie) (port string) {
	t.Helper()
	const browserWSPath = "/devtools/browser/fake-browser-id"
	const pageWSPath = "/devtools/page/fake-page-id"

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, portOnly, _ := net.SplitHostPort(server.Listener.Addr().String())
		switch r.URL.Path {
		case "/json/version":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"Browser":              "Chrome/154.0",
				"User-Agent":           "Chrome/154.0",
				"webSocketDebuggerUrl": "ws://127.0.0.1:" + portOnly + browserWSPath,
			})
		case "/json/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{
					"id":                   "fake-page-id",
					"type":                 "page",
					"webSocketDebuggerUrl": "ws://127.0.0.1:" + portOnly + pageWSPath,
				},
			})
		case browserWSPath:
			serveFakeCDPWebSocketError(t, w, r, "Browser context management is not supported.")
		case pageWSPath:
			serveFakeCDPWebSocket(t, w, r, cookies)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	return port
}

func serveFakeCDPWebSocketError(t *testing.T, w http.ResponseWriter, r *http.Request, message string) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	defer func() { _ = conn.Close() }()

	h := sha1.New()
	h.Write([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	_ = rw.Flush()

	frame, err := readFakeWSFrame(rw.Reader)
	if err != nil {
		t.Errorf("read client frame: %v", err)
		return
	}
	var command struct {
		ID int `json:"id"`
	}
	_ = json.Unmarshal(frame, &command)

	payload, _ := json.Marshal(map[string]any{
		"id":    command.ID,
		"error": map[string]string{"message": message},
	})
	_, _ = rw.Write(encodeFakeWSFrame(payload))
	_ = rw.Flush()
}

// Chrome 154+ headed profiles reject browser-level Storage.getCookies; the
// import must still work by reading the same jar through a page target.
func TestFetchCookiesFromCDPEndpointFallsBackToPageTarget(t *testing.T) {
	port := newFakeCDPServerPageFallback(t, signedInMimoCDPCookies())
	endpoint, err := probeCDPPort(context.Background(), port)
	if err != nil {
		t.Fatalf("probeCDPPort() error: %v", err)
	}
	cookies, err := fetchCookiesFromCDPEndpoint(context.Background(), endpoint.Port, endpoint.Path)
	if err != nil {
		t.Fatalf("fetchCookiesFromCDPEndpoint() error: %v", err)
	}
	found := false
	for _, c := range cookies {
		if c.Name == "api-platform_serviceToken" {
			found = true
		}
	}
	if !found {
		t.Fatalf("page-target fallback missed cookies: %+v", cookies)
	}
}
