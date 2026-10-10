"""Entry point the daemon runs: python3.15 main.py, from the plugin folder."""

import sys
from pathlib import Path

# The plugin ships its dependencies in vendor/ (see python/README.md in
# keyforge-sdk), so it runs on any Python 3.15 without a virtual environment.
sys.path.insert(0, str(Path(__file__).resolve().parent / "vendor"))

import asyncio
import signal

from keyforge_sdk.plugin import run, text_logger

from counter import VERSION, handlers


async def main() -> int:
    """Run the plugin until the daemon stops it, and return the exit code."""
    logger = text_logger()
    task = asyncio.current_task()
    if task is not None and sys.platform != "win32":
        asyncio.get_running_loop().add_signal_handler(signal.SIGTERM, task.cancel)
    try:
        await run(version=VERSION, handlers=handlers(logger), logger=logger)
    except asyncio.CancelledError:
        return 0
    except Exception as exc:  # noqa: BLE001 - any failure stops the plugin
        logger.error("plugin stopped", extra={"error": str(exc)})  # noqa: TRY400
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
