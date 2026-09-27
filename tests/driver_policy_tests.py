#!/usr/bin/env python3
"""
Autonomous driver policy suite.

``command_run`` — the `blake run` driver — is the one command in the
engine that can move money without a human in the loop, so the decision
that guards every automatic action is the most safety-critical code in
the repository:

    claim-btc      only as protocol Alice, only with confirmed funding
                   on both chains, only with a locally generated secret
                   that matches the hashlock, and only with a healthy
                   Bitcoin timeout margin;
    fund-btc       only as protocol Bob, only when readiness was
                   evaluated and passed, only with confirmed BLAKE
                   funding, and only while Bitcoin funding is absent;
    refund-btc     only as protocol Bob, only from BTC_FUNDED, only with
                   confirmed funding, and only after the timelock;
    refund-blake   only as protocol Alice, only from BLAKE_FUNDED or
                   BTC_REFUNDED, and only with independent evidence that
                   Bitcoin funding is absent.

Driving those guards through real chains would need a swap in every
combination of checkpoint, chain status, secret provenance and timeout
margin. This suite drives them instead through the driver's actual
decision code, with the reconciliation report — its only input — supplied
directly, and with every action executor replaced by a recorder.

That makes each check a statement about the policy rather than about the
harness: an action must be attempted exactly when it is permitted, and
must never be attempted when it is refused.

Exit status: 0 pass, 1 fail.
"""

from __future__ import annotations

import contextlib
import io
import os
import sys
from pathlib import Path
from types import SimpleNamespace


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"

sys.path.insert(0, str(SRC))
sys.path.insert(0, str(ROOT / "tests"))

import node_config  # noqa: E402

node_config.require_coincurve()

import swap_v2  # noqa: E402

PASS = 0
FAIL = 0

SWAP = "1234567890ab"

ACTION_FUNCTIONS = (
    "command_sync_funding",
    "command_sync_settlement",
    "command_complete",
    "command_claim_blake",
    "command_claim_btc",
    "command_fund_btc",
    "command_refund_btc",
    "command_refund_blake",
)


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


def report(**overrides):
    """
    A reconciliation report that permits `claim-btc`.

    Individual checks relax exactly one field, so a failing check points
    at one guard rather than at the whole report.
    """

    base = {
        "swap_id": SWAP,
        "protocol_role": "alice",
        "checkpoint_state": "BLAKE_FUNDED",
        "decision_class": "ready",
        "next_action": "claim-btc",
        "reason": "test fixture",
        "blake": {"status": "funded"},
        "btc": {"status": "funded"},
        "secret": {
            "known": True,
            "source": "locally-generated",
            "hash_matches": True,
        },
        "timeouts": {
            "btc_blocks_remaining": 10,
            "blake_blocks_remaining": 10,
        },
        "readiness": {"checked": True, "ready": True, "reasons": []},
    }

    base.update(overrides)

    return base


class Driver:
    """
    Run ``command_run`` once against a stubbed report.

    ``load_swap`` and ``build_reconcile_report`` are replaced because
    they are the driver's only inputs; the eight action executors are
    replaced so that "did the driver try to act?" is directly
    observable and nothing ever touches a chain.
    """

    def __init__(self, *, raises=None):
        self.raises = raises

    def __enter__(self):
        self.calls = []
        self.overrides = {}

        self.overrides["load_swap"] = swap_v2.load_swap
        self.overrides["build_reconcile_report"] = swap_v2.build_reconcile_report

        swap_v2.load_swap = lambda *a, **k: object()
        swap_v2.build_reconcile_report = lambda swap: self.report

        for name in ACTION_FUNCTIONS:
            self.overrides[name] = getattr(swap_v2, name)
            setattr(swap_v2, name, self._recorder(name))

        return self

    def _recorder(self, name):
        def call(ns):
            self.calls.append((name, getattr(ns, "swap_id", None), getattr(ns, "role", None)))

            if self.raises == name:
                raise RuntimeError("injected failure")

        return call

    def __exit__(self, *exc):
        for name, value in self.overrides.items():
            setattr(swap_v2, name, value)

        return False

    def run(self, report_value, *, max_steps=1):
        self.report = report_value

        buffer = io.StringIO()
        status = 0

        with contextlib.redirect_stdout(buffer):
            try:
                swap_v2.command_run(
                    SimpleNamespace(swap_id=SWAP, max_steps=max_steps)
                )
            except SystemExit as e:
                status = e.code

        return buffer.getvalue(), status


def drive(report_value, *, max_steps=1, raises=None):
    with Driver(raises=raises) as driver:
        text, status = driver.run(report_value, max_steps=max_steps)

    return text, driver.calls, status


def expect_refusal(report_value, phrase, *, actions=()):
    """Assert the driver refused, named the reason, and did not act."""

    text, calls, status = drive(report_value)

    if status != 0:
        raise AssertionError(f"the driver exited {status}:\n{text}")

    if phrase not in text:
        raise AssertionError(
            f"the driver did not say {phrase!r}:\n{text}"
        )

    unexpected = [name for name, _, _ in calls if name not in actions]

    if unexpected:
        raise AssertionError(
            f"the driver refused but still called {unexpected}"
        )


def expect_step_failure(report_value, phrase):
    """
    Assert a violated precondition stopped the driver, and nothing acted.

    ``command_run`` contains a step's failure deliberately: a raised
    precondition becomes a reported "step failed" with a non-zero exit,
    not a traceback and not a partially executed action.
    """

    text, calls, status = drive(report_value)

    if status != 1:
        raise AssertionError(
            f"the driver exited {status}, expected a contained failure:\n{text}"
        )

    if "RUN RESULT: step failed" not in text:
        raise AssertionError(f"the failure was not contained:\n{text}")

    if phrase not in text:
        raise AssertionError(f"the driver did not say {phrase!r}:\n{text}")

    if calls:
        raise AssertionError(
            f"a refused precondition still executed {[c[0] for c in calls]}"
        )


def expect_action(report_value, action_function, *, max_steps=1):
    text, calls, status = drive(report_value, max_steps=max_steps)

    if status != 0:
        raise AssertionError(f"the driver exited {status}:\n{text}")

    called = [name for name, _, _ in calls]

    if called != [action_function] * max_steps:
        raise AssertionError(
            f"expected {action_function} x{max_steps}, got {called}"
        )

    if "PASS: automatic action completed" not in text:
        raise AssertionError(
            f"the driver did not report completion:\n{text}"
        )

    return text, calls


# ------------------------------------------------------------- decisions


def test_settled_does_nothing():
    expect_refusal(
        report(decision_class="settled", next_action="none"),
        "settled; no action required",
    )


def test_waiting_does_nothing():
    expect_refusal(
        report(decision_class="waiting", next_action="wait"),
        "waiting for counterparty or chain condition",
    )


def test_manual_attention_does_nothing():
    expect_refusal(
        report(decision_class="manual-attention"),
        "manual attention required",
    )


def test_economic_action_is_never_taken():
    expect_refusal(
        report(next_action="fund-blake", decision_class="ready"),
        "ACTION REQUIRED",
    )


def test_unrecognised_action_is_refused():
    expect_refusal(
        report(next_action="publish-offer"),
        "refusing unrecognized or non-allowlisted action",
    )


def test_unknown_role_is_refused():
    expect_refusal(
        report(protocol_role="unknown"),
        "protocol role is not securely known",
    )


def test_step_budget_is_bounded():
    for bad in (0, -1, 21, 100):
        try:
            drive(report(), max_steps=bad)
        except ValueError as e:
            if "--max-steps" not in str(e):
                raise AssertionError(f"unhelpful error: {e}") from e
        else:
            raise AssertionError(f"max_steps={bad} was accepted")


# ------------------------------------------------------------- claim-btc


def test_claim_btc_requires_protocol_alice():
    expect_step_failure(
        report(protocol_role="bob"),
        "SECURITY FAILURE: automatic claim-btc requires current participant "
        "to be protocol Alice",
    )


def test_claim_btc_requires_confirmed_blake_funding():
    expect_refusal(
        report(blake={"status": "absent"}),
        "BLAKE HTLC is not confirmed and unspent",
    )


def test_claim_btc_requires_confirmed_btc_funding():
    expect_refusal(
        report(btc={"status": "absent"}),
        "Bitcoin HTLC is not confirmed and unspent",
    )


def test_claim_btc_requires_a_locally_generated_matching_secret():
    for secret in (
        {"known": False},
        {"known": True, "source": "learned-from-chain", "hash_matches": True},
        {"known": True, "source": "locally-generated", "hash_matches": False},
        {"known": True, "source": "locally-generated"},
    ):
        expect_refusal(
            report(secret=secret),
            "protocol Alice secret preconditions are not proven",
        )


def test_claim_btc_requires_a_timeout_margin():
    expect_refusal(
        report(timeouts={"btc_blocks_remaining": 1}),
        "Bitcoin timeout margin is too small",
    )


def test_claim_btc_margin_override_is_validated():
    original = os.environ.get("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS")
    os.environ["BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS"] = "0"

    try:
        expect_step_failure(
            report(),
            "BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS must be >= 1",
        )
    finally:
        if original is None:
            os.environ.pop("BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS", None)
        else:
            os.environ["BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS"] = original


def test_claim_btc_executes_when_permitted():
    expect_action(report(), "command_claim_btc")


def test_claim_btc_records_the_swap_and_role():
    _, calls, _ = drive(report())

    if calls != [("command_claim_btc", SWAP, "alice")]:
        raise AssertionError(f"unexpected executor call: {calls}")


# ------------------------------------------------------------ claim-blake


def test_claim_blake_requires_protocol_bob():
    expect_step_failure(
        report(next_action="claim-blake", protocol_role="alice"),
        "SECURITY FAILURE: automatic claim-blake requires current participant "
        "to be protocol Bob",
    )


def test_claim_blake_executes_for_bob():
    expect_action(
        report(next_action="claim-blake", protocol_role="bob"),
        "command_claim_blake",
    )


# --------------------------------------------------------------- fund-btc


def test_fund_btc_requires_protocol_bob():
    expect_step_failure(
        report(next_action="fund-btc", protocol_role="alice"),
        "SECURITY FAILURE: automatic fund-btc requires current participant "
        "to be protocol Bob",
    )


def test_fund_btc_requires_evaluated_readiness():
    expect_refusal(
        report(
            next_action="fund-btc",
            protocol_role="bob",
            readiness={"checked": False},
        ),
        "funding readiness was not evaluated",
    )


def test_fund_btc_requires_passing_readiness():
    expect_refusal(
        report(
            next_action="fund-btc",
            protocol_role="bob",
            readiness={
                "checked": True,
                "ready": False,
                "reasons": ["wallet is not loaded"],
            },
        ),
        "local funding readiness did not pass",
    )


def test_fund_btc_requires_confirmed_blake_funding():
    expect_refusal(
        report(
            next_action="fund-btc",
            protocol_role="bob",
            blake={"status": "absent"},
        ),
        "BLAKE HTLC is not confirmed and unspent",
    )


def test_fund_btc_requires_absent_bitcoin_funding():
    expect_refusal(
        report(
            next_action="fund-btc",
            protocol_role="bob",
            btc={"status": "funded"},
        ),
        "Bitcoin funding is no longer absent",
    )


def test_fund_btc_executes_when_permitted():
    expect_action(
        report(
            next_action="fund-btc",
            protocol_role="bob",
            btc={"status": "absent"},
        ),
        "command_fund_btc",
    )


# ------------------------------------------------------------- refund-btc


def test_refund_btc_requires_protocol_bob():
    expect_step_failure(
        report(next_action="refund-btc", protocol_role="alice"),
        "SECURITY FAILURE: automatic refund-btc requires current participant "
        "to be protocol Bob",
    )


def test_refund_btc_requires_the_btc_funded_checkpoint():
    expect_refusal(
        report(
            next_action="refund-btc",
            protocol_role="bob",
            checkpoint_state="BLAKE_FUNDED",
        ),
        "local checkpoint is not BTC_FUNDED",
    )


def test_refund_btc_requires_confirmed_funding():
    expect_refusal(
        report(
            next_action="refund-btc",
            protocol_role="bob",
            checkpoint_state="BTC_FUNDED",
            btc={"status": "absent"},
        ),
        "Bitcoin HTLC is not confirmed and unspent",
    )


def test_refund_btc_requires_a_mature_timelock():
    expect_refusal(
        report(
            next_action="refund-btc",
            protocol_role="bob",
            checkpoint_state="BTC_FUNDED",
            timeouts={"btc_blocks_remaining": 5},
        ),
        "Bitcoin refund timelock is not mature",
    )


def test_refund_btc_executes_when_permitted():
    expect_action(
        report(
            next_action="refund-btc",
            protocol_role="bob",
            checkpoint_state="BTC_FUNDED",
            timeouts={"btc_blocks_remaining": 0},
        ),
        "command_refund_btc",
    )


# ----------------------------------------------------------- refund-blake


def test_refund_blake_requires_protocol_alice():
    expect_step_failure(
        report(next_action="refund-blake", protocol_role="bob"),
        "SECURITY FAILURE: automatic refund-blake requires current participant "
        "to be protocol Alice",
    )


def test_refund_blake_requires_a_refund_eligible_checkpoint():
    expect_refusal(
        report(
            next_action="refund-blake",
            protocol_role="alice",
            checkpoint_state="OFFERED",
        ),
        "local checkpoint is not refund-eligible",
    )


def test_refund_blake_requires_confirmed_funding():
    expect_refusal(
        report(
            next_action="refund-blake",
            protocol_role="alice",
            blake={"status": "absent"},
        ),
        "BLAKE HTLC is not confirmed and unspent",
    )


def test_pre_btc_refund_requires_absent_bitcoin_funding():
    expect_refusal(
        report(
            next_action="refund-blake",
            protocol_role="alice",
            checkpoint_state="BLAKE_FUNDED",
            btc={"status": "funded"},
        ),
        "pre-BTC refund requires Bitcoin funding to be independently absent",
    )


def test_refund_blake_requires_a_mature_timelock():
    expect_refusal(
        report(
            next_action="refund-blake",
            protocol_role="alice",
            checkpoint_state="BLAKE_FUNDED",
            btc={"status": "absent"},
            timeouts={"blake_blocks_remaining": 3},
        ),
        "BLAKE refund timelock is not mature",
    )


def test_refund_blake_executes_when_permitted():
    expect_action(
        report(
            next_action="refund-blake",
            protocol_role="alice",
            checkpoint_state="BLAKE_FUNDED",
            btc={"status": "absent"},
            timeouts={"blake_blocks_remaining": 0},
        ),
        "command_refund_blake",
    )


def test_refund_blake_executes_after_a_bitcoin_refund():
    #
    # BTC_REFUNDED is the other refund-eligible checkpoint: the
    # pre-BTC-absence rule does not apply, because Bitcoin funding
    # already happened and was refunded.
    #
    expect_action(
        report(
            next_action="refund-blake",
            protocol_role="alice",
            checkpoint_state="BTC_REFUNDED",
            btc={"status": "spent"},
            timeouts={"blake_blocks_remaining": 0},
        ),
        "command_refund_blake",
    )


# ------------------------------------------------- sync and completion


def test_sync_funding_is_automatic():
    expect_action(report(next_action="sync-funding"), "command_sync_funding")


def test_sync_settlement_is_automatic():
    expect_action(
        report(next_action="sync-settlement"),
        "command_sync_settlement",
    )


def test_complete_is_automatic():
    expect_action(report(next_action="complete"), "command_complete")


def test_the_driver_repeats_until_the_step_budget_is_spent():
    text, calls = expect_action(
        report(next_action="complete"),
        "command_complete",
        max_steps=3,
    )

    if "maximum reconciliation steps reached" not in text:
        raise AssertionError(f"the driver did not report the budget:\n{text}")


# ------------------------------------------------------ failure containment


def test_a_failing_step_is_contained_and_reported():
    text, calls, status = drive(
        report(next_action="complete"),
        max_steps=2,
        raises="command_complete",
    )

    if status != 1:
        raise AssertionError(f"a failed step exited {status}, expected 1")

    if "RUN RESULT: step failed" not in text:
        raise AssertionError(f"the step failure was not reported:\n{text}")

    if "step:  1 of 2" not in text:
        raise AssertionError(f"the failing step number is wrong:\n{text}")

    if "PASS: no further automatic action was attempted" not in text:
        raise AssertionError(f"the driver did not stop cleanly:\n{text}")

    if len(calls) != 1:
        raise AssertionError(f"the driver kept going after a failure: {calls}")


def test_the_runner_banner_states_the_policy():
    text, _, _ = drive(report(next_action="complete"))

    for expected in (
        "BLAKE SWAP AUTONOMOUS DRIVER",
        "POLICY: recovery, local completion, protocol-Alice Bitcoin claim, "
        "and protocol-Bob BLAKE claim may execute automatically",
        "POLICY: BLAKE funding and Nostr publication are disabled",
        "BLAKE SWAP RUNNER",
        "RECONCILIATION 1",
    ):
        if expected not in text:
            raise AssertionError(f"the driver omitted {expected!r}:\n{text}")


def main():
    print()
    print("=" * 72)
    print("AUTONOMOUS DRIVER POLICY")
    print("=" * 72)
    print()

    check("a settled swap needs no action", test_settled_does_nothing)
    check("a waiting swap is left alone", test_waiting_does_nothing)
    check("manual attention stops the driver", test_manual_attention_does_nothing)
    check("the driver never makes the economic decision", test_economic_action_is_never_taken)
    check("an unrecognised action is refused", test_unrecognised_action_is_refused)
    check("an unknown protocol role is refused", test_unknown_role_is_refused)
    check("the step budget is bounded", test_step_budget_is_bounded)

    check("claim-btc requires protocol Alice", test_claim_btc_requires_protocol_alice)
    check("claim-btc requires confirmed BLAKE funding", test_claim_btc_requires_confirmed_blake_funding)
    check("claim-btc requires confirmed Bitcoin funding", test_claim_btc_requires_confirmed_btc_funding)
    check("claim-btc requires a locally generated matching secret", test_claim_btc_requires_a_locally_generated_matching_secret)
    check("claim-btc requires a Bitcoin timeout margin", test_claim_btc_requires_a_timeout_margin)
    check("the claim-btc margin override is validated", test_claim_btc_margin_override_is_validated)
    check("claim-btc executes when permitted", test_claim_btc_executes_when_permitted)
    check("claim-btc records the swap and role", test_claim_btc_records_the_swap_and_role)

    check("claim-blake requires protocol Bob", test_claim_blake_requires_protocol_bob)
    check("claim-blake executes for Bob", test_claim_blake_executes_for_bob)

    check("fund-btc requires protocol Bob", test_fund_btc_requires_protocol_bob)
    check("fund-btc requires evaluated readiness", test_fund_btc_requires_evaluated_readiness)
    check("fund-btc requires passing readiness", test_fund_btc_requires_passing_readiness)
    check("fund-btc requires confirmed BLAKE funding", test_fund_btc_requires_confirmed_blake_funding)
    check("fund-btc requires absent Bitcoin funding", test_fund_btc_requires_absent_bitcoin_funding)
    check("fund-btc executes when permitted", test_fund_btc_executes_when_permitted)

    check("refund-btc requires protocol Bob", test_refund_btc_requires_protocol_bob)
    check("refund-btc requires the BTC_FUNDED checkpoint", test_refund_btc_requires_the_btc_funded_checkpoint)
    check("refund-btc requires confirmed funding", test_refund_btc_requires_confirmed_funding)
    check("refund-btc requires a mature timelock", test_refund_btc_requires_a_mature_timelock)
    check("refund-btc executes when permitted", test_refund_btc_executes_when_permitted)

    check("refund-blake requires protocol Alice", test_refund_blake_requires_protocol_alice)
    check("refund-blake requires a refund-eligible checkpoint", test_refund_blake_requires_a_refund_eligible_checkpoint)
    check("refund-blake requires confirmed funding", test_refund_blake_requires_confirmed_funding)
    check("a pre-BTC refund requires absent Bitcoin funding", test_pre_btc_refund_requires_absent_bitcoin_funding)
    check("refund-blake requires a mature timelock", test_refund_blake_requires_a_mature_timelock)
    check("refund-blake executes when permitted", test_refund_blake_executes_when_permitted)
    check("refund-blake executes after a Bitcoin refund", test_refund_blake_executes_after_a_bitcoin_refund)

    check("sync-funding is automatic", test_sync_funding_is_automatic)
    check("sync-settlement is automatic", test_sync_settlement_is_automatic)
    check("complete is automatic", test_complete_is_automatic)
    check("the driver repeats until the budget is spent", test_the_driver_repeats_until_the_step_budget_is_spent)

    check("a failing step is contained and reported", test_a_failing_step_is_contained_and_reported)
    check("the runner banner states the policy", test_the_runner_banner_states_the_policy)

    print()
    print("=" * 72)
    print("DRIVER POLICY SUMMARY")
    print("=" * 72)
    print(f"passes:   {PASS}")
    print(f"failures: {FAIL}")
    print()

    return 1 if FAIL else 0


if __name__ == "__main__":
    raise SystemExit(main())
