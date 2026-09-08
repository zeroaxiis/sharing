// Package peer owns the direct device-to-device data path.
//
// TODO(M7): establish a WebRTC DataChannel between two paired devices, using
// the daemon WebSocket purely as the signalling channel (the signal:offer,
// signal:answer and signal:ice message types already reserved in the protocol
// package). Bulk payload bytes must never travel over the control socket:
// keeping them on the peer connection is what makes transfers direct, keeps
// them off the loopback HTTP server, and lets the two ends negotiate their own
// encryption via DTLS-SRTP.
package peer

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// ErrNotImplemented is returned by every function in this package until
// Milestone 7 lands.
var ErrNotImplemented = errors.New("peer: WebRTC transport not implemented yet (M7)")

// State is the lifecycle of a peer connection.
type State string

// Peer connection states.
const (
	StateNew          State = "new"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateDisconnected State = "disconnected"
	StateFailed       State = "failed"
	StateClosed       State = "closed"
)

// Session is one live peer connection to a remote device.
type Session interface {
	// State reports the current connection state.
	State() State
	// Close tears the peer connection down.
	Close() error
}

// Signaller carries locally generated SDP and ICE out to the remote device.
type Signaller interface {
	SendOffer(ctx context.Context, msg protocol.SignalOfferMessage) error
	SendAnswer(ctx context.Context, msg protocol.SignalAnswerMessage) error
	SendICE(ctx context.Context, msg protocol.SignalICEMessage) error
}

// Options configures a Session.
type Options struct {
	// SelfID is this daemon device id, used as the From field in signalling.
	SelfID string
	// RemoteID is the paired peer device id.
	RemoteID string
	// Signaller relays signalling messages to the remote device.
	Signaller Signaller
	// Logger receives structured logs. Nil uses slog.Default.
	Logger *slog.Logger
}

// Dial starts an outbound peer connection and sends the initial offer.
//
// TODO(M7): create the PeerConnection, open the data channel, and drive the
// offer/answer exchange through Options.Signaller.
func Dial(ctx context.Context, opts Options) (Session, error) {
	_ = ctx
	_ = opts
	return nil, ErrNotImplemented
}

// Accept answers an inbound offer relayed over the signalling channel.
//
// TODO(M7): set the remote description, produce an answer, and send it back.
func Accept(ctx context.Context, opts Options, offer protocol.SignalOfferMessage) (Session, error) {
	_ = ctx
	_ = opts
	_ = offer
	return nil, ErrNotImplemented
}
