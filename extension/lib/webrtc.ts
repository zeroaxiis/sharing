/**
 * PeerLink — one RTCPeerConnection + one RTCDataChannel to one device.
 *
 * WHERE THIS RUNS
 * ---------------
 * The SIDE PANEL document, never the background script. RTCPeerConnection does
 * not exist in a Chrome MV3 service worker; constructing one there fails at
 * runtime with no useful error. The background keeps owning the daemon
 * WebSocket and relays signalling both ways over browser.runtime messaging, so
 * a transfer requires the panel to stay open. That limitation is surfaced in
 * the UI rather than hidden.
 *
 * TODO(M12): a Chrome offscreen document (reason DOM_PARSER / WEBRTC) would let
 * a transfer survive the panel being closed. Firefox has no equivalent, so the
 * panel-open path has to keep working either way.
 *
 * The daemon is only a signalling relay. Once ICE completes, bytes go directly
 * between the two browsers over a DTLS-encrypted SCTP DataChannel and never
 * touch the daemon or any server.
 */

import type {
  PeerState,
  SignalIceInbound,
  SignalInboundMessage,
  SignalOutboundMessage,
} from '@/types';

/**
 * Local-network-only configuration.
 *
 * `iceServers: []` is deliberate: host candidates are enough for two devices on
 * the same subnet, and an empty list skips a pointless STUN round trip that
 * would only add latency and leak our public address.
 *
 * TODO(M15): when off-LAN transfers land, take STUN/TURN URLs from daemon
 * config and merge them in here — this is the only place that needs to change.
 */
export const RTC_CONFIG: RTCConfiguration = {
  iceServers: [],
  iceTransportPolicy: 'all',
};

/** Channel label. Fixed by the spec (v2 section 5); both ends must agree. */
export const DATA_CHANNEL_LABEL = 'sharing';

/** Channel options. `ordered` is what lets chunks omit a sequence number. */
export const DATA_CHANNEL_INIT: RTCDataChannelInit = { ordered: true };

/** Who created the DataChannel. Decided by which side the user clicked on. */
export type PeerLinkRole = 'initiator' | 'responder';

/**
 * Lifecycle as the UI should present it.
 *
 * `connected` means BOTH `pc.connectionState === 'connected'` and
 * `dc.readyState === 'open'`; either alone is not yet usable.
 */
export type PeerLinkState =
  | 'new'
  | 'connecting'
  | 'connected'
  | 'disconnected'
  | 'failed'
  | 'closed';

export type PeerLinkErrorCode =
  | 'unsupported_environment'
  | 'negotiation_failed'
  | 'signal_failed'
  | 'ice_failed'
  | 'ice_disconnected'
  | 'connection_failed'
  | 'channel_error';

/**
 * A failure worth showing a human.
 *
 * Every one of these carries a `message` written for the UI, because a WebRTC
 * connection that hangs with no explanation is the worst possible outcome. None
 * of these are swallowed.
 */
export interface PeerLinkError {
  code: PeerLinkErrorCode;
  /** Displayable, already phrased for a person. */
  message: string;
  /** False when the link may still recover on its own (ICE `disconnected`). */
  fatal: boolean;
  cause?: unknown;
}

export interface PeerLinkOptions {
  /** Our own device id. Used for the glare tie-break. */
  localId: string;
  /** The remote device id. Every outbound signal is addressed to it. */
  remoteId: string;
  /** Human name of the remote device, used in error messages. */
  remoteName?: string;
  /**
   * Delivers an outbound signal to the daemon — in practice a
   * `{kind:'relayToDaemon'}` runtime message to the background script.
   * Throwing or rejecting here surfaces as a `signal_failed` PeerLinkError.
   */
  sendSignal: (message: SignalOutboundMessage) => void | Promise<void>;
  /** Correlation id factory for control messages. Defaults to crypto.randomUUID. */
  newId?: () => string;
  /**
   * Test seam. Production leaves it unset and the real RTCPeerConnection is
   * constructed. Unit tests inject a fake so ICE buffering and glare can be
   * exercised without a browser.
   */
  createConnection?: (config: RTCConfiguration) => RTCPeerConnection;
  /** Overrides RTC_CONFIG. Only tests and future STUN/TURN work should. */
  config?: RTCConfiguration;
}

type Listener<T> = (value: T) => void;

/** Tiny listener set. Handlers are isolated so one throw cannot break a fan-out. */
class Emitter<T> {
  private readonly listeners = new Set<Listener<T>>();

  add(listener: Listener<T>): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  emit(value: T): void {
    for (const listener of [...this.listeners]) {
      try {
        listener(value);
      } catch (error) {
        console.error('[sharing] PeerLink listener threw', error);
      }
    }
  }

  clear(): void {
    this.listeners.clear();
  }
}

/** Maps a PeerLinkState onto the daemon's `peer:state` vocabulary for the UI. */
export function toPeerState(state: PeerLinkState): PeerState {
  switch (state) {
    case 'connected':
      return 'paired';
    case 'connecting':
      return 'online';
    case 'failed':
      return 'error';
    default:
      return 'offline';
  }
}

export class PeerLink {
  readonly localId: string;
  readonly remoteId: string;
  readonly remoteName: string;

  private readonly sendSignalFn: PeerLinkOptions['sendSignal'];
  private readonly newId: () => string;
  private readonly createConnection: (config: RTCConfiguration) => RTCPeerConnection;
  private readonly config: RTCConfiguration;

  private pc: RTCPeerConnection | null = null;
  private dc: RTCDataChannel | null = null;
  private role: PeerLinkRole | null = null;
  private state: PeerLinkState = 'new';

  /**
   * Remote ICE candidates that arrived before setRemoteDescription.
   *
   * This buffer is not an optimisation. addIceCandidate() before a remote
   * description throws, and a dropped candidate is the single most common cause
   * of a WebRTC connection that never establishes — the SDP exchange succeeds,
   * both sides sit in `checking`, and nothing ever explains why. Trickle ICE
   * makes the race routine: the peer's first candidate frequently overtakes its
   * own answer through the relay.
   */
  private readonly pendingCandidates: RTCIceCandidateInit[] = [];
  private remoteDescriptionSet = false;

  /** Glare bookkeeping: true from createOffer until setLocalDescription lands. */
  private makingOffer = false;
  /**
   * Loser of a simultaneous-offer race. The lexicographically SMALLER deviceId
   * wins, so the side with the larger id is "polite" and rolls back.
   */
  private readonly polite: boolean;

  private closed = false;
  /**
   * True once the close event has been fanned out.
   *
   * The event has to fire on a REMOTE teardown as well as a local close(), but
   * exactly once: a remote close is usually followed by our own close(), and a
   * consumer that heard it twice would settle the same transfer twice.
   */
  private closeEmitted = false;
  private bufferedLowThreshold = 0;

  private readonly stateEvents = new Emitter<PeerLinkState>();
  private readonly openEvents = new Emitter<void>();
  private readonly closeEvents = new Emitter<void>();
  private readonly errorEvents = new Emitter<PeerLinkError>();
  private readonly stringEvents = new Emitter<string>();
  private readonly binaryEvents = new Emitter<ArrayBuffer>();
  private readonly drainEvents = new Emitter<void>();

  constructor(options: PeerLinkOptions) {
    this.localId = options.localId;
    this.remoteId = options.remoteId;
    this.remoteName = options.remoteName ?? options.remoteId;
    this.sendSignalFn = options.sendSignal;
    this.newId = options.newId ?? defaultId;
    this.config = options.config ?? RTC_CONFIG;
    this.createConnection =
      options.createConnection ??
      ((config: RTCConfiguration) => {
        if (typeof RTCPeerConnection === 'undefined') {
          // Almost always means this was constructed in an MV3 service worker.
          throw new Error(
            'RTCPeerConnection is unavailable here. WebRTC must run in the side panel document, not the background script.',
          );
        }
        return new RTCPeerConnection(config);
      });
    this.polite = options.localId > options.remoteId;
  }

  // -- observation ----------------------------------------------------------

  getState(): PeerLinkState {
    return this.state;
  }

  getRole(): PeerLinkRole | null {
    return this.role;
  }

  /** True only when the channel can actually carry bytes right now. */
  get isOpen(): boolean {
    return !this.closed && this.dc?.readyState === 'open';
  }

  get bufferedAmount(): number {
    return this.dc?.bufferedAmount ?? 0;
  }

  get bufferedAmountLowThreshold(): number {
    return this.dc?.bufferedAmountLowThreshold ?? this.bufferedLowThreshold;
  }

  /** Remembered until a channel exists, then applied to it. */
  set bufferedAmountLowThreshold(value: number) {
    this.bufferedLowThreshold = value;
    if (this.dc) this.dc.bufferedAmountLowThreshold = value;
  }

  /** Diagnostic: how many remote candidates are waiting for a remote description. */
  get pendingRemoteCandidateCount(): number {
    return this.pendingCandidates.length;
  }

  onState(listener: Listener<PeerLinkState>): () => void {
    return this.stateEvents.add(listener);
  }

  onOpen(listener: () => void): () => void {
    return this.openEvents.add(listener);
  }

  onClose(listener: () => void): () => void {
    return this.closeEvents.add(listener);
  }

  onError(listener: Listener<PeerLinkError>): () => void {
    return this.errorEvents.add(listener);
  }

  onString(listener: Listener<string>): () => void {
    return this.stringEvents.add(listener);
  }

  onBinary(listener: Listener<ArrayBuffer>): () => void {
    return this.binaryEvents.add(listener);
  }

  /** Fires on `bufferedamountlow`. The transfer engine's drain signal. */
  onBufferedAmountLow(listener: () => void): () => void {
    return this.drainEvents.add(listener);
  }

  // -- negotiation ----------------------------------------------------------

  /**
   * Become the initiator: create the DataChannel, then offer.
   *
   * The channel MUST exist before createOffer, otherwise the offer carries no
   * m=application section and the responder never gets an `ondatachannel`.
   */
  async connect(): Promise<void> {
    if (this.closed) return;
    if (this.role === 'initiator') return;
    this.role = 'initiator';

    try {
      const pc = this.ensureConnection();
      const channel = pc.createDataChannel(DATA_CHANNEL_LABEL, DATA_CHANNEL_INIT);
      this.attachChannel(channel);
      await this.sendOffer(pc);
    } catch (error) {
      this.fail('negotiation_failed', 'Could not start a connection to ' + this.remoteName + '.', error);
    }
  }

  /** Feeds one inbound signalling message from the daemon relay. */
  async handleSignal(message: SignalInboundMessage): Promise<void> {
    if (this.closed) return;
    if (message.from !== this.remoteId) return; // Not ours; the router mis-dispatched.

    try {
      switch (message.type) {
        case 'signal:offer':
          await this.handleOffer(message.sdp);
          return;
        case 'signal:answer':
          await this.handleAnswer(message.sdp);
          return;
        case 'signal:ice':
          await this.handleIce(message);
          return;
        default:
          return;
      }
    } catch (error) {
      this.fail(
        'negotiation_failed',
        'Signalling with ' + this.remoteName + ' failed. Try connecting again.',
        error,
      );
    }
  }

  private async handleOffer(sdp: string): Promise<void> {
    const pc = this.ensureConnection();

    // Glare guard. Normally unreachable — exactly one side initiates, because a
    // human clicked a device — but if both offer at once, the lexicographically
    // smaller deviceId wins and the larger rolls back. Without this both ends
    // sit in `have-local-offer` forever.
    const collision = this.makingOffer || pc.signalingState !== 'stable';
    if (collision) {
      if (!this.polite) {
        // We win: ignore their offer, ours is already in flight.
        return;
      }
      await pc.setLocalDescription({ type: 'rollback' });
    }

    if (this.role === null) this.role = 'responder';

    await pc.setRemoteDescription({ type: 'offer', sdp });
    this.remoteDescriptionSet = true;
    await this.flushPendingCandidates();

    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);
    this.emitSignal({
      type: 'signal:answer',
      id: this.newId(),
      to: this.remoteId,
      sdp: pc.localDescription?.sdp ?? answer.sdp ?? '',
    });
  }

  private async handleAnswer(sdp: string): Promise<void> {
    const pc = this.ensureConnection();
    if (pc.signalingState !== 'have-local-offer') {
      // A duplicate or late answer. Applying it would throw InvalidStateError.
      return;
    }
    await pc.setRemoteDescription({ type: 'answer', sdp });
    this.remoteDescriptionSet = true;
    await this.flushPendingCandidates();
  }

  private async handleIce(message: SignalIceInbound): Promise<void> {
    const init: RTCIceCandidateInit = {
      candidate: message.candidate,
      // An empty sdpMid must become null, or the browser rejects the candidate.
      sdpMid: message.sdpMid === '' ? null : message.sdpMid,
      sdpMLineIndex: message.sdpMLineIndex,
    };

    if (!this.remoteDescriptionSet) {
      this.pendingCandidates.push(init);
      return;
    }
    await this.addCandidate(init);
  }

  private async flushPendingCandidates(): Promise<void> {
    if (this.pendingCandidates.length === 0) return;
    const queued = this.pendingCandidates.splice(0, this.pendingCandidates.length);
    for (const candidate of queued) {
      await this.addCandidate(candidate);
    }
  }

  private async addCandidate(init: RTCIceCandidateInit): Promise<void> {
    const pc = this.pc;
    if (!pc) return;
    try {
      await pc.addIceCandidate(init);
    } catch (error) {
      // One bad candidate is survivable — others usually still pair up — so this
      // is reported, not fatal.
      this.errorEvents.emit({
        code: 'ice_failed',
        message: 'A network candidate from ' + this.remoteName + ' was rejected.',
        fatal: false,
        cause: error,
      });
    }
  }

  private async sendOffer(pc: RTCPeerConnection): Promise<void> {
    this.makingOffer = true;
    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      this.emitSignal({
        type: 'signal:offer',
        id: this.newId(),
        to: this.remoteId,
        sdp: pc.localDescription?.sdp ?? offer.sdp ?? '',
      });
    } finally {
      this.makingOffer = false;
    }
  }

  // -- data path ------------------------------------------------------------

  /** Sends a JSON control frame. Returns false when the channel is not open. */
  sendString(text: string): boolean {
    const dc = this.dc;
    if (!dc || dc.readyState !== 'open') return false;
    try {
      dc.send(text);
      return true;
    } catch (error) {
      this.errorEvents.emit({
        code: 'channel_error',
        message: 'Could not send to ' + this.remoteName + '.',
        fatal: false,
        cause: error,
      });
      return false;
    }
  }

  /** Sends one binary chunk. Returns false when the channel is not open. */
  sendBinary(data: ArrayBuffer): boolean {
    const dc = this.dc;
    if (!dc || dc.readyState !== 'open') return false;
    try {
      dc.send(data);
      return true;
    } catch (error) {
      this.errorEvents.emit({
        code: 'channel_error',
        message: 'Could not send data to ' + this.remoteName + '.',
        fatal: false,
        cause: error,
      });
      return false;
    }
  }

  /** Resolves once the channel is open, rejects on timeout or close. */
  waitUntilOpen(timeoutMs = 30_000): Promise<void> {
    if (this.isOpen) return Promise.resolve();
    if (this.closed) return Promise.reject(new Error('Peer link is closed.'));

    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => {
        cleanup();
        reject(new Error('Timed out waiting for a connection to ' + this.remoteName + '.'));
      }, timeoutMs);

      const offOpen = this.onOpen(() => {
        cleanup();
        resolve();
      });
      const offClose = this.onClose(() => {
        cleanup();
        reject(new Error('The connection to ' + this.remoteName + ' closed.'));
      });

      function cleanup(): void {
        clearTimeout(timer);
        offOpen();
        offClose();
      }
    });
  }

  // -- teardown -------------------------------------------------------------

  /** Releases everything. Safe to call any number of times. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.pendingCandidates.length = 0;

    const dc = this.dc;
    this.dc = null;
    if (dc) {
      dc.onopen = null;
      dc.onclose = null;
      dc.onerror = null;
      dc.onmessage = null;
      dc.onbufferedamountlow = null;
      try {
        dc.close();
      } catch {
        // Already closing.
      }
    }

    const pc = this.pc;
    this.pc = null;
    if (pc) {
      pc.onicecandidate = null;
      pc.ondatachannel = null;
      pc.onconnectionstatechange = null;
      pc.oniceconnectionstatechange = null;
      pc.onicecandidateerror = null;
      try {
        pc.close();
      } catch {
        // Already closed.
      }
    }

    this.setState('closed');
    this.emitClosed();

    // Drop subscriptions last, so the close event above still reaches everyone.
    this.stateEvents.clear();
    this.openEvents.clear();
    this.closeEvents.clear();
    this.errorEvents.clear();
    this.stringEvents.clear();
    this.binaryEvents.clear();
    this.drainEvents.clear();
  }

  // -- internals ------------------------------------------------------------

  private ensureConnection(): RTCPeerConnection {
    const existing = this.pc;
    if (existing) return existing;

    const pc = this.createConnection(this.config);
    this.pc = pc;

    pc.onicecandidate = (event: RTCPeerConnectionIceEvent) => {
      const candidate = event.candidate;
      // A null candidate is the end-of-candidates marker; there is nothing to
      // relay, and the peer infers completion from its own gathering state.
      if (!candidate || candidate.candidate === '') return;
      this.emitSignal({
        type: 'signal:ice',
        id: this.newId(),
        to: this.remoteId,
        candidate: candidate.candidate,
        sdpMid: candidate.sdpMid ?? '',
        sdpMLineIndex: candidate.sdpMLineIndex ?? 0,
      });
    };

    pc.ondatachannel = (event: RTCDataChannelEvent) => {
      if (this.role === null) this.role = 'responder';
      this.attachChannel(event.channel);
    };

    pc.onconnectionstatechange = () => {
      this.syncState();
    };

    pc.oniceconnectionstatechange = () => {
      this.reportIceState(pc.iceConnectionState);
      this.syncState();
    };

    this.setState('connecting');
    return pc;
  }

  private attachChannel(channel: RTCDataChannel): void {
    // arraybuffer, not blob: the transfer engine reads chunk headers
    // synchronously and a Blob would force an extra async hop per chunk.
    channel.binaryType = 'arraybuffer';
    channel.bufferedAmountLowThreshold = this.bufferedLowThreshold;
    this.dc = channel;

    channel.onopen = () => {
      this.syncState();
      if (this.state === 'connected') this.openEvents.emit(undefined);
    };

    channel.onclose = () => {
      this.syncState();
      // A channel that has closed can never carry another byte, and nothing
      // else will ever tell the consumer. TransferEngine parks its send loop on
      // `bufferedamountlow` and arms an onClose() escape hatch precisely for
      // this case; without this emit that escape hatch never fires and a
      // transfer interrupted by the peer disappearing hangs forever.
      this.emitClosed();
    };

    channel.onerror = (event: Event) => {
      // RTCErrorEvent in browsers that implement it; a bare Event elsewhere.
      const detail = 'error' in event ? (event as RTCErrorEvent).error : undefined;
      this.errorEvents.emit({
        code: 'channel_error',
        message: 'The data channel to ' + this.remoteName + ' reported an error.',
        fatal: false,
        cause: detail,
      });
    };

    channel.onbufferedamountlow = () => {
      this.drainEvents.emit(undefined);
    };

    channel.onmessage = (event: MessageEvent) => {
      const data: unknown = event.data;
      if (typeof data === 'string') {
        this.stringEvents.emit(data);
        return;
      }
      if (data instanceof ArrayBuffer) {
        this.binaryEvents.emit(data);
        return;
      }
      if (typeof Blob !== 'undefined' && data instanceof Blob) {
        // Only reachable if binaryType was ignored; keep it correct anyway.
        void data
          .arrayBuffer()
          .then((buffer) => {
            this.binaryEvents.emit(buffer);
          })
          .catch(() => undefined);
        return;
      }
      if (ArrayBuffer.isView(data)) {
        this.binaryEvents.emit(toArrayBuffer(data));
      }
    };

    // A channel adopted from ondatachannel can already be open by the time we
    // attach, in which case onopen has fired and will never fire again.
    if (channel.readyState === 'open') {
      this.syncState();
      if (this.state === 'connected') this.openEvents.emit(undefined);
    }
  }

  /**
   * Usable means BOTH halves agree: the transport is connected AND the channel
   * is open. Reporting `connected` off either one alone produces a UI that says
   * "connected" while every send silently fails.
   */
  private syncState(): void {
    if (this.closed) return;
    const pc = this.pc;
    if (!pc) return;

    const channelOpen = this.dc?.readyState === 'open';
    switch (pc.connectionState) {
      case 'connected':
        this.setState(channelOpen ? 'connected' : 'connecting');
        return;
      case 'failed':
        this.setState('failed');
        // Terminal: WebRTC never recovers from `failed` without an ICE restart,
        // which this link does not attempt. Treat it as a close so anything
        // waiting on the data path is released instead of hanging.
        this.emitClosed();
        return;
      case 'disconnected':
        // NOT terminal, and deliberately not a close: `disconnected` routinely
        // recovers on its own, and tearing a transfer down on a transient blip
        // would be worse than the wait.
        this.setState('disconnected');
        return;
      case 'closed':
        this.setState('closed');
        this.emitClosed();
        return;
      default:
        this.setState('connecting');
    }
  }

  /**
   * ICE failures are reported LOUDLY. On a LAN they nearly always mean the two
   * machines cannot see each other — client isolation on the access point, a
   * firewall, or different subnets — and the user can act on that. Silence here
   * produces a spinner that never resolves and no way to diagnose it.
   */
  private reportIceState(state: RTCIceConnectionState): void {
    if (state === 'failed') {
      this.errorEvents.emit({
        code: 'ice_failed',
        message:
          'Could not reach ' +
          this.remoteName +
          ' directly. Check that both devices are on the same network and that it is set to Private, not Public.',
        fatal: true,
      });
      return;
    }
    if (state === 'disconnected') {
      this.errorEvents.emit({
        code: 'ice_disconnected',
        message: 'Lost the direct connection to ' + this.remoteName + '. Trying to recover…',
        fatal: false,
      });
    }
  }

  private emitSignal(message: SignalOutboundMessage): void {
    try {
      const result = this.sendSignalFn(message);
      if (result instanceof Promise) {
        result.catch((error: unknown) => {
          this.signalFailed(error);
        });
      }
    } catch (error) {
      this.signalFailed(error);
    }
  }

  private signalFailed(cause: unknown): void {
    this.errorEvents.emit({
      code: 'signal_failed',
      message:
        'Could not reach the Sharing daemon to set up the connection to ' +
        this.remoteName +
        '.',
      fatal: true,
      cause,
    });
  }

  private fail(code: PeerLinkErrorCode, message: string, cause: unknown): void {
    this.errorEvents.emit({ code, message, fatal: true, cause });
    this.setState('failed');
  }

  private setState(next: PeerLinkState): void {
    if (this.state === next) return;
    this.state = next;
    this.stateEvents.emit(next);
  }

  /**
   * Fans out the close event at most once, from whichever end tore the link
   * down: our own close(), the channel's `close` event, or the peer connection
   * reaching a terminal state.
   */
  private emitClosed(): void {
    if (this.closeEmitted) return;
    this.closeEmitted = true;
    this.closeEvents.emit(undefined);
  }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

function defaultId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return 's-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}

/** Copies a view's bytes out of its (possibly larger, possibly pooled) buffer. */
function toArrayBuffer(view: ArrayBufferView): ArrayBuffer {
  const bytes = new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
  return bytes.slice().buffer;
}
