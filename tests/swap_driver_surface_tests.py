#!/usr/bin/env python3
"""
Operator and driver surface.

The protocol suites drive the *protocol* commands in a fixed sequence,
and the market suite drives the marketplace path. Neither of them
exercises the surface an operator or a scheduler actually uses around
those commands:

    next          the single safe coordination step
    dashboard     the reconciled read-only view
    reconcile     the per-swap forensic view
    show / list   the raw local records
    ignore-match  a local tombstone for an unwanted match
    dismiss       archive an abandoned swap
    resume        drive one swap's safe post-funding work
    resume-all    drive every independent swap
    run           the conservative autonomous driver

This suite drives a real marketplace negotiation through that surface,
step by step, and asserts the documented observable outcome of each
call: which banner it prints, whether it moved any state, and whether it
moved any funds.

It is deliberately an assertion suite, not a smoke test: every check
names a property the operator depends on, such as "`next` refused to
choose between two negotiations" or "`dismiss` refused a live swap".

Needs the two-chain regtest rig for settlement and the in-process relay
stub for coordination.
"""

from __future__ import annotations

import contextlib
import io
import json
import re
import sys
import tempfile
from pathlib import Path


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent

sys.path.insert(0, str(ROOT / "src"))
sys.path.insert(0, str(HERE))

from nostr_relay_stub import NostrRelayStub  # noqa: E402

import swap_protocol_tests as h  # noqa: E402

PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS  {name}")


def fail(name, detail):
    global FAIL
    FAIL += 1
    print(f"FAIL  {name}")
    for line in str(detail).splitlines():
        print(f"      {line}")


def check(name, fn):
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
    else:
        ok(name)


# ---------------------------------------------------------------- setup

STUB = NostrRelayStub()
RELAY_URL = STUB.start()

BASE = Path(tempfile.mkdtemp(prefix="blake-driver-surface-"))

PROFILES = {
    "alice": {
        "state": BASE / "alice",
        "key": BASE / "alice" / "participant.key",
        "wallet": h.ALICE_WALLET,
        "secret": "31" * 32,
    },
    "bob": {
        "state": BASE / "bob",
        "key": BASE / "bob" / "participant.key",
        "wallet": h.BOB_WALLET,
        "secret": "32" * 32,
    },
}

for profile in PROFILES.values():
    profile["state"].mkdir(parents=True, exist_ok=True)
    profile["key"].write_text(profile["secret"] + "\n")
    profile["key"].chmod(0o600)


def env_for(role):
    profile = PROFILES[role]

    return {
        "BLAKE_SWAP_TRANSPORT": "nostr",
        "BLAKE_SWAP_NOSTR_RELAYS": RELAY_URL,
        "BLAKE_SWAP_ROLE": role,
        "BLAKE_SWAP_STATE_DIR": str(profile["state"]),
        "BLAKE_SWAP_MESSAGE_DIR": str(profile["state"] / "messages"),
        "BLAKE_SWAP_CHAIN_WALLET": profile["wallet"],
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE": str(profile["key"]),
        # `next` windows are wall-clock based; the test must not inherit
        # an operator's override.
        "BLAKE_SWAP_NEXT_FRESH_SECONDS": "3600",
    }


def run(role, *args, expect_ok=True):
    return h.run(
        [sys.executable, h.SWAP_V2, *map(str, args)],
        env=env_for(role),
        expect_ok=expect_ok,
    )


def quiet(role, *args):
    with contextlib.redirect_stdout(io.StringIO()):
        return run(role, *args)


BANNERS = (
    "NEXT SAFE STEP",
    "MULTIPLE SAFE COORDINATION STEPS",
    "SETTLEMENT WORK AVAILABLE",
    "MULTIPLE ACTIVE SWAPS",
    "READY TO FUND",
    "MULTIPLE FUNDING-READY SWAPS",
    "NOTHING TO DO",
)


def banner_of(text):
    """The first recognised `next` banner in the output, or None."""

    for banner in BANNERS:
        if banner in text:
            return banner

    return None


def next_step(role, expected=None):
    """
    Run `next` and return (banner, output).

    When ``expected`` is given, a different banner is a hard failure with
    the whole output attached, so a behaviour change is diagnosable from
    the report alone.
    """

    result = run(role, "next")
    text = result.stdout + result.stderr
    banner = banner_of(text)

    print(f"      {role}: next -> {banner}")

    if banner is None:
        raise AssertionError(
            f"`next` printed no recognised banner for {role}:\n{text}"
        )

    if expected is not None and banner != expected:
        raise AssertionError(
            f"`next` for {role} printed {banner!r}, expected {expected!r}:\n"
            f"{text}"
        )

    return banner, text


def load_state(role, swap_id):
    path = PROFILES[role]["state"] / f"{swap_id}.json"

    if not path.exists():
        raise AssertionError(f"no {role} state for {swap_id}")

    return json.loads(path.read_text())


def local_swaps(role):
    """Twelve-character swap ids with local state for a participant."""

    return {
        path.stem
        for path in PROFILES[role]["state"].glob("*.json")
        if len(path.stem) == 12
    }


def stub_events(tag):
    return [
        event
        for event in STUB.events.values()
        if any(
            len(t) >= 2 and t[0] == "t" and t[1] == tag
            for t in event.get("tags", [])
        )
    ]


def newest(tag):
    events = stub_events(tag)

    if not events:
        raise AssertionError(f"no {tag} event was published")

    return sorted(
        events,
        key=lambda e: (int(e.get("created_at", 0)), e.get("id", "")),
    )[-1]


def settle(role, *args):
    quiet(role, *args)


# --------------------------------------------------------- the scenarios

SWAP_ID = None
MATCH_ID = None
OFFER_ID = None
TAKE_ID = None

# Filled in by the two-negotiation scenario.
PENDING_OFFERS = []
NEW_MATCHES = []
FIRST_NEW_SWAP = None
SECOND_NEW_SWAP = None
FRESH_SWAP = None


def negotiate_via_next():
    """
    Reach READY TO FUND using only `next`, one safe step at a time.

    `next` is the command an autonomous scheduler calls in a loop, so the
    property being checked is that the loop converges: each call either
    performs exactly one coordination step or reports that it will not
    choose.
    """

    global MATCH_ID, OFFER_ID, SWAP_ID, TAKE_ID

    quiet(
        "alice",
        "publish-market-offer",
        "--blake", "0.25",
        "--sats", "40000",
        "--minutes", "60",
        "--direction", "blake-for-btc",
    )

    OFFER_ID = newest("blake-swap-market")["id"]

    # While the offer is still open, the human marketplace view must
    # list it. (After the take it becomes MATCHED, which is a different
    # view.)
    browsed = quiet("alice", "market", "--limit", "50").stdout

    if OFFER_ID[:12] not in browsed and OFFER_ID not in browsed:
        raise AssertionError(
            f"`market` did not list the open offer:\n{browsed}"
        )

    # Bob's `next` finds exactly one safe step -- taking the offer -- and
    # performs it.
    banner, text = next_step("bob", "NEXT SAFE STEP")

    if "Take open offer" not in text:
        raise AssertionError(f"Bob's next step was not the take:\n{text}")

    TAKE_ID = newest("blake-swap-market-take")["id"]

    # The take already exists, so a second pass must not publish another.
    before = len(stub_events("blake-swap-market-take"))
    next_step("bob", "NOTHING TO DO")

    if len(stub_events("blake-swap-market-take")) != before:
        raise AssertionError("`next` published a duplicate take")

    # Alice's `next` finds the one valid take and accepts it, which
    # creates her swap and publishes the match.
    banner, text = next_step("alice", "NEXT SAFE STEP")

    if "Accept take" not in text:
        raise AssertionError(f"Alice's next step was not the take:\n{text}")

    match_event = newest("blake-swap-market-match")
    MATCH_ID = match_event["id"]
    SWAP_ID = json.loads(match_event["content"])["swap_id"]

    if SWAP_ID not in local_swaps("alice"):
        raise AssertionError("accepting the take created no local swap")

    # Alice now waits for Bob: the negotiation is bound but he has not
    # published his acceptance, so there is nothing safe to do.
    next_step("alice", "NOTHING TO DO")

    # Bob's `next` imports the fresh match and publishes his acceptance.
    banner, text = next_step("bob", "NEXT SAFE STEP")

    if "Import fresh market match" not in text:
        raise AssertionError(f"Bob's next step was not the match:\n{text}")

    if load_state("bob", SWAP_ID)["state"] != "OFFERED":
        raise AssertionError("Bob did not import the swap as OFFERED")

    # Alice imports Bob's acceptance.
    banner, text = next_step("alice", "NEXT SAFE STEP")

    if "Import Bob acceptance" not in text:
        raise AssertionError(f"Alice's next step was not the accept:\n{text}")

    alice = load_state("alice", SWAP_ID)

    if not (alice.get("blake_bob_pub") and alice.get("btc_bob_pub")):
        raise AssertionError("Alice did not record Bob's settlement keys")

    # Finally `next` reports that funding is the next required action,
    # and refuses to perform it.
    banner, text = next_step("alice", "READY TO FUND")

    if "STOP: funding is an explicit economic commitment." not in text:
        raise AssertionError(
            f"READY TO FUND did not stop at the economic boundary:\n{text}"
        )


def test_sync_funding_finds_nothing_for_an_unfunded_swap():
    """
    `sync-funding` is a recovery command, not a trigger. On an unfunded
    swap it must report that it found nothing and change nothing.
    """

    result = quiet("alice", "sync-funding", SWAP_ID, "--role", "alice")
    text = result.stdout + result.stderr

    if "FUNDING CHAIN RECONCILIATION" not in text:
        raise AssertionError(f"`sync-funding` printed no banner:\n{text}")

    if "start:   OFFERED" not in text:
        raise AssertionError(f"`sync-funding` misreported the state:\n{text}")

    if "No confirmed BLAKE funding output found." not in text:
        raise AssertionError(
            f"`sync-funding` did not report finding nothing:\n{text}"
        )


def test_sync_settlement_changes_nothing_before_any_settlement():
    result = quiet("alice", "sync-settlement", SWAP_ID, "--role", "alice")
    text = result.stdout + result.stderr

    if "SETTLEMENT CHAIN RECONCILIATION" not in text:
        raise AssertionError(
            f"`sync-settlement` printed no banner:\n{text}"
        )

    if "start:   OFFERED" not in text:
        raise AssertionError(
            f"`sync-settlement` misreported the state:\n{text}"
        )

    if load_state("alice", SWAP_ID)["state"] != "OFFERED":
        raise AssertionError("`sync-settlement` changed an unfunded swap")


def test_resume_refuses_an_empty_queue():
    """
    With no local swaps at all, `resume` must say so rather than
    inventing work. This runs before the negotiation below, so the
    participant state is genuinely empty.
    """

    result = quiet("alice", "resume")
    text = result.stdout + result.stderr

    if "NO RESUMABLE SWAPS" not in text:
        raise AssertionError(
            f"`resume` did not report an empty queue:\n{text}"
        )

    if local_swaps("alice"):
        raise AssertionError("`resume` created local state")


def test_resume_rejects_a_short_prefix():
    result = run("alice", "resume", "abc", expect_ok=False)

    if result.returncode == 0:
        raise AssertionError("`resume` accepted a three-character prefix")

    combined = result.stdout + result.stderr

    if "too short" not in combined:
        raise AssertionError(
            f"`resume` refused without saying why:\n{combined}"
        )


def test_next_converges():
    negotiate_via_next()


def test_next_never_funded():
    alice = load_state("alice", SWAP_ID)

    if alice.get("blake_funding_txid"):
        raise AssertionError("`next` funded the BLAKE HTLC by itself")

    next_step("alice", "READY TO FUND")
    next_step("bob", "NOTHING TO DO")

    if load_state("alice", SWAP_ID).get("blake_funding_txid"):
        raise AssertionError("a second `next` funded the BLAKE HTLC")


def test_market_views_render_for_a_human():
    """
    The marketplace has machine-readable (`--json`) and human views for
    the same state. Both must work on a live market, not just the JSON
    one that the market suites assert against.
    """

    state = quiet("alice", "market-state", "--limit", "50").stdout

    if "blake-swap-market" not in state and OFFER_ID[:12] not in state:
        raise AssertionError(
            f"`market-state` did not list the open offer:\n{state}"
        )

    #
    # Alice's own offer is MATCHED now, so the *open offers* view must
    # correctly report nothing rather than listing a consumed offer.
    #
    browsed = quiet("alice", "market", "--limit", "50").stdout

    if "AVAILABLE BLAKE/BTC OFFERS" not in browsed:
        raise AssertionError(
            f"`market` did not render its offers view:\n{browsed}"
        )

    if "No valid open offers found." not in browsed:
        raise AssertionError(
            f"`market` listed a matched offer as open:\n{browsed}"
        )

    takes = quiet("alice", "market-takes", OFFER_ID).stdout

    if TAKE_ID[:12] not in takes and TAKE_ID not in takes:
        raise AssertionError(f"`market-takes` did not list the take:\n{takes}")

    #
    # Matches are addressed to the taker, so Bob is the participant who
    # sees them; Alice, as the maker, must see none.
    #
    matches = quiet("bob", "market-matches").stdout

    if not matches.startswith("MARKET MATCHES FOR") and "MARKET MATCHES FOR" not in matches:
        raise AssertionError(
            f"`market-matches` did not render its view:\n{matches}"
        )

    if MATCH_ID[:12] not in matches and MATCH_ID not in matches:
        raise AssertionError(
            f"`market-matches` did not list the taker's match:\n{matches}"
        )

    #
    # The labels must follow the direction. This flow is blake-for-btc:
    # the market maker is protocol Alice and the taker is protocol Bob.
    #
    for expected in (
        "direction: blake-for-btc",
        "maker is protocol: alice",
        "taker is protocol: bob",
    ):
        if expected not in matches:
            raise AssertionError(
                f"`market-matches` omitted {expected!r}:\n{matches}"
            )

    if "FOR BOB" in matches or "Alice maker identity" in matches:
        raise AssertionError(
            f"`market-matches` still hardcodes the forward labels:\n{matches}"
        )

    maker_view = quiet("alice", "market-matches").stdout

    if "No valid matches found." not in maker_view:
        raise AssertionError(
            f"the maker was shown matches that are not hers:\n{maker_view}"
        )

    #
    # market-matches is a human view only; the machine-readable market
    # surfaces are `market-state --json` and `market-takes --json`.
    #
    limited = quiet("bob", "market-matches", "--limit", "50").stdout

    if MATCH_ID[:12] not in limited and MATCH_ID not in limited:
        raise AssertionError(
            f"`market-matches --limit` dropped the match:\n{limited}"
        )


def test_complete_refuses_a_swap_that_is_not_claimable():
    """
    `complete` closes out a settled swap. Running it earlier must fail
    closed: finalising an unfunded swap would erase the record of a
    trade that never happened.
    """

    result = run("alice", "complete", SWAP_ID, "--role", "alice", expect_ok=False)

    if result.returncode == 0:
        raise AssertionError(
            "`complete` finalised a swap that was never funded:\n"
            + result.stdout
        )

    if "SWAP COMPLETE" in result.stdout:
        raise AssertionError("`complete` reported success on an OFFERED swap")

    if load_state("alice", SWAP_ID)["state"] != "OFFERED":
        raise AssertionError("`complete` mutated an unfunded swap")


def test_dashboard_reports_the_live_swap():
    result = quiet("alice", "dashboard")
    text = result.stdout

    for expected in (
        "BLAKE SWAP DASHBOARD",
        "ACTIVE",
        "NEEDS ATTENTION",
        "SETTLED",
        "ARCHIVED",
        "SUMMARY",
    ):
        if expected not in text:
            raise AssertionError(f"dashboard omitted {expected!r}:\n{text}")

    if SWAP_ID not in text:
        raise AssertionError(f"dashboard omitted the live swap:\n{text}")


def test_dashboard_json_matches_human_view():
    result = quiet("alice", "dashboard", "--json")

    text = result.stdout.strip()
    payload = json.loads(text[text.index("{"):])

    lists = ("active", "attention", "settled", "archived")

    missing = [key for key in lists if key not in payload]

    if missing:
        raise AssertionError(
            f"dashboard --json is missing {missing}: {sorted(payload)}"
        )

    ids = {
        item.get("swap_id")
        for key in lists
        for item in payload[key]
    }

    if SWAP_ID not in ids:
        raise AssertionError(
            f"dashboard --json omitted the live swap: {sorted(ids)}"
        )

    if payload.get("version") != 1:
        raise AssertionError(f"unexpected dashboard schema: {payload.get('version')}")


def test_reconcile_is_read_only_on_an_unfunded_swap():
    result = quiet("alice", "reconcile", SWAP_ID, "--json")

    text = result.stdout.strip()
    payload = json.loads(text[text.index("{"):])

    if payload.get("read_only") is not True:
        raise AssertionError(f"reconcile was not read-only: {payload}")

    if payload.get("checkpoint_state") != "OFFERED":
        raise AssertionError(f"unexpected checkpoint state: {payload}")

    if load_state("alice", SWAP_ID)["state"] != "OFFERED":
        raise AssertionError("reconcile mutated the local record")


def test_reconcile_human_report_is_read_only():
    result = quiet("alice", "reconcile", SWAP_ID)
    text = result.stdout + result.stderr

    for expected in (
        "BLAKE SWAP RECONCILIATION",
        "CHAIN REALITY",
        "SECRET",
        "TIMEOUT DISTANCE",
        "NEXT SAFE ACTION",
        "PASS: reconciliation was read-only",
    ):
        if expected not in text:
            raise AssertionError(f"`reconcile` omitted {expected!r}:\n{text}")

    if load_state("alice", SWAP_ID)["state"] != "OFFERED":
        raise AssertionError("the human reconcile mutated the record")


def test_show_and_list_expose_the_record():
    shown = quiet("alice", "show", SWAP_ID).stdout

    if SWAP_ID not in shown:
        raise AssertionError(f"`show` did not name the swap:\n{shown}")

    listed = quiet("alice", "list").stdout

    if SWAP_ID not in listed:
        raise AssertionError(f"`list` did not include the swap:\n{listed}")


def test_run_refuses_economic_actions_before_funding():
    result = quiet("alice", "run", SWAP_ID, "--max-steps", "1")
    text = result.stdout + result.stderr

    if "BLAKE SWAP AUTONOMOUS DRIVER" not in text:
        raise AssertionError(f"`run` printed no driver banner:\n{text}")

    if "RUN RESULT" not in text and "ACTION REQUIRED" not in text:
        raise AssertionError(f"`run` printed no result:\n{text}")

    if load_state("alice", SWAP_ID).get("blake_funding_txid"):
        raise AssertionError("`run` funded the BLAKE HTLC")


def test_resume_all_refuses_initial_funding():
    result = quiet("alice", "resume-all")
    text = result.stdout + result.stderr

    if "PASS: initial BLAKE funding was not performed" not in text:
        raise AssertionError(
            "`resume-all` did not state that it refused initial funding:\n"
            f"{text}"
        )

    if load_state("alice", SWAP_ID).get("blake_funding_txid"):
        raise AssertionError("`resume-all` funded the BLAKE HTLC")


def test_ignore_match_tombstones_without_creating_state():
    """
    A second offer, taken by Bob, gives Alice a match she does not want.

    Ignoring it must create a purely local tombstone: no swap state, no
    Nostr deletion, and `next` must stop surfacing it.
    """

    quiet(
        "alice",
        "publish-market-offer",
        "--blake", "0.3",
        "--sats", "50000",
        "--minutes", "60",
        "--direction", "blake-for-btc",
    )

    offer_id = newest("blake-swap-market")["id"]

    quiet("bob", "take-market-offer", offer_id)

    take_id = newest("blake-swap-market-take")["id"]

    quiet("alice", "accept-market-take", take_id)

    #
    # The match is tagged for the taker, so Bob is the one who can
    # decline it -- and Bob has no local swap for it yet, which is the
    # only state in which ignoring is meaningful.
    #
    match_id = newest("blake-swap-market-match")["id"]
    unwanted_swap = json.loads(
        newest("blake-swap-market-match")["content"]
    )["swap_id"]

    result = quiet("bob", "ignore-match", match_id)

    if "local tombstone created" not in result.stdout:
        raise AssertionError(
            f"`ignore-match` did not report a tombstone:\n{result.stdout}"
        )

    tombstone = (
        PROFILES["bob"]["state"]
        / "ignored-market-matches"
        / f"{match_id}.json"
    )

    if not tombstone.exists():
        raise AssertionError(f"no tombstone was written: {tombstone}")

    if unwanted_swap in local_swaps("bob"):
        raise AssertionError("`ignore-match` created swap state")

    record = json.loads(tombstone.read_text())

    for field in (
        "match_id",
        "swap_id",
        "offer_id",
        "take_id",
        "maker",
        "taker",
        "direction",
        "blake_amount",
        "btc_sats",
        "ignored_at",
        "local_only",
    ):
        if field not in record:
            raise AssertionError(f"tombstone is missing {field}: {record}")

    if record["match_id"] != match_id:
        raise AssertionError("tombstone names a different match")

    if any(
        event.get("id") == match_id
        for event in STUB.events.values()
    ) is False:
        raise AssertionError("`ignore-match` deleted the relay event")

    #
    # The ignored match must not come back through `next`. Bob's only
    # live swap is already mid-negotiation, so the ignored match can
    # never appear as a fresh import.
    #
    banner, text = next_step("bob")

    if "Import fresh market match" in text:
        raise AssertionError(f"`next` re-surfaced the ignored match:\n{text}")

    if banner not in {"NOTHING TO DO", "SETTLEMENT WORK AVAILABLE"}:
        raise AssertionError(
            f"the ignored match changed Bob's next step to {banner!r}:\n{text}"
        )


def test_dismiss_archives_a_negotiation_that_never_became_fundable():
    """
    The marketplace negotiation opened by the ignore-match scenario left
    Alice with a swap that never became settlement-capable: the taker
    declined, so no settlement keys exist and no HTLC was ever funded.

    `dismiss` must archive exactly that shape -- and nothing else.
    """

    remaining = local_swaps("alice") - {SWAP_ID}

    if len(remaining) != 1:
        raise AssertionError(
            f"expected one leftover market swap, found {sorted(remaining)}"
        )

    abandoned = remaining.pop()

    result = quiet("alice", "dismiss", abandoned)
    text = result.stdout

    if "SAFE TO ARCHIVE" not in text or "SWAP ARCHIVED" not in text:
        raise AssertionError(f"`dismiss` did not archive:\n{text}")

    if "negotiation never became settlement-capable" not in text:
        raise AssertionError(
            f"`dismiss` did not name the reason it was safe:\n{text}"
        )

    if abandoned in local_swaps("alice"):
        raise AssertionError("the archived swap is still in the active set")

    if not (PROFILES["alice"]["state"] / "archive" / f"{abandoned}.json").exists():
        raise AssertionError("no archived copy was preserved")

    if SWAP_ID not in local_swaps("alice"):
        raise AssertionError("`dismiss` removed an unrelated swap")


def test_dismiss_refuses_a_funded_swap():
    """
    Once BLAKE is on chain, dismissing is no longer a local decision:
    the refusal is the safety property, not the archive.
    """

    result = run("alice", "dismiss", SWAP_ID, expect_ok=False)

    if result.returncode == 0:
        raise AssertionError(
            "`dismiss` archived a swap with a funded HTLC:\n" + result.stdout
        )

    combined = result.stdout + result.stderr

    if "Refusing to dismiss" not in combined:
        raise AssertionError(
            f"`dismiss` refused without saying why:\n{combined}"
        )

    if SWAP_ID not in local_swaps("alice"):
        raise AssertionError("the refused dismiss still removed the swap")


def test_fund_commits_the_single_ready_swap():
    """
    `next` refuses to fund; `fund` is the explicit economic commitment.

    This is the one place in this suite where funds move on purpose, and
    it exercises the documented path: import the authenticated
    acceptance, verify Bob is fully pinned, then fund.
    """

    result = quiet("alice", "fund")
    text = result.stdout + result.stderr

    for expected in (
        "FUNDING-READY SWAP",
        "EXPLICIT COMMITMENT",
        "This moves funds.",
    ):
        if expected not in text:
            raise AssertionError(f"`fund` omitted {expected!r}:\n{text}")

    alice = load_state("alice", SWAP_ID)

    if not alice.get("blake_funding_txid"):
        raise AssertionError(f"`fund` broadcast no funding:\n{text}")

    if alice["state"] != "BLAKE_FUNDED":
        raise AssertionError(f"`fund` left Alice in {alice['state']}")

    if not (alice.get("blake_bob_pub") and alice.get("btc_bob_pub")):
        raise AssertionError("`fund` did not pin Bob's settlement keys")


def test_settlement_then_next_reports_settlement_work():
    settle("alice", "publish-blake-funded", SWAP_ID)

    #
    # BLAKE is now funded on chain, so `next` must stop offering
    # coordination and route the operator to the settlement policy.
    #
    banner, text = next_step("alice", "SETTLEMENT WORK AVAILABLE")

    if "did not execute settlement economics" not in text:
        raise AssertionError(
            f"`next` did not disclaim economic execution:\n{text}"
        )

    if load_state("alice", SWAP_ID)["state"] != "BLAKE_FUNDED":
        raise AssertionError("funding did not advance Alice's checkpoint")


def test_sync_funding_discovers_the_funding_from_the_chain():
    """
    Bob is told nothing: `sync-funding` has to find Alice's confirmed
    BLAKE HTLC in his own wallet scan.
    """

    h.mine_blake(1)

    result = quiet("bob", "sync-funding", SWAP_ID, "--role", "bob")
    text = result.stdout + result.stderr

    bob = load_state("bob", SWAP_ID)
    alice = load_state("alice", SWAP_ID)

    if not bob.get("blake_funding_txid"):
        raise AssertionError(
            f"`sync-funding` recorded no funding from the chain:\n{text}"
        )

    if bob["blake_funding_txid"] != alice["blake_funding_txid"]:
        raise AssertionError(
            "`sync-funding` recorded a different funding transaction"
        )

    if bob["state"] != "BLAKE_FUNDED":
        raise AssertionError(
            f"`sync-funding` left Bob in {bob['state']} instead of BLAKE_FUNDED"
        )


def test_sync_settlement_finds_no_spend_while_only_blake_is_funded():
    """
    With only BLAKE funded, the only settlement that could exist is a
    BLAKE refund. There is none, so the command must say exactly that
    and leave Bob's checkpoint alone.
    """

    result = quiet("bob", "sync-settlement", SWAP_ID, "--role", "bob")
    text = result.stdout + result.stderr

    if "No confirmed BLAKE HTLC spend requires reconciliation." not in text:
        raise AssertionError(
            f"`sync-settlement` did not report the missing spend:\n{text}"
        )

    if load_state("bob", SWAP_ID)["state"] != "BLAKE_FUNDED":
        raise AssertionError("`sync-settlement` moved Bob's checkpoint")


def test_funding_is_discoverable_from_the_chain():
    """
    Bob funds Bitcoin and announces it, but Alice is told nothing: she
    must find the confirmed HTLC in her own chain view. This is the
    restart path, and the counterpart of the `sync-funding` check above.
    """

    settle("bob", "fund-btc", SWAP_ID)
    settle("bob", "publish-btc-funded", SWAP_ID)

    h.mine_btc(2)

    result = quiet("alice", "sync-funding", SWAP_ID, "--role", "alice")
    text = result.stdout + result.stderr

    alice = load_state("alice", SWAP_ID)
    bob = load_state("bob", SWAP_ID)

    if not alice.get("btc_funding_txid"):
        raise AssertionError(
            f"`sync-funding` found no Bitcoin funding:\n{text}"
        )

    if alice["btc_funding_txid"] != bob["btc_funding_txid"]:
        raise AssertionError(
            "`sync-funding` recorded a different funding transaction"
        )

    if alice["state"] != "BTC_FUNDED":
        raise AssertionError(
            f"`sync-funding` left Alice in {alice['state']}"
        )


def test_the_claim_is_discoverable_from_the_chain():
    """
    Alice claims Bitcoin, and Bob has to learn the secret the only way
    the protocol allows: from the confirmed claim's witness, not from a
    message. `sync-settlement` is the command that does it.
    """

    settle("alice", "claim-btc", SWAP_ID)
    settle("alice", "publish-btc-claimed", SWAP_ID)

    h.mine_btc(1)

    result = quiet("bob", "sync-settlement", SWAP_ID, "--role", "bob")
    text = result.stdout + result.stderr

    bob = load_state("bob", SWAP_ID)
    alice = load_state("alice", SWAP_ID)

    if bob["state"] != "BTC_CLAIMED":
        raise AssertionError(
            f"`sync-settlement` left Bob in {bob['state']}:\n{text}"
        )

    recovered = bob.get("revealed_secret_hex")

    if not recovered:
        raise AssertionError(f"Bob did not recover the secret:\n{text}")

    if recovered != alice.get("secret_hex"):
        raise AssertionError(
            "Bob recovered a secret that is not Alice's"
        )

    #
    # Bob must never hold Alice's own secret field; only the revealed one.
    #
    if bob.get("secret_hex"):
        raise AssertionError("Bob holds Alice's secret_hex")


def test_sync_settlement_finds_no_spend_after_the_bitcoin_claim():
    """
    After Bob has learned the secret, the next authoritative fact is his
    own BLAKE claim. Asking again before he has claimed must report that
    it found nothing rather than doing anything.
    """

    result = quiet("bob", "sync-settlement", SWAP_ID, "--role", "bob")
    text = result.stdout + result.stderr

    if "start:   BTC_CLAIMED" not in text:
        raise AssertionError(f"`sync-settlement` misread the state:\n{text}")

    if "No confirmed BLAKE HTLC spend requires reconciliation." not in text:
        raise AssertionError(
            f"`sync-settlement` did not report the missing spend:\n{text}"
        )

    if load_state("bob", SWAP_ID)["state"] != "BTC_CLAIMED":
        raise AssertionError("`sync-settlement` moved Bob's checkpoint")


def test_resume_completes_the_settlement_path():
    #
    # `resume` is the documented way to drive settlement. From
    # BLAKE_FUNDED it must fund Bitcoin, claim, and finish.
    #
    #
    # `resume` performs one safe action per cycle, exactly like `next`.
    # An operator (or a scheduler) repeats it until the swap stops
    # changing; the property to check is that repeating it converges on
    # COMPLETE rather than livelocking.
    #
    for _ in range(12):
        if all(
            load_state(role, SWAP_ID)["state"] == "COMPLETE"
            for role in ("alice", "bob")
        ):
            break

        for role in ("alice", "bob"):
            if load_state(role, SWAP_ID)["state"] != "COMPLETE":
                quiet(role, "resume", SWAP_ID)

    for role in ("alice", "bob"):
        state = load_state(role, SWAP_ID)["state"]

        if state != "COMPLETE":
            raise AssertionError(
                f"{role} did not reach COMPLETE after repeated resume: {state}"
            )


def test_reports_agree_once_settled():
    result = quiet("alice", "dashboard")
    text = result.stdout

    if SWAP_ID not in text:
        raise AssertionError(f"settled swap missing from dashboard:\n{text}")

    if "No settled swaps." in text:
        raise AssertionError(
            f"dashboard still reports no settled swaps:\n{text}"
        )

    result = quiet("alice", "reconcile", SWAP_ID, "--json")
    payload = json.loads(result.stdout[result.stdout.index("{"):])

    if payload.get("checkpoint_state") != "COMPLETE":
        raise AssertionError(f"reconcile disagrees with the dashboard: {payload}")


def test_next_is_quiet_once_everything_is_settled():
    next_step("alice", "NOTHING TO DO")


TERMINAL_FIELDS = (
    "state",
    "blake_funding_txid",
    "btc_funding_txid",
    "btc_claim_txid",
    "blake_claim_txid",
    "blake_refund_txid",
    "btc_refund_txid",
)


def test_rebroadcast_republishes_a_cached_event_verbatim():
    """
    `rebroadcast-nostr-event` exists for relays that dropped an event.
    It must reuse the cached, verified event rather than rebuild or
    re-sign anything, and it must publish exactly that event.
    """

    before = STUB.published

    result = quiet("bob", "rebroadcast-nostr-event", OFFER_ID)
    text = result.stdout + result.stderr

    for expected in (
        "REBROADCASTING CACHED NOSTR EVENT",
        "PASS: cached event re-verified",
        "PASS: existing signature will be reused",
        "PASS: event will NOT be rebuilt or re-signed",
    ):
        if expected not in text:
            raise AssertionError(f"`rebroadcast` omitted {expected!r}:\n{text}")

    if STUB.published <= before:
        raise AssertionError("`rebroadcast` published nothing")

    if OFFER_ID not in STUB.events:
        raise AssertionError("the republished offer is not in the relay store")


def test_rebroadcast_refuses_an_uncached_event():
    result = run(
        "alice",
        "rebroadcast-nostr-event",
        "ab" * 32,
        expect_ok=False,
    )

    if result.returncode == 0:
        raise AssertionError(
            "`rebroadcast` accepted an event it has never verified:\n"
            + result.stdout
        )

    combined = result.stdout + result.stderr

    if "No verified cached Nostr event" not in combined:
        raise AssertionError(
            f"`rebroadcast` refused without saying why:\n{combined}"
        )


def test_sync_settlement_is_terminal_aware():
    """
    COMPLETE is terminal: there is no settlement left to look for, so the
    command must recognise the state and stop rather than scanning a
    chain for a spend that cannot exist.
    """

    result = quiet("bob", "sync-settlement", SWAP_ID, "--role", "bob")
    text = result.stdout + result.stderr

    if "start:   COMPLETE" not in text:
        raise AssertionError(f"`sync-settlement` misread the state:\n{text}")

    if load_state("bob", SWAP_ID)["state"] != "COMPLETE":
        raise AssertionError("`sync-settlement` disturbed a terminal swap")


def test_sync_settlement_leaves_a_settled_swap_alone():
    before = load_state("alice", SWAP_ID)

    # A settled swap has nothing left to sync, so the command may
    # legitimately refuse; what it may never do is change the record.
    run("alice", "sync-settlement", SWAP_ID, expect_ok=False)

    after = load_state("alice", SWAP_ID)

    changed = [
        field
        for field in TERMINAL_FIELDS
        if after.get(field) != before.get(field)
    ]

    if changed:
        raise AssertionError(
            f"`sync-settlement` changed a settled swap: {changed}"
        )


def test_next_refuses_to_choose_between_two_negotiations():
    """
    Two takers on two offers is exactly the ambiguity `next` exists to
    refuse: it must list both and perform neither.
    """

    global PENDING_OFFERS

    before = {event["id"] for event in stub_events("blake-swap-market")}

    for index, minutes in enumerate(("60", "61"), start=1):
        quiet(
            "alice",
            "publish-market-offer",
            "--blake", f"0.{40 + index}",
            "--sats", str(60000 + index),
            "--minutes", minutes,
            "--direction", "blake-for-btc",
        )

    PENDING_OFFERS = sorted(
        {event["id"] for event in stub_events("blake-swap-market")} - before
    )

    if len(PENDING_OFFERS) != 2:
        raise AssertionError(
            f"published {len(PENDING_OFFERS)} offers, expected 2"
        )

    for offer_id in PENDING_OFFERS:
        quiet("bob", "take-market-offer", offer_id)

    banner, text = next_step("alice", "MULTIPLE SAFE COORDINATION STEPS")

    if "Refusing to choose automatically" not in text:
        raise AssertionError(f"`next` did not disclaim the choice:\n{text}")

    for offer_id in PENDING_OFFERS:
        if offer_id[:12] not in text:
            raise AssertionError(
                f"`next` did not list offer {offer_id[:12]}:\n{text}"
            )

    if SWAP_ID not in local_swaps("alice"):
        raise AssertionError("`next` disturbed an unrelated swap")


def test_accept_take_accepts_the_only_valid_take():
    global NEW_MATCHES

    before = {event["id"] for event in stub_events("blake-swap-market-match")}

    result = quiet("alice", "accept-take", PENDING_OFFERS[0])
    text = result.stdout + result.stderr

    if "ONE VALID TAKE FOUND" not in text:
        raise AssertionError(
            f"`accept-take` did not announce the single valid take:\n{text}"
        )

    NEW_MATCHES = sorted(
        {event["id"] for event in stub_events("blake-swap-market-match")} - before
    )

    if len(NEW_MATCHES) != 1:
        raise AssertionError(
            f"`accept-take` published {len(NEW_MATCHES)} matches, expected 1"
        )


def test_accept_match_imports_a_fresh_match():
    """
    `accept-match` is the explicit counterpart to `next`: it imports the
    match the operator chose, and only that one.
    """

    global FIRST_NEW_SWAP, FRESH_SWAP

    match_id = NEW_MATCHES[0]
    swap_id = json.loads(STUB.events[match_id]["content"])["swap_id"]

    before = local_swaps("bob")

    result = quiet("bob", "accept-match", match_id)
    text = result.stdout + result.stderr

    created = local_swaps("bob") - before

    if created != {swap_id}:
        raise AssertionError(
            f"`accept-match` created {sorted(created)}, expected {swap_id}:\n{text}"
        )

    if load_state("bob", swap_id)["state"] != "OFFERED":
        raise AssertionError("`accept-match` did not import the swap as OFFERED")

    FIRST_NEW_SWAP = swap_id
    FRESH_SWAP = swap_id


def test_next_refuses_two_different_kinds_of_action():
    """
    After one accept-take, Alice has an acceptance to import *and* a
    second take to accept. Those are different kinds of action, so `next`
    must list both rather than ordering them for her.
    """

    banner, text = next_step("alice", "MULTIPLE SAFE COORDINATION STEPS")

    if "Import Bob acceptance" not in text:
        raise AssertionError(
            f"`next` omitted the pending acceptance:\n{text}"
        )

    if "Accept take" not in text:
        raise AssertionError(f"`next` omitted the pending take:\n{text}")

    if load_state("bob", FRESH_SWAP)["state"] != "OFFERED":
        raise AssertionError("the second negotiation did not import cleanly")


def test_explicit_resolution_reaches_two_funding_ready_swaps():
    """
    The operator resolves the ambiguity explicitly, and then `next` must
    stop guessing and report that two trades are ready for the one
    economic decision the engine never makes for you.
    """

    global SECOND_NEW_SWAP

    quiet("alice", "accept-take", PENDING_OFFERS[1])

    SECOND_NEW_SWAP = json.loads(
        newest("blake-swap-market-match")["content"]
    )["swap_id"]

    # Bob imports the second match, which publishes his acceptance.
    next_step("bob", "NEXT SAFE STEP")

    for swap_id in (FIRST_NEW_SWAP, SECOND_NEW_SWAP):
        quiet("alice", "receive-accept", swap_id)

    banner, text = next_step("alice", "MULTIPLE FUNDING-READY SWAPS")

    if "Refusing to choose" not in text:
        raise AssertionError(
            f"`next` did not refuse to choose a trade to fund:\n{text}"
        )

    for swap_id in (FIRST_NEW_SWAP, SECOND_NEW_SWAP):
        if swap_id not in text:
            raise AssertionError(
                f"`next` did not list funding-ready swap {swap_id}:\n{text}"
            )

        if load_state("alice", swap_id)["state"] != "OFFERED":
            raise AssertionError(f"{swap_id} is not OFFERED for Alice")


def test_resume_reports_manual_attention():
    """
    A swap the engine cannot reason about must be reported, not driven.

    A freshly created swap has no counterparty identities pinned, so
    reconciliation cannot classify it; `resume` must hand it back to the
    operator instead of running the autonomous policy against it.
    """

    before = local_swaps("alice")

    quiet(
        "alice",
        "new",
        "--blake", "0.12",
        "--sats", "21000",
        "--btc-timeout", "300",
        "--blake-timeout", "200",
    )

    created = local_swaps("alice") - before

    if len(created) != 1:
        raise AssertionError(f"`new` created {len(created)} swaps, expected 1")

    unclassified = created.pop()

    result = quiet("alice", "resume", unclassified)
    text = result.stdout + result.stderr

    if "MANUAL ATTENTION REQUIRED" not in text:
        raise AssertionError(
            f"`resume` did not hand the swap back:\n{text}"
        )

    if "Refusing to hand a manual-attention swap" not in text:
        raise AssertionError(
            f"`resume` did not explain the refusal:\n{text}"
        )

    if load_state("alice", unclassified)["state"] != "OFFERED":
        raise AssertionError("`resume` mutated a manual-attention swap")


def test_resume_all_reports_attention_without_blocking_safe_swaps():
    """
    `resume-all` drives every independently safe swap. A swap it cannot
    classify must be reported as attention, and must not stop the pass
    from handling the rest, nor fund anything by itself.
    """

    before = {
        swap_id: load_state("alice", swap_id)["state"]
        for swap_id in local_swaps("alice")
    }

    result = quiet("alice", "resume-all")
    text = result.stdout + result.stderr

    if "MULTI-SWAP SETTLEMENT PASS" not in text:
        raise AssertionError(f"`resume-all` printed no pass banner:\n{text}")

    if "PASS: initial BLAKE funding was not performed" not in text:
        raise AssertionError(
            f"`resume-all` did not refuse initial funding:\n{text}"
        )

    after = {
        swap_id: load_state("alice", swap_id)["state"]
        for swap_id in local_swaps("alice")
    }

    if after != before:
        changed = {
            key: (before.get(key), after.get(key))
            for key in set(before) | set(after)
            if before.get(key) != after.get(key)
        }
        raise AssertionError(f"`resume-all` changed swap state: {changed}")


def test_a_new_swap_archives_an_unannounced_placeholder_loudly():
    """
    Creating a swap supersedes an earlier unannounced placeholder.

    A placeholder (OFFERED, no counterparty, no Alice contract keys, no
    funds) cannot be referenced by anyone else, so the record is moved to
    archive/ rather than left behind to confuse `next`, `fund` and
    `resume` -- and the move is announced, because a swap disappearing
    silently is the failure being fixed.
    """

    quiet(
        "alice",
        "new",
        "--blake", "0.13",
        "--sats", "22000",
        "--btc-timeout", "300",
        "--blake-timeout", "200",
    )

    first = local_swaps("alice")

    # A second placeholder supersedes the first.
    result = quiet(
        "alice",
        "new",
        "--blake", "0.14",
        "--sats", "23000",
        "--btc-timeout", "300",
        "--blake-timeout", "200",
    )

    text = result.stdout + result.stderr

    if "SUPERSEDED PLACEHOLDER ARCHIVED" not in text:
        raise AssertionError(
            f"the superseded placeholder was archived silently:\n{text}"
        )

    if "PASS: it was preserved under archive/, not deleted" not in text:
        raise AssertionError(
            f"the archive was not explained:\n{text}"
        )

    after = local_swaps("alice") - first

    if len(after) != 1:
        raise AssertionError(
            f"expected exactly one live placeholder, found {sorted(after)}"
        )

    superseded = first - local_swaps("alice")

    if len(superseded) != 1:
        raise AssertionError(
            "the earlier placeholder was not archived: "
            f"{sorted(first)} -> {sorted(local_swaps('alice'))}"
        )

    archived = PROFILES["alice"]["state"] / "archive" / f"{superseded.pop()}.json"

    if not archived.exists():
        raise AssertionError(f"no archived copy was preserved: {archived}")

    #
    # An announced swap must never be touched by this sweep.
    #
    if SWAP_ID not in local_swaps("alice"):
        raise AssertionError("the sweep archived an announced swap")


def test_fund_refuses_when_several_swaps_are_ready():
    """
    With two funding-ready trades, `fund` must refuse: the choice of
    which trade receives funds is the operator's, never the engine's.
    """

    result = quiet("alice", "fund")
    text = result.stdout + result.stderr

    if "MULTIPLE FUNDING-READY SWAPS" not in text:
        raise AssertionError(f"`fund` did not report the ambiguity:\n{text}")

    if "Refusing to choose which trade receives funds." not in text:
        raise AssertionError(f"`fund` did not refuse:\n{text}")

    for swap_id in (FIRST_NEW_SWAP, SECOND_NEW_SWAP):
        if load_state("alice", swap_id).get("blake_funding_txid"):
            raise AssertionError(f"`fund` funded {swap_id} despite the ambiguity")


def test_resume_reports_an_already_settled_swap():
    result = quiet("alice", "resume", SWAP_ID)
    text = result.stdout + result.stderr

    if "SWAP ALREADY SETTLED" not in text:
        raise AssertionError(
            f"`resume` did not report the terminal swap:\n{text}"
        )

    if load_state("alice", SWAP_ID)["state"] != "COMPLETE":
        raise AssertionError("`resume` disturbed a settled swap")


def test_resume_without_a_prefix_refuses_an_ambiguous_queue():
    result = quiet("alice", "resume")
    text = result.stdout + result.stderr

    if "MULTIPLE ACTIVE SWAPS" not in text:
        raise AssertionError(
            f"`resume` did not report the ambiguous queue:\n{text}"
        )

    if "Run `resume <short-swap-prefix>`" not in text:
        raise AssertionError(f"`resume` offered no way forward:\n{text}")


def test_resume_rejects_an_absent_prefix():
    result = run("alice", "resume", "ffffffff", expect_ok=False)

    if result.returncode == 0:
        raise AssertionError(
            "`resume` accepted a prefix that matches no swap:\n" + result.stdout
        )


def test_accept_only_match_refuses_a_local_swap():
    """
    `accept-match` must refuse a match whose swap already has local
    state; re-importing would overwrite a live negotiation.
    """

    result = run("alice", "accept-match", MATCH_ID, expect_ok=False)

    if result.returncode == 0:
        raise AssertionError(
            "`accept-match` imported a match whose swap is already local:\n"
            + result.stdout
        )


def test_truthy_swap_prefix_is_rejected_by_dismiss():
    result = run("alice", "dismiss", "ffffffff", expect_ok=False)

    if result.returncode == 0:
        raise AssertionError("`dismiss` accepted a non-existent prefix")


def main():
    print()
    print("=" * 72)
    print("OPERATOR AND DRIVER SURFACE (regtest + local relay stub)")
    print("=" * 72)
    print()
    print("relay:", RELAY_URL)
    print("state:", BASE)
    print()

    try:
        h.preflight()
        ok("preflight")

        check("`resume` refuses an empty queue", test_resume_refuses_an_empty_queue)
        check("`resume` rejects a short prefix", test_resume_rejects_a_short_prefix)
        check("`next` drives the negotiation one safe step at a time", test_next_converges)
        check("`next` never funds a swap by itself", test_next_never_funded)
        check("`sync-funding` finds nothing for an unfunded swap", test_sync_funding_finds_nothing_for_an_unfunded_swap)
        check("`sync-settlement` changes nothing before any settlement", test_sync_settlement_changes_nothing_before_any_settlement)
        check("`complete` refuses an unfunded swap", test_complete_refuses_a_swap_that_is_not_claimable)
        check("`dashboard` reports the live swap", test_dashboard_reports_the_live_swap)
        check("the human marketplace views render", test_market_views_render_for_a_human)
        check("`dashboard --json` agrees with the human view", test_dashboard_json_matches_human_view)
        check("`reconcile --json` is read-only on an unfunded swap", test_reconcile_is_read_only_on_an_unfunded_swap)
        check("the human `reconcile` report is read-only", test_reconcile_human_report_is_read_only)
        check("`show` and `list` expose the local record", test_show_and_list_expose_the_record)
        check("`run` refuses economic actions before funding", test_run_refuses_economic_actions_before_funding)
        check("`resume-all` refuses initial funding", test_resume_all_refuses_initial_funding)
        check("`ignore-match` tombstones locally without creating state", test_ignore_match_tombstones_without_creating_state)
        check("`dismiss` archives a negotiation that never became fundable", test_dismiss_archives_a_negotiation_that_never_became_fundable)
        check("`fund` commits the single ready swap", test_fund_commits_the_single_ready_swap)
        check("`next` reports settlement work once BLAKE is funded", test_settlement_then_next_reports_settlement_work)
        check("`dismiss` refuses a funded swap", test_dismiss_refuses_a_funded_swap)
        check("`sync-funding` discovers the funding from the chain", test_sync_funding_discovers_the_funding_from_the_chain)
        check("`sync-settlement` finds no spend while only BLAKE is funded", test_sync_settlement_finds_no_spend_while_only_blake_is_funded)
        check("`sync-funding` discovers Bitcoin funding from the chain", test_funding_is_discoverable_from_the_chain)
        check("`sync-settlement` recovers the secret from the claim", test_the_claim_is_discoverable_from_the_chain)
        check("`sync-settlement` finds no spend after the Bitcoin claim", test_sync_settlement_finds_no_spend_after_the_bitcoin_claim)
        check("`resume` drives the settlement path to COMPLETE", test_resume_completes_the_settlement_path)
        check("the reports agree once the swap is settled", test_reports_agree_once_settled)
        check("`next` is quiet once everything is settled", test_next_is_quiet_once_everything_is_settled)
        check("`sync-settlement` is terminal-aware", test_sync_settlement_is_terminal_aware)
        check("`sync-settlement` leaves a settled swap alone", test_sync_settlement_leaves_a_settled_swap_alone)
        check("`rebroadcast-nostr-event` republishes a cached event verbatim", test_rebroadcast_republishes_a_cached_event_verbatim)
        check("`rebroadcast-nostr-event` refuses an uncached event", test_rebroadcast_refuses_an_uncached_event)
        check("`next` refuses to choose between two negotiations", test_next_refuses_to_choose_between_two_negotiations)
        check("`accept-take` accepts the only valid take", test_accept_take_accepts_the_only_valid_take)
        check("`accept-match` imports a fresh match", test_accept_match_imports_a_fresh_match)
        check("`next` refuses two different kinds of action", test_next_refuses_two_different_kinds_of_action)
        check("explicit resolution reaches two funding-ready swaps", test_explicit_resolution_reaches_two_funding_ready_swaps)
        check("a new swap archives an unannounced placeholder loudly", test_a_new_swap_archives_an_unannounced_placeholder_loudly)
        check("`resume` reports a swap it cannot classify", test_resume_reports_manual_attention)
        check("`resume-all` reports attention without blocking", test_resume_all_reports_attention_without_blocking_safe_swaps)
        check("`fund` refuses when several swaps are ready", test_fund_refuses_when_several_swaps_are_ready)
        check("`resume` reports an already settled swap", test_resume_reports_an_already_settled_swap)
        check("`resume` refuses an ambiguous queue", test_resume_without_a_prefix_refuses_an_ambiguous_queue)
        check("`resume` rejects an absent prefix", test_resume_rejects_an_absent_prefix)
        check("`accept-match` refuses a match with local state", test_accept_only_match_refuses_a_local_swap)
        check("`dismiss` rejects a non-existent prefix", test_truthy_swap_prefix_is_rejected_by_dismiss)
    finally:
        STUB.stop()

    print()
    print("=" * 72)
    print("DRIVER SURFACE SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()
    print(f"swap: {SWAP_ID}")
    print(
        f"stub traffic: connections={STUB.connections} "
        f"published={STUB.published} requests={STUB.requests}"
    )
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
