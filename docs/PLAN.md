# Sharing — Project Plan

Roadmap, milestones, schedules and matrices. The wire contract lives in
[`../shared/PROTOCOL.md`](../shared/PROTOCOL.md) and is authoritative; nothing here overrides it.

---

## 1. MVP definition

> **MVP = two machines on the same Wi-Fi, both running `share-daemon`, both with the extension
> installed. Device A's popup lists Device B. A types text, hits Send. B gets a notification and the
> text lands on B's clipboard / in B's popup. Zero cloud, zero account, zero configuration.**

That is milestone **M8**. Everything before it is infrastructure; everything after it (files, images,
video, resume, installers) is polish and scale.

Explicit non-goals for the MVP:

- No internet relay, no TURN server, no account system, no sync.
- No mobile app. Browser extension + desktop daemon only.
- No transfer resume, no multi-file queue, no folder transfer.

---

## 2. Milestones

### M1 — Repo scaffolding + extension shell  **[DONE]**

Repo layout, ignore/attribute/editor config, README, the authoritative protocol spec, the plan and
architecture docs. WXT + React + TypeScript extension that builds for Chrome MV3 and Firefox MV2, with
a popup that renders a device list from hardcoded placeholder data and a connection badge.

*Done when:* `npm run build` and `npm run build:firefox` both succeed; the unpacked extension loads in
Chrome and Firefox; the popup renders; `shared/PROTOCOL.md` exists and the TS types match it exactly.

### M2 — Go daemon skeleton  **[DONE]**

`share-daemon` binds `127.0.0.1:8765`. `GET /info` returns `DaemonInfo`, `GET /health` returns
`{"status":"ok","uptimeSeconds":N}`, unknown routes 404 as JSON, `OPTIONS` returns 204. CORS + Private
Network Access headers on every response with the origin allowlist. `/ws` upgrades and answers
`hello` -> `ready` and `ping` -> `pong`, with `error` for anything else. Device identity is a v4 UUID
persisted to `os.UserConfigDir()/sharing/config.json` (0600 / dir 0700).

*Done when:* `curl http://127.0.0.1:8765/info` returns valid `DaemonInfo`; a `wscat`/browser socket to
`/ws` completes the hello/ready and ping/pong exchanges; restarting the daemon returns the same
`deviceId`; deleting the config regenerates it with a warn log.

### M3 — Extension <-> daemon live connection  **[PENDING]**

Background service worker owns the WebSocket. `GET /info` on startup, then connect `/ws` and send
`hello`. Exponential reconnect backoff 500ms -> 1s -> 2s -> 4s, capped 15s, with jitter. Popup asks the
background for state on mount (`{kind:'getState'}`) and receives live `{kind:'stateChanged'}` pushes.
The hardcoded device list is removed from the popup once real state exists.

*Done when:* starting the daemon flips the popup badge to `connected` within one backoff tick without
reopening the popup; killing the daemon flips it to `reconnecting` then back to `connected` on restart;
this survives an MV3 service-worker termination.

### M4 — mDNS advertise + browse  **[PENDING]**

`github.com/libp2p/zeroconf/v2`. Advertise `_sharing._tcp` in `local.` with TXT keys `id`, `name`,
`ver`, `proto`. Browse continuously, maintain an in-memory peer table with `lastSeen`, expire stale
entries, and ignore our own advertisement by `deviceId`.

*Done when:* two daemons on the same LAN each list the other within ~3s; `dns-sd -B _sharing._tcp`
(macOS) / `avahi-browse -r _sharing._tcp` (Linux) shows the service; a killed daemon disappears
from the peer table inside the expiry window.

### M5 — Device list to the extension  **[PENDING]**

The daemon pushes `{"type":"devices","devices":[Device]}` on every peer-table change (debounced). The
background caches it and includes it in `getState` replies and `stateChanged` pushes. Popup renders
real devices with online/offline status and last-seen.

*Done when:* plugging a second machine onto the network makes it appear in the popup with no user
action; unplugging it greys it out then removes it.

### M6 — Pairing and trust  **[PENDING]**

`device:pair:request` / `device:pair:response` with a 6-digit code shown on the initiator and confirmed
on the receiver. Paired peers persist to the config directory. Unpaired peers are visible but cannot
open a transfer.

*Done when:* pairing succeeds with the matching code and fails with a wrong one; the trust survives a
daemon restart on both ends; a transfer attempt from an unpaired peer is rejected.

### M7 — WebRTC signaling + DataChannel  **[PENDING]**

The daemon relays `signal:offer` / `signal:answer` / `signal:ice` between paired peers over their
daemon-to-daemon link. The extension owns the `RTCPeerConnection` and opens a DataChannel. LAN host
candidates first; public STUN as fallback.

*Done when:* `chrome://webrtc-internals` shows an `RTCPeerConnection` reaching `connected` with a
host-to-host candidate pair on the LAN, and a DataChannel reaching `open` on both ends.

### M8 — Send text end-to-end  **[PENDING]**  *(MVP)*

Compose text in the popup, pick a paired device, send over the DataChannel. Receiver shows a browser
notification, exposes the text in its popup and offers copy-to-clipboard.

*Done when:* text typed on machine A appears on machine B in under one second, with the daemon never
having seen the payload (only signaling), and no internet connection required.

---

## 3. Beyond the MVP (M9–M15)

| Milestone | Scope |
|-----------|-------|
| M9  | File transfer over the DataChannel: `transfer:start` / `:progress` / `:complete` / `:cancel`, 64 KiB chunks, backpressure via `bufferedAmountLowThreshold`, progress UI |
| M10 | Images and clipboard integration: paste-to-send, drag-and-drop, image preview on the receiver |
| M11 | Large files and video: streaming to disk on the receiver, resumable chunk offsets, integrity hash |
| M12 | Context menus and keyboard shortcuts: right-click "Send to device" on links, images, selection |
| M13 | Daemon lifecycle: run at login, tray icon, auto-update check, structured logging, `--name` / `--port` flags |
| M14 | Packaging and distribution: store submissions and signed installers (see section 7) |
| M15 | Optional hosted signaling relay for devices on different networks — offer/answer/ICE only, never file bytes. Placeholder lives in [`../signaling/`](../signaling/) and [`../docker-compose.yml`](../docker-compose.yml). Not required for LAN transfer. |

---

## 4. Week one, day by day

| Day | Focus | Deliverable |
|-----|-------|-------------|
| Day 1 | M1 | Repo scaffolding, protocol spec locked, WXT extension builds for Chrome + Firefox, popup shell with placeholder devices |
| Day 2 | M2 | Go daemon: config/identity, `/info`, `/health`, 404 JSON, CORS + PNA, `/ws` hello/ready/ping/pong |
| Day 3 | M3 | Background-owned WebSocket, backoff + jitter reconnect, popup/background messaging, real connection badge |
| Day 4 | M4 | zeroconf advertise + browse, peer table with expiry, self-filtering, verified with two machines |
| Day 5 | M5 | `devices` push, background cache, popup renders live peers; first real "I can see my other laptop" moment |
| Day 6 | M6 | Pairing flow, 6-digit code UI, persisted trust store, rejection path for unpaired peers |
| Day 7 | M7 + M8 | Signaling relay, `RTCPeerConnection` + DataChannel, send text end-to-end. **MVP demo.** |

Buffer expectation: M4 (mDNS across Windows Firewall / macOS local-network permission) and M7 (ICE
candidate gathering inside an extension context) are the two days most likely to slip. Budget a spare
half-day for each.

---

## 5. Cross-browser test matrix

Every cell is *sender -> receiver*, exercised with each payload class.

```
                              R E C E I V E R
              +--------------+--------------+--------------+
  SENDER      |   Chrome     |    Brave     |   Firefox    |
+-------------+--------------+--------------+--------------+
| Chrome      |  text/img/   |  text/img/   |  text/img/   |
|             |  file/video  |  file/video  |  file/video  |
+-------------+--------------+--------------+--------------+
| Brave       |  text/img/   |  text/img/   |  text/img/   |
|             |  file/video  |  file/video  |  file/video  |
+-------------+--------------+--------------+--------------+
| Firefox     |  text/img/   |  text/img/   |  text/img/   |
|             |  file/video  |  file/video  |  file/video  |
+-------------+--------------+--------------+--------------+
```

Payload classes per cell:

| Class | Fixture | Passes when |
|-------|---------|-------------|
| text  | 12 chars, then 1 MB of UTF-8 incl. emoji | byte-identical on arrival |
| image | 2.4 MB PNG, 6 MB JPEG | byte-identical, preview renders |
| file  | 40 MB zip | byte-identical, progress monotonic, no OOM |
| video | 700 MB MP4 | byte-identical, streams to disk, memory stays flat |

Browser-specific checks that must pass in every column:

- **Chrome / Brave (MV3):** service worker termination mid-transfer; Private Network Access preflight on
  `127.0.0.1`; `chrome://webrtc-internals` candidate pair is host-to-host on LAN.
- **Brave:** Shields on (its default) — Brave restricts local-network access and can block WebRTC IP
  discovery; verify with Shields both up and down.
- **Firefox (MV2):** the background page persists, so the reconnect path is exercised by a daemon
  restart rather than worker death; `about:webrtc` for the candidate pair; reloading the temporary
  add-on resets state.

---

## 6. OS matrix

```
                       D A E M O N   H O S T
              +-----------+-----------+-----------+
              |  Windows  |   macOS   |   Linux   |
              |    11     |   14/15   | Ubuntu 24 |
+-------------+-----------+-----------+-----------+
| Windows 11  |    OK     |    OK     |    OK     |
| macOS       |    OK     |    OK     |    OK     |
| Linux       |    OK     |    OK     |    OK     |
+-------------+-----------+-----------+-----------+
   (rows = the other endpoint; every pair must transfer in both directions)
```

Per-OS gotchas to verify explicitly:

- **Windows 11:** Windows Defender Firewall prompt on the first mDNS bind; `os.UserConfigDir()` resolves
  to `%AppData%`; file ACLs are the closest analogue to `0600` — verify the config is not
  world-readable.
- **macOS 14+:** the *Local Network* privacy permission must be granted or mDNS silently sees nothing;
  Gatekeeper requires the binary to be signed and notarized for a non-terminal launch.
- **Linux:** Avahi must be running for `_sharing._tcp` to resolve; `ufw`/`firewalld` must allow
  UDP 5353; `XDG_CONFIG_HOME` falls back to `~/.config`.

---

## 7. Packaging targets

**Extension**

| Target | Artifact | Notes |
|--------|----------|-------|
| Chrome Web Store | `chrome-mv3` zip from `extension/.output` | MV3; justify the `127.0.0.1:8765` `host_permissions` in the review notes; PNA declared |
| Firefox Add-ons (AMO) |  signed `.xpi` from `firefox-mv3` | MV3 build; AMO source-code submission because the bundle is built |
| Brave | uses the Chrome Web Store listing | no separate submission; test with Shields on |
| Edge Add-ons | *stretch* — same MV3 zip | not in the MVP |

**Daemon (`share-daemon`)**

| Target | Artifact |
|--------|----------|
| Windows | `share-daemon.exe` (amd64, arm64) + signed MSI/NSIS installer, run-at-login registration |
| macOS   | universal binary (amd64 + arm64), signed and notarized `.pkg`, LaunchAgent plist |
| Linux   | static amd64/arm64 binaries, `.deb` + `.rpm` + tarball, systemd user unit |

Every release ships checksums, and the daemon version must equal `APP_VERSION` in the protocol spec.

**Release checklist**

1. Bump `APP_VERSION` in `shared/PROTOCOL.md`, the Go build, and the extension manifest — all three.
2. Cross-compile the daemon for all six OS/arch pairs; sign and notarize.
3. Build both extension targets; run the full matrix in section 5 on at least one OS pair.
4. Tag, publish binaries + checksums, submit both store packages.
