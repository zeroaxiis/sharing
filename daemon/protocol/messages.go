// Package protocol defines the wire format shared by the Nearby Share daemon
// and its clients (browser extension, future native clients).
//
// Every struct in this file mirrors the TypeScript declarations in
// extension/types/index.ts field-for-field. JSON names are camelCase and must
// never be renamed, re-cased, or extended without changing both sides at once.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the wire protocol revision spoken by this build. Peers
// announcing a different version are rejected with ErrCodeProtocolMismatch.
const ProtocolVersion = 1

// Platform values mirror the TypeScript Platform union.
const (
	PlatformWindows = "windows"
	PlatformDarwin  = "darwin"
	PlatformLinux   = "linux"
	PlatformUnknown = "unknown"
)

// Device status values mirror the TypeScript DeviceStatus union.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// Message type discriminators carried in the envelope "type" field.
const (
	// Implemented in Milestone 2.
	TypeHello = "hello"
	TypePing  = "ping"
	TypeReady = "ready"
	TypePong  = "pong"
	TypeError = "error"

	// Reserved. Declared now so later milestones never have to rename a type.
	TypeDevices            = "devices"              // M5 push
	TypeDevicePairRequest  = "device:pair:request"  // M6
	TypeDevicePairResponse = "device:pair:response" // M6
	TypeSignalOffer        = "signal:offer"         // M7
	TypeSignalAnswer       = "signal:answer"        // M7
	TypeSignalICE          = "signal:ice"           // M7
	TypeTransferStart      = "transfer:start"       // M9
	TypeTransferProgress   = "transfer:progress"    // M9
	TypeTransferComplete   = "transfer:complete"    // M9
	TypeTransferCancel     = "transfer:cancel"      // M9
)

// Error codes carried in the "code" field of an ErrorMessage.
const (
	ErrCodeUnknownType      = "unknown_type"
	ErrCodeBadJSON          = "bad_json"
	ErrCodeProtocolMismatch = "protocol_mismatch"
	ErrCodeInternal         = "internal"
)

// ErrMissingType is returned by DecodeEnvelope when a frame parses as JSON but
// carries no discriminator.
var ErrMissingType = errors.New("message envelope has no type field")

// -----------------------------------------------------------------------------
// Shared value types
// -----------------------------------------------------------------------------

// DaemonInfo is the payload of GET /info. Mirrors the TypeScript DaemonInfo.
type DaemonInfo struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Platform        string   `json:"platform"`
	ProtocolVersion int      `json:"protocolVersion"`
	Capabilities    []string `json:"capabilities"`
}

// Device is a peer discovered on the local network. Mirrors the TypeScript Device.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
	Status   string `json:"status"`
	LastSeen int64  `json:"lastSeen"`
}

// SelfDevice is this daemon own identity, as embedded in a ReadyMessage.
// Mirrors the TypeScript SelfDevice.
type SelfDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

// HealthResponse is the payload of GET /health.
type HealthResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

// HTTPError is the payload returned for HTTP failures, e.g. {"error":"not found"}.
type HTTPError struct {
	Error string `json:"error"`
}

// -----------------------------------------------------------------------------
// Envelope
// -----------------------------------------------------------------------------

// Envelope is the common prefix of every WebSocket message. Decoding pattern:
// unmarshal into Envelope, switch on Type, then re-unmarshal the same raw bytes
// into the concrete message struct.
//
// Requests carry a client-generated ID and the matching response echoes it.
// Server-initiated pushes have no ID.
type Envelope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// DecodeEnvelope extracts the type/id prefix from a raw WebSocket frame. The
// caller keeps the raw bytes for the second, concrete unmarshal.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, fmt.Errorf("decode envelope: %w", ErrMissingType)
	}
	return env, nil
}

// -----------------------------------------------------------------------------
// Client -> Server (implemented)
// -----------------------------------------------------------------------------

// HelloMessage is the handshake a client sends immediately after connecting.
type HelloMessage struct {
	Type            string `json:"type"`
	ID              string `json:"id,omitempty"`
	ProtocolVersion int    `json:"protocolVersion"`
	Client          string `json:"client"`
	ClientVersion   string `json:"clientVersion"`
}

// PingMessage is a liveness probe. T is a client clock reading in milliseconds
// which the daemon echoes verbatim so the client can measure round-trip time.
type PingMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	T    int64  `json:"t"`
}

// -----------------------------------------------------------------------------
// Server -> Client (implemented)
// -----------------------------------------------------------------------------

// ReadyMessage answers a HelloMessage once the handshake is accepted.
type ReadyMessage struct {
	Type            string     `json:"type"`
	ID              string     `json:"id,omitempty"`
	ProtocolVersion int        `json:"protocolVersion"`
	Device          SelfDevice `json:"device"`
}

// PongMessage answers a PingMessage, echoing both ID and T.
type PongMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	T    int64  `json:"t"`
}

// ErrorMessage reports a failure. ID echoes the offending request when known.
type ErrorMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewReady builds a ReadyMessage answering the request carrying the given id.
func NewReady(id string, self SelfDevice) ReadyMessage {
	return ReadyMessage{
		Type:            TypeReady,
		ID:              id,
		ProtocolVersion: ProtocolVersion,
		Device:          self,
	}
}

// NewPong builds a PongMessage echoing the request id and timestamp.
func NewPong(id string, t int64) PongMessage {
	return PongMessage{Type: TypePong, ID: id, T: t}
}

// NewError builds an ErrorMessage. Pass an empty id for failures that answer no
// identifiable request, such as a frame that did not parse as JSON at all.
func NewError(id, code, message string) ErrorMessage {
	return ErrorMessage{Type: TypeError, ID: id, Code: code, Message: message}
}

// -----------------------------------------------------------------------------
// RESERVED - declared now, wired up in later milestones.
// -----------------------------------------------------------------------------

// DevicesMessage is the server push carrying the current discovery snapshot.
//
// TODO(M5): emit on every discovery-cache change.
type DevicesMessage struct {
	Type    string   `json:"type"`
	Devices []Device `json:"devices"`
}

// DevicePairRequestMessage asks a peer to confirm a six-digit pairing code.
//
// TODO(M6): wire up to the pairing store.
type DevicePairRequestMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
	Code     string `json:"code"`
}

// DevicePairResponseMessage reports the outcome of a pairing request.
//
// TODO(M6): wire up to the pairing store.
type DevicePairResponseMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
	Accepted bool   `json:"accepted"`
}

// SignalOfferMessage relays a WebRTC SDP offer between peers.
//
// TODO(M7): route through the peer package.
type SignalOfferMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	To   string `json:"to"`
	From string `json:"from"`
	SDP  string `json:"sdp"`
}

// SignalAnswerMessage relays a WebRTC SDP answer between peers.
//
// TODO(M7): route through the peer package.
type SignalAnswerMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	To   string `json:"to"`
	From string `json:"from"`
	SDP  string `json:"sdp"`
}

// SignalICEMessage relays a single WebRTC ICE candidate between peers.
//
// TODO(M7): route through the peer package.
type SignalICEMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	To        string `json:"to"`
	From      string `json:"from"`
	Candidate string `json:"candidate"`
}

// TransferStartMessage announces an incoming or outgoing payload.
//
// TODO(M9): wire up to the transfer package.
type TransferStartMessage struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	TransferID string `json:"transferId"`
	Name       string `json:"name"`
	Mime       string `json:"mime"`
	Size       int64  `json:"size"`
}

// TransferProgressMessage is a server push reporting bytes moved so far.
//
// TODO(M9): emit from the transfer package.
type TransferProgressMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Bytes      int64  `json:"bytes"`
	Total      int64  `json:"total"`
}

// TransferCompleteMessage is a server push marking a transfer finished.
//
// TODO(M9): emit from the transfer package.
type TransferCompleteMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
}

// TransferCancelMessage aborts a transfer in either direction.
//
// TODO(M9): wire up to the transfer package.
type TransferCancelMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Reason     string `json:"reason"`
}
