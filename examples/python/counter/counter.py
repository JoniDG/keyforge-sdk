"""The counter plugin: a counter for streams (deaths, wins, coffees).

A key adds one; on an encoder, turning right adds one, turning left takes one
away and pressing resets it. The count can be written to a text file that OBS
shows as a text source. Same plugin as examples/go/counter.
"""

import logging
import os
from pathlib import Path

from keyforge_protocol import InputEvent
from keyforge_sdk.plugin import Handlers, Invocation

VERSION = "0.1.0"
"""Must match "version" in manifest.json."""


def handlers(logger: logging.Logger) -> Handlers:
    """Return the plugin's handlers, one per action in manifest.json."""
    # Handlers run one at a time, so the dict needs no lock.
    counts: dict[str, int] = {}

    async def count(inv: Invocation) -> None:
        label = _param(inv, "label")
        # Each binding has its own context, and an encoder needs one binding
        # per trigger. Bindings that share a label share the count, which is
        # how an encoder's three bindings drive one counter.
        key = f"label:{label}" if label else inv["context"]
        counts[key] = next_count(counts.get(key, 0), inv.get("input"))

        logger.info("count", extra={"label": label, "count": counts[key]})
        if file := _param(inv, "file"):
            write_count(file, label, counts[key])

    return {"count": count}


def _param(inv: Invocation, name: str) -> str:
    """Return a string param of the binding, or "" when it is not set."""
    value = inv["action"]["params"].get(name)
    return value if isinstance(value, str) else ""


def next_count(count: int, event: InputEvent | None) -> int:
    """Return the count after event.

    A missing event means the action was run from the KeyForge app rather than
    by hardware.
    """
    match event["action"] if event else None:
        case "rotate_ccw":
            return max(count - 1, 0)
        case "click":
            return 0
        case _:
            return count + 1


def write_count(file: str, label: str, count: int) -> None:
    """Write the count to file, prefixed with label when there is one."""
    # The daemon starts the plugin inside its install folder, so a relative
    # path would land there, and installing a new version replaces it.
    path = Path(file)
    if not path.is_absolute():
        raise ValueError(f"counter.write_count: param file must be an absolute path, got {file!r}")
    text = f"{label}: {count}" if label else str(count)
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
    except OSError as exc:
        raise OSError(f"counter.write_count: {exc}") from exc
