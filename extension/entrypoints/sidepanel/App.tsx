import { useCallback, useEffect, useState } from 'react';
import { DeviceList } from '@/components/DeviceList';
import { FileDrop } from '@/components/FileDrop';
import { TransferHistory } from '@/components/TransferHistory';
import { TransferProgress } from '@/components/TransferProgress';
import { FAKE_DEVICES } from '@/lib/discovery';
import type { TransferRecord } from '@/lib/transfer';
import type { BackgroundState, Device, ReconnectRequest, SetEnabledRequest } from '@/types';

/** `enabled` starts true here for the same reason it does in the background. */
const INITIAL_STATE: BackgroundState = {
  state: 'idle',
  info: null,
  devices: [],
  enabled: true,
};

/**
 * Three-way status, because "not connected" has two very different causes.
 *
 * `off` is the user's own doing and must never be dressed up as a failure;
 * `down` means we are trying and the daemon is not answering.
 */
type StatusTone = 'on' | 'pending' | 'down' | 'off';

interface Status {
  label: string;
  tone: StatusTone;
}

function describeStatus(snapshot: BackgroundState): Status {
  if (!snapshot.enabled) return { label: 'Off', tone: 'off' };

  switch (snapshot.state) {
    case 'connected':
      return { label: 'Connected', tone: 'on' };
    case 'connecting':
    // `reconnecting` is the backoff loop between attempts — still trying, so it
    // reads the same way to the user.
    case 'reconnecting':
      return { label: 'Connecting…', tone: 'pending' };
    default:
      return { label: 'Disconnected', tone: 'down' };
  }
}

/**
 * Narrows an untyped runtime message. `browser.runtime` hands back `any`, so
 * every crossing into our types goes through here. `enabled` is checked too: a
 * reply without it cannot be trusted to describe the connect/disconnect toggle.
 */
function isBackgroundState(value: unknown): value is BackgroundState {
  if (typeof value !== 'object' || value === null) return false;
  const candidate = value as Partial<BackgroundState>;
  return (
    typeof candidate.state === 'string' &&
    typeof candidate.enabled === 'boolean' &&
    Array.isArray(candidate.devices)
  );
}

export function App() {
  const [snapshot, setSnapshot] = useState<BackgroundState>(INITIAL_STATE);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  // M9 fills these; the components below are already wired to their shapes.
  const [active] = useState<TransferRecord[]>([]);
  const [history] = useState<TransferRecord[]>([]);

  // The background owns the socket, so the panel pulls a snapshot on mount and
  // then rides the stateChanged pushes for as long as it stays open.
  useEffect(() => {
    let cancelled = false;

    void browser.runtime
      .sendMessage({ kind: 'getState' })
      .then((reply: unknown) => {
        if (!cancelled && isBackgroundState(reply)) setSnapshot(reply);
      })
      .catch(() => {
        // Service worker still waking up; the push below will catch us up.
      });

    const onPush = (message: unknown) => {
      if (isBackgroundState(message)) setSnapshot(message);
    };

    browser.runtime.onMessage.addListener(onPush);
    return () => {
      cancelled = true;
      browser.runtime.onMessage.removeListener(onPush);
    };
  }, []);

  const retry = useCallback(() => {
    const request: ReconnectRequest = { kind: 'reconnect' };
    void browser.runtime.sendMessage(request).catch(() => undefined);
  }, []);

  const setEnabled = useCallback((next: boolean) => {
    // Optimistic, so the button feels immediate. Turning off drops everything
    // the socket taught us, which is exactly what the background is about to
    // do; turning on shows "Connecting…" until the socket says otherwise.
    setSnapshot((prev) =>
      next
        ? { ...prev, enabled: true, state: prev.state === 'connected' ? 'connected' : 'connecting' }
        : { state: 'idle', info: null, devices: [], enabled: false },
    );

    const request: SetEnabledRequest = { kind: 'setEnabled', enabled: next };
    void browser.runtime
      .sendMessage(request)
      .then((reply: unknown) => {
        if (!isBackgroundState(reply)) return;
        // Only `enabled` is taken from the reply. The background answers before
        // it has finished tearing the socket down, so the rest of that snapshot
        // can still describe the old connection; the stateChanged push that
        // follows carries the truth.
        const confirmed = reply.enabled;
        setSnapshot((prev) => ({ ...prev, enabled: confirmed }));
      })
      .catch(() => undefined);
  }, []);

  const enabled = snapshot.enabled;
  const connected = snapshot.state === 'connected';
  const status = describeStatus(snapshot);

  // Milestone 1: with no daemon running we deliberately show the mock roster so
  // the UI is inspectable. Real devices only ever come from the daemon.
  const devices: Device[] = connected ? snapshot.devices : FAKE_DEVICES;
  const usingFakes = !connected;

  const selected = devices.find((device) => device.id === selectedId) ?? null;

  const onFiles = useCallback((files: File[]) => {
    // TODO(M9): hand these to lib/transfer.ts startTransfer().
    console.info('[nearby-share] files selected (not yet sent):', files.map((f) => f.name));
  }, []);

  return (
    <div className="app">
      <header className="topbar">
        <span className="topbar__brand">Nearby Share</span>
        <span className={'pill pill--' + status.tone} aria-live="polite">
          <span className="pill__dot" aria-hidden="true" />
          {status.label}
        </span>
      </header>

      <main className="scroll">
        <h1 className="display">Share across your devices</h1>

        <p className="lede">
          {!enabled ? (
            <>Nearby Share is off. Nothing is being discovered, and nothing can be sent.</>
          ) : connected ? (
            <>Pick a device on your network, then drop in whatever you want to send.</>
          ) : status.tone === 'pending' ? (
            <>Looking for the Nearby Share daemon on this computer…</>
          ) : (
            <>
              The daemon is not answering on 127.0.0.1:8765. Start it, then{' '}
              <button type="button" className="linkbutton" onClick={retry}>
                Retry
              </button>
              .
            </>
          )}
        </p>

        <section className="section">
          <h2 className="section__label">Nearby devices</h2>
          <DeviceList
            devices={devices}
            selectedId={selectedId}
            onSelect={(device) => setSelectedId(device.id === selectedId ? null : device.id)}
          />
          {usingFakes ? (
            <p className="note">
              {enabled
                ? 'Showing example devices until the daemon connects.'
                : 'Showing example devices. Nearby Share is off.'}
            </p>
          ) : null}
        </section>

        <section className="section">
          <h2 className="section__label">Send</h2>
          <FileDrop
            onFiles={onFiles}
            disabled={!enabled || !selected}
            hint={
              !enabled
                ? 'Turn Nearby Share on to send'
                : selected
                  ? 'Drop a file for ' + selected.name
                  : 'Pick a device first'
            }
          />
          {active.map((transfer) => (
            <TransferProgress key={transfer.transferId} transfer={transfer} />
          ))}
        </section>

        <section className="section">
          <h2 className="section__label">Recent</h2>
          <TransferHistory transfers={history} />
        </section>
      </main>

      <footer className="footer">
        <button
          type="button"
          className="primary"
          // The toggle governs one thing — the connection — so its pressed state
          // tracks `enabled` while its label names what a click will do.
          aria-pressed={enabled}
          onClick={() => setEnabled(!enabled)}
        >
          {enabled ? 'Disconnect' : 'Connect'}
        </button>

        <p className="footer__meta">
          {snapshot.info
            ? 'This device: ' + snapshot.info.name + ' · v' + snapshot.info.version
            : enabled
              ? 'No daemon reachable on 127.0.0.1:8765 yet.'
              : 'Reconnects only when you ask it to.'}
        </p>
      </footer>
    </div>
  );
}

export default App;
