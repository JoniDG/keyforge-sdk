# Your first KeyForge plugin in 30 minutes (Go)

This tutorial builds a **counter for streams** (deaths, wins, coffees) from an empty folder to a plugin installed in KeyForge:

- A key adds one.
- On an encoder, turning right adds one, turning left takes one away and pressing resets it.
- Optionally, it writes the count to a text file, which OBS (or any streaming tool) can show as a text source.

On the way you'll use everything a plugin usually needs: the manifest, action params, per-binding state, encoders and tests. The finished plugin lives in [`examples/go/counter`](../examples/go/counter/); if you get stuck, compare against it.

**You need:** Go 1.24+, the KeyForge app with `keyforged` running, and a device with at least one key (an encoder for step 6).

## 1. How a plugin works

A plugin is a separate program. When `keyforged` starts it (or you install the plugin), it runs the command your `manifest.json` declares for the current OS and passes it, in the `KEYFORGE_PLUGIN_INFO` environment variable, the address of a local WebSocket. The plugin connects and introduces itself; from then on, every time a key bound to one of its actions fires, the daemon sends it an `action_invoked` event. When the daemon closes the connection, the plugin exits.

The Go SDK's `plugin.Run` does all of that. You only write one **handler** per action. (Curious about the wire format, or writing a plugin in another language? See [`protocol.md`](./protocol.md).)

## 2. Scaffold the plugin

Create a module. Its path can be anything; use your own repository:

```bash
mkdir keyforge-counter && cd keyforge-counter
go mod init github.com/you/keyforge-counter
go get github.com/JoniDG/keyforge-sdk/go
```

Every plugin has a `manifest.json` at its root. It tells KeyForge who the plugin is, how to start it and which actions it offers:

```json
{
  "manifest_version": 1,
  "id": "io.github.you.counter",
  "name": "Counter",
  "version": "0.1.0",
  "protocol_version": "1",
  "author": "You",
  "description": "A counter for streams.",
  "entrypoint": {
    "darwin": { "path": "bin/counter-darwin" },
    "linux": { "path": "bin/counter-linux" },
    "windows": { "path": "bin/counter.exe" }
  },
  "actions": [{ "id": "count", "name": "Count", "inputs": ["key", "encoder"], "params": [] }]
}
```

- **`id`** is reverse-DNS, built from a domain or GitHub account you control, so it never collides with someone else's plugin without a central registry. A binding refers to this action as `plugin.io.github.you.counter.count`.
- **`entrypoint`** maps each OS to the program to run, relative to the plugin folder. The daemon knows nothing about Go (or Node, or Python): it just runs that file. Declare only the OSes you ship a binary for: KeyForge refuses to install a plugin with no entrypoint for its OS, but it can't tell that a declared binary is missing until it tries to start it.
- **`actions`** lists what users can bind. `id` is `snake_case`, `inputs` restricts the kinds of input it fits.

Now `main.go`, with one handler for the `count` action:

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/JoniDG/keyforge-sdk/go/plugin"
)

// version must match "version" in manifest.json.
const version = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := plugin.Run(ctx, plugin.Config{
		Version:  version,
		Handlers: handlers(logger),
		Logger:   logger,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("plugin stopped", "error", err)
		os.Exit(1)
	}
}

func handlers(logger *slog.Logger) plugin.Handlers {
	count := 0
	return plugin.Handlers{
		"count": func(_ context.Context, inv plugin.Invocation) error {
			count++
			logger.Info("count", "count", count)
			return nil
		},
	}
}
```

The keys of `plugin.Handlers` are the action ids from the manifest. Log to stderr: the daemon shows each line of your plugin's output prefixed with its id. A handler that returns an error gets it logged too; there's no one else to report it to, because the daemon doesn't wait for the result.

Run it by hand and it stops right away: that's expected, since only the daemon provides `KEYFORGE_PLUGIN_INFO`.

```bash
go run .
# ... level=ERROR msg="plugin stopped" error="... KEYFORGE_PLUGIN_INFO ..."
```

## 3. Build, package and install

Build one binary per OS the manifest declares, into the path it declares. Go cross-compiles, so one machine builds all three:

```bash
GOOS=darwin  GOARCH=arm64 go build -o bin/counter-darwin .   # macOS (Apple silicon; amd64 for Intel Macs)
GOOS=linux   GOARCH=amd64 go build -o bin/counter-linux .    # Linux
GOOS=windows GOARCH=amd64 go build -o bin/counter.exe .      # Windows
```

(On Windows, set the variables first: `set GOOS=linux` in cmd, `$env:GOOS="linux"` in PowerShell.) Each OS gets a single path, so pick the CPU architecture your users have. While you're only trying it out, building for your own OS is enough.

A plugin ships as a `.keyforgeplugin` file: a zip with `manifest.json` at its root (not inside a folder) plus the files it references:

```bash
zip -r counter.keyforgeplugin manifest.json bin/   # macOS, Linux
```

Windows has no `zip`, but its built-in `tar` writes zip files; it picks the format from the extension, so rename afterwards:

```bat
tar -a -c -f counter.zip manifest.json bin
ren counter.zip counter.keyforgeplugin
```

In the KeyForge app, open **Plugins**, click **Install plugin** and pick `counter.keyforgeplugin`. The app shows what it's about to install; confirm, and the plugin appears in the list once the daemon has started it.

Now bind it: in your device's bindings, every input has one row per trigger. On a key's **press** row, choose the **Count** action and save. (Binding its **release** row too would count twice per keystroke.) Press the key a few times and watch `keyforged`'s output:

```
[io.github.you.counter] time=... level=INFO msg=count count=1
[io.github.you.counter] time=... level=INFO msg=count count=2
```

**Iterating:** after changing the code, rebuild, re-zip and install again. Installing a plugin whose id is already installed upgrades it, and your bindings keep working. Bump `version` in both the manifest and `main.go` when you release.

## 4. Per-binding state

Bind a second key to **Count**. Both keys now move the same number, because there's one `count` variable for the whole plugin. Usually each key should be its own counter.

Every invocation carries `inv.Context`, an id for the binding that fired: different for each binding, and the same every time that binding fires (even across restarts). It's opaque (don't parse it) and made to be a map key:

```go
func handlers(logger *slog.Logger) plugin.Handlers {
	// Handlers run one at a time, so the map needs no lock.
	counts := map[string]int{}
	return plugin.Handlers{
		"count": func(_ context.Context, inv plugin.Invocation) error {
			counts[inv.Context]++
			logger.Info("count", "count", counts[inv.Context])
			return nil
		},
	}
}
```

No mutex needed: the SDK runs your handlers **one at a time**, in the order the keys were pressed. The flip side is that a slow handler holds up the next ones; if an action has to wait on something (a network call, a long computation), start a goroutine from the handler, and protect whatever it shares.

## 5. Params

Let users name each counter and, optionally, write it to a file. Declare the params in the manifest; the app renders a field for each one when the action is picked:

```json
"params": [
  { "name": "label", "label": "Label", "type": "string", "required": false, "placeholder": "Deaths" },
  { "name": "file", "label": "Write to file", "type": "string", "required": false, "placeholder": "/Users/me/obs/deaths.txt" }
]
```

Params are always strings for now. Their values arrive in `inv.Action.Params`, a map holding only the params the user filled in, so read them with a fallback:

```go
// param returns a string param of the binding, or "" when it is not set.
func param(inv plugin.Invocation, name string) string {
	value, _ := inv.Action.Params[name].(string)
	return value
}
```

Writing the file is plain Go. Insist on an absolute path: the daemon starts the plugin inside its install folder, so a relative path would land there, and installing a new version replaces that folder. When something is wrong, return an error that tells the user how to fix it, since it ends up in the logs:

```go
func writeCount(file, label string, count int) error {
	// The daemon starts the plugin inside its install folder, so a relative
	// path would land there, and installing a new version replaces it.
	if !filepath.IsAbs(file) {
		return fmt.Errorf("counter.writeCount: param file must be an absolute path, got %q", file)
	}
	text := fmt.Sprint(count)
	if label != "" {
		text = fmt.Sprintf("%s: %d", label, count)
	}
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		return fmt.Errorf("counter.writeCount: %w", err)
	}
	return nil
}
```

It needs the `fmt` and `path/filepath` imports. Call it from the handler when `file` is set:

```go
counts[inv.Context]++
logger.Info("count", "count", counts[inv.Context])
if file := param(inv, "file"); file != "" {
	return writeCount(file, param(inv, "label"), counts[inv.Context])
}
return nil
```

Rebuild, reinstall, set **Write to file** on a binding and add a *Text (read from file)* source in OBS pointing to the same file.

## 6. Encoders

`inv.Input` is the hardware event behind the invocation: which input, of what kind, and what happened to it (`press`, `release`, `rotate_cw`, `rotate_ccw`, `click`). It's `nil` when the action was run from the app instead of by hardware, so always check it. Turn it into the next count:

```go
// next returns the count after input. A nil input means the action was run
// from the KeyForge app rather than by hardware.
func next(count int, input *protocol.InputEvent) int {
	if input == nil {
		return count + 1
	}
	switch input.Action {
	case protocol.InputActionRotateCcw:
		return max(count-1, 0)
	case protocol.InputActionClick:
		return 0
	default:
		return count + 1
	}
}
```

(`protocol` is `github.com/JoniDG/keyforge-protocol/go/protocol`, which the SDK already depends on; run `go mod tidy` after adding the import.)

There's a catch. A binding is one input **and one trigger**, so an encoder takes three bindings: turn right, turn left, press. Three bindings means three contexts, and with the step 4 code they would be three separate counters. The fix is to give users a way to say "these bindings are the same counter". The label already does that: bindings that share a label share the count. This is the final handler:

```go
"count": func(_ context.Context, inv plugin.Invocation) error {
	label := param(inv, "label")
	// Each binding has its own context, and an encoder needs one binding
	// per trigger. Bindings that share a label share the count, which
	// is how an encoder's three bindings drive one counter.
	key := inv.Context
	if label != "" {
		key = "label:" + label
	}
	counts[key] = next(counts[key], inv.Input)

	logger.Info("count", "label", label, "count", counts[key])
	if file := param(inv, "file"); file != "" {
		return writeCount(file, label, counts[key])
	}
	return nil
},
```

Bind **Count** with the label `Deaths` to the encoder's three triggers (and, if you like, to a key too), and they all drive the same counter.

## 7. Test it

Handlers are plain functions, so you can test them without a daemon: build an `Invocation` by hand and call the handler. Two tests worth having in every plugin:

- **The manifest matches the code**: every action in `manifest.json` has a handler and the versions agree. It catches the most common packaging mistake.
- **The handlers' behaviour**, including the error paths.

```go
func TestManifestMatchesPlugin(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("manifest.json")
	require.NoError(t, err)
	var manifest protocol.PluginManifest
	require.NoError(t, json.Unmarshal(raw, &manifest))

	assert.Equal(t, version, manifest.Version)
	h := handlers(slog.New(slog.DiscardHandler))
	require.Len(t, h, len(manifest.Actions))
	for _, action := range manifest.Actions {
		assert.Contains(t, h, action.Id)
	}
}

func TestEncoderSharesCounterByLabel(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "deaths.txt")
	count := handlers(slog.New(slog.DiscardHandler))["count"]
	fire := func(context string, action protocol.InputAction) {
		inv := plugin.Invocation{
			Context: context,
			Action: protocol.ActionInvokedSchemaJsonDataAction{
				Id:     "count",
				Params: protocol.ActionInvokedSchemaJsonDataActionParams{"label": "Deaths", "file": file},
			},
			Input: &protocol.InputEvent{Kind: protocol.InputKindEncoder, InputId: "encoder_0", Action: action},
		}
		require.NoError(t, count(t.Context(), inv))
	}

	fire("cw", protocol.InputActionRotateCw)
	fire("cw", protocol.InputActionRotateCw)
	fire("ccw", protocol.InputActionRotateCcw)

	got, err := os.ReadFile(file) // #nosec G304 -- reads the file the test just had the plugin write
	require.NoError(t, err)
	assert.Equal(t, "Deaths: 1", string(got))
}
```

They go in `main_test.go`, in `package main`, and import `encoding/json`, `log/slog`, `os`, `path/filepath`, `testing`, the `protocol` and `plugin` packages, and [testify](https://github.com/stretchr/testify)'s `assert` and `require` (`go get github.com/stretchr/testify`). If you run [gosec](https://github.com/securego/gosec), it flags reading a file from a variable path; the `#nosec` comment says why that's fine here. Both tests, and a few more, are in [`examples/go/counter/main_test.go`](../examples/go/counter/main_test.go).

## 8. When something goes wrong

| Symptom | Likely cause |
|---|---|
| The app refuses the package | `manifest.json` isn't at the root of the zip, a required field is missing, or there's no `entrypoint` for your OS. |
| Installed, but the plugin never starts | The entrypoint path doesn't match the built binary (name, or a missing `bin/`), or the file isn't executable. |
| Starts, then exits right away | Look at `keyforged`'s output for its error. A panic in a goroutine you started kills the plugin; panics inside handlers are recovered and logged. |
| Pressing the key does nothing | The binding points at another action, or the handler failed: look for `action failed` in the logs, with your error. |
| `no handler for action` in the logs | The action id in the manifest and the key in `plugin.Handlers` differ. The manifest test from step 7 catches this. |

## Where to go next

- [`examples/go/spotify`](../examples/go/spotify/): a real-world plugin. It logs in to an online service (OAuth, with the token stored outside the plugin folder so upgrades keep it) and moves slow work out of the handler into a goroutine.
- [`examples/go/echo`](../examples/go/echo/): how to run a plugin without packaging it, and drive the daemon by hand over its WebSocket.
- [`protocol.md`](./protocol.md): the wire protocol, to write a plugin in any language.
- The [`plugin` package docs](../go/plugin/doc.go): exactly how handlers are scheduled, queued and cancelled.
