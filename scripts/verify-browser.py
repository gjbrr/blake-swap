#!/usr/bin/env python3
"""Drive the browser client with the DevTools protocol.

The shell check in `verify-hello.sh` uses `--dump-dom`, which dumps when the
virtual-time budget expires. That is fine for a page that answers in
milliseconds and wrong for one that does real work: the worker's encrypted
self-test derives a key and seals a record, and a dump taken before it finishes
looks exactly like a broken client.

This script instead attaches to a running headless browser, navigates, and polls
the document until the flags the page sets are present, on real time. It asserts
the same two facts the shell check asserts — the greeting came back and the boot
gate cleared — plus the encrypted round trip.

Usage: scripts/verify-browser.py [--port PORT] [--budget SECONDS]
Exit:  0 pass, 1 fail, 77 skipped (no browser).
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

import asyncio

try:
    import websockets
except ImportError:  # pragma: no cover - the venv has it
    print("SKIP: the websockets package is not installed", file=sys.stderr)
    raise SystemExit(77)


def find_browser() -> str:
    override = os.environ.get("BROWSER", "")

    if override and shutil.which(override):
        return override

    for candidate in (
        "chromium",
        "chromium-browser",
        "google-chrome",
        "google-chrome-stable",
        "brave-browser",
    ):
        found = shutil.which(candidate)

        if found:
            return found

    return ""


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))

        return sock.getsockname()[1]


async def evaluate(ws, expression: str, counter: list[int]) -> str:
    counter[0] += 1
    message_id = counter[0]

    await ws.send(
        json.dumps(
            {
                "id": message_id,
                "method": "Runtime.evaluate",
                "params": {"expression": expression, "returnByValue": True},
            }
        )
    )

    while True:
        raw = await asyncio.wait_for(ws.recv(), timeout=30)
        response = json.loads(raw)

        if response.get("id") != message_id:
            continue

        result = response.get("result", {}).get("result", {})

        return result.get("value")


async def poll(ws, expression: str, budget: float) -> object:
    counter = [0]
    deadline = time.time() + budget
    last = None

    while time.time() < deadline:
        last = await evaluate(ws, expression, counter)

        if last:
            return last

        await asyncio.sleep(0.25)

    return last


async def drive(url: str, budget: float) -> int:
    targets = json.load(urllib.request.urlopen(f"{url}/json/list"))
    pages = [target for target in targets if target.get("type") == "page"]

    if not pages:
        print("FAIL: the browser opened no page target", file=sys.stderr)

        return 1

    ws_url = pages[0]["webSocketDebuggerUrl"]

    async with websockets.connect(ws_url, max_size=8 << 20) as ws:
        await ws.send(json.dumps({"id": 1, "method": "Runtime.enable", "params": {}}))

        while True:
            message = json.loads(await asyncio.wait_for(ws.recv(), timeout=10))

            if message.get("id") == 1:
                break

        hello = await poll(
            ws,
            "document.body.dataset.helloVerified || ''",
            budget,
        )

        if hello != "true":
            dom = await evaluate(ws, "document.body.innerText.slice(0, 800)", [100])

            print(f"FAIL: the greeting never rendered; page text:\n{dom}", file=sys.stderr)

            return 1

        print("PASS: hello round trip rendered")

        boot = await evaluate(ws, "document.body.dataset.bootComplete || ''", [200])

        if boot != "true":
            print("FAIL: the boot gate never cleared", file=sys.stderr)

            return 1

        print("PASS: boot gate cleared")

        vault = await poll(
            ws,
            "document.body.dataset.vaultVerified || ''",
            budget,
        )

        text = await evaluate(ws, "document.getElementById('vault').innerText", [300])

        if vault != "true":
            print(f"FAIL: the encrypted self-test never finished; panel:\n{text}", file=sys.stderr)

            return 1

        print(f"PASS: encrypted round trip in the browser: {text}")

    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--budget", type=float, default=90.0)
    parser.add_argument("--keep", action="store_true")

    args = parser.parse_args()

    browser = find_browser()

    if not browser:
        print("SKIP: no chromium/chrome found; set BROWSER to run the check", file=sys.stderr)

        return 77

    if not (ROOT / "web" / "page.wasm").is_file() or not (ROOT / "web" / "worker.wasm").is_file():
        print("ERROR: wasm modules missing; run make wasm", file=sys.stderr)

        return 1

    work = Path(tempfile.mkdtemp(prefix="blake-swap-browser-"))
    app_port = free_port()
    debug_port = free_port()

    binary = ROOT / "bin" / "blake-swap-app"

    if not binary.is_file():
        subprocess.run(["make", "server"], cwd=ROOT, check=True, stdout=subprocess.DEVNULL)

    server = subprocess.Popen(
        [str(ROOT / "bin" / "blake-swap-app"), "-addr", f"127.0.0.1:{app_port}", "-dir", "web"],
        cwd=ROOT,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    profile = work / "profile"

    chromium = subprocess.Popen(
        [
            browser,
            "--headless=new",
            "--no-sandbox",
            "--disable-gpu",
            f"--remote-debugging-port={debug_port}",
            f"--user-data-dir={profile}",
            f"http://127.0.0.1:{app_port}/?vaulttest=1",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    try:
        deadline = time.time() + 20

        while time.time() < deadline:
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{debug_port}/json/version", timeout=1)
                break
            except Exception:
                time.sleep(0.2)
        else:
            print("FAIL: the browser never opened a debugging port", file=sys.stderr)

            return 1

        return asyncio.run(drive(f"http://127.0.0.1:{debug_port}", args.budget))
    finally:
        for process in (chromium, server):
            process.terminate()

            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()

        if not args.keep:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
