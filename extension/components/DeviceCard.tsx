import type { Device, PeerState } from '@/types';

const PLATFORM_LABEL: Record<Device['platform'], string> = {
  windows: 'Windows',
  darwin: 'macOS',
  linux: 'Linux',
  unknown: 'Unknown',
};

export interface DeviceCardProps {
  device: Device;
  /** True while this device is the one the panel has a peer connection to. */
  selected?: boolean;
  /** Peer-plane state from the daemon's `peer:state` push. */
  peerState?: PeerState;
  /** The message that came with `peer:state`; shown only when it says something. */
  peerMessage?: string;
  /** Disables both actions — no daemon, or a placeholder row. */
  disabled?: boolean;
  /** True while this row's own action is in flight (pairing, connecting). */
  busy?: boolean;
  /** Start pairing. Offered only for an unpaired device. */
  onPair?: (device: Device) => void;
  /** Open a peer connection and make this the send target. Paired devices only. */
  onSend?: (device: Device) => void;
  /** Drop the stored pairing token. */
  onForget?: (device: Device) => void;
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

/**
 * Peer-plane state worth putting on screen.
 *
 * `online` and `offline` are already covered by the mDNS status dot, so
 * repeating them here would just be noise; `pairing` and `error` are the two a
 * user can act on.
 */
function peerNote(state: PeerState | undefined, message: string): string | null {
  if (state === 'pairing') return message || 'Pairing…';
  if (state === 'error') return message || 'The daemon could not reach this device.';
  return null;
}

/**
 * One card in the nearby-devices list.
 *
 * The card itself is NOT a button. Each row carries its own real action —
 * "Pair" for an unknown device, "Send" for a paired one — and a button inside a
 * button is invalid HTML that keyboard and screen-reader users cannot
 * disentangle. Selection is therefore a consequence of pressing Send, not a
 * separate mode the user has to discover.
 */
export function DeviceCard({
  device,
  selected = false,
  peerState,
  peerMessage = '',
  disabled = false,
  busy = false,
  onPair,
  onSend,
  onForget,
}: DeviceCardProps) {
  const online = device.status === 'online';
  const meta = [device.address, PLATFORM_LABEL[device.platform]].filter(Boolean).join(' · ');
  const note = peerNote(peerState, peerMessage);
  const actionsDisabled = disabled || busy;

  return (
    <div className={'device' + (selected ? ' device--selected' : '')}>
      <div className="device__main">
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
      </div>

      {note ? <p className="device__note">{note}</p> : null}

      <div className="device__foot">
        <span className={'chip' + (device.paired ? ' chip--ok' : '')}>
          {device.paired ? 'Paired' : 'Not paired'}
        </span>

        <span className="device__actions">
          {device.paired ? (
            <>
              {onForget ? (
                <button
                  type="button"
                  className="linkbutton linkbutton--quiet"
                  disabled={actionsDisabled}
                  onClick={() => onForget(device)}
                >
                  Forget
                </button>
              ) : null}
              <button
                type="button"
                className="chipbutton chipbutton--accent"
                disabled={actionsDisabled || !onSend}
                aria-pressed={selected}
                onClick={() => onSend?.(device)}
              >
                {selected ? 'Connected' : busy ? 'Connecting…' : 'Send'}
              </button>
            </>
          ) : (
            <button
              type="button"
              className="chipbutton"
              disabled={actionsDisabled || !onPair}
              onClick={() => onPair?.(device)}
            >
              {busy ? 'Pairing…' : 'Pair'}
            </button>
          )}
        </span>
      </div>
    </div>
  );
}

export default DeviceCard;
