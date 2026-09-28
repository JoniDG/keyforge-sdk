package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/JoniDG/keyforge-sdk/go/client"
)

// LaunchInfoEnv is the environment variable the daemon sets, when it spawns a
// plugin, to a JSON protocol.PluginLaunchInfo.
const LaunchInfoEnv = "KEYFORGE_PLUGIN_INFO"

// queueSize bounds how many invocations wait while a handler runs. When it
// fills up, new invocations are dropped: pausing the read instead would stall
// the daemon's writes until it closes the connection, and the daemon doesn't
// restart plugins.
const queueSize = 128

// Invocation is one firing of a plugin action: Context identifies the binding
// instance (opaque, stable; use it as a key for per-instance state), Action
// holds the action id and its params, and Input is the hardware event that
// fired it, or nil when it wasn't fired by hardware (e.g. a test run from the
// GUI).
type Invocation = protocol.ActionInvokedSchemaJsonData

// Handler runs an action. Its ctx is cancelled when the connection to the
// daemon closes. A returned error is logged; the daemon doesn't wait for the
// result. A panic is recovered and logged with its stack, and the plugin goes
// on with the next invocation; panics in goroutines the handler starts are
// not recovered and crash the plugin.
type Handler func(ctx context.Context, inv Invocation) error

// Handlers maps an action id, as declared in the manifest (e.g. "play_pause",
// without the "plugin.<id>." prefix), to its handler.
type Handlers map[string]Handler

// Config configures Run.
type Config struct {
	// Version is the plugin version sent in hello. Required; keep it in sync
	// with the manifest.
	Version string
	// Handlers holds one handler per action the manifest declares.
	Handlers Handlers
	// Logger receives handler errors and ignored frames. Defaults to a text
	// logger on stderr.
	Logger *slog.Logger
}

// Run connects the plugin to the daemon and dispatches actions to
// cfg.Handlers until the daemon closes the connection, which returns nil, or
// ctx is cancelled, which returns ctx.Err(). Either way, invocations still
// queued at that point are dropped. It fails right away if
// KEYFORGE_PLUGIN_INFO is missing or invalid, which means the process wasn't
// launched by the daemon, or if the daemon rejects the hello.
func Run(ctx context.Context, cfg Config) error {
	return run(ctx, cfg, os.LookupEnv)
}

func run(ctx context.Context, cfg Config, lookupEnv func(string) (string, bool)) error {
	info, err := launchInfo(lookupEnv)
	if err != nil {
		return err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	logger = logger.With("plugin", string(info.PluginId))

	conn, _, err := client.Dial(ctx, info.WsUrl, protocol.HelloSchemaJsonParams{
		ProtocolVersion: info.ProtocolVersion,
		Client:          protocol.PeerInfo{Name: string(info.PluginId), Version: cfg.Version},
	})
	if err != nil {
		return fmt.Errorf("plugin.Run: %w", err)
	}
	defer conn.Close() //nolint:errcheck // the connection is usually already closed by the daemon

	return serve(ctx, conn, cfg.Handlers, logger)
}

func launchInfo(lookupEnv func(string) (string, bool)) (protocol.PluginLaunchInfo, error) {
	var info protocol.PluginLaunchInfo
	raw, ok := lookupEnv(LaunchInfoEnv)
	if !ok || raw == "" {
		return info, fmt.Errorf("plugin.Run: %s is not set; plugins must be launched by the KeyForge daemon", LaunchInfoEnv)
	}
	// The value is never included in errors: ws_url carries the auth token.
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return info, fmt.Errorf("plugin.Run: invalid %s: %w", LaunchInfoEnv, err)
	}
	return info, nil
}

// serve reads events in one goroutine and runs handlers in this one, so a
// closed connection is noticed (and the running handler's ctx cancelled)
// even while a handler is busy.
func serve(parent context.Context, conn *client.Conn, handlers Handlers, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	queue := make(chan Invocation, queueSize)
	var readErr error
	go func() {
		defer close(queue)
		defer cancel()
		readErr = read(conn, queue, logger)
	}()

	// Cancelling a blocked read would drop the connection without a close
	// frame, so on cancellation the connection is closed normally instead,
	// which also unblocks the reader.
	stop := context.AfterFunc(parent, func() { _ = conn.Close() })
	defer stop()

	for inv := range queue {
		if ctx.Err() != nil {
			continue
		}
		dispatch(ctx, handlers, inv, logger)
	}

	switch {
	case parent.Err() != nil:
		return parent.Err()
	case errors.Is(readErr, client.ErrClosed):
		return nil
	default:
		return fmt.Errorf("plugin.Run: %w", readErr)
	}
}

// read queues action_invoked events until the connection is gone, and
// returns the error that ended it. It never blocks on a full queue (it drops
// the invocation instead), so a closed connection is always noticed.
func read(conn *client.Conn, queue chan<- Invocation, logger *slog.Logger) error {
	for {
		ev, err := conn.ReadEvent(context.Background())
		if errors.Is(err, client.ErrInvalidFrame) {
			logger.Warn("ignoring invalid frame", "error", err)
			continue
		}
		if err != nil {
			return err
		}
		if ev.Name != "action_invoked" {
			logger.Debug("ignoring event", "event", ev.Name)
			continue
		}
		var inv Invocation
		if err := json.Unmarshal(ev.Data, &inv); err != nil {
			logger.Warn("ignoring invalid action_invoked", "error", err)
			continue
		}
		select {
		case queue <- inv:
		default:
			logger.Warn("dropping action_invoked: handlers are not keeping up", "action", inv.Action.Id, "context", inv.Context)
		}
	}
}

func dispatch(ctx context.Context, handlers Handlers, inv Invocation, logger *slog.Logger) {
	handler, ok := handlers[inv.Action.Id]
	if !ok {
		logger.Warn("no handler for action", "action", inv.Action.Id)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Error("action panicked", "action", inv.Action.Id, "context", inv.Context, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := handler(ctx, inv); err != nil {
		logger.Error("action failed", "action", inv.Action.Id, "context", inv.Context, "error", err)
	}
}
