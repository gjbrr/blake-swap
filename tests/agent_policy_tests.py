#!/usr/bin/env python3
"""
Autonomous agent policy suite.

``swap_agent.py`` is the supervisor that lets a participant walk away: it
polls the local swap state, decides which engine command that state
implies, runs it as a child process, and keeps going through crashes and
restarts. Because it is the component that decides *when* money moves
without a human present, its (state -> command) table is worth pinning
exactly.

This suite drives that table directly. ``load_state`` and ``run_swap``
are replaced, so every check is a statement about the agent's decision
rather than about a chain:

  * each state maps to the command the protocol requires, for both roles,
    including the two states that reconcile first and therefore re-read
    the state before acting;
  * a terminal state ends the agent with no command at all;
  * an unknown state is an error naming the role, never a guess;
  * a child that fails is a non-fatal step, but a child that reports a
    `SECURITY FAILURE` stops the agent immediately;
  * the loop gives up after repeated consecutive failures instead of
    spinning forever;
  * SIGTERM unwinds cleanly rather than terminating mid-write.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import contextlib
import io
import sys
from pathlib import Path
from types import SimpleNamespace


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(ROOT / "tests"))

import node_config  # noqa: E402

node_config.require_coincurve()

import swap_agent  # noqa: E402

PASS = 0
FAIL = 0

SWAP = "1234567890ab"


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


class patched:
    """Replace module attributes, restoring them on exit."""

    def __init__(self, module, **values):
        self.module = module
        self.values = values
        self.saved = {}

    def __enter__(self):
        for name, value in self.values.items():
            self.saved[name] = getattr(self.module, name)
            setattr(self.module, name, value)

        return self

    def __exit__(self, *exc):
        for name, value in self.saved.items():
            setattr(self.module, name, value)

        return False


class recorder:
    """
    A stand-in for ``run_swap``.

    ``results`` is consumed in order; the last value is repeated. Each
    call also advances the state the agent will read next, which is how
    the "reconcile first, then act" paths are exercised.
    """

    def __init__(self, results=(True,), states=None, initial=None, extra=None):
        self.results = list(results)
        self.states = list(states or [])
        self.state = initial
        self.extra = dict(extra or {})
        self.calls = []

    def load_state(self, swap_id, role):
        data = {"state": self.state, "swap_id": swap_id}
        data.update(self.extra)
        return data

    def run_swap(self, role, *args):
        self.calls.append((role,) + tuple(str(a) for a in args))

        if self.states:
            self.state = self.states.pop(0)

        if self.results:
            return self.results.pop(0) if len(self.results) > 1 else self.results[0]

        return True

    def commands(self):
        return [call[1] for call in self.calls]


def step(role, state, *, states=(), results=(True,), fn=None, extra=None):
    """Run one agent step and return (result, recorded calls)."""

    rec = recorder(
        results=results,
        states=states,
        initial=state,
        extra=extra,
    )

    with patched(swap_agent, load_state=rec.load_state, run_swap=rec.run_swap):
        result = (fn or getattr(swap_agent, f"{role}_step"))(SWAP)

    return result, rec.calls


def verify(name, role, state, expected, **kwargs):
    try:
        _, calls = step(role, state, **kwargs)
    except Exception as e:  # noqa: BLE001
        fail(name, f"{type(e).__name__}: {e}")
        return

    observed = [call[1] for call in calls]

    if observed != expected:
        fail(name, f"expected {expected}, got {observed}")
        return

    # Every recorded call must carry the role and the swap id.
    for call in calls:
        if call[0] != role:
            fail(name, f"a command ran as {call[0]}, expected {role}")
            return

        if SWAP not in call:
            fail(name, f"a command omitted the swap id: {call}")
            return

    ok(name)


# --------------------------------------------------------------- alice


def test_alice_state_table():
    verify("Alice funds BLAKE from OFFERED", "alice", "OFFERED", ["fund-blake"])

    verify(
        "Alice rediscovers funding from BLAKE_FUNDED",
        "alice",
        "BLAKE_FUNDED",
        ["sync-funding"],
    )

    verify(
        "Alice reconciles then claims from BTC_FUNDED",
        "alice",
        "BTC_FUNDED",
        ["sync-settlement", "claim-btc"],
        states=["BTC_FUNDED"],
    )

    verify(
        "Alice stops when reconciliation already advanced the swap",
        "alice",
        "BTC_FUNDED",
        ["sync-settlement"],
        states=["BTC_CLAIMED"],
        results=(True,),
    )

    verify(
        "Alice reconciles a claimed Bitcoin spend",
        "alice",
        "BTC_CLAIMED",
        ["sync-settlement"],
    )

    verify(
        "Alice completes a claimed BLAKE swap",
        "alice",
        "BLAKE_CLAIMED",
        ["complete"],
    )

    verify(
        "Alice reconciles after a Bitcoin refund",
        "alice",
        "BTC_REFUNDED",
        ["sync-settlement"],
    )

    for terminal in ("COMPLETE", "BLAKE_REFUNDED"):
        result, calls = step("alice", terminal)

        if result is not True or calls:
            fail(
                f"Alice stops at {terminal}",
                f"result={result!r} calls={calls}",
            )
        else:
            ok(f"Alice stops at {terminal}")


def test_alice_refuses_an_unknown_state():
    try:
        step("alice", "TELEPORTED")
    except RuntimeError as e:
        if "Alice" not in str(e) or "TELEPORTED" not in str(e):
            raise AssertionError(f"unhelpful error: {e}") from e
    else:
        raise AssertionError("Alice acted on an unknown state")


# ----------------------------------------------------------------- bob


def test_bob_state_table():
    verify(
        "Bob watches BLAKE from OFFERED",
        "bob",
        "OFFERED",
        ["sync-funding"],
    )

    verify(
        "Bob rediscovers funding then funds Bitcoin",
        "bob",
        "BLAKE_FUNDED",
        ["sync-funding", "fund-btc"],
        states=["BLAKE_FUNDED"],
    )

    verify(
        "Bob stops when reconciliation already advanced the swap",
        "bob",
        "BLAKE_FUNDED",
        ["sync-funding"],
        states=["BTC_FUNDED"],
    )

    verify(
        "Bob waits for Alice's claim after funding Bitcoin",
        "bob",
        "BTC_FUNDED",
        ["sync-settlement"],
    )

    #
    # `revealed_secret_hex` is what distinguishes "reconciliation has
    # already recovered S" from the older durable state that still needs
    # an explicit extraction step.
    #
    verify(
        "Bob claims BLAKE once the secret is known",
        "bob",
        "BTC_CLAIMED",
        ["sync-settlement", "claim-blake"],
        states=["BTC_CLAIMED"],
        extra={"revealed_secret_hex": "ab" * 32},
    )

    verify(
        "Bob extracts a missing secret before claiming",
        "bob",
        "BTC_CLAIMED",
        ["extract-secret", "sync-settlement", "claim-blake"],
        states=["BTC_CLAIMED", "BTC_CLAIMED"],
    )

    verify(
        "Bob stops when the secret cannot be extracted",
        "bob",
        "BTC_CLAIMED",
        ["extract-secret"],
        results=(False,),
    )

    verify(
        "Bob stops when reconciliation already claimed BLAKE",
        "bob",
        "BTC_CLAIMED",
        ["sync-settlement"],
        states=["BLAKE_CLAIMED"],
        extra={"revealed_secret_hex": "ab" * 32},
    )

    verify(
        "Bob completes a claimed BLAKE swap",
        "bob",
        "BLAKE_CLAIMED",
        ["complete"],
    )

    verify(
        "Bob reconciles after a Bitcoin refund",
        "bob",
        "BTC_REFUNDED",
        ["sync-settlement"],
    )

    for terminal in ("COMPLETE", "BLAKE_REFUNDED"):
        result, calls = step("bob", terminal)

        if result is not True or calls:
            fail(
                f"Bob stops at {terminal}",
                f"result={result!r} calls={calls}",
            )
        else:
            ok(f"Bob stops at {terminal}")


def test_bob_refuses_an_unknown_state():
    try:
        step("bob", "TELEPORTED")
    except RuntimeError as e:
        if "Bob" not in str(e) or "TELEPORTED" not in str(e):
            raise AssertionError(f"unhelpful error: {e}") from e
    else:
        raise AssertionError("Bob acted on an unknown state")


# ------------------------------------------------------------- run_swap


class fake_run:
    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        self.calls = []

    def __call__(self, cmd, **kwargs):
        self.calls.append((list(cmd), kwargs))

        return SimpleNamespace(
            returncode=self.returncode,
            stdout=self.stdout,
            stderr=self.stderr,
        )


def test_run_swap_configures_the_child():
    fake = fake_run(stdout="ok\n")

    with patched(swap_agent.subprocess, run=fake):
        with contextlib.redirect_stdout(io.StringIO()):
            result = swap_agent.run_swap("alice", "fund-blake", SWAP)

    if result is not True:
        raise AssertionError("a successful child was treated as a failure")

    cmd, kwargs = fake.calls[0]

    if str(swap_agent.SWAP) not in cmd or "fund-blake" not in cmd:
        raise AssertionError(f"wrong command: {cmd}")

    env = kwargs["env"]

    if env.get("BLAKE_SWAP_ROLE") != "alice":
        raise AssertionError(f"role not set for the child: {env}")

    if env.get("BLAKE_SWAP_TRANSPORT") != "file":
        raise AssertionError(f"transport not pinned: {env}")


def test_run_swap_forwards_child_output():
    fake = fake_run(stdout="hello out\n", stderr="hello err\n")

    out = io.StringIO()
    err = io.StringIO()

    with patched(swap_agent.subprocess, run=fake):
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            swap_agent.run_swap("bob", "sync-funding", SWAP)

    if "hello out" not in out.getvalue():
        raise AssertionError(f"stdout was not forwarded: {out.getvalue()!r}")

    if "hello err" not in err.getvalue():
        raise AssertionError(f"stderr was not forwarded: {err.getvalue()!r}")


def test_run_swap_reports_a_plain_failure_as_a_failed_step():
    for stderr, stdout in (("boom\n", ""), ("", "boom\n")):
        fake = fake_run(returncode=1, stdout=stdout, stderr=stderr)

        with patched(swap_agent.subprocess, run=fake):
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(
                io.StringIO()
            ):
                result = swap_agent.run_swap("bob", "fund-btc", SWAP)

        if result is not False:
            raise AssertionError("a failing child was reported as success")


def test_run_swap_escalates_a_security_failure():
    for stderr, stdout in (
        ("SECURITY FAILURE: nested funding lock\n", ""),
        ("", "SECURITY FAILURE: wrong role\n"),
    ):
        fake = fake_run(returncode=1, stdout=stdout, stderr=stderr)

        with patched(swap_agent.subprocess, run=fake):
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(
                io.StringIO()
            ):
                try:
                    swap_agent.run_swap("bob", "claim-blake", SWAP)
                except RuntimeError as e:
                    if "SECURITY FAILURE" not in str(e):
                        raise AssertionError(f"wrong error: {e}") from e
                else:
                    raise AssertionError(
                        "a SECURITY FAILURE child was treated as a normal step"
                    )


# ------------------------------------------------------------------ main


def run_main(role, states, results, argv=None):
    """
    Drive ``main()`` with a scripted state sequence.

    ``states`` must end with a terminal state, which is then repeated:
    that is what makes the loop finite. ``results`` are returned in order
    by the step function, and its last value is repeated.
    """

    sequence = list(states)
    outcomes = list(results)

    calls = []

    def load_state(swap_id, role_name):
        if len(sequence) > 1:
            state = sequence.pop(0)
        else:
            state = sequence[0]

        return {"state": state}

    def step_fn(swap_id):
        calls.append(swap_id)
        return outcomes.pop(0) if len(outcomes) > 1 else outcomes[0]

    output = io.StringIO()
    error = io.StringIO()

    with patched(
        swap_agent,
        load_state=load_state,
        alice_step=step_fn,
        bob_step=step_fn,
        time=SimpleNamespace(sleep=lambda seconds: None),
    ):
        with patched(
            sys,
            argv=argv or ["swap_agent.py", SWAP, "--role", role, "--interval", "0"],
        ):
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error):
                status = swap_agent.main()

    return status, output.getvalue(), error.getvalue(), calls


def test_main_exits_zero_on_a_terminal_state():
    status, text, _, calls = run_main("alice", ["COMPLETE"], [True])


    if status != 0:
        raise AssertionError(f"main returned {status}:\n{text}")

    if calls:
        raise AssertionError(f"main acted on a terminal swap: {calls}")

    if "FINISHED state=COMPLETE" not in text:
        raise AssertionError(f"main did not report finishing:\n{text}")


def test_main_gives_up_after_repeated_failures():
    #
    # A step that keeps failing must not spin forever. 20 consecutive
    # failures is the documented bound.
    #
    status, text, error, calls = run_main(
        "bob",
        ["BLAKE_FUNDED"] * 20 + ["COMPLETE"],
        [False] * 20,
    )

    if status != 3:
        raise AssertionError(f"main returned {status}, expected 3:\n{error}")

    if "too many consecutive failures" not in error:
        raise AssertionError(f"main did not explain giving up:\n{error}")

    if len(calls) != 20:
        raise AssertionError(f"main made {len(calls)} attempts, expected 20")


def test_main_stops_immediately_on_a_security_failure():
    output = io.StringIO()
    err = io.StringIO()
    recorded = []

    def step_fn(swap_id):
        recorded.append(swap_id)
        raise RuntimeError("SECURITY FAILURE: wrong role")

    with patched(
        swap_agent,
        load_state=lambda swap_id, role_name: {"state": "BLAKE_FUNDED"},
        alice_step=step_fn,
        bob_step=step_fn,
        time=SimpleNamespace(sleep=lambda seconds: None),
    ):
        with patched(
            sys,
            argv=["swap_agent.py", SWAP, "--role", "alice", "--interval", "0"],
        ):
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(err):
                status = swap_agent.main()

    if status != 2:
        raise AssertionError(f"a SECURITY FAILURE returned {status}, expected 2")

    if len(recorded) != 1:
        raise AssertionError(
            f"the agent kept acting after a SECURITY FAILURE: {recorded}"
        )

    if "SECURITY FAILURE" not in err.getvalue():
        raise AssertionError(f"the failure was not reported:\n{err.getvalue()}")


def test_main_resets_the_failure_count_after_a_success():
    #
    # 19 failures, a success, then 19 more failures must never reach the
    # bound: the counter tracks *consecutive* failures.
    #
    results = [False] * 19 + [True] + [False] * 19 + [True]

    status, _, error, calls = run_main(
        "alice",
        ["BLAKE_FUNDED"] * len(results) + ["COMPLETE"],
        results,
    )

    if status == 3:
        raise AssertionError(
            f"the failure counter did not reset:\n{error}"
        )

    if len(calls) != len(results):
        raise AssertionError(f"main made {len(calls)} attempts: {len(results)}")


def test_sigterm_unwinds_cleanly():
    try:
        swap_agent._graceful_termination(15, None)
    except SystemExit as e:
        if e.code != 0:
            raise AssertionError(f"SIGTERM exited with {e.code}") from e
    else:
        raise AssertionError("SIGTERM did not unwind")


def main():
    print()
    print("=" * 72)
    print("AUTONOMOUS AGENT POLICY")
    print("=" * 72)
    print()

    check("Alice's state table", test_alice_state_table)
    check("Alice refuses an unknown state", test_alice_refuses_an_unknown_state)

    check("Bob's state table", test_bob_state_table)
    check("Bob refuses an unknown state", test_bob_refuses_an_unknown_state)

    check("the child process is configured", test_run_swap_configures_the_child)
    check("child output is forwarded", test_run_swap_forwards_child_output)
    check("a plain failure is a failed step", test_run_swap_reports_a_plain_failure_as_a_failed_step)
    check("a SECURITY FAILURE escalates", test_run_swap_escalates_a_security_failure)

    check("a terminal state exits zero", test_main_exits_zero_on_a_terminal_state)
    check("the loop gives up after repeated failures", test_main_gives_up_after_repeated_failures)
    check("a SECURITY FAILURE stops the loop", test_main_stops_immediately_on_a_security_failure)
    check("the failure counter resets on success", test_main_resets_the_failure_count_after_a_success)
    check("SIGTERM unwinds cleanly", test_sigterm_unwinds_cleanly)

    print()
    print("=" * 72)
    print("AGENT POLICY SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
