/**
 * View models for the side panel's two long-lived conversations: pairing a
 * device, and holding a peer connection to one.
 *
 * These are UI state, not wire types — nothing here crosses a socket, and the
 * Go daemon has no mirror of it. They live in `lib/` rather than beside the
 * panel because both the hook that owns the state and the components that
 * render it need the shapes, and a component importing from `entrypoints/`
 * would invert the dependency.
 */

import type { PairDirection, PeerState } from '@/types';
import type { PeerLinkState } from '@/lib/webrtc';

// ---------------------------------------------------------------------------
// Peer session
// ---------------------------------------------------------------------------

/**
 * How the panel presents a peer connection.
 *
 * `unstable` is PeerLink's `disconnected`: ICE lost the path but may recover on
 * its own. It is deliberately not folded into `failed`, because telling someone
 * a transfer died when it is about to resume is its own kind of lie.
 */
export type SessionPhase = 'connecting' | 'connected' | 'unstable' | 'failed' | 'closed';

export interface SessionView {
  peerId: string;
  peerName: string;
  phase: SessionPhase;
  /** Whichever failure the link last reported, already phrased for a person. */
  error: string | null;
  /** True when we created the DataChannel — i.e. the user started this. */
  outgoing: boolean;
}

export function phaseFromLink(state: PeerLinkState): SessionPhase {
  switch (state) {
    case 'new':
    case 'connecting':
      return 'connecting';
    case 'connected':
      return 'connected';
    case 'disconnected':
      return 'unstable';
    case 'failed':
      return 'failed';
    case 'closed':
      return 'closed';
  }
}

/** One line of status for the connection banner. Never a bare spinner. */
export function describePhase(view: SessionView): string {
  switch (view.phase) {
    case 'connecting':
      return 'Connecting to ' + view.peerName + '…';
    case 'connected':
      return 'Connected to ' + view.peerName + ' directly. Files never leave your network.';
    case 'unstable':
      return view.error ?? 'The connection to ' + view.peerName + ' dropped. Trying to recover…';
    case 'failed':
      return view.error ?? 'Could not connect to ' + view.peerName + '.';
    case 'closed':
      return 'Disconnected from ' + view.peerName + '.';
  }
}

/** True while the panel is holding something the user must not close. */
export function sessionIsLive(view: SessionView | null): boolean {
  return view !== null && (view.phase === 'connecting' || view.phase === 'connected' || view.phase === 'unstable');
}

// ---------------------------------------------------------------------------
// Pairing
// ---------------------------------------------------------------------------

/**
 * `starting`   pair:start sent, waiting for the daemon to produce a code
 * `code`       the six digits are on screen; the human has not answered
 * `submitting` pair:confirm sent, waiting for pair:result
 * `done`       paired
 * `failed`     rejected, timed out, or refused — `message` says which
 */
export type PairingPhase = 'starting' | 'code' | 'submitting' | 'done' | 'failed';

export interface PairingView {
  deviceId: string;
  name: string;
  direction: PairDirection;
  phase: PairingPhase;
  /** Six digits. Null until the daemon has computed them. */
  code: string | null;
  message: string | null;
}

// ---------------------------------------------------------------------------
// Received payloads
// ---------------------------------------------------------------------------

/**
 * Something that arrived over the DataChannel and is waiting on screen.
 *
 * A `file` item owns a live object URL. `revoke` is idempotent (the transfer
 * engine guarantees that) and MUST be called when the item leaves the list or
 * the Blob is pinned for the life of the document.
 */
export type ReceivedItem =
  | {
      kind: 'file';
      id: string;
      name: string;
      mime: string;
      size: number;
      url: string;
      revoke: () => void;
      from: string;
      at: number;
    }
  | { kind: 'text'; id: string; text: string; from: string; at: number };

/** Peer-plane words for the device row, when the daemon has an opinion. */
export function describePeerState(state: PeerState): string {
  switch (state) {
    case 'offline':
      return 'Offline';
    case 'online':
      return 'Reachable';
    case 'pairing':
      return 'Pairing…';
    case 'paired':
      return 'Paired';
    case 'error':
      return 'Unreachable';
  }
}
