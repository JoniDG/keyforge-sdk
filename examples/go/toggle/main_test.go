package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/JoniDG/keyforge-sdk/go/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestToggleKeepsStatePerContext(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	toggle := handlers(slog.New(slog.NewTextHandler(&logs, nil)))["toggle"]
	invoke := func(context string, input *protocol.InputEvent) {
		inv := plugin.Invocation{Context: context, Action: protocol.ActionInvokedSchemaJsonDataAction{Id: "toggle"}, Input: input}
		require.NoError(t, toggle(t.Context(), inv))
	}

	invoke("key-a", &protocol.InputEvent{InputId: "key_0x04"})
	invoke("key-b", nil)
	invoke("key-a", &protocol.InputEvent{InputId: "key_0x04"})

	assert.Contains(t, logs.String(), "context=key-a on=true source=key_0x04")
	assert.Contains(t, logs.String(), "context=key-b on=true source=app")
	assert.Contains(t, logs.String(), "context=key-a on=false source=key_0x04")
}
