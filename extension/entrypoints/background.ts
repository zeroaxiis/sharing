import { DaemonClient } from '@/lib/daemon';
import type {
  BackgroundState,
  ConnectionState,
  DaemonInfo,
  DaemonProblem,
  DaemonPush,
  RelayableServerMessage,
  RelayToDaemonResponse,
  RuntimeRequest,
  StateChangedPush,
} from '@/types';

/** Persisted across service-worker restarts so a manual Disconnect sticks. */
const ENABLED_KEY = 'sharing.enabled';

/**
 * Signalling held for a side panel that is not open.
 *
 * A dropped offer is indistinguishable from a hung connection, so inbound
 * signalling is queued rather than discarded — but only briefly and only a
 * little of it. Both bounds matter: an unbounded queue is a memory leak a
 * remote peer can drive, and a five-minute-old ICE candidate is worse than
 * useless because it makes the panel look like it is doing something.
 */
const PANEL_QUEUE_LIMIT = 64;
const PANEL_QUEUE_TTL_MS = 60_000;

interface QueuedPush {
  message: RelayableServerMessage;
  at: number;
}

/**
 * The background script OWNS the daemon connection.
 *
 * The side panel is destroyed the moment it closes, so a panel-owned socket
 * would tear down and reconnect on every open. Instead the panel asks us for
 * the current snapshot on mount ({kind:'getState'}) and we push
 * {kind:'stateChanged'} whenever anything moves, so an open panel stays live.
 *
 * Chrome MV3 terminates an idle service worker after ~30s. We do not fight
 * that: this module reconnects on every wake (the top-level start() below
 * runs again when the worker restarts) and DaemonClient retries with
 * exponential backoff — 500ms, 1s, 2s, 4s, ... capped at 15s, with jitter.
 * An open WebSocket also counts as activity, so a healthy connection keeps the
 * worker alive on its own.
 *
 * `enabled` is the user's explicit choice and is deliberately separate from
 * `state`. Without persisting it, a Disconnect would be undone by the next
 * service-worker wake, which the user would read as the button not working.
 *
 * WEBRTC IS NOT HERE, AND MUST NOT BE. RTCPeerConnection does not exist in a
 * Chrome MV3 service worker; constructing one here fails at runtime with no
 * useful error. This script is a SIGNALLING RELAY only: daemon -> panel for
 * signal/pair/peer pushes, panel -> daemon for everything the panel wants to
 * say. The peer connection itself lives in the side panel document, which is a
 * real document in both browsers.
 *
 * TODO(M12): a Chrome offscreen document (reason WEBRTC) would let a transfer
 * survive the panel closing. Firefox has no equivalent, so the panel-open path
 * has to keep working either way.
 */
export default defineBackground(() => {
  const snapshot: BackgroundState = {
    state: 'idle',
    info: null,
    devices: [],
    enabled: true,
    problem: null,
  };

  const client = new DaemonClient({
    onStateChange: (state: ConnectionState) => {
      snapshot.state = state;
      if (state !== 'connected') {
        // Anything we learned over the socket is stale once it drops, and so is
        // any signalling still waiting for a panel: it belongs to a session the
        // other end has already given up on.
        snapshot.devices = [];
        clearQueue();
      }
      broadcast();
    },
    onInfo: (info: DaemonInfo | null) => {
      snapshot.info = info;
      broadcast();
    },
    onProblem: (problem: DaemonProblem | null) => {
      snapshot.problem = problem;
      broadcast();
    },
  });

  // -- daemon -> panel ------------------------------------------------------

  const queue: QueuedPush[] = [];
  let draining = false;

  function clearQueue(): void {
    if (queue.length === 0) return;
    queue.length = 0;
    updateBadge();
  }

  function dropExpired(now: number): void {
    while (queue.length > 0) {
      const head = queue[0];
      if (!head || now - head.at <= PANEL_QUEUE_TTL_MS) break;
      queue.shift();
    }
  }

  function enqueue(message: RelayableServerMessage): void {
    dropExpired(Date.now());
    // Drop the OLDEST on overflow: the newest signalling is the only kind that
    // can still complete a connection.
    while (queue.length >= PANEL_QUEUE_LIMIT) queue.shift();
    queue.push({ message, at: Date.now() });
    updateBadge();
  }

  /**
   * Delivers queued pushes in order, one at a time.
   *
   * Order is the reason this is a serial loop rather than a fan of
   * `sendMessage` calls: an answer that overtakes its own offer is a connection
   * that never establishes. A rejection means no receiving end — the panel is
   * shut — so we stop and leave everything queued for its next open.
   */
  async function drainQueue(): Promise<void> {
    if (draining) return;
    draining = true;
    try {
      dropExpired(Date.now());
      while (queue.length > 0) {
        const head = queue[0];
        if (!head) break;
        const push: DaemonPush = { kind: 'daemonPush', message: head.message };
        try {
          await browser.runtime.sendMessage(push);
        } catch {
          return; // Panel closed. Keep the queue; it flushes on the next open.
        }
        queue.shift();
      }
    } finally {
      draining = false;
      updateBadge();
    }
  }

  function relayToPanel(message: RelayableServerMessage): void {
    enqueue(message);
    void drainQueue();
  }

  /**
   * The toolbar badge is the only signal we can raise with the panel closed.
   *
   * It matters most for `pair:code`: an incoming pairing request that arrives
   * while the panel is shut would otherwise be completely invisible, and the
   * user would only learn about it from the other machine. We cannot open the
   * panel ourselves — Chrome requires a user gesture for sidePanel.open() — so
   * a badge plus the queued push is the honest best effort.
   */
  function updateBadge(): void {
    const action = browser.action;
    if (!action?.setBadgeText) return;
    try {
      void Promise.resolve(action.setBadgeText({ text: queue.length > 0 ? '!' : '' })).catch(
        () => undefined,
      );
    } catch {
      // Firefox for Android and some builds have no badge. Not worth a log.
    }
  }

  // `devices` is deliberately NOT relayed as a daemonPush: it already travels
  // in the state snapshot below, and two paths for one fact means two versions
  // of the truth in the panel.
  client.on('devices', (message) => {
    snapshot.devices = message.devices;
    broadcast();
  });

  client.on('pair:code', relayToPanel);
  client.on('pair:result', relayToPanel);
  client.on('peer:state', relayToPanel);
  client.on('signal:offer', relayToPanel);
  client.on('signal:answer', relayToPanel);
  client.on('signal:ice', relayToPanel);

  client.on('error', (message) => {
    console.warn('[sharing] daemon error:', message.code, message.message);
  });

  // -- state snapshot -------------------------------------------------------

  function currentState(): BackgroundState {
    return {
      state: snapshot.state,
      info: snapshot.info,
      devices: snapshot.devices,
      enabled: snapshot.enabled,
      problem: snapshot.problem,
    };
  }

  function broadcast(): void {
    const push: StateChangedPush = { kind: 'stateChanged', ...currentState() };
    // Rejects with "Receiving end does not exist" whenever the panel is shut,
    // which is most of the time. That is expected, not an error.
    void browser.runtime.sendMessage(push).catch(() => undefined);
  }

  /** Applies the user's connect/disconnect choice and persists it. */
  async function setEnabled(enabled: boolean): Promise<void> {
    snapshot.enabled = enabled;
    try {
      await browser.storage.local.set({ [ENABLED_KEY]: enabled });
    } catch {
      // Storage can fail in a private window; the in-memory choice still holds
      // for this worker's lifetime.
    }

    if (enabled) {
      void client.fetchInfo();
      client.connect();
    } else {
      // close() sets the client's `closed` flag, which stops the backoff loop —
      // otherwise it would keep retrying behind a user-requested disconnect.
      client.close();
      snapshot.info = null;
      snapshot.devices = [];
      snapshot.problem = null;
      clearQueue();
    }
    broadcast();
  }

  // -- panel -> daemon ------------------------------------------------------

  browser.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    const request = message as RuntimeRequest | undefined;
    if (!request || typeof request.kind !== 'string') return false;

    switch (request.kind) {
      case 'getState':
        sendResponse(currentState());
        // A panel that is asking for state is a panel that exists, which makes
        // this the reliable moment to hand over anything queued for it.
        void drainQueue();
        return true;
      case 'reconnect':
        if (snapshot.enabled) client.reconnectNow();
        sendResponse(currentState());
        return true;
      case 'setEnabled':
        // Reply immediately; the socket work continues and lands via broadcast.
        void setEnabled(request.enabled);
        sendResponse({ ...currentState(), enabled: request.enabled });
        return true;
      case 'relayToDaemon': {
        // Every WebRTC offer, answer and candidate the panel produces comes
        // through here, plus pair:start / pair:confirm / pair:forget /
        // devices:refresh. The panel is told when a send fails so it can say so
        // instead of waiting on a connection that will never be negotiated.
        const response: RelayToDaemonResponse = client.isConnected()
          ? client.send(request.message)
            ? { ok: true }
            : { ok: false, reason: 'send_failed' }
          : { ok: false, reason: 'not_connected' };
        sendResponse(response);
        void drainQueue();
        return true;
      }
      default:
        return false;
    }
  });

  async function start(): Promise<void> {
    let enabled = true;
    try {
      const stored = await browser.storage.local.get(ENABLED_KEY);
      const value = stored[ENABLED_KEY];
      if (typeof value === 'boolean') enabled = value;
    } catch {
      // Fall through to the default: connect.
    }
    snapshot.enabled = enabled;

    if (!enabled) {
      // Respect a disconnect the user made before the worker was torn down.
      snapshot.state = 'idle';
      broadcast();
      return;
    }

    // Best-effort: fills in capabilities/name even before the socket is up.
    void client.fetchInfo();
    client.connect();
  }

  /**
   * Chrome does not open a side panel when the toolbar icon is clicked unless
   * we opt in. `browser` here is `globalThis.chrome` in Chromium (WXT's
   * `browser` export resolves to it), and it is the only spelling that is
   * typed in this project, so `chrome.sidePanel` is written as
   * `browser.sidePanel`. The API is Chromium-only: on Firefox the property is
   * absent at runtime and the optional chain short-circuits to `undefined`,
   * so this is a no-op there (Firefox opens `sidebar_action` on its own). The
   * catch covers Chromium builds that reject the call.
   */
  void browser.sidePanel
    ?.setPanelBehavior({ openPanelOnActionClick: true })
    .catch(() => undefined);

  browser.runtime.onInstalled.addListener(() => void start());
  browser.runtime.onStartup.addListener(() => void start());

  // Runs on every service-worker wake, which is the real reconnect trigger.
  void start();
});
