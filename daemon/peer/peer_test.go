package peer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

const testWait = 5 * time.Second

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

type pairCodeEvent struct{ deviceID, name, code, direction string }

type pairResultEvent struct {
	deviceID string
	paired   bool
	reason   string
}

type peerStateEvent struct{ deviceID, state, message string }

// recorder is a Handler that funnels every callback into a channel. Sends are
// non-blocking: a test that stops draining must never wedge the daemon.
type recorder struct {
	codes   chan pairCodeEvent
	results chan pairResultEvent
	states  chan peerStateEvent
	signals chan protocol.PeerSignalMessage
}

func newRecorder() *recorder {
	return &recorder{
		codes:   make(chan pairCodeEvent, 32),
		results: make(chan pairResultEvent, 32),
		states:  make(chan peerStateEvent, 64),
		signals: make(chan protocol.PeerSignalMessage, 32),
	}
}

func (r *recorder) OnPairCode(deviceID, name, code, direction string) {
	select {
	case r.codes <- pairCodeEvent{deviceID, name, code, direction}:
	default:
	}
}

func (r *recorder) OnPairResult(deviceID string, paired bool, reason string) {
	select {
	case r.results <- pairResultEvent{deviceID, paired, reason}:
	default:
	}
}

func (r *recorder) OnPeerState(deviceID, state, message string) {
	select {
	case r.states <- peerStateEvent{deviceID, state, message}:
	default:
	}
}

func (r *recorder) OnSignal(msg protocol.PeerSignalMessage) {
	select {
	case r.signals <- msg:
	default:
	}
}

func (r *recorder) code(t *testing.T) pairCodeEvent {
	t.Helper()
	select {
	case ev := <-r.codes:
		return ev
	case <-time.After(testWait):
		t.Fatal("timed out waiting for a pair:code callback")
		return pairCodeEvent{}
	}
}

func (r *recorder) result(t *testing.T) pairResultEvent {
	t.Helper()
	select {
	case ev := <-r.results:
		return ev
	case <-time.After(testWait):
		t.Fatal("timed out waiting for a pair:result callback")
		return pairResultEvent{}
	}
}

func (r *recorder) signal(t *testing.T) protocol.PeerSignalMessage {
	t.Helper()
	select {
	case msg := <-r.signals:
		return msg
	case <-time.After(testWait):
		t.Fatal("timed out waiting for a relayed signal")
		return protocol.PeerSignalMessage{}
	}
}

type testPeer struct {
	*Manager
	self  protocol.SelfDevice
	trust *config.TrustStore
	rec   *recorder
}

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPeer starts a manager on an ephemeral loopback port. Tests must never
// bind protocol.DefaultPeerPort on 0.0.0.0: that would collide with a running
// daemon and, on Windows, raise a firewall prompt in the middle of a test run.
func newTestPeer(t *testing.T, name string, pairTimeout time.Duration) *testPeer {
	t.Helper()

	logger := testLogger()
	trust, err := config.LoadTrustStoreAt(logger, filepath.Join(t.TempDir(), "trusted.json"))
	if err != nil {
		t.Fatalf("load trust store: %v", err)
	}

	self := protocol.SelfDevice{
		ID:       uuid.NewString(),
		Name:     name,
		Version:  protocol.AppVersion,
		Platform: protocol.PlatformLinux,
	}
	rec := newRecorder()

	m, err := New(Options{
		Self:        self,
		Trust:       trust,
		Handler:     rec,
		Logger:      logger,
		PairTimeout: pairTimeout,
		BindHost:    "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	m.port = 0 // let the OS pick the port

	if err := m.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("manager %s run: %v", name, err)
			}
		case <-time.After(testWait):
			t.Errorf("manager %s did not shut down", name)
		}
	})

	return &testPeer{Manager: m, self: self, trust: trust, rec: rec}
}

// device renders the manager as the Device a discovery snapshot would carry.
func (p *testPeer) device() protocol.Device {
	host, portStr, err := net.SplitHostPort(p.Addr())
	if err != nil {
		panic(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		panic(err)
	}
	return protocol.Device{
		ID:       p.self.ID,
		Name:     p.self.Name,
		Address:  host,
		Port:     port,
		Platform: p.self.Platform,
		Version:  p.self.Version,
		Status:   protocol.StatusOnline,
		LastSeen: time.Now().UnixMilli(),
	}
}

// rawDial opens a bare WebSocket to the peer port, bypassing the Manager
// entirely, so a test can speak whatever a hostile peer might speak.
func rawDial(t *testing.T, ctx context.Context, p *testPeer) *websocket.Conn {
	t.Helper()
	ws, resp, err := websocket.Dial(ctx, "ws://"+p.Addr()+protocol.PeerWebSocketPath, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial peer port: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func writeFrame(t *testing.T, ctx context.Context, ws *websocket.Conn, msg any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, testWait)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func readFrame(t *testing.T, ctx context.Context, ws *websocket.Conn) (protocol.Envelope, []byte) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, testWait)
	defer cancel()

	typ, data, err := ws.Read(rctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("expected a text frame, got %v", typ)
	}
	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode envelope from %s: %v", data, err)
	}
	return env, data
}

// expectClose drains frames until the socket closes and asserts the close code.
func expectClose(t *testing.T, ctx context.Context, ws *websocket.Conn, want int) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, testWait)
	defer cancel()

	for i := 0; i < 8; i++ {
		_, _, err := ws.Read(rctx)
		if err == nil {
			continue
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusCode(want) {
			t.Fatalf("close status = %d, want %d (error: %v)", got, want, err)
		}
		return
	}
	t.Fatalf("socket did not close with status %d", want)
}

func helloFrom(self protocol.SelfDevice, token string) protocol.PeerHelloMessage {
	return protocol.NewPeerHello(self, token)
}

func strangerDevice(name string) protocol.SelfDevice {
	return protocol.SelfDevice{
		ID:       uuid.NewString(),
		Name:     name,
		Version:  protocol.AppVersion,
		Platform: protocol.PlatformDarwin,
	}
}

// -----------------------------------------------------------------------------
// Handshake
// -----------------------------------------------------------------------------

// A socket that opens with anything but peer:hello is closed. Accepting other
// frames first would expose pre-authentication code to the whole LAN.
func TestHandshakeRejectsNonHelloFirstFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", time.Second)

	for _, first := range []any{
		protocol.NewPeerSignalOffer(uuid.NewString(), "v=0"),
		protocol.NewPeerPairAccept(uuid.NewString()),
		protocol.PingMessage{Type: protocol.TypePing},
		map[string]any{"type": "peer:hello but not really"},
	} {
		ws := rawDial(t, ctx, host)
		writeFrame(t, ctx, ws, first)
		expectClose(t, ctx, ws, protocol.PeerCloseBadFrame)
	}
}

// A frame that is not JSON at all must not reach a decoder that expects one.
func TestHandshakeRejectsGarbageFirstFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", time.Second)

	ws := rawDial(t, ctx, host)
	wctx, cancel := context.WithTimeout(ctx, testWait)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, []byte("not json at all")); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectClose(t, ctx, ws, protocol.PeerCloseBadFrame)
}

// The peer plane carries signalling JSON only. A binary frame is how file bytes
// would arrive if anything ever tried to push them through the daemon.
func TestHandshakeRejectsBinaryFrame(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", time.Second)

	ws := rawDial(t, ctx, host)
	wctx, cancel := context.WithTimeout(ctx, testWait)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageBinary, []byte{0x4e, 0x53, 0x43, 0x31}); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectClose(t, ctx, ws, protocol.PeerCloseBadFrame)
}

// A peer speaking another protocol version is told so and closed, rather than
// being dragged through a pairing that could never work.
func TestHandshakeRejectsProtocolMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", time.Second)

	ws := rawDial(t, ctx, host)
	hello := helloFrom(strangerDevice("old"), "")
	hello.ProtocolVersion = protocol.ProtocolVersionV1
	writeFrame(t, ctx, ws, hello)

	env, data := readFrame(t, ctx, ws)
	if env.Type != protocol.TypePeerPairReject {
		t.Fatalf("type = %q, want %q", env.Type, protocol.TypePeerPairReject)
	}
	var reject protocol.PeerPairRejectMessage
	if err := json.Unmarshal(data, &reject); err != nil {
		t.Fatalf("decode reject: %v", err)
	}
	if reject.Reason != protocol.PairReasonProtocolMismatch {
		t.Fatalf("reason = %q, want %q", reject.Reason, protocol.PairReasonProtocolMismatch)
	}
	expectClose(t, ctx, ws, protocol.PeerCloseProtocolMismatch)
}

// An unknown device gets the pairing path and nothing else, and the code it is
// told to display is the one both ends derive independently.
func TestUnknownDeviceIsForcedIntoPairing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", 2*time.Second)
	stranger := strangerDevice("stranger")

	ws := rawDial(t, ctx, host)
	writeFrame(t, ctx, ws, helloFrom(stranger, ""))

	env, data := readFrame(t, ctx, ws)
	if env.Type != protocol.TypePeerPairRequired {
		t.Fatalf("type = %q, want %q", env.Type, protocol.TypePeerPairRequired)
	}
	var required protocol.PeerPairRequiredMessage
	if err := json.Unmarshal(data, &required); err != nil {
		t.Fatalf("decode pair:required: %v", err)
	}
	if err := required.Validate(); err != nil {
		t.Fatalf("pair:required is invalid: %v", err)
	}
	if !protocol.ValidNonceFormat(required.Nonce) {
		t.Fatalf("nonce %q is not 32 lowercase hex", required.Nonce)
	}

	want := protocol.Code(host.self.ID, stranger.ID, required.Nonce)
	if required.Code != want {
		t.Fatalf("code = %q, want the derived %q", required.Code, want)
	}

	// The human on this side is asked, with the same six digits.
	ev := host.rec.code(t)
	if ev.deviceID != stranger.ID || ev.code != want || ev.direction != protocol.PairDirectionIncoming {
		t.Fatalf("OnPairCode = %+v, want deviceId %s, code %s, direction %s",
			ev, stranger.ID, want, protocol.PairDirectionIncoming)
	}

	// Nothing but pairing frames is accepted while it waits.
	writeFrame(t, ctx, ws, protocol.NewPeerSignalOffer(stranger.ID, "v=0"))
	expectClose(t, ctx, ws, protocol.PeerCloseBadFrame)
}

// A stored device presenting the wrong token is treated exactly like a
// stranger: no peer:ready, no signal relay, pairing only.
func TestTokenMismatchIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", 2*time.Second)

	known := strangerDevice("known")
	good, err := config.NewToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if _, err := host.trust.Add(known.ID, known.Name, good); err != nil {
		t.Fatalf("add trusted peer: %v", err)
	}
	wrong, err := config.NewToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	cases := []struct {
		name  string
		self  protocol.SelfDevice
		token string
		want  string
	}{
		{"correct token", known, good, protocol.TypePeerReady},
		{"wrong token", known, wrong, protocol.TypePeerPairRequired},
		{"no token", known, "", protocol.TypePeerPairRequired},
		{"unknown device with a well-formed token", strangerDevice("nobody"), good, protocol.TypePeerPairRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := rawDial(t, ctx, host)
			writeFrame(t, ctx, ws, helloFrom(tc.self, tc.token))
			env, data := readFrame(t, ctx, ws)
			if env.Type != tc.want {
				t.Fatalf("type = %q, want %q (frame %s)", env.Type, tc.want, data)
			}
			if env.Type == protocol.TypePeerReady {
				var ready protocol.PeerReadyMessage
				if err := json.Unmarshal(data, &ready); err != nil {
					t.Fatalf("decode ready: %v", err)
				}
				if ready.DeviceID != host.self.ID {
					t.Fatalf("ready deviceId = %q, want %q", ready.DeviceID, host.self.ID)
				}
			}
			_ = ws.Close(websocket.StatusNormalClosure, "")
		})
	}

	// A refused token must never have promoted the socket: the trust store
	// still holds exactly the one original entry, with its original token.
	if got, ok := host.trust.Get(known.ID); !ok || !config.TokensEqual(got.Token, good) {
		t.Fatal("the stored token changed during a failed handshake")
	}
}

// The pairing code cannot be brute-forced from the LAN: the sixth attempt in a
// window is refused before a code is even derived.
func TestPairingIsRateLimitedPerSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", 300*time.Millisecond)

	for i := 0; i < protocol.PeerPairAttemptsPerWindow; i++ {
		ws := rawDial(t, ctx, host)
		writeFrame(t, ctx, ws, helloFrom(strangerDevice("guesser"), ""))
		env, _ := readFrame(t, ctx, ws)
		if env.Type != protocol.TypePeerPairRequired {
			t.Fatalf("attempt %d: type = %q, want %q", i+1, env.Type, protocol.TypePeerPairRequired)
		}
		_ = ws.CloseNow()
	}

	ws := rawDial(t, ctx, host)
	writeFrame(t, ctx, ws, helloFrom(strangerDevice("guesser"), ""))
	env, data := readFrame(t, ctx, ws)
	if env.Type != protocol.TypePeerPairReject {
		t.Fatalf("type = %q, want %q", env.Type, protocol.TypePeerPairReject)
	}
	var reject protocol.PeerPairRejectMessage
	if err := json.Unmarshal(data, &reject); err != nil {
		t.Fatalf("decode reject: %v", err)
	}
	if reject.Reason != protocol.PairReasonRateLimited {
		t.Fatalf("reason = %q, want %q", reject.Reason, protocol.PairReasonRateLimited)
	}
	expectClose(t, ctx, ws, protocol.PeerCloseRateLimited)
}

// A browser cannot drive the peer plane. WebSocket handshakes are exempt from
// the same-origin policy, so an Origin header is refused outright.
func TestPeerListenerRejectsBrowserOrigins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", time.Second)

	_, resp, err := websocket.Dial(ctx, "ws://"+host.Addr()+protocol.PeerWebSocketPath, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Origin": {"https://evil.example"}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("a browser-originated upgrade was accepted")
	}
}

// -----------------------------------------------------------------------------
// End to end pairing between two daemons
// -----------------------------------------------------------------------------

func TestPairingEndToEndAndSignalRelay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	alice := newTestPeer(t, "alice", 10*time.Second)
	bob := newTestPeer(t, "bob", 10*time.Second)

	if err := alice.StartPairing(ctx, bob.device()); err != nil {
		t.Fatalf("start pairing: %v", err)
	}

	outgoing := alice.rec.code(t)
	incoming := bob.rec.code(t)

	if outgoing.direction != protocol.PairDirectionOutgoing {
		t.Fatalf("alice direction = %q, want outgoing", outgoing.direction)
	}
	if incoming.direction != protocol.PairDirectionIncoming {
		t.Fatalf("bob direction = %q, want incoming", incoming.direction)
	}
	// Both humans must see the same six digits, derived independently.
	if outgoing.code != incoming.code {
		t.Fatalf("codes differ: alice %q, bob %q", outgoing.code, incoming.code)
	}
	if !protocol.ValidCodeFormat(outgoing.code) {
		t.Fatalf("code %q is not six digits", outgoing.code)
	}
	if outgoing.deviceID != bob.self.ID || incoming.deviceID != alice.self.ID {
		t.Fatalf("codes attributed to the wrong devices: %+v / %+v", outgoing, incoming)
	}

	// Nothing is trusted until both humans have clicked.
	if alice.trust.IsPaired(bob.self.ID) || bob.trust.IsPaired(alice.self.ID) {
		t.Fatal("a device was trusted before anyone accepted")
	}

	if err := bob.Confirm(PairDecision{DeviceID: alice.self.ID, Accept: true}); err != nil {
		t.Fatalf("bob confirm: %v", err)
	}
	if err := alice.Confirm(PairDecision{DeviceID: bob.self.ID, Accept: true}); err != nil {
		t.Fatalf("alice confirm: %v", err)
	}

	if res := alice.rec.result(t); !res.paired || res.deviceID != bob.self.ID {
		t.Fatalf("alice result = %+v, want paired with bob", res)
	}
	if res := bob.rec.result(t); !res.paired || res.deviceID != alice.self.ID {
		t.Fatalf("bob result = %+v, want paired with alice", res)
	}

	// Both ends stored the same token, under each other's device id.
	aliceSide, ok := alice.trust.Get(bob.self.ID)
	if !ok {
		t.Fatal("alice did not store bob")
	}
	bobSide, ok := bob.trust.Get(alice.self.ID)
	if !ok {
		t.Fatal("bob did not store alice")
	}
	if !config.TokensEqual(aliceSide.Token, bobSide.Token) {
		t.Fatal("the two ends stored different tokens")
	}
	if !protocol.ValidTokenFormat(aliceSide.Token) {
		t.Fatalf("token %q is not 64 lowercase hex", aliceSide.Token)
	}

	if !alice.IsConnected(bob.self.ID) || !bob.IsConnected(alice.self.ID) {
		t.Fatal("the paired socket was not kept in the registry")
	}

	// The socket now carries signalling, in both directions, without a redial.
	offer := protocol.NewPeerSignalOffer(alice.self.ID, "v=0 offer")
	if err := alice.SendSignal(ctx, bob.self.ID, offer); err != nil {
		t.Fatalf("alice send offer: %v", err)
	}
	got := bob.rec.signal(t)
	if got.Kind != protocol.SignalKindOffer || got.SDP != "v=0 offer" || got.From != alice.self.ID {
		t.Fatalf("bob received %+v, want alice's offer", got)
	}

	answer := protocol.NewPeerSignalAnswer(bob.self.ID, "v=0 answer")
	if err := bob.SendSignal(ctx, alice.self.ID, answer); err != nil {
		t.Fatalf("bob send answer: %v", err)
	}
	got = alice.rec.signal(t)
	if got.Kind != protocol.SignalKindAnswer || got.SDP != "v=0 answer" || got.From != bob.self.ID {
		t.Fatalf("alice received %+v, want bob's answer", got)
	}

	ice := protocol.NewPeerSignalICE(alice.self.ID, "candidate:1 1 udp 2 10.0.0.1 5000 typ host", "0", 0)
	if err := alice.SendSignal(ctx, bob.self.ID, ice); err != nil {
		t.Fatalf("alice send ice: %v", err)
	}
	got = bob.rec.signal(t)
	if got.Kind != protocol.SignalKindICE || got.Candidate == "" {
		t.Fatalf("bob received %+v, want an ice candidate", got)
	}

	// Forgetting drops the trust and the socket with it.
	if err := alice.Forget(bob.self.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if alice.trust.IsPaired(bob.self.ID) {
		t.Fatal("device still trusted after Forget")
	}
	if err := alice.SendSignal(ctx, bob.self.ID, offer); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("SendSignal after Forget = %v, want ErrNotPaired", err)
	}
}

// Silence is not consent: an unanswered pairing expires as a rejection on both
// ends, and never leaves a trusted device behind.
func TestPairingTimesOutAsRejection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	alice := newTestPeer(t, "alice", 400*time.Millisecond)
	bob := newTestPeer(t, "bob", 400*time.Millisecond)

	if err := alice.StartPairing(ctx, bob.device()); err != nil {
		t.Fatalf("start pairing: %v", err)
	}
	alice.rec.code(t)
	bob.rec.code(t)

	res := alice.rec.result(t)
	if res.paired {
		t.Fatalf("alice paired without an answer: %+v", res)
	}
	if res.reason != protocol.PairReasonTimeout {
		t.Fatalf("alice reason = %q, want %q", res.reason, protocol.PairReasonTimeout)
	}

	res = bob.rec.result(t)
	if res.paired {
		t.Fatalf("bob paired without an answer: %+v", res)
	}

	if alice.trust.Len() != 0 || bob.trust.Len() != 0 {
		t.Fatal("an expired pairing wrote to a trust store")
	}

	// The expired pairing is gone, so a late click cannot revive it.
	if err := alice.Confirm(PairDecision{DeviceID: bob.self.ID, Accept: true}); !errors.Is(err, ErrNoPendingPairing) {
		t.Fatalf("late Confirm = %v, want ErrNoPendingPairing", err)
	}
	if alice.trust.IsPaired(bob.self.ID) {
		t.Fatal("a late accept paired the device anyway")
	}
}

// One human declining is enough to fail the pairing on both ends.
func TestPairingDeclineFailsBothEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	alice := newTestPeer(t, "alice", 10*time.Second)
	bob := newTestPeer(t, "bob", 10*time.Second)

	if err := alice.StartPairing(ctx, bob.device()); err != nil {
		t.Fatalf("start pairing: %v", err)
	}
	alice.rec.code(t)
	bob.rec.code(t)

	if err := alice.Confirm(PairDecision{DeviceID: bob.self.ID, Accept: true}); err != nil {
		t.Fatalf("alice confirm: %v", err)
	}
	if err := bob.Confirm(PairDecision{
		DeviceID: alice.self.ID,
		Accept:   false,
		Reason:   protocol.PairReasonDeclined,
	}); err != nil {
		t.Fatalf("bob decline: %v", err)
	}

	for _, p := range []*testPeer{alice, bob} {
		res := p.rec.result(t)
		if res.paired {
			t.Fatalf("%s paired despite a decline: %+v", p.self.Name, res)
		}
		if res.reason != protocol.PairReasonDeclined {
			t.Fatalf("%s reason = %q, want %q", p.self.Name, res.reason, protocol.PairReasonDeclined)
		}
	}
	if alice.trust.Len() != 0 || bob.trust.Len() != 0 {
		t.Fatal("a declined pairing wrote to a trust store")
	}
}

// A second pairing with the same device is refused rather than putting two sets
// of six digits on screen for one peer.
func TestStartPairingRejectsDuplicates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	alice := newTestPeer(t, "alice", 5*time.Second)
	bob := newTestPeer(t, "bob", 5*time.Second)

	if err := alice.StartPairing(ctx, bob.device()); err != nil {
		t.Fatalf("start pairing: %v", err)
	}
	if err := alice.StartPairing(ctx, bob.device()); !errors.Is(err, ErrPairInProgress) {
		t.Fatalf("second StartPairing = %v, want ErrPairInProgress", err)
	}
}

// -----------------------------------------------------------------------------
// Control-plane surface
// -----------------------------------------------------------------------------

func TestConfirmWithoutPairingIsRejected(t *testing.T) {
	t.Parallel()
	host := newTestPeer(t, "host", time.Second)

	if err := host.Confirm(PairDecision{DeviceID: uuid.NewString(), Accept: true}); !errors.Is(err, ErrNoPendingPairing) {
		t.Fatalf("Confirm = %v, want ErrNoPendingPairing", err)
	}
	if err := host.Confirm(PairDecision{Accept: true}); !errors.Is(err, ErrNoPendingPairing) {
		t.Fatalf("Confirm with no device id = %v, want ErrNoPendingPairing", err)
	}
}

func TestSendSignalRequiresPairingAndSignalFrames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := newTestPeer(t, "host", time.Second)
	other := uuid.NewString()

	err := host.SendSignal(ctx, other, protocol.NewPeerSignalOffer(host.self.ID, "v=0"))
	if !errors.Is(err, ErrNotPaired) {
		t.Fatalf("SendSignal to a stranger = %v, want ErrNotPaired", err)
	}

	// Trusted but never seen on the LAN: unreachable, not a panic.
	token, err := config.NewToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if _, err := host.trust.Add(other, "ghost", token); err != nil {
		t.Fatalf("add trusted peer: %v", err)
	}
	if err := host.SendSignal(ctx, other, protocol.NewPeerSignalOffer(host.self.ID, "v=0")); !errors.Is(err, ErrPeerUnreachable) {
		t.Fatalf("SendSignal to an unreachable peer = %v, want ErrPeerUnreachable", err)
	}

	// The relay carries signalling and nothing else. A frame of any other type
	// is refused before it can reach the socket.
	notASignal := protocol.PeerSignalMessage{Type: protocol.TypeTransferStart, From: host.self.ID, Kind: protocol.SignalKindOffer, SDP: "v=0"}
	if err := host.SendSignal(ctx, other, notASignal); err == nil {
		t.Fatal("a non-signal frame was accepted by the relay")
	}

	// And it speaks only for this daemon.
	impersonation := protocol.NewPeerSignalOffer(uuid.NewString(), "v=0")
	if err := host.SendSignal(ctx, other, impersonation); err == nil {
		t.Fatal("a signal attributed to another device was accepted")
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()
	logger := testLogger()
	trust, err := config.LoadTrustStoreAt(logger, filepath.Join(t.TempDir(), "trusted.json"))
	if err != nil {
		t.Fatalf("load trust store: %v", err)
	}
	self := strangerDevice("self")
	rec := newRecorder()

	cases := map[string]Options{
		"no trust store": {Self: self, Handler: rec},
		"no handler":     {Self: self, Trust: trust},
		"no identity":    {Trust: trust, Handler: rec},
		"bad port":       {Self: self, Trust: trust, Handler: rec, Port: 70000},
	}
	for name, opts := range cases {
		if _, err := New(opts); err == nil {
			t.Errorf("New(%s) succeeded, want an error", name)
		}
	}
}

func TestRememberIgnoresSelfAndBlankAddresses(t *testing.T) {
	t.Parallel()
	host := newTestPeer(t, "host", time.Second)

	host.Remember(protocol.Device{ID: host.self.ID, Address: "10.0.0.9", Port: 8766})
	host.Remember(protocol.Device{ID: uuid.NewString()})
	if _, ok := host.addrFor(host.self.ID); ok {
		t.Fatal("the manager remembered an address for itself")
	}

	other := uuid.NewString()
	host.Remember(protocol.Device{ID: other, Address: "10.0.0.9", Status: protocol.StatusOnline})
	addr, ok := host.addrFor(other)
	if !ok {
		t.Fatal("address not remembered")
	}
	if want := net.JoinHostPort("10.0.0.9", strconv.Itoa(protocol.DefaultPeerPort)); addr != want {
		t.Fatalf("addr = %q, want %q (default peer port when none is advertised)", addr, want)
	}
}
