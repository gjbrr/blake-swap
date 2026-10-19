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


def rig_args() -> list[str]:
    """Server flags that point the node proxy at a live two-chain rig.

    The engine panel is only worth asserting when there are real nodes behind it: a
    fake proxy would prove that the page can render a string, not that a browser can
    reach a chain. When the rig is absent or not running the check says so and skips
    that part, rather than passing on nothing.
    """
    path = Path(os.environ.get("RIG_ENV", Path.home() / ".blake-swap-test" / "rig-env.json"))

    if not path.is_file():
        return []

    try:
        rig = json.loads(path.read_text())
    except (OSError, ValueError):
        return []

    if not rig.get("running"):
        return []

    args: list[str] = []

    for name in ("btc", "blake"):
        port = rig.get(f"{name}_rpcport")
        datadir = rig.get(f"{name}_datadir")

        if not port or not datadir:
            return []

        # The rig runs in regtest, where Core writes its cookie a directory down.
        cookie = next(
            (
                candidate
                for candidate in (Path(datadir) / "regtest" / ".cookie", Path(datadir) / ".cookie")
                if candidate.is_file()
            ),
            None,
        )

        if cookie is None:
            return []

        args += [f"-{name}-rpc", f"http://127.0.0.1:{port}/", f"-{name}-cookie", str(cookie)]

    return args


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


def start_relay(work: Path):
    """Start the local relay the market check reads and writes."""
    binary = ROOT / "bin" / "blake-swap-relay"

    if not binary.is_file():
        subprocess.run(
            ["go", "build", "-o", str(binary), "./cmd/blake-swap-relay"],
            cwd=ROOT,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    port = free_port()
    log = (work / "relay.log").open("wb")

    process = subprocess.Popen(
        [str(binary), "-addr", f"127.0.0.1:{port}"],
        cwd=ROOT,
        stdout=log,
        stderr=log,
    )

    return process, f"ws://127.0.0.1:{port}"


def cli_binary() -> Path:
    """Build the Go CLI the coordination peer commands live in.

    It is rebuilt rather than reused: a stale binary in bin/ would make the check assert
    the transport of last week's source, which is exactly the kind of pass that hides a
    regression.
    """
    binary = ROOT / "bin" / "blake-swap-cli"

    subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/blake-swap-cli"],
        cwd=ROOT,
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )

    return binary


def write_key(work: Path, name: str, secret_hex: str) -> Path:
    """Write a participant key file, which is where the CLI reads its identity."""
    path = work / name
    path.write_text(secret_hex + "\n")

    return path


def peer(work: Path, key_file: Path, relay_url: str, *args: str, encryption: str = "off"):
    """Run one coordination peer command and return (exit code, combined output).

    The peer is the Go CLI with no node: it reads the same relay the browser wrote to,
    and its key file is the participant the check controls.
    """
    environment = dict(os.environ)
    environment.update(
        {
            "BLAKE_SWAP_STATE_DIR": str(work / "peer-state"),
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE": str(key_file),
            "BLAKE_SWAP_NOSTR_RELAYS": relay_url,
            "BLAKE_SWAP_NOSTR_ENCRYPTION": encryption,
        }
    )

    completed = subprocess.run(
        [str(cli_binary())] + list(args),
        cwd=ROOT,
        env=environment,
        capture_output=True,
        text=True,
        timeout=60,
    )

    return completed.returncode, (completed.stdout or "") + (completed.stderr or "")


def field(text: str, label: str) -> str:
    """Read one `name: value` line out of a rendered result or a printout."""
    for line in str(text).splitlines():
        stripped = line.strip()

        if stripped.startswith(label):
            return stripped[len(label):].strip()

    return ""


async def drive(url: str, budget: float, work: Path, relay_url: str = "") -> int:
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

        text = await evaluate(ws, "document.getElementById('vaulttest').innerText", [300])

        if vault != "true":
            print(f"FAIL: the encrypted self-test never finished; panel:\n{text}", file=sys.stderr)

            return 1

        print(f"PASS: encrypted round trip in the browser: {text}")

        if not await open_vault_and_identity(ws, budget):
            return 1

        if relay_url:
            market = await market_round_trip(ws, budget, relay_url)

            if market is False:
                return 1

            print(f"PASS: market panel read and published through the relay: {market}")
        else:
            print("SKIP: no local relay was started for the market check")

        if relay_url:
            coordination = await coordination_round_trip(ws, budget, work, relay_url)

            if coordination is False:
                return 1

            print(f"PASS: settlement transport round trip: {coordination}")
        else:
            print("SKIP: no local relay was started for the coordination check")

        engine = await engine_round_trip(ws, budget)

        if engine is None:
            print("SKIP: the engine round trip needs a live two-chain rig (see RIG_ENV)")

            return 0

        if engine is False:
            return 1

        print(f"PASS: engine read both chains through the proxy: {engine}")

    return 0


# The steps every later check needs: an open vault and an identity in it. They are
# typed and clicked exactly as a person would, so a regression in either control fails
# here rather than being hidden by a test that sets the worker's state directly.
COMMON_STEPS = {
    "vault": (
        "document.getElementById('passphrase').value = 'browser-check-passphrase';"
        "document.querySelector(\"[data-uicmd='vault-create']\").click();"
        "'clicked'"
    ),
    "identity": (
        "document.getElementById('identity-secret').value = '11'.repeat(32);"
        "document.querySelector(\"[data-uicmd='identity-set']\").click();"
        "'clicked'"
    ),
}

COMMON_MARKERS = {
    "vault": "document.body.dataset.vaultOpen || ''",
    "identity": "document.body.dataset.identitySet || ''",
}

COMMON_PANELS = {"vault": "vault", "identity": "identity"}


async def open_vault_and_identity(ws, budget: float) -> bool:
    """Open the vault and install an identity, unless this page already did."""
    for step in ("vault", "identity"):
        if await evaluate(ws, COMMON_MARKERS[step], [100]) == "true":
            print(f"PASS: {step} already set")

            continue

        await evaluate(ws, COMMON_STEPS[step], [700])

        if await poll(ws, COMMON_MARKERS[step], budget) != "true":
            panel = await evaluate(
                ws, f"document.getElementById('{COMMON_PANELS[step]}').innerText", [800],
            )

            print(f"FAIL: the {step} step never completed; panel:\n{panel}", file=sys.stderr)

            return False

        print(f"PASS: {step} panel completed")

    return True


async def market_round_trip(ws, budget: float, relay_url: str):
    """Drive the market panel against a local relay the check started.

    The market needs no chain, so this runs whether or not a two-chain rig is behind the
    proxy — which is the point: reading and advertising on the marketplace is the part of
    the client that works with nothing but a relay.
    """
    steps = {
        "relays": (
            f"document.getElementById('relays').value = {relay_url!r};"
            "document.querySelector(\"[data-uicmd='market-set-relays']\").click();"
            "'clicked'"
        ),
        "publish": (
            "document.getElementById('market-blake').value = '10';"
            "document.getElementById('market-sats').value = '50000';"
            "document.getElementById('market-minutes').value = '30';"
            "document.querySelector(\"[data-uicmd='market-publish']\").click();"
            "'clicked'"
        ),
        "list": (
            "document.querySelector(\"[data-uicmd='market-list']\").click(); 'clicked'"
        ),
    }

    for step, script in steps.items():
        await evaluate(ws, script, [800])

        # Each command answers into the same panel, so the wait is for text that only
        # that command produces.
        if step == "relays":
            ready = await poll(
                ws,
                "document.getElementById('market').innerText.includes('relays set') ? 'true' : ''",
                budget,
            )
        elif step == "publish":
            ready = await poll(
                ws,
                "document.getElementById('market').innerText.includes('MARKET OFFER PUBLISHED') ? 'true' : ''",
                budget,
            )

            # Read the id out of the panel while it is still the publish answer: the next
            # command replaces this text, and the market view only shows prefixes.
            await evaluate(ws, (
                "(() => {"
                "  const match = document.getElementById('market').innerText"
                "    .match(/offer id:\\s*([0-9a-f]{64})/);"
                "  window.__offerId = match ? match[1] : '';"
                "  return window.__offerId;"
                "})()"
            ), [300])
        else:
            ready = await poll(
                ws,
                "document.body.dataset.marketVerified || ''",
                budget,
            )

        if ready != "true":
            panel = await evaluate(ws, "document.getElementById('market').innerText", [900])

            print(f"FAIL: the market {step} step never completed; panel:\n{panel}", file=sys.stderr)

            return False

        print(f"PASS: market {step} completed")

    # Take the offer that was just advertised. Reading the id out of the panel is what a
    # person does too: the market view is where the id comes from.
    clicked = await evaluate(ws, (
        "(() => {"
        "  if (!window.__offerId) { return 'no-offer'; }"
        "  document.getElementById('market-offer-id').value = window.__offerId;"
        "  document.querySelector(\"[data-uicmd='market-take']\").click();"
        "  return 'clicked';"
        "})()"
    ), [900])

    if clicked != "clicked":
        print(f"FAIL: no offer id was captured to take ({clicked})", file=sys.stderr)

        return False

    taken = await poll(
        ws,
        "document.getElementById('market').innerText.includes('MARKET OFFER TAKEN') ? 'true' : ''",
        budget,
    )

    if taken != "true":
        panel = await evaluate(ws, "document.getElementById('market').innerText", [900])
        worker = await evaluate(ws, "document.getElementById('worker-status').innerText", [200])

        print(
            f"FAIL: the market take step never completed; panel:\n{panel}\nworker: {worker}",
            file=sys.stderr,
        )

        return False

    print("PASS: market take completed")

    # The panel now holds the take's answer; the market view was asserted at the list
    # step, before the take replaced it.
    text = await evaluate(ws, "document.getElementById('market').innerText", [900])

    if "MARKET OFFER TAKEN" not in text:
        print(f"FAIL: the market panel did not confirm the take:\n{text}", file=sys.stderr)

        return False

    return " ".join(str(text).split())


async def coordination_round_trip(ws, budget: float, work: Path, relay_url: str):
    """Drive the settlement transport through the cockpit, with the Go CLI as the peer.

    Six asserts, in the order the protocol has them:

      a. the browser publishes a signed coordination message to the local relay;
      b. a second client — the Go CLI, no node, the same relay — reads and validates it;
      c. the second client replies with its acceptance;
      d. the browser receives and validates that reply against the pinned author;
      e. a reply signed by the wrong key is refused, and the refusal names the author;
      f. one encrypted round trip under the configured NIP-44 mode, preceded by the
         proof that a plaintext reply in that mode is refused as a downgrade.

    Every step is a real click on a real control, and the page drives the worker over the
    channel it already had. The peer is a process the check owns, so both sides are
    ordinary clients of the same relay rather than one program agreeing with itself.
    """
    bob = write_key(work, "bob.nostr-key", "22" * 32)
    mallory = write_key(work, "mallory.nostr-key", "33" * 32)

    code, output = peer(work, bob, relay_url, "coord-key")

    if code != 0:
        print(f"FAIL: the peer could not read its key:\n{output}", file=sys.stderr)

        return False

    bob_pub = field(output, "public key:")

    code, output = peer(work, mallory, relay_url, "coord-key")

    if code != 0:
        print(f"FAIL: the wrong-key peer could not read its key:\n{output}", file=sys.stderr)

        return False

    mallory_pub = field(output, "public key:")

    if len(bob_pub) != 64 or len(mallory_pub) != 64:
        print(f"FAIL: peer keys are not x-only keys ({bob_pub!r}, {mallory_pub!r})", file=sys.stderr)

        return False

    print(f"PASS: coordination peers ready (bob {bob_pub[:12]}…, wrong key {mallory_pub[:12]}…)")

    async def panel() -> str:
        return str(await evaluate(ws, "document.getElementById('coord').innerText", [1200]))

    async def wait_for(needle: str):
        reached = await poll(
            ws,
            "document.getElementById('coord').innerText.includes(" + repr(needle) + ") ? 'true' : ''",
            budget,
        )

        if reached != "true":
            text = await panel()

            print(f"FAIL: the coordination panel never showed {needle!r}:\n{text}", file=sys.stderr)

            return None

        return await panel()

    async def open_record(pinned: str):
        """Click the control that opens a swap record, and read its id back."""
        await evaluate(ws, (
            "(() => {"
            "  document.getElementById('coord-blake').value = '10';"
            "  document.getElementById('coord-sats').value = '50000';"
            "  document.getElementById('coord-peer').value = " + repr(pinned) + ";"
            "  document.querySelector(\"[data-uicmd='coord-open']\").click();"
            "  return 'clicked';"
            "})()"
        ), [1000])

        text = await wait_for("COORDINATION SWAP OPENED")

        if text is None:
            return None

        swap_id = field(text, "swap id:")

        if len(swap_id) != 12:
            print(f"FAIL: the opened record printed no swap id:\n{text}", file=sys.stderr)

            return None

        return swap_id

    async def announce(kind: str, swap_id: str):
        """Click announce, and read the published event id and sender back."""
        await evaluate(ws, (
            "(() => {"
            "  document.getElementById('swap-id').value = " + repr(swap_id) + ";"
            "  document.getElementById('coord-kind').value = " + repr(kind) + ";"
            "  document.querySelector(\"[data-uicmd='coord-send']\").click();"
            "  return 'clicked';"
            "})()"
        ), [1000])

        text = await wait_for("COORDINATION MESSAGE PUBLISHED")

        if text is None:
            return None

        event_id = field(text, "event id:")

        if len(event_id) != 64:
            print(f"FAIL: the announcement printed no event id:\n{text}", file=sys.stderr)

            return None

        return {"event_id": event_id, "sender": field(text, "sender:"), "text": text}

    async def receive(kind: str, swap_id: str):
        """Click receive and return the panel, whatever it says."""
        await evaluate(ws, (
            "(() => {"
            "  document.getElementById('swap-id').value = " + repr(swap_id) + ";"
            "  document.getElementById('coord-kind').value = " + repr(kind) + ";"
            "  document.querySelector(\"[data-uicmd='coord-receive']\").click();"
            "  return 'clicked';"
            "})()"
        ), [1000])

        # Either answer is a fact about the transport, so wait for the command to leave
        # its pending line rather than for one particular outcome.
        reached = await poll(ws, (
            "(() => {"
            "  const text = document.getElementById('coord').innerText;"
            "  if (text.includes('running coord.')) { return ''; }"
            "  return text.includes('COORDINATION MESSAGE') || text.includes('failed') ? 'true' : '';"
            "})()"
        ), budget)

        if reached != "true":
            print(f"FAIL: the receive step never answered:\n{await panel()}", file=sys.stderr)

            return None

        return await panel()

    #
    # (a) the browser publishes a signed coordination message.
    #
    swap_a = await open_record(bob_pub)

    if swap_a is None:
        return False

    print(f"PASS: a: opened swap {swap_a} pinned to the peer")

    offer_a = await announce("offer", swap_a)

    if offer_a is None:
        return False

    print(f"PASS: a: the browser published a signed offer ({offer_a['event_id'][:12]}…)")

    alice_pub = offer_a["sender"]

    if len(alice_pub) != 64:
        print(f"FAIL: the announcement did not print the sender:\n{offer_a['text']}", file=sys.stderr)

        return False

    #
    # (b) the second client reads and validates it.
    #
    code, output = peer(work, bob, relay_url, "coord-read", swap_a, "--author", alice_pub,
                        "--kind", "offer")

    if code != 0:
        print(f"FAIL: the peer could not read the offer:\n{output}", file=sys.stderr)

        return False

    if "COORDINATION MESSAGE VALIDATED" not in output or f"sender:   {alice_pub}" not in output:
        print(f"FAIL: the peer validated something else:\n{output}", file=sys.stderr)

        return False

    if f'"swap_id":"{swap_a}"' not in output or '"type":"offer"' not in output:
        print(f"FAIL: the validated body is not the offer for {swap_a}:\n{output}", file=sys.stderr)

        return False

    print(f"PASS: b: the peer read and validated the signed offer from {alice_pub[:12]}…")

    #
    # (c) the second client replies.
    #
    code, output = peer(work, bob, relay_url, "coord-reply", swap_a, "--author", alice_pub,
                        "--blake-bob-pub", "02" + "ab" * 32, "--btc-bob-pub", "03" + "cd" * 32)

    if code != 0:
        print(f"FAIL: the peer could not reply:\n{output}", file=sys.stderr)

        return False

    reply_event = field(output, "event id:")

    if len(reply_event) != 64:
        print(f"FAIL: the reply printed no event id:\n{output}", file=sys.stderr)

        return False

    print(f"PASS: c: the peer replied with an accept ({reply_event[:12]}…)")

    #
    # (d) the browser receives and validates the reply.
    #
    text = await receive("accept", swap_a)

    if text is None:
        return False

    if "COORDINATION MESSAGE RECEIVED" not in text:
        print(f"FAIL: the browser refused the peer's reply:\n{text}", file=sys.stderr)

        return False

    if field(text, "sender:") != bob_pub:
        print(f"FAIL: the reply was accepted from the wrong author:\n{text}", file=sys.stderr)

        return False

    if '"type":"accept"' not in text or f'"swap_id":"{swap_a}"' not in text:
        print(f"FAIL: the validated reply is not the accept for {swap_a}:\n{text}", file=sys.stderr)

        return False

    if await evaluate(ws, "document.body.dataset.coordVerified || ''", [200]) != "true":
        print("FAIL: a validated receive never set its marker", file=sys.stderr)

        return False

    print(f"PASS: d: the browser received and validated the reply from {bob_pub[:12]}…")

    #
    # (e) a message signed by the wrong key is refused.
    #
    swap_b = await open_record(bob_pub)

    if swap_b is None:
        return False

    if await announce("offer", swap_b) is None:
        return False

    code, output = peer(work, mallory, relay_url, "coord-reply", swap_b, "--author", alice_pub,
                        "--blake-bob-pub", "02" + "ab" * 32, "--btc-bob-pub", "03" + "cd" * 32)

    if code != 0:
        print(f"FAIL: the wrong-key peer could not publish:\n{output}", file=sys.stderr)

        return False

    text = await receive("accept", swap_b)

    if text is None:
        return False

    if "SECURITY FAILURE" not in text:
        print(f"FAIL: a reply signed by the wrong key was not refused:\n{text}", file=sys.stderr)

        return False

    if mallory_pub not in text or bob_pub not in text:
        print(f"FAIL: the refusal does not name both authors:\n{text}", file=sys.stderr)

        return False

    print("PASS: e: a reply signed by the wrong key was refused, naming both authors")

    #
    # (f) one encrypted round trip under the configured NIP-44 mode.
    #
    await evaluate(ws, (
        "document.getElementById('coord-mode').value = 'nip44_v2';"
        "document.querySelector(\"[data-uicmd='coord-set-mode']\").click(); 'clicked'"
    ), [1000])

    if await wait_for("encryption set: nip44_v2") is None:
        return False

    print("PASS: f: the browser is configured for nip44_v2")

    bobs_keys = ("--blake-bob-pub", "02" + "ab" * 32, "--btc-bob-pub", "03" + "cd" * 32)

    # A plaintext reply in this mode is a downgrade, not a message.
    swap_c = await open_record(bob_pub)

    if swap_c is None:
        return False

    offer_c = await announce("offer", swap_c)

    if offer_c is None:
        return False

    if "ciphertext: none (offer is exempt" not in offer_c["text"]:
        print(f"FAIL: the offer is not exempt from encryption:\n{offer_c['text']}", file=sys.stderr)

        return False

    code, output = peer(work, bob, relay_url, "coord-reply", swap_c, "--author", alice_pub,
                        *bobs_keys)

    if code != 0:
        print(f"FAIL: the peer could not publish a plaintext reply:\n{output}", file=sys.stderr)

        return False

    text = await receive("accept", swap_c)

    if text is None:
        return False

    if "SECURITY FAILURE" not in text or "downgrade" not in text:
        print(f"FAIL: a plaintext accept under nip44_v2 was not refused:\n{text}", file=sys.stderr)

        return False

    print("PASS: f: a plaintext reply under nip44_v2 was refused as a downgrade")

    # And the encrypted reply decrypts.
    swap_d = await open_record(bob_pub)

    if swap_d is None:
        return False

    if await announce("offer", swap_d) is None:
        return False

    code, output = peer(work, bob, relay_url, "coord-reply", swap_d, "--author", alice_pub,
                        *bobs_keys, encryption="nip44_v2")

    if code != 0:
        print(f"FAIL: the peer could not publish an encrypted reply:\n{output}", file=sys.stderr)

        return False

    text = await receive("accept", swap_d)

    if text is None:
        return False

    if "COORDINATION MESSAGE RECEIVED" not in text or field(text, "sender:") != bob_pub:
        print(f"FAIL: the encrypted accept was not received:\n{text}", file=sys.stderr)

        return False

    if "mode:       nip44_v2" not in text or "ciphertext: required" not in text:
        print(f"FAIL: the accepted reply was not encrypted under the configured mode:\n{text}",
              file=sys.stderr)

        return False

    return " ".join(str(text).split())


async def engine_round_trip(ws, budget: float):
    """Drive the vault, identity, proxy and engine panels the way a person would.

    Every step is a real click on a real control with the value typed into the field
    the page reads, so this exercises the same path a user takes: page domain, link,
    worker, node proxy, node.
    """
    scripts = {
        "vault": (
            "document.getElementById('passphrase').value = 'browser-check-passphrase';"
            "document.querySelector(\"[data-uicmd='vault-create']\").click();"
            "'clicked'"
        ),
        "proxy": (
            "document.getElementById('proxy-url').value = location.origin;"
            "document.querySelector(\"[data-uicmd='proxy-set']\").click();"
            "'clicked'"
        ),
        "identity": (
            "document.getElementById('identity-secret').value = '11'.repeat(32);"
            "document.querySelector(\"[data-uicmd='identity-set']\").click();"
            "'clicked'"
        ),
        "engine": (
            "document.querySelector(\"[data-uicmd='engine-status']\").click(); 'clicked'"
        ),
    }

    markers = {
        "vault": "document.body.dataset.vaultOpen || ''",
        "proxy": "document.body.dataset.proxySet || ''",
        "identity": "document.body.dataset.identitySet || ''",
        "engine": "document.body.dataset.engineVerified || ''",
    }

    for step in ("vault", "proxy", "identity", "engine"):
        if step != "engine":
            already = await evaluate(ws, markers[step], [100])

            if already == "true":
                # The market check already opened the vault and installed the identity;
                # repeating those clicks would test the second click, not the first.
                continue

        await evaluate(ws, scripts[step], [700])

        reached = await poll(ws, markers[step], budget)

        if reached != "true":
            panel = await evaluate(ws, f"document.getElementById('{step}').innerText", [800])

            print(f"FAIL: the {step} step never completed; panel:\n{panel}", file=sys.stderr)

            return False

        print(f"PASS: {step} panel completed")

    text = await evaluate(ws, "document.getElementById('engine').innerText", [900])

    if not text or "height" not in text.lower():
        print(f"FAIL: the engine panel reported no chain heights:\n{text}", file=sys.stderr)

        return False

    return " ".join(str(text).split())


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

    # The market check needs a relay that is not the internet: a local one keeps the
    # result about this code rather than about somebody else's retention policy.
    relay_process, relay_url = start_relay(work)

    # The server's own log is kept: when a browser step fails the reason is often on
    # the server side, and a discarded log turns a five-second diagnosis into a bisect.
    server_log = (work / "server.log").open("wb")

    server = subprocess.Popen(
        [str(ROOT / "bin" / "blake-swap-app"), "-addr", f"127.0.0.1:{app_port}", "-dir", "web"]
        + rig_args(),
        cwd=ROOT,
        stdout=server_log,
        stderr=server_log,
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

        return asyncio.run(
            drive(f"http://127.0.0.1:{debug_port}", args.budget, work, relay_url)
        )
    finally:
        for process in (chromium, server, relay_process):
            process.terminate()

            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()

        if not args.keep:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
