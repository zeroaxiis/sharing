import { DeviceCard } from '@/components/DeviceCard';
import { sortDevices } from '@/lib/discovery';
import type { Device, PeerState, PeerStatePush } from '@/types';

export interface DeviceListProps {
  devices: readonly Device[];
  /** The device the panel currently holds a peer connection to. */
  selectedId?: string | null;
  /** Peer-plane state per device id, from the daemon's `peer:state` pushes. */
  peerStates?: ReadonlyMap<string, PeerStatePush>;
  /** Device id whose own action is in flight. */
  busyId?: string | null;
  disabled?: boolean;
  onPair?: (device: Device) => void;
  onSend?: (device: Device) => void;
  onForget?: (device: Device) => void;
  /** Rendered instead of the default copy when the list is empty. */
  emptyTitle?: string;
  emptyHint?: string;
}

/** The "Nearby devices" list. */
export function DeviceList({
  devices,
  selectedId = null,
  peerStates,
  busyId = null,
  disabled = false,
  onPair,
  onSend,
  onForget,
  emptyTitle = 'No devices?',
  emptyHint = 'Check your connection',
}: DeviceListProps) {
  if (devices.length === 0) {
    return (
      <div className="empty">
        <p className="empty__title">{emptyTitle}</p>
        <p className="empty__hint">{emptyHint}</p>
      </div>
    );
  }

  return (
    <ul className="device-list">
      {sortDevices(devices).map((device) => {
        const push = peerStates?.get(device.id);
        const peerState: PeerState | undefined = push?.state;
        return (
          <li key={device.id}>
            <DeviceCard
              device={device}
              selected={device.id === selectedId}
              peerState={peerState}
              peerMessage={push?.message ?? ''}
              busy={device.id === busyId}
              disabled={disabled}
              onPair={onPair}
              onSend={onSend}
              onForget={onForget}
            />
          </li>
        );
      })}
    </ul>
  );
}

export default DeviceList;
