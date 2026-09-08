// Package transfer splits payloads into fixed-size chunks for the peer data
// channel and reassembles them on the receiving side.
//
// TODO(M9): implement chunked send/receive over the peer.Session data channel,
// with per-transfer progress reporting through protocol.TransferProgressMessage.
// TODO(M10): resume interrupted transfers from the last acknowledged chunk.
// TODO(M11): verify payload integrity end to end before completing a transfer.
package transfer

import (
	"context"
	"errors"
	"io"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// ChunkSize is the payload size of a single chunk, in bytes.
//
// 64 KiB matches the spec and sits just under the practical maximum message
// size for a WebRTC data channel, so a chunk always fits in one message and the
// SCTP layer never has to fragment it.
const ChunkSize = 65536

// ErrNotImplemented is returned by every function in this package until
// Milestone 9 lands.
var ErrNotImplemented = errors.New("transfer: chunked transfer not implemented yet (M9)")

// Direction says which way a transfer is moving.
type Direction string

// Transfer directions.
const (
	DirectionSend    Direction = "send"
	DirectionReceive Direction = "receive"
)

// Chunk is one framed slice of a payload.
type Chunk struct {
	// TransferID ties the chunk to its Transfer.
	TransferID string
	// Index is the zero-based chunk ordinal, used for ordering and resume.
	Index int
	// Data is at most ChunkSize bytes. The final chunk may be shorter.
	Data []byte
	// Last marks the final chunk of the payload.
	Last bool
}

// Transfer is the in-flight state of one payload moving in one direction.
type Transfer struct {
	ID        string
	Direction Direction
	Name      string
	Mime      string
	Size      int64
	Sent      int64
}

// ChunkCount reports how many chunks a payload of the given size occupies.
func ChunkCount(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return (size + ChunkSize - 1) / ChunkSize
}

// ProgressMessage renders the current state of t as a protocol push.
func (t *Transfer) ProgressMessage() protocol.TransferProgressMessage {
	return protocol.TransferProgressMessage{
		Type:       protocol.TypeTransferProgress,
		TransferID: t.ID,
		Bytes:      t.Sent,
		Total:      t.Size,
	}
}

// Split reads r and hands each ChunkSize slice to fn until r is exhausted.
// Returning an error from fn aborts the walk.
//
// TODO(M9): call from the sender once the data channel is open.
func Split(ctx context.Context, r io.Reader, transferID string, fn func(Chunk) error) error {
	_ = ctx
	_ = r
	_ = transferID
	_ = fn
	return ErrNotImplemented
}

// Assemble writes incoming chunks to w in index order, buffering any that
// arrive early.
//
// TODO(M9): call from the receiver; TODO(M10) persist partial state for resume.
func Assemble(ctx context.Context, w io.Writer, chunks <-chan Chunk) error {
	_ = ctx
	_ = w
	_ = chunks
	return ErrNotImplemented
}
