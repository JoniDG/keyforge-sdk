package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/JoniDG/keyforge-protocol/go/protocol"
	"github.com/coder/websocket"
)

// ErrClosed is wrapped by the error ReadEvent returns once the daemon has
// closed the connection normally (status 1000 or 1001). For a plugin this is
// the signal to exit.
var ErrClosed = errors.New("client: connection closed")

// ErrInvalidFrame is wrapped by errors about a single frame that doesn't match
// the protocol. When ReadEvent returns it, the connection is still usable.
var ErrInvalidFrame = errors.New("client: invalid frame")

// ServerError is an error response from the daemon, e.g. a hello rejected
// with code FORBIDDEN or UNSUPPORTED_PROTOCOL_VERSION. Use errors.As to get it.
type ServerError struct {
	Detail protocol.Error
}

// Error implements the error interface.
func (e *ServerError) Error() string {
	return fmt.Sprintf("client: daemon error: %s: %s", e.Detail.Code, e.Detail.Message)
}

// Conn is a connection to the daemon that has completed the hello handshake.
type Conn struct {
	ws *websocket.Conn
}

// Event is an event frame pushed by the daemon.
type Event struct {
	// Name is the event name, e.g. "action_invoked".
	Name string
	// Data is the raw event payload. Decode it with json.Unmarshal into the
	// generated protocol type for Name (e.g. protocol.ActionInvokedSchemaJsonData),
	// whose UnmarshalJSON validates it.
	Data json.RawMessage
}

// frame holds the envelope fields needed to route an incoming frame. Each
// frame is then validated by decoding it into its generated protocol type.
type frame struct {
	Type string          `json:"type"`
	OK   *bool           `json:"ok"`
	Data json.RawMessage `json:"data"`
}

// Dial connects to wsURL, sends the hello request and waits for the daemon to
// accept it. It returns the daemon's hello result.
//
// Any error means there is no connection, even when it wraps ErrClosed or
// ErrInvalidFrame. A rejected hello wraps a *ServerError. Because wsURL
// carries the auth token, a failed dial is reported with the URL redacted and
// without wrapping the underlying transport error; only a context error
// (context.Canceled, context.DeadlineExceeded) is kept for errors.Is.
func Dial(ctx context.Context, wsURL string, hello protocol.HelloSchemaJsonParams) (*Conn, protocol.HelloSchemaJsonResult, error) {
	var result protocol.HelloSchemaJsonResult
	req, id, err := helloRequest(hello)
	if err != nil {
		return nil, result, err
	}

	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, result, dialError(ctx, wsURL, err)
	}

	c := &Conn{ws: ws}
	result, err = c.handshake(ctx, req, id)
	if err != nil {
		_ = ws.CloseNow()
		return nil, result, err
	}
	return c, result, nil
}

// ReadEvent blocks until the daemon pushes an event. It returns ErrClosed when
// the daemon closes the connection normally, and an error wrapping
// ErrInvalidFrame for a frame that doesn't match the protocol. Any other error
// means the connection is gone. Cancelling ctx aborts the read and may close
// the connection; call Close when done either way. A frame larger than 32 KiB
// (the default read limit on both ends) closes the connection.
func (c *Conn) ReadEvent(ctx context.Context) (Event, error) {
	raw, f, err := c.readFrame(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("client.ReadEvent: %w", err)
	}
	if f.Type != "event" {
		return Event{}, fmt.Errorf("client.ReadEvent: %w: unexpected %q frame", ErrInvalidFrame, f.Type)
	}
	var ev protocol.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Event{}, fmt.Errorf("client.ReadEvent: %w: %w", ErrInvalidFrame, err)
	}
	return Event{Name: ev.Name, Data: f.Data}, nil
}

// Close closes the connection with a normal closure.
func (c *Conn) Close() error {
	if err := c.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		return fmt.Errorf("client.Close: %w", err)
	}
	return nil
}

func helloRequest(hello protocol.HelloSchemaJsonParams) ([]byte, string, error) {
	encoded, err := json.Marshal(hello)
	if err != nil {
		return nil, "", fmt.Errorf("client.Dial: encode hello: %w", err)
	}
	// Round-tripping through the generated type validates the params before
	// anything is sent, and yields the generic params map the envelope needs.
	var check protocol.HelloSchemaJsonParams
	if err := json.Unmarshal(encoded, &check); err != nil {
		return nil, "", fmt.Errorf("client.Dial: invalid hello params: %w", err)
	}
	var params protocol.RequestParams
	if err := json.Unmarshal(encoded, &params); err != nil {
		return nil, "", fmt.Errorf("client.Dial: encode hello: %w", err)
	}

	id := rand.Text()
	req, err := json.Marshal(protocol.Request{Type: "request", Id: id, Method: "hello", Params: params})
	if err != nil {
		return nil, "", fmt.Errorf("client.Dial: encode hello: %w", err)
	}
	return req, id, nil
}

func (c *Conn) handshake(ctx context.Context, req []byte, id string) (protocol.HelloSchemaJsonResult, error) {
	var result protocol.HelloSchemaJsonResult
	if err := c.ws.Write(ctx, websocket.MessageText, req); err != nil {
		return result, fmt.Errorf("client.Dial: send hello: %w", err)
	}

	raw, f, err := c.readFrame(ctx)
	if err != nil {
		return result, fmt.Errorf("client.Dial: read hello response: %w", err)
	}
	if f.Type != "response" || f.OK == nil {
		return result, fmt.Errorf("client.Dial: %w: expected hello response, got %q frame", ErrInvalidFrame, f.Type)
	}

	if !*f.OK {
		var resp protocol.ResponseErr
		if err := json.Unmarshal(raw, &resp); err != nil {
			return result, fmt.Errorf("client.Dial: %w: %w", ErrInvalidFrame, err)
		}
		if resp.Id != id {
			return result, fmt.Errorf("client.Dial: %w: response id %q does not match hello id %q", ErrInvalidFrame, resp.Id, id)
		}
		return result, fmt.Errorf("client.Dial: hello rejected: %w", &ServerError{Detail: resp.Error})
	}

	var resp protocol.ResponseOk
	if err := json.Unmarshal(raw, &resp); err != nil {
		return result, fmt.Errorf("client.Dial: %w: %w", ErrInvalidFrame, err)
	}
	if resp.Id != id {
		return result, fmt.Errorf("client.Dial: %w: response id %q does not match hello id %q", ErrInvalidFrame, resp.Id, id)
	}
	// The generated UnmarshalJSON also rejects a protocol_version other than
	// the one hello params are pinned to.
	if err := json.Unmarshal(f.Data, &result); err != nil {
		return result, fmt.Errorf("client.Dial: %w: hello result: %w", ErrInvalidFrame, err)
	}
	return result, nil
}

// readFrame reads one frame and decodes its routing fields. Decoding errors
// wrap ErrInvalidFrame; a normal closure is reported as ErrClosed.
func (c *Conn) readFrame(ctx context.Context) ([]byte, frame, error) {
	var f frame
	typ, raw, err := c.ws.Read(ctx)
	if err != nil {
		switch websocket.CloseStatus(err) {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return nil, f, ErrClosed
		}
		return nil, f, err
	}
	if typ != websocket.MessageText {
		return nil, f, fmt.Errorf("%w: expected a text frame, got %s", ErrInvalidFrame, typ)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, f, fmt.Errorf("%w: %w", ErrInvalidFrame, err)
	}
	return raw, f, nil
}

// dialError rewrites a dial error so it never contains the auth token: the
// underlying errors (e.g. *url.Error) embed the full URL, query included. The
// cause is not wrapped for the same reason; only a context error is kept.
func dialError(ctx context.Context, wsURL string, err error) error {
	msg := err.Error()
	if wsURL != "" {
		msg = strings.ReplaceAll(msg, wsURL, "<redacted url>")
	}
	if u, parseErr := url.Parse(wsURL); parseErr == nil && u.RawQuery != "" {
		msg = strings.ReplaceAll(msg, u.RawQuery, "<redacted>")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("client.Dial: %s: %w", msg, ctxErr)
	}
	return fmt.Errorf("client.Dial: %s", msg)
}
