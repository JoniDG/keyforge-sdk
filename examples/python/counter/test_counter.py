import json
import logging
from pathlib import Path

import pytest
from keyforge_protocol import InputAction, InputEvent, JsonObject
from keyforge_sdk.plugin import Invocation

from counter import VERSION, handlers, next_count

SILENT = logging.Logger("silent", logging.CRITICAL)


def key() -> InputEvent:
    return {
        "device_id": "VID_1_PID_2",
        "kind": "key",
        "input_id": "key_1",
        "action": "press",
        "timestamp_ms": 1,
    }


def encoder(action: InputAction) -> InputEvent:
    return {
        "device_id": "VID_1_PID_2",
        "kind": "encoder",
        "input_id": "encoder_0",
        "action": action,
        "timestamp_ms": 1,
    }


def invocation(context: str, event: InputEvent | None, params: JsonObject) -> Invocation:
    inv: Invocation = {"context": context, "action": {"id": "count", "params": params}}
    if event is not None:
        inv["input"] = event
    return inv


def test_manifest_matches_the_plugin() -> None:
    manifest = json.loads((Path(__file__).parent / "manifest.json").read_text(encoding="utf-8"))

    assert manifest["version"] == VERSION
    assert manifest["protocol_version"] == "1"
    assert manifest["entrypoint"]["darwin"] == {"path": "python3.15", "args": ["main.py"]}
    assert manifest["entrypoint"]["linux"] == {"path": "python3.15", "args": ["main.py"]}
    assert manifest["entrypoint"]["windows"] == {"path": "py", "args": ["-3.15", "main.py"]}
    assert sorted(a["id"] for a in manifest["actions"]) == sorted(handlers(SILENT))


@pytest.mark.parametrize(
    ("current", "event", "want"),
    [
        pytest.param(2, key(), 3, id="key press adds one"),
        pytest.param(2, None, 3, id="run from the app adds one"),
        pytest.param(2, encoder("rotate_cw"), 3, id="turn right adds one"),
        pytest.param(2, encoder("rotate_ccw"), 1, id="turn left takes one away"),
        pytest.param(0, encoder("rotate_ccw"), 0, id="never below zero"),
        pytest.param(7, encoder("click"), 0, id="press resets"),
    ],
)
def test_next_count(current: int, event: InputEvent | None, want: int) -> None:
    assert next_count(current, event) == want


async def test_keeps_one_counter_per_binding(tmp_path: Path) -> None:
    first, second = tmp_path / "first.txt", tmp_path / "second.txt"
    count = handlers(SILENT)["count"]

    await count(invocation("binding-1", key(), {"file": str(first)}))
    await count(invocation("binding-1", key(), {"file": str(first)}))
    await count(invocation("binding-2", key(), {"file": str(second)}))

    assert first.read_text(encoding="utf-8") == "2"
    assert second.read_text(encoding="utf-8") == "1"


async def test_shares_the_counter_between_bindings_with_the_same_label(tmp_path: Path) -> None:
    file = tmp_path / "deaths.txt"
    params: JsonObject = {"label": "Deaths", "file": str(file)}
    count = handlers(SILENT)["count"]

    # An encoder's three triggers are three bindings with their own contexts.
    await count(invocation("rotate-cw", encoder("rotate_cw"), params))
    await count(invocation("rotate-cw", encoder("rotate_cw"), params))
    await count(invocation("rotate-ccw", encoder("rotate_ccw"), params))
    assert file.read_text(encoding="utf-8") == "Deaths: 1"

    await count(invocation("click", encoder("click"), params))
    assert file.read_text(encoding="utf-8") == "Deaths: 0"


async def test_counts_without_writing_when_there_is_no_file() -> None:
    await handlers(SILENT)["count"](invocation("binding-1", None, {}))


@pytest.mark.parametrize(
    ("file", "error", "want"),
    [
        pytest.param("deaths.txt", ValueError, "must be an absolute path", id="relative path"),
        pytest.param(None, OSError, "counter.write_count", id="missing folder"),
    ],
)
async def test_rejects_a_bad_file(
    tmp_path: Path, file: str | None, error: type[Exception], want: str
) -> None:
    path = file or str(tmp_path / "nope" / "deaths.txt")

    with pytest.raises(error, match=want):
        await handlers(SILENT)["count"](invocation("binding-1", key(), {"file": path}))
