# keyforge-sdk — Node.js (placeholder)

🚧 Not started yet.

The Node.js SDK will mirror the Go SDK API once that one stabilizes. Until then, plugin authors targeting Node can talk to the daemon directly via any WebSocket library and follow the protocol spec in [`../docs/`](../docs/).

If you want to bootstrap this SDK, the API surface should match `../go/` semantically: a `Client` for external apps and a `Plugin` base for plugin authors.
