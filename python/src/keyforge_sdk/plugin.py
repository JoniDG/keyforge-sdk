"""Run a KeyForge plugin: the author writes one handler per action.

`run` reads the launch info the daemon passes in the KEYFORGE_PLUGIN_INFO
environment variable, connects, performs the hello handshake, dispatches each
action_invoked event to the handler registered for its action id and returns
when the daemon closes the connection, which is how the daemon stops plugins.

Handlers run one at a time, in the order the daemon fired them, so state kept
per `Invocation["context"]` (e.g. a toggle bound to two keys) needs no
coordination. A handler that needs to do slow work should start it in its own
task instead of awaiting it: up to 128 invocations wait behind a running
handler, and any that arrive past that are dropped and logged. A handler that
raises is logged and skipped; the plugin keeps running.
"""

import asyncio
import json
import logging
import os
import sys
from collections.abc import Awaitable, Callable, Mapping

from keyforge_protocol import (
    ActionInvokedData,
    InputAction,
    InputEvent,
    InputKind,
    JsonObject,
    JsonValue,
    PluginLaunchInfo,
)

from keyforge_sdk.client import ClosedError, Conn, InvalidFrameError, dial

__all__ = [
    "LAUNCH_INFO_ENV",
    "Handler",
    "Handlers",
    "Invocation",
    "LogfmtFormatter",
    "run",
    "text_logger",
]

LAUNCH_INFO_ENV = "KEYFORGE_PLUGIN_INFO"
"""The environment variable the daemon passes the launch info in."""

# How many invocations can wait behind a running handler before new ones are
# dropped. Large enough to absorb a fast encoder turn.
_QUEUE_SIZE = 128

_INPUT_KINDS: Mapping[str, InputKind] = {"key": "key", "encoder": "encoder"}
_INPUT_ACTIONS: Mapping[str, InputAction] = {
    "press": "press",
    "release": "release",
    "rotate_cw": "rotate_cw",
    "rotate_ccw": "rotate_ccw",
    "click": "click",
}

type Invocation = ActionInvokedData
"""One firing of a plugin action.

`context` identifies the binding that fired, `action["params"]` holds its
settings and `input` the hardware event (absent when the action was run from
the KeyForge app).
"""

type Handler = Callable[[Invocation], Awaitable[None]]
"""Runs one plugin action.

When the plugin is stopping, the handler's task is cancelled, so long work can
clean up on `asyncio.CancelledError`. Any other exception is logged.
"""

type Handlers = Mapping[str, Handler]
"""Maps an action id, as declared in the manifest (e.g. "play_pause", without
the "plugin.<id>." prefix), to its handler."""

type _Logger = logging.LoggerAdapter[logging.Logger]


async def run(
    *,
    version: str,
    handlers: Handlers,
    logger: logging.Logger | None = None,
) -> None:
    """Connect the plugin to the daemon and dispatch actions to handlers.

    It returns when the daemon closes the connection normally. Cancelling it
    (e.g. Ctrl+C under `asyncio.run`) cancels the running handler, closes the
    connection normally and re-raises the cancellation. Either way,
    invocations still queued at that point are dropped.

    Args:
        version: The plugin version sent in hello; keep it in sync with the
            manifest.
        handlers: One handler per action the manifest declares.
        logger: Receives handler errors and ignored frames. Defaults to
            `text_logger()`.

    Raises:
        RuntimeError: KEYFORGE_PLUGIN_INFO is missing or invalid, which means
            the process wasn't launched by the daemon.
        ConnectionError: the connection failed, the daemon rejected the hello
            (the `ServerError` is the cause) or closed the connection
            abnormally.
    """
    info = _launch_info(os.environ.get(LAUNCH_INFO_ENV))
    log = logging.LoggerAdapter(
        logger or text_logger(), {"plugin": info["plugin_id"]}, merge_extra=True
    )

    try:
        conn, _ = await dial(
            info["ws_url"],
            {
                "protocol_version": info["protocol_version"],
                "client": {"name": info["plugin_id"], "version": version},
            },
        )
    except Exception as exc:
        raise ConnectionError(f"plugin.run: {exc}") from exc

    try:
        await _serve(conn, handlers, log)
    finally:
        await conn.close()


def _launch_info(raw: str | None) -> PluginLaunchInfo:
    if not raw:
        raise RuntimeError(
            f"plugin.run: {LAUNCH_INFO_ENV} is not set; "
            "plugins must be launched by the KeyForge daemon"
        )
    # The value is never included in errors: ws_url carries the auth token.
    try:
        info: JsonValue = json.loads(raw)
    except ValueError:
        raise RuntimeError(f"plugin.run: invalid {LAUNCH_INFO_ENV}: not JSON") from None
    if (
        not isinstance(info, dict)
        or not isinstance(plugin_id := info.get("plugin_id"), str)
        or not isinstance(ws_url := info.get("ws_url"), str)
        or info.get("protocol_version") != "1"
    ):
        raise RuntimeError(
            f"plugin.run: invalid {LAUNCH_INFO_ENV}: "
            'want plugin_id, ws_url and protocol_version "1"'
        )
    return {"plugin_id": plugin_id, "ws_url": ws_url, "protocol_version": "1"}


# _serve reads events in one task and runs handlers in another, so a closed
# connection is noticed (and the running handler cancelled) even while a
# handler is busy.
async def _serve(conn: Conn, handlers: Handlers, log: _Logger) -> None:
    queue: asyncio.Queue[Invocation] = asyncio.Queue(_QUEUE_SIZE)
    reader = asyncio.create_task(_read(conn, queue, log))
    dispatcher = asyncio.create_task(_dispatch_loop(handlers, queue, log))
    try:
        read_err = await reader
    finally:
        # On cancellation the reader is stopped too (cancelling a read is
        # safe) and run closes the connection normally.
        for task in (reader, dispatcher):
            task.cancel()
        await asyncio.gather(reader, dispatcher, return_exceptions=True)

    if isinstance(read_err, ClosedError):
        return
    raise ConnectionError(f"plugin.run: {read_err}") from read_err


# _read queues action_invoked events until the connection is gone, and returns
# the error that ended it. It never waits on a full queue (it drops the
# invocation instead), so a closed connection is always noticed.
async def _read(conn: Conn, queue: asyncio.Queue[Invocation], log: _Logger) -> Exception:
    while True:
        try:
            ev = await conn.read_event()
        except InvalidFrameError as exc:
            log.warning("ignoring invalid frame", extra={"error": str(exc)})
            continue
        except Exception as exc:  # noqa: BLE001 - whatever ended the connection is returned
            return exc
        if ev.name != "action_invoked":
            log.debug("ignoring event", extra={"event": ev.name})
            continue
        inv = _invocation(ev.data)
        if inv is None:
            log.warning("ignoring invalid action_invoked")
            continue
        try:
            queue.put_nowait(inv)
        except asyncio.QueueFull:
            log.warning(
                "dropping action_invoked: handlers are not keeping up",
                extra={"action": inv["action"]["id"], "context": inv["context"]},
            )


# _invocation checks the fields the SDK and handlers read (and the ones the
# schema requires of them) and builds the typed invocation from them.
def _invocation(data: JsonObject) -> Invocation | None:
    action = data.get("action")
    if (
        not isinstance(context := data.get("context"), str)
        or not isinstance(action, dict)
        or not isinstance(action_id := action.get("id"), str)
        or not isinstance(params := action.get("params"), dict)
    ):
        return None
    inv: Invocation = {"context": context, "action": {"id": action_id, "params": params}}
    if "input" not in data:
        return inv
    event = _input_event(data["input"])
    if event is None:
        return None
    inv["input"] = event
    return inv


def _input_event(value: JsonValue) -> InputEvent | None:
    if not isinstance(value, dict):
        return None
    kind = _INPUT_KINDS.get(_str(value.get("kind")))
    action = _INPUT_ACTIONS.get(_str(value.get("action")))
    timestamp = value.get("timestamp_ms")
    if (
        not isinstance(device_id := value.get("device_id"), str)
        or not isinstance(input_id := value.get("input_id"), str)
        or kind is None
        or action is None
        # bool is an int in Python, but not a JSON integer.
        or not isinstance(timestamp, int)
        or isinstance(timestamp, bool)
    ):
        return None
    return {
        "device_id": device_id,
        "kind": kind,
        "input_id": input_id,
        "action": action,
        "timestamp_ms": timestamp,
    }


def _str(value: JsonValue) -> str:
    return value if isinstance(value, str) else ""


async def _dispatch_loop(
    handlers: Handlers, queue: asyncio.Queue[Invocation], log: _Logger
) -> None:
    task = asyncio.current_task()
    # A handler that swallows its CancelledError would otherwise keep the loop
    # (and run) waiting for invocations that never come.
    while task is None or not task.cancelling():
        await _dispatch(handlers, await queue.get(), log)


async def _dispatch(handlers: Handlers, inv: Invocation, log: _Logger) -> None:
    action_id = inv["action"]["id"]
    handler = handlers.get(action_id)
    if handler is None:
        log.warning("no handler for action", extra={"action": action_id})
        return
    try:
        await handler(inv)
    except asyncio.CancelledError as exc:
        task = asyncio.current_task()
        if task is not None and task.cancelling():
            raise
        # Not the SDK stopping the plugin (e.g. the handler awaited a task
        # someone cancelled): a failed action like any other, or it would kill
        # the dispatcher and leave the plugin connected but deaf.
        _log_failure(log, inv, exc)
    except Exception as exc:  # noqa: BLE001 - logged with its traceback
        _log_failure(log, inv, exc)


def _log_failure(log: _Logger, inv: Invocation, exc: BaseException) -> None:
    log.exception(
        "action failed",
        extra={"action": inv["action"]["id"], "context": inv["context"], "error": str(exc)},
    )


# The level names Go's slog and the Node SDK print.
_LEVELS = {"WARNING": "WARN"}

# Attributes every LogRecord has; anything else came in through extra=.
_RECORD_ATTRS = frozenset(vars(logging.makeLogRecord({}))) | {"message", "asctime"}


class LogfmtFormatter(logging.Formatter):
    """Formats records as logfmt lines, like the Go SDK's default logger.

    A line looks like `level=INFO msg="..." key=value`, with one key=value per
    attribute passed through `extra=`. A record logged with exc_info is
    followed by its traceback.
    """

    def format(self, record: logging.LogRecord) -> str:
        """Format record as one logfmt line, plus its traceback if any."""
        fields = {
            "level": _LEVELS.get(record.levelname, record.levelname),
            "msg": record.getMessage(),
        }
        fields |= {
            key: _logfmt_text(value)
            for key, value in vars(record).items()
            if key not in _RECORD_ATTRS
        }
        line = " ".join(f"{key}={_logfmt_value(value)}" for key, value in fields.items())
        if record.exc_info:
            line += "\n" + self.formatException(record.exc_info)
        return line


def text_logger(*, debug: bool = False) -> logging.Logger:
    """Return a logger that writes logfmt lines to stderr.

    Debug lines are skipped unless debug is true. stdout is left to the
    plugin. The logger is not registered with `logging.getLogger`, so it
    doesn't touch the application's logging setup.
    """
    logger = logging.Logger("keyforge_sdk.plugin", logging.DEBUG if debug else logging.INFO)
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(LogfmtFormatter())
    logger.addHandler(handler)
    return logger


def _logfmt_text(value: object) -> str:
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value)
    except TypeError, ValueError:
        # Not JSON (e.g. an object): a log line must never raise.
        return str(value)


def _logfmt_value(text: str) -> str:
    if text and not any(c.isspace() or c in '"=' for c in text):
        return text
    return json.dumps(text)
