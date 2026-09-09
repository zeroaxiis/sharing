// Package peer owns the LAN-facing half of the daemon: a second WebSocket
// listener that other daemons dial, the pairing exchange that gates it, and the
// signal relay that carries WebRTC offers, answers and ICE candidates between
// two paired machines.
//
// This is the only part of the process reachable from the network, so every
// inbound byte is treated as hostile:
//
//   - frames are capped at protocol.MaxFrameBytes in both directions;
//   - the first frame on a socket must be a peer:hello, and anything else
//     closes it;
//   - a device with no token, an unknown token or a wrong token gets the
//     pairing exchange and nothing else, never the signal relay;
//   - tokens are compared by config.TrustStore.VerifyToken, in constant time;
//   - pairing attempts are rate limited per source IP so the six-digit code
//     cannot be ground down by a script;
//   - a pairing completes only after a human accepts on BOTH ends. There is no
//     auto-accept, and an unanswered attempt expires as a rejection.
//
// SIGNALLING ONLY. File bytes never traverse this port. The only frame type
// accepted after the handshake is protocol.TypePeerSignal, binary frames are
// refused outright (see errBinaryFrame), and SendSignal refuses to relay
// anything that is not a peer:signal. Payload bytes travel over the WebRTC
// DataChannel the two browsers negotiate from the SDP this package relays,
// which is what keeps transfers direct and end-to-end encrypted.
//
// WebRTC itself lives in the extension, not here: the daemon is a signalling
// relay and a trust store, and never sees a data packet.
package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// DefaultBindHost is the interface the peer listener binds.
//
// Unlike the control plane this one MUST leave loopback: two daemons on
// different machines cannot reach each other through 127.0.0.1. Everything this
// port exposes is therefore either authenticated by a token or gated behind a
// human pressing accept.
const DefaultBindHost = "0.0.0.0"

// Tunables for the peer plane. They are deliberately not configurable: these
// are security properties, not preferences.
const (
	// sendBuffer is how many frames may queue for one peer before it is
	// considered wedged and dropped.
	sendBuffer = 32
	// handshakeTimeout bounds every read a peer has not yet earned the right to
	// make us wait on: the first frame in either direction. It is the answer to
	// a slow-loris peer that connects and then says nothing.
	handshakeTimeout = 10 * time.Second
	// dialTimeout bounds an outbound connection attempt.
	dialTimeout = 8 * time.Second
	// writeTimeout bounds a single outbound frame write.
	writeTimeout = 10 * time.Second
	// keepaliveInterval and keepaliveTimeout notice a peer that went away
	// without closing, which bare TCP can take many minutes to work out.
	keepaliveInterval = 30 * time.Second
	keepaliveTimeout  = 10 * time.Second
	// closeGrace is how long an ordered close may take before the socket is
	// killed outright.
	closeGrace = 2 * time.Second
	// defaultPairTimeout is how long a pairing waits for its two humans.
	defaultPairTimeout = 120 * time.Second
	// maxPendingHandshakes caps how many unauthenticated sockets may be
	// mid-handshake at once, so a flood of connections cannot exhaust
	// goroutines and file descriptors before any of them is authenticated.
	maxPendingHandshakes = 64
	// Reconnect backoff for a paired peer that reappeared on the LAN.
	reconnectInitialDelay = 500 * time.Millisecond
	reconnectMaxDelay     = 30 * time.Second
	maxReconnectAttempts  = 6
)

// Errors surfaced to the control plane, which maps them onto protocol error
// codes. Their identity matters more than their text.
var (
	// ErrNotPaired means the device is not in the trust store, or no longer
	// trusts us, so nothing may be relayed to it.
	ErrNotPaired = errors.New("peer: device is not paired")
	// ErrPeerUnreachable means the device is trusted but could not be reached.
	ErrPeerUnreachable = errors.New("peer: device unreachable")
	// ErrNoPendingPairing means a confirmation arrived for a pairing that is
	// not in flight, has already been answered, or has already expired.
	ErrNoPendingPairing = errors.New("peer: no pairing awaiting confirmation")
	// ErrPairTimeout means neither human answered before the deadline.
	ErrPairTimeout = errors.New("peer: pairing timed out")
	// ErrPairInProgress means a pairing with that device is already running.
	ErrPairInProgress = errors.New("peer: pairing already in progress")
)

// PairDecision is how a human's answer reaches an in-flight pairing. It is
// produced by the control plane when the user clicks and by nothing else: no
// path in this package fabricates an accept.
type PairDecision struct {
	// DeviceID names the pairing being answered.
	DeviceID string
	// Accept is the human's answer.
	Accept bool
	// Reason is one of the protocol.PairReason* constants when Accept is false,
	// and empty when it is true.
	Reason string
}

// Handler is what the peer layer calls into. The wiring layer (the control
// server) implements it; this package never does.
//
// Every method is called from a peer goroutine, so implementations must not
// block and must not call back into the Manager synchronously.
type Handler interface {
	// OnPairCode fires once the six digits are derived and a human must decide.
	// direction is protocol.PairDirectionOutgoing or PairDirectionIncoming.
	OnPairCode(deviceID, name, code, direction string)
	// OnPairResult fires exactly once per pairing attempt, success or failure.
	OnPairResult(deviceID string, paired bool, reason string)
	// OnPeerState reports a lifecycle change, one of protocol.PeerState*.
	OnPeerState(deviceID, state, message string)
	// OnSignal delivers a peer:signal received from a paired device.
	OnSignal(msg protocol.PeerSignalMessage)
}

// Options configures a Manager.
type Options struct {
	// Self is this daemon's identity, sent in peer:hello and peer:ready.
	Self protocol.SelfDevice
	// Port is the LAN TCP port to listen on. Zero selects
	// protocol.DefaultPeerPort.
	Port int
	// Trust is the paired-device store. Required.
	Trust *config.TrustStore
	// Handler receives pairing and signalling events. Required.
	Handler Handler
	// Logger receives structured logs. Nil uses slog.Default.
	Logger *slog.Logger
	// PairTimeout is how long a pairing waits for both humans. Zero selects
	// defaultPairTimeout.
	PairTimeout time.Duration
	// BindHost overrides the listen interface. Empty selects DefaultBindHost.
	// Setting it to 127.0.0.1 keeps a build off the network entirely, which is
	// what the tests do.
	BindHost string
}

// Manager owns the peer listener, the registry of live peer sockets, the
// address book of discovered devices and every in-flight pairing.
type Manager struct {
	self        protocol.SelfDevice
	trust       *config.TrustStore
	handler     Handler
	log         *slog.Logger
	pairTimeout time.Duration
	bindHost    string
	limiter     *rateLimiter
	http        *http.Server

	mu         sync.Mutex
	port       int
	ln         net.Listener
	baseCtx    context.Context
	conns      map[string]*conn    // deviceId -> live authenticated socket
	all        map[*conn]struct{}  // every socket, authenticated or not
	pending    map[string]*pairing // deviceId -> pairing awaiting humans
	addrs      map[string]string   // deviceId -> host:port on the LAN
	dialing    map[string]chan struct{}
	reconnects map[string]struct{}
	handshakes int
}

// New builds a Manager. It binds nothing; call Listen for that.
func New(opts Options) (*Manager, error) {
	if opts.Trust == nil {
		return nil, errors.New("peer: Options.Trust is required")
	}
	if opts.Handler == nil {
		return nil, errors.New("peer: Options.Handler is required")
	}
	if opts.Self.ID == "" {
		return nil, errors.New("peer: Options.Self.ID is required")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	port := opts.Port
	if port == 0 {
		port = protocol.DefaultPeerPort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("peer: port %d out of range", port)
	}
	pairTimeout := opts.PairTimeout
	if pairTimeout <= 0 {
		pairTimeout = defaultPairTimeout
	}
	bindHost := opts.BindHost
	if bindHost == "" {
		bindHost = DefaultBindHost
	}

	m := &Manager{
		self:        opts.Self,
		trust:       opts.Trust,
		handler:     opts.Handler,
		log:         logger,
		pairTimeout: pairTimeout,
		bindHost:    bindHost,
		limiter:     newRateLimiter(protocol.PeerPairAttemptsPerWindow, protocol.PeerPairRateWindow),
		port:        port,
		baseCtx:     context.Background(),
		conns:       make(map[string]*conn),
		all:         make(map[*conn]struct{}),
		pending:     make(map[string]*pairing),
		addrs:       make(map[string]string),
		dialing:     make(map[string]chan struct{}),
		reconnects:  make(map[string]struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PeerWebSocketPath, m.handlePeer)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately terse: this port is on the LAN and owes an unknown
		// caller no information about what runs behind it.
		m.log.Debug("unrouted peer request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "not found", http.StatusNotFound)
	})

	m.http = &http.Server{
		Handler: mux,
		// The WebSocket handler hijacks the connection, so a global write
		// timeout would kill live sockets. Bound the handshake only.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	return m, nil
}

// Addr is the host:port the peer listener is on, or will be once bound.
func (m *Manager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln != nil {
		return m.ln.Addr().String()
	}
	return net.JoinHostPort(m.bindHost, strconv.Itoa(m.port))
}

// Port is the TCP port the listener is bound to, which is what mDNS must
// advertise.
func (m *Manager) Port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// Listen binds the LAN socket. It is separate from Run so the daemon fails fast
// on a port conflict, before mDNS advertises a port nothing is listening on.
func (m *Manager) Listen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln != nil {
		return nil
	}

	addr := net.JoinHostPort(m.bindHost, strconv.Itoa(m.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	m.ln = ln
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		m.port = tcp.Port
	}
	m.log.Info("peer listener bound", "addr", ln.Addr().String(),
		"note", "this port is LAN-facing and accepts nothing but a pairing request until a human approves it")
	return nil
}

// Run serves the peer plane until ctx is cancelled, then closes every peer
// socket and drains the listener.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.Listen(); err != nil {
		return err
	}

	connCtx, cancelConns := context.WithCancel(ctx)
	defer cancelConns()

	m.mu.Lock()
	ln := m.ln
	m.baseCtx = connCtx
	m.mu.Unlock()

	serveErr := make(chan error, 1)
	go func() {
		if err := m.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("serve peer plane: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	m.log.Info("shutting down peer listener")
	cancelConns()
	m.closeAllConns()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := m.http.Shutdown(shutdownCtx); err != nil {
		if closeErr := m.http.Close(); closeErr != nil {
			return fmt.Errorf("graceful peer shutdown: %w (forced close also failed: %v)", err, closeErr)
		}
		return fmt.Errorf("graceful peer shutdown: %w", err)
	}
	if err := <-serveErr; err != nil {
		return err
	}
	return nil
}

// base is the context every peer socket derives from, so cancelling Run tears
// all of them down.
func (m *Manager) base() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseCtx
}

// -----------------------------------------------------------------------------
// Connection registry
// -----------------------------------------------------------------------------

// track records a socket for shutdown, whether or not it is authenticated: a
// socket that never finishes its handshake still has to die on ctx cancel.
func (m *Manager) track(c *conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.all[c] = struct{}{}
}

func (m *Manager) untrack(c *conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.all, c)
}

// registerConn publishes an authenticated socket as the live one for its
// device, evicting any older socket for the same device. Two daemons that dial
// each other at the same moment end up with two sockets; keeping the newest
// converges both ends without a tie-break negotiation.
func (m *Manager) registerConn(c *conn) {
	id := c.deviceID()
	if id == "" {
		return
	}
	m.mu.Lock()
	old := m.conns[id]
	m.conns[id] = c
	m.mu.Unlock()

	if old != nil && old != c {
		old.log.Debug("replacing older peer socket", "deviceId", id)
		old.closeWith(int(websocket.StatusNormalClosure), "replaced by a newer connection")
	}
}

// unregisterConn removes c from the registry, but only if it is still the live
// socket for that device. It reports whether it was.
func (m *Manager) unregisterConn(c *conn) bool {
	id := c.deviceID()
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[id] == c {
		delete(m.conns, id)
		return true
	}
	return false
}

func (m *Manager) liveConn(deviceID string) *conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[deviceID]
	if c == nil {
		return nil
	}
	if c.ctx.Err() != nil {
		delete(m.conns, deviceID)
		return nil
	}
	return c
}

// IsConnected reports whether an authenticated socket to deviceID is live.
func (m *Manager) IsConnected(deviceID string) bool {
	return m.liveConn(deviceID) != nil
}

func (m *Manager) closeAllConns() {
	m.mu.Lock()
	conns := make([]*conn, 0, len(m.all))
	for c := range m.all {
		conns = append(conns, c)
	}
	m.mu.Unlock()

	if len(conns) == 0 {
		return
	}
	// In parallel: each close waits for its own frame to reach the wire, and a
	// peer that has stopped reading must not delay every socket behind it.
	m.log.Info("closing peer sockets", "count", len(conns))
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *conn) {
			defer wg.Done()
			c.closeWith(int(websocket.StatusGoingAway), "daemon shutting down")
		}(c)
	}
	wg.Wait()
}

// acquireHandshake reserves one of the unauthenticated handshake slots.
func (m *Manager) acquireHandshake() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handshakes >= maxPendingHandshakes {
		return false
	}
	m.handshakes++
	return true
}

func (m *Manager) releaseHandshake() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handshakes > 0 {
		m.handshakes--
	}
}

// -----------------------------------------------------------------------------
// Address book
// -----------------------------------------------------------------------------

// Remember records where a discovered device can be reached and, for a paired
// device that is online with no live socket, schedules a reconnect.
//
// The manager has no view of mDNS on its own, so the wiring layer feeds
// discovery results in through here. Everything the address book holds is a
// hint: a stale entry costs one failed dial, never a wrong trust decision,
// because the token handshake still has to succeed afterwards.
func (m *Manager) Remember(dev protocol.Device) {
	if dev.ID == "" || dev.ID == m.self.ID || dev.Address == "" {
		return
	}
	port := dev.Port
	if port == 0 {
		port = protocol.DefaultPeerPort
	}
	m.rememberAddr(dev.ID, net.JoinHostPort(dev.Address, strconv.Itoa(port)))

	if dev.Status == protocol.StatusOffline || !m.trust.IsPaired(dev.ID) || m.IsConnected(dev.ID) {
		return
	}
	m.scheduleReconnect(dev.ID)
}

// RememberAll is Remember over a whole discovery snapshot.
func (m *Manager) RememberAll(devices []protocol.Device) {
	for _, d := range devices {
		m.Remember(d)
	}
}

func (m *Manager) rememberAddr(deviceID, addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addrs[deviceID] = addr
}

func (m *Manager) addrFor(deviceID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addr, ok := m.addrs[deviceID]
	return addr, ok
}

// scheduleReconnect dials a paired device that just reappeared, backing off
// between attempts. One attempt at a time per device: a device that flaps
// through several mDNS updates must not spawn a dial per update.
func (m *Manager) scheduleReconnect(deviceID string) {
	m.mu.Lock()
	if _, running := m.reconnects[deviceID]; running {
		m.mu.Unlock()
		return
	}
	m.reconnects[deviceID] = struct{}{}
	ctx := m.baseCtx
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.reconnects, deviceID)
			m.mu.Unlock()
		}()

		delay := reconnectInitialDelay
		for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
			// Jitter, so two daemons that lost each other do not retry in
			// lockstep forever.
			timer := time.NewTimer(delay + time.Duration(rand.Int64N(int64(delay/2)+1)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			if !m.trust.IsPaired(deviceID) || m.IsConnected(deviceID) {
				return
			}
			if _, err := m.ensureConn(ctx, deviceID); err == nil {
				m.log.Info("reconnected to paired peer", "deviceId", deviceID, "attempt", attempt)
				return
			}
			delay *= 2
			if delay > reconnectMaxDelay {
				delay = reconnectMaxDelay
			}
		}
		m.log.Debug("giving up reconnecting to peer for now", "deviceId", deviceID)
	}()
}

// -----------------------------------------------------------------------------
// Signal relay
// -----------------------------------------------------------------------------

// SendSignal relays one signalling item to a paired device, dialling and
// completing a token handshake first if no socket is live.
//
// It carries signalling and nothing else: a frame that is not a peer:signal is
// refused here, which is half of the guarantee that file bytes never reach this
// port. The other half is the read side, which refuses binary frames and
// anything but peer:signal on an authenticated socket.
func (m *Manager) SendSignal(ctx context.Context, deviceID string, msg protocol.PeerSignalMessage) error {
	if deviceID == "" {
		return errors.New("peer: SendSignal requires a device id")
	}
	if msg.Type != protocol.TypePeerSignal {
		return fmt.Errorf("peer: refusing to relay a %q frame over the peer plane", msg.Type)
	}
	if msg.From == "" {
		msg.From = m.self.ID
	}
	if msg.From != m.self.ID {
		// The relay speaks for this daemon only. Forwarding a frame attributed
		// to someone else would let one paired device impersonate another.
		return fmt.Errorf("peer: refusing to relay a signal attributed to %q", msg.From)
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	if !m.trust.IsPaired(deviceID) {
		return fmt.Errorf("%w: %s", ErrNotPaired, deviceID)
	}

	c, err := m.ensureConn(ctx, deviceID)
	if err != nil {
		return err
	}
	if err := c.enqueue(msg); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrPeerUnreachable, deviceID, err)
	}
	return nil
}

// Forget drops a device from the trust store and closes any live socket to it,
// so a forgotten peer cannot keep using a connection it had already earned.
func (m *Manager) Forget(deviceID string) error {
	if deviceID == "" {
		return errors.New("peer: Forget requires a device id")
	}

	// Cancel an in-flight pairing first. The human just said "forget", which is
	// not the moment to let a pairing land behind their back.
	if p := m.pendingFor(deviceID); p != nil {
		p.pushEvent(pairEvent{kind: evCancelled, reason: protocol.PairReasonDeclined})
	}

	removed, err := m.trust.Remove(deviceID)
	if c := m.liveConn(deviceID); c != nil {
		m.unregisterConn(c)
		c.closeWith(protocol.PeerCloseUnauthorized, "device unpaired")
	}
	if removed {
		m.handler.OnPeerState(deviceID, protocol.PeerStateOffline, "unpaired")
	}
	return err
}

// -----------------------------------------------------------------------------
// Frame loop
// -----------------------------------------------------------------------------

// serveConn runs the frame loop for the life of a socket and cleans up after
// it. It is the single reader goroutine for that socket.
func (m *Manager) serveConn(c *conn) {
	defer m.teardown(c)
	m.frameLoop(c)
}

// frameLoop reads frames until the socket dies.
//
// Which frames are legal depends on whether the socket is authenticated, and
// that flag is only ever set by a verified token or a completed pairing. Until
// then a peer may send exactly three things: an accept, a reject, or the
// responder's token. Anything else closes the socket.
func (m *Manager) frameLoop(c *conn) {
	for {
		data, err := c.readFrame(0)
		if err != nil {
			if errors.Is(err, errBinaryFrame) {
				c.log.Warn("peer sent a binary frame; the peer plane carries signalling only",
					"deviceId", c.deviceID())
				c.closeWith(protocol.PeerCloseBadFrame, "binary frames are not accepted")
			}
			return
		}

		env, err := protocol.DecodeEnvelope(data)
		if err != nil {
			c.log.Debug("undecodable peer frame", "deviceId", c.deviceID(), "error", err)
			c.closeWith(protocol.PeerCloseBadFrame, "frame is not a valid protocol envelope")
			return
		}

		if c.paired.Load() {
			if !m.handleSignalFrame(c, env, data) {
				return
			}
			continue
		}
		if !m.handlePairingFrame(c, env, data) {
			return
		}
	}
}

// handleSignalFrame processes a frame on an authenticated socket and reports
// whether the socket may continue.
func (m *Manager) handleSignalFrame(c *conn, env protocol.Envelope, data []byte) bool {
	if env.Type != protocol.TypePeerSignal {
		c.log.Warn("unexpected frame on a paired peer socket", "deviceId", c.deviceID(), "type", env.Type)
		c.closeWith(protocol.PeerCloseBadFrame, "only peer:signal is accepted on a paired socket")
		return false
	}

	var msg protocol.PeerSignalMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.closeWith(protocol.PeerCloseBadFrame, "malformed peer:signal")
		return false
	}
	if err := msg.Validate(); err != nil {
		c.log.Debug("invalid peer:signal", "deviceId", c.deviceID(), "error", err)
		c.closeWith(protocol.PeerCloseBadFrame, "invalid peer:signal")
		return false
	}
	// A paired peer may speak for itself and for nobody else. Without this
	// check a device that legitimately paired could inject signalling
	// attributed to another device and hijack its session.
	if msg.From != c.deviceID() {
		c.log.Warn("peer signal claims another device id",
			"socketDeviceId", c.deviceID(), "claimed", msg.From)
		c.closeWith(protocol.PeerCloseBadFrame, "signal from a mismatched device id")
		return false
	}

	c.log.Debug("relaying peer signal", "deviceId", c.deviceID(), "kind", msg.Kind)
	m.handler.OnSignal(msg)
	return true
}
