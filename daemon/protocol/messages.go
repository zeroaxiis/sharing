// Package protocol defines the wire format shared by the Sharing daemon,
// its clients (browser extension, future native clients) and other daemons on
// the LAN.
//
// There are two planes and they are deliberately different:
//
//	CONTROL plane  127.0.0.1:8765  /ws     extension <-> its own daemon
//	PEER    plane  0.0.0.0:8766    /peer   daemon    <-> daemon, across the LAN
//
// Every struct in this file mirrors the TypeScript declarations in
// extension/types/index.ts field-for-field. JSON names are camelCase and must
// never be renamed, re-cased, or extended without changing both sides at once.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// Versions
// -----------------------------------------------------------------------------

// Protocol revisions. V1 is kept so a stale client is answered with a clean
// ErrCodeProtocolMismatch instead of a confusing decode failure: the daemon can
// recognise the version it is being offered and say so precisely.
const (
	// ProtocolVersionV1 is the Milestone 1-3 control-only protocol.
	ProtocolVersionV1 = 1
	// ProtocolVersionV2 adds the peer plane, pairing and the signal relay.
	ProtocolVersionV2 = 2
)

// ProtocolVersion is the wire protocol revision spoken by this build. Peers
// announcing a different version are rejected with ErrCodeProtocolMismatch.
const ProtocolVersion = ProtocolVersionV2

// AppVersion is the daemon build version reported on the wire.
const AppVersion = "0.1.0"

// -----------------------------------------------------------------------------
// Network defaults and hard limits
// -----------------------------------------------------------------------------

const (
	// DefaultControlPort is the loopback control listener port (extension side).
	DefaultControlPort = 8765
	// DefaultPeerPort is the LAN-facing peer listener port (daemon to daemon).
	DefaultPeerPort = 8766

	// ControlWebSocketPath is the control-plane upgrade route on the loopback
	// listener.
	ControlWebSocketPath = "/ws"
	// PeerWebSocketPath is the peer-plane upgrade route on the LAN listener.
	PeerWebSocketPath = "/peer"

	// MaxFrameBytes caps a single inbound WebSocket frame on either plane. A
	// hostile peer must not be able to make the daemon allocate without bound;
	// control frames are tiny and file bytes never travel over either socket.
	MaxFrameBytes = 1 << 20 // 1 MiB

	// PeerPairAttemptsPerWindow and PeerPairRateWindow bound how often one
	// source IP may attempt pairing. Six digits is under 20 bits of entropy, so
	// an unthrottled attacker would brute-force a code in hours; five attempts
	// a minute makes that centuries.
	PeerPairAttemptsPerWindow = 5
	PeerPairRateWindow        = time.Minute

	// DeviceOnlineTTL is how long a device stays "online" after its last mDNS
	// sighting.
	DeviceOnlineTTL = 30 * time.Second
)

// mDNS / DNS-SD coordinates. The advertised port is the PEER port, never the
// control port: the control port is loopback-only and unreachable from the LAN.
const (
	ServiceType = "_sharing._tcp"
	Domain      = "local."

	TXTKeyID       = "id"    // device UUID
	TXTKeyName     = "name"  // human-readable device name
	TXTKeyVersion  = "ver"   // daemon version, e.g. "0.1.0"
	TXTKeyProtocol = "proto" // wire protocol version, e.g. "2"
	TXTKeyPlatform = "plat"  // windows | darwin | linux
)

// DataChannel framing (extension side, Milestones 8-12). Declared here so the
// Go and TypeScript sides cannot drift.
const (
	// ChunkSize is the payload size of one binary DataChannel frame. 64 KiB is
	// the safe interoperable ceiling across Chrome and Firefox SCTP limits.
	ChunkSize = 65536
	// ChunkMagic prefixes every binary DataChannel frame.
	ChunkMagic = "NSC1"
	// ChunkHeaderSize is 4 magic bytes plus a 36-char canonical UUID.
	ChunkHeaderSize = 40
	// MaxInMemoryBytes is the largest incoming transfer the current
	// accumulate-then-Blob receiver will accept. Anything larger must be
	// refused with a clear message rather than crashing the tab.
	//
	// TODO: streaming to disk (File System Access API / chrome.downloads)
	// removes this ceiling; that is its own milestone.
	MaxInMemoryBytes = 512 << 20 // 512 MiB
	// MaxInlineTextBytes is the largest text payload delivered without an
	// explicit accept from the receiving human.
	MaxInlineTextBytes = 64 << 10 // 64 KiB
)

// -----------------------------------------------------------------------------
// Enumerations
// -----------------------------------------------------------------------------

// Platform values mirror the TypeScript Platform union.
const (
	PlatformWindows = "windows"
	PlatformDarwin  = "darwin"
	PlatformLinux   = "linux"
	PlatformUnknown = "unknown"
)

// Device status values mirror the TypeScript DeviceStatus union. These describe
// mDNS reachability only; see the PeerState values for the pairing and
// connection lifecycle.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// PeerState values carried in a "peer:state" push.
const (
	PeerStateOffline = "offline"
	PeerStateOnline  = "online"
	PeerStatePairing = "pairing"
	PeerStatePaired  = "paired"
	PeerStateError   = "error"
)

// PairDirection values carried in a "pair:code" push. "outgoing" means this
// device started the pairing; "incoming" means a peer did.
const (
	PairDirectionOutgoing = "outgoing"
	PairDirectionIncoming = "incoming"
)

// SignalKind values carried in a "peer:signal" frame.
const (
	SignalKindOffer  = "offer"
	SignalKindAnswer = "answer"
	SignalKindICE    = "ice"
)

// Pairing failure reasons. Used for the "reason" field of both
// PeerPairRejectMessage (peer plane) and PairResultMessage (control plane), so
// a rejection can be relayed to the UI without translation.
const (
	PairReasonDeclined         = "declined"          // a human said no
	PairReasonTimeout          = "timeout"           // nobody answered in time
	PairReasonCodeMismatch     = "code_mismatch"     // derived codes differ
	PairReasonRateLimited      = "rate_limited"      // too many attempts
	PairReasonBusy             = "busy"              // another pairing in flight
	PairReasonProtocolMismatch = "protocol_mismatch" // peer speaks another version
	PairReasonUnreachable      = "unreachable"       // could not dial the peer
	PairReasonInternal         = "internal"          // local failure
)

// Transfer cancel reasons carried in a "transfer:cancel" frame.
const (
	CancelReasonUserCancelled = "user_cancelled"
	CancelReasonError         = "error"
	CancelReasonTooLarge      = "too_large"
	CancelReasonDeclined      = "declined"
)

// -----------------------------------------------------------------------------
// Message type discriminators carried in the envelope "type" field
// -----------------------------------------------------------------------------

// Control plane, both directions.
const (
	// v1, implemented since Milestone 2.
	TypeHello = "hello"
	TypePing  = "ping"
	TypeReady = "ready"
	TypePong  = "pong"
	TypeError = "error"

	// v2 control plane.
	TypeDevices        = "devices"         // server -> client push
	TypeDevicesRefresh = "devices:refresh" // client -> server
	TypePairStart      = "pair:start"      // client -> server
	TypePairConfirm    = "pair:confirm"    // client -> server
	TypePairForget     = "pair:forget"     // client -> server
	TypePairCode       = "pair:code"       // server -> client push
	TypePairResult     = "pair:result"     // server -> client push
	TypePeerState      = "peer:state"      // server -> client push

	// Signal relay. Carried in both directions with different shapes: a client
	// sends "to", the daemon pushes "from". See the Request/Push type pairs.
	TypeSignalOffer  = "signal:offer"
	TypeSignalAnswer = "signal:answer"
	TypeSignalICE    = "signal:ice"

	// v1 reserved, superseded by pair:start / pair:confirm. Kept so a v1 frame
	// still has a name; never emitted by this build.
	TypeDevicePairRequest  = "device:pair:request"
	TypeDevicePairResponse = "device:pair:response"
)

// Peer plane, daemon to daemon over ws://<host>:8766/peer.
const (
	TypePeerHello        = "peer:hello"         // dialer -> responder, first frame
	TypePeerReady        = "peer:ready"         // responder -> dialer, token accepted
	TypePeerPairRequired = "peer:pair:required" // responder -> dialer, pairing needed
	TypePeerPairAccept   = "peer:pair:accept"   // either -> either, human approved
	TypePeerPaired       = "peer:paired"        // responder -> dialer, token issued
	TypePeerPairReject   = "peer:pair:reject"   // either -> either, refused
	TypePeerSignal       = "peer:signal"        // either -> either, paired only
)

// DataChannel control frames (extension side). Declared here so both language
// bindings share one spelling.
const (
	TypeText             = "text"
	TypeTransferStart    = "transfer:start"
	TypeTransferAccept   = "transfer:accept"
	TypeTransferAck      = "transfer:ack"
	TypeTransferProgress = "transfer:progress"
	TypeTransferComplete = "transfer:complete"
	TypeTransferCancel   = "transfer:cancel"
)

// WebSocket close codes used by the peer listener. Application close codes live
// in the 4000-4999 range; both ends agree on these so a dialer can report why
// it was hung up on instead of "connection closed".
const (
	PeerCloseProtocolMismatch = 4000
	PeerClosePairRequired     = 4001
	PeerCloseRateLimited      = 4002
	PeerCloseBadFrame         = 4003
	PeerClosePairRejected     = 4004
	PeerCloseUnauthorized     = 4005
)

// -----------------------------------------------------------------------------
// Error codes carried in the "code" field of an ErrorMessage
// -----------------------------------------------------------------------------

const (
	// v1.
	ErrCodeUnknownType      = "unknown_type"
	ErrCodeBadJSON          = "bad_json"
	ErrCodeProtocolMismatch = "protocol_mismatch"
	ErrCodeInternal         = "internal"

	// v2.
	ErrCodeInvalidRequest  = "invalid_request"  // valid JSON, missing or blank required field
	ErrCodeUnknownDevice   = "unknown_device"   // deviceId not in the discovery cache
	ErrCodeDeviceOffline   = "device_offline"   // known device, not seen recently
	ErrCodeNotPaired       = "not_paired"       // action requires a trusted peer
	ErrCodeAlreadyPaired   = "already_paired"   // pair:start on an already trusted peer
	ErrCodePairInProgress  = "pair_in_progress" // another pairing is already running
	ErrCodePairFailed      = "pair_failed"      // pairing ended without trust
	ErrCodeRateLimited     = "rate_limited"     // too many attempts
	ErrCodePeerUnreachable = "peer_unreachable" // dial or relay to the peer failed
)

// Sentinel errors.
var (
	// ErrMissingType is returned by DecodeEnvelope when a frame parses as JSON
	// but carries no discriminator.
	ErrMissingType = errors.New("message envelope has no type field")
	// ErrInvalidMessage wraps every Validate failure so a caller can map the
	// whole class onto ErrCodeInvalidRequest with a single errors.Is.
	ErrInvalidMessage = errors.New("invalid message")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMessage, fmt.Sprintf(format, args...))
}

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

// Device is a peer discovered on the local network. Mirrors the TypeScript
// Device.
//
// Port is the peer's PEER port (8766 by default), taken from the mDNS SRV
// record: it is the only port of a remote device this daemon can dial. Paired
// reports whether the device is in the local trust store. LastSeen is epoch
// milliseconds of the last mDNS sighting.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
	Status   string `json:"status"`
	Paired   bool   `json:"paired"`
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

// Envelope is the common prefix of every WebSocket message on either plane.
// Decoding pattern: unmarshal into Envelope, switch on Type, then re-unmarshal
// the same raw bytes into the concrete message struct.
//
// Requests carry a client-generated ID and the matching response echoes it.
// Server-initiated pushes have no ID. Peer-plane frames never carry an ID.
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
// Control plane, client -> server (v1)
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
// Control plane, server -> client (v1)
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
// Control plane, server -> client pushes (v2)
// -----------------------------------------------------------------------------

// DevicesMessage is the server push carrying the current discovery snapshot. It
// is sent on every change to the discovery cache and in answer to a
// DevicesRefreshMessage.
type DevicesMessage struct {
	Type    string   `json:"type"`
	Devices []Device `json:"devices"`
}

// NewDevices builds a "devices" push. A nil slice is normalised to an empty
// array so the JSON is always `"devices":[]` and never `"devices":null`.
func NewDevices(devices []Device) DevicesMessage {
	if devices == nil {
		devices = []Device{}
	}
	return DevicesMessage{Type: TypeDevices, Devices: devices}
}

// PairCodeMessage is the push that puts the six-digit verification code in
// front of the human. Direction is PairDirectionOutgoing when this device
// started the pairing and PairDirectionIncoming when a peer did. Name is the
// peer's human-readable name, so the UI can say who is asking.
//
// The code is always derived locally with Code(); it is never taken from the
// wire.
type PairCodeMessage struct {
	Type      string `json:"type"`
	DeviceID  string `json:"deviceId"`
	Code      string `json:"code"`
	Direction string `json:"direction"`
	Name      string `json:"name"`
}

// NewPairCode builds a "pair:code" push.
func NewPairCode(deviceID, code, direction, name string) PairCodeMessage {
	return PairCodeMessage{
		Type:      TypePairCode,
		DeviceID:  deviceID,
		Code:      code,
		Direction: direction,
		Name:      name,
	}
}

// PairResultMessage reports the outcome of a pairing exchange. Reason is empty
// on success and one of the PairReason* constants on failure.
type PairResultMessage struct {
	Type     string `json:"type"`
	DeviceID string `json:"deviceId"`
	Paired   bool   `json:"paired"`
	Reason   string `json:"reason"`
}

// NewPairResult builds a "pair:result" push.
func NewPairResult(deviceID string, paired bool, reason string) PairResultMessage {
	return PairResultMessage{
		Type:     TypePairResult,
		DeviceID: deviceID,
		Paired:   paired,
		Reason:   reason,
	}
}

// PeerStateMessage reports a peer lifecycle transition. State is one of the
// PeerState* constants; Message is a human-readable detail, empty unless State
// is PeerStateError.
type PeerStateMessage struct {
	Type     string `json:"type"`
	DeviceID string `json:"deviceId"`
	State    string `json:"state"`
	Message  string `json:"message"`
}

// NewPeerState builds a "peer:state" push.
func NewPeerState(deviceID, state, message string) PeerStateMessage {
	return PeerStateMessage{
		Type:     TypePeerState,
		DeviceID: deviceID,
		State:    state,
		Message:  message,
	}
}

// -----------------------------------------------------------------------------
// Control plane, client -> server requests (v2)
// -----------------------------------------------------------------------------

// DevicesRefreshMessage asks the daemon to re-emit the discovery snapshot (and,
// if it wants, to kick a fresh mDNS query). The answer is a DevicesMessage.
type DevicesRefreshMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// PairStartMessage asks the daemon to begin pairing with a discovered device.
// The daemon dials that device's peer port and drives the peer:hello exchange.
type PairStartMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
}

// Validate reports whether the message carries the fields the handler needs.
func (m PairStartMessage) Validate() error {
	if m.DeviceID == "" {
		return invalid("pair:start requires a non-empty deviceId")
	}
	return nil
}

// PairConfirmMessage is the human's answer to a PairCodeMessage. Accept false
// is a decline and must produce a PeerPairRejectMessage on the peer plane.
//
// Nothing pairs without this message: a pairing request never auto-accepts.
type PairConfirmMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
	Accept   bool   `json:"accept"`
}

// Validate reports whether the message carries the fields the handler needs.
func (m PairConfirmMessage) Validate() error {
	if m.DeviceID == "" {
		return invalid("pair:confirm requires a non-empty deviceId")
	}
	return nil
}

// PairForgetMessage drops a device from the trust store, revoking its token.
type PairForgetMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
}

// Validate reports whether the message carries the fields the handler needs.
func (m PairForgetMessage) Validate() error {
	if m.DeviceID == "" {
		return invalid("pair:forget requires a non-empty deviceId")
	}
	return nil
}

// -----------------------------------------------------------------------------
// Control plane, signal relay (v2)
// -----------------------------------------------------------------------------
//
// The relay is asymmetric on purpose. A client says who to send TO; the daemon
// says who a frame came FROM. Modelling both directions with one struct
// carrying two optional fields would let a caller build a message with neither
// set - something the compiler could not catch and that is unroutable at
// runtime. Two types make that state unrepresentable.

// SignalOfferRequest is a client asking the daemon to relay an SDP offer to a
// paired device. Client -> server.
type SignalOfferRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	To   string `json:"to"`
	SDP  string `json:"sdp"`
}

// Validate reports whether the message can be routed.
func (m SignalOfferRequest) Validate() error {
	if m.To == "" {
		return invalid("signal:offer requires a non-empty to")
	}
	if m.SDP == "" {
		return invalid("signal:offer requires a non-empty sdp")
	}
	return nil
}

// SignalOfferPush is the daemon delivering a peer's SDP offer to its own
// client. Server -> client push, no id.
type SignalOfferPush struct {
	Type string `json:"type"`
	From string `json:"from"`
	SDP  string `json:"sdp"`
}

// NewSignalOfferPush builds a "signal:offer" push from the remote device id.
func NewSignalOfferPush(from, sdp string) SignalOfferPush {
	return SignalOfferPush{Type: TypeSignalOffer, From: from, SDP: sdp}
}

// SignalAnswerRequest is a client asking the daemon to relay an SDP answer to a
// paired device. Client -> server.
type SignalAnswerRequest struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	To   string `json:"to"`
	SDP  string `json:"sdp"`
}

// Validate reports whether the message can be routed.
func (m SignalAnswerRequest) Validate() error {
	if m.To == "" {
		return invalid("signal:answer requires a non-empty to")
	}
	if m.SDP == "" {
		return invalid("signal:answer requires a non-empty sdp")
	}
	return nil
}

// SignalAnswerPush is the daemon delivering a peer's SDP answer to its own
// client. Server -> client push, no id.
type SignalAnswerPush struct {
	Type string `json:"type"`
	From string `json:"from"`
	SDP  string `json:"sdp"`
}

// NewSignalAnswerPush builds a "signal:answer" push from the remote device id.
func NewSignalAnswerPush(from, sdp string) SignalAnswerPush {
	return SignalAnswerPush{Type: TypeSignalAnswer, From: from, SDP: sdp}
}

// SignalICERequest is a client asking the daemon to relay one trickled ICE
// candidate to a paired device. Client -> server.
type SignalICERequest struct {
	Type          string `json:"type"`
	ID            string `json:"id,omitempty"`
	To            string `json:"to"`
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
}

// Validate reports whether the message can be routed. An empty candidate is
// rejected: end-of-candidates is signalled by simply not sending more, so a
// blank candidate would only waste a round trip.
func (m SignalICERequest) Validate() error {
	if m.To == "" {
		return invalid("signal:ice requires a non-empty to")
	}
	if m.Candidate == "" {
		return invalid("signal:ice requires a non-empty candidate")
	}
	return nil
}

// SignalICEPush is the daemon delivering a peer's ICE candidate to its own
// client. Server -> client push, no id.
type SignalICEPush struct {
	Type          string `json:"type"`
	From          string `json:"from"`
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
}

// NewSignalICEPush builds a "signal:ice" push from the remote device id.
func NewSignalICEPush(from, candidate, sdpMid string, sdpMLineIndex int) SignalICEPush {
	return SignalICEPush{
		Type:          TypeSignalICE,
		From:          from,
		Candidate:     candidate,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	}
}

// -----------------------------------------------------------------------------
// Peer plane, daemon <-> daemon (v2)
// -----------------------------------------------------------------------------

// PeerHelloMessage is the first frame on a peer socket, sent by the dialer.
// Nothing else is accepted before it.
//
// Token is the long-lived credential issued the last time these two devices
// paired, and is omitted entirely when this device has never paired with the
// responder.
type PeerHelloMessage struct {
	Type            string `json:"type"`
	DeviceID        string `json:"deviceId"`
	Name            string `json:"name"`
	Platform        string `json:"platform"`
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocolVersion"`
	Token           string `json:"token,omitempty"`
}

// NewPeerHello builds a "peer:hello". Pass an empty token when this device has
// never paired with the responder; the field is then omitted from the JSON.
func NewPeerHello(self SelfDevice, token string) PeerHelloMessage {
	return PeerHelloMessage{
		Type:            TypePeerHello,
		DeviceID:        self.ID,
		Name:            self.Name,
		Platform:        self.Platform,
		Version:         self.Version,
		ProtocolVersion: ProtocolVersion,
		Token:           token,
	}
}

// Validate checks the frame before any trust decision is made. It deliberately
// does NOT compare the token value: that comparison must go through
// config.TrustStore.VerifyToken, which is constant time.
func (m PeerHelloMessage) Validate() error {
	if m.DeviceID == "" {
		return invalid("peer:hello requires a non-empty deviceId")
	}
	if m.ProtocolVersion != ProtocolVersion {
		return invalid("peer:hello protocol version %d, this daemon speaks %d",
			m.ProtocolVersion, ProtocolVersion)
	}
	if m.Token != "" && !ValidTokenFormat(m.Token) {
		return invalid("peer:hello token must be %d lowercase hex characters", TokenHexLen)
	}
	return nil
}

// PeerReadyMessage answers a PeerHelloMessage whose token matched. The socket
// is trusted from this point and may carry PeerSignalMessage frames.
type PeerReadyMessage struct {
	Type     string `json:"type"`
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
}

// NewPeerReady builds a "peer:ready" carrying this device's own identity.
func NewPeerReady(self SelfDevice) PeerReadyMessage {
	return PeerReadyMessage{Type: TypePeerReady, DeviceID: self.ID, Name: self.Name}
}

// PeerPairRequiredMessage answers a PeerHelloMessage with no token, an unknown
// token, or a token that failed verification. Until pairing completes, nothing
// but the pairing frames is accepted on the socket.
//
// Nonce is 32 hex characters (16 random bytes) chosen by the responder and is
// the only value that has to cross the wire for both ends to derive the code.
//
// Code is present because the spec's frame layout lists it, but it is NOT an
// input to any trust decision. The dialer MUST derive its own code with Code()
// and show only that to its human. If the received Code is non-empty and
// differs from the derived one, the dialer MUST abort with
// PairReasonCodeMismatch rather than display either value.
type PeerPairRequiredMessage struct {
	Type  string `json:"type"`
	Code  string `json:"code"`
	Nonce string `json:"nonce"`
}

// NewPeerPairRequired builds a "peer:pair:required".
func NewPeerPairRequired(code, nonce string) PeerPairRequiredMessage {
	return PeerPairRequiredMessage{Type: TypePeerPairRequired, Code: code, Nonce: nonce}
}

// Validate checks the nonce shape. A malformed nonce means the two sides cannot
// possibly derive the same code, so fail loudly instead of showing six digits
// that will never match.
func (m PeerPairRequiredMessage) Validate() error {
	if !ValidNonceFormat(m.Nonce) {
		return invalid("peer:pair:required nonce must be %d lowercase hex characters", NonceHexLen)
	}
	if m.Code != "" && !ValidCodeFormat(m.Code) {
		return invalid("peer:pair:required code must be %d digits when present", CodeDigits)
	}
	return nil
}

// PeerPairAcceptMessage is sent by each side once its own human approves the
// displayed code. DeviceID is the sender's own device id.
type PeerPairAcceptMessage struct {
	Type     string `json:"type"`
	DeviceID string `json:"deviceId"`
}

// NewPeerPairAccept builds a "peer:pair:accept" carrying this device's id.
func NewPeerPairAccept(selfID string) PeerPairAcceptMessage {
	return PeerPairAcceptMessage{Type: TypePeerPairAccept, DeviceID: selfID}
}

// Validate reports whether the accept identifies its sender.
func (m PeerPairAcceptMessage) Validate() error {
	if m.DeviceID == "" {
		return invalid("peer:pair:accept requires a non-empty deviceId")
	}
	return nil
}

// PeerPairedMessage is sent by the responder once both humans have accepted. It
// carries the long-lived token, which both sides then store: the responder
// against the dialer's device id, the dialer against the responder's.
type PeerPairedMessage struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

// NewPeerPaired builds a "peer:paired" carrying a freshly minted token.
func NewPeerPaired(token string) PeerPairedMessage {
	return PeerPairedMessage{Type: TypePeerPaired, Token: token}
}

// Validate checks the token shape before it is written to the trust store.
func (m PeerPairedMessage) Validate() error {
	if !ValidTokenFormat(m.Token) {
		return invalid("peer:paired token must be %d lowercase hex characters", TokenHexLen)
	}
	return nil
}

// PeerPairRejectMessage refuses a pairing. Reason is one of the PairReason*
// constants and is relayed verbatim into PairResultMessage.Reason.
type PeerPairRejectMessage struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// NewPeerPairReject builds a "peer:pair:reject". An empty reason becomes
// PairReasonDeclined so the far side never has to interpret a blank string.
func NewPeerPairReject(reason string) PeerPairRejectMessage {
	if reason == "" {
		reason = PairReasonDeclined
	}
	return PeerPairRejectMessage{Type: TypePeerPairReject, Reason: reason}
}

// PeerSignalMessage relays one WebRTC signalling item between two paired
// daemons. It is accepted only on a socket that has completed the handshake.
//
// From is the sending daemon's device id. Kind selects which fields matter:
//
//	SignalKindOffer, SignalKindAnswer -> SDP set, Candidate ""
//	SignalKindICE                     -> Candidate, SDPMid, SDPMLineIndex set, SDP ""
//
// Every field is always present on the wire (no omitempty) so the shape stays
// stable for the TypeScript side.
type PeerSignalMessage struct {
	Type          string `json:"type"`
	From          string `json:"from"`
	Kind          string `json:"kind"`
	SDP           string `json:"sdp"`
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
}

// NewPeerSignalOffer builds a "peer:signal" carrying an SDP offer.
func NewPeerSignalOffer(from, sdp string) PeerSignalMessage {
	return PeerSignalMessage{Type: TypePeerSignal, From: from, Kind: SignalKindOffer, SDP: sdp}
}

// NewPeerSignalAnswer builds a "peer:signal" carrying an SDP answer.
func NewPeerSignalAnswer(from, sdp string) PeerSignalMessage {
	return PeerSignalMessage{Type: TypePeerSignal, From: from, Kind: SignalKindAnswer, SDP: sdp}
}

// NewPeerSignalICE builds a "peer:signal" carrying one ICE candidate.
func NewPeerSignalICE(from, candidate, sdpMid string, sdpMLineIndex int) PeerSignalMessage {
	return PeerSignalMessage{
		Type:          TypePeerSignal,
		From:          from,
		Kind:          SignalKindICE,
		Candidate:     candidate,
		SDPMid:        sdpMid,
		SDPMLineIndex: sdpMLineIndex,
	}
}

// Validate reports whether the frame is internally consistent for its kind.
func (m PeerSignalMessage) Validate() error {
	if m.From == "" {
		return invalid("peer:signal requires a non-empty from")
	}
	switch m.Kind {
	case SignalKindOffer, SignalKindAnswer:
		if m.SDP == "" {
			return invalid("peer:signal kind %q requires a non-empty sdp", m.Kind)
		}
	case SignalKindICE:
		if m.Candidate == "" {
			return invalid("peer:signal kind %q requires a non-empty candidate", m.Kind)
		}
	default:
		return invalid("peer:signal unknown kind %q", m.Kind)
	}
	return nil
}

// ToOfferPush converts an inbound peer signal into the control-plane push the
// local client expects. ok is false when Kind is not SignalKindOffer.
func (m PeerSignalMessage) ToOfferPush() (msg SignalOfferPush, ok bool) {
	if m.Kind != SignalKindOffer {
		return SignalOfferPush{}, false
	}
	return NewSignalOfferPush(m.From, m.SDP), true
}

// ToAnswerPush converts an inbound peer signal into the control-plane push the
// local client expects. ok is false when Kind is not SignalKindAnswer.
func (m PeerSignalMessage) ToAnswerPush() (msg SignalAnswerPush, ok bool) {
	if m.Kind != SignalKindAnswer {
		return SignalAnswerPush{}, false
	}
	return NewSignalAnswerPush(m.From, m.SDP), true
}

// ToICEPush converts an inbound peer signal into the control-plane push the
// local client expects. ok is false when Kind is not SignalKindICE.
func (m PeerSignalMessage) ToICEPush() (msg SignalICEPush, ok bool) {
	if m.Kind != SignalKindICE {
		return SignalICEPush{}, false
	}
	return NewSignalICEPush(m.From, m.Candidate, m.SDPMid, m.SDPMLineIndex), true
}

// -----------------------------------------------------------------------------
// DataChannel control frames (extension side; declared here for parity)
// -----------------------------------------------------------------------------

// TextMessage is a short text payload delivered inline, without an accept step,
// as long as it is under MaxInlineTextBytes.
type TextMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// TransferStartMessage announces a payload and waits for consent.
type TransferStartMessage struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	TransferID string `json:"transferId"`
	Name       string `json:"name"`
	Mime       string `json:"mime"`
	Size       int64  `json:"size"`
}

// TransferAcceptMessage is the receiving human's consent decision. Bytes do not
// flow until Accept is true.
type TransferAcceptMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Accept     bool   `json:"accept"`
}

// TransferAckMessage reports how many bytes the receiver has committed.
type TransferAckMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Bytes      int64  `json:"bytes"`
}

// TransferProgressMessage is a control-plane push reporting bytes moved so far.
type TransferProgressMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Bytes      int64  `json:"bytes"`
	Total      int64  `json:"total"`
}

// TransferCompleteMessage marks a transfer finished.
type TransferCompleteMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
}

// TransferCancelMessage aborts a transfer in either direction. Reason is one of
// the CancelReason* constants.
type TransferCancelMessage struct {
	Type       string `json:"type"`
	TransferID string `json:"transferId"`
	Reason     string `json:"reason"`
}

// -----------------------------------------------------------------------------
// v1 reserved types, superseded but kept decodable
// -----------------------------------------------------------------------------

// DevicePairRequestMessage is the v1 pairing request.
//
// Deprecated: superseded by PairStartMessage and PairCodeMessage in protocol
// v2. Retained so a v1 frame still has a named shape while the daemon answers
// ErrCodeProtocolMismatch.
type DevicePairRequestMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
	Code     string `json:"code"`
}

// DevicePairResponseMessage is the v1 pairing response.
//
// Deprecated: superseded by PairConfirmMessage and PairResultMessage in
// protocol v2.
type DevicePairResponseMessage struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"deviceId"`
	Accepted bool   `json:"accepted"`
}
