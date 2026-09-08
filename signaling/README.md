# signaling/ — placeholder (Milestone 15)

**Status: NOT IMPLEMENTED. There is no code in this directory and none is planned before M15.**

## You do not need this

Nearby Share works on a LAN with no server at all:

- devices find each other with **mDNS** (`_nearby-share._tcp` in `local.`),
- the two local `share-daemon` processes relay the WebRTC offer/answer/ICE between themselves,
- the payload travels **browser to browser** over a WebRTC DataChannel.

Nothing in the transfer path touches the internet. If your devices are on the same Wi-Fi, this
directory is irrelevant to you.

## What it would be, eventually

An optional, self-hostable relay for the one case mDNS cannot cover: two devices on **different
networks**, where there is no local link over which to exchange a WebRTC offer.

Scope, fixed now so it cannot creep later:

**It would do exactly this**

- Accept WebSocket connections from `share-daemon` instances.
- Pair them into short-lived, in-memory rooms.
- Forward `signal:offer`, `signal:answer` and `signal:ice` envelopes between the two peers in a room.
- Expose `/health`.

**It would never do any of this**

- Never receive, buffer, cache, or store file, image, or text payloads. Payloads only ever traverse
  the DataChannel, end-to-end encrypted, and never reach this service.
- Never act as a TURN relay. Media relaying is a separate, explicitly opt-in concern.
- Never persist anything to disk or to a database. Rooms are in-memory and a restart forgets them.
- Never require an account, collect analytics, or log message contents.

A signaling relay is a switchboard: it connects two callers and hangs up. It is not a mailbox.

## Where the pieces are

- Roadmap entry: [`../docs/PLAN.md`](../docs/PLAN.md) — section 3, M15.
- Fallback ladder that explains when a relay would even be reached:
  [`../docs/ARCHITECTURE.md`](../docs/ARCHITECTURE.md) — section 4, LAN -> STUN -> TURN.
- Message shapes it would forward, unchanged: [`../shared/PROTOCOL.md`](../shared/PROTOCOL.md) —
  the reserved `signal:offer` / `signal:answer` / `signal:ice` types.
- Deployment placeholder: [`../docker-compose.yml`](../docker-compose.yml) — fully commented out.
