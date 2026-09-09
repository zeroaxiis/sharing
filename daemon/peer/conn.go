package peer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// Errors raised while moving frames over a peer socket.
var (
	// errBinaryFrame is returned when a peer sends a binary WebSocket frame.
	//
	// This is the enforcement point for the rule in the spec: the peer plane
	// carries signalling JSON and nothing else. File bytes travel over the
	// WebRTC DataChannel the two browsers negotiate, never through the daemon,
	// so a binary frame here is either a broken implementation or an attempt to
	// push payload through this process. Both are refused.
	errBinaryFrame = errors.New("peer: binary frames are not accepted on the peer plane")

	// errConnClosed is returned when a frame is queued on a socket that is
	// already going away.
	errConnClosed = errors.New("peer: connection is closed")
)

// outFrame is one item on a connection's write queue. A frame either carries
// data or asks the writer to close, which keeps the close ordered behind
// everything already queued: a peer:pair:reject sent immediately before a close
// still reaches the far side.
type outFrame struct {
	data       []byte
	closeFrame bool
	status     websocket.StatusCode
	reason     string
}

// remoteIdentity is who the far side claims to be, learned from peer:hello (or
// known up front when we dialled). It is stored behind an atomic pointer
// because the writer goroutine is already running when the handshake fills it
// in, and the manager reads it from other goroutines entirely.
type remoteIdentity struct {
	id   string
	name string
}

// conn is one peer-plane WebSocket connection, inbound or outbound.
//
// Exactly one goroutine reads (the handshake goroutine, which then becomes the
// frame loop) and exactly one goroutine writes (writeLoop, fed by send), so the
// socket never sees concurrent access from the many goroutines that want to
// push a frame: the pairing driver, the signal relay and shutdown.
type conn struct {
	mgr        *Manager
	ws         *websocket.Conn
	log        *slog.Logger
	outbound   bool
	remoteAddr string

	remote atomic.Pointer[remoteIdentity]

	send   chan outFrame
	ctx    context.Context
	cancel context.CancelFunc

	// paired flips to true the instant the socket is authenticated, either by a
	// verified token or by a completed pairing. The frame loop reads it to
	// decide whether a frame belongs to the pairing exchange or the signal
	// relay, so nothing but pairing frames is ever accepted before it is set.
	paired atomic.Bool

	// writerDone closes when writeLoop has stopped, which is how closeWith
	// knows the close frame has reached the wire.
	writerDone chan struct{}

	closeOnce sync.Once
}

// newConn wraps an accepted or dialled socket. The caller must start writeLoop.
func newConn(m *Manager, parent context.Context, ws *websocket.Conn, outbound bool, remoteAddr string) *conn {
	// Cap every inbound frame, in both directions of the peer plane. Without
	// this a hostile peer can hand the daemon a multi-gigabyte "JSON" frame and
	// have it buffered in full before a single byte is parsed.
	ws.SetReadLimit(protocol.MaxFrameBytes)

	ctx, cancel := context.WithCancel(parent)
	c := &conn{
		mgr:        m,
		ws:         ws,
		log:        m.log.With("remoteAddr", remoteAddr, "outbound", outbound),
		outbound:   outbound,
		remoteAddr: remoteAddr,
		send:       make(chan outFrame, sendBuffer),
		ctx:        ctx,
		cancel:     cancel,
		writerDone: make(chan struct{}),
	}
	m.track(c)
	return c
}

// setRemote records who the far side is.
func (c *conn) setRemote(id, name string) {
	c.remote.Store(&remoteIdentity{id: id, name: name})
}

// deviceID is the far side's device id, empty until the handshake names it.
func (c *conn) deviceID() string {
	if r := c.remote.Load(); r != nil {
		return r.id
	}
	return ""
}

// readFrame reads one text frame. A positive timeout bounds the wait, which is
// what stops a peer from opening a socket, saying nothing, and pinning a
// goroutine and a file descriptor for as long as it likes.
func (c *conn) readFrame(timeout time.Duration) ([]byte, error) {
	ctx := c.ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(c.ctx, timeout)
		defer cancel()
	}

	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errBinaryFrame
	}
	return data, nil
}

// enqueue marshals msg and hands it to writeLoop.
func (c *conn) enqueue(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		c.log.Error("encode outbound peer frame", "error", err)
		return err
	}
	return c.enqueueFrame(outFrame{data: data})
}

func (c *conn) enqueueFrame(f outFrame) error {
	select {
	case <-c.ctx.Done():
		return errConnClosed
	default:
	}

	select {
	case c.send <- f:
		return nil
	case <-c.ctx.Done():
		return errConnClosed
	default:
		// A peer that cannot keep up with a handful of tiny JSON frames is
		// wedged. Dropping it beats growing an unbounded queue on its behalf.
		c.log.Warn("peer send buffer full, dropping connection", "deviceId", c.deviceID())
		c.hardClose()
		return errConnClosed
	}
}

// closeWith closes the socket with an application close code, letting anything
// already queued (a peer:pair:reject, say) go out first.
func (c *conn) closeWith(status int, reason string) {
	code := websocket.StatusCode(status)
	if err := c.enqueueFrame(outFrame{closeFrame: true, status: code, reason: reason}); err != nil {
		c.hardClose()
		return
	}

	// Wait for the writer to put the close frame on the wire before the socket
	// is torn down. It is the difference between the far side learning that its
	// pairing was rejected, or rate limited, and it seeing a mystery EOF. The
	// wait is bounded: a peer that has stopped reading does not get to hold a
	// goroutine here.
	select {
	case <-c.writerDone:
	case <-time.After(closeGrace):
	}
	c.hardClose()
}

// hardClose tears the socket down immediately and unblocks every goroutine
// hanging off it. Safe from any goroutine, any number of times.
func (c *conn) hardClose() {
	c.closeOnce.Do(func() {
		if err := c.ws.CloseNow(); err != nil {
			// A peer that already vanished is routine, not worth a warning.
			c.log.Debug("peer socket close", "deviceId", c.deviceID(), "error", err)
		}
		c.cancel()
		c.mgr.untrack(c)
	})
}

// writeLoop is the only goroutine that writes to the socket. It also owns the
// keepalive ping, so a ping can never race a data frame, and a half-open TCP
// connection is noticed instead of lingering until the OS gives up on it.
func (c *conn) writeLoop() {
	defer close(c.writerDone)
	defer c.cancel()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case f := <-c.send:
			if f.closeFrame {
				if err := c.ws.Close(f.status, f.reason); err != nil {
					c.log.Debug("peer close handshake", "error", err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, f.data)
			cancel()
			if err != nil {
				c.log.Debug("peer write failed", "error", err)
				return
			}

		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, keepaliveTimeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.log.Debug("peer keepalive failed, dropping socket", "error", err)
				return
			}
		}
	}
}
