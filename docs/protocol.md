# KeyForge wire protocol for plugins

This is the language-neutral view of what a plugin has to do to talk to the KeyForge daemon (`keyforged`). You don't need an SDK: any language with WebSocket and JSON support can implement it. The schemas and their generated Go and TypeScript types live in [`keyforge-protocol`](https://github.com/JoniDG/keyforge-protocol), which is the source of truth; this page summarizes them.

## Frames

Every WebSocket message is a JSON text frame of one of three types, discriminated by `type`: `request`, `response` (with an `ok: true` and an `ok: false` variant) and `event`:

```json
{ "type": "request",  "id": "<string>", "method": "hello", "params": { } }
{ "type": "response", "id": "<string>", "ok": true,  "data": { } }
{ "type": "response", "id": "<string>", "ok": false, "error": { "code": "FORBIDDEN", "message": "..." } }
{ "type": "event",    "name": "action_invoked", "data": { } }
```

- Requests go client → daemon; the `id` is a string the client generates, echoed in the response.
- Events go daemon → client and are not correlated to any request.
- Method and event names are `snake_case`.

## Plugin lifecycle

1. **Launch.** The daemon spawns the command declared in the plugin's `manifest.json` for the current OS and sets the `KEYFORGE_PLUGIN_INFO` environment variable:

   ```json
   { "plugin_id": "dev.jonidg.spotify", "ws_url": "ws://127.0.0.1:53817/ws?token=3f9c2a7e1b", "protocol_version": "1" }
   ```

   `ws_url` carries a per-plugin auth token. Treat it as a secret: don't log it. If the variable is missing, the process wasn't launched by the daemon and should exit with an error.

2. **Hello.** Connect to `ws_url` and send a `hello` request, with `client.name` set to the plugin id:

   ```json
   { "type": "request", "id": "1", "method": "hello", "params": { "protocol_version": "1", "client": { "name": "dev.jonidg.spotify", "version": "0.1.0" } } }
   ```

   The daemon answers `ok: true` with its own name, version and `protocol_version`. Otherwise it answers with an error and closes the connection: `FORBIDDEN` when the name doesn't match the token, `UNSUPPORTED_PROTOCOL_VERSION` when it can't speak that version.

3. **Actions.** Each time a binding (or a macro step) for `plugin.<plugin_id>.<action_id>` fires, the daemon sends:

   ```json
   {
     "type": "event",
     "name": "action_invoked",
     "data": {
       "context": "b1f0c9",
       "action": { "id": "volume", "params": { "step": "5" } },
       "input": { "device_id": "VID_1234_PID_5678", "input_id": "encoder_0", "kind": "encoder", "action": "rotate_cw", "timestamp_ms": 1700000000000 }
     }
   }
   ```

   - `context` identifies the binding instance. It's opaque and stable, so use it as the key for per-instance state (e.g. a toggle bound to two keys).
   - `action.params` is always present (`{}` when the binding has none).
   - `input` is absent when the action wasn't fired by hardware (e.g. a test run from the app).
   - Delivery is fire-and-forget: the daemon doesn't wait for, or learn about, the result.

4. **Exit.** When the daemon closes the connection, the plugin must exit. That's how the daemon stops plugins on shutdown or uninstall. Don't reconnect.

A plugin connection may only call `hello` and only receives its own `action_invoked` events; any other method is answered with `FORBIDDEN`.

## Packaging

A plugin ships as a `.keyforgeplugin` file: a zip with `manifest.json` at its root plus every file the manifest references. See the [manifest reference](https://github.com/JoniDG/keyforge-protocol#plugin-manifest-and-package-format) and the example in [`examples/go/toggle`](../examples/go/toggle/). For a step-by-step walkthrough with the Go SDK, see the [plugin author tutorial](./tutorial-go.md).
