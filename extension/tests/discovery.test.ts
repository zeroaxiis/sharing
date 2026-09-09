import { assert, assertEqual, test } from './harness.ts';
import {
  DeviceRoster,
  FAKE_DEVICES,
  filterDevices,
  isFresh,
  isPlaceholderDevice,
  sortDevices,
  withDerivedStatus,
} from '@/lib/discovery';
import { DEVICE_STALE_MS, type Device } from '@/types';

const NOW = 1_757_370_000_000;

function device(overrides: Partial<Device> & { id: string }): Device {
  return {
    name: overrides.id,
    address: '192.168.1.2',
    port: 8766,
    platform: 'linux',
    version: '0.1.0',
    status: 'online',
    lastSeen: NOW,
    paired: false,
    ...overrides,
  };
}

test('sortDevices puts online first, then paired, then most recently seen', () => {
  const sorted = sortDevices([
    device({ id: 'offline-recent', status: 'offline', lastSeen: NOW }),
    device({ id: 'online-old', lastSeen: NOW - 5000 }),
    device({ id: 'online-paired-old', paired: true, lastSeen: NOW - 9000 }),
  ]);

  assertEqual(sorted[0]?.id, 'online-paired-old', 'paired online device leads');
  assertEqual(sorted[1]?.id, 'online-old', 'other online device follows');
  assertEqual(sorted[2]?.id, 'offline-recent', 'offline devices sink');
});

test('a device unseen for longer than DEVICE_STALE_MS decays to offline', () => {
  const stale = device({ id: 'stale', lastSeen: NOW - DEVICE_STALE_MS - 1 });
  const fresh = device({ id: 'fresh', lastSeen: NOW - 1000 });

  assertEqual(isFresh(stale, NOW), false, 'stale');
  assertEqual(isFresh(fresh, NOW), true, 'fresh');

  const derived = withDerivedStatus([stale, fresh], NOW);
  assertEqual(derived[0]?.status, 'offline', 'stale device is offline');
  assertEqual(derived[1]?.status, 'online', 'fresh device stays online');
});

test('filterDevices matches name or address and honours the flags', () => {
  const roster = [
    device({ id: 'a', name: 'Desktop', address: '192.168.1.31', paired: true }),
    device({ id: 'b', name: 'Phone', address: '192.168.1.44', status: 'offline' }),
    device({ id: 'self', name: 'This Laptop', address: '192.168.1.24' }),
  ];

  assertEqual(filterDevices(roster, { query: 'desk' }).length, 1, 'name match');
  assertEqual(filterDevices(roster, { query: '1.44' }).length, 1, 'address match');
  assertEqual(filterDevices(roster, { onlineOnly: true }).length, 2, 'online only');
  assertEqual(filterDevices(roster, { pairedOnly: true }).length, 1, 'paired only');
  assertEqual(filterDevices(roster, { excludeId: 'self' }).length, 2, 'exclude self');
  assertEqual(filterDevices(roster).length, 3, 'no filter');
});

test('DeviceRoster replaces rather than merges, and overlays peer state', () => {
  const roster = new DeviceRoster();
  const seen: string[][] = [];
  roster.subscribe((devices) => {
    seen.push(devices.map((entry) => entry.id));
  });

  roster.replace([device({ id: 'a' }), device({ id: 'b' })]);
  roster.applyPeerState({ type: 'peer:state', deviceId: 'a', state: 'paired', message: '' });
  assertEqual(roster.peerState('a'), 'paired', 'peer state applied');
  assertEqual(roster.peerState('b'), 'offline', 'unknown peer state defaults to offline');

  // 'b' left the network: a replace must drop it, not merge it forward.
  roster.replace([device({ id: 'a' })]);
  assertEqual(roster.size, 1, 'roster size after replace');
  assertEqual(roster.get('b'), null, 'a vanished device must be gone');
  assertEqual(roster.peerState('a'), 'paired', 'peer state survives a replace');

  assertEqual(seen.length, 3, 'every mutation notifies');
  assertEqual(seen[2]?.length, 1, 'last snapshot has one device');
});

test('DeviceRoster forgets peer state for a device that disappears', () => {
  const roster = new DeviceRoster();
  roster.replace([device({ id: 'a' })]);
  roster.applyPeerState({ type: 'peer:state', deviceId: 'a', state: 'error', message: 'nope' });
  assertEqual(roster.peerMessage('a'), 'nope', 'message retained');

  roster.replace([device({ id: 'z' })]);
  assertEqual(roster.peerState('a'), 'offline', 'stale peer state cleared');
  assertEqual(roster.peerMessage('a'), '', 'stale message cleared');
});

test('the placeholder roster is recognisable so nothing is ever sent to it', () => {
  assert(FAKE_DEVICES.length > 0, 'the placeholder roster should not be empty');
  for (const entry of FAKE_DEVICES) {
    assert(isPlaceholderDevice(entry), 'every fake device must be flagged');
  }
  assertEqual(isPlaceholderDevice(device({ id: 'real' })), false, 'a real device is not a fake');
});
