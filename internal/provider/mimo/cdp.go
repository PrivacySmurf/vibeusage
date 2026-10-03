package mimo

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Chrome DevTools Protocol (CDP) cookie import for Xiaomi MiMo.
//
// Reading cookies over CDP avoids both the SQLite cookie database (which the
// running browser keeps locked) and the OS keychain (which pops a password
// dialog on macOS). Any Chromium started with --remote-debugging-port can be
// read this way, so this file is platform-independent.

const (
	cdpPortsEnv         = "VIBEUSAGE_CDP_PORTS"
	cdpDefaultPort      = "9222"
	cdpProbeTimeout     = 2 * time.Second
	cdpEndpointTimeout  = 4 * time.Second
	mimoTargetCookieURL = "https://platform.xiaomimimo.com/api/v1/balance"
)

type cdpEndpoint struct {
	// Label is user-facing, e.g. "Chrome (CDP :9444)".
	Label string
	Port  string
	// Path is the browser-level WebSocket path, e.g. /devtools/browser/<id>.
	Path string
}

type cdpCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Secure  bool    `json:"secure"`
	Expires float64 `json:"expires"`
}

type cdpVersionInfo struct {
	Browser              string `json:"Browser"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpFixedPorts returns the fixed remote-debugging ports to probe and whether
// they were explicitly configured (env or config) rather than defaulted.
// VIBEUSAGE_CDP_PORTS (comma-separated) overrides [providers.mimo]
// cdp_ports, which overrides the Chrome default of 9222.
func cdpFixedPorts(configured []int) ([]string, bool) {
	if raw := strings.TrimSpace(os.Getenv(cdpPortsEnv)); raw != "" {
		var ports []string
		for _, part := range strings.Split(raw, ",") {
			if port := normalizeCDPPort(strings.TrimSpace(part)); port != "" {
				ports = append(ports, port)
			}
		}
		return dedupeStrings(ports), true
	}
	if len(configured) > 0 {
		ports := make([]string, 0, len(configured))
		for _, port := range configured {
			if normalized := normalizeCDPPort(strconv.Itoa(port)); normalized != "" {
				ports = append(ports, normalized)
			}
		}
		return dedupeStrings(ports), true
	}
	return []string{cdpDefaultPort}, false
}

func normalizeCDPPort(value string) string {
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 {
		return ""
	}
	return strconv.Itoa(port)
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// discoverFixedPortCDPEndpoints asks each port's /json/version for its
// browser-level WebSocket URL. Unreachable ports are skipped silently.
func discoverFixedPortCDPEndpoints(ctx context.Context, ports []string) []cdpEndpoint {
	var endpoints []cdpEndpoint
	for _, port := range ports {
		endpoint, err := probeCDPPort(ctx, port)
		if err != nil {
			continue
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints
}

func probeCDPPort(ctx context.Context, port string) (cdpEndpoint, error) {
	probeCtx, cancel := context.WithTimeout(ctx, cdpProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://127.0.0.1:"+port+"/json/version", nil)
	if err != nil {
		return cdpEndpoint{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cdpEndpoint{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cdpEndpoint{}, fmt.Errorf("cdp :%s version status %d", port, resp.StatusCode)
	}

	var info cdpVersionInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&info); err != nil {
		return cdpEndpoint{}, fmt.Errorf("cdp :%s version: %w", port, err)
	}
	wsURL, err := url.Parse(info.WebSocketDebuggerURL)
	if err != nil || wsURL.Path == "" {
		return cdpEndpoint{}, fmt.Errorf("cdp :%s version: missing webSocketDebuggerUrl", port)
	}
	return cdpEndpoint{
		Label: cdpBrowserLabel(info.Browser, info.UserAgent, port),
		Port:  port,
		Path:  wsURL.Path,
	}, nil
}

func cdpBrowserLabel(browser, userAgent, port string) string {
	name := strings.TrimSpace(strings.SplitN(browser, "/", 2)[0])
	headless := strings.Contains(userAgent, "HeadlessChrome") || strings.EqualFold(name, "HeadlessChrome")
	switch {
	case name == "" || strings.EqualFold(name, "HeadlessChrome"):
		name = "Chrome"
	case strings.EqualFold(name, "Edg"):
		name = "Edge"
	}
	if headless {
		name += " headless"
	}
	return fmt.Sprintf("%s (CDP :%s)", name, port)
}

// discoverCDPEndpoints merges explicitly configured fixed ports with any
// DevToolsActivePort files the platform knows about, de-duplicated by port
// with configured ports taking priority.
func discoverCDPEndpoints(ctx context.Context) ([]cdpEndpoint, []string) {
	ports, _ := cdpFixedPorts(configuredCDPPorts())
	endpoints := discoverFixedPortCDPEndpoints(ctx, ports)
	seen := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		seen[endpoint.Port] = true
	}
	for _, endpoint := range platformCDPActivePortEndpoints() {
		if seen[endpoint.Port] {
			continue
		}
		seen[endpoint.Port] = true
		endpoints = append(endpoints, endpoint)
	}
	return endpoints, ports
}

// importMimoCDPSession tries every discovered CDP endpoint and returns
// the first one holding a signed-in Xiaomi MiMo session. On failure the
// error names each browser that was checked so the user knows where to sign in.
func importMimoCDPSession(ctx context.Context) (browserSession, error) {
	endpoints, probed := discoverCDPEndpoints(ctx)
	if len(endpoints) == 0 {
		return browserSession{}, fmt.Errorf("%w: no Chrome DevTools endpoint reachable on 127.0.0.1 (probed ports %s)",
			errBrowserCookieImportUnavailable, strings.Join(probed, ", "))
	}

	checked := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		endpointCtx, cancel := context.WithTimeout(ctx, cdpEndpointTimeout)
		cookies, err := fetchCookiesFromCDPEndpoint(endpointCtx, endpoint.Port, endpoint.Path)
		cancel()
		if err != nil {
			checked = append(checked, endpoint.Label+" (unreadable)")
			continue
		}
		header, err := buildMimoCookieHeader(
			cookies,
			mimoTargetCookieURL,
			time.Now(),
		)
		if err != nil {
			checked = append(checked, endpoint.Label)
			continue
		}
		return browserSession{Cookie: header, SourceLabel: endpoint.Label}, nil
	}

	return browserSession{}, fmt.Errorf("%w: no signed-in Xiaomi MiMo session in %s",
		errBrowserCookieImportUnavailable, strings.Join(checked, ", "))
}

// fetchCookiesFromCDPEndpoint asks the browser target for every cookie it holds.
func fetchCookiesFromCDPEndpoint(ctx context.Context, port, path string) ([]browserCookie, error) {
	conn, err := dialCDP(ctx, port, path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var res struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	if err := conn.call("Storage.getCookies", nil, &res); err != nil {
		return nil, err
	}
	out := make([]browserCookie, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		var exp time.Time
		if c.Expires > 0 {
			exp = time.Unix(int64(c.Expires), 0)
		}
		out = append(out, browserCookie{
			Name:       c.Name,
			Value:      c.Value,
			Domain:     c.Domain,
			Path:       c.Path,
			Secure:     c.Secure,
			ExpiresAt:  exp,
			LastAccess: time.Now(),
		})
	}
	return out, nil
}

// cdpConn is a minimal WebSocket client for one DevTools target: enough to
// send commands and match their replies, without pulling in a WebSocket
// dependency. Events are discarded.
type cdpConn struct {
	conn   net.Conn
	reader *bufio.Reader
	nextID int
}

func dialCDP(ctx context.Context, port, path string) (*cdpConn, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(cdpEndpointTimeout))
	}

	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	secKey := base64.StdEncoding.EncodeToString(keyBytes)

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, port, secKey)
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ws handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("ws handshake status %d", resp.StatusCode)
	}

	h := sha1.New()
	h.Write([]byte(secKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expectedAccept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if resp.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		_ = conn.Close()
		return nil, errors.New("invalid Sec-WebSocket-Accept")
	}
	return &cdpConn{conn: conn, reader: reader}, nil
}

func (c *cdpConn) Close() {
	_ = c.conn.Close()
}

// call sends one command and decodes its "result" into out (which may be
// nil). A DevTools error reply is returned as an error.
func (c *cdpConn) call(method string, params any, out any) error {
	c.nextID++
	id := c.nextID
	msg := struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.writeFrame(0x1, payload); err != nil {
		return err
	}

	for {
		opcode, buf, err := c.readFrame()
		if err != nil {
			return err
		}
		switch opcode {
		case 0x8: // close
			return errors.New("cdp connection closed by browser")
		case 0x9: // ping
			_ = c.writeFrame(0xA, buf)
			continue
		case 0x1, 0x2:
		default:
			continue
		}

		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(buf, &reply); err != nil || reply.ID != id {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("%s: %s", method, reply.Error.Message)
		}
		if out == nil || len(reply.Result) == 0 {
			return nil
		}
		return json.Unmarshal(reply.Result, out)
	}
}

// writeFrame sends one masked client frame (RFC 6455 requires masking).
func (c *cdpConn) writeFrame(opcode byte, payload []byte) error {
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, 0x80|opcode)
	length := len(payload)
	switch {
	case length < 126:
		frame = append(frame, byte(0x80|length))
	case length <= 65535:
		frame = append(frame, 0x80|126, byte(length>>8), byte(length))
	default:
		frame = append(frame, 0x80|127)
		for shift := 56; shift >= 0; shift -= 8 {
			frame = append(frame, byte(uint64(length)>>uint(shift)))
		}
	}
	maskKey := make([]byte, 4)
	_, _ = rand.Read(maskKey)
	frame = append(frame, maskKey...)
	for i, b := range payload {
		frame = append(frame, b^maskKey[i%4])
	}
	_, err := c.conn.Write(frame)
	return err
}

func (c *cdpConn) readFrame() (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.reader, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	isMasked := (header[1] & 0x80) != 0
	payloadLen := int64(header[1] & 0x7f)
	switch payloadLen {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(c.reader, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = int64(ext[0])<<8 | int64(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(c.reader, ext); err != nil {
			return 0, nil, err
		}
		payloadLen = 0
		for _, b := range ext {
			payloadLen = payloadLen<<8 | int64(b)
		}
	}
	var mask [4]byte
	if isMasked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	buf := make([]byte, payloadLen)
	if _, err := io.ReadFull(c.reader, buf); err != nil {
		return 0, nil, err
	}
	if isMasked {
		for i := range buf {
			buf[i] ^= mask[i%4]
		}
	}
	return opcode, buf, nil
}

// --- CDP target management for auto-login ---

type cdpTarget struct {
	ID                   string `json:"id"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func (t cdpTarget) wsPath() string {
	if parsed, err := url.Parse(t.WebSocketDebuggerURL); err == nil && parsed.Path != "" {
		return parsed.Path
	}
	return "/devtools/page/" + t.ID
}

func openCDPTarget(ctx context.Context, port, pageURL string) (cdpTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://127.0.0.1:"+port+"/json/new?"+pageURL, nil)
	if err != nil {
		return cdpTarget{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cdpTarget{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cdpTarget{}, fmt.Errorf("/json/new status %d", resp.StatusCode)
	}
	var target cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		return cdpTarget{}, err
	}
	if target.ID == "" {
		return cdpTarget{}, errors.New("/json/new returned no target id")
	}
	return target, nil
}

func closeCDPTarget(port, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/json/close/"+id, nil)
	if err != nil {
		return
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// evalInContext runs expression in executionContextId (0 = page default)
// and decodes its by-value result into out.
func (c *cdpConn) evalInContext(contextID int, expression string, out any) error {
	params := map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}
	if contextID != 0 {
		params["contextId"] = contextID
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call("Runtime.evaluate", params, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("page script error: %s", res.ExceptionDetails.Text)
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}
