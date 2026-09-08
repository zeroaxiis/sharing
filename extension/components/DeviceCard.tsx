import type { Device } from '@/types';

const PLATFORM_LABEL: Record<Device['platform'], string> = {
  windows: 'Windows',
  darwin: 'macOS',
  linux: 'Linux',
  unknown: 'Unknown',
};

export interface DeviceCardProps {
  device: Device;
  selected?: boolean;
  onSelect?: (device: Device) => void;
}

/** One row in the nearby-devices list: status dot, name, and address. */
export function DeviceCard({ device, selected = false, onSelect }: DeviceCardProps) {
  const online = device.status === 'online';
  const meta = [device.address, PLATFORM_LABEL[device.platform]].filter(Boolean).join(' · ');

  return (
    <button
      type="button"
      className={'device' + (selected ? ' device--selected' : '')}
      aria-pressed={selected}
      onClick={() => onSelect?.(device)}
    >
      <span
        className={'device__dot' + (online ? '' : ' device__dot--offline')}
        aria-hidden="true"
      />
      <span className="device__text">
        <span className="device__name">{device.name}</span>
        {meta ? <span className="device__meta">{meta}</span> : null}
      </span>
      <span className="device__status">{online ? 'Online' : 'Offline'}</span>
    </button>
  );
}

export default DeviceCard;
