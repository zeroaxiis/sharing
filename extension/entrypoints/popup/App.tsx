import { useCallback, useEffect, useState } from 'react';
import { DeviceList } from '@/components/DeviceList';
import { FileDrop } from '@/components/FileDrop';
import { TransferHistory } from '@/components/TransferHistory';
import { TransferProgress } from '@/components/TransferProgress';
import { FAKE_DEVICES } from '@/lib/discovery';
import type { TransferRecord } from '@/lib/transfer';
import type { BackgroundState, ConnectionState, Device } from '@/types';

const INITIAL_STATE: BackgroundState = { state: 'idle', info: null, devices: [] };

/** The status pill copy. Anything that is not a live socket reads as offline. */
function statusLabel(state: ConnectionState): string {
  if (state === 'connected') return 'Connected';
  if (state === 'connecting') return 'Connecting…';
  return 'Disconnected';
}

/**
 * Narrows an untyped runtime message. `browser.runtime` hands back `any`, so
 * every crossing into our types goes through here.
 */
function isBackgroundState(value: unknown): value is BackgroundState {
  if (typeof value !== 'object' || value === null) return false;
  const candidate = value as Partial<BackgroundState>;
  return typeof candidate.state === 'string' && Array.isArray(candidate.devices);
}

export function App() {
  const [snapshot, setSnapshot] = useState<BackgroundState>(INITIAL_STATE);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  // M9 fills these; the components below are already wired to their shapes.
  const [active] = useState<TransferRecord[]>([]);
  const [history] = useState<TransferRecord[]>([]);

  // The background owns the socket, so the popup pulls a snapshot on mount and
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
    void browser.runtime.sendMessage({ kind: 'reconnect' }).catch(() => undefined);
  }, []);

  const connected = snapshot.state === 'connected';

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
      <header className="header">
        <h1 className="header__title">Nearby Share</h1>
        <span className={'pill' + (connected ? ' pill--on' : '')}>
          <span className="pill__dot" aria-hidden="true" />
          {statusLabel(snapshot.state)}
        </span>
      </header>

      {snapshot.info ? (
        <p className="subtle">
          This device: {snapshot.info.name} · v{snapshot.info.version}
        </p>
      ) : (
        <p className="subtle">
          Daemon not running.{' '}
          <button type="button" className="linkbutton" onClick={retry}>
            Retry
          </button>
        </p>
      )}

      <section className="section">
        <h2 className="section__title">Nearby devices</h2>
        <DeviceList
          devices={devices}
          selectedId={selectedId}
          onSelect={(device) => setSelectedId(device.id === selectedId ? null : device.id)}
        />
        {usingFakes ? <p className="note">Showing example devices until the daemon connects.</p> : null}
      </section>

      <section className="section">
        <h2 className="section__title">Send</h2>
        <FileDrop
          onFiles={onFiles}
          disabled={!selected}
          hint={selected ? 'Drop a file for ' + selected.name : 'Pick a device first'}
        />
        {active.map((transfer) => (
          <TransferProgress key={transfer.transferId} transfer={transfer} />
        ))}
      </section>

      <section className="section">
        <h2 className="section__title">Recent</h2>
        <TransferHistory transfers={history} />
      </section>
    </div>
  );
}

export default App;
