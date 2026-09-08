package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &config.Config{DeviceID: "8f3a2d91-4c1b-4a77-9e02-1f6b5c3d0a11", Name: "Test Laptop"}
	s, err := New(Options{Config: cfg, Port: 8765})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.http.Handler)
	s.hub.setBaseContext(context.Background())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestOriginAllowed(t *testing.T) {
	ok := []string{
		"chrome-extension://abcdefghijklmnop",
		"moz-extension://1234",
		"safari-web-extension://xyz",
		"http://localhost:3000",
		"http://127.0.0.1:5173",
	}
	bad := []string{
		"", "https://evil.com", "http://evil.com:80", "http://localhost",
		"chrome-extension://", "http://localhost:0", "http://127.0.0.1:99999",
		"http://localhost:3000/x", "file://",
	}
	for _, o := range ok {
		if !OriginAllowed(o) {
			t.Errorf("expected allowed: %q", o)
		}
	}
	for _, o := range bad {
		if OriginAllowed(o) {
			t.Errorf("expected rejected: %q", o)
		}
	}
}

func TestHTTPRoutes(t *testing.T) {
	_, ts := newTestServer(t)
	c := ts.Client()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/info", nil)
	req.Header.Set("Origin", "chrome-extension://abc")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var info protocol.DaemonInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if info.ProtocolVersion != 1 || info.Version != "0.1.0" || len(info.Capabilities) != 1 {
		t.Fatalf("bad info: %+v", info)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "chrome-extension://abc" {
		t.Fatalf("ACAO = %q", got)
	}
	if resp.Header.Get("Access-Control-Allow-Private-Network") != "true" || resp.Header.Get("Vary") != "Origin" {
		t.Fatalf("missing PNA/Vary headers: %v", resp.Header)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/info", nil)
	req.Header.Set("Origin", "https://evil.com")
	resp, _ = c.Do(req)
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("evil origin echoed: %q", got)
	}

	req, _ = http.NewRequest(http.MethodOptions, ts.URL+"/info", nil)
	req.Header.Set("Origin", "chrome-extension://abc")
	resp, _ = c.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS = %d", resp.StatusCode)
	}

	resp, _ = c.Get(ts.URL + "/health")
	var h protocol.HealthResponse
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if h.Status != "ok" {
		t.Fatalf("health = %+v", h)
	}

	resp, _ = c.Get(ts.URL + "/nope")
	var e protocol.HTTPError
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if resp.StatusCode != 404 || e.Error != "not found" {
		t.Fatalf("404 = %d %+v", resp.StatusCode, e)
	}
}

func TestWebSocketDispatch(t *testing.T) {
	_, ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws" + ts.URL[len("http"):] + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://abc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	send := func(v any) map[string]any {
		b, _ := json.Marshal(v)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := send(map[string]any{"type": "hello", "id": "c1", "protocolVersion": 1, "client": "extension", "clientVersion": "0.1.0"})
	if got["type"] != "ready" || got["id"] != "c1" || got["protocolVersion"].(float64) != 1 {
		t.Fatalf("ready = %v", got)
	}
	dev := got["device"].(map[string]any)
	if dev["name"] != "Test Laptop" || dev["version"] != "0.1.0" {
		t.Fatalf("device = %v", dev)
	}

	got = send(map[string]any{"type": "ping", "id": "c2", "t": 1757370000000})
	if got["type"] != "pong" || got["id"] != "c2" || int64(got["t"].(float64)) != 1757370000000 {
		t.Fatalf("pong = %v", got)
	}

	got = send(map[string]any{"type": "foo", "id": "c3"})
	if got["type"] != "error" || got["code"] != "unknown_type" || got["message"] != "unsupported message type: foo" {
		t.Fatalf("unknown = %v", got)
	}

	got = send(map[string]any{"type": "hello", "id": "c4", "protocolVersion": 99})
	if got["code"] != "protocol_mismatch" {
		t.Fatalf("mismatch = %v", got)
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	_, data, _ := conn.Read(ctx)
	var out map[string]any
	json.Unmarshal(data, &out)
	if out["code"] != "bad_json" {
		t.Fatalf("bad json = %v", out)
	}
	if _, ok := out["id"]; ok {
		t.Fatalf("push carried an id: %v", out)
	}
}

func TestWebSocketRejectsBadOrigin(t *testing.T) {
	_, ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "ws"+ts.URL[len("http"):]+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.com"}},
	})
	if err == nil {
		t.Fatal("expected evil origin to be rejected")
	}
}
