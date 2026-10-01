# echo

The smallest useful KeyForge plugin. Its `echo` action logs the `input_id` of the key that fired it and the binding's `context`, so it's a quick way to check that the daemon launches a plugin and routes key presses to it.

```
[dev.jonidg.echo] time=... level=INFO msg=echo input_id=key_0x68 context=...
```

`input_id=none` would mean the action wasn't fired by hardware (e.g. a test run from the KeyForge app).

## Try it by hand against keyforged

These steps use a POSIX shell (on Windows, use WSL or translate the commands) and skip the `.keyforgeplugin` package and the app: they drop the plugin folder straight into the daemon's plugins directory and create the binding over the WebSocket.

**1. Build the entrypoint** the manifest declares for your OS, from this folder:

```bash
go build -o bin/echo-darwin .   # macOS
go build -o bin/echo-linux .    # Linux
go build -o bin/echo.exe .      # Windows
```

**2. Copy the folder** into the plugins directory, under the plugin id. The folder name must match the manifest `id`, or the daemon skips it:

| OS | Plugins directory |
|---|---|
| macOS | `~/Library/Application Support/KeyForge/plugins/` |
| Linux | `~/.config/KeyForge/plugins/` (or `$XDG_CONFIG_HOME/KeyForge/plugins/`) |
| Windows | `%AppData%\KeyForge\plugins\` |

```bash
# macOS; adjust CONFIG for your OS. Step 4 reuses $CONFIG, so stay in this shell.
CONFIG="$HOME/Library/Application Support/KeyForge"
mkdir -p "$CONFIG/plugins"
cp -R . "$CONFIG/plugins/dev.jonidg.echo"
```

Only `manifest.json` and `bin/` are needed; the Go sources are ignored.

**3. (Re)start keyforged.** It discovers plugins at startup, launches `echo` and prefixes the plugin's log lines with `[dev.jonidg.echo]` in its own output.

**4. Bind a key to `plugin.dev.jonidg.echo.echo`.** The app lists it as the **Echo** action; to skip the app as well, send `set_binding` yourself with [websocat](https://github.com/vi/websocat) and [jq](https://jqlang.org) (`brew install websocat jq`), using the port and token the daemon writes to `runtime.json`:

```bash
PORT=$(jq -r .port "$CONFIG/runtime.json")
TOKEN=$(jq -r .token "$CONFIG/runtime.json")
websocat "ws://127.0.0.1:$PORT/ws?token=$TOKEN"
```

Then paste these requests one line at a time. `list_devices` gives you the `device_id` and the key `input_id`s to use in `set_binding`:

```json
{"type":"request","id":"1","method":"hello","params":{"protocol_version":"1","client":{"name":"manual","version":"0.0.0"}}}
{"type":"request","id":"2","method":"list_devices","params":{}}
{"type":"request","id":"3","method":"set_binding","params":{"binding":{"device_id":"VID_1234_PID_5678","input_id":"key_0x68","trigger":"press","action":{"type":"plugin.dev.jonidg.echo.echo"}}}}
```

**5. Press the key.** keyforged's output shows the `echo` line with that key's `input_id`. Bind a second key and it logs a different `context`: that's the value a plugin uses to keep per-binding state (see [`toggle`](../toggle/)).
