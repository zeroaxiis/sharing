import { DeviceCard } from '@/components/DeviceCard';
import { sortDevices } from '@/lib/discovery';
import type { Device } from '@/types';

export interface DeviceListProps {
  devices: readonly Device[];
  selectedId?: string | null;
  onSelect?: (device: Device) => void;
}

/** The "Nearby devices" list, with the mock's empty state. */
export function DeviceList({ devices, selectedId = null, onSelect }: DeviceListProps) {
  if (devices.length === 0) {
    return (
      <div className="empty">
        <p className="empty__title">No devices?</p>
        <p className="empty__hint">Check your connection</p>
      </div>
    );
  }

  return (
    <ul className="device-list">
      {sortDevices(devices).map((device) => (
        <li key={device.id}>
          <DeviceCard
            device={device}
            selected={device.id === selectedId}
            onSelect={onSelect}
          />
        </li>
      ))}
    </ul>
  );
}

export default DeviceList;
