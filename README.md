# Nearby Share

AirDrop for the browser — send text, images and files to devices on your LAN from a browser
extension, routed peer-to-peer by a tiny local Go daemon. No cloud, no account, no upload.

---

## Architecture

```
  +-----------------------------------------------------------+
  |  Browser (Chrome / Brave / Firefox)                        |
  |                                                            |
  |   +--------------+        +-----------------------------+  |
  |   |    Popup     | <----> |  Background service worker  |  |
  |   |  (React UI)  |  msg   |   owns the WebSocket        |  |
  |   +--------------+        +--------------+--------------+  |
  +----------------------------------------------|-------------+
                                                 |
                        HTTP  GET /info /health  |  ws://127.0.0.1:8765/ws
                        (loopback only)          |
                                                 v
  +-----------------------------------------------------------+
  |  share-daemon  (Go, binds 127.0.0.1:8765)                  |
  |                                                            |
  |   HTTP API  |  WebSocket hub  |  device identity/config    |
  |   mDNS advertise + browse  (_nearby-share._tcp . local.)   |
  |   WebRTC signaling relay (offer / answer / ICE)            |
  +-------------------------------|---------------------------+
                                  |
                       mDNS on the local network
                                  |
                                  v
                    +-------------------------------+
                    |  Peer device running the same |
                    |  daemon + extension           |
                    +---------------|---------------+
                                    |
                    WebRTC DataChannel (DTLS-encrypted)
                    LAN direct  ->  STUN  ->  TURN (fallback)
                                    |
                                    v
                         bytes move device <-> device
```

The daemon exists because a browser extension cannot do mDNS, UDP broadcast, raw sockets, or bind to
arbitrary network interfaces. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Features

- Local-network device discovery over mDNS (`_nearby-share._tcp`) — **planned, M4**
- Peer-to-peer transfer over a WebRTC DataChannel — **planned, M7**
- Send text / links, then images, files and video — **planned, M9+**
- Cross-browser: Chrome, Brave and Firefox from one WXT codebase
- Cross-platform daemon: Windows, macOS, Linux
- Loopback-only daemon bind — nothing listens on your LAN interface except mDNS
- No cloud relay, no account, no telemetry

---

## Status

| Milestone | Scope                                                        | Status  |
|-----------|--------------------------------------------------------------|---------|
| M1        | Repo scaffolding, protocol spec, extension shell + popup UI   | Done    |
| M2        | Go daemon skeleton: HTTP `/info` + `/health`, WebSocket `/ws`, device identity, CORS/PNA | Done    |
| M3        | Extension <-> daemon live connection, reconnect backoff, real state in popup | Pending |
| M4        | mDNS advertise + browse, real device list                     | Pending |
| M5        | Device list push to the extension (`devices` message)         | Pending |
| M6        | Pairing and trust (6-digit code, persisted peers)             | Pending |
| M7        | WebRTC signaling relay + DataChannel establishment            | Pending |
| M8        | Send text end-to-end between two machines                     | Pending |

Everything past M2 is **not implemented**. With no daemon running (the normal case today) the popup
reports `disconnected` and shows a hardcoded placeholder device list — that is intended M1 behaviour.

Full roadmap through M15: [`docs/PLAN.md`](docs/PLAN.md).

---

## Quick Start

### 1. Run the daemon

```bash
cd daemon
go run ./cmd/daemon
```

It binds `127.0.0.1:8765`. Verify:

```bash
curl http://127.0.0.1:8765/info
curl http://127.0.0.1:8765/health
```

### 2. Run the extension

```bash
cd extension
npm install
npm run dev
```

For Firefox:

```bash
npm run dev:firefox
```

`npm run dev` launches a browser with the extension already loaded. To load a build by hand:

**Chrome / Brave**
1. Open `chrome://extensions`
2. Enable **Developer mode** (top right)
3. **Load unpacked** -> select `extension/.output/chrome-mv3`

**Firefox**
1. Open `about:debugging#/runtime/this-firefox`
2. **Load Temporary Add-on...**
3. Select `extension/.output/firefox-mv3/manifest.json`

---

## Repo Layout

```
sharing/
├── extension/            WXT + React + TypeScript browser extension
│   ├── entrypoints/
│   │   ├── background.ts   owns the WebSocket to the daemon
│   │   └── popup/          React UI
│   ├── types/index.ts      shared TS types (mirrors shared/PROTOCOL.md)
│   └── wxt.config.ts
├── daemon/               Go local daemon (share-daemon)
│   ├── cmd/daemon/         main
│   ├── protocol/           wire types, mirrors shared/PROTOCOL.md 1:1
│   ├── server/             HTTP + WebSocket
│   └── config/             device identity, config.json
├── shared/
│   └── PROTOCOL.md         AUTHORITATIVE wire contract — both sides follow it
├── docs/
│   ├── PLAN.md             roadmap, milestones, test matrix, packaging
│   └── ARCHITECTURE.md     why a daemon, responsibility split, lifecycle
├── signaling/              placeholder for the optional M15 relay (not built)
├── docker-compose.yml      commented-out placeholder for that relay
├── .editorconfig
├── .gitattributes
└── .gitignore
```

---

## Security Model

- **Loopback-only bind.** The daemon's HTTP + WebSocket listener binds `127.0.0.1`, never `0.0.0.0`.
  Nothing on your LAN can reach the control plane.
- **Origin allowlist + Private Network Access.** Only `chrome-extension://`, `moz-extension://`,
  `safari-web-extension://` and `http://localhost:<port>` / `http://127.0.0.1:<port>` origins are
  echoed back in `Access-Control-Allow-Origin`. Chrome's PNA preflight for `127.0.0.1` is answered
  explicitly.
- **No cloud relay.** Bytes go device to device on your LAN. A TURN relay is a last-resort fallback
  and is not part of the MVP; there is no server that ever stores your files.
- **WebRTC encryption.** DataChannels are DTLS-encrypted end to end (mandatory in WebRTC, not
  optional).
- **Device trust.** Each device has a persisted RFC 4122 v4 UUID. Pairing (M6) uses a 6-digit code
  confirmed on both ends; unpaired devices cannot open a transfer.
- **Local identity file.** `os.UserConfigDir()/nearby-share/config.json`, mode `0600`, dir `0700`.

---

## Documentation

- [`shared/PROTOCOL.md`](shared/PROTOCOL.md) — the authoritative wire protocol. TypeScript and Go
  must match it field-for-field.
- [`docs/PLAN.md`](docs/PLAN.md) — milestones, week-one schedule, browser/OS test matrix, packaging.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — design rationale and connection lifecycle.
