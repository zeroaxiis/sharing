package peer

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/coder/websocket"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// handlePeer upgrades an inbound LAN connection on /peer.
//
// Everything that reaches this function came off the network from a machine
// this daemon has never necessarily met, so the guards run before a single
// protocol frame is read.
func (m *Manager) handlePeer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A daemon never sends an Origin header; a browser always does. The
	// same-origin policy does not apply to WebSocket handshakes, so without
	// this check any web page open on any machine on the LAN could script the
	// pairing exchange against every device on the network and hope somebody
	// clicks accept. Peers are programs, not pages: refuse anything that
	// announces itself as one.
	if origin := r.Header.Get("Origin"); origin != "" {
		m.log.Warn("rejected browser-originated peer connection",
			"origin", origin, "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Bound how many unauthenticated sockets can be mid-handshake. Each one
	// costs a goroutine, a file descriptor and a read buffer, and none of them
	// has proved anything yet.
	if !m.acquireHandshake() {
		m.log.Warn("too many peer handshakes in flight, refusing connection", "remote", r.RemoteAddr)
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	release := sync.OnceFunc(m.releaseHandshake)

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		release()
		m.log.Debug("peer websocket upgrade failed", "remote", r.RemoteAddr, "error", err)
		return
	}

	c := newConn(m, m.base(), ws, false, r.RemoteAddr)
	go c.writeLoop()
	m.serveInbound(c, release)
}

// serveInbound runs the responder half of the handshake and then the frame
// loop. It is the single reader goroutine for this socket.
func (m *Manager) serveInbound(c *conn, release func()) {
	defer m.teardown(c)
	defer release()

	hello, ok := m.readHello(c)
	if !ok {
		return
	}
	c.setRemote(hello.DeviceID, hello.Name)

	if hello.DeviceID == m.self.ID {
		// Either a loop back to ourselves or a peer claiming our identity.
		// Neither is worth a socket.
		c.log.Warn("peer claims this device's own id", "deviceId", hello.DeviceID)
		c.closeWith(protocol.PeerCloseUnauthorized, "device id collision")
		return
	}

	// The single authentication decision on this plane. VerifyToken compares in
	// constant time and treats an unknown device exactly like a wrong token, so
	// nothing here reveals which devices this machine has paired with. There is
	// no other branch that can reach the signal relay.
	if m.trust.VerifyToken(hello.DeviceID, hello.Token) {
		m.acceptAuthenticated(c, hello, release)
		return
	}

	if hello.Token != "" {
		// A token that does not verify is either a stale pairing or an attack.
		// The answer is the same either way, and it deliberately does not say
		// which: the peer is offered pairing, like any stranger.
		c.log.Warn("peer presented a token that did not verify",
			"deviceId", hello.DeviceID, "name", hello.Name)
	}

	m.offerPairing(c, hello, release)
}

// readHello reads and validates the mandatory first frame.
func (m *Manager) readHello(c *conn) (protocol.PeerHelloMessage, bool) {
	var hello protocol.PeerHelloMessage

	data, err := c.readFrame(handshakeTimeout)
	if err != nil {
		if errors.Is(err, errBinaryFrame) {
			c.log.Warn("peer opened with a binary frame; the peer plane carries signalling only")
		} else {
			c.log.Debug("peer handshake read failed", "error", err)
		}
		c.closeWith(protocol.PeerCloseBadFrame, "expected peer:hello")
		return hello, false
	}

	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		c.log.Debug("peer handshake frame is not a protocol envelope", "error", err)
		c.closeWith(protocol.PeerCloseBadFrame, "first frame must be peer:hello")
		return hello, false
	}
	// Nothing but peer:hello opens a socket. Accepting anything else, even to
	// answer it with an error, would give an unknown peer a way to reach code
	// that has no business running before authentication.
	if env.Type != protocol.TypePeerHello {
		c.log.Warn("first peer frame was not peer:hello", "type", env.Type)
		c.closeWith(protocol.PeerCloseBadFrame, "first frame must be peer:hello")
		return hello, false
	}

	if err := json.Unmarshal(data, &hello); err != nil {
		c.log.Debug("malformed peer:hello", "error", err)
		c.closeWith(protocol.PeerCloseBadFrame, "malformed peer:hello")
		return hello, false
	}
	if hello.ProtocolVersion != protocol.ProtocolVersion {
		c.log.Info("peer speaks a different protocol version",
			"theirs", hello.ProtocolVersion, "ours", protocol.ProtocolVersion)
		_ = c.enqueue(protocol.NewPeerPairReject(protocol.PairReasonProtocolMismatch))
		c.closeWith(protocol.PeerCloseProtocolMismatch, "protocol version mismatch")
		return hello, false
	}
	if err := hello.Validate(); err != nil {
		c.log.Debug("invalid peer:hello", "error", err)
		c.closeWith(protocol.PeerCloseBadFrame, "invalid peer:hello")
		return hello, false
	}
	return hello, true
}

// acceptAuthenticated answers a verified peer:hello with peer:ready and hands
// the socket to the signal relay.
func (m *Manager) acceptAuthenticated(c *conn, hello protocol.PeerHelloMessage, release func()) {
	if err := c.enqueue(protocol.NewPeerReady(m.self)); err != nil {
		return
	}
	c.paired.Store(true)
	m.registerConn(c)
	release()

	m.rememberInbound(c)
	if err := m.trust.Touch(hello.DeviceID); err != nil {
		c.log.Debug("touch trusted peer", "deviceId", hello.DeviceID, "error", err)
	}
	if hello.Name != "" {
		// A device that was renamed since pairing should not show up under its
		// old name forever.
		if err := m.trust.Rename(hello.DeviceID, hello.Name); err != nil {
			c.log.Debug("rename trusted peer", "deviceId", hello.DeviceID, "error", err)
		}
	}

	c.log.Info("authenticated peer connected", "deviceId", hello.DeviceID, "name", hello.Name)
	m.handler.OnPeerState(hello.DeviceID, protocol.PeerStateOnline, "")
	m.frameLoop(c)
}

// offerPairing answers an unauthenticated peer:hello with peer:pair:required
// and drives the responder half of the pairing exchange.
func (m *Manager) offerPairing(c *conn, hello protocol.PeerHelloMessage, release func()) {
	// Rate limit by source address, before any state is created for this
	// attempt. Five a minute is the difference between a six-digit code that
	// takes centuries to guess and one that falls in an afternoon.
	source := sourceKey(c.remoteAddr)
	if !m.limiter.allow(source) {
		c.log.Warn("pairing rate limit exceeded", "source", source, "deviceId", hello.DeviceID)
		_ = c.enqueue(protocol.NewPeerPairReject(protocol.PairReasonRateLimited))
		c.closeWith(protocol.PeerCloseRateLimited, "too many pairing attempts")
		return
	}

	// The responder chooses the nonce, so a dialer cannot replay an old one to
	// make two screens show a code it has seen before.
	nonce, err := protocol.NewNonce()
	if err != nil {
		m.log.Error("generate pairing nonce", "error", err)
		_ = c.enqueue(protocol.NewPeerPairReject(protocol.PairReasonInternal))
		c.closeWith(protocol.PeerClosePairRejected, "internal error")
		return
	}
	code := protocol.Code(m.self.ID, hello.DeviceID, nonce)

	p := newPairing(hello.DeviceID, hello.Name, protocol.PairDirectionIncoming, true)
	p.code = code
	p.setConn(c)
	if err := m.beginPairing(p); err != nil {
		c.log.Info("refusing a second pairing with the same device", "deviceId", hello.DeviceID)
		_ = c.enqueue(protocol.NewPeerPairReject(protocol.PairReasonBusy))
		c.closeWith(protocol.PeerClosePairRejected, "a pairing with this device is already in progress")
		return
	}

	if err := c.enqueue(protocol.NewPeerPairRequired(code, nonce)); err != nil {
		m.endPairing(p)
		return
	}
	release()

	c.log.Info("pairing requested by peer", "deviceId", hello.DeviceID, "name", hello.Name)
	m.handler.OnPeerState(hello.DeviceID, protocol.PeerStatePairing, "")
	m.handler.OnPairCode(hello.DeviceID, hello.Name, code, protocol.PairDirectionIncoming)

	go m.runPairing(m.base(), p)
	m.frameLoop(c)
}

// teardown drops a socket and reports the peer offline if it was the live one.
func (m *Manager) teardown(c *conn) {
	wasLive := m.unregisterConn(c)
	c.hardClose()
	if wasLive {
		if id := c.deviceID(); id != "" {
			m.handler.OnPeerState(id, protocol.PeerStateOffline, "")
		}
	}
}

// rememberInbound records a fallback address for a peer that has only ever
// dialled us.
//
// peer:hello carries no port, so the best guess for a device we have not seen
// over mDNS is its source address on the default peer port. It is only ever a
// hint: the token handshake still has to succeed, so a wrong guess costs one
// failed dial. A real mDNS record always wins, because Remember overwrites it.
func (m *Manager) rememberInbound(c *conn) {
	id := c.deviceID()
	if id == "" {
		return
	}
	if _, known := m.addrFor(id); known {
		return
	}
	host, _, err := net.SplitHostPort(c.remoteAddr)
	if err != nil {
		return
	}
	m.rememberAddr(id, net.JoinHostPort(host, strconv.Itoa(protocol.DefaultPeerPort)))
}

// sourceKey reduces a remote address to the host part, so the rate limit counts
// a machine rather than a TCP connection: the ephemeral port changes on every
// attempt and would make the limit meaningless.
func sourceKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
