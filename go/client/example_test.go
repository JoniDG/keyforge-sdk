package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/JoniDG/keyforge-sdk/go/client"
)

// Connects to the daemon and prints every action_invoked event until the
// daemon closes the connection. Plugins usually rely on the plugin package,
// which does this (and reads the launch info) for them.
func ExampleDial() {
	ctx := context.Background()
	conn, _, err := client.Dial(ctx, "ws://127.0.0.1:53817/ws?token=...", protocol.HelloSchemaJsonParams{
		ProtocolVersion: "1",
		Client:          protocol.PeerInfo{Name: "dev.jonidg.example", Version: "1.0.0"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // example

	for {
		ev, err := conn.ReadEvent(ctx)
		if errors.Is(err, client.ErrClosed) {
			return
		}
		if errors.Is(err, client.ErrInvalidFrame) {
			log.Print(err)
			continue
		}
		if err != nil {
			log.Fatal(err)
		}
		var inv protocol.ActionInvokedSchemaJsonData
		if err := json.Unmarshal(ev.Data, &inv); err != nil {
			log.Print(err)
			continue
		}
		log.Printf("%s fired for %s", inv.Action.Id, inv.Context)
	}
}
