package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/peer"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

const selfDeviceID = "8f3a2d91-4c1b-4a77-9e02-1f6b5c3d0a11"

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	return newRoutedTestServer(t, nil)
}

// newRoutedTestServer builds a control server backed by a throwaway trust store
// and, optionally, a fake peer layer.
func newRoutedTestServer(t *testing.T, peers PeerRouter) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &config.Config{DeviceID: selfDeviceID, Name: "Test Laptop"}
	trust, err := config.LoadTrustStoreAt(nil, filepath.Join(t.TempDir(), config.TrustFileName))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Config: cfg, Port: 8765, Trust: trust, Peers: peers})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.http.Handler)
	s.hub.setBaseContext(context.Background())
	t.Cleanup(func() {
		s.stopDeviceTimer()
		ts.Close()
	})
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
	if info.ProtocolVersion != protocol.ProtocolVersion || info.Version != "0.1.0" || len(info.Capabilities) != 1 {
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

	got := send(map[string]any{"type": "hello", "id": "c1", "protocolVersion": protocol.ProtocolVersion, "client": "extension", "clientVersion": "0.1.0"})
	if got["type"] != "ready" || got["id"] != "c1" || int(got["protocolVersion"].(float64)) != protocol.ProtocolVersion {
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

	// A v1 client must be turned away cleanly rather than half-served: v2
	// changed the shape of the signal relay and added the peer plane, so
	// "close enough" would surface as an undecodable frame much later.
	got = send(map[string]any{"type": "hello", "id": "c5", "protocolVersion": protocol.ProtocolVersionV1, "client": "extension", "clientVersion": "0.1.0"})
	if got["type"] != "error" || got["id"] != "c5" || got["code"] != protocol.ErrCodeProtocolMismatch {
		t.Fatalf("v1 hello = %v", got)
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

// -----------------------------------------------------------------------------
// v2 routing: devices, pairing and the signal relay
// -----------------------------------------------------------------------------

// fakeRouter stands in for *peer.Manager so the control plane can be tested
// without a LAN listener. It records what it was asked to do and returns
// whatever error the test planted.
type fakeRouter struct {
	mu sync.Mutex

	started   []protocol.Device
	confirmed []peer.PairDecision
	forgotten []string
	signals   []routedSignal

	startErr   error
	confirmErr error
	forgetErr  error
	signalErr  error
}

type routedSignal struct {
	to  string
	msg protocol.PeerSignalMessage
}

func (f *fakeRouter) StartPairing(_ context.Context, dev protocol.Device) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, dev)
	return f.startErr
}

func (f *fakeRouter) Confirm(d peer.PairDecision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmed = append(f.confirmed, d)
	return f.confirmErr
}

func (f *fakeRouter) Forget(deviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, deviceID)
	return f.forgetErr
}

func (f *fakeRouter) SendSignal(_ context.Context, deviceID string, msg protocol.PeerSignalMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, routedSignal{to: deviceID, msg: msg})
	return f.signalErr
}

func (f *fakeRouter) snapshot() fakeRouter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeRouter{
		started:   append([]protocol.Device(nil), f.started...),
		confirmed: append([]peer.PairDecision(nil), f.confirmed...),
		forgotten: append([]string(nil), f.forgotten...),
		signals:   append([]routedSignal(nil), f.signals...),
	}
}

// wsClient is a connected control-plane client with the handshake done.
type wsClient struct {
	t    *testing.T
	ctx  context.Context
	conn *websocket.Conn
}

func dialWS(t *testing.T, ts *httptest.Server) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+ts.URL[len("http"):]+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"chrome-extension://abc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	c := &wsClient{t: t, ctx: ctx, conn: conn}
	c.send(map[string]any{
		"type": "hello", "id": "h1", "protocolVersion": protocol.ProtocolVersion,
		"client": "extension", "clientVersion": "0.1.0",
	})
	if got := c.expect(protocol.TypeReady); got["id"] != "h1" {
		t.Fatalf("handshake: %v", got)
	}
	return c
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.conn.Write(c.ctx, websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) read() map[string]any {
	c.t.Helper()
	_, data, err := c.conn.Read(c.ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		c.t.Fatal(err)
	}
	return out
}

// expect reads until a frame of the wanted type arrives, skipping debounced
// device pushes that may land at any moment.
func (c *wsClient) expect(want string) map[string]any {
	c.t.Helper()
	for i := 0; i < 8; i++ {
		got := c.read()
		if got["type"] == want {
			return got
		}
		if got["type"] == protocol.TypeDevices && want != protocol.TypeDevices {
			continue
		}
		c.t.Fatalf("expected %q, got %v", want, got)
	}
	c.t.Fatalf("never saw a %q frame", want)
	return nil
}

func (c *wsClient) expectError(reqID, code string) map[string]any {
	c.t.Helper()
	got := c.expect(protocol.TypeError)
	if got["id"] != reqID || got["code"] != code {
		c.t.Fatalf("expected error %s/%s, got %v", reqID, code, got)
	}
	return got
}

func onlineDevice(id, name string) protocol.Device {
	return protocol.Device{
		ID: id, Name: name, Address: "192.168.1.42", Port: protocol.DefaultPeerPort,
		Platform: protocol.PlatformLinux, Version: "0.1.0",
		Status: protocol.StatusOnline, LastSeen: time.Now().UnixMilli(),
	}
}

// A devices push must reach every connected client when the roster changes, and
// it must carry the trust store view of who is paired.
func TestDevicesPushAndRefresh(t *testing.T) {
	s, ts := newRoutedTestServer(t, &fakeRouter{})
	c := dialWS(t, ts)

	s.SetDevices([]protocol.Device{onlineDevice("peer-1", "Their Laptop")})

	got := c.expect(protocol.TypeDevices)
	list, _ := got["devices"].([]any)
	if len(list) != 1 {
		t.Fatalf("devices push = %v", got)
	}
	dev := list[0].(map[string]any)
	if dev["id"] != "peer-1" || dev["name"] != "Their Laptop" ||
		int(dev["port"].(float64)) != protocol.DefaultPeerPort ||
		dev["status"] != protocol.StatusOnline || dev["paired"] != false {
		t.Fatalf("device = %v", dev)
	}
	if _, ok := got["id"]; ok {
		t.Fatalf("push carried an id: %v", got)
	}

	// Pairing flips the flag without any new discovery event.
	token := strings.Repeat("ab", protocol.TokenBytes)
	if _, err := s.trust.Add("peer-1", "Their Laptop", token); err != nil {
		t.Fatal(err)
	}

	c.send(map[string]any{"type": protocol.TypeDevicesRefresh, "id": "r1"})
	got = c.expect(protocol.TypeDevices)
	list, _ = got["devices"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["paired"] != true {
		t.Fatalf("refresh after pairing = %v", got)
	}

	// A paired device that discovery has never seen still has to appear, or the
	// user has no way to forget it.
	s.SetDevices(nil)
	s.stopDeviceTimer()
	c.send(map[string]any{"type": protocol.TypeDevicesRefresh, "id": "r2"})
	got = c.expect(protocol.TypeDevices)
	list, _ = got["devices"].([]any)
	if len(list) != 1 {
		t.Fatalf("paired-but-absent device dropped: %v", got)
	}
	if dev := list[0].(map[string]any); dev["paired"] != true || dev["status"] != protocol.StatusOffline {
		t.Fatalf("paired-but-absent device = %v", dev)
	}
}

// A stale sighting must read as offline even if no sweep has run, and must be
// dropped once it is well past the TTL.
func TestDeviceSweep(t *testing.T) {
	s, _ := newRoutedTestServer(t, nil)

	stale := onlineDevice("peer-stale", "Old Laptop")
	stale.LastSeen = time.Now().Add(-3 * protocol.DeviceOnlineTTL).UnixMilli()
	s.SetDevices([]protocol.Device{stale})
	s.stopDeviceTimer()

	devices := s.Devices()
	if len(devices) != 1 || devices[0].Status != protocol.StatusOffline {
		t.Fatalf("stale device = %+v", devices)
	}

	s.SweepDevices()
	s.stopDeviceTimer()
	if got := s.Devices(); len(got) != 0 {
		t.Fatalf("expected the stale device to be dropped, got %+v", got)
	}
}

// pair:start is answered locally whenever it can be: an unknown id, a device
// that has aged out, or one that is already trusted.
func TestPairStartRouting(t *testing.T) {
	router := &fakeRouter{}
	s, ts := newRoutedTestServer(t, router)
	c := dialWS(t, ts)

	c.send(map[string]any{"type": protocol.TypePairStart, "id": "p0"})
	c.expectError("p0", protocol.ErrCodeInvalidRequest)

	c.send(map[string]any{"type": protocol.TypePairStart, "id": "p1", "deviceId": "ghost"})
	c.expectError("p1", protocol.ErrCodeUnknownDevice)

	stale := onlineDevice("peer-stale", "Old Laptop")
	stale.LastSeen = time.Now().Add(-2 * protocol.DeviceOnlineTTL).UnixMilli()
	s.SetDevices([]protocol.Device{stale, onlineDevice("peer-1", "Their Laptop")})
	s.stopDeviceTimer()

	c.send(map[string]any{"type": protocol.TypePairStart, "id": "p2", "deviceId": "peer-stale"})
	c.expectError("p2", protocol.ErrCodeDeviceOffline)

	c.send(map[string]any{"type": protocol.TypePairStart, "id": "p3", "deviceId": "peer-1"})
	// Success is silent on the request; progress arrives as pair:code and
	// pair:result pushes. Prove the daemon is still answering.
	c.send(map[string]any{"type": protocol.TypePing, "id": "p4", "t": 1})
	if got := c.expect(protocol.TypePong); got["id"] != "p4" {
		t.Fatalf("pong = %v", got)
	}
	if snap := router.snapshot(); len(snap.started) != 1 || snap.started[0].ID != "peer-1" {
		t.Fatalf("StartPairing calls = %+v", router.snapshot().started)
	}

	token := strings.Repeat("cd", protocol.TokenBytes)
	if _, err := s.trust.Add("peer-1", "Their Laptop", token); err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"type": protocol.TypePairStart, "id": "p5", "deviceId": "peer-1"})
	c.expectError("p5", protocol.ErrCodeAlreadyPaired)
}

// A confirm with nothing waiting must be rejected cleanly: the user is clicking
// a button for a pairing that already timed out.
func TestPairConfirmWithoutPairing(t *testing.T) {
	router := &fakeRouter{confirmErr: peer.ErrNoPendingPairing}
	_, ts := newRoutedTestServer(t, router)
	c := dialWS(t, ts)

	c.send(map[string]any{"type": protocol.TypePairConfirm, "id": "c1", "deviceId": "peer-1", "accept": true})
	c.expectError("c1", protocol.ErrCodeInvalidRequest)

	c.send(map[string]any{"type": protocol.TypePairConfirm, "id": "c2", "accept": true})
	c.expectError("c2", protocol.ErrCodeInvalidRequest)

	// A decline must carry a reason down to the peer layer, so the far end can
	// be told why rather than being left to time out.
	router.mu.Lock()
	router.confirmErr = nil
	router.mu.Unlock()
	c.send(map[string]any{"type": protocol.TypePairConfirm, "id": "c3", "deviceId": "peer-1", "accept": false})
	c.send(map[string]any{"type": protocol.TypePing, "id": "c4", "t": 2})
	c.expect(protocol.TypePong)

	// Two calls, not three: the confirm with no deviceId never reaches the peer
	// layer, it is rejected on validation.
	snap := router.snapshot()
	if len(snap.confirmed) != 2 {
		t.Fatalf("Confirm calls = %+v", snap.confirmed)
	}
	last := snap.confirmed[1]
	if last.DeviceID != "peer-1" || last.Accept || last.Reason != protocol.PairReasonDeclined {
		t.Fatalf("decline decision = %+v", last)
	}
}

func TestPairForget(t *testing.T) {
	router := &fakeRouter{}
	s, ts := newRoutedTestServer(t, router)
	c := dialWS(t, ts)

	token := strings.Repeat("ef", protocol.TokenBytes)
	if _, err := s.trust.Add("peer-1", "Their Laptop", token); err != nil {
		t.Fatal(err)
	}

	c.send(map[string]any{"type": protocol.TypePairForget, "id": "f1", "deviceId": "peer-1"})
	got := c.expect(protocol.TypeDevices)
	if _, ok := got["devices"]; !ok {
		t.Fatalf("forget did not re-broadcast the roster: %v", got)
	}
	if snap := router.snapshot(); len(snap.forgotten) != 1 || snap.forgotten[0] != "peer-1" {
		t.Fatalf("Forget calls = %+v", router.snapshot().forgotten)
	}
}

// A signal that cannot be routed must come back as a typed error. Dropping it
// silently would surface as WebRTC hanging forever with nothing in the logs.
func TestSignalRouting(t *testing.T) {
	router := &fakeRouter{}
	s, ts := newRoutedTestServer(t, router)
	c := dialWS(t, ts)

	c.send(map[string]any{"type": protocol.TypeSignalOffer, "id": "s1", "to": "ghost", "sdp": "v=0"})
	c.expectError("s1", protocol.ErrCodeUnknownDevice)

	c.send(map[string]any{"type": protocol.TypeSignalOffer, "id": "s2", "sdp": "v=0"})
	c.expectError("s2", protocol.ErrCodeInvalidRequest)

	c.send(map[string]any{"type": protocol.TypeSignalAnswer, "id": "s3", "to": "peer-1"})
	c.expectError("s3", protocol.ErrCodeInvalidRequest)

	c.send(map[string]any{"type": protocol.TypeSignalOffer, "id": "s4", "to": selfDeviceID, "sdp": "v=0"})
	c.expectError("s4", protocol.ErrCodeInvalidRequest)

	s.SetDevices([]protocol.Device{onlineDevice("peer-1", "Their Laptop")})
	s.stopDeviceTimer()

	c.send(map[string]any{"type": protocol.TypeSignalOffer, "id": "s5", "to": "peer-1", "sdp": "v=0 offer"})
	c.send(map[string]any{"type": protocol.TypeSignalICE, "id": "s6", "to": "peer-1",
		"candidate": "candidate:1 1 udp", "sdpMid": "0", "sdpMLineIndex": 0})
	c.send(map[string]any{"type": protocol.TypePing, "id": "s7", "t": 3})
	if got := c.expect(protocol.TypePong); got["id"] != "s7" {
		t.Fatalf("pong = %v", got)
	}

	snap := router.snapshot()
	if len(snap.signals) != 2 {
		t.Fatalf("relayed signals = %+v", snap.signals)
	}
	offer := snap.signals[0]
	if offer.to != "peer-1" || offer.msg.Kind != protocol.SignalKindOffer ||
		offer.msg.SDP != "v=0 offer" || offer.msg.From != selfDeviceID {
		t.Fatalf("relayed offer = %+v", offer)
	}
	ice := snap.signals[1]
	if ice.msg.Kind != protocol.SignalKindICE || ice.msg.Candidate != "candidate:1 1 udp" ||
		ice.msg.From != selfDeviceID {
		t.Fatalf("relayed ice = %+v", ice)
	}

	// A peer that cannot be reached is reported, not swallowed.
	router.mu.Lock()
	router.signalErr = peer.ErrPeerUnreachable
	router.mu.Unlock()
	c.send(map[string]any{"type": protocol.TypeSignalOffer, "id": "s8", "to": "peer-1", "sdp": "v=0"})
	c.expectError("s8", protocol.ErrCodePeerUnreachable)

	router.mu.Lock()
	router.signalErr = peer.ErrNotPaired
	router.mu.Unlock()
	c.send(map[string]any{"type": protocol.TypeSignalOffer, "id": "s9", "to": "peer-1", "sdp": "v=0"})
	c.expectError("s9", protocol.ErrCodeNotPaired)
}

// The peer layer reports up through peer.Handler; those calls must land on the
// wire as the pushes the extension is waiting for.
func TestPeerHandlerPushes(t *testing.T) {
	s, ts := newRoutedTestServer(t, &fakeRouter{})
	c := dialWS(t, ts)

	s.OnPairCode("peer-1", "Their Laptop", "482913", protocol.PairDirectionOutgoing)
	got := c.expect(protocol.TypePairCode)
	if got["deviceId"] != "peer-1" || got["code"] != "482913" ||
		got["direction"] != protocol.PairDirectionOutgoing || got["name"] != "Their Laptop" {
		t.Fatalf("pair:code = %v", got)
	}

	s.OnPeerState("peer-1", protocol.PeerStatePairing, "")
	got = c.expect(protocol.TypePeerState)
	if got["state"] != protocol.PeerStatePairing {
		t.Fatalf("peer:state = %v", got)
	}

	s.OnPairResult("peer-1", true, "")
	got = c.expect(protocol.TypePairResult)
	if got["paired"] != true || got["deviceId"] != "peer-1" {
		t.Fatalf("pair:result = %v", got)
	}
	// A result changes the paired flag, so the roster follows it immediately.
	c.expect(protocol.TypeDevices)

	s.OnSignal(protocol.NewPeerSignalOffer("peer-1", "v=0 remote"))
	got = c.expect(protocol.TypeSignalOffer)
	if got["from"] != "peer-1" || got["sdp"] != "v=0 remote" {
		t.Fatalf("signal:offer push = %v", got)
	}
	if _, ok := got["to"]; ok {
		t.Fatalf("push carried a to field: %v", got)
	}

	s.OnSignal(protocol.NewPeerSignalICE("peer-1", "candidate:9", "0", 0))
	got = c.expect(protocol.TypeSignalICE)
	if got["from"] != "peer-1" || got["candidate"] != "candidate:9" {
		t.Fatalf("signal:ice push = %v", got)
	}
}
