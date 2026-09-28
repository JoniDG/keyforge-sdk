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

func TestEchoLogsInputAndContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input *protocol.InputEvent
		want  string
	}{
		{name: "fired by a key", input: &protocol.InputEvent{InputId: "key_0x04"}, want: "msg=echo input_id=key_0x04 context=binding-1"},
		{name: "fired from the app", input: nil, want: "msg=echo input_id=none context=binding-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			echo := handlers(slog.New(slog.NewTextHandler(&logs, nil)))["echo"]
			inv := plugin.Invocation{Context: "binding-1", Action: protocol.ActionInvokedSchemaJsonDataAction{Id: "echo"}, Input: tt.input}
			require.NoError(t, echo(t.Context(), inv))
			assert.Contains(t, logs.String(), tt.want)
		})
	}
}
