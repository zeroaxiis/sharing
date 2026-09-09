# Sharing — Architecture

Design rationale and the connection lifecycle. The wire format itself is specified in
[`../shared/PROTOCOL.md`](../shared/PROTOCOL.md), which is authoritative; this document explains *why*
the pieces are shaped the way they are.

---

## 1. Why a local daemon exists

The obvious question: this is a browser extension, so why ship a native binary alongside it?

Because a browser — extension APIs included — cannot do any of the things local device discovery
requires. This is a hard sandbox boundary, not a missing feature:

| Capability needed | Available to an extension? | Why not |
|-------------------|----------------------------|---------|
| mDNS / DNS-SD (multicast UDP 5353) | **No** | Extensions have no multicast API. `chrome.mdns` existed only for Chrome Apps and is gone. |
| UDP broadcast / arbitrary UDP sockets | **No** | `chrome.sockets.udp` was a Chrome Apps API; MV3 extensions have no socket API at all. |
| Raw TCP listener (accept inbound) | **No** | An extension can only make outbound `fetch`/WebSocket connections. It cannot listen. |
| Enumerate / bind a specific network interface | **No** | No interface enumeration exists. Even WebRTC hides local IPs behind mDNS-obfuscated candidates. |
| Read the LAN IP directly | **No** | Deliberately blocked as a fingerprinting vector. |
| Write files outside the download flow | **No** | Downloads only, and always through the browser's download machinery. |
| Persist and run while the browser is closed | **No** | MV3 kills an idle service worker in about 30 seconds. |

What the browser *can* do is exactly two useful things: talk to `127.0.0.1` over HTTP/WebSocket (with
the right permissions and a Private Network Access preflight), and run a full WebRTC stack including
DataChannels.

So the split writes itself: put every OS-level capability in a small native daemon, keep every UI and
every byte of the payload in the browser, and let them talk over loopback.

```
   what the browser CAN do              what only a native process can do
  +----------------------------+      +-----------------------------------+
  |  UI, clipboard, downloads  |      |  mDNS advertise + browse          |
  |  WebRTC PeerConnection     |      |  bind sockets, list interfaces    |
  |  DataChannel (DTLS)        |      |  persistent identity on disk      |
  |  fetch / WebSocket to      |      |  run independent of the browser   |
  |  127.0.0.1 only            |      |  daemon <-> daemon signaling      |
  +-------------+--------------+      +------------------+----------------+
                |                                        |
                +----------- loopback: HTTP + ws ---------+
```

---

## 2. Responsibility split

```
+---------------------------------------------------------------+
|  EXTENSION  (WXT + React + TypeScript)                         |
|                                                                |
|  background service worker  — the only WebSocket owner         |
|    * GET /info on startup, connect /ws, send hello             |
|    * reconnect with exponential backoff + jitter               |
|    * caches ConnectionState, DaemonInfo, Device[]              |
|    * owns RTCPeerConnection and the DataChannel  (M7+)         |
|    * chunks and reassembles payloads             (M9+)         |
|                                                                |
|  popup  — pure view                                            |
|    * on mount: sendMessage {kind:'getState'}                   |
|    * listens for {kind:'stateChanged'} pushes                  |
|    * never opens a socket of its own                           |
+---------------------------------------------------------------+
                         | loopback only
+---------------------------------------------------------------+
|  DAEMON  (Go, share-daemon, binds 127.0.0.1:8765)              |
|                                                                |
|  HTTP     /info  /health  404-as-JSON  OPTIONS  CORS+PNA       |
|  WS       /ws  hello->ready, ping->pong, error                 |
|  config   v4 UUID + name, persisted 0600 in the user config dir|
|  mDNS     advertise + browse _sharing._tcp   (M4)         |
|  peers    in-memory table with lastSeen + expiry   (M4/M5)     |
|  trust    paired device store                      (M6)        |
|  signal   relays offer/answer/ICE between daemons  (M7)        |
|                                                                |
|  NEVER sees the payload. Signaling and discovery only.         |
+---------------------------------------------------------------+
```

Two rules that the split depends on, and that must not be relaxed:

1. **The daemon never touches payload bytes.** Text, images and files travel only over the WebRTC
   DataChannel, browser to browser. The daemon's job ends when the DataChannel opens. This keeps the
   trusted-code surface small and makes "no cloud, no relay" true by construction rather than by policy.
2. **The WebSocket belongs to the background script, not the popup.** The popup is destroyed the moment
   it loses focus. A popup-owned socket would reconnect on every open, re-run the handshake, and drop
   any in-flight transfer. The popup is a view over state the background already holds.

### MV3 service-worker termination

Chrome terminates an idle MV3 service worker after roughly 30 seconds. Design consequences:

- All state that must survive is either re-derivable (reconnect and re-fetch `/info`) or persisted.
- On wake, the background reconnects with backoff **500ms -> 1s -> 2s -> 4s, capped at 15s, with
  jitter**. Jitter matters when several browser profiles wake at once against one daemon.
- The popup never assumes the background is alive with fresh state; it always asks on mount.
- Firefox MV2 keeps a persistent background page, so it exercises the same reconnect path only on a
  daemon restart. Both paths must be tested — see the matrix in [`PLAN.md`](PLAN.md).

### Loopback and the browser's local-network rules

Reaching `127.0.0.1` from an extension trips Chrome's Private Network Access checks. The daemon answers
the PNA preflight explicitly (`Access-Control-Allow-Private-Network: true`) and echoes only allowlisted
origins — extension schemes and `localhost`/`127.0.0.1` with any port. Every other origin gets no
`Access-Control-Allow-Origin` header at all, so the browser blocks the response. Because the daemon
binds `127.0.0.1` and never `0.0.0.0`, no machine on the LAN can even open the socket; the origin
allowlist is defence in depth against a malicious page in the *user's own* browser.

---

## 3. Connection lifecycle

From cold start to bytes moving:

```
  DEVICE A                          LAN                        DEVICE B
  --------                          ---                        --------

  1. daemon starts
     load-or-create config.json  (v4 UUID + name, 0600)
     bind 127.0.0.1:8765
     advertise _sharing._tcp   ---- mDNS ---->             (B sees A)
     browse  _sharing._tcp     <--- mDNS -----             (A sees B)

  2. extension background wakes
     GET http://127.0.0.1:8765/info   -> DaemonInfo
     ws  ://127.0.0.1:8765/ws
       -> {"type":"hello","id":"c1","protocolVersion":1,...}
       <- {"type":"ready","id":"c1","protocolVersion":1,"device":{...}}
     state: connecting -> connected

  3. discovery push
       <- {"type":"devices","devices":[Device,...]}      (M5)
     popup renders B, status online

  4. pairing                                             (M6)
     A: device:pair:request  {deviceId:B, code:"482913"}
        code shown on A, typed/confirmed on B
     B: device:pair:response {deviceId:A, accepted:true}
     both persist the peer; trust survives restarts

  5. signaling — daemons relay, browsers negotiate       (M7)
     A ext -> A daemon : signal:offer  {to:B, sdp}
                         A daemon -> B daemon -> B ext
     B ext -> B daemon : signal:answer {to:A, sdp}
     both directions   : signal:ice    {to:.., candidate}

  6. DataChannel opens (DTLS, host-to-host on the LAN)
     ==========================================================
      payload flows browser <-> browser, 64 KiB chunks
      the daemons are now idle bystanders
     ==========================================================

  7. transfer                                            (M9)
     transfer:start -> chunks -> transfer:progress -> transfer:complete
     cancel at any point with transfer:cancel
```

State machine held by the background script:

```
        +--------+   connect    +------------+   ready    +-----------+
        |  idle  | -----------> | connecting | ---------> | connected |
        +--------+              +------------+            +-----------+
                                      ^  |                    |
                       backoff+jitter |  | fail          drop |
                                      |  v                    v
                                +--------------+         +--------------+
                                |    error     | <------ | reconnecting |
                                +--------------+         +--------------+
                                        (fatal: protocol_mismatch)
```

`protocol_mismatch` is terminal on purpose: a daemon speaking a different `protocolVersion` should
surface a clear "update one side" message rather than reconnect-loop forever.

---

## 4. Connectivity ladder: LAN -> STUN -> TURN

WebRTC gathers candidates in tiers. Sharing is built so the first tier almost always wins, and so
that the last tier is optional and off by default.

```
  Tier 1  HOST / LAN  (the design target, ~100% of intended use)
  -------------------------------------------------------------
    Both devices are on the same subnet. ICE picks a host-to-host
    candidate pair. No server of any kind is contacted.
    Latency: sub-millisecond.  Throughput: wire speed.
    Requires: mDNS worked, and the LAN does not have client isolation.

              A ============================ B
                    direct, DTLS, on-link

  Tier 2  STUN / SERVER-REFLEXIVE  (fallback)
  -------------------------------------------------------------
    Same network but ICE cannot use host candidates -- e.g. AP client
    isolation, a guest VLAN, a docker/VPN interface confusing the
    candidate set, or two different subnets that still route.
    A public STUN server reports each peer's reflexive address; the
    media path is still peer to peer. STUN sees an IP and a port,
    never a byte of payload.

              A ---- stun:? ----> [ STUN ]
              B ---- stun:? ----> [ STUN ]
              A ============================ B
                    direct, via reflexive candidates

  Tier 3  TURN / RELAYED  (last resort, NOT in the MVP)
  -------------------------------------------------------------
    Symmetric NAT on both ends, or peers on genuinely different
    networks. All traffic is relayed through a TURN server.
    Still end-to-end DTLS-encrypted -- the relay forwards ciphertext
    and cannot read it -- but it costs bandwidth and it is a third
    party in the path.

              A <====> [ TURN relay ] <====> B
                     encrypted, relayed

    Sharing ships with NO TURN server configured. If a hosted
    relay is ever added it is the M15 signaling service, it is
    opt-in, and it relays offer/answer/ICE -- see ../signaling/.
```

Practical consequence for the MVP: if tier 1 fails, the correct product behaviour is to tell the user
*"these devices can see each other but cannot connect directly — check for AP/client isolation or a
guest network"*, not to silently fall back to a relay. Discovery working while connection fails is a
network-configuration problem, and saying so is more useful than hiding it.

---

## 5. Why these specific choices

| Choice | Reason |
|--------|--------|
| Go for the daemon | Single static binary per OS/arch, no runtime to install, first-class cross-compilation, solid mDNS and WebSocket libraries. |
| `github.com/coder/websocket` | Maintained successor to `nhooyr.io/websocket`; context-aware, small API, no cgo. |
| `github.com/libp2p/zeroconf/v2` | Maintained fork of `grandcat/zeroconf`, which is stuck at v1.0.0; handles advertise *and* browse in one library. |
| WXT + React | One codebase that emits both Chrome MV3 and Firefox MV2 bundles, with HMR in development. Avoids maintaining two manifests by hand. |
| WebRTC DataChannel over a plain TCP socket | DTLS encryption is mandatory and free; NAT traversal comes with it; the browser already has the stack, so no crypto code of our own. |
| Loopback HTTP + WebSocket rather than native messaging | Native messaging requires a per-browser host manifest installed per user, and is Chrome/Firefox-specific glue. One loopback port works for every browser and is trivially debuggable with `curl`. |
| mDNS rather than a broadcast ping of our own | Standard, already permitted on most networks, discoverable with `dns-sd` / `avahi-browse`, and gives us TXT records for `id` / `name` / `ver` / `proto` for free. |
