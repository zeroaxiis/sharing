package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/zeroaxiis/sharing/daemon/peer"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

const (
	// sendBuffer is how many outbound frames may queue for one client before it
	// is considered wedged and disconnected.
	sendBuffer = 32
	// readLimit caps a single inbound frame. Control messages are tiny; bulk
	// payloads travel over WebRTC, never over this socket.
	readLimit = protocol.MaxFrameBytes
	// writeTimeout bounds a single outbound frame write.
	writeTimeout = 10 * time.Second
	// keepaliveInterval is how often the daemon pings an idle client so that a
	// half-open TCP connection is noticed instead of lingering forever.
	keepaliveInterval = 30 * time.Second
	// keepaliveTimeout is how long a ping may go unanswered before the client
	// is dropped.
	keepaliveTimeout = 10 * time.Second
)

// allowedOriginPatterns mirrors OriginAllowed in the pattern syntax used by
// github.com/coder/websocket. It is a second, independent gate behind the
// explicit OriginAllowed check in handleWebSocket.
//
// InsecureSkipVerify is deliberately NOT used. Disabling origin verification
// would make the daemon trivially CSRF-able: any web page the user visits could
// open ws://127.0.0.1:8765/ws from the background and drive transfers, because
// browsers attach no same-origin restriction to WebSocket handshakes on their
// own. Keeping the library check on means a coding mistake in OriginAllowed
// still cannot open the socket to the whole web.
var allowedOriginPatterns = []string{
	"chrome-extension://*",
	"moz-extension://*",
	"safari-web-extension://*",
	"http://localhost:*",
	"http://127.0.0.1:*",
}

// hub is the registry of live WebSocket clients.
type hub struct {
	log *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
	baseCtx context.Context
}

func newHub(logger *slog.Logger) *hub {
	return &hub{
		log:     logger,
		clients: make(map[*client]struct{}),
		baseCtx: context.Background(),
	}
}

// setBaseContext installs the parent context every client connection derives
// from, so cancelling it tears down all of them.
func (h *hub) setBaseContext(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.baseCtx = ctx
}

func (h *hub) base() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.baseCtx
}

func (h *hub) add(c *client) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	return len(h.clients)
}

func (h *hub) remove(c *client) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	return len(h.clients)
}

func (h *hub) snapshot() []*client {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

// closeAll closes every live client with a going-away status.
func (h *hub) closeAll() {
	clients := h.snapshot()
	if len(clients) == 0 {
		return
	}
	h.log.Info("closing websocket clients", "count", len(clients))
	for _, c := range clients {
		c.closeWith(websocket.StatusGoingAway, "daemon shutting down")
	}
}

// client is one WebSocket connection.
//
// All frames leave through send and are written by the single writeLoop
// goroutine, so the connection never sees concurrent writes no matter how many
// goroutines want to push a message.
type client struct {
	id     string
	conn   *websocket.Conn
	send   chan []byte
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
}

// handleWebSocket upgrades a request on /ws and serves it until it closes.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, protocol.HTTPError{Error: "method not allowed"}, s.log)
		return
	}

	// Explicit origin gate, using exactly the allow-list from the spec. A
	// browser cannot be trusted to enforce anything here: WebSocket handshakes
	// are exempt from the same-origin policy, so this check is the actual
	// security boundary. A request with no Origin (native client, curl) is
	// allowed through deliberately - there is no ambient browser authority to
	// abuse in that case.
	origin := r.Header.Get("Origin")
	if origin != "" && !OriginAllowed(origin) {
		s.log.Warn("rejected websocket upgrade from disallowed origin", "origin", origin)
		writeJSON(w, http.StatusForbidden, protocol.HTTPError{Error: "origin not allowed"}, s.log)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: allowedOriginPatterns,
		// InsecureSkipVerify stays false on purpose; see allowedOriginPatterns.
	})
	if err != nil {
		// Accept has already written an error response.
		s.log.Warn("websocket upgrade failed", "error", err, "origin", origin)
		return
	}
	conn.SetReadLimit(readLimit)

	ctx, cancel := context.WithCancel(s.hub.base())
	defer cancel()

	c := &client{
		id:     uuid.NewString(),
		conn:   conn,
		send:   make(chan []byte, sendBuffer),
		log:    s.log.With("origin", origin),
		ctx:    ctx,
		cancel: cancel,
	}

	total := s.hub.add(c)
	s.log.Info("websocket client connected", "clientId", c.id, "origin", origin, "clients", total)

	defer func() {
		remaining := s.hub.remove(c)
		c.closeWith(websocket.StatusNormalClosure, "")
		s.log.Info("websocket client disconnected", "clientId", c.id, "clients", remaining)
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writeLoop()
	}()

	s.readLoop(c)

	// Unblock writeLoop, then wait for it so the connection is not closed out
	// from under an in-flight write.
	cancel()
	wg.Wait()
}

// readLoop consumes frames until the peer or the daemon closes the connection.
func (s *Server) readLoop(c *client) {
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			switch {
			case errors.Is(err, context.Canceled), c.ctx.Err() != nil:
				c.log.Debug("websocket read stopped", "clientId", c.id, "reason", "context cancelled")
			case websocket.CloseStatus(err) != -1:
				c.log.Debug("websocket closed by peer", "clientId", c.id, "status", websocket.CloseStatus(err))
			default:
				c.log.Debug("websocket read error", "clientId", c.id, "error", err)
			}
			return
		}

		if typ != websocket.MessageText {
			c.enqueue(protocol.NewError("", protocol.ErrCodeBadJSON, "expected a UTF-8 text frame containing JSON"))
			continue
		}

		s.dispatch(c, data)
	}
}

// dispatch decodes one frame and answers it. Decode pattern per the spec: read
// the envelope, switch on type, then re-unmarshal the same raw bytes.
func (s *Server) dispatch(c *client, data []byte) {
	env, err := protocol.DecodeEnvelope(data)
	if err != nil {
		c.log.Debug("undecodable frame", "clientId", c.id, "error", err)
		c.enqueue(protocol.NewError("", protocol.ErrCodeBadJSON, "message is not a valid protocol envelope"))
		return
	}

	switch env.Type {
	case protocol.TypeHello:
		var msg protocol.HelloMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed hello message"))
			return
		}
		if msg.ProtocolVersion != protocol.ProtocolVersion {
			c.log.Warn("protocol version mismatch",
				"clientId", c.id, "client", msg.Client, "clientVersion", msg.ClientVersion,
				"theirs", msg.ProtocolVersion, "ours", protocol.ProtocolVersion)
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeProtocolMismatch,
				fmt.Sprintf("unsupported protocol version %d, daemon speaks version %d",
					msg.ProtocolVersion, protocol.ProtocolVersion)))
			return
		}
		c.log.Info("client handshake accepted",
			"clientId", c.id, "client", msg.Client, "clientVersion", msg.ClientVersion)
		c.enqueue(protocol.NewReady(env.ID, s.cfg.SelfDevice()))

	case protocol.TypePing:
		var msg protocol.PingMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed ping message"))
			return
		}
		c.enqueue(protocol.NewPong(env.ID, msg.T))

	case protocol.TypeDevicesRefresh:
		// Age the roster first, so a refresh after a peer disappeared reports
		// the truth rather than the last cached sighting. The answer goes to
		// the asking client only; the debounced push serves everyone else.
		s.SweepDevices()
		c.enqueue(protocol.NewDevices(s.Devices()))

	case protocol.TypePairStart:
		var msg protocol.PairStartMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed pair:start message"))
			return
		}
		s.handlePairStart(c, env.ID, msg)

	case protocol.TypePairConfirm:
		var msg protocol.PairConfirmMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed pair:confirm message"))
			return
		}
		s.handlePairConfirm(c, env.ID, msg)

	case protocol.TypePairForget:
		var msg protocol.PairForgetMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed pair:forget message"))
			return
		}
		s.handlePairForget(c, env.ID, msg)

	case protocol.TypeSignalOffer:
		var msg protocol.SignalOfferRequest
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed signal:offer message"))
			return
		}
		if err := msg.Validate(); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeInvalidRequest, err.Error()))
			return
		}
		s.relaySignal(c, env.ID, msg.To, protocol.NewPeerSignalOffer(s.cfg.DeviceID, msg.SDP))

	case protocol.TypeSignalAnswer:
		var msg protocol.SignalAnswerRequest
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed signal:answer message"))
			return
		}
		if err := msg.Validate(); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeInvalidRequest, err.Error()))
			return
		}
		s.relaySignal(c, env.ID, msg.To, protocol.NewPeerSignalAnswer(s.cfg.DeviceID, msg.SDP))

	case protocol.TypeSignalICE:
		var msg protocol.SignalICERequest
		if err := json.Unmarshal(data, &msg); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeBadJSON, "malformed signal:ice message"))
			return
		}
		if err := msg.Validate(); err != nil {
			c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeInvalidRequest, err.Error()))
			return
		}
		s.relaySignal(c, env.ID, msg.To,
			protocol.NewPeerSignalICE(s.cfg.DeviceID, msg.Candidate, msg.SDPMid, msg.SDPMLineIndex))

	default:
		c.log.Debug("unsupported message type", "clientId", c.id, "type", env.Type)
		c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeUnknownType,
			fmt.Sprintf("unsupported message type: %s", env.Type)))
	}
}

// Broadcast queues a message for every connected client. Safe to call from any
// goroutine, including the peer layer callbacks below.
func (s *Server) Broadcast(msg any) {
	for _, c := range s.hub.snapshot() {
		c.enqueue(msg)
	}
}

// -----------------------------------------------------------------------------
// Control plane -> peer layer: what the extension asked this daemon to do
// -----------------------------------------------------------------------------

// peerContext is the context handed to the peer layer for work that outlives
// the frame that started it.
//
// It is deliberately not derived from the client connection, and carries no
// deadline of its own. A pairing waits up to two minutes for two humans to
// press a button, and relaying an offer may open a socket that has to stay up
// for the answer and the ICE candidates that follow. Scoping either to the
// request would cancel a pairing the moment the popup closed and tear down a
// peer connection as soon as the first frame was delivered. The peer layer
// applies its own dial, handshake and pairing timeouts; this context exists
// only so that everything it owns still dies at shutdown.
func (s *Server) peerContext() context.Context {
	return s.hub.base()
}

// handlePairStart begins pairing with a discovered device.
//
// Everything answerable locally is answered locally, with a specific code. The
// alternative - handing any device id straight to the peer layer - turns a
// typo into a ten-second dial timeout, and a stale UI row into an error the
// user cannot act on.
func (s *Server) handlePairStart(c *client, reqID string, msg protocol.PairStartMessage) {
	if err := msg.Validate(); err != nil {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInvalidRequest, err.Error()))
		return
	}
	if msg.DeviceID == s.cfg.DeviceID {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInvalidRequest,
			"cannot pair a device with itself"))
		return
	}

	dev, ok := s.device(msg.DeviceID)
	if !ok {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeUnknownDevice,
			"no device with that id has been discovered"))
		return
	}
	if dev.Status == protocol.StatusOffline || dev.Address == "" || dev.Port == 0 {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeDeviceOffline,
			"device has not been seen recently enough to dial"))
		return
	}
	if s.trust != nil && s.trust.IsPaired(msg.DeviceID) {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeAlreadyPaired,
			"device is already paired; forget it first to pair again"))
		return
	}
	if s.peers == nil {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInternal, "peer layer is not running"))
		return
	}

	s.log.Info("pairing requested", "deviceId", dev.ID, "name", dev.Name, "address", dev.Address, "port", dev.Port)
	if err := s.peers.StartPairing(s.peerContext(), dev); err != nil {
		code, detail := peerErrorCode(err, protocol.ErrCodePairFailed)
		s.log.Warn("pair:start failed", "deviceId", dev.ID, "error", err)
		c.enqueue(protocol.NewError(reqID, code, detail))
		return
	}
	// Progress is not a reply: it arrives as pair:code, peer:state and finally
	// pair:result pushes, which every client sees.
}

// handlePairConfirm delivers the human decision into a pairing already in
// flight, in either direction.
func (s *Server) handlePairConfirm(c *client, reqID string, msg protocol.PairConfirmMessage) {
	if err := msg.Validate(); err != nil {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInvalidRequest, err.Error()))
		return
	}
	if s.peers == nil {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInternal, "peer layer is not running"))
		return
	}

	decision := peer.PairDecision{DeviceID: msg.DeviceID, Accept: msg.Accept}
	if !msg.Accept {
		decision.Reason = protocol.PairReasonDeclined
	}

	if err := s.peers.Confirm(decision); err != nil {
		// A confirm with nothing waiting is the interesting case: the pairing
		// timed out, or the peer hung up, and the user is clicking a button
		// that is no longer connected to anything. Say so rather than pretend
		// it worked.
		code, detail := peerErrorCode(err, protocol.ErrCodePairFailed)
		s.log.Warn("pair:confirm failed", "deviceId", msg.DeviceID, "accept", msg.Accept, "error", err)
		c.enqueue(protocol.NewError(reqID, code, detail))
		return
	}
	s.log.Info("pairing decision delivered", "deviceId", msg.DeviceID, "accept", msg.Accept)
}

// handlePairForget revokes trust in a device and drops any live socket to it.
func (s *Server) handlePairForget(c *client, reqID string, msg protocol.PairForgetMessage) {
	if err := msg.Validate(); err != nil {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInvalidRequest, err.Error()))
		return
	}

	if s.peers != nil {
		if err := s.peers.Forget(msg.DeviceID); err != nil {
			code, detail := peerErrorCode(err, protocol.ErrCodeInternal)
			s.log.Warn("pair:forget failed", "deviceId", msg.DeviceID, "error", err)
			c.enqueue(protocol.NewError(reqID, code, detail))
			return
		}
	} else if s.trust != nil {
		// No peer layer (tests, or a degraded start): the trust store is still
		// the source of truth for what is paired, so honour the revocation.
		if _, err := s.trust.Remove(msg.DeviceID); err != nil {
			s.log.Error("forget device", "deviceId", msg.DeviceID, "error", err)
			c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInternal, "could not update the trust store"))
			return
		}
	}

	s.log.Info("device forgotten", "deviceId", msg.DeviceID)
	// The paired flag just changed for everyone, so push rather than debounce.
	s.BroadcastDevices()
}

// relaySignal hands one SDP or ICE item to the peer layer, addressed by "to".
//
// Every failure is reported back with a code. A silently dropped signal is the
// worst possible outcome here: WebRTC simply never connects, with no error
// anywhere, and the user is left staring at a spinner.
func (s *Server) relaySignal(c *client, reqID, to string, frame protocol.PeerSignalMessage) {
	if to == s.cfg.DeviceID {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInvalidRequest,
			"cannot signal this device to itself"))
		return
	}
	if !s.knownDevice(to) {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeUnknownDevice,
			"no discovered or paired device with that id"))
		return
	}
	if s.peers == nil {
		c.enqueue(protocol.NewError(reqID, protocol.ErrCodeInternal, "peer layer is not running"))
		return
	}

	if err := s.peers.SendSignal(s.peerContext(), to, frame); err != nil {
		code, detail := peerErrorCode(err, protocol.ErrCodePeerUnreachable)
		s.log.Warn("signal relay failed", "to", to, "kind", frame.Kind, "error", err)
		c.enqueue(protocol.NewError(reqID, code, detail))
		return
	}
	s.log.Debug("signal relayed", "to", to, "kind", frame.Kind)
}

// peerErrorCode maps a peer-layer error onto a protocol error code the
// extension can branch on, falling back to fallbackCode for anything unknown.
func peerErrorCode(err error, fallbackCode string) (code, message string) {
	switch {
	case errors.Is(err, peer.ErrNotPaired):
		return protocol.ErrCodeNotPaired, "device is not paired"
	case errors.Is(err, peer.ErrPeerUnreachable):
		return protocol.ErrCodePeerUnreachable, "could not reach the device"
	case errors.Is(err, peer.ErrNoPendingPairing):
		return protocol.ErrCodeInvalidRequest, "no pairing is waiting for a decision on that device"
	case errors.Is(err, peer.ErrPairTimeout):
		return protocol.ErrCodePairFailed, "pairing timed out"
	case errors.Is(err, protocol.ErrInvalidMessage):
		return protocol.ErrCodeInvalidRequest, err.Error()
	default:
		return fallbackCode, err.Error()
	}
}

// -----------------------------------------------------------------------------
// Peer layer -> control plane: what arrived from the LAN
//
// *Server implements peer.Handler, so the peer layer never needs to know a
// WebSocket exists; it just reports facts and they turn into pushes.
// -----------------------------------------------------------------------------

var _ peer.Handler = (*Server)(nil)

// OnPairCode puts the six digits in front of the human on every connected UI.
func (s *Server) OnPairCode(deviceID, name, code, direction string) {
	s.log.Info("pairing code ready", "deviceId", deviceID, "name", name, "direction", direction)
	s.Broadcast(protocol.NewPairCode(deviceID, code, direction, name))
}

// OnPairResult reports the outcome of a pairing attempt exactly once.
func (s *Server) OnPairResult(deviceID string, paired bool, reason string) {
	s.log.Info("pairing finished", "deviceId", deviceID, "paired", paired, "reason", reason)
	s.Broadcast(protocol.NewPairResult(deviceID, paired, reason))
	// The paired flag on that device just changed, so the roster is stale.
	s.BroadcastDevices()
}

// OnPeerState reports a peer lifecycle transition.
func (s *Server) OnPeerState(deviceID, state, message string) {
	s.log.Debug("peer state", "deviceId", deviceID, "state", state, "message", message)
	s.Broadcast(protocol.NewPeerState(deviceID, state, message))
}

// OnSignal converts an inbound peer:signal into the control-plane push the
// extension expects, carrying "from" instead of "to".
func (s *Server) OnSignal(msg protocol.PeerSignalMessage) {
	if p, ok := msg.ToOfferPush(); ok {
		s.Broadcast(p)
		return
	}
	if p, ok := msg.ToAnswerPush(); ok {
		s.Broadcast(p)
		return
	}
	if p, ok := msg.ToICEPush(); ok {
		s.Broadcast(p)
		return
	}
	// Unroutable: an unknown kind, or a kind missing its payload. Dropping it
	// is right, but it must not be silent.
	s.log.Warn("dropping unroutable peer signal", "from", msg.From, "kind", msg.Kind)
}

// writeLoop is the only goroutine that writes to the connection. It also owns
// the keepalive ping, so a ping can never race a data frame.
func (c *client) writeLoop() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case data := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.log.Debug("websocket write failed", "clientId", c.id, "error", err)
				c.cancel()
				return
			}

		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, keepaliveTimeout)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				c.log.Debug("keepalive ping failed, dropping client", "clientId", c.id, "error", err)
				c.cancel()
				return
			}
		}
	}
}

// enqueue marshals msg and hands it to writeLoop. A client that cannot drain
// its buffer is disconnected rather than allowed to block the daemon.
func (c *client) enqueue(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		c.log.Error("encode outbound message", "clientId", c.id, "error", err)
		return
	}

	select {
	case <-c.ctx.Done():
		return
	default:
	}

	select {
	case c.send <- data:
	case <-c.ctx.Done():
	default:
		c.log.Warn("client send buffer full, dropping connection", "clientId", c.id)
		c.closeWith(websocket.StatusPolicyViolation, "send buffer overflow")
	}
}

// closeWith closes the connection once and cancels everything hanging off it.
// Safe to call from any goroutine, any number of times.
func (c *client) closeWith(status websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		if err := c.conn.Close(status, reason); err != nil {
			// A peer that vanished mid-close is routine, not worth a warning.
			c.log.Debug("websocket close", "clientId", c.id, "error", err)
		}
		c.cancel()
	})
}
