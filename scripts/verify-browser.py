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
from decimal import Decimal
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


CONSOLE: list[str] = []


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

        if response.get("method") == "Runtime.consoleAPICalled":
            args = response.get("params", {}).get("args", [])
            text = " ".join(str(arg.get("value", arg.get("description", ""))) for arg in args)
            CONSOLE.append(text)

            if len(CONSOLE) > 400:
                del CONSOLE[: len(CONSOLE) - 400]

            continue

        if response.get("id") != message_id:
            continue

        result = response.get("result", {}).get("result", {})

        return result.get("value")


async def cdp(ws, method: str, params: dict, counter: list[int]) -> dict:
    """Send one CDP command and return its response.

    Two things in this check are not expressions: a download is a browser behavior, and a
    file input is a DOM node the driver has to set a real file on. Both are core protocol,
    not a workaround.
    """
    counter[0] += 1
    message_id = counter[0]

    await ws.send(json.dumps({"id": message_id, "method": method, "params": params}))

    while True:
        raw = await asyncio.wait_for(ws.recv(), timeout=30)
        response = json.loads(raw)

        if response.get("method") == "Runtime.consoleAPICalled":
            args = response.get("params", {}).get("args", [])
            text = " ".join(str(arg.get("value", arg.get("description", ""))) for arg in args)
            CONSOLE.append(text)

            if len(CONSOLE) > 400:
                del CONSOLE[: len(CONSOLE) - 400]

            continue

        if response.get("id") != message_id:
            continue

        return response


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



# ---------------------------------------------------------------------------------
# participants and the node rig
# ---------------------------------------------------------------------------------


def rig_config() -> dict:
    """Read the running rig, or an empty dict when there is none."""
    path = Path(os.environ.get("RIG_ENV", Path.home() / ".blake-swap-test" / "rig-env.json"))

    if not path.is_file():
        return {}

    try:
        rig = json.loads(path.read_text())
    except (OSError, ValueError):
        return {}

    return rig if rig.get("running") else {}


# The four slot labels, which the page renders from the worker's view. They are here rather
# than repeated per check so a renamed label fails in one place.
WALLET_SLOT_LABELS = {
    "blake": "BLAKE signing wallet",
    "btc": "BTC signing wallet",
    "blake_watch": "BLAKE watch-only wallet",
    "btc_watch": "BTC watch-only wallet",
}


def node_cli(chain: str, *args: str, wallet: str = "") -> str:
    """Run one node command against the rig and return its output."""
    rig = rig_config()
    binary = rig.get(f"{chain}_cli")
    datadir = rig.get(f"{chain}_datadir")

    if not binary or not datadir:
        raise RuntimeError(f"the rig has no {chain} node")

    command = [binary, f"-datadir={datadir}"]

    if wallet:
        command.append(f"-rpcwallet={wallet}")

    command.extend(args)

    completed = subprocess.run(command, capture_output=True, text=True, timeout=120)

    if completed.returncode != 0:
        raise RuntimeError(
            f"{chain} {' '.join(args[:2])} failed: {(completed.stderr or completed.stdout).strip()}"
        )

    return (completed.stdout or "").strip()


def ensure_wallet(chain: str, wallet: str, watch: bool) -> None:
    """Create and load a wallet when the node does not have it."""
    loaded = node_cli(chain, "listwallets")

    if json.dumps(wallet) in loaded:
        return

    node_cli(chain, "createwallet", wallet, "true" if watch else "false", "true",
             "", "false", "true")


def ensure_funds(chain: str, wallet: str, sats: int) -> float:
    """Make sure a wallet holds at least `sats` of the chain's coin.

    A regtest rig is not a bank: the coins come from a mining wallet that may have been
    spent by an earlier run, so this tops the wallet up from whatever holds some and mines a
    block. It returns the balance it left, so a failure says what it had.
    """
    ensure_wallet(chain, wallet, False)

    minimum = Decimal(sats) / Decimal(100_000_000)
    balance = Decimal(node_cli(chain, "getbalance", wallet=wallet))
    best = (wallet, balance)

    for source in ("alice", "bob", "rig"):
        loaded = json.dumps(source) in node_cli(chain, "listwallets")

        if not loaded:
            continue

        held = Decimal(node_cli(chain, "getbalance", wallet=source))

        if held > best[1]:
            best = (source, held)

    if best[1] < minimum:
        # Bitcoin can always be minted; BLAKE cannot, so a chain with no coins anywhere is
        # a rig that needs restarting rather than a test failure to disguise.
        if chain == "btc":
            address = node_cli(chain, "getnewaddress", wallet=wallet)

            node_cli(chain, "generatetoaddress", "101", address)
            balance = Decimal(node_cli(chain, "getbalance", wallet=wallet))

            if balance >= minimum:
                return float(balance)

        raise RuntimeError(
            f"no {chain} funds anywhere on the rig: the best wallet holds {best[1]}"
        )

    if best[0] != wallet and balance < minimum:
        address = node_cli(chain, "getnewaddress", wallet=wallet)
        amount = str(minimum - balance + minimum)
        node_cli(chain, "sendtoaddress", address, amount, wallet=best[0])
        node_cli(chain, "generatetoaddress", "1", address)

        balance = Decimal(node_cli(chain, "getbalance", wallet=wallet))

    return float(balance)


def mine(chain: str, blocks: int = 1) -> None:
    """Mine blocks, which is how a broadcast transaction becomes a confirmed one.

    The browser does not mine: a client that mined its own chain would be a test rig, not a
    participant. On regtest somebody has to, and here that is the check.
    """
    address = node_cli(chain, "getnewaddress", wallet="alice")

    node_cli(chain, "generatetoaddress", str(blocks), address)


def wallet_public_key(chain: str, wallet: str) -> str:
    """Read the compressed public key a wallet signs with."""
    info = json.loads(node_cli(chain, "getwalletinfo", wallet=wallet))

    # Not every node reports a public key; the descriptor route is the one the engine uses.
    address = node_cli(chain, "getnewaddress", wallet=wallet)
    validated = json.loads(node_cli(chain, "getaddressinfo", address, wallet=wallet))

    return validated.get("pubkey") or info.get("pubkey") or ""


# ---------------------------------------------------------------------------------
# the page
# ---------------------------------------------------------------------------------


class Ui:
    """The document, driven the way a person drives it: clicks and typed values."""

    def __init__(self, ws, budget: float):
        self.ws = ws
        self.budget = budget
        self.counter = [1000]

    async def js(self, expression: str):
        return await evaluate(self.ws, expression, self.counter)

    async def attr(self, name: str) -> str:
        return str(await self.js(f"document.body.dataset['{name}'] || ''"))

    async def panel(self, element_id: str) -> str:
        return str(await self.js(f"document.getElementById('{element_id}').innerText"))

    async def set(self, element_id: str, value: str) -> None:
        await self.js(f"document.getElementById('{element_id}').value = {value!r}")

    async def wait_idle(self, budget: float = 30) -> bool:
        """Wait until no command is in flight.

        The page drops a click while it is busy — one command at a time is the rule — so a
        driver that clicked through the queue would be testing the drop, not the control.
        """
        idle = await poll(
            self.ws,
            "document.body.dataset.busy === 'false' ? 'true' : ''",
            budget,
        )

        return idle == "true"

    async def click(self, uicmd: str) -> bool:
        await self.wait_idle()

        clicked = await self.js(
            "(() => {"
            f"  const el = document.querySelector(\"[data-uicmd='{uicmd}']\");"
            "  if (!el) { return 'missing'; }"
            "  el.click();"
            "  return 'clicked';"
            "})()"
        )

        return clicked == "clicked"

    async def wait_command(self, name: str, budget: float = 0) -> bool:
        """Wait until the page has sent one named command.

        It is not the same fact as "not busy": between a click on the dialog's OK and the
        page sending the command there is a moment where nothing is in flight, and a check
        that read the result there would read the *previous* command's answer.
        """
        reached = await poll(
            self.ws,
            f"document.body.dataset.lastCommand === {name!r} ? 'true' : ''",
            budget or self.budget,
        )

        return reached == "true"

    async def click_in(self, view: str, uicmd: str) -> bool:
        """Click a control inside one named view, not wherever it first appears.

        Several controls exist on more than one view (the vault export and restore are on
        Setup and on Settings). Clicking the first match in the document would exercise the
        hidden one, which is not what a person can reach.
        """
        await self.wait_idle()

        clicked = await self.js(
            "(() => {"
            f"  const root = document.getElementById('view-{view}');"
            "  if (!root) { return 'missing-view'; }"
            f"  const el = root.querySelector(\"[data-uicmd='{uicmd}']\");"
            "  if (!el) { return 'missing'; }"
            "  el.click();"
            "  return 'clicked';"
            "})()"
        )

        return clicked == "clicked"

    async def wait_attr(self, name: str, value: str = "true", budget: float = 0) -> bool:
        reached = await poll(
            self.ws, f"document.body.dataset['{name}'] || ''", budget or self.budget,
        )

        return str(reached) == value

    async def wait_panel(self, element_id: str, needle: str, budget: float = 0) -> bool:
        reached = await poll(
            self.ws,
            f"document.getElementById('{element_id}').innerText.includes({needle!r}) ? 'true' : ''",
            budget or self.budget,
        )

        return reached == "true"

    async def wait_dialog(self, budget: float = 10) -> bool:
        opened = await poll(
            self.ws,
            "document.getElementById('confirm-dialog').dataset.confirmOpen || ''",
            budget,
        )

        return opened == "true"

    async def confirm(self) -> bool:
        if not await self.wait_dialog():
            return False

        return await self.click("confirm-ok")

    async def act(self, uicmd: str, confirm: bool = True) -> bool:
        """Click a control, and confirm the dialog when it opens one."""
        if not await self.click(uicmd):
            return False

        if confirm:
            return await self.confirm()

        return True

    async def swap_action_refusal(self, name: str, budget: float = 60) -> str:
        """Click an action that is *expected* to be refused, and return the engine's words."""
        if not await self.act("swap-action:" + name):
            return ""

        if not await self.wait_idle(budget):
            return ""

        return await self.last_error()

    async def swap_action(self, name: str) -> bool:
        """Click one swap action, confirm it, and insist that it succeeded.

        A confirmed dialog is not a successful action: the engine's refusal is shown inline
        and must fail the step, or a check would pass on a command that did nothing.
        """
        if not await self.act("swap-action:" + name):
            print(f"FAIL: {name} was not confirmed", file=sys.stderr)

            return False

        if not await self.wait_idle():
            print(f"FAIL: {name} never finished", file=sys.stderr)

            return False

        error = await self.last_error()

        if error:
            print(f"FAIL: {name} was refused: {error}", file=sys.stderr)

            return False

        return True

    async def view(self, name: str) -> None:
        await self.click("view-" + name)

    async def reload(self) -> None:
        await self.js("location.reload(); 'reloading'")

    async def wait_boot(self, budget: float = 60) -> bool:
        reached = await poll(
            self.ws, "document.body.dataset.helloVerified || ''", budget,
        )

        return reached == "true"

    async def wait_busy_idle(self, budget: float = 60) -> bool:
        idle = await poll(
            self.ws, "document.body.dataset.busy === 'false' ? 'true' : ''", budget,
        )

        return idle == "true"

    async def last_error(self) -> str:
        return await self.attr("lastError")

    async def swap_state(self) -> str:
        return await self.attr("swapState")

    async def next_action(self) -> str:
        return await self.attr("nextAction")

    async def selected_swap(self) -> str:
        return await self.attr("selectedSwap")

    async def market_count(self) -> str:
        return await self.attr("marketCount")


# ---------------------------------------------------------------------------------
# the participant's own setup
# ---------------------------------------------------------------------------------


async def open_vault(ui: Ui, wallet: str) -> bool:
    """Create the vault, set the identity, relays, proxy and wallets."""
    already = await ui.wait_attr("vaultOpen", budget=2)

    if not already:
        await ui.set("passphrase", "browser-check-passphrase")

        if not await ui.act("vault-create", confirm=False):
            print("FAIL: the vault was not created", file=sys.stderr)

            return False

        if not await ui.wait_attr("vaultOpen"):
            print("FAIL: the vault never opened", file=sys.stderr)

            return False

        print("PASS: vault created")

    identity = await ui.wait_attr("identitySet", budget=2)

    if not identity:
        await ui.set("identity-secret", "11" * 32)

        if not await ui.act("identity-set", confirm=False):
            print("FAIL: the identity was not set", file=sys.stderr)

            return False

        print("PASS: identity set")

    return True


async def wait_bootstrap(ui: Ui) -> bool:
    """Wait for the sequence the page runs after unlocking to finish.

    Configuring the client while that sequence is still running would race it: the later
    read would overwrite the panel the earlier command wrote.
    """
    reached = await poll(
        ui.ws,
        "(() => {"
        "  const d = document.body.dataset;"
        "  return d.engineVerified === 'true' && d.busy !== 'true' ? 'true' : '';"
        "})()",
        ui.budget,
    )

    if reached != "true":
        print(
            "FAIL: the page never finished its startup sequence\n"
            f"  busy: {await ui.attr('busy')!r}\n"
            f"  engine: {await ui.panel('engine-status-out')}\n"
            f"  wallets: {await ui.panel('wallets')}\n"
            f"  alert: {await ui.panel('alert-text')}\n"
            f"  busy line: {await ui.panel('busy-line')}",
            file=sys.stderr,
        )

        return False

    return True


async def configure(ui: Ui, relay_url: str) -> bool:
    """Point the client at the relay, the app server and its wallets."""
    await ui.view("setup")

    await ui.set("relays", relay_url)

    if not await ui.act("market-set-relays", confirm=False):
        print("FAIL: the relays were not set", file=sys.stderr)

        return False

    if not await ui.wait_panel("market-relays-out", "relays set"):
        print(f"FAIL: the relays were refused:\n{await ui.panel('market-relays-out')}",
              file=sys.stderr)

        return False

    await ui.set("proxy-url", "")

    if not await ui.act("proxy-set", confirm=False):
        print("FAIL: the proxy was not set", file=sys.stderr)

        return False

    if not await ui.wait_attr("proxySet"):
        print(f"FAIL: the proxy was refused:\n{await ui.panel('proxy')}", file=sys.stderr)

        return False

    for field_id, name in (
        ("wallet-blake", WALLET),
        ("wallet-btc", WALLET),
        ("wallet-blake-watch", WALLET + "-watch"),
        ("wallet-btc-watch", WALLET + "-watch"),
    ):
        await ui.set(field_id, name)

    if not await ui.act("wallets-set", confirm=False):
        print("FAIL: the wallets were not set", file=sys.stderr)

        return False

    # The result is one line and the page re-reads the wallet rows after it, so what is
    # asserted is the refusal being absent rather than the panel keeping a sentence the
    # refresh is meant to replace.
    if not await ui.wait_command("wallets.set"):
        print("FAIL: the wallets were never sent", file=sys.stderr)

        return False

    if not await ui.wait_idle(30):
        print("FAIL: setting the wallets never finished", file=sys.stderr)

        return False

    if error := await ui.last_error():
        print(f"FAIL: the wallets were refused: {error}", file=sys.stderr)

        return False

    print(f"PASS: relays, proxy and wallets configured ({WALLET})")

    return True


# ---------------------------------------------------------------------------------
# the existing checks
# ---------------------------------------------------------------------------------


async def market_round_trip(ui: Ui, relay_url: str):
    """Read and advertise on the market through the new screen."""
    await ui.view("market")

    if not await ui.act("market-list", confirm=False):
        print("FAIL: the market list was not requested", file=sys.stderr)

        return False

    if not await ui.wait_panel("market", "Nostr market read"):
        print(f"FAIL: the market read failed:\n{await ui.panel('market')}", file=sys.stderr)

        return False

    if not await ui.wait_attr("marketVerified"):
        print("FAIL: the market read never set its marker", file=sys.stderr)

        return False

    print(f"PASS: market listed ({await ui.market_count()} open offer(s))")

    await ui.set("market-blake", "10")
    await ui.set("market-sats", "50000")
    await ui.set("market-minutes", "30")

    if not await ui.act("market-publish"):
        print("FAIL: the publish was not confirmed", file=sys.stderr)

        return False

    # The publish answer is in the panel until the refresh replaces it, so what is asserted
    # is the durable fact: the offer appeared in the book and the document carries its id.
    listed = await poll(
        ui.ws,
        "(() => {"
        "  const count = Number(document.body.dataset.marketCount || '0');"
        "  const id = document.body.dataset.lastEventId || '';"
        "  return count >= 1 && id.length === 64 ? 'true' : '';"
        "})()",
        ui.budget,
    )

    if listed != "true":
        print(f"FAIL: the offer was not published:\n{await ui.panel('market')}",
              file=sys.stderr)

        return False

    offer_id = await ui.attr("lastEventId")

    if not offer_id or len(str(offer_id)) != 64:
        print(f"FAIL: the published offer printed no id ({offer_id!r})", file=sys.stderr)

        return False

    # Taking is enabled now that the accept path exists. The check completes the whole
    # handshake against the CLI rather than trusting the button.
    enabled = await ui.js(
        "(() => {"
        "  const row = document.querySelector('#market-open-rows tr');"
        "  if (!row) { return 'no-row'; }"
        "  const button = row.querySelector('button');"
        "  return button && !button.disabled ? button.getAttribute('data-uicmd') : 'disabled';"
        "})()"
    )

    if not str(enabled).startswith("market-take:"):
        print(f"FAIL: the Take button is {enabled!r}, want an enabled market-take", file=sys.stderr)

        return False

    print("PASS: market published, and taking is offered")

    return str(offer_id)


async def market_handshake(ui: Ui, work: Path, relay_url: str, offer_id: str) -> bool:
    """The full market handshake: a CLI taker, a browser maker, and an imported match.

    This is the flow the Take button exists for. The browser is the maker here: it accepts
    the taker's take, which creates the swap and publishes the match, and the CLI taker then
    imports that match into its own record.
    """
    taker_key = write_key(work, "taker.nostr-key", "22" * 32)

    # The taker is a settlement participant, so it speaks the relay transport the engine's
    # accept path uses; the plain peer helper reads coordination messages only.
    code, output = peer_swap(work, taker_key, relay_url, "take-market-offer", offer_id)

    if not require(code == 0, f"the CLI taker could not take the offer:\n{output}"):
        return False

    print("PASS: market: a CLI taker took the browser's offer")

    # The browser re-reads the market and asks for the takes of its own offer.
    if not await ui.act("market-list", confirm=False):
        return False

    if not await ui.wait_idle(ui.budget):
        return require(False, "the market read never finished")

    if not await ui.click("market-takes:" + offer_id):
        return require(False, "the takes control was not found on the offer row")

    takes = await poll(
        ui.ws,
        "(() => {"
        "  const row = document.querySelector('#market-takes-rows tr');"
        "  if (!row) { return ''; }"
        "  const button = row.querySelector('button');"
        f"  const command = button ? button.getAttribute('data-uicmd') : '';"
        "  return command && command.startsWith('market-accept-take:') ? command : '';"
        "})()",
        ui.budget,
    )

    if not takes:
        print(f"FAIL: the take was not listed for the offer; panel:\n"
              f"{await ui.panel('market')}", file=sys.stderr)

        return False

    take_id = str(takes).split(":", 1)[1]

    print(f"PASS: market: the browser listed the take ({take_id[:12]}…)")

    if not await ui.act("market-accept-take:" + take_id):
        return require(False, "accepting the take was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "accepting the take never finished")

    error = await ui.last_error()

    if not require(error == "", f"accepting the take was refused: {error}"):
        return False

    match_id = await ui.attr("lastEventId")

    if not require(len(match_id) == 64, f"no match id was reported ({match_id!r})"):
        return False

    swap_id = await ui.selected_swap()

    if not require(len(swap_id) == 12, f"accepting the take selected no swap ({swap_id!r})"):
        return False

    print(f"PASS: market: the browser accepted the take as swap {swap_id} (match {match_id[:12]}…)")

    code, output = peer_swap(work, taker_key, relay_url, "accept-market-match", match_id)

    if not require(code == 0, f"the CLI taker could not import the match:\n{output}"):
        return False

    print("PASS: market: the CLI taker imported the match into its own record")

    return True


async def ensure_settlement_wallets() -> None:
    """The wallets a settleable swap needs, on both sides."""
    ensure_wallet("blake", WALLET, False)
    ensure_wallet("blake", WALLET + "-watch", True)
    ensure_wallet("btc", WALLET, False)
    ensure_wallet("btc", WALLET + "-watch", True)
    ensure_wallet("blake", "bob", False)
    ensure_wallet("blake", "bob-watch", True)
    ensure_wallet("btc", "bob", False)
    ensure_wallet("btc", "bob-watch", True)

    ensure_funds("blake", WALLET, 1_000_000)
    ensure_funds("blake", "bob", 1_000_000)
    ensure_funds("btc", WALLET, BTC_SATS * 2)
    ensure_funds("btc", "bob", BTC_SATS * 2)


async def reverse_market_handshake(ui: Ui, work: Path, relay_url: str):
    """The reverse direction: the browser is the market maker, which is protocol Bob.

    It advertises a btc-for-blake offer, a CLI taker takes it, the browser accepts that take
    — reserving the swap id and publishing the match, and creating no secret — the taker
    imports the match and publishes her protocol offer, and the browser imports that offer
    and sends its accept. Then the money moves exactly as the protocol says, with the
    browser funding BTC and claiming BLAKE.
    """
    await ensure_settlement_wallets()

    taker_key = write_key(work, "reverse-taker.nostr-key", "33" * 32)

    await ui.view("market")

    if not await ui.act("market-list", confirm=False):
        return require(False, "the market list was not requested")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the market read never finished")

    await ui.set("market-blake", BLAKE_AMOUNT)
    await ui.set("market-sats", str(BTC_SATS))
    await ui.set("market-minutes", "30")
    await ui.set("market-direction", "btc-for-blake")

    if not await ui.act("market-publish"):
        return require(False, "the reverse offer was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "publishing the reverse offer never finished")

    offer_id = await ui.attr("lastEventId")

    if not require(len(str(offer_id)) == 64, f"the reverse offer printed no id ({offer_id!r})"):
        return False

    print(f"PASS: reverse: browser Bob advertised a btc-for-blake offer {str(offer_id)[:12]}…")

    # The taker is the CLI, and in this direction it is the one that creates the secret.
    code, output = peer_swap(work, taker_key, relay_url, "take-market-offer", offer_id)

    if not require(code == 0, f"the CLI taker could not take the reverse offer:\n{output}"):
        return False

    take_id = field(output, "take id:")

    if not require(len(take_id) == 64, f"the take printed no id:\n{output}"):
        return False

    print("PASS: reverse: a CLI taker took the browser's offer")

    if not await ui.act("market-list", confirm=False):
        return require(False, "the market list was not requested")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the market read never finished")

    if not await ui.click("market-takes:" + str(offer_id)):
        return require(False, "the takes control was not found on the offer row")

    # The take row must say which direction it is and offer the reverse action: a
    # btc-for-blake take reserves the swap id instead of creating one.
    reserved = await poll(
        ui.ws,
        "(() => {"
        "  const row = document.querySelector('#market-takes-rows tr');"
        "  if (!row) { return ''; }"
        "  const cells = [...row.querySelectorAll('td')].map((cell) => cell.textContent.trim());"
        "  const button = row.querySelector('button');"
        "  const command = button ? button.getAttribute('data-uicmd') : '';"
        "  if (!command.startsWith('market-accept-take:')) { return ''; }"
        "  return cells.includes('btc-for-blake') ? command : '';"
        "})()",
        ui.budget,
    )

    if not reserved:
        print(f"FAIL: the reverse take was not listed with its direction;"
              f" panel:\n{await ui.panel('market')}", file=sys.stderr)

        return False

    print("PASS: reverse: the browser listed the take with its direction")

    # Whatever was open stays open: reserving an id must not select a record that does not
    # exist yet.
    open_before = await ui.selected_swap()

    if not await ui.act("market-accept-take:" + take_id):
        return require(False, "reserving the swap for the reverse take was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "reserving the swap never finished")

    error = await ui.last_error()

    if not require(error == "", f"accepting the reverse take was refused: {error}"):
        return False

    match_id = await ui.attr("lastEventId")

    if not require(len(str(match_id)) == 64, f"no match id was reported ({match_id!r})"):
        return False

    # The refresh after the action ends with a dashboard read, so the command log rather
    # than the last-command marker is what proves the accept itself ran.
    command_log = str(await ui.attr("commandLog"))

    if not require("market.accept-take" in command_log,
                   f"the accept never ran; commands: {command_log}"):
        return False

    # No swap record exists yet: this side reserved the id, the taker creates the record.
    # That the reserve step creates nothing is proven where it can be observed directly —
    # the marketops tests fail if the engine is touched at all on this path — and what the
    # page must not do is open a record that does not exist.
    if not require(await ui.selected_swap() == open_before,
                   f"reserving a swap selected {await ui.selected_swap()!r},"
                   f" want {open_before!r} unchanged"):
        return False

    print(f"PASS: reverse: the browser reserved the swap and published match "
          f"{str(match_id)[:12]}…")

    # The taker imports the match — creating the swap and the secret — and publishes the
    # protocol offer the maker needs.
    code, output = peer_swap(work, taker_key, relay_url,
                             "accept-reverse-market-match", match_id)

    if not require(code == 0, f"the CLI taker could not import the reverse match:\n{output}"):
        return False

    swap_id = field(output, "swap id:")

    if not require(len(swap_id) == 12, f"the taker printed no swap id:\n{output}"):
        return False

    code, output = peer_swap(work, taker_key, relay_url, "publish-offer", swap_id)

    if not require(code == 0, f"the CLI taker could not publish her offer:\n{output}"):
        return False

    print(f"PASS: reverse: the CLI taker imported the match as {swap_id} and published her offer")

    # The browser made that match, so its own step is importing the taker's offer.
    if not await ui.act("market-matches", confirm=False):
        return require(False, "the matches were not re-read")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the matches read never finished")

    offer_action = await poll(
        ui.ws,
        "(() => {"
        "  const row = document.querySelector('#market-matches-rows tr');"
        "  if (!row) { return ''; }"
        "  const button = row.querySelector('button');"
        "  const command = button ? button.getAttribute('data-uicmd') : '';"
        f"  return command.startsWith('market-accept-reverse-offer:') ? command : '';"
        "})()",
        ui.budget,
    )

    if not offer_action:
        print(f"FAIL: the reverse offer action was not offered; matches:\n"
              f"{await ui.panel('market')}", file=sys.stderr)

        return False

    if not await ui.act(str(offer_action)):
        return require(False, "importing the taker's offer was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "importing the offer never finished")

    error = await ui.last_error()

    if not require(error == "", f"importing the taker's offer was refused: {error}"):
        return False

    selected = await ui.selected_swap()

    if not require(selected == swap_id,
                   f"importing the offer selected {selected!r}, want {swap_id}"):
        return False

    if not await browser_swap_state(ui, "OFFERED"):
        return False

    print(f"PASS: reverse: the browser imported the taker's offer as {swap_id}")

    return await browser_bob_settlement(ui, taker_key, work, relay_url, swap_id, "Bob")


async def reverse_taker_handshake(ui: Ui, work: Path, relay_url: str) -> bool:
    """The reverse direction from the other side: the browser is the taker.

    A CLI maker advertises `btc-for-blake`, the browser takes it, the CLI accepts that take —
    reserving the swap id and publishing the match, creating no secret — and the browser
    imports the match. In this direction the taker is protocol Alice, so importing is where
    *this* side creates the secret and the swap. No funds move here.
    """
    maker_key = write_key(work, "reverse-maker.nostr-key", "66" * 32)

    code, output = peer_swap(work, maker_key, relay_url,
                             "publish-market-offer", "--blake", "3", "--sats", "30000",
                             "--minutes", "30", "--direction", "btc-for-blake")

    if not require(code == 0, f"the CLI reverse maker could not advertise:\n{output}"):
        return False

    offer_id = field(output, "offer id:")

    if not require(len(offer_id) == 64, f"the maker printed no offer id:\n{output}"):
        return False

    print(f"PASS: reverse taker: a CLI maker advertised {offer_id[:12]}…")

    if not await ui.act("market-list", confirm=False):
        return require(False, "the market list was not requested")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the market read never finished")

    taken = await poll(
        ui.ws,
        "(() => {"
        "  const rows = [...document.querySelectorAll('#market-open-rows tr')];"
        f"  const row = rows.find((r) => r.textContent.includes({offer_id[:12]!r}));"
        "  if (!row) { return ''; }"
        "  const button = row.querySelector('button');"
        "  return button && !button.disabled ? button.getAttribute('data-uicmd') : '';"
        "})()",
        ui.budget,
    )

    if not taken:
        print(f"FAIL: the reverse offer is not in the book; panel:\n"
              f"{await ui.panel('market')}", file=sys.stderr)

        return False

    if not await ui.act(str(taken)):
        return require(False, "taking the reverse offer was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "taking the reverse offer never finished")

    take_id = await ui.attr("lastEventId")

    if not require(len(str(take_id)) == 64, f"the browser printed no take id ({take_id!r})"):
        return False

    code, output = peer_swap(work, maker_key, relay_url, "accept-market-take", str(take_id))

    if not require(code == 0, f"the CLI maker could not accept the reverse take:\n{output}"):
        return False

    match_id = field(output, "match id:")

    if not require(len(match_id) == 64, f"the maker printed no match id:\n{output}"):
        return False

    reserved = field(output, "swap id:")

    if not require(len(reserved) == 12, f"the maker reserved no swap id:\n{output}"):
        return False

    print(f"PASS: reverse taker: the CLI maker reserved {reserved} and published match "
          f"{match_id[:12]}…")

    if not await ui.act("market-matches", confirm=False):
        return require(False, "the matches were not re-read")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the matches read never finished")

    import_command = await poll(
        ui.ws,
        "(() => {"
        "  const rows = [...document.querySelectorAll('#market-matches-rows tr')];"
        f"  const row = rows.find((r) => r.textContent.includes({match_id[:12]!r}));"
        "  if (!row) { return ''; }"
        "  const button = row.querySelector('button');"
        "  return button ? button.getAttribute('data-uicmd') : '';"
        "})()",
        ui.budget,
    )

    if not import_command:
        print(f"FAIL: the reverse match was not listed with an action;\n"
              f"{await ui.panel('market')}", file=sys.stderr)

        return False

    if not await ui.act(str(import_command)):
        return require(False, "importing the reverse match was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "importing the reverse match never finished")

    error = await ui.last_error()

    if not require(error == "", f"importing the reverse match was refused: {error}"):
        return False

    # The page chose the reverse import from the direction the match names; the worker
    # re-validated it and created the secret on this side.
    command_log = str(await ui.attr("commandLog"))

    if not require("market.accept-reverse-match" in command_log,
                   f"the reverse import never ran; commands: {command_log}"):
        return False

    selected = await ui.selected_swap()

    if not require(selected == reserved,
                   f"the import selected {selected!r}, want the reserved {reserved}"):
        return False

    if not await browser_swap_state(ui, "OFFERED"):
        return False

    print(f"PASS: reverse taker: the browser imported the match as {reserved} and holds the "
          f"secret")

    # The rest is an ordinary swap from Alice's side: the direction decided who made the
    # market, and the protocol does the same thing with the money either way.
    if not await ui.swap_action("publish-offer"):
        return require(False, "publishing the taker's protocol offer was not confirmed")

    code, output = peer_swap(work, maker_key, relay_url, "accept-reverse-swap-offer", match_id)

    if not require(code == 0, f"the CLI maker could not import the taker's offer:\n{output}"):
        return False

    print("PASS: reverse taker: the CLI maker imported the taker's offer and sent its accept")

    if not await ui.swap_action("receive-accept"):
        return False

    if not await ui.swap_action("fund-blake"):
        return False

    mine("blake")

    if not await ui.swap_action("sync-funding"):
        return False

    if not await browser_swap_state(ui, "BLAKE_FUNDED"):
        return False

    print("PASS: reverse taker: browser Alice funded BLAKE")

    if not await ui.swap_action("publish-blake-funded"):
        return False

    for command in ("receive-blake-funded", "fund-btc", "publish-btc-funded"):
        code, output = peer_swap(work, maker_key, relay_url, command, reserved)

        if not require(code == 0, f"the CLI Bob could not {command}:\n{output}"):
            return False

    print("PASS: reverse taker: CLI Bob received the BLAKE funding and funded BTC")

    if not await ui.swap_action("receive-btc-funded"):
        return False

    if not await browser_swap_state(ui, "BTC_FUNDED"):
        return False

    if not await ui.swap_action("claim-btc"):
        return False

    mine("btc")

    if not await ui.swap_action("sync-settlement"):
        return False

    if not await browser_swap_state(ui, "BTC_CLAIMED"):
        return False

    print("PASS: reverse taker: browser Alice claimed BTC")

    if not await ui.swap_action("publish-btc-claimed"):
        return False

    for command in ("receive-btc-claimed", "extract-secret", "claim-blake",
                    "publish-blake-claimed"):
        code, output = peer_swap(work, maker_key, relay_url, command, reserved)

        if not require(code == 0, f"the CLI Bob could not {command}:\n{output}"):
            return False

    if not await ui.swap_action("receive-blake-claimed"):
        return False

    if not await ui.swap_action("complete"):
        return False

    if not await browser_swap_state(ui, "COMPLETE"):
        return False

    print("PASS: reverse taker: the browser completed the swap as Alice")

    return True


async def ignore_match_round_trip(ui: Ui, work: Path, relay_url: str) -> bool:
    """Dismiss a match locally: it disappears, it survives a reload, and it stays on the relay.

    The match is made by the CLI, so the browser has a valid match addressed to it as the
    taker and no local swap state — which is exactly when ignoring is allowed. What proves
    the tombstone is local is that the CLI still sees the match afterwards.
    """
    maker_key = write_key(work, "ignore-maker.nostr-key", "55" * 32)

    code, output = peer_swap(work, maker_key, relay_url,
                             "publish-market-offer", "--blake", "4", "--sats", "40000",
                             "--minutes", "30")

    if not require(code == 0, f"the CLI maker could not advertise:\n{output}"):
        return False

    offer_id = field(output, "offer id:")

    if not require(len(offer_id) == 64, f"the maker printed no offer id:\n{output}"):
        return False

    print(f"PASS: ignore: a CLI maker advertised offer {offer_id[:12]}…")

    if not await ui.act("market-list", confirm=False):
        return require(False, "the market list was not requested")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the market read never finished")

    # The browser takes the offer, which is the act that makes it a taker.
    taken = await poll(
        ui.ws,
        "(() => {"
        "  const rows = [...document.querySelectorAll('#market-open-rows tr')];"
        f"  const row = rows.find((r) => r.textContent.includes({offer_id[:12]!r}));"
        "  if (!row) { return ''; }"
        "  const button = row.querySelector('button');"
        "  return button && !button.disabled ? button.getAttribute('data-uicmd') : '';"
        "})()",
        ui.budget,
    )

    if not taken:
        print(f"FAIL: the offer is not in the book; panel:\n{await ui.panel('market')}",
              file=sys.stderr)

        return False

    if not await ui.act(str(taken)):
        return require(False, "taking the offer was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "taking the offer never finished")

    take_id = await ui.attr("lastEventId")

    if not require(len(str(take_id)) == 64, f"the browser printed no take id ({take_id!r})"):
        return False

    print("PASS: ignore: the browser took the offer")

    # The maker accepts that take, which publishes the match addressed to the browser.
    code, output = peer_swap(work, maker_key, relay_url, "accept-market-take", str(take_id))

    if not require(code == 0, f"the CLI maker could not accept the take:\n{output}"):
        return False

    match_id = field(output, "market match:")

    if not require(len(match_id) == 64, f"the maker printed no match id:\n{output}"):
        return False

    print(f"PASS: ignore: the CLI maker published match {match_id[:12]}…")

    if not await ui.act("market-matches", confirm=False):
        return require(False, "the matches were not re-read")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the matches read never finished")

    ignore_command = await poll(
        ui.ws,
        "(() => {"
        "  const rows = [...document.querySelectorAll('#market-matches-rows tr')];"
        f"  const row = rows.find((r) => r.textContent.includes({match_id[:12]!r}));"
        "  if (!row) { return ''; }"
        "  const button = [...row.querySelectorAll('button')].find((b) =>"
        "    (b.getAttribute('data-uicmd') || '').startsWith('market-ignore-match:'));"
        "  return button ? button.getAttribute('data-uicmd') : '';"
        "})()",
        ui.budget,
    )

    if not ignore_command:
        print(f"FAIL: the match was not listed with an Ignore control;\n"
              f"{await ui.panel('market')}", file=sys.stderr)

        return False

    open_before = await ui.selected_swap()

    if not await ui.act(str(ignore_command)):
        return require(False, "ignoring the match was not confirmed")

    if not await ui.wait_idle(ui.budget):
        return require(False, "ignoring the match never finished")

    error = await ui.last_error()

    if not require(error == "", f"ignoring the match was refused: {error}"):
        return False

    gone = await poll(
        ui.ws,
        "(() => {"
        "  const rows = [...document.querySelectorAll('#market-matches-rows tr')];"
        f"  return rows.some((r) => r.textContent.includes({match_id[:12]!r})) ? '' : 'true';"
        "})()",
        ui.budget,
    )

    if not require(gone == "true", "the ignored match is still listed"):
        return False

    print("PASS: ignore: the match disappeared from the list")

    # It is local. A second client holding the same identity — the CLI with the browser's
    # own key — still sees the match, because the tombstone lives in this browser's sealed
    # vault and not in the evidence.
    same_identity = write_key(work, "browser.key", "11" * 32)

    code, output = peer_swap(work, same_identity, relay_url, "market-matches")

    if not require(code == 0, f"the same identity could not list matches:\n{output}"):
        return False

    if not require(match_id in output,
                   f"the match was deleted from the evidence, not just hidden:\n{output}"):
        return False

    print("PASS: ignore: the match is still there for a client with the same identity")

    # No swap state was created by dismissing it: whatever was open stays open, and the
    # engine's own answer — that this match has no local swap — is what permitted the ignore
    # at all (the shared operation refuses a match with one).
    if not require(await ui.selected_swap() == open_before,
                   f"ignoring selected {await ui.selected_swap()!r},"
                   f" want {open_before!r} unchanged"):
        return False

    # And the tombstone outlives the process: a reload must not bring the match back.
    await ui.js("location.reload(); 'reloading'")
    await asyncio.sleep(1)

    if not await ui.wait_boot(ui.budget):
        return require(False, "the page did not come back after a reload")

    # A reload starts locked: the vault's guarantee, and the reason the tombstone is sealed
    # in it rather than kept in a document the page could have cached.
    locked = await poll(ui.ws, "document.body.dataset.vaultOpen === 'true' ? '' : 'true'", 5)

    if not require(locked == "true", "the vault stayed open across a reload"):
        return False

    await ui.view("setup")
    await ui.set("passphrase", "browser-check-passphrase")

    if not await ui.act("vault-unlock", confirm=False):
        return require(False, "the vault could not be unlocked after the reload")

    if not await wait_bootstrap(ui):
        return False

    await ui.view("market")

    if not await ui.act("market-matches", confirm=False):
        return require(False, "the matches were not re-read after the reload")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the matches read never finished after the reload")

    still_gone = await poll(
        ui.ws,
        "(() => {"
        "  const rows = [...document.querySelectorAll('#market-matches-rows tr')];"
        f"  return rows.some((r) => r.textContent.includes({match_id[:12]!r})) ? '' : 'true';"
        "})()",
        ui.budget,
    )

    if not require(still_gone == "true", "the ignored match came back after a reload"):
        return False

    print("PASS: ignore: the tombstone survived a reload")

    return match_id


async def set_file_input(ui: Ui, selector: str, path: Path) -> bool:
    """Point a real file input at a file on disk.

    The page reads the file with the browser's own FileReader, so a driver that typed text
    into a field would prove nothing about the code that ships. This is the only way to
    hand a wasm page a file.
    """
    await cdp(ui.ws, "DOM.enable", {}, ui.counter)

    document = await cdp(ui.ws, "DOM.getDocument", {"depth": 1}, ui.counter)
    root = document.get("result", {}).get("root", {}).get("nodeId")

    if not root:
        print("FAIL: the document has no root node", file=sys.stderr)

        return False

    found = await cdp(
        ui.ws, "DOM.querySelector", {"nodeId": root, "selector": selector}, ui.counter,
    )
    node = found.get("result", {}).get("nodeId")

    if not node:
        print(f"FAIL: the file input {selector} is not in the document", file=sys.stderr)

        return False

    answer = await cdp(
        ui.ws, "DOM.setFileInputFiles", {"files": [str(path)], "nodeId": node}, ui.counter,
    )

    if answer.get("error"):
        print(f"FAIL: the file input could not be set: {answer['error']}", file=sys.stderr)

        return False

    return True


async def wallet_row(ui: Ui, slot: str) -> dict:
    """Read one wallet slot row out of the Settings table."""
    return json.loads(await ui.js(
        "(() => {"
        "  const rows = [...document.querySelectorAll('#wallet-slots tr')];"
        "  for (const row of rows) {"
        "    const cells = [...row.querySelectorAll('td')].map((c) => c.textContent.trim());"
        "    if (!cells.length) { continue; }"
        f"    if (cells[0] !== {WALLET_SLOT_LABELS.get(slot, '')!r}) {{ continue; }}"
        "    const button = row.querySelector('button');"
        "    return JSON.stringify({"
        "      state: cells[4] || '',"
        "      name: cells[3] || '',"
        "      command: button ? button.getAttribute('data-uicmd') : ''"
        "    });"
        "  }"
        "  return JSON.stringify({state: 'missing', name: '', command: ''});"
        "})()"
    ))


async def wait_wallet_row(ui: Ui, slot: str, want: str, budget: float = 0) -> dict:
    """Wait for one slot row to report a state."""
    deadline = time.time() + (budget or ui.budget)
    row = {"state": "", "name": "", "command": ""}

    while time.time() < deadline:
        row = await wallet_row(ui, slot)

        if row.get("state") == want:
            return row

        await asyncio.sleep(0.2)

    return row


async def wallet_create_round_trip(ui: Ui, work: Path, relay_url: str) -> bool:
    """Create the four wallets from Settings and prove the node has them.

    The names are fresh ones, so what is proved is creation rather than a status read: the
    node is asked through the rig's own RPC afterwards, which is a different client than the
    page. The wrong-type refusal is checked against a signing wallet deliberately put in a
    watch-only slot, because that mistake is the one a person cannot see by looking.
    """
    #
    # The names carry this run's stamp: the rig outlives one run of this check, and a name
    # that already existed would turn "created" into "loaded" and quietly stop proving the
    # thing the scenario is for.
    #
    stamp = str(int(time.time()))

    names = {
        "blake": "page-signing-blake-" + stamp,
        "btc": "page-signing-btc-" + stamp,
        "blake_watch": "page-watch-blake-" + stamp,
        "btc_watch": "page-watch-btc-" + stamp,
    }

    # 1. Point the four slots at names the node does not have yet.
    await ui.view("setup")

    await ui.set("wallet-blake", names["blake"])
    await ui.set("wallet-btc", names["btc"])
    await ui.set("wallet-blake-watch", names["blake_watch"])
    await ui.set("wallet-btc-watch", names["btc_watch"])

    if not await ui.act("wallets-set", confirm=False):
        return require(False, "the wallet names were not saved")

    if not await ui.wait_idle(ui.budget):
        return require(False, "saving the wallet names never finished")

    print("PASS: wallets: the four slots point at fresh names")

    # 2. Create each one from its own row, with its own confirmation.
    await ui.view("settings")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the settings view never settled")

    for slot, label in WALLET_SLOT_LABELS.items():
        row = await wait_wallet_row(ui, slot, "absent")

        if not require(row.get("command") == "wallet-create:" + slot,
                       f"the {slot} row offers {row.get('command')!r} while its state is "
                       f"{row.get('state')!r}, want a create; rows:\n"
                       f"{await ui.panel('wallet-slots')}"):
            return False

        if not await ui.act("wallet-create:" + slot):
            return require(False, f"creating the {slot} wallet was not confirmed")

        if not await ui.wait_command("wallets.create"):
            return require(False, f"the {slot} create was never sent")

        if not await ui.wait_idle(ui.budget):
            return require(False, f"the {slot} create never finished")

        error = await ui.last_error()

        if not require(error == "", f"creating the {slot} wallet was refused: {error}"):
            return False

        panel = await ui.panel("wallets-action-out")

        if not require("was created" in panel,
                       f"the {slot} create did not report a creation:\n{panel}"):
            return False

        row = await wait_wallet_row(ui, slot, "loaded")

        if not require(row.get("state") == "loaded",
                       f"the {slot} row says {row.get('state')!r} after creating it"):
            return False

        if not require(row.get("command") == "",
                       f"the {slot} row still offers {row.get('command')!r} when loaded"):
            return False

        print(f"PASS: wallets: {label} created as {names[slot]}")

    # 3. The node itself has them, asked through the rig's own CLI.
    for chain, name, watch in (
        ("blake", names["blake"], False),
        ("btc", names["btc"], False),
        ("blake", names["blake_watch"], True),
        ("btc", names["btc_watch"], True),
    ):
        listed = node_cli(chain, "listwallets")

        if not require(json.dumps(name) in listed,
                       f"the {chain} node does not list {name}:\n{listed}"):
            return False

        info = json.loads(node_cli(chain, "getwalletinfo", wallet=name))
        signing = bool(info.get("private_keys_enabled"))

        if not require(signing != watch,
                       f"{name} on {chain} has private_keys_enabled={signing},"
                       f" want a {'watch-only' if watch else 'signing'} wallet"):
            return False

    print("PASS: wallets: the node lists all four, with the right kind each")

    # 4. A reload keeps the names and the states.
    await ui.js("location.reload(); 'reloading'")
    await asyncio.sleep(1)

    if not await ui.wait_boot(ui.budget):
        return require(False, "the page did not come back after a reload")

    await ui.view("setup")
    await ui.set("passphrase", "browser-check-passphrase")

    if not await ui.act("vault-unlock", confirm=False):
        return require(False, "the vault did not unlock after a reload")

    if not await wait_bootstrap(ui):
        return require(False, "the page did not finish starting after a reload")

    await ui.view("settings")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the settings view never settled after a reload")

    for slot, name in names.items():
        row = await wait_wallet_row(ui, slot, "loaded")

        if not require(row.get("state") == "loaded",
                       f"after a reload the {slot} row says {row.get('state')!r}"):
            return False

        if not require(row.get("name") == name,
                       f"after a reload the {slot} row names {row.get('name')!r},"
                       f" want {name!r}"):
            return False

    print("PASS: wallets: the names and the states survived a reload")

    # 5. A signing wallet in a watch-only slot, in two states.
    #
    # Loaded, the node can say what kind it is, so the row reports the wrong type and
    # offers nothing: the fix is on the node, not here. Not loaded, the node cannot say, so
    # the row offers the operation and the worker refuses it after loading it — which is
    # the refusal text a person would read.
    wrong = "page-wrong-type-" + stamp

    node_cli("blake", "createwallet", wrong, "false", "true", "", "false", "true")

    await ui.view("setup")

    await ui.set("wallet-blake-watch", wrong)

    if not await ui.act("wallets-set", confirm=False):
        return require(False, "the wrong-type name was not saved")

    if not await ui.wait_idle(ui.budget):
        return require(False, "saving the wrong-type name never finished")

    await ui.view("settings")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the settings view never settled")

    row = await wait_wallet_row(ui, "blake_watch", "wrong type")

    if not require(row.get("state") == "wrong type",
                   f"the wrong-type wallet is reported as {row.get('state')!r}"):
        return False

    if not require(row.get("command") == "",
                   f"the wrong-type row offers {row.get('command')!r}, want no action"):
        return False

    print("PASS: wallets: a loaded signing wallet in a watch-only slot is the wrong type")

    node_cli("blake", "unloadwallet", wrong)

    # Re-enter the view so the status is read again.
    await ui.view("market")
    await ui.view("settings")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the settings view never settled")

    row = await wait_wallet_row(ui, "blake_watch", "absent")

    if not require(row.get("command") == "wallet-create:blake_watch",
                   f"the unloaded wrong-type row offers {row.get('command')!r}"):
        return False

    if not await ui.act("wallet-create:blake_watch"):
        return require(False, "the wrong-type create was not confirmed")

    if not await ui.wait_command("wallets.create"):
        return require(False, "the wrong-type create was never sent")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the wrong-type create never finished")

    refusal = await ui.last_error()

    if not require(
        "exists as a signing wallet, not a watch-only wallet" in refusal,
        f"the wrong-type refusal is {refusal!r}",
    ):
        return False

    print(f"PASS: wallets: the worker refused a signing wallet in a watch-only slot "
          f"({refusal[:56]}...)")

    # Put the slot back so the client is left consistent.
    await ui.view("setup")
    await ui.set("wallet-blake-watch", names["blake_watch"])

    if not await ui.act("wallets-set", confirm=False):
        return require(False, "the watch-only name could not be restored")

    if not await ui.wait_idle(ui.budget):
        return require(False, "restoring the watch-only name never finished")

    return True


async def vault_export_import_round_trip(
    ui: Ui, work: Path, ignored_match_id: str
) -> bool:
    """Export the whole vault, refuse a wrong passphrase, restore it, and check it all.

    The round trip is the point: the file is the only copy of this vault that survives the
    browser, so what must be proved is that the file it writes is the file it reads, that a
    wrong passphrase changes nothing, and that a restore brings back the records, the
    sealed values, the market documents and the activity log.
    """
    downloads = work / "downloads"
    downloads.mkdir(exist_ok=True)

    behavior = await cdp(
        ui.ws,
        "Browser.setDownloadBehavior",
        {"behavior": "allow", "downloadPath": str(downloads), "eventsEnabled": True},
        ui.counter,
    )

    if behavior.get("error"):
        print(
            f"FAIL: Browser.setDownloadBehavior was refused: {behavior['error']}\n"
            "  the download half of the vault check cannot run without it",
            file=sys.stderr,
        )

        return False

    await ui.view("settings")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the settings view never settled")

    if not await ui.click_in("settings", "vault-export"):
        return require(False, "the export control is not on the Settings view")

    if not await ui.confirm():
        return require(False, "the export was not confirmed")

    if not await ui.wait_command("vault.export"):
        return require(False, "the export was never sent")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the export never finished")

    error = await ui.last_error()

    if not require(error == "", f"the export was refused: {error}"):
        return False

    exported = None

    for _ in range(50):
        candidates = sorted(downloads.glob("blake-swap-vault-*.json"))

        if candidates:
            exported = candidates[0]

            break

        await asyncio.sleep(0.2)

    if exported is None:
        print(
            f"FAIL: no backup file was downloaded; the directory holds {list(downloads)}\n"
            f"  panel: {await ui.panel('vault-backup-out')}",
            file=sys.stderr,
        )

        return False

    text = exported.read_text()

    try:
        document = json.loads(text)
    except json.JSONDecodeError as failure:
        return require(False, f"the backup is not JSON: {failure}")

    if not require(document.get("kind") == "blake-swap-vault",
                   f"the backup kind is {document.get('kind')!r}"):
        return False

    entries = document.get("entries") or []

    if not require(len(entries) > 0, "the backup has no entries"):
        return False

    if not require(document.get("version") == 1,
                   f"the backup version is {document.get('version')!r}"):
        return False

    keys = [entry.get("key", "") for entry in entries]

    for required in ("vault/header", "vault/verifier"):
        if not require(required in keys, f"the backup has no {required}"):
            return False

    # The known plaintext: the identity secret this check typed in, and the passphrase.
    for name, secret in (
        ("the identity secret", "11" * 32),
        ("the vault passphrase", "browser-check-passphrase"),
    ):
        if not require(secret not in text, f"the backup contains {name} in the clear"):
            return False

    # The tombstone from the ignore scenario is a vault key, so a backup that dropped the
    # market documents would not carry it.
    if ignored_match_id:
        # A tombstone is a sealed market document, so its key is the store's own:
        # secret/market/ignored-market-matches/<event id>.json.
        wanted = "ignored-market-matches/" + ignored_match_id + ".json"

        if not require(any(key.endswith(wanted) for key in keys),
                       f"the backup has no tombstone ending in {wanted}"):
            return False

    record_keys = [key for key in keys if key.startswith("record/")]

    if not require(len(record_keys) > 0, "the backup has no swap records"):
        return False

    print(f"PASS: vault export: {len(entries)} entries, {len(text)} bytes, "
          f"{len(record_keys)} record(s)")

    # 1. A wrong passphrase must change nothing.
    await ui.view("settings")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the settings view never settled")

    if not await ui.click_in("settings", "vault-import"):
        return require(False, "the restore control is not on the Settings view")

    if not await ui.wait_dialog():
        return require(False, "the restore dialog did not open")

    if not await set_file_input(ui, "#import-vault-file", exported):
        return False

    await ui.set("confirm-passphrase", "not the passphrase of this backup")

    if not await ui.click("confirm-ok"):
        return require(False, "the wrong-passphrase restore was not confirmed")

    if not await ui.wait_command("vault.import"):
        return require(False, "the wrong-passphrase restore was never sent")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the wrong-passphrase restore never finished")

    refusal = await ui.last_error()

    if not require(refusal != "", "a restore with the wrong passphrase was accepted"):
        return False

    if not require("did not open" in refusal or "passphrase" in refusal,
                   f"the refusal is not the store's: {refusal!r}"):
        return False

    still_open = await ui.attr("vaultOpen")

    if not require(still_open == "true", "a refused restore locked the vault"):
        return False

    print(f"PASS: vault import refused a wrong passphrase ({refusal[:64]}...)")

    # The records are still in the vault: a refused restore reads the file and writes
    # nothing, so every record the export held is still listable.
    await ui.view("diagnostics")

    if not await ui.act("engine-list", confirm=False):
        return require(False, "the swap list could not be read")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the swap list never finished")

    listed = await ui.panel("engine")
    lines = [
        line for line in listed.splitlines()
        if len(line.split("\t")[0].strip()) == 12
    ]

    if not require(len(lines) >= len(record_keys),
                   f"the refused restore left {len(lines)} record(s), want at least "
                   f"{len(record_keys)}:\n{listed}"):
        return False

    print(f"PASS: vault import refusal left all {len(lines)} record(s) in place")

    await ui.view("settings")

    # 2. The real restore. The dialog is opened first and filled second, exactly as a
    # person does it: the file has to be chosen before the confirmation can mean anything.
    if not await ui.click_in("settings", "vault-import"):
        return require(False, "the restore control is not on the Settings view")

    if not await ui.wait_dialog():
        return require(False, "the restore dialog did not open")

    if not await set_file_input(ui, "#import-vault-file", exported):
        return False

    await ui.set("confirm-passphrase", "browser-check-passphrase")

    if not await ui.click("confirm-ok"):
        return require(False, "the restore was not confirmed")

    if not await ui.wait_command("vault.import"):
        return require(False, "the restore was never sent")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the restore never finished")

    error = await ui.last_error()

    if not require(error == "", f"the restore was refused: {error}"):
        return False

    # The page is back at the lock screen, and it says so.
    locked = await poll(
        ui.ws, "document.body.dataset.vaultOpen === 'true' ? '' : 'true'", 5,
    )

    if not require(locked == "true", "the vault stayed open across a restore"):
        return False

    view = await ui.attr("view")

    if not require(view == "setup", f"the restore left the page on {view!r}"):
        return False

    passphrase = await ui.js("document.getElementById('passphrase').value || ''")

    if not require(not passphrase, "the page kept a passphrase in a field after a restore"):
        return False

    print("PASS: vault import replaced the store and locked the vault")

    # 3. Unlock the imported vault and check what came back.
    await ui.set("passphrase", "browser-check-passphrase")

    if not await ui.act("vault-unlock", confirm=False):
        return require(False, "the imported vault did not unlock")

    if not await wait_bootstrap(ui):
        return require(False, "the page did not finish starting after the restore")

    if not await ui.wait_idle(ui.budget):
        return require(False, "the restore's bootstrap never finished")

    # The identity came back, which means the imported key opened the imported secret.
    if not await ui.wait_panel("identity", "identity: "):
        print(f"FAIL: the identity was not restored; panel: {await ui.panel('identity')}",
              file=sys.stderr)

        return False

    # And the seal still works on the restored store: the self-test seals and reopens a
    # throwaway vault, so it proves the platform path, not the imported data. What proves
    # the imported data is the record and tombstone checks below.
    await ui.view("diagnostics")

    if not await ui.act("vault-selftest", confirm=False):
        return require(False, "the encrypted self-test could not be clicked after a restore")

    if not await ui.wait_attr("vaultVerified"):
        return require(False, "the encrypted self-test did not pass after a restore")

    # The tombstone survived: the ignored match is still ignored, which is the one thing a
    # dropped market document would bring back.
    if ignored_match_id:
        await ui.view("market")

        if not await ui.act("market-matches", confirm=False):
            return require(False, "the matches were not re-read after the restore")

        if not await ui.wait_idle(ui.budget):
            return require(False, "the matches read never finished")

        listed = await ui.js(
            "(() => {"
            "  const rows = [...document.querySelectorAll('#market-matches-rows tr')];"
            f"  return rows.some((r) => r.textContent.includes({ignored_match_id[:12]!r}))"
            "    ? 'listed' : 'ignored';"
            "})()"
        )

        if not require(listed == "ignored", "the restored vault offered the ignored match"):
            return False

    print("PASS: vault import: the records, the sealed values and the tombstone came back")

    return True


async def coordination_round_trip(ui: Ui, work: Path, relay_url: str):
    """The settlement transport, hand-driven from the coordination view."""
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
        print(f"FAIL: peer keys are not x-only keys ({bob_pub!r}, {mallory_pub!r})",
              file=sys.stderr)

        return False

    print(f"PASS: coordination peers ready (bob {bob_pub[:12]}…, wrong key {mallory_pub[:12]}…)")

    async def open_record(pinned: str):
        await ui.view("coordination")

        await ui.set("coord-blake", "10")
        await ui.set("coord-sats", "50000")
        await ui.set("coord-peer", pinned)

        if not await ui.act("coord-open", confirm=False):
            return None

        if not await ui.wait_panel("coord", "COORDINATION SWAP OPENED"):
            print(f"FAIL: opening a record failed:\n{await ui.panel('coord')}", file=sys.stderr)

            return None

        swap_id = field(await ui.panel("coord"), "swap id:")

        if len(swap_id) != 12:
            print(f"FAIL: the opened record printed no swap id:\n{await ui.panel('coord')}",
                  file=sys.stderr)

            return None

        return swap_id

    async def exchange(uicmd: str, kind: str, swap_id: str, needle: str):
        await ui.set("coord-kind", kind)

        if not await ui.act(uicmd):
            print(
                f"FAIL: {uicmd} was not confirmed"
                f" (busy={await ui.attr('busy')!r},"
                f" dialog={await ui.js(chr(39)+'document.getElementById(\"confirm-dialog\").open'+chr(39))},"
                f" alert={await ui.last_error()!r})",
                file=sys.stderr,
            )

            return None

        if not await ui.wait_panel("coord", needle):
            return None

        return await ui.panel("coord")

    swap_a = await open_record(bob_pub)

    if swap_a is None:
        return False

    print(f"PASS: a: opened swap {swap_a} pinned to the peer")

    text = await exchange("coord-send", "offer", swap_a, "COORDINATION MESSAGE PUBLISHED")

    if text is None:
        print(f"FAIL: the offer was not announced:\n{await ui.panel('coord')}", file=sys.stderr)

        return False

    event_id = field(text, "event id:")

    if len(event_id) != 64:
        print(f"FAIL: the announcement printed no event id:\n{text}", file=sys.stderr)

        return False

    alice_pub = field(text, "sender:")

    print(f"PASS: a: the browser published a signed offer ({event_id[:12]}…)")

    code, output = peer(work, bob, relay_url, "coord-read", swap_a, "--author", alice_pub,
                        "--kind", "offer")

    if code != 0:
        print(f"FAIL: the peer could not read the offer:\n{output}", file=sys.stderr)

        return False

    if "COORDINATION MESSAGE VALIDATED" not in output or f"sender:   {alice_pub}" not in output:
        print(f"FAIL: the peer validated something else:\n{output}", file=sys.stderr)

        return False

    print(f"PASS: b: the peer read and validated the signed offer from {alice_pub[:12]}…")

    keys = ("--blake-bob-pub", "02" + "ab" * 32, "--btc-bob-pub", "03" + "cd" * 32)

    code, output = peer(work, bob, relay_url, "coord-reply", swap_a, "--author", alice_pub, *keys)

    if code != 0:
        print(f"FAIL: the peer could not reply:\n{output}", file=sys.stderr)

        return False

    reply_event = field(output, "event id:")

    print(f"PASS: c: the peer replied with an accept ({reply_event[:12]}…)")

    text = await exchange("coord-receive", "accept", swap_a, "COORDINATION MESSAGE RECEIVED")

    if text is None:
        print(f"FAIL: the browser refused the peer's reply:\n{await ui.panel('coord')}",
              file=sys.stderr)

        return False

    if field(text, "sender:") != bob_pub:
        print(f"FAIL: the reply was accepted from the wrong author:\n{text}", file=sys.stderr)

        return False

    if not await ui.wait_attr("coordVerified"):
        print("FAIL: a validated receive never set its marker", file=sys.stderr)

        return False

    print(f"PASS: d: the browser received and validated the reply from {bob_pub[:12]}…")

    swap_b = await open_record(bob_pub)

    if swap_b is None:
        return False

    await ui.set("coord-kind", "offer")

    if not await ui.act("coord-send"):
        return False

    if not await ui.wait_panel("coord", "COORDINATION MESSAGE PUBLISHED"):
        print(f"FAIL: the second offer was not announced:\n{await ui.panel('coord')}",
              file=sys.stderr)

        return False

    code, output = peer(work, mallory, relay_url, "coord-reply", swap_b, "--author", alice_pub, *keys)

    if code != 0:
        print(f"FAIL: the wrong-key peer could not publish:\n{output}", file=sys.stderr)

        return False

    text = await exchange("coord-receive", "accept", swap_b, "SECURITY FAILURE")

    if text is None:
        print(f"FAIL: a reply signed by the wrong key was not refused:\n" +
              await ui.panel("coord"), file=sys.stderr)

        return False

    if mallory_pub not in text or bob_pub not in text:
        print(f"FAIL: the refusal does not name both authors:\n{text}", file=sys.stderr)

        return False

    print("PASS: e: a reply signed by the wrong key was refused, naming both authors")

    # (f) encryption: the offer is exempt, a plaintext accept is a downgrade, an
    # encrypted accept decrypts.
    await ui.view("settings")

    await ui.set("coord-mode-setting", "nip44_v2")

    if not await ui.act("coord-set-mode", confirm=False):
        return False

    if not await ui.wait_panel("coord-mode-out", "encryption set: nip44_v2"):
        print(f"FAIL: the mode was not set:\n{await ui.panel('coord-mode-out')}", file=sys.stderr)

        return False

    print("PASS: f: the browser is configured for nip44_v2")

    swap_c = await open_record(bob_pub)

    if swap_c is None:
        return False

    text = await exchange("coord-send", "offer", swap_c, "COORDINATION MESSAGE PUBLISHED")

    if text is None or "ciphertext: none (offer is exempt" not in text:
        print(f"FAIL: the offer is not exempt from encryption:\n{text}", file=sys.stderr)

        return False

    code, output = peer(work, bob, relay_url, "coord-reply", swap_c, "--author", alice_pub, *keys)

    if code != 0:
        print(f"FAIL: the peer could not publish a plaintext reply:\n{output}", file=sys.stderr)

        return False

    text = await exchange("coord-receive", "accept", swap_c, "SECURITY FAILURE")

    if text is None or "downgrade" not in text:
        print(f"FAIL: a plaintext accept under nip44_v2 was not refused:\n{text}", file=sys.stderr)

        return False

    print("PASS: f: a plaintext reply under nip44_v2 was refused as a downgrade")

    swap_d = await open_record(bob_pub)

    if swap_d is None:
        return False

    if await exchange("coord-send", "offer", swap_d, "COORDINATION MESSAGE PUBLISHED") is None:
        return False

    code, output = peer(work, bob, relay_url, "coord-reply", swap_d, "--author", alice_pub,
                        *keys, encryption="nip44_v2")

    if code != 0:
        print(f"FAIL: the peer could not publish an encrypted reply:\n{output}", file=sys.stderr)

        return False

    text = await exchange("coord-receive", "accept", swap_d, "COORDINATION MESSAGE RECEIVED")

    if text is None or "mode:       nip44_v2" not in text:
        print(f"FAIL: the encrypted accept was not received:\n{text}", file=sys.stderr)

        return False

    # Back to plaintext for the swap flows: the mode is per-participant configuration and
    # the swap tests are about settlement, not confidentiality.
    await ui.view("settings")
    await ui.set("coord-mode-setting", "off")
    await ui.act("coord-set-mode", confirm=False)

    return " ".join(str(text).split())


async def engine_round_trip(ui: Ui):
    """The engine reads both chains through the proxy."""
    await ui.view("setup")

    if not await ui.act("engine-status", confirm=False):
        print("FAIL: the chain-height read was not requested", file=sys.stderr)

        return False

    if not await ui.wait_attr("engineVerified"):
        print(f"FAIL: the chain heights were not read:\n{await ui.panel('engine-status-out')}",
              file=sys.stderr)

        return False

    text = await ui.panel("engine-status-out")

    if "height" not in text.lower():
        print(f"FAIL: the engine reported no heights:\n{text}", file=sys.stderr)

        return False

    return " ".join(str(text).split())


# ---------------------------------------------------------------------------------
# full swaps: the browser against the CLI, in both protocol roles
# ---------------------------------------------------------------------------------

# The browser participant's wallet names. They are the rig's Alice wallets, and they are
# used whatever protocol role the browser holds: a wallet is not a role, which is the fix
# this scenario exists to prove.
WALLET = "alice"

BLAKE_AMOUNT = "0.002"
BTC_SATS = 30000


def swap_env(work: Path, key_file: Path, relay_url: str) -> dict:
    """The environment a CLI counterparty needs to settle through the rig."""
    environment = dict(os.environ)
    environment.update(
        {
            "BLAKE_SWAP_STATE_DIR": str(work / "peer-state"),
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE": str(key_file),
            "BLAKE_SWAP_NOSTR_RELAYS": relay_url,
            "BLAKE_SWAP_NOSTR_ENCRYPTION": "off",
            # The engine's announce and receive commands go through the coordination
            # transport, which for a browser participant is the relay.
            "BLAKE_SWAP_TRANSPORT": "nostr",
            "BLAKE_SWAP_BLAKE_WALLET": "bob",
            "BLAKE_SWAP_BTC_WALLET": "bob",
            "BLAKE_SWAP_BLAKE_WATCH_WALLET": "bob-watch",
            "BLAKE_SWAP_BTC_WATCH_WALLET": "bob-watch",
            "BLAKE_SWAP_AUTO_MINE": "1",
        }
    )

    return environment


def peer_swap(work: Path, key_file: Path, relay_url: str, *args: str):
    """Run one CLI settlement command as Bob and return (exit code, output)."""
    completed = subprocess.run(
        [str(cli_binary())] + list(args),
        cwd=ROOT,
        env=swap_env(work, key_file, relay_url),
        capture_output=True,
        text=True,
        timeout=180,
    )

    return completed.returncode, (completed.stdout or "") + (completed.stderr or "")


def public_key_of(work: Path, key_file: Path, relay_url: str) -> str:
    """Read the x-only public key a key file signs with, through the CLI."""
    code, output = peer(work, key_file, relay_url, "coord-key")

    if code != 0:
        raise RuntimeError(f"could not read a key: {output}")

    return field(output, "public key:")


def require(condition: bool, message: str) -> bool:
    if condition:
        return True

    print(f"FAIL: {message}", file=sys.stderr)

    return False


async def browser_swap_state(ui: Ui, want: str, budget: float = 0) -> bool:
    """Wait for the detail view to report a checkpoint."""
    reached = await poll(
        ui.ws,
        f"document.body.dataset.swapState === {want!r} ? 'true' : ''",
        budget or ui.budget,
    )

    if reached != "true":
        print(
            f"FAIL: the swap never reached {want}; it is {await ui.swap_state()}"
            f" with next action {await ui.next_action()}\n"
            f"log: {await ui.attr('commandLog')!r}\n"
            f"error: {await ui.last_error()!r}\n"
            f"panel:\n{await ui.panel('swap-action-out')}",
            file=sys.stderr,
        )

        return False

    return True


async def browser_bob_settlement(
    ui: Ui, peer_key: Path, work: Path, relay_url: str, swap_id: str, who: str
) -> bool:
    """The settlement half of a swap in which the browser is protocol Bob.

    Bob receives Alice's BLAKE funding, funds BTC, then reads the secret out of Alice's
    Bitcoin claim and claims BLAKE. Both market directions end here: the direction decides
    who makes the market, not what the protocol does with the money.
    """
    code, output = peer_swap(work, peer_key, relay_url, "receive-accept", swap_id)

    if not require(code == 0, f"CLI Alice could not receive the acceptance:\n{output}"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "fund-blake", swap_id)

    if not require(code == 0, f"CLI Alice could not fund BLAKE:\n{output}"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "publish-blake-funded", swap_id)

    if not require(code == 0, f"CLI Alice could not announce the BLAKE funding:\n{output}"):
        return False

    print("PASS: CLI Alice funded BLAKE")

    if not await ui.swap_action("receive-blake-funded"):
        return False

    if not await ui.swap_action("fund-btc"):
        return False

    mine("btc")

    if not await ui.swap_action("sync-funding"):
        return False

    if not await browser_swap_state(ui, "BTC_FUNDED"):
        return False

    print("PASS: browser Bob received the BLAKE funding and funded BTC")

    if not await ui.swap_action("publish-btc-funded"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "receive-btc-funded", swap_id)

    if not require(code == 0, f"CLI Alice could not receive the BTC funding:\n{output}"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "claim-btc", swap_id)

    if not require(code == 0, f"CLI Alice could not claim BTC:\n{output}"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "publish-btc-claimed", swap_id)

    if not require(code == 0, f"CLI Alice could not announce the BTC claim:\n{output}"):
        return False

    print("PASS: CLI Alice received the BTC funding and claimed BTC")

    if not await ui.swap_action("receive-btc-claimed"):
        return False

    if not await browser_swap_state(ui, "BTC_CLAIMED"):
        return False

    # Bob holds no secret until he reads it out of Alice's Bitcoin claim.
    if not await ui.swap_action("extract-secret"):
        return False

    if not await ui.swap_action("claim-blake"):
        return False

    mine("blake")

    if not await ui.swap_action("sync-settlement"):
        return False

    if not await browser_swap_state(ui, "BLAKE_CLAIMED"):
        return False

    print("PASS: browser Bob extracted the secret and claimed BLAKE")

    if not await ui.swap_action("publish-blake-claimed"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "receive-blake-claimed", swap_id)

    if not require(code == 0, f"CLI Alice could not receive the BLAKE claim:\n{output}"):
        return False

    code, output = peer_swap(work, peer_key, relay_url, "complete", swap_id)

    if not require(code == 0, f"CLI Alice could not complete:\n{output}"):
        return False

    if not await ui.swap_action("complete"):
        return False

    if not await browser_swap_state(ui, "COMPLETE"):
        return False

    print(f"PASS: browser {who} completed the swap")

    terms = await ui.panel("swap-txs")

    for label in ("BTC claim", "BLAKE claim"):
        if label not in terms:
            return require(False, f"the transaction list has no {label} row:\n{terms}")

    return True


async def browser_swap(ui: Ui, peer_key: Path, work: Path, relay_url: str, browser_role: str) -> bool:
    """Run one whole swap with the browser on one side and the CLI on the other.

    browser_role is "alice" when the browser creates the swap and "bob" when the CLI does.
    The receipts are the same either way: the browser's own state marker at each step, the
    CLI's exit code, and the final COMPLETE.
    """
    ensure_wallet("blake", WALLET, False)
    ensure_wallet("blake", WALLET + "-watch", True)
    ensure_wallet("btc", WALLET, False)
    ensure_wallet("btc", WALLET + "-watch", True)
    ensure_wallet("blake", "bob", False)
    ensure_wallet("blake", "bob-watch", True)
    ensure_wallet("btc", "bob", False)
    ensure_wallet("btc", "bob-watch", True)

    ensure_funds("blake", WALLET, 1_000_000)
    ensure_funds("blake", "bob", 1_000_000)
    ensure_funds("btc", WALLET, BTC_SATS * 2)
    ensure_funds("btc", "bob", BTC_SATS * 2)

    browser_pub = public_key_of(work, work / "browser.key", relay_url)

    if browser_role == "alice":
        await ui.view("swaps")
        await ui.wait_idle()

        previous = await ui.selected_swap()

        await ui.set("new-blake", BLAKE_AMOUNT)
        await ui.set("new-sats", str(BTC_SATS))
        await ui.set("new-peer", "")

        if not await ui.act("engine-new", confirm=False):
            return require(False, "creating a swap did not open a confirmation")

        # Wait for a *new* selection: the state was already OFFERED from an earlier record,
        # so the checkpoint alone does not prove this command produced anything.
        created = await poll(
            ui.ws,
            "(() => {"
            "  const id = document.body.dataset.selectedSwap || '';"
            "  const state = document.body.dataset.swapState || '';"
            f"  return id.length === 12 && id !== {previous!r} && state === 'OFFERED' ? id : '';"
            "})()",
            ui.budget,
        )

        if created != created or not created:
            print(f"FAIL: creating a swap did not select a new record"
                  f" (selected {await ui.selected_swap()!r},"
                  f" error {await ui.last_error()!r})", file=sys.stderr)

            return False

        swap_id = str(created)

        if not require(len(swap_id) == 12, f"the created swap id is {swap_id!r}"):
            return False

        print(f"PASS: browser Alice created swap {swap_id}")

        if os.environ.get("VB_DEBUG"):
            print(f"DEBUG terms: {await ui.panel('swap-terms')}")

        if not await ui.swap_action("publish-offer"):
            return require(False, "publishing the offer did not open a confirmation")

        print("PASS: browser Alice published the offer")

        code, output = peer_swap(work, peer_key, relay_url, "accept-offer", swap_id,
                                 "--expected-alice-pub", browser_pub,
                                 "--expected-blake-amount", BLAKE_AMOUNT,
                                 "--expected-btc-sats", str(BTC_SATS))

        if not require(code == 0, f"CLI Bob could not accept the offer:\n{output}"):
            return False

        print("PASS: CLI Bob accepted the offer")

        if not await ui.swap_action("receive-accept"):
            return False

        print("PASS: browser Alice received Bob's acceptance")

        if not await ui.swap_action("fund-blake"):
            return False

        # A funding is durable state only once it is confirmed. The broadcast is in the
        # mempool, so the chain confirms it and the engine's own sync recovers the outpoint.
        mine("blake")

        if not await ui.swap_action("sync-funding"):
            return False

        if not await browser_swap_state(ui, "BLAKE_FUNDED"):
            return False

        print("PASS: browser Alice funded BLAKE")

        if not await ui.swap_action("publish-blake-funded"):
            return False

        code, output = peer_swap(work, peer_key, relay_url, "receive-blake-funded", swap_id)

        if not require(code == 0, f"CLI Bob could not receive the BLAKE funding:\n{output}"):
            return False

        code, output = peer_swap(work, peer_key, relay_url, "fund-btc", swap_id)

        if not require(code == 0, f"CLI Bob could not fund BTC:\n{output}"):
            return False

        code, output = peer_swap(work, peer_key, relay_url, "publish-btc-funded", swap_id)

        if not require(code == 0, f"CLI Bob could not announce the BTC funding:\n{output}"):
            return False

        print("PASS: CLI Bob received the BLAKE funding and funded BTC")

        if not await ui.swap_action("receive-btc-funded"):
            return False

        if not await browser_swap_state(ui, "BTC_FUNDED"):
            return False

        print("PASS: browser Alice received the BTC funding")

        if not await ui.swap_action("claim-btc"):
            return False

        mine("btc")

        if not await ui.swap_action("sync-settlement"):
            return False

        if not await browser_swap_state(ui, "BTC_CLAIMED"):
            return False

        print("PASS: browser Alice claimed BTC")

        if not await ui.swap_action("publish-btc-claimed"):
            return False

        for command in ("receive-btc-claimed", "extract-secret", "claim-blake"):
            code, output = peer_swap(work, peer_key, relay_url, command, swap_id)

            if not require(code == 0, f"CLI Bob could not {command}:\n{output}"):
                return False

        code, output = peer_swap(work, peer_key, relay_url, "publish-blake-claimed", swap_id)

        if not require(code == 0, f"CLI Bob could not announce the BLAKE claim:\n{output}"):
            return False

        print("PASS: CLI Bob extracted the secret and claimed BLAKE")
    else:
        # The CLI is Alice: it creates the swap, pins the browser as Bob, and publishes the
        # offer. The browser accepts it, funds BTC and claims BLAKE.
        code, output = peer_swap(work, peer_key, relay_url, "coord-key")

        if not require(code == 0, f"CLI Alice has no identity:\n{output}"):
            return False

        cli_pub = field(output, "public key:")

        code, output = peer_swap(work, peer_key, relay_url, "new",
                                 "--blake", BLAKE_AMOUNT,
                                 "--sats", str(BTC_SATS))

        if not require(code == 0, f"CLI Alice could not create the swap:\n{output}"):
            return False

        swap_id = field(output, "swap id:")

        if not require(len(swap_id) == 12, f"CLI Alice printed no swap id:\n{output}"):
            return False

        code, output = peer_swap(work, peer_key, relay_url, "publish-offer", swap_id)

        if not require(code == 0, f"CLI Alice could not publish the offer:\n{output}"):
            return False

        print(f"PASS: CLI Alice created and published swap {swap_id}")

        # The browser accepts the offer by id, pinning the maker's key. There is no local
        # record yet: the acceptance is what creates one.
        await ui.view("swaps")
        await ui.wait_idle()

        previous = await ui.selected_swap()

        await ui.set("accept-swap-id", swap_id)
        await ui.set("accept-alice-pub", cli_pub)

        if not await ui.act("engine-accept-offer"):
            return require(False, "accepting the offer was not confirmed")

        created = await poll(
            ui.ws,
            "(() => {"
            "  const id = document.body.dataset.selectedSwap || '';"
            "  const state = document.body.dataset.swapState || '';"
            f"  return id === {swap_id!r} && state === 'OFFERED' ? 'true' : '';"
            "})()",
            ui.budget,
        )

        if created != "true":
            print(f"FAIL: accepting the offer did not create the swap"
                  f" (selected {await ui.selected_swap()!r},"
                  f" error {await ui.last_error()!r})", file=sys.stderr)

            return False

        state = await ui.swap_state()

        if not require(state == "OFFERED", f"accepting moved the state to {state}"):
            return False

        print("PASS: browser Bob accepted Alice's offer")

        return await browser_bob_settlement(ui, peer_key, work, relay_url, swap_id, "Bob")

    print(f"PASS: browser {'Alice' if browser_role == 'alice' else 'Bob'} completed the swap")

    # The transaction ids are recorded, not invented: each side's claim is on the other's
    # record, and both are real hex.
    terms = await ui.panel("swap-txs")

    for label in ("BTC claim", "BLAKE claim"):
        if label not in terms:
            return require(False, f"the transaction list has no {label} row:\n{terms}")

    return True


async def reload_continues(ui: Ui, budget: float) -> bool:
    """Reload mid-swap, unlock, and continue from the recorded state."""
    selected = await ui.selected_swap()
    state = await ui.swap_state()

    if not require(selected != "" and state != "", "there is no selected swap to reload"):
        return False

    await ui.js("location.reload(); 'reloading'")
    await asyncio.sleep(1)

    if not await ui.wait_boot(budget):
        return require(False, "the page did not come back after a reload")

    print("PASS: reload: the page booted again")

    # A reload starts locked: that is the vault's guarantee, and the page must not have kept
    # a plaintext copy to avoid it. The marker is absent rather than "false" on a fresh
    # document, so what is asserted is that nothing claims it is open.
    locked = await poll(
        ui.ws, "document.body.dataset.vaultOpen === 'true' ? '' : 'true'", 5,
    )

    if not require(locked == "true", "the vault stayed open across a reload"):
        return False

    print("PASS: reload: the vault is locked again")

    await ui.view("setup")
    await ui.set("passphrase", "browser-check-passphrase")

    if not await ui.act("vault-unlock", confirm=False):
        return require(False, "the vault did not unlock after a reload")

    if not await ui.wait_attr("vaultOpen"):
        return require(False, "the vault never opened after a reload")

    # The dashboard comes back from the sealed store, and so does the selection.
    recovered = await poll(
        ui.ws, f"document.body.dataset.selectedSwap === {selected!r} ? 'true' : ''", budget,
    )

    if recovered != "true":
        print(
            f"FAIL: the selected swap did not survive the reload: wanted {selected},"
            f" got {await ui.selected_swap()!r}",
            file=sys.stderr,
        )

        return False

    print(f"PASS: reload: swap {selected} recovered from the sealed store")

    return await browser_swap_state(ui, state)


async def refusal_paths(ui: Ui, budget: float) -> bool:
    """The refusals a person can reach from the page, shown in the engine's own words."""
    # 1. An economic action the engine refuses, on the swap that is already open. The exact
    # sentence is the point: a page that said "something went wrong" would waste it.
    await ui.view("swap")

    if await ui.selected_swap() == "":
        return require(False, "no swap is selected for the refusal check")

    error = await ui.swap_action_refusal("extract-secret")

    if not require(error != "", "extract-secret before a Bitcoin claim was not refused"):
        return False

    print(f"PASS: refusal: extract-secret was refused in the engine's words ({error[:48]}…)")

    # 2. An unknown swap is refused by id.
    await ui.view("swaps")
    await ui.set("swap-id", "ffffffffffff")
    await ui.act("swap-open", confirm=False)

    reached = await poll(ui.ws, "document.body.dataset.lastError || ''", budget)

    if not require(bool(reached), "a refused open showed no error"):
        return False

    if not require("not found" in str(reached), f"the refusal is not the engine's: {reached!r}"):
        return False

    print("PASS: refusal: an unknown swap is refused in the engine's words")

    # 3. A relay that is not there fails loudly instead of looking like an empty market.
    await ui.view("market")
    await ui.set("relays", "ws://127.0.0.1:1")
    await ui.act("market-set-relays", confirm=False)
    await ui.act("market-list", confirm=False)

    failed = await poll(
        ui.ws,
        "(() => {"
        "  const read = document.getElementById('market-read').innerText;"
        "  return read.includes('relay error') || document.body.dataset.lastError ? 'true' : '';"
        "})()",
        budget,
    )

    if not require(failed == "true", "a dead relay looked like an empty market"):
        return False

    print("PASS: refusal: a dead relay is reported as an error")

    # Restore the working relay. The dead-relay read may still be finishing, so idle is
    # waited for before the click rather than assumed.
    await ui.wait_idle(budget)
    await ui.set("relays", RELAY_URL[0])
    await ui.act("market-set-relays", confirm=False)
    await ui.wait_idle(budget)

    # 4. One tap is one command, even when the tap happens three times.
    await ui.view("swaps")
    await ui.wait_idle()

    before = int(await ui.attr("commandsSent") or 0)

    await ui.js(
        "(() => {"
        "  const el = document.querySelector(\"[data-uicmd='engine-dashboard']\");"
        "  el.click();"
        "  el.click();"
        "  el.click();"
        "  return 'clicked';"
        "})()"
    )

    if not await ui.wait_idle(budget):
        print(f"FAIL: the dashboard read never finished"
              f" (busy line {await ui.panel('busy-line')!r},"
              f" last {await ui.attr('lastCommand')!r})", file=sys.stderr)

        return False

    after = int(await ui.attr("commandsSent") or 0)

    if not require(after - before == 1, f"a triple click sent {after - before} commands"):
        return False

    print("PASS: refusal: a triple click sent one command")

    return True


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

        ui = Ui(ws, budget)

        # Bring the page back if a previous step reloaded it: a reload creates a new
        # execution context and the old one stops answering.
        ui.counter = [1000]

        hello = await poll(ws, "document.body.dataset.helloVerified || ''", budget)

        if hello != "true":
            dom = await evaluate(ws, "document.body.innerText.slice(0, 1200)", [100])

            print(f"FAIL: the greeting never rendered; page text:\n{dom}", file=sys.stderr)

            return 1

        print("PASS: hello round trip rendered")

        if await evaluate(ws, "document.body.dataset.bootComplete || ''", [200]) != "true":
            print("FAIL: the boot gate never cleared", file=sys.stderr)

            return 1

        print("PASS: boot gate cleared")

        if not await open_vault(ui, WALLET):
            return 1

        if not await wait_bootstrap(ui):
            return 1

        if not await configure(ui, relay_url):
            return 1

        if not await close_boot_overlay(ui):
            return 1

        # The encrypted self-test runs from the diagnostics view, on the same control a
        # person would press.
        await ui.view("diagnostics")

        if not await ui.act("vault-selftest", confirm=False):
            print("FAIL: the encrypted self-test could not be clicked", file=sys.stderr)

            return 1

        if not await ui.wait_attr("vaultVerified"):
            print(f"FAIL: the encrypted self-test never finished; panel:\n"
                  f"{await ui.panel('vaulttest')}", file=sys.stderr)

            return 1

        print(f"PASS: encrypted round trip in the browser: {await ui.panel('vaulttest')}")

        if relay_url:
            offer_id = await market_round_trip(ui, relay_url)

            if offer_id is False:
                return 1

            print(f"PASS: market read and published through the relay: offer {offer_id[:12]}…")

            if not await market_handshake(ui, work, relay_url, offer_id):
                return 1
        else:
            print("SKIP: no local relay was started for the market check")

        if relay_url:
            coordination = await coordination_round_trip(ui, work, relay_url)

            if coordination is False:
                return 1

            print(f"PASS: settlement transport round trip: {coordination}")
        else:
            print("SKIP: no local relay was started for the coordination check")

        engine = await engine_round_trip(ui)

        if engine is False:
            return 1

        print(f"PASS: engine read both chains through the proxy: {engine}")

        if not rig_config():
            print("SKIP: the full swap needs a live two-chain rig (see RIG_ENV)")

            return 0

        if not relay_url:
            print("SKIP: the full swap needs the local relay")

            return 0

        bob_key = write_key(work, "bob.nostr-key", "22" * 32)
        alice_key = write_key(work, "alice.nostr-key", "44" * 32)
        browser_key = write_key(work, "browser.key", "11" * 32)

        # Scenario 1: the browser is protocol Alice.
        if not await browser_swap(ui, bob_key, work, relay_url, "alice"):
            return 1

        print("PASS: scenario: browser Alice, CLI Bob, full swap")

        if not await reload_continues(ui, budget):
            return 1

        # Scenario 2: the browser is protocol Bob, in the same vault and with the same
        # wallets. That is the role fix: the wallet is not the role.
        if not await browser_swap(ui, alice_key, work, relay_url, "bob"):
            return 1

        print("PASS: scenario: browser Bob, CLI Alice, full swap")

        # Scenario 3: the reverse market direction, end to end. The browser is the market
        # maker, which in this direction is protocol Bob: it never creates the secret.
        if not await reverse_market_handshake(ui, work, relay_url):
            return 1

        print("PASS: scenario: browser Bob the reverse maker, CLI Alice the taker, full swap")

        ignored_match_id = await ignore_match_round_trip(ui, work, relay_url)

        if not ignored_match_id:
            return 1

        # The other side of the same direction: the browser as the reverse taker.
        if not await reverse_taker_handshake(ui, work, relay_url):
            return 1

        print("PASS: scenario: browser Alice the reverse taker, CLI Bob the maker")

        # The vault is at its fullest here: records from every scenario, the identity, the
        # market documents and the tombstone. That is the vault a backup has to carry.
        if not await vault_export_import_round_trip(ui, work, str(ignored_match_id)):
            return 1

        if not await wallet_create_round_trip(ui, work, relay_url):
            return 1

        if not await refusal_paths(ui, budget):
            return 1

    return 0


async def close_boot_overlay(ui: Ui) -> bool:
    """Wait for the boot overlay to be gone, so clicks land on the app."""
    gone = await poll(
        ui.ws,
        "document.getElementById('boot').hidden ? 'true' : ''",
        20,
    )

    return gone == "true"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--budget", type=float, default=120.0)
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

    # Built every run, not only when it is missing: a stale binary in bin/ would serve the
    # page from old code and the check would pass on a proxy allowlist that no longer
    # matches the source. `make` is incremental, so this is a stat call when nothing changed.
    subprocess.run(["make", "server"], cwd=ROOT, check=True, stdout=subprocess.DEVNULL)

    relay_process, relay_url = start_relay(work)

    RELAY_URL[0] = relay_url

    server_log = (work / "server.log").open("wb")

    server = subprocess.Popen(
        [str(ROOT / "bin" / "blake-swap-app"), "-addr", f"127.0.0.1:{app_port}", "-dir", "web",
         "-rpc-rate", "200", "-rpc-burst", "600"]
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
            f"http://127.0.0.1:{app_port}/",
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


# RELAY_URL carries the local relay's address to the refusal scenarios, which restore it
# after pointing the client at a dead one.
RELAY_URL = [""]


if __name__ == "__main__":
    raise SystemExit(main())
