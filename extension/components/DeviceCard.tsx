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

/**
 * A neutral device glyph for the card's left gutter. Deliberately one shape for
 * every platform: the OS is already named in the meta line, and drawing vendor
 * logos here would put someone else's trademarks in our UI.
 */
function DeviceGlyph() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <rect x="1.75" y="2.75" width="12.5" height="8.5" rx="1.5" />
      <path d="M5.5 13.75h5" />
    </svg>
  );
}

/** One card in the nearby-devices list: icon gutter, name, address, status. */
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
      <span className="device__icon">
        <DeviceGlyph />
      </span>
      <span className="device__text">
        <span className="device__name" title={device.name}>
          {device.name}
        </span>
        {meta ? (
          <span className="device__meta" title={meta}>
            {meta}
          </span>
        ) : null}
      </span>
      {/* The dot is decorative; the word next to it is what conveys status. */}
      <span className="device__status">
        <span
          className={'device__dot' + (online ? '' : ' device__dot--offline')}
          aria-hidden="true"
        />
        {online ? 'Online' : 'Offline'}
      </span>
    </button>
  );
}

export default DeviceCard;
