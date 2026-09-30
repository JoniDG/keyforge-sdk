// Command spotify is a KeyForge plugin that controls Spotify playback through
// the Spotify Web API: play/pause, next and previous track on keys, and
// volume on an encoder.
//
// It needs a one-time login, run by hand from a terminal:
//
//	bin/spotify-darwin login -client-id <your Spotify app's client id>
//
// Run without arguments, it is the plugin the daemon launches. See README.md
// for creating the Spotify app and binding the actions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/JoniDG/keyforge-sdk/go/plugin"
)

const (
	pluginID = "dev.jonidg.spotify"
	// version must match "version" in manifest.json.
	version = "0.1.0"

	defaultStep = 5
	// unmuteLevel is where unmuting goes when there is no level to restore,
	// e.g. the volume was set to 0 in a Spotify app.
	unmuteLevel = 30
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	if len(os.Args) > 1 && os.Args[1] == "login" {
		err = runLogin(ctx, os.Args[2:], os.Stdout)
	} else {
		err = runPlugin(ctx, logger)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("spotify stopped", "error", err)
		os.Exit(1)
	}
}

func runLogin(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(out)
	clientID := flags.String("client-id", "", "Client ID of your app in the Spotify Developer Dashboard (required)")
	port := flags.Int("port", 8888, "local port of the redirect URI registered in the app: http://127.0.0.1:<port>/callback")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("spotify.runLogin: %w", err)
	}
	if *clientID == "" {
		return errors.New("spotify.runLogin: -client-id is required")
	}
	path, err := tokenPath()
	if err != nil {
		return err
	}
	// Spotify only accepts loopback redirect URIs as an IP literal, not "localhost".
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("spotify.runLogin: %w", err)
	}
	return login(ctx, loginConfig{
		clientID:    *clientID,
		listener:    listener,
		accountsURL: accountsURL,
		client:      &http.Client{Timeout: 10 * time.Second},
		openBrowser: openBrowser,
		out:         out,
		tokenPath:   path,
	})
}

func runPlugin(ctx context.Context, logger *slog.Logger) error {
	path, err := tokenPath()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	p := &player{
		baseURL: apiURL,
		client:  client,
		tokens:  &tokens{path: path, tokenURL: accountsURL + "/api/token", client: client, now: time.Now},
	}
	vol := newVolume(p, logger)
	go vol.run(ctx)
	return plugin.Run(ctx, plugin.Config{
		Version:  version,
		Handlers: handlers(p, vol),
		Logger:   logger,
	})
}

func handlers(p *player, vol *volume) plugin.Handlers {
	return plugin.Handlers{
		"play_pause": func(ctx context.Context, _ plugin.Invocation) error {
			st, err := p.state(ctx)
			if err != nil {
				return err
			}
			if st.IsPlaying {
				return p.pause(ctx)
			}
			return p.play(ctx)
		},
		"next": func(ctx context.Context, _ plugin.Invocation) error {
			return p.next(ctx)
		},
		"previous": func(ctx context.Context, _ plugin.Invocation) error {
			return p.previous(ctx)
		},
		// Bind it to the encoder's rotate_cw, rotate_ccw and click triggers:
		// each one is its own binding, and all of them share the level.
		"volume": func(ctx context.Context, inv plugin.Invocation) error {
			if inv.Input == nil {
				return errors.New("volume only runs from an encoder")
			}
			step, err := stepParam(inv.Action.Params)
			if err != nil {
				return err
			}
			switch inv.Input.Action {
			case protocol.InputActionRotateCw:
				return vol.step(ctx, step)
			case protocol.InputActionRotateCcw:
				return vol.step(ctx, -step)
			case protocol.InputActionClick:
				return vol.toggleMute(ctx, unmuteLevel)
			default:
				return nil
			}
		},
	}
}

func stepParam(params protocol.ActionInvokedSchemaJsonDataActionParams) (int, error) {
	raw, ok := params["step"]
	if !ok {
		return defaultStep, nil
	}
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("param step must be a string, got %T", raw)
	}
	if s == "" {
		return defaultStep, nil
	}
	step, err := strconv.Atoi(s)
	if err != nil || step < 1 || step > 100 {
		return 0, fmt.Errorf("param step must be a whole number from 1 to 100, got %q", s)
	}
	return step, nil
}
