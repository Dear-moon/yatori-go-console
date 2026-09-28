package mooc

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestNativeCDPHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		r, err := http.ReadRequest(bufio.NewReader(server))
		if err == nil {
			if _, exists := r.Header["Origin"]; exists {
				t.Error("native handshake sent web origin")
			}
			if r.Header.Get("Upgrade") != "websocket" {
				t.Error("upgrade header lost")
			}
			var payload [4]byte
			_, err = io.ReadFull(server, payload[:])
			if string(payload[:]) != "next" {
				t.Error("subsequent frames modified")
			}
		}
		done <- err
	}()
	c := &nativeCDPConn{Conn: client}
	parts := []string{"GET /devtools/page/test HTTP/1.1\r\nHost: localhost\r\nOri", "gin: devtools://devtools\r\nUpgrade: websocket\r\n\r\n", "next"}
	for _, p := range parts {
		n, err := c.Write([]byte(p))
		if err != nil || n != len(p) {
			t.Fatalf("write failed: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBrowserEndpointRestriction(t *testing.T) {
	for _, endpoint := range []string{"http://example.com:9222", "http://127.0.0.1:9222@evil.example", "https://127.0.0.1:9222", "http://127.0.0.1:9222?secret=value"} {
		cookies, err := readBrowserCookies(context.Background(), endpoint)
		if err == nil || cookies != nil {
			t.Fatal("invalid endpoint accepted")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("endpoint leaked")
		}
	}
}
