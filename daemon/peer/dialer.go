package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/coder/websocket"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// StartPairing dials a discovered device and drives the pairing exchange from
// the initiating side. It returns as soon as the attempt is registered;
// everything after that arrives through the Handler.
//
// ctx is only checked for an early cancellation. The pairing itself outlives
// any single control-plane request - it is waiting on two humans - so it hangs
// off the manager's own lifetime instead.
func (m *Manager) StartPairing(ctx context.Context, dev protocol.Device) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dev.ID == "" {
		return errors.New("peer: StartPairing requires a device id")
	}
	if dev.ID == m.self.ID {
		return errors.New("peer: refusing to pair a device with itself")
	}
	if dev.Address == "" {
		return fmt.Errorf("%w: no address for %s", ErrPeerUnreachable, dev.ID)
	}

	port := dev.Port
	if port == 0 {
		port = protocol.DefaultPeerPort
	}
	addr := net.JoinHostPort(dev.Address, strconv.Itoa(port))
	m.rememberAddr(dev.ID, addr)

	// Registered before the dial so two clicks in quick succession cannot start
	// two pairings, each showing its own six digits for the same peer.
	p := newPairing(dev.ID, dev.Name, protocol.PairDirectionOutgoing, false)
	if err := m.beginPairing(p); err != nil {
		return err
	}

	go m.pairOutbound(m.base(), p, addr)
	return nil
}

// pairOutbound runs the dialer half of the pairing exchange.
func (m *Manager) pairOutbound(ctx context.Context, p *pairing, addr string) {
	m.handler.OnPeerState(p.deviceID, protocol.PeerStatePairing, "")

	// Deliberately no token, even if one is stored. Pairing is a fresh trust
	// decision by two humans; presenting an old token here would silently
	// re-use the previous one and skip them both.
	c, err := m.dial(ctx, addr, p.deviceID, p.name, "")
	if err != nil {
		m.log.Info("could not reach peer for pairing",
			"deviceId", p.deviceID, "addr", addr, "error", err)
		m.abortPairing(p, protocol.PairReasonUnreachable, false)
		return
	}
	p.setConn(c)
	defer m.teardown(c)

	data, err := c.readFrame(handshakeTimeout)
	if err != nil {
		m.log.Debug("no answer to peer:hello", "deviceId", p.deviceID, "error", err)
		m.abortPairing(p, protocol.PairReasonUnreachable, false)
		return
	}
	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		m.abortPairing(p, protocol.PairReasonInternal, true)
		return
	}

	switch env.Type {
	case protocol.TypePeerPairRequired:
		var msg protocol.PeerPairRequiredMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			m.abortPairing(p, protocol.PairReasonInternal, true)
			return
		}
		if err := msg.Validate(); err != nil {
			c.log.Warn("invalid peer:pair:required", "deviceId", p.deviceID, "error", err)
			m.abortPairing(p, protocol.PairReasonInternal, true)
			return
		}

		// Derive the code locally from the nonce and the two device ids. The
		// code that arrived on the wire is never displayed and never trusted:
		// if it were, a machine in the middle could make both screens agree on
		// digits it chose, which is exactly what the human check exists to
		// prevent.
		code := protocol.Code(m.self.ID, p.deviceID, msg.Nonce)
		if msg.Code != "" && msg.Code != code {
			c.log.Warn("peer's pairing code does not match the derived one; refusing to show either",
				"deviceId", p.deviceID)
			m.abortPairing(p, protocol.PairReasonCodeMismatch, true)
			return
		}
		p.code = code

		c.log.Info("pairing with peer", "deviceId", p.deviceID, "name", p.name)
		m.handler.OnPairCode(p.deviceID, p.name, code, protocol.PairDirectionOutgoing)

		go m.runPairing(ctx, p)
		m.frameLoop(c)

	case protocol.TypePeerPairReject:
		var msg protocol.PeerPairRejectMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			m.abortPairing(p, protocol.PairReasonInternal, false)
			return
		}
		reason := msg.Reason
		if reason == "" {
			reason = protocol.PairReasonDeclined
		}
		m.abortPairing(p, reason, false)

	case protocol.TypePeerReady:
		// We offered no token, so the peer had nothing to verify. A peer:ready
		// here means the far side skipped authentication entirely, which is a
		// broken or hostile implementation.
		c.log.Warn("peer answered a tokenless hello with peer:ready", "deviceId", p.deviceID)
		m.abortPairing(p, protocol.PairReasonInternal, true)

	default:
		c.log.Warn("unexpected answer to peer:hello", "deviceId", p.deviceID, "type", env.Type)
		m.abortPairing(p, protocol.PairReasonInternal, true)
	}
}

// abortPairing ends a pairing that never reached its driver goroutine.
func (m *Manager) abortPairing(p *pairing, reason string, notifyPeer bool) {
	m.endPairing(p)
	m.failPairing(p, reason, notifyPeer)
}

// dial opens a peer socket and sends peer:hello. An empty token asks for the
// pairing exchange; a stored token asks to be recognised.
func (m *Manager) dial(ctx context.Context, addr, remoteID, remoteName, token string) (*conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	//nolint:bodyclose // handled below; websocket.Dial returns the handshake response.
	ws, resp, err := websocket.Dial(dialCtx, "ws://"+addr+protocol.PeerWebSocketPath, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrPeerUnreachable, addr, err)
	}

	c := newConn(m, m.base(), ws, true, addr)
	c.setRemote(remoteID, remoteName)
	go c.writeLoop()

	if err := c.enqueue(protocol.NewPeerHello(m.self, token)); err != nil {
		c.hardClose()
		return nil, fmt.Errorf("%w: %s: %v", ErrPeerUnreachable, addr, err)
	}
	return c, nil
}

// ensureConn returns the live socket to a paired device, dialling and
// completing a token handshake if there is not one.
func (m *Manager) ensureConn(ctx context.Context, deviceID string) (*conn, error) {
	if c := m.liveConn(deviceID); c != nil {
		return c, nil
	}

	// Single-flight per device. Two signalling frames racing for the same
	// absent peer would otherwise open two sockets, and each would evict the
	// other from the registry.
	m.mu.Lock()
	if wait, running := m.dialing[deviceID]; running {
		m.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c := m.liveConn(deviceID); c != nil {
			return c, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrPeerUnreachable, deviceID)
	}
	done := make(chan struct{})
	m.dialing[deviceID] = done
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.dialing, deviceID)
		m.mu.Unlock()
		close(done)
	}()

	trusted, ok := m.trust.Get(deviceID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotPaired, deviceID)
	}
	addr, ok := m.addrFor(deviceID)
	if !ok {
		return nil, fmt.Errorf("%w: no known LAN address for %s", ErrPeerUnreachable, deviceID)
	}

	c, err := m.dial(ctx, addr, deviceID, trusted.Name, trusted.Token)
	if err != nil {
		return nil, err
	}

	data, err := c.readFrame(handshakeTimeout)
	if err != nil {
		c.hardClose()
		return nil, fmt.Errorf("%w: %s: %v", ErrPeerUnreachable, deviceID, err)
	}
	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		c.closeWith(protocol.PeerCloseBadFrame, "unreadable handshake response")
		return nil, fmt.Errorf("%w: %s: malformed handshake response", ErrPeerUnreachable, deviceID)
	}

	switch env.Type {
	case protocol.TypePeerReady:
		c.paired.Store(true)
		m.registerConn(c)
		go m.serveConn(c)
		if err := m.trust.Touch(deviceID); err != nil {
			c.log.Debug("touch trusted peer", "deviceId", deviceID, "error", err)
		}
		c.log.Info("connected to paired peer", "deviceId", deviceID, "addr", addr)
		m.handler.OnPeerState(deviceID, protocol.PeerStateOnline, "")
		return c, nil

	case protocol.TypePeerPairRequired:
		// The token was refused: that device forgot us, or something else now
		// answers on its address. The local trust entry is deliberately NOT
		// deleted here - doing so would let anyone who can answer on the peer's
		// address unpair two devices at will. Surface it and let a human pair
		// again.
		c.closeWith(protocol.PeerClosePairRequired, "peer requires pairing")
		m.handler.OnPeerState(deviceID, protocol.PeerStateError,
			"the peer no longer recognises this device; pair again")
		return nil, fmt.Errorf("%w: %s no longer trusts this device", ErrNotPaired, deviceID)

	case protocol.TypePeerPairReject:
		var msg protocol.PeerPairRejectMessage
		_ = json.Unmarshal(data, &msg)
		c.hardClose()
		return nil, fmt.Errorf("%w: %s refused the connection: %s",
			ErrPeerUnreachable, deviceID, protocol.NewPeerPairReject(msg.Reason).Reason)

	default:
		c.closeWith(protocol.PeerCloseBadFrame, "unexpected handshake response")
		return nil, fmt.Errorf("%w: %s answered peer:hello with %q", ErrPeerUnreachable, deviceID, env.Type)
	}
}
