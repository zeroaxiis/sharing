/**
 * The side panel's wiring: background messaging, pairing, one peer connection,
 * and the transfers running over it.
 *
 * WHY THIS LIVES IN THE PANEL
 * ---------------------------
 * RTCPeerConnection does not exist in a Chrome MV3 service worker, so the
 * background cannot hold the peer connection. It keeps the daemon WebSocket and
 * relays signalling both ways over browser.runtime messaging; everything WebRTC
 * runs here, in a real document. The consequence is honest and load-bearing: a
 * transfer needs this panel to stay open, and the UI says so.
 *
 * TODO(M12): a Chrome offscreen document (reason WEBRTC) would let a transfer
 * outlive the panel. Firefox has no equivalent, so this path must keep working.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { newId } from '@/lib/daemon';
import { FAKE_DEVICES, filterDevices, isPlaceholderDevice, withDerivedStatus } from '@/lib/discovery';
import { phaseFromLink, sessionIsLive } from '@/lib/session';
import type { PairingView, ReceivedItem, SessionView } from '@/lib/session';
import { TransferEngine } from '@/lib/transfer';
import type { IncomingTransferOffer, TransferRecord } from '@/lib/transfer';
import { PeerLink } from '@/lib/webrtc';
import type {
  BackgroundState,
  ClientMessage,
  Device,
  PeerStatePush,
  RelayableServerMessage,
  RelayToDaemonRequest,
  RelayToDaemonResponse,
  SignalInboundMessage,
  SignalOutboundMessage,
} from '@/types';

/** `enabled` starts true here for the same reason it does in the background. */
const INITIAL_STATE: BackgroundState = {
  state: 'idle',
  info: null,
  devices: [],
  enabled: true,
  problem: null,
};

/**
 * How long a pairing step may sit with no word from the daemon before the
 * dialog says so. A prompt that spins forever is the failure mode this whole
 * panel is trying to avoid, so every wait has an end.
 */
const PAIR_TIMEOUT_MS = 30_000;

/** Terminal transfer records kept for the history list. */
const HISTORY_LIMIT = 20;

/** Received payloads held on screen at once; the oldest is revoked on overflow. */
const RECEIVED_LIMIT = 6;

/** How often the roster's online/offline is re-derived from `lastSeen`. */
const FRESHNESS_TICK_MS = 5_000;

interface Session {
  peerId: string;
  peerName: string;
  outgoing: boolean;
  link: PeerLink;
  engine: TransferEngine;
  offs: Array<() => void>;
}

export interface Notice {
  tone: 'error' | 'info';
  text: string;
}

export interface Sharing {
  snapshot: BackgroundState;
  /** Ready to render: self excluded, staleness re-derived, placeholders when down. */
  devices: Device[];
  /** True while `devices` is the inspectable mock roster, not real hardware. */
  usingPlaceholders: boolean;
  peerStates: ReadonlyMap<string, PeerStatePush>;
  /** The device id whose own action is in flight. */
  busyId: string | null;

  pairing: PairingView | null;
  startPair: (device: Device) => void;
  answerPairing: (accept: boolean) => void;
  closePairing: () => void;
  forgetPair: (device: Device) => void;

  session: SessionView | null;
  connectTo: (device: Device) => void;
  disconnect: () => void;
  /** True while closing the panel would destroy something the user wants. */
  keepOpen: boolean;

  active: TransferRecord[];
  history: TransferRecord[];
  incoming: IncomingTransferOffer | null;
  answerIncoming: (accept: boolean) => void;
  sendFiles: (files: File[]) => void;
  sendText: (text: string) => void;
  cancelTransfer: (transferId: string) => void;

  received: ReceivedItem[];
  dismissReceived: (id: string) => void;

  notice: Notice | null;
  dismissNotice: () => void;

  refreshDevices: () => void;
  retryDaemon: () => void;
  setEnabled: (enabled: boolean) => void;
}

// ---------------------------------------------------------------------------
// Runtime message plumbing
// ---------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

/**
 * Narrows a `stateChanged` push, or the reply to `getState`.
 *
 * `browser.runtime` hands back `any`, so every crossing into our types goes
 * through a check like this one.
 */
function asBackgroundState(value: unknown): BackgroundState | null {
  if (!isRecord(value)) return null;
  if (typeof value['state'] !== 'string') return null;
  if (typeof value['enabled'] !== 'boolean') return null;
  if (!Array.isArray(value['devices'])) return null;
  const candidate = value as unknown as BackgroundState;
  return {
    state: candidate.state,
    info: candidate.info ?? null,
    devices: candidate.devices,
    enabled: candidate.enabled,
    problem: candidate.problem ?? null,
  };
}

function asDaemonPush(value: unknown): RelayableServerMessage | null {
  if (!isRecord(value)) return null;
  if (value['kind'] !== 'daemonPush') return null;
  const message = value['message'];
  if (!isRecord(message) || typeof message['type'] !== 'string') return null;
  return message as unknown as RelayableServerMessage;
}

function asRelayResponse(value: unknown): RelayToDaemonResponse | null {
  if (!isRecord(value) || typeof value['ok'] !== 'boolean') return null;
  return value as unknown as RelayToDaemonResponse;
}

/** Sends one message to the daemon through the background relay. */
async function relay(message: ClientMessage): Promise<RelayToDaemonResponse> {
  const request: RelayToDaemonRequest = { kind: 'relayToDaemon', message };
  try {
    const reply: unknown = await browser.runtime.sendMessage(request);
    return asRelayResponse(reply) ?? { ok: false, reason: 'send_failed' };
  } catch {
    // The service worker is asleep or mid-restart; sending woke it, but this
    // particular message did not land.
    return { ok: false, reason: 'send_failed' };
  }
}

function relayFailure(response: RelayToDaemonResponse): string {
  return response.reason === 'not_connected'
    ? 'The Sharing daemon is not connected, so that could not be sent.'
    : 'Could not reach the Sharing background service.';
}

// ---------------------------------------------------------------------------
// The hook
// ---------------------------------------------------------------------------

export function useSharing(): Sharing {
  const [snapshot, setSnapshotState] = useState<BackgroundState>(INITIAL_STATE);
  const [peerStates, setPeerStates] = useState<ReadonlyMap<string, PeerStatePush>>(new Map());
  const [pairing, setPairing] = useState<PairingView | null>(null);
  const [session, setSession] = useState<SessionView | null>(null);
  const [records, setRecords] = useState<TransferRecord[]>([]);
  const [received, setReceived] = useState<ReceivedItem[]>([]);
  const [incoming, setIncoming] = useState<IncomingTransferOffer | null>(null);
  const [notice, setNotice] = useState<Notice | null>(null);
  const [, setTick] = useState(0);

  // Refs mirror anything an event handler needs to read, so the listeners below
  // can be registered exactly once instead of being torn down on every render.
  const snapshotRef = useRef<BackgroundState>(INITIAL_STATE);
  const sessionRef = useRef<Session | null>(null);
  const consentRef = useRef<{ offer: IncomingTransferOffer; resolve: (ok: boolean) => void } | null>(
    null,
  );
  const pairTimerRef = useRef<number | null>(null);
  const pairingRef = useRef<PairingView | null>(null);
  const receivedRef = useRef<ReceivedItem[]>([]);
  const sendQueueRef = useRef<Promise<void>>(Promise.resolve());

  const applySnapshot = useCallback((next: BackgroundState) => {
    snapshotRef.current = next;
    setSnapshotState(next);
  }, []);

  const updatePairing = useCallback((next: PairingView | null) => {
    pairingRef.current = next;
    setPairing(next);
  }, []);

  const updateReceived = useCallback((next: ReceivedItem[]) => {
    receivedRef.current = next;
    setReceived(next);
  }, []);

  // -- pairing timers -------------------------------------------------------

  const clearPairTimer = useCallback(() => {
    if (pairTimerRef.current !== null) {
      window.clearTimeout(pairTimerRef.current);
      pairTimerRef.current = null;
    }
  }, []);

  const armPairTimer = useCallback(
    (deviceId: string, message: string) => {
      clearPairTimer();
      pairTimerRef.current = window.setTimeout(() => {
        pairTimerRef.current = null;
        const current = pairingRef.current;
        if (!current || current.deviceId !== deviceId) return;
        if (current.phase === 'done' || current.phase === 'failed') return;
        updatePairing({ ...current, phase: 'failed', message });
      }, PAIR_TIMEOUT_MS);
    },
    [clearPairTimer, updatePairing],
  );

  // -- transfers ------------------------------------------------------------

  const upsertRecord = useCallback((record: TransferRecord) => {
    setRecords((prev) => {
      const index = prev.findIndex((entry) => entry.transferId === record.transferId);
      if (index === -1) return [record, ...prev].slice(0, HISTORY_LIMIT + 8);
      const next = prev.slice();
      next[index] = record;
      return next;
    });
  }, []);

  const pushReceived = useCallback(
    (item: ReceivedItem) => {
      const next = [item, ...receivedRef.current];
      // Anything past the cap is dropped here, which is also the only place its
      // object URL can still be revoked.
      for (const evicted of next.slice(RECEIVED_LIMIT)) {
        if (evicted.kind === 'file') evicted.revoke();
      }
      updateReceived(next.slice(0, RECEIVED_LIMIT));
    },
    [updateReceived],
  );

  const dismissReceived = useCallback(
    (id: string) => {
      const item = receivedRef.current.find((entry) => entry.id === id);
      if (item?.kind === 'file') item.revoke();
      updateReceived(receivedRef.current.filter((entry) => entry.id !== id));
    },
    [updateReceived],
  );

  /**
   * The consent gate for incoming files.
   *
   * The engine parks the sender on `transfer:accept` until this resolves, and
   * refuses everything if no handler is installed. Nothing is ever accepted
   * without a human answering the dialog this opens.
   */
  const askConsent = useCallback((offer: IncomingTransferOffer): Promise<boolean> => {
    // One prompt at a time. A second offer arriving while the first is open is
    // declined rather than silently replacing a question already on screen.
    if (consentRef.current) return Promise.resolve(false);
    return new Promise<boolean>((resolve) => {
      consentRef.current = { offer, resolve };
      setIncoming(offer);
    });
  }, []);

  const answerIncoming = useCallback((accept: boolean) => {
    const pending = consentRef.current;
    consentRef.current = null;
    setIncoming(null);
    pending?.resolve(accept);
  }, []);

  // -- peer session ---------------------------------------------------------

  const closeSession = useCallback((reason: string | null) => {
    const current = sessionRef.current;
    sessionRef.current = null;

    // A prompt tied to a connection that is going away has no answer left to
    // give; resolving false declines it cleanly instead of leaking the promise.
    const pending = consentRef.current;
    if (pending) {
      consentRef.current = null;
      setIncoming(null);
      pending.resolve(false);
    }

    if (!current) return;
    // Dispose BEFORE unsubscribing: dispose settles everything in flight, and
    // those final records are exactly what the history list should show.
    current.engine.dispose();
    current.link.close();
    for (const off of current.offs) off();

    setSession((prev) =>
      prev && prev.peerId === current.peerId
        ? { ...prev, phase: 'closed', error: reason ?? prev.error }
        : prev,
    );
  }, []);

  const sendSignal = useCallback(async (message: SignalOutboundMessage): Promise<void> => {
    const result = await relay(message);
    // Throwing is the contract: PeerLink turns a rejected sendSignal into a
    // `signal_failed` error the UI shows, rather than a connection that quietly
    // never negotiates.
    if (!result.ok) throw new Error(relayFailure(result));
  }, []);

  const createSession = useCallback(
    (peerId: string, peerName: string, outgoing: boolean): Session | null => {
      const localId = snapshotRef.current.info?.id;
      if (!localId) {
        setNotice({
          tone: 'error',
          text: 'Waiting for the daemon to say who this device is. Try again in a moment.',
        });
        return null;
      }

      closeSession(null);

      const link = new PeerLink({
        localId,
        remoteId: peerId,
        remoteName: peerName,
        sendSignal,
      });

      // Built immediately rather than on open: the responder's first frame can
      // arrive in the same task as the channel opening, and an engine wired up
      // one tick later would miss it.
      const engine = new TransferEngine({
        channel: link,
        peerId,
        peerName,
        consent: (offer) => askConsent(offer),
      });

      const patch = (update: Partial<SessionView>) => {
        setSession((prev) => (prev && prev.peerId === peerId ? { ...prev, ...update } : prev));
      };

      const offs: Array<() => void> = [
        link.onState((state) => {
          patch({ phase: phaseFromLink(state) });
        }),
        link.onError((error) => {
          patch({ error: error.message, ...(error.fatal ? { phase: 'failed' as const } : {}) });
          setNotice({ tone: 'error', text: error.message });
        }),
        engine.onRecord(upsertRecord),
        engine.onText((text) => {
          pushReceived({
            kind: 'text',
            id: 'text-' + newId(),
            text,
            from: peerName,
            at: Date.now(),
          });
        }),
        engine.onFile((file) => {
          pushReceived({
            kind: 'file',
            id: file.transferId,
            name: file.name,
            mime: file.mime,
            size: file.size,
            url: file.url,
            revoke: file.revoke,
            from: peerName,
            at: Date.now(),
          });
        }),
        engine.onError((error) => {
          setNotice({ tone: 'error', text: error.message });
        }),
      ];

      const created: Session = { peerId, peerName, outgoing, link, engine, offs };
      sessionRef.current = created;
      setSession({ peerId, peerName, phase: 'connecting', error: null, outgoing });
      return created;
    },
    [askConsent, closeSession, pushReceived, sendSignal, upsertRecord],
  );

  const connectTo = useCallback(
    (device: Device) => {
      if (isPlaceholderDevice(device)) {
        setNotice({ tone: 'info', text: 'That is an example device. Start the daemon to see real ones.' });
        return;
      }
      if (!device.paired) {
        setNotice({ tone: 'info', text: 'Pair with ' + device.name + ' before sending to it.' });
        return;
      }

      const current = sessionRef.current;
      if (current?.peerId === device.id) return;
      if (current) {
        const busy = current.engine
          .getRecords()
          .some((record) => record.status === 'pending' || record.status === 'active');
        if (busy) {
          setNotice({
            tone: 'error',
            text: 'Finish or cancel the transfer with ' + current.peerName + ' first.',
          });
          return;
        }
      }

      setNotice(null);
      const created = createSession(device.id, device.name, true);
      // connect() creates the DataChannel and then offers, in that order — an
      // offer built before the channel exists carries no m=application section
      // and the other side never sees `ondatachannel`.
      if (created) void created.link.connect();
    },
    [createSession],
  );

  const disconnect = useCallback(() => {
    closeSession(null);
    setSession(null);
  }, [closeSession]);

  // -- inbound signalling ---------------------------------------------------

  const routeSignal = useCallback(
    (message: SignalInboundMessage) => {
      const current = sessionRef.current;
      if (current && current.peerId === message.from) {
        void current.link.handleSignal(message);
        return;
      }

      // Anything that is not an offer belongs to a session we no longer have.
      // Adopting a stray candidate would build a link with no negotiation.
      if (message.type !== 'signal:offer') return;

      if (current) {
        const busy = current.engine
          .getRecords()
          .some((record) => record.status === 'pending' || record.status === 'active');
        if (busy) {
          setNotice({
            tone: 'info',
            text: 'Another device tried to connect while the transfer with ' + current.peerName + ' was running.',
          });
          return;
        }
      }

      // The daemon's peer listener refuses signalling from anything unpaired
      // (spec v2 §4), so an offer that reached us has already been vouched for.
      // The roster is only used to put a name on it.
      const known = snapshotRef.current.devices.find((device) => device.id === message.from);
      const created = createSession(message.from, known?.name ?? message.from, false);
      // Deliberately NOT connect(): the responder adopts the channel from
      // `ondatachannel`, and offering back here would be glare we caused.
      if (created) void created.link.handleSignal(message);
    },
    [createSession],
  );

  // -- daemon pushes --------------------------------------------------------

  const handleDaemonPush = useCallback(
    (message: RelayableServerMessage) => {
      switch (message.type) {
        case 'devices': {
          // The background sends the roster in the state snapshot, not as a
          // push; handled anyway so a future change cannot silently drop it.
          const next: BackgroundState = { ...snapshotRef.current, devices: message.devices };
          applySnapshot(next);
          return;
        }
        case 'peer:state': {
          setPeerStates((prev) => {
            const next = new Map(prev);
            next.set(message.deviceId, message);
            return next;
          });
          return;
        }
        case 'pair:code': {
          clearPairTimer();
          updatePairing({
            deviceId: message.deviceId,
            name: message.name || message.deviceId,
            direction: message.direction,
            phase: 'code',
            code: message.code,
            message: null,
          });
          return;
        }
        case 'pair:result': {
          clearPairTimer();
          const current = pairingRef.current;
          const name = current?.deviceId === message.deviceId ? current.name : message.deviceId;
          updatePairing({
            deviceId: message.deviceId,
            name,
            direction: current?.direction ?? 'incoming',
            phase: message.paired ? 'done' : 'failed',
            code: current?.code ?? null,
            message: message.paired
              ? null
              : message.reason || 'The other device did not confirm the code.',
          });
          // The `paired` flag on the roster is what turns "Pair" into "Send",
          // so ask for a fresh one rather than waiting for the next browse.
          void relay({ type: 'devices:refresh', id: newId() });
          return;
        }
        case 'signal:offer':
        case 'signal:answer':
        case 'signal:ice':
          routeSignal(message);
          return;
      }
    },
    [applySnapshot, clearPairTimer, routeSignal, updatePairing],
  );

  // -- background wiring ----------------------------------------------------

  useEffect(() => {
    let cancelled = false;

    void browser.runtime
      .sendMessage({ kind: 'getState' })
      .then((reply: unknown) => {
        const state = asBackgroundState(reply);
        if (!cancelled && state) applySnapshot(state);
      })
      .catch(() => {
        // Service worker still waking up; the push below catches us up.
      });

    const onMessage = (raw: unknown) => {
      const state = asBackgroundState(raw);
      if (state && isRecord(raw) && raw['kind'] === 'stateChanged') {
        applySnapshot(state);
        return;
      }
      const push = asDaemonPush(raw);
      if (push) handleDaemonPush(push);
    };

    browser.runtime.onMessage.addListener(onMessage);
    return () => {
      cancelled = true;
      browser.runtime.onMessage.removeListener(onMessage);
    };
  }, [applySnapshot, handleDaemonPush]);

  // Tear the peer connection down when the panel goes away. This is the
  // limitation the UI warns about, made explicit rather than left to chance.
  useEffect(() => {
    return () => {
      closeSession(null);
      for (const item of receivedRef.current) {
        if (item.kind === 'file') item.revoke();
      }
      receivedRef.current = [];
      if (pairTimerRef.current !== null) window.clearTimeout(pairTimerRef.current);
    };
  }, [closeSession]);

  // A roster that stopped updating has to decay to offline instead of showing
  // green forever, so re-derive freshness on a slow tick.
  useEffect(() => {
    if (snapshot.state !== 'connected' || snapshot.devices.length === 0) return;
    const timer = window.setInterval(() => setTick((value) => value + 1), FRESHNESS_TICK_MS);
    return () => window.clearInterval(timer);
  }, [snapshot.state, snapshot.devices.length]);

  // The daemon dropping invalidates every peer connection it was relaying for.
  useEffect(() => {
    if (snapshot.state === 'connected') return;
    if (!sessionRef.current) return;
    closeSession('The daemon connection dropped, so the direct connection ended too.');
    setSession(null);
    setPeerStates(new Map());
  }, [snapshot.state, closeSession]);

  // -- actions --------------------------------------------------------------

  const startPair = useCallback(
    (device: Device) => {
      if (isPlaceholderDevice(device)) {
        setNotice({ tone: 'info', text: 'That is an example device. Start the daemon to see real ones.' });
        return;
      }
      setNotice(null);
      updatePairing({
        deviceId: device.id,
        name: device.name,
        direction: 'outgoing',
        phase: 'starting',
        code: null,
        message: null,
      });
      armPairTimer(
        device.id,
        device.name + ' did not answer. Check that it is awake and running Sharing.',
      );

      void relay({ type: 'pair:start', id: newId(), deviceId: device.id }).then((result) => {
        if (result.ok) return;
        clearPairTimer();
        const current = pairingRef.current;
        if (current?.deviceId !== device.id) return;
        updatePairing({ ...current, phase: 'failed', message: relayFailure(result) });
      });
    },
    [armPairTimer, clearPairTimer, updatePairing],
  );

  const answerPairing = useCallback(
    (accept: boolean) => {
      const current = pairingRef.current;
      if (!current) return;

      void relay({
        type: 'pair:confirm',
        id: newId(),
        deviceId: current.deviceId,
        accept,
      });

      if (!accept) {
        // Rejecting is final on this side, so the dialog closes rather than
        // waiting on a pair:result that only confirms what the user chose.
        clearPairTimer();
        updatePairing(null);
        return;
      }

      updatePairing({ ...current, phase: 'submitting' });
      armPairTimer(
        current.deviceId,
        'No confirmation came back from ' + current.name + '. Nothing was paired.',
      );
    },
    [armPairTimer, clearPairTimer, updatePairing],
  );

  const closePairing = useCallback(() => {
    clearPairTimer();
    updatePairing(null);
  }, [clearPairTimer, updatePairing]);

  const forgetPair = useCallback(
    (device: Device) => {
      if (sessionRef.current?.peerId === device.id) {
        closeSession('Pairing removed.');
        setSession(null);
      }
      void relay({ type: 'pair:forget', id: newId(), deviceId: device.id }).then((result) => {
        if (!result.ok) {
          setNotice({ tone: 'error', text: relayFailure(result) });
          return;
        }
        setNotice({ tone: 'info', text: device.name + ' is no longer paired.' });
        void relay({ type: 'devices:refresh', id: newId() });
      });
    },
    [closeSession],
  );

  /** Serialises sends: two files interleaving on one channel share bandwidth and RAM on the receiver for no gain. */
  const enqueueSend = useCallback((run: () => Promise<unknown>) => {
    sendQueueRef.current = sendQueueRef.current.then(run).then(
      () => undefined,
      () => undefined,
    );
  }, []);

  const sendFiles = useCallback(
    (files: File[]) => {
      const current = sessionRef.current;
      if (!current || !current.link.isOpen) {
        setNotice({ tone: 'error', text: 'Not connected yet, so nothing was sent.' });
        return;
      }
      setNotice(null);
      for (const file of files) {
        enqueueSend(() => current.engine.sendFile(file));
      }
    },
    [enqueueSend],
  );

  const sendText = useCallback(
    (text: string) => {
      const current = sessionRef.current;
      if (!current || !current.link.isOpen) {
        setNotice({ tone: 'error', text: 'Not connected yet, so nothing was sent.' });
        return;
      }
      setNotice(null);
      enqueueSend(() => current.engine.sendText(text));
    },
    [enqueueSend],
  );

  const cancelTransfer = useCallback((transferId: string) => {
    sessionRef.current?.engine.cancel(transferId);
  }, []);

  const refreshDevices = useCallback(() => {
    void relay({ type: 'devices:refresh', id: newId() }).then((result) => {
      if (!result.ok) setNotice({ tone: 'error', text: relayFailure(result) });
    });
  }, []);

  const retryDaemon = useCallback(() => {
    void browser.runtime.sendMessage({ kind: 'reconnect' }).catch(() => undefined);
  }, []);

  const setEnabled = useCallback(
    (next: boolean) => {
      if (!next) {
        closeSession(null);
        setSession(null);
        setPeerStates(new Map());
        updatePairing(null);
      }
      // Optimistic, so the button feels immediate. The stateChanged push that
      // follows carries the truth.
      const previous = snapshotRef.current;
      applySnapshot(
        next
          ? {
              ...previous,
              enabled: true,
              state: previous.state === 'connected' ? 'connected' : 'connecting',
            }
          : { state: 'idle', info: null, devices: [], enabled: false, problem: null },
      );

      void browser.runtime
        .sendMessage({ kind: 'setEnabled', enabled: next })
        .then((reply: unknown) => {
          const state = asBackgroundState(reply);
          if (!state) return;
          // Only `enabled` is taken from the reply: the background answers
          // before it has finished tearing the socket down, so the rest of that
          // snapshot can still describe the old connection.
          applySnapshot({ ...snapshotRef.current, enabled: state.enabled });
        })
        .catch(() => undefined);
    },
    [applySnapshot, closeSession, updatePairing],
  );

  // -- derived --------------------------------------------------------------

  const connected = snapshot.state === 'connected';

  const devices = useMemo(() => {
    if (!connected) return FAKE_DEVICES;
    // The daemon self-filters by TXT id, but a roster is cheap to double-check
    // and listing yourself as a send target is a confusing bug to chase.
    return filterDevices(withDerivedStatus(snapshot.devices), {
      excludeId: snapshot.info?.id,
    });
    // `records`/tick intentionally excluded; the freshness tick re-renders and
    // that is enough to recompute this.
  }, [connected, snapshot.devices, snapshot.info?.id]);

  const active = useMemo(
    () => records.filter((record) => record.status === 'pending' || record.status === 'active'),
    [records],
  );

  const history = useMemo(
    () =>
      records
        .filter((record) => record.status !== 'pending' && record.status !== 'active')
        .slice(0, HISTORY_LIMIT),
    [records],
  );

  const dismissNotice = useCallback(() => setNotice(null), []);

  const busyId = useMemo(() => {
    if (pairing && (pairing.phase === 'starting' || pairing.phase === 'submitting')) {
      return pairing.deviceId;
    }
    if (session && session.phase === 'connecting') return session.peerId;
    return null;
  }, [pairing, session]);

  return {
    snapshot,
    devices,
    usingPlaceholders: !connected,
    peerStates,
    busyId,

    pairing,
    startPair,
    answerPairing,
    closePairing,
    forgetPair,

    session,
    connectTo,
    disconnect,
    keepOpen: active.length > 0 || sessionIsLive(session),

    active,
    history,
    incoming,
    answerIncoming,
    sendFiles,
    sendText,
    cancelTransfer,

    received,
    dismissReceived,

    notice,
    dismissNotice,

    refreshDevices,
    retryDaemon,
    setEnabled,
  };
}
