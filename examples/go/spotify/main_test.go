package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
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

	assert.Equal(t, pluginID, string(manifest.Id))
	assert.Equal(t, version, manifest.Version)
	h := handlers(nil, nil)
	require.Len(t, h, len(manifest.Actions))
	for _, action := range manifest.Actions {
		assert.Contains(t, h, action.Id)
	}
}

func invocation(id string, input *protocol.InputEvent, params protocol.ActionInvokedSchemaJsonDataActionParams) plugin.Invocation {
	if params == nil {
		params = protocol.ActionInvokedSchemaJsonDataActionParams{}
	}
	return plugin.Invocation{Context: "binding-1", Action: protocol.ActionInvokedSchemaJsonDataAction{Id: id, Params: params}, Input: input}
}

func encoder(action protocol.InputAction) *protocol.InputEvent {
	return &protocol.InputEvent{DeviceId: "dev", Kind: protocol.InputKindEncoder, InputId: "encoder_0", Action: action}
}

func TestPlayPause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state http.HandlerFunc
		want  string
	}{
		{name: "pauses while playing", state: respond(http.StatusOK, `{"is_playing":true,"device":{}}`), want: "PUT /me/player/pause"},
		{name: "plays while paused", state: respond(http.StatusOK, `{"is_playing":false,"device":{}}`), want: "PUT /me/player/play"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, api := newFakeAPI(t, map[string]http.HandlerFunc{"GET /me/player": tt.state})
			require.NoError(t, handlers(p, nil)["play_pause"](t.Context(), invocation("play_pause", nil, nil)))
			assert.Equal(t, []string{"GET /me/player", tt.want}, api.called())
		})
	}
}

func TestPlayPauseWithoutDevice(t *testing.T) {
	t.Parallel()

	p, api := newFakeAPI(t, map[string]http.HandlerFunc{"GET /me/player": respond(http.StatusNoContent, "")})
	err := handlers(p, nil)["play_pause"](t.Context(), invocation("play_pause", nil, nil))
	require.ErrorContains(t, err, "no active Spotify device")
	assert.Equal(t, []string{"GET /me/player"}, api.called())
}

func TestSkipTrack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		want string
	}{
		{id: "next", want: "POST /me/player/next"},
		{id: "previous", want: "POST /me/player/previous"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			p, api := newFakeAPI(t, nil)
			require.NoError(t, handlers(p, nil)[tt.id](t.Context(), invocation(tt.id, nil, nil)))
			assert.Equal(t, []string{tt.want}, api.called())
		})
	}
}

func TestVolumeHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   *protocol.InputEvent
		params  protocol.ActionInvokedSchemaJsonDataActionParams
		want    int
		wantErr string
	}{
		{name: "turn right", input: encoder(protocol.InputActionRotateCw), want: 45},
		{name: "turn left with a custom step", input: encoder(protocol.InputActionRotateCcw), params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": "10"}, want: 30},
		{name: "press mutes", input: encoder(protocol.InputActionClick), want: 0},
		{name: "other triggers do nothing", input: encoder(protocol.InputActionPress), want: 40},
		{name: "not from an encoder", input: nil, wantErr: "only runs from an encoder"},
		{name: "invalid step", input: encoder(protocol.InputActionRotateCw), params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": "loud"}, wantErr: "param step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v, _ := newTestVolume(t, map[string]http.HandlerFunc{"GET /me/player": stateAt(40)})
			v.target = 40 // what a no-op trigger must leave untouched
			err := handlers(v.player, v)["volume"](t.Context(), invocation("volume", tt.input, tt.params))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, v.target)
		})
	}
}

func TestStepParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		params  protocol.ActionInvokedSchemaJsonDataActionParams
		want    int
		wantErr string
	}{
		{name: "missing", params: protocol.ActionInvokedSchemaJsonDataActionParams{}, want: defaultStep},
		{name: "empty", params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": ""}, want: defaultStep},
		{name: "set", params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": "10"}, want: 10},
		{name: "zero", params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": "0"}, wantErr: "from 1 to 100"},
		{name: "above 100", params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": "101"}, wantErr: "from 1 to 100"},
		{name: "not a number", params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": "loud"}, wantErr: "from 1 to 100"},
		{name: "not a string", params: protocol.ActionInvokedSchemaJsonDataActionParams{"step": 5.0}, wantErr: "must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			step, err := stepParam(tt.params)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, step)
		})
	}
}

func TestRunLoginFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "client id missing", args: nil, want: "-client-id is required"},
		{name: "unknown flag", args: []string{"-nope"}, want: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, runLogin(t.Context(), tt.args, &bytes.Buffer{}), tt.want)
		})
	}
}

func TestRunPluginWithoutDaemon(t *testing.T) {
	t.Parallel()

	// Run by hand, outside keyforged, the plugin stops right away.
	require.ErrorContains(t, runPlugin(t.Context(), slog.New(slog.DiscardHandler)), "KEYFORGE_PLUGIN_INFO")
}
