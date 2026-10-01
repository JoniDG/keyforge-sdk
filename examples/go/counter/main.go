// Command counter is the plugin built in docs/tutorial-go.md: a counter for
// streams (deaths, wins, coffees). A key adds one; on an encoder, turning
// right adds one, turning left takes one away and pressing resets it. The
// count can be written to a text file that OBS shows as a text source.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
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
	counts := map[string]int{}
	return plugin.Handlers{
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
	}
}

// param returns a string param of the binding, or "" when it is not set.
func param(inv plugin.Invocation, name string) string {
	value, _ := inv.Action.Params[name].(string)
	return value
}

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
