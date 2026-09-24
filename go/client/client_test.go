package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "s3cr3t-token"

func validHello() protocol.HelloSchemaJsonParams {
	return protocol.HelloSchemaJsonParams{
		ProtocolVersion: "1",
		Client:          protocol.PeerInfo{Name: "dev.jonidg.test", Version: "1.0.0"},
	}
}

// fakeDaemon starts a WebSocket server that runs handle for each connection
// and returns a ws:// URL carrying testToken.
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
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + testToken
}

// readHello reads the hello request and returns its id.
func readHello(ctx context.Context, t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	if !assert.NoError(t, err) {
		return ""
	}
	var req protocol.Request
	if !assert.NoError(t, json.Unmarshal(raw, &req)) {
		return ""
	}
	return req.Id
}

func writeJSON(ctx context.Context, t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	assert.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(frame)))
}

func okResponse(id string) string {
	return fmt.Sprintf(`{"type":"response","id":%q,"ok":true,"data":{"server":{"name":"keyforged","version":"0.1.0"},"protocol_version":"1"}}`, id)
}

// acceptHello answers the hello with an ok response.
func acceptHello(ctx context.Context, t *testing.T, conn *websocket.Conn) {
	t.Helper()
	writeJSON(ctx, t, conn, okResponse(readHello(ctx, t, conn)))
}

// waitClosed blocks until the client goes away, so the server doesn't close
// the connection before the client has read what it needs.
func waitClosed(ctx context.Context, conn *websocket.Conn) {
	_, _, _ = conn.Read(ctx) //nolint:errcheck // any result means the client is gone
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestDial_SendsHelloAndReturnsResult(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	received := make(chan map[string]json.RawMessage, 1)
	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		_, raw, err := conn.Read(ctx)
		if !assert.NoError(t, err) {
			return
		}
		var req protocol.Request
		assert.NoError(t, json.Unmarshal(raw, &req))
		var fields map[string]json.RawMessage
		assert.NoError(t, json.Unmarshal(raw, &fields))
		received <- fields
		writeJSON(ctx, t, conn, okResponse(req.Id))
		waitClosed(ctx, conn)
	})

	conn, result, err := Dial(ctx, url, validHello())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	assert.Equal(t, protocol.HelloSchemaJsonResult{
		ProtocolVersion: "1",
		Server:          protocol.PeerInfo{Name: "keyforged", Version: "0.1.0"},
	}, result)

	fields := <-received
	assert.JSONEq(t, `"request"`, string(fields["type"]))
	assert.JSONEq(t, `"hello"`, string(fields["method"]))
	assert.JSONEq(t, `{"protocol_version":"1","client":{"name":"dev.jonidg.test","version":"1.0.0"}}`, string(fields["params"]))
	var id string
	require.NoError(t, json.Unmarshal(fields["id"], &id))
	assert.NotEmpty(t, id)
}

func TestDial_InvalidHelloParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		hello protocol.HelloSchemaJsonParams
	}{
		{name: "unsupported protocol version", hello: protocol.HelloSchemaJsonParams{
			ProtocolVersion: "2", Client: protocol.PeerInfo{Name: "dev.jonidg.test", Version: "1.0.0"},
		}},
		{name: "missing client version", hello: protocol.HelloSchemaJsonParams{
			ProtocolVersion: "1", Client: protocol.PeerInfo{Name: "dev.jonidg.test"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Nothing listens on this URL: validation must fail before dialing.
			_, _, err := Dial(testContext(t), "ws://127.0.0.1:1/ws", tt.hello)
			require.ErrorContains(t, err, "invalid hello params")
		})
	}
}

func TestDial_ServerRejectsHello(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		id := readHello(ctx, t, conn)
		writeJSON(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":false,"error":{"code":"FORBIDDEN","message":"client name does not match token"}}`, id))
		waitClosed(ctx, conn)
	})

	_, _, err := Dial(ctx, url, validHello())
	var serverErr *ServerError
	require.ErrorAs(t, err, &serverErr)
	assert.Equal(t, "FORBIDDEN", serverErr.Detail.Code)
	assert.EqualError(t, err, "client.Dial: hello rejected: client: daemon error: FORBIDDEN: client name does not match token")
}

func TestDial_InvalidHelloResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		respond func(ctx context.Context, t *testing.T, conn *websocket.Conn, id string)
		wantErr string
	}{
		{
			name: "event instead of response",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, _ string) {
				writeJSON(ctx, t, conn, `{"type":"event","name":"input","data":{}}`)
			},
			wantErr: `expected hello response, got "event" frame`,
		},
		{
			name: "response without ok",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) {
				writeJSON(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q}`, id))
			},
			wantErr: `expected hello response, got "response" frame`,
		},
		{
			name: "not json",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, _ string) {
				writeJSON(ctx, t, conn, `not json`)
			},
			wantErr: "invalid frame",
		},
		{
			name: "binary frame",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, _ string) {
				assert.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte(`{}`)))
			},
			wantErr: "expected a text frame",
		},
		{
			name: "ok response id mismatch",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, _ string) {
				writeJSON(ctx, t, conn, okResponse("other"))
			},
			wantErr: `response id "other" does not match hello id`,
		},
		{
			name: "error response id mismatch",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, _ string) {
				writeJSON(ctx, t, conn, `{"type":"response","id":"other","ok":false,"error":{"code":"FORBIDDEN","message":"no"}}`)
			},
			wantErr: `response id "other" does not match hello id`,
		},
		{
			name: "ok response missing data",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) {
				writeJSON(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":true}`, id))
			},
			wantErr: "field data in ResponseOk: required",
		},
		{
			name: "error response missing error",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) {
				writeJSON(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":false}`, id))
			},
			wantErr: "field error in ResponseErr: required",
		},
		{
			name: "invalid hello result",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) {
				writeJSON(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":true,"data":{"protocol_version":"1"}}`, id))
			},
			wantErr: "hello result: field server in HelloSchemaJsonResult: required",
		},
		{
			name: "protocol version mismatch",
			respond: func(ctx context.Context, t *testing.T, conn *websocket.Conn, id string) {
				writeJSON(ctx, t, conn, fmt.Sprintf(`{"type":"response","id":%q,"ok":true,"data":{"server":{"name":"keyforged","version":"0.1.0"},"protocol_version":"2"}}`, id))
			},
			wantErr: "hello result: field protocol_version: must be equal to 1",
		},
		{
			name: "closed before responding",
			respond: func(_ context.Context, t *testing.T, conn *websocket.Conn, _ string) {
				assert.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
			},
			wantErr: "read hello response: client: connection closed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
				tt.respond(ctx, t, conn, readHello(ctx, t, conn))
				waitClosed(ctx, conn)
			})
			_, _, err := Dial(testContext(t), url, validHello())
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestHandshake_ClosedSocket(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) { waitClosed(ctx, conn) })
	ws, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	require.NoError(t, ws.CloseNow())

	conn := &Conn{ws: ws}
	_, err = conn.handshake(ctx, []byte(`{}`), "id")
	require.ErrorContains(t, err, "client.Dial: send hello")
	require.ErrorContains(t, conn.Close(), "client.Close")
}

func TestDial_ErrorsNeverLeakToken(t *testing.T) {
	t.Parallel()

	// A server that is already shut down: the dial fails with a *url.Error
	// that embeds the full URL, query included.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + testToken

	tests := []struct {
		name string
		url  string
	}{
		{name: "connection refused", url: base},
		{name: "unparseable url", url: "://bad/ws?token=" + testToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := Dial(testContext(t), tt.url, validHello())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testToken)
		})
	}
}

func TestDial_UpgradeRejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, _, err := Dial(testContext(t), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?token="+testToken, validHello())
	require.ErrorContains(t, err, "401")
	assert.NotContains(t, err.Error(), testToken)
}

func TestDial_CancelledContext(t *testing.T) {
	t.Parallel()

	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) { waitClosed(ctx, conn) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := Dial(ctx, url, validHello())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), testToken)
}

func TestReadEvent_DecodesActionInvoked(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		writeJSON(ctx, t, conn, `{"type":"event","name":"action_invoked","data":{"context":"ctx-1","action":{"id":"play_pause","params":{"volume":5}},"input":{"device_id":"VID_1234_PID_5678","input_id":"key_0x04","kind":"key","action":"press","timestamp_ms":1700000000000}}}`)
		writeJSON(ctx, t, conn, `{"type":"event","name":"action_invoked","data":{"context":"ctx-2","action":{"id":"play_pause","params":{}}}}`)
		waitClosed(ctx, conn)
	})

	conn, _, err := Dial(ctx, url, validHello())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ev, err := conn.ReadEvent(ctx)
	require.NoError(t, err)
	assert.Equal(t, "action_invoked", ev.Name)
	var data protocol.ActionInvokedSchemaJsonData
	require.NoError(t, json.Unmarshal(ev.Data, &data))
	assert.Equal(t, "ctx-1", data.Context)
	assert.Equal(t, "play_pause", data.Action.Id)
	assert.Equal(t, protocol.ActionInvokedSchemaJsonDataActionParams{"volume": float64(5)}, data.Action.Params)
	require.NotNil(t, data.Input)
	assert.Equal(t, "key_0x04", data.Input.InputId)

	ev, err = conn.ReadEvent(ctx)
	require.NoError(t, err)
	var noInput protocol.ActionInvokedSchemaJsonData
	require.NoError(t, json.Unmarshal(ev.Data, &noInput))
	assert.Equal(t, "ctx-2", noInput.Context)
	assert.Nil(t, noInput.Input)
}

func TestReadEvent_InvalidFrameKeepsConnection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		send    func(ctx context.Context, t *testing.T, conn *websocket.Conn)
		wantErr string
	}{
		{
			name: "response frame",
			send: func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
				writeJSON(ctx, t, conn, okResponse("late"))
			},
			wantErr: `unexpected "response" frame`,
		},
		{
			name: "not json",
			send: func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
				writeJSON(ctx, t, conn, `{`)
			},
			wantErr: "invalid frame",
		},
		{
			name: "invalid event name",
			send: func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
				writeJSON(ctx, t, conn, `{"type":"event","name":"Bad-Name","data":{}}`)
			},
			wantErr: "must match",
		},
		{
			name: "binary frame",
			send: func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
				assert.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte(`{}`)))
			},
			wantErr: "expected a text frame",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)
			url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
				acceptHello(ctx, t, conn)
				tt.send(ctx, t, conn)
				writeJSON(ctx, t, conn, `{"type":"event","name":"action_invoked","data":{}}`)
				waitClosed(ctx, conn)
			})

			conn, _, err := Dial(ctx, url, validHello())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			_, err = conn.ReadEvent(ctx)
			require.ErrorIs(t, err, ErrInvalidFrame)
			require.ErrorContains(t, err, tt.wantErr)

			ev, err := conn.ReadEvent(ctx)
			require.NoError(t, err)
			assert.Equal(t, "action_invoked", ev.Name)
		})
	}
}

func TestReadEvent_ConnectionClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     websocket.StatusCode
		wantClosed bool
	}{
		{name: "normal closure", status: websocket.StatusNormalClosure, wantClosed: true},
		{name: "going away", status: websocket.StatusGoingAway, wantClosed: true},
		{name: "policy violation", status: websocket.StatusPolicyViolation, wantClosed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := testContext(t)
			url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
				acceptHello(ctx, t, conn)
				assert.NoError(t, conn.Close(tt.status, "bye"))
			})

			conn, _, err := Dial(ctx, url, validHello())
			require.NoError(t, err)

			_, err = conn.ReadEvent(ctx)
			require.Error(t, err)
			assert.Equal(t, tt.wantClosed, errors.Is(err, ErrClosed))
			assert.NotErrorIs(t, err, ErrInvalidFrame)
		})
	}
}

func TestReadEvent_CancelledContext(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		waitClosed(ctx, conn)
	})

	conn, _, err := Dial(ctx, url, validHello())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Cancel while ReadEvent is blocked, as a plugin does on shutdown.
	readCtx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = conn.ReadEvent(readCtx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestClose_SendsNormalClosure(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	status := make(chan websocket.StatusCode, 1)
	url := fakeDaemon(t, func(ctx context.Context, conn *websocket.Conn) {
		acceptHello(ctx, t, conn)
		_, _, err := conn.Read(ctx)
		status <- websocket.CloseStatus(err)
	})

	conn, _, err := Dial(ctx, url, validHello())
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.Equal(t, websocket.StatusNormalClosure, <-status)

}
