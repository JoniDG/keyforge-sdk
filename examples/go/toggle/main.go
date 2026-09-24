// Command toggle is a minimal KeyForge plugin. Its "toggle" action flips an
// on/off state kept per binding: bind it to two keys and each key has its own
// state, because the daemon sends a different Invocation.Context for each.
//
// Build it into the path the manifest declares for your OS, e.g.:
//
//	go build -o bin/toggle-darwin .
//
// then zip manifest.json and bin/ into toggle.keyforgeplugin and install it
// from the KeyForge app. Run by hand (without the daemon), it exits with an
// error saying KEYFORGE_PLUGIN_INFO is not set.
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
	// Handlers run one at a time, so the map needs no lock.
	on := map[string]bool{}
	return plugin.Handlers{
		"toggle": func(_ context.Context, inv plugin.Invocation) error {
			on[inv.Context] = !on[inv.Context]
			source := "app" // no hardware input: fired from the KeyForge app
			if inv.Input != nil {
				source = inv.Input.InputId
			}
			logger.Info("toggled", "context", inv.Context, "on", on[inv.Context], "source", source)
			return nil
		},
	}
}
