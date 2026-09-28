// Command echo is the smallest useful KeyForge plugin: its "echo" action logs
// which input fired it and the binding's context, which makes it handy to
// check that the daemon launches a plugin and routes a key to it. See
// README.md for how to try it against keyforged by hand.
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
	return plugin.Handlers{
		"echo": func(_ context.Context, inv plugin.Invocation) error {
			inputID := "none" // not fired by hardware, e.g. a test run from the KeyForge app
			if inv.Input != nil {
				inputID = inv.Input.InputId
			}
			logger.Info("echo", "input_id", inputID, "context", inv.Context)
			return nil
		},
	}
}
