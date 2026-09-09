/**
 * Sharing — shared protocol types.
 *
 * AUTHORITATIVE PROTOCOL SPEC v2 (v1 lives in `shared/PROTOCOL.md`; v2 extends
 * it). These shapes are mirrored 1:1 by the Go daemon in
 * `daemon/protocol/messages.go`. All wire field names are camelCase.
 * Do not rename, do not add fields not listed, do not change casing — a casing
 * slip here is a silent interop failure, not a compile error.
 *
 * Layout:
 *   1. Constants
 *   2. Core domain types
 *   3. Control plane  (extension <-> its own daemon, ws://127.0.0.1:8765/ws)
 *   4. DataChannel frames (browser <-> browser, peer to peer)
 *   5. Runtime messaging (background <-> side panel)
 */

// ---------------------------------------------------------------------------
// 1. Constants
// ---------------------------------------------------------------------------

/** Current wire version. The daemon must answer `ready` with this. */
export const PROTOCOL_VERSION = 2;

/**
 * The version this codebase shipped first. Kept so a future compatibility shim
 * can name the old contract instead of hard-coding `1`.
 */
export const PROTOCOL_VERSION_V1 = 1;

/** Extension version. Must match package.json / manifest version. */
export const APP_VERSION = '0.1.0';

/** Default daemon control endpoint — loopback ONLY, never 0.0.0.0. */
export const DAEMON_HOST = '127.0.0.1';
export const DAEMON_PORT = 8765;
export const DAEMON_HTTP_ORIGIN = `http://${DAEMON_HOST}:${DAEMON_PORT}`;
export const DAEMON_WS_URL = `ws://${DAEMON_HOST}:${DAEMON_PORT}/ws`;

/**
 * Daemon-to-daemon LAN port. The extension never dials it; the constant exists
 * so the number has one definition shared with the mDNS TXT records the daemon
 * publishes. That port carries signalling only — file bytes never touch it.
 */
export const DAEMON_PEER_PORT = 8766;

/**
 * Transfer chunk size — 64 KiB.
 *
 * Do NOT raise this. SCTP message-size limits differ across browsers and 64 KiB
 * is the safe interoperable ceiling for a Chrome-to-Firefox DataChannel.
 */
export const CHUNK_SIZE = 65536;

/**
 * Hard ceiling on an incoming transfer, because the receive path accumulates
 * chunks in memory and builds one Blob at the end — the cost is RAM equal to
 * the file size. Anything larger is refused with `transfer:cancel` reason
 * `too_large` rather than accepted and left to kill the tab.
 *
 * TODO(M13): stream to disk (File System Access API, or chrome.downloads) and
 * lift this limit. Until then the ceiling is stated honestly in the UI.
 */
export const MAX_IN_MEMORY_BYTES = 512 * 1024 * 1024;

/**
 * Send-loop backpressure ceiling. When `dc.bufferedAmount` exceeds this the
 * sender stops and waits for `bufferedamountlow`. See lib/transfer.ts.
 */
export const HIGH_WATER = 4 * 1024 * 1024;

/** Value written to `dc.bufferedAmountLowThreshold` — 256 KiB. */
export const BUFFERED_LOW = 262144;

/** Text at or under this size travels inline as a `text` frame, no consent. */
export const MAX_INLINE_TEXT = 65536;

/** Binary chunk header: ASCII magic, then the 36-char canonical UUID. */
export const FRAME_MAGIC = 'NSC1';
export const FRAME_MAGIC_BYTES = 4;
export const TRANSFER_ID_LENGTH = 36;
export const FRAME_HEADER_BYTES = FRAME_MAGIC_BYTES + TRANSFER_ID_LENGTH; // 40

/** A device seen less recently than this is presented as offline. */
export const DEVICE_STALE_MS = 30_000;

// ---------------------------------------------------------------------------
// 2. Core domain types
// ---------------------------------------------------------------------------

export type Platform = 'windows' | 'darwin' | 'linux' | 'unknown';
export type DeviceStatus = 'online' | 'offline';
export type ConnectionState = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'error';

/**
 * A device on the LAN, as discovered by the daemon's mDNS browser.
 *
 * v2 adds `paired`. `lastSeen` is epoch milliseconds; `status` is the daemon's
 * own verdict, which the extension re-derives from `lastSeen` against
 * DEVICE_STALE_MS when a push has gone quiet (see lib/discovery.ts).
 */
export interface Device {
  id: string;
  name: string;
  address: string;
  port: number;
  platform: Platform;
  version: string;
  status: DeviceStatus;
  lastSeen: number;
  paired: boolean;
}

export interface DaemonInfo {
  id: string;
  name: string;
  version: string;
  platform: Platform;
  protocolVersion: number;
  capabilities: string[];
}

export interface SelfDevice {
  id: string;
  name: string;
  version: string;
  platform: Platform;
}

/** GET /health response. */
export interface HealthResponse {
  status: string;
  uptimeSeconds: number;
}

// ---------------------------------------------------------------------------
// 3. Control plane — tagged union over `type`
// ---------------------------------------------------------------------------

export type WsErrorCode = 'unknown_type' | 'bad_json' | 'protocol_mismatch' | 'internal';

/** `peer:state` values (spec v2 section 3). */
export type PeerState = 'offline' | 'online' | 'pairing' | 'paired' | 'error';

/** `pair:code` direction: who started the pairing. */
export type PairDirection = 'outgoing' | 'incoming';

/** `peer:signal` kind on the daemon-to-daemon plane (spec v2 section 4). */
export type SignalKind = 'offer' | 'answer' | 'ice';

/**
 * Reasons carried by the DataChannel `transfer:cancel` frame (spec v2 §6).
 * This is the wire-authoritative list; the Go daemon mirrors it verbatim.
 */
export type TransferCancelReason = 'user_cancelled' | 'error' | 'too_large' | 'declined';

/**
 * The v1 reason list, kept because the v1 control-socket `transfer:cancel`
 * message below still declares it. New code uses TransferCancelReason.
 */
export type TransferCancelReasonV1 = 'user_cancelled' | 'peer_cancelled' | 'timeout' | 'error';

// --- Client -> Server: session ---------------------------------------------

export interface HelloMessage {
  type: 'hello';
  id: string;
  protocolVersion: number;
  client: string;
  clientVersion: string;
}

export interface PingMessage {
  type: 'ping';
  id: string;
  /** Client clock, epoch milliseconds. */
  t: number;
}

// --- Server -> Client: session ---------------------------------------------

export interface ReadyMessage {
  type: 'ready';
  id: string;
  protocolVersion: number;
  device: SelfDevice;
}

export interface PongMessage {
  type: 'pong';
  id: string;
  /** Echo of the ping's `t`. */
  t: number;
}

export interface ErrorMessage {
  type: 'error';
  id: string;
  code: WsErrorCode;
  message: string;
}

// --- Discovery -------------------------------------------------------------

/** Server push: the current discovered-device set. No `id` — unsolicited. */
export interface DevicesPush {
  type: 'devices';
  devices: Device[];
}

/** v1 name for {@link DevicesPush}. Same shape, same wire bytes. */
export type DevicesMessage = DevicesPush;

/** Client request: re-browse now instead of waiting for the next push. */
export interface DevicesRefreshMessage {
  type: 'devices:refresh';
  id: string;
}

// --- Pairing (v2) ----------------------------------------------------------

/** Client: begin pairing with the device the user just picked. */
export interface PairStartMessage {
  type: 'pair:start';
  id: string;
  deviceId: string;
}

/** Client: the human answered the six-digit code prompt. */
export interface PairConfirmMessage {
  type: 'pair:confirm';
  id: string;
  deviceId: string;
  accept: boolean;
}

/** Client: drop the stored token for a device. */
export interface PairForgetMessage {
  type: 'pair:forget';
  id: string;
  deviceId: string;
}

/**
 * Server push: show these six digits. Both ends derive the same code from the
 * nonce; it is never transmitted between daemons, only shown to each human.
 */
export interface PairCodePush {
  type: 'pair:code';
  deviceId: string;
  code: string;
  direction: PairDirection;
  name: string;
}

/** Server push: pairing settled. `reason` is empty on success. */
export interface PairResultPush {
  type: 'pair:result';
  deviceId: string;
  paired: boolean;
  reason: string;
}

/** Server push: peer-plane connectivity for one device. */
export interface PeerStatePush {
  type: 'peer:state';
  deviceId: string;
  state: PeerState;
  message: string;
}

// --- WebRTC signalling (v2) ------------------------------------------------
//
// Outbound and inbound are DELIBERATELY separate shapes. An outbound signal
// must carry `to`, because the daemon has to route it; an inbound one carries
// `from`, because we have to know who is talking. Merging them into one struct
// with both fields optional would let a caller construct a message that cannot
// be routed, and that mistake would only surface later as a connection that
// never establishes. The compiler should refuse it instead.

export interface SignalOfferOutbound {
  type: 'signal:offer';
  id: string;
  to: string;
  sdp: string;
}

export interface SignalAnswerOutbound {
  type: 'signal:answer';
  id: string;
  to: string;
  sdp: string;
}

export interface SignalIceOutbound {
  type: 'signal:ice';
  id: string;
  to: string;
  candidate: string;
  sdpMid: string;
  sdpMLineIndex: number;
}

export interface SignalOfferInbound {
  type: 'signal:offer';
  from: string;
  sdp: string;
}

export interface SignalAnswerInbound {
  type: 'signal:answer';
  from: string;
  sdp: string;
}

export interface SignalIceInbound {
  type: 'signal:ice';
  from: string;
  candidate: string;
  sdpMid: string;
  sdpMLineIndex: number;
}

export type SignalOutboundMessage = SignalOfferOutbound | SignalAnswerOutbound | SignalIceOutbound;

export type SignalInboundMessage = SignalOfferInbound | SignalAnswerInbound | SignalIceInbound;

// --- v1 RESERVED shapes ----------------------------------------------------
//
// Superseded by the v2 messages above and no longer part of the live unions,
// but kept exported so nothing that already names them breaks and so the old
// contract stays readable beside the new one.

/** v1 M6 pairing placeholder. Superseded by PairStartMessage/PairConfirmMessage. */
export interface DevicePairRequestMessage {
  type: 'device:pair:request';
  id: string;
  deviceId: string;
  code: string;
}

/** v1 M6 pairing placeholder. Superseded by PairResultPush. */
export interface DevicePairResponseMessage {
  type: 'device:pair:response';
  id: string;
  deviceId: string;
  accepted: boolean;
}

/** v1 signalling: carried `to` AND `from`. Superseded by the split pairs above. */
export interface SignalOfferMessage {
  type: 'signal:offer';
  id: string;
  to: string;
  from: string;
  sdp: string;
}

export interface SignalAnswerMessage {
  type: 'signal:answer';
  id: string;
  to: string;
  from: string;
  sdp: string;
}

export interface SignalIceMessage {
  type: 'signal:ice';
  id: string;
  to: string;
  from: string;
  candidate: string;
}

/**
 * v1 transfer control over the daemon socket. In v2 transfer control moved onto
 * the DataChannel (section 4 below), so the daemon never sees file metadata.
 */
export interface TransferStartMessage {
  type: 'transfer:start';
  id: string;
  transferId: string;
  name: string;
  mime: string;
  size: number;
}

export interface TransferProgressMessage {
  type: 'transfer:progress';
  transferId: string;
  bytes: number;
  total: number;
}

export interface TransferCompleteMessage {
  type: 'transfer:complete';
  transferId: string;
}

export interface TransferCancelMessage {
  type: 'transfer:cancel';
  transferId: string;
  reason: TransferCancelReasonV1;
}

// --- Control-plane unions --------------------------------------------------

/** Anything the extension may send to its daemon. */
export type ClientMessage =
  | HelloMessage
  | PingMessage
  | DevicesRefreshMessage
  | PairStartMessage
  | PairConfirmMessage
  | PairForgetMessage
  | SignalOfferOutbound
  | SignalAnswerOutbound
  | SignalIceOutbound;

/** Anything the daemon may send to the extension. */
export type ServerMessage =
  | ReadyMessage
  | PongMessage
  | ErrorMessage
  | DevicesPush
  | PairCodePush
  | PairResultPush
  | PeerStatePush
  | SignalOfferInbound
  | SignalAnswerInbound
  | SignalIceInbound;

export type WsMessage = ClientMessage | ServerMessage;

/** Discriminant of every server message; useful for typed `on(...)` handlers. */
export type ServerMessageType = ServerMessage['type'];

/** Narrow a `ServerMessageType` to its concrete message shape. */
export type ServerMessageOf<T extends ServerMessageType> = Extract<ServerMessage, { type: T }>;

export type ClientMessageType = ClientMessage['type'];

export type ClientMessageOf<T extends ClientMessageType> = Extract<ClientMessage, { type: T }>;

/**
 * Minimal envelope used to peek at `type`/`id` before narrowing — mirrors the
 * Go decode pattern (decode Type/ID, switch, re-unmarshal the raw bytes).
 */
export interface Envelope {
  type: string;
  id?: string;
}

// ---------------------------------------------------------------------------
// 4. DataChannel frames — browser to browser, never seen by any daemon
// ---------------------------------------------------------------------------
//
// One channel carries two frame kinds. STRING frames are the JSON control
// messages below. BINARY frames are file chunks behind the fixed 40-byte header
// described by FRAME_MAGIC / TRANSFER_ID_LENGTH above, so every chunk is
// self-describing and a receiver can drop one it does not recognise instead of
// buffering bytes it will never be able to assemble.

/** Short text, delivered without consent (see MAX_INLINE_TEXT). */
export interface TextFrame {
  type: 'text';
  text: string;
}

/** Sender announces a file, then waits for TransferAcceptFrame. */
export interface TransferStartFrame {
  type: 'transfer:start';
  transferId: string;
  name: string;
  mime: string;
  size: number;
}

/** The receiver's human answered. `accept:false` ends the transfer. */
export interface TransferAcceptFrame {
  type: 'transfer:accept';
  transferId: string;
  accept: boolean;
}

/** The receiver's running total of bytes actually held. */
export interface TransferAckFrame {
  type: 'transfer:ack';
  transferId: string;
  bytes: number;
}

/** The sender has written the last chunk. */
export interface TransferCompleteFrame {
  type: 'transfer:complete';
  transferId: string;
}

/** Either end aborts. Valid at any point after `transfer:start`. */
export interface TransferCancelFrame {
  type: 'transfer:cancel';
  transferId: string;
  reason: TransferCancelReason;
}

export type DataChannelFrame =
  | TextFrame
  | TransferStartFrame
  | TransferAcceptFrame
  | TransferAckFrame
  | TransferCompleteFrame
  | TransferCancelFrame;

export type DataChannelFrameType = DataChannelFrame['type'];

export type DataChannelFrameOf<T extends DataChannelFrameType> = Extract<
  DataChannelFrame,
  { type: T }
>;

// ---------------------------------------------------------------------------
// 5. Side panel <-> background runtime messaging
// ---------------------------------------------------------------------------

/**
 * A socket failure that is more specific than "nothing is answering".
 *
 * Without this the panel can only say "the daemon is not answering", which is a
 * lie when the daemon IS answering and simply speaks the wrong protocol
 * version — the exact situation while the daemon half of v2 is still landing.
 */
export type DaemonProblemCode = 'protocol_mismatch' | 'hello_timeout';

export interface DaemonProblem {
  code: DaemonProblemCode;
  /** Already phrased for a person. */
  message: string;
  /** Only set for `protocol_mismatch`: what the daemon said it speaks. */
  daemonProtocolVersion?: number;
}

/** Snapshot of everything the background knows, rendered by the side panel. */
export interface BackgroundState {
  /** What the socket is actually doing right now. */
  state: ConnectionState;
  info: DaemonInfo | null;
  devices: Device[];
  /**
   * The user's explicit connect/disconnect choice, NOT a connection result.
   *
   * Persisted by the background under `sharing.enabled` so a Disconnect
   * survives an MV3 service-worker restart. Deliberately distinct from
   * `state`: `enabled && state !== 'connected'` means "trying, or the daemon
   * is not running", while `!enabled` means "the user switched this off" and
   * must never be presented as a failure.
   */
  enabled: boolean;
  /**
   * Set when the daemon is reachable but unusable. Null in every other case,
   * including the ordinary "no daemon running" one.
   */
  problem: DaemonProblem | null;
}

export interface GetStateRequest {
  kind: 'getState';
}

export interface ReconnectRequest {
  kind: 'reconnect';
}

/** Sets the user's connect/disconnect choice; the background persists it. */
export interface SetEnabledRequest {
  kind: 'setEnabled';
  enabled: boolean;
}

/**
 * Panel -> background: forward this message to the daemon socket.
 *
 * RTCPeerConnection does not exist in a Chrome MV3 service worker, so all
 * WebRTC lives in the side panel document while the daemon socket stays in the
 * background. Every signalling message therefore takes one hop through runtime
 * messaging in each direction, and this is the outbound half.
 */
export interface RelayToDaemonRequest {
  kind: 'relayToDaemon';
  message: ClientMessage;
}

/** Reply to {@link RelayToDaemonRequest}. */
export interface RelayToDaemonResponse {
  ok: boolean;
  /** Present only when `ok` is false. */
  reason?: 'not_connected' | 'send_failed';
}

export type RuntimeRequest =
  | GetStateRequest
  | ReconnectRequest
  | SetEnabledRequest
  | RelayToDaemonRequest;

/** Reply to `{kind:'getState'}` — the state snapshot itself. */
export type GetStateResponse = BackgroundState;

/** Background -> side panel push when anything in the snapshot changes. */
export interface StateChangedPush extends BackgroundState {
  kind: 'stateChanged';
}

/**
 * The daemon pushes the panel actually needs. Deliberately a subset of
 * ServerMessage: `ready`, `pong` and `error` are the background's business and
 * do not belong in the panel's message loop.
 */
export type RelayableServerMessage =
  | DevicesPush
  | PairCodePush
  | PairResultPush
  | PeerStatePush
  | SignalOfferInbound
  | SignalAnswerInbound
  | SignalIceInbound;

/** Background -> side panel: a daemon push, forwarded verbatim. */
export interface DaemonPush {
  kind: 'daemonPush';
  message: RelayableServerMessage;
}

export type RuntimePush = StateChangedPush | DaemonPush;

/** Anything that can travel over browser.runtime messaging in this extension. */
export type RuntimeMessage = RuntimeRequest | RuntimePush;
