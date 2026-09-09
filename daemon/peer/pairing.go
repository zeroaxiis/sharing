package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// pairEventKind is something the remote side (or a local cancellation) did to
// an in-flight pairing.
type pairEventKind int

const (
	// evAccept is a peer:pair:accept from the far side. It is one half of the
	// approval; the local human is the other half, and both are required.
	evAccept pairEventKind = iota
	// evReject is a peer:pair:reject from the far side.
	evReject
	// evPaired is the responder's peer:paired, carrying the issued token.
	evPaired
	// evCancelled is a local abort, such as the user forgetting the device
	// while its pairing is still on screen.
	evCancelled
)

type pairEvent struct {
	kind   pairEventKind
	reason string
	token  string
}

// pairing is one in-flight pairing attempt with one device, in either
// direction.
//
// Three things can happen to it: the local human answers (decision), the remote
// side does something (remote), or the clock runs out. A single driver
// goroutine owns the state machine and selects over all three, which is why
// none of the accept flags need a lock.
type pairing struct {
	deviceID  string
	name      string
	direction string
	// responder is true when this side received the peer:hello and therefore
	// mints the token once both humans have accepted.
	responder bool
	// code is the six digits both humans compare. It is derived locally on both
	// ends and never transmitted.
	code string
	// conn is the socket the pairing runs over. The dialer fills it in once the
	// connection is up, before the driver starts, and inbound frames are
	// checked against it so a straggler from a dying socket cannot be fed into
	// a different pairing with the same device.
	conn atomic.Pointer[conn]

	decision chan PairDecision
	remote   chan pairEvent
	done     chan struct{}
}

func newPairing(deviceID, name, direction string, responder bool) *pairing {
	return &pairing{
		deviceID:  deviceID,
		name:      name,
		direction: direction,
		responder: responder,
		// Buffered so Confirm never blocks on a driver that is momentarily
		// busy, and so a decision that arrives a hair before the driver starts
		// is not lost.
		decision: make(chan PairDecision, 1),
		remote:   make(chan pairEvent, 4),
		done:     make(chan struct{}),
	}
}

// setConn binds the pairing to the socket it runs over.
func (p *pairing) setConn(c *conn) { p.conn.Store(c) }

// socket is the connection the pairing runs over, nil before it is dialled.
func (p *pairing) socket() *conn { return p.conn.Load() }

// pushEvent hands an event to the driver without ever blocking the reader
// goroutine on a driver that has already finished.
func (p *pairing) pushEvent(ev pairEvent) {
	select {
	case p.remote <- ev:
	case <-p.done:
	default:
	}
}

// -----------------------------------------------------------------------------
// Pending registry
// -----------------------------------------------------------------------------

// beginPairing registers a pairing, refusing a second one for the same device.
// Allowing two would mean two codes on screen for one peer and a race over
// which token wins.
func (m *Manager) beginPairing(p *pairing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.pending[p.deviceID]; exists {
		return fmt.Errorf("%w: %s", ErrPairInProgress, p.deviceID)
	}
	m.pending[p.deviceID] = p
	return nil
}

func (m *Manager) pendingFor(deviceID string) *pairing {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[deviceID]
}

// endPairing removes p from the pending set and wakes anything waiting on it.
func (m *Manager) endPairing(p *pairing) {
	m.mu.Lock()
	if m.pending[p.deviceID] == p {
		delete(m.pending, p.deviceID)
	}
	m.mu.Unlock()

	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

// Confirm delivers a human's answer to whichever pairing is waiting on that
// device, in either direction.
//
// This is the only way an accept enters the state machine. It returns
// ErrNoPendingPairing when nothing is waiting, when the pairing has already
// been answered, or when it has already expired, so a late click on a stale
// prompt can never revive a pairing the timeout already rejected.
func (m *Manager) Confirm(d PairDecision) error {
	if d.DeviceID == "" {
		return ErrNoPendingPairing
	}
	p := m.pendingFor(d.DeviceID)
	if p == nil {
		return fmt.Errorf("%w: %s", ErrNoPendingPairing, d.DeviceID)
	}

	select {
	case <-p.done:
		return fmt.Errorf("%w: %s", ErrNoPendingPairing, d.DeviceID)
	default:
	}

	select {
	case p.decision <- d:
		return nil
	default:
		// The buffer holds one answer. A second one means the human already
		// answered and this is a duplicate click or a stale prompt.
		return fmt.Errorf("%w: %s already answered", ErrNoPendingPairing, d.DeviceID)
	}
}

// -----------------------------------------------------------------------------
// The pairing state machine
// -----------------------------------------------------------------------------

// runPairing drives one pairing to a single, definite outcome. It always calls
// OnPairResult exactly once, and it never completes a pairing that a human did
// not explicitly accept on this side.
func (m *Manager) runPairing(ctx context.Context, p *pairing) {
	defer m.endPairing(p)

	sock := p.socket()
	if sock == nil {
		// Only reachable if a caller started the driver before the dial
		// completed, which the two entry points never do.
		m.failPairing(p, protocol.PairReasonInternal, false)
		return
	}

	timer := time.NewTimer(m.pairTimeout)
	defer timer.Stop()

	localAccepted, remoteAccepted := false, false

	for {
		select {
		case <-ctx.Done():
			m.failPairing(p, protocol.PairReasonInternal, true)
			return

		case <-sock.ctx.Done():
			// The socket died under us. Nothing to tell the peer.
			m.failPairing(p, protocol.PairReasonUnreachable, false)
			return

		case <-timer.C:
			// An unanswered pairing expires as a rejection. Treating silence as
			// consent would defeat the whole point of the six digits: a peer
			// could pair with an unattended machine simply by waiting.
			m.log.Info("pairing expired without an answer", "deviceId", p.deviceID)
			m.failPairing(p, protocol.PairReasonTimeout, true)
			return

		case d := <-p.decision:
			if !d.Accept {
				reason := d.Reason
				if reason == "" {
					reason = protocol.PairReasonDeclined
				}
				m.failPairing(p, reason, true)
				return
			}
			if localAccepted {
				continue
			}
			localAccepted = true
			if err := sock.enqueue(protocol.NewPeerPairAccept(m.self.ID)); err != nil {
				m.failPairing(p, protocol.PairReasonUnreachable, false)
				return
			}

		case ev := <-p.remote:
			switch ev.kind {
			case evAccept:
				remoteAccepted = true
			case evReject:
				m.failPairing(p, ev.reason, false)
				return
			case evCancelled:
				m.failPairing(p, ev.reason, true)
				return
			case evPaired:
				if p.responder {
					// Only the responder issues tokens. A dialer that sends one
					// is trying to plant a token of its choosing.
					m.log.Warn("dialer tried to issue a pairing token", "deviceId", p.deviceID)
					m.failPairing(p, protocol.PairReasonInternal, true)
					return
				}
				if !localAccepted {
					// The token arrived before this human agreed to anything.
					m.log.Warn("peer sent a token before the local human accepted", "deviceId", p.deviceID)
					m.failPairing(p, protocol.PairReasonInternal, true)
					return
				}
				m.completePairing(p, ev.token, false)
				return
			}
		}

		if p.responder && localAccepted && remoteAccepted {
			// Both humans have accepted, so the responder mints the shared
			// token and hands it over. This is the only place a token is
			// created.
			token, err := config.NewToken()
			if err != nil {
				m.log.Error("mint pairing token", "deviceId", p.deviceID, "error", err)
				m.failPairing(p, protocol.PairReasonInternal, true)
				return
			}
			m.completePairing(p, token, true)
			return
		}
	}
}

// completePairing stores the shared token and promotes the socket to
// authenticated. Both ends run this with the same token: the responder with the
// one it minted, the dialer with the one it received.
//
// announce is true on the responder, which owes the dialer the token. It is
// sent only after this side is already authenticated, so a peer:signal that
// comes straight back down the socket is not mistaken for a stray frame in the
// middle of a pairing and used to close the connection.
func (m *Manager) completePairing(p *pairing, token string, announce bool) {
	if !protocol.ValidTokenFormat(token) {
		m.log.Warn("refusing a malformed pairing token", "deviceId", p.deviceID)
		m.failPairing(p, protocol.PairReasonInternal, true)
		return
	}
	if _, err := m.trust.Add(p.deviceID, p.name, token); err != nil {
		m.log.Error("record paired device", "deviceId", p.deviceID, "error", err)
		m.failPairing(p, protocol.PairReasonInternal, true)
		return
	}

	sock := p.socket()
	sock.paired.Store(true)
	m.registerConn(sock)

	if announce {
		if err := sock.enqueue(protocol.NewPeerPaired(token)); err != nil {
			// The socket died between the two humans accepting and the token
			// going out. This side is paired; the far side will time out and
			// pair again, overwriting this entry.
			m.log.Warn("could not deliver the pairing token", "deviceId", p.deviceID, "error", err)
		}
	}

	m.log.Info("paired with device", "deviceId", p.deviceID, "name", p.name, "direction", p.direction)
	m.handler.OnPairResult(p.deviceID, true, "")
	m.handler.OnPeerState(p.deviceID, protocol.PeerStatePaired, "")
}

// failPairing ends a pairing without trust. notifyPeer sends a
// peer:pair:reject first, which is ordered ahead of the close so the far side
// learns why instead of seeing a bare disconnect.
func (m *Manager) failPairing(p *pairing, reason string, notifyPeer bool) {
	if reason == "" {
		reason = protocol.PairReasonDeclined
	}
	if sock := p.socket(); sock != nil {
		if notifyPeer {
			_ = sock.enqueue(protocol.NewPeerPairReject(reason))
		}
		sock.closeWith(protocol.PeerClosePairRejected, "pairing rejected: "+reason)
	}

	m.log.Info("pairing failed", "deviceId", p.deviceID, "reason", reason, "direction", p.direction)
	m.handler.OnPairResult(p.deviceID, false, reason)

	// declined and timeout are ordinary human outcomes, not faults; anything
	// else is worth surfacing as an error state in the UI.
	state := protocol.PeerStateOffline
	message := ""
	switch reason {
	case protocol.PairReasonDeclined, protocol.PairReasonTimeout:
	default:
		state, message = protocol.PeerStateError, reason
	}
	m.handler.OnPeerState(p.deviceID, state, message)
}

// -----------------------------------------------------------------------------
// Pairing frames off the wire
// -----------------------------------------------------------------------------

// handlePairingFrame processes a frame on a socket that is not yet
// authenticated, and reports whether the socket may continue.
//
// The set of frames accepted here is the whole pre-authentication surface of
// the daemon: an accept, a reject, and (on the dialer) the issued token.
func (m *Manager) handlePairingFrame(c *conn, env protocol.Envelope, data []byte) bool {
	p := m.pendingFor(c.deviceID())
	if p == nil {
		c.log.Debug("frame on a socket with no pairing in flight", "type", env.Type, "deviceId", c.deviceID())
		c.closeWith(protocol.PeerCloseBadFrame, "no pairing in progress")
		return false
	}
	// The pairing in flight for this device must be this socket's own. A frame
	// arriving late on a dying socket must never be counted as an answer in a
	// pairing that some other connection is driving.
	if p.socket() != c {
		c.log.Warn("frame belongs to a different pairing socket", "type", env.Type, "deviceId", c.deviceID())
		c.closeWith(protocol.PeerCloseBadFrame, "stale pairing socket")
		return false
	}

	switch env.Type {
	case protocol.TypePeerPairAccept:
		var msg protocol.PeerPairAcceptMessage
		if err := json.Unmarshal(data, &msg); err != nil || msg.Validate() != nil {
			c.closeWith(protocol.PeerCloseBadFrame, "malformed peer:pair:accept")
			return false
		}
		if msg.DeviceID != c.deviceID() {
			c.log.Warn("peer:pair:accept from a mismatched device id",
				"socketDeviceId", c.deviceID(), "claimed", msg.DeviceID)
			c.closeWith(protocol.PeerCloseBadFrame, "accept from a mismatched device id")
			return false
		}
		p.pushEvent(pairEvent{kind: evAccept})
		return true

	case protocol.TypePeerPaired:
		var msg protocol.PeerPairedMessage
		if err := json.Unmarshal(data, &msg); err != nil || msg.Validate() != nil {
			c.closeWith(protocol.PeerCloseBadFrame, "malformed peer:paired")
			return false
		}
		p.pushEvent(pairEvent{kind: evPaired, token: msg.Token})
		return true

	case protocol.TypePeerPairReject:
		var msg protocol.PeerPairRejectMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.closeWith(protocol.PeerCloseBadFrame, "malformed peer:pair:reject")
			return false
		}
		reason := msg.Reason
		if reason == "" {
			reason = protocol.PairReasonDeclined
		}
		p.pushEvent(pairEvent{kind: evReject, reason: reason})
		return true

	default:
		c.log.Debug("unexpected frame during pairing", "type", env.Type, "deviceId", c.deviceID())
		c.closeWith(protocol.PeerCloseBadFrame, "unexpected frame during pairing")
		return false
	}
}
