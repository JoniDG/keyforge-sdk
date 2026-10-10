"""A fake daemon the SDK connects to, so tests never need the real one."""

import json
from collections.abc import AsyncIterator, Awaitable, Callable

import pytest
from keyforge_protocol import JsonObject, JsonValue
from websockets.asyncio.server import Server, ServerConnection, serve

TOKEN = "s3cr3t-token"


class Peer:
    """The daemon side of one connection."""

    def __init__(self, ws: ServerConnection) -> None:
        self.ws = ws

    async def next(self) -> JsonObject:
        """Return the next frame the client sent."""
        frame: JsonValue = json.loads(await self.ws.recv())
        assert isinstance(frame, dict)
        return frame

    async def send(self, frame: JsonObject | str | bytes) -> None:
        await self.ws.send(frame if isinstance(frame, str | bytes) else json.dumps(frame))

    async def answer_hello(self, answer: Callable[[str], JsonObject | str]) -> JsonObject:
        """Read the hello request and answer it with answer(request id)."""
        req = await self.next()
        req_id = req["id"]
        assert isinstance(req_id, str)
        await self.send(answer(req_id))
        return req

    async def accept_hello(self) -> JsonObject:
        return await self.answer_hello(hello_ok)

    async def close_code(self) -> int | None:
        """Wait until the connection is closed and return the client's close code."""
        await self.ws.wait_closed()
        return self.ws.close_code


def hello_ok(req_id: str) -> JsonObject:
    return {
        "type": "response",
        "id": req_id,
        "ok": True,
        "data": {"server": {"name": "keyforged", "version": "0.1.0"}, "protocol_version": "1"},
    }


type Handle = Callable[[Peer], Awaitable[None]]
type FakeDaemon = Callable[[Handle], Awaitable[str]]


@pytest.fixture
async def fake_daemon() -> AsyncIterator[FakeDaemon]:
    """Start fake daemons that run handle for each connection.

    Each call returns the daemon's URL, auth token included. The connection
    stays open after handle returns, until either side closes it.
    """
    servers: list[Server] = []

    async def start(handle: Handle) -> str:
        async def on_connection(ws: ServerConnection) -> None:
            await handle(Peer(ws))
            await ws.wait_closed()

        server = await serve(on_connection, "127.0.0.1", 0, compression=None)
        servers.append(server)
        port = next(iter(server.sockets)).getsockname()[1]
        return f"ws://127.0.0.1:{port}/ws?token={TOKEN}"

    yield start
    for server in servers:
        server.close(close_connections=False)
        for conn in server.connections:
            conn.transport.abort()
        await server.wait_closed()
