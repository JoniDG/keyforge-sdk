import asyncio
import socket

import pytest
from keyforge_protocol import HelloParams, JsonObject

from keyforge_sdk.client import (
    READ_LIMIT,
    ClosedError,
    Conn,
    Event,
    InvalidFrameError,
    ServerError,
    dial,
)

from .conftest import TOKEN, FakeDaemon, Peer

HELLO: HelloParams = {"protocol_version": "1", "client": {"name": "test", "version": "1.2.3"}}


async def connected(fake_daemon: FakeDaemon) -> tuple[Conn, asyncio.Queue[Peer]]:
    """Dial a fake daemon that accepts hello and hand back its peer."""
    peers: asyncio.Queue[Peer] = asyncio.Queue()

    async def handle(peer: Peer) -> None:
        await peer.accept_hello()
        peers.put_nowait(peer)

    conn, _ = await dial(await fake_daemon(handle), HELLO)
    return conn, peers


def unused_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port: int = sock.getsockname()[1]
        return port


class TestDial:
    async def test_sends_hello_and_returns_the_daemon_result(self, fake_daemon: FakeDaemon) -> None:
        requests: list[JsonObject] = []

        async def handle(peer: Peer) -> None:
            requests.append(await peer.accept_hello())

        conn, result = await dial(await fake_daemon(handle), HELLO)
        await conn.close()

        assert result == {
            "protocol_version": "1",
            "server": {"name": "keyforged", "version": "0.1.0"},
        }
        assert requests[0]["type"] == "request"
        assert requests[0]["method"] == "hello"
        assert requests[0]["params"] == HELLO

    async def test_raises_server_error_when_the_daemon_rejects_hello(
        self, fake_daemon: FakeDaemon
    ) -> None:
        async def handle(peer: Peer) -> None:
            await peer.answer_hello(
                lambda req_id: {
                    "type": "response",
                    "id": req_id,
                    "ok": False,
                    "error": {"code": "FORBIDDEN", "message": "bad token", "details": {"a": 1}},
                }
            )

        with pytest.raises(ServerError, match="FORBIDDEN: bad token") as exc:
            await dial(await fake_daemon(handle), HELLO)
        assert exc.value.detail == {
            "code": "FORBIDDEN",
            "message": "bad token",
            "details": {"a": 1},
        }

    async def test_server_error_without_details(self, fake_daemon: FakeDaemon) -> None:
        async def handle(peer: Peer) -> None:
            await peer.answer_hello(
                lambda req_id: {
                    "type": "response",
                    "id": req_id,
                    "ok": False,
                    "error": {"code": "UNSUPPORTED_PROTOCOL_VERSION", "message": "no"},
                }
            )

        with pytest.raises(ServerError) as exc:
            await dial(await fake_daemon(handle), HELLO)
        assert exc.value.detail == {"code": "UNSUPPORTED_PROTOCOL_VERSION", "message": "no"}

    @pytest.mark.parametrize(
        ("answer", "want"),
        [
            pytest.param(lambda _: "nope", "not JSON", id="not JSON"),
            pytest.param(lambda _: "[1]", "not an envelope", id="not an object"),
            pytest.param(lambda _: {"type": 1}, "not an envelope", id="type not a string"),
            pytest.param(
                lambda _: {"type": "event", "name": "x", "data": {}},
                'expected hello response, got "event" frame',
                id="an event",
            ),
            pytest.param(
                lambda _: {"type": "response", "id": "x"},
                "expected hello response",
                id="response without ok",
            ),
            pytest.param(
                lambda _: {"type": "response", "id": "other", "ok": True, "data": {}},
                'response id "other" does not match hello id',
                id="another id",
            ),
            pytest.param(
                lambda req_id: {"type": "response", "id": req_id, "ok": False, "error": {}},
                "error response without a code or message",
                id="error without code",
            ),
            pytest.param(
                lambda req_id: {"type": "response", "id": req_id, "ok": True, "data": {}},
                "hello result without server info",
                id="result without server",
            ),
            pytest.param(
                lambda req_id: {"type": "response", "id": req_id, "ok": True, "data": []},
                "hello result without server info",
                id="result not an object",
            ),
            pytest.param(
                lambda req_id: {
                    "type": "response",
                    "id": req_id,
                    "ok": True,
                    "data": {"server": {"name": "k", "version": "1"}, "protocol_version": "2"},
                },
                'hello result protocol_version "2", want "1"',
                id="another protocol version",
            ),
        ],
    )
    async def test_rejects_an_invalid_hello_response(
        self, fake_daemon: FakeDaemon, answer: object, want: str
    ) -> None:
        closed: asyncio.Queue[int | None] = asyncio.Queue()

        async def handle(peer: Peer) -> None:
            await peer.answer_hello(answer)  # type: ignore[arg-type]
            closed.put_nowait(await peer.close_code())

        with pytest.raises(InvalidFrameError, match=want):
            await dial(await fake_daemon(handle), HELLO)
        assert await closed.get() == 1000

    async def test_rejects_a_binary_hello_response(self, fake_daemon: FakeDaemon) -> None:
        async def handle(peer: Peer) -> None:
            await peer.next()
            await peer.send(b"\x00")

        with pytest.raises(InvalidFrameError, match="binary"):
            await dial(await fake_daemon(handle), HELLO)

    async def test_raises_closed_error_when_the_daemon_closes_before_answering(
        self, fake_daemon: FakeDaemon
    ) -> None:
        async def handle(peer: Peer) -> None:
            await peer.next()
            await peer.ws.close(1000)

        with pytest.raises(ClosedError):
            await dial(await fake_daemon(handle), HELLO)

    @pytest.mark.parametrize(
        "hello",
        [
            {},
            {"protocol_version": "2", "client": {"name": "a", "version": "1"}},
            {"protocol_version": "1"},
            {"protocol_version": "1", "client": {"name": "a"}},
            "hello",
        ],
    )
    async def test_rejects_invalid_hello_params_before_connecting(self, hello: object) -> None:
        with pytest.raises(TypeError, match="invalid hello params"):
            await dial("ws://127.0.0.1:1/ws", hello)  # type: ignore[arg-type]

    @pytest.mark.parametrize(
        "ws_url",
        [
            pytest.param(f"ws://127.0.0.1:{unused_port()}/ws?token={TOKEN}", id="refused"),
            pytest.param(f"not a url?token={TOKEN}", id="invalid url"),
            pytest.param(f"http://127.0.0.1/ws?token={TOKEN}", id="wrong scheme"),
            pytest.param(f"ws://[::1/ws?token={TOKEN}", id="unparsable url"),
            pytest.param("", id="empty url"),
        ],
    )
    async def test_never_leaks_the_token_when_the_connection_fails(self, ws_url: str) -> None:
        with pytest.raises(ConnectionError, match=r"^client\.dial: ") as exc:
            await dial(ws_url, HELLO)
        assert TOKEN not in str(exc.value)
        assert exc.value.__cause__ is None
        assert exc.value.__context__ is None

    async def test_closes_the_connection_when_cancelled_waiting_for_hello(
        self, fake_daemon: FakeDaemon
    ) -> None:
        closed: asyncio.Queue[int | None] = asyncio.Queue()

        async def handle(peer: Peer) -> None:
            await peer.next()
            closed.put_nowait(await peer.close_code())

        url = await fake_daemon(handle)
        with pytest.raises(TimeoutError):
            async with asyncio.timeout(0.2):
                await dial(url, HELLO)
        assert await closed.get() == 1000


class TestReadEvent:
    async def test_returns_each_event_and_raises_for_invalid_frames(
        self, fake_daemon: FakeDaemon
    ) -> None:
        conn, peers = await connected(fake_daemon)
        peer = await peers.get()
        frames: list[JsonObject | str | bytes] = [
            {"type": "event", "name": "first", "data": {"n": 1}},
            "nope",
            b"\x01",
            {"type": "response", "id": "1", "ok": True, "data": {}},
            {"type": "event", "name": "no data"},
            {"type": "event", "name": "second", "data": {}},
        ]
        for frame in frames:
            await peer.send(frame)

        assert await conn.read_event() == Event(name="first", data={"n": 1})
        for want in ["not JSON", "binary", 'unexpected "response" frame', "without a name or data"]:
            with pytest.raises(InvalidFrameError, match=want):
                await conn.read_event()
        assert await conn.read_event() == Event(name="second", data={})
        await conn.close()

    @pytest.mark.parametrize("code", [1000, 1001])
    async def test_raises_closed_error_after_a_normal_close(
        self, fake_daemon: FakeDaemon, code: int
    ) -> None:
        conn, peers = await connected(fake_daemon)
        await (await peers.get()).ws.close(code)

        with pytest.raises(ClosedError):
            await conn.read_event()

    async def test_raises_connection_error_after_an_abnormal_close(
        self, fake_daemon: FakeDaemon
    ) -> None:
        conn, peers = await connected(fake_daemon)
        await (await peers.get()).ws.close(1011, "boom")

        with pytest.raises(ConnectionError, match="closed abnormally"):
            await conn.read_event()

    async def test_raises_connection_error_when_the_connection_drops(
        self, fake_daemon: FakeDaemon
    ) -> None:
        conn, peers = await connected(fake_daemon)
        (await peers.get()).ws.transport.abort()

        with pytest.raises(ConnectionError, match="closed abnormally"):
            await conn.read_event()

    async def test_accepts_a_frame_at_the_read_limit_and_closes_on_a_larger_one(
        self, fake_daemon: FakeDaemon
    ) -> None:
        conn, peers = await connected(fake_daemon)
        peer = await peers.get()
        envelope = '{"type":"event","name":"big","data":{"pad":""}}'
        at_limit = envelope.replace('""', '"' + "x" * (READ_LIMIT - len(envelope)) + '"')
        assert len(at_limit) == READ_LIMIT
        await peer.send(at_limit)
        await peer.send(at_limit.replace('"x', '"xx'))

        assert (await conn.read_event()).name == "big"
        with pytest.raises(ConnectionError, match="1009"):
            await conn.read_event()
        assert await peer.close_code() == 1009

    async def test_cancelling_keeps_the_connection_usable(self, fake_daemon: FakeDaemon) -> None:
        conn, peers = await connected(fake_daemon)
        peer = await peers.get()

        with pytest.raises(TimeoutError):
            async with asyncio.timeout(0.05):
                await conn.read_event()
        await peer.send({"type": "event", "name": "after", "data": {}})

        assert (await conn.read_event()).name == "after"
        await conn.close()


class TestClose:
    async def test_closes_with_a_normal_closure(self, fake_daemon: FakeDaemon) -> None:
        conn, peers = await connected(fake_daemon)
        peer = await peers.get()

        await conn.close()

        assert await peer.close_code() == 1000
        with pytest.raises(ClosedError):
            await conn.read_event()
