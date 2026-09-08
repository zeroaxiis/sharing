/**
 * WebRTC peer connection between two Nearby Share devices.
 *
 * The daemon is only the signalling relay (`signal:offer` / `signal:answer` /
 * `signal:ice`); bytes themselves travel peer-to-peer over a DataChannel on the
 * local network, so no STUN/TURN server is needed for the common case.
 *
 * TODO(M7): offer/answer exchange over the daemon socket.
 * TODO(M8): DataChannel setup, backpressure, and teardown.
 */

import type { DaemonClient } from '@/lib/daemon';

/** Everything a peer connection needs from its owner. */
export interface PeerOptions {
  client: DaemonClient;
  /** Device id of the remote peer. */
  remoteId: string;
  /** Our own device id, echoed as `from` on every signalling message. */
  localId: string;
}

export interface PeerConnectionHandle {
  /** Opens the DataChannel and sends `signal:offer`. */
  start(): Promise<void>;
  /** Closes the peer connection and unsubscribes from signalling. */
  close(): void;
}

/**
 * Local-network-only configuration: host candidates are enough for two devices
 * on the same LAN, and adding public ICE servers would leak addresses.
 */
export const RTC_CONFIG: RTCConfiguration = {
  iceServers: [],
  iceTransportPolicy: 'all',
};

/** Name of the single DataChannel used for control + payload framing. */
export const DATA_CHANNEL_LABEL = 'nearby-share';

/**
 * Creates a peer connection driven by the daemon's signalling messages.
 *
 * TODO(M7): implement. Returns an inert handle for now so callers can be
 * written against the real shape.
 */
export function createPeerConnection(options: PeerOptions): PeerConnectionHandle {
  const { remoteId } = options;
  return {
    async start(): Promise<void> {
      console.warn('[nearby-share] createPeerConnection is a stub (M7):', remoteId);
    },
    close(): void {
      // Nothing to tear down yet.
    },
  };
}
