import asyncio
import json
import logging
import sys

import pytest
from keyforge_protocol import JsonObject, JsonValue

from keyforge_sdk.client import ServerError
from keyforge_sdk.plugin import (
    LAUNCH_INFO_ENV,
    Handlers,
    Invocation,
    LogfmtFormatter,
    run,
    text_logger,
)

from .conftest import TOKEN, FakeDaemon, Peer

PLUGIN_ID = "dev.test.plugin"


class Records(logging.Handler):
    """Keeps every record logged, with its extra attributes."""

    def __init__(self) -> None:
        super().__init__(logging.DEBUG)
        self.records: list[logging.LogRecord] = []
        self._logged = asyncio.Event()

    def emit(self, record: logging.LogRecord) -> None:
        self.records.append(record)
        self._logged.set()

    async def wait_for(self, level: int, msg: str) -> None:
        while msg not in self.messages(level):
            self._logged.clear()
            await self._logged.wait()

    def messages(self, level: int) -> list[str]:
        return [r.getMessage() for r in self.records if r.levelno == level]


def capture() -> tuple[logging.Logger, Records]:
    logger = logging.Logger("test", logging.DEBUG)
    records = Records()
    logger.addHandler(records)
    return logger, records


def launch(monkeypatch: pytest.MonkeyPatch, ws_url: str) -> None:
    monkeypatch.setenv(
        LAUNCH_INFO_ENV,
        json.dumps({"plugin_id": PLUGIN_ID, "ws_url": ws_url, "protocol_version": "1"}),
    )


def invoked(action: str, context: str = "ctx", **extra: JsonValue) -> JsonObject:
    data: JsonObject = {"context": context, "action": {"id": action, "params": {}}}
    data.update(extra)
    return {"type": "event", "name": "action_invoked", "data": data}


def key_input(action: str = "press") -> JsonObject:
    return {
        "device_id": "VID_1_PID_2",
        "kind": "key",
        "input_id": "key_1",
        "action": action,
        "timestamp_ms": 1,
    }


async def daemon_that(
    fake_daemon: FakeDaemon,
    script: list[JsonObject | str | bytes],
    *,
    close: int | None = 1000,
    after: asyncio.Event | None = None,
) -> tuple[str, asyncio.Queue[Peer]]:
    """Start a daemon that accepts hello, sends script and then closes with close.

    With after, it waits for that event before closing: a close cancels the
    running handler and drops the queue, so tests that need every handler to
    run close once the last one did.
    """
    peers: asyncio.Queue[Peer] = asyncio.Queue()

    async def handle(peer: Peer) -> None:
        await peer.accept_hello()
        for frame in script:
            await peer.send(frame)
        peers.put_nowait(peer)
        if after is not None:
            await after.wait()
        if close is not None:
            await peer.ws.close(close)

    return await fake_daemon(handle), peers


class TestRun:
    async def test_dispatches_in_order_until_the_daemon_closes(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        hello: list[JsonObject] = []
        done = asyncio.Event()

        async def handle(peer: Peer) -> None:
            hello.append(await peer.accept_hello())
            await peer.send(invoked("a", "1", input=key_input()))
            await peer.send(invoked("b", "2"))
            await peer.send(invoked("a", "3", input=key_input("release")))
            await done.wait()
            await peer.ws.close(1000)

        launch(monkeypatch, await fake_daemon(handle))
        seen: list[Invocation] = []

        async def record(inv: Invocation) -> None:
            seen.append(inv)
            if len(seen) == 3:
                done.set()

        logger, _ = capture()
        await run(version="1.2.3", handlers={"a": record, "b": record}, logger=logger)

        assert hello[0]["params"] == {
            "protocol_version": "1",
            "client": {"name": PLUGIN_ID, "version": "1.2.3"},
        }
        assert [inv["context"] for inv in seen] == ["1", "2", "3"]
        assert seen[0] == {
            "context": "1",
            "action": {"id": "a", "params": {}},
            "input": key_input(),
        }
        assert "input" not in seen[1]

    async def test_runs_handlers_one_at_a_time(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        done = asyncio.Event()
        url, _ = await daemon_that(
            fake_daemon, [invoked("slow", "1"), invoked("slow", "2")], after=done
        )
        launch(monkeypatch, url)
        trace: list[str] = []

        async def slow(inv: Invocation) -> None:
            trace.append(f"start {inv['context']}")
            await asyncio.sleep(0.02)
            trace.append(f"end {inv['context']}")
            if inv["context"] == "2":
                done.set()

        await run(version="1", handlers={"slow": slow}, logger=capture()[0])

        assert trace == ["start 1", "end 1", "start 2", "end 2"]

    async def test_logs_failures_and_ignored_frames_and_keeps_running(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        done = asyncio.Event()
        url, _ = await daemon_that(
            fake_daemon,
            [
                invoked("fail", "1"),
                invoked("unknown"),
                "not json",
                {"type": "event", "name": "something_else", "data": {}},
                {"type": "event", "name": "action_invoked", "data": {"context": 1}},
                invoked("ok", input={**key_input(), "kind": "slider"}),
                invoked("ok", input={**key_input(), "timestamp_ms": True}),
                invoked("ok", input="key"),
                invoked("ok", "last"),
            ],
            after=done,
        )
        launch(monkeypatch, url)
        ran: list[str] = []

        async def fail(_: Invocation) -> None:
            raise ValueError("boom")

        async def ok(inv: Invocation) -> None:
            ran.append(inv["context"])
            done.set()

        logger, records = capture()
        await run(version="1", handlers={"fail": fail, "ok": ok}, logger=logger)

        assert ran == ["last"]
        assert records.messages(logging.ERROR) == ["action failed"]
        failed = next(r for r in records.records if r.levelno == logging.ERROR)
        assert failed.__dict__["error"] == "boom"
        assert failed.__dict__["plugin"] == PLUGIN_ID
        assert failed.exc_info is not None
        # The reader and the handlers run in separate tasks, so their lines interleave.
        assert sorted(records.messages(logging.WARNING)) == [
            "ignoring invalid action_invoked",
            "ignoring invalid action_invoked",
            "ignoring invalid action_invoked",
            "ignoring invalid action_invoked",
            "ignoring invalid frame",
            "no handler for action",
        ]
        assert records.messages(logging.DEBUG) == ["ignoring event"]

    async def test_drops_invocations_past_the_queue_while_a_handler_runs(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        release = asyncio.Event()
        started = asyncio.Event()
        # The first one runs, 128 wait in the queue and the last one is dropped.
        script: list[JsonObject | str | bytes] = [invoked("block", "first")]
        script += [invoked("count", str(i)) for i in range(128)]
        script += [invoked("count", "dropped")]

        peers: asyncio.Queue[Peer] = asyncio.Queue()

        async def handle(peer: Peer) -> None:
            await peer.accept_hello()
            await peer.send(script[0])
            await started.wait()
            for frame in script[1:]:
                await peer.send(frame)
            peers.put_nowait(peer)

        launch(monkeypatch, await fake_daemon(handle))
        counted: list[str] = []

        async def block(_: Invocation) -> None:
            started.set()
            await release.wait()

        async def count(inv: Invocation) -> None:
            counted.append(inv["context"])
            if len(counted) == 128:
                await peer.ws.close(1000)

        logger, records = capture()
        task = asyncio.create_task(
            run(version="1", handlers={"block": block, "count": count}, logger=logger)
        )
        peer = await peers.get()
        await records.wait_for(
            logging.WARNING, "dropping action_invoked: handlers are not keeping up"
        )
        release.set()
        await task

        assert counted == [str(i) for i in range(128)]
        dropped = next(r for r in records.records if r.levelno == logging.WARNING)
        assert dropped.__dict__["context"] == "dropped"
        assert dropped.__dict__["action"] == "count"

    async def test_cancels_the_running_handler_and_drops_the_queue_when_the_daemon_closes(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        url, _ = await daemon_that(
            fake_daemon, [invoked("block", "1"), invoked("block", "2"), invoked("block", "3")]
        )
        launch(monkeypatch, url)
        started: list[str] = []
        cancelled: list[str] = []

        async def block(inv: Invocation) -> None:
            started.append(inv["context"])
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                cancelled.append(inv["context"])
                raise

        await run(version="1", handlers={"block": block}, logger=capture()[0])

        assert started == ["1"]
        assert cancelled == ["1"]

    async def test_stops_even_if_a_handler_swallows_its_cancellation(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        url, _ = await daemon_that(
            fake_daemon, [invoked("stubborn", "1"), invoked("stubborn", "2")]
        )
        launch(monkeypatch, url)
        started: list[str] = []

        async def stubborn(inv: Invocation) -> None:
            started.append(inv["context"])
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                return

        async with asyncio.timeout(5):
            await run(version="1", handlers={"stubborn": stubborn}, logger=capture()[0])

        assert started == ["1"]

    async def test_keeps_running_when_a_handler_raises_a_foreign_cancellation(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        done = asyncio.Event()
        url, _ = await daemon_that(fake_daemon, [invoked("inner"), invoked("ok")], after=done)
        launch(monkeypatch, url)

        async def inner(_: Invocation) -> None:
            # Awaits a task someone else cancelled: not the SDK stopping.
            other = asyncio.create_task(asyncio.Event().wait())
            other.cancel()
            await other

        async def ok(_: Invocation) -> None:
            done.set()

        logger, records = capture()
        async with asyncio.timeout(5):
            await run(version="1", handlers={"inner": inner, "ok": ok}, logger=logger)

        assert records.messages(logging.ERROR) == ["action failed"]

    async def test_cancelling_closes_normally_and_cancels_the_handler(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        url, peers = await daemon_that(fake_daemon, [invoked("block")], close=None)
        launch(monkeypatch, url)
        started = asyncio.Event()
        cancelled = asyncio.Event()

        async def block(_: Invocation) -> None:
            started.set()
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                cancelled.set()
                raise

        task = asyncio.create_task(run(version="1", handlers={"block": block}, logger=capture()[0]))
        await started.wait()
        task.cancel()

        with pytest.raises(asyncio.CancelledError):
            await task
        assert cancelled.is_set()
        assert await (await peers.get()).close_code() == 1000

    async def test_raises_when_the_connection_drops(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        url, _ = await daemon_that(fake_daemon, [], close=1011)
        launch(monkeypatch, url)

        with pytest.raises(ConnectionError, match=r"^plugin\.run: .*closed abnormally"):
            await run(version="1", handlers={}, logger=capture()[0])

    async def test_raises_when_the_daemon_rejects_hello(
        self, fake_daemon: FakeDaemon, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        async def handle(peer: Peer) -> None:
            await peer.answer_hello(
                lambda req_id: {
                    "type": "response",
                    "id": req_id,
                    "ok": False,
                    "error": {"code": "FORBIDDEN", "message": "unknown plugin"},
                }
            )

        launch(monkeypatch, await fake_daemon(handle))

        with pytest.raises(ConnectionError, match="FORBIDDEN") as exc:
            await run(version="1", handlers={}, logger=capture()[0])
        assert isinstance(exc.value.__cause__, ServerError)

    @pytest.mark.parametrize(
        ("value", "want"),
        [
            pytest.param(None, "is not set", id="unset"),
            pytest.param("", "is not set", id="empty"),
            pytest.param(f"ws://x?token={TOKEN}", "not JSON", id="not JSON"),
            pytest.param(
                json.dumps({"plugin_id": PLUGIN_ID, "protocol_version": "1"}),
                "want plugin_id, ws_url",
                id="no ws_url",
            ),
            pytest.param(
                json.dumps(
                    {
                        "plugin_id": PLUGIN_ID,
                        "ws_url": f"ws://x?token={TOKEN}",
                        "protocol_version": "2",
                    }
                ),
                "want plugin_id, ws_url",
                id="another protocol version",
            ),
            pytest.param("[]", "want plugin_id, ws_url", id="not an object"),
        ],
    )
    async def test_raises_when_not_launched_by_the_daemon(
        self, monkeypatch: pytest.MonkeyPatch, value: str | None, want: str
    ) -> None:
        if value is None:
            monkeypatch.delenv(LAUNCH_INFO_ENV, raising=False)
        else:
            monkeypatch.setenv(LAUNCH_INFO_ENV, value)

        with pytest.raises(RuntimeError, match=want) as exc:
            await run(version="1", handlers={})
        assert TOKEN not in str(exc.value)

    async def test_uses_the_text_logger_by_default(
        self,
        fake_daemon: FakeDaemon,
        monkeypatch: pytest.MonkeyPatch,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        done = asyncio.Event()
        url, _ = await daemon_that(fake_daemon, [invoked("missing"), invoked("done")], after=done)
        launch(monkeypatch, url)

        async def finish(_: Invocation) -> None:
            done.set()

        handlers: Handlers = {"done": finish}

        await run(version="1", handlers=handlers)

        assert (
            f'level=WARN msg="no handler for action" plugin={PLUGIN_ID} action=missing'
            in capsys.readouterr().err
        )


class TestTextLogger:
    def test_writes_logfmt_lines_to_stderr_and_skips_debug(
        self, capsys: pytest.CaptureFixture[str]
    ) -> None:
        logger = text_logger()
        logger.debug("hidden")
        logger.info("count", extra={"label": "Deaths", "count": 3, "file": "a b", "empty": ""})

        captured = capsys.readouterr()
        assert captured.out == ""
        assert captured.err == 'level=INFO msg=count label=Deaths count=3 file="a b" empty=""\n'

    def test_writes_debug_when_asked(self, capsys: pytest.CaptureFixture[str]) -> None:
        text_logger(debug=True).debug("shown", extra={"q": 'say "hi"', "eq": "a=b"})

        assert capsys.readouterr().err == 'level=DEBUG msg=shown q="say \\"hi\\"" eq="a=b"\n'

    def test_never_raises_on_values_that_are_not_json(
        self, capsys: pytest.CaptureFixture[str]
    ) -> None:
        text_logger().info("odd", extra={"obj": object, "nums": [1, 2]})

        assert (
            capsys.readouterr().err == 'level=INFO msg=odd obj="<class \'object\'>" nums="[1, 2]"\n'
        )

    def test_appends_the_traceback(self) -> None:
        record = logging.LogRecord("x", logging.ERROR, __file__, 1, "failed", None, None)
        try:
            int("boom")
        except ValueError:
            record.exc_info = sys.exc_info()

        lines = LogfmtFormatter().format(record).splitlines()

        assert lines[0] == "level=ERROR msg=failed"
        assert lines[-1].startswith("ValueError: invalid literal")
