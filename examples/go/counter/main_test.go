package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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

func key() *protocol.InputEvent {
	return &protocol.InputEvent{Kind: protocol.InputKindKey, InputId: "key_1", Action: protocol.InputActionPress}
}

func encoder(action protocol.InputAction) *protocol.InputEvent {
	return &protocol.InputEvent{Kind: protocol.InputKindEncoder, InputId: "encoder_0", Action: action}
}

func invocation(context string, input *protocol.InputEvent, params map[string]string) plugin.Invocation {
	p := protocol.ActionInvokedSchemaJsonDataActionParams{}
	for name, value := range params {
		p[name] = value
	}
	return plugin.Invocation{Context: context, Action: protocol.ActionInvokedSchemaJsonDataAction{Id: "count", Params: p}, Input: input}
}

func TestNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		count int
		input *protocol.InputEvent
		want  int
	}{
		{name: "key press adds one", count: 2, input: key(), want: 3},
		{name: "run from the app adds one", count: 2, input: nil, want: 3},
		{name: "turn right adds one", count: 2, input: encoder(protocol.InputActionRotateCw), want: 3},
		{name: "turn left takes one away", count: 2, input: encoder(protocol.InputActionRotateCcw), want: 1},
		{name: "never below zero", count: 0, input: encoder(protocol.InputActionRotateCcw), want: 0},
		{name: "press resets", count: 7, input: encoder(protocol.InputActionClick), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, next(tt.count, tt.input))
		})
	}
}

func TestCountKeepsOneCounterPerBinding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.txt"), filepath.Join(dir, "second.txt")
	count := handlers(slog.New(slog.DiscardHandler))["count"]
	require.NoError(t, count(t.Context(), invocation("binding-1", key(), map[string]string{"file": first})))
	require.NoError(t, count(t.Context(), invocation("binding-1", key(), map[string]string{"file": first})))
	require.NoError(t, count(t.Context(), invocation("binding-2", key(), map[string]string{"file": second})))

	assertFile(t, first, "2")
	assertFile(t, second, "1")
}

// Same as in docs/tutorial-go.md, step 7.
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

func TestCountSharesCounterByLabel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "deaths.txt")
	count := handlers(slog.New(slog.DiscardHandler))["count"]
	params := map[string]string{"label": "Deaths", "file": file}

	// An encoder's three triggers are three bindings with their own contexts.
	require.NoError(t, count(t.Context(), invocation("rotate-cw", encoder(protocol.InputActionRotateCw), params)))
	require.NoError(t, count(t.Context(), invocation("rotate-cw", encoder(protocol.InputActionRotateCw), params)))
	require.NoError(t, count(t.Context(), invocation("rotate-ccw", encoder(protocol.InputActionRotateCcw), params)))
	assertFile(t, file, "Deaths: 1")

	require.NoError(t, count(t.Context(), invocation("click", encoder(protocol.InputActionClick), params)))
	assertFile(t, file, "Deaths: 0")
}

func TestCountWritesOnlyTheNumberWithoutLabel(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "count.txt")
	count := handlers(slog.New(slog.DiscardHandler))["count"]
	require.NoError(t, count(t.Context(), invocation("binding-1", key(), map[string]string{"file": file})))
	assertFile(t, file, "1")
}

func TestCountFileErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file func(t *testing.T) string
		want string
	}{
		{name: "relative path", file: func(*testing.T) string { return "deaths.txt" }, want: "must be an absolute path"},
		{name: "missing folder", file: func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope", "deaths.txt") }, want: "counter.writeCount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			count := handlers(slog.New(slog.DiscardHandler))["count"]
			err := count(t.Context(), invocation("binding-1", key(), map[string]string{"file": tt.file(t)}))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path) // #nosec G304 -- test reads the file it just had the plugin write
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}
