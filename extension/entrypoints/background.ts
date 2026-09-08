import { DaemonClient } from '@/lib/daemon';
import type {
  BackgroundState,
  ConnectionState,
  DaemonInfo,
  RuntimeRequest,
  StateChangedPush,
} from '@/types';

/** Persisted across service-worker restarts so a manual Disconnect sticks. */
const ENABLED_KEY = 'nearbyShare.enabled';

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
 */
export default defineBackground(() => {
  const snapshot: BackgroundState = {
    state: 'idle',
    info: null,
    devices: [],
    enabled: true,
  };

  const client = new DaemonClient({
    onStateChange: (state: ConnectionState) => {
      snapshot.state = state;
      if (state !== 'connected') {
        // Anything we learned over the socket is stale once it drops.
        snapshot.devices = [];
      }
      broadcast();
    },
    onInfo: (info: DaemonInfo | null) => {
      snapshot.info = info;
      broadcast();
    },
  });

  // M5 wires real discovery; the daemon already owns this message shape.
  client.on('devices', (message) => {
    snapshot.devices = message.devices;
    broadcast();
  });

  client.on('error', (message) => {
    console.warn('[nearby-share] daemon error:', message.code, message.message);
  });

  function currentState(): BackgroundState {
    return {
      state: snapshot.state,
      info: snapshot.info,
      devices: snapshot.devices,
      enabled: snapshot.enabled,
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
    }
    broadcast();
  }

  browser.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    const request = message as RuntimeRequest | undefined;
    if (!request || typeof request.kind !== 'string') return false;

    switch (request.kind) {
      case 'getState':
        sendResponse(currentState());
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
