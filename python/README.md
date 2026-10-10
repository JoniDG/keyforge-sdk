# jdg-keyforge-sdk

Python SDK for [KeyForge](https://github.com/JoniDG/keyforge) plugins and clients. It mirrors the [Go SDK](../go/) and the [Node.js SDK](../node/): same modules, same behavior.

- **`keyforge_sdk.plugin`** runs a plugin: you write one handler per action, it handles the launch info, the connection, dispatch and shutdown.
- **`keyforge_sdk.client`** is the connection underneath: connect, hello handshake, read events.

Requires Python 3.15 or later. It is built on asyncio, its only runtime dependency is [`websockets`](https://websockets.readthedocs.io), and the protocol types come from [`jdg-keyforge-protocol`](https://pypi.org/project/jdg-keyforge-protocol/) (`TypedDict`s: they type the frames but don't validate them at runtime).

```bash
pip install jdg-keyforge-sdk   # or: uv add jdg-keyforge-sdk
```

## Writing a plugin

Register one async handler per action declared in your `manifest.json`. `run` reads the launch info the daemon passes in `KEYFORGE_PLUGIN_INFO`, connects, dispatches each invocation and returns when the daemon stops the plugin:

```python
import asyncio

from keyforge_sdk.plugin import Invocation, run


async def play_pause(inv: Invocation) -> None:
    # inv["context"]: binding instance, inv["action"]["params"]: user settings,
    # inv.get("input"): hardware event (absent when fired from the app).
    await toggle_playback()


asyncio.run(
    run(version="0.1.0", handlers={"play_pause": play_pause})
)  # same version as manifest.json
```

- Handlers run **one at a time, in firing order**, so state kept per `inv["context"]` needs no locks. For slow work, start a task (`asyncio.create_task`) instead of awaiting it: up to 128 invocations wait behind a running handler, and later ones are dropped with a warning. Blocking calls belong in `asyncio.to_thread`, or they stall the connection too.
- A task a handler starts is yours: keep a reference to it (the event loop holds only a weak one) and know that it isn't cancelled when the plugin stops.
- A handler that raises is logged with its traceback and the plugin keeps running.
- When the plugin is stopping, the running handler's task is cancelled: catch `asyncio.CancelledError` to clean up. Cancelling `run` yourself (Ctrl+C under `asyncio.run`, or `task.cancel()` on `SIGTERM`) closes the connection normally and re-raises the cancellation.
- `run` raises `RuntimeError` when the process wasn't launched by the daemon and `ConnectionError` when the connection fails, the daemon rejects the hello (the `ServerError` is the `__cause__`) or the connection drops.
- Logs go to stderr as `level=INFO msg=... key=value` lines (`text_logger()`, with `LogfmtFormatter`); pass any `logging.Logger` as `logger` to replace it. Attributes travel in `extra=`.

See [`examples/python/counter`](../examples/python/counter/) for a complete plugin with tests.

### Packaging

The daemon runs the manifest `entrypoint` with the plugin folder as working directory and knows nothing about Python, so point it at a Python 3.15 interpreter and your entry file:

```json
"entrypoint": {
  "darwin": { "path": "python3.15", "args": ["main.py"] },
  "linux": { "path": "python3.15", "args": ["main.py"] },
  "windows": { "path": "py", "args": ["-3.15", "main.py"] }
}
```

The interpreter is looked up on the daemon's `PATH` (`py` is the Python launcher for Windows). A user installs your plugin, not a virtual environment, so the plugin carries its own dependencies in a `vendor/` folder, and the entry file puts that folder first on `sys.path` before importing anything else:

```python
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "vendor"))
```

From a uv project, fill `vendor/` with the locked runtime dependencies:

```bash
uv export --no-dev --no-emit-project --no-hashes --frozen | uv pip install --target vendor -r -
```

The `.keyforgeplugin` zip then holds `manifest.json`, your `.py` files and `vendor/`. A `vendor/` built on one OS works on the others as long as every dependency is pure Python or, like `websockets`, falls back to pure Python when its compiled part doesn't load.

## Using the client directly

```python
from keyforge_sdk.client import ClosedError, dial

conn, result = await dial(
    ws_url, {"protocol_version": "1", "client": {"name": "my-tool", "version": "1.0.0"}}
)
try:
    while True:
        event = await conn.read_event()
        print(event.name, event.data)
except ClosedError:
    pass  # the daemon closed normally
finally:
    await conn.close()
```

Errors: `ServerError` (the daemon rejected the request; `detail` has its code), `ClosedError` (normal close), `InvalidFrameError` (a frame that doesn't match the protocol; the connection is still usable), `ConnectionError` (the connection failed or dropped). Cancelling a call (`asyncio.timeout`, `task.cancel()`) is safe: a cancelled `read_event` leaves the connection open. Frames larger than 32 KiB close the connection, like in the Go SDK. A failed `dial` never includes the URL's auth token in its message.

## Development

```bash
uv sync
uv run pytest            # tests with coverage (gate: 95%)
uv run ruff check .      # lint
uv run ruff format .     # format
uv run mypy src tests    # strict type check (Any is banned)
```

The examples in `examples/python/*` share this ruff config through `extend` and run the same commands from their own folder.

## License

[Apache 2.0](./LICENSE) — Copyright (c) 2026 Jonathan Daniel Gomez.
