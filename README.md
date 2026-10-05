# keyforge-sdk

> Plugin and client SDK for [KeyForge](https://github.com/JoniDG/keyforge).

`keyforge-sdk` is a polyglot toolkit for writing **plugins** and **external clients** that talk to the KeyForge daemon. It contains:

- A language-neutral **protocol specification** (in [`docs/`](./docs/)).
- Convenience helpers in **Go** and **Node.js**, with the same behavior.
- A reserved slot for a **Python** SDK (coming later).

You don't need this SDK to write a plugin — any language with WebSocket and JSON support works. The SDK just makes it less repetitive.

## Status

🚧 **Pre-alpha.** The Go and Node.js SDKs can run a plugin end to end; the API may still change before v1.

## Languages

| Language | Status | Path |
|---|---|---|
| Go | 🟡 `plugin` (run a plugin) and `client` (connect + hello + events) | [`go/`](./go/) |
| Node.js | 🟡 `plugin` and `client`, same as Go (npm: `@jdg-keyforge/sdk`) | [`node/`](./node/) |
| Python | ⚪ Planned | [`python/`](./python/) |

## Writing a plugin in Go

New to KeyForge plugins? Start with the tutorial: [Your first KeyForge plugin in 30 minutes](./docs/tutorial-go.md).

```bash
go get github.com/JoniDG/keyforge-sdk/go
```

Register one handler per action declared in your `manifest.json`; `plugin.Run` reads the launch info the daemon passes, connects, dispatches each invocation and returns when the daemon stops the plugin:

```go
err := plugin.Run(ctx, plugin.Config{
	Version: "0.1.0", // same as manifest.json
	Handlers: plugin.Handlers{
		"play_pause": func(ctx context.Context, inv plugin.Invocation) error {
			// inv.Context: binding instance, inv.Action.Params: user settings,
			// inv.Input: hardware event (nil when fired from the app).
			return togglePlayback()
		},
	},
})
```

Handlers run one at a time, in firing order. See [`examples/go/counter`](./examples/go/counter/) for the plugin the tutorial builds, [`examples/go/toggle`](./examples/go/toggle/) for a minimal one, [`examples/go/echo`](./examples/go/echo/) for how to try a plugin by hand against the daemon, [`examples/go/spotify`](./examples/go/spotify/) for a real-world plugin (OAuth credentials, work outside the handler), and [`docs/protocol.md`](./docs/protocol.md) for what happens on the wire.

## Writing a plugin in Node.js

```bash
npm install @jdg-keyforge/sdk
```

```ts
import { run } from '@jdg-keyforge/sdk/plugin';

await run({
  version: '0.1.0', // same as manifest.json
  handlers: {
    play_pause: async (inv) => togglePlayback(),
  },
});
```

Same model as Go: handlers run one at a time, in firing order. See [`node/`](./node/) for the details and packaging, and [`examples/node/counter`](./examples/node/counter/) for the counter plugin ported to Node.

## License

[Apache 2.0](./LICENSE) — Copyright (c) 2026 Jonathan Daniel Gomez.
