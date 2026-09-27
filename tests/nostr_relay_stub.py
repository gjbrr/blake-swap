#!/usr/bin/env python3
"""
In-process Nostr relay stub for the blake-swap test suites.

The engine speaks NIP-01 over `websockets`: it publishes kind-1 events with
EVENT and reads them back with REQ/EOSE. Pointing
`BLAKE_SWAP_NOSTR_RELAYS` at this stub lets the Nostr transport, marketplace
discovery, and event-cache code paths be exercised **without touching any
public relay**, which is both faster and safer than the alternative.

Supported subset:
  * ["EVENT", event]            -> stored, answered with ["OK", id, true]
  * ["REQ", sub_id, *filters]   -> matching events, then ["EOSE", sub_id]
  * ["CLOSE", sub_id]           -> acknowledged by doing nothing further

Filter matching covers the fields the engine actually uses: ids, authors,
kinds, and single-letter tag filters such as `#d`, plus `limit`.

A stub is deliberately not a correct relay: it has no signatures of its own,
no auth, no persistence, and it accepts whatever it is given so that the
*client's* verification logic is what gets tested.
"""

from __future__ import annotations

import asyncio
import json
import threading
import time
from typing import Any

from websockets.asyncio.server import serve


class NostrRelayStub:
    """A minimal in-memory NIP-01 relay running on its own event loop."""

    def __init__(self) -> None:
        self.events: dict[str, dict] = {}
        self.url: str | None = None

        # Observability for assertions.
        self.connections = 0
        self.published = 0
        self.requests = 0
        self.closed_subscriptions = 0

        self._loop: asyncio.AbstractEventLoop | None = None
        self._stop_event: asyncio.Event | None = None
        self._thread: threading.Thread | None = None

    # -- lifecycle --------------------------------------------------

    def start(self, timeout: float = 10.0) -> str:
        if self.url is not None:
            return self.url

        self._thread = threading.Thread(
            target=self._thread_main,
            name="nostr-relay-stub",
            daemon=True,
        )
        self._thread.start()

        deadline = time.time() + timeout

        while self.url is None and time.time() < deadline:
            time.sleep(0.02)

        if self.url is None:
            raise RuntimeError("Nostr relay stub did not start")

        return self.url

    def stop(self, timeout: float = 5.0) -> None:
        if self._loop is not None and self._stop_event is not None:
            self._loop.call_soon_threadsafe(self._stop_event.set)

        if self._thread is not None:
            self._thread.join(timeout=timeout)
            self._thread = None

    def __enter__(self) -> "NostrRelayStub":
        self.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.stop()

    # -- internals --------------------------------------------------

    def _thread_main(self) -> None:
        loop = asyncio.new_event_loop()
        self._loop = loop
        asyncio.set_event_loop(loop)

        try:
            loop.run_until_complete(self._serve())
        finally:
            try:
                loop.close()
            except Exception:
                pass

    async def _serve(self) -> None:
        self._stop_event = asyncio.Event()

        # Port 0 lets the OS pick a free port, so suites never collide.
        async with serve(self._handler, "127.0.0.1", 0) as server:
            sock = server.sockets[0]
            _host, port = sock.getsockname()[:2]
            self.url = f"ws://127.0.0.1:{port}"

            await self._stop_event.wait()

    async def _handler(self, websocket) -> None:
        self.connections += 1

        try:
            async for raw in websocket:
                await self._handle_message(websocket, raw)
        except Exception:
            # A client that walks away mid-exchange is normal here.
            pass

    async def _handle_message(self, websocket, raw) -> None:
        try:
            message = json.loads(raw)
        except Exception:
            return

        if not isinstance(message, list) or not message:
            return

        verb = message[0]

        if verb == "EVENT" and len(message) >= 2:
            event = message[1]

            if not isinstance(event, dict):
                return

            event_id = event.get("id")

            if not isinstance(event_id, str):
                return

            self.events[event_id] = event
            self.published += 1

            await websocket.send(
                json.dumps(["OK", event_id, True, ""])
            )
            return

        if verb == "REQ" and len(message) >= 2:
            sub_id = message[1]
            filters = [
                f for f in message[2:]
                if isinstance(f, dict)
            ]

            self.requests += 1

            for event in self._match(filters):
                await websocket.send(
                    json.dumps(["EVENT", sub_id, event])
                )

            await websocket.send(json.dumps(["EOSE", sub_id]))
            return

        if verb == "CLOSE" and len(message) >= 2:
            self.closed_subscriptions += 1
            return

    def _match(self, filters: list[dict]) -> list[dict]:
        """
        Return stored events matching any filter.

        A limit takes the NEWEST events, as a real relay does — returning
        the oldest N instead would silently hide recently published
        events once enough history accumulated.
        """
        if not filters:
            filters = [{}]

        selected: dict[str, dict] = {}
        limits = []

        for filt in filters:
            for event in self.events.values():
                if self._event_matches(event, filt):
                    selected[event["id"]] = event

            limit = filt.get("limit")

            if isinstance(limit, int) and limit > 0:
                limits.append(limit)

        # Newest first, so a limit keeps the most recent.
        events = sorted(
            selected.values(),
            key=lambda e: (int(e.get("created_at", 0)), e.get("id", "")),
            reverse=True,
        )

        if limits:
            events = events[: min(limits)]

        # Hand back oldest-first, which is the order a client expects to
        # reason about.
        events.reverse()

        return events

    @staticmethod
    def _event_matches(event: dict, filt: dict) -> bool:
        for key, expected in filt.items():
            if key in ("limit", "since", "until"):
                continue

            if key == "ids":
                if event.get("id") not in expected:
                    return False
                continue

            if key == "authors":
                if event.get("pubkey") not in expected:
                    return False
                continue

            if key == "kinds":
                if event.get("kind") not in expected:
                    return False
                continue

            if key.startswith("#"):
                tag_name = key[1:]
                wanted = expected if isinstance(expected, list) else [expected]

                values = [
                    tag[1]
                    for tag in event.get("tags", [])
                    if len(tag) >= 2 and tag[0] == tag_name
                ]

                if not any(v in wanted for v in values):
                    return False
                continue

            # Unknown filter field: be permissive, as a stub should be.
            continue

        return True


def main() -> int:
    """Run the stub standalone on a fixed port, for manual experiments."""
    import argparse
    import signal

    parser = argparse.ArgumentParser(
        description="Standalone Nostr relay stub",
    )
    parser.add_argument("--port", type=int, default=0)
    args = parser.parse_args()

    stub = NostrRelayStub()
    url = stub.start()

    if args.port:
        # The stub picks its own port; tell the caller what it got.
        print(f"relay listening: {url}")

    print(f"relay listening: {url}")
    print("Ctrl-C to stop.")

    stop = threading.Event()

    def _handle(_signum, _frame):
        stop.set()

    signal.signal(signal.SIGINT, _handle)
    signal.signal(signal.SIGTERM, _handle)

    try:
        while not stop.is_set():
            stop.wait(0.5)
    finally:
        stub.stop()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
