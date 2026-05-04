# KeyForge wire protocol — overview

> **Status:** 🚧 Draft. Subject to change while we get to v1.

The KeyForge daemon (`keyforged`) exposes a **local WebSocket** that GUIs and plugins use to interact with the engine. All payloads are JSON and conform to the schemas in [`keyforge-protocol`](../../keyforge-protocol/).

## Endpoints

- WebSocket URL (default): `ws://127.0.0.1:<PORT>/ws` — port published in the daemon's runtime info file. (Exact mechanism TBD.)
- All clients (GUI and plugins) connect to the same endpoint.

## Message shape

Every message has a `msg` discriminator at the top level, plus a `payload`:

```json
{
  "msg": "<message_id>",
  "id": "<correlation_id, when applicable>",
  "payload": { /* typed body, schema in keyforge-protocol */ }
}
```

Direction prefixes in schema filenames:
- `c2s_*` — Client → Server (GUI or plugin to daemon).
- `s2c_*` — Server → Client (daemon to GUI or plugin).

## Roles

A connected client identifies itself as either:
- **GUI** — full configuration access (TBD: auth model).
- **Plugin** — limited to its own actions and to events for inputs assigned to those actions.

Role negotiation happens on the first message after the WebSocket handshake. (Exact registration message: TBD.)

## TBD (open design)

- Concrete handshake message (`c2s_hello`).
- Subscription model: do plugins subscribe to events explicitly or does the daemon push only what's relevant?
- Manifest format for plugins (`manifest.json`).
- Plugin packaging (`.keyforgePlugin` zip layout).
- Auth model for GUI vs plugins.

This document grows as the daemon implementation lands.
