import { DaemonClient } from '@/lib/daemon';
import type {
  BackgroundState,
  ConnectionState,
  DaemonInfo,
  RuntimeRequest,
  StateChangedPush,
} from '@/types';

/**
 * The background script OWNS the daemon connection.
 *
 * The popup is destroyed the moment it closes, so a popup-owned socket would
 * tear down and reconnect on every open. Instead the popup asks us for the
 * current snapshot on mount ({kind:'getState'}) and we push {kind:'stateChanged'}
 * whenever anything moves, so an open popup stays live.
 *
 * Chrome MV3 terminates an idle service worker after ~30s. We do not fight
 * that: this module reconnects on every wake (the top-level connect() below
 * runs again when the worker restarts) and DaemonClient retries with
 * exponential backoff — 500ms, 1s, 2s, 4s, ... capped at 15s, with jitter.
 * An open WebSocket also counts as activity, so a healthy connection keeps the
 * worker alive on its own.
 */
export default defineBackground(() => {
  const snapshot: BackgroundState = {
    state: 'idle',
    info: null,
    devices: [],
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

  function broadcast(): void {
    const push: StateChangedPush = {
      kind: 'stateChanged',
      state: snapshot.state,
      info: snapshot.info,
      devices: snapshot.devices,
    };
    // Rejects with "Receiving end does not exist" whenever the popup is shut,
    // which is most of the time. That is expected, not an error.
    void browser.runtime.sendMessage(push).catch(() => undefined);
  }

  browser.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    const request = message as RuntimeRequest | undefined;
    if (!request || typeof request.kind !== 'string') return false;

    switch (request.kind) {
      case 'getState':
        sendResponse({
          state: snapshot.state,
          info: snapshot.info,
          devices: snapshot.devices,
        } satisfies BackgroundState);
        return true;
      case 'reconnect':
        client.reconnectNow();
        sendResponse({
          state: snapshot.state,
          info: snapshot.info,
          devices: snapshot.devices,
        } satisfies BackgroundState);
        return true;
      default:
        return false;
    }
  });

  function start(): void {
    // Best-effort: fills in capabilities/name even before the socket is up.
    void client.fetchInfo();
    client.connect();
  }

  browser.runtime.onInstalled.addListener(() => start());
  browser.runtime.onStartup.addListener(() => start());

  // Runs on every service-worker wake, which is the real reconnect trigger.
  start();
});
