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

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

const (
	// sendBuffer is how many outbound frames may queue for one client before it
	// is considered wedged and disconnected.
	sendBuffer = 32
	// readLimit caps a single inbound frame. Control messages are tiny; bulk
	// payloads travel over WebRTC, never over this socket.
	readLimit = 1 << 20 // 1 MiB
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

	default:
		c.log.Debug("unsupported message type", "clientId", c.id, "type", env.Type)
		c.enqueue(protocol.NewError(env.ID, protocol.ErrCodeUnknownType,
			fmt.Sprintf("unsupported message type: %s", env.Type)))
	}
}

// Broadcast queues a message for every connected client.
//
// TODO(M5): used by discovery to push the `devices` snapshot.
func (s *Server) Broadcast(msg any) {
	for _, c := range s.hub.snapshot() {
		c.enqueue(msg)
	}
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
