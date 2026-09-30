# spotify

A KeyForge plugin that controls Spotify through the [Spotify Web API](https://developer.spotify.com/documentation/web-api): play/pause, next and previous track, and volume on an encoder.

| Action | Bind it to | What it does |
|---|---|---|
| `play_pause` | a key or an encoder press | Pauses if something is playing, plays otherwise |
| `next` / `previous` | a key or an encoder press | Skips to the next or previous track |
| `volume` | an encoder: `rotate_cw`, `rotate_ccw` and `click` | Turning right raises the volume, turning left lowers it, pressing mutes or unmutes. The optional `step` param sets the percentage per tick (default `5`) |

> **Spotify Premium is required.** The Web API only lets Premium accounts control playback; with a free account every action logs `controlling playback requires Spotify Premium`.

It shows a few things a real plugin needs beyond [`echo`](../echo/) and [`toggle`](../toggle/):

- **Credentials without a settings UI.** A one-time `login` subcommand, run by hand, does the OAuth flow and saves a refresh token; the plugin the daemon launches reads it.
- **Work outside the handler.** Encoders fire many ticks per second, and one API request per tick would hit Spotify's rate limit. The `volume` handler only moves a target level in memory, and a goroutine sends it at most once every 150 ms.
- **Errors worth reading.** A failing action returns an error, which the SDK logs, and keyforged shows it in its output under `[dev.jonidg.spotify]`, for example `no active Spotify device: start playing in any Spotify app first`.

## 1. Create a Spotify app

Every user brings their own app, so no secret ships with the plugin:

1. In the [Spotify Developer Dashboard](https://developer.spotify.com/dashboard), create an app and tick **Web API**.
2. Add the redirect URI `http://127.0.0.1:8888/callback`. Spotify only accepts loopback redirects as an IP literal: `localhost` won't work.
3. Copy the app's **Client ID**. The plugin uses [PKCE](https://developer.spotify.com/documentation/web-api/tutorials/code-pkce-flow), so it never needs the client secret.

## 2. Build and install

Build the entrypoint the manifest declares for your OS, from this folder:

```bash
go build -o bin/spotify-darwin .   # macOS
go build -o bin/spotify-linux .    # Linux
go build -o bin/spotify.exe .      # Windows
```

Then either package it and install `spotify.keyforgeplugin` from the KeyForge app:

```bash
zip -r spotify.keyforgeplugin manifest.json bin/
```

or copy the folder into the daemon's plugins directory as described in [`echo`](../echo/README.md#try-it-by-hand-against-keyforged).

## 3. Log in once

Run the binary with `login` from a terminal (on the installed copy, or on the one you just built: both read the same token file):

```bash
bin/spotify-darwin login -client-id <your client id>
```

It opens the browser to approve access, and saves the refresh token to `KeyForge/plugin-data/dev.jonidg.spotify/token.json` in your OS config directory (`~/Library/Application Support` on macOS, `~/.config` on Linux, `%AppData%` on Windows). The file lives outside the plugin folder, so upgrading the plugin keeps you logged in. If port 8888 is taken, pass `-port <n>` and register `http://127.0.0.1:<n>/callback` in the app instead.

No restart is needed: the plugin reads the token file the first time an action runs. If Spotify revokes the token, actions log `run login again`; do that and the running plugin picks up the new token.

## 4. Bind the actions

Bind `plugin.dev.jonidg.spotify.play_pause`, `next` and `previous` to keys. For volume, create three bindings on the same encoder, one per trigger. Over the WebSocket (see [`echo`](../echo/README.md#try-it-by-hand-against-keyforged) for how to connect):

```json
{"type":"request","id":"1","method":"set_binding","params":{"binding":{"device_id":"VID_1234_PID_5678","input_id":"encoder_0","trigger":"rotate_cw","action":{"type":"plugin.dev.jonidg.spotify.volume","params":{"step":"5"}}}}}
{"type":"request","id":"2","method":"set_binding","params":{"binding":{"device_id":"VID_1234_PID_5678","input_id":"encoder_0","trigger":"rotate_ccw","action":{"type":"plugin.dev.jonidg.spotify.volume","params":{"step":"5"}}}}}
{"type":"request","id":"3","method":"set_binding","params":{"binding":{"device_id":"VID_1234_PID_5678","input_id":"encoder_0","trigger":"click","action":{"type":"plugin.dev.jonidg.spotify.volume"}}}}
```

Start playing in any Spotify app (desktop, phone, web player): the Web API controls whichever device is active, and does nothing when none is.
