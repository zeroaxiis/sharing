/**
 * Device discovery — the extension-side view of the daemon's mDNS browser.
 *
 * Discovery itself is the daemon's job: a browser extension cannot speak
 * multicast DNS, so the daemon advertises and browses `_sharing._tcp`
 * (domain `local.`) and pushes the resulting roster over the control socket as
 * `{"type":"devices","devices":[Device]}`. This module subscribes to that push,
 * keeps a roster, and provides the sorting/filtering the side panel needs.
 *
 * A device is considered online while it was seen within DEVICE_STALE_MS (30s).
 * The daemon sends its own `status`, but a roster can also go stale in the
 * panel when the socket drops mid-session, so `list()` re-derives it.
 */

import { newId } from '@/lib/daemon';
import type { DaemonClient } from '@/lib/daemon';
import { DEVICE_STALE_MS, type Device, type PeerState, type PeerStatePush } from '@/types';

/** Called whenever the known device set changes. */
export type DevicesListener = (devices: Device[]) => void;

/**
 * PLACEHOLDER DATA — NOT REAL DEVICES.
 *
 * The side panel renders these only while no daemon is reachable, so the UI is
 * inspectable with nothing running. They are never merged with a real roster,
 * never persisted, and must never be used as a transfer target: their ids do
 * not correspond to anything on the network. Guard with isPlaceholderDevice().
 */
export const FAKE_DEVICES: Device[] = [
  {
    id: 'placeholder-laptop',
    name: "Aashish's Laptop",
    address: '192.168.1.24',
    port: 8766,
    platform: 'windows',
    version: '0.1.0',
    status: 'online',
    lastSeen: 0,
    paired: true,
  },
  {
    id: 'placeholder-desktop',
    name: 'Desktop',
    address: '192.168.1.31',
    port: 8766,
    platform: 'linux',
    version: '0.1.0',
    status: 'online',
    lastSeen: 0,
    paired: false,
  },
];

const PLACEHOLDER_IDS: ReadonlySet<string> = new Set(FAKE_DEVICES.map((device) => device.id));

/** True for the mock roster above. Nothing may be sent to one of these. */
export function isPlaceholderDevice(device: Device): boolean {
  return PLACEHOLDER_IDS.has(device.id);
}

// ---------------------------------------------------------------------------
// Daemon subscription
// ---------------------------------------------------------------------------

/**
 * Mirrors the daemon's device roster.
 *
 * Returns an unsubscribe function. The listener fires only when the daemon
 * pushes; with no daemon running it simply never fires.
 */
export function watchDevices(client: DaemonClient, onChange: DevicesListener): () => void {
  return client.on('devices', (message) => {
    onChange(message.devices);
  });
}

/** Subscribes to per-device peer-plane state (`peer:state`). */
export function watchPeerState(
  client: DaemonClient,
  onChange: (push: PeerStatePush) => void,
): () => void {
  return client.on('peer:state', (message) => {
    onChange(message);
  });
}

/**
 * Asks the daemon to re-browse now rather than waiting for the next push.
 * Returns false when the socket is not open.
 */
export function requestDevicesRefresh(client: DaemonClient): boolean {
  return client.send({ type: 'devices:refresh', id: newId() });
}

// ---------------------------------------------------------------------------
// Pure roster helpers
// ---------------------------------------------------------------------------

/** True while the device was seen recently enough to be treated as present. */
export function isFresh(device: Device, now: number = Date.now()): boolean {
  // lastSeen 0 means "the daemon did not say"; trust its own status field then.
  if (device.lastSeen <= 0) return device.status === 'online';
  return now - device.lastSeen <= DEVICE_STALE_MS;
}

/**
 * Re-derives `status` from `lastSeen`, so a roster that stopped updating decays
 * to offline instead of showing green forever.
 */
export function withDerivedStatus(devices: readonly Device[], now: number = Date.now()): Device[] {
  return devices.map((device) => {
    const status = isFresh(device, now) ? 'online' : 'offline';
    return status === device.status ? device : { ...device, status };
  });
}

/**
 * Sorts a roster for display: online first, then paired, then most recently
 * seen, then by name so the list does not jitter between renders.
 */
export function sortDevices(devices: readonly Device[]): Device[] {
  return [...devices].sort((a, b) => {
    if (a.status !== b.status) return a.status === 'online' ? -1 : 1;
    if (a.paired !== b.paired) return a.paired ? -1 : 1;
    if (a.lastSeen !== b.lastSeen) return b.lastSeen - a.lastSeen;
    return a.name.localeCompare(b.name);
  });
}

export interface DeviceFilter {
  /** Case-insensitive substring match on name or address. */
  query?: string;
  onlineOnly?: boolean;
  pairedOnly?: boolean;
  /** Drop this id — normally our own, so we never list ourselves. */
  excludeId?: string;
}

export function filterDevices(devices: readonly Device[], filter: DeviceFilter = {}): Device[] {
  const query = filter.query?.trim().toLowerCase() ?? '';
  return devices.filter((device) => {
    if (filter.excludeId && device.id === filter.excludeId) return false;
    if (filter.onlineOnly && device.status !== 'online') return false;
    if (filter.pairedOnly && !device.paired) return false;
    if (query === '') return true;
    return (
      device.name.toLowerCase().includes(query) || device.address.toLowerCase().includes(query)
    );
  });
}

// ---------------------------------------------------------------------------
// DeviceRoster
// ---------------------------------------------------------------------------

/**
 * The current device set, plus the per-device peer state that arrives on a
 * separate message.
 *
 * The daemon's `devices` push is authoritative and REPLACES the roster — a
 * device that disappears from mDNS must disappear here too, so this is a
 * replace, never a merge. `peer:state` is overlaid on top and survives a
 * replace, because connectivity to a peer is orthogonal to whether its mDNS
 * record was in the last browse result.
 */
export class DeviceRoster {
  private devices: Device[] = [];
  private readonly peerStates = new Map<string, PeerStatePush>();
  private readonly listeners = new Set<DevicesListener>();

  /** Applies a `devices` push. */
  replace(devices: readonly Device[]): void {
    this.devices = [...devices];
    // A device that vanished from the roster has no meaningful peer state.
    const present = new Set(this.devices.map((device) => device.id));
    for (const id of [...this.peerStates.keys()]) {
      if (!present.has(id)) this.peerStates.delete(id);
    }
    this.notify();
  }

  /** Applies a `peer:state` push. */
  applyPeerState(push: PeerStatePush): void {
    this.peerStates.set(push.deviceId, push);
    this.notify();
  }

  /** Peer-plane state for a device, or 'offline' when nothing is known. */
  peerState(deviceId: string): PeerState {
    return this.peerStates.get(deviceId)?.state ?? 'offline';
  }

  /** The last message that came with a `peer:state`, for error display. */
  peerMessage(deviceId: string): string {
    return this.peerStates.get(deviceId)?.message ?? '';
  }

  /** Sorted, with `status` re-derived from `lastSeen`. */
  list(now: number = Date.now()): Device[] {
    return sortDevices(withDerivedStatus(this.devices, now));
  }

  get(deviceId: string): Device | null {
    return this.devices.find((device) => device.id === deviceId) ?? null;
  }

  get size(): number {
    return this.devices.length;
  }

  clear(): void {
    this.devices = [];
    this.peerStates.clear();
    this.notify();
  }

  subscribe(listener: DevicesListener): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /**
   * Wires the roster to a live daemon client. Returns one unsubscribe that
   * detaches both subscriptions.
   */
  attach(client: DaemonClient): () => void {
    const offDevices = watchDevices(client, (devices) => {
      this.replace(devices);
    });
    const offPeerState = watchPeerState(client, (push) => {
      this.applyPeerState(push);
    });
    return () => {
      offDevices();
      offPeerState();
    };
  }

  private notify(): void {
    const snapshot = this.list();
    for (const listener of [...this.listeners]) {
      try {
        listener(snapshot);
      } catch (error) {
        console.error('[sharing] devices listener threw', error);
      }
    }
  }
}
