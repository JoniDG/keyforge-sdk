# keyforge-sdk

> Plugin and client SDK for [KeyForge](https://github.com/JoniDG/keyforge).

`keyforge-sdk` is a polyglot toolkit for writing **plugins** and **external clients** that talk to the KeyForge daemon. It contains:

- A language-neutral **protocol specification** (in [`docs/`](./docs/)).
- Convenience helpers in **Go** (active).
- Reserved slots for **Node.js** and **Python** SDKs (coming later).

You don't need this SDK to write a plugin — any language with WebSocket and JSON support works. The SDK just makes it less repetitive.

## Status

🚧 **Pre-alpha.** Only the Go SDK has scaffold; the protocol spec is being authored.

## Languages

| Language | Status | Path |
|---|---|---|
| Go | 🟡 Scaffolded, no API yet | [`go/`](./go/) |
| Node.js | ⚪ Planned | [`node/`](./node/) |
| Python | ⚪ Planned | [`python/`](./python/) |

## License

[Apache 2.0](./LICENSE) — Copyright (c) 2026 Jonathan Daniel Gomez.
