/**
 * Device discovery.
 *
 * Discovery itself is the daemon's job — it runs mDNS (`_nearby-share._tcp`,
 * domain `local.`) because a browser extension cannot speak multicast DNS.
 * This module is the extension-side view of that: it subscribes to the
 * daemon's `devices` push and keeps a cached roster for the side panel.
 *
 * TODO(M4): daemon-side mDNS advertise/browse via libp2p/zeroconf.
 * TODO(M5): subscribe to the `devices` push here and drive the side panel list.
 */

import type { DaemonClient } from '@/lib/daemon';
import type { Device } from '@/types';

/** Called whenever the known device set changes. */
export type DevicesListener = (devices: Device[]) => void;

/**
 * Devices shown in the side panel while no daemon is reachable, so the UI is
 * inspectable during Milestone 1. Never merged with real discovery results.
 */
export const FAKE_DEVICES: Device[] = [
  {
    id: 'abc',
    name: "Aashish's Laptop",
    address: '192.168.1.24',
    port: 8765,
    platform: 'windows',
    version: '0.1.0',
    status: 'online',
    lastSeen: 0,
  },
  {
    id: 'def',
    name: 'Desktop',
    address: '192.168.1.31',
    port: 8765,
    platform: 'linux',
    version: '0.1.0',
    status: 'online',
    lastSeen: 0,
  },
];

/**
 * Starts mirroring the daemon's device roster.
 *
 * Returns an unsubscribe function. In Milestone 1 the daemon never sends
 * `devices`, so the listener simply never fires.
 */
export function watchDevices(client: DaemonClient, onChange: DevicesListener): () => void {
  return client.on('devices', (message) => {
    onChange(message.devices);
  });
}

/**
 * Sorts a roster for display: online first, then most recently seen, then by
 * name so the list does not jitter between renders.
 */
export function sortDevices(devices: readonly Device[]): Device[] {
  return [...devices].sort((a, b) => {
    if (a.status !== b.status) return a.status === 'online' ? -1 : 1;
    if (a.lastSeen !== b.lastSeen) return b.lastSeen - a.lastSeen;
    return a.name.localeCompare(b.name);
  });
}
