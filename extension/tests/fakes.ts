/**
 * Test doubles for the transport layer.
 *
 * The whole point of `TransferChannel` and `PeerLinkOptions.createConnection`
 * being injectable is that the send loop, the backpressure wait, the too-large
 * refusal and the ICE candidate buffer can be exercised with no browser.
 */

import { HIGH_WATER } from '@/types';
import type { DataChannelFrame } from '@/types';
import { parseFrame } from '@/lib/transfer';
import type { TransferChannel } from '@/lib/transfer';

type Fn<T> = (value: T) => void;

class Fanout<T> {
  private readonly listeners = new Set<Fn<T>>();

  add(listener: Fn<T>): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  emit(value: T): void {
    for (const listener of [...this.listeners]) listener(value);
  }
}

/** An in-memory DataChannel with a controllable buffer. */
export class FakeChannel implements TransferChannel {
  isOpen = true;
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;

  readonly sentStrings: string[] = [];
  readonly sentBinary: ArrayBuffer[] = [];

  /**
   * When true, every binary send pushes bufferedAmount above HIGH_WATER, so the
   * next chunk must wait for drain(). That is how the backpressure test proves
   * the send loop actually parks instead of running ahead of the network.
   */
  stallAfterSend = false;

  private readonly strings = new Fanout<string>();
  private readonly binaries = new Fanout<ArrayBuffer>();
  private readonly lows = new Fanout<void>();
  private readonly closes = new Fanout<void>();

  sendString(text: string): boolean {
    if (!this.isOpen) return false;
    this.sentStrings.push(text);
    return true;
  }

  sendBinary(data: ArrayBuffer): boolean {
    if (!this.isOpen) return false;
    this.sentBinary.push(data);
    if (this.stallAfterSend) this.bufferedAmount = HIGH_WATER + 1;
    return true;
  }

  onString(listener: (text: string) => void): () => void {
    return this.strings.add(listener);
  }

  onBinary(listener: (data: ArrayBuffer) => void): () => void {
    return this.binaries.add(listener);
  }

  onBufferedAmountLow(listener: () => void): () => void {
    return this.lows.add(listener);
  }

  onClose(listener: () => void): () => void {
    return this.closes.add(listener);
  }

  // -- test controls --------------------------------------------------------

  /** Delivers a string frame as if the peer had sent it. */
  receiveString(frame: DataChannelFrame): void {
    this.strings.emit(JSON.stringify(frame));
  }

  receiveRaw(text: string): void {
    this.strings.emit(text);
  }

  receiveBinary(data: ArrayBuffer): void {
    this.binaries.emit(data);
  }

  /** Empties the buffer and fires `bufferedamountlow`. */
  drain(): void {
    this.bufferedAmount = 0;
    this.lows.emit(undefined);
  }

  closeChannel(): void {
    this.isOpen = false;
    this.closes.emit(undefined);
  }

  /** Every control frame sent so far, parsed. */
  frames(): DataChannelFrame[] {
    const out: DataChannelFrame[] = [];
    for (const raw of this.sentStrings) {
      const frame = parseFrame(raw);
      if (frame) out.push(frame);
    }
    return out;
  }

  lastFrame(): DataChannelFrame | null {
    const all = this.frames();
    return all[all.length - 1] ?? null;
  }
}

// ---------------------------------------------------------------------------
// WebRTC doubles
// ---------------------------------------------------------------------------

export class FakeDataChannel {
  readyState: RTCDataChannelState = 'connecting';
  binaryType = 'arraybuffer';
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;

  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onbufferedamountlow: (() => void) | null = null;

  readonly sent: unknown[] = [];
  closed = false;

  readonly label: string;

  // Written out rather than a parameter property: Node's type stripping is
  // strip-only and cannot emit the assignment a parameter property implies.
  constructor(label: string) {
    this.label = label;
  }

  send(data: unknown): void {
    this.sent.push(data);
  }

  close(): void {
    this.closed = true;
    this.readyState = 'closed';
  }

  open(): void {
    this.readyState = 'open';
    this.onopen?.();
  }
}

/**
 * Enough of RTCPeerConnection to drive negotiation. Records the order of calls
 * so a test can prove the DataChannel is created BEFORE createOffer.
 */
export class FakePeerConnection {
  signalingState: RTCSignalingState = 'stable';
  connectionState: RTCPeerConnectionState = 'new';
  iceConnectionState: RTCIceConnectionState = 'new';
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;

  onicecandidate: ((event: RTCPeerConnectionIceEvent) => void) | null = null;
  ondatachannel: ((event: RTCDataChannelEvent) => void) | null = null;
  onconnectionstatechange: (() => void) | null = null;
  oniceconnectionstatechange: (() => void) | null = null;
  onicecandidateerror: ((event: Event) => void) | null = null;

  readonly calls: string[] = [];
  readonly addedCandidates: RTCIceCandidateInit[] = [];
  readonly channels: FakeDataChannel[] = [];
  rollbacks = 0;
  closed = false;

  createDataChannel(label: string): FakeDataChannel {
    this.calls.push('createDataChannel');
    const channel = new FakeDataChannel(label);
    this.channels.push(channel);
    return channel;
  }

  createOffer(): Promise<RTCSessionDescriptionInit> {
    this.calls.push('createOffer');
    return Promise.resolve({ type: 'offer', sdp: 'FAKE-OFFER' });
  }

  createAnswer(): Promise<RTCSessionDescriptionInit> {
    this.calls.push('createAnswer');
    return Promise.resolve({ type: 'answer', sdp: 'FAKE-ANSWER' });
  }

  setLocalDescription(description?: RTCLocalSessionDescriptionInit): Promise<void> {
    if (description?.type === 'rollback') {
      this.rollbacks += 1;
      this.signalingState = 'stable';
      return Promise.resolve();
    }
    this.calls.push('setLocalDescription');
    // RTCLocalSessionDescriptionInit leaves `type` optional; the real API then
    // infers it from the signalling state. The fake only ever receives an
    // explicit description, so an absent type falls back to an offer.
    const applied: RTCSessionDescriptionInit = {
      type: description?.type ?? 'offer',
      sdp: description?.sdp ?? 'FAKE-OFFER',
    };
    this.localDescription = applied;
    this.signalingState = applied.type === 'offer' ? 'have-local-offer' : 'stable';
    return Promise.resolve();
  }

  setRemoteDescription(description: RTCSessionDescriptionInit): Promise<void> {
    this.calls.push('setRemoteDescription');
    this.remoteDescription = description;
    this.signalingState = description.type === 'offer' ? 'have-remote-offer' : 'stable';
    return Promise.resolve();
  }

  addIceCandidate(candidate: RTCIceCandidateInit): Promise<void> {
    // The real API rejects when no remote description has been applied yet.
    // Reproducing that is the whole point: it is what makes an unbuffered
    // candidate a silently lost candidate.
    if (!this.remoteDescription) {
      return Promise.reject(new Error('InvalidStateError: no remote description'));
    }
    this.addedCandidates.push(candidate);
    return Promise.resolve();
  }

  close(): void {
    this.closed = true;
    this.connectionState = 'closed';
  }

  // -- test controls --------------------------------------------------------

  emitLocalCandidate(candidate: string, sdpMid = '0', sdpMLineIndex = 0): void {
    // Only the three fields PeerLink reads are needed; the constructed object
    // stands in for an RTCIceCandidate.
    const fake = { candidate, sdpMid, sdpMLineIndex };
    this.onicecandidate?.({ candidate: fake } as unknown as RTCPeerConnectionIceEvent);
  }

  emitRemoteChannel(channel: FakeDataChannel): void {
    this.ondatachannel?.({ channel } as unknown as RTCDataChannelEvent);
  }

  setConnectionState(state: RTCPeerConnectionState): void {
    this.connectionState = state;
    this.onconnectionstatechange?.();
  }

  setIceConnectionState(state: RTCIceConnectionState): void {
    this.iceConnectionState = state;
    this.oniceconnectionstatechange?.();
  }
}

/**
 * Adapts a FakePeerConnection to the RTCPeerConnection parameter type.
 *
 * Cast rationale: the fake implements only the members PeerLink touches, which
 * is deliberate — a full structural implementation of RTCPeerConnection would
 * be several hundred lines of unused surface, and every extra member would be
 * one more thing that could drift from the real API without anything noticing.
 */
export function asPeerConnection(fake: FakePeerConnection): RTCPeerConnection {
  return fake as unknown as RTCPeerConnection;
}
