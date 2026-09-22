package mooc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

func browserCookies(ctx context.Context) ([]*http.Cookie, error) {
	endpoint := os.Getenv("YATORI_MOOC_CDP_URL")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:9222"
	}
	return readBrowserCookies(ctx, endpoint)
}

func readBrowserCookies(ctx context.Context, endpoint string) ([]*http.Cookie, error) {
	fail := errors.New("MOOC browser connection failed; open a dedicated browser with remote debugging enabled")
	base, e := url.Parse(endpoint)
	if e != nil || base.Scheme != "http" || !loopbackURL(base) || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, fail
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	target := *base
	target.Path = "/json/list"
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if e != nil {
		return nil, fail
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fail
	}
	var tabs []struct {
		Type   string `json:"type"`
		URL    string `json:"url"`
		Socket string `json:"webSocketDebuggerUrl"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tabs) != nil {
		return nil, fail
	}
	var socket string
	for _, tab := range tabs {
		u, err := url.Parse(tab.URL)
		if err == nil && tab.Type == "page" && u.Scheme == "https" && u.Hostname() == "www.icourse163.org" {
			socket = tab.Socket
			break
		}
	}
	if socket == "" {
		return nil, errors.New("MOOC page not found in debug browser; open https://www.icourse163.org/ and sign in")
	}
	wsURL, e := url.Parse(socket)
	if e != nil || wsURL.Scheme != "ws" || !loopbackURL(wsURL) || wsURL.Host != base.Host {
		return nil, fail
	}
	cfg, e := websocket.NewConfig(socket, "devtools://devtools")
	if e != nil {
		return nil, fail
	}
	raw, e := (&net.Dialer{}).DialContext(ctx, "tcp", wsURL.Host)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail
	}
	defer raw.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	stopHandshake := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stopHandshake()
	ws, e := websocket.NewClient(cfg, &nativeCDPConn{Conn: raw})
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	ws.MaxPayloadBytes = 1 << 20
	if deadline, ok := ctx.Deadline(); ok {
		_ = ws.SetDeadline(deadline)
	}
	command := struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{1, "Network.getCookies", map[string]any{"urls": []string{"https://www.icourse163.org/", "https://reg.icourse163.org/"}}}
	if websocket.JSON.Send(ws, command) != nil {
		return nil, fail
	}
	for {
		var result struct {
			ID     int             `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result struct {
				Cookies []struct {
					Name, Value, Domain, Path string
					Secure, HttpOnly, Session bool
					Expires                   float64
				}
			} `json:"result"`
		}
		if websocket.JSON.Receive(ws, &result) != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fail
		}
		if result.ID != 1 {
			continue
		}
		if len(result.Error) != 0 {
			return nil, fail
		}
		cookies := make([]*http.Cookie, 0, len(result.Result.Cookies))
		for _, v := range result.Result.Cookies {
			host := strings.TrimPrefix(strings.ToLower(v.Domain), ".")
			if host != "icourse163.org" && !strings.HasSuffix(host, ".icourse163.org") {
				continue
			}
			c := &http.Cookie{Name: v.Name, Value: v.Value, Domain: v.Domain, Path: v.Path, Secure: v.Secure, HttpOnly: v.HttpOnly}
			if !v.Session && v.Expires > 0 {
				c.Expires = time.Unix(int64(v.Expires), 0)
			}
			cookies = append(cookies, c)
		}
		if len(cookies) == 0 {
			return nil, errors.New("MOOC browser has no session cookies; sign in first")
		}
		return cookies, nil
	}
}

func loopbackURL(u *url.URL) bool {
	ip := net.ParseIP(u.Hostname())
	return u.User == nil && ip != nil && ip.IsLoopback()
}

// Native CDP clients have no web origin; keep browser origin restrictions intact.
type nativeCDPConn struct {
	net.Conn
	pending []byte
	ready   bool
}

func (c *nativeCDPConn) Write(p []byte) (int, error) {
	if c.ready {
		return c.Conn.Write(p)
	}
	c.pending = append(c.pending, p...)
	if len(c.pending) > 16384 {
		return 0, errors.New("CDP handshake too large")
	}
	end := bytes.Index(c.pending, []byte("\r\n\r\n"))
	if end < 0 {
		return len(p), nil
	}
	var request bytes.Buffer
	for _, line := range bytes.Split(c.pending[:end], []byte("\r\n")) {
		if bytes.HasPrefix(bytes.ToLower(line), []byte("origin:")) {
			continue
		}
		request.Write(line)
		request.WriteString("\r\n")
	}
	request.WriteString("\r\n")
	request.Write(c.pending[end+4:])
	_, err := io.Copy(c.Conn, &request)
	c.pending = nil
	if err != nil {
		return 0, err
	}
	c.ready = true
	return len(p), nil
}
