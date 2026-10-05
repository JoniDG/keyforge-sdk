# @jdg-keyforge/sdk

Node.js SDK for [KeyForge](https://github.com/JoniDG/keyforge) plugins and clients. It mirrors the [Go SDK](../go/): same modules, same behavior.

- **`@jdg-keyforge/sdk/plugin`** runs a plugin: you write one handler per action, it handles the launch info, the connection, dispatch and shutdown.
- **`@jdg-keyforge/sdk/client`** is the connection underneath: connect, hello handshake, read events.

Requires Node.js 22.4 or later (it uses the built-in `WebSocket`). It has no runtime dependencies; the protocol types come from [`@jdg-keyforge/protocol`](https://www.npmjs.com/package/@jdg-keyforge/protocol).

```bash
npm install @jdg-keyforge/sdk
```

## Writing a plugin

Register one handler per action declared in your `manifest.json`. `run` reads the launch info the daemon passes in `KEYFORGE_PLUGIN_INFO`, connects, dispatches each invocation and resolves when the daemon stops the plugin:

```ts
import { run } from '@jdg-keyforge/sdk/plugin';

await run({
  version: '0.1.0', // same as manifest.json
  handlers: {
    play_pause: async (inv, signal) => {
      // inv.context: binding instance, inv.action.params: user settings,
      // inv.input: hardware event (undefined when fired from the app).
      await togglePlayback();
    },
  },
});
```

- Handlers run **one at a time, in firing order**, even when they are async, so state kept per `inv.context` needs no coordination. For slow work, start it without awaiting it: up to 128 invocations wait behind a running handler, and later ones are dropped with a warning.
- A handler that throws or rejects is logged and the plugin keeps running.
- `signal` is aborted when the plugin is stopping. Pass your own `signal` to `run` (e.g. aborted on `SIGTERM`) to stop it yourself; `run` then closes the connection normally and rejects with the abort reason.
- Logs go to stderr as `level=INFO msg=...` lines (`textLogger()`); pass any `{ debug, info, warn, error }` object as `logger` to replace it.

See [`examples/node/counter`](../examples/node/counter/) for a complete plugin with tests.

### Packaging

The daemon runs the manifest `entrypoint` with the plugin folder as working directory and knows nothing about Node, so point it at `node` and your built entry file:

```json
"entrypoint": {
  "darwin": { "path": "node", "args": ["dist/main.js"] },
  "linux": { "path": "node", "args": ["dist/main.js"] },
  "windows": { "path": "node", "args": ["dist/main.js"] }
}
```

`node` is looked up on the daemon's `PATH`. The `.keyforgeplugin` zip must contain everything the entry file imports: the built `dist/` plus a production `node_modules/` (`npm ci --omit=dev`), or a single bundled file.

## Using the client directly

```ts
import { ClosedError, dial } from '@jdg-keyforge/sdk/client';

const { conn, result } = await dial(wsUrl, {
  protocol_version: '1',
  client: { name: 'my-tool', version: '1.0.0' },
});
for (;;) {
  try {
    const event = await conn.readEvent();
    console.log(event.name, event.data);
  } catch (err) {
    if (err instanceof ClosedError) break; // the daemon closed normally
    throw err;
  }
}
```

Errors: `ServerError` (the daemon rejected the request; `detail` has its code), `ClosedError` (normal close), `InvalidFrameError` (a frame that doesn't match the protocol; the connection is still usable). Frames larger than 32 KiB close the connection, like in the Go SDK. A failed `dial` never includes the URL's auth token in its message.

## Development

```bash
npm ci
npm test               # vitest with coverage (gate: 95%)
npm run lint           # ESLint + Prettier
npm run lint:examples  # the same config over examples/node/*
npm run build          # compile to dist/
```

## License

[Apache 2.0](./LICENSE) — Copyright (c) 2026 Jonathan Daniel Gomez.
