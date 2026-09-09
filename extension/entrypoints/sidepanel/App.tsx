import { useMemo } from 'react';
import { DeviceList } from '@/components/DeviceList';
import { FileDrop } from '@/components/FileDrop';
import { IncomingTransferDialog } from '@/components/IncomingTransferDialog';
import { PairingDialog } from '@/components/PairingDialog';
import { ReceivedList } from '@/components/ReceivedList';
import { TextComposer } from '@/components/TextComposer';
import { TransferHistory } from '@/components/TransferHistory';
import { TransferProgress } from '@/components/TransferProgress';
import { describePhase } from '@/lib/session';
import type { TransferRecord } from '@/lib/transfer';
import type { BackgroundState } from '@/types';
import { useSharing } from './useSharing';

/**
 * Four-way status, because "not connected" has four very different causes and
 * showing one word for all of them is how a user ends up restarting a daemon
 * that was never the problem.
 *
 * `off`     the user's own doing; never dressed up as a failure
 * `pending` first attempt, still in flight
 * `down`    we tried and nothing answered
 * `on`      usable
 */
type StatusTone = 'on' | 'pending' | 'down' | 'off';

interface Status {
  label: string;
  tone: StatusTone;
}

function describeStatus(snapshot: BackgroundState): Status {
  if (!snapshot.enabled) return { label: 'Off', tone: 'off' };
  if (snapshot.problem) return { label: 'Incompatible', tone: 'down' };

  switch (snapshot.state) {
    case 'connected':
      return { label: 'Connected', tone: 'on' };
    case 'connecting':
      return { label: 'Connecting…', tone: 'pending' };
    // `reconnecting` means at least one attempt has already failed, which is a
    // different thing to say than "connecting".
    case 'reconnecting':
      return { label: 'Retrying…', tone: 'down' };
    case 'error':
      return { label: 'Problem', tone: 'down' };
    default:
      return { label: 'Disconnected', tone: 'down' };
  }
}

/** A single, non-repeating sentence for the aria-live transfer region. */
function liveSummary(active: TransferRecord[], history: TransferRecord[]): string {
  const first = active[0];
  if (first) {
    return active.length === 1
      ? (first.direction === 'send' ? 'Sending ' : 'Receiving ') + first.name + '.'
      : String(active.length) + ' transfers in progress.';
  }
  const last = history[0];
  if (!last) return '';
  switch (last.status) {
    case 'complete':
      return last.direction === 'send'
        ? 'Sent ' + last.name + ' to ' + last.peerName + '.'
        : 'Received ' + last.name + ' from ' + last.peerName + '.';
    case 'cancelled':
      return last.name + ' was cancelled.';
    case 'failed':
      return last.name + ' failed. ' + (last.error ?? '');
    default:
      return '';
  }
}

export function App() {
  const share = useSharing();
  const {
    snapshot,
    devices,
    usingPlaceholders,
    peerStates,
    busyId,
    pairing,
    session,
    keepOpen,
    active,
    history,
    incoming,
    received,
    notice,
  } = share;

  const enabled = snapshot.enabled;
  const connected = snapshot.state === 'connected';
  const status = describeStatus(snapshot);
  const anyPaired = devices.some((device) => device.paired);
  const canSend = session !== null && session.phase === 'connected';

  const live = useMemo(() => liveSummary(active, history), [active, history]);

  return (
    <div className="app">
      <header className="topbar">
        <span className="topbar__brand">Sharing</span>
        <span className={'pill pill--' + status.tone} aria-live="polite">
          <span className="pill__dot" aria-hidden="true" />
          {status.label}
        </span>
      </header>

      <main className="scroll">
        <h1 className="display">Share across your devices</h1>

        <p className="lede">
          {!enabled ? (
            <>Sharing is off. Nothing is being discovered, and nothing can be sent.</>
          ) : snapshot.problem ? (
            <>
              {snapshot.problem.message}{' '}
              <button type="button" className="linkbutton" onClick={share.retryDaemon}>
                Try again
              </button>
            </>
          ) : connected ? (
            devices.length === 0 ? (
              <>No other devices have appeared on your network yet.</>
            ) : anyPaired ? (
              <>Pick a paired device, then send text or drop in a file.</>
            ) : (
              <>Pair a device once. After that, sending to it is one click.</>
            )
          ) : snapshot.state === 'connecting' ? (
            <>Looking for the Sharing daemon on this computer…</>
          ) : (
            <>
              The daemon is not answering on 127.0.0.1:8765. Start it, then{' '}
              <button type="button" className="linkbutton" onClick={share.retryDaemon}>
                Retry
              </button>
              .
            </>
          )}
        </p>

        {notice ? (
          <div
            className={'banner banner--' + notice.tone}
            role={notice.tone === 'error' ? 'alert' : 'status'}
          >
            <span className="banner__text">{notice.text}</span>
            <button
              type="button"
              className="linkbutton linkbutton--quiet"
              onClick={share.dismissNotice}
            >
              Dismiss
            </button>
          </div>
        ) : null}

        <section className="section">
          <div className="section__head">
            <h2 className="section__label">Nearby devices</h2>
            {connected ? (
              <button type="button" className="linkbutton" onClick={share.refreshDevices}>
                Refresh
              </button>
            ) : null}
          </div>

          <DeviceList
            devices={devices}
            selectedId={session?.peerId ?? null}
            peerStates={peerStates}
            busyId={busyId}
            disabled={!enabled || usingPlaceholders}
            onPair={share.startPair}
            onSend={share.connectTo}
            onForget={share.forgetPair}
            emptyTitle={connected ? 'Nothing found yet' : 'No devices'}
            emptyHint={
              connected
                ? 'Other devices need Sharing running on the same network.'
                : 'Start the daemon to discover devices.'
            }
          />

          {usingPlaceholders ? (
            <p className="note">
              {enabled
                ? 'Showing example devices until the daemon connects. Nothing can be sent to them.'
                : 'Showing example devices. Sharing is off.'}
            </p>
          ) : null}
        </section>

        {session ? (
          <section className="section">
            <div className="section__head">
              <h2 className="section__label">
                {session.phase === 'connected' ? 'Send to ' + session.peerName : 'Connection'}
              </h2>
              <button type="button" className="linkbutton" onClick={share.disconnect}>
                Disconnect
              </button>
            </div>

            <p
              className={
                'banner banner--' +
                (session.phase === 'failed' ? 'error' : session.phase === 'connected' ? 'ok' : 'info')
              }
              role="status"
            >
              <span className="banner__text">{describePhase(session)}</span>
            </p>

            {session.phase === 'failed' ? (
              <p className="note">
                Nothing was sent. Both machines must be on the same network, and that network must
                be set to Private rather than Public.
              </p>
            ) : null}

            <TextComposer
              onSend={share.sendText}
              disabled={!canSend}
              peerName={session.peerName}
            />

            <FileDrop
              onFiles={share.sendFiles}
              disabled={!canSend}
              hint={
                canSend
                  ? 'Drop a file for ' + session.peerName
                  : 'Waiting for the connection to ' + session.peerName
              }
            />

            {/*
              Progress bars carry aria-valuenow, which assistive tech reads on
              demand. This line is the part that gets announced, and it only
              changes when a transfer starts or ends — a per-chunk live region
              would be unusable.
            */}
            <p className="live" role="status" aria-live="polite">
              {live}
            </p>

            {active.map((transfer) => (
              <TransferProgress
                key={transfer.transferId}
                transfer={transfer}
                onCancel={share.cancelTransfer}
              />
            ))}

            {keepOpen ? (
              <p className="keepopen">
                <strong>Keep this panel open while transferring.</strong> The connection lives in
                this panel, so closing it cancels anything in flight.
              </p>
            ) : null}
          </section>
        ) : null}

        {received.length > 0 ? (
          <section className="section">
            <h2 className="section__label">Received</h2>
            <ReceivedList items={received} onDismiss={share.dismissReceived} />
            <p className="note">
              Saved files leave this list. Anything still here is held in the panel and is lost when
              the panel closes.
            </p>
          </section>
        ) : null}

        <section className="section">
          <h2 className="section__label">Recent</h2>
          <TransferHistory transfers={history} />
          {history.some((record) => record.status === 'failed' && record.error) ? (
            <p className="note note--bad">
              {history.find((record) => record.status === 'failed' && record.error)?.error}
            </p>
          ) : null}
        </section>
      </main>

      <footer className="footer">
        <button
          type="button"
          className="primary"
          // The toggle governs one thing — the connection — so its pressed state
          // tracks `enabled` while its label names what a click will do.
          aria-pressed={enabled}
          onClick={() => share.setEnabled(!enabled)}
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

      {pairing ? (
        <PairingDialog
          pairing={pairing}
          onAnswer={share.answerPairing}
          onClose={share.closePairing}
        />
      ) : null}

      {incoming ? (
        <IncomingTransferDialog offer={incoming} onAnswer={share.answerIncoming} />
      ) : null}
    </div>
  );
}

export default App;
