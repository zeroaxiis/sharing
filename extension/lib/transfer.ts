/**
 * TransferEngine — chunked file and text transfer over one DataChannel.
 *
 * Wire format (spec v2 section 6): STRING frames are JSON control messages,
 * BINARY frames are file chunks behind a fixed 40-byte header —
 *
 *     offset 0..3     magic "NSC1" (ASCII)
 *     offset 4..39    transferId, 36 ASCII chars (canonical UUID)
 *     offset 40..     payload, at most CHUNK_SIZE bytes
 *
 * Ordering comes from the channel's `ordered: true`, so no sequence number is
 * needed. A chunk whose transferId is unknown is DROPPED, never buffered.
 *
 * The engine is deliberately transport-agnostic: it talks to a
 * {@link TransferChannel}, which `PeerLink` from lib/webrtc.ts satisfies
 * structurally. That is what makes the send loop, the backpressure and the
 * too-large refusal testable without a browser.
 */

import {
  BUFFERED_LOW,
  CHUNK_SIZE,
  FRAME_MAGIC,
  FRAME_MAGIC_BYTES,
  FRAME_HEADER_BYTES,
  HIGH_WATER,
  MAX_INLINE_TEXT,
  MAX_IN_MEMORY_BYTES,
  TRANSFER_ID_LENGTH,
  type DataChannelFrame,
  type TransferCancelReason,
  type TransferStartFrame,
} from '@/types';

// ---------------------------------------------------------------------------
// Public shapes
// ---------------------------------------------------------------------------

export type TransferDirection = 'send' | 'receive';

export type TransferStatus = 'pending' | 'active' | 'complete' | 'cancelled' | 'failed';

/**
 * One transfer as the UI sees it.
 *
 * The first eleven fields are the contract components/TransferProgress.tsx and
 * components/TransferHistory.tsx already render; do not remove or rename them.
 * Records handed to listeners are shallow copies, so React sees a new object
 * identity on every change.
 */
export interface TransferRecord {
  transferId: string;
  direction: TransferDirection;
  /** Device id of the other end. */
  peerId: string;
  peerName: string;
  name: string;
  mime: string;
  size: number;
  bytes: number;
  status: TransferStatus;
  startedAt: number;
  finishedAt: number | null;
  error: string | null;
  /** Bytes the receiver has confirmed via `transfer:ack`. Sender side only. */
  ackedBytes?: number;
  /** Present on a cancelled or failed transfer. */
  cancelReason?: TransferCancelReason;
}

/** The transport the engine writes to. `PeerLink` satisfies this. */
export interface TransferChannel {
  readonly isOpen: boolean;
  readonly bufferedAmount: number;
  bufferedAmountLowThreshold: number;
  sendString(text: string): boolean;
  sendBinary(data: ArrayBuffer): boolean;
  onString(listener: (text: string) => void): () => void;
  onBinary(listener: (data: ArrayBuffer) => void): () => void;
  onBufferedAmountLow(listener: () => void): () => void;
  onClose(listener: () => void): () => void;
}

/** What the receiving human is being asked to approve. */
export interface IncomingTransferOffer {
  transferId: string;
  peerId: string;
  peerName: string;
  name: string;
  mime: string;
  size: number;
}

/** Returns true to accept. Rejecting or throwing is treated as a decline. */
export type ConsentHandler = (offer: IncomingTransferOffer) => boolean | Promise<boolean>;

/**
 * A fully received file.
 *
 * `url` is an object URL and stays alive until `revoke()` runs. Revocation is
 * explicit and mandatory: an un-revoked object URL pins its Blob for the life
 * of the document, which on a 500 MB video is half a gigabyte of leaked RAM.
 * Call it after the download anchor has been clicked, or when the record
 * scrolls out of the UI.
 */
export interface ReceivedFile {
  transferId: string;
  name: string;
  mime: string;
  size: number;
  blob: Blob;
  url: string;
  revoke: () => void;
}

export interface TransferError {
  /** Null when the failure is not tied to one transfer. */
  transferId: string | null;
  message: string;
  cause?: unknown;
}

export interface TransferEngineOptions {
  channel: TransferChannel;
  /** Device id of the peer on the other end of `channel`. */
  peerId: string;
  peerName: string;
  /**
   * Asks the local human about an incoming file. Omitted means "refuse
   * everything", which is the safe default — never auto-accept.
   */
  consent?: ConsentHandler;
  /** Transfer id factory. Must produce a 36-char canonical UUID. */
  newTransferId?: () => string;
  /** Override for tests. Defaults to MAX_IN_MEMORY_BYTES. */
  maxInMemoryBytes?: number;
  /** How long the sender waits for `transfer:accept`. */
  acceptTimeoutMs?: number;
  /** Clock seam for tests. */
  now?: () => number;
}

// ---------------------------------------------------------------------------
// Tuning
// ---------------------------------------------------------------------------

/** Receiver sends a `transfer:ack` at most this often, by bytes. */
const ACK_INTERVAL_BYTES = 1024 * 1024;

/** Progress is emitted at most this often, so React is not re-rendered per chunk. */
const PROGRESS_INTERVAL_MS = 100;

/** Default patience for the receiver's human to answer. */
const DEFAULT_ACCEPT_TIMEOUT_MS = 120_000;

// ---------------------------------------------------------------------------
// Frame codec — 40-byte header, then payload
// ---------------------------------------------------------------------------

export interface DecodedChunk {
  transferId: string;
  /**
   * A view onto the received frame, NOT a copy. Cheap, and safe because the
   * engine only ever hands it to a Blob, which takes its own snapshot.
   *
   * Explicitly `Uint8Array<ArrayBuffer>` rather than the default
   * `Uint8Array<ArrayBufferLike>`: only the non-shared form is a valid BlobPart,
   * and this view always comes from a plain ArrayBuffer off the wire.
   */
  payload: Uint8Array<ArrayBuffer>;
}

/**
 * Wraps one chunk payload in the binary frame header.
 *
 * @throws RangeError when `transferId` is not 36 ASCII characters, because a
 * malformed id would produce a frame the peer silently drops — far harder to
 * diagnose later than a throw here.
 */
export function encodeChunk(transferId: string, payload: Uint8Array): ArrayBuffer {
  if (transferId.length !== TRANSFER_ID_LENGTH) {
    throw new RangeError(
      'transferId must be ' + TRANSFER_ID_LENGTH + ' characters, got ' + transferId.length,
    );
  }

  const frame = new Uint8Array(FRAME_HEADER_BYTES + payload.byteLength);
  for (let i = 0; i < FRAME_MAGIC_BYTES; i += 1) {
    frame[i] = FRAME_MAGIC.charCodeAt(i);
  }
  for (let i = 0; i < TRANSFER_ID_LENGTH; i += 1) {
    const code = transferId.charCodeAt(i);
    if (code > 0x7f) {
      throw new RangeError('transferId must be ASCII; got a non-ASCII character at ' + i);
    }
    frame[FRAME_MAGIC_BYTES + i] = code;
  }
  frame.set(payload, FRAME_HEADER_BYTES);
  return frame.buffer;
}

/** Reads a binary frame. Returns null for anything that is not one of ours. */
export function decodeChunk(data: ArrayBuffer): DecodedChunk | null {
  if (data.byteLength < FRAME_HEADER_BYTES) return null;
  const bytes = new Uint8Array(data);

  for (let i = 0; i < FRAME_MAGIC_BYTES; i += 1) {
    if (bytes[i] !== FRAME_MAGIC.charCodeAt(i)) return null;
  }

  let transferId = '';
  for (let i = 0; i < TRANSFER_ID_LENGTH; i += 1) {
    const code = bytes[FRAME_MAGIC_BYTES + i];
    if (code === undefined) return null;
    transferId += String.fromCharCode(code);
  }

  return { transferId, payload: bytes.subarray(FRAME_HEADER_BYTES) };
}

// ---------------------------------------------------------------------------
// Display helpers (used by components/TransferProgress + TransferHistory)
// ---------------------------------------------------------------------------

/** Number of 64 KiB chunks a payload of `size` bytes splits into. */
export function chunkCount(size: number): number {
  if (size <= 0) return 0;
  return Math.ceil(size / CHUNK_SIZE);
}

/** 0-1 progress fraction, clamped; 0 when the total is unknown. */
export function progressFraction(bytes: number, total: number): number {
  if (!Number.isFinite(total) || total <= 0) return 0;
  return Math.min(1, Math.max(0, bytes / total));
}

/** Human-readable byte count, e.g. "2.3 MB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return bytes + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return value.toFixed(value < 10 ? 1 : 0) + ' ' + (units[unit] ?? 'TB');
}

// ---------------------------------------------------------------------------
// Internal state
// ---------------------------------------------------------------------------

type Waiter = () => void;

interface OutgoingState {
  record: TransferRecord;
  file: File;
  /** Set by a cancel from either end; the send loop checks it every chunk. */
  aborted: boolean;
  /** Resolves the accept wait; replaced with null once settled. */
  settleAccept: ((accepted: boolean) => void) | null;
  /** Drain waits parked in awaitDrain(), woken on abort or close. */
  drainWaiters: Set<Waiter>;
}

interface IncomingState {
  record: TransferRecord;
  chunks: Uint8Array<ArrayBuffer>[];
  received: number;
  /** Bytes at the last `transfer:ack` we sent. */
  acked: number;
  /** False until our human has said yes. Chunks before that are dropped. */
  accepted: boolean;
}

type Listener<T> = (value: T) => void;

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
        console.error('[sharing] transfer listener threw', error);
      }
    }
  }

  clear(): void {
    this.listeners.clear();
  }
}

// ---------------------------------------------------------------------------
// TransferEngine
// ---------------------------------------------------------------------------

export class TransferEngine {
  private readonly channel: TransferChannel;
  private readonly peerId: string;
  private readonly peerName: string;
  private readonly newTransferId: () => string;
  private readonly maxInMemoryBytes: number;
  private readonly acceptTimeoutMs: number;
  private readonly now: () => number;

  private consent: ConsentHandler | null;

  private readonly outgoing = new Map<string, OutgoingState>();
  private readonly incoming = new Map<string, IncomingState>();
  private readonly finished: TransferRecord[] = [];

  /** Diagnostic counter: binary frames dropped for an unknown transferId. */
  private droppedFrames = 0;

  private readonly lastProgressAt = new Map<string, number>();
  private readonly unsubscribes: Array<() => void> = [];
  private disposed = false;

  private readonly recordEvents = new Emitter<TransferRecord>();
  private readonly textEvents = new Emitter<string>();
  private readonly fileEvents = new Emitter<ReceivedFile>();
  private readonly errorEvents = new Emitter<TransferError>();
  private readonly offerEvents = new Emitter<IncomingTransferOffer>();

  private readonly encoder = new TextEncoder();

  constructor(options: TransferEngineOptions) {
    this.channel = options.channel;
    this.peerId = options.peerId;
    this.peerName = options.peerName;
    this.consent = options.consent ?? null;
    this.newTransferId = options.newTransferId ?? defaultTransferId;
    this.maxInMemoryBytes = options.maxInMemoryBytes ?? MAX_IN_MEMORY_BYTES;
    this.acceptTimeoutMs = options.acceptTimeoutMs ?? DEFAULT_ACCEPT_TIMEOUT_MS;
    this.now = options.now ?? (() => Date.now());

    // Set once here rather than per transfer: the threshold is a property of
    // the channel, and the send loop below depends on it being in place before
    // the first chunk goes out.
    this.channel.bufferedAmountLowThreshold = BUFFERED_LOW;

    this.unsubscribes.push(
      this.channel.onString((text) => {
        this.handleString(text);
      }),
      this.channel.onBinary((data) => {
        this.handleBinary(data);
      }),
      this.channel.onClose(() => {
        this.handleChannelClose();
      }),
    );
  }

  // -- events ---------------------------------------------------------------

  /** Fires on every create/progress/status change, with a fresh copy. */
  onRecord(listener: Listener<TransferRecord>): () => void {
    return this.recordEvents.add(listener);
  }

  /** Inline text (under MAX_INLINE_TEXT). Needs no consent by design. */
  onText(listener: Listener<string>): () => void {
    return this.textEvents.add(listener);
  }

  /** A completed incoming file, with its object URL and explicit revoke(). */
  onFile(listener: Listener<ReceivedFile>): () => void {
    return this.fileEvents.add(listener);
  }

  /** An incoming offer, emitted before the consent handler is consulted. */
  onOffer(listener: Listener<IncomingTransferOffer>): () => void {
    return this.offerEvents.add(listener);
  }

  onError(listener: Listener<TransferError>): () => void {
    return this.errorEvents.add(listener);
  }

  setConsentHandler(handler: ConsentHandler | null): void {
    this.consent = handler;
  }

  // -- observation ----------------------------------------------------------

  getRecords(): TransferRecord[] {
    const live: TransferRecord[] = [];
    for (const state of this.outgoing.values()) live.push({ ...state.record });
    for (const state of this.incoming.values()) live.push({ ...state.record });
    return [...live, ...this.finished.map((record) => ({ ...record }))];
  }

  getRecord(transferId: string): TransferRecord | null {
    const record = this.findRecord(transferId);
    return record ? { ...record } : null;
  }

  /** Binary frames discarded because their transferId was unknown. */
  getDroppedFrameCount(): number {
    return this.droppedFrames;
  }

  // -- sending --------------------------------------------------------------

  /**
   * Sends a file: announce, wait for consent, then stream it.
   *
   * Resolves when the transfer has settled, in any terminal state. The returned
   * record is a snapshot; subscribe with onRecord() for live progress.
   */
  async sendFile(file: File): Promise<TransferRecord> {
    const transferId = this.newTransferId();
    const record: TransferRecord = {
      transferId,
      direction: 'send',
      peerId: this.peerId,
      peerName: this.peerName,
      name: file.name,
      mime: file.type || 'application/octet-stream',
      size: file.size,
      bytes: 0,
      status: 'pending',
      startedAt: this.now(),
      finishedAt: null,
      error: null,
      ackedBytes: 0,
    };

    const state: OutgoingState = {
      record,
      file,
      aborted: false,
      settleAccept: null,
      drainWaiters: new Set<Waiter>(),
    };
    this.outgoing.set(transferId, state);
    this.emitRecord(record);

    if (!this.channel.isOpen) {
      this.failOutgoing(state, 'error', 'Not connected to ' + this.peerName + '.');
      return { ...record };
    }

    const announced = this.sendFrame({
      type: 'transfer:start',
      transferId,
      name: record.name,
      mime: record.mime,
      size: record.size,
    });
    if (!announced) {
      this.failOutgoing(state, 'error', 'Could not announce the transfer.');
      return { ...record };
    }

    const accepted = await this.waitForAccept(state);
    if (state.aborted) return { ...record };
    if (!accepted) {
      this.settleOutgoing(state, 'cancelled', 'declined', this.peerName + ' declined the file.');
      return { ...record };
    }

    record.status = 'active';
    this.emitRecord(record);

    try {
      await this.streamFile(state);
    } catch (error) {
      if (!state.aborted) {
        this.failOutgoing(state, 'error', describeError(error), error);
      }
    }
    return { ...record };
  }

  /**
   * Sends text. Short text goes inline as a `text` frame and needs no consent;
   * anything larger takes the file path so it gets consent and backpressure.
   *
   * Resolves to null for the inline case — no record is created for something
   * that lands in a single frame.
   */
  async sendText(text: string): Promise<TransferRecord | null> {
    // Strictly less than the limit: the JSON envelope adds bytes of its own,
    // and escaping can inflate the payload further.
    if (this.encoder.encode(text).byteLength < MAX_INLINE_TEXT) {
      const ok = this.sendFrame({ type: 'text', text });
      if (!ok) {
        this.errorEvents.emit({
          transferId: null,
          message: 'Could not send text to ' + this.peerName + '.',
        });
      }
      return null;
    }

    const file = new File([text], 'shared-text.txt', { type: 'text/plain' });
    return this.sendFile(file);
  }

  /** Cancels a transfer in either direction and tells the peer. */
  cancel(transferId: string, reason: TransferCancelReason = 'user_cancelled'): void {
    const outgoing = this.outgoing.get(transferId);
    if (outgoing) {
      this.sendFrame({ type: 'transfer:cancel', transferId, reason });
      this.settleOutgoing(outgoing, 'cancelled', reason, 'Cancelled.');
      return;
    }
    const incoming = this.incoming.get(transferId);
    if (incoming) {
      this.sendFrame({ type: 'transfer:cancel', transferId, reason });
      this.settleIncoming(incoming, 'cancelled', reason, 'Cancelled.');
    }
  }

  /** Releases listeners and aborts everything in flight. Idempotent. */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const off of this.unsubscribes) off();
    this.unsubscribes.length = 0;

    for (const state of [...this.outgoing.values()]) {
      this.settleOutgoing(state, 'cancelled', 'user_cancelled', 'Closed.');
    }
    for (const state of [...this.incoming.values()]) {
      this.settleIncoming(state, 'cancelled', 'user_cancelled', 'Closed.');
    }

    this.recordEvents.clear();
    this.textEvents.clear();
    this.fileEvents.clear();
    this.errorEvents.clear();
    this.offerEvents.clear();
  }

  // -- send loop ------------------------------------------------------------

  /**
   * Streams the file chunk by chunk.
   *
   * Reading uses file.stream().getReader(), NOT file.arrayBuffer(): the latter
   * materialises the entire file in memory before the first byte leaves, which
   * defeats the whole point on anything large.
   */
  private async streamFile(state: OutgoingState): Promise<void> {
    const reader = state.file.stream().getReader();
    let carry: Uint8Array | null = null;

    try {
      for (;;) {
        if (state.aborted) return;
        const result = await reader.read();
        if (result.done) break;

        let view: Uint8Array = result.value;
        if (carry) {
          view = concatBytes(carry, view);
          carry = null;
        }

        let offset = 0;
        while (view.byteLength - offset >= CHUNK_SIZE) {
          await this.sendChunk(state, view.subarray(offset, offset + CHUNK_SIZE));
          if (state.aborted) return;
          offset += CHUNK_SIZE;
        }
        // A stream read almost never lands on a chunk boundary; the tail is
        // carried into the next read so every frame but the last is full.
        if (offset < view.byteLength) carry = view.slice(offset);
      }

      if (carry && carry.byteLength > 0) {
        await this.sendChunk(state, carry);
      }
      if (state.aborted) return;

      this.sendFrame({ type: 'transfer:complete', transferId: state.record.transferId });
      this.settleOutgoing(state, 'complete', undefined, null);
    } finally {
      try {
        reader.releaseLock();
      } catch {
        // Reader already released, or the stream errored.
      }
    }
  }

  private async sendChunk(state: OutgoingState, payload: Uint8Array): Promise<void> {
    // ******************* THE MOST IMPORTANT DETAIL IN THIS FILE **************
    // Backpressure. dc.send() never blocks: it queues, and the queue lives in
    // the tab's memory. Without this wait a multi-GB file is read as fast as
    // the disk allows and buffered in full before SCTP has drained a fraction
    // of it, and the tab dies with an out-of-memory crash that looks like a
    // browser bug rather than our bug. bufferedAmountLowThreshold is set in the
    // constructor; here we simply refuse to run ahead of the network.
    // *************************************************************************
    await this.awaitDrain(state);
    if (state.aborted) return;

    if (!this.channel.isOpen) {
      this.failOutgoing(state, 'error', 'The connection closed mid-transfer.');
      return;
    }

    const frame = encodeChunk(state.record.transferId, payload);
    if (!this.channel.sendBinary(frame)) {
      this.failOutgoing(state, 'error', 'Could not write to the connection.');
      return;
    }

    state.record.bytes += payload.byteLength;
    this.emitProgress(state.record);
  }

  /** Resolves immediately below the high-water mark, otherwise on the next drain. */
  private awaitDrain(state: OutgoingState): Promise<void> {
    if (state.aborted) return Promise.resolve();
    if (this.channel.bufferedAmount <= HIGH_WATER) return Promise.resolve();

    return new Promise<void>((resolve) => {
      let settled = false;
      const finish: Waiter = () => {
        if (settled) return;
        settled = true;
        offLow();
        offClose();
        state.drainWaiters.delete(finish);
        resolve();
      };
      // Woken by the drain event, by the channel closing, or by a cancel —
      // a wait that only the drain event could end would hang forever on a
      // connection that dies mid-transfer.
      const offLow = this.channel.onBufferedAmountLow(finish);
      const offClose = this.channel.onClose(finish);
      state.drainWaiters.add(finish);
    });
  }

  private waitForAccept(state: OutgoingState): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => {
        state.settleAccept = null;
        resolve(false);
      }, this.acceptTimeoutMs);

      state.settleAccept = (accepted: boolean) => {
        clearTimeout(timer);
        state.settleAccept = null;
        resolve(accepted);
      };
    });
  }

  // -- receive --------------------------------------------------------------

  private handleString(raw: string): void {
    const frame = parseFrame(raw);
    if (!frame) return;

    switch (frame.type) {
      case 'text':
        this.textEvents.emit(frame.text);
        return;
      case 'transfer:start':
        void this.handleStart(frame);
        return;
      case 'transfer:accept': {
        const state = this.outgoing.get(frame.transferId);
        state?.settleAccept?.(frame.accept);
        return;
      }
      case 'transfer:ack': {
        const state = this.outgoing.get(frame.transferId);
        if (!state) return;
        state.record.ackedBytes = frame.bytes;
        return;
      }
      case 'transfer:complete':
        this.handleComplete(frame.transferId);
        return;
      case 'transfer:cancel':
        this.handleRemoteCancel(frame.transferId, frame.reason);
        return;
      default:
        return;
    }
  }

  private async handleStart(frame: TransferStartFrame): Promise<void> {
    const { transferId, name, mime, size } = frame;
    if (this.incoming.has(transferId) || this.outgoing.has(transferId)) return;

    if (!Number.isSafeInteger(size) || size < 0) {
      this.refuse(transferId, name, mime, size, 'error', 'The sender declared an invalid size.');
      return;
    }

    // Refused BEFORE asking a human. There is no point prompting someone about
    // a file we already know we cannot hold, and accepting it would accumulate
    // chunks until the tab is killed.
    // TODO(M13): stream to disk (File System Access API / chrome.downloads) and
    // this ceiling goes away.
    if (size > this.maxInMemoryBytes) {
      this.refuse(
        transferId,
        name,
        mime,
        size,
        'too_large',
        name +
          ' is ' +
          formatBytes(size) +
          '. Sharing can currently receive up to ' +
          formatBytes(this.maxInMemoryBytes) +
          ' at once.',
      );
      return;
    }

    const record: TransferRecord = {
      transferId,
      direction: 'receive',
      peerId: this.peerId,
      peerName: this.peerName,
      name,
      mime: mime || 'application/octet-stream',
      size,
      bytes: 0,
      status: 'pending',
      startedAt: this.now(),
      finishedAt: null,
      error: null,
    };
    const state: IncomingState = {
      record,
      chunks: [],
      received: 0,
      acked: 0,
      accepted: false,
    };
    this.incoming.set(transferId, state);
    this.emitRecord(record);

    const offer: IncomingTransferOffer = {
      transferId,
      peerId: this.peerId,
      peerName: this.peerName,
      name,
      mime: record.mime,
      size,
    };
    this.offerEvents.emit(offer);

    let accepted = false;
    try {
      // No handler means no human is watching, so nothing is accepted. Consent
      // is never implicit.
      accepted = this.consent ? await this.consent(offer) : false;
    } catch (error) {
      accepted = false;
      this.errorEvents.emit({
        transferId,
        message: 'The consent prompt failed; the transfer was declined.',
        cause: error,
      });
    }

    // The peer may have cancelled while the prompt was open.
    if (!this.incoming.has(transferId)) return;

    this.sendFrame({ type: 'transfer:accept', transferId, accept: accepted });
    if (!accepted) {
      this.settleIncoming(state, 'cancelled', 'declined', 'Declined.');
      return;
    }

    state.accepted = true;
    state.record.status = 'active';
    this.emitRecord(state.record);
  }

  private handleBinary(data: ArrayBuffer): void {
    const decoded = decodeChunk(data);
    if (!decoded) {
      this.droppedFrames += 1;
      return;
    }

    const state = this.incoming.get(decoded.transferId);
    // Unknown, already finished, or not yet consented to: DROP the frame. The
    // alternative — buffering bytes for a transfer that may never be approved —
    // is exactly the memory exhaustion the size ceiling exists to prevent.
    if (!state || !state.accepted) {
      this.droppedFrames += 1;
      return;
    }

    const length = decoded.payload.byteLength;
    if (state.received + length > state.record.size) {
      this.sendFrame({
        type: 'transfer:cancel',
        transferId: state.record.transferId,
        reason: 'error',
      });
      this.settleIncoming(state, 'failed', 'error', 'The sender sent more data than it declared.');
      return;
    }

    // The view is kept rather than copied: it costs 40 extra bytes of retained
    // header per chunk and saves a 64 KiB memcpy, and Blob() snapshots it later.
    state.chunks.push(decoded.payload);
    state.received += length;
    state.record.bytes = state.received;

    if (state.received - state.acked >= ACK_INTERVAL_BYTES) {
      state.acked = state.received;
      this.sendFrame({
        type: 'transfer:ack',
        transferId: state.record.transferId,
        bytes: state.received,
      });
    }

    this.emitProgress(state.record);
  }

  private handleComplete(transferId: string): void {
    const outgoing = this.outgoing.get(transferId);
    if (outgoing) {
      // The peer echoing our own completion. Nothing left to do.
      return;
    }

    const state = this.incoming.get(transferId);
    if (!state) return;

    if (state.received !== state.record.size) {
      this.settleIncoming(
        state,
        'failed',
        'error',
        'The transfer ended early: got ' +
          formatBytes(state.received) +
          ' of ' +
          formatBytes(state.record.size) +
          '.',
      );
      return;
    }

    const blob = new Blob(state.chunks, { type: state.record.mime });
    // Chunks are released here; the Blob owns the bytes from now on.
    state.chunks.length = 0;

    let url: string;
    try {
      url = URL.createObjectURL(blob);
    } catch (error) {
      this.settleIncoming(state, 'failed', 'error', 'Could not open the received file.');
      this.errorEvents.emit({ transferId, message: describeError(error), cause: error });
      return;
    }

    let revoked = false;
    const received: ReceivedFile = {
      transferId,
      name: state.record.name,
      mime: state.record.mime,
      size: state.record.size,
      blob,
      url,
      revoke: () => {
        if (revoked) return;
        revoked = true;
        try {
          URL.revokeObjectURL(url);
        } catch {
          // Already gone, or the document is unloading.
        }
      },
    };

    this.sendFrame({ type: 'transfer:ack', transferId, bytes: state.received });
    this.settleIncoming(state, 'complete', undefined, null);
    this.fileEvents.emit(received);
  }

  private handleRemoteCancel(transferId: string, reason: TransferCancelReason): void {
    const outgoing = this.outgoing.get(transferId);
    if (outgoing) {
      const status: TransferStatus =
        reason === 'error' || reason === 'too_large' ? 'failed' : 'cancelled';
      this.settleOutgoing(outgoing, status, reason, describeCancel(reason, this.peerName));
      return;
    }
    const incoming = this.incoming.get(transferId);
    if (incoming) {
      this.settleIncoming(incoming, 'cancelled', reason, describeCancel(reason, this.peerName));
    }
  }

  private handleChannelClose(): void {
    const message = 'The connection to ' + this.peerName + ' closed.';
    for (const state of [...this.outgoing.values()]) {
      this.settleOutgoing(state, 'failed', 'error', message);
    }
    for (const state of [...this.incoming.values()]) {
      this.settleIncoming(state, 'failed', 'error', message);
    }
  }

  // -- refusal / settlement -------------------------------------------------

  /**
   * Declines an incoming transfer we will not even hold: tells the peer with a
   * `transfer:cancel`, and records a failed entry so the UI can say why.
   * Deliberately never enters `this.incoming`, so subsequent chunks for this id
   * hit the unknown-transferId drop path.
   */
  private refuse(
    transferId: string,
    name: string,
    mime: string,
    size: number,
    reason: TransferCancelReason,
    message: string,
  ): void {
    this.sendFrame({ type: 'transfer:cancel', transferId, reason });

    const record: TransferRecord = {
      transferId,
      direction: 'receive',
      peerId: this.peerId,
      peerName: this.peerName,
      name,
      mime: mime || 'application/octet-stream',
      size: Number.isFinite(size) ? size : 0,
      bytes: 0,
      status: 'failed',
      startedAt: this.now(),
      finishedAt: this.now(),
      error: message,
      cancelReason: reason,
    };
    this.finished.unshift(record);
    this.emitRecord(record);
    this.errorEvents.emit({ transferId, message });
  }

  private failOutgoing(
    state: OutgoingState,
    reason: TransferCancelReason,
    message: string,
    cause?: unknown,
  ): void {
    this.settleOutgoing(state, 'failed', reason, message);
    this.errorEvents.emit({ transferId: state.record.transferId, message, cause });
  }

  private settleOutgoing(
    state: OutgoingState,
    status: TransferStatus,
    reason: TransferCancelReason | undefined,
    error: string | null,
  ): void {
    if (!this.outgoing.has(state.record.transferId)) return;
    state.aborted = status !== 'complete';
    state.settleAccept?.(false);
    // Wake anything parked on backpressure so the send loop can notice `aborted`
    // instead of waiting on a drain event that may never arrive.
    for (const waiter of [...state.drainWaiters]) waiter();
    state.drainWaiters.clear();

    this.outgoing.delete(state.record.transferId);
    this.finalise(state.record, status, reason, error);
  }

  private settleIncoming(
    state: IncomingState,
    status: TransferStatus,
    reason: TransferCancelReason | undefined,
    error: string | null,
  ): void {
    if (!this.incoming.has(state.record.transferId)) return;
    this.incoming.delete(state.record.transferId);
    state.chunks.length = 0; // Release the partial payload immediately.
    this.finalise(state.record, status, reason, error);
  }

  private finalise(
    record: TransferRecord,
    status: TransferStatus,
    reason: TransferCancelReason | undefined,
    error: string | null,
  ): void {
    record.status = status;
    record.finishedAt = this.now();
    record.error = error;
    if (reason) record.cancelReason = reason;
    this.lastProgressAt.delete(record.transferId);
    this.finished.unshift(record);
    this.emitRecord(record);
  }

  // -- plumbing -------------------------------------------------------------

  private sendFrame(frame: DataChannelFrame): boolean {
    if (!this.channel.isOpen) return false;
    return this.channel.sendString(JSON.stringify(frame));
  }

  private emitRecord(record: TransferRecord): void {
    this.lastProgressAt.set(record.transferId, this.now());
    this.recordEvents.emit({ ...record });
  }

  /** Throttled: a 64 KiB chunk every few milliseconds must not re-render React. */
  private emitProgress(record: TransferRecord): void {
    const last = this.lastProgressAt.get(record.transferId) ?? 0;
    const now = this.now();
    if (now - last < PROGRESS_INTERVAL_MS && record.bytes < record.size) return;
    this.lastProgressAt.set(record.transferId, now);
    this.recordEvents.emit({ ...record });
  }

  private findRecord(transferId: string): TransferRecord | null {
    const outgoing = this.outgoing.get(transferId);
    if (outgoing) return outgoing.record;
    const incoming = this.incoming.get(transferId);
    if (incoming) return incoming.record;
    return this.finished.find((record) => record.transferId === transferId) ?? null;
  }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

function defaultTransferId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Fallback must still be exactly 36 chars, or encodeChunk throws.
  const hex = '0123456789abcdef';
  let out = '';
  for (let i = 0; i < 36; i += 1) {
    if (i === 8 || i === 13 || i === 18 || i === 23) {
      out += '-';
      continue;
    }
    out += hex.charAt(Math.floor(Math.random() * 16));
  }
  return out;
}

function concatBytes(a: Uint8Array, b: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.byteLength + b.byteLength);
  out.set(a, 0);
  out.set(b, a.byteLength);
  return out;
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message;
  return 'The transfer failed.';
}

function describeCancel(reason: TransferCancelReason, peerName: string): string {
  switch (reason) {
    case 'declined':
      return peerName + ' declined the file.';
    case 'too_large':
      return peerName + ' cannot hold a file that large.';
    case 'user_cancelled':
      return peerName + ' cancelled the transfer.';
    default:
      return 'The transfer failed on ' + peerName + '.';
  }
}

function isRecordObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

/**
 * Parses a STRING frame. The peer is another copy of this extension, but it is
 * still a remote party, so every field is checked before it is trusted.
 */
export function parseFrame(raw: string): DataChannelFrame | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isRecordObject(parsed)) return null;

  const type = parsed['type'];
  const transferId = parsed['transferId'];

  switch (type) {
    case 'text': {
      const text = parsed['text'];
      return typeof text === 'string' ? { type: 'text', text } : null;
    }
    case 'transfer:start': {
      const name = parsed['name'];
      const mime = parsed['mime'];
      const size = parsed['size'];
      if (typeof transferId !== 'string' || typeof name !== 'string') return null;
      if (typeof size !== 'number') return null;
      return {
        type: 'transfer:start',
        transferId,
        name,
        mime: typeof mime === 'string' ? mime : '',
        size,
      };
    }
    case 'transfer:accept': {
      const accept = parsed['accept'];
      if (typeof transferId !== 'string' || typeof accept !== 'boolean') return null;
      return { type: 'transfer:accept', transferId, accept };
    }
    case 'transfer:ack': {
      const bytes = parsed['bytes'];
      if (typeof transferId !== 'string' || typeof bytes !== 'number') return null;
      return { type: 'transfer:ack', transferId, bytes };
    }
    case 'transfer:complete': {
      if (typeof transferId !== 'string') return null;
      return { type: 'transfer:complete', transferId };
    }
    case 'transfer:cancel': {
      if (typeof transferId !== 'string') return null;
      return { type: 'transfer:cancel', transferId, reason: asCancelReason(parsed['reason']) };
    }
    default:
      return null;
  }
}

/** Anything unrecognised becomes `error`; never widen the union at runtime. */
export function asCancelReason(value: unknown): TransferCancelReason {
  return value === 'user_cancelled' || value === 'too_large' || value === 'declined'
    ? value
    : 'error';
}
