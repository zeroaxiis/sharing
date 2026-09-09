# SHARING — AUTHORITATIVE PROTOCOL SPEC v1

This is the single source of truth. The TypeScript and Go implementations MUST match field-for-field.
All JSON field names are camelCase. Do not rename, do not add fields not listed, do not change casing.

## Constants

- `PROTOCOL_VERSION = 1`
- `APP_VERSION = "0.1.0"`
- Default daemon bind: `127.0.0.1:8765`  (loopback ONLY — never `0.0.0.0`)
- WebSocket path: `/ws`        -> `ws://127.0.0.1:8765/ws`
- mDNS service type: `"_sharing._tcp"`   domain `"local."`
- mDNS TXT keys: `id`, `name`, `ver`, `proto`
- `CHUNK_SIZE = 65536`  (64 KiB)

## HTTP API

`GET /info`  -> 200 `application/json`, `DaemonInfo`:

```json
{
  "id": "8f3a2d91-4c1b-4a77-9e02-1f6b5c3d0a11",
  "name": "Aashish's Laptop",
  "version": "0.1.0",
  "platform": "windows",
  "protocolVersion": 1,
  "capabilities": ["text"]
}
```

`GET /health` -> 200 `{"status":"ok","uptimeSeconds":42}`

`OPTIONS` on any route -> 204 with the CORS headers below.

Any other route -> 404 `{"error":"not found"}`

### CORS / Private Network Access headers (on every response, including 404 and the OPTIONS preflight)

- `Access-Control-Allow-Origin: <echo the request Origin if it is allowed, else omit the header entirely>`
- `Access-Control-Allow-Methods: GET, POST, OPTIONS`
- `Access-Control-Allow-Headers: Content-Type`
- `Access-Control-Allow-Private-Network: true`      (Chrome PNA preflight for 127.0.0.1)
- `Access-Control-Max-Age: 600`
- `Vary: Origin`

An Origin is ALLOWED iff it starts with `"chrome-extension://"` or `"moz-extension://"` or
`"safari-web-extension://"`, or it is exactly `"http://localhost:<port>"` / `"http://127.0.0.1:<port>"`.
Everything else is rejected.

A request with NO Origin header (curl, native client) is allowed for HTTP but must be handled explicitly.

## WebSocket protocol (`ws://127.0.0.1:8765/ws`)

Envelope: every message is a JSON object with a `"type"` string. Requests carry a client-generated
`"id"` string; the matching response echoes the same `"id"`. Server-initiated pushes have no `"id"`.

### Implemented in this milestone

Client -> Server:

```json
{"type":"hello","id":"c1","protocolVersion":1,"client":"extension","clientVersion":"0.1.0"}
{"type":"ping","id":"c2","t":1757370000000}
```

Server -> Client:

```json
{"type":"ready","id":"c1","protocolVersion":1,"device":{"id":"8f3a...","name":"Aashish's Laptop","version":"0.1.0","platform":"windows"}}
{"type":"pong","id":"c2","t":1757370000000}
{"type":"error","id":"c2","code":"unknown_type","message":"unsupported message type: foo"}
```

Error codes: `"unknown_type"` | `"bad_json"` | `"protocol_mismatch"` | `"internal"`

### RESERVED — define the types NOW so later milestones do not rename anything. Not wired up in this pass.

```json
{"type":"devices","devices":[Device]}                                              (M5 push)
{"type":"device:pair:request","id":"c3","deviceId":"...","code":"482913"}          (M6)
{"type":"device:pair:response","id":"c3","deviceId":"...","accepted":true}         (M6)
{"type":"signal:offer","id":"c4","to":"...","from":"...","sdp":"..."}              (M7)
{"type":"signal:answer","id":"c4","to":"...","from":"...","sdp":"..."}             (M7)
{"type":"signal:ice","id":"c4","to":"...","from":"...","candidate":"..."}          (M7)
{"type":"transfer:start","id":"t1","transferId":"...","name":"photo.png","mime":"image/png","size":2458123}   (M9)
{"type":"transfer:progress","transferId":"...","bytes":1048576,"total":2458123}    (M9)
{"type":"transfer:complete","transferId":"..."}                                    (M9)
{"type":"transfer:cancel","transferId":"...","reason":"user_cancelled"}            (M9)
```

## Shared types — TypeScript (`extension/types/index.ts` must contain exactly these shapes)

```ts
export const PROTOCOL_VERSION = 1;
export type Platform = 'windows' | 'darwin' | 'linux' | 'unknown';
export type DeviceStatus = 'online' | 'offline';
export type ConnectionState = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'error';

export interface Device {
  id: string; name: string; address: string; port: number;
  platform: Platform; version: string; status: DeviceStatus; lastSeen: number;
}
export interface DaemonInfo {
  id: string; name: string; version: string;
  platform: Platform; protocolVersion: number; capabilities: string[];
}
export interface SelfDevice { id: string; name: string; version: string; platform: Platform; }
```

## Shared types — Go (`daemon/protocol/messages.go` must mirror the above 1:1)

```go
type DaemonInfo struct {
  ID string `json:"id"`
  Name string `json:"name"`
  Version string `json:"version"`
  Platform string `json:"platform"`
  ProtocolVersion int `json:"protocolVersion"`
  Capabilities []string `json:"capabilities"`
}
type Device struct {
  ID string `json:"id"`; Name string `json:"name"`; Address string `json:"address"`; Port int `json:"port"`
  Platform string `json:"platform"`; Version string `json:"version"`; Status string `json:"status"`; LastSeen int64 `json:"lastSeen"`
}
```

Envelope decode pattern: decode into a struct with only `Type`/`ID`, switch on `Type`, then
re-unmarshal the raw bytes into the concrete type.

## Device identity

- `deviceId`: RFC 4122 v4 UUID, generated once, persisted.
- Config file: `os.UserConfigDir()/sharing/config.json`, mode `0600`, dir `0700`.

```json
{"deviceId":"8f3a...","name":"Aashish's Laptop","createdAt":"2026-09-09T01:00:00Z"}
```

- `name` default: `os.Hostname()`; overridable with `--name`.
- Corrupt/missing config -> regenerate silently, log at warn level.
- `platform` from `runtime.GOOS` mapped to the Platform union (`"windows"` | `"darwin"` | `"linux"` | else `"unknown"`).

## Extension architecture directive (follow exactly)

- The WebSocket is owned by the BACKGROUND script, not the popup. The popup is destroyed on close, so a
  popup-owned socket would reconnect on every open.
- Chrome MV3 terminates an idle service worker (~30s). Mitigate: the background reconnects on wake with
  exponential backoff (500ms, 1s, 2s, 4s, capped at 15s, with jitter), and the popup asks the background for
  current state on mount via `browser.runtime.sendMessage`.
- Popup <-> background messaging: request `{kind:'getState'}` -> reply
  `{state: ConnectionState, info: DaemonInfo|null, devices: Device[]}`.
  Background pushes `{kind:'stateChanged', ...}` so an open popup updates live.
- `host_permissions` must include `"http://127.0.0.1:8765/*"` and `"http://localhost:8765/*"`.
  NOTE: `"ws://"` is NOT a valid `host_permissions` scheme — the `http://` entry covers the WebSocket upgrade.
- When no daemon is reachable (the normal case right now), the popup shows connection status
  `"disconnected"` and falls back to the hardcoded fake device list. This is Milestone 1 behaviour and is
  intentional.
