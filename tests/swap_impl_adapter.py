#!/usr/bin/env python3

"""
Participant-specific implementation boundary for portable swap tests.

The adversarial scheduler may combine implementations role by role:

    Python Alice / Python Bob
    Python Alice / Moxie Bob
    Moxie Alice  / Python Bob
    Moxie Alice  / Moxie Bob

The pair object coordinates participants but does not require them to use
the same language, process model, state layout, or locking mechanism.
"""

from dataclasses import dataclass
from typing import Any, Protocol


class ParticipantAdapter(Protocol):
    name: str
    role: str

    def command(
        self,
        *args,
        expect_ok: bool = True,
        echo: bool = False,
        extra_env: dict | None = None,
    ):
        ...

    def state(
        self,
        swap_id: str,
    ) -> dict:
        ...

    def assert_state(
        self,
        swap_id: str,
        expected: str,
    ) -> None:
        ...


@dataclass
class PythonParticipantAdapter:
    """
    One protocol participant backed by the Python reference engine.
    """

    h: Any
    role: str

    def __post_init__(self):
        if self.role not in {
            "alice",
            "bob",
        }:
            raise ValueError(
                f"unknown protocol role: {self.role}"
            )

    @property
    def name(self):
        return (
            f"python-reference-{self.role}"
        )

    def command(
        self,
        *args,
        expect_ok=True,
        echo=False,
        extra_env=None,
    ):
        return self.h.swap_cmd(
            *args,
            role=self.role,
            expect_ok=expect_ok,
            echo=echo,
            extra_env=extra_env,
        )

    def state(
        self,
        swap_id: str,
    ) -> dict:
        return self.h.load_state(
            swap_id,
            self.role,
        )

    def assert_state(
        self,
        swap_id: str,
        expected: str,
    ) -> None:
        self.h.assert_state(
            swap_id,
            self.role,
            expected,
        )


class SwapImplementationPair:
    """
    Two independently replaceable protocol participants.

    The convenience methods below are scenario orchestration only.
    Each protocol operation is explicitly routed to Alice or Bob.

    No assumption is made that both participant adapters have the same
    implementation.
    """

    def __init__(
        self,
        h,
        alice: ParticipantAdapter,
        bob: ParticipantAdapter,
    ):
        if alice.role != "alice":
            raise ValueError(
                "Alice adapter must have role alice"
            )

        if bob.role != "bob":
            raise ValueError(
                "Bob adapter must have role bob"
            )

        self.h = h
        self.alice = alice
        self.bob = bob

    @property
    def name(self):
        return (
            f"{self.alice.name} / "
            f"{self.bob.name}"
        )

    def participant(
        self,
        role: str,
    ) -> ParticipantAdapter:
        if role == "alice":
            return self.alice

        if role == "bob":
            return self.bob

        raise ValueError(
            f"unknown protocol role: {role}"
        )

    def preflight(self):
        #
        # Chain/wallet preflight currently belongs to the common regtest
        # harness, not either swap implementation.
        #
        self.h.preflight()

    def command(
        self,
        *args,
        role=None,
        expect_ok=True,
        echo=False,
        extra_env=None,
    ):
        if role is None:
            raise ValueError(
                "portable command requires explicit protocol role"
            )

        return self.participant(
            role
        ).command(
            *args,
            expect_ok=expect_ok,
            echo=echo,
            extra_env=extra_env,
        )

    def state(
        self,
        swap_id: str,
        role: str,
    ) -> dict:
        return self.participant(
            role
        ).state(
            swap_id
        )

    def assert_state(
        self,
        swap_id: str,
        role: str,
        expected: str,
    ):
        self.participant(
            role
        ).assert_state(
            swap_id,
            expected,
        )

    def make_fresh_swap(
        self,
        *,
        blake=0.001,
        sats=25_000,
    ) -> str:
        #
        # Alice alone creates S and the initial swap.
        #
        p = self.alice.command(
            "new",
            "--blake",
            str(blake),
            "--sats",
            str(sats),
        )

        swap_id = self.h.parse_swap_id(
            p.stdout
        )

        state = self.alice.state(
            swap_id
        )

        self.h.require(
            state.get(
                "timeout_policy_version"
            ) == "1.5",
            (
                "fresh portable test swap did "
                "not use mandatory v1.5 timeout policy"
            ),
        )

        return swap_id

    def handshake(
        self,
        swap_id: str,
    ):
        #
        # Explicit participant boundary.
        #
        self.alice.command(
            "publish-offer",
            swap_id,
        )

        self.bob.command(
            "accept-offer",
            swap_id,
        )

        self.alice.command(
            "receive-accept",
            swap_id,
        )

        a = self.alice.state(
            swap_id
        )

        b = self.bob.state(
            swap_id
        )

        self.h.require(
            a.get("blake_bob_pub"),
            "Alice did not learn Bob BLAKE pubkey",
        )

        self.h.require(
            a.get("btc_bob_pub"),
            "Alice did not learn Bob BTC pubkey",
        )

        self.h.require(
            b.get("blake_alice_pub"),
            "Bob did not learn Alice BLAKE pubkey",
        )

        self.h.require(
            b.get("btc_alice_pub"),
            "Bob did not learn Alice BTC pubkey",
        )

    def fund_blake_and_sync(
        self,
        swap_id: str,
    ):
        #
        # Alice moves BLAKE.
        #
        self.alice.command(
            "fund-blake",
            swap_id,
        )

        self.alice.assert_state(
            swap_id,
            "BLAKE_FUNDED",
        )

        #
        # Coordination carries only public funding identity.
        #
        self.alice.command(
            "publish-blake-funded",
            swap_id,
        )

        #
        # Bob independently validates it.
        #
        self.bob.command(
            "receive-blake-funded",
            swap_id,
        )

        self.bob.assert_state(
            swap_id,
            "BLAKE_FUNDED",
        )

        #
        # Exact replay must remain harmless.
        #
        self.bob.command(
            "receive-blake-funded",
            swap_id,
        )

        self.bob.assert_state(
            swap_id,
            "BLAKE_FUNDED",
        )

    def fund_btc_and_sync(
        self,
        swap_id: str,
    ):
        #
        # Bob moves Bitcoin.
        #
        self.bob.command(
            "fund-btc",
            swap_id,
        )

        self.bob.assert_state(
            swap_id,
            "BTC_FUNDED",
        )

        self.bob.command(
            "publish-btc-funded",
            swap_id,
        )

        #
        # Alice independently validates it.
        #
        self.alice.command(
            "receive-btc-funded",
            swap_id,
        )

        self.alice.assert_state(
            swap_id,
            "BTC_FUNDED",
        )

        #
        # Exact replay must remain harmless.
        #
        self.alice.command(
            "receive-btc-funded",
            swap_id,
        )

        self.alice.assert_state(
            swap_id,
            "BTC_FUNDED",
        )

    def assert_secret_separation(
        self,
        swap_id: str,
    ):
        #
        # State knowledge is observed through the participant adapters.
        #
        alice = self.alice.state(
            swap_id
        )

        bob = self.bob.state(
            swap_id
        )

        self.h.require(
            bool(alice.get("secret_hex")),
            "Alice lost original secret S",
        )

        self.h.require(
            bob.get("secret_hex") is None,
            "Bob received Alice's original secret S",
        )

        self.h.require(
            bob.get(
                "revealed_secret_hex"
            ) is None,
            "Bob learned S before Bitcoin witness extraction",
        )

        #
        # FILE-transport inspection remains a responsibility of the
        # common adversarial harness. It is deliberately outside either
        # participant implementation.
        #
        self.h.assert_secret_separation(
            swap_id
        )

    #
    # Chain control belongs to the test laboratory, not Alice or Bob.
    #
    def btc_height(self):
        return int(
            self.h.btc_cli(
                "getblockcount"
            )
        )

    def blake_height(self):
        return int(
            self.h.blake_cli(
                "getblockcount"
            )
        )

    def mine_btc(
        self,
        blocks=1,
    ):
        return self.h.mine_btc(
            blocks
        )

    def mine_blake(
        self,
        blocks=1,
    ):
        return self.h.mine_blake(
            blocks
        )


def make_python_reference_pair(h):
    return SwapImplementationPair(
        h,
        alice=PythonParticipantAdapter(
            h,
            "alice",
        ),
        bob=PythonParticipantAdapter(
            h,
            "bob",
        ),
    )


PARTICIPANT_ADAPTER_SURFACE = (
    "command",
    "state",
    "assert_state",
)


# ----------------------------------------------------------------------
# JSON subprocess boundary
# ----------------------------------------------------------------------

import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace


class JsonSubprocessParticipantAdapter:
    """
    Participant controlled through a language-neutral JSON subprocess.

    The child executable receives one JSON request on stdin and emits one
    JSON response on stdout.

    Python's bridge is only the reference implementation of this wire
    contract. A Moxie executable can implement the same request/response
    behavior without sharing any Python code.
    """

    def __init__(
        self,
        h,
        role,
        executable,
        *,
        bridge=None,
    ):
        if role not in {
            "alice",
            "bob",
        }:
            raise ValueError(
                f"unknown protocol role: {role}"
            )

        self.h = h
        self.role = role
        self.executable = str(
            executable
        )

        if bridge is None:
            bridge = (
                Path(__file__).resolve().parent
                / "swap_adapter_bridge.py"
            )

        self.bridge = str(
            bridge
        )

    @property
    def name(self):
        return (
            f"json-subprocess-python-"
            f"{self.role}"
        )

    def _environment(self):
        env = os.environ.copy()

        #
        # Do not inherit an operator's participant session.
        #
        for key in (
            "BLAKE_SWAP_STATE_DIR",
            "BLAKE_SWAP_MESSAGE_DIR",
            "BLAKE_SWAP_ROLE",
            "BLAKE_SWAP_CHAIN_WALLET",
            "BLAKE_SWAP_BLAKE_WALLET",
            "BLAKE_SWAP_BTC_WALLET",
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_DIR",
        ):
            env.pop(
                key,
                None,
            )

        for key, value in (
            self.h.node_env()
        ).items():
            env.setdefault(
                key,
                value,
            )

        env[
            "BLAKE_SWAP_TRANSPORT"
        ] = "file"

        env[
            "BLAKE_SWAP_TEST_MODE"
        ] = "1"

        env[
            "BLAKE_SWAP_NOSTR_RELAYS"
        ] = "ws://127.0.0.1:1"

        env[
            "BLAKE_SWAP_MESSAGE_DIR"
        ] = str(
            self.h.MESSAGE_DIR
        )

        env[
            "BLAKE_SWAP_ROLE"
        ] = self.role

        if self.role == "alice":
            base = self.h.ALICE_BASE
            wallet = self.h.ALICE_WALLET
            keyfile = (
                self.h.ALICE_PARTICIPANT_KEY
            )
        else:
            base = self.h.BOB_BASE
            wallet = self.h.BOB_WALLET
            keyfile = (
                self.h.BOB_PARTICIPANT_KEY
            )

        env[
            "BLAKE_SWAP_STATE_DIR"
        ] = str(base)

        env[
            "BLAKE_SWAP_CHAIN_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BLAKE_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BTC_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE"
        ] = str(keyfile)

        return env

    def _request(
        self,
        request,
        *,
        extra_env=None,
    ):
        env = self._environment()

        if extra_env:
            env.update(
                extra_env
            )

        p = subprocess.run(
            [
                sys.executable,
                self.bridge,
            ],
            input=(
                json.dumps(
                    request,
                    sort_keys=True,
                )
                + "\n"
            ),
            text=True,
            capture_output=True,
            env=env,
        )

        if p.returncode != 0:
            raise self.h.TestFailure(
                (
                    "adapter bridge process "
                    f"failed ({p.returncode})\n"
                    f"stdout:\n{p.stdout}\n"
                    f"stderr:\n{p.stderr}"
                )
            )

        try:
            response = json.loads(
                p.stdout
            )
        except json.JSONDecodeError as e:
            raise self.h.TestFailure(
                (
                    "adapter bridge returned "
                    "non-JSON output:\n"
                    + p.stdout
                )
            ) from e

        return response

    def command(
        self,
        *args,
        expect_ok=True,
        echo=False,
        extra_env=None,
    ):
        response = self._request(
            {
                "op": "command",
                "role": self.role,
                "executable":
                    self.executable,
                "args": [
                    str(x)
                    for x in args
                ],
            },
            extra_env=extra_env,
        )

        rc = int(
            response.get(
                "returncode",
                1,
            )
        )

        stdout = (
            response.get(
                "stdout"
            )
            or ""
        )

        stderr = (
            response.get(
                "stderr"
            )
            or ""
        )

        result = SimpleNamespace(
            returncode=rc,
            stdout=stdout,
            stderr=stderr,
        )

        if echo or (
            expect_ok
            and rc != 0
        ):
            if stdout:
                print(
                    stdout,
                    end=(
                        ""
                        if stdout.endswith("\n")
                        else "\n"
                    ),
                )

            if stderr:
                print(
                    stderr,
                    end=(
                        ""
                        if stderr.endswith("\n")
                        else "\n"
                    ),
                    file=sys.stderr,
                )

        if (
            expect_ok
            and rc != 0
        ):
            raise self.h.TestFailure(
                (
                    f"adapter command failed "
                    f"({rc}): "
                    + " ".join(
                        str(x)
                        for x in args
                    )
                    + "\n"
                    + (
                        stderr.strip()
                        or stdout.strip()
                    )
                )
            )

        return result

    def state(
        self,
        swap_id,
    ):
        #
        # h.state_file knows the Python reference's storage location.
        # This field is bridge configuration, not a portable Moxie
        # requirement.
        #
        path = self.h.state_file(
            swap_id,
            self.role,
        )

        response = self._request({
            "op": "state",
            "role": self.role,
            "swap_id": swap_id,
            "state_path": str(path),
        })

        if not response.get(
            "ok"
        ):
            raise self.h.TestFailure(
                response.get(
                    "error",
                    "adapter state read failed",
                )
            )

        return response[
            "state"
        ]

    def assert_state(
        self,
        swap_id,
        expected,
    ):
        actual = self.state(
            swap_id
        ).get(
            "state"
        )

        self.h.require(
            actual == expected,
            (
                f"{self.role} state "
                f"{actual!r}, "
                f"expected {expected!r}"
            ),
        )


def make_python_json_subprocess_pair(h):
    return SwapImplementationPair(
        h,
        alice=JsonSubprocessParticipantAdapter(
            h,
            "alice",
            h.SWAP_V2,
        ),
        bob=JsonSubprocessParticipantAdapter(
            h,
            "bob",
            h.SWAP_V2,
        ),
    )


# ----------------------------------------------------------------------
# JSON subprocess boundary
# ----------------------------------------------------------------------

import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace


class JsonSubprocessParticipantAdapter:
    """
    Participant controlled through a language-neutral JSON subprocess.

    The child executable receives one JSON request on stdin and emits one
    JSON response on stdout.

    Python's bridge is only the reference implementation of this wire
    contract. A Moxie executable can implement the same request/response
    behavior without sharing any Python code.
    """

    def __init__(
        self,
        h,
        role,
        executable,
        *,
        bridge=None,
    ):
        if role not in {
            "alice",
            "bob",
        }:
            raise ValueError(
                f"unknown protocol role: {role}"
            )

        self.h = h
        self.role = role
        self.executable = str(
            executable
        )

        if bridge is None:
            bridge = (
                Path(__file__).resolve().parent
                / "swap_adapter_bridge.py"
            )

        self.bridge = str(
            bridge
        )

    @property
    def name(self):
        return (
            f"json-subprocess-python-"
            f"{self.role}"
        )

    def _environment(self):
        env = os.environ.copy()

        #
        # Do not inherit an operator's participant session.
        #
        for key in (
            "BLAKE_SWAP_STATE_DIR",
            "BLAKE_SWAP_MESSAGE_DIR",
            "BLAKE_SWAP_ROLE",
            "BLAKE_SWAP_CHAIN_WALLET",
            "BLAKE_SWAP_BLAKE_WALLET",
            "BLAKE_SWAP_BTC_WALLET",
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_DIR",
        ):
            env.pop(
                key,
                None,
            )

        for key, value in (
            self.h.node_env()
        ).items():
            env.setdefault(
                key,
                value,
            )

        env[
            "BLAKE_SWAP_TRANSPORT"
        ] = "file"

        env[
            "BLAKE_SWAP_TEST_MODE"
        ] = "1"

        env[
            "BLAKE_SWAP_NOSTR_RELAYS"
        ] = "ws://127.0.0.1:1"

        env[
            "BLAKE_SWAP_MESSAGE_DIR"
        ] = str(
            self.h.MESSAGE_DIR
        )

        env[
            "BLAKE_SWAP_ROLE"
        ] = self.role

        if self.role == "alice":
            base = self.h.ALICE_BASE
            wallet = self.h.ALICE_WALLET
            keyfile = (
                self.h.ALICE_PARTICIPANT_KEY
            )
        else:
            base = self.h.BOB_BASE
            wallet = self.h.BOB_WALLET
            keyfile = (
                self.h.BOB_PARTICIPANT_KEY
            )

        env[
            "BLAKE_SWAP_STATE_DIR"
        ] = str(base)

        env[
            "BLAKE_SWAP_CHAIN_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BLAKE_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BTC_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE"
        ] = str(keyfile)

        return env

    def _request(
        self,
        request,
        *,
        extra_env=None,
    ):
        env = self._environment()

        if extra_env:
            env.update(
                extra_env
            )

        p = subprocess.run(
            [
                sys.executable,
                self.bridge,
            ],
            input=(
                json.dumps(
                    request,
                    sort_keys=True,
                )
                + "\n"
            ),
            text=True,
            capture_output=True,
            env=env,
        )

        if p.returncode != 0:
            raise self.h.TestFailure(
                (
                    "adapter bridge process "
                    f"failed ({p.returncode})\n"
                    f"stdout:\n{p.stdout}\n"
                    f"stderr:\n{p.stderr}"
                )
            )

        try:
            response = json.loads(
                p.stdout
            )
        except json.JSONDecodeError as e:
            raise self.h.TestFailure(
                (
                    "adapter bridge returned "
                    "non-JSON output:\n"
                    + p.stdout
                )
            ) from e

        return response

    def command(
        self,
        *args,
        expect_ok=True,
        echo=False,
        extra_env=None,
    ):
        response = self._request(
            {
                "op": "command",
                "role": self.role,
                "executable":
                    self.executable,
                "args": [
                    str(x)
                    for x in args
                ],
            },
            extra_env=extra_env,
        )

        rc = int(
            response.get(
                "returncode",
                1,
            )
        )

        stdout = (
            response.get(
                "stdout"
            )
            or ""
        )

        stderr = (
            response.get(
                "stderr"
            )
            or ""
        )

        result = SimpleNamespace(
            returncode=rc,
            stdout=stdout,
            stderr=stderr,
        )

        if echo or (
            expect_ok
            and rc != 0
        ):
            if stdout:
                print(
                    stdout,
                    end=(
                        ""
                        if stdout.endswith("\n")
                        else "\n"
                    ),
                )

            if stderr:
                print(
                    stderr,
                    end=(
                        ""
                        if stderr.endswith("\n")
                        else "\n"
                    ),
                    file=sys.stderr,
                )

        if (
            expect_ok
            and rc != 0
        ):
            raise self.h.TestFailure(
                (
                    f"adapter command failed "
                    f"({rc}): "
                    + " ".join(
                        str(x)
                        for x in args
                    )
                    + "\n"
                    + (
                        stderr.strip()
                        or stdout.strip()
                    )
                )
            )

        return result

    def state(
        self,
        swap_id,
    ):
        #
        # h.state_file knows the Python reference's storage location.
        # This field is bridge configuration, not a portable Moxie
        # requirement.
        #
        path = self.h.state_file(
            swap_id,
            self.role,
        )

        response = self._request({
            "op": "state",
            "role": self.role,
            "swap_id": swap_id,
            "state_path": str(path),
        })

        if not response.get(
            "ok"
        ):
            raise self.h.TestFailure(
                response.get(
                    "error",
                    "adapter state read failed",
                )
            )

        return response[
            "state"
        ]

    def assert_state(
        self,
        swap_id,
        expected,
    ):
        actual = self.state(
            swap_id
        ).get(
            "state"
        )

        self.h.require(
            actual == expected,
            (
                f"{self.role} state "
                f"{actual!r}, "
                f"expected {expected!r}"
            ),
        )


def make_python_json_subprocess_pair(h):
    return SwapImplementationPair(
        h,
        alice=JsonSubprocessParticipantAdapter(
            h,
            "alice",
            h.SWAP_V2,
        ),
        bob=JsonSubprocessParticipantAdapter(
            h,
            "bob",
            h.SWAP_V2,
        ),
    )


# ----------------------------------------------------------------------
# JSON subprocess boundary
# ----------------------------------------------------------------------

import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace


class JsonSubprocessParticipantAdapter:
    """
    Participant controlled through a language-neutral JSON subprocess.

    The child executable receives one JSON request on stdin and emits one
    JSON response on stdout.
    """

    def __init__(
        self,
        h,
        role,
        executable,
        *,
        bridge=None,
    ):
        if role not in {
            "alice",
            "bob",
        }:
            raise ValueError(
                f"unknown protocol role: {role}"
            )

        self.h = h
        self.role = role
        self.executable = str(executable)

        if bridge is None:
            bridge = (
                Path(__file__).resolve().parent
                / "swap_adapter_bridge.py"
            )

        self.bridge = str(bridge)

    @property
    def name(self):
        return (
            f"json-subprocess-python-"
            f"{self.role}"
        )

    def _environment(self):
        env = os.environ.copy()

        for key in (
            "BLAKE_SWAP_STATE_DIR",
            "BLAKE_SWAP_MESSAGE_DIR",
            "BLAKE_SWAP_ROLE",
            "BLAKE_SWAP_CHAIN_WALLET",
            "BLAKE_SWAP_BLAKE_WALLET",
            "BLAKE_SWAP_BTC_WALLET",
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_DIR",
        ):
            env.pop(
                key,
                None,
            )

        for key, value in (
            self.h.node_env()
        ).items():
            env.setdefault(
                key,
                value,
            )

        env[
            "BLAKE_SWAP_TRANSPORT"
        ] = "file"

        env[
            "BLAKE_SWAP_TEST_MODE"
        ] = "1"

        env[
            "BLAKE_SWAP_NOSTR_RELAYS"
        ] = "ws://127.0.0.1:1"

        env[
            "BLAKE_SWAP_MESSAGE_DIR"
        ] = str(
            self.h.MESSAGE_DIR
        )

        env[
            "BLAKE_SWAP_ROLE"
        ] = self.role

        if self.role == "alice":
            base = self.h.ALICE_BASE
            wallet = self.h.ALICE_WALLET
            keyfile = (
                self.h.ALICE_PARTICIPANT_KEY
            )
        else:
            base = self.h.BOB_BASE
            wallet = self.h.BOB_WALLET
            keyfile = (
                self.h.BOB_PARTICIPANT_KEY
            )

        env[
            "BLAKE_SWAP_STATE_DIR"
        ] = str(base)

        env[
            "BLAKE_SWAP_CHAIN_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BLAKE_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BTC_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE"
        ] = str(keyfile)

        return env

    def _request(
        self,
        request,
        *,
        extra_env=None,
    ):
        env = self._environment()

        if extra_env:
            env.update(
                extra_env
            )

        p = subprocess.run(
            [
                sys.executable,
                self.bridge,
            ],
            input=(
                json.dumps(
                    request,
                    sort_keys=True,
                )
                + "\n"
            ),
            text=True,
            capture_output=True,
            env=env,
        )

        if p.returncode != 0:
            raise self.h.TestFailure(
                (
                    "adapter bridge process "
                    f"failed ({p.returncode})\n"
                    f"stdout:\n{p.stdout}\n"
                    f"stderr:\n{p.stderr}"
                )
            )

        try:
            response = json.loads(
                p.stdout
            )
        except json.JSONDecodeError as e:
            raise self.h.TestFailure(
                (
                    "adapter bridge returned "
                    "non-JSON output:\n"
                    + p.stdout
                )
            ) from e

        return response

    def command(
        self,
        *args,
        expect_ok=True,
        echo=False,
        extra_env=None,
    ):
        response = self._request(
            {
                "op": "command",
                "role": self.role,
                "executable":
                    self.executable,
                "args": [
                    str(x)
                    for x in args
                ],
            },
            extra_env=extra_env,
        )

        rc = int(
            response.get(
                "returncode",
                1,
            )
        )

        stdout = (
            response.get(
                "stdout"
            )
            or ""
        )

        stderr = (
            response.get(
                "stderr"
            )
            or ""
        )

        result = SimpleNamespace(
            returncode=rc,
            stdout=stdout,
            stderr=stderr,
        )

        if echo or (
            expect_ok
            and rc != 0
        ):
            if stdout:
                print(
                    stdout,
                    end=(
                        ""
                        if stdout.endswith("\n")
                        else "\n"
                    ),
                )

            if stderr:
                print(
                    stderr,
                    end=(
                        ""
                        if stderr.endswith("\n")
                        else "\n"
                    ),
                    file=sys.stderr,
                )

        if (
            expect_ok
            and rc != 0
        ):
            raise self.h.TestFailure(
                (
                    f"adapter command failed "
                    f"({rc}): "
                    + " ".join(
                        str(x)
                        for x in args
                    )
                    + "\n"
                    + (
                        stderr.strip()
                        or stdout.strip()
                    )
                )
            )

        return result

    def state(
        self,
        swap_id,
    ):
        path = self.h.state_file(
            swap_id,
            self.role,
        )

        response = self._request({
            "op": "state",
            "role": self.role,
            "swap_id": swap_id,
            "state_path": str(path),
        })

        if not response.get("ok"):
            raise self.h.TestFailure(
                response.get(
                    "error",
                    "adapter state read failed",
                )
            )

        return response["state"]

    def assert_state(
        self,
        swap_id,
        expected,
    ):
        actual = self.state(
            swap_id
        ).get("state")

        self.h.require(
            actual == expected,
            (
                f"{self.role} state "
                f"{actual!r}, "
                f"expected {expected!r}"
            ),
        )


def make_python_json_subprocess_pair(h):
    return SwapImplementationPair(
        h,
        alice=JsonSubprocessParticipantAdapter(
            h,
            "alice",
            h.SWAP_V2,
        ),
        bob=JsonSubprocessParticipantAdapter(
            h,
            "bob",
            h.SWAP_V2,
        ),
    )


class ExternalJsonParticipantAdapter:
    """
    Participant implemented by an arbitrary external executable.

    The executable receives exactly one JSON request on stdin and must
    emit exactly one JSON response on stdout.

    No Python bridge is involved.

    This is the integration boundary intended for Moxie.
    """

    def __init__(
        self,
        h,
        role,
        executable,
        *,
        extra_env=None,
    ):
        if role not in {
            "alice",
            "bob",
        }:
            raise ValueError(
                f"unknown protocol role: {role}"
            )

        self.h = h
        self.role = role
        self.executable = str(
            executable
        )
        self.extra_env = dict(
            extra_env or {}
        )

    @property
    def name(self):
        return (
            f"external-json-{self.role}"
        )

    def _environment(self):
        env = os.environ.copy()

        for key in (
            "BLAKE_SWAP_STATE_DIR",
            "BLAKE_SWAP_MESSAGE_DIR",
            "BLAKE_SWAP_ROLE",
            "BLAKE_SWAP_CHAIN_WALLET",
            "BLAKE_SWAP_BLAKE_WALLET",
            "BLAKE_SWAP_BTC_WALLET",
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_FILE",
            "BLAKE_SWAP_NOSTR_KEY_DIR",
        ):
            env.pop(
                key,
                None,
            )

        for key, value in (
            self.h.node_env()
        ).items():
            env.setdefault(
                key,
                value,
            )

        env[
            "BLAKE_SWAP_TRANSPORT"
        ] = "file"

        env[
            "BLAKE_SWAP_TEST_MODE"
        ] = "1"

        env[
            "BLAKE_SWAP_NOSTR_RELAYS"
        ] = "ws://127.0.0.1:1"

        env[
            "BLAKE_SWAP_MESSAGE_DIR"
        ] = str(
            self.h.MESSAGE_DIR
        )

        env[
            "BLAKE_SWAP_ROLE"
        ] = self.role

        if self.role == "alice":
            base = self.h.ALICE_BASE
            wallet = self.h.ALICE_WALLET
            keyfile = (
                self.h.ALICE_PARTICIPANT_KEY
            )
        else:
            base = self.h.BOB_BASE
            wallet = self.h.BOB_WALLET
            keyfile = (
                self.h.BOB_PARTICIPANT_KEY
            )

        #
        # These are laboratory resources, not requirements on Moxie's
        # internal storage architecture. Moxie may use the supplied
        # directory or place its own files beneath it.
        #
        env[
            "BLAKE_SWAP_STATE_DIR"
        ] = str(base)

        env[
            "BLAKE_SWAP_CHAIN_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BLAKE_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_BTC_WALLET"
        ] = wallet

        env[
            "BLAKE_SWAP_PARTICIPANT_KEY_FILE"
        ] = str(keyfile)

        env.update(
            self.extra_env
        )

        return env

    def _request(
        self,
        request,
        *,
        extra_env=None,
    ):
        env = self._environment()

        if extra_env:
            env.update(
                extra_env
            )

        p = subprocess.run(
            [
                self.executable,
            ],
            input=(
                json.dumps(
                    request,
                    sort_keys=True,
                )
                + "\n"
            ),
            text=True,
            capture_output=True,
            env=env,
        )

        if p.returncode != 0:
            raise self.h.TestFailure(
                (
                    "external adapter process "
                    f"failed ({p.returncode})\n"
                    f"stdout:\n{p.stdout}\n"
                    f"stderr:\n{p.stderr}"
                )
            )

        try:
            response = json.loads(
                p.stdout
            )
        except json.JSONDecodeError as e:
            raise self.h.TestFailure(
                (
                    "external adapter returned "
                    "non-JSON output:\n"
                    + p.stdout
                )
            ) from e

        return response

    def capabilities(self):
        return self._request({
            "op": "capabilities",
        })

    def command(
        self,
        *args,
        expect_ok=True,
        echo=False,
        extra_env=None,
    ):
        response = self._request(
            {
                "op": "command",
                "role": self.role,
                "args": [
                    str(x)
                    for x in args
                ],
            },
            extra_env=extra_env,
        )

        rc = int(
            response.get(
                "returncode",
                1,
            )
        )

        stdout = (
            response.get(
                "stdout"
            )
            or ""
        )

        stderr = (
            response.get(
                "stderr"
            )
            or ""
        )

        result = SimpleNamespace(
            returncode=rc,
            stdout=stdout,
            stderr=stderr,
        )

        if echo or (
            expect_ok
            and rc != 0
        ):
            if stdout:
                print(
                    stdout,
                    end=(
                        ""
                        if stdout.endswith("\n")
                        else "\n"
                    ),
                )

            if stderr:
                print(
                    stderr,
                    end=(
                        ""
                        if stderr.endswith("\n")
                        else "\n"
                    ),
                    file=sys.stderr,
                )

        if (
            expect_ok
            and rc != 0
        ):
            raise self.h.TestFailure(
                (
                    f"external adapter command failed "
                    f"({rc}): "
                    + " ".join(
                        str(x)
                        for x in args
                    )
                    + "\n"
                    + (
                        stderr.strip()
                        or stdout.strip()
                    )
                )
            )

        return result

    def state(
        self,
        swap_id,
    ):
        response = self._request({
            "op": "state",
            "role": self.role,
            "swap_id": swap_id,
        })

        if not response.get(
            "ok"
        ):
            raise self.h.TestFailure(
                response.get(
                    "error",
                    "external adapter state read failed",
                )
            )

        state = response.get(
            "state"
        )

        if not isinstance(
            state,
            dict,
        ):
            raise self.h.TestFailure(
                "external adapter returned non-object state"
            )

        return state

    def assert_state(
        self,
        swap_id,
        expected,
    ):
        actual = self.state(
            swap_id
        ).get(
            "state"
        )

        self.h.require(
            actual == expected,
            (
                f"{self.role} state "
                f"{actual!r}, "
                f"expected {expected!r}"
            ),
        )


def make_python_alice_external_bob_pair(
    h,
    bob_executable,
):
    """
    First cross-language milestone:

        Python Alice / external Bob
    """
    return SwapImplementationPair(
        h,
        alice=PythonParticipantAdapter(
            h,
            "alice",
        ),
        bob=ExternalJsonParticipantAdapter(
            h,
            "bob",
            bob_executable,
        ),
    )


def make_external_alice_python_bob_pair(
    h,
    alice_executable,
):
    return SwapImplementationPair(
        h,
        alice=ExternalJsonParticipantAdapter(
            h,
            "alice",
            alice_executable,
        ),
        bob=PythonParticipantAdapter(
            h,
            "bob",
        ),
    )


def make_external_pair(
    h,
    alice_executable,
    bob_executable,
):
    return SwapImplementationPair(
        h,
        alice=ExternalJsonParticipantAdapter(
            h,
            "alice",
            alice_executable,
        ),
        bob=ExternalJsonParticipantAdapter(
            h,
            "bob",
            bob_executable,
        ),
    )
