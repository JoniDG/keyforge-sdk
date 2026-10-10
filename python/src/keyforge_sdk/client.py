"""Connection to the KeyForge daemon for plugins and external clients.

Connect, perform the hello handshake and read the events the daemon pushes.
Calls are aborted the asyncio way: cancel the task or wrap the call in
`asyncio.timeout`.
"""

import json
import uuid
from dataclasses import dataclass
from urllib.parse import urlsplit

from keyforge_protocol import Error, HelloParams, HelloResult, JsonObject, JsonValue, PeerInfo
from websockets.asyncio.client import ClientConnection, connect
from websockets.exceptions import ConnectionClosed, ConnectionClosedOK

__all__ = [
    "READ_LIMIT",
    "ClosedError",
    "Conn",
    "Event",
    "InvalidFrameError",
    "ServerError",
    "dial",
]

READ_LIMIT = 32 * 1024
"""Largest frame the client accepts, in bytes: the default read limit of the
Go SDK and the daemon. A larger frame closes the connection (status 1009)."""

# How long close waits for the daemon to answer the close frame, like
# coder/websocket in the Go SDK.
_CLOSE_TIMEOUT_S = 5.0


class ClosedError(Exception):
    """The daemon closed the connection normally (status 1000 or 1001).

    For a plugin this is the signal to exit.
    """

    def __init__(self) -> None:
        """Create the error."""
        super().__init__("client: connection closed")


class InvalidFrameError(Exception):
    """A single frame that doesn't match the protocol.

    When `Conn.read_event` raises it, the connection is still usable.
    """

    def __init__(self, reason: str) -> None:
        """Create the error for a frame rejected because of reason."""
        super().__init__(f"client: invalid frame: {reason}")


class ServerError(Exception):
    """An error response from the daemon.

    For example, a hello rejected with code FORBIDDEN or
    UNSUPPORTED_PROTOCOL_VERSION.
    """

    detail: Error

    def __init__(self, detail: Error) -> None:
        """Create the error for the daemon's error detail."""
        super().__init__(f"client: daemon error: {detail['code']}: {detail['message']}")
        self.detail = detail


@dataclass(frozen=True, slots=True)
class Event:
    """An event frame pushed by the daemon."""

    name: str
    """The event name, e.g. "action_invoked"."""
    data: JsonObject
    """The event payload, unchecked: check the fields you use before relying
    on the protocol type for name (e.g. ActionInvokedData)."""


class Conn:
    """A connection to the daemon that has completed the hello handshake.

    Get one with `dial`; don't construct it directly.
    """

    def __init__(self, ws: ClientConnection) -> None:
        """Wrap ws. Internal: use `dial`."""
        self._ws = ws

    async def read_event(self) -> Event:
        """Wait until the daemon pushes an event.

        Cancelling it is safe: the connection stays open and the next call
        returns the next event.

        Raises:
            ClosedError: the daemon closed the connection normally.
            InvalidFrameError: a frame that doesn't match the protocol; the
                connection is still usable.
            ConnectionError: the connection is gone.
        """
        frame = _parse_frame(await self._next_frame())
        if frame["type"] != "event":
            raise InvalidFrameError(f"unexpected {json.dumps(frame['type'])} frame")
        name = frame.get("name")
        data = frame.get("data")
        if not isinstance(name, str) or not isinstance(data, dict):
            raise InvalidFrameError("event without a name or data")
        return Event(name=name, data=data)

    async def close(self) -> None:
        """Close the connection with a normal closure.

        It waits until the daemon answers, for at most 5 seconds, and then
        drops the connection.
        """
        await self._ws.close(1000)

    async def _handshake(self, hello: HelloParams) -> HelloResult:
        request_id = str(uuid.uuid4())
        params: JsonObject = {
            "protocol_version": hello["protocol_version"],
            "client": {"name": hello["client"]["name"], "version": hello["client"]["version"]},
        }
        await self._send({"type": "request", "id": request_id, "method": "hello", "params": params})

        frame = _parse_frame(await self._next_frame())
        ok = frame.get("ok")
        if frame["type"] != "response" or not isinstance(ok, bool):
            raise InvalidFrameError(
                f"expected hello response, got {json.dumps(frame['type'])} frame"
            )
        if frame.get("id") != request_id:
            raise InvalidFrameError(
                f"response id {json.dumps(frame.get('id'))} does not match hello id "
                f"{json.dumps(request_id)}"
            )
        if not ok:
            raise ServerError(_error_detail(frame.get("error")))

        result = frame.get("data")
        server = _peer_info(result.get("server")) if isinstance(result, dict) else None
        if not isinstance(result, dict) or server is None:
            raise InvalidFrameError("hello result without server info")
        version = result.get("protocol_version")
        if version != "1":
            raise InvalidFrameError(
                f'hello result protocol_version {json.dumps(version)}, want "1"'
            )
        return {"protocol_version": "1", "server": server}

    async def _send(self, frame: JsonObject) -> None:
        await self._ws.send(json.dumps(frame))

    async def _next_frame(self) -> str:
        try:
            frame = await self._ws.recv()
        except ConnectionClosedOK:
            raise ClosedError from None
        except ConnectionClosed as exc:
            raise ConnectionError(f"client: connection closed abnormally: {exc}") from None
        if not isinstance(frame, str):
            raise InvalidFrameError("expected a text frame, got a binary one")
        return frame


async def dial(ws_url: str, hello: HelloParams) -> tuple[Conn, HelloResult]:
    """Connect to ws_url, send the hello request and wait for the daemon to accept it.

    Any error means there is no connection. Because ws_url carries the auth
    token, a failed connection is reported with the URL redacted.

    Raises:
        ServerError: the daemon rejected the hello.
        ClosedError: the daemon closed the connection before answering.
        InvalidFrameError: the daemon answered with something other than a
            hello response.
        ConnectionError: the connection failed.
        TypeError: hello is not valid hello params.
    """
    _check_hello(hello)
    failure: str | None = None
    try:
        ws = await connect(
            ws_url,
            max_size=READ_LIMIT,
            close_timeout=_CLOSE_TIMEOUT_S,
            # Like the Go and Node SDKs: no timeout of our own (the caller
            # sets one), no keepalive pings and no compression over loopback.
            open_timeout=None,
            ping_interval=None,
            compression=None,
            proxy=None,
        )
    except Exception as exc:  # noqa: BLE001 - every failure is reported the same way
        failure = _redact(ws_url, f"client.dial: {exc}")
    if failure is not None:
        # Raised outside the except block so the original exception, which may
        # carry the URL, isn't even kept as __context__.
        raise ConnectionError(failure)

    conn = Conn(ws)
    try:
        result = await conn._handshake(hello)
    except BaseException:
        await ws.close()
        raise
    return conn, result


def _check_hello(hello: HelloParams) -> None:
    # Type-checked callers can't get this wrong, others can.
    client = hello.get("client") if isinstance(hello, dict) else None
    if (
        not isinstance(hello, dict)
        or hello.get("protocol_version") != "1"
        or not isinstance(client, dict)
        or not isinstance(client.get("name"), str)
        or not isinstance(client.get("version"), str)
    ):
        raise TypeError(
            "client.dial: invalid hello params: "
            'want {"protocol_version": "1", "client": {"name", "version"}}'
        )


def _error_detail(value: JsonValue) -> Error:
    if (
        not isinstance(value, dict)
        or not isinstance(code := value.get("code"), str)
        or not isinstance(message := value.get("message"), str)
    ):
        raise InvalidFrameError("error response without a code or message")
    detail: Error = {"code": code, "message": message}
    if isinstance(details := value.get("details"), dict):
        detail["details"] = details
    return detail


def _peer_info(value: JsonValue) -> PeerInfo | None:
    if (
        not isinstance(value, dict)
        or not isinstance(name := value.get("name"), str)
        or not isinstance(version := value.get("version"), str)
    ):
        return None
    return {"name": name, "version": version}


def _parse_frame(raw: str) -> JsonObject:
    try:
        frame: JsonValue = json.loads(raw)
    except ValueError:
        raise InvalidFrameError("not JSON") from None
    if not isinstance(frame, dict) or not isinstance(frame.get("type"), str):
        raise InvalidFrameError("not an envelope")
    return frame


def _redact(ws_url: str, msg: str) -> str:
    # A connection error must never contain the auth token, which travels in
    # the URL's query.
    if ws_url:
        msg = msg.replace(ws_url, "<redacted url>")
    try:
        query = urlsplit(ws_url).query
    except ValueError:
        query = ""
    if query:
        msg = msg.replace(query, "<redacted>")
    return msg
