package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/JoniDG/keyforge-sdk/go/client"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testToken    = "s3cr3t-token"
	testPluginID = "dev.jonidg.test"
)

// fakeDaemon starts a WebSocket server that runs handle for each connection
// and returns the KEYFORGE_PLUGIN_INFO value pointing at it.
func fakeDaemon(t *testing.T, handle func(ctx context.Context, conn *websocket.Conn)) string {
	t.Helper()
	// srv.Close doesn't wait for hijacked (WebSocket) connections, so handlers
	// are tracked to keep them from outliving the test.
	var handlers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort cleanup in a test server
		handle(r.Context(), conn)
	}))
	t.Cleanup(func() {
		srv.Close()
		handlers.Wait()
	})
	return launchInfoJSON(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?token="+testToken)
}

func launchInfoJSON(t *testing.T, wsURL string) string {
	t.Helper()
	raw, err := json.Marshal(protocol.PluginLaunchInfo{PluginId: testPluginID, ProtocolVersion: "1", WsUrl: wsURL})
	require.NoError(t, err)
	return string(raw)
}

func env(value string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key != LaunchInfoEnv {
			return "", false
		}
		return value, true
	}
}

// acceptHello reads the hello request, checks it identifies the plugin and
// answers ok.
func acceptHello(ctx context.Context, t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	if !assert.NoError(t, err) {
		return
	}
	var req protocol.Request
	if !assert.NoError(t, json.Unmarshal(raw, &req)) {
		return
	}
	assert.Equal(t, "hello", req.Method)
	params, err := json.Marshal(req.Params)
	assert.NoError(t, err)
	assert.JSONEq(t, `{"protocol_version":"1","client":{"name":"dev.jonidg.test","version":"1.0.0"}}`, string(params))
	send(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":true,"data":{"server":{"name":"keyforged","version":"0.1.0"},"protocol_version":"1"}}`, req.Id))
}

// waitFor blocks until ch is closed or the connection goes away, so a plugin
// that fails early doesn't leave the fake daemon hanging.
func waitFor(ctx context.Context, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

func send(ctx context.Context, t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	assert.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(frame)))
}

func invoked(context, action, extra string) string {
	return fmt.Sprintf(`{"type":"event","name":"action_invoked","data":{"context":%q,"action":{"id":%q,"params":{}}%s}}`, context, action, extra)
}

// syncBuffer is a log sink that can be read while a plugin is still logging.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testConfig(handlers Handlers) (Config, *syncBuffer) {
	logs := &syncBuffer{}
	return Config{
		Version:  "1.0.0",
		Handlers: handlers,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, logs
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRun_DispatchesInOrderUntilDaemonCloses(t *testing.T) {
	t.Parallel()

	// Invocations still queued when the connection closes are dropped, so the
	// daemon waits for the last one before closing.
	done := make(chan struct{})
	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		send(ctx, t, conn, invoked("ctx-1", "toggle", `,"input":{"device_id":"VID_1234_PID_5678","input_id":"key_0x04","kind":"key","action":"press","timestamp_ms":1}`))
		send(ctx, t, conn, invoked("ctx-2", "toggle", ""))
		send(ctx, t, conn, invoked("ctx-1", "other", ""))
		waitFor(ctx, done)
		assert.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
	})

	var got []string
	record := func(_ context.Context, inv Invocation) error {
		input := "none"
		if inv.Input != nil {
			input = inv.Input.InputId
		}
		got = append(got, inv.Action.Id+"/"+inv.Context+"/"+input)
		if len(got) == 3 {
			close(done)
		}
		return nil
	}
	cfg, _ := testConfig(Handlers{"toggle": record, "other": record})

	require.NoError(t, run(testContext(t), cfg, env(info)))
	assert.Equal(t, []string{"toggle/ctx-1/key_0x04", "toggle/ctx-2/none", "other/ctx-1/none"}, got)
}

func TestRun_LogsAndSkipsWhatItCannotDispatch(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		send(ctx, t, conn, `not json`)
		send(ctx, t, conn, `{"type":"event","name":"input","data":{}}`)
		send(ctx, t, conn, `{"type":"event","name":"action_invoked","data":{"context":"ctx-1"}}`)
		send(ctx, t, conn, invoked("ctx-1", "missing", ""))
		send(ctx, t, conn, invoked("ctx-1", "failing", ""))
		send(ctx, t, conn, invoked("ctx-1", "ok", ""))
		waitFor(ctx, done)
		assert.NoError(t, conn.Close(websocket.StatusGoingAway, ""))
	})

	var ran []string
	cfg, logs := testConfig(Handlers{
		"failing": func(context.Context, Invocation) error {
			ran = append(ran, "failing")
			return errors.New("spotify is not running")
		},
		"ok": func(context.Context, Invocation) error {
			ran = append(ran, "ok")
			close(done)
			return nil
		},
	})

	require.NoError(t, run(testContext(t), cfg, env(info)))
	assert.Equal(t, []string{"failing", "ok"}, ran)

	out := logs.String()
	assert.Contains(t, out, `msg="ignoring invalid frame"`)
	assert.Contains(t, out, `msg="ignoring event" plugin=dev.jonidg.test event=input`)
	assert.Contains(t, out, `msg="ignoring invalid action_invoked"`)
	assert.Contains(t, out, `msg="no handler for action" plugin=dev.jonidg.test action=missing`)
	assert.Contains(t, out, `msg="action failed" plugin=dev.jonidg.test action=failing context=ctx-1 error="spotify is not running"`)
	assert.NotContains(t, out, testToken)
}

func TestRun_CancelsRunningHandlerWhenDaemonCloses(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		send(ctx, t, conn, invoked("ctx-1", "slow", ""))
		send(ctx, t, conn, invoked("ctx-1", "after", ""))
		waitFor(ctx, started)
		assert.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
	})

	var handlerErr error
	ranAfter := false
	cfg, _ := testConfig(Handlers{
		"slow": func(ctx context.Context, _ Invocation) error {
			close(started)
			<-ctx.Done()
			handlerErr = ctx.Err()
			return nil
		},
		"after": func(context.Context, Invocation) error {
			ranAfter = true
			return nil
		},
	})

	require.NoError(t, run(testContext(t), cfg, env(info)))
	require.ErrorIs(t, handlerErr, context.Canceled)
	assert.False(t, ranAfter, "invocations queued after the connection closed must not run")
}

func TestRun_AbnormalClosureIsAnError(t *testing.T) {
	t.Parallel()

	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		assert.NoError(t, conn.Close(websocket.StatusInternalError, "daemon crashed"))
	})
	cfg, _ := testConfig(nil)

	err := run(testContext(t), cfg, env(info))
	require.ErrorContains(t, err, "plugin.Run: client.ReadEvent:")
	assert.NotErrorIs(t, err, client.ErrClosed)
}

func TestRun_ContextCancelClosesConnection(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	status := make(chan websocket.StatusCode, 1)
	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		send(ctx, t, conn, invoked("ctx-1", "stop", ""))
		_, _, err := conn.Read(ctx)
		status <- websocket.CloseStatus(err)
	})

	runCtx, cancel := context.WithCancel(ctx)
	cfg, _ := testConfig(Handlers{"stop": func(context.Context, Invocation) error {
		cancel()
		return nil
	}})

	require.ErrorIs(t, run(runCtx, cfg, env(info)), context.Canceled)
	assert.Equal(t, websocket.StatusNormalClosure, <-status)
}

func TestRun_HelloRejected(t *testing.T) {
	t.Parallel()

	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		_, raw, err := conn.Read(ctx)
		if !assert.NoError(t, err) {
			return
		}
		var req protocol.Request
		assert.NoError(t, json.Unmarshal(raw, &req))
		send(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":false,"error":{"code":"FORBIDDEN","message":"name does not match token"}}`, req.Id))
	})
	cfg, _ := testConfig(nil)

	err := run(testContext(t), cfg, env(info))
	var serverErr *client.ServerError
	require.ErrorAs(t, err, &serverErr)
	assert.Equal(t, "FORBIDDEN", serverErr.Detail.Code)
}

func TestRun_MissingVersion(t *testing.T) {
	t.Parallel()

	cfg, _ := testConfig(nil)
	cfg.Version = ""
	err := run(testContext(t), cfg, env(launchInfoJSON(t, "ws://127.0.0.1:1/ws?token="+testToken)))
	require.ErrorContains(t, err, "invalid hello params")
}

func TestRun_InvalidLaunchInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lookup  func(string) (string, bool)
		wantErr string
	}{
		{name: "not set", lookup: func(string) (string, bool) { return "", false }, wantErr: "KEYFORGE_PLUGIN_INFO is not set; plugins must be launched by the KeyForge daemon"},
		{name: "empty", lookup: env(""), wantErr: "KEYFORGE_PLUGIN_INFO is not set"},
		{name: "not json", lookup: env("token=" + testToken), wantErr: "invalid KEYFORGE_PLUGIN_INFO"},
		{name: "missing ws_url", lookup: env(`{"plugin_id":"dev.jonidg.test","protocol_version":"1"}`), wantErr: "field ws_url in PluginLaunchInfo: required"},
		{name: "invalid plugin id", lookup: env(`{"plugin_id":"Test","protocol_version":"1","ws_url":"ws://127.0.0.1/ws?token=` + testToken + `"}`), wantErr: "invalid KEYFORGE_PLUGIN_INFO"},
		{name: "unsupported protocol", lookup: env(`{"plugin_id":"dev.jonidg.test","protocol_version":"2","ws_url":"ws://127.0.0.1/ws?token=` + testToken + `"}`), wantErr: "protocol_version: must be equal to 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, _ := testConfig(nil)
			err := run(testContext(t), cfg, tt.lookup)
			require.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), testToken)
		})
	}
}

// Not parallel: t.Setenv can't be used in parallel tests. The other tests
// cover the same logic through run with an injected lookup.
func TestRun_ReadsEnvironment(t *testing.T) {
	t.Setenv(LaunchInfoEnv, "")
	err := Run(context.Background(), Config{Version: "1.0.0"})
	require.ErrorContains(t, err, "KEYFORGE_PLUGIN_INFO is not set")
}

func TestRun_DefaultLogger(t *testing.T) {
	t.Parallel()

	info := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		assert.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
	})
	require.NoError(t, run(testContext(t), Config{Version: "1.0.0"}, env(info)))
}
