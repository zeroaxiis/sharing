/**
 * Nearby Share — shared protocol types.
 *
 * AUTHORITATIVE PROTOCOL SPEC v1. These shapes are mirrored 1:1 by the Go
 * daemon in `daemon/protocol/messages.go`. All wire field names are camelCase.
 * Do not rename, do not add fields not listed, do not change casing.
 */

export const PROTOCOL_VERSION = 1;

/** Extension version. Must match package.json / manifest version. */
export const APP_VERSION = '0.1.0';

/** Default daemon endpoint — loopback ONLY. */
export const DAEMON_HOST = '127.0.0.1';
export const DAEMON_PORT = 8765;
export const DAEMON_HTTP_ORIGIN = `http://${DAEMON_HOST}:${DAEMON_PORT}`;
export const DAEMON_WS_URL = `ws://${DAEMON_HOST}:${DAEMON_PORT}/ws`;

/** Transfer chunk size — 64 KiB. */
export const CHUNK_SIZE = 65536;

// ---------------------------------------------------------------------------
// Core domain types (spec §"Shared types — TypeScript")
// ---------------------------------------------------------------------------

export type Platform = 'windows' | 'darwin' | 'linux' | 'unknown';
export type DeviceStatus = 'online' | 'offline';
export type ConnectionState = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'error';

export interface Device {
  id: string;
  name: string;
  address: string;
  port: number;
  platform: Platform;
  version: string;
  status: DeviceStatus;
  lastSeen: number;
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
// WebSocket protocol — tagged union over `type`
// ---------------------------------------------------------------------------

export type WsErrorCode = 'unknown_type' | 'bad_json' | 'protocol_mismatch' | 'internal';

/** Reason strings carried by `transfer:cancel`. */
export type TransferCancelReason = 'user_cancelled' | 'peer_cancelled' | 'timeout' | 'error';

// --- Client -> Server (implemented in Milestone 1) -------------------------

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

// --- Server -> Client (implemented in Milestone 1) -------------------------

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

// --- RESERVED — declared now so later milestones do not rename anything ----

/** M5: server push of the current discovered-device set. Has no `id`. */
export interface DevicesMessage {
  type: 'devices';
  devices: Device[];
}

/** M6: pairing. */
export interface DevicePairRequestMessage {
  type: 'device:pair:request';
  id: string;
  deviceId: string;
  code: string;
}

export interface DevicePairResponseMessage {
  type: 'device:pair:response';
  id: string;
  deviceId: string;
  accepted: boolean;
}

/** M7: WebRTC signalling. */
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

/** M9: transfers. */
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
  reason: TransferCancelReason;
}

// --- Unions ----------------------------------------------------------------

/** Anything the extension may send to the daemon. */
export type ClientMessage =
  | HelloMessage
  | PingMessage
  | DevicePairRequestMessage
  | DevicePairResponseMessage
  | SignalOfferMessage
  | SignalAnswerMessage
  | SignalIceMessage
  | TransferStartMessage
  | TransferCancelMessage;

/** Anything the daemon may send to the extension. */
export type ServerMessage =
  | ReadyMessage
  | PongMessage
  | ErrorMessage
  | DevicesMessage
  | DevicePairRequestMessage
  | DevicePairResponseMessage
  | SignalOfferMessage
  | SignalAnswerMessage
  | SignalIceMessage
  | TransferProgressMessage
  | TransferCompleteMessage
  | TransferCancelMessage;

export type WsMessage = ClientMessage | ServerMessage;

/** Discriminant of every server message; useful for typed `on(...)` handlers. */
export type ServerMessageType = ServerMessage['type'];

/** Narrow a `ServerMessageType` to its concrete message shape. */
export type ServerMessageOf<T extends ServerMessageType> = Extract<ServerMessage, { type: T }>;

/**
 * Minimal envelope used to peek at `type`/`id` before narrowing — mirrors the
 * Go decode pattern (decode Type/ID, switch, re-unmarshal the raw bytes).
 */
export interface Envelope {
  type: string;
  id?: string;
}

// ---------------------------------------------------------------------------
// Side panel <-> background runtime messaging
// ---------------------------------------------------------------------------

/** Snapshot of everything the background knows, rendered by the side panel. */
export interface BackgroundState {
  /** What the socket is actually doing right now. */
  state: ConnectionState;
  info: DaemonInfo | null;
  devices: Device[];
  /**
   * The user's explicit connect/disconnect choice, NOT a connection result.
   *
   * Persisted by the background under `nearbyShare.enabled` so a Disconnect
   * survives an MV3 service-worker restart. Deliberately distinct from
   * `state`: `enabled && state !== 'connected'` means "trying, or the daemon
   * is not running", while `!enabled` means "the user switched this off" and
   * must never be presented as a failure.
   */
  enabled: boolean;
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

export type RuntimeRequest = GetStateRequest | ReconnectRequest | SetEnabledRequest;

/** Reply to `{kind:'getState'}` — the state snapshot itself. */
export type GetStateResponse = BackgroundState;

/** Background -> side panel push when anything in the snapshot changes. */
export interface StateChangedPush extends BackgroundState {
  kind: 'stateChanged';
}

export type RuntimePush = StateChangedPush;
