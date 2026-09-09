/**
 * DaemonClient — owns the connection to the local Sharing daemon.
 *
 * The daemon listens on loopback only (http://127.0.0.1:8765). This client is
 * instantiated by the BACKGROUND script, never by the side panel: the panel is
 * torn down whenever it closes, so a panel-owned socket would reconnect on
 * every open. See `entrypoints/background.ts`.
 *
 * Everything here is defensive: with no daemon running (the normal case in
 * Milestone 1) nothing throws and nothing rejects unhandled — the client simply
 * settles into `reconnecting` and keeps retrying with backoff.
 */

import {
  APP_VERSION,
  DAEMON_HTTP_ORIGIN,
  DAEMON_PORT,
  DAEMON_WS_URL,
  PROTOCOL_VERSION,
  type ClientMessage,
  type ConnectionState,
  type DaemonInfo,
  type DaemonProblem,
  type Envelope,
  type Platform,
  type ServerMessage,
  type ServerMessageOf,
  type ServerMessageType,
} from '@/types';

/** Backoff schedule in ms: 500, 1s, 2s, 4s, ... capped at 15s, plus jitter. */
const BACKOFF_BASE_MS = 500;
const BACKOFF_MAX_MS = 15_000;
/** Up to 20% jitter either way so retries do not land in lockstep. */
const BACKOFF_JITTER = 0.2;

/** Keepalive ping cadence. */
const PING_INTERVAL_MS = 20_000;

/** How long we wait for `ready` before treating the socket as broken. */
const HELLO_TIMEOUT_MS = 5_000;

/** How long a GET /info may take before we give up. */
const FETCH_TIMEOUT_MS = 3_000;

export interface DaemonClientOptions {
  httpOrigin?: string;
  wsUrl?: string;
  /** Called on every ConnectionState transition. */
  onStateChange?: (state: ConnectionState) => void;
  /** Called whenever a fresh DaemonInfo is learned (via `ready` or /info). */
  onInfo?: (info: DaemonInfo | null) => void;
  /**
   * Called when the daemon is reachable but unusable, and again with null once
   * it becomes usable. `state === 'reconnecting'` alone cannot distinguish "no
   * daemon" from "wrong protocol version", and telling a user to start a daemon
   * that is already running is the kind of wrong that costs an hour.
   */
  onProblem?: (problem: DaemonProblem | null) => void;
}

type MessageHandler<T extends ServerMessageType> = (message: ServerMessageOf<T>) => void;

/**
 * Handlers are stored erased to a common shape because a Map cannot express
 * "each key maps to a handler of its own message type". `on()` and `emit()`
 * re-establish that correspondence, so the cast stays contained to two places.
 */
type ErasedHandler = (message: ServerMessage) => void;

export class DaemonClient {
  private readonly httpOrigin: string;
  private readonly wsUrl: string;

  private socket: WebSocket | null = null;
  private state: ConnectionState = 'idle';
  private info: DaemonInfo | null = null;
  private problem: DaemonProblem | null = null;

  private readonly handlers = new Map<ServerMessageType, Set<ErasedHandler>>();
  private readonly stateListeners = new Set<(state: ConnectionState) => void>();
  private readonly infoListeners = new Set<(info: DaemonInfo | null) => void>();
  private readonly problemListeners = new Set<(problem: DaemonProblem | null) => void>();

  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private helloTimer: ReturnType<typeof setTimeout> | null = null;

  /** Set by close(); suppresses all further reconnect scheduling. */
  private closed = false;

  constructor(options: DaemonClientOptions = {}) {
    this.httpOrigin = options.httpOrigin ?? DAEMON_HTTP_ORIGIN;
    this.wsUrl = options.wsUrl ?? DAEMON_WS_URL;
    if (options.onStateChange) this.stateListeners.add(options.onStateChange);
    if (options.onInfo) this.infoListeners.add(options.onInfo);
    if (options.onProblem) this.problemListeners.add(options.onProblem);
  }

  // -- public surface -------------------------------------------------------

  getState(): ConnectionState {
    return this.state;
  }

  getInfo(): DaemonInfo | null {
    return this.info;
  }

  getProblem(): DaemonProblem | null {
    return this.problem;
  }

  isConnected(): boolean {
    return this.state === 'connected' && this.socket?.readyState === WebSocket.OPEN;
  }

  /**
   * GET /info. Resolves to null when the daemon is unreachable or replies with
   * something we cannot use — it never rejects.
   */
  async fetchInfo(): Promise<DaemonInfo | null> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
    try {
      const response = await fetch(this.httpOrigin + '/info', {
        method: 'GET',
        signal: controller.signal,
        cache: 'no-store',
      });
      if (!response.ok) return null;
      const parsed: unknown = await response.json();
      const info = asDaemonInfo(parsed);
      if (info) this.setInfo(info);
      return info;
    } catch {
      // Daemon absent, blocked, or slow. Not exceptional in Milestone 1.
      return null;
    } finally {
      clearTimeout(timer);
    }
  }

  /** Opens the WebSocket. Safe to call repeatedly; a live socket is kept. */
  connect(): void {
    this.closed = false;
    const existing = this.socket;
    if (
      existing &&
      (existing.readyState === WebSocket.OPEN || existing.readyState === WebSocket.CONNECTING)
    ) {
      return;
    }
    this.clearReconnectTimer();
    this.setState(this.attempt === 0 ? 'connecting' : 'reconnecting');

    let socket: WebSocket;
    try {
      socket = new WebSocket(this.wsUrl);
    } catch {
      // Constructing can throw synchronously on a blocked or malformed URL.
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;

    socket.onopen = () => {
      if (this.socket !== socket) return;
      this.sendHello();
    };

    socket.onmessage = (event: MessageEvent) => {
      if (this.socket !== socket) return;
      this.handleRaw(event.data);
    };

    socket.onerror = () => {
      // Always paired with a `close` event; swallow so nothing goes unhandled.
    };

    socket.onclose = () => {
      if (this.socket !== socket) return;
      this.socket = null;
      this.stopPing();
      this.clearHelloTimer();
      if (this.closed) {
        this.setState('idle');
        return;
      }
      this.scheduleReconnect();
    };
  }

  /** Tears everything down and stops reconnecting. */
  close(): void {
    this.closed = true;
    // The user asked for this, so there is no problem left to report.
    this.setProblem(null);
    this.clearReconnectTimer();
    this.clearHelloTimer();
    this.stopPing();
    this.attempt = 0;
    this.detachSocket('client closing');
    this.setState('idle');
  }

  /** Drops the current socket and reconnects immediately (user-initiated). */
  reconnectNow(): void {
    this.attempt = 0;
    this.clearReconnectTimer();
    this.clearHelloTimer();
    this.stopPing();
    this.detachSocket('reconnecting');
    this.connect();
  }

  /** Sends a typed client message. Returns false when the socket is not open. */
  send(message: ClientMessage): boolean {
    const socket = this.socket;
    if (!socket || socket.readyState !== WebSocket.OPEN) return false;
    try {
      socket.send(JSON.stringify(message));
      return true;
    } catch {
      return false;
    }
  }

  /** Subscribes to one server message type. Returns an unsubscribe function. */
  on<T extends ServerMessageType>(type: T, handler: MessageHandler<T>): () => void {
    let set = this.handlers.get(type);
    if (!set) {
      set = new Set<ErasedHandler>();
      this.handlers.set(type, set);
    }
    // Safe: emit() only invokes handlers registered under the matching `type`.
    const erased = handler as ErasedHandler;
    set.add(erased);
    return () => {
      this.handlers.get(type)?.delete(erased);
    };
  }

  onStateChange(listener: (state: ConnectionState) => void): () => void {
    this.stateListeners.add(listener);
    return () => {
      this.stateListeners.delete(listener);
    };
  }

  onInfo(listener: (info: DaemonInfo | null) => void): () => void {
    this.infoListeners.add(listener);
    return () => {
      this.infoListeners.delete(listener);
    };
  }

  onProblem(listener: (problem: DaemonProblem | null) => void): () => void {
    this.problemListeners.add(listener);
    return () => {
      this.problemListeners.delete(listener);
    };
  }

  // -- internals ------------------------------------------------------------

  private sendHello(): void {
    const sent = this.send({
      type: 'hello',
      id: newId(),
      protocolVersion: PROTOCOL_VERSION,
      client: 'extension',
      clientVersion: APP_VERSION,
    });
    if (!sent) return;

    // We are open but not yet usable; `ready` is what promotes us to connected.
    this.clearHelloTimer();
    this.helloTimer = setTimeout(() => {
      this.helloTimer = null;
      if (this.state !== 'connected') {
        this.setProblem({
          code: 'hello_timeout',
          message:
            'Something is listening on 127.0.0.1:' +
            String(DAEMON_PORT) +
            ' but it did not answer the Sharing handshake.',
        });
        this.setState('error');
        this.dropSocketAndRetry('hello timeout');
      }
    }, HELLO_TIMEOUT_MS);
  }

  private handleRaw(data: unknown): void {
    if (typeof data !== 'string') return; // Binary frames arrive in M9.
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      return;
    }
    const envelope = asEnvelope(parsed);
    if (!envelope) return;

    // The daemon is a local, trusted peer speaking a versioned schema, so the
    // envelope check above is the only validation before we narrow.
    const message = parsed as ServerMessage;

    if (envelope.type === 'ready') {
      const ready = message as ServerMessageOf<'ready'>;
      this.attempt = 0;
      this.clearHelloTimer();
      if (ready.protocolVersion !== PROTOCOL_VERSION) {
        console.warn(
          '[sharing] protocol mismatch: daemon speaks',
          ready.protocolVersion,
          'expected',
          PROTOCOL_VERSION,
        );
        this.setProblem({
          code: 'protocol_mismatch',
          message:
            'The daemon speaks protocol v' +
            String(ready.protocolVersion) +
            '; this extension needs v' +
            String(PROTOCOL_VERSION) +
            '. Update the daemon.',
          daemonProtocolVersion: ready.protocolVersion,
        });
        this.setState('error');
        this.dropSocketAndRetry('protocol mismatch');
        return;
      }
      this.setProblem(null);
      this.setInfo({
        id: ready.device.id,
        name: ready.device.name,
        version: ready.device.version,
        platform: ready.device.platform,
        protocolVersion: ready.protocolVersion,
        capabilities: this.info?.capabilities ?? [],
      });
      this.setState('connected');
      this.startPing();
    }

    this.emit(envelope.type, message);
  }

  private emit(type: string, message: ServerMessage): void {
    const set = this.handlers.get(type as ServerMessageType);
    if (!set) return;
    for (const handler of set) {
      try {
        handler(message);
      } catch (error) {
        console.error('[sharing] message handler threw', error);
      }
    }
  }

  private startPing(): void {
    this.stopPing();
    this.pingTimer = setInterval(() => {
      const ok = this.send({ type: 'ping', id: newId(), t: Date.now() });
      if (!ok) this.stopPing();
    }, PING_INTERVAL_MS);
  }

  private stopPing(): void {
    if (this.pingTimer !== null) {
      clearInterval(this.pingTimer);
      this.pingTimer = null;
    }
  }

  private clearHelloTimer(): void {
    if (this.helloTimer !== null) {
      clearTimeout(this.helloTimer);
      this.helloTimer = null;
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  /** Unhooks and closes the current socket without firing our own handlers. */
  private detachSocket(reason: string): void {
    const socket = this.socket;
    this.socket = null;
    if (!socket) return;
    socket.onopen = null;
    socket.onmessage = null;
    socket.onerror = null;
    socket.onclose = null;
    try {
      socket.close(1000, reason);
    } catch {
      // Already closing or closed.
    }
  }

  /** Closes the current socket and schedules exactly one retry. */
  private dropSocketAndRetry(reason: string): void {
    this.detachSocket(reason);
    this.stopPing();
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    if (this.closed) return;
    this.clearReconnectTimer();
    const delay = backoffDelay(this.attempt);
    this.attempt += 1;
    this.setState('reconnecting');
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  private setState(next: ConnectionState): void {
    if (this.state === next) return;
    this.state = next;
    for (const listener of this.stateListeners) {
      try {
        listener(next);
      } catch (error) {
        console.error('[sharing] state listener threw', error);
      }
    }
  }

  private setProblem(next: DaemonProblem | null): void {
    if (this.problem === null && next === null) return;
    if (this.problem?.code === next?.code && this.problem?.message === next?.message) return;
    this.problem = next;
    for (const listener of this.problemListeners) {
      try {
        listener(next);
      } catch (error) {
        console.error('[sharing] problem listener threw', error);
      }
    }
  }

  private setInfo(next: DaemonInfo | null): void {
    this.info = next;
    for (const listener of this.infoListeners) {
      try {
        listener(next);
      } catch (error) {
        console.error('[sharing] info listener threw', error);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

/** 500ms, 1s, 2s, 4s, ... capped at 15s, with up to 20% jitter either way. */
export function backoffDelay(attempt: number): number {
  const exponent = Math.min(Math.max(0, attempt), 20);
  const capped = Math.min(BACKOFF_BASE_MS * 2 ** exponent, BACKOFF_MAX_MS);
  const jitter = capped * BACKOFF_JITTER * (Math.random() * 2 - 1);
  return Math.max(BACKOFF_BASE_MS, Math.round(capped + jitter));
}

/** Client-generated correlation id, echoed back by the daemon. */
export function newId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return 'c-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function asEnvelope(value: unknown): Envelope | null {
  if (!isRecord(value)) return null;
  const type = value['type'];
  if (typeof type !== 'string') return null;
  const id = value['id'];
  return typeof id === 'string' ? { type, id } : { type };
}

/** Validates a GET /info body before we trust it. */
export function asDaemonInfo(value: unknown): DaemonInfo | null {
  if (!isRecord(value)) return null;
  const id = value['id'];
  const name = value['name'];
  const version = value['version'];
  const protocolVersion = value['protocolVersion'];
  const capabilities = value['capabilities'];
  if (typeof id !== 'string' || typeof name !== 'string' || typeof version !== 'string') {
    return null;
  }
  if (typeof protocolVersion !== 'number') return null;
  return {
    id,
    name,
    version,
    platform: asPlatform(value['platform']),
    protocolVersion,
    capabilities: Array.isArray(capabilities)
      ? capabilities.filter((entry): entry is string => typeof entry === 'string')
      : [],
  };
}

export function asPlatform(value: unknown): Platform {
  return value === 'windows' || value === 'darwin' || value === 'linux' ? value : 'unknown';
}
