package zhihuishu

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yatori-dev/yatori-go-core/api/zhihuishu"
	"golang.org/x/net/websocket"
)

func browserSession(ctx context.Context) (*zhihuishu.BrowserSession, error) {
	endpoint := os.Getenv("YATORI_ZHIHUISHU_CDP_URL")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:9223"
	}
	return readBrowserSession(ctx, endpoint)
}

func readBrowserSession(ctx context.Context, endpoint string) (*zhihuishu.BrowserSession, error) {
	fail := errors.New("ZHIHUISHU browser connection failed; open a dedicated browser with remote debugging enabled")
	base, e := url.Parse(endpoint)
	if e != nil || base.Scheme != "http" || !loopbackURL(base) || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, fail
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
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
		if err == nil && tab.Type == "page" && u.Scheme == "https" && u.Hostname() == "ai-smart-course-student-pro.zhihuishu.com" {
			socket = tab.Socket
			break
		}
	}
	if socket == "" {
		return nil, errors.New("ZHIHUISHU page not found in debug browser; open https://ai-smart-course-student-pro.zhihuishu.com/ and sign in")
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
	call := func(id int, method string, params map[string]any, output any) error {
		if websocket.JSON.Send(ws, map[string]any{"id": id, "method": method, "params": params}) != nil {
			return fail
		}
		for {
			var response struct {
				ID     int             `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if websocket.JSON.Receive(ws, &response) != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fail
			}
			if response.ID != id {
				continue
			}
			if len(response.Error) != 0 || json.Unmarshal(response.Result, output) != nil {
				return fail
			}
			return nil
		}
	}
	// Restored background tabs may suspend asynchronous application requests.
	if err := call(4, "Page.setWebLifecycleState", map[string]any{"state": "active"}, &struct{}{}); err != nil {
		return nil, err
	}
	if err := call(3, "Page.bringToFront", map[string]any{}, &struct{}{}); err != nil {
		return nil, err
	}
	var evaluated struct {
		Exception json.RawMessage `json:"exceptionDetails"`
		Result    struct {
			Value *struct {
				Keys  map[string]string `json:"keys"`
				IV    string            `json:"iv"`
				MapID string            `json:"mapUid"`
			} `json:"value"`
		} `json:"result"`
	}
	// Resolve rotating protocol material through the signed-in application.
	expression := `(async()=>{const keys={};for(const id of [6,13]){const wrapped=await window.labc(id);const pair=rsaUtil.genKeyPair();keys[id]=rsaUtil.decrypt(wrapped,pair.publicKey).cKey}return {keys,iv:aesUtil.iv.toString(CryptoJS.enc.Hex),mapUid:sessionStorage.getItem('mapUid')||''}})()`
	fail = errors.New("智慧树动态会话读取失败，请刷新课程章节页面后重试")
	if err := call(1, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}, &evaluated); err != nil {
		return nil, err
	}
	if len(evaluated.Exception) != 0 || evaluated.Result.Value == nil {
		return nil, errors.New("智慧树页面尚未就绪，请先打开 AI 课程的章节页面")
	}
	material := evaluated.Result.Value
	fail = errors.New("智慧树动态协议材料格式不匹配")
	iv, err := hex.DecodeString(material.IV)
	if err != nil || len(iv) != 16 || len(material.Keys["6"]) != 16 || len(material.Keys["13"]) != 16 {
		return nil, fail
	}
	var result struct {
		Cookies []struct {
			Name, Value, Domain, Path string
			Secure, HttpOnly, Session bool
			Expires                   float64
		} `json:"cookies"`
	}
	fail = errors.New("智慧树浏览器 Cookie 读取失败")
	if err := call(2, "Network.getCookies", map[string]any{"urls": []string{"https://onlineservice-api.zhihuishu.com/", "https://kg-ai-run.zhihuishu.com/", "https://newbase.zhihuishu.com/"}}, &result); err != nil {
		return nil, err
	}
	cookies := make([]*http.Cookie, 0, len(result.Cookies))
	for _, v := range result.Cookies {
		host := strings.TrimPrefix(strings.ToLower(v.Domain), ".")
		if host != "zhihuishu.com" && !strings.HasSuffix(host, ".zhihuishu.com") {
			continue
		}
		c := &http.Cookie{Name: v.Name, Value: v.Value, Domain: v.Domain, Path: v.Path, Secure: v.Secure, HttpOnly: v.HttpOnly}
		if !v.Session && v.Expires > 0 {
			c.Expires = time.Unix(int64(v.Expires), 0)
		}
		cookies = append(cookies, c)
	}
	if len(cookies) == 0 {
		return nil, errors.New("ZHIHUISHU browser has no session cookies; sign in first")
	}
	return &zhihuishu.BrowserSession{Cookies: cookies, AIKey: []byte(material.Keys["6"]), CourseKey: []byte(material.Keys["13"]), IV: iv, MapID: material.MapID}, nil
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
