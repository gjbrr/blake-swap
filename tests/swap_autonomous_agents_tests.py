#!/usr/bin/env python3

from __future__ import annotations

import os
from pathlib import Path
import signal
import subprocess
import sys
import time

import swap_protocol_tests as h


AGENT = str(
    Path(
        os.environ.get(
            "SWAP_AGENT",
            Path(__file__).resolve().parents[1]
            / "src"
            / "swap_agent.py",
        )
    )
)

LOG_DIR = (
    h.MESSAGE_DIR.parent
    / "agent-logs"
)

ECONOMIC_MESSAGES = (
    "blake-funded",
    "btc-funded",
    "btc-claimed",
    "blake-claimed",
)


def message_path(
    swap_id,
    message_type,
):
    return (
        h.MESSAGE_DIR
        / f"{swap_id}.{message_type}.json"
    )


def start_agent(
    swap_id,
    role,
    generation,
):
    LOG_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )

    path = (
        LOG_DIR
        / (
            f"{swap_id}."
            f"{role}."
            f"{generation}.log"
        )
    )

    log = path.open(
        "w"
    )

    env = os.environ.copy()

    #
    # Autonomous Alice and Bob now have independent
    # participant state roots.
    #
    if role == "alice":
        env["BLAKE_SWAP_STATE_DIR"] = str(
            h.ALICE_BASE
        )
        env["BLAKE_SWAP_CHAIN_WALLET"] = (
            h.ALICE_WALLET
        )
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.ALICE_PARTICIPANT_KEY
        )

    elif role == "bob":
        env["BLAKE_SWAP_STATE_DIR"] = str(
            h.BOB_BASE
        )
        env["BLAKE_SWAP_CHAIN_WALLET"] = (
            h.BOB_WALLET
        )
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.BOB_PARTICIPANT_KEY
        )

    else:
        raise h.TestFailure(
            f"unknown autonomous role: {role}"
        )

    #
    # File transport itself remains shared between
    # the two isolated participant state trees.
    #
    env["BLAKE_SWAP_MESSAGE_DIR"] = str(
        h.MESSAGE_DIR
    )

    env[
        "BLAKE_SWAP_TRANSPORT"
    ] = "file"

    env[
        "BLAKE_SWAP_ROLE"
    ] = role

    p = subprocess.Popen(
        [
            sys.executable,
            AGENT,
            swap_id,
            "--role",
            role,
            "--interval",
            "0.15",
        ],
        stdout=log,
        stderr=subprocess.STDOUT,
        env=env,
        start_new_session=True,
    )

    return {
        "process": p,
        "log": log,
        "path": path,
        "generation": generation,
    }


def kill_agent(
    item,
):
    p = item["process"]

    if p.poll() is None:

        try:
            os.killpg(
                p.pid,
                signal.SIGKILL,
            )
        except ProcessLookupError:
            pass

    try:
        p.wait(
            timeout=2
        )
    except subprocess.TimeoutExpired:
        pass

    item["log"].close()


def close_agent(
    item,
):
    p = item["process"]

    if p.poll() is None:

        try:
            os.killpg(
                p.pid,
                signal.SIGTERM,
            )
        except ProcessLookupError:
            pass

        try:
            p.wait(
                timeout=2
            )
        except subprocess.TimeoutExpired:

            try:
                os.killpg(
                    p.pid,
                    signal.SIGKILL,
                )
            except ProcessLookupError:
                pass

    item["log"].close()


def mine_stranded_mempool_transactions():
    """
    Public networks mine independently.

    On regtest, commands normally mine their own confirmation block.
    If SIGKILL lands between broadcast and that mining call, simulate
    external block production by mining ONLY when a mempool is nonempty.

    This avoids artificially marching the short regtest timelocks forward.
    """

    btc_pool = h.btc_cli(
        "getrawmempool"
    )

    if btc_pool:
        h.mine_btc(1)

    blake_pool = h.blake_cli(
        "getrawmempool"
    )

    if blake_pool:
        h.mine_blake(1)


def states(
    swap_id,
):
    return (
        h.load_state(
            swap_id,
            "alice",
        )["state"],
        h.load_state(
            swap_id,
            "bob",
        )["state"],
    )


def require_no_economic_messages(
    swap_id,
):
    for message_type in ECONOMIC_MESSAGES:

        path = message_path(
            swap_id,
            message_type,
        )

        h.require(
            not path.exists(),
            (
                "autonomous swap unexpectedly used "
                f"{message_type} coordination message"
            ),
        )


def print_log_tail(
    path,
    lines=30,
):
    try:
        data = path.read_text().splitlines()

    except Exception:
        return

    print()
    print(
        f"--- {path.name} ---"
    )

    for line in data[
        -lines:
    ]:
        print(
            line
        )


def autonomous_trial(
    trial,
):
    h.banner(
        f"AUTONOMOUS PARTICIPANTS TRIAL {trial}"
    )

    swap_id = h.make_fresh_swap()

    #
    # Negotiation still exists.
    #
    h.handshake(
        swap_id
    )

    require_no_economic_messages(
        swap_id
    )

    alice_gen = 1
    bob_gen = 1

    alice = start_agent(
        swap_id,
        "alice",
        alice_gen,
    )

    bob = start_agent(
        swap_id,
        "bob",
        bob_gen,
    )

    all_logs = [
        alice["path"],
        bob["path"],
    ]

    #
    # Deterministic chaos schedule.
    #
    # We are not trying to hit exact instruction boundaries here;
    # those are already covered by the deterministic settlement
    # fault-injection suite.
    #
    chaos = [
        (0.35, "alice"),
        (0.60, "bob"),
        (0.85, "both"),
        (1.10, "alice"),
        (1.35, "bob"),
    ]

    start = time.time()
    chaos_index = 0

    timeout = 30.0

    try:

        while True:

            elapsed = (
                time.time()
                - start
            )

            mine_stranded_mempool_transactions()

            a_state, b_state = states(
                swap_id
            )

            if (
                a_state == "COMPLETE"
                and b_state == "COMPLETE"
            ):
                break

            #
            # If an agent exited unexpectedly before COMPLETE,
            # automatically restart it.
            #
            if (
                alice["process"].poll()
                is not None
                and a_state != "COMPLETE"
            ):
                alice["log"].close()

                alice_gen += 1

                alice = start_agent(
                    swap_id,
                    "alice",
                    alice_gen,
                )

                all_logs.append(
                    alice["path"]
                )

            if (
                bob["process"].poll()
                is not None
                and b_state != "COMPLETE"
            ):
                bob["log"].close()

                bob_gen += 1

                bob = start_agent(
                    swap_id,
                    "bob",
                    bob_gen,
                )

                all_logs.append(
                    bob["path"]
                )

            #
            # Kill/restart participants while the protocol is live.
            #
            if chaos_index < len(
                chaos
            ):

                when, target = chaos[
                    chaos_index
                ]

                if elapsed >= when:

                    if target in {
                        "alice",
                        "both",
                    }:

                        kill_agent(
                            alice
                        )

                        alice_gen += 1

                        alice = start_agent(
                            swap_id,
                            "alice",
                            alice_gen,
                        )

                        all_logs.append(
                            alice["path"]
                        )

                    if target in {
                        "bob",
                        "both",
                    }:

                        kill_agent(
                            bob
                        )

                        bob_gen += 1

                        bob = start_agent(
                            swap_id,
                            "bob",
                            bob_gen,
                        )

                        all_logs.append(
                            bob["path"]
                        )

                    chaos_index += 1

            if elapsed > timeout:

                raise RuntimeError(
                    "autonomous swap timed out; "
                    f"Alice={a_state}, Bob={b_state}"
                )

            time.sleep(
                0.05
            )

    except Exception:

        print()
        print(
            "AUTONOMOUS AGENT LOG TAILS"
        )

        for path in all_logs:
            print_log_tail(
                path
            )

        raise

    finally:

        close_agent(
            alice
        )

        close_agent(
            bob
        )

    h.assert_state(
        swap_id,
        "alice",
        "COMPLETE",
    )

    h.assert_state(
        swap_id,
        "bob",
        "COMPLETE",
    )

    h.ok(
        "Alice and Bob autonomously reached COMPLETE"
    )

    require_no_economic_messages(
        swap_id
    )

    h.ok(
        "no post-negotiation economic messages were used"
    )

    h.assert_secret_separation(
        swap_id
    )

    h.ok(
        "Bob learned S only from Bitcoin"
    )

    h.ok(
        "agents survived repeated process death and restart"
    )

    print(
        "swap:",
        swap_id,
    )

    print(
        "Alice generations:",
        alice_gen,
    )

    print(
        "Bob generations:  ",
        bob_gen,
    )


def main():
    h.PASS_COUNT = 0
    h.FAIL_COUNT = 0

    started = time.time()

    try:

        h.preflight()
        h.ok(
            "preflight"
        )

        for trial in range(
            1,
            4,
        ):

            try:

                autonomous_trial(
                    trial
                )

            except Exception as e:

                h.fail(
                    f"AUTONOMOUS TRIAL {trial}",
                    str(e),
                )

    except Exception as e:

        h.fail(
            "HARNESS",
            str(e),
        )

    elapsed = (
        time.time()
        - started
    )

    h.banner(
        "AUTONOMOUS PARTICIPANT SUMMARY"
    )

    print(
        f"passes:   {h.PASS_COUNT}"
    )

    print(
        f"failures: {h.FAIL_COUNT}"
    )

    print(
        f"elapsed:  {elapsed:.1f}s"
    )

    print()
    print(
        "Regtest only. Alice and Bob ran as independent "
        "processes and were repeatedly killed/restarted."
    )

    return (
        1
        if h.FAIL_COUNT
        else 0
    )


if __name__ == "__main__":
    raise SystemExit(
        main()
    )
