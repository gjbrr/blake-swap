#!/usr/bin/env python3

import base64
import argparse
import hashlib
import json
import secrets
import sys
import time
import uuid
from decimal import Decimal, InvalidOperation, ROUND_HALF_EVEN

from dataclasses import dataclass, asdict
from enum import Enum
from pathlib import Path

#
# The canonical wire schema. The dependency is one-way: this module may
# import swap_wire, and swap_wire must never import this one, so the
# schema stays usable by the independent tools that speak the protocol
# without the engine.
#
import swap_wire

from swap_timeout_policy import (
    TimeoutPolicy,
    build_timeout_plan,
    validate_timeout_plan,
)

from fee_policy import (
    choose_fee_rate_sat_vb,
    coin_per_kvb_to_sat_vb,
    fee_for_vsize,
    enforce_fee_cap,
    ABSOLUTE_CAP_ENV,
    PERCENT_CAP_ENV,
)

import errno
import fcntl
import os
import shlex
import shutil
import subprocess

BASE_SWAP_DIR = Path(
    os.environ.get(
        "BLAKE_SWAP_STATE_DIR",
        Path.home() / ".blake-swaps-v2",
    )
).expanduser()

MESSAGE_DIR = Path(
    os.environ.get(
        "BLAKE_SWAP_MESSAGE_DIR",
        BASE_SWAP_DIR / "messages",
    )
).expanduser()

# Verified Nostr event cache. Created once here, so that path
# computations on read-only reconciliation paths have no side effects.
NOSTR_EVENT_CACHE_DIR = BASE_SWAP_DIR / "nostr-event-cache"

for directory in (
    BASE_SWAP_DIR,
    MESSAGE_DIR,
    NOSTR_EVENT_CACHE_DIR,
):
    directory.mkdir(parents=True, exist_ok=True)

# BLAKE node
BLAKE_CLI = os.environ.get(
    "BLAKE_SWAP_BLAKE_CLI",
    "/home/gary/bitcoin-blake-rc2/build-rc2/bin/bitcoin-cli",
)
BLAKE_DATADIR = os.environ.get(
    "BLAKE_SWAP_BLAKE_DATADIR",
    "/home/gary/blake-swap-regtest",
)

BLAKE_RPC_ARGS = tuple(
    shlex.split(
        os.environ.get(
            "BLAKE_SWAP_BLAKE_RPC_ARGS",
            "",
        )
    )
)

# Normal Bitcoin Core node
BTC_CLI = os.environ.get(
    "BLAKE_SWAP_BTC_CLI",
    "/home/gary/bitcoin-core/build/bin/bitcoin-cli",
)
BTC_DATADIR = os.environ.get(
    "BLAKE_SWAP_BTC_DATADIR",
    "/home/gary/bitcoin-swap-regtest",
)

BTC_RPC_ARGS = tuple(
    shlex.split(
        os.environ.get(
            "BLAKE_SWAP_BTC_RPC_ARGS",
            "",
        )
    )
)

ALICE_WALLET = "alice"
BOB_WALLET = "bob"

PARTICIPANT_ROLE = os.environ.get(
    "BLAKE_SWAP_ROLE",
    "alice",
)

LOCAL_WALLET = os.environ.get(
    "BLAKE_SWAP_CHAIN_WALLET",
    ALICE_WALLET
    if PARTICIPANT_ROLE == "alice"
    else BOB_WALLET,
)

# Backward-compatible chain-specific wallet overrides.
#
# Existing profiles that set only BLAKE_SWAP_CHAIN_WALLET continue
# to behave exactly as before. Mixed-provider deployments may select
# independent signing wallets for BLAKE and Bitcoin.
BLAKE_WALLET = os.environ.get(
    "BLAKE_SWAP_BLAKE_WALLET",
    LOCAL_WALLET,
)

BTC_WALLET = os.environ.get(
    "BLAKE_SWAP_BTC_WALLET",
    LOCAL_WALLET,
)

MARKET_DIRECTION_BLAKE_FOR_BTC = "blake-for-btc"
MARKET_DIRECTION_BTC_FOR_BLAKE = "btc-for-blake"


#
# BLAKE amounts are integers, like Bitcoin amounts, and BLAKE is a
# Bitcoin fork with the same 8 decimal places. The engine stores, compares
# and transmits base units, and only renders a decimal string for humans
# and for the wire. The previous float representation had three
# representations of one quantity (Python float in memory, a decimal
# string on the wire, a tolerance-based comparison) that could disagree,
# and 0.1 has no exact binary form.
#
BLAKE_DECIMALS = 8
BLAKE_BASE_UNITS_PER_COIN = 10 ** BLAKE_DECIMALS
BLAKE_AMOUNT_QUANTUM = Decimal(1) / Decimal(BLAKE_BASE_UNITS_PER_COIN)


def blake_base_units(amount):
    """
    Convert a BLAKE amount to integer base units, exactly.

    Accepts a decimal string, an int, a Decimal or a float. Anything
    finer than 8 decimal places is refused rather than silently rounded:
    the wire carries a decimal string, and a value the engine cannot
    represent would be a different amount on the chain than in the
    message.
    """
    if isinstance(amount, bool):
        raise ValueError("BLAKE amount must be a number, not a boolean")

    try:
        value = Decimal(str(amount))
    except (InvalidOperation, ValueError):
        raise ValueError(
            f"BLAKE amount {amount!r} is not a decimal number"
        ) from None

    if not value.is_finite():
        raise ValueError(f"BLAKE amount {amount!r} must be finite")

    if value <= 0:
        raise ValueError("BLAKE amount must be positive")

    scaled = value * BLAKE_BASE_UNITS_PER_COIN

    if scaled != scaled.to_integral_value():
        raise ValueError(
            f"BLAKE amount {amount!r} is finer than {BLAKE_DECIMALS} "
            "decimal places, which the protocol cannot represent"
        )

    return int(scaled)


def blake_base_units_from_legacy(amount):
    """
    Convert a pre-migration stored amount, which was a float.

    Legacy state was written from a Python float, so it can hold a value
    like 0.30000000000000004 that has no exact base-unit form. Round to
    the nearest base unit and report whether that was necessary, so the
    migration can say what it changed rather than silently discarding
    value.
    """
    value = Decimal(str(amount))

    if not value.is_finite() or value <= 0:
        raise ValueError(
            f"legacy BLAKE amount {amount!r} is not a positive finite number"
        )

    quantized = value.quantize(
        BLAKE_AMOUNT_QUANTUM,
        rounding=ROUND_HALF_EVEN,
    )

    return int(quantized * BLAKE_BASE_UNITS_PER_COIN), quantized != value


def btc_sats_from_coin(value):
    """
    Exact integer satoshis from a node-reported coin amount.

    The node reports output values as decimal strings already quantised
    to one satoshi, so this conversion is lossless; anything finer is
    refused rather than rounded into a different amount.
    """
    if isinstance(value, bool):
        raise ValueError("Bitcoin amount must be a number, not a boolean")

    try:
        amount = Decimal(str(value))
    except (InvalidOperation, ValueError):
        raise ValueError(
            f"Bitcoin amount {value!r} is not a decimal number"
        ) from None

    if not amount.is_finite() or amount < 0:
        raise ValueError(
            f"Bitcoin amount {value!r} must be a non-negative finite number"
        )

    scaled = amount * Decimal(100_000_000)

    if scaled != scaled.to_integral_value():
        raise ValueError(
            f"Bitcoin amount {value!r} is finer than one satoshi"
        )

    return int(scaled)


def blake_amount_decimal(base_units):
    """The exact decimal value of an integer base-unit amount."""
    return Decimal(int(base_units)) / Decimal(BLAKE_BASE_UNITS_PER_COIN)


def swap_blake_base_units(swap):
    """
    The canonical base-unit amount of a swap, or of a swap-like object.

    A stored `Swap` always carries `blake_amount_base_units`. Lightweight
    namespaces built by callers and by tests may carry only the
    whole-coin `blake_amount`, and the message builders accept either, so
    the tolerance lives here at the boundary rather than in storage or in
    every caller.
    """
    units = getattr(swap, "blake_amount_base_units", None)

    if units is not None:
        return int(units)

    return blake_base_units(getattr(swap, "blake_amount"))


def format_blake_amount(base_units):
    """
    The canonical wire and human rendering of a base-unit amount.

    Always a plain decimal string, never exponent notation, because the
    wire schema requires a decimal string.
    """
    value = blake_amount_decimal(base_units)

    rendered = format(value.normalize(), "f")

    return rendered


def market_role_labels(direction):
    """
    The protocol role each market role binds, for one market direction.

    `maker` and `taker` are market roles and never move; the protocol
    roles they bind invert with the direction. Every human-facing view
    derives its labels here, because the original single-direction
    implementation hardcoded `blake-for-btc` and therefore mislabelled
    every `btc-for-blake` negotiation.
    """
    if direction == MARKET_DIRECTION_BTC_FOR_BLAKE:
        return {"maker": "bob", "taker": "alice"}

    if direction == MARKET_DIRECTION_BLAKE_FOR_BTC:
        return {"maker": "alice", "taker": "bob"}

    return {"maker": "unknown", "taker": "unknown"}

MARKET_DIRECTIONS = {
    MARKET_DIRECTION_BLAKE_FOR_BTC,
    MARKET_DIRECTION_BTC_FOR_BLAKE,
}

# Tolerated clock skew for a counterparty-signed event whose created_at
# lies in the future. Applies to every automatic-action freshness test,
# so an event dated far ahead cannot be treated as permanently "fresh".
FUTURE_SKEW_SECONDS = 300


def market_protocol_roles(direction):
    """
    Convert human market direction into the already-tested
    atomic-swap protocol roles.

    Protocol Alice always funds/sells BLAKE.
    Protocol Bob always funds/pays BTC.
    """
    if direction == MARKET_DIRECTION_BLAKE_FOR_BTC:
        return {
            "maker_role": "alice",
            "taker_role": "bob",
        }

    if direction == MARKET_DIRECTION_BTC_FOR_BLAKE:
        return {
            "maker_role": "bob",
            "taker_role": "alice",
        }

    raise ValueError(
        f"Unknown market direction: {direction!r}"
    )


def market_give_want(direction, blake_amount, btc_sats):
    if direction == MARKET_DIRECTION_BLAKE_FOR_BTC:
        return {
            "give": f"{blake_amount:g} BLAKE",
            "want": f"{int(btc_sats):,} sats",
        }

    if direction == MARKET_DIRECTION_BTC_FOR_BLAKE:
        return {
            "give": f"{int(btc_sats):,} sats",
            "want": f"{blake_amount:g} BLAKE",
        }

    raise ValueError(
        f"Unknown market direction: {direction!r}"
    )


class State(str, Enum):
    OFFERED = "OFFERED"

    BLAKE_FUNDED = "BLAKE_FUNDED"
    BTC_FUNDED = "BTC_FUNDED"

    SECRET_REVEALED = "SECRET_REVEALED"

    BTC_CLAIMED = "BTC_CLAIMED"
    BLAKE_CLAIMED = "BLAKE_CLAIMED"

    COMPLETE = "COMPLETE"

    BTC_REFUNDED = "BTC_REFUNDED"
    BLAKE_REFUNDED = "BLAKE_REFUNDED"


ALLOWED_TRANSITIONS = {
    State.OFFERED: {
        State.BLAKE_FUNDED,
    },

    State.BLAKE_FUNDED: {
        State.BTC_FUNDED,
        State.BLAKE_REFUNDED,
    },

    State.BTC_FUNDED: {
        State.SECRET_REVEALED,
        State.BTC_REFUNDED,
    },

    State.SECRET_REVEALED: {
        State.BTC_CLAIMED,
    },

    State.BTC_CLAIMED: {
        State.BLAKE_CLAIMED,
    },

    State.BLAKE_CLAIMED: {
        State.COMPLETE,
    },

    State.BTC_REFUNDED: {
        State.BLAKE_REFUNDED,
    },

    State.COMPLETE: set(),
    State.BLAKE_REFUNDED: set(),
}

#
# Backward corrections that ONLY reorganization recovery may perform.
#
# ALLOWED_TRANSITIONS models forward protocol progress. A reorganization
# can prove that an earlier checkpoint is authoritative again, which
# requires a reverse edge that the forward-only table deliberately does
# not contain. Those edges are enumerated here as data so they are
# auditable and testable, instead of living as ad-hoc state assignments.
#
REORG_TRANSITIONS = {
    State.BLAKE_CLAIMED: {
        State.BTC_CLAIMED,
    },
    State.BLAKE_REFUNDED: {
        State.BTC_REFUNDED,
    },
}


def apply_reorg_transition(swap, target):
    """
    Move durable state backward as part of reorganization recovery.

    Refuses any edge not listed in REORG_TRANSITIONS, so a recovery path
    cannot invent a transition. Normal forward progress must keep using
    Swap.transition().
    """
    current = State(swap.state)
    target = State(target)

    if target not in REORG_TRANSITIONS.get(current, set()):
        raise RuntimeError(
            "SECURITY FAILURE: reorganization transition "
            f"{current.value} -> {target.value} is not permitted"
        )

    swap.state = target.value


@dataclass
class Swap:
    swap_id: str

    state: str

    created_at: int

    # Bob pays this many sats
    btc_sats: int

    secret_hex: str | None
    secret_hash: str

    # Absolute block heights for now.
    blake_timeout: int
    btc_timeout: int

    # Alice sells this many BLAKE base units (an integer, exactly like
    # satoshis).
    #
    # This is the canonical stored representation. The `blake_amount`
    # property below is a derived whole-coin view, kept so display and
    # coordination code can keep speaking in coins; it cannot drift,
    # because it is computed from the integer. `None` only exists while a
    # record written by an older build is being migrated.
    blake_amount_base_units: int | None = None

    # v1.5 timeout-safety metadata.
    # These describe the wall-clock policy used to derive
    # the independent CLTV heights above.
    timeout_policy_version: str | None = None
    btc_timeout_window_seconds: int | None = None
    blake_timeout_window_seconds: int | None = None
    bob_recovery_seconds: int | None = None

    # Local blockchain scan anchors captured before either
    # funding action can legitimately occur. These are local
    # recovery hints, not counterparty-authorized facts.
    blake_scan_start_height: int | None = None
    btc_scan_start_height: int | None = None

    # Funding transactions.
    blake_funding_txid: str | None = None
    blake_funding_vout: int | None = None
    blake_funding_blockhash: str | None = None

    btc_funding_txid: str | None = None
    btc_funding_vout: int | None = None
    btc_funding_blockhash: str | None = None

    blake_alice_pub: str | None = None
    blake_bob_pub: str | None = None

    btc_alice_pub: str | None = None
    btc_bob_pub: str | None = None

    # Nostr transport identities.
    nostr_alice_pub: str | None = None
    nostr_bob_pub: str | None = None

    revealed_secret_hex: str | None = None

    btc_claim_txid: str | None = None
    btc_claim_blockhash: str | None = None

    # Exact signed Bitcoin claim transaction prepared by Alice.
    #
    # This is durable recovery material. If the node loses its
    # mempool and Alice's wallet is unavailable, the engine can
    # independently verify and rebroadcast this exact transaction
    # rather than constructing a competing spend.
    btc_claim_hex: str | None = None

    blake_claim_txid: str | None = None
    blake_claim_blockhash: str | None = None

    # Exact signed BLAKE claim transaction prepared by Bob.
    #
    # This is durable recovery material.  If the node loses its
    # mempool and Bob's wallet is unavailable, the engine can
    # independently verify and rebroadcast this exact transaction
    # rather than constructing a competing spend.
    blake_claim_hex: str | None = None

    # Exact prior BLAKE claim remembered across a reorg rollback.
    # This is not authoritative settlement state. It exists so a
    # wallet-rebroadcast of the previously confirmed claim can be
    # recognized rather than mistaken for an unknown mempool spender.
    blake_reorg_claim_txid: str | None = None

    btc_refund_txid: str | None = None
    blake_refund_txid: str | None = None
    btc_refund_blockhash: str | None = None
    blake_refund_blockhash: str | None = None

    # Exact signed Bitcoin refund transaction prepared by Bob.
    #
    # This is durable recovery material. If the node loses its
    # mempool and Bob's wallet is unavailable, the engine can
    # independently verify and rebroadcast this exact transaction
    # rather than constructing a competing spend.
    btc_refund_hex: str | None = None

    # Exact signed BLAKE refund transaction prepared by Alice.
    #
    # This is durable recovery material. If the node loses its
    # mempool and Alice's wallet is unavailable, the engine can
    # independently verify and rebroadcast this exact transaction
    # rather than constructing a competing spend.
    blake_refund_hex: str | None = None

    # Exact prior BLAKE refund remembered across a reorg rollback.
    # This is a recovery hint, not authoritative settlement state.
    blake_reorg_refund_txid: str | None = None

    @property
    def blake_amount(self):
        """
        The BLAKE amount as a whole-coin float, derived from base units.

        Kept so display and coordination code can keep speaking in coins,
        while the stored and compared value stays an exact integer.
        """
        if self.blake_amount_base_units is None:
            raise ValueError(
                "swap has no BLAKE amount; the record did not migrate"
            )

        return float(
            blake_amount_decimal(self.blake_amount_base_units)
        )

    @blake_amount.setter
    def blake_amount(self, value):
        self.blake_amount_base_units = blake_base_units(value)

    def validate(self):
        # secret_hash must always be a 32-byte SHA256 hash
        try:
            secret_hash_bytes = bytes.fromhex(
                self.secret_hash
            )
        except (TypeError, ValueError):
            raise ValueError(
                "secret_hash must be valid hex"
            )

        if len(secret_hash_bytes) != 32:
            raise ValueError(
                "secret_hash must be 32 bytes"
                )

        # Alice knows S. Bob legitimately does not.
        if self.secret_hex is not None:
            try:
                secret = bytes.fromhex(
                self.secret_hex
                )
            except (TypeError, ValueError):
                raise ValueError(
                    "secret_hex must be valid hex"
                )

            if len(secret) != 32:
                raise ValueError(
                    "secret_hex must be 32 bytes"
                )

            calculated_hash = hashlib.sha256(
                secret
            ).hexdigest()

            if calculated_hash != self.secret_hash:
                raise ValueError(
                    "secret_hex does not match secret_hash"
                )
            secret = bytes.fromhex(self.secret_hex)

            calculated = hashlib.sha256(secret).hexdigest()

            if calculated != self.secret_hash:
                raise ValueError(
                    "Stored secret does not match secret_hash"
                )

        # Bob may permanently retain a secret learned from a
        # Bitcoin claim even if that claim is later reorged out.
        # Knowledge is not settlement authority, but durable knowledge
        # must still be structurally valid and bound to this swap's
        # original hashlock.
        if self.revealed_secret_hex is not None:
            try:
                revealed_secret = bytes.fromhex(
                    self.revealed_secret_hex
                )
            except (TypeError, ValueError):
                raise ValueError(
                    "revealed_secret_hex must be valid hex"
                )

            if len(revealed_secret) != 32:
                raise ValueError(
                    "revealed_secret_hex must be 32 bytes"
                )

            revealed_hash = hashlib.sha256(
                revealed_secret
            ).hexdigest()

            if revealed_hash != self.secret_hash:
                raise ValueError(
                    "revealed_secret_hex does not match secret_hash"
                )

        if self.blake_amount_base_units is None:
            raise ValueError(
                "BLAKE amount is missing; a stored record must carry "
                "blake_amount_base_units"
            )

        if isinstance(self.blake_amount_base_units, bool) or not isinstance(
            self.blake_amount_base_units, int
        ):
            raise ValueError(
                "blake_amount_base_units must be an integer"
            )

        if self.blake_amount_base_units <= 0:
            raise ValueError("BLAKE amount must be positive")

        if self.btc_sats <= 0:
            raise ValueError("BTC amount must be positive")

        def require_32_byte_hex(field_name, value):
            if value is None:
                return

            if not isinstance(value, str):
                raise ValueError(
                    f"{field_name} must be 32-byte hex"
                )

            try:
                raw = bytes.fromhex(value)
            except ValueError:
                raise ValueError(
                    f"{field_name} must be 32-byte hex"
                ) from None

            if len(raw) != 32:
                raise ValueError(
                    f"{field_name} must be 32-byte hex"
                )

        for field_name, value in (
            ("blake_funding_txid", self.blake_funding_txid),
            ("btc_funding_txid", self.btc_funding_txid),
            ("btc_claim_txid", self.btc_claim_txid),
            ("blake_claim_txid", self.blake_claim_txid),
            ("btc_refund_txid", self.btc_refund_txid),
            ("blake_refund_txid", self.blake_refund_txid),
            ("blake_reorg_claim_txid", self.blake_reorg_claim_txid),
            ("blake_reorg_refund_txid", self.blake_reorg_refund_txid),
        ):
            require_32_byte_hex(
                field_name,
                value,
            )

        settlement_hex_fields = (
            (
                "btc_claim_hex",
                self.btc_claim_hex,
                self.btc_claim_txid,
            ),
            (
                "btc_refund_hex",
                self.btc_refund_hex,
                self.btc_refund_txid,
            ),
            (
                "blake_claim_hex",
                self.blake_claim_hex,
                (
                    self.blake_claim_txid
                    or self.blake_reorg_claim_txid
                ),
            ),
            (
                "blake_refund_hex",
                self.blake_refund_hex,
                (
                    self.blake_refund_txid
                    or self.blake_reorg_refund_txid
                ),
            ),
        )

        for field_name, tx_hex, identity_anchor in settlement_hex_fields:
            if tx_hex is None:
                continue

            if not identity_anchor:
                raise ValueError(
                    f"{field_name} requires a settlement txid identity anchor"
                )

            if not isinstance(tx_hex, str) or not tx_hex:
                raise ValueError(
                    f"{field_name} must be valid hex"
                )

            try:
                bytes.fromhex(tx_hex)
            except ValueError:
                raise ValueError(
                    f"{field_name} must be valid hex"
                ) from None

    def transition(self, new_state):
        current = State(self.state)
        target = State(new_state)

        allowed = ALLOWED_TRANSITIONS[current]

        if target not in allowed:
            raise ValueError(
                f"Illegal transition: "
                f"{current.value} -> {target.value}"
            )

        self.state = target.value


def role_dir(role):
    """
    Return this participant's local swap-state directory.

    `role` is the participant's protocol role in a particular
    swap.  It is NOT a filesystem identity.

    BASE_SWAP_DIR is already participant-local via
    BLAKE_SWAP_STATE_DIR, so protocol Alice and protocol Bob
    records owned by this participant belong in the same
    local state tree.
    """
    if role not in ("alice", "bob"):
        raise ValueError(
            f"Unknown swap role: {role}"
        )

    return BASE_SWAP_DIR


def swap_path(swap_id, role="alice"):
    return role_dir(role) / f"{swap_id}.json"


def archived_swap_path(swap_id):
    return BASE_SWAP_DIR / "archive" / f"{swap_id}.json"


def swap_id_is_locally_retired(swap_id):
    return archived_swap_path(swap_id).exists()


def fsync_directory_best_effort(directory):
    """
    Persist a directory entry update, where the platform allows it.

    os.replace() is atomic with respect to concurrent readers, but the
    rename itself is only durable once the containing directory is
    fsynced. Some filesystems and platforms do not support fsync on a
    directory; a failure there says nothing about whether the data
    write succeeded, so it is deliberately not fatal.
    """
    try:
        dir_fd = os.open(
            directory,
            os.O_RDONLY,
        )
    except OSError:
        return

    try:
        os.fsync(dir_fd)
    except OSError:
        pass
    finally:
        os.close(dir_fd)


def atomic_write_bytes(
    path,
    payload,
    *,
    mode=None,
    fsync_directory=False,
):
    """
    Replace `path` with `payload` so a crash never leaves it truncated.

    The bytes go to a sibling temp file which is flushed and fsynced
    before an atomic os.replace() publishes it. Any reader therefore
    sees either the complete previous contents or the complete new
    contents, never a prefix of either, and never a mix.

    `mode` is applied to the temp file before publication, so the
    published file is never briefly more permissive than the caller
    asked for.

    `fsync_directory` additionally persists the rename. Without it the
    directory entry update can be lost by a power failure even though
    the file contents reached the disk, so durable protocol state asks
    for it and disposable caches do not.
    """
    path = Path(path)

    tmp_path = path.with_name(
        path.name + ".tmp"
    )

    with open(tmp_path, "wb") as f:
        f.write(payload)
        f.flush()
        os.fsync(f.fileno())

    if mode is not None:
        os.chmod(
            tmp_path,
            mode,
        )

    os.replace(
        tmp_path,
        path,
    )

    if fsync_directory:
        fsync_directory_best_effort(
            path.parent
        )

    return path


def atomic_write_text(path, text, **kwargs):
    return atomic_write_bytes(
        path,
        text.encode("utf-8"),
        **kwargs,
    )


def atomic_write_json(
    path,
    data,
    *,
    indent=2,
    compact=False,
    mode=None,
    fsync_directory=False,
):
    """
    Write one JSON document atomically.

    `compact` selects the single-line form used for network event
    caches; the default is the indented, key-sorted form used for local
    protocol state. The trailing newline is unconditional, so a
    text-mode diff of two state files is well behaved.
    """
    if compact:
        text = json.dumps(
            data,
            sort_keys=True,
            separators=(",", ":"),
        )
    else:
        text = json.dumps(
            data,
            indent=indent,
            sort_keys=True,
        )

    return atomic_write_text(
        path,
        text + "\n",
        mode=mode,
        fsync_directory=fsync_directory,
    )


def is_swap_id(value):
    """
    Whether `value` is a canonical swap ID.

    The rule itself lives in `swap_wire`, which owns the externally
    visible schema, and is reused rather than restated here: a local scan
    that accepted a name the wire rejects would load a record that can
    never be named in a message.
    """
    return (
        isinstance(value, str)
        and swap_wire.SWAP_ID_RE.fullmatch(value) is not None
    )


def local_swap_paths(directory=None):
    """
    Every canonical swap record in one state directory, sorted by name.

    A file whose name is not a canonical swap ID is not a swap record.
    The state root is a directory the operator also uses for other
    things, and a scan that tried to load those would report them as
    corrupt swaps.
    """
    root = BASE_SWAP_DIR if directory is None else directory

    return [
        path
        for path in sorted(root.glob("*.json"))
        if is_swap_id(path.stem)
    ]


def load_local_swap(path):
    """
    Load one canonical local swap record, or None when it is unreadable.

    Scans use this rather than `load_swap` because a record that cannot
    be read is not a reason to abort a dashboard or a work queue. The
    failure is still reported where it matters: `command_resume_all`
    reads the raw JSON itself and puts every unreadable record in its
    attention list instead of skipping it.
    """
    try:
        return load_swap(
            Path(path).stem,
            role=os.environ.get(
                "BLAKE_SWAP_ROLE",
                "alice",
            ),
        )
    except Exception:
        return None


def save_swap(swap, role="alice"):
    directory = role_dir(role)

    directory.mkdir(
        parents=True,
        exist_ok=True,
    )

    swap.validate()

    path = swap_path(
        swap.swap_id,
        role,
    )

    #
    # Crash-safe state replacement. The swap file is the protocol's
    # authoritative local record, so the rename itself must be durable,
    # not merely the file contents.
    #
    return atomic_write_json(
        path,
        asdict(swap),
        fsync_directory=True,
    )


def migrate_swap_record(data):
    """
    Bring one stored swap record up to the current on-disk shape.

    The only migration so far is BLAKE amounts: records written before
    the change carry `blake_amount` as a JSON number (a Python float),
    and records written after it carry `blake_amount_base_units` as an
    integer. A legacy value is converted exactly where that is possible;
    where the float cannot be represented in base units at all it is
    rounded to the nearest one and the change is reported, because
    silently discarding value would be worse than saying so.

    Reading is non-destructive: the file is rewritten in the new shape
    the next time anything saves it, so a record can be inspected with an
    older build in the meantime.
    """
    migrated = dict(data)

    legacy = migrated.pop("blake_amount", None)

    if migrated.get("blake_amount_base_units") is not None:
        if legacy is not None:
            #
            # Both present: the integer is canonical, but a disagreement
            # means the record was hand-edited or half-written. Trust the
            # integer, and say so rather than picking silently.
            #
            try:
                expected = blake_base_units(legacy)
            except ValueError:
                expected = None

            if expected is not None and expected != int(
                migrated["blake_amount_base_units"]
            ):
                print(
                    "WARNING: stored blake_amount disagrees with "
                    "blake_amount_base_units; using the integer",
                    file=sys.stderr,
                )
    elif legacy is not None:
        base_units, rounded = blake_base_units_from_legacy(legacy)

        migrated["blake_amount_base_units"] = base_units

        if rounded:
            print(
                "WARNING: migrated legacy BLAKE amount "
                f"{legacy!r} to {base_units} base units "
                f"({format_blake_amount(base_units)} BLAKE), which is "
                "the nearest representable value",
                file=sys.stderr,
            )

    return migrated


def load_swap(swap_id, role="alice"):
    path = swap_path(
        swap_id,
        role,
    )

    if not path.exists():
        raise FileNotFoundError(
            f"No swap found: {swap_id}"
        )

    with open(path) as f:
        data = json.load(f)

    swap = Swap(**migrate_swap_record(data))
    swap.validate()

    return swap


def make_swap_id():
    """
    Allocate a protocol swap identifier.

    A swap ID is public coordination data.
    Creating it does NOT create Alice's secret S
    and does not imply either party has funded.
    """
    return uuid.uuid4().hex[:12]


def make_swap(
    blake_amount,
    btc_sats,
    blake_timeout,
    btc_timeout,
    swap_id=None,
):
    secret = secrets.token_bytes(32)

    if swap_id is None:
        swap_id = make_swap_id()

    if (
        not isinstance(swap_id, str)
        or len(swap_id) != 12
    ):
        raise ValueError(
            "swap_id must be 12 hex characters"
        )

    try:
        int(swap_id, 16)
    except ValueError as e:
        raise ValueError(
            "swap_id must be 12 hex characters"
        ) from e

    secret_hash = hashlib.sha256(
        secret
    ).hexdigest()

    swap = Swap(
        swap_id=swap_id,

        state=State.OFFERED.value,

        created_at=int(time.time()),

        blake_amount_base_units=blake_base_units(blake_amount),
        btc_sats=btc_sats,

        secret_hex=secret.hex(),
        secret_hash=secret_hash,

        blake_timeout=blake_timeout,
        btc_timeout=btc_timeout,
    )

    # Alice's local recovery anchors. Funding has not yet
    # occurred, so confirmed funding cannot legitimately
    # precede these heights.
    swap.blake_scan_start_height = int(
        blake_cli("getblockcount")
    )

    swap.btc_scan_start_height = int(
        btc_cli("getblockcount")
    )

    swap.validate()

    return swap


def print_swap(swap):
    print()
    print("ATOMIC SWAP")
    print("===========")
    print("swap id:       ", swap.swap_id)
    print("state:         ", swap.state)

    print()
    print("TRADE")
    print("-----")
    print("BLAKE:         ", swap.blake_amount)
    print("BTC sats:      ", swap.btc_sats)

    print()
    print("HASHLOCK")
    print("--------")
    print("secret:        ", swap.secret_hex)
    print("SHA256:        ", swap.secret_hash)

    print()
    print("TIMEOUTS")
    print("--------")
    print("BTC timeout:   ", swap.btc_timeout)
    print("BLAKE timeout: ", swap.blake_timeout)

    #
    # Deliberately do NOT print a difference between the two CLTV
    # heights. They are absolute heights on independent blockchains and
    # have no ordering relationship; see
    # swap_timeout_policy.validate_timeout_plan. The v1.5 policy records
    # the safety margins as wall-clock seconds, which is the only
    # meaningful comparison.
    #
    if swap.timeout_policy_version == "1.5":
        print(
            "BTC window:    ",
            f"{swap.btc_timeout_window_seconds or 0} s",
        )
        print(
            "Bob recovery:  ",
            f"{swap.bob_recovery_seconds or 0} s",
        )
        print(
            "BLAKE window:  ",
            f"{swap.blake_timeout_window_seconds or 0} s",
        )

    print()
    print("TRANSACTIONS")
    print("------------")
    print(
        "BLAKE funding: ",
        swap.blake_funding_txid or "-"
    )
    print(
        "BTC funding:   ",
        swap.btc_funding_txid or "-"
    )
    print(
        "BTC claim:     ",
        swap.btc_claim_txid or "-"
    )
    print(
        "BLAKE claim:   ",
        swap.blake_claim_txid or "-"
    )

def rpc_timeout_seconds():
    """
    Chain RPC timeout, in seconds.

    A hung node must not hang the engine forever. Override with
    BLAKE_SWAP_RPC_TIMEOUT_SECONDS.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_RPC_TIMEOUT_SECONDS",
        "120",
    )

    try:
        value = float(raw)
    except (TypeError, ValueError):
        raise RuntimeError(
            "BLAKE_SWAP_RPC_TIMEOUT_SECONDS must be a number"
        ) from None

    if value <= 0:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_TIMEOUT_SECONDS must be positive"
        )

    return value


def funding_scan_override_enabled():
    """
    Whether an unanchored (genesis) funding scan is permitted.

    Off by default: a missing anchor means the scan cannot be bounded,
    and an unbounded historical scan is an operational hazard rather
    than a recovery feature. Opt in with
    BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN=1 for a legacy state.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN",
        "",
    ).strip().lower()

    return raw in {"1", "true", "yes", "on"}


def funding_scan_max_blocks():
    """
    Maximum number of blocks a single funding scan may cover.

    Anchors are recorded when a swap is created, so a scan far larger
    than the swap's age means the anchor is wrong, not that the swap is
    long-lived. Override with BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS",
        "100000",
    )

    try:
        value = int(raw)
    except (TypeError, ValueError):
        raise RuntimeError(
            "BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS must be an integer"
        ) from None

    if value < 1:
        raise RuntimeError(
            "BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS must be positive"
        )

    return value


#
# Linux caps a single argv element at MAX_ARG_STRLEN (32 pages, 128 KiB
# on x86-64). Any argument kept below this bound is safe in argv; above
# it, the kernel refuses to exec at all with E2BIG. Transaction hex is
# the only argument here that can grow without bound: a wallet that has
# to consolidate a few hundred inputs produces a funding transaction
# whose hex alone exceeds the cap.
#
ARGV_SAFE_LENGTH = 100_000


#
# RPC methods that may be retried automatically after a timeout or a
# transient connection failure.
#
# This is an allowlist, and that direction is the whole safety argument: a
# method nobody has classified gets no retry, which is the behaviour the
# engine had before retries existed. A denylist would do the opposite --
# a new mutating call would silently become retryable, and a repeated
# `sendtoaddress` funds a swap twice.
#
# Everything here is either a pure read of chain or wallet state, or a
# deterministic local transformation of data the caller already has.
#
RETRYABLE_RPC_METHODS = frozenset({
    # Chain reads.
    "getblockcount",
    "getblockhash",
    "getblock",
    "getrawtransaction",
    "gettxout",
    "getmempoolentry",
    "getmempoolinfo",
    # Fee, descriptor and address queries.
    "estimatesmartfee",
    "getdescriptorinfo",
    "deriveaddresses",
    "getaddressinfo",
    # Wallet reads.
    "gettransaction",
    "getbalances",
    "getwalletinfo",
    "listwallets",
    "listtransactions",
    "listunspent",
    "listdescriptors",
    # Pure construction and decoding: no node state is involved.
    "decoderawtransaction",
    "createrawtransaction",
    "converttopsbt",
    "finalizepsbt",
    "utxoupdatepsbt",
    # Deterministic local signing of a PSBT that was already given to us.
    "walletprocesspsbt",
})


#
# errnos that mean "the node was not reachable right now" rather than "the
# call cannot work". ENOENT and E2BIG are deliberately absent: retrying a
# missing binary or an over-long argument list only wastes the operator's
# time.
#
TRANSIENT_ERRNOS = frozenset(
    errno
    for errno in (
        getattr(errno, name, None)
        for name in (
            "ECONNREFUSED",
            "ECONNRESET",
            "EHOSTUNREACH",
            "ENETUNREACH",
            "ETIMEDOUT",
            "EPIPE",
            "EAGAIN",
        )
    )
    if errno is not None
)


def rpc_max_attempts():
    """
    How many times one read-only RPC may be attempted.

    Default 3 (the first attempt plus two retries). 1 disables retries
    without disabling the timeout. Override with
    BLAKE_SWAP_RPC_MAX_ATTEMPTS.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_RPC_MAX_ATTEMPTS",
        "3",
    ).strip()

    try:
        value = int(raw)
    except ValueError:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_MAX_ATTEMPTS must be an integer"
        ) from None

    if value < 1:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_MAX_ATTEMPTS must be at least 1"
        )

    return value


def rpc_retry_backoff_seconds():
    """
    The delay before the second attempt, doubled for each one after it.

    Override with BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS; 0 retries
    immediately. Bounded by BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS
    (default 30) so a large base cannot turn into an unbounded sleep.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS",
        "1.0",
    ).strip()

    try:
        value = float(raw)
    except ValueError:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS must be a number"
        ) from None

    if value < 0:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS must not be negative"
        )

    cap = 30.0

    raw_cap = os.environ.get(
        "BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS",
        str(cap),
    ).strip()

    try:
        cap = float(raw_cap)
    except ValueError:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS must be a number"
        ) from None

    if cap < 0:
        raise RuntimeError(
            "BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS must not be negative"
        )

    return min(value, cap)


def rpc_method_is_retryable(method):
    """
    Whether `method` may be retried automatically.

    Unknown or unclassified methods are not retryable. See
    RETRYABLE_RPC_METHODS for why the allowlist points that way.
    """
    return isinstance(method, str) and method in RETRYABLE_RPC_METHODS


def _transient_os_error(e):
    """Whether an OSError is the kind a retry could survive."""
    return getattr(e, "errno", None) in TRANSIENT_ERRNOS


def run_chain_cli(label, cmd, stdin_text=None, method=None):
    """
    Run one chain RPC command and decode its JSON result.

    ``stdin_text``, when given, is fed to the CLI's standard input. That
    is how oversized arguments are delivered: bitcoin-cli's ``-stdin``
    mode reads the command and its arguments from stdin, so the value
    never touches argv.

    ``method`` names the RPC being called, and decides whether a timeout
    or a transient connection failure is retried. Only methods in
    RETRYABLE_RPC_METHODS are; a call that can change state is reported
    instead, because after a timeout the engine cannot know whether the
    node acted. The engine is written so that re-running is what resolves
    that: funding records a durable intent first, and recovery finds a
    broadcast that did happen.

    Fails closed with a named diagnostic when the node does not answer in
    time, rather than blocking indefinitely.
    """
    timeout = rpc_timeout_seconds()

    retryable = rpc_method_is_retryable(method)
    attempts = rpc_max_attempts() if retryable else 1

    last_error = None

    for attempt in range(1, attempts + 1):
        try:
            p = subprocess.run(
                cmd,
                text=True,
                capture_output=True,
                timeout=timeout,
                input=stdin_text,
            )
        except subprocess.TimeoutExpired as e:
            last_error = e

            if attempt < attempts:
                delay = rpc_retry_backoff_seconds() * (2 ** (attempt - 1))

                print(
                    f"{label} RPC timed out after {timeout:g}s; "
                    f"retrying in {delay:g}s "
                    f"(attempt {attempt + 1}/{attempts}): {method or cmd[0]}"
                )

                time.sleep(delay)
                continue

            raise RuntimeError(
                f"{label} RPC timed out after {timeout:g}s "
                f"(BLAKE_SWAP_RPC_TIMEOUT_SECONDS) on attempt "
                f"{attempt}/{attempts}: {cmd[0]} {method or ''}".strip()
                + (
                    ""
                    if retryable
                    else ". This call can change state, so it is NOT "
                    "retried: the node may or may not have acted. Check "
                    "the node, then re-run the command -- a durable "
                    "funding intent and funding recovery resolve a "
                    "broadcast that did happen."
                )
            ) from e
        except OSError as e:
            last_error = e

            if _transient_os_error(e) and attempt < attempts:
                delay = rpc_retry_backoff_seconds() * (2 ** (attempt - 1))

                print(
                    f"{label} RPC could not reach the node ({e}); "
                    f"retrying in {delay:g}s "
                    f"(attempt {attempt + 1}/{attempts}): {method or cmd[0]}"
                )

                time.sleep(delay)
                continue

            #
            # E2BIG (argument list too long) and ENOENT both surface here.
            # A raw OSError from deep inside subprocess is not a useful
            # diagnostic for an operator, and the caller's `except
            # Exception` guards would swallow it into a generic failure.
            #
            raise RuntimeError(
                f"{label} RPC could not be executed: {e}"
            ) from e

        if p.returncode != 0:
            raise RuntimeError(p.stderr.strip())

        out = p.stdout.strip()

        try:
            return json.loads(out)
        except json.JSONDecodeError:
            pass

        #
        # A node can put a warning in front of its JSON. Most warnings go
        # to stderr, but not all of them, and the wording and destination
        # are not stable across Bitcoin Core and Knots versions. Decoding
        # from the first structural character keeps a warning-prefixed
        # reply from being handed to a caller as a string when it expects
        # an object.
        #
        start = _first_json_offset(out)

        if start is not None:
            try:
                value, _end = json.JSONDecoder().raw_decode(out, start)
            except ValueError:
                pass
            else:
                return value

        return out

    raise RuntimeError(
        f"{label} RPC failed with no attempt recorded: {last_error}"
    )


def _first_json_offset(text):
    """
    Offset of the first `{` or `[` in `text`, or None.

    Used to find a JSON document that follows a node warning. A bare
    number or quoted string cannot be found this way, and does not need to
    be: `json.loads` already handles those.
    """
    offsets = [
        offset
        for offset in (text.find("{"), text.find("["))
        if offset >= 0
    ]

    return min(offsets) if offsets else None


def _rpc_argument(x):
    """
    Render one Python value as a bitcoin-cli argument.

    The engine passes Python booleans for the JSON booleans the RPC
    expects, and ``str(True)`` is not ``"true"``. Lists and dicts are the
    same problem in a different shape: several RPCs take a JSON array or
    object, and ``str([{"desc": ...}])`` is not JSON, so a structured
    argument is encoded rather than repr'd. The node rejects the repr with
    "Error parsing JSON", which is a confusing way to learn this.
    """
    if x is True:
        return "true"
    if x is False:
        return "false"
    if isinstance(x, (list, dict)):
        return json.dumps(x)
    return str(x)


def _call_chain_cli(label, base_cmd, wallet, args):
    """
    Invoke a chain CLI, choosing argv or stdin transport automatically.

    The two are interchangeable to the node: ``-stdin`` reads the same
    command and arguments from standard input, one per line. Choosing
    per call keeps every RPC call site unchanged and keeps huge
    transaction hexes out of argv.
    """
    rendered = [_rpc_argument(x) for x in args]

    cmd = list(base_cmd)

    if wallet:
        cmd.append(f"-rpcwallet={wallet}")

    oversized = any(len(value) > ARGV_SAFE_LENGTH for value in rendered)

    if oversized:
        cmd.append("-stdin")

        return run_chain_cli(
            label,
            cmd,
            stdin_text="\n".join(rendered) + "\n",
            method=args[0] if args else None,
        )

    cmd.extend(rendered)

    return run_chain_cli(
        label,
        cmd,
        method=args[0] if args else None,
    )


def blake_cli(*args, wallet=None):
    cmd = [
        BLAKE_CLI,
        "-regtest",
    ]

    if BLAKE_DATADIR:
        cmd.append(
            f"-datadir={BLAKE_DATADIR}"
        )

    cmd.extend(BLAKE_RPC_ARGS)

    return _call_chain_cli("BLAKE", cmd, wallet, args)

def btc_cli(*args, wallet=None):
    cmd = [
        BTC_CLI,
    ]

    if BTC_DATADIR:
        cmd.append(
            f"-datadir={BTC_DATADIR}"
        )

    cmd.extend(BTC_RPC_ARGS)

    return _call_chain_cli("Bitcoin", cmd, wallet, args)

def chain_dispatch(chain):
    """
    Everything the engine needs that differs between the two HTLC chains.

    The chains differ only in prefix, entry point, contract builder and
    verifier, so this is derived rather than repeated as a pair of if/elif
    branches at every call site: a per-chain fact added here cannot be
    wired up for one chain and forgotten for the other.

    Returns a dict with `name`, `cli`, `wallet`, `tx_field`, `vout_field`,
    `block_field`, `verifier`, `descriptor` and `derive_address`.
    Function references are resolved at call time rather than stored in a
    module-level table, so replacing `blake_cli`/`btc_cli` on the module
    keeps working: the transport and fail-closed suites rely on exactly
    that.
    """
    if chain == "btc":
        return {
            "name": "Bitcoin",
            "cli": btc_cli,
            "wallet": BTC_WALLET,
            "tx_field": "btc_funding_txid",
            "vout_field": "btc_funding_vout",
            "block_field": "btc_funding_blockhash",
            "verifier": verify_btc_contract,
            "descriptor": get_btc_contract_descriptor,
            "derive_address": derive_btc_contract_address,
        }

    if chain == "blake":
        return {
            "name": "BLAKE",
            "cli": blake_cli,
            "wallet": BLAKE_WALLET,
            "tx_field": "blake_funding_txid",
            "vout_field": "blake_funding_vout",
            "block_field": "blake_funding_blockhash",
            "verifier": verify_blake_contract,
            "descriptor": get_blake_contract_descriptor,
            "derive_address": derive_blake_contract_address,
        }

    raise ValueError(f"Unknown HTLC chain: {chain}")


def htlc_descriptor_body(swap, chain, private_keys=None):
    """
    The swap's HTLC descriptor, optionally carrying private keys.

    Both branches are of the form:

        wsh(or_i(and_v(v:pk(CLAIMER),sha256(H)),
                 and_v(v:after(T),pk(REFUNDER))))

    `private_keys` maps a compressed public key to the WIF (or extended
    private key) that controls it, so a participant can build the
    descriptor *their* wallet can sign with while the counterparty's key
    stays public.

    Substituting a key changes the descriptor string, and therefore its
    checksum: `getdescriptorinfo` must be run again on the substituted
    form. That is why this returns the body rather than a checksummed
    descriptor.

    The engine funds whatever the node compiles from this body, so the
    body is the single source of truth for the HTLC's script.
    """

    keys = private_keys or {}

    def render(pubkey):
        return keys.get(pubkey, pubkey)

    if chain == "blake":
        return (
            "wsh(or_i("
            f"and_v(v:pk({render(swap.blake_bob_pub)}),"
            f"sha256({swap.secret_hash})),"
            f"and_v(v:after({swap.blake_timeout}),"
            f"pk({render(swap.blake_alice_pub)}))"
            "))"
        )

    if chain == "btc":
        return (
            "wsh(or_i("
            f"and_v(v:pk({render(swap.btc_alice_pub)}),"
            f"sha256({swap.secret_hash})),"
            f"and_v(v:after({swap.btc_timeout}),"
            f"pk({render(swap.btc_bob_pub)}))"
            "))"
        )

    raise ValueError(f"Unknown HTLC chain: {chain}")


def descriptor_checksum(body, cli):
    """The 8-character checksum `importdescriptors` requires."""

    info = cli(
        "getdescriptorinfo",
        body,
    )

    checksum = info.get("checksum")

    if not checksum:
        raise RuntimeError(
            "the node returned no descriptor checksum for the HTLC"
        )

    return checksum


def htlc_contract(swap, chain):
    """
    The node's canonical public descriptor for this swap's HTLC, with its
    derived address.

    The descriptor comes back from `getdescriptorinfo` already
    checksummed, which is the form `importdescriptors` requires and the
    reason a caller must not checksum it again.
    """
    fields = chain_dispatch(chain)

    descriptor = fields["descriptor"](swap)

    return descriptor, fields["derive_address"](descriptor)


def watch_only_wallet_name(chain):
    """
    The dedicated wallet that holds this participant's watch-only HTLCs.

    A wallet of its own rather than the participant's, for the two reasons
    Core enforces: a wallet with private keys enabled refuses to import a
    watch-only descriptor at all ("Cannot import descriptor without
    private keys to a wallet with private keys enabled"), and the
    participant wallet's keys are exactly what a key loss destroys -- a
    watch-only copy kept inside it would be lost with them.

    The name is per chain but the wallet is per node, because the two
    chains are separate datadirs: a default of `<wallet>-watch` on each
    node is two wallets, not one.

    Override with BLAKE_SWAP_BLAKE_WATCH_WALLET / BLAKE_SWAP_BTC_WATCH_WALLET.
    """
    if chain == "btc":
        return os.environ.get(
            "BLAKE_SWAP_BTC_WATCH_WALLET",
            f"{BTC_WALLET}-watch",
        )

    if chain == "blake":
        return os.environ.get(
            "BLAKE_SWAP_BLAKE_WATCH_WALLET",
            f"{BLAKE_WALLET}-watch",
        )

    raise ValueError(f"Unknown HTLC chain: {chain}")


def ensure_watch_only_wallet(chain):
    """
    Return `(cli, wallet)` for the watch-only wallet, creating it if needed.

    `disable_private_keys` and `blank` are the arguments that matter: the
    wallet holds no key of its own and generates none, so it can never be
    confused with the signing wallet, and importing the public HTLC
    descriptor is the only way it learns about an output.
    """
    cli = chain_dispatch(chain)["cli"]

    wallet = watch_only_wallet_name(chain)

    loaded = cli("listwallets")

    if isinstance(loaded, list) and wallet in loaded:
        return cli, wallet

    try:
        cli("loadwallet", wallet)
    except Exception:
        cli(
            "createwallet",
            wallet,
            True,     # disable_private_keys
            True,     # blank
            "",       # passphrase
            False,    # avoid_reuse
            True,     # descriptors
        )

    return cli, wallet


def import_watch_only_htlc(swap, chain):
    """
    Import this swap's public HTLC descriptor into the watch-only wallet.

    Idempotent: the descriptor is a pure function of the swap terms, so a
    second call imports an identical string and the node reports that as a
    warning rather than an error.

    `timestamp` is `"now"` on purpose. The descriptor is imported before
    the HTLC is funded, and a rescan of the whole chain to find an output
    that does not exist yet would be wasted work. The point of the import
    is that the output is *solvable* the moment it appears, which does not
    need the wallet to have seen the chain before.

    Returns a status dict rather than raising: the caller decides whether
    a node that cannot keep a watch-only wallet should stop a funding
    operation.
    """
    status = {
        "chain": chain,
        "descriptor": None,
        "address": None,
        "wallet": None,
        "imported": False,
        "reason": None,
    }

    try:
        descriptor, address = htlc_contract(swap, chain)
        cli, wallet = ensure_watch_only_wallet(chain)

        status["descriptor"] = descriptor
        status["address"] = address
        status["wallet"] = wallet

        result = cli(
            "importdescriptors",
            [
                {
                    "desc": descriptor,
                    "timestamp": "now",
                    "active": False,
                    "internal": False,
                    "label": f"htlc-{swap.swap_id}-{chain}",
                }
            ],
            wallet=wallet,
        )

        if not isinstance(result, list) or not result:
            status["reason"] = "the node returned no import result"
            return status

        failed = [
            item.get("error", {}).get("message", "unknown error")
            for item in result
            if isinstance(item, dict) and not item.get("success")
        ]

        if failed:
            status["reason"] = "; ".join(failed)
            return status

        status["imported"] = True
        return status

    except Exception as e:
        status["reason"] = f"{type(e).__name__}: {e}"
        return status


def htlc_solvability(swap, chain):
    """
    Whether a loaded wallet can currently solve this swap's HTLC output.

    Asks each wallet that might: the participant wallet, which can spend
    it if it still holds the keys, and the watch-only wallet, which
    imported the public descriptor and can therefore at least see and
    confirm the output.

    Returns a status dict; a chain that cannot be reached reports
    `solvable: None` with a reason rather than raising, because this is
    reported in a read-only dashboard.
    """
    status = {
        "chain": chain,
        "address": None,
        "wallet": None,
        "solvable": None,
        "reason": None,
    }

    fields = chain_dispatch(chain)
    cli = fields["cli"]

    try:
        _descriptor, address = htlc_contract(swap, chain)
    except Exception as e:
        status["reason"] = f"{type(e).__name__}: {e}"
        return status

    status["address"] = address

    for wallet in (
        fields["wallet"],
        watch_only_wallet_name(chain),
    ):
        try:
            loaded = cli("listwallets")
        except Exception as e:
            status["reason"] = (
                f"wallet service unavailable: {type(e).__name__}: {e}"
            )
            return status

        if not isinstance(loaded, list) or wallet not in loaded:
            continue

        try:
            info = cli(
                "getaddressinfo",
                address,
                wallet=wallet,
            )
        except Exception:
            continue

        if isinstance(info, dict) and info.get("solvable"):
            status["wallet"] = wallet
            status["solvable"] = True
            return status

    status["solvable"] = False
    status["reason"] = (
        "no loaded wallet can solve this HTLC; import the descriptor with "
        "`watch-htlc` (see docs/KEY_RECOVERY.md)"
    )
    return status


def report_watch_only_import(swap, chain):
    """
    Import the HTLC into the watch-only wallet before funds move, loudly.

    A failure here does not stop funding: the private keys are present at
    this moment, so the swap is spendable, and refusing to fund because a
    *watch-only* copy could not be recorded would break the case it exists
    to protect. What it must not do is pass unmentioned, so the outcome is
    printed either way.
    """
    status = import_watch_only_htlc(swap, chain)

    print()
    print(f"WATCH-ONLY {chain.upper()} HTLC")
    print("=" * (12 + len(chain)))

    if status["imported"]:
        print("wallet:  ", status["wallet"])
        print("address: ", status["address"])
        print(
            "PASS: descriptor imported; this HTLC stays solvable "
            "without the signing wallet"
        )
        return status

    print("wallet:  ", status["wallet"] or "(none)")
    print("address: ", status["address"] or "-")
    print(
        "WARNING: the HTLC was NOT recorded in a watch-only wallet: "
        f"{status['reason']}"
    )
    print(
        "         Funding may continue, but a later loss of the signing "
        "wallet would leave this swap unspendable."
    )
    return status


def get_blake_contract_descriptor(swap):
    if not swap.blake_alice_pub:
        raise ValueError(
            "Swap has no BLAKE Alice pubkey"
        )

    if not swap.blake_bob_pub:
        raise ValueError(
            "Swap has no BLAKE Bob pubkey"
        )

    body = htlc_descriptor_body(swap, "blake")

    info = blake_cli(
        "getdescriptorinfo",
        body,
    )

    return info["descriptor"
                ]

def derive_blake_contract_address(desc):
    result = blake_cli(
        "deriveaddresses",
        desc,
    )

    return result[0]

def get_btc_contract_descriptor(swap):
    if not swap.btc_alice_pub:
        raise ValueError("Swap has no Bitcoin Alice pubkey")

    if not swap.btc_bob_pub:
        raise ValueError("Swap has no Bitcoin Bob pubkey")

    body = htlc_descriptor_body(swap, "btc")

    info = btc_cli(
        "getdescriptorinfo",
        body,
    )

    return info["descriptor"]


def derive_btc_contract_address(desc):
    result = btc_cli(
        "deriveaddresses",
        desc,
    )

    return result[0]

def extract_secret_from_btc_claim(swap):
    if not swap.btc_claim_txid:
        raise ValueError(
            "Swap has no Bitcoin claim txid"
        )

    if not swap.btc_claim_blockhash:
        raise ValueError(
            "Swap has no Bitcoin claim blockhash"
        )

    if not swap.btc_funding_txid:
        raise ValueError(
            "Swap has no Bitcoin funding txid"
        )

    if swap.btc_funding_vout is None:
        raise ValueError(
            "Swap has no Bitcoin funding vout"
        )

    tx = btc_cli(
        "getrawtransaction",
        swap.btc_claim_txid,
        True,
        swap.btc_claim_blockhash,
    )

    # Find the input that actually spends
    # Bob's Bitcoin HTLC.
    htlc_input = None

    for vin in tx["vin"]:
        if (
            vin.get("txid") == swap.btc_funding_txid
            and vin.get("vout") == swap.btc_funding_vout
        ):
            htlc_input = vin
            break

    if htlc_input is None:
        raise RuntimeError(
            "Bitcoin claim does not spend "
            "the expected HTLC output"
        )

    witness = htlc_input.get(
        "txinwitness",
        [],
    )

    for item in witness:
        # Our secret is exactly 32 bytes = 64 hex chars.
        if len(item) != 64:
            continue

        try:
            candidate = bytes.fromhex(item)
        except ValueError:
            continue

        candidate_hash = hashlib.sha256(
            candidate
        ).hexdigest()

        if candidate_hash == swap.secret_hash:
            return item

    raise RuntimeError(
        "No witness item matched the swap SHA256 hash"
    )


def classify_htlc_spend(
    tx,
    funding_txid,
    funding_vout,
    secret_hash,
):
    """
    Classify a confirmed spend of our exact HTLC outpoint.

    The contract is a two-branch Miniscript HTLC:

        wsh(or_i(and_v(v:pk(CLAIMER), sha256(H)),
                 and_v(v:after(T), pk(REFUNDER))))

    A P2WSH spend of it has one of exactly two shapes, with the or_i
    selector immediately before the witness script:

        claim:   <preimage(32B)> <signature> <01> <witnessScript>
        refund:  <signature> <0-or-empty> <witnessScript>

    The branch is therefore decided from the witness STRUCTURE, not
    merely from the absence of a preimage. A witness matching neither
    shape returns "unknown", and callers must treat that as a
    mismatch and fail closed rather than assuming a refund.

    The spend itself has already been accepted by consensus, and the
    caller has already tied it to our exact HTLC outpoint, so the script
    bytes need not be re-derived here.
    """
    htlc_input = None

    for vin in tx.get("vin", []):
        if (
            vin.get("txid") == funding_txid
            and vin.get("vout") == funding_vout
        ):
            htlc_input = vin
            break

    if htlc_input is None:
        raise RuntimeError(
            "Internal error: candidate transaction does not "
            "spend the expected HTLC outpoint"
        )

    witness = htlc_input.get(
        "txinwitness",
        [],
    )

    if not witness:
        raise RuntimeError(
            "SECURITY FAILURE: confirmed HTLC spend "
            "has no witness"
        )

    #
    # Need at least: <arg> <selector> <witnessScript>.
    #
    if len(witness) < 3:
        return "unknown"

    selector = witness[-2]

    preimage_present = False

    for item in witness:
        if len(item) != 64:
            continue

        try:
            candidate = bytes.fromhex(item)
        except ValueError:
            continue

        if hashlib.sha256(candidate).hexdigest() == secret_hash:
            preimage_present = True
            break

    if selector == "01":
        # OP_IF taken: the claim branch, which must carry the preimage.
        if preimage_present:
            return "claim"

        return "unknown"

    if selector in ("", "00"):
        # OP_ELSE taken: the refund branch, which must not carry the
        # preimage.
        if preimage_present:
            return "unknown"

        return "refund"

    return "unknown"


def verify_announced_settlement(
    swap,
    chain,
    txid,
    blockhash,
    expected_branch,
):
    """
    Independently verify an untrusted settlement announcement
    before allowing it to advance durable protocol state.

    The announced transaction must:

      * exist in the announced block,
      * remain confirmed on the active chain,
      * spend this swap's exact HTLC outpoint, and
      * use the announced claim/refund branch.

    Nothing from the peer announcement is persisted by this helper.
    """
    fields = chain_dispatch(chain)

    cli = fields["cli"]
    chain_name = fields["name"]

    funding_txid = getattr(
        swap,
        fields["tx_field"],
    )
    funding_vout = getattr(
        swap,
        fields["vout_field"],
    )

    if not funding_txid:
        raise RuntimeError(
            f"SECURITY FAILURE: swap has no "
            f"{chain_name} funding txid"
        )

    if funding_vout is None:
        raise RuntimeError(
            f"SECURITY FAILURE: swap has no "
            f"{chain_name} funding vout"
        )

    tx = cli(
        "getrawtransaction",
        txid,
        True,
        blockhash,
    )

    if tx.get("in_active_chain") is False:
        raise RuntimeError(
            "SECURITY FAILURE: announced "
            f"{chain_name} {expected_branch} transaction "
            "is not on the active chain"
        )

    confirmations = int(
        tx.get("confirmations", 0) or 0
    )

    if confirmations < 1:
        raise RuntimeError(
            "SECURITY FAILURE: announced "
            f"{chain_name} {expected_branch} transaction "
            "is not confirmed on the active chain"
        )

    spends_htlc = any(
        vin.get("txid") == funding_txid
        and int(vin.get("vout", -1)) == int(funding_vout)
        for vin in tx.get("vin", [])
    )

    if not spends_htlc:
        raise RuntimeError(
            "SECURITY FAILURE: announced "
            f"{chain_name} {expected_branch} transaction "
            "does not spend this swap's HTLC"
        )

    actual_branch = classify_htlc_spend(
        tx,
        funding_txid,
        funding_vout,
        swap.secret_hash,
    )

    if actual_branch != expected_branch:
        raise RuntimeError(
            "SECURITY FAILURE: announced "
            f"{chain_name} settlement branch mismatch: "
            f"expected {expected_branch}, "
            f"observed {actual_branch}"
        )

    return tx


def find_confirmed_htlc_spend(
    swap,
    chain,
):
    """
    Find a confirmed transaction spending this swap's HTLC.

    If the outpoint is unavailable but no confirmed spend can
    yet be found, fail closed rather than risk rebroadcasting
    a competing transaction.
    """
    fields = chain_dispatch(chain)

    cli = fields["cli"]
    chain_name = fields["name"]

    funding_txid = getattr(swap, fields["tx_field"])
    funding_vout = getattr(swap, fields["vout_field"])
    funding_blockhash = getattr(swap, fields["block_field"])

    if not funding_txid:
        raise ValueError(
            f"Swap has no {chain_name} funding txid"
        )

    if funding_vout is None:
        raise ValueError(
            f"Swap has no {chain_name} funding vout"
        )

    #
    # A funding blockhash is useful as a precise historical scan
    # starting point, but it is not required to identify this HTLC.
    # The durable funding txid:vout is the authoritative outpoint.
    #
    # Production state may persist the funding outpoint while pending
    # and then miss the later funding blockhash before the output is
    # spent. Recovery must still be possible from confirmed spender
    # evidence.
    #

    #
    # Fast path: if durable state already records the exact confirmed
    # settlement transaction, verify that transaction directly rather
    # than rescanning every block since HTLC funding.
    #
    # The persisted txid is NOT trusted blindly.  We independently:
    #
    #   * fetch the exact transaction from its recorded block,
    #   * require it to remain on the active chain,
    #   * require at least one confirmation,
    #   * prove that it spends this swap's HTLC outpoint, and
    #   * classify the actual witness/script branch.
    #
    # Historical block scanning remains the recovery fallback when
    # durable state does not know the spender.
    #
    known_spenders = []

    if chain == "btc":
        known_spenders = [
            (
                "claim",
                swap.btc_claim_txid,
                swap.btc_claim_blockhash,
            ),
            (
                "refund",
                swap.btc_refund_txid,
                swap.btc_refund_blockhash,
            ),
        ]

    elif chain == "blake":
        known_spenders = [
            (
                "claim",
                swap.blake_claim_txid,
                swap.blake_claim_blockhash,
            ),
            (
                "refund",
                swap.blake_refund_txid,
                swap.blake_refund_blockhash,
            ),
        ]

    populated = [
        (expected_branch, txid, blockhash)
        for expected_branch, txid, blockhash in known_spenders
        if txid is not None
    ]

    if len(populated) > 1:
        raise RuntimeError(
            "SECURITY FAILURE: durable state records multiple "
            f"{chain_name} HTLC settlement transactions"
        )

    if populated:
        expected_branch, settlement_txid, settlement_blockhash = (
            populated[0]
        )

        if not settlement_blockhash:
            #
            # Production-style settlement persists the broadcast txid
            # immediately, while deliberately leaving protocol state
            # unchanged until active-chain confirmation.
            #
            # A known txid with no blockhash is therefore a normal pending
            # condition, not corruption.  Independently verify the exact
            # transaction before refusing any rebroadcast.
            #
            try:
                tx = cli(
                    "getrawtransaction",
                    settlement_txid,
                    True,
                )
            except Exception:
                #
                # Nodes without txindex cannot necessarily retrieve a
                # wallet-created transaction through getrawtransaction
                # once it is no longer exposed by the mempool lookup.
                #
                # This participant created the recorded settlement, so
                # recover it from the local wallet and independently
                # decode the raw transaction instead.
                #
                wallet = (
                    BTC_WALLET
                    if chain == "btc"
                    else BLAKE_WALLET
                )

                try:
                    wallet_tx = cli(
                        "gettransaction",
                        settlement_txid,
                        wallet=wallet,
                    )

                    tx = cli(
                        "decoderawtransaction",
                        wallet_tx["hex"],
                    )

                    tx["confirmations"] = int(
                        wallet_tx.get("confirmations", 0) or 0
                    )

                    if wallet_tx.get("blockhash"):
                        tx["blockhash"] = wallet_tx["blockhash"]

                except Exception as e:
                    raise RuntimeError(
                        "SECURITY FAILURE: durable state records "
                        f"{chain_name} {expected_branch} txid "
                        "but the transaction cannot be independently verified"
                    ) from e

            spends_htlc = any(
                vin.get("txid") == funding_txid
                and int(vin.get("vout", -1)) == int(funding_vout)
                for vin in tx.get("vin", [])
            )

            if not spends_htlc:
                raise RuntimeError(
                    "SECURITY FAILURE: recorded "
                    f"{chain_name} {expected_branch} transaction "
                    "does not spend this swap's HTLC"
                )

            actual_branch = classify_htlc_spend(
                tx,
                funding_txid,
                funding_vout,
                swap.secret_hash,
            )

            if actual_branch != expected_branch:
                raise RuntimeError(
                    "SECURITY FAILURE: recorded "
                    f"{chain_name} settlement branch mismatch: "
                    f"expected {expected_branch}, observed {actual_branch}"
                )

            confirmations = int(
                tx.get("confirmations", 0) or 0
            )

            if confirmations < 1:
                raise RuntimeError(
                    "PENDING: recorded "
                    f"{chain_name} {expected_branch} transaction "
                    "is unconfirmed; refusing to rebroadcast"
                )

            settlement_blockhash = tx.get("blockhash")

            if not settlement_blockhash:
                raise RuntimeError(
                    "SECURITY FAILURE: confirmed recorded "
                    f"{chain_name} {expected_branch} transaction "
                    "has no blockhash"
                )

        else:
            tx = cli(
                "getrawtransaction",
                settlement_txid,
                True,
                settlement_blockhash,
            )

        if tx.get("in_active_chain") is False:
            raise RuntimeError(
                "SECURITY FAILURE: recorded "
                f"{chain_name} {expected_branch} transaction "
                "is no longer on the active chain"
            )

        confirmations = int(
            tx.get("confirmations", 0) or 0
        )

        if confirmations < 1:
            raise RuntimeError(
                "SECURITY FAILURE: recorded "
                f"{chain_name} {expected_branch} transaction "
                "is not confirmed on the active chain"
            )

        spends_htlc = any(
            vin.get("txid") == funding_txid
            and vin.get("vout") == funding_vout
            for vin in tx.get("vin", [])
        )

        if not spends_htlc:
            raise RuntimeError(
                "SECURITY FAILURE: recorded "
                f"{chain_name} {expected_branch} transaction "
                "does not spend this swap's HTLC"
            )

        actual_branch = classify_htlc_spend(
            tx,
            funding_txid,
            funding_vout,
            swap.secret_hash,
        )

        if actual_branch != expected_branch:
            raise RuntimeError(
                "SECURITY FAILURE: recorded "
                f"{chain_name} settlement branch mismatch: "
                f"expected {expected_branch}, observed {actual_branch}"
            )

        return {
            "txid": settlement_txid,
            "blockhash": settlement_blockhash,
            "branch": actual_branch,
        }

    if funding_blockhash:
        funding_block = cli(
            "getblock",
            funding_blockhash,
            1,
        )

        start_height = int(
            funding_block["height"]
        )

    else:
        anchor_name = (
            "btc_scan_start_height"
            if chain == "btc"
            else "blake_scan_start_height"
        )

        start_height = getattr(
            swap,
            anchor_name,
            None,
        )

        if start_height is None:
            raise RuntimeError(
                "SECURITY FAILURE: "
                f"{chain_name} funding blockhash is unknown and "
                f"{anchor_name} is unavailable; refusing an "
                "unbounded spender scan"
            )

        start_height = int(start_height)

    tip_height = int(
        cli("getblockcount")
    )

    for height in range(
        start_height,
        tip_height + 1,
    ):
        blockhash = cli(
            "getblockhash",
            height,
        )

        block = cli(
            "getblock",
            blockhash,
            2,
        )

        for tx in block.get("tx", []):
            spends_htlc = False

            for vin in tx.get("vin", []):
                if (
                    vin.get("txid") == funding_txid
                    and vin.get("vout") == funding_vout
                ):
                    spends_htlc = True
                    break

            if not spends_htlc:
                continue

            branch = classify_htlc_spend(
                tx,
                funding_txid,
                funding_vout,
                swap.secret_hash,
            )

            return {
                "txid": tx["txid"],
                "blockhash": blockhash,
                "branch": branch,
            }

    # No confirmed spend was found.
    #
    # gettxout includes mempool effects by default. Therefore
    # None here can mean a transaction in the mempool is already
    # spending the HTLC. In that situation we MUST NOT create
    # another irreversible action.
    utxo = cli(
        "gettxout",
        funding_txid,
        funding_vout,
    )

    if utxo is None:
        raise RuntimeError(
            f"{chain_name} HTLC outpoint is unavailable, "
            "but no confirmed spender was found. "
            "It may already have an unconfirmed spender. "
            "Refusing to rebroadcast."
        )

    return None

def transaction_in_mempool(cli, txid):
    """
    Return the node's mempool entry for txid, or None when the node does
    not have it in its mempool.

    Callers must branch on presence/absence rather than on the wording
    of an error message, which varies by node version and locale. A node
    that is genuinely unreachable fails loudly on the next RPC call, so
    treating an error here as "not in mempool" does not mask an outage.
    """
    if not txid:
        return None

    try:
        return cli(
            "getmempoolentry",
            txid,
        )
    except RuntimeError:
        return None


def require_recovered_branch(
    swap,
    chain,
    expected_branch,
    recorded_txid_override=None,
):
    fields = chain_dispatch(chain)

    cli = fields["cli"]

    if expected_branch not in ("claim", "refund"):
        raise ValueError(
            f"Unknown {fields['name']} settlement branch: "
            f"{expected_branch}"
        )

    if recorded_txid_override is not None:
        recorded_txid = recorded_txid_override
    else:
        recorded_txid = getattr(
            swap,
            f"{chain}_{expected_branch}_txid",
        )

    funding_txid = getattr(swap, fields["tx_field"])
    funding_vout = getattr(swap, fields["vout_field"])

    #
    # If this participant previously broadcast an exact settlement
    # transaction, recognize that exact tx while it remains pending.
    #
    # This does NOT weaken the fail-closed behavior for an unknown
    # mempool spender: without a recorded txid we still fall through
    # to find_confirmed_htlc_spend(), whose gettxout check refuses to
    # rebroadcast against an unavailable outpoint.
    #
    if recorded_txid:
        #
        # Branch on the *presence* of a mempool entry rather than on the
        # wording of an error message: node text varies by version and
        # locale. A node that is genuinely unreachable fails loudly on
        # the next RPC call, so this does not mask an outage.
        #
        entry = transaction_in_mempool(cli, recorded_txid)

        if entry is not None:
            tx = cli(
                "getrawtransaction",
                recorded_txid,
                True,
            )

            actual = classify_htlc_spend(
                tx,
                funding_txid,
                funding_vout,
                swap.secret_hash,
            )

            if actual != expected_branch:
                raise RuntimeError(
                    "SECURITY FAILURE: recorded pending settlement "
                    f"uses {actual} branch; expected {expected_branch}"
                )

            return "pending"

        #
        # A node restart can legitimately lose its mempool.  If durable
        # state also contains the exact signed transaction, verify that
        # transaction independently before rebroadcasting it.
        #
        # Settlement branches may store this recovery material.
        # getattr keeps older swap records and branches without durable
        # signed hex backward-compatible.
        #
        recorded_hex = getattr(
            swap,
            f"{chain}_{expected_branch}_hex",
            None,
        )

        if recorded_hex:
            decoded = cli(
                "decoderawtransaction",
                recorded_hex,
            )

            decoded_txid = decoded.get("txid")

            if decoded_txid != recorded_txid:
                raise RuntimeError(
                    "SECURITY FAILURE: durable signed settlement "
                    "transaction does not match recorded txid"
                )

            actual = classify_htlc_spend(
                decoded,
                funding_txid,
                funding_vout,
                swap.secret_hash,
            )

            if actual != expected_branch:
                raise RuntimeError(
                    "SECURITY FAILURE: durable signed settlement "
                    f"uses {actual} branch; expected {expected_branch}"
                )

            #
            # include_mempool=True is intentional.  If some other
            # unconfirmed transaction is already spending the HTLC,
            # gettxout returns None and we MUST NOT rebroadcast.
            #
            utxo = cli(
                "gettxout",
                funding_txid,
                funding_vout,
            )

            if utxo is not None:
                try:
                    rebroadcast_txid = cli(
                        "sendrawtransaction",
                        recorded_hex,
                    )
                except RuntimeError as broadcast_error:
                    #
                    # The exact transaction may already be confirmed.
                    # Do not infer that from node error text or numeric
                    # codes.  Ask the existing active-chain recovery
                    # logic to prove it independently.
                    #
                    found = find_confirmed_htlc_spend(
                        swap,
                        chain,
                    )

                    if found is None:
                        raise broadcast_error

                    actual = found["branch"]

                    if actual != expected_branch:
                        raise RuntimeError(
                            "SECURITY FAILURE: HTLC was already "
                            f"spent by the {actual} branch; "
                            f"expected {expected_branch}"
                        )

                    if found["txid"] != recorded_txid:
                        raise RuntimeError(
                            "SECURITY FAILURE: confirmed settlement "
                            "txid differs from durable signed "
                            "transaction"
                        )

                    return found

                if rebroadcast_txid != recorded_txid:
                    raise RuntimeError(
                        "SECURITY FAILURE: exact settlement "
                        "rebroadcast returned a different txid"
                    )

                print()
                print(
                    f"{fields['name']} "
                    f"{expected_branch.upper()} "
                    "REBROADCAST FROM DURABLE STATE"
                )
                print("=" * 46)
                print("txid:  ", recorded_txid)
                print("block:  pending")
                print("state: ", swap.state)
                print()
                print(
                    "PASS: exact stored signed transaction "
                    "was rebroadcast"
                )

                return "pending"

    found = find_confirmed_htlc_spend(
        swap,
        chain,
    )

    if found is None:
        return None

    actual = found["branch"]

    if actual != expected_branch:
        raise RuntimeError(
            "SECURITY FAILURE: HTLC was already spent by "
            f"the {actual} branch; expected {expected_branch}"
        )

    if (
        recorded_txid
        and found["txid"] != recorded_txid
    ):
        raise RuntimeError(
            "SECURITY FAILURE: confirmed settlement txid differs "
            "from the exact transaction previously broadcast"
        )

    return found



def command_extract_secret(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    if State(swap.state) != State.BTC_CLAIMED:
        raise ValueError(
            "extract-secret requires state BTC_CLAIMED"
        )

    print("BOB EXTRACTS SECRET FROM BITCOIN")
    print("===============================")
    print("BTC claim txid:", swap.btc_claim_txid)
    print("expected H:    ", swap.secret_hash)
    print()

    revealed = extract_secret_from_btc_claim(
        swap
    )

    # Important:
    # This value came from Bitcoin's witness.
    # We do NOT read swap.secret_hex here.
    swap.revealed_secret_hex = revealed

    save_swap(
        swap,
        role="bob",
    )

    print("SECRET FOUND")
    print("============")
    print("S:       ", revealed)
    print(
        "SHA256:  ",
        hashlib.sha256(
            bytes.fromhex(revealed)
        ).hexdigest(),
    )
    print()
    print(
        "PASS: Bob recovered S from the "
        "Bitcoin transaction"
    )

def auto_mine_enabled():
    raw = os.environ.get(
        "BLAKE_SWAP_AUTO_MINE",
        "1",
    ).strip().lower()

    if raw in {"1", "true", "yes", "on"}:
        return True

    if raw in {"0", "false", "no", "off"}:
        return False

    raise ValueError(
        "BLAKE_SWAP_AUTO_MINE must be 0/1, "
        "false/true, no/yes, or off/on"
    )


def mine_btc(blocks=1):
    wallet = BTC_WALLET

    addr = btc_cli(
        "getnewaddress",
        "",
        "bech32",
        wallet=wallet,
    )

    return btc_cli(
        "generatetoaddress",
        blocks,
        addr,
    )


def get_wallet_pubkey(chain_cli, wallet):
    address = chain_cli(
        "getnewaddress",
        "",
        "bech32",
        wallet=wallet,
    )

    info = chain_cli(
        "getaddressinfo",
        address,
        wallet=wallet,
    )

    pubkey = info.get("pubkey")

    if not pubkey:
        raise RuntimeError(
            f"Could not obtain pubkey for wallet {wallet}"
        )

    return pubkey


def mine_blake(blocks=1):
    wallet = BLAKE_WALLET

    addr = blake_cli(
        "getnewaddress",
        wallet=wallet,
    )

    return blake_cli(
        "generatetoaddress",
        blocks,
        addr,
    )


FUNDING_INTENT_DIR = BASE_SWAP_DIR / "funding-intents"


def funding_intent_path(swap_id, chain):
    FUNDING_INTENT_DIR.mkdir(
        parents=True,
        exist_ok=True,
    )
    return FUNDING_INTENT_DIR / f"{swap_id}.{chain}.json"


def write_funding_intent(
    swap,
    chain,
    address,
    amount,
):
    """
    Durable marker written before funds may move.

    The marker does not contain secrets.  It binds the attempted
    funding operation to this swap, chain, contract address and amount.
    """
    path = funding_intent_path(
        swap.swap_id,
        chain,
    )

    data = {
        "swap_id": swap.swap_id,
        "chain": chain,
        "address": address,
        "amount": str(amount),
        "secret_hash": swap.secret_hash,
    }

    #
    # The funding marker is what makes a broadcast resumable, so the
    # rename must be durable before any funds can move.
    #
    atomic_write_json(
        path,
        data,
        fsync_directory=True,
    )

    return path


def record_funding_broadcast(
    swap_id,
    chain,
    txid,
    vout,
):
    """
    Extend the already-durable funding intent with the exact
    transaction identity after broadcast.

    The protocol checkpoint is deliberately NOT advanced here.
    """
    path = funding_intent_path(
        swap_id,
        chain,
    )

    if not path.exists():
        raise RuntimeError(
            "Funding intent disappeared before "
            "broadcast identity could be recorded"
        )

    data = json.loads(path.read_text())

    data["txid"] = str(txid)
    data["vout"] = int(vout)

    #
    # Extending the marker must be at least as durable as creating it:
    # a broadcast whose identity was lost would send a second funding
    # transaction for a contract that is already paid.
    #
    atomic_write_json(
        path,
        data,
        fsync_directory=True,
    )


def locate_wallet_payment_output(
    cli,
    wallet,
    txid,
    address,
    amount,
    *,
    sats=False,
):
    """
    Decode one exact wallet transaction and return the expected
    funding output index plus its current blockhash, if confirmed.
    """
    wallet_tx = cli(
        "gettransaction",
        txid,
        wallet=wallet,
    )

    raw_hex = wallet_tx.get("hex")

    if not raw_hex:
        raise RuntimeError(
            "Wallet transaction has no raw hex"
        )

    decoded = cli(
        "decoderawtransaction",
        raw_hex,
    )

    for output in decoded.get("vout", []):
        actual_address = (
            output
            .get("scriptPubKey", {})
            .get("address")
        )

        if actual_address != address:
            continue

        if sats:
            observed = int(
                Decimal(str(output["value"]))
                * Decimal(100_000_000)
            )
            expected = int(amount)
        else:
            observed = Decimal(
                str(output["value"])
            )
            expected = Decimal(
                str(amount)
            )

        if observed != expected:
            continue

        return (
            int(output["n"]),
            wallet_tx.get("blockhash"),
        )

    raise RuntimeError(
        "Could not locate expected HTLC funding output "
        "in exact wallet transaction"
    )


def clear_funding_intent(swap_id, chain):
    path = funding_intent_path(
        swap_id,
        chain,
    )

    try:
        path.unlink()
    except FileNotFoundError:
        pass


def reconcile_funding_intent(swap, chain):
    """
    Resolve a funding intent against this participant's own wallet.

    An intent is written before money may move and cleared once the
    outpoint is durable. A crash between those two points leaves an
    orphan that would otherwise block timeout refresh forever, because
    an existing intent means a spend may have started.

    Read-only with respect to the chains: it never broadcasts, never
    mines, and never clears an unresolved intent. Returns:

        "none"       no intent exists; nothing to resolve
        "confirmed"  the exact payment is confirmed, so the funding
                     happened and the swap's own record is the authority
                     from here on; the intent was cleared
        "pending"    the exact payment exists but is unconfirmed, so a
                     broadcast is genuinely in flight
        "absent"     no payment to the intent's address is known to this
                     wallet, which is consistent with a crash before the
                     broadcast -- but not proof, since a replaced wallet
                     would look the same
        "unknown"    an intent exists but is unreadable or incomplete

    "pending", "absent" and "unknown" all require an operator decision;
    only "confirmed" is resolved automatically.
    """
    path = funding_intent_path(swap.swap_id, chain)

    if not path.exists():
        return "none"

    try:
        with open(path) as f:
            intent = json.load(f)
    except Exception:
        return "unknown"

    if not isinstance(intent, dict):
        return "unknown"

    address = intent.get("address")

    if not address:
        return "unknown"

    if chain == "blake":
        found = find_wallet_payment(
            blake_cli,
            BLAKE_WALLET,
            address,
            swap.blake_amount,
        )
    elif chain == "btc":
        found = find_wallet_payment(
            btc_cli,
            BTC_WALLET,
            address,
            swap.btc_sats,
            sats=True,
        )
    else:
        return "unknown"

    if not found:
        #
        # No payment this wallet ever made to that address. That is
        # consistent with a crash before the broadcast, but it is not
        # proof: a replaced or restored wallet would look the same. Do
        # not clear it here; --clear-stale-intent exists for the
        # operator to assert that no funding was broadcast.
        #
        return "absent"

    _txid, _vout, blockhash = found

    if blockhash:
        clear_funding_intent(swap.swap_id, chain)
        return "confirmed"

    return "pending"


FUNDING_LOCK_DIR = BASE_SWAP_DIR / "funding-locks"



def protocol_failpoint(name):
    expected = os.environ.get(
        "BLAKE_SWAP_FAILPOINT"
    )

    if expected != name:
        return

    print(
        f"TEST FAILPOINT TRIGGERED: {name}",
        flush=True,
    )

    # Deliberately bypass normal Python cleanup.
    # The OS will still close file descriptors and
    # release flock locks, which is exactly what we
    # want to test.
    os._exit(97)



#
# Funding locks held by *this* process, keyed by (swap_id, chain).
#
# flock(2) locks belong to the open file description, not the process,
# so acquiring the same lock twice in one process would block forever.
# Tracking the keys we already hold turns that latent deadlock into an
# immediate, named error.
#
_HELD_FUNDING_LOCKS = set()


def serialized_funding(chain):
    """
    Serialize one local funding operation per swap+chain.

    The lock is owned by the process, not by the existence of
    the lock file.  If the process exits or crashes, the OS
    releases the lock automatically.
    """
    def decorate(fn):
        def wrapped(args):
            key = (args.swap_id, chain)

            if key in _HELD_FUNDING_LOCKS:
                raise RuntimeError(
                    "SECURITY FAILURE: nested funding lock for "
                    f"{args.swap_id} on {chain} would deadlock"
                )

            FUNDING_LOCK_DIR.mkdir(
                parents=True,
                exist_ok=True,
            )

            path = (
                FUNDING_LOCK_DIR
                / f"{args.swap_id}.{chain}.lock"
            )

            with open(path, "a+") as lock_file:
                fcntl.flock(
                    lock_file.fileno(),
                    fcntl.LOCK_EX,
                )

                _HELD_FUNDING_LOCKS.add(key)

                try:
                    return fn(args)
                finally:
                    _HELD_FUNDING_LOCKS.discard(key)

                    fcntl.flock(
                        lock_file.fileno(),
                        fcntl.LOCK_UN,
                    )

        return wrapped

    return decorate


def find_wallet_payment(
    cli,
    wallet,
    address,
    amount,
    *,
    sats=False,
):
    """
    Look in our own wallet for an already-created transaction paying
    the exact expected contract address and amount.

    Returns (txid, vout, blockhash) or None.

    This is recovery logic, not counterparty trust: every candidate is
    decoded locally and its actual transaction output is checked.
    """
    entries = cli(
        "listtransactions",
        "*",
        1000,
        0,
        True,
        wallet=wallet,
    )

    candidates = []

    for entry in entries:
        if entry.get("category") != "send":
            continue

        if entry.get("address") != address:
            continue

        raw_amount = entry.get("amount")
        if raw_amount is None:
            continue

        if sats:
            observed = int(
                Decimal(str(abs(raw_amount)))
                * Decimal(100_000_000)
            )
            expected = int(amount)
        else:
            observed = Decimal(str(abs(raw_amount)))
            expected = Decimal(str(amount))

        if observed != expected:
            continue

        txid = entry.get("txid")
        if txid:
            candidates.append(txid)

    # Deduplicate while retaining most-recent wallet order.
    candidates = list(dict.fromkeys(candidates))

    for txid in candidates:
        wallet_tx = cli(
            "gettransaction",
            txid,
            wallet=wallet,
        )

        raw_hex = wallet_tx.get("hex")
        if not raw_hex:
            continue

        decoded = cli(
            "decoderawtransaction",
            raw_hex,
        )

        for output in decoded.get("vout", []):
            spk = output.get("scriptPubKey", {})
            actual_address = spk.get("address")

            if actual_address != address:
                continue

            if sats:
                value = int(
                    Decimal(str(output["value"]))
                    * Decimal(100_000_000)
                )
                if value != int(amount):
                    continue
            else:
                value = Decimal(str(output["value"]))
                if value != Decimal(str(amount)):
                    continue

            return (
                txid,
                output["n"],
                wallet_tx.get("blockhash"),
            )

    return None


def recover_blake_funding(swap, address):
    found = find_wallet_payment(
        blake_cli,
        BLAKE_WALLET,
        address,
        swap.blake_amount,
    )

    if not found:
        return None

    txid, vout, blockhash = found

    if not blockhash and auto_mine_enabled():
        mine_blake(1)

        tx = blake_cli(
            "gettransaction",
            txid,
            wallet=BLAKE_WALLET,
        )

        blockhash = tx.get("blockhash")

    # In production an exact wallet payment may legitimately
    # be pending. Return it without pretending it is confirmed.
    return txid, vout, blockhash


def recover_btc_funding(swap, address):
    found = find_wallet_payment(
        btc_cli,
        BTC_WALLET,
        address,
        swap.btc_sats,
        sats=True,
    )

    if not found:
        return None

    txid, vout, blockhash = found

    if not blockhash and auto_mine_enabled():
        mine_btc(1)

        tx = btc_cli(
            "gettransaction",
            txid,
            wallet=BTC_WALLET,
        )

        blockhash = tx.get("blockhash")

    # Production recovery may find the exact payment while it
    # is still in the mempool. Do not advance durable state yet.
    return txid, vout, blockhash


def command_fund_ready_swap(args):
    """
    Fund the only local protocol-Alice swap whose authenticated
    Bob acceptance is currently available.

    This is an explicit economic command: when exactly one candidate
    exists, it imports Bob's acceptance and then calls the existing
    fund-blake implementation. The underlying funding command retains
    all of its normal validation, serialization, and crash recovery.
    """
    import asyncio
    import json
    from types import SimpleNamespace

    me = current_nostr_pubkey()
    candidates = []

    for path in local_swap_paths():
        try:
            with open(path) as f:
                data = json.load(f)
        except Exception:
            continue

        if data.get("swap_id") != path.stem:
            continue

        if data.get("state") != State.OFFERED.value:
            continue

        if data.get("nostr_alice_pub") != me:
            continue

        bob_pub = data.get("nostr_bob_pub")
        if not bob_pub:
            continue

        # If Bob's settlement keys are already imported, this swap is
        # immediately funding-ready.
        if (
            data.get("blake_bob_pub")
            and data.get("btc_bob_pub")
        ):
            candidates.append(
                {
                    "swap_id": path.stem,
                    "accept_available": True,
                    "already_imported": True,
                    "accept_event_id": None,
                }
            )
            continue

        # Otherwise look for the exact authenticated acceptance from the
        # already-pinned Bob identity. Discovery itself is read-only.
        try:
            message, event_id, signer = asyncio.run(
                nostr_fetch_message(
                    path.stem,
                    "accept",
                    expected_pubkey=bob_pub,
                )
            )
        except Exception:
            continue

        if signer != bob_pub:
            continue

        if message.get("type") != "accept":
            continue

        if message.get("swap_id") != path.stem:
            continue

        if message.get("secret_hash") != data.get("secret_hash"):
            continue

        if not message.get("blake_bob_pub"):
            continue

        if not message.get("btc_bob_pub"):
            continue

        candidates.append(
            {
                "swap_id": path.stem,
                "accept_available": True,
                "already_imported": False,
                "accept_event_id": event_id,
            }
        )

    if not candidates:
        print()
        print("NO FUNDING-READY SWAP")
        print("=====================")
        print(
            "No local protocol-Alice OFFERED swap has an "
            "authenticated Bob acceptance ready."
        )
        print()
        print(
            "The taker may still need to run `accept-match`."
        )
        return

    if len(candidates) > 1:
        print()
        print("MULTIPLE FUNDING-READY SWAPS")
        print("============================")
        print(
            "Refusing to choose which trade receives funds."
        )
        print()

        for index, candidate in enumerate(
            candidates,
            start=1,
        ):
            swap = load_swap(
                candidate["swap_id"],
                role="alice",
            )
            print(
                f"{index}. {swap.swap_id}  "
                f"{swap.blake_amount} BLAKE / "
                f"{swap.btc_sats:,} sats"
            )

        print()
        print(
            "Use `fund-blake <swap-id>` for the one "
            "you intend to commit."
        )
        return

    candidate = candidates[0]
    swap_id = candidate["swap_id"]

    swap = load_swap(
        swap_id,
        role="alice",
    )

    validate_btc_trade_amount(
        swap.btc_sats,
        context=f"swap {swap.swap_id}",
    )

    print()
    print("FUNDING-READY SWAP")
    print("==================")
    print("swap: ", swap.swap_id)
    print("BLAKE:", swap.blake_amount)
    print("sats: ", f"{swap.btc_sats:,}")
    print()

    ns = SimpleNamespace(
        swap_id=swap_id
    )

    if not candidate["already_imported"]:
        print(
            "Importing Bob's authenticated acceptance..."
        )
        command_receive_accept(ns)

    swap = load_swap(
        swap_id,
        role="alice",
    )

    if not (
        swap.blake_bob_pub
        and swap.btc_bob_pub
        and swap.nostr_bob_pub
    ):
        raise RuntimeError(
            "SECURITY FAILURE: Bob acceptance is not fully pinned"
        )

    print()
    print("EXPLICIT COMMITMENT")
    print("===================")
    print(
        "Funding the BLAKE HTLC now. This moves funds."
    )
    print()

    command_fund_blake(ns)


@serialized_funding("blake")
def command_fund_blake(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    require_local_protocol_role(
        swap,
        "alice",
        "BLAKE funding",
    )

    if State(swap.state) != State.OFFERED:
        raise ValueError(
            "fund-blake requires swap state OFFERED"
        )

    height = blake_cli("getblockcount")

    if height >= swap.blake_timeout:
        raise ValueError(
            f"BLAKE timeout {swap.blake_timeout} "
            f"has already been reached; current height={height}"
        )

    desc = get_blake_contract_descriptor(swap)
    address = derive_blake_contract_address(desc)

    print("BLAKE contract")
    print("==============")
    print("height:  ", height)
    print("timeout: ", swap.blake_timeout)
    print("address: ", address)
    print("hash:    ", swap.secret_hash)
    print()

    # Before moving money, see whether a previous invocation
    # already funded this exact contract but crashed before saving
    # the canonical swap state.
    recovered = recover_blake_funding(
        swap,
        address,
    )

    if recovered:
        txid, contract_vout, blockhash = recovered

        print(
            "RECOVERY: found existing BLAKE HTLC funding; "
            "will not fund it again"
        )
    else:
        # Friendly balance preflight. Keep this AFTER crash recovery:
        # an already-funded HTLC must remain recoverable even if the
        # wallet's present balance is now below the original amount.
        balances = blake_cli(
            "getbalances",
            wallet=BLAKE_WALLET,
        )
        trusted = Decimal(
            str(
                balances
                .get("mine", {})
                .get("trusted", 0)
            )
        )
        required = blake_amount_decimal(
            swap.blake_amount_base_units
        )

        if trusted < required:
            print()
            print("INSUFFICIENT BLAKE BALANCE")
            print("==========================")
            print("wallet:   ", BLAKE_WALLET)
            print("trusted:  ", trusted)
            print("required: ", required)
            print()
            print(
                "STOP: the BLAKE HTLC was NOT funded and "
                "no funding intent was written."
            )
            raise RuntimeError(
                "Insufficient trusted BLAKE balance for HTLC funding"
            )

        #
        # Record the HTLC as watch-only before any funds move, so a later
        # loss of the signing wallet does not leave a funded swap that
        # cannot even be observed. Non-fatal by design; see
        # report_watch_only_import().
        #
        report_watch_only_import(swap, "blake")

        # Persist intent before funds may move.
        write_funding_intent(
            swap,
            "blake",
            address,
            swap.blake_amount,
        )

        funding_rate, funding_rate_source = funding_fee_rate(
            blake_cli,
            "blake",
        )

        print(
            "BLAKE funding fee rate:",
            funding_rate,
            "sat/vB",
            f"({funding_rate_source})",
        )

        txid = blake_cli(
            "sendtoaddress",
            address,
            swap.blake_amount,
            "",
            "",
            "false",
            "true",
            "null",
            "unset",
            "null",
            str(funding_rate),
            wallet=BLAKE_WALLET,
        )

        contract_vout, blockhash = (
            locate_wallet_payment_output(
                blake_cli,
                BLAKE_WALLET,
                txid,
                address,
                swap.blake_amount,
            )
        )

        record_funding_broadcast(
            swap.swap_id,
            "blake",
            txid,
            contract_vout,
        )

        if not blockhash and auto_mine_enabled():
            mine_blake(1)

            wallet_tx = blake_cli(
                "gettransaction",
                txid,
                wallet=BLAKE_WALLET,
            )

            blockhash = wallet_tx.get("blockhash")

    # Whether freshly broadcast or crash-recovered, retain the
    # exact broadcast identity in the durable intent until the
    # transaction is actually confirmed.
    record_funding_broadcast(
        swap.swap_id,
        "blake",
        txid,
        contract_vout,
    )

    if not blockhash:
        print()
        print("BLAKE FUNDING BROADCAST")
        print("=======================")
        print("txid:  ", txid)
        print("vout:  ", contract_vout)
        print("block:  pending")
        print("state: ", swap.state)
        print()
        print(
            "BLAKE funding is awaiting "
            "active-chain confirmation."
        )
        return

    swap.blake_funding_txid = txid
    swap.blake_funding_vout = contract_vout
    swap.blake_funding_blockhash = blockhash

    swap.transition(
        State.BLAKE_FUNDED
    )

    save_swap(
        swap,
        role="alice",
    )
    clear_funding_intent(
        swap.swap_id,
        "blake",
    )

    print("FUNDED")
    print("------")
    print("txid:   ", txid)
    print("vout:   ", contract_vout)
    print("block:  ", blockhash)
    print("state:  ", swap.state)

def superseded_placeholders(keep_swap_id, role):
    """
    Local OFFERED swaps that are pure, unannounced placeholders.

    A placeholder has no counterparty, no Alice contract keys (filling
    those in is what `publish-offer` does) and no funding or settlement
    transaction. Nobody else can be relying on such a record, so creating
    a new swap supersedes it.

    Anything that has been announced is deliberately excluded: it may
    still be accepted, so it is left alone and reported instead of being
    archived behind the operator's back.
    """
    placeholders = []

    for path in local_swap_paths():
        if path.stem == keep_swap_id:
            continue

        try:
            candidate = load_swap(path.stem, role=role)
        except Exception:
            continue

        if candidate.state != State.OFFERED.value:
            continue

        if (
            candidate.nostr_bob_pub
            or candidate.blake_alice_pub
            or candidate.btc_alice_pub
        ):
            continue

        if any(
            getattr(candidate, field, None)
            for field in (
                "blake_funding_txid",
                "btc_funding_txid",
                "btc_claim_txid",
                "blake_claim_txid",
                "btc_refund_txid",
                "blake_refund_txid",
            )
        ):
            continue

        placeholders.append(candidate)

    return placeholders


def archive_superseded_placeholders(keep_swap_id, role):
    """
    Archive the local placeholders a newly created swap supersedes.

    Auto-archiving is safe only for a record nobody else can reference,
    which is exactly what `superseded_placeholders` identifies, and the
    archive directory preserves it either way. Every archive is
    announced: a swap that disappears silently is the failure being
    fixed, so the replacement must never be quiet about it.
    """
    placeholders = superseded_placeholders(keep_swap_id, role)

    if not placeholders:
        return []

    archive_dir = BASE_SWAP_DIR / "archive"
    archive_dir.mkdir(parents=True, exist_ok=True)

    archived = []

    for candidate in placeholders:
        source = swap_path(candidate.swap_id)
        target = archive_dir / source.name

        if target.exists():
            continue

        shutil.move(str(source), str(target))
        archived.append(candidate.swap_id)

    if archived:
        print()
        print("SUPERSEDED PLACEHOLDER ARCHIVED")
        print("==============================")
        print("new swap:", keep_swap_id)

        for swap_id in archived:
            print("archived: ", swap_id)

        print()
        print(
            "PASS: the archived record was never announced, so no "
            "counterparty can reference it"
        )
        print("PASS: it was preserved under archive/, not deleted")
        print()

    return archived


def command_new(args):
    #
    # v1.5 has two construction modes:
    #
    #   normal:
    #       no explicit timeout heights
    #       -> derive them from TimeoutPolicy
    #
    #   explicit:
    #       both heights supplied
    #       -> backward-compatible regtest/test override
    #
    # Supplying only one timeout is never meaningful.
    #

    explicit_btc = (
        args.btc_timeout
        is not None
    )

    explicit_blake = (
        args.blake_timeout
        is not None
    )

    if explicit_btc != explicit_blake:
        raise ValueError(
            "Either supply both --btc-timeout and "
            "--blake-timeout, or supply neither."
        )

    if explicit_btc:
        #
        # Legacy/regtest explicit contract construction.
        #
        swap = make_swap(
            args.blake,
            args.sats,
            args.blake_timeout,
            args.btc_timeout,
        )

        #
        # Explicit heights are a test/regtest override, but the offer
        # they produce must still be well formed: a peer refuses an
        # offer with no v1.5 metadata, so an explicit swap could never
        # be accepted. Advertise the wall-clock windows that the
        # supplied heights actually imply, measured from the chain tips
        # make_swap just recorded.
        #
        # This does not weaken the peer's safety check: Bob still
        # validates the advertised windows AND the real heights against
        # his own policy, so an explicit offer that leaves too little
        # recovery time is rejected exactly as a derived one would be.
        #
        policy = TimeoutPolicy()

        btc_reference_height = int(
            swap.btc_scan_start_height
        )
        blake_reference_height = int(
            swap.blake_scan_start_height
        )

        btc_window = max(
            0,
            swap.btc_timeout - btc_reference_height,
        ) * policy.btc_block_seconds

        blake_window = max(
            0,
            swap.blake_timeout - blake_reference_height,
        ) * policy.blake_block_seconds

        swap.timeout_policy_version = "1.5"
        swap.btc_timeout_window_seconds = btc_window
        swap.blake_timeout_window_seconds = blake_window
        swap.bob_recovery_seconds = max(
            0,
            blake_window - btc_window,
        )

    else:
        #
        # v1.5 production-style construction.
        #
        timeout_policy = TimeoutPolicy()

        btc_height = int(
            btc_cli(
                "getblockcount"
            )
        )

        blake_height = int(
            blake_cli(
                "getblockcount"
            )
        )

        timeout_plan = build_timeout_plan(
            btc_height=btc_height,
            blake_height=blake_height,
            policy=timeout_policy,
        )

        validate_timeout_plan(
            timeout_plan,
            timeout_policy,
        )

        swap = make_swap(
            args.blake,
            args.sats,
            timeout_plan.blake_timeout_height,
            timeout_plan.btc_timeout_height,
        )

        swap.timeout_policy_version = "1.5"

        swap.btc_timeout_window_seconds = (
            timeout_plan.btc_window_seconds
        )

        swap.blake_timeout_window_seconds = (
            timeout_plan.blake_window_seconds
        )

        swap.bob_recovery_seconds = (
            timeout_plan.bob_recovery_seconds
        )

        #
        # Validate again after attaching the persisted safety
        # evidence. This exercises Swap.validate(), not merely
        # the standalone policy module.
        #
        swap.validate()

    #
    # The creator of a normal swap is protocol Alice.
    # Persist Alice's own participant identity immediately.
    #
    # This is local identity evidence derived from the configured
    # participant key; it does not depend on Nostr transport or on
    # any counterparty-supplied message.
    #
    alice_identity = current_nostr_pubkey()

    if (
        swap.nostr_alice_pub is not None
        and swap.nostr_alice_pub != alice_identity
    ):
        raise RuntimeError(
            "SECURITY FAILURE: existing Alice participant "
            "identity does not match current participant"
        )

    swap.nostr_alice_pub = alice_identity

    path = save_swap(
        swap
    )

    archive_superseded_placeholders(
        swap.swap_id,
        os.environ.get("BLAKE_SWAP_ROLE", "alice"),
    )

    print_swap(
        swap
    )

    if (
        getattr(
            swap,
            "timeout_policy_version",
            None,
        )
        == "1.5"
    ):
        print()
        print(
            "timeout policy:",
            swap.timeout_policy_version,
        )

        print(
            "BTC safety window:",
            f"{swap.btc_timeout_window_seconds / 3600:.2f} h",
        )

        print(
            "Bob recovery window:",
            f"{swap.bob_recovery_seconds / 3600:.2f} h",
        )

        print(
            "BLAKE safety window:",
            f"{swap.blake_timeout_window_seconds / 3600:.2f} h",
        )

    print()
    print(
        "saved:",
        path,
    )

def command_refresh_timeouts(args):
    """
    Refresh an unfunded protocol-Alice swap's v1.5
    timeout plan without changing swap identity,
    economic terms, keys, S, or H.

    This is only valid before any funding action may
    have occurred.
    """
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    if State(swap.state) != State.OFFERED:
        raise RuntimeError(
            "refresh-timeouts requires state OFFERED"
        )

    #
    # Timeout refresh is unilateral only before the
    # counterparty has accepted the protocol offer.
    #
    # Once Bob's settlement keys / identity are bound,
    # the advertised CLTV terms are part of the accepted
    # contract and Alice may not silently replace them.
    #
    if (
        swap.blake_bob_pub is not None
        or swap.btc_bob_pub is not None
        or swap.nostr_bob_pub is not None
    ):
        raise RuntimeError(
            "SECURITY FAILURE: cannot refresh timeouts "
            "after counterparty acceptance"
        )

    #
    # Refuse if canonical state contains any evidence
    # that funding has begun or completed.
    #
    funding_fields = (
        "blake_funding_txid",
        "blake_funding_vout",
        "blake_funding_blockhash",
        "btc_funding_txid",
        "btc_funding_vout",
        "btc_funding_blockhash",
    )

    for field in funding_fields:
        if getattr(swap, field, None) is not None:
            raise RuntimeError(
                "SECURITY FAILURE: cannot refresh "
                f"timeouts after funding evidence exists: {field}"
            )

    #
    # Funding intents are written before money may move. Their
    # existence means a crash-window funding action may have started
    # even if canonical state is still OFFERED.
    #
    # A confirmed funding resolves itself. Anything else refuses by
    # default, so an orphan cannot silently block refresh forever: the
    # operator is given an explicit, deliberate way out that they may
    # use only after verifying no funding was broadcast.
    #
    for chain in ("blake", "btc"):
        status = reconcile_funding_intent(
            swap,
            chain,
        )

        if status in ("none", "confirmed"):
            continue

        if getattr(args, "clear_stale_intent", False):
            if status == "pending":
                raise RuntimeError(
                    "SECURITY FAILURE: refusing to clear the "
                    f"{chain} funding intent: a funding broadcast is "
                    "still pending. Let it confirm or be replaced first."
                )

            clear_funding_intent(swap.swap_id, chain)

            print(
                "CLEARED STALE FUNDING INTENT:",
                chain.upper(),
                f"({status})",
            )
            continue

        raise RuntimeError(
            "SECURITY FAILURE: cannot refresh timeouts while a "
            f"{chain} funding intent exists ({status}). If you have "
            "verified that no funding transaction was broadcast, "
            "re-run with --clear-stale-intent."
        )

    original_swap_id = swap.swap_id
    original_secret = swap.secret_hex
    original_hash = swap.secret_hash
    original_blake_amount = swap.blake_amount
    original_btc_sats = swap.btc_sats

    old_btc_timeout = swap.btc_timeout
    old_blake_timeout = swap.blake_timeout

    btc_height = int(
        btc_cli("getblockcount")
    )

    blake_height = int(
        blake_cli("getblockcount")
    )

    policy = TimeoutPolicy()

    plan = build_timeout_plan(
        btc_height=btc_height,
        blake_height=blake_height,
        policy=policy,
    )

    validate_timeout_plan(
        plan,
        policy,
    )

    swap.btc_timeout = (
        plan.btc_timeout_height
    )

    swap.blake_timeout = (
        plan.blake_timeout_height
    )

    swap.timeout_policy_version = "1.5"

    swap.btc_timeout_window_seconds = (
        plan.btc_window_seconds
    )

    swap.blake_timeout_window_seconds = (
        plan.blake_window_seconds
    )

    swap.bob_recovery_seconds = (
        plan.bob_recovery_seconds
    )

    #
    # Since we proved no funding action has started,
    # move the recovery scan anchors forward too.
    #
    swap.btc_scan_start_height = btc_height
    swap.blake_scan_start_height = blake_height

    swap.validate()

    #
    # Explicit invariant checks.
    #
    if swap.swap_id != original_swap_id:
        raise RuntimeError(
            "SECURITY FAILURE: swap_id changed during timeout refresh"
        )

    if swap.secret_hex != original_secret:
        raise RuntimeError(
            "SECURITY FAILURE: secret S changed during timeout refresh"
        )

    if swap.secret_hash != original_hash:
        raise RuntimeError(
            "SECURITY FAILURE: hash H changed during timeout refresh"
        )

    if swap.blake_amount != original_blake_amount:
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE amount changed during timeout refresh"
        )

    if swap.btc_sats != original_btc_sats:
        raise RuntimeError(
            "SECURITY FAILURE: BTC amount changed during timeout refresh"
        )

    save_swap(
        swap,
        role="alice",
    )

    print()
    print("TIMEOUTS REFRESHED")
    print("==================")
    print("swap id:          ", swap.swap_id)
    print("Bitcoin height:   ", btc_height)
    print("BLAKE height:     ", blake_height)
    print()
    print("old BTC timeout:  ", old_btc_timeout)
    print("new BTC timeout:  ", swap.btc_timeout)
    print("old BLAKE timeout:", old_blake_timeout)
    print("new BLAKE timeout:", swap.blake_timeout)
    print()
    print(
        "BTC safety window:",
        f"{swap.btc_timeout_window_seconds / 3600:.2f} h",
    )
    print(
        "Bob recovery window:",
        f"{swap.bob_recovery_seconds / 3600:.2f} h",
    )
    print(
        "BLAKE safety window:",
        f"{swap.blake_timeout_window_seconds / 3600:.2f} h",
    )
    print()
    print("PASS: same swap_id")
    print("PASS: same secret S")
    print("PASS: same hash H")
    print("PASS: same economic terms")
    print("PASS: no funding evidence existed")


def command_status(args):
    blake_height = blake_cli("getblockcount")
    btc_height = btc_cli("getblockcount")

    print("CHAIN STATUS")
    print("============")
    print("BLAKE height:  ", blake_height)
    print("Bitcoin height:", btc_height)

def command_show(args):
    role = os.environ.get(
        "BLAKE_SWAP_ROLE",
        "alice",
    )

    if role not in ("alice", "bob"):
        raise ValueError(
            "BLAKE_SWAP_ROLE must be alice or bob"
        )

    swap = load_swap(
        args.swap_id,
        role=role,
    )

    print_swap(swap)


def command_advance(args):
    if os.environ.get("BLAKE_SWAP_TEST_MODE") != "1":
        raise RuntimeError(
            "advance is available only in BLAKE_SWAP_TEST_MODE=1"
        )

    swap = load_swap(
        args.swap_id,
        role=args.role,
    )

    old_state = swap.state

    swap.transition(args.state)

    save_swap(
        swap,
        role=args.role,
    )

    print(
        f"{old_state} -> {swap.state}"
    )

def command_list(args):
    found = False

    for role in ("alice", "bob"):
        directory = role_dir(role)

        files = sorted(
            directory.glob("*.json")
        )

        if not files:
            continue

        found = True

        print()
        print(role.upper())
        print("=" * len(role))

        for path in files:
            with open(path) as f:
                data = json.load(f)

            print(
                data["swap_id"],
                data["state"],
            )

    if not found:
        print("No swaps.")

TRANSPORT = os.environ.get(
    "BLAKE_SWAP_TRANSPORT",
    "file",
)


def message_path(swap_id, message_type):
    return (
        MESSAGE_DIR
        / f"{swap_id}.{message_type}.json"
    )


def _wire_view(message):
    """
    The wire fields of a message, without engine-internal annotations.

    Receive paths attach the authenticated sender identity as
    `_nostr_pubkey`, and the FILE envelope mirrors that. Those keys are
    engine bookkeeping rather than protocol content, so the canonical
    schema is applied to the message without them. A key is internal
    exactly when it begins with an underscore, and no wire field does.
    """
    return {
        key: value
        for key, value in message.items()
        if not key.startswith("_")
    }


def validate_transport_message(
    swap_id,
    message_type,
    message,
    *,
    allow_internal=False,
):
    """
    Validation shared by every transport backend.

    In particular, Alice's original secret S must never
    travel through the coordination layer.

    `allow_internal` is set only by the receive path, where the transport
    has already attached the authenticated sender. On the send path it
    stays false, so an internal annotation can never be transmitted.
    """

    if message.get("type") != message_type:
        raise ValueError(
            "message type does not match transport type"
        )

    if message.get("swap_id") != swap_id:
        raise ValueError(
            "message swap_id does not match transport swap_id"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: refusing to transport secret_hex"
        )

    #
    # The canonical schema is authoritative for settlement messages.
    #
    # Checking the name `secret_hex` only catches a top-level key with
    # exactly that name: a nested or renamed field would pass. The schema
    # rejects *unknown fields*, so running it here enforces the invariant
    # by shape rather than by one key name, and it means the engine and
    # the wire specification cannot drift apart without a test failing.
    #
    # The explicit check above is kept as defence in depth, and because
    # it reports a SECURITY FAILURE rather than a schema error.
    #
    if message_type in swap_wire.SETTLEMENT_TYPES:
        internal = sorted(
            key for key in message if key.startswith("_")
        )

        if internal and not allow_internal:
            raise RuntimeError(
                "SECURITY FAILURE: refusing to transmit engine-internal "
                f"field(s) {internal} on the wire"
            )

        try:
            swap_wire.validate_message(
                _wire_view(message),
                expected_type=message_type,
            )
        except swap_wire.WireError as e:
            raise RuntimeError(
                f"SECURITY FAILURE: {message_type} message does not "
                f"satisfy the canonical wire schema: {e}"
            ) from e


def file_send_message(
    swap_id,
    message_type,
    message,
):
    path = message_path(
        swap_id,
        message_type,
    )

    #
    # FILE transport is a local/regtest testing backend, not a
    # cryptographically authenticated network transport.
    #
    # Still, preserve the participant identity selected by the
    # sending process so black-box FILE tests exercise the same
    # identity-binding paths used by Nostr transport.
    #
    envelope = {
        "_file_transport_version": 2,
        "sender_pubkey": current_nostr_pubkey(),
        "message": message,
    }

    #
    # A transport spool rather than protocol state: the message can be
    # re-sent, so the rename does not need to be durable. It does need to
    # be atomic, so the peer never reads half an envelope.
    #
    atomic_write_json(
        path,
        envelope,
    )

    return path


def file_receive_message(
    swap_id,
    message_type,
):
    path = message_path(
        swap_id,
        message_type,
    )

    if not path.exists():
        raise FileNotFoundError(
            f"No {message_type} message: {swap_id}"
        )

    with open(path) as f:
        data = json.load(f)

    #
    # v2 FILE envelope: restore sender identity as local transport
    # metadata. This mirrors the `_nostr_pubkey` metadata attached
    # after Nostr signature verification.
    #
    if (
        isinstance(data, dict)
        and data.get("_file_transport_version") == 2
        and isinstance(data.get("message"), dict)
    ):
        sender_pubkey = data.get("sender_pubkey")

        if (
            not isinstance(sender_pubkey, str)
            or len(sender_pubkey) != 64
        ):
            raise RuntimeError(
                "Invalid FILE transport sender identity"
            )

        try:
            bytes.fromhex(sender_pubkey)
        except ValueError as e:
            raise RuntimeError(
                "Invalid FILE transport sender identity"
            ) from e

        message = dict(data["message"])
        message["_nostr_pubkey"] = sender_pubkey

        return message, path

    #
    # Backward compatibility for old FILE messages written before
    # participant identity metadata was added.
    #
    if isinstance(data, dict):
        return data, path

    raise RuntimeError(
        "Invalid FILE transport message"
    )


_nostr_relays_raw = os.environ.get(
    "BLAKE_SWAP_NOSTR_RELAYS",
    os.environ.get(
        "BLAKE_SWAP_NOSTR_RELAY",
        "wss://nos.lol",
    ),
)

NOSTR_RELAYS = tuple(
    relay.strip()
    for relay in _nostr_relays_raw.split(",")
    if relay.strip()
)

if not NOSTR_RELAYS:
    raise RuntimeError(
        "No Nostr relays configured"
    )

# Transitional compatibility: the *legacy environment variable*
# BLAKE_SWAP_NOSTR_RELAY is still honoured above, but reads always use
# every configured relay.


def nostr_private_key_hex():
    """
    Return this participant's Nostr private key.

    Participant identity is intentionally independent
    of the dynamic atomic-swap protocol role.

    Preferred configuration:

        BLAKE_SWAP_PARTICIPANT_KEY_FILE

    Legacy compatibility:

        BLAKE_SWAP_NOSTR_KEY_FILE
        BLAKE_SWAP_NOSTR_KEY_DIR/<role>.key
    """

    participant_key = os.environ.get(
        "BLAKE_SWAP_PARTICIPANT_KEY_FILE"
    )

    legacy_explicit_key = os.environ.get(
        "BLAKE_SWAP_NOSTR_KEY_FILE"
    )

    if participant_key:
        key_path = Path(
            participant_key
        ).expanduser()

    elif legacy_explicit_key:
        key_path = Path(
            legacy_explicit_key
        ).expanduser()

    else:
        #
        # Backward compatibility only.
        #
        # Historically the identity-key filename was
        # derived from protocol role.  That assumption
        # is no longer conceptually correct because the
        # same participant can be Alice in one swap and
        # Bob in another.
        #
        role = os.environ.get(
            "BLAKE_SWAP_ROLE"
        )

        if role not in ("alice", "bob"):
            raise RuntimeError(
                "Configure "
                "BLAKE_SWAP_PARTICIPANT_KEY_FILE, "
                "or set BLAKE_SWAP_ROLE for legacy "
                "role-named key lookup"
            )

        key_dir = Path(
            os.environ.get(
                "BLAKE_SWAP_NOSTR_KEY_DIR",
                BASE_SWAP_DIR / "nostr-keys",
            )
        ).expanduser()

        key_path = (
            key_dir
            / f"{role}.key"
        )

    if not key_path.exists():
        raise RuntimeError(
            f"Nostr participant key does not exist: "
            f"{key_path}"
        )

    key_hex = key_path.read_text().strip()

    try:
        raw = bytes.fromhex(
            key_hex
        )
    except ValueError as e:
        raise RuntimeError(
            f"Invalid Nostr key in {key_path}"
        ) from e

    if len(raw) != 32:
        raise RuntimeError(
            f"Nostr private key in {key_path} "
            "must be exactly 32 bytes"
        )

    return key_hex



def nostr_event_id(event):
    import hashlib

    serialized = json.dumps(
        [
            0,
            event["pubkey"],
            event["created_at"],
            event["kind"],
            event["tags"],
            event["content"],
        ],
        separators=(",", ":"),
        ensure_ascii=False,
    )

    return hashlib.sha256(
        serialized.encode()
    ).digest()


def verify_nostr_event(event):
    from coincurve import PublicKeyXOnly

    required = {
        "id",
        "pubkey",
        "created_at",
        "kind",
        "tags",
        "content",
        "sig",
    }

    missing = required - set(event)

    if missing:
        raise RuntimeError(
            f"Nostr event missing fields: {sorted(missing)}"
        )

    event_id = nostr_event_id(event)

    if event_id.hex() != event["id"]:
        raise RuntimeError(
            "Nostr event ID verification failed"
        )

    pubkey = PublicKeyXOnly(
        bytes.fromhex(event["pubkey"])
    )

    if not pubkey.verify(
        bytes.fromhex(event["sig"]),
        event_id,
    ):
        raise RuntimeError(
            "Nostr Schnorr signature verification failed"
        )

    #
    # A valid signature proves authorship, not timeliness. Retrieval
    # selects the latest event by (created_at, id), so an event dated far
    # in the future would otherwise win every comparison and could be
    # treated as permanently fresh. Bound the tolerated clock skew.
    #
    try:
        created_at = int(event["created_at"])
    except (TypeError, ValueError):
        raise RuntimeError(
            "Nostr event created_at is not an integer"
        ) from None

    if created_at > int(time.time()) + FUTURE_SKEW_SECONDS:
        raise RuntimeError(
            "Nostr event created_at is too far in the future "
            f"(skew limit {FUTURE_SKEW_SECONDS}s)"
        )


NOSTR_ENCRYPTION_TAG = "nip44_v2"

#
# Coordination content is plaintext by default. The protocol does not
# depend on transport confidentiality -- S never travels, and every fact a
# message carries is re-verified against a chain -- so encryption is an
# opt-in that shields the metadata as well.
#
# Set BLAKE_SWAP_NOSTR_ENCRYPTION=nip44_v2 on BOTH participants to enable
# it. A participant that requires encryption refuses a plaintext
# coordination message rather than accepting a downgrade, and a
# participant that does not understand encryption refuses an event that
# declares it rather than guessing at it.
#
NOSTR_ENCRYPTION_MODES = ("off", NOSTR_ENCRYPTION_TAG)

#
# Which settlement messages can be encrypted. The `offer` is first
# contact: at the moment it is published the maker does not yet know the
# taker's identity, and NIP-44 is pairwise by construction, so there is no
# key to encrypt it to. It is also public coordination data by nature --
# the announcement is the ask. Everything after first contact is pairwise
# and is encrypted when the mode is on.
#
NOSTR_ENCRYPTABLE_TYPES = tuple(
    sorted(swap_wire.SETTLEMENT_TYPES - {"offer"})
)


def nostr_encryption_required(message_type):
    """
    Whether this message must be encrypted in the current mode.

    The offer is exempt in both directions: it cannot be encrypted on
    send, and it must still be readable on receipt, or two participants
    could never meet.
    """

    return (
        nostr_encryption_enabled()
        and message_type in NOSTR_ENCRYPTABLE_TYPES
    )


def nostr_encryption_mode():
    """The configured coordination-confidentiality mode."""

    raw = os.environ.get(
        "BLAKE_SWAP_NOSTR_ENCRYPTION",
        "off",
    ).strip().lower()

    if raw in ("", "0", "false", "no"):
        raw = "off"

    if raw not in NOSTR_ENCRYPTION_MODES:
        raise RuntimeError(
            "BLAKE_SWAP_NOSTR_ENCRYPTION must be one of "
            f"{', '.join(NOSTR_ENCRYPTION_MODES)}"
        )

    return raw


def nostr_encryption_enabled():
    return nostr_encryption_mode() == NOSTR_ENCRYPTION_TAG


def nostr_event_encryption(event):
    """
    The encryption a coordination event declares, or None.

    The tag is inside the signed event, so a relay cannot add or remove
    it without invalidating the signature.
    """

    for tag in event.get("tags", []):
        if (
            isinstance(tag, list)
            and len(tag) >= 2
            and tag[0] == "encryption"
        ):
            return tag[1]

    return None


def nostr_event_message_type(event):
    """
    The coordination message type an event declares.

    Read from the signed `type` tag rather than from the content, because
    the content may be encrypted and therefore unreadable until the right
    key is known.
    """

    for tag in event.get("tags", []):
        if (
            isinstance(tag, list)
            and len(tag) >= 2
            and tag[0] == "type"
        ):
            return tag[1]

    return None


def nostr_conversation_key(peer_pubkey):
    """The conversation key for one peer, from our own Nostr key."""

    import nip44

    return nip44.conversation_key(
        nostr_private_key_hex(),
        peer_pubkey,
    )


def build_nostr_event(
    swap_id,
    message_type,
    message,
    expected_role=None,
):
    import time
    from coincurve import PrivateKey

    if expected_role is not None:
        if expected_role not in ("alice", "bob"):
            raise ValueError(
                "expected_role must be alice or bob"
            )

        # BLAKE_SWAP_ROLE identifies the local participant profile.
        # For a reverse-direction swap that participant may occupy
        # the opposite protocol role. Once durable swap state exists,
        # derive the role from identities pinned into that swap.
        current_role = None

        for candidate_role in ("alice", "bob"):
            try:
                candidate = load_swap(
                    swap_id,
                    role=candidate_role,
                )
            except Exception:
                continue

            try:
                derived_role, _, _ = _reconcile_local_role(
                    candidate
                )
            except Exception:
                continue

            if (
                current_role is not None
                and current_role != derived_role
            ):
                raise RuntimeError(
                    "SECURITY FAILURE: conflicting local "
                    "protocol-role derivations for Nostr signing"
                )

            current_role = derived_role

        # Before a swap exists there is no durable role binding,
        # so retain the existing participant-profile guard.
        if current_role is None:
            current_role = os.environ.get(
                "BLAKE_SWAP_ROLE"
            )

        if current_role != expected_role:
            raise RuntimeError(
                "SECURITY FAILURE: refusing to sign "
                f"{message_type} as protocol {current_role!r}; "
                f"protocol requires {expected_role!r}"
            )

    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = private_key.public_key.format(
        compressed=True
    )

    pubkey_hex = compressed[1:].hex()

    content = json.dumps(
        message,
        separators=(",", ":"),
        sort_keys=True,
    )

    tags = [
        ["t", "blake-swap"],
        ["d", swap_id],
        ["type", message_type],
    ]

    if nostr_encryption_required(message_type):
        #
        # Settlement coordination is encrypted when the mode is on. A
        # plaintext message would be a downgrade of our own making, and a
        # peer in encrypted mode refuses it, so there is no useful
        # fallback here: fail closed instead.
        #
        if current_role is None:
            raise RuntimeError(
                "SECURITY FAILURE: cannot encrypt a coordination message "
                "without knowing the local protocol role"
            )

        local = load_swap(
            swap_id,
            role=current_role,
        )

        peer_field = (
            "nostr_bob_pub"
            if current_role == "alice"
            else "nostr_alice_pub"
        )

        peer_pubkey = getattr(local, peer_field, None)

        if not peer_pubkey:
            raise RuntimeError(
                "SECURITY FAILURE: no pinned counterparty Nostr identity "
                "to encrypt to"
            )

        import nip44

        content = nip44.encrypt(
            content,
            nostr_conversation_key(peer_pubkey),
        )

        tags.append(["encryption", NOSTR_ENCRYPTION_TAG])

    event = {
        "pubkey": pubkey_hex,
        "created_at": int(time.time()),
        "kind": 1,
        "tags": tags,
        "content": content,
    }

    event_id = nostr_event_id(event)

    event["id"] = event_id.hex()
    event["sig"] = private_key.sign_schnorr(
        event_id
    ).hex()

    return event


async def nostr_publish_event_to_relay(
    relay,
    event,
):
    import asyncio
    import websockets

    async with websockets.connect(
        relay,
        open_timeout=10,
        close_timeout=5,
    ) as ws:

        await ws.send(
            json.dumps(
                ["EVENT", event]
            )
        )

        while True:
            raw = await asyncio.wait_for(
                ws.recv(),
                timeout=10,
            )

            response = json.loads(raw)

            # Some relays may send AUTH before giving
            # us the final EVENT acknowledgement.
            if (
                isinstance(response, list)
                and len(response) >= 2
                and response[0] == "AUTH"
            ):
                return {
                    "relay": relay,
                    "accepted": False,
                    "auth_required": True,
                    "message": response,
                }

            if (
                isinstance(response, list)
                and len(response) >= 3
                and response[0] == "OK"
                and response[1] == event["id"]
            ):
                return {
                    "relay": relay,
                    "accepted": bool(
                        response[2]
                    ),
                    "auth_required": False,
                    "message": response,
                }


def max_nostr_event_bytes():
    """
    The largest serialized event this engine will publish.

    Relays enforce their own limits, advertised in NIP-11 as
    `max_message_length`, and the useful range across public relays is
    wide. This is deliberately not an attempt to match any particular
    relay: it is a bound the engine applies before handing an event to a
    network it does not control, so that a runaway `content` field is
    refused here with a clear message instead of being rejected by
    whichever relay happens to be strictest, after seven others accepted
    it.

    A settlement or market message is a few kilobytes, so the default
    leaves more than an order of magnitude of headroom. Override with
    BLAKE_SWAP_MAX_EVENT_BYTES.
    """
    default = 64 * 1024

    raw = os.environ.get(
        "BLAKE_SWAP_MAX_EVENT_BYTES",
        "",
    ).strip()

    if not raw:
        return default

    try:
        value = int(raw)
    except ValueError as e:
        raise RuntimeError(
            "BLAKE_SWAP_MAX_EVENT_BYTES must be an integer"
        ) from e

    if value < 1024:
        raise RuntimeError(
            "BLAKE_SWAP_MAX_EVENT_BYTES must be at least 1024"
        )

    return value


def require_publishable_event_size(event):
    """
    Refuse to publish an event larger than the configured bound.

    Returns the serialized size in bytes. Raises before any signature
    verification, caching or network work, so an oversized event costs
    nothing and cannot be half-published.
    """
    size = len(
        json.dumps(
            event,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    )

    limit = max_nostr_event_bytes()

    if size > limit:
        raise RuntimeError(
            f"Refusing to publish a {size}-byte Nostr event: "
            f"the limit is {limit} bytes "
            f"(BLAKE_SWAP_MAX_EVENT_BYTES)"
        )

    return size


async def nostr_publish_event(event):
    import asyncio

    #
    # Bound the event first. An event too large for the relays must be
    # refused by the engine, before it is cached or offered to any of
    # them.
    #
    require_publishable_event_size(event)

    #
    # Preserve our own exact signed event before
    # depending on any relay for storage.
    #
    # nostr_cache_event() cryptographically verifies
    # the event before writing it to durable storage.
    #
    cache_path = nostr_cache_event(event)

    print()
    print("Nostr outbound event cached:")
    print(event["id"])
    print("cache:", cache_path)

    tasks = [
        nostr_publish_event_to_relay(
            relay,
            event,
        )
        for relay in NOSTR_RELAYS
    ]

    results = await asyncio.gather(
        *tasks,
        return_exceptions=True,
    )

    accepted = []

    print()
    print("NOSTR RELAY RESULTS")
    print("===================")

    for relay, result in zip(
        NOSTR_RELAYS,
        results,
    ):
        if isinstance(
            result,
            Exception,
        ):
            print(
                f"{relay}: ERROR "
                f"{type(result).__name__}: {result}"
            )
            continue

        if result["accepted"]:
            accepted.append(relay)
            print(
                f"{relay}: ACCEPTED"
            )

        elif result["auth_required"]:
            print(
                f"{relay}: AUTH REQUIRED"
            )
            print(
                "  challenge:",
                result["message"],
            )

        else:
            print(
                f"{relay}: REJECTED"
            )
            print(
                "  response:",
                result["message"],
            )

    print()

    if not accepted:
        raise RuntimeError(
            "No configured Nostr relay "
            "accepted the event"
        )

    print(
        f"PASS: {len(accepted)} of "
        f"{len(NOSTR_RELAYS)} relay(s) "
        "accepted the identical signed event"
    )

    return event["id"]


async def nostr_fetch_message_from_relay(
    relay,
    swap_id,
    message_type,
    expected_pubkey=None,
):
    import asyncio
    import secrets
    import websockets

    sub_id = (
        "blake-swap-"
        + secrets.token_hex(4)
    )

    event_filter = {
        "kinds": [1],
        "#d": [swap_id],
        "limit": 20,
    }

    # If the counterparty identity is already pinned,
    # ask the relay only for that author's events.
    if expected_pubkey:
        event_filter["authors"] = [
            expected_pubkey
        ]

    events = []

    async with websockets.connect(
        relay,
        open_timeout=10,
        close_timeout=5,
    ) as ws:

        await ws.send(
            json.dumps(
                [
                    "REQ",
                    sub_id,
                    event_filter,
                ]
            )
        )

        while True:
            raw = await asyncio.wait_for(
                ws.recv(),
                timeout=10,
            )

            response = json.loads(raw)

            if (
                isinstance(response, list)
                and len(response) >= 3
                and response[0] == "EVENT"
                and response[1] == sub_id
            ):
                events.append(
                    response[2]
                )

            elif (
                isinstance(response, list)
                and len(response) >= 2
                and response[0] == "EOSE"
                and response[1] == sub_id
            ):
                break

        await ws.send(
            json.dumps(
                ["CLOSE", sub_id]
            )
        )

    return events


async def nostr_fetch_message(
    swap_id,
    message_type,
    expected_pubkey=None,
):
    import asyncio

    tasks = [
        nostr_fetch_message_from_relay(
            relay,
            swap_id,
            message_type,
            expected_pubkey=expected_pubkey,
        )
        for relay in NOSTR_RELAYS
    ]

    results = await asyncio.gather(
        *tasks,
        return_exceptions=True,
    )

    events_by_id = {}
    successful_relays = 0

    for relay, result in zip(
        NOSTR_RELAYS,
        results,
    ):
        if isinstance(result, Exception):
            continue

        successful_relays += 1

        for event in result:
            if not isinstance(event, dict):
                continue

            event_id = event.get("id")

            if not isinstance(event_id, str):
                continue

            events_by_id.setdefault(
                event_id,
                event,
            )

    if successful_relays == 0:
        raise RuntimeError(
            "Could not query any configured "
            "Nostr relay for swap messages"
        )

    if not events_by_id:
        raise FileNotFoundError(
            f"No Nostr {message_type} message: "
            f"{swap_id}"
        )

    valid = []

    for event in events_by_id.values():
        try:
            verify_nostr_event(event)

            # Relay-side author filtering is only an
            # optimization. This local check is the
            # actual security boundary.
            if (
                expected_pubkey is not None
                and event["pubkey"]
                != expected_pubkey
            ):
                continue

        except Exception:
            # Relays and relay contents are untrusted.
            continue

        #
        # Content decoding is deliberately OUTSIDE the skip-guard above.
        # A declared encryption that cannot be honoured -- an encrypted
        # message for a plaintext participant, a downgrade, an untagged
        # payload -- is a security condition, not a relay artefact, and
        # swallowing it into "no valid message matched" would hide the
        # very thing the mode exists to detect.
        #
        message = decode_coordination_content(
            event,
            swap_id=swap_id,
            message_type=message_type,
        )

        if message is None:
            continue

        valid.append(
            (
                event["created_at"],
                event["id"],
                event["pubkey"],
                message,
            )
        )

    if not valid:
        raise RuntimeError(
            "No valid signed Nostr message matched"
        )

    valid.sort(
        key=lambda item: (
            item[0],
            item[1],
        )
    )

    _, event_id, signer_pubkey, message = (
        valid[-1]
    )

    print(
        "Nostr message read:",
        len(events_by_id),
        "unique event(s) from",
        f"{successful_relays}/"
        f"{len(NOSTR_RELAYS)} relay(s)",
    )

    return message, event_id, signer_pubkey


def decode_coordination_content(event, *, swap_id, message_type):
    """
    The coordination message inside a verified event.

    Returns the message dict, or None when the event is simply not the
    one being looked for (a different swap or type, or content that does
    not parse). Raises when the *declared* encryption cannot be honoured,
    because that is a security condition rather than a relay artefact:

      * the event declares `nip44_v2` and we cannot read it;
      * the event looks encrypted but declares nothing, while we require
        encryption;
      * the event is plaintext while we require encryption (a downgrade).
    """

    #
    # Filter by the event's own signed type FIRST. The encryption policy
    # is keyed on the message type being looked for, and applying it
    # before this filter would make an unrelated plaintext event (an
    # `offer`, say) look like a downgrade of an `accept`.
    #
    if nostr_event_message_type(event) != message_type:
        return None

    declared = nostr_event_encryption(event)
    raw = event.get("content")

    if not isinstance(raw, str):
        return None

    if declared is not None:
        if declared != NOSTR_ENCRYPTION_TAG:
            raise RuntimeError(
                "SECURITY FAILURE: coordination event declares an "
                f"unsupported encryption {declared!r}"
            )

        if not nostr_encryption_enabled():
            raise RuntimeError(
                "SECURITY FAILURE: coordination event is encrypted but "
                "BLAKE_SWAP_NOSTR_ENCRYPTION is not enabled; refusing to "
                "guess at its content"
            )

        import nip44

        try:
            plaintext = nip44.decrypt(
                raw,
                nostr_conversation_key(event["pubkey"]),
            )
        except nip44.Nip44Error as e:
            raise RuntimeError(
                "SECURITY FAILURE: coordination event declares "
                f"{NOSTR_ENCRYPTION_TAG} but did not decrypt: {e}"
            ) from e

        try:
            message = json.loads(plaintext)
        except Exception as e:
            raise RuntimeError(
                "SECURITY FAILURE: decrypted coordination content is not "
                f"a JSON message: {e}"
            ) from e

    else:
        import nip44

        if nip44.looks_encrypted(raw):
            #
            # A payload with no `encryption` tag. Either the sender is
            # misconfigured or something is stripping the tag; both are
            # refused rather than treated as plaintext.
            #
            raise RuntimeError(
                "SECURITY FAILURE: coordination content looks like a "
                f"{NOSTR_ENCRYPTION_TAG} payload but carries no "
                "encryption tag"
            )

        if nostr_encryption_required(message_type):
            raise RuntimeError(
                "SECURITY FAILURE: refusing a plaintext coordination "
                f"event while BLAKE_SWAP_NOSTR_ENCRYPTION={NOSTR_ENCRYPTION_TAG} "
                "is required (downgrade attempt)"
            )

        try:
            message = json.loads(raw)
        except Exception:
            return None

    if not isinstance(message, dict):
        return None

    if message.get("swap_id") != swap_id:
        return None

    if message.get("type") != message_type:
        return None

    return message


def nostr_send_message(
    swap_id,
    message_type,
    message,
    expected_role=None,
):
    import asyncio

    event = build_nostr_event(
        swap_id,
        message_type,
        message,
        expected_role=expected_role,
    )

    event_id = asyncio.run(
        nostr_publish_event(event)
    )

    print(
        "Nostr event:",
        event_id,
    )

    return event_id


def nostr_receive_message(
    swap_id,
    message_type,
):
    import asyncio

    # Incoming protocol messages after the initial handshake
    # have an already-pinned expected sender.
    sender_rules = {
        "blake-funded": (
            "bob",
            "nostr_alice_pub",
            "Alice",
        ),
        "btc-funded": (
            "alice",
            "nostr_bob_pub",
            "Bob",
        ),
        "btc-claimed": (
            "bob",
            "nostr_alice_pub",
            "Alice",
        ),
        "blake-claimed": (
            "alice",
            "nostr_bob_pub",
            "Bob",
        ),
        "btc-refunded": (
            "alice",
            "nostr_bob_pub",
            "Bob",
        ),
    }

    expected_pubkey = None
    sender_name = None

    if message_type in sender_rules:
        (
            recipient_role,
            expected_field,
            sender_name,
        ) = sender_rules[message_type]

        swap = load_swap(
            swap_id,
            role=recipient_role,
        )

        expected_pubkey = getattr(
            swap,
            expected_field,
        )

        if not expected_pubkey:
            raise RuntimeError(
                f"SECURITY FAILURE: no pinned Nostr "
                f"identity for {sender_name}"
            )

    message, event_id, signer_pubkey = asyncio.run(
        nostr_fetch_message(
            swap_id,
            message_type,
            expected_pubkey=expected_pubkey,
        )
    )

    # Local transport metadata. This was NOT part of the
    # signed event content. It came from the verified event
    # envelope itself.
    message["_nostr_pubkey"] = signer_pubkey

    # Defense in depth: even though the relay query requested
    # the expected author, independently enforce it here.
    if (
        expected_pubkey is not None
        and signer_pubkey != expected_pubkey
    ):
        raise RuntimeError(
            f"SECURITY FAILURE: {message_type} "
            f"was signed by the wrong Nostr identity"
        )

    print(
        "Nostr event:",
        event_id,
    )

    return message, event_id

def send_message(
    swap_id,
    message_type,
    message,
    expected_role=None,
):
    validate_transport_message(
        swap_id,
        message_type,
        message,
    )

    if TRANSPORT == "file":
        return file_send_message(
            swap_id,
            message_type,
            message,
        )

    if TRANSPORT == "nostr":
        return nostr_send_message(
            swap_id,
            message_type,
            message,
            expected_role=expected_role,
        )

    raise ValueError(
        f"Unknown transport backend: {TRANSPORT}"
    )


def receive_message(
    swap_id,
    message_type,
):
    if TRANSPORT == "file":
        message, location = file_receive_message(
            swap_id,
            message_type,
        )

    elif TRANSPORT == "nostr":
        message, location = nostr_receive_message(
            swap_id,
            message_type,
        )

    else:
        raise ValueError(
            f"Unknown transport backend: {TRANSPORT}"
        )

    validate_transport_message(
        swap_id,
        message_type,
        message,
        allow_internal=True,
    )

    return message, location


def command_publish_offer(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )
    
    # Alice creates only Alice-owned keys.
    if not swap.blake_alice_pub:
        swap.blake_alice_pub = get_wallet_pubkey(
            blake_cli,
            BLAKE_WALLET,
        )

    if not swap.btc_alice_pub:
        swap.btc_alice_pub = get_wallet_pubkey(
            btc_cli,
            BTC_WALLET,
        )

    save_swap(
        swap,
        role="alice",
    )
   
    message = {
        "type": "offer",
        "swap_id": swap.swap_id,
        "blake_amount": format_blake_amount(
            swap.blake_amount_base_units
        ),
        "btc_sats": swap.btc_sats,
        "secret_hash": swap.secret_hash,
        "btc_timeout": swap.btc_timeout,
        "blake_timeout": swap.blake_timeout,
        "created_at": swap.created_at,

        # Alice-owned public keys only.
        "blake_alice_pub": swap.blake_alice_pub,
        "btc_alice_pub": swap.btc_alice_pub,
    }

    #
    # v1.5 timeout policy metadata is signed/published with
    # the offer. Bob will independently validate the actual
    # CLTV heights against Bob's own chain tips and policy.
    #
    if (
        swap.timeout_policy_version
        == "1.5"
    ):
        message.update({
            "timeout_policy_version":
                swap.timeout_policy_version,

            "btc_timeout_window_seconds":
                swap.btc_timeout_window_seconds,

            "blake_timeout_window_seconds":
                swap.blake_timeout_window_seconds,

            "bob_recovery_seconds":
                swap.bob_recovery_seconds,
        })

    path = send_message(
        swap.swap_id,
        "offer",
        message,
        expected_role="alice",
    )

    print()
    print("OFFER PUBLISHED")
    print("===============")
    print("swap id:", swap.swap_id)
    print("message:", path)
    print()
    print("PASS: secret_hex was NOT published")


def validate_v15_offer_timeouts(message):
    """
    Bob independently validates Alice's proposed CLTVs.

    Alice's metadata is authenticated evidence, but it is
    NOT the authority. Bob measures the actual advertised
    timeout heights against Bob's own current chain tips and
    Bob's own local TimeoutPolicy.
    """

    version = message.get(
        "timeout_policy_version"
    )

    #
    # Timeout safety is mandatory.  BTC and BLAKE heights belong
    # to independent chains, so there is no safe legacy fallback
    # based on comparing their raw CLTV heights.
    #
    if version is None:
        raise RuntimeError(
            "SECURITY FAILURE: timeout policy version "
            "is required"
        )

    if version != "1.5":
        raise RuntimeError(
            "SECURITY FAILURE: unsupported timeout "
            f"policy version {version!r}"
        )

    required_fields = [
        "btc_timeout",
        "blake_timeout",
        "btc_timeout_window_seconds",
        "blake_timeout_window_seconds",
        "bob_recovery_seconds",
    ]

    for field in required_fields:
        if field not in message:
            raise RuntimeError(
                "SECURITY FAILURE: v1.5 offer missing "
                f"{field}"
            )

    integer_fields = [
        "btc_timeout",
        "blake_timeout",
        "btc_timeout_window_seconds",
        "blake_timeout_window_seconds",
        "bob_recovery_seconds",
    ]

    for field in integer_fields:
        value = message[field]

        if (
            isinstance(value, bool)
            or not isinstance(value, int)
        ):
            raise RuntimeError(
                "SECURITY FAILURE: v1.5 offer field "
                f"{field} must be an integer"
            )

        if value <= 0:
            raise RuntimeError(
                "SECURITY FAILURE: v1.5 offer field "
                f"{field} must be positive"
            )

    policy = TimeoutPolicy()

    btc_height = int(
        btc_cli(
            "getblockcount"
        )
    )

    blake_height = int(
        blake_cli(
            "getblockcount"
        )
    )

    btc_timeout = int(
        message["btc_timeout"]
    )

    blake_timeout = int(
        message["blake_timeout"]
    )

    if btc_timeout <= btc_height:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin timeout "
            "is already expired"
        )

    if blake_timeout <= blake_height:
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE timeout "
            "is already expired"
        )

    #
    # Bob derives his own minimum acceptable policy windows.
    #
    local_minimum = build_timeout_plan(
        btc_height=btc_height,
        blake_height=blake_height,
        policy=policy,
    )

    #
    # Convert Alice's ACTUAL remaining CLTV heights into
    # independently estimated wall-clock windows.
    #
    btc_remaining_seconds = (
        btc_timeout
        - btc_height
    ) * policy.btc_block_seconds

    blake_remaining_seconds = (
        blake_timeout
        - blake_height
    ) * policy.blake_block_seconds

    actual_recovery_gap = (
        blake_remaining_seconds
        - btc_remaining_seconds
    )

    #
    # Alice must leave enough time for Alice's BTC claim path.
    #
    if (
        btc_remaining_seconds
        < local_minimum.btc_window_seconds
    ):
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin timeout "
            "does not leave Bob's locally required "
            "BTC safety window"
        )

    #
    # After Bitcoin's deadline window, Bob must still have
    # his full cross-chain recovery budget on BLAKE.
    #
    if (
        actual_recovery_gap
        < local_minimum.bob_recovery_seconds
    ):
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE timeout "
            "does not leave Bob's locally required "
            "cross-chain recovery window"
        )

    #
    # The signed metadata must also describe a coherent v1.5
    # proposal. Bob still does not TRUST it; the checks above
    # were performed from the actual CLTV heights.
    #
    offered_btc_window = int(
        message[
            "btc_timeout_window_seconds"
        ]
    )

    offered_blake_window = int(
        message[
            "blake_timeout_window_seconds"
        ]
    )

    offered_recovery = int(
        message[
            "bob_recovery_seconds"
        ]
    )

    if (
        offered_btc_window
        < local_minimum.btc_window_seconds
    ):
        raise RuntimeError(
            "SECURITY FAILURE: advertised BTC "
            "safety window is below Bob's policy"
        )

    if (
        offered_recovery
        < local_minimum.bob_recovery_seconds
    ):
        raise RuntimeError(
            "SECURITY FAILURE: advertised Bob "
            "recovery budget is below Bob's policy"
        )

    if (
        offered_blake_window
        - offered_btc_window
        < offered_recovery
    ):
        raise RuntimeError(
            "SECURITY FAILURE: advertised timeout "
            "metadata does not preserve its own "
            "recovery budget"
        )

    return {
        "btc_height": btc_height,
        "blake_height": blake_height,
        "btc_remaining_seconds": (
            btc_remaining_seconds
        ),
        "blake_remaining_seconds": (
            blake_remaining_seconds
        ),
        "actual_recovery_gap": (
            actual_recovery_gap
        ),
        "required_btc_window": (
            local_minimum.btc_window_seconds
        ),
        "required_recovery": (
            local_minimum.bob_recovery_seconds
        ),
    }


def command_accept_offer(args):
    expected_alice_pub = getattr(
        args,
        "expected_alice_pub",
        None,
    )

    expected_blake_amount = getattr(
        args,
        "expected_blake_amount",
        None,
    )

    expected_bob_pub = getattr(
        args,
        "expected_bob_pub",
        None,
    )

    if (
        expected_bob_pub is not None
        and expected_bob_pub != current_nostr_pubkey()
    ):
        raise RuntimeError(
            "SECURITY FAILURE: expected Bob identity "
            "is not the current participant"
        )


    expected_btc_sats = getattr(
        args,
        "expected_btc_sats",
        None,
    )

    expected_bob_pub = getattr(
        args,
        "expected_bob_pub",
        None,
    )

    # A market match has already authenticated Alice.
    # In that case, constrain the Nostr query itself
    # to that exact signer instead of accepting an
    # arbitrary first-contact offer.
    #
    # Over Nostr, an unpinned author means accepting whichever signed
    # offer the relay happens to return first — a public-relay peer
    # could supply it. That must be chosen explicitly, not reached by
    # omission. FILE transport is local and asserted rather than
    # authenticated, so the requirement does not apply there.
    #
    if (
        TRANSPORT == "nostr"
        and expected_alice_pub is None
        and not getattr(args, "allow_unpinned_author", False)
    ):
        raise RuntimeError(
            "SECURITY FAILURE: accepting an offer over Nostr requires "
            "the expected Alice identity. Pass --expected-alice-pub, or "
            "pass --allow-unpinned-author to accept whichever author "
            "the relay returns."
        )

    if (
        TRANSPORT == "nostr"
        and expected_alice_pub is not None
    ):
        import asyncio

        message, path, signer = asyncio.run(
            nostr_fetch_message(
                args.swap_id,
                "offer",
                expected_pubkey=expected_alice_pub,
            )
        )

        message["_nostr_pubkey"] = signer

    else:
        message, path = receive_message(
            args.swap_id,
            "offer",
        )

    if message.get("type") != "offer":
        raise ValueError(
            "Message is not an offer"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: offer contains secret_hex"
        )

    if expected_alice_pub is not None:
        signer = message.get(
            "_nostr_pubkey"
        )

        if signer != expected_alice_pub:
            raise RuntimeError(
                "SECURITY FAILURE: swap offer "
                "was not signed by matched Alice"
            )

    if expected_blake_amount is not None:
        if (
            float(message["blake_amount"])
            != float(expected_blake_amount)
        ):
            raise RuntimeError(
                "SECURITY FAILURE: swap offer "
                "BLAKE amount differs from "
                "market match"
            )

    if expected_btc_sats is not None:
        if (
            int(message["btc_sats"])
            != int(expected_btc_sats)
        ):
            raise RuntimeError(
                "SECURITY FAILURE: swap offer "
                "BTC amount differs from "
                "market match"
            )

    #
    # This is the timeout-security boundary.
    #
    # It runs before Bob creates settlement keys, saves swap
    # state, or transmits an acceptance.
    #
    timeout_validation = (
        validate_v15_offer_timeouts(
            message
        )
    )

    swap = Swap(
        swap_id=message["swap_id"],
        state=State.OFFERED.value,
        created_at=message["created_at"],

        blake_amount_base_units=blake_base_units(
            message["blake_amount"]
        ),
        btc_sats=message["btc_sats"],

        secret_hex=None,
        secret_hash=message["secret_hash"],

        blake_timeout=message["blake_timeout"],
        btc_timeout=message["btc_timeout"],

        timeout_policy_version=message.get(
            "timeout_policy_version"
        ),

        btc_timeout_window_seconds=message.get(
            "btc_timeout_window_seconds"
        ),

        blake_timeout_window_seconds=message.get(
            "blake_timeout_window_seconds"
        ),

        bob_recovery_seconds=message.get(
            "bob_recovery_seconds"
        ),

        blake_alice_pub=message[
            "blake_alice_pub"
        ],

        btc_alice_pub=message[
            "btc_alice_pub"
        ],

        nostr_alice_pub=message.get(
            "_nostr_pubkey"
        ),
        nostr_bob_pub=expected_bob_pub,
    )
    
    # Bob independently records his own local recovery anchors.
    # Alice cannot legitimately fund until Bob's acceptance
    # exists, so Bob need not trust an Alice-supplied height.
    swap.blake_scan_start_height = int(
        blake_cli("getblockcount")
    )

    swap.btc_scan_start_height = int(
        btc_cli("getblockcount")
    )

    # Bob creates only Bob-owned keys.
    swap.blake_bob_pub = get_wallet_pubkey(
        blake_cli,
        BLAKE_WALLET,
    )

    swap.btc_bob_pub = get_wallet_pubkey(
        btc_cli,
        BTC_WALLET,
    )

    actual_bob_pub = current_nostr_pubkey()

    if (
        expected_bob_pub is not None
        and actual_bob_pub != expected_bob_pub
    ):
        raise RuntimeError(
            "SECURITY FAILURE: current participant "
            "does not match authenticated market taker"
        )

    #
    # Bob always binds his own participant identity locally.
    # When a market match supplied expected_bob_pub, the check
    # above proves the current participant is that authenticated
    # taker. FILE-transport tests have no market match, but Bob's
    # own identity is still known and should not remain unset.
    #
    swap.nostr_bob_pub = actual_bob_pub

    save_swap(
        swap,
        role="bob",
    )

    accept_message = {
        "type": "accept",
        "swap_id": swap.swap_id,
        "secret_hash": swap.secret_hash,
        "blake_bob_pub": swap.blake_bob_pub,
        "btc_bob_pub": swap.btc_bob_pub,
    }

    accept_path = send_message(
        swap.swap_id,
        "accept",
        accept_message,
        expected_role="bob",
    )

    print("accept message:", accept_path)

    print()
    print("OFFER ACCEPTED")
    print("==============")
    print("swap id:", swap.swap_id)
    print("Bob state:", swap_path(
        swap.swap_id,
        role="bob",
    ))
    print()
    print("PASS: Bob has hash H")
    print("PASS: Bob does NOT have secret S")

    if timeout_validation is not None:
        print(
            "PASS: Bob independently validated "
            "v1.5 timeout safety"
        )

        print(
            "BTC remaining safety:",
            f"{timeout_validation['btc_remaining_seconds'] / 3600:.2f} h",
        )

        print(
            "cross-chain recovery gap:",
            f"{timeout_validation['actual_recovery_gap'] / 3600:.2f} h",
        )

        print(
            "Bob required recovery:",
            f"{timeout_validation['required_recovery'] / 3600:.2f} h",
        )

def command_receive_accept(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    message, path = receive_message(
        args.swap_id,
        "accept",
    )

    if message.get("type") != "accept":
        raise ValueError(
            "Message is not an accept"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "accept message contains secret_hex"
        )

    if message["swap_id"] != swap.swap_id:
        raise ValueError(
            "swap_id mismatch"
        )

    if message["secret_hash"] != swap.secret_hash:
        raise ValueError(
            "secret_hash mismatch"
        )

    swap.blake_bob_pub = message[
        "blake_bob_pub"
    ]

    swap.btc_bob_pub = message[
        "btc_bob_pub"
    ]

    #
    # Both authenticated Nostr transport and the v2 local FILE
    # test envelope attach sender identity as `_nostr_pubkey`.
    #
    # Nostr requires this metadata because it came from signature
    # verification.  FILE transport is intentionally only a
    # local/regtest backend; its v2 envelope preserves the sending
    # participant identity so black-box tests exercise the same
    # identity-binding state transitions.
    #
    signer = message.get(
        "_nostr_pubkey"
    )

    if TRANSPORT == "nostr" and not signer:
        raise RuntimeError(
            "Nostr acceptance has no verified signer identity"
        )

    if signer is not None:
        if (
            swap.nostr_bob_pub is not None
            and swap.nostr_bob_pub != signer
        ):
            raise RuntimeError(
                "SECURITY FAILURE: acceptance sender identity "
                "differs from pinned Bob identity"
            )

        swap.nostr_bob_pub = signer

    save_swap(
        swap,
        role="alice",
    )

    print()
    print("ACCEPT RECEIVED")
    print("===============")
    print("swap id:", swap.swap_id)
    print()
    print(
        "Bob BLAKE pubkey:",
        swap.blake_bob_pub,
    )
    print(
        "Bob BTC pubkey:  ",
        swap.btc_bob_pub,
    )
    print()
    print(
        "PASS: Alice received only Bob public keys"
    )

def command_publish_blake_funded(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    if State(swap.state) != State.BLAKE_FUNDED:
        raise ValueError(
            "publish-blake-funded requires "
            "Alice state BLAKE_FUNDED"
        )

    if not swap.blake_funding_txid:
        raise ValueError(
            "Alice has no BLAKE funding txid"
        )

    if swap.blake_funding_vout is None:
        raise ValueError(
            "Alice has no BLAKE funding vout"
        )

    message = {
        "type": "blake-funded",
        "swap_id": swap.swap_id,
        "secret_hash": swap.secret_hash,
        "blake_amount": str(
            swap.blake_amount
        ),
        "blake_timeout": swap.blake_timeout,
        "blake_funding_txid":
            swap.blake_funding_txid,
        "blake_funding_vout":
            swap.blake_funding_vout,
    }

    path = send_message(
        swap.swap_id,
        "blake-funded",
        message,
        expected_role="alice",
    )

    print()
    print("BLAKE FUNDING PUBLISHED")
    print("=======================")
    print("swap id:", swap.swap_id)
    print("message:", path)
    print()
    print("PASS: secret_hex was NOT published")

def command_receive_blake_funded(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    require_local_protocol_role(
        swap,
        "bob",
        "BLAKE funding receipt",
    )

    message, path = receive_message(
        args.swap_id,
        "blake-funded",
    )

    if message.get("type") != "blake-funded":
        raise ValueError(
            "Message is not blake-funded"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "BLAKE funding message contains secret_hex"
        )

    if message["swap_id"] != swap.swap_id:
        raise ValueError(
            "swap_id mismatch"
        )

    if message["secret_hash"] != swap.secret_hash:
        raise ValueError(
            "secret_hash mismatch"
        )

    if blake_base_units(
        message["blake_amount"]
    ) != swap.blake_amount_base_units:
        raise ValueError(
            "BLAKE amount mismatch"
        )

    if (
        message["blake_timeout"]
        != swap.blake_timeout
    ):
        raise ValueError(
            "BLAKE timeout mismatch"
        )

    if TRANSPORT == "nostr":
        signer = message.get(
            "_nostr_pubkey"
        )

        if not signer:
            raise RuntimeError(
                "Nostr BLAKE funding message has "
                "no verified signer identity"
            )

        if signer != swap.nostr_alice_pub:
            raise RuntimeError(
                "SECURITY FAILURE: BLAKE funding "
                "announcement was not signed by "
                "the matched Alice"
            )

    incoming_txid = (
        message["blake_funding_txid"]
    )

    incoming_vout = (
        message["blake_funding_vout"]
    )

    # Receiving the exact same authenticated funding
    # announcement more than once is harmless. Nostr relays,
    # reconnects, and retries can legitimately cause replay.
    if (
        swap.blake_funding_txid is not None
        or swap.blake_funding_vout is not None
    ):
        if (
            swap.blake_funding_txid == incoming_txid
            and swap.blake_funding_vout == incoming_vout
        ):
            print()
            print("BLAKE FUNDING ALREADY RECEIVED")
            print("==============================")
            print("swap id:", swap.swap_id)
            print(
                "txid:   ",
                swap.blake_funding_txid,
            )
            print(
                "vout:   ",
                swap.blake_funding_vout,
            )
            print("state:  ", swap.state)
            print()
            print(
                "PASS: duplicate authenticated "
                "funding message is a safe no-op"
            )
            return

        raise RuntimeError(
            "SECURITY FAILURE: conflicting BLAKE "
            "funding message for an already-funded swap"
        )

    # Bind the announced outpoint only in memory first.
    # Do not advance Bob's protocol state until the
    # actual BLAKE-chain output has been independently
    # verified against this swap's contract.
    swap.blake_funding_txid = incoming_txid
    swap.blake_funding_vout = incoming_vout

    verify_blake_contract(
        swap,
        verbose=False,
    )

    # Bob independently derives the confirmed funding block
    # from his own BLAKE node.  Transport identifies the
    # outpoint; the chain supplies the recovery anchor.
    utxo = blake_cli(
        "gettxout",
        swap.blake_funding_txid,
        swap.blake_funding_vout,
    )

    if not utxo:
        raise RuntimeError(
            "BLAKE HTLC disappeared immediately "
            "after successful verification"
        )

    confirmations = int(
        utxo.get("confirmations", 0)
    )

    if confirmations < 1:
        raise RuntimeError(
            "BLAKE funding transaction "
            "is not confirmed"
        )

    tip_height = int(
        blake_cli("getblockcount")
    )

    funding_height = (
        tip_height
        - confirmations
        + 1
    )

    if funding_height < 0:
        raise RuntimeError(
            "Invalid BLAKE funding height "
            "derived from confirmations"
        )

    funding_blockhash = blake_cli(
        "getblockhash",
        funding_height,
    )

    funding_block = blake_cli(
        "getblock",
        funding_blockhash,
        1,
    )

    if swap.blake_funding_txid not in funding_block.get(
        "tx",
        [],
    ):
        raise RuntimeError(
            "SECURITY FAILURE: independently derived "
            "BLAKE funding block does not contain "
            "the announced funding transaction"
        )

    swap.blake_funding_blockhash = (
        funding_blockhash
    )

    swap.transition(
        State.BLAKE_FUNDED
    )

    save_swap(
        swap,
        role="bob",
    )

    print()
    print("BLAKE FUNDING RECEIVED")
    print("======================")
    print("swap id:", swap.swap_id)
    print(
        "txid:   ",
        swap.blake_funding_txid,
    )
    print(
        "vout:   ",
        swap.blake_funding_vout,
    )
    print("state:  ", swap.state)
    print()
    print("PASS: Bob still does NOT know secret S")

def verify_blake_contract(swap, verbose=True):
    if not swap.blake_funding_txid:
        raise ValueError(
            "Swap has no BLAKE funding txid"
        )

    if swap.blake_funding_vout is None:
        raise ValueError(
            "Swap has no BLAKE funding vout"
        )

    desc = get_blake_contract_descriptor(swap)
    expected_address = derive_blake_contract_address(desc)

    utxo = blake_cli(
        "gettxout",
        swap.blake_funding_txid,
        swap.blake_funding_vout,
    )

    if not utxo:
        raise RuntimeError(
            "BLAKE HTLC is already spent or unavailable"
        )

    actual_amount = utxo["value"]

    actual_address = (
        utxo
        .get("scriptPubKey", {})
        .get("address")
    )

    #
    # Compare base units exactly. A tolerance is the wrong instrument
    # here: it accepts a chain output that is a different amount from the
    # one the offer, the message and this record all claim, and 1e-8 of a
    # BLAKE is still a real (if small) quantity. The node reports the
    # output value as a decimal string, so the conversion is exact.
    #
    try:
        actual_base_units = blake_base_units(actual_amount)
    except ValueError as e:
        raise RuntimeError(
            f"BLAKE funding output has an unrepresentable amount: {e}"
        ) from e

    if actual_base_units != swap.blake_amount_base_units:
        raise RuntimeError(
            "Wrong BLAKE amount: expected "
            f"{format_blake_amount(swap.blake_amount_base_units)} "
            f"({swap.blake_amount_base_units} base units), found "
            f"{actual_amount}"
        )

    if actual_address != expected_address:
        raise RuntimeError(
            "BLAKE funding output does not match "
            "the expected HTLC address"
        )

    if utxo.get("confirmations", 0) < 1:
        raise RuntimeError(
            "BLAKE funding transaction is not confirmed"
        )

    if verbose:
        print("VERIFY BLAKE CONTRACT")
        print("=====================")
        print("txid:         ", swap.blake_funding_txid)
        print("vout:         ", swap.blake_funding_vout)
        print("amount:       ", actual_amount)
        print("address:      ", actual_address)
        print("expected:     ", expected_address)
        print("confirmations:", utxo.get("confirmations", 0))
        print()
        print("PASS: BLAKE funding verified")

    return True

def verify_btc_contract(swap, verbose=True):
    if not swap.btc_funding_txid:
        raise ValueError(
            "Swap has no Bitcoin funding txid"
        )

    if swap.btc_funding_vout is None:
        raise ValueError(
            "Swap has no Bitcoin funding vout"
        )

    desc = get_btc_contract_descriptor(swap)
    expected_address = derive_btc_contract_address(desc)

    utxo = btc_cli(
        "gettxout",
        swap.btc_funding_txid,
        swap.btc_funding_vout,
    )

    if not utxo:
        raise RuntimeError(
            "Bitcoin HTLC is already spent or unavailable"
        )

    actual_sats = int(
        Decimal(str(utxo["value"]))
        * Decimal(100_000_000)
    )

    actual_address = (
        utxo
        .get("scriptPubKey", {})
        .get("address")
    )

    if actual_sats != swap.btc_sats:
        raise RuntimeError(
            f"Wrong Bitcoin amount: expected "
            f"{swap.btc_sats} sats, "
            f"found {actual_sats} sats"
        )

    if actual_address != expected_address:
        raise RuntimeError(
            "Bitcoin funding output does not match "
            "the expected HTLC address"
        )

    if utxo.get("confirmations", 0) < 1:
        raise RuntimeError(
            "Bitcoin funding transaction "
            "is not confirmed"
        )

    if verbose:
        print("VERIFY BITCOIN CONTRACT")
        print("=======================")
        print("txid:         ", swap.btc_funding_txid)
        print("vout:         ", swap.btc_funding_vout)
        print("amount sats:  ", actual_sats)
        print("address:      ", actual_address)
        print("expected:     ", expected_address)
        print("confirmations:", utxo.get("confirmations", 0))
        print("SHA256:       ", swap.secret_hash)
        print("Alice claim:  ", swap.btc_alice_pub)
        print("Bob refund:   ", swap.btc_bob_pub)
        print()
        print("PASS: Bitcoin funding verified")

    return True

def command_verify_btc(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    if State(swap.state) != State.BTC_FUNDED:
        raise ValueError(
            "verify-btc requires state BTC_FUNDED"
        )

    verify_btc_contract(
        swap,
        verbose=True,
    )

def read_compact_size(buf, pos):
    """
    Decode one Bitcoin CompactSize integer.

    Bounds-checked and minimality-checked: a truncated or non-canonical
    buffer raises a clean ValueError rather than an IndexError or a
    silent short read. Bitcoin Core's own PSBT parser likewise rejects
    non-minimal encodings.
    """
    if pos < 0 or pos >= len(buf):
        raise ValueError(
            "malformed PSBT: compact-size read past end of buffer"
        )

    first = buf[pos]
    pos += 1

    if first < 253:
        return first, pos

    width = {
        253: 2,
        254: 4,
        255: 8,
    }[first]

    if pos + width > len(buf):
        raise ValueError(
            "malformed PSBT: truncated compact-size integer"
        )

    value = int.from_bytes(
        buf[pos:pos + width],
        "little",
    )

    minimum = {
        2: 253,
        4: 0x10000,
        8: 0x100000000,
    }[width]

    if value < minimum:
        raise ValueError(
            "malformed PSBT: non-minimal compact-size encoding"
        )

    return value, pos + width


def find_map_end(buf, pos):
    """
    Return the offset of the 0x00 terminator of the map at `pos`.

    Bounds-checked: a map that runs past the end of the buffer is a
    malformed PSBT, not an infinite loop or an IndexError.
    """
    while True:
        if pos < 0 or pos >= len(buf):
            raise ValueError(
                "malformed PSBT: map is not terminated"
            )

        keylen, p = read_compact_size(buf, pos)

        if keylen == 0:
            return pos

        pos = p + keylen

        if pos >= len(buf):
            raise ValueError(
                "malformed PSBT: key runs past end of buffer"
            )

        vallen, p = read_compact_size(buf, pos)
        pos = p + vallen


def add_sha256_preimage(
    psbt_b64,
    hash_hex,
    secret_hex,
    input_index=0,
):
    """
    Add a BIP174 PSBT_IN_SHA256 record (the HTLC preimage) to a PSBT.

    Only input 0 is supported, because every settlement transaction this
    implementation builds has exactly one input. The restriction is
    enforced so a future multi-input settlement cannot silently annotate
    the wrong input.
    """
    h = bytes.fromhex(hash_hex)
    secret = bytes.fromhex(secret_hex)

    if len(h) != 32:
        raise ValueError("hash must be 32 bytes")

    if len(secret) != 32:
        raise ValueError("secret must be 32 bytes")

    if hashlib.sha256(secret).digest() != h:
        raise ValueError(
            "SECRET does not hash to HASH"
        )

    raw = bytearray(
        base64.b64decode(psbt_b64)
    )

    if raw[:5] != b"psbt\xff":
        raise ValueError("not a PSBT")

    #
    # This inserts into the FIRST input map only. Every settlement
    # transaction this implementation builds has exactly one input, so
    # that is correct today, but the assumption is enforced rather than
    # implied: a future multi-input settlement must implement
    # input-index selection explicitly instead of silently annotating
    # the wrong input.
    #
    if input_index != 0:
        raise NotImplementedError(
            "PSBT hashlock injection only supports input 0; "
            f"requested input {input_index}. Settlement PSBTs are "
            "single-input by construction."
        )

    global_end = find_map_end(raw, 5)

    input_start = global_end + 1
    input_end = find_map_end(
        raw,
        input_start,
    )

    # BIP174 PSBT_IN_SHA256:
    # key   = 0x0b || 32-byte hash
    # value = 32-byte preimage
    key = bytes([0x0b]) + h

    record = (
        bytes([len(key)])
        + key
        + bytes([len(secret)])
        + secret
    )

    raw[input_end:input_end] = record

    return base64.b64encode(raw).decode()

def command_verify_blake(args):
    role = os.environ.get(
        "BLAKE_SWAP_ROLE",
        "alice",
    )

    if role not in ("alice", "bob"):
        raise ValueError(
            "BLAKE_SWAP_ROLE must be alice or bob"
        )

    swap = load_swap(
        args.swap_id,
        role=role,
    )

    if State(swap.state) != State.BLAKE_FUNDED:
        raise ValueError(
            "verify-blake requires state BLAKE_FUNDED"
        )

    verify_blake_contract(
        swap,
        verbose=True,
    )

@serialized_funding("btc")
def command_fund_btc(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    require_local_protocol_role(
        swap,
        "bob",
        "BTC funding",
    )

    if not swap.btc_alice_pub:
        raise ValueError(
            "Bob has no Bitcoin Alice pubkey"
        )

    if not swap.btc_bob_pub:
        raise ValueError(
            "Bob has no Bitcoin Bob pubkey"
        )

    if State(swap.state) != State.BLAKE_FUNDED:
        raise ValueError(
            "fund-btc requires state BLAKE_FUNDED"
        )

    verify_blake_contract(
        swap,
        verbose=True,
    )

    print()
    print("BLAKE contract is acceptable.")
    print("Preparing Bitcoin HTLC...")
    print()

    btc_height = btc_cli("getblockcount")

    if btc_height >= swap.btc_timeout:
        raise ValueError(
            f"BTC timeout {swap.btc_timeout} "
            f"has already been reached; "
            f"current height={btc_height}"
        )

    # Fresh Bitcoin-side keys for this swap.

    desc = get_btc_contract_descriptor(swap)
    address = derive_btc_contract_address(desc)

    amount_btc = (
        Decimal(swap.btc_sats)
        / Decimal(100_000_000)
    )

    print("BITCOIN CONTRACT")
    print("================")
    print("height:       ", btc_height)
    print("timeout:      ", swap.btc_timeout)
    print("address:      ", address)
    print("amount sats:  ", swap.btc_sats)
    print("SHA256:       ", swap.secret_hash)
    print("Alice pubkey: ", swap.btc_alice_pub)
    print("Bob pubkey:   ", swap.btc_bob_pub)
    print()

    # Before moving money, see whether a previous invocation
    # already funded this exact contract but crashed before saving
    # the canonical swap state.
    recovered = recover_btc_funding(
        swap,
        address,
    )

    if recovered:
        txid, contract_vout, blockhash = recovered

        print(
            "RECOVERY: found existing Bitcoin HTLC funding; "
            "will not fund it again"
        )
    else:
        #
        # Same as the BLAKE side: the watch-only record is written before
        # funds may move, and its failure is reported rather than fatal.
        #
        report_watch_only_import(swap, "btc")

        # Persist intent before funds may move.
        write_funding_intent(
            swap,
            "btc",
            address,
            swap.btc_sats,
        )

        funding_rate, funding_rate_source = funding_fee_rate(
            btc_cli,
            "btc",
        )

        print(
            "Bitcoin funding fee rate:",
            funding_rate,
            "sat/vB",
            f"({funding_rate_source})",
        )

        txid = btc_cli(
            "sendtoaddress",
            address,
            f"{amount_btc:.8f}",
            "",
            "",
            "false",
            "true",
            "null",
            "unset",
            "null",
            str(funding_rate),
            wallet=BTC_WALLET,
        )

        #
        # Money may now have moved, but the outpoint is not yet durable.
        # This is the exact crash window the funding-intent file exists
        # to cover, so expose it to the crash-recovery tests.
        #
        protocol_failpoint(
            "after-btc-funding-broadcast-before-outpoint-record"
        )

        contract_vout, blockhash = (
            locate_wallet_payment_output(
                btc_cli,
                BTC_WALLET,
                txid,
                address,
                swap.btc_sats,
                sats=True,
            )
        )

        record_funding_broadcast(
            swap.swap_id,
            "btc",
            txid,
            contract_vout,
        )

        if not blockhash and auto_mine_enabled():
            mine_btc(1)

            wallet_tx = btc_cli(
                "gettransaction",
                txid,
                wallet=BTC_WALLET,
            )

            blockhash = wallet_tx.get("blockhash")

    record_funding_broadcast(
        swap.swap_id,
        "btc",
        txid,
        contract_vout,
    )

    if not blockhash:
        # Persist the exact broadcast outpoint immediately, while
        # deliberately keeping the protocol checkpoint at BLAKE_FUNDED.
        # Reconciliation can then distinguish a known pending funding
        # transaction from genuinely absent Bitcoin funding.
        swap.btc_funding_txid = txid
        swap.btc_funding_vout = contract_vout
        swap.btc_funding_blockhash = None

        save_swap(
            swap,
            role="bob",
        )

        print()
        print("BITCOIN FUNDING BROADCAST")
        print("=========================")
        print("txid:  ", txid)
        print("vout:  ", contract_vout)
        print("block:  pending")
        print("state: ", swap.state)
        print()
        print(
            "Bitcoin funding is awaiting "
            "active-chain confirmation."
        )
        return

    swap.btc_funding_txid = txid
    swap.btc_funding_vout = contract_vout
    swap.btc_funding_blockhash = blockhash

    swap.transition(
        State.BTC_FUNDED
    )

    save_swap(
        swap,
        role="bob",
    )

    clear_funding_intent(
        swap.swap_id,
        "btc",
    )

    print("BTC FUNDED")
    print("==========")
    print("txid:  ", txid)
    print("vout:  ", contract_vout)
    print("block: ", blockhash)
    print("state: ", swap.state)

def command_publish_btc_funded(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    if State(swap.state) != State.BTC_FUNDED:
        raise ValueError(
            "publish-btc-funded requires "
            "Bob state BTC_FUNDED"
        )

    if not swap.btc_funding_txid:
        raise ValueError(
            "Bob has no BTC funding txid"
        )

    if swap.btc_funding_vout is None:
        raise ValueError(
            "Bob has no BTC funding vout"
        )

    message = {
        "type": "btc-funded",
        "swap_id": swap.swap_id,
        "secret_hash": swap.secret_hash,
        "btc_sats": swap.btc_sats,
        "btc_timeout": swap.btc_timeout,
        "btc_funding_txid":
            swap.btc_funding_txid,
        "btc_funding_vout":
            swap.btc_funding_vout,
    }

    path = send_message(
        swap.swap_id,
        "btc-funded",
        message,
        expected_role="bob",
    )

    print()
    print("BTC FUNDING PUBLISHED")
    print("=====================")
    print("swap id:", swap.swap_id)
    print("message:", path)
    print()
    print("PASS: secret_hex was NOT published")

def command_receive_btc_funded(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    require_local_protocol_role(
        swap,
        "alice",
        "BTC funding receipt",
    )

    message, path = receive_message(
        args.swap_id,
        "btc-funded",
    )

    if message.get("type") != "btc-funded":
        raise ValueError(
            "Message is not btc-funded"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "BTC funding message contains secret_hex"
        )

    if message["swap_id"] != swap.swap_id:
        raise ValueError(
            "swap_id mismatch"
        )

    if message["secret_hash"] != swap.secret_hash:
        raise ValueError(
            "secret_hash mismatch"
        )

    if message["btc_sats"] != swap.btc_sats:
        raise ValueError(
            "BTC amount mismatch"
        )

    if message["btc_timeout"] != swap.btc_timeout:
        raise ValueError(
            "BTC timeout mismatch"
        )

    if TRANSPORT == "nostr":
        signer = message.get(
            "_nostr_pubkey"
        )

        if not signer:
            raise RuntimeError(
                "Nostr BTC funding message has "
                "no verified signer identity"
            )

        if signer != swap.nostr_bob_pub:
            raise RuntimeError(
                "SECURITY FAILURE: BTC funding "
                "announcement was not signed by "
                "the matched Bob"
            )

    incoming_txid = message["btc_funding_txid"]
    incoming_vout = message["btc_funding_vout"]

    if (
        swap.btc_funding_txid is not None
        or swap.btc_funding_vout is not None
    ):
        if (
            swap.btc_funding_txid == incoming_txid
            and swap.btc_funding_vout == incoming_vout
        ):
            print()
            print("BTC FUNDING ALREADY RECEIVED")
            print("============================")
            print("swap id:", swap.swap_id)
            print("txid:   ", swap.btc_funding_txid)
            print("vout:   ", swap.btc_funding_vout)
            print("state:  ", swap.state)
            print()
            print(
                "PASS: duplicate authenticated "
                "BTC funding message is a safe no-op"
            )
            return

        raise RuntimeError(
            "SECURITY FAILURE: conflicting BTC funding "
            "message for an already-funded swap"
        )

    # Bind the announced outpoint only in memory first.
    # Alice must independently verify the actual Bitcoin
    # HTLC before protocol state can advance.
    swap.btc_funding_txid = incoming_txid
    swap.btc_funding_vout = incoming_vout

    verify_btc_contract(
        swap,
        verbose=False,
    )

    # Alice independently derives the confirmed funding block
    # from her own Bitcoin node.  The transport identifies the
    # outpoint; the chain supplies the recovery anchor.
    utxo = btc_cli(
        "gettxout",
        swap.btc_funding_txid,
        swap.btc_funding_vout,
    )

    if not utxo:
        raise RuntimeError(
            "Bitcoin HTLC disappeared immediately "
            "after successful verification"
        )

    confirmations = int(
        utxo.get("confirmations", 0)
    )

    if confirmations < 1:
        raise RuntimeError(
            "Bitcoin funding transaction "
            "is not confirmed"
        )

    tip_height = int(
        btc_cli("getblockcount")
    )

    funding_height = (
        tip_height
        - confirmations
        + 1
    )

    if funding_height < 0:
        raise RuntimeError(
            "Invalid Bitcoin funding height "
            "derived from confirmations"
        )

    funding_blockhash = btc_cli(
        "getblockhash",
        funding_height,
    )

    # Do not merely trust the confirmation arithmetic:
    # prove that this exact transaction is in that block.
    funding_block = btc_cli(
        "getblock",
        funding_blockhash,
        1,
    )

    if swap.btc_funding_txid not in funding_block.get(
        "tx",
        [],
    ):
        raise RuntimeError(
            "SECURITY FAILURE: independently derived "
            "Bitcoin funding block does not contain "
            "the announced funding transaction"
        )

    swap.btc_funding_blockhash = (
        funding_blockhash
    )

    swap.transition(
        State.BTC_FUNDED
    )

    save_swap(
        swap,
        role="alice",
    )

    print()
    print("BTC FUNDING RECEIVED")
    print("====================")
    print("swap id:", swap.swap_id)
    print(
        "txid:   ",
        swap.btc_funding_txid,
    )
    print(
        "vout:   ",
        swap.btc_funding_vout,
    )
    print("state:  ", swap.state)
    print()
    print("PASS: Alice imported public BTC funding facts")

def command_publish_blake_claimed(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    if State(swap.state) != State.BLAKE_CLAIMED:
        raise ValueError(
            "publish-blake-claimed requires "
            "Bob state BLAKE_CLAIMED"
        )

    if not swap.blake_claim_txid:
        raise ValueError(
            "Bob has no BLAKE claim txid"
        )

    # Backward-compatible recovery for claims made before
    # blake_claim_blockhash was persisted.
    if not swap.blake_claim_blockhash:
        wallet_tx = blake_cli(
            "gettransaction",
            swap.blake_claim_txid,
            wallet=BLAKE_WALLET,
        )

        recovered_blockhash = wallet_tx.get("blockhash")

        if not recovered_blockhash:
            raise RuntimeError(
                "Could not recover BLAKE claim blockhash"
            )

        swap.blake_claim_blockhash = recovered_blockhash

        save_swap(
            swap,
            role="bob",
        )

    message = {
        "type": "blake-claimed",
        "swap_id": swap.swap_id,
        "secret_hash": swap.secret_hash,
        "blake_claim_txid": swap.blake_claim_txid,
        "blake_claim_blockhash": swap.blake_claim_blockhash,
    }

    path = send_message(
        swap.swap_id,
        "blake-claimed",
        message,
        expected_role="bob",
    )

    print()
    print("BLAKE CLAIM PUBLISHED")
    print("=====================")
    print("swap id:", swap.swap_id)
    print("txid:   ", swap.blake_claim_txid)
    print("block:  ", swap.blake_claim_blockhash)
    print("message:", path)
    print()
    print("PASS: only public settlement data published")


def command_receive_blake_claimed(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    message, path = receive_message(
        args.swap_id,
        "blake-claimed",
    )

    if message.get("type") != "blake-claimed":
        raise ValueError(
            "Message is not blake-claimed"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "BLAKE claimed message contains secret_hex"
        )

    if message["swap_id"] != swap.swap_id:
        raise ValueError(
            "swap_id mismatch"
        )

    if message["secret_hash"] != swap.secret_hash:
        raise ValueError(
            "secret_hash mismatch"
        )

    if TRANSPORT == "nostr":
        signer = message.get(
            "_nostr_pubkey"
        )

        if not signer:
            raise RuntimeError(
                "Nostr BLAKE claim message has "
                "no verified signer identity"
            )

        if signer != swap.nostr_bob_pub:
            raise RuntimeError(
                "SECURITY FAILURE: BLAKE claim "
                "announcement was not signed by "
                "the matched Bob"
            )

    incoming_txid = message["blake_claim_txid"]
    incoming_blockhash = message["blake_claim_blockhash"]

    duplicate_claim = False

    if (
        swap.blake_claim_txid is not None
        or swap.blake_claim_blockhash is not None
    ):
        if (
            swap.blake_claim_txid == incoming_txid
            and swap.blake_claim_blockhash == incoming_blockhash
        ):
            duplicate_claim = True
        else:
            raise RuntimeError(
                "SECURITY FAILURE: conflicting BLAKE claim "
                "message for an already-recorded claim"
            )

    verify_announced_settlement(
        swap,
        "blake",
        incoming_txid,
        incoming_blockhash,
        "claim",
    )

    if duplicate_claim:
        print()
        print("BLAKE CLAIM ALREADY RECEIVED")
        print("============================")
        print("swap id:", swap.swap_id)
        print("txid:   ", incoming_txid)
        print("block:  ", incoming_blockhash)
        print("state:  ", swap.state)
        print()
        print(
            "PASS: duplicate authenticated BLAKE claim "
            "was independently re-verified on BLAKE"
        )
        return

    swap.blake_claim_txid = incoming_txid
    swap.blake_claim_blockhash = incoming_blockhash

    swap.transition(
        State.BLAKE_CLAIMED
    )

    save_swap(
        swap,
        role="alice",
    )

    print()
    print("BLAKE CLAIM RECEIVED")
    print("====================")
    print("swap id:", swap.swap_id)
    print("txid:   ", swap.blake_claim_txid)
    print("block:  ", swap.blake_claim_blockhash)
    print("state:  ", swap.state)
    print()
    print(
        "PASS: authenticated Bob claim was "
        "independently verified on BLAKE"
    )



def verify_confirmed_btc_claim_for_blake(swap):
    """
    Before Bob spends BLAKE with S, independently prove that
    the exact Bitcoin claim which revealed S is still confirmed
    on Bitcoin's active chain.
    """

    if not swap.btc_claim_txid:
        raise RuntimeError(
            "SECURITY FAILURE: Bob has no Bitcoin claim txid"
        )

    if not swap.btc_claim_blockhash:
        raise RuntimeError(
            "SECURITY FAILURE: Bob has no Bitcoin claim blockhash"
        )

    if not swap.btc_funding_txid:
        raise RuntimeError(
            "SECURITY FAILURE: swap has no Bitcoin funding txid"
        )

    if swap.btc_funding_vout is None:
        raise RuntimeError(
            "SECURITY FAILURE: swap has no Bitcoin funding vout"
        )

    if not swap.revealed_secret_hex:
        raise RuntimeError(
            "SECURITY FAILURE: Bob has no revealed Bitcoin secret"
        )

    tx = btc_cli(
        "getrawtransaction",
        swap.btc_claim_txid,
        True,
        swap.btc_claim_blockhash,
    )

    if tx.get("in_active_chain") is False:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin claim is no longer "
            "on the active chain"
        )

    confirmations = int(
        tx.get("confirmations", 0) or 0
    )

    if confirmations < 1:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin claim is not confirmed "
            "on the active chain"
        )

    htlc_input = None

    for vin in tx.get("vin", []):
        if (
            vin.get("txid") == swap.btc_funding_txid
            and vin.get("vout") == swap.btc_funding_vout
        ):
            htlc_input = vin
            break

    if htlc_input is None:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin claim no longer spends "
            "the expected HTLC"
        )

    witness = htlc_input.get(
        "txinwitness",
        [],
    )

    if swap.revealed_secret_hex not in witness:
        raise RuntimeError(
            "SECURITY FAILURE: confirmed Bitcoin claim does not "
            "contain Bob's revealed secret"
        )

    return True


def _coin_decimal_to_sats(value):
    value = Decimal(str(value))
    sats = value * Decimal(100_000_000)

    if sats != sats.to_integral_value():
        raise ValueError(
            f"amount has sub-satoshi precision: {value}"
        )

    return int(sats)


def _sats_to_coin_decimal(value):
    return (
        Decimal(int(value))
        / Decimal(100_000_000)
    )


def _decoded_tx_vsize(cli, tx_hex):
    decoded = cli(
        "decoderawtransaction",
        tx_hex,
    )

    vsize = int(decoded.get("vsize", 0))

    if vsize < 1:
        raise RuntimeError(
            "finalized transaction has invalid vsize"
        )

    return vsize


def _decoded_spend_fee_sats(
    cli,
    tx_hex,
    input_sats,
):
    decoded = cli(
        "decoderawtransaction",
        tx_hex,
    )

    output_sats = 0

    for output in decoded.get("vout", []):
        output_sats += _coin_decimal_to_sats(
            output["value"]
        )

    fee_sats = int(input_sats) - output_sats

    if fee_sats < 1:
        raise RuntimeError(
            "finalized settlement transaction "
            "does not pay a positive fee"
        )

    return fee_sats


def build_dynamic_settlement_spend(
    *,
    chain,
    cli,
    input_sats,
    build_finalized,
    fee_rate=None,
):
    """
    Build a finalized HTLC spend whose actual vsize agrees with
    the selected fee policy.

    build_finalized(fee_sats) must return fully finalized raw hex.
    It must not broadcast.

    `fee_rate` is the `--fee-rate` parameter, in sat/vB. It outranks
    every environment knob, including the exact-satoshis override: a
    parameter is this invocation's instruction, and the environment is
    the host's standing default.
    """
    input_sats = int(input_sats)

    #
    # A parameter outranks the environment, so the exact-satoshis
    # variable is not even consulted when `--fee-rate` was given. If an
    # operator has set it -- typically as an emergency -- say that it was
    # passed over, because a silently ignored override is worse than no
    # override at all.
    #
    if fee_rate is not None:
        sats_name = settlement_fee_sats_env_name(chain)
        env_sats = settlement_fee_override_sats(chain)

        if env_sats is not None:
            print(
                f"NOTE: {sats_name}={env_sats} is overridden by "
                f"--fee-rate {fee_rate} sat/vB"
            )

        override = None
    else:
        override = settlement_fee_override_sats(chain)

    if override is not None:
        enforce_fee_cap(
            override,
            input_sats,
            max_percent=settlement_fee_cap_percent(),
            absolute_cap_sats=settlement_fee_absolute_cap_sats(),
        )

        tx_hex = build_finalized(override)
        vsize = _decoded_tx_vsize(cli, tx_hex)

        return {
            "hex": tx_hex,
            "fee_sats": override,
            "vsize": vsize,
            "rate_sat_vb": (
                Decimal(override)
                / Decimal(vsize)
            ),
            "source": "override",
        }

    rate, rate_source = settlement_fee_rate(cli, chain, fee_rate)

    if rate_source != "dynamic":
        print(
            f"Settlement fee rate: {rate} sat/vB "
            f"(from {rate_source})"
        )

    # Start deliberately small. The provisional transaction is
    # never broadcast; it exists only to measure the real witness.
    fee_sats = 1

    for _attempt in range(8):
        enforce_fee_cap(
            fee_sats,
            input_sats,
            max_percent=settlement_fee_cap_percent(),
            absolute_cap_sats=settlement_fee_absolute_cap_sats(),
        )

        tx_hex = build_finalized(fee_sats)
        vsize = _decoded_tx_vsize(cli, tx_hex)

        required_fee = fee_for_vsize(
            vsize,
            rate,
        )

        enforce_fee_cap(
            required_fee,
            input_sats,
            max_percent=settlement_fee_cap_percent(),
            absolute_cap_sats=settlement_fee_absolute_cap_sats(),
            vsize=vsize,
        )

        #
        # Converged, or already over the requirement.
        #
        # Comparing for equality is not enough. The fee required by a
        # transaction is measured *from* that transaction, and a change
        # output appearing or disappearing across the dust threshold makes
        # vsize a decreasing step function of the fee: paying more can
        # remove the output, which lowers the requirement, which puts the
        # output back. That two-cycle never reaches equality, and the
        # builder used to give up after eight attempts -- a happy-path
        # settlement failure with nothing wrong on either chain.
        #
        # The transaction being returned is the one that was measured, so
        # its fee covers its own requirement even when the requirement is
        # now a satoshi or two lower. Paying the larger of the two is what
        # any wallet does; the cap above still bounds it.
        #
        if required_fee <= fee_sats:
            return {
                "hex": tx_hex,
                "fee_sats": fee_sats,
                "vsize": vsize,
                "rate_sat_vb": rate,
                "source": rate_source,
            }

        fee_sats = required_fee

    raise RuntimeError(
        "Settlement fee/vsize did not converge "
        "after 8 signing attempts"
    )


@serialized_funding("blake")
def command_claim_blake(args):

    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    require_local_protocol_role(
        swap,
        "bob",
        "BLAKE claim",
    )


    if State(swap.state) != State.BTC_CLAIMED:
        raise ValueError(
            "claim-blake requires state BTC_CLAIMED"
        )

    if not swap.revealed_secret_hex:
        raise ValueError(
            "Bob has not extracted the secret from Bitcoin"
        )

    # Bob verifies that the secret he learned from Bitcoin
    # really satisfies the original hashlock.
    revealed_hash = hashlib.sha256(
        bytes.fromhex(swap.revealed_secret_hex)
    ).hexdigest()

    if revealed_hash != swap.secret_hash:
        raise RuntimeError(
            "Revealed secret does not match swap hash"
        )

    #
    # Knowing S is not sufficient. Before Bob moves BLAKE,
    # the Bitcoin spend which revealed S must still be
    # confirmed on Bitcoin's active chain.
    #
    verify_confirmed_btc_claim_for_blake(
        swap
    )

    #
    # A final claim may have been rolled out of the active chain, removed
    # from the mempool, and then later resurrected by the signing wallet.
    # The reorg rollback deliberately remembers that exact txid without
    # treating it as authoritative settlement.
    #
    if swap.blake_reorg_claim_txid:
        prior_txid = swap.blake_reorg_claim_txid

        entry = transaction_in_mempool(
            blake_cli,
            prior_txid,
        )

        if entry is not None:
            prior_tx = blake_cli(
                "getrawtransaction",
                prior_txid,
                True,
            )

            branch = classify_htlc_spend(
                prior_tx,
                swap.blake_funding_txid,
                swap.blake_funding_vout,
                swap.secret_hash,
            )

            if branch != "claim":
                raise RuntimeError(
                    "SECURITY FAILURE: reorged BLAKE claim txid "
                    "reappeared in mempool using the wrong branch"
                )

            swap.blake_claim_txid = prior_txid
            swap.blake_claim_blockhash = None
            swap.blake_reorg_claim_txid = None

            save_swap(
                swap,
                role="bob",
            )

            print()
            print("BLAKE CLAIM PENDING AFTER REORG")
            print("===============================")
            print("txid:  ", prior_txid)
            print("block:  pending")
            print("state: ", swap.state)
            print()
            print(
                "PASS: exact reorged BLAKE claim "
                "was recognized after wallet rebroadcast"
            )
            return

        #
        # The prior transaction is no longer visible in mempool.
        # If durable signed claim material still exists, bind recovery
        # to the exact reorged txid and rebroadcast that transaction
        # rather than constructing a replacement.
        #
        recovered_reorg = require_recovered_branch(
            swap,
            "blake",
            "claim",
            recorded_txid_override=prior_txid,
        )

        if recovered_reorg == "pending":
            swap.blake_claim_txid = prior_txid
            swap.blake_claim_blockhash = None
            swap.blake_reorg_claim_txid = None

            save_swap(
                swap,
                role="bob",
            )

            print()
            print("BLAKE CLAIM PENDING AFTER REORG")
            print("===============================")
            print("txid:  ", prior_txid)
            print("block:  pending")
            print("state: ", swap.state)
            print()
            print(
                "PASS: exact reorged BLAKE claim "
                "was rebroadcast from durable state"
            )
            return

        if recovered_reorg is not None:
            swap.blake_claim_txid = recovered_reorg["txid"]
            swap.blake_claim_blockhash = recovered_reorg["blockhash"]
            swap.blake_reorg_claim_txid = None

            save_swap(
                swap,
                role="bob",
            )

            return

        #
        # No durable exact transaction can be recovered. Only now drop
        # the hint and permit normal replacement-claim construction.
        #
        swap.blake_reorg_claim_txid = None

        save_swap(
            swap,
            role="bob",
        )

    recovered = require_recovered_branch(
        swap,
        "blake",
        "claim",
    )

    if recovered == "pending":
        print()
        print("BLAKE CLAIM PENDING")
        print("===================")
        print("txid:  ", swap.blake_claim_txid)
        print("block:  pending")
        print("state: ", swap.state)
        return

    if recovered is not None:
        swap.blake_claim_txid = recovered["txid"]
        swap.blake_claim_blockhash = recovered["blockhash"]

        swap.transition(
            State.BLAKE_CLAIMED
        )

        save_swap(
            swap,
            role="bob",
        )

        print("BLAKE CLAIM RECOVERED FROM CHAIN")
        print("===============================")
        print("txid:  ", recovered["txid"])
        print("block: ", recovered["blockhash"])
        print("state: ", swap.state)
        return

    print("Bob verifies BLAKE HTLC is still available...")
    print()

    verify_blake_contract(
        swap,
        verbose=True,
    )

    desc = get_blake_contract_descriptor(swap)

    destination = blake_cli(
        "getnewaddress",
        "",
        "bech32",
        wallet=BLAKE_WALLET,
    )

    amount = blake_amount_decimal(
        swap.blake_amount_base_units
    )
    input_sats = _coin_decimal_to_sats(amount)

    def build_claim_with_fee(fee_sats):
        if fee_sats >= input_sats:
            raise ValueError(
                "BLAKE amount is too small for claim fee"
            )

        output_sats = input_sats - fee_sats
        output_amount = _sats_to_coin_decimal(
            output_sats
        )

        raw = blake_cli(
            "createrawtransaction",
            json.dumps([
                {
                    "txid": swap.blake_funding_txid,
                    "vout": swap.blake_funding_vout,
                }
            ]),
            json.dumps({
                destination: float(output_amount)
            }),
        )

        psbt = blake_cli(
            "converttopsbt",
            raw,
        )

        psbt = blake_cli(
            "utxoupdatepsbt",
            psbt,
            json.dumps([desc]),
        )

        signed = blake_cli(
            "walletprocesspsbt",
            psbt,
            True,
            "ALL",
            True,
            False,
            wallet=BLAKE_WALLET,
        )

        # CRITICAL:
        # This uses the secret Bob extracted from Bitcoin.
        # It does NOT use swap.secret_hex.
        with_secret = add_sha256_preimage(
            signed["psbt"],
            swap.secret_hash,
            swap.revealed_secret_hex,
        )

        finalized = blake_cli(
            "finalizepsbt",
            with_secret,
        )

        if not finalized.get("complete"):
            raise RuntimeError(
                "BLAKE claim PSBT did not finalize"
            )

        return finalized["hex"]

    fee_plan = build_dynamic_settlement_spend(
        chain="blake",
        cli=blake_cli,
        input_sats=input_sats,
        build_finalized=build_claim_with_fee,
        fee_rate=args.fee_rate,
    )

    claim_hex = fee_plan["hex"]
    fee_sats = fee_plan["fee_sats"]
    fee = _sats_to_coin_decimal(fee_sats)
    output_amount = _sats_to_coin_decimal(
        input_sats - fee_sats
    )

    print()
    print("BLAKE CLAIM READY")
    print("=================")
    print("input: ", amount, "BLAKE")
    print("output:", output_amount, "BLAKE")
    print("fee:   ", fee, "BLAKE")
    print("vsize: ", fee_plan["vsize"], "vB")
    print(
        "rate:  ",
        fee_plan["rate_sat_vb"],
        "sat/vB",
    )
    print("source:", fee_plan["source"])
    print()

    #
    # Derive and persist the exact transaction identity BEFORE
    # broadcast.  This closes the crash window between signing and
    # durable recording: after this save, every retry is bound to this
    # exact transaction even if sendrawtransaction or the node dies.
    #
    decoded_claim = blake_cli(
        "decoderawtransaction",
        claim_hex,
    )

    txid = decoded_claim.get("txid")

    if not txid:
        raise RuntimeError(
            "Signed BLAKE claim has no decodable txid"
        )

    actual = classify_htlc_spend(
        decoded_claim,
        swap.blake_funding_txid,
        swap.blake_funding_vout,
        swap.secret_hash,
    )

    if actual != "claim":
        raise RuntimeError(
            "SECURITY FAILURE: signed BLAKE claim "
            f"classified as {actual}"
        )

    swap.blake_claim_txid = txid
    swap.blake_claim_blockhash = None
    swap.blake_claim_hex = claim_hex

    save_swap(
        swap,
        role="bob",
    )

    broadcast_txid = blake_cli(
        "sendrawtransaction",
        claim_hex,
    )

    if broadcast_txid != txid:
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE claim broadcast "
            "returned a different txid"
        )

    _test_crash_after_broadcast("blake-claim-after-broadcast")

    protocol_failpoint(
        "after_blake_claim_broadcast"
    )

    if auto_mine_enabled():
        mine_blake(1)

    recovered = require_recovered_branch(
        swap,
        "blake",
        "claim",
    )

    if recovered == "pending":
        print()
        print("BLAKE CLAIM BROADCAST")
        print("=====================")
        print("txid:  ", txid)
        print("block:  pending")
        print("state: ", swap.state)
        print()
        print(
            "BLAKE claim is awaiting "
            "active-chain confirmation."
        )
        return

    if recovered is None:
        raise RuntimeError(
            "BLAKE claim broadcast is neither pending "
            "nor confirmed"
        )

    swap.blake_claim_txid = recovered["txid"]
    swap.blake_claim_blockhash = recovered["blockhash"]

    swap.transition(
        State.BLAKE_CLAIMED
    )

    save_swap(
        swap,
        role="bob",
    )

    print("BLAKE CLAIMED")
    print("=============")
    print("txid:  ", txid)
    print("block: ", swap.blake_claim_blockhash)
    print("state: ", swap.state)

    # Final sanity check: original HTLC must now be spent.
    utxo = blake_cli(
        "gettxout",
        swap.blake_funding_txid,
        swap.blake_funding_vout,
    )

    if utxo:
        raise RuntimeError(
            "BLAKE HTLC still appears unspent"
        )

    print()
    print("PASS: original BLAKE HTLC is spent")

def verify_confirmed_btc_refund_for_blake(swap):
    """
    Before Alice refunds BLAKE from BTC_REFUNDED state,
    independently prove that Bob's exact Bitcoin refund
    is still confirmed on Bitcoin's active chain.
    """

    if not swap.btc_refund_txid:
        raise RuntimeError(
            "SECURITY FAILURE: Alice has no Bitcoin refund txid"
        )

    if not swap.btc_refund_blockhash:
        raise RuntimeError(
            "SECURITY FAILURE: Alice has no Bitcoin refund blockhash"
        )

    if not swap.btc_funding_txid:
        raise RuntimeError(
            "SECURITY FAILURE: swap has no Bitcoin funding txid"
        )

    if swap.btc_funding_vout is None:
        raise RuntimeError(
            "SECURITY FAILURE: swap has no Bitcoin funding vout"
        )

    tx = btc_cli(
        "getrawtransaction",
        swap.btc_refund_txid,
        True,
        swap.btc_refund_blockhash,
    )

    if tx.get("in_active_chain") is False:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin refund is no longer "
            "on the active chain"
        )

    confirmations = int(
        tx.get("confirmations", 0) or 0
    )

    if confirmations < 1:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin refund is not confirmed "
            "on the active chain"
        )

    spends_htlc = False

    for vin in tx.get("vin", []):
        if (
            vin.get("txid") == swap.btc_funding_txid
            and vin.get("vout") == swap.btc_funding_vout
        ):
            spends_htlc = True
            break

    if not spends_htlc:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin refund no longer spends "
            "the expected HTLC"
        )

    branch = classify_htlc_spend(
        tx,
        swap.btc_funding_txid,
        swap.btc_funding_vout,
        swap.secret_hash,
    )

    if branch != "refund":
        raise RuntimeError(
            "SECURITY FAILURE: recorded Bitcoin refund is not "
            "the refund branch"
        )

    return True


@serialized_funding("blake")
def command_refund_blake(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    require_local_protocol_role(
        swap,
        "alice",
        "BLAKE refund",
    )

    if State(swap.state) not in {
        State.BLAKE_FUNDED,
        State.BTC_REFUNDED,
    }:
        raise ValueError(
            "refund-blake requires state "
            "BLAKE_FUNDED or BTC_REFUNDED"
        )

    if State(swap.state) == State.BTC_REFUNDED:
        #
        # Stale local BTC_REFUNDED state is not sufficient
        # authority to move BLAKE. Re-prove Bob's refund
        # against Bitcoin's active chain immediately before
        # constructing/broadcasting Alice's BLAKE refund.
        #
        verify_confirmed_btc_refund_for_blake(
            swap
        )

    if not swap.blake_funding_txid:
        raise ValueError(
            "Swap has no BLAKE funding txid"
        )

    if swap.blake_funding_vout is None:
        raise ValueError(
            "Swap has no BLAKE funding vout"
        )

    #
    # A final refund may have been rolled out of the active chain,
    # disappeared from mempool, and later been rebroadcast by the
    # signing wallet. Recognize that exact prior transaction before
    # treating the HTLC as unavailable.
    #
    if swap.blake_reorg_refund_txid:
        prior_txid = swap.blake_reorg_refund_txid

        entry = transaction_in_mempool(
            blake_cli,
            prior_txid,
        )

        if entry is not None:
            prior_tx = blake_cli(
                "getrawtransaction",
                prior_txid,
                True,
            )

            branch = classify_htlc_spend(
                prior_tx,
                swap.blake_funding_txid,
                swap.blake_funding_vout,
                swap.secret_hash,
            )

            if branch != "refund":
                raise RuntimeError(
                    "SECURITY FAILURE: reorged BLAKE refund txid "
                    "reappeared in mempool using the wrong branch"
                )

            swap.blake_refund_txid = prior_txid
            swap.blake_refund_blockhash = None
            swap.blake_reorg_refund_txid = None

            save_swap(
                swap,
                role="alice",
            )

            print()
            print("BLAKE REFUND PENDING AFTER REORG")
            print("================================")
            print("txid:  ", prior_txid)
            print("block:  pending")
            print("state: ", swap.state)
            print()
            print(
                "PASS: exact reorged BLAKE refund "
                "was recognized after wallet rebroadcast"
            )
            return

        #
        # The prior refund is no longer visible in mempool.
        # If durable signed refund material still exists, bind recovery
        # to the exact reorged txid and rebroadcast that transaction
        # rather than constructing a replacement.
        #
        recovered_reorg = require_recovered_branch(
            swap,
            "blake",
            "refund",
            recorded_txid_override=prior_txid,
        )

        if recovered_reorg == "pending":
            swap.blake_refund_txid = prior_txid
            swap.blake_refund_blockhash = None
            swap.blake_reorg_refund_txid = None

            save_swap(
                swap,
                role="alice",
            )

            print()
            print("BLAKE REFUND PENDING AFTER REORG")
            print("================================")
            print("txid:  ", prior_txid)
            print("block:  pending")
            print("state: ", swap.state)
            print()
            print(
                "PASS: exact reorged BLAKE refund "
                "was rebroadcast from durable state"
            )
            return

        if recovered_reorg is not None:
            swap.blake_refund_txid = recovered_reorg["txid"]
            swap.blake_refund_blockhash = recovered_reorg["blockhash"]
            swap.blake_reorg_refund_txid = None

            save_swap(
                swap,
                role="alice",
            )

            return

        #
        # No durable exact transaction can be recovered. Only now drop
        # the hint and permit normal replacement-refund construction.
        #
        swap.blake_reorg_refund_txid = None

        save_swap(
            swap,
            role="alice",
        )

    recovered = require_recovered_branch(
        swap,
        "blake",
        "refund",
    )

    if recovered == "pending":
        print()
        print("BLAKE REFUND PENDING")
        print("====================")
        print("txid:  ", swap.blake_refund_txid)
        print("block:  pending")
        print("state: ", swap.state)
        return

    if recovered is not None:
        swap.blake_refund_txid = recovered["txid"]
        swap.blake_refund_blockhash = recovered["blockhash"]

        swap.transition(
            State.BLAKE_REFUNDED
        )

        save_swap(
            swap,
            role="alice",
        )

        print("BLAKE REFUND RECOVERED FROM CHAIN")
        print("=================================")
        print("txid:  ", recovered["txid"])
        print("block: ", recovered["blockhash"])
        print("state: ", swap.state)
        return

    if State(swap.state) == State.BLAKE_FUNDED:
        #
        # Abort-before-BTC-funding recovery.
        #
        # Confirmed-spend recovery above runs first so a crash
        # after broadcasting/mining our exact refund remains
        # idempotently recoverable.
        #
        # If no confirmed refund exists, a stale BLAKE_FUNDED
        # checkpoint is not sufficient authority to refund.
        # Immediately re-prove:
        #
        #   1. Bitcoin funding does not exist.
        #   2. The expected BLAKE HTLC is still funded/unspent.
        #   3. The BLAKE refund timelock is mature.
        #
        btc_obs = _reconcile_observe_htlc(
            swap,
            "btc",
        )
        if btc_obs.get("status") != "absent":
            raise RuntimeError(
                "SECURITY FAILURE: BLAKE_FUNDED refund requires "
                "Bitcoin funding to be independently absent; "
                f"observed {btc_obs.get('status')}"
            )

        blake_obs = _reconcile_observe_htlc(
            swap,
            "blake",
        )
        if blake_obs.get("status") != "funded":
            raise RuntimeError(
                "SECURITY FAILURE: BLAKE_FUNDED refund requires "
                "the expected BLAKE HTLC to remain confirmed "
                "and unspent; "
                f"observed {blake_obs.get('status')}"
            )

        blake_height = int(
            blake_cli("getblockcount")
        )
        if blake_height < int(swap.blake_timeout):
            raise RuntimeError(
                "SECURITY FAILURE: BLAKE refund timelock "
                "is not mature"
            )

    verify_blake_contract(
        swap,
        verbose=True,
    )

    current_height = blake_cli(
        "getblockcount"
    )

    amount = blake_amount_decimal(
        swap.blake_amount_base_units
    )
    input_sats = _coin_decimal_to_sats(amount)

    refund_path = (
        BASE_SWAP_DIR
        / f"{swap.swap_id}.blake-refund.hex"
    )

    #
    # If Alice already prepared the refund,
    # reuse the exact same signed transaction.
    #
    if refund_path.exists():
        refund_hex = refund_path.read_text().strip()

        fee_sats = _decoded_spend_fee_sats(
            blake_cli,
            refund_hex,
            input_sats,
        )

        fee_plan = {
            "hex": refund_hex,
            "fee_sats": fee_sats,
            "vsize": _decoded_tx_vsize(
                blake_cli,
                refund_hex,
            ),
            "rate_sat_vb": (
                Decimal(fee_sats)
                / Decimal(
                    _decoded_tx_vsize(
                        blake_cli,
                        refund_hex,
                    )
                )
            ),
            "source": "saved",
        }

        print()
        print("Reusing saved signed BLAKE refund:")
        print(refund_path)

    else:
        desc = get_blake_contract_descriptor(swap)

        destination = blake_cli(
            "getnewaddress",
            "",
            "bech32",
            wallet=BLAKE_WALLET,
        )

        inputs = [
            {
                "txid": swap.blake_funding_txid,
                "vout": swap.blake_funding_vout,
                "sequence": 0xfffffffe,
            }
        ]

        def build_refund_with_fee(fee_sats):
            if fee_sats >= input_sats:
                raise ValueError(
                    "BLAKE amount is too small for refund fee"
                )

            output_sats = input_sats - fee_sats
            output_amount = _sats_to_coin_decimal(
                output_sats
            )

            outputs = {
                destination: float(output_amount)
            }

            raw = blake_cli(
                "createrawtransaction",
                json.dumps(inputs),
                json.dumps(outputs),
                swap.blake_timeout,
            )

            psbt = blake_cli(
                "converttopsbt",
                raw,
            )

            psbt = blake_cli(
                "utxoupdatepsbt",
                psbt,
                json.dumps([desc]),
            )

            signed = blake_cli(
                "walletprocesspsbt",
                psbt,
                True,
                "ALL",
                True,
                False,
                wallet=BLAKE_WALLET,
            )

            finalized = blake_cli(
                "finalizepsbt",
                signed["psbt"],
            )

            if not finalized.get("complete"):
                raise RuntimeError(
                    "BLAKE refund PSBT did not finalize"
                )

            return finalized["hex"]

        fee_plan = build_dynamic_settlement_spend(
            chain="blake",
            cli=blake_cli,
            input_sats=input_sats,
            build_finalized=build_refund_with_fee,
            fee_rate=args.fee_rate,
        )

        refund_hex = fee_plan["hex"]

        refund_path.write_text(
            refund_hex + "\n"
        )

        print()
        print("Saved signed BLAKE refund:")
        print(refund_path)

    print()
    print("BLAKE REFUND READY")
    print("==================")
    print("current height:", current_height)
    print("refund height: ", swap.blake_timeout)
    fee_sats = fee_plan["fee_sats"]
    fee = _sats_to_coin_decimal(fee_sats)
    output_amount = _sats_to_coin_decimal(
        input_sats - fee_sats
    )

    print("input:         ", amount, "BLAKE")
    print("output:        ", output_amount, "BLAKE")
    print("fee:           ", fee, "BLAKE")
    print("vsize:         ", fee_plan["vsize"], "vB")
    print(
        "rate:          ",
        fee_plan["rate_sat_vb"],
        "sat/vB",
    )
    print("fee source:    ", fee_plan["source"])

    if current_height < swap.blake_timeout:
        print()
        print("Refund is correctly timelocked.")
        return

    #
    # Derive and persist the exact transaction identity BEFORE
    # broadcast. This closes the crash window between signing and
    # durable recording: every retry is now bound to this exact
    # refund even if sendrawtransaction or the node dies.
    #
    decoded_refund = blake_cli(
        "decoderawtransaction",
        refund_hex,
    )

    txid = decoded_refund.get("txid")

    if not txid:
        raise RuntimeError(
            "Signed BLAKE refund has no decodable txid"
        )

    actual = classify_htlc_spend(
        decoded_refund,
        swap.blake_funding_txid,
        swap.blake_funding_vout,
        swap.secret_hash,
    )

    if actual != "refund":
        raise RuntimeError(
            "SECURITY FAILURE: signed BLAKE refund "
            f"classified as {actual}"
        )

    swap.blake_refund_txid = txid
    swap.blake_refund_blockhash = None
    swap.blake_refund_hex = refund_hex

    save_swap(
        swap,
        role="alice",
    )

    broadcast_txid = blake_cli(
        "sendrawtransaction",
        refund_hex,
    )

    if broadcast_txid != txid:
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE refund broadcast "
            "returned a different txid"
        )

    _test_crash_after_broadcast("blake-refund-after-broadcast")

    protocol_failpoint(
        "after_blake_refund_broadcast"
    )

    if auto_mine_enabled():
        mine_blake(1)

    recovered = require_recovered_branch(
        swap,
        "blake",
        "refund",
    )

    if recovered == "pending":
        print()
        print("BLAKE REFUND BROADCAST")
        print("======================")
        print("txid:  ", txid)
        print("block:  pending")
        print("state: ", swap.state)
        print()
        print(
            "BLAKE refund is awaiting "
            "active-chain confirmation."
        )
        return

    if recovered is None:
        raise RuntimeError(
            "BLAKE refund broadcast is neither pending "
            "nor confirmed"
        )

    swap.blake_refund_txid = recovered["txid"]
    swap.blake_refund_blockhash = recovered["blockhash"]

    swap.transition(
        State.BLAKE_REFUNDED
    )

    save_swap(
        swap,
        role="alice",
    )

    print()
    print("BLAKE REFUNDED")
    print("==============")
    print("txid:  ", txid)
    print("block: ", swap.blake_refund_blockhash)
    print("state: ", swap.state)

def command_receive_btc_refunded(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    message, path = receive_message(
        args.swap_id,
        "btc-refunded",
    )

    if message.get("type") != "btc-refunded":
        raise ValueError(
            "Message is not btc-refunded"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "BTC refunded message contains secret_hex"
        )

    if message["swap_id"] != swap.swap_id:
        raise ValueError("swap_id mismatch")

    if message["secret_hash"] != swap.secret_hash:
        raise ValueError("secret_hash mismatch")

    txid = message["btc_refund_txid"]
    blockhash = message["btc_refund_blockhash"]

    duplicate_refund = False

    if (
        swap.btc_refund_txid is not None
        or swap.btc_refund_blockhash is not None
    ):
        if (
            swap.btc_refund_txid == txid
            and swap.btc_refund_blockhash == blockhash
        ):
            duplicate_refund = True
        else:
            raise RuntimeError(
                "SECURITY FAILURE: conflicting BTC refund "
                "message for an already-recorded refund"
            )

    verify_announced_settlement(
        swap,
        "btc",
        txid,
        blockhash,
        "refund",
    )

    if duplicate_refund:
        print()
        print("BTC REFUND ALREADY RECEIVED")
        print("===========================")
        print("swap id:", swap.swap_id)
        print("txid:   ", txid)
        print("block:  ", blockhash)
        print("state:  ", swap.state)
        print()
        print(
            "PASS: duplicate authenticated BTC refund "
            "was independently re-verified on Bitcoin"
        )
        return

    swap.btc_refund_txid = txid
    swap.btc_refund_blockhash = blockhash

    swap.transition(
        State.BTC_REFUNDED
    )

    save_swap(
        swap,
        role="alice",
    )

    print()
    print("BTC REFUND RECEIVED")
    print("===================")
    print("swap id:", swap.swap_id)
    print("txid:   ", txid)
    print("block:  ", blockhash)
    print("state:  ", swap.state)
    print()
    print("PASS: Alice independently verified BTC refund")

def command_publish_btc_refunded(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    if State(swap.state) != State.BTC_REFUNDED:
        raise ValueError(
            "publish-btc-refunded requires "
            "Bob state BTC_REFUNDED"
        )

    if not swap.btc_refund_txid:
        raise ValueError(
            "Bob has no BTC refund txid"
        )

    if not swap.btc_refund_blockhash:
        raise ValueError(
            "Bob has no BTC refund blockhash"
        )

    message = {
        "type": "btc-refunded",
        "swap_id": swap.swap_id,
        "secret_hash": swap.secret_hash,
        "btc_refund_txid": swap.btc_refund_txid,
        "btc_refund_blockhash": swap.btc_refund_blockhash,
    }

    path = send_message(
        swap.swap_id,
        "btc-refunded",
        message,
        expected_role="bob",
    )

    print()
    print("BTC REFUND PUBLISHED")
    print("====================")
    print("swap id:", swap.swap_id)
    print("txid:   ", swap.btc_refund_txid)
    print("block:  ", swap.btc_refund_blockhash)
    print("message:", path)
    print()
    print("PASS: only public refund data published")

def command_publish_btc_claimed(args):
    swap = load_swap(
        args.swap_id,
        role="alice",
    )

    if State(swap.state) != State.BTC_CLAIMED:
        raise ValueError(
            "publish-btc-claimed requires "
            "Alice state BTC_CLAIMED"
        )

    if not swap.btc_claim_txid:
        raise ValueError(
            "Alice has no BTC claim txid"
        )

    if not swap.btc_claim_blockhash:
        raise ValueError(
            "Alice has no BTC claim blockhash"
        )

    message = {
        "type": "btc-claimed",
        "swap_id": swap.swap_id,
        "secret_hash": swap.secret_hash,
        "btc_claim_txid": swap.btc_claim_txid,
        "btc_claim_blockhash": swap.btc_claim_blockhash,
    }

    path = send_message(
        swap.swap_id,
        "btc-claimed",
        message,
        expected_role="alice",
    )

    print()
    print("BTC CLAIM PUBLISHED")
    print("===================")
    print("swap id:", swap.swap_id)
    print("txid:   ", swap.btc_claim_txid)
    print("block:  ", swap.btc_claim_blockhash)
    print("message:", path)
    print()
    print("PASS: secret_hex was NOT published")

def command_receive_btc_claimed(args):
    swap = load_swap(
        args.swap_id,
        role="bob",
    )

    message, path = receive_message(
        args.swap_id,
        "btc-claimed",
    )

    if message.get("type") != "btc-claimed":
        raise ValueError(
            "Message is not btc-claimed"
        )

    if "secret_hex" in message:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "BTC claimed message contains secret_hex"
        )

    if message["swap_id"] != swap.swap_id:
        raise ValueError(
            "swap_id mismatch"
        )

    if message["secret_hash"] != swap.secret_hash:
        raise ValueError(
            "secret_hash mismatch"
        )

    if TRANSPORT == "nostr":
        signer = message.get(
            "_nostr_pubkey"
        )

        if not signer:
            raise RuntimeError(
                "Nostr BTC claim message has "
                "no verified signer identity"
            )

        if signer != swap.nostr_alice_pub:
            raise RuntimeError(
                "SECURITY FAILURE: BTC claim "
                "announcement was not signed by "
                "the matched Alice"
            )

    incoming_txid = message["btc_claim_txid"]
    incoming_blockhash = message["btc_claim_blockhash"]

    duplicate_claim = False

    if (
        swap.btc_claim_txid is not None
        or swap.btc_claim_blockhash is not None
    ):
        if (
            swap.btc_claim_txid == incoming_txid
            and swap.btc_claim_blockhash == incoming_blockhash
        ):
            duplicate_claim = True
        else:
            raise RuntimeError(
                "SECURITY FAILURE: conflicting BTC claim "
                "message for an already-recorded claim"
            )

    verify_announced_settlement(
        swap,
        "btc",
        incoming_txid,
        incoming_blockhash,
        "claim",
    )

    if duplicate_claim:
        print()
        print("BTC CLAIM ALREADY RECEIVED")
        print("==========================")
        print("swap id:", swap.swap_id)
        print("txid:   ", incoming_txid)
        print("block:  ", incoming_blockhash)
        print("state:  ", swap.state)
        print()
        print(
            "PASS: duplicate authenticated BTC claim "
            "was independently re-verified on Bitcoin"
        )
        return

    # Only persist the announcement after Bitcoin itself
    # confirms that this transaction spends Bob's HTLC.
    swap.btc_claim_txid = incoming_txid
    swap.btc_claim_blockhash = incoming_blockhash

    # Preserve the existing proven state-machine ordering.
    # Actual S is still learned independently by extract-secret.
    swap.transition(
        State.SECRET_REVEALED
    )

    swap.transition(
        State.BTC_CLAIMED
    )

    save_swap(
        swap,
        role="bob",
    )

    print()
    print("BTC CLAIM RECEIVED")
    print("==================")
    print("swap id:", swap.swap_id)
    print("txid:   ", swap.btc_claim_txid)
    print("block:  ", swap.btc_claim_blockhash)
    print("state:  ", swap.state)
    print()
    print(
        "PASS: authenticated Alice claim was "
        "independently verified on Bitcoin"
    )
    print(
        "PASS: secret_hex was not transported; "
        "Bob must extract S from the witness"
    )


@serialized_funding("btc")
def command_claim_btc(args):
    swap = load_swap(args.swap_id, role="alice")

    require_local_protocol_role(
        swap,
        "alice",
        "BTC claim",
    )

    if State(swap.state) != State.BTC_FUNDED:
        raise ValueError(
            "claim-btc requires state BTC_FUNDED"
        )

    recovered = require_recovered_branch(
        swap,
        "btc",
        "claim",
    )

    if recovered == "pending":
        print()
        print("BTC CLAIM PENDING")
        print("=================")
        print("txid:  ", swap.btc_claim_txid)
        print("block:  pending")
        print("state: ", swap.state)
        return

    if recovered is not None:
        swap.btc_claim_txid = recovered["txid"]
        swap.btc_claim_blockhash = recovered["blockhash"]

        swap.transition(
            State.SECRET_REVEALED
        )

        swap.transition(
            State.BTC_CLAIMED
        )

        save_swap(
            swap,
            role="alice",
        )

        print("BTC CLAIM RECOVERED FROM CHAIN")
        print("==============================")
        print("txid:  ", recovered["txid"])
        print("block: ", recovered["blockhash"])
        print("state: ", swap.state)
        return

    print("Alice verifies Bitcoin HTLC...")
    print()

    verify_btc_contract(
        swap,
        verbose=True,
    )

    desc = get_btc_contract_descriptor(swap)

    destination = btc_cli(
        "getnewaddress",
        "",
        "bech32",
        wallet=BTC_WALLET,
    )

    input_sats = int(swap.btc_sats)

    def build_claim_with_fee(fee_sats):
        if fee_sats >= input_sats:
            raise ValueError(
                "BTC amount is too small for claim fee"
            )

        output_sats = input_sats - fee_sats
        output_btc = _sats_to_coin_decimal(
            output_sats
        )

        raw = btc_cli(
            "createrawtransaction",
            json.dumps([
                {
                    "txid": swap.btc_funding_txid,
                    "vout": swap.btc_funding_vout,
                }
            ]),
            json.dumps({
                destination: float(output_btc)
            }),
        )

        psbt = btc_cli(
            "converttopsbt",
            raw,
        )

        psbt = btc_cli(
            "utxoupdatepsbt",
            psbt,
            json.dumps([desc]),
        )

        signed = btc_cli(
            "walletprocesspsbt",
            psbt,
            True,
            "ALL",
            True,
            False,
            wallet=BTC_WALLET,
        )

        with_secret = add_sha256_preimage(
            signed["psbt"],
            swap.secret_hash,
            swap.secret_hex,
        )

        finalized = btc_cli(
            "finalizepsbt",
            with_secret,
        )

        if not finalized.get("complete"):
            raise RuntimeError(
                "Bitcoin claim PSBT did not finalize"
            )

        return finalized["hex"]

    fee_plan = build_dynamic_settlement_spend(
        chain="btc",
        cli=btc_cli,
        input_sats=input_sats,
        build_finalized=build_claim_with_fee,
        fee_rate=args.fee_rate,
    )

    claim_hex = fee_plan["hex"]
    fee_sats = fee_plan["fee_sats"]
    output_sats = input_sats - fee_sats

    print()
    print("BTC CLAIM READY")
    print("===============")
    print("input sats: ", swap.btc_sats)
    print("output sats:", output_sats)
    print("fee sats:   ", fee_sats)
    print("vsize:      ", fee_plan["vsize"], "vB")
    print(
        "rate:       ",
        fee_plan["rate_sat_vb"],
        "sat/vB",
    )
    print("fee source:  ", fee_plan["source"])
    print()

    #
    # Derive and persist the exact transaction identity BEFORE
    # broadcast. Broadcasting exposes S in the Bitcoin mempool,
    # but durable protocol state remains BTC_FUNDED until the
    # exact transaction is confirmed on the active chain.
    #
    decoded_claim = btc_cli(
        "decoderawtransaction",
        claim_hex,
    )

    txid = decoded_claim.get("txid")

    if not txid:
        raise RuntimeError(
            "Signed Bitcoin claim has no decodable txid"
        )

    actual = classify_htlc_spend(
        decoded_claim,
        swap.btc_funding_txid,
        swap.btc_funding_vout,
        swap.secret_hash,
    )

    if actual != "claim":
        raise RuntimeError(
            "SECURITY FAILURE: signed Bitcoin claim "
            f"classified as {actual}"
        )

    swap.btc_claim_txid = txid
    swap.btc_claim_blockhash = None
    swap.btc_claim_hex = claim_hex

    save_swap(
        swap,
        role="alice",
    )

    broadcast_txid = btc_cli(
        "sendrawtransaction",
        claim_hex,
    )

    if broadcast_txid != txid:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin claim broadcast "
            "returned a different txid"
        )

    _test_crash_after_broadcast("btc-claim-after-broadcast")

    protocol_failpoint(
        "after_btc_claim_broadcast"
    )

    if auto_mine_enabled():
        mine_btc(1)

    recovered = require_recovered_branch(
        swap,
        "btc",
        "claim",
    )

    if recovered == "pending":
        print()
        print("BTC CLAIM BROADCAST")
        print("===================")
        print("txid:  ", txid)
        print("block:  pending")
        print("state: ", swap.state)
        print()
        print(
            "Bitcoin claim is awaiting "
            "active-chain confirmation."
        )
        return

    if recovered is None:
        raise RuntimeError(
            "Bitcoin claim broadcast is neither pending "
            "nor confirmed"
        )

    swap.btc_claim_txid = recovered["txid"]
    swap.btc_claim_blockhash = recovered["blockhash"]

    # Only confirmed Bitcoin settlement advances the durable
    # protocol state through SECRET_REVEALED -> BTC_CLAIMED.
    swap.transition(
        State.SECRET_REVEALED
    )

    swap.transition(
        State.BTC_CLAIMED
    )

    save_swap(
        swap,
        role="alice",
    )

    print("BTC CLAIMED")
    print("===========")
    print("txid:  ", txid)
    print("block: ", swap.btc_claim_blockhash)
    print("state: ", swap.state)

@serialized_funding("btc")
def command_refund_btc(args):
    swap = load_swap(args.swap_id,role="bob")

    require_local_protocol_role(
        swap,
        "bob",
        "BTC refund",
    )

    if State(swap.state) != State.BTC_FUNDED:
        raise ValueError(
            "refund-btc requires state BTC_FUNDED"
        )

    if not swap.btc_funding_txid:
        raise ValueError(
            "Swap has no Bitcoin funding txid"
        )

    if swap.btc_funding_vout is None:
        raise ValueError(
            "Swap has no Bitcoin funding vout"
        )

    recovered = require_recovered_branch(
        swap,
        "btc",
        "refund",
    )

    if recovered == "pending":
        print()
        print("BTC REFUND PENDING")
        print("==================")
        print("txid:  ", swap.btc_refund_txid)
        print("block:  pending")
        print("state: ", swap.state)
        return

    if recovered is not None:
        swap.btc_refund_txid = recovered["txid"]
        swap.btc_refund_blockhash = recovered["blockhash"]

        swap.transition(
            State.BTC_REFUNDED
        )

        save_swap(
            swap,
            role="bob",
        )

        print("BTC REFUND RECOVERED FROM CHAIN")
        print("===============================")
        print("txid:  ", recovered["txid"])
        print("block: ", recovered["blockhash"])
        print("state: ", swap.state)
        return

    verify_btc_contract(
        swap,
        verbose=True,
    )

    current_height = btc_cli(
        "getblockcount"
    )

    refund_path = (
        BASE_SWAP_DIR
        / f"{swap.swap_id}.btc-refund.hex"
    )

    #
    # If we've already constructed the refund,
    # reuse the exact same signed transaction.
    #
    if refund_path.exists():
        refund_hex = refund_path.read_text().strip()

        input_sats = int(swap.btc_sats)

        fee_sats = _decoded_spend_fee_sats(
            btc_cli,
            refund_hex,
            input_sats,
        )

        vsize = _decoded_tx_vsize(
            btc_cli,
            refund_hex,
        )

        fee_plan = {
            "hex": refund_hex,
            "fee_sats": fee_sats,
            "vsize": vsize,
            "rate_sat_vb": (
                Decimal(fee_sats)
                / Decimal(vsize)
            ),
            "source": "saved",
        }

        print()
        print("Reusing saved signed Bitcoin refund:")
        print(refund_path)

    else:
        desc = get_btc_contract_descriptor(swap)

        destination = btc_cli(
            "getnewaddress",
            "",
            "bech32",
            wallet=BTC_WALLET,
        )

        input_sats = int(swap.btc_sats)

        inputs = [
            {
                "txid": swap.btc_funding_txid,
                "vout": swap.btc_funding_vout,
                "sequence": 0xfffffffe,
            }
        ]

        def build_refund_with_fee(fee_sats):
            if fee_sats >= input_sats:
                raise ValueError(
                    "BTC amount is too small for refund fee"
                )

            output_sats = input_sats - fee_sats
            output_btc = _sats_to_coin_decimal(
                output_sats
            )

            outputs = {
                destination: float(output_btc)
            }

            raw = btc_cli(
                "createrawtransaction",
                json.dumps(inputs),
                json.dumps(outputs),
                swap.btc_timeout,
            )

            psbt = btc_cli(
                "converttopsbt",
                raw,
            )

            psbt = btc_cli(
                "utxoupdatepsbt",
                psbt,
                json.dumps([desc]),
            )

            signed = btc_cli(
                "walletprocesspsbt",
                psbt,
                True,
                "ALL",
                True,
                False,
                wallet=BTC_WALLET,
            )

            finalized = btc_cli(
                "finalizepsbt",
                signed["psbt"],
            )

            if not finalized.get("complete"):
                raise RuntimeError(
                    "Bitcoin refund PSBT did not finalize"
                )

            return finalized["hex"]

        fee_plan = build_dynamic_settlement_spend(
            chain="btc",
            cli=btc_cli,
            input_sats=input_sats,
            build_finalized=build_refund_with_fee,
            fee_rate=args.fee_rate,
        )

        refund_hex = fee_plan["hex"]

        refund_path.write_text(
            refund_hex + "\n"
        )

        print()
        print("Saved signed Bitcoin refund:")
        print(refund_path)

    print()
    print("BITCOIN REFUND")
    print("==============")
    print("current height:", current_height)
    print("refund height: ", swap.btc_timeout)
    print("input sats:    ", swap.btc_sats)
    print("output sats:   ", int(swap.btc_sats) - fee_plan["fee_sats"])
    print("fee sats:      ", fee_plan["fee_sats"])
    print("vsize:         ", fee_plan["vsize"], "vB")
    print(
        "rate:          ",
        fee_plan["rate_sat_vb"],
        "sat/vB",
    )
    print("fee source:    ", fee_plan["source"])

    if current_height < swap.btc_timeout:
        print()
        print("Refund is not mature yet.")
        print("Testing early broadcast...")

        try:
            btc_cli(
                "sendrawtransaction",
                refund_hex,
            )

        except RuntimeError as e:
            if "non-final" in str(e):
                print()
                print("PASS: early Bitcoin refund rejected as non-final")
                return

            raise

        raise RuntimeError(
            "Bitcoin refund was unexpectedly accepted early"
        )

    #
    # Derive and persist the exact transaction identity BEFORE
    # broadcast. Durable protocol state remains BTC_FUNDED until
    # the exact refund is confirmed on the active Bitcoin chain.
    #
    decoded_refund = btc_cli(
        "decoderawtransaction",
        refund_hex,
    )

    txid = decoded_refund.get("txid")

    if not txid:
        raise RuntimeError(
            "Signed Bitcoin refund has no decodable txid"
        )

    actual = classify_htlc_spend(
        decoded_refund,
        swap.btc_funding_txid,
        swap.btc_funding_vout,
        swap.secret_hash,
    )

    if actual != "refund":
        raise RuntimeError(
            "SECURITY FAILURE: signed Bitcoin refund "
            f"classified as {actual}"
        )

    swap.btc_refund_txid = txid
    swap.btc_refund_blockhash = None
    swap.btc_refund_hex = refund_hex

    save_swap(
        swap,
        role="bob",
    )

    broadcast_txid = btc_cli(
        "sendrawtransaction",
        refund_hex,
    )

    if broadcast_txid != txid:
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin refund broadcast "
            "returned a different txid"
        )

    _test_crash_after_broadcast("btc-refund-after-broadcast")

    protocol_failpoint(
        "after_btc_refund_broadcast"
    )

    if auto_mine_enabled():
        mine_btc(1)

    recovered = require_recovered_branch(
        swap,
        "btc",
        "refund",
    )

    if recovered == "pending":
        print()
        print("BTC REFUND BROADCAST")
        print("====================")
        print("txid:  ", txid)
        print("block:  pending")
        print("state: ", swap.state)
        print()
        print(
            "Bitcoin refund is awaiting "
            "active-chain confirmation."
        )
        return

    if recovered is None:
        raise RuntimeError(
            "Bitcoin refund broadcast is neither pending "
            "nor confirmed"
        )

    swap.btc_refund_txid = recovered["txid"]
    swap.btc_refund_blockhash = recovered["blockhash"]

    swap.transition(
         State.BTC_REFUNDED
    )

    save_swap(
        swap,
        role="bob",
    )


    print()
    print("BTC REFUNDED")
    print("============")
    print("txid:  ", txid)
    print("block: ", swap.btc_refund_blockhash)
    print("state: ", swap.state)

def verify_final_blake_claim_active(swap):
    """
    Prove that Bob's final BLAKE claim still exists on the
    active BLAKE chain and still spends this swap's HTLC
    through the claim branch.
    """

    if not swap.blake_claim_txid:
        raise RuntimeError(
            "SECURITY FAILURE: swap has no BLAKE claim txid"
        )

    recovered = find_confirmed_htlc_spend(
        swap,
        "blake",
    )

    if recovered is None:
        raise RuntimeError(
            "SECURITY FAILURE: final BLAKE claim is no longer "
            "confirmed on the active chain"
        )

    if recovered.get("branch") != "claim":
        raise RuntimeError(
            "SECURITY FAILURE: active BLAKE HTLC spend is not "
            "the expected claim branch"
        )

    if recovered.get("txid") != swap.blake_claim_txid:
        raise RuntimeError(
            "SECURITY FAILURE: active BLAKE claim txid differs "
            "from recorded final settlement"
        )

    return True


def recorded_settlement_confirmed(cli, txid, blockhash):
    """
    Read-only: is this exact transaction confirmed on the active chain?

    Returns True only for an active-chain transaction with at least one
    confirmation. A missing, unknown, or reorg-disconnected transaction
    returns False. The recorded block hash is passed to
    getrawtransaction so this works on nodes without a transaction
    index.

    Reorganization recovery needs this narrow question because
    find_confirmed_htlc_spend() fails closed on a disconnected recorded
    settlement. That is the right behaviour for an action path, but it
    would make the recovery path unreachable, since a disconnected
    transaction is precisely the input recovery exists to handle.
    """
    if not txid:
        return False

    try:
        if blockhash:
            tx = cli(
                "getrawtransaction",
                txid,
                True,
                blockhash,
            )
        else:
            tx = cli(
                "getrawtransaction",
                txid,
                True,
            )
    except Exception:
        return False

    if not isinstance(tx, dict):
        return False

    if tx.get("in_active_chain") is False:
        return False

    return int(tx.get("confirmations", 0) or 0) >= 1


def active_htlc_spend_or_none(swap, chain, txid, blockhash):
    """
    The confirmed active-chain spend of this swap's HTLC, if any.

    `find_confirmed_htlc_spend` fails closed on a reorg-disconnected
    recorded settlement. That is right for an action path, but it would
    make the recovery paths unreachable, because a disconnected
    transaction is precisely the input they exist to handle. So the
    narrow "is this exact recorded transaction still confirmed?" question
    is asked first, and only then is the HTLC spend looked up.

    Returns None when the recorded transaction is not confirmed. A
    *conflicting* confirmed spend is not None; the caller must treat that
    as the security failure it is rather than as "nothing to recover".
    """
    cli = chain_dispatch(chain)["cli"]

    if not recorded_settlement_confirmed(
        cli,
        txid,
        blockhash,
    ):
        return None

    return find_confirmed_htlc_spend(
        swap,
        chain,
    )


def recover_final_blake_claim_reorg(swap):
    """
    Recover from a reorg which removed Bob's final BLAKE claim.

    This is intentionally NOT a normal state transition. The normal
    protocol graph remains forward-only.

    A backward correction is permitted here only after both chains
    prove that BTC_CLAIMED is again the authoritative durable state.
    """

    #
    # First ask the active BLAKE chain what currently spends
    # the original HTLC. A recorded final claim that a reorg has
    # disconnected is the normal input here; see
    # active_htlc_spend_or_none() for why the narrow question comes first.
    #
    current_blake_spend = active_htlc_spend_or_none(
        swap,
        "blake",
        swap.blake_claim_txid,
        swap.blake_claim_blockhash,
    )

    if current_blake_spend is not None:
        #
        # If the recorded final claim still exists, there is
        # nothing to recover.
        #
        if (
            current_blake_spend.get("branch") == "claim"
            and current_blake_spend.get("txid")
            == swap.blake_claim_txid
        ):
            return False

        #
        # Something else spent the BLAKE HTLC. Do not guess.
        #
        raise RuntimeError(
            "SECURITY FAILURE: active BLAKE HTLC spend "
            "conflicts with recorded final settlement"
        )

    #
    # No confirmed BLAKE spend exists anymore.
    #
    # A reorg normally returns the disconnected transaction
    # to the mempool. If Bob's exact recorded final claim is
    # still pending there, do NOT roll durable state backward.
    #
    # Presence of a mempool entry is the test, not the wording of any
    # error message.
    #
    if transaction_in_mempool(
        blake_cli,
        swap.blake_claim_txid,
    ) is not None:
        pending_tx = blake_cli(
            "getrawtransaction",
            swap.blake_claim_txid,
            True,
        )

        pending_branch = classify_htlc_spend(
            pending_tx,
            swap.blake_funding_txid,
            swap.blake_funding_vout,
            swap.secret_hash,
        )

        if pending_branch != "claim":
            raise RuntimeError(
                "SECURITY FAILURE: recorded BLAKE claim "
                "is pending in mempool but does not use "
                "the expected claim branch"
            )

        return "pending"

    #
    # The recorded claim is neither confirmed nor pending.
    #
    # Prove the funding HTLC has returned on the active chain,
    # deliberately ignoring mempool effects.
    #
    utxo = blake_cli(
        "gettxout",
        swap.blake_funding_txid,
        swap.blake_funding_vout,
        False,
    )

    if not utxo:
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE HTLC is unavailable "
            "after final claim disappeared from both "
            "active chain and mempool"
        )

    #
    # Now establish the last still-confirmed settlement fact:
    # Alice's Bitcoin claim.
    #
    current_btc_spend = find_confirmed_htlc_spend(
        swap,
        "btc",
    )

    if current_btc_spend is None:
        raise RuntimeError(
            "SECURITY FAILURE: cannot recover final BLAKE "
            "claim reorg because no confirmed Bitcoin HTLC "
            "spend establishes BTC_CLAIMED"
        )

    if current_btc_spend.get("branch") != "claim":
        raise RuntimeError(
            "SECURITY FAILURE: Bitcoin HTLC is not currently "
            "spent through Alice's claim branch"
        )

    if (
        not swap.btc_claim_txid
        or current_btc_spend.get("txid")
        != swap.btc_claim_txid
    ):
        raise RuntimeError(
            "SECURITY FAILURE: active Bitcoin claim differs "
            "from recorded BTC settlement"
        )

    #
    # At this point both chains agree:
    #
    #   Bitcoin: Alice's claim is confirmed.
    #   BLAKE:   Bob's claim vanished and the HTLC is unspent.
    #
    # The authoritative state is therefore BTC_CLAIMED.
    #
    # Do not use transition(); reorg recovery is deliberately
    # separate from the normal forward-only protocol graph.
    #
    # Preserve the exact old claim identity as a recovery hint before
    # clearing canonical settlement state. A wallet may rebroadcast this
    # same transaction after restart.
    swap.blake_reorg_claim_txid = swap.blake_claim_txid

    swap.blake_claim_txid = None
    swap.blake_claim_blockhash = None

    apply_reorg_transition(
        swap,
        State.BTC_CLAIMED,
    )

    return True


def recover_final_blake_refund_reorg(swap):
    """
    Revalidate a persisted BLAKE_REFUNDED terminal state.

    Returns:
        False       exact refund is still confirmed
        "pending"   exact refund is currently in the mempool
        True        refund vanished; state was corrected to BTC_REFUNDED

    Normal protocol transitions remain forward-only. The backward
    correction here is permitted only after both chains establish
    that BTC_REFUNDED is again authoritative.
    """

    if not swap.blake_refund_txid:
        raise RuntimeError(
            "SECURITY FAILURE: swap has no BLAKE refund txid"
        )

    #
    # First inspect confirmed active-chain truth for the recorded refund;
    # see active_htlc_spend_or_none() for why the narrow question comes
    # first.
    #
    active = active_htlc_spend_or_none(
        swap,
        "blake",
        swap.blake_refund_txid,
        swap.blake_refund_blockhash,
    )

    if active is not None:
        if (
            active.get("branch") == "refund"
            and active.get("txid")
            == swap.blake_refund_txid
        ):
            return False

        raise RuntimeError(
            "SECURITY FAILURE: active BLAKE HTLC spend "
            "conflicts with recorded final refund"
        )

    #
    # No confirmed spend. A disconnected refund will normally
    # return to the mempool. If our exact refund is still there,
    # settlement is pending rather than rolled back.
    #
    # Presence of a mempool entry is the test, not the wording of any
    # error message.
    #
    if transaction_in_mempool(
        blake_cli,
        swap.blake_refund_txid,
    ) is not None:
        pending_tx = blake_cli(
            "getrawtransaction",
            swap.blake_refund_txid,
            True,
        )

        branch = classify_htlc_spend(
            pending_tx,
            swap.blake_funding_txid,
            swap.blake_funding_vout,
            swap.secret_hash,
        )

        if branch != "refund":
            raise RuntimeError(
                "SECURITY FAILURE: recorded BLAKE refund "
                "is pending but is not the refund branch"
            )

        return "pending"

    #
    # The exact refund is neither confirmed nor pending.
    # Confirm that the original HTLC exists again on-chain,
    # ignoring mempool effects.
    #
    utxo = blake_cli(
        "gettxout",
        swap.blake_funding_txid,
        swap.blake_funding_vout,
        False,
    )

    if not utxo:
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE HTLC is unavailable "
            "after final refund disappeared"
        )

    #
    # Before moving durable state backward, independently prove
    # that Bob's Bitcoin refund is STILL confirmed. That makes
    # BTC_REFUNDED the authoritative cross-chain state.
    #
    verify_confirmed_btc_refund_for_blake(
        swap
    )

    # Preserve the exact old refund identity as a recovery hint before
    # clearing canonical settlement state. A wallet may rebroadcast this
    # same transaction after restart.
    swap.blake_reorg_refund_txid = swap.blake_refund_txid

    swap.blake_refund_txid = None
    swap.blake_refund_blockhash = None

    #
    # This is deliberate reorg recovery, not a normal transition, so it
    # goes through the explicit reverse-edge table rather than
    # Swap.transition().
    #
    apply_reorg_transition(
        swap,
        State.BTC_REFUNDED,
    )

    return True


def command_complete(args):
    swap = load_swap(
        args.swap_id,
        role=args.role,
    )

    if State(swap.state) != State.BLAKE_CLAIMED:
        raise ValueError(
            "complete requires state BLAKE_CLAIMED"
        )

    #
    # COMPLETE is only valid while the final Bob claim
    # remains confirmed on the active BLAKE chain.
    #
    verify_final_blake_claim_active(
        swap
    )

    swap.transition(
        State.COMPLETE
    )

    save_swap(
        swap,
        role=args.role,
    )

    print("SWAP COMPLETE")
    print("=============")
    print("role:  ", args.role)
    print("state: ", swap.state)







async def nostr_fetch_event_by_id_from_relay(
    relay,
    event_id,
):
    import asyncio
    import secrets
    import websockets

    sub_id = (
        "blake-event-"
        + secrets.token_hex(4)
    )

    found = None

    async with websockets.connect(
        relay,
        open_timeout=10,
        close_timeout=5,
    ) as ws:

        await ws.send(
            json.dumps(
                [
                    "REQ",
                    sub_id,
                    {
                        "ids": [event_id],
                        "limit": 1,
                    },
                ]
            )
        )

        while True:
            raw = await asyncio.wait_for(
                ws.recv(),
                timeout=10,
            )

            response = json.loads(raw)

            if (
                isinstance(response, list)
                and len(response) >= 3
                and response[0] == "EVENT"
                and response[1] == sub_id
            ):
                found = response[2]

            elif (
                isinstance(response, list)
                and len(response) >= 2
                and response[0] == "EOSE"
                and response[1] == sub_id
            ):
                break

        await ws.send(
            json.dumps(
                ["CLOSE", sub_id]
            )
        )

    if found is None:
        return None

    verify_nostr_event(found)

    if found["id"] != event_id:
        raise RuntimeError(
            f"Relay {relay} returned wrong event ID"
        )

    return found


def nostr_event_cache_dir():
    """
    The verified-event cache directory.

    Pure: the directory is created once at import (see
    NOSTR_EVENT_CACHE_DIR). This helper is called from read-only
    reconciliation paths, and a path computation must not have
    filesystem side effects.
    """
    return NOSTR_EVENT_CACHE_DIR


def nostr_event_cache_path(event_id):
    if (
        not isinstance(event_id, str)
        or len(event_id) != 64
    ):
        raise ValueError(
            "Nostr event ID must be 64 hex characters"
        )

    try:
        int(event_id, 16)
    except ValueError as e:
        raise ValueError(
            "Nostr event ID is not hex"
        ) from e

    return nostr_event_cache_dir() / f"{event_id}.json"


def nostr_cache_event(event):
    import json
    import os

    #
    # Never put an event into the durable cache merely
    # because a relay supplied it.  Cryptographically
    # verify it first.
    #
    verify_nostr_event(event)

    event_id = event.get("id")

    if (
        not isinstance(event_id, str)
        or len(event_id) != 64
    ):
        raise RuntimeError(
            "Refusing to cache event with invalid ID"
        )

    path = nostr_event_cache_path(event_id)

    #
    # The directory is created at import, but recreate it defensively
    # in case it was removed while the process was running.
    #
    nostr_event_cache_dir().mkdir(
        parents=True,
        exist_ok=True,
        mode=0o700,
    )

    #
    # A cache, not protocol state: the event is re-verified on every read
    # and can be fetched again, so the rename does not need to be durable.
    # It does need to be atomic and private, which is what mode=0o600 on
    # the temp file guarantees before publication.
    #
    atomic_write_json(
        path,
        event,
        compact=True,
        mode=0o600,
    )

    return path


def nostr_load_cached_event(event_id):
    import json

    path = nostr_event_cache_path(event_id)

    if not path.exists():
        return None

    try:
        with open(path) as f:
            event = json.load(f)
    except Exception as e:
        raise RuntimeError(
            f"Cached Nostr event is unreadable: "
            f"{event_id}"
        ) from e

    #
    # Disk is not an authority either.
    # Re-verify every cached event every time it is used.
    #
    verify_nostr_event(event)

    if event.get("id") != event_id:
        raise RuntimeError(
            "SECURITY FAILURE: cached Nostr event "
            "ID does not match requested event"
        )

    return event


def nostr_market_cancellation_applies(
    event,
    offer_id,
    maker_pubkey,
):
    import json

    #
    # Caller may supply either relay-discovered or cached
    # evidence.  Never trust either source by location.
    #
    verify_nostr_event(event)

    #
    # Cryptographic authenticity alone does not make
    # an event a protocol-valid cancellation.
    #
    if event.get("kind") != 1:
        return False

    if not any(
        len(tag) >= 2
        and tag[0] == "t"
        and tag[1] == "blake-swap-market-cancel"
        for tag in event.get("tags", [])
    ):
        return False

    try:
        message = json.loads(
            event["content"]
        )
    except Exception:
        return False

    if message.get("type") != "market-cancel":
        return False

    if message.get("version") != 1:
        return False

    if (
        message.get("created_at")
        != event["created_at"]
    ):
        return False

    if message.get("offer_id") != offer_id:
        return False

    if not any(
        len(tag) >= 2
        and tag[0] == "e"
        and tag[1] == offer_id
        for tag in event["tags"]
    ):
        return False

    #
    # Critical authorization rule:
    # only the signer of the original offer can cancel it.
    #
    if event["pubkey"] != maker_pubkey:
        return False

    return True



def nostr_derive_market_offer_state(
    offer_event,
    market_events,
    now=None,
):
    import time

    message, offer = (
        nostr_parse_market_offer_event(
            offer_event
        )
    )

    if now is None:
        now = int(time.time())
    else:
        now = int(now)

    expires_at = int(
        offer["expires_at"]
    )

    #
    # MATCH and CANCEL are both terminal,
    # maker-authorized market events.
    #
    # Once either happened while the offer was live,
    # later wall-clock expiry must not rewrite history.
    #
    terminal_events = []

    #
    # Immutable evidence lookup used by MATCH validation.
    #
    events_by_id = {
        event.get("id"): event
        for event in market_events
        if (
            isinstance(event, dict)
            and isinstance(
                event.get("id"),
                str,
            )
        )
    }

    for event in market_events:
        try:
            created_at = int(
                event.get("created_at", 0)
            )

            if created_at >= expires_at:
                continue

            if nostr_market_cancellation_applies(
                event,
                offer["event_id"],
                offer["maker"],
            ):
                terminal_events.append(
                    (
                        created_at,
                        event["id"],
                        "CANCELLED",
                        event,
                    )
                )
                continue

            #
            # MATCHED requires complete offer + take + match
            # evidence. A maker-signed but economically
            # inconsistent match must not hide an OPEN offer.
            #
            try:
                import json

                match_message = json.loads(
                    event.get(
                        "content",
                        "",
                    )
                )
            except Exception:
                continue

            if (
                match_message.get("type")
                != "market-match"
            ):
                continue

            take_id = match_message.get(
                "take_id"
            )

            take_event = events_by_id.get(
                take_id
            )

            if take_event is None:
                continue

            validate_market_match_evidence(
                event,
                offer_event,
                take_event,
            )

            terminal_events.append(
                (
                    created_at,
                    event["id"],
                    "MATCHED",
                    event,
                )
            )

        except Exception:
            continue

    #
    # First valid terminal event wins.
    #
    # Event ID gives deterministic ordering if two
    # terminal events share the same created_at.
    #
    if terminal_events:
        terminal_events.sort(
            key=lambda item: (
                item[0],
                item[1],
            )
        )

        (
            _created_at,
            _event_id,
            state,
            terminal_event,
        ) = terminal_events[0]

        return {
            "state": state,
            "offer": offer,
            "message": message,
            "cancel_event_id": (
                terminal_event["id"]
                if state == "CANCELLED"
                else None
            ),
            "match_event_id": (
                terminal_event["id"]
                if state == "MATCHED"
                else None
            ),
        }

    if expires_at <= now:
        return {
            "state": "EXPIRED",
            "offer": offer,
            "message": message,
            "cancel_event_id": None,
            "match_event_id": None,
        }

    return {
        "state": "OPEN",
        "offer": offer,
        "message": message,
        "cancel_event_id": None,
        "match_event_id": None,
    }

def nostr_cached_market_events():
    #
    # Return cryptographically authentic immutable
    # Nostr events from our durable local cache.
    #
    # This storage layer deliberately does NOT decide
    # what an event means or whether its signer has
    # authority over a market offer.
    #
    # Protocol semantics belong to the canonical
    # market-state derivation layer.
    #
    # Important security behavior:
    # if a canonical cache file is corrupt or fails
    # signature verification, do NOT silently ignore
    # that local integrity failure.
    #
    cache_dir = (
        BASE_SWAP_DIR
        / "nostr-event-cache"
    )

    if not cache_dir.exists():
        return []

    events = []

    for path in cache_dir.glob("*.json"):
        event_id = path.stem

        #
        # Only canonical event-cache filenames belong
        # to this scan.
        #
        if len(event_id) != 64:
            continue

        try:
            int(event_id, 16)
        except ValueError:
            continue

        #
        # This performs signature verification and
        # exact filename/event-ID binding.
        #
        event = nostr_load_cached_event(
            event_id
        )

        if event is None:
            continue

        events.append(event)

    events.sort(
        key=lambda event: (
            event["created_at"],
            event["id"],
        )
    )

    return events


def nostr_prior_market_take(
    offer_id,
    taker_pubkey=None,
):
    """
    Return this participant's previously signed market take
    for offer_id, or None.

    Only cryptographically verified events from the durable
    Nostr cache are considered.
    """
    if taker_pubkey is None:
        taker_pubkey = current_nostr_pubkey()

    for event in nostr_cached_market_events():
        try:
            verify_nostr_event(event)

            if event.get("pubkey") != taker_pubkey:
                continue

            message = json.loads(
                event.get("content", "{}")
            )

            if (
                message.get("type") == "market-take"
                and message.get("offer_id") == offer_id
            ):
                return event

        except Exception:
            continue

    return None


def resolve_cached_nostr_prefix(
    value,
    expected_type=None,
    label="Nostr event",
    announce=True,
):
    import json

    if not isinstance(value, str):
        raise ValueError(f"{label} ID must be text")

    value = value.strip().lower()

    if len(value) == 64:
        try:
            int(value, 16)
        except ValueError as e:
            raise ValueError(f"{label} ID is not hexadecimal") from e
        return value

    if len(value) < 8:
        raise ValueError(
            f"{label} ID prefix is too short; use at least 8 hex characters"
        )

    try:
        int(value, 16)
    except ValueError as e:
        raise ValueError(
            f"{label} ID prefix is not hexadecimal"
        ) from e

    matches = []

    for event in nostr_cached_market_events():
        event_id = event.get("id")
        if (
            not isinstance(event_id, str)
            or not event_id.startswith(value)
        ):
            continue

        if expected_type is not None:
            try:
                message = json.loads(event.get("content", ""))
            except Exception:
                continue
            if message.get("type") != expected_type:
                continue

        matches.append(event_id)

    matches = sorted(set(matches))

    if not matches:
        hints = {
            "market-offer": "run `blake-swap market` first",
            "market-take": "run `market-takes <offer>` first",
            "market-match": "use a full match ID or refresh match discovery",
        }
        raise RuntimeError(
            f"No verified cached {label.lower()} matches prefix {value!r}; "
            + hints.get(expected_type, "refresh the relevant Nostr view first")
        )

    if len(matches) > 1:
        print()
        print("AMBIGUOUS ID PREFIX")
        print("===================")
        print(value)
        print()
        for event_id in matches:
            print(event_id)
        raise RuntimeError(
            f"{label} ID prefix matches more than one verified event; "
            "use a longer prefix"
        )

    resolved = matches[0]

    if announce:
        print(f"Resolved {label.lower()} prefix {value} -> {resolved}")

    return resolved


async def nostr_fetch_event_by_id(event_id):
    import asyncio

    tasks = [
        nostr_fetch_event_by_id_from_relay(
            relay,
            event_id,
        )
        for relay in NOSTR_RELAYS
    ]

    results = await asyncio.gather(
        *tasks,
        return_exceptions=True,
    )

    found_by_id = {}
    locations = []

    for relay, result in zip(
        NOSTR_RELAYS,
        results,
    ):
        if isinstance(result, Exception):
            continue

        if result is None:
            continue

        found_by_id[result["id"]] = result
        locations.append(relay)

    if event_id not in found_by_id:
        cached = nostr_load_cached_event(
            event_id
        )

        if cached is not None:
            print(
                "Nostr event unavailable on configured "
                "relays; using verified local cache:"
            )
            print(event_id)
            return cached

        raise RuntimeError(
            f"Nostr event not found on any "
            f"configured relay or verified local cache: "
            f"{event_id}"
        )

    event = found_by_id[event_id]

    # Verify again after merging results.
    verify_nostr_event(event)

    #
    # Store the complete immutable signed event only
    # after local cryptographic verification.
    #
    nostr_cache_event(event)

    print(
        "Nostr event found on:",
        ", ".join(locations),
    )

    return event


async def nostr_fetch_market_events_from_relay(
    relay,
    limit=50,
):
    import asyncio
    import secrets
    import websockets

    sub_id = (
        "blake-market-"
        + secrets.token_hex(4)
    )

    offer_filter = {
        "kinds": [1],
        "#t": ["blake-swap-market"],
        "limit": limit,
    }

    cancel_filter = {
        "kinds": [1],
        "#t": ["blake-swap-market-cancel"],
        "limit": max(100, limit * 4),
    }

    match_filter = {
        "kinds": [1],
        "#t": ["blake-swap-market-match"],
        "limit": max(100, limit * 4),
    }

    events = []

    async with websockets.connect(
        relay,
        open_timeout=10,
        close_timeout=5,
    ) as ws:

        await ws.send(
            json.dumps(
                [
                    "REQ",
                    sub_id,
                    offer_filter,
                    cancel_filter,
                    match_filter,
                ]
            )
        )

        while True:
            raw = await asyncio.wait_for(
                ws.recv(),
                timeout=10,
            )

            response = json.loads(raw)

            if (
                isinstance(response, list)
                and len(response) >= 3
                and response[0] == "EVENT"
                and response[1] == sub_id
            ):
                events.append(
                    response[2]
                )

            elif (
                isinstance(response, list)
                and len(response) >= 2
                and response[0] == "EOSE"
                and response[1] == sub_id
            ):
                break

        await ws.send(
            json.dumps(
                ["CLOSE", sub_id]
            )
        )

    return events


def nostr_parse_market_offer_event(event):
    import json

    #
    # Storage location is irrelevant.  The event must
    # prove its own cryptographic authenticity.
    #
    verify_nostr_event(event)

    #
    # A valid signature proves who signed the event,
    # not that the event belongs to this protocol.
    #
    if event.get("kind") != 1:
        raise RuntimeError(
            "Market offer Nostr kind is not 1"
        )

    if not any(
        len(tag) >= 2
        and tag[0] == "t"
        and tag[1] == "blake-swap-market"
        for tag in event.get("tags", [])
    ):
        raise RuntimeError(
            "Market offer is missing expected "
            "Nostr market tag"
        )

    try:
        message = json.loads(
            event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market offer content is invalid JSON"
        ) from e

    if message.get("type") != "market-offer":
        raise RuntimeError(
            "Event is not a market offer"
        )

    version = message.get(
        "version"
    )

    if version not in (1, 2):
        raise RuntimeError(
            "Unsupported market offer version"
        )

    if message.get("base") != "BLAKE":
        raise RuntimeError(
            "Market offer base is not BLAKE"
        )

    if message.get("quote") != "BTC":
        raise RuntimeError(
            "Market offer quote is not BTC"
        )

    #
    # v1 had only one possible direction:
    # maker sells BLAKE for BTC.
    #
    if version == 1:
        if message.get("side") != "sell":
            raise RuntimeError(
                "Market offer side is not sell"
            )

        direction = (
            MARKET_DIRECTION_BLAKE_FOR_BTC
        )

    #
    # v2 makes economic direction explicit.
    #
    else:
        direction = message.get(
            "direction"
        )

        if direction not in MARKET_DIRECTIONS:
            raise RuntimeError(
                "Market offer has invalid direction"
            )

        if not any(
            len(tag) >= 2
            and tag[0] == "direction"
            and tag[1] == direction
            for tag in event.get("tags", [])
        ):
            raise RuntimeError(
                "Market offer direction does not "
                "match Nostr direction tag"
            )

    if (
        message.get("created_at")
        != event["created_at"]
    ):
        raise RuntimeError(
            "Market offer created_at mismatch"
        )

    try:
        blake_amount = float(
            message["blake_amount"]
        )
        btc_sats = int(
            message["btc_sats"]
        )
        expires_at = int(
            message["expires_at"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market offer contains invalid "
            "economic fields"
        ) from e

    if blake_amount <= 0:
        raise RuntimeError(
            "Market offer BLAKE amount "
            "must be positive"
        )

    if btc_sats <= 0:
        raise RuntimeError(
            "Market offer BTC amount "
            "must be positive"
        )

    if expires_at <= event["created_at"]:
        raise RuntimeError(
            "Market offer expiry must be "
            "after creation"
        )

    forbidden = {
        "secret_hex",
        "secret_hash",
        "blake_alice_pub",
        "blake_bob_pub",
        "btc_alice_pub",
        "btc_bob_pub",
        "blake_funding_txid",
        "btc_funding_txid",
    }

    if forbidden.intersection(
        message.keys()
    ):
        raise RuntimeError(
            "Market offer contains forbidden "
            "settlement data"
        )

    roles = market_protocol_roles(
        direction
    )

    offer = {
        "event_id": event["id"],
        "maker": event["pubkey"],
        "created_at": event["created_at"],
        "expires_at": expires_at,
        "blake_amount": blake_amount,
        "btc_sats": btc_sats,
        "direction": direction,
        "maker_role": roles["maker_role"],
        "taker_role": roles["taker_role"],
    }

    return message, offer


async def nostr_fetch_market_states(
    limit=50,
    announce=True,
):
    import asyncio
    import time

    tasks = [
        nostr_fetch_market_events_from_relay(
            relay,
            limit=limit,
        )
        for relay in NOSTR_RELAYS
    ]

    results = await asyncio.gather(
        *tasks,
        return_exceptions=True,
    )

    events_by_id = {}
    successful_relays = 0

    for relay, result in zip(
        NOSTR_RELAYS,
        results,
    ):
        if isinstance(result, Exception):
            continue

        successful_relays += 1

        for event in result:
            if not isinstance(event, dict):
                continue

            event_id = event.get("id")

            if not isinstance(event_id, str):
                continue

            #
            # Same immutable Nostr event may appear on
            # several relays. Process it exactly once.
            #
            events_by_id.setdefault(
                event_id,
                event,
            )

    if successful_relays == 0:
        raise RuntimeError(
            "Could not query any configured "
            "Nostr relay for market events"
        )

    #
    # First establish cryptographic authenticity and
    # retain every authentic immutable market event.
    #
    # Protocol meaning is derived only after that.
    #
    verified_market_events = {}
    offer_events = {}

    for event in events_by_id.values():
        try:
            verify_nostr_event(event)

            #
            # Relay storage is disposable.
            # Preserve authentic evidence locally.
            #
            nostr_cache_event(event)

        except Exception:
            continue

        event_id = event.get("id")

        if not isinstance(event_id, str):
            continue

        verified_market_events.setdefault(
            event_id,
            event,
        )

        #
        # Only canonical protocol-valid offers become
        # offer-state candidates.
        #
        try:
            nostr_parse_market_offer_event(
                event
            )
        except Exception:
            continue

        offer_events.setdefault(
            event_id,
            event,
        )

    #
    # Current relay reads are only discovery windows.
    # They are not complete protocol history.
    #
    # Merge previously cached authentic immutable Nostr
    # evidence before deriving canonical offer state.
    #
    # Disk is not authority: re-verify every cached event
    # before allowing it to participate in state derivation.
    #
    for event in nostr_cached_market_events():
        try:
            verify_nostr_event(event)
        except Exception:
            continue

        event_id = event.get("id")

        if not isinstance(event_id, str):
            continue

        verified_market_events.setdefault(
            event_id,
            event,
        )

        try:
            nostr_parse_market_offer_event(
                event
            )
        except Exception:
            continue

        offer_events.setdefault(
            event_id,
            event,
        )

    now = int(time.time())
    states = []

    for offer_event in offer_events.values():
        try:
            derived = (
                nostr_derive_market_offer_state(
                    offer_event,
                    verified_market_events.values(),
                    now=now,
                )
            )
        except Exception:
            continue

        offer = dict(
            derived["offer"]
        )

        offer["state"] = (
            derived["state"]
        )

        offer["cancel_event_id"] = (
            derived.get(
                "cancel_event_id"
            )
        )

        offer["match_event_id"] = (
            derived.get(
                "match_event_id"
            )
        )

        states.append(
            offer
        )

    states.sort(
        key=lambda x: (
            x["created_at"],
            x["event_id"],
        ),
        reverse=True,
    )

    if announce:
        print(
            "Nostr market read:",
            len(events_by_id),
            "unique event(s) from",
            f"{successful_relays}/"
            f"{len(NOSTR_RELAYS)} relay(s)",
        )

    return states


async def nostr_fetch_market_offers(limit=50):
    #
    # Backward-compatible human-market API:
    # only currently OPEN offers are returned.
    #
    states = await nostr_fetch_market_states(
        limit=limit,
        announce=True,
    )

    return [
        item
        for item in states
        if item["state"] == "OPEN"
    ]




def command_market_state(args):
    """
    Emit canonical market offer state.

    This is the machine-readable boundary for UIs and
    other local tooling.  Clients must not re-derive
    OPEN / MATCHED / CANCELLED / EXPIRED themselves.
    """
    import asyncio
    import json

    offers = asyncio.run(
        nostr_fetch_market_states(
            limit=args.limit,
            announce=False,
        )
    )

    result = {
        "offers": offers,
    }

    if args.json:
        print(
            json.dumps(
                result,
                indent=2,
                sort_keys=True,
            )
        )
        return

    print()
    print("BLAKE/BTC MARKET STATE")
    print("======================")
    print()

    if not offers:
        print("No valid market offers found.")
        return

    for item in offers:
        print(
            item["event_id"][:12],
            item["state"],
            item["direction"],
            item["blake_amount"],
            "BLAKE",
            f'{item["btc_sats"]:,}',
            "sats",
        )


def command_market(args):
    import asyncio
    import time

    offers = asyncio.run(
        nostr_fetch_market_offers(
            limit=args.limit
        )
    )

    print()
    print("AVAILABLE BLAKE/BTC OFFERS")
    print("==========================")
    print()

    if not offers:
        print("No valid open offers found.")
        return

    print(
        f'{"Offer":14} '
        f'{"Maker":14} '
        f'{"Giving":18} '
        f'{"Wanting":18} '
        f'{"Expires"}'
    )

    print(
        f'{"-" * 12:14} '
        f'{"-" * 12:14} '
        f'{"-" * 14:16} '
        f'{"-" * 14:16} '
        f'{"-" * 8}'
    )

    now = int(time.time())

    for offer in offers:
        remaining = max(
            0,
            offer["expires_at"] - now,
        )

        if remaining < 60:
            ttl = f"{remaining}s"

        elif remaining < 3600:
            ttl = (
                f"{remaining // 60}m"
            )

        elif remaining < 86400:
            ttl = (
                f"{remaining // 3600}h"
            )

        else:
            ttl = (
                f"{remaining // 86400}d"
            )

        terms = market_give_want(
            offer["direction"],
            offer["blake_amount"],
            offer["btc_sats"],
        )

        print(
            f'{offer["event_id"][:12]:14} '
            f'{offer["maker"][:12]:14} '
            f'{terms["give"]:18} '
            f'{terms["want"]:18} '
            f'{ttl}'
        )

    print()
    print(
        f"{len(offers)} open offer(s)"
    )
    print()
    print(
        "Expired offers and valid maker "
        "cancellations are hidden."
    )



def build_market_offer_v2_event(
    blake_amount,
    btc_sats,
    minutes,
    direction,
):
    import time
    import secrets
    from coincurve import PrivateKey

    if direction not in MARKET_DIRECTIONS:
        raise ValueError(
            f"Unsupported market direction: {direction!r}"
        )

    if blake_amount <= 0:
        raise ValueError(
            "BLAKE amount must be positive"
        )

    if btc_sats <= 0:
        raise ValueError(
            "BTC sat amount must be positive"
        )

    if minutes <= 0:
        raise ValueError(
            "Offer lifetime must be positive"
        )

    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = (
        private_key.public_key.format(
            compressed=True
        )
    )

    pubkey_hex = compressed[1:].hex()

    created_at = int(time.time())

    expires_at = (
        created_at
        + (minutes * 60)
    )

    message = {
        "type": "market-offer",
        "version": 2,

        # Economic pair is canonical regardless
        # of which asset the maker is giving.
        "base": "BLAKE",
        "quote": "BTC",

        "direction": direction,

        "blake_amount": format_blake_amount(
            blake_base_units(blake_amount)
        ),
        "btc_sats": int(
            btc_sats
        ),

        "created_at": created_at,
        "expires_at": expires_at,
        "nonce": secrets.token_hex(8),
    }

    content = json.dumps(
        message,
        separators=(",", ":"),
        sort_keys=True,
    )

    event = {
        "pubkey": pubkey_hex,
        "created_at": created_at,
        "kind": 1,
        "tags": [
            [
                "t",
                "blake-swap-market",
            ],
            [
                "base",
                "BLAKE",
            ],
            [
                "quote",
                "BTC",
            ],
            [
                "direction",
                direction,
            ],
        ],
        "content": content,
    }

    event_id = nostr_event_id(
        event
    )

    event["id"] = event_id.hex()

    event["sig"] = (
        private_key.sign_schnorr(
            event_id
        ).hex()
    )

    return event, message


def settlement_fee_target_blocks():
    target = int(
        os.environ.get(
            "BLAKE_SWAP_FEE_TARGET_BLOCKS",
            "2",
        )
    )

    if target < 1:
        raise ValueError(
            "BLAKE_SWAP_FEE_TARGET_BLOCKS must be >= 1"
        )

    return target


def funding_fee_target_blocks(chain):
    """
    Wallet-funding confirmation target, separate from settlement policy.
    """
    if chain == "btc":
        name = "BLAKE_SWAP_BTC_FUNDING_FEE_TARGET_BLOCKS"
    elif chain == "blake":
        name = "BLAKE_SWAP_BLAKE_FUNDING_FEE_TARGET_BLOCKS"
    else:
        raise ValueError(
            f"unknown funding-fee chain: {chain}"
        )

    target = int(
        os.environ.get(
            name,
            "1",
        )
    )

    if target < 1:
        raise ValueError(
            f"{name} must be >= 1"
        )

    return target


def btc_funding_min_fee_rate():
    """
    Minimum feerate accepted by the Bitcoin wallet for transaction
    creation, in sat/vB.

    Bitcoin Core's default -mintxfee is 0.00001 BTC/kvB = 1 sat/vB.
    This is a wallet-construction floor, separate from relay and mempool
    policy, so it applies to wallet-built BTC funding transactions only.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE",
        "1.0",
    )

    return parse_settlement_fee_rate(
        raw,
        "BLAKE_SWAP_BTC_FUNDING_MIN_FEE_RATE",
    )


def settlement_fee_multiplier():
    multiplier = Decimal(
        os.environ.get(
            "BLAKE_SWAP_FEE_RATE_MULTIPLIER",
            "1.0",
        )
    )

    if multiplier < Decimal("1.0"):
        raise ValueError(
            "BLAKE_SWAP_FEE_RATE_MULTIPLIER must be >= 1.0"
        )

    return multiplier


def settlement_fee_cap_percent():
    value = Decimal(
        os.environ.get(
            PERCENT_CAP_ENV,
            "10",
        )
    )

    if value <= 0 or value >= 100:
        raise ValueError(
            f"{PERCENT_CAP_ENV} must be > 0 and < 100"
        )

    return value


def settlement_fee_absolute_cap_sats():
    """
    An optional cap on the absolute settlement fee, in sat-units.

    `enforce_fee_cap` has always accepted one; nothing could reach it. It
    is the control that a percentage cannot express: on a large HTLC a
    percentage cap can permit a fee far larger than the operator wants to
    pay, and the mitigation is a flat ceiling rather than a smaller share.
    Unset means no absolute cap, so the percentage governs alone.
    """
    raw = os.environ.get(ABSOLUTE_CAP_ENV, "").strip()

    if not raw:
        return None

    try:
        value = int(raw)
    except ValueError as e:
        raise ValueError(f"{ABSOLUTE_CAP_ENV} must be an integer") from e

    if value < 1:
        raise ValueError(f"{ABSOLUTE_CAP_ENV} must be >= 1")

    return value


def settlement_fee_sats_env_name(chain):
    """
    The environment variable carrying the per-chain exact-fee override.
    """
    if chain == "btc":
        return "BLAKE_SWAP_BTC_SPEND_FEE_SATS"

    if chain == "blake":
        return "BLAKE_SWAP_BLAKE_SPEND_FEE_SATS"

    raise ValueError(
        f"unknown settlement-fee chain: {chain}"
    )


def settlement_fee_override_sats(chain):
    name = settlement_fee_sats_env_name(chain)

    raw = os.environ.get(name)

    if raw in (None, ""):
        return None

    value = int(raw)

    if value < 1:
        raise ValueError(
            f"{name} must be >= 1"
        )

    return value


def parse_settlement_fee_rate(raw, label):
    """
    Parse a settlement feerate given in sat/vB.

    `label` names the surface it arrived on -- `--fee-rate` or an
    environment variable -- so a refusal tells the operator which knob to
    fix rather than showing a bare number.

    Fractional rates are allowed on purpose: a rate is a price per vbyte,
    and rounding it up to a whole number is the engine inventing a policy
    the client did not ask for.
    """
    try:
        value = Decimal(str(raw).strip())
    except (InvalidOperation, ValueError):
        raise ValueError(
            f"{label} must be a number in sat/vB"
        ) from None

    if not value.is_finite() or value <= 0:
        raise ValueError(
            f"{label} must be a positive number in sat/vB"
        )

    return value


def settlement_fee_rate_argument(value):
    """
    argparse `type` for `--fee-rate`.

    Validating here makes an unusable rate a usage error: the CLI exits 2
    before it has read any state or built any transaction. The engine
    validates the same value again, because a rate can also arrive through
    the function API and `build_dynamic_settlement_spend` is the only
    place that must not be wrong.
    """
    try:
        return parse_settlement_fee_rate(value, "--fee-rate")
    except ValueError as e:
        raise argparse.ArgumentTypeError(str(e)) from None


def add_settlement_fee_rate_argument(parser):
    """
    Give a command the client's explicit settlement feerate, in sat/vB.

    The parameter outranks the environment, which is the whole point of
    offering it: the environment is a host default, and the client driving
    this swap is the party that knows what it wants to pay. Both fee caps
    still apply -- this says what to pay, not what may be paid.
    """
    parser.add_argument(
        "--fee-rate",
        type=settlement_fee_rate_argument,
        default=None,
        help=(
            "Settlement fee rate in sat/vB; overrides "
            "BLAKE_SWAP_{BTC,BLAKE}_SPEND_FEE_RATE and "
            "BLAKE_SWAP_{BTC,BLAKE}_SPEND_FEE_SATS. Both fee caps "
            "still apply."
        ),
    )


def settlement_fee_rate_env_name(chain):
    """
    The environment variable carrying the per-chain settlement feerate,
    whether or not it is set.

    Named in refusals so the operator is told which knob to turn rather
    than left to search for it.
    """
    if chain == "btc":
        return "BLAKE_SWAP_BTC_SPEND_FEE_RATE"

    if chain == "blake":
        return "BLAKE_SWAP_BLAKE_SPEND_FEE_RATE"

    raise ValueError(
        f"unknown settlement-fee chain: {chain}"
    )


def settlement_fee_rate_override(chain):
    """
    An explicit settlement feerate, in sat/vB, if the operator set one.

    This is the knob a client with its own view of the fee market wants:
    a client reads current rates (its own source, or the node's), adds a
    margin for how quickly it wants the swap settled, and tells the engine
    the exact rate to use. The engine does not try to read the market on
    its client's behalf beyond the node estimator, and it does not know
    how urgent this particular swap is.

    Precedence, most explicit first:

      1. `BLAKE_SWAP_{BTC,BLAKE}_SPEND_FEE_SATS` -- an exact fee
      2. `BLAKE_SWAP_{BTC,BLAKE}_SPEND_FEE_RATE` -- an exact rate (sat/vB)
      3. the node estimator and policy floors (the default)

    Both caps still apply to every path: an override says what to pay, not
    what may be paid.
    """
    name = settlement_fee_rate_env_name(chain)

    raw = os.environ.get(name)

    if raw in (None, ""):
        return None

    return parse_settlement_fee_rate(raw, name)


def node_fee_rate_floor(cli):
    """
    Highest currently enforced node feerate floor, in sat/vB.
    """
    mempool = cli("getmempoolinfo")

    floor = Decimal("0")
    floor_field = None

    if isinstance(mempool, dict):
        for field in ("minrelaytxfee", "mempoolminfee"):
            value = mempool.get(field)

            if value is None:
                continue

            try:
                candidate = coin_per_kvb_to_sat_vb(value)
            except (ValueError, TypeError, ArithmeticError):
                continue

            if candidate > floor:
                floor = candidate
                floor_field = field

    return floor, floor_field


def funding_fee_rate(cli, chain):
    """
    Explicit feerate for a wallet-built funding transaction.

    Funding deliberately does not use the settlement-only
    BLAKE_SWAP_{BTC,BLAKE}_SPEND_FEE_* overrides or --fee-rate.
    It derives a rate from the node estimator and current policy floors,
    then applies any wallet-construction floor required by that chain.
    """
    estimate = cli(
        "estimatesmartfee",
        funding_fee_target_blocks(chain),
    )

    mempool = cli("getmempoolinfo")

    rate = choose_fee_rate_sat_vb(
        estimate,
        mempool,
        multiplier=settlement_fee_multiplier(),
    )

    source = "dynamic"

    if chain == "btc":
        wallet_floor = btc_funding_min_fee_rate()

        if rate < wallet_floor:
            rate = wallet_floor
            source = "wallet-minimum"

    elif chain != "blake":
        raise ValueError(
            f"unknown funding-fee chain: {chain}"
        )

    return rate, source


def settlement_fee_rate(cli, chain, fee_rate=None):
    """
    The feerate to build a settlement transaction at, and where it came
    from.

    Returns `(rate, source)`. `fee_rate` is the `--fee-rate` parameter, if
    the caller passed one, and it outranks the environment: the
    environment is the host's default, while the client driving this
    particular swap is the party that knows what it wants to pay.

    With an explicit rate the node is still asked for its acceptance
    floors, because a rate below them cannot be relayed or admitted at
    all: failing here names the knob and the binding floor, where letting
    the node refuse later would surface as an unrelated-looking broadcast
    failure.

    Both floors are checked, not just `minrelaytxfee`: `mempoolminfee`
    rises above relay policy when the mempool is congested, and a
    transaction below it is rejected just as surely, if only for as long
    as the congestion lasts. An override that the node will not accept
    right now is a settlement that stalls, so it is refused with the
    node's own numbers.
    """
    if fee_rate is not None:
        override = parse_settlement_fee_rate(
            fee_rate,
            "--fee-rate",
        )
        source = "--fee-rate"
    else:
        override = settlement_fee_rate_override(chain)
        source = settlement_fee_rate_env_name(chain)

    if override is None:
        estimate = cli(
            "estimatesmartfee",
            settlement_fee_target_blocks(),
        )

        mempool = cli(
            "getmempoolinfo"
        )

        return (
            choose_fee_rate_sat_vb(
                estimate,
                mempool,
                multiplier=settlement_fee_multiplier(),
            ),
            "dynamic",
        )

    floor, floor_field = node_fee_rate_floor(cli)

    if floor > 0 and override < floor:
        if fee_rate is not None:
            advice = (
                "Raise it, or omit --fee-rate to fall back to "
                f"{settlement_fee_rate_env_name(chain)} and the node's "
                "estimator."
            )
        else:
            advice = (
                f"Raise the rate, or unset {source} to use the node's "
                "estimator."
            )

        raise ValueError(
            f"{source} is {override} sat/vB, below the node's {floor} "
            f"sat/vB {floor_field}; a transaction paying it would not "
            f"be accepted. {advice}"
        )

    return override, (
        "rate-option"
        if fee_rate is not None
        else "rate-override"
    )


def estimate_dynamic_settlement_fee(
    cli,
    *,
    vsize,
    input_sats=None,
    chain="btc",
):
    """
    Derive a settlement fee from the node's current estimator
    and policy floors.

    `chain` selects which per-chain feerate override is honoured, so a
    caller on the BLAKE leg is not steered by the BTC knob.

    This helper does not broadcast or mutate chain state.
    """
    rate, _source = settlement_fee_rate(cli, chain)

    fee_sats = fee_for_vsize(
        vsize,
        rate,
    )

    if input_sats is not None:
        enforce_fee_cap(
            fee_sats,
            input_sats,
            max_percent=settlement_fee_cap_percent(),
            absolute_cap_sats=settlement_fee_absolute_cap_sats(),
            vsize=vsize,
        )

    return fee_sats, rate


def btc_trade_minimum_sats():
    """
    Local settlement-feasibility floor for the BTC leg.

    An explicit legacy override remains available for emergency
    operator control. Otherwise derive the floor from current
    Bitcoin node fee conditions using a conservative settlement
    vsize estimate.
    """
    override = settlement_fee_override_sats("btc")

    if override is not None:
        return override + 1

    vsize = int(
        os.environ.get(
            "BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE",
            "200",
        )
    )

    if vsize < 1:
        raise ValueError(
            "BLAKE_SWAP_BTC_SETTLEMENT_VSIZE_ESTIMATE "
            "must be >= 1"
        )

    fee_sats, _rate = estimate_dynamic_settlement_fee(
        btc_cli,
        vsize=vsize,
        chain="btc",
    )

    return fee_sats + 1


def validate_btc_trade_amount(btc_sats, context="trade"):
    minimum = btc_trade_minimum_sats()
    btc_sats = int(btc_sats)

    if btc_sats < minimum:
        raise ValueError(
            f"{context} BTC amount {btc_sats} sats is too small; "
            f"minimum settlement-feasible amount is {minimum} sats "
            "under the current settlement fee policy"
        )

    return True


def command_publish_market_offer(args):
    import asyncio

    if args.blake <= 0:
        raise ValueError(
            "BLAKE amount must be positive"
        )

    if args.sats <= 0:
        raise ValueError(
            "BTC sat amount must be positive"
        )

    validate_btc_trade_amount(
        args.sats,
        context="market offer",
    )

    if args.minutes <= 0:
        raise ValueError(
            "Offer lifetime must be positive"
        )

    event, message = build_market_offer_v2_event(
        args.blake,
        args.sats,
        args.minutes,
        args.direction,
    )

    event_id = asyncio.run(
        nostr_publish_event(event)
    )

    print()
    print("MARKET OFFER PUBLISHED")
    print("======================")
    print("offer id:", event_id)
    print(
        "maker:   ",
        event["pubkey"],
    )
    terms = market_give_want(
        message["direction"],
        float(message["blake_amount"]),
        int(message["btc_sats"]),
    )

    roles = market_protocol_roles(
        message["direction"]
    )

    print(
        "giving:  ",
        terms["give"],
    )
    print(
        "wanting: ",
        terms["want"],
    )
    print(
        "direction:",
        message["direction"],
    )
    print(
        "maker protocol role:",
        roles["maker_role"],
    )
    print(
        "lifetime:",
        args.minutes,
        "minute(s)",
    )
    print(
        "expires: ",
        message["expires_at"],
    )
    print()
    print(
        "PASS: advertisement contains no secret, "
        "hashlock, HTLC, or chain wallet keys"
    )



def build_market_cancel_event(
    offer_id,
):
    import time
    from coincurve import PrivateKey

    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = private_key.public_key.format(
        compressed=True
    )

    pubkey_hex = compressed[1:].hex()
    created_at = int(time.time())

    message = {
        "type": "market-cancel",
        "version": 1,
        "offer_id": offer_id,
        "created_at": created_at,
    }

    content = json.dumps(
        message,
        separators=(",", ":"),
        sort_keys=True,
    )

    event = {
        "pubkey": pubkey_hex,
        "created_at": created_at,
        "kind": 1,
        "tags": [
            [
                "t",
                "blake-swap-market-cancel",
            ],
            ["e", offer_id],
        ],
        "content": content,
    }

    event_id = nostr_event_id(event)

    event["id"] = event_id.hex()

    event["sig"] = private_key.sign_schnorr(
        event_id
    ).hex()

    return event



def command_cancel_market_offer(args):
    import asyncio

    if len(args.offer_id) != 64:
        raise ValueError(
            "offer_id must be a 64-character "
            "Nostr event ID"
        )

    original = asyncio.run(
        nostr_fetch_event_by_id(
            args.offer_id
        )
    )

    original_message = json.loads(
        original["content"]
    )

    if (
        original_message.get("type")
        != "market-offer"
    ):
        raise RuntimeError(
            "Referenced event is not a "
            "market offer"
        )

    cancel_event = build_market_cancel_event(
        args.offer_id
    )

    if (
        cancel_event["pubkey"]
        != original["pubkey"]
    ):
        raise RuntimeError(
            "SECURITY FAILURE: current Nostr "
            "identity is not the maker of "
            "this offer"
        )

    cancel_id = asyncio.run(
        nostr_publish_event(
            cancel_event
        )
    )

    print()
    print("MARKET OFFER CANCELLED")
    print("======================")
    print("offer id: ", args.offer_id)
    print("cancel id:", cancel_id)
    print(
        "maker:    ",
        cancel_event["pubkey"],
    )
    print()
    print(
        "PASS: cancellation was signed "
        "by the original offer maker"
    )



def build_market_take_event(
    offer_event,
    offer_message,
):
    import time
    import secrets
    from coincurve import PrivateKey

    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = (
        private_key.public_key.format(
            compressed=True
        )
    )

    taker_pubkey = compressed[1:].hex()
    created_at = int(time.time())

    offer_version = offer_message.get(
        "version"
    )

    #
    # Historical v1 negotiation stays byte/schema
    # compatible with the original implementation.
    #
    if offer_version == 1:
        message = {
            "type": "market-take",
            "version": 1,

            "offer_id": offer_event["id"],
            "maker": offer_event["pubkey"],

            "base": "BLAKE",
            "quote": "BTC",

            "blake_amount": format_blake_amount(
                blake_base_units(
                    offer_message["blake_amount"]
                )
            ),
            "btc_sats": int(
                offer_message["btc_sats"]
            ),

            "created_at": created_at,
            "nonce": secrets.token_hex(8),
        }

        tags = [
            [
                "t",
                "blake-swap-market-take",
            ],
            [
                "e",
                offer_event["id"],
            ],
            [
                "p",
                offer_event["pubkey"],
            ],
        ]

    elif offer_version == 2:
        direction = offer_message.get(
            "direction"
        )

        roles = market_protocol_roles(
            direction
        )

        message = {
            "type": "market-take",
            "version": 2,

            "offer_id": offer_event["id"],
            "maker": offer_event["pubkey"],
            "taker": taker_pubkey,

            "base": "BLAKE",
            "quote": "BTC",

            "direction": direction,

            #
            # These roles are DERIVED from direction,
            # not chosen independently by the taker.
            #
            "maker_role":
                roles["maker_role"],
            "taker_role":
                roles["taker_role"],

            "blake_amount": format_blake_amount(
                blake_base_units(
                    offer_message["blake_amount"]
                )
            ),
            "btc_sats": int(
                offer_message["btc_sats"]
            ),

            "created_at": created_at,
            "nonce": secrets.token_hex(8),
        }

        tags = [
            [
                "t",
                "blake-swap-market-take",
            ],
            [
                "e",
                offer_event["id"],
            ],
            [
                "p",
                offer_event["pubkey"],
            ],
            [
                "direction",
                direction,
            ],
        ]

    else:
        raise RuntimeError(
            "Cannot build take for unsupported "
            f"market offer version {offer_version!r}"
        )

    content = json.dumps(
        message,
        separators=(",", ":"),
        sort_keys=True,
    )

    event = {
        "pubkey": taker_pubkey,
        "created_at": created_at,
        "kind": 1,
        "tags": tags,
        "content": content,
    }

    event_id = nostr_event_id(
        event
    )

    event["id"] = event_id.hex()

    event["sig"] = (
        private_key.sign_schnorr(
            event_id
        ).hex()
    )

    return event, message



async def nostr_fetch_market_take_events_from_relay(
    relay,
    offer_id,
    limit=100,
):
    import asyncio
    import secrets
    import websockets

    sub_id = (
        "blake-market-takes-"
        + secrets.token_hex(4)
    )

    event_filter = {
        "kinds": [1],
        "#t": ["blake-swap-market-take"],
        "#e": [offer_id],
        "limit": limit,
    }

    events = []

    async with websockets.connect(
        relay,
        open_timeout=10,
        close_timeout=5,
    ) as ws:

        await ws.send(
            json.dumps(
                [
                    "REQ",
                    sub_id,
                    event_filter,
                ]
            )
        )

        while True:
            raw = await asyncio.wait_for(
                ws.recv(),
                timeout=10,
            )

            response = json.loads(raw)

            if (
                isinstance(response, list)
                and len(response) >= 3
                and response[0] == "EVENT"
                and response[1] == sub_id
            ):
                events.append(
                    response[2]
                )

            elif (
                isinstance(response, list)
                and len(response) >= 2
                and response[0] == "EOSE"
                and response[1] == sub_id
            ):
                break

        await ws.send(
            json.dumps(
                ["CLOSE", sub_id]
            )
        )

    return events


def nostr_validate_market_take_event(
    event,
    original,
    offer_message,
    offer_id,
):
    import json

    #
    # Canonical definition of a valid market take.
    #
    # Event source -- relay, cache, or explicit caller
    # evidence -- grants no protocol trust.
    #
    verify_nostr_event(event)

    if event.get("kind") != 1:
        raise RuntimeError(
            "Market take Nostr kind is not 1"
        )

    if not any(
        len(tag) >= 2
        and tag[0] == "t"
        and tag[1] == "blake-swap-market-take"
        for tag in event.get("tags", [])
    ):
        raise RuntimeError(
            "Market take is missing expected "
            "Nostr market-take tag"
        )

    #
    # Authentic immutable evidence may be retained.
    # This does not imply protocol validity.
    #
    nostr_cache_event(event)

    try:
        message = json.loads(
            event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market take contains invalid JSON"
        ) from e

    if message.get("type") != "market-take":
        raise RuntimeError(
            "Event is not a market take"
        )

    version = message.get(
        "version"
    )

    if version not in (1, 2):
        raise RuntimeError(
            "Unsupported market take version"
        )

    offer_version = offer_message.get(
        "version"
    )

    #
    # Negotiation versions must move together.
    # A v2 offer cannot be downgraded by answering
    # it with an old v1 take.
    #
    if version != offer_version:
        raise RuntimeError(
            "Market take version does not match "
            "market offer version"
        )

    if message.get("offer_id") != offer_id:
        raise RuntimeError(
            "Market take references another offer"
        )

    if (
        message.get("maker")
        != original["pubkey"]
    ):
        raise RuntimeError(
            "Market take maker mismatch"
        )

    if message.get("base") != "BLAKE":
        raise RuntimeError(
            "Market take base mismatch"
        )

    if message.get("quote") != "BTC":
        raise RuntimeError(
            "Market take quote mismatch"
        )

    #
    # v2 explicitly commits both parties to the
    # economic direction and its derived protocol roles.
    #
    if version == 2:
        direction = offer_message.get(
            "direction"
        )

        if direction not in MARKET_DIRECTIONS:
            raise RuntimeError(
                "Market offer has invalid direction"
            )

        if message.get("direction") != direction:
            raise RuntimeError(
                "Market take changed direction"
            )

        roles = market_protocol_roles(
            direction
        )

        if (
            message.get("maker_role")
            != roles["maker_role"]
        ):
            raise RuntimeError(
                "Market take maker role mismatch"
            )

        if (
            message.get("taker_role")
            != roles["taker_role"]
        ):
            raise RuntimeError(
                "Market take taker role mismatch"
            )

        if message.get("taker") != event["pubkey"]:
            raise RuntimeError(
                "Market take taker identity mismatch"
            )

        if not any(
            len(tag) >= 2
            and tag[0] == "direction"
            and tag[1] == direction
            for tag in event.get("tags", [])
        ):
            raise RuntimeError(
                "Market take direction does not "
                "match Nostr direction tag"
            )

    else:
        direction = (
            MARKET_DIRECTION_BLAKE_FOR_BTC
        )

        roles = market_protocol_roles(
            direction
        )

    if (
        float(message["blake_amount"])
        != float(
            offer_message["blake_amount"]
        )
    ):
        raise RuntimeError(
            "Market take changed BLAKE terms"
        )

    if (
        int(message["btc_sats"])
        != int(
            offer_message["btc_sats"]
        )
    ):
        raise RuntimeError(
            "Market take changed BTC terms"
        )

    if (
        message.get("created_at")
        != event["created_at"]
    ):
        raise RuntimeError(
            "Market take timestamp mismatch"
        )

    if not any(
        len(tag) >= 2
        and tag[0] == "e"
        and tag[1] == offer_id
        for tag in event["tags"]
    ):
        raise RuntimeError(
            "Market take does not reference "
            "the offer"
        )

    if not any(
        len(tag) >= 2
        and tag[0] == "p"
        and tag[1] == original["pubkey"]
        for tag in event["tags"]
    ):
        raise RuntimeError(
            "Market take does not tag maker"
        )

    return {
        "event_id": event["id"],
        "taker": event["pubkey"],
        "created_at": event[
            "created_at"
        ],
        "offer_id": offer_id,
        "maker": original[
            "pubkey"
        ],
        "blake_amount": float(
            message["blake_amount"]
        ),
        #
        # Exact base units alongside the display value; see
        # command_dashboard's JSON view for why both are emitted.
        #
        "blake_amount_base_units": blake_base_units(
            message["blake_amount"]
        ),
        "btc_sats": int(
            message["btc_sats"]
        ),
        "direction": direction,
        "maker_role":
            roles["maker_role"],
        "taker_role":
            roles["taker_role"],
    }


async def nostr_fetch_market_takes(
    offer_id,
    limit=100,
    extra_events=None,
    allow_expired=False,
    allow_matched=False,
):
    import asyncio
    import time

    # The offer itself is authoritative for
    # maker identity and economic terms.
    original = await nostr_fetch_event_by_id(
        offer_id
    )

    #
    # Canonical parsing establishes the offer's
    # authenticity, schema, maker, and economic terms.
    #
    offer_message, offer = (
        nostr_parse_market_offer_event(
            original
        )
    )

    #
    # The exact verified offer is authoritative
    # evidence that the offer exists. We do NOT
    # require a broad relay scan to rediscover it.
    #
    # We query current market events only to gather
    # additional state evidence such as cancellations.
    #
    market_tasks = [
        nostr_fetch_market_events_from_relay(
            relay,
            limit=200,
        )
        for relay in NOSTR_RELAYS
    ]

    market_results = await asyncio.gather(
        *market_tasks,
        return_exceptions=True,
    )

    market_events = {}

    for result in market_results:
        if isinstance(result, Exception):
            continue

        for event in result:
            try:
                verify_nostr_event(event)

                #
                # Preserve authentic immutable evidence
                # before interpreting protocol semantics.
                #
                nostr_cache_event(event)

            except Exception:
                continue

            event_id = event.get("id")

            if isinstance(event_id, str):
                market_events.setdefault(
                    event_id,
                    event,
                )

    #
    # Merge all cryptographically authentic cached
    # Nostr evidence with current relay evidence.
    #
    # Storage does not decide whether an event is an
    # offer, cancellation, authorized, or relevant.
    # The canonical state deriver owns those semantics.
    #
    cached_market_events = (
        nostr_cached_market_events()
    )

    for event in cached_market_events:
        market_events.setdefault(
            event["id"],
            event,
        )

    #
    # ONE canonical definition of market state.
    #
    derived = nostr_derive_market_offer_state(
        original,
        market_events.values(),
    )

    if (
        derived["state"] == "EXPIRED"
        and not allow_expired
    ):
        raise RuntimeError(
            "Original offer is expired"
        )

    if derived["state"] == "CANCELLED":
        raise RuntimeError(
            "Original offer was cancelled by maker: "
            + derived["cancel_event_id"]
        )

    if (
        derived["state"] != "OPEN"
        and not (
            allow_expired
            and derived["state"] == "EXPIRED"
        )
        and not (
            allow_matched
            and derived["state"] == "MATCHED"
        )
    ):
        raise RuntimeError(
            "Unknown market offer state: "
            + str(derived["state"])
        )

    tasks = [
        nostr_fetch_market_take_events_from_relay(
            relay,
            offer_id,
            limit=limit,
        )
        for relay in NOSTR_RELAYS
    ]

    results = await asyncio.gather(
        *tasks,
        return_exceptions=True,
    )

    events_by_id = {}
    successful_relays = 0

    for relay, result in zip(
        NOSTR_RELAYS,
        results,
    ):
        if isinstance(result, Exception):
            continue

        successful_relays += 1

        for event in result:
            if not isinstance(event, dict):
                continue

            event_id = event.get("id")

            if not isinstance(event_id, str):
                continue

            events_by_id.setdefault(
                event_id,
                event,
            )

    #
    # A caller may already possess the exact immutable event
    # it wants validated.  Merge it into the candidate set
    # rather than forcing the relay network to rediscover it.
    #
    for event in (extra_events or []):
        if not isinstance(event, dict):
            continue

        event_id = event.get("id")

        if not isinstance(event_id, str):
            continue

        events_by_id.setdefault(
            event_id,
            event,
        )

    #
    # Merge all durable cryptographically authentic
    # Nostr evidence into the candidate set.
    #
    # Storage does not decide whether an event is
    # actually a market take or whether it applies
    # to this offer.  The validator below owns those
    # semantics.
    #
    for event in nostr_cached_market_events():
        if not isinstance(event, dict):
            continue

        event_id = event.get("id")

        if not isinstance(event_id, str):
            continue

        events_by_id.setdefault(
            event_id,
            event,
        )

    if (
        successful_relays == 0
        and not events_by_id
    ):
        raise RuntimeError(
            "Could not query any configured "
            "Nostr relay for market takes and no "
            "local candidate events were supplied"
        )

    valid = []

    for event in events_by_id.values():
        try:
            item = nostr_validate_market_take_event(
                event,
                original,
                offer_message,
                offer_id,
            )

            valid.append(item)

        except Exception:
            # Every relay, cached event, and explicit
            # candidate remains untrusted.
            continue

    valid.sort(
        key=lambda x: (
            x["created_at"],
            x["event_id"],
        ),
        reverse=True,
    )

    print(
        "Nostr take read:",
        len(events_by_id),
        "unique event(s) from",
        f"{successful_relays}/"
        f"{len(NOSTR_RELAYS)} relay(s)",
    )

    return original, offer_message, valid


def command_market_takes(args):
    import asyncio
    import json
    from coincurve import PrivateKey

    offer_id = resolve_cached_nostr_prefix(
        args.offer_id,
        expected_type="market-offer",
        label="market offer",
        announce=not args.json,
    )

    original, offer_message, takes = (
        asyncio.run(
            nostr_fetch_market_takes(
                offer_id,
                limit=args.limit,
                allow_matched=True,
            )
        )
    )

    # This command is Alice's inbox.
    # Refuse to inspect somebody else's offer
    # as though it were ours.
    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = (
        private_key.public_key.format(
            compressed=True
        )
    )

    alice_pubkey = compressed[1:].hex()

    if original["pubkey"] != alice_pubkey:
        raise RuntimeError(
            "SECURITY FAILURE: "
            "this market offer was not signed "
            "by the current Alice identity"
        )

    result = {
        "offer_id": offer_id,
        "takes": takes,
    }

    if args.json:
        print(
            json.dumps(
                result,
                indent=2,
                sort_keys=True,
            )
        )
        return

    print()
    print("TAKES FOR MARKET OFFER")
    print("======================")
    print()
    print("offer:", offer_id)
    print(
        "selling:",
        offer_message["blake_amount"],
        "BLAKE",
    )
    print(
        "asking: ",
        f'{int(offer_message["btc_sats"]):,}',
        "sats",
    )
    print()

    if not takes:
        print("No valid takes found.")
        print()
        print(
            "PASS: offer is open, but nobody "
            "has submitted a valid take"
        )
        return

    print(
        "Take          Taker          "
        "Selling        Asking"
    )
    print(
        "------------  ------------   "
        "------------   ------------"
    )

    for take in takes:
        print(
            f'{take["event_id"][:12]:<12}  '
            f'{take["taker"][:12]:<12}   '
            f'{take["blake_amount"]:g} BLAKE'
            f'{"":<4} '
            f'{take["btc_sats"]:,} sats'
        )

    print()
    print(
        f"{len(takes)} valid take(s)"
    )
    print()

    print("FULL TAKE ID(S)")
    print("===============")

    for take in takes:
        print(take["event_id"])

    print()
    print(
        "PASS: original offer is still open"
    )
    print(
        "PASS: every take signature verifies"
    )
    print(
        "PASS: every take references this "
        "exact offer"
    )
    print(
        "PASS: maker and economic terms match"
    )
    print(
        "PASS: no swap was created"
    )
    print(
        "PASS: no money moved"
    )





def current_nostr_pubkey():
    from coincurve import PrivateKey

    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = (
        private_key.public_key.format(
            compressed=True
        )
    )

    return compressed[1:].hex()


def build_market_match_event(
    offer_event,
    take_event,
    swap,
):
    import time

    maker_pubkey = current_nostr_pubkey()
    created_at = int(time.time())

    if maker_pubkey != offer_event["pubkey"]:
        raise RuntimeError(
            "SECURITY FAILURE: market match "
            "must be signed by offer maker"
        )

    offer_message, parsed_offer = (
        nostr_parse_market_offer_event(
            offer_event
        )
    )

    offer_version = offer_message.get(
        "version"
    )

    #
    # Validate the exact take through the canonical
    # take validator before building a match around it.
    #
    take_item = (
        nostr_validate_market_take_event(
            take_event,
            offer_event,
            offer_message,
            offer_event["id"],
        )
    )

    if offer_version == 1:
        #
        # Preserve historical v1 match schema.
        #
        message = {
            "type": "market-match",
            "version": 1,

            "offer_id": offer_event["id"],
            "take_id": take_event["id"],
            "swap_id": swap.swap_id,

            "maker": maker_pubkey,
            "taker": take_event["pubkey"],

            "base": "BLAKE",
            "quote": "BTC",

            "blake_amount": format_blake_amount(
                swap_blake_base_units(swap)
            ),
            "btc_sats": int(
                swap.btc_sats
            ),

            "created_at": created_at,
        }

        extra_tags = []

    elif offer_version == 2:
        direction = parsed_offer[
            "direction"
        ]

        roles = market_protocol_roles(
            direction
        )

        #
        # The take already committed to these roles.
        # Do not allow the match builder to reinterpret
        # them.
        #
        if (
            take_item["maker_role"]
            != roles["maker_role"]
        ):
            raise RuntimeError(
                "Market take maker role disagrees "
                "with offer direction"
            )

        if (
            take_item["taker_role"]
            != roles["taker_role"]
        ):
            raise RuntimeError(
                "Market take taker role disagrees "
                "with offer direction"
            )

        message = {
            "type": "market-match",
            "version": 2,

            "offer_id": offer_event["id"],
            "take_id": take_event["id"],
            "swap_id": swap.swap_id,

            "maker": maker_pubkey,
            "taker": take_event["pubkey"],

            "base": "BLAKE",
            "quote": "BTC",

            "direction": direction,
            "maker_role":
                roles["maker_role"],
            "taker_role":
                roles["taker_role"],

            "blake_amount": format_blake_amount(
                swap_blake_base_units(swap)
            ),
            "btc_sats": int(
                swap.btc_sats
            ),

            "created_at": created_at,
        }

        extra_tags = [
            [
                "direction",
                direction,
            ],
        ]

    else:
        raise RuntimeError(
            "Cannot build match for unsupported "
            f"market offer version {offer_version!r}"
        )

    content = json.dumps(
        message,
        separators=(",", ":"),
        sort_keys=True,
    )

    event = {
        "pubkey": maker_pubkey,
        "created_at": created_at,
        "kind": 1,
        "tags": [
            [
                "t",
                "blake-swap-market-match",
            ],
            [
                "e",
                offer_event["id"],
            ],
            [
                "e",
                take_event["id"],
            ],
            [
                "p",
                take_event["pubkey"],
            ],
            [
                "d",
                swap.swap_id,
            ],
            *extra_tags,
        ],
        "content": content,
    }

    event_id = nostr_event_id(
        event
    )

    from coincurve import PrivateKey

    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    event["id"] = event_id.hex()

    event["sig"] = (
        private_key.sign_schnorr(
            event_id
        ).hex()
    )

    return event, message



def command_publish_market_match(args):
    import asyncio
    import json

    if len(args.take_id) != 64:
        raise ValueError(
            "take_id must be the full "
            "64-character Nostr event ID"
        )

    selection_path = (
        BASE_SWAP_DIR
        / "market-selections"
        / f"{args.take_id}.json"
    )

    if not selection_path.exists():
        raise RuntimeError(
            "Alice has not accepted this "
            "market take"
        )

    with open(selection_path) as f:
        selection = json.load(f)

    swap_id = selection.get(
        "swap_id"
    )

    if not swap_id:
        raise RuntimeError(
            "Market selection has no swap_id"
        )

    swap = load_swap(
        swap_id,
        role="alice",
    )

    take_event = asyncio.run(
        nostr_fetch_event_by_id(
            args.take_id
        )
    )

    verify_nostr_event(take_event)

    try:
        take_message = json.loads(
            take_event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Take content is invalid JSON"
        ) from e

    if take_message.get("type") != "market-take":
        raise RuntimeError(
            "Referenced event is not "
            "a market take"
        )

    offer_id = selection.get(
        "offer_id"
    )

    if (
        take_message.get("offer_id")
        != offer_id
    ):
        raise RuntimeError(
            "SECURITY FAILURE: take references "
            "a different market offer"
        )

    offer_event = asyncio.run(
        nostr_fetch_event_by_id(
            offer_id
        )
    )

    verify_nostr_event(
        offer_event
    )

    try:
        offer_message = json.loads(
            offer_event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Offer content is invalid JSON"
        ) from e

    if (
        offer_message.get("type")
        != "market-offer"
    ):
        raise RuntimeError(
            "Referenced event is not "
            "a market offer"
        )

    # A market match must itself be created while the
    # advertised offer is still live.  A durable local
    # selection may allow recovery/inspection of an old
    # negotiation, but it must not create a newly signed
    # post-expiry match that counterparties will reject.
    if (
        int(offer_message["expires_at"])
        <= int(time.time())
    ):
        raise RuntimeError(
            "Market offer expired before match publication; "
            "publish a fresh offer instead"
        )

    alice_pub = current_nostr_pubkey()
    bob_pub = take_event["pubkey"]

    if offer_event["pubkey"] != alice_pub:
        raise RuntimeError(
            "SECURITY FAILURE: market offer "
            "was not signed by current Alice"
        )

    if (
        selection.get("nostr_alice_pub")
        != alice_pub
    ):
        raise RuntimeError(
            "SECURITY FAILURE: stored Alice "
            "identity mismatch"
        )

    if (
        selection.get("nostr_bob_pub")
        != bob_pub
    ):
        raise RuntimeError(
            "SECURITY FAILURE: stored Bob "
            "identity mismatch"
        )

    if swap.nostr_alice_pub != alice_pub:
        raise RuntimeError(
            "SECURITY FAILURE: swap Alice "
            "identity mismatch"
        )

    if swap.nostr_bob_pub != bob_pub:
        raise RuntimeError(
            "SECURITY FAILURE: swap Bob "
            "identity mismatch"
        )

    if (
        blake_base_units(offer_message["blake_amount"])
        != swap.blake_amount_base_units
    ):
        raise RuntimeError(
            "SECURITY FAILURE: BLAKE terms "
            "changed"
        )

    if (
        int(offer_message["btc_sats"])
        != int(swap.btc_sats)
    ):
        raise RuntimeError(
            "SECURITY FAILURE: BTC terms changed"
        )

    event, message = (
        build_market_match_event(
            offer_event,
            take_event,
            swap,
        )
    )

    verify_nostr_event(event)

    match_id = asyncio.run(
        nostr_publish_event(event)
    )

    selection[
        "market_match_id"
    ] = match_id

    #
    # Durable: this is what binds the published match to the swap, and a
    # crash that lost it would leave the match unresolvable locally.
    #
    atomic_write_json(
        selection_path,
        selection,
        fsync_directory=True,
    )

    print()
    print("MARKET MATCH PUBLISHED")
    print("======================")
    print("offer id:", offer_id)
    print("take id: ", args.take_id)
    print("match id:", match_id)
    print("swap id: ", swap.swap_id)
    print()
    print(
        "PASS: match is signed by the offer's maker"
    )
    print(
        "PASS: the exact signed take is referenced"
    )
    print(
        "PASS: no secret was published"
    )
    print(
        "PASS: no funds moved"
    )

    return match_id


async def nostr_fetch_market_match_events_from_relay(
    relay,
    bob_pubkey,
    limit=100,
):
    import asyncio
    import secrets
    import websockets

    sub_id = (
        "blake-market-matches-"
        + secrets.token_hex(4)
    )

    event_filter = {
        "kinds": [1],
        "#t": [
            "blake-swap-market-match"
        ],
        "#p": [bob_pubkey],
        "limit": limit,
    }

    events = []

    async with websockets.connect(
        relay,
        open_timeout=10,
        close_timeout=5,
    ) as ws:

        await ws.send(
            json.dumps(
                [
                    "REQ",
                    sub_id,
                    event_filter,
                ]
            )
        )

        while True:
            raw = await asyncio.wait_for(
                ws.recv(),
                timeout=10,
            )

            response = json.loads(raw)

            if (
                isinstance(response, list)
                and len(response) >= 3
                and response[0] == "EVENT"
                and response[1] == sub_id
            ):
                events.append(
                    response[2]
                )

            elif (
                isinstance(response, list)
                and len(response) >= 2
                and response[0] == "EOSE"
                and response[1] == sub_id
            ):
                break

        await ws.send(
            json.dumps(
                ["CLOSE", sub_id]
            )
        )

    return events



def validate_market_match_evidence(
    event,
    offer_event,
    take_event,
    expected_taker_pubkey=None,
):
    """
    Pure validation of one market-match against its exact
    referenced offer and take.

    No relay access occurs here.

    This is the canonical definition of a protocol-valid
    MATCHED terminal event.
    """
    import json

    verify_nostr_event(event)

    if event.get("kind") != 1:
        raise RuntimeError(
            "Market match Nostr kind is not 1"
        )

    tags = event.get("tags", [])

    if not any(
        len(tag) >= 2
        and tag[0] == "t"
        and tag[1] == "blake-swap-market-match"
        for tag in tags
    ):
        raise RuntimeError(
            "Market match is missing expected "
            "Nostr market-match tag"
        )

    try:
        message = json.loads(
            event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market match contains invalid JSON"
        ) from e

    if message.get("type") != "market-match":
        raise RuntimeError(
            "Event is not a market match"
        )

    version = message.get("version")

    if version not in (1, 2):
        raise RuntimeError(
            "Unsupported market match version"
        )

    if (
        message.get("created_at")
        != event.get("created_at")
    ):
        raise RuntimeError(
            "Market match timestamp mismatch"
        )

    if message.get("base") != "BLAKE":
        raise RuntimeError(
            "Market match base mismatch"
        )

    if message.get("quote") != "BTC":
        raise RuntimeError(
            "Market match quote mismatch"
        )

    if version == 1:
        direction = (
            MARKET_DIRECTION_BLAKE_FOR_BTC
        )
        roles = market_protocol_roles(
            direction
        )

    else:
        direction = message.get(
            "direction"
        )

        if direction not in MARKET_DIRECTIONS:
            raise RuntimeError(
                "Market match has invalid direction"
            )

        roles = market_protocol_roles(
            direction
        )

        if (
            message.get("maker_role")
            != roles["maker_role"]
        ):
            raise RuntimeError(
                "Market match maker role mismatch"
            )

        if (
            message.get("taker_role")
            != roles["taker_role"]
        ):
            raise RuntimeError(
                "Market match taker role mismatch"
            )

        if not any(
            len(tag) >= 2
            and tag[0] == "direction"
            and tag[1] == direction
            for tag in tags
        ):
            raise RuntimeError(
                "Market match direction does not "
                "match Nostr direction tag"
            )

    offer_id = message.get("offer_id")
    take_id = message.get("take_id")
    swap_id = message.get("swap_id")

    def require_hex(value, length, label):
        if (
            not isinstance(value, str)
            or len(value) != length
        ):
            raise RuntimeError(
                f"Invalid {label}"
            )

        try:
            int(value, 16)
        except ValueError as e:
            raise RuntimeError(
                f"{label} is not hex"
            ) from e

    require_hex(
        offer_id,
        64,
        "market offer ID",
    )

    require_hex(
        take_id,
        64,
        "market take ID",
    )

    require_hex(
        swap_id,
        12,
        "atomic swap ID",
    )

    if offer_event.get("id") != offer_id:
        raise RuntimeError(
            "Market match references a different offer"
        )

    if take_event.get("id") != take_id:
        raise RuntimeError(
            "Market match references a different take"
        )

    if not any(
        len(tag) >= 2
        and tag[0] == "d"
        and tag[1] == swap_id
        for tag in tags
    ):
        raise RuntimeError(
            "Market match swap_id does not match "
            "Nostr d-tag"
        )

    e_refs = {
        tag[1]
        for tag in tags
        if (
            len(tag) >= 2
            and tag[0] == "e"
        )
    }

    if offer_id not in e_refs:
        raise RuntimeError(
            "Market match does not reference "
            "the offer"
        )

    if take_id not in e_refs:
        raise RuntimeError(
            "Market match does not reference "
            "the take"
        )

    #
    # Canonical offer validation.
    #
    offer_message, parsed_offer = (
        nostr_parse_market_offer_event(
            offer_event
        )
    )

    maker = parsed_offer["maker"]

    if event.get("pubkey") != maker:
        raise RuntimeError(
            "SECURITY FAILURE: market match "
            "was not signed by original maker"
        )

    if message.get("maker") != maker:
        raise RuntimeError(
            "Market match maker mismatch"
        )

    #
    # Canonical take validation.
    #
    take_item = nostr_validate_market_take_event(
        take_event,
        offer_event,
        offer_message,
        offer_id,
    )

    taker = take_item["taker"]

    if message.get("taker") != taker:
        raise RuntimeError(
            "Market match taker disagrees "
            "with referenced take"
        )

    if (
        expected_taker_pubkey is not None
        and taker != expected_taker_pubkey
    ):
        raise RuntimeError(
            "Market match is for another taker"
        )

    if not any(
        len(tag) >= 2
        and tag[0] == "p"
        and tag[1] == taker
        for tag in tags
    ):
        raise RuntimeError(
            "Market match does not tag taker"
        )

    if (
        take_item["direction"]
        != direction
    ):
        raise RuntimeError(
            "Market match direction disagrees "
            "with referenced take"
        )

    if (
        take_item["maker_role"]
        != roles["maker_role"]
    ):
        raise RuntimeError(
            "Market match maker role disagrees "
            "with referenced take"
        )

    if (
        take_item["taker_role"]
        != roles["taker_role"]
    ):
        raise RuntimeError(
            "Market match taker role disagrees "
            "with referenced take"
        )

    try:
        match_blake = float(
            message["blake_amount"]
        )
        match_sats = int(
            message["btc_sats"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market match has invalid economic terms"
        ) from e

    if (
        match_blake
        != float(
            offer_message["blake_amount"]
        )
    ):
        raise RuntimeError(
            "Match changed BLAKE terms"
        )

    if (
        match_sats
        != int(
            offer_message["btc_sats"]
        )
    ):
        raise RuntimeError(
            "Match changed BTC terms"
        )

    #
    # A maker cannot select a take before that take exists.
    #
    if (
        int(event["created_at"])
        < int(take_event["created_at"])
    ):
        raise RuntimeError(
            "Market match predates the take"
        )

    #
    # Canonical expiry boundary:
    #
    # At expires_at the offer is already EXPIRED.
    # Match must therefore be strictly earlier.
    #
    if (
        int(event["created_at"])
        >= int(offer_message["expires_at"])
    ):
        raise RuntimeError(
            "Market match was created at or after "
            "offer expiry"
        )

    return {
        "event_id": event["id"],
        "offer_id": offer_id,
        "take_id": take_id,
        "swap_id": swap_id,
        "maker": maker,
        "taker": taker,
        "blake_amount": float(
            offer_message["blake_amount"]
        ),
        "btc_sats": int(
            offer_message["btc_sats"]
        ),
        "direction": direction,
        "maker_role":
            roles["maker_role"],
        "taker_role":
            roles["taker_role"],
        "created_at":
            event["created_at"],
    }


async def validate_market_match_event(
    event,
    expected_taker_pubkey,
):
    """
    Network wrapper around the canonical pure
    market-match evidence validator.
    """
    import asyncio
    import json

    verify_nostr_event(event)

    try:
        message = json.loads(
            event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market match contains invalid JSON"
        ) from e

    offer_id = message.get(
        "offer_id"
    )

    take_id = message.get(
        "take_id"
    )

    if (
        not isinstance(offer_id, str)
        or len(offer_id) != 64
    ):
        raise RuntimeError(
            "Invalid market offer ID"
        )

    if (
        not isinstance(take_id, str)
        or len(take_id) != 64
    ):
        raise RuntimeError(
            "Invalid market take ID"
        )

    # Immutable referenced evidence is safe to satisfy from the
    # verified local cache.  nostr_load_cached_event() re-verifies
    # the signature and exact event-ID binding on every use.
    #
    # Only fall back to relay lookup for cache misses.  This avoids
    # repeated network round trips when validating historical matches.
    offer_event = nostr_load_cached_event(offer_id)
    take_event = nostr_load_cached_event(take_id)

    missing = []

    if offer_event is None:
        missing.append(("offer", offer_id))

    if take_event is None:
        missing.append(("take", take_id))

    if missing:
        fetched = await asyncio.gather(
            *[
                nostr_fetch_event_by_id(event_id)
                for _, event_id in missing
            ]
        )

        for (kind, _), fetched_event in zip(
            missing,
            fetched,
        ):
            if kind == "offer":
                offer_event = fetched_event
            else:
                take_event = fetched_event

    return validate_market_match_evidence(
        event,
        offer_event,
        take_event,
        expected_taker_pubkey=(
            expected_taker_pubkey
        ),
    )


async def nostr_fetch_market_matches(
    expected_taker_pubkey,
    limit=100,
    dependency_fetch_after=None,
):
    """
    Discover and fully validate market-match events
    addressed to one taker identity.

    Relay results are discovery only. Every candidate
    is cryptographically and protocol validated before
    it is returned.
    """
    import asyncio

    tasks = [
        nostr_fetch_market_match_events_from_relay(
            relay,
            expected_taker_pubkey,
            limit=limit,
        )
        for relay in NOSTR_RELAYS
    ]

    results = await asyncio.gather(
        *tasks,
        return_exceptions=True,
    )

    events_by_id = {}

    for result in results:
        if isinstance(result, Exception):
            continue

        for event in result:
            if not isinstance(event, dict):
                continue

            event_id = event.get("id")

            if not isinstance(event_id, str):
                continue

            events_by_id.setdefault(
                event_id,
                event,
            )

    #
    # Cached authentic events may contain older matches
    # no longer present in a bounded relay window.
    #
    for event in nostr_cached_market_events():
        try:
            verify_nostr_event(event)
        except Exception:
            continue

        event_id = event.get("id")

        if not isinstance(event_id, str):
            continue

        events_by_id.setdefault(
            event_id,
            event,
        )

    matches = []

    for event in events_by_id.values():
        #
        # `dependency_fetch_after` is a live-work optimization only.
        #
        # Older signed matches are not declared invalid.  Explicit
        # historical commands remain exhaustive because they leave this
        # argument unset.
        #
        # For `next`, however, an event already outside the automatic
        # coordination window cannot become automatic work.  If its
        # referenced offer/take evidence is also missing from the
        # verified local cache, avoid going back to the relays merely to
        # reconstruct historical evidence.
        #
        if dependency_fetch_after is not None:
            try:
                verify_nostr_event(event)

                created_at = int(
                    event.get("created_at", 0)
                )

                if created_at < int(dependency_fetch_after):
                    message = json.loads(
                        event["content"]
                    )

                    offer_id = message.get(
                        "offer_id"
                    )
                    take_id = message.get(
                        "take_id"
                    )

                    if (
                        isinstance(offer_id, str)
                        and len(offer_id) == 64
                        and isinstance(take_id, str)
                        and len(take_id) == 64
                    ):
                        try:
                            offer_cached = (
                                nostr_load_cached_event(
                                    offer_id
                                )
                                is not None
                            )
                            take_cached = (
                                nostr_load_cached_event(
                                    take_id
                                )
                                is not None
                            )
                        except Exception:
                            offer_cached = False
                            take_cached = False

                        if not (
                            offer_cached
                            and take_cached
                        ):
                            continue

            except Exception:
                #
                # Malformed candidates still fall through to the
                # canonical validator below, which remains authoritative.
                #
                pass

        try:
            item = await validate_market_match_event(
                event,
                expected_taker_pubkey,
            )
        except Exception:
            continue

        matches.append(
            item
        )

    matches.sort(
        key=lambda item: (
            item["created_at"],
            item["event_id"],
        ),
        reverse=True,
    )

    return matches


def command_market_matches(args):
    import asyncio

    bob_pubkey = current_nostr_pubkey()

    matches = asyncio.run(
        nostr_fetch_market_matches(
            bob_pubkey,
            limit=args.limit,
        )
    )

    print()
    print(
        "MARKET MATCHES FOR TAKER",
        bob_pubkey[:12],
    )
    print("=" * 40)
    print()
    print(
        "Market role: taker. The maker/taker roles are market roles; "
        "the protocol role each one binds depends on the direction."
    )
    print()

    if not matches:
        print("No valid matches found.")
        return

    for i, item in enumerate(
        matches,
        1,
    ):
        print(f"MATCH {i}")
        print("-" * 40)
        print(
            "match id:",
            item["event_id"],
        )
        print(
            "offer id:",
            item["offer_id"],
        )
        print(
            "take id: ",
            item["take_id"],
        )
        print(
            "swap id: ",
            item["swap_id"],
        )
        print(
            "maker:   ",
            item["maker"],
        )
        print(
            "taker:   ",
            item["taker"],
        )

        labels = market_role_labels(
            item.get("direction")
        )

        print(
            "direction:",
            item.get("direction", "unknown"),
        )
        print(
            "maker is protocol:",
            labels["maker"],
        )
        print(
            "taker is protocol:",
            labels["taker"],
        )

        print(
            "selling: ",
            item["blake_amount"],
            "BLAKE",
        )
        print(
            "asking:  ",
            f'{item["btc_sats"]:,}',
            "sats",
        )
        print()

    print(
        f"{len(matches)} valid match(es)"
    )
    print()
    print(
        "PASS: every match signature verifies"
    )
    print(
        "PASS: maker identity verified"
    )
    print(
        "PASS: taker identity verified"
    )
    print(
        "PASS: market terms preserved"
    )


def command_accept_reverse_market_match(args):
    """
    Import a validated v2 btc-for-blake market match
    as the TAKING participant.

    For this direction:

        market maker -> protocol Bob
        market taker -> protocol Alice

    This command is the point where protocol Alice
    creates secret S.
    """
    import asyncio

    if len(args.match_id) != 64:
        raise ValueError(
            "match_id must be the full "
            "64-character Nostr event ID"
        )

    alice_pubkey = current_nostr_pubkey()

    event = asyncio.run(
        nostr_fetch_event_by_id(
            args.match_id
        )
    )

    #
    # The validator requires the exact expected taker
    # identity.  In reverse direction, the taker is Alice.
    #
    item = asyncio.run(
        validate_market_match_event(
            event,
            alice_pubkey,
        )
    )

    if (
        item.get("direction")
        != MARKET_DIRECTION_BTC_FOR_BLAKE
    ):
        raise RuntimeError(
            "Market match is not btc-for-blake"
        )

    if item.get("maker_role") != "bob":
        raise RuntimeError(
            "SECURITY FAILURE: reverse match "
            "maker is not protocol Bob"
        )

    if item.get("taker_role") != "alice":
        raise RuntimeError(
            "SECURITY FAILURE: reverse match "
            "taker is not protocol Alice"
        )

    if item.get("taker") != alice_pubkey:
        raise RuntimeError(
            "SECURITY FAILURE: current participant "
            "is not the matched taker"
        )

    bob_pubkey = item["maker"]
    swap_id = item["swap_id"]

    #
    # Idempotency:
    #
    # If Alice already created this swap, reuse it.
    # NEVER generate a second S for the same negotiated swap.
    #
    existing_path = swap_path(
        swap_id,
        role="alice",
    )

    if existing_path.exists():
        existing = load_swap(
            swap_id,
            role="alice",
        )

        if (
            existing.nostr_alice_pub
            != alice_pubkey
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Alice "
                "swap has different Alice identity"
            )

        if (
            existing.nostr_bob_pub
            != bob_pubkey
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Alice "
                "swap has different Bob identity"
            )

        if (
            float(existing.blake_amount)
            != float(item["blake_amount"])
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Alice "
                "swap has different BLAKE terms"
            )

        if (
            int(existing.btc_sats)
            != int(item["btc_sats"])
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Alice "
                "swap has different BTC terms"
            )

        print()
        print("REVERSE MARKET MATCH ALREADY IMPORTED")
        print("=====================================")
        print("match id:", args.match_id)
        print("swap id: ", swap_id)
        print()
        print(
            "PASS: existing Alice swap reused"
        )
        print(
            "PASS: no second secret was generated"
        )
        print(
            "PASS: no funds moved"
        )
        return

    #
    # Alice independently derives fresh v1.5 deadlines
    # using her own view of both chains.
    #
    timeout_policy = TimeoutPolicy()

    btc_height = int(
        btc_cli(
            "getblockcount"
        )
    )

    blake_height = int(
        blake_cli(
            "getblockcount"
        )
    )

    timeout_plan = build_timeout_plan(
        btc_height=btc_height,
        blake_height=blake_height,
        policy=timeout_policy,
    )

    validate_timeout_plan(
        timeout_plan,
        timeout_policy,
    )

    #
    # THIS is the security boundary:
    #
    # make_swap() generates a fresh 32-byte S locally,
    # computes H = SHA256(S), and adopts the already
    # negotiated public swap_id.
    #
    swap = make_swap(
        float(item["blake_amount"]),
        int(item["btc_sats"]),
        timeout_plan.blake_timeout_height,
        timeout_plan.btc_timeout_height,
        swap_id=swap_id,
    )

    swap.timeout_policy_version = "1.5"

    swap.btc_timeout_window_seconds = (
        timeout_plan.btc_window_seconds
    )

    swap.blake_timeout_window_seconds = (
        timeout_plan.blake_window_seconds
    )

    swap.bob_recovery_seconds = (
        timeout_plan.bob_recovery_seconds
    )

    #
    # Bind protocol roles to the authenticated
    # market identities.
    #
    swap.nostr_alice_pub = (
        alice_pubkey
    )

    swap.nostr_bob_pub = (
        bob_pubkey
    )

    swap.validate()

    save_swap(
        swap,
        role="alice",
    )

    archive_superseded_placeholders(
        swap.swap_id,
        "alice",
    )

    #
    # Durable reverse-market binding.
    #
    binding_dir = market_match_binding_dir()

    binding_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    binding_path = (
        binding_dir
        / f"{swap_id}.json"
    )

    binding = {
        "version": 2,
        "direction":
            MARKET_DIRECTION_BTC_FOR_BLAKE,

        "offer_id": item["offer_id"],
        "take_id": item["take_id"],
        "match_id": item["event_id"],
        "swap_id": swap_id,

        "maker": item["maker"],
        "taker": item["taker"],

        "maker_role": "bob",
        "taker_role": "alice",

        "blake_amount": float(
            item["blake_amount"]
        ),

        "btc_sats": int(
            item["btc_sats"]
        ),
    }

    #
    # The binding reserves the swap ID for this exact match, so it must
    # survive a crash that loses the process but not the disk.
    #
    atomic_write_json(
        binding_path,
        binding,
        fsync_directory=True,
    )

    print()
    print("REVERSE MARKET MATCH IMPORTED")
    print("=============================")
    print("match id:", args.match_id)
    print("swap id: ", swap.swap_id)
    print()
    print("direction: btc-for-blake")

    labels = market_role_labels(
        MARKET_DIRECTION_BTC_FOR_BLAKE
    )

    print("maker is protocol:", labels["maker"])
    print("taker is protocol:", labels["taker"])
    print()
    print(
        "Alice Nostr:",
        swap.nostr_alice_pub,
    )
    print(
        "Bob Nostr:  ",
        swap.nostr_bob_pub,
    )
    print()
    print(
        "BTC timeout:  ",
        swap.btc_timeout,
    )
    print(
        "BLAKE timeout:",
        swap.blake_timeout,
    )
    print()
    print(
        "BTC window:",
        f"{swap.btc_timeout_window_seconds / 3600:.2f} h",
    )
    print(
        "Bob recovery:",
        f"{swap.bob_recovery_seconds / 3600:.2f} h",
    )
    print(
        "BLAKE window:",
        f"{swap.blake_timeout_window_seconds / 3600:.2f} h",
    )
    print()
    print(
        "PASS: reverse match independently validated"
    )
    print(
        "PASS: current participant derived as protocol Alice"
    )
    print(
        "PASS: reserved swap_id adopted"
    )
    print(
        "PASS: fresh secret S created only by Alice"
    )
    print(
        "PASS: timeout plan independently derived"
    )
    print(
        "PASS: no funds moved"
    )


def command_accept_reverse_swap_offer(args):
    """
    Import the ordinary atomic-swap offer after a
    btc-for-blake market match.

    In this direction:

        market maker -> protocol Bob
        market taker -> protocol Alice
    """
    import asyncio
    import json
    from types import SimpleNamespace

    if len(args.match_id) != 64:
        raise ValueError(
            "match_id must be the full "
            "64-character Nostr event ID"
        )

    bob_pubkey = current_nostr_pubkey()

    match_event = asyncio.run(
        nostr_fetch_event_by_id(
            args.match_id
        )
    )

    verify_nostr_event(
        match_event
    )

    try:
        match_message = json.loads(
            match_event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market match content is invalid JSON"
        ) from e

    if (
        match_message.get("type")
        != "market-match"
    ):
        raise RuntimeError(
            "Referenced event is not a market match"
        )

    #
    # For reverse direction the current participant
    # must be the MARKET MAKER, not the taker.
    #
    if (
        match_event["pubkey"]
        != bob_pubkey
    ):
        raise RuntimeError(
            "SECURITY FAILURE: current participant "
            "did not sign this market match"
        )

    if (
        match_message.get("maker")
        != bob_pubkey
    ):
        raise RuntimeError(
            "SECURITY FAILURE: market match "
            "maker identity mismatch"
        )

    expected_taker = match_message.get(
        "taker"
    )

    if not isinstance(
        expected_taker,
        str,
    ):
        raise RuntimeError(
            "Market match has no valid taker identity"
        )

    #
    # Now invoke the canonical validator using the
    # ACTUAL signed taker identity.
    #
    # That validator independently verifies:
    #   offer
    #   take
    #   match
    #   signatures
    #   direction
    #   roles
    #   economic terms
    #
    item = asyncio.run(
        validate_market_match_event(
            match_event,
            expected_taker,
        )
    )

    if (
        item.get("direction")
        != MARKET_DIRECTION_BTC_FOR_BLAKE
    ):
        raise RuntimeError(
            "Market match is not btc-for-blake"
        )

    if item.get("maker_role") != "bob":
        raise RuntimeError(
            "SECURITY FAILURE: maker did not "
            "derive to protocol Bob"
        )

    if item.get("taker_role") != "alice":
        raise RuntimeError(
            "SECURITY FAILURE: taker did not "
            "derive to protocol Alice"
        )

    if item["maker"] != bob_pubkey:
        raise RuntimeError(
            "SECURITY FAILURE: validated maker "
            "is not current Bob"
        )

    alice_pubkey = item["taker"]

    #
    # Idempotency:
    # if Bob already imported the resulting protocol
    # offer, do not generate new Bob settlement keys.
    #
    existing_path = swap_path(
        item["swap_id"],
        role="bob",
    )

    if existing_path.exists():
        existing = load_swap(
            item["swap_id"],
            role="bob",
        )

        if (
            existing.nostr_alice_pub
            != alice_pubkey
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "state has different Alice identity"
            )

        if existing.nostr_bob_pub not in (None, bob_pubkey):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "state has different Bob identity"
            )

        if existing.nostr_bob_pub is None:
            # Narrow crash-recovery case:
            # command_accept_offer() may have durably created Bob's
            # settlement keys before Nostr accept publication failed.
            # Resume only if the record is still completely unfunded
            # and otherwise matches the authenticated reverse match.
            if existing.state != State.OFFERED.value:
                raise RuntimeError(
                    "SECURITY FAILURE: partial reverse Bob state "
                    "is not OFFERED"
                )

            if existing.secret_hex is not None:
                raise RuntimeError(
                    "SECURITY FAILURE: protocol Bob unexpectedly "
                    "possesses secret S"
                )

            tx_fields = (
                "blake_funding_txid",
                "btc_funding_txid",
                "btc_claim_txid",
                "blake_claim_txid",
                "btc_refund_txid",
                "blake_refund_txid",
            )

            if any(
                getattr(existing, field) is not None
                for field in tx_fields
            ):
                raise RuntimeError(
                    "SECURITY FAILURE: refusing partial-handshake "
                    "recovery after settlement activity"
                )

            if not (
                existing.blake_bob_pub
                and existing.btc_bob_pub
            ):
                raise RuntimeError(
                    "SECURITY FAILURE: partial reverse Bob state "
                    "does not contain Bob settlement keys"
                )

            existing.nostr_bob_pub = bob_pubkey

            save_swap(
                existing,
                role="bob",
            )

            accept_message = {
                "type": "accept",
                "swap_id": existing.swap_id,
                "secret_hash": existing.secret_hash,
                "blake_bob_pub": existing.blake_bob_pub,
                "btc_bob_pub": existing.btc_bob_pub,
            }

            accept_path = send_message(
                existing.swap_id,
                "accept",
                accept_message,
                expected_role="bob",
            )

            print()
            print("REVERSE BOB HANDSHAKE RECOVERED")
            print("===============================")
            print("swap id:       ", existing.swap_id)
            print("accept message:", accept_path)
            print()
            print("PASS: existing Bob settlement keys reused")
            print("PASS: authenticated Bob identity pinned")
            print("PASS: Bob still does NOT know secret S")
            print("PASS: no funds moved")
            return

        if (
            float(existing.blake_amount)
            != float(item["blake_amount"])
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "state has different BLAKE terms"
            )

        if (
            int(existing.btc_sats)
            != int(item["btc_sats"])
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "state has different BTC terms"
            )

        print()
        print("REVERSE SWAP OFFER ALREADY IMPORTED")
        print("===================================")
        print("match id:", args.match_id)
        print("swap id: ", item["swap_id"])
        print()
        print(
            "PASS: existing Bob state reused"
        )
        print(
            "PASS: no new Bob settlement keys created"
        )
        print(
            "PASS: Bob still does NOT know secret S"
        )
        return

    #
    # Import Start9's ordinary protocol offer, but
    # constrain it to the identities and economics
    # already authenticated by the market match.
    #
    command_accept_offer(
        SimpleNamespace(
            swap_id=item["swap_id"],
            expected_alice_pub=alice_pubkey,
            expected_bob_pub=bob_pubkey,
            expected_blake_amount=item[
                "blake_amount"
            ],
            expected_btc_sats=item[
                "btc_sats"
            ],
        )
    )

    #
    # command_accept_offer() created Bob state and
    # generated Bob-owned settlement keys.
    #
    swap = load_swap(
        item["swap_id"],
        role="bob",
    )

    if swap.secret_hex is not None:
        raise RuntimeError(
            "SECURITY FAILURE: Bob learned "
            "Alice's original secret"
        )

    if (
        swap.nostr_alice_pub
        != alice_pubkey
    ):
        raise RuntimeError(
            "SECURITY FAILURE: protocol offer "
            "Alice identity mismatch"
        )

    #
    # Bob's Nostr identity was already proven by the
    # market match. Persist that binding locally.
    #
    swap.nostr_bob_pub = bob_pubkey

    save_swap(
        swap,
        role="bob",
    )

    print()
    print("REVERSE SWAP OFFER IMPORTED")
    print("===========================")
    print("match id:", args.match_id)
    print("swap id: ", swap.swap_id)
    print()
    print("Alice Nostr:", swap.nostr_alice_pub)
    print("Bob Nostr:  ", swap.nostr_bob_pub)
    print()
    print(
        "PASS: market maker derived as protocol Bob"
    )
    print(
        "PASS: market taker derived as protocol Alice"
    )
    print(
        "PASS: protocol offer signed by matched Alice"
    )
    print(
        "PASS: economic terms match signed market match"
    )
    print(
        "PASS: v1.5 timeout safety independently validated"
    )
    print(
        "PASS: Bob does NOT know secret S"
    )



def command_accept_only_match(args):
    import asyncio
    from types import SimpleNamespace

    me = current_nostr_pubkey()
    matches = asyncio.run(
        nostr_fetch_market_matches(
            me,
            limit=200,
        )
    )

    candidates = []
    for item in matches:
        swap_id = item.get("swap_id")
        if (
            isinstance(swap_id, str)
            and len(swap_id) == 12
            and (
                swap_path(swap_id).exists()
                or swap_id_is_locally_retired(swap_id)
            )
        ):
            continue
        candidates.append(item)

    requested = getattr(args, "match_id", None)

    if requested:
        requested = requested.strip().lower()
        if len(requested) < 8:
            raise ValueError(
                "match ID prefix is too short; use at least 8 characters"
            )

        filtered = [
            item for item in candidates
            if item["event_id"].startswith(requested)
        ]

        if not filtered:
            #
            # The match may simply not be pending any more because this
            # participant already imported it. A crash retry runs the same
            # command again, and "no pending match" would be a false and
            # alarming report about a swap that exists and is fine.
            #
            bound = locally_bound_market_match(requested)

            if bound is not None:
                bound_swap_id, bound_match_id = bound

                print()
                print("MARKET MATCH ALREADY IMPORTED")
                print("=============================")
                print("match id:", bound_match_id)
                print("swap id: ", bound_swap_id)
                print()
                print(
                    "PASS: duplicate command did not create a new swap"
                )
                print(
                    "PASS: durable market binding is intact"
                )
                print("PASS: no funds moved")
                return

            raise RuntimeError(
                f"No pending valid match begins with {requested!r}"
            )

        if len(filtered) > 1:
            print()
            print("AMBIGUOUS MATCH PREFIX")
            print("======================")
            for item in filtered:
                print(
                    item["event_id"],
                    "swap=" + str(item.get("swap_id")),
                )
            raise RuntimeError(
                "Match prefix is ambiguous; use more characters"
            )

        candidates = filtered

    if not candidates:
        print()
        print("NO PENDING MATCH")
        print("================")
        print(
            "No unimported valid market match for "
            "this participant was found."
        )
        return

    if len(candidates) > 1:
        print()
        print("MULTIPLE PENDING MATCHES")
        print("========================")
        print(
            "More than one valid unimported match "
            "belongs to this participant."
        )
        print("Refusing to choose automatically.")
        print()

        for index, item in enumerate(candidates, start=1):
            print(
                f"{index}. "
                f"match={item['event_id'][:12]}  "
                f"swap={item.get('swap_id', '-')}"
            )

        print()
        print(
            "Use `accept-match <match-prefix>` "
            "for the one you want."
        )
        return

    item = candidates[0]

    print()
    print("ONE PENDING MATCH FOUND")
    print("=======================")
    print("match:", item["event_id"])
    print("swap: ", item["swap_id"])
    print(
        "trade:",
        item["blake_amount"],
        "BLAKE /",
        f"{int(item['btc_sats']):,}",
        "sats",
    )
    print()

    direction = item.get("direction")

    if direction == MARKET_DIRECTION_BLAKE_FOR_BTC:
        print("Importing match as protocol Bob.")
        command_accept_market_match(
            SimpleNamespace(
                match_id=item["event_id"]
            )
        )
        return

    if direction == MARKET_DIRECTION_BTC_FOR_BLAKE:
        print("Importing reverse-direction match as protocol Alice.")
        command_accept_reverse_market_match(
            SimpleNamespace(
                match_id=item["event_id"]
            )
        )

        # Explicit accept-match must finish the same safe coordination
        # step as `blake next`: publish protocol Alice's ordinary offer
        # if it is not already available. This moves no funds.
        offer_exists = False

        try:
            import contextlib
            import io

            with contextlib.redirect_stdout(io.StringIO()):
                asyncio.run(
                    nostr_fetch_message(
                        item["swap_id"],
                        "offer",
                        expected_pubkey=me,
                    )
                )
            offer_exists = True
        except Exception:
            pass

        if not offer_exists:
            print()
            print("Publishing reverse protocol offer...")
            command_publish_offer(
                SimpleNamespace(
                    swap_id=item["swap_id"]
                )
            )

        return

    raise RuntimeError(
        f"Unsupported market direction: {direction!r}"
    )



def market_match_binding_dir():
    return BASE_SWAP_DIR / "market-match-bindings"


def market_match_ids_in_binding(binding):
    """
    The match IDs a durable market binding records.

    The two directions store different keys: the reverse binding (written
    by the maker's side) carries `match_id`, and the forward binding
    carries `first_match_id` as informational evidence. Both names answer
    the same question -- which match produced this swap -- so both are
    read here rather than duplicating the lookup per direction.
    """
    return [
        value
        for value in (
            binding.get("match_id"),
            binding.get("first_match_id"),
        )
        if isinstance(value, str)
    ]


def locally_bound_market_match(prefix):
    """
    Find a match that has already been imported, by its ID prefix.

    After an import the match is no longer *pending*, so relay discovery
    cannot be asked about it again. A repeat of a command that was
    interrupted after the import therefore has to be answered from the
    durable binding the import wrote, which is what this reads.

    Returns `(swap_id, match_id)`, or None when no local binding matches.
    """
    directory = market_match_binding_dir()

    if not directory.exists():
        return None

    for path in sorted(directory.glob("*.json")):
        if not is_swap_id(path.stem):
            continue

        try:
            with open(path) as f:
                binding = json.load(f)
        except Exception:
            continue

        if not isinstance(binding, dict):
            continue

        for match_id in market_match_ids_in_binding(binding):
            if match_id.startswith(prefix):
                return path.stem, match_id

    return None


def command_accept_market_match(args):
    args.match_id = resolve_cached_nostr_prefix(
        args.match_id,
        expected_type="market-match",
        label="market match",
    )
    import asyncio
    import json
    import os
    from types import SimpleNamespace

    if len(args.match_id) != 64:
        raise ValueError(
            "match_id must be the full "
            "64-character Nostr event ID"
        )

    bob_pubkey = current_nostr_pubkey()

    event = asyncio.run(
        nostr_fetch_event_by_id(
            args.match_id
        )
    )

    item = asyncio.run(
        validate_market_match_event(
            event,
            bob_pubkey,
        )
    )

    #
    # Bob durably binds the atomic swap ID to the
    # market negotiation that created it.
    #
    # A later signed match may be new evidence for
    # the SAME tuple, but it may not redefine:
    #
    #   offer
    #   selected take
    #   swap
    #   maker
    #   taker
    #   economic terms
    #
    binding_dir = market_match_binding_dir()

    binding_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    binding_path = (
        binding_dir
        / f"{item['swap_id']}.json"
    )

    proposed_binding = {
        "version": 1,
        "offer_id": item["offer_id"],
        "take_id": item["take_id"],
        "swap_id": item["swap_id"],
        "maker": item["maker"],
        "taker": item["taker"],
        "blake_amount": float(
            item["blake_amount"]
        ),
        "btc_sats": int(
            item["btc_sats"]
        ),
        #
        # Informational evidence only.
        #
        # This is intentionally NOT part of the
        # immutable semantic tuple.
        #
        "first_match_id": item["event_id"],
    }

    existing_binding = None

    if binding_path.exists():
        try:
            with open(binding_path) as f:
                existing_binding = json.load(f)
        except Exception as e:
            raise RuntimeError(
                "Existing market match binding "
                "is unreadable"
            ) from e

        required = (
            "offer_id",
            "take_id",
            "swap_id",
            "maker",
            "taker",
            "blake_amount",
            "btc_sats",
        )

        if any(
            key not in existing_binding
            for key in required
        ):
            raise RuntimeError(
                "Existing market match binding "
                "is malformed"
            )

        conflict = (
            existing_binding["offer_id"]
            != proposed_binding["offer_id"]

            or existing_binding["take_id"]
            != proposed_binding["take_id"]

            or existing_binding["swap_id"]
            != proposed_binding["swap_id"]

            or existing_binding["maker"]
            != proposed_binding["maker"]

            or existing_binding["taker"]
            != proposed_binding["taker"]

            or float(
                existing_binding["blake_amount"]
            )
            != float(
                proposed_binding["blake_amount"]
            )

            or int(
                existing_binding["btc_sats"]
            )
            != int(
                proposed_binding["btc_sats"]
            )
        )

        if conflict:
            raise RuntimeError(
                "SECURITY FAILURE: existing "
                "market match binding disagrees "
                "with validated match"
            )

    def write_binding():
        atomic_write_json(
            binding_path,
            proposed_binding,
            fsync_directory=True,
        )

    existing_path = swap_path(
        item["swap_id"],
        role="bob",
    )

    if existing_path.exists():
        existing = load_swap(
            item["swap_id"],
            role="bob",
        )

        if (
            existing.nostr_alice_pub
            != item["maker"]
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "swap has different Alice identity"
            )

        if existing.nostr_bob_pub is None:
            existing.nostr_bob_pub = bob_pubkey

            save_swap(
                existing,
                role="bob",
            )

        elif (
            existing.nostr_bob_pub
            != bob_pubkey
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "swap has different Bob identity"
            )

        if (
            float(existing.blake_amount)
            != float(item["blake_amount"])
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "swap has different BLAKE terms"
            )

        if (
            int(existing.btc_sats)
            != int(item["btc_sats"])
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing Bob "
                "swap has different BTC terms"
            )

        #
        # Legacy/self-healing case:
        #
        # The Bob swap may already exist while the
        # durable market binding is missing because
        # this code is newer or a previous execution
        # stopped between writes.
        #
        if existing_binding is None:
            write_binding()

        print()
        print("MARKET MATCH ALREADY IMPORTED")
        print("=============================")
        print(
            "match id:",
            args.match_id,
        )
        print(
            "swap id: ",
            item["swap_id"],
        )
        print()
        print(
            "PASS: duplicate command did not "
            "create new Bob keys"
        )
        print(
            "PASS: immutable market negotiation "
            "binding preserved"
        )
        print(
            "PASS: missing binding, if any, "
            "was repaired"
        )
        return

    #
    # No Bob swap exists yet.
    #
    # If a durable binding already exists, it has
    # already been checked above and exactly matches
    # this validated market evidence.
    #
    command_accept_offer(
        SimpleNamespace(
            swap_id=item["swap_id"],
            expected_alice_pub=item[
                "maker"
            ],
            expected_blake_amount=item[
                "blake_amount"
            ],
            expected_btc_sats=item[
                "btc_sats"
            ],
            expected_bob_pub=bob_pubkey,
        )
    )

    # Record Bob's already-authenticated identity
    # in his local swap state too.
    swap = load_swap(
        item["swap_id"],
        role="bob",
    )

    swap.nostr_bob_pub = (
        bob_pubkey
    )

    save_swap(
        swap,
        role="bob",
    )

    #
    # Persist the immutable market->swap binding
    # only after Bob's swap state exists.
    #
    if existing_binding is None:
        write_binding()

    print()
    print("MARKET MATCH IMPORTED")
    print("=====================")
    print(
        "match id:",
        args.match_id,
    )
    print(
        "swap id: ",
        item["swap_id"],
    )
    print()
    print(
        "PASS: swap offer signer == "
        "matched Alice"
    )
    print(
        "PASS: Bob identity == signed take"
    )
    print(
        "PASS: market negotiation tuple "
        "durably bound to swap"
    )
    print(
        "PASS: Bob still does NOT know secret S"
    )
    print(
        "PASS: no funds moved"
    )


def accept_reverse_market_take(
    args,
    original,
    offer_message,
    selected,
    take_event,
):
    """
    Accept a v2 btc-for-blake market take.

    The offer maker is protocol Bob.
    The taker is protocol Alice.

    Bob may select the take and reserve the public swap_id,
    but MUST NOT create Alice's secret S.
    """
    import asyncio
    import json
    from types import SimpleNamespace

    direction = selected["direction"]

    if (
        direction
        != MARKET_DIRECTION_BTC_FOR_BLAKE
    ):
        raise RuntimeError(
            "Reverse-market helper called for "
            "wrong direction"
        )

    roles = market_protocol_roles(
        direction
    )

    if (
        roles["maker_role"] != "bob"
        or roles["taker_role"] != "alice"
    ):
        raise RuntimeError(
            "SECURITY FAILURE: reverse direction "
            "did not derive Bob/Alice roles"
        )

    maker_pub = original["pubkey"]
    taker_pub = take_event["pubkey"]

    if current_nostr_pubkey() != maker_pub:
        raise RuntimeError(
            "SECURITY FAILURE: current participant "
            "did not create this market offer"
        )

    if selected["taker"] != taker_pub:
        raise RuntimeError(
            "SECURITY FAILURE: selected taker "
            "identity mismatch"
        )

    selection_dir = (
        BASE_SWAP_DIR
        / "market-selections"
    )

    selection_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    selection_path = (
        selection_dir
        / f"{args.take_id}.json"
    )

    offer_selection_path = (
        selection_dir
        / f"offer-{original['id']}.json"
    )

    #
    # If already selected, reuse the exact same binding.
    #
    existing = None

    for path in (
        selection_path,
        offer_selection_path,
    ):
        if not path.exists():
            continue

        with open(path) as f:
            candidate = json.load(f)

        if existing is None:
            existing = candidate

        elif candidate != existing:
            raise RuntimeError(
                "SECURITY FAILURE: reverse market "
                "selection records disagree"
            )

    if existing is not None:
        if (
            existing.get("offer_id")
            != original["id"]
            or existing.get("take_id")
            != args.take_id
            or existing.get("direction")
            != direction
            or existing.get("maker_role")
            != "bob"
            or existing.get("taker_role")
            != "alice"
            or existing.get("nostr_bob_pub")
            != maker_pub
            or existing.get("nostr_alice_pub")
            != taker_pub
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing reverse "
                "market selection is inconsistent"
            )

        swap_id = existing.get(
            "swap_id"
        )

        if not swap_id:
            raise RuntimeError(
                "Existing reverse selection has "
                "no swap_id"
            )

        print()
        print("REVERSE MARKET TAKE ALREADY ACCEPTED")
        print("====================================")
        print("offer id:", original["id"])
        print("take id: ", args.take_id)
        print("swap id: ", swap_id)
        print()
        print(
            "PASS: no second swap ID was created"
        )
        print(
            "PASS: no secret was created by Bob"
        )
        return existing

    #
    # Public coordination ID only.
    # No make_swap() here.
    #
    swap_id = make_swap_id()

    selection = {
        "version": 2,
        "offer_id": original["id"],
        "take_id": args.take_id,
        "swap_id": swap_id,

        "direction": direction,
        "maker_role": "bob",
        "taker_role": "alice",

        "nostr_bob_pub": maker_pub,
        "nostr_alice_pub": taker_pub,

        "blake_amount": float(
            offer_message["blake_amount"]
        ),
        "btc_sats": int(
            offer_message["btc_sats"]
        ),
    }

    for path in (
        selection_path,
        offer_selection_path,
    ):
        #
        # Both copies of the selection are written before either is
        # announced, and both must survive a crash: losing one would make
        # the two agree on different terms.
        #
        atomic_write_json(
            path,
            selection,
            fsync_directory=True,
        )

    #
    # Build the signed v2 match using only public
    # negotiated terms.  Still no secret/hashlock.
    #
    public_swap = SimpleNamespace(
        swap_id=swap_id,
        #
        # Canonical base units, derived from the durable selection. The
        # market match carries public terms only; it still carries an
        # exact amount.
        #
        blake_amount_base_units=blake_base_units(
            selection["blake_amount"]
        ),
        btc_sats=selection[
            "btc_sats"
        ],
    )

    match_event, match_message = (
        build_market_match_event(
            original,
            take_event,
            public_swap,
        )
    )

    verify_nostr_event(
        match_event
    )

    match_id = asyncio.run(
        nostr_publish_event(
            match_event
        )
    )

    selection[
        "market_match_id"
    ] = match_id

    for path in (
        selection_path,
        offer_selection_path,
    ):
        #
        # Both copies of the selection are written before either is
        # announced, and both must survive a crash: losing one would make
        # the two agree on different terms.
        #
        atomic_write_json(
            path,
            selection,
            fsync_directory=True,
        )

    print()
    print("REVERSE MARKET TAKE ACCEPTED")
    print("============================")
    print("offer id:", original["id"])
    print("take id: ", args.take_id)
    print("match id:", match_id)
    print("swap id: ", swap_id)
    print()
    print("direction:", direction)

    labels = market_role_labels(direction)

    print("maker is protocol:", labels["maker"])
    print("taker is protocol:", labels["taker"])
    print()
    print(
        "PASS: maker selected exact signed take"
    )
    print(
        "PASS: public swap_id reserved"
    )
    print(
        "PASS: Bob did NOT create secret S"
    )
    print(
        "PASS: no atomic-swap state exists yet"
    )
    print(
        "PASS: no funds moved"
    )

    return selection



def command_accept_only_take(args):
    import asyncio
    from types import SimpleNamespace

    #
    # accept-take is a state-changing coordination action, not a
    # discovery command: it has no JSON output mode, so prefix
    # resolution always announces. (market-takes is the machine-readable
    # discovery command and suppresses the announcement for --json.)
    #
    offer_id = resolve_cached_nostr_prefix(
        args.offer_id,
        expected_type="market-offer",
        label="market offer",
    )

    original, offer_message, valid_takes = asyncio.run(
        nostr_fetch_market_takes(
            offer_id,
            limit=200,
        )
    )

    if not valid_takes:
        raise RuntimeError(
            "No valid takes currently exist for this offer"
        )

    if len(valid_takes) > 1:
        print()
        print("MULTIPLE VALID TAKES")
        print("====================")
        print("offer:", offer_id)
        print()
        print(
            "More than one counterparty wants this offer. "
            "Refusing to choose automatically."
        )
        print()

        for index, candidate in enumerate(valid_takes, start=1):
            print(
                f"{index}. "
                f"{candidate['event_id'][:12]}  "
                f"taker={candidate['taker'][:12]}"
            )

        print()
        print(
            "Use accept-market-take with the chosen "
            "take ID/prefix."
        )
        return

    take_id = valid_takes[0]["event_id"]

    print()
    print("ONE VALID TAKE FOUND")
    print("====================")
    print("offer:", offer_id)
    print("take: ", take_id)
    print()
    print("Accepting the only valid take.")

    command_accept_market_take(
        SimpleNamespace(
            take_id=take_id
        )
    )


def command_accept_market_take(args):
    import asyncio
    import json
    from pathlib import Path
    from types import SimpleNamespace
    from coincurve import PrivateKey

    args.take_id = resolve_cached_nostr_prefix(
        args.take_id,
        expected_type="market-take",
        label="market take",
    )

    #
    # Step 1:
    # Fetch the exact take event so we can learn
    # which market offer it claims to reference.
    #
    take_event = asyncio.run(
        nostr_fetch_event_by_id(
            args.take_id
        )
    )

    verify_nostr_event(take_event)

    try:
        take_message = json.loads(
            take_event["content"]
        )
    except Exception as e:
        raise RuntimeError(
            "Market take content is invalid JSON"
        ) from e

    if (
        take_message.get("type")
        != "market-take"
    ):
        raise RuntimeError(
            "Referenced event is not a market take"
        )

    offer_id = take_message.get(
        "offer_id"
    )

    if (
        not isinstance(offer_id, str)
        or len(offer_id) != 64
    ):
        raise RuntimeError(
            "Market take has invalid offer_id"
        )

    #
    # Before applying current market actionability,
    # determine whether durable local state says this
    # exact negotiation was already accepted.
    #
    # Expiry may prevent a NEW acceptance, but must
    # not erase an already-established negotiation.
    #
    selection_dir = (
        BASE_SWAP_DIR
        / "market-selections"
    )

    prior_take_selection_path = (
        selection_dir
        / f"{args.take_id}.json"
    )

    prior_offer_selection_path = (
        selection_dir
        / f"offer-{offer_id}.json"
    )

    already_selected = (
        prior_take_selection_path.exists()
        or prior_offer_selection_path.exists()
    )

    #
    # Step 2:
    # Run the canonical market-take validator.
    #
    # Historical expiry is tolerated ONLY when
    # durable selection evidence already exists.
    # All signature, schema, identity, term, and
    # later durable-state checks still run.
    #
    # `allow_matched` is passed for the same reason as `allow_expired`:
    # accepting this take is what matches the offer, so a retry after a
    # crash sees a MATCHED offer and would otherwise be refused by the
    # state gate above before the idempotent path below could report
    # "MARKET TAKE ALREADY ACCEPTED". Without it, a retry failed with
    # "Unknown market offer state: MATCHED" instead of being a no-op.
    #
    original, offer_message, valid_takes = (
        asyncio.run(
            nostr_fetch_market_takes(
                offer_id,
                limit=200,
                extra_events=[take_event],
                allow_expired=already_selected,
                allow_matched=already_selected,
            )
        )
    )

    selected = None

    for candidate in valid_takes:
        if (
            candidate["event_id"]
            == args.take_id
        ):
            selected = candidate
            break

    if selected is None:
        raise RuntimeError(
            "SECURITY FAILURE: selected take "
            "did not pass market-take validation"
        )

    #
    # v2 reverse direction:
    #
    # The offer maker is protocol Bob and must NOT
    # create Alice's secret/hashlock.  Stop before
    # entering the historical Alice-maker path.
    #
    if (
        selected.get("direction")
        == MARKET_DIRECTION_BTC_FOR_BLAKE
    ):
        return accept_reverse_market_take(
            args,
            original,
            offer_message,
            selected,
            take_event,
        )

    #
    # Step 3:
    # Prove this offer actually belongs to the
    # Alice identity currently running the command.
    #
    private_key = PrivateKey(
        bytes.fromhex(
            nostr_private_key_hex()
        )
    )

    compressed = (
        private_key.public_key.format(
            compressed=True
        )
    )

    alice_nostr_pub = compressed[1:].hex()

    if (
        original["pubkey"]
        != alice_nostr_pub
    ):
        raise RuntimeError(
            "SECURITY FAILURE: "
            "current Alice identity did not "
            "create the referenced market offer"
        )

    bob_nostr_pub = take_event["pubkey"]

    if (
        selected["taker"]
        != bob_nostr_pub
    ):
        raise RuntimeError(
            "SECURITY FAILURE: "
            "validated taker identity mismatch"
        )

    #
    # Step 4:
    # Prevent an accidental second swap from being
    # created if Alice runs this command twice.
    #
    selection_dir = (
        BASE_SWAP_DIR
        / "market-selections"
    )

    selection_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    #
    # One market offer may receive many valid takes,
    # but only ONE take may become the accepted
    # negotiation.
    #
    offer_selection_path = (
        selection_dir
        / f"offer-{offer_id}.json"
    )

    existing_offer_selection = None

    if offer_selection_path.exists():
        with open(offer_selection_path) as f:
            existing_offer_selection = json.load(f)

        existing_take_id = (
            existing_offer_selection.get(
                "take_id"
            )
        )

        existing_swap_id = (
            existing_offer_selection.get(
                "swap_id"
            )
        )

        if (
            existing_offer_selection.get("offer_id")
            != offer_id
            or not existing_take_id
            or not existing_swap_id
        ):
            raise RuntimeError(
                "Existing market offer selection "
                "record is malformed"
            )

        if existing_take_id != args.take_id:
            raise RuntimeError(
                "SECURITY FAILURE: market offer "
                "has already been accepted through "
                "a different take"
            )

    selection_path = (
        selection_dir
        / f"{args.take_id}.json"
    )

    existing_take_selection = None

    if selection_path.exists():
        with open(selection_path) as f:
            existing_take_selection = json.load(f)

        if (
            existing_take_selection.get("offer_id")
            != offer_id
            or existing_take_selection.get("take_id")
            != args.take_id
            or not existing_take_selection.get("swap_id")
        ):
            raise RuntimeError(
                "Existing market selection record "
                "is malformed"
            )

    #
    # If either durable record already exists,
    # this selection has already happened.
    #
    # Both records must describe the same accepted
    # take and swap.  If one side is missing because
    # of an interrupted write, reconstruct it.
    #
    if (
        existing_offer_selection is not None
        or existing_take_selection is not None
    ):
        canonical_selection = (
            existing_take_selection
            if existing_take_selection is not None
            else existing_offer_selection
        )

        existing_swap_id = (
            canonical_selection["swap_id"]
        )

        if (
            existing_offer_selection is not None
            and existing_take_selection is not None
            and existing_offer_selection["swap_id"]
            != existing_take_selection["swap_id"]
        ):
            raise RuntimeError(
                "SECURITY FAILURE: market selection "
                "records disagree about swap_id"
            )

        if (
            canonical_selection.get("nostr_alice_pub")
            != alice_nostr_pub
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing market "
                "selection Alice identity mismatch"
            )

        if (
            canonical_selection.get("nostr_bob_pub")
            != bob_nostr_pub
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing market "
                "selection Bob identity mismatch"
            )

        existing_swap = load_swap(
            existing_swap_id,
            role="alice",
        )

        if (
            existing_swap.nostr_alice_pub
            != alice_nostr_pub
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing swap "
                "Alice identity mismatch"
            )

        if (
            existing_swap.nostr_bob_pub
            != bob_nostr_pub
        ):
            raise RuntimeError(
                "SECURITY FAILURE: existing swap "
                "Bob identity mismatch"
            )

        #
        # Repair either half of the durable binding
        # if an earlier run stopped between writes.
        #
        if existing_take_selection is None:
            atomic_write_json(
                selection_path,
                canonical_selection,
                fsync_directory=True,
            )

        if existing_offer_selection is None:
            atomic_write_json(
                offer_selection_path,
                canonical_selection,
                fsync_directory=True,
            )

        print()
        print("MARKET TAKE ALREADY ACCEPTED")
        print("============================")
        print("offer id:", offer_id)
        print("take id: ", args.take_id)
        print("swap id: ", existing_swap_id)
        print()
        print(
            "PASS: duplicate command did not "
            "create another swap"
        )
        print(
            "PASS: offer/take selection records "
            "are mutually consistent"
        )
        print(
            "PASS: missing selection record, if "
            "any, was repaired"
        )
        return

    #
    # Step 5:
    # Create fresh atomic-swap parameters using the
    # same v1.5 wall-clock-aware timeout policy used
    # by normal production swap construction.
    #
    timeout_policy = TimeoutPolicy()

    btc_height = int(
        btc_cli("getblockcount")
    )

    blake_height = int(
        blake_cli("getblockcount")
    )

    timeout_plan = build_timeout_plan(
        btc_height=btc_height,
        blake_height=blake_height,
        policy=timeout_policy,
    )

    validate_timeout_plan(
        timeout_plan,
        timeout_policy,
    )

    swap = make_swap(
        float(
            offer_message[
                "blake_amount"
            ]
        ),
        int(
            offer_message[
                "btc_sats"
            ]
        ),
        timeout_plan.blake_timeout_height,
        timeout_plan.btc_timeout_height,
    )

    swap.timeout_policy_version = "1.5"

    swap.btc_timeout_window_seconds = (
        timeout_plan.btc_window_seconds
    )

    swap.blake_timeout_window_seconds = (
        timeout_plan.blake_window_seconds
    )

    swap.bob_recovery_seconds = (
        timeout_plan.bob_recovery_seconds
    )

    #
    # Exercise the complete persisted-swap invariants,
    # not only the standalone timeout-plan validator.
    #
    swap.validate()

    #
    # The marketplace handshake now supplies
    # both authenticated Nostr identities.
    #
    swap.nostr_alice_pub = (
        alice_nostr_pub
    )

    swap.nostr_bob_pub = (
        bob_nostr_pub
    )

    save_swap(
        swap,
        role="alice",
    )

    #
    # Accepting a take creates the swap for this participation, so any
    # earlier unannounced placeholder is now superseded.
    #
    archive_superseded_placeholders(
        swap.swap_id,
        "alice",
    )

    #
    # Record take -> swap before publishing.
    # If publication later fails, we still will
    # not generate a second secret on retry.
    #
    selection_record = {
        "version": 1,
        "offer_id": offer_id,
        "take_id": args.take_id,
        "swap_id": swap.swap_id,
        "nostr_alice_pub":
            alice_nostr_pub,
        "nostr_bob_pub":
            bob_nostr_pub,
    }

    #
    # Durable before anything is announced: these two files are what stop
    # a second take creating a second swap from the same offer, so losing
    # either one is losing that guarantee.
    #
    atomic_write_json(
        selection_path,
        selection_record,
        fsync_directory=True,
    )

    #
    # Also bind the original market offer to its
    # one accepted take.  Other valid takes remain
    # authentic evidence, but cannot create another
    # swap from this offer.
    #
    atomic_write_json(
        offer_selection_path,
        selection_record,
        fsync_directory=True,
    )

    print()
    print("MARKET TAKE ACCEPTED")
    print("====================")
    print("offer id:", offer_id)
    print("take id: ", args.take_id)
    print("swap id: ", swap.swap_id)
    print()
    print(
        "Alice Nostr:",
        swap.nostr_alice_pub,
    )
    print(
        "Bob Nostr:  ",
        swap.nostr_bob_pub,
    )
    print()
    print(
        "BLAKE:",
        swap.blake_amount,
    )
    print(
        "BTC sats:",
        swap.btc_sats,
    )
    print()
    print(
        "BTC timeout:  ",
        swap.btc_timeout,
    )
    print(
        "BLAKE timeout:",
        swap.blake_timeout,
    )
    print()
    print(
        "PASS: fresh secret/hashlock created"
    )
    print(
        "PASS: Bob pinned from exact signed take"
    )
    print(
        "PASS: Alice pinned from exact signed offer"
    )
    print(
        "PASS: no funds have moved"
    )

    #
    # Step 6:
    # Reuse the existing, already-proven
    # swap-level offer publisher.
    #
    print()
    print(
        "Publishing normal authenticated "
        "atomic-swap offer..."
    )

    command_publish_offer(
        SimpleNamespace(
            swap_id=swap.swap_id
        )
    )

    print()
    print(
        "Publishing signed market match..."
    )

    match_id = command_publish_market_match(
        SimpleNamespace(
            take_id=args.take_id
        )
    )

    print()
    print("MARKET -> SWAP BRIDGE CREATED")
    print("=============================")
    print("market offer:", offer_id)
    print("market take: ", args.take_id)
    print("market match:", match_id)
    print("atomic swap: ", swap.swap_id)
    print()
    print(
        "PASS: settlement engine was not modified"
    )
    print(
        "PASS: HTLC funding remains a separate "
        "explicit action"
    )



def command_inspect_market_take(args):
    import asyncio
    import json
    import time

    if len(args.offer_id) != 64:
        raise ValueError(
            "offer_id must be the full 64-character Nostr event ID"
        )

    open_offers = asyncio.run(
        nostr_fetch_market_offers(limit=200)
    )

    if not any(
        x["event_id"] == args.offer_id
        for x in open_offers
    ):
        raise RuntimeError(
            "Offer is not currently open"
        )

    original = asyncio.run(
        nostr_fetch_event_by_id(
            args.offer_id
        )
    )

    verify_nostr_event(original)

    offer_message = json.loads(
        original["content"]
    )

    if int(offer_message["expires_at"]) <= int(time.time()):
        raise RuntimeError("Offer is expired")

    event, message = build_market_take_event(
        original,
        offer_message,
    )

    # Prove our own event is internally valid.
    verify_nostr_event(event)

    print()
    print("MARKET TAKE EVENT VALID")
    print("=======================")
    print("event id:", event["id"])
    print("signer:  ", event["pubkey"])
    print("kind:    ", event["kind"])
    print()
    print("tags:")
    print(json.dumps(event["tags"], indent=2))
    print()
    print("content:")
    print(json.dumps(message, indent=2, sort_keys=True))
    print()
    print("PASS: event ID recomputes correctly")
    print("PASS: Bob's Schnorr signature verifies")
    print("PASS: event was NOT published")

def command_take_market_offer(args):
    import asyncio
    import time

    args.offer_id = resolve_cached_nostr_prefix(
        args.offer_id,
        expected_type="market-offer",
        label="market offer",
    )

    # First ask our normal market scanner what is
    # actually open right now. This automatically
    # applies signature, expiry, and cancellation rules.
    open_offers = asyncio.run(
        nostr_fetch_market_offers(
            limit=200
        )
    )

    selected = None

    for offer in open_offers:
        if offer["event_id"] == args.offer_id:
            selected = offer
            break

    if selected is None:
        raise RuntimeError(
            "Offer is not currently open. "
            "It may be expired, cancelled, invalid, "
            "or unavailable from this relay."
        )

    validate_btc_trade_amount(
        selected["btc_sats"],
        context="selected market offer",
    )

    original = asyncio.run(
        nostr_fetch_event_by_id(
            args.offer_id
        )
    )

    verify_nostr_event(original)

    offer_message = json.loads(
        original["content"]
    )

    if (
        offer_message.get("type")
        != "market-offer"
    ):
        raise RuntimeError(
            "Referenced event is not a market offer"
        )

    if (
        int(offer_message["expires_at"])
        <= int(time.time())
    ):
        raise RuntimeError(
            "Offer expired before it could be taken"
        )

        # Do not publish another take for an offer this
    # participant has already taken.
    prior_take = nostr_prior_market_take(
        args.offer_id
    )


    if prior_take is not None:
        print()
        print("MARKET OFFER ALREADY TAKEN")
        print("==========================")
        print("offer id:", args.offer_id)
        print("take id: ", prior_take["id"])
        print("taker:   ", prior_take["pubkey"])
        print()
        print(
            "PASS: existing signed take reused"
        )
        print(
            "PASS: no duplicate Nostr event created"
        )
        print(
            "PASS: no money has moved"
        )
        print()
        print(
            "Retrying publication of the exact "
            "same signed take..."
        )

        try:
            asyncio.run(
                nostr_publish_event(
                    prior_take
                )
            )
        except RuntimeError as e:
            print()
            print(
                "WARNING: relay publication was "
                "not confirmed"
            )
            print(str(e))
            print()
            print(
                "PASS: original signed take remains "
                "durably cached"
            )
            print(
                "PASS: retry did not create a new "
                "take event"
            )

        return

    take_event, take_message = (
        build_market_take_event(
            original,
            offer_message,
        )
    )

    take_id = asyncio.run(
        nostr_publish_event(
            take_event
        )
    )

    print()
    print("MARKET OFFER TAKEN")
    print("==================")
    print("offer id:", args.offer_id)
    print("take id: ", take_id)
    print("maker:   ", original["pubkey"])
    print("taker:   ", take_event["pubkey"])
    print(
        "selling: ",
        take_message["blake_amount"],
        "BLAKE",
    )
    print(
        "asking:  ",
        f'{take_message["btc_sats"]:,}',
        "sats",
    )
    print()
    print(
        "PASS: offer was open when taker took it"
    )
    print(
        "PASS: take request is signed by taker"
    )
    print(
        "PASS: no money has moved"
    )

def command_rebroadcast_nostr_event(args):
    import asyncio

    requested_id = args.event_id.lower()

    #
    # Load only from our verified immutable cache.
    # nostr_load_cached_event() re-verifies both the
    # event ID and Schnorr signature before returning.
    #
    event = nostr_load_cached_event(
        requested_id
    )

    if event is None:
        raise FileNotFoundError(
            "No verified cached Nostr event: "
            + requested_id
        )

    if event.get("id") != requested_id:
        raise RuntimeError(
            "SECURITY FAILURE: cached event ID "
            "does not match requested event ID"
        )

    print()
    print("REBROADCASTING CACHED NOSTR EVENT")
    print("=================================")
    print("event id:", requested_id)
    print()
    print("PASS: cached event re-verified")
    print("PASS: existing signature will be reused")
    print("PASS: event will NOT be rebuilt or re-signed")

    published_id = asyncio.run(
        nostr_publish_event(event)
    )

    if published_id != requested_id:
        raise RuntimeError(
            "SECURITY FAILURE: rebroadcast changed "
            "the Nostr event ID"
        )

    print()
    print("NOSTR EVENT REBROADCAST")
    print("=======================")
    print("event id:", published_id)
    print()
    print("PASS: exact signed event was rebroadcast")
    print("PASS: event ID remained unchanged")





def scan_chain_for_expected_funding(swap, chain):
    """
    Find the unique confirmed funding output for this swap directly
    from the relevant blockchain.

    No coordination-layer txid or vout is trusted or required.

    The expected script/address is deterministically derived from the
    already-negotiated swap facts:
        H
        settlement pubkeys
        timeout

    The expected amount is also already part of the negotiated swap.

    Local regtest implementation: scans confirmed blocks from genesis.
    Public-network implementation will need an indexed/watch-only
    mechanism rather than a genesis scan.
    """

    if chain == "blake":
        cli = blake_cli

        desc = get_blake_contract_descriptor(
            swap
        )

        expected_address = derive_blake_contract_address(
            desc
        )

        expected_base_units = swap.blake_amount_base_units
        expected_amount_text = format_blake_amount(
            expected_base_units
        )

    elif chain == "btc":
        cli = btc_cli

        desc = get_btc_contract_descriptor(
            swap
        )

        expected_address = derive_btc_contract_address(
            desc
        )

        expected_base_units = int(swap.btc_sats)
        expected_amount_text = format(
            (Decimal(expected_base_units) / Decimal(100_000_000))
            .normalize(),
            "f",
        )

    else:
        raise ValueError(
            f"Unknown funding chain: {chain}"
        )

    tip = int(
        cli("getblockcount")
    )

    matches = []

    if chain == "blake":
        anchor_name = "blake_scan_start_height"
    else:
        anchor_name = "btc_scan_start_height"

    start_height = getattr(
        swap,
        anchor_name,
        None,
    )

    #
    # A missing anchor must NOT silently become a scan from genesis.
    # New swaps always record an anchor before funding is possible, and
    # the spender scan (find_confirmed_htlc_spend) already refuses to
    # scan unbounded in exactly this situation. Degrading to O(genesis)
    # is an operational denial of service against the participant's own
    # node and gives no warning.
    #
    # A genuinely ancient durable state can still be recovered by opting
    # in explicitly, which logs loudly.
    #
    if start_height is None:
        if funding_scan_override_enabled():
            print(
                "WARNING: no recorded "
                f"{chain.upper()} funding scan anchor. "
                "BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN is set, so "
                "this will scan from genesis. This is slow and "
                "unnecessary for any swap created by this version."
            )
            start_height = 0
        else:
            raise RuntimeError(
                f"SECURITY FAILURE: no recorded "
                f"{chain.upper()} funding scan anchor. Refusing to "
                "scan from genesis. Set "
                "BLAKE_SWAP_ALLOW_UNANCHORED_FUNDING_SCAN=1 to "
                "force it for a legacy durable state."
            )

    start_height = int(start_height)

    if start_height < 0:
        raise RuntimeError(
            f"Invalid {chain.upper()} funding scan anchor: "
            f"{start_height}"
        )

    if start_height > tip:
        raise RuntimeError(
            f"{chain.upper()} funding scan anchor "
            f"{start_height} is ahead of local tip {tip}; "
            "refusing to guess after possible chain reset "
            "or inconsistent local state."
        )

    scan_blocks = (
        tip - start_height + 1
    )

    #
    # Bound the work. Anchors are recorded at swap creation, so a huge
    # range means the anchor is wrong or stale, not that the swap is
    # long-lived. Refuse rather than quietly spending hours of RPC.
    #
    max_scan_blocks = funding_scan_max_blocks()

    if scan_blocks > max_scan_blocks:
        raise RuntimeError(
            f"SECURITY FAILURE: {chain.upper()} funding scan would "
            f"cover {scan_blocks} blocks, above the "
            f"{max_scan_blocks}-block limit "
            "(BLAKE_SWAP_MAX_FUNDING_SCAN_BLOCKS). Refusing an "
            "unbounded historical scan."
        )

    print(
        "FUNDING SCAN:",
        chain.upper(),
        f"start={start_height}",
        f"tip={tip}",
        f"blocks={scan_blocks}",
    )

    for height in range(
        start_height,
        tip + 1,
    ):
        blockhash = cli(
            "getblockhash",
            height,
        )

        block = cli(
            "getblock",
            blockhash,
            2,
        )

        for tx in block.get(
            "tx",
            [],
        ):
            txid = tx.get(
                "txid"
            )

            if not txid:
                continue

            for output in tx.get(
                "vout",
                [],
            ):
                script = output.get(
                    "scriptPubKey",
                    {},
                )

                addresses = []

                address = script.get(
                    "address"
                )

                if address:
                    addresses.append(
                        address
                    )

                old_addresses = script.get(
                    "addresses",
                    [],
                )

                if isinstance(
                    old_addresses,
                    list,
                ):
                    addresses.extend(
                        old_addresses
                    )

                if expected_address not in addresses:
                    continue

                try:
                    if chain == "blake":
                        value = blake_base_units(
                            output["value"]
                        )
                    else:
                        value = btc_sats_from_coin(
                            output["value"]
                        )

                except Exception:
                    continue

                if value != expected_base_units:
                    continue

                matches.append(
                    {
                        "txid": txid,
                        "vout": int(
                            output["n"]
                        ),
                        "blockhash": blockhash,
                        "height": height,
                        "address":
                            expected_address,
                        "amount":
                            expected_amount_text,
                    }
                )

    if not matches:
        return None

    if len(matches) != 1:
        raise RuntimeError(
            "SECURITY FAILURE: funding discovery is ambiguous. "
            f"Found {len(matches)} confirmed outputs matching "
            f"the exact {chain.upper()} HTLC address and amount. "
            "Refusing to guess which output is canonical."
        )

    return matches[0]


def _decoded_output(decoded, vout):
    """Return one output of a decoded transaction by index, or None."""

    if not isinstance(decoded, dict):
        return None

    for output in decoded.get("vout", []):
        if int(output.get("n", -1)) == int(vout):
            return output

    return None


def _require_funding_output_matches(swap, chain, output):
    """
    Prove a decoded transaction output is this swap's HTLC.

    Amount and derived HTLC address are what make a *recorded* outpoint
    trustworthy, so this is the check that replaces consulting the current
    UTXO set. A mismatch is a security failure: the caller must not fall
    back to searching for some different transaction.
    """
    if output is None:
        raise RuntimeError(
            f"Recorded {chain.upper()} funding transaction does not "
            "contain the recorded vout"
        )

    actual_address = (
        output
        .get("scriptPubKey", {})
        .get("address")
    )

    if chain == "blake":
        expected_address = derive_blake_contract_address(
            get_blake_contract_descriptor(swap)
        )

        actual = blake_base_units(output["value"])

        if actual != swap.blake_amount_base_units:
            raise RuntimeError(
                "Recorded BLAKE funding has the wrong amount: expected "
                f"{format_blake_amount(swap.blake_amount_base_units)} "
                f"({swap.blake_amount_base_units} base units), found "
                f"{output['value']}"
            )

    elif chain == "btc":
        expected_address = derive_btc_contract_address(
            get_btc_contract_descriptor(swap)
        )

        actual = btc_sats_from_coin(output["value"])

        if actual != int(swap.btc_sats):
            raise RuntimeError(
                "Recorded Bitcoin funding has the wrong amount: "
                f"expected {swap.btc_sats} sats, found {actual} sats"
            )

    else:
        raise ValueError(f"Unknown funding chain: {chain}")

    if actual_address != expected_address:
        raise RuntimeError(
            f"Recorded {chain.upper()} funding output does not match "
            "the expected HTLC address"
        )


def funding_transaction_facts(
    cli,
    wallet,
    txid,
    vout,
    recorded_blockhash=None,
):
    """
    Establish a recorded funding outpoint from the transaction itself.

    `gettxout` answers "is this unspent right now", so it stops being
    evidence the moment the HTLC is spent -- which is exactly the case
    that recovery of an older record has to handle. The transaction and
    its block are permanent, so confirmation is derived from those:

      1. the funding wallet, when this participant created the funding
         transaction (`gettransaction` reports the block and count);
      2. `getrawtransaction` from the recorded blockhash, which works on a
         node without `txindex`;
      3. `getrawtransaction` alone, for a mempool transaction or a node
         that does have `txindex`.

    Returns ``{"confirmations", "blockhash", "output", "tx"}`` or None
    when the transaction cannot be fetched at all.
    """

    #
    # The wallet path is best effort: the counterparty does not own the
    # other side's funding transaction and cannot see it this way.
    #
    if wallet:
        try:
            wallet_tx = cli(
                "gettransaction",
                txid,
                wallet=wallet,
            )
        except Exception:
            wallet_tx = None

        if isinstance(wallet_tx, dict) and wallet_tx.get("hex"):
            decoded = cli(
                "decoderawtransaction",
                wallet_tx["hex"],
            )

            return {
                "confirmations": int(
                    wallet_tx.get("confirmations", 0) or 0
                ),
                "blockhash": (
                    wallet_tx.get("blockhash")
                    or recorded_blockhash
                ),
                "output": _decoded_output(decoded, vout),
                "tx": decoded,
            }

    attempts = []

    if recorded_blockhash:
        attempts.append(recorded_blockhash)

    attempts.append(None)

    for blockhash in attempts:
        try:
            if blockhash:
                decoded = cli(
                    "getrawtransaction",
                    txid,
                    True,
                    blockhash,
                )
            else:
                decoded = cli(
                    "getrawtransaction",
                    txid,
                    True,
                )
        except Exception:
            continue

        if not isinstance(decoded, dict):
            continue

        return {
            "confirmations": int(
                decoded.get("confirmations", 0) or 0
            ),
            "blockhash": decoded.get("blockhash") or blockhash,
            "output": _decoded_output(decoded, vout),
            "tx": decoded,
        }

    return None


def recover_recorded_confirmed_funding(swap, chain):
    """
    Fast path for a funding transaction whose exact outpoint was already
    durably recorded before confirmation.

    Verify the recorded outpoint directly.  If it is confirmed and valid,
    recover its active blockhash from the local funding wallet and avoid a
    historical chain scan.

    Return None only when the recorded outpoint cannot be used as the fast
    path; callers may then fall back to scan_chain_for_expected_funding().
    A present-but-invalid recorded outpoint is a security failure and must
    not silently fall back to a different transaction.
    """

    fields = chain_dispatch(chain)

    cli = fields["cli"]
    wallet = fields["wallet"]
    block_field = fields["block_field"]

    txid = getattr(swap, fields["tx_field"])
    vout = getattr(swap, fields["vout_field"])

    if txid is None and vout is None:
        return None

    if txid is None or vout is None:
        raise RuntimeError(
            "SECURITY FAILURE: incomplete recorded "
            f"{chain.upper()} funding outpoint"
        )

    #
    # Confirm the recorded outpoint from the transaction and its block,
    # NOT from the current UTXO set. `gettxout` answers "is this unspent
    # right now", so it stops being evidence the moment the HTLC is spent
    # -- and recovering an older record after the HTLC has been spent is
    # exactly the case this fast path exists for.
    #
    facts = funding_transaction_facts(
        cli,
        wallet,
        txid,
        vout,
        getattr(swap, block_field),
    )

    if facts is None:
        # The transaction cannot be fetched at all here: fall back to the
        # bounded historical scan.
        return None

    confirmations = int(facts["confirmations"])

    if confirmations < 1:
        # Not confirmed yet, so this is not the confirmed fast path. The
        # caller keeps waiting rather than treating it as missing.
        return None

    #
    # This proves amount, derived HTLC address and confirmation. Any
    # mismatch in an already-recorded outpoint is a security failure; do
    # not silently search for some different transaction.
    #
    _require_funding_output_matches(
        swap,
        chain,
        facts["output"],
    )

    blockhash = (
        facts["blockhash"]
        or getattr(swap, block_field)
    )

    if not blockhash:
        # Confirmed, but no block can be identified: the historical scan
        # is the only way to anchor the block height. Rare, and the
        # outpoint itself has already been proven above.
        return None

    return {
        "txid": txid,
        "vout": int(vout),
        "blockhash": blockhash,
        "confirmations": confirmations,
        "recorded_fast_path": True,
        "spent_evidence": True,
    }

def observe_funding_from_watch_wallet(swap, chain):
    """
    Find this swap's confirmed funding output from the watch-only wallet.

    This is the observation path a public chain needs, and it is the
    reason the descriptor is imported before funding. The wallet already
    holds the HTLC's script, so one `listunspent` for that address answers
    where the output is and how deep it is -- no block scan, no
    transaction index, and it works equally on a pruned node. The
    verbosity-2 scan remains the fallback for the cases this cannot see:
    a swap funded before the descriptor was imported, or an output already
    spent before the participant ever looked.

    Returns None when the wallet has not seen a confirmed output for this
    swap, which is the normal answer before funding and the only reason to
    fall back to a scan. A present-but-wrong output is a security failure
    and is not resolved by scanning for a different one.
    """
    fields = chain_dispatch(chain)

    cli = fields["cli"]

    try:
        descriptor, expected_address = htlc_contract(swap, chain)
    except Exception:
        return None

    status = import_watch_only_htlc(swap, chain)

    if not status["imported"]:
        #
        # No watch-only wallet, so this path is unavailable rather than
        # empty: say so once and let the caller fall back.
        #
        print(
            "FUNDING OBSERVATION:",
            chain.upper(),
            "watch-only wallet unavailable:",
            status["reason"],
        )
        return None

    wallet = status["wallet"]

    unspent = cli(
        "listunspent",
        1,
        9_999_999,
        [expected_address],
        True,
        wallet=wallet,
    )

    if not isinstance(unspent, list) or not unspent:
        return None

    expected_base_units = (
        swap.blake_amount_base_units
        if chain == "blake"
        else int(swap.btc_sats)
    )

    for output in unspent:
        if not isinstance(output, dict):
            continue

        if output.get("address") != expected_address:
            continue

        try:
            if chain == "blake":
                value = blake_base_units(output["amount"])
            else:
                value = btc_sats_from_coin(output["amount"])
        except Exception:
            continue

        if value != expected_base_units:
            continue

        confirmations = int(output.get("confirmations", 0) or 0)

        if confirmations < 1:
            continue

        txid = output.get("txid")
        vout = output.get("vout")

        if not isinstance(txid, str) or vout is None:
            continue

        #
        # The block hash has to come from the transaction: `listunspent`
        # reports depth, not the block, and every later step anchors on
        # the hash rather than on a height that a reorg can invalidate.
        # The watch-only wallet holds the transaction, so its own
        # `gettransaction` answers this without an index.
        #
        facts = funding_transaction_facts(
            cli,
            wallet,
            txid,
            int(vout),
            None,
        )

        if facts is None:
            continue

        _require_funding_output_matches(
            swap,
            chain,
            facts["output"],
        )

        blockhash = facts["blockhash"]

        if not blockhash:
            continue

        print(
            "FUNDING OBSERVATION:",
            chain.upper(),
            f"watch-only wallet {wallet!r}",
            f"txid={txid}",
            f"vout={int(vout)}",
            f"confirmations={confirmations}",
        )

        return {
            "txid": txid,
            "vout": int(vout),
            "blockhash": blockhash,
            "confirmations": confirmations,
            "watch_only_observation": True,
            "spent_evidence": True,
        }

    return None


def command_sync_funding(args):
    """
    Reconcile funding state directly from confirmed chain history.

    This deliberately does not require blake-funded or btc-funded
    coordination messages.

    One invocation may recover both funding stages if local state is
    sufficiently stale and both confirmed outputs already exist.
    """

    swap = load_swap(
        args.swap_id,
        role=args.role,
    )

    print()
    print("FUNDING CHAIN RECONCILIATION")
    print("============================")
    print("swap id:", swap.swap_id)
    print("role:   ", args.role)
    print("start:  ", swap.state)

    progress = 0

    for _ in range(3):

        state = State(
            swap.state
        )

        #
        # The first economic action is Alice funding BLAKE.
        #
        if state == State.OFFERED:

            recovered = (
                recover_recorded_confirmed_funding(
                    swap,
                    "blake",
                )
                or observe_funding_from_watch_wallet(
                    swap,
                    "blake",
                )
                or scan_chain_for_expected_funding(
                    swap,
                    "blake",
                )
            )

            if recovered is None:
                print()
                print(
                    "No confirmed BLAKE funding output found."
                )
                break

            swap.blake_funding_txid = recovered[
                "txid"
            ]

            swap.blake_funding_vout = recovered[
                "vout"
            ]

            swap.blake_funding_blockhash = recovered[
                "blockhash"
            ]

            swap.transition(
                State.BLAKE_FUNDED
            )

            save_swap(
                swap,
                role=args.role,
            )

            progress += 1

            print()
            print("RECOVERED BLAKE FUNDING")
            print("-----------------------")
            print(
                "txid: ",
                swap.blake_funding_txid,
            )
            print(
                "vout: ",
                swap.blake_funding_vout,
            )
            print(
                "block:",
                swap.blake_funding_blockhash,
            )
            print(
                "state:",
                swap.state,
            )

            #
            # A half-populated outpoint is corruption, not a normal
            # condition. Fail closed here with a named diagnostic rather
            # than relying on a later stage to notice.
            #
            if (
                (swap.btc_funding_txid is None)
                != (swap.btc_funding_vout is None)
            ):
                raise RuntimeError(
                    "SECURITY FAILURE: incomplete recorded "
                    "BTC funding outpoint"
                )

            #
            # If Bitcoin funding is already recorded, continue around the
            # loop and synchronize it too.  Otherwise stop here.  Missing
            # Bitcoin funding is the normal next protocol condition after
            # BLAKE funding, and ordinary Resume must not launch a
            # historical Bitcoin scan merely to prove that Bob has not
            # funded yet.
            #
            if (
                swap.btc_funding_txid is None
                and swap.btc_funding_vout is None
            ):
                print()
                print(
                    "Bitcoin funding is not recorded; "
                    "leaving BTC discovery for the next protocol stage."
                )
                break

            continue

        #
        # The next economic action is Bob funding Bitcoin.
        #
        if state == State.BLAKE_FUNDED:

            recovered = (
                recover_recorded_confirmed_funding(
                    swap,
                    "btc",
                )
                or observe_funding_from_watch_wallet(
                    swap,
                    "btc",
                )
                or scan_chain_for_expected_funding(
                    swap,
                    "btc",
                )
            )

            if recovered is None:
                print()
                print(
                    "No confirmed Bitcoin funding output found."
                )
                break

            swap.btc_funding_txid = recovered[
                "txid"
            ]

            swap.btc_funding_vout = recovered[
                "vout"
            ]

            swap.btc_funding_blockhash = recovered[
                "blockhash"
            ]

            swap.transition(
                State.BTC_FUNDED
            )

            save_swap(
                swap,
                role=args.role,
            )

            progress += 1

            print()
            print("RECOVERED BITCOIN FUNDING")
            print("-------------------------")
            print(
                "txid: ",
                swap.btc_funding_txid,
            )
            print(
                "vout: ",
                swap.btc_funding_vout,
            )
            print(
                "block:",
                swap.btc_funding_blockhash,
            )
            print(
                "state:",
                swap.state,
            )

            continue

        #
        # Funding is already reconciled once BTC_FUNDED or any later
        # settlement state has been reached.
        #
        break

    print()
    print("FUNDING SYNC RESULT")
    print("===================")
    print(
        "progress:",
        progress,
    )
    print(
        "state:   ",
        swap.state,
    )

def command_sync_settlement(args):
    """
    Reconcile one party's durable swap state with confirmed blockchain truth.

    Coordination messages are NOT required.

    The party already knows the exact HTLC funding outpoints and contract
    semantics from previously verified swap state.  This command scans the
    relevant chain for a confirmed spend and advances local state only when
    the chain proves the corresponding settlement branch.

    It loops because one invocation may be able to recover more than one
    missed protocol event.
    """

    role = args.role

    swap = load_swap(
        args.swap_id,
        role=role,
    )

    print()
    print("SETTLEMENT CHAIN RECONCILIATION")
    print("===============================")
    print("swap id:", swap.swap_id)
    print("role:   ", role)
    print("start:  ", swap.state)

    progress = 0

    # More than enough for the current state graph, while still giving
    # us a hard bound against accidental infinite loops.
    for _ in range(6):

        state = State(swap.state)

        if state in {
            State.COMPLETE,
            State.BLAKE_CLAIMED,
        }:
            #
            # Terminal claim state is not trusted merely
            # because it was persisted earlier.
            #
            # If the recorded final BLAKE claim disappeared
            # in a reorg, recover only after both chains prove
            # that BTC_CLAIMED is again authoritative.
            #
            rolled_back = recover_final_blake_claim_reorg(
                swap
            )

            if rolled_back == "pending":
                print()
                print("FINAL BLAKE CLAIM PENDING AFTER REORG")
                print("====================================")
                print("state:", swap.state)
                print(
                    "PASS: exact recorded BLAKE claim "
                    "remains live in mempool"
                )
                print(
                    "PASS: durable state was not rolled back"
                )
                break

            if rolled_back is True:
                old_state = state.value

                save_swap(
                    swap,
                    role=role,
                )

                progress += 1

                print()
                print("FINAL BLAKE CLAIM REORG RECOVERED")
                print("=================================")
                print("old state:", old_state)
                print("new state:", swap.state)
                print(
                    "PASS: Bitcoin claim remains confirmed"
                )
                print(
                    "PASS: BLAKE HTLC is available again"
                )

                #
                # Continue through reconciliation using the
                # newly authoritative BTC_CLAIMED state.
                #
                continue

            break

        if state == State.BLAKE_REFUNDED:
            recovered_refund = (
                recover_final_blake_refund_reorg(
                    swap
                )
            )

            if recovered_refund == "pending":
                print()
                print("FINAL BLAKE REFUND PENDING AFTER REORG")
                print("=====================================")
                print("state:", swap.state)
                print(
                    "PASS: exact recorded BLAKE refund "
                    "remains live in mempool"
                )
                print(
                    "PASS: durable state was not rolled back"
                )
                break

            if recovered_refund is True:
                old_state = state.value

                save_swap(
                    swap,
                    role=role,
                )

                progress += 1

                print()
                print("FINAL BLAKE REFUND REORG RECOVERED")
                print("==================================")
                print("old state:", old_state)
                print("new state:", swap.state)
                print(
                    "PASS: Bitcoin refund remains confirmed"
                )
                print(
                    "PASS: BLAKE HTLC is available again"
                )

                continue

            #
            # False means the exact recorded refund remains
            # confirmed on the active BLAKE chain.
            #
            break

        # No settlement spend exists yet at OFFERED.
        if state == State.OFFERED:
            break

        #
        # Decide which chain contains the next authoritative fact.
        #
        # BLAKE_FUNDED:
        #     Only a BLAKE refund can end the swap before BTC funding
        #     has been acknowledged in local state.
        #
        # BTC_FUNDED:
        #     Bitcoin can now reveal either Alice's claim or Bob's refund.
        #
        # BTC_CLAIMED:
        #     BLAKE should next reveal Bob's claim.
        #
        # BTC_REFUNDED:
        #     BLAKE should next reveal Alice's refund.
        #
        if state == State.BLAKE_FUNDED:
            chain = "blake"
            permitted = {"refund"}

        elif state == State.BTC_FUNDED:
            chain = "btc"
            permitted = {"claim", "refund"}

        elif state == State.BTC_CLAIMED:
            chain = "blake"
            permitted = {"claim"}

        elif state == State.BTC_REFUNDED:
            chain = "blake"
            permitted = {"refund"}

        elif state == State.SECRET_REVEALED:
            raise RuntimeError(
                "SECURITY FAILURE: durable state is unexpectedly "
                "SECRET_REVEALED. This intermediate state should not "
                "exist independently on disk."
            )

        else:
            raise RuntimeError(
                f"Cannot reconcile settlement from state {state.value}"
            )

        recovered = find_confirmed_htlc_spend(
            swap,
            chain,
        )

        if recovered is None:
            print()
            print(
                f"No confirmed {chain.upper()} HTLC spend "
                f"requires reconciliation."
            )
            break

        if not isinstance(recovered, dict):
            raise RuntimeError(
                "Settlement recovery helper returned malformed result"
            )

        txid = recovered.get("txid")
        blockhash = recovered.get("blockhash")
        branch = recovered.get("branch")

        if not txid or not blockhash:
            raise RuntimeError(
                "Settlement recovery result is missing txid/blockhash"
            )

        if branch not in {"claim", "refund"}:
            raise RuntimeError(
                f"Settlement recovery returned unknown branch: {branch!r}"
            )

        if branch not in permitted:
            raise RuntimeError(
                "SECURITY FAILURE: confirmed HTLC spend is inconsistent "
                f"with local protocol state. "
                f"state={state.value} chain={chain} branch={branch}"
            )

        old_state = swap.state

        #
        # Bitcoin claim:
        # Alice revealed S.  Bob can independently recover it directly
        # from the confirmed Bitcoin witness.
        #
        if chain == "btc" and branch == "claim":

            swap.btc_claim_txid = txid
            swap.btc_claim_blockhash = blockhash

            swap.transition(
                State.SECRET_REVEALED
            )

            swap.transition(
                State.BTC_CLAIMED
            )

            if role == "bob":
                revealed = extract_secret_from_btc_claim(
                    swap
                )

                calculated = hashlib.sha256(
                    bytes.fromhex(revealed)
                ).hexdigest()

                if calculated != swap.secret_hash:
                    raise RuntimeError(
                        "SECURITY FAILURE: Bitcoin-recovered secret "
                        "does not match swap hash"
                    )

                swap.revealed_secret_hex = revealed

        #
        # Bitcoin refund.
        #
        elif chain == "btc" and branch == "refund":

            swap.btc_refund_txid = txid
            swap.btc_refund_blockhash = blockhash

            swap.transition(
                State.BTC_REFUNDED
            )

        #
        # BLAKE claim.
        #
        elif chain == "blake" and branch == "claim":

            swap.blake_claim_txid = txid
            swap.blake_claim_blockhash = blockhash

            swap.transition(
                State.BLAKE_CLAIMED
            )

        #
        # BLAKE refund.
        #
        elif chain == "blake" and branch == "refund":

            swap.blake_refund_txid = txid
            swap.blake_refund_blockhash = blockhash

            swap.transition(
                State.BLAKE_REFUNDED
            )

        else:
            raise RuntimeError(
                "Unhandled settlement reconciliation case"
            )

        save_swap(
            swap,
            role=role,
        )

        progress += 1

        print()
        print("RECOVERED CONFIRMED SETTLEMENT")
        print("------------------------------")
        print("chain: ", chain)
        print("branch:", branch)
        print("txid:  ", txid)
        print("block: ", blockhash)
        print("state: ", old_state, "->", swap.state)

        if (
            role == "bob"
            and chain == "btc"
            and branch == "claim"
        ):
            print(
                "PASS: Bob independently recovered S "
                "from Bitcoin; no settlement message was required."
            )

    print()
    print("SYNC RESULT")
    print("===========")
    print("progress:", progress)
    print("state:   ", swap.state)


def _test_crash_after_broadcast(point):
    """
    Deterministic LOCAL REGTEST fault injection.

    Disabled unless BLAKE_SWAP_TEST_MODE=1.
    """
    import os as _os

    if _os.environ.get("BLAKE_SWAP_TEST_MODE") != "1":
        return

    if _os.environ.get("BLAKE_SWAP_TEST_CRASH_POINT") != point:
        return

    if _os.environ.get("BLAKE_SWAP_TRANSPORT", "file") != "file":
        raise RuntimeError(
            "Settlement crash injection requires file transport"
        )

    _os._exit(86)



def _reconcile_local_role(swap):
    """Derive protocol role from identities pinned into this swap."""
    me = current_nostr_pubkey()
    alice = bool(swap.nostr_alice_pub) and me == swap.nostr_alice_pub
    bob = bool(swap.nostr_bob_pub) and me == swap.nostr_bob_pub

    if alice and bob:
        raise RuntimeError(
            "SECURITY FAILURE: current participant is pinned as both protocol roles"
        )
    if alice:
        return "alice", me, swap.nostr_bob_pub
    if bob:
        return "bob", me, swap.nostr_alice_pub

    raise RuntimeError(
        "SECURITY FAILURE: current Nostr identity is not pinned to this swap"
    )


def require_local_protocol_role(
    swap,
    expected_role,
    action,
):
    """
    Fail closed before any economic action unless the persistent
    participant identity is pinned to this swap in the protocol
    role required by that action.
    """
    if expected_role not in ("alice", "bob"):
        raise ValueError(
            "expected_role must be alice or bob"
        )

    actual_role, me, counterparty = (
        _reconcile_local_role(swap)
    )

    if actual_role != expected_role:
        raise RuntimeError(
            "SECURITY FAILURE: refusing "
            f"{action} as protocol {actual_role}; "
            f"protocol requires {expected_role}"
        )

    return actual_role, me, counterparty


def _reconcile_quiet(fn, *args, **kwargs):
    import contextlib
    import io
    with contextlib.redirect_stdout(io.StringIO()):
        return fn(*args, **kwargs)


def _reconcile_observe_htlc(swap, chain):
    """Observe funding/spend truth without mutating durable swap state."""
    import copy

    work = copy.copy(swap)

    fields = chain_dispatch(chain)

    cli = fields["cli"]
    tx_field = fields["tx_field"]
    vout_field = fields["vout_field"]
    block_field = fields["block_field"]
    verifier = fields["verifier"]

    txid = getattr(work, tx_field)
    vout = getattr(work, vout_field)
    blockhash = getattr(work, block_field)


    if (txid is None) != (vout is None):
        raise RuntimeError(
            f"SECURITY FAILURE: incomplete recorded {chain.upper()} funding outpoint"
        )

    discovered = False

    if txid is None:
        # Direct-chain recovery is deliberately allowed in this read-only
        # report.  It updates only this temporary in-memory copy.
        try:

            match = _reconcile_quiet(
                scan_chain_for_expected_funding,
                work,
                chain,
            )

        except Exception as e:
            return {
                "status": "unknown",
                "reason": f"{type(e).__name__}: {e}",
            }

        if match is None:
            return {"status": "absent"}

        discovered = True
        txid = match["txid"]
        vout = match["vout"]
        blockhash = match["blockhash"]
        setattr(work, tx_field, txid)
        setattr(work, vout_field, vout)
        setattr(work, block_field, blockhash)


    utxo = cli("gettxout", txid, vout)


    if utxo:
        confirmations = int(
            utxo.get("confirmations", 0) or 0
        )

        #
        # A zero-confirmation funding output is not invalid merely
        # because it is still waiting for a block.  Before classifying
        # it as pending, independently prove that the observed output
        # has the expected amount and HTLC address.
        #
        if confirmations < 1:
            try:
                actual_address = (
                    utxo
                    .get("scriptPubKey", {})
                    .get("address")
                )

                if chain == "blake":
                    expected_address = (
                        derive_blake_contract_address(
                            get_blake_contract_descriptor(work)
                        )
                    )

                    actual_amount = blake_base_units(
                        utxo["value"]
                    )

                    if (
                        actual_amount
                        != work.blake_amount_base_units
                    ):
                        raise RuntimeError(
                            "Wrong BLAKE amount: expected "
                            f"{format_blake_amount(work.blake_amount_base_units)} "
                            f"({work.blake_amount_base_units} base units), "
                            f"found {utxo['value']}"
                        )

                    if actual_address != expected_address:
                        raise RuntimeError(
                            "BLAKE funding output does not match "
                            "the expected HTLC address"
                        )

                else:
                    expected_address = (
                        derive_btc_contract_address(
                            get_btc_contract_descriptor(work)
                        )
                    )

                    actual_sats = btc_sats_from_coin(
                        utxo["value"]
                    )

                    if actual_sats != work.btc_sats:
                        raise RuntimeError(
                            "Wrong Bitcoin amount: expected "
                            f"{work.btc_sats} sats, "
                            f"found {actual_sats} sats"
                        )

                    if actual_address != expected_address:
                        raise RuntimeError(
                            "Bitcoin funding output does not match "
                            "the expected HTLC address"
                        )

            except Exception as e:
                return {
                    "status": "invalid",
                    "txid": txid,
                    "vout": vout,
                    "reason": f"{type(e).__name__}: {e}",
                }

            return {
                "status": "pending",
                "txid": txid,
                "vout": vout,
                "blockhash": blockhash,
                "confirmations": confirmations,
                "discovered": discovered,
            }

        try:
            _reconcile_quiet(verifier, work, verbose=False)
        except Exception as e:
            return {
                "status": "invalid",
                "txid": txid,
                "vout": vout,
                "reason": f"{type(e).__name__}: {e}",
            }

        return {
            "status": "funded",
            "txid": txid,
            "vout": vout,
            "blockhash": blockhash,
            "confirmations": confirmations,
            "discovered": discovered,
        }

    #
    # A recorded funding transaction may still be in the mempool.
    # Some RPC/provider paths can fail to expose the output through
    # gettxout even though the exact recorded transaction is available.
    #
    # Before calling that state unknown, independently fetch the
    # recorded transaction and prove that its exact vout is the expected
    # HTLC output.  Only an unconfirmed transaction is classified here;
    # confirmed/spent cases continue through normal settlement recovery.
    #
    if not blockhash:
        try:
            funding_tx = cli(
                "getrawtransaction",
                txid,
                True,
            )
        except Exception:
            funding_tx = None

        if funding_tx is not None:
            confirmations = int(
                funding_tx.get("confirmations", 0) or 0
            )

            if confirmations < 1:
                funding_output = next(
                    (
                        output
                        for output in funding_tx.get("vout", [])
                        if int(output.get("n", -1)) == int(vout)
                    ),
                    None,
                )

                if funding_output is None:
                    return {
                        "status": "invalid",
                        "txid": txid,
                        "vout": vout,
                        "reason": (
                            "recorded unconfirmed funding transaction "
                            "does not contain the recorded vout"
                        ),
                    }

                actual_address = (
                    funding_output
                    .get("scriptPubKey", {})
                    .get("address")
                )

                try:
                    if chain == "blake":
                        expected_address = (
                            derive_blake_contract_address(
                                get_blake_contract_descriptor(work)
                            )
                        )

                        actual_amount = blake_base_units(
                            funding_output["value"]
                        )

                        if (
                            actual_amount
                            != work.blake_amount_base_units
                        ):
                            raise RuntimeError(
                                "recorded pending BLAKE funding "
                                "has the wrong amount: expected "
                                f"{format_blake_amount(work.blake_amount_base_units)} "
                                f"({work.blake_amount_base_units} base "
                                f"units), found {funding_output['value']}"
                            )

                    else:
                        expected_address = (
                            derive_btc_contract_address(
                                get_btc_contract_descriptor(work)
                            )
                        )

                        actual_sats = btc_sats_from_coin(
                            funding_output["value"]
                        )

                        if actual_sats != int(work.btc_sats):
                            raise RuntimeError(
                                "recorded pending Bitcoin funding "
                                "has the wrong amount"
                            )

                    if actual_address != expected_address:
                        raise RuntimeError(
                            f"recorded pending {chain.upper()} funding "
                            "does not match the expected HTLC address"
                        )

                except Exception as e:
                    return {
                        "status": "invalid",
                        "txid": txid,
                        "vout": vout,
                        "reason": f"{type(e).__name__}: {e}",
                    }

                return {
                    "status": "pending",
                    "txid": txid,
                    "vout": vout,
                    "blockhash": None,
                    "confirmations": 0,
                    "discovered": discovered,
                }

        #
        # The funding blockhash may never have been synchronized before
        # this HTLC was spent.  Missing funding blockhash therefore does
        # not prove ambiguity: continue to settlement-spender recovery
        # below.  find_confirmed_htlc_spend() can establish the exact
        # confirmed claim/refund independently, including through the
        # hardened wallet/block fallback on nodes without txindex.
        #

    #
    # The local participant may already have broadcast its expected
    # claim/refund transaction and durably recorded its txid while that
    # settlement transaction is still unconfirmed.
    #
    # Prove the exact mempool transaction spends this HTLC and uses the
    # expected branch.  This is a WAIT condition, not manual attention.
    #
    if chain == "btc":
        known_spenders = [
            ("claim", work.btc_claim_txid, work.btc_claim_blockhash),
            ("refund", work.btc_refund_txid, work.btc_refund_blockhash),
        ]
    else:
        known_spenders = [
            ("claim", work.blake_claim_txid, work.blake_claim_blockhash),
            ("refund", work.blake_refund_txid, work.blake_refund_blockhash),
        ]

    pending_spenders = [
        (expected_branch, settlement_txid)
        for expected_branch, settlement_txid, settlement_blockhash
        in known_spenders
        if settlement_txid is not None
        and settlement_blockhash is None
    ]

    if len(pending_spenders) > 1:
        return {
            "status": "invalid",
            "txid": txid,
            "vout": vout,
            "reason": (
                "durable state records multiple unconfirmed "
                f"{chain.upper()} settlement transactions"
            ),
        }

    if pending_spenders:
        expected_branch, settlement_txid = pending_spenders[0]

        #
        # Direct lookup is only a fast path for recognizing our exact
        # unconfirmed settlement.  Failure to retrieve the tx here is
        # not itself ambiguous: it may have just confirmed, or a node
        # without txindex may no longer expose it through this RPC.
        #
        # In either case, fall through to find_confirmed_htlc_spend(),
        # which has the hardened wallet/block recovery path.
        #
        try:
            settlement_tx = cli(
                "getrawtransaction",
                settlement_txid,
                True,
            )
        except Exception:
            settlement_tx = None

        if settlement_tx is not None:
            confirmations = int(
                settlement_tx.get("confirmations", 0) or 0
            )

            if confirmations < 1:
                try:
                    spends_htlc = any(
                        vin.get("txid") == txid
                        and int(vin.get("vout", -1)) == int(vout)
                        for vin in settlement_tx.get("vin", [])
                    )

                    if not spends_htlc:
                        raise RuntimeError(
                            "recorded pending settlement transaction "
                            "does not spend this swap's HTLC"
                        )

                    actual_branch = classify_htlc_spend(
                        settlement_tx,
                        txid,
                        vout,
                        work.secret_hash,
                    )

                    if actual_branch != expected_branch:
                        raise RuntimeError(
                            "recorded pending settlement branch mismatch: "
                            f"expected {expected_branch}, "
                            f"observed {actual_branch}"
                        )

                except Exception as e:
                    return {
                        "status": "pending-or-ambiguous",
                        "txid": txid,
                        "vout": vout,
                        "reason": f"{type(e).__name__}: {e}",
                    }

                return {
                    "status": "pending-spend",
                    "txid": txid,
                    "vout": vout,
                    "spend_txid": settlement_txid,
                    "branch": actual_branch,
                    "confirmations": 0,
                    "discovered": discovered,
                }

            # A directly visible confirmed transaction deliberately falls
            # through too.  find_confirmed_htlc_spend() independently
            # verifies the recorded txid, HTLC outpoint, branch, active
            # chain status, confirmations, and blockhash.

    try:

        spend = _reconcile_quiet(
            find_confirmed_htlc_spend,
            work,
            chain,
        )

    except Exception as e:
        return {
            "status": "pending-or-ambiguous",
            "txid": txid,
            "vout": vout,
            "reason": f"{type(e).__name__}: {e}",
        }

    if spend is None:
        return {
            "status": "unknown",
            "txid": txid,
            "vout": vout,
            "reason": "outpoint unavailable but no confirmed spender was found",
        }

    return {
        "status": "spent",
        "txid": txid,
        "vout": vout,
        "spend_txid": spend["txid"],
        "spend_blockhash": spend["blockhash"],
        "branch": spend["branch"],
        "discovered": discovered,
    }

def _reconcile_next_action(swap, role, blake, btc, blake_height, btc_height):
    """Return one conservative recommendation.  Never execute it here."""
    state = State(swap.state)

    if state == State.COMPLETE:
        if (
            blake.get("status") == "spent"
            and blake.get("branch") == "claim"
            and btc.get("status") == "spent"
            and btc.get("branch") == "claim"
        ):
            return (
                "none",
                "swap is COMPLETE and both confirmed "
                "claim branches remain active",
            )

        return (
            "manual-attention",
            "local checkpoint is COMPLETE but current "
            "chain reality does not prove both claim branches",
        )

    if blake.get("status") == "pending-spend":
        return (
            "wait",
            "BLAKE settlement transaction is broadcast "
            "and awaiting confirmation",
        )

    if btc.get("status") == "pending-spend":
        return (
            "wait",
            "Bitcoin settlement transaction is broadcast "
            "and awaiting confirmation",
        )

    unsafe = {"unknown", "invalid", "pending-or-ambiguous"}
    if blake.get("status") in unsafe:
        return "manual-attention", f"BLAKE observation is {blake['status']}"
    if btc.get("status") in unsafe:
        return "manual-attention", f"Bitcoin observation is {btc['status']}"

    if blake.get("status") == "pending":
        return (
            "wait",
            "BLAKE funding is broadcast and awaiting confirmation",
        )

    if btc.get("status") == "pending":
        return (
            "wait",
            "Bitcoin funding is broadcast and awaiting confirmation",
        )

    # Chain truth outranks a stale checkpoint.

    # Confirmed funding may also be ahead of the durable checkpoint.
    #
    # Never recommend an action whose command requires a newer local
    # state than we have persisted. First synchronize the checkpoint
    # from confirmed chain truth, then reconcile again.
    if (
        blake.get("status") == "funded"
        and state == State.OFFERED
    ):
        return (
            "sync-funding",
            "confirmed BLAKE funding is ahead of local checkpoint",
        )

    if (
        btc.get("status") == "funded"
        and state in {
            State.OFFERED,
            State.BLAKE_FUNDED,
        }
    ):
        return (
            "sync-funding",
            "confirmed Bitcoin funding is ahead of local checkpoint",
        )


    # A confirmed Bitcoin spend necessarily proves that Bitcoin funding
    # happened first. If durable state has not yet imported BTC funding,
    # restore that checkpoint before attempting settlement reconciliation.
    if (
        btc.get("status") == "spent"
        and state in {
            State.OFFERED,
            State.BLAKE_FUNDED,
        }
    ):
        return (
            "sync-funding",
            "confirmed Bitcoin settlement exists but Bitcoin funding "
            "is ahead of local checkpoint",
        )

    if blake.get("status") == "spent":
        if blake.get("branch") == "claim":
            if state == State.BLAKE_CLAIMED:
                return "complete", "final BLAKE claim is confirmed"
            return "sync-settlement", "confirmed BLAKE claim is ahead of local checkpoint"
        if blake.get("branch") == "refund":
            if state == State.BLAKE_REFUNDED:
                return "none", "final BLAKE refund is confirmed"
            return "sync-settlement", "confirmed BLAKE refund is ahead of local checkpoint"

    if btc.get("status") == "spent":
        if btc.get("branch") == "claim":
            if state not in {State.BTC_CLAIMED, State.BLAKE_CLAIMED}:
                return "sync-settlement", "confirmed Bitcoin claim is ahead of local checkpoint"
            if role == "bob":
                if not swap.revealed_secret_hex:
                    return "sync-settlement", "Bitcoin claim reveals S but local Bob state has not recovered it"
                if blake.get("status") == "funded":
                    return "claim-blake", "S is known and the BLAKE HTLC remains unspent"
            return "wait", "Bitcoin claim is confirmed; protocol Bob must claim BLAKE"

        if btc.get("branch") == "refund":
            if state != State.BTC_REFUNDED:
                return "sync-settlement", "confirmed Bitcoin refund is ahead of local checkpoint"
            if role == "alice" and blake.get("status") == "funded":
                if blake_height >= int(swap.blake_timeout):
                    return "refund-blake", "Bitcoin was refunded and BLAKE refund is mature"
                return "wait", "Bitcoin was refunded; BLAKE refund is not mature"
            return "wait", "Bitcoin refund is confirmed; protocol Alice controls BLAKE refund"

    if role == "alice":
        if blake.get("status") == "absent":
            if not (swap.blake_bob_pub and swap.btc_bob_pub and swap.nostr_bob_pub):
                return "wait", "protocol Bob acceptance/public keys are not fully pinned"
            if blake_height >= int(swap.blake_timeout):
                return "manual-attention", "BLAKE timeout has already been reached"
            return "fund-blake", "counterparty is pinned and no BLAKE funding exists"

        if blake.get("status") == "funded" and btc.get("status") == "absent":
            if blake_height >= int(swap.blake_timeout):
                return (
                    "refund-blake",
                    "Bitcoin was never funded and BLAKE refund is mature",
                )

            if btc_height >= int(swap.btc_timeout):
                return (
                    "wait",
                    "Bitcoin funding window has closed; "
                    "waiting for BLAKE refund maturity",
                )

            return "wait", "BLAKE is funded; protocol Bob must fund Bitcoin"

        if btc.get("status") == "funded":
            if not swap.secret_hex:
                return "manual-attention", "protocol Alice has no local secret S"
            if btc_height >= int(swap.btc_timeout):
                return "manual-attention", "Bitcoin refund timelock is mature; claim may race a refund"
            return "claim-btc", "Bitcoin HTLC is confirmed/unspent and protocol Alice possesses S"

    if role == "bob":
        if blake.get("status") == "absent":
            return "wait", "protocol Alice must fund BLAKE first"

        if blake.get("status") == "funded" and btc.get("status") == "absent":
            if btc_height >= int(swap.btc_timeout):
                return "manual-attention", "Bitcoin timeout has already been reached"
            return "fund-btc", "BLAKE HTLC is confirmed/unspent and Bitcoin is not yet funded"

        if btc.get("status") == "funded":
            if btc_height >= int(swap.btc_timeout):
                return "refund-btc", "Bitcoin HTLC is unspent and its refund timelock is mature"
            return "wait", "Bitcoin is funded; protocol Alice must claim and reveal S"

    return "manual-attention", f"no conservative rule matched checkpoint {state.value}"

def _reconcile_negotiation_status(swap):
    """
    Classify whether enough authenticated negotiation state
    exists to derive HTLCs and safely inspect chain reality.

    This is deliberately separate from blockchain state.
    """

    if not swap.nostr_alice_pub or not swap.nostr_bob_pub:
        return (
            "identity-unbound",
            "both protocol Nostr identities are not pinned",
        )

    alice_keys = (
        bool(swap.blake_alice_pub)
        and bool(swap.btc_alice_pub)
    )

    bob_keys = (
        bool(swap.blake_bob_pub)
        and bool(swap.btc_bob_pub)
    )

    if not alice_keys:
        return (
            "negotiation-incomplete",
            "protocol Alice settlement pubkeys were never created or imported",
        )

    if not bob_keys:
        return (
            "negotiation-incomplete",
            "protocol Bob settlement pubkeys were never created or imported",
        )

    return (
        "ready",
        "all protocol identities and settlement pubkeys are pinned",
    )



def _reconcile_execution_readiness(
    swap,
    role,
    action,
    blake_height,
    btc_height,
):
    result = {
        "checked": False,
        "action": action,
        "ready": None,
        "wallet": (
            BLAKE_WALLET
            if action == "fund-blake"
            else BTC_WALLET
            if action == "fund-btc"
            else None
        ),
        "wallet_loaded": None,
        "trusted_balance": None,
        "required_balance": None,
        "fee_reserve": None,
        "balance_unit": None,
        "timeout_blocks_remaining": None,
        "minimum_timeout_blocks": None,
        "reasons": [],
    }

    if action not in {"fund-blake", "fund-btc"}:
        return result

    result["checked"] = True

    if action == "fund-blake":
        wallet = BLAKE_WALLET

        if role != "alice":
            result["ready"] = False
            result["reasons"].append("fund-blake requires protocol Alice")
            return result

        cli = blake_cli
        chain_name = "BLAKE"
        balance_unit = "BLAKE"

        fee_reserve = Decimal(
            os.environ.get(
                "BLAKE_SWAP_RUN_BLAKE_FEE_RESERVE",
                "0.001",
            )
        )

        if fee_reserve < 0:
            raise ValueError(
                "BLAKE_SWAP_RUN_BLAKE_FEE_RESERVE must be >= 0"
            )

        required = blake_amount_decimal(
            swap.blake_amount_base_units
        ) + fee_reserve
        remaining = int(swap.blake_timeout) - int(blake_height)

        minimum_blocks = int(
            os.environ.get(
                "BLAKE_SWAP_RUN_MIN_BLAKE_FUND_BLOCKS",
                "2",
            )
        )

    else:
        wallet = BTC_WALLET

        if role != "bob":
            result["ready"] = False
            result["reasons"].append("fund-btc requires protocol Bob")
            return result

        cli = btc_cli
        chain_name = "Bitcoin"
        balance_unit = "BTC"

        fee_reserve_sats = int(
            os.environ.get(
                "BLAKE_SWAP_RUN_BTC_FEE_RESERVE_SATS",
                "5000",
            )
        )

        if fee_reserve_sats < 0:
            raise ValueError(
                "BLAKE_SWAP_RUN_BTC_FEE_RESERVE_SATS must be >= 0"
            )

        fee_reserve = (
            Decimal(fee_reserve_sats)
            / Decimal(100_000_000)
        )

        required = (
            Decimal(swap.btc_sats)
            / Decimal(100_000_000)
            + fee_reserve
        )

        remaining = int(swap.btc_timeout) - int(btc_height)

        minimum_blocks = int(
            os.environ.get(
                "BLAKE_SWAP_RUN_MIN_BTC_FUND_BLOCKS",
                "2",
            )
        )

    if minimum_blocks < 1:
        raise ValueError(
            "funding minimum timeout blocks must be >= 1"
        )

    result["fee_reserve"] = str(fee_reserve)
    result["required_balance"] = str(required)
    result["balance_unit"] = balance_unit
    result["timeout_blocks_remaining"] = remaining
    result["minimum_timeout_blocks"] = minimum_blocks

    try:
        loaded = cli("listwallets")
    except Exception as e:
        result["ready"] = False
        result["reasons"].append(
            f"{chain_name} wallet service unavailable: "
            f"{type(e).__name__}: {e}"
        )
        return result

    wallet_loaded = isinstance(loaded, list) and wallet in loaded
    result["wallet_loaded"] = wallet_loaded

    if not wallet_loaded:
        result["reasons"].append(
            f"local wallet {wallet!r} is not loaded"
        )

    trusted = None

    if wallet_loaded:
        try:
            balances = cli(
                "getbalances",
                wallet=wallet,
            )

            trusted = Decimal(
                str(
                    balances
                    .get("mine", {})
                    .get("trusted", 0)
                )
            )

            result["trusted_balance"] = str(trusted)

        except Exception as e:
            result["reasons"].append(
                "could not read trusted wallet balance: "
                f"{type(e).__name__}: {e}"
            )

    if trusted is not None and trusted < required:
        result["reasons"].append(
            "trusted wallet balance is below amount plus configured fee reserve"
        )

    if remaining < minimum_blocks:
        result["reasons"].append(
            "timeout margin is below configured funding minimum"
        )

    result["ready"] = (
        wallet_loaded
        and trusted is not None
        and trusted >= required
        and remaining >= minimum_blocks
    )

    if result["ready"]:
        result["reasons"].append(
            "wallet, balance, fee reserve, and timeout margin are ready"
        )

    return result

def build_reconcile_report(swap):
    """Build a machine-readable, read-only protocol report."""

    negotiation_status, negotiation_reason = (
        _reconcile_negotiation_status(swap)
    )


    try:
        role, me, counterparty = _reconcile_local_role(swap)
    except RuntimeError as e:
        return {
            "swap_id": swap.swap_id,
            "participant_pubkey": current_nostr_pubkey(),
            "protocol_role": None,
            "counterparty_pubkey": None,
            "checkpoint_state": swap.state,
            "decision_class": "manual-attention",
            "negotiation_status": "identity-unbound",
            "negotiation_reason": str(e),
            "blake": {"status": "not-evaluated"},
            "btc": {"status": "not-evaluated"},
            "secret": {
                "known": False,
                "source": "not-evaluated",
                "hash_matches": None,
            },
            "next_action": "manual-attention",
            "reason": (
                "current participant cannot be securely mapped "
                "to a protocol role"
            ),
            "read_only": True,
        }

    if negotiation_status != "ready":
        secret_value = None
        secret_source = "none"

        if role == "alice" and swap.secret_hex:
            secret_value = swap.secret_hex
            secret_source = "locally-generated"

        elif role == "bob" and swap.revealed_secret_hex:
            secret_value = swap.revealed_secret_hex
            secret_source = "recovered-from-bitcoin"

        secret_matches = None

        if secret_value:
            secret_matches = (
                hashlib.sha256(
                    bytes.fromhex(secret_value)
                ).hexdigest()
                == swap.secret_hash
            )

        return {
            "swap_id": swap.swap_id,
            "participant_pubkey": me,
            "protocol_role": role,
            "counterparty_pubkey": counterparty,
            "checkpoint_state": swap.state,
            "decision_class": "manual-attention",
            "negotiation_status": negotiation_status,
            "negotiation_reason": negotiation_reason,
            "blake": {"status": "not-evaluated"},
            "btc": {"status": "not-evaluated"},
            "secret": {
                "known": bool(secret_value),
                "source": secret_source,
                "hash_matches": secret_matches,
            },
            "next_action": "manual-attention",
            "reason": negotiation_reason,
            "read_only": True,
        }

    blake_height = int(blake_cli("getblockcount"))

    btc_height = int(btc_cli("getblockcount"))

    blake = _reconcile_observe_htlc(swap, "blake")

    state = State(swap.state)

    #
    # Do not perform an expensive forensic Bitcoin funding scan when
    # current protocol state already has a cheaper, unambiguous next step.
    #
    # OFFERED + confirmed BLAKE means the durable checkpoint is stale:
    # synchronize BLAKE funding first, then reconcile again.
    #
    # BLAKE_FUNDED + no recorded Bitcoin outpoint is the normal waiting
    # condition while Bob's Bitcoin funding window remains open.  Treat it
    # as absent for ordinary reconciliation.  Once that window closes,
    # fall back to full chain discovery before any refund decision so that
    # "Bitcoin was never funded" is independently established.
    #
    if (
        state == State.OFFERED
        and blake.get("status") == "funded"
    ):
        btc = {
            "status": "not-evaluated",
            "reason": (
                "Bitcoin observation deferred until confirmed "
                "BLAKE funding is synchronized locally"
            ),
        }

    elif (
        role == "bob"
        and state == State.BLAKE_FUNDED
        and swap.btc_funding_txid is None
        and swap.btc_funding_vout is None
        and btc_height < int(swap.btc_timeout)
    ):
        btc = {
            "status": "absent",
            "discovery_deferred": True,
            "reason": (
                "no Bitcoin funding outpoint is recorded and "
                "the Bitcoin funding window remains open"
            ),
        }

    else:
        btc = _reconcile_observe_htlc(swap, "btc")

    secret_value = None
    secret_source = "none"

    if role == "alice" and swap.secret_hex:
        secret_value = swap.secret_hex
        secret_source = "locally-generated"
    elif role == "bob" and swap.revealed_secret_hex:
        secret_value = swap.revealed_secret_hex
        secret_source = "recovered-from-bitcoin"

    secret_matches = None
    if secret_value:
        secret_matches = (
            hashlib.sha256(bytes.fromhex(secret_value)).hexdigest()
            == swap.secret_hash
        )
        if not secret_matches:
            raise RuntimeError(
                "SECURITY FAILURE: locally known secret does not match swap hash"
            )


    action, reason = _reconcile_next_action(
        swap,
        role,
        blake,
        btc,
        blake_height,
        btc_height,
    )



    readiness = _reconcile_execution_readiness(
        swap,
        role,
        action,
        blake_height,
        btc_height,
    )


    return {
        "swap_id": swap.swap_id,
        "participant_pubkey": me,
        "protocol_role": role,
        "counterparty_pubkey": counterparty,
        "checkpoint_state": swap.state,
        "decision_class": (
            "settled"
            if action == "none"
            else "waiting"
            if action == "wait"
            else "recovery-needed"
            if action in {"sync-funding", "sync-settlement"}
            else "manual-attention"
            if action == "manual-attention"
            else "actionable"
        ),
        "negotiation_status": negotiation_status,
        "negotiation_reason": negotiation_reason,
        "secret": {
            "known": bool(secret_value),
            "source": secret_source,
            "hash_matches": secret_matches,
        },
        "heights": {
            "blake": blake_height,
            "btc": btc_height,
        },
        "timeouts": {
            "blake": int(swap.blake_timeout),
            "btc": int(swap.btc_timeout),
            "blake_blocks_remaining": int(swap.blake_timeout) - blake_height,
            "btc_blocks_remaining": int(swap.btc_timeout) - btc_height,
        },
        "blake": blake,
        "btc": btc,
        "next_action": action,
        "reason": reason,
        "readiness": readiness,
        "read_only": True,
    }

def command_watch_htlc(args):
    """
    Record this swap's HTLCs in the watch-only wallets, and report them.

    Run automatically for each chain before that chain is funded; this is
    the explicit form, for a swap funded before the watch-only wallet
    existed, for a wallet that was recreated, or to check the answer
    without funding anything.

    A watch-only wallet is a second copy of what the swap *is*, not of the
    keys that spend it: it holds the same descriptor with every key public,
    so a later loss of the signing wallet leaves the outpoint observable
    and the recovery procedure in docs/KEY_RECOVERY.md workable.
    """
    import json

    swap = load_swap(args.swap_id)

    chains = [args.chain] if args.chain else ["blake", "btc"]

    results = []

    for chain in chains:
        fields = chain_dispatch(chain)

        print()
        print(f"{fields['name'].upper()} HTLC")
        print("=" * (len(fields["name"]) + 5))

        status = import_watch_only_htlc(swap, chain)
        solvable = htlc_solvability(swap, chain)

        results.append(
            {
                "chain": chain,
                "wallet": status["wallet"],
                "address": status["address"],
                "imported": status["imported"],
                "reason": status["reason"],
                "solvable": solvable["solvable"],
                "solvable_by": solvable["wallet"],
            }
        )

        if status["imported"]:
            print("wallet:  ", status["wallet"])
            print("address: ", status["address"])
            print("PASS: descriptor imported into the watch-only wallet")
        else:
            print("wallet:  ", status["wallet"] or "(none)")
            print("FAIL: not imported:", status["reason"])

        if solvable["solvable"]:
            print(
                "PASS: this HTLC is solvable by wallet "
                f"{solvable['wallet']!r}"
            )
        elif solvable["solvable"] is False:
            print(f"FAIL: not solvable: {solvable['reason']}")
        else:
            print(f"UNKNOWN: {solvable['reason']}")

    if getattr(args, "json", False):
        print(
            json.dumps(
                {
                    "swap_id": swap.swap_id,
                    "chains": results,
                },
                indent=2,
                sort_keys=True,
            )
        )
        return

    if not all(item["imported"] for item in results):
        raise RuntimeError(
            "at least one HTLC could not be recorded in a watch-only wallet"
        )


def command_recovery_plan(args):
    """
    How to recover an HTLC whose keys this wallet no longer holds.

    A wallet restore, rename or re-creation that loses the private keys
    behind an HTLC's addresses makes both the claim and the refund
    impossible, and nothing in the swap record says so. This command is
    read-only: it reports the exact descriptor, its checksum, the funding
    outpoint, which branch and key belong to this participant, and whether
    the wallet can currently spend it -- plus the import and spend steps
    if it cannot.

    The descriptor is printed in its public form. To import it into a
    signing wallet the operator substitutes their own key (WIF or extended
    private key) where their public key appears, then runs
    `getdescriptorinfo` again on that exact string, because substituting a
    key changes the checksum. `importdescriptors` requires the checksum of
    the string being imported and rejects a missing or stale one.
    """

    swap = load_swap(args.swap_id)

    try:
        local_role, _, _ = _reconcile_local_role(swap)
    except Exception as e:  # noqa: BLE001
        print()
        print("RECOVERY PLAN")
        print("=============")
        print("swap:", swap.swap_id)
        print()
        print(
            "This record does not pin enough identities to derive the "
            f"local protocol role ({type(e).__name__}: {e}), so the "
            "recovery plan cannot say which branch is yours."
        )
        return

    print()
    print("HTLC RECOVERY PLAN")
    print("==================")
    print("swap:  ", swap.swap_id)
    print("state: ", swap.state)
    print("role:  ", local_role)
    print()
    print(
        "Read-only. Nothing is imported, signed, funded or broadcast by "
        "this command."
    )

    for chain, chain_name, cli, cli_name, wallet in (
        ("blake", "BLAKE", blake_cli, "blake", BLAKE_WALLET),
        ("btc", "Bitcoin", btc_cli, "btc", BTC_WALLET),
    ):
        tx_field = f"{chain}_funding_txid"
        vout_field = f"{chain}_funding_vout"
        timeout_field = (
            "blake_timeout" if chain == "blake" else "btc_timeout"
        )

        print()
        print(chain_name.upper())
        print("-" * len(chain_name))

        try:
            body = htlc_descriptor_body(swap, chain)
        except Exception as e:  # noqa: BLE001
            print("not derivable:", f"{type(e).__name__}: {e}")
            continue

        try:
            checksum = descriptor_checksum(body, cli)
        except Exception as e:  # noqa: BLE001
            print("descriptor:", body)
            print(
                "checksum:  could not be obtained "
                f"({type(e).__name__}: {e})"
            )
            continue

        print("descriptor:", f"{body}#{checksum}")

        #
        # `deriveaddresses` requires the checksum, and the checksum of
        # this exact body is the one just obtained, so build the
        # checksummed form rather than relying on the node's canonical
        # rendering.
        #
        checksummed = f"{body}#{checksum}"

        try:
            address = (
                derive_blake_contract_address(checksummed)
                if chain == "blake"
                else derive_btc_contract_address(checksummed)
            )
        except Exception as e:  # noqa: BLE001
            print("address:   not derivable:", f"{type(e).__name__}: {e}")
            continue

        print("address:   ", address)

        txid = getattr(swap, tx_field)
        vout = getattr(swap, vout_field)

        print(
            "funding:   ",
            f"{txid}:{vout}" if txid else "not recorded",
        )

        #
        # Which branch this participant can take, and with which key.
        #
        if chain == "blake":
            claimer_pub = swap.blake_bob_pub
            refunder_pub = swap.blake_alice_pub
            claimer_role = "bob"
        else:
            claimer_pub = swap.btc_alice_pub
            refunder_pub = swap.btc_bob_pub
            claimer_role = "alice"

        if local_role == claimer_role:
            branch = "claim"
            own_key = claimer_pub
        else:
            branch = "refund"
            own_key = refunder_pub

        print(
            "your branch:",
            branch,
            f"(protocol {local_role})",
        )
        print("your key:   ", own_key)
        print(
            "timeout:    ",
            getattr(swap, timeout_field),
            "(the refund branch is spendable at or after this height)",
        )

        descriptor_known = None
        private_keys_enabled = None

        try:
            info = cli("getaddressinfo", address, wallet=wallet)
            descriptor_known = bool(
                isinstance(info, dict)
                and info.get("solvable")
            )
            print(
                "descriptor known:",
                "yes" if descriptor_known else "no",
                f"(wallet {wallet!r}, solvable={info.get('solvable') if isinstance(info, dict) else 'unknown'})",
            )
        except Exception as e:  # noqa: BLE001
            print(
                "descriptor known: unknown",
                f"({type(e).__name__}: {e})",
            )

        try:
            wallet_info = cli("getwalletinfo", wallet=wallet)
            if isinstance(wallet_info, dict):
                private_keys_enabled = bool(
                    wallet_info.get("private_keys_enabled")
                )

            if private_keys_enabled is None:
                print("private keys enabled: unknown")
            else:
                print(
                    "private keys enabled:",
                    "yes" if private_keys_enabled else "no",
                )
        except Exception as e:  # noqa: BLE001
            print(
                "private keys enabled: unknown",
                f"({type(e).__name__}: {e})",
            )

        #
        # `solvable` means the wallet knows enough of the descriptor/script
        # to construct the spend.  A watch-only wallet can therefore report
        # solvable=true while private_keys_enabled=false.  Do not describe
        # that condition as "can spend".
        #
        if descriptor_known and private_keys_enabled:
            print(
                "signing prerequisites: present "
                "(descriptor known and private keys enabled)"
            )
            print()
            print(
                "The signing wallet has the HTLC descriptor and private-key "
                "capability. Actual branch-key availability is proven only "
                "when the spend signs successfully."
            )
            print(f"  scripts/blake {branch}-{chain} {swap.swap_id}")
            continue

        print(
            "signing prerequisites: not established"
        )

        print()
        print("RECOVER IT")
        print("~~~~~~~~~~")
        print(
            "1. Re-import the descriptor with your own private key in "
            "place of your public key:"
        )
        print()
        print(f"     {cli_name}-cli -rpcwallet=<wallet> getdescriptorinfo \\")
        print(
            f"       '{htlc_descriptor_body(swap, chain, {own_key: '<YOUR-WIF-OR-XPRV>'})}'"
        )
        print()
        print(
            "   Substituting the key changes the descriptor string, so "
            "the checksum from this call is the one to use, not the one "
            "printed above."
        )
        print()
        print("2. Import it, with a timestamp that avoids a rescan:")
        print()
        print(f"     {cli_name}-cli -rpcwallet=<wallet> importdescriptors \\")
        print(
            '       \'[{"desc":"<descriptor>#<checksum>",'
            '"timestamp":"now"}]\''
        )
        print()
        print(
            "   `timestamp` is required. Use \"now\" for an HTLC that "
            "has not been funded yet; a numeric timestamp triggers a "
            "rescan from that time minus two hours, and a single "
            "`\"timestamp\": 0` in a batch forces a full rescan of the "
            "whole batch."
        )
        print()
        print("3. Verify, then spend:")
        print()
        print(f"     {cli_name}-cli -rpcwallet=<wallet> getaddressinfo {address}")
        print(
            "       # expect solvable=true (and ismine=true) before "
            "spending"
        )

        if branch == "refund":
            print()
            print(
                "   Refund. The refund branch signs with the wallet once "
                "the locktime is mature:"
            )
            print()
            print(
                f"     scripts/blake refund-{chain} {swap.swap_id}"
            )
            print(
                "       # the engine builds and signs it; the locktime "
                f"must be at or after height {getattr(swap, timeout_field)}"
            )
        else:
            print()
            print(
                "   Claim. A P2WSH claim cannot be signed from a raw "
                "transaction over RPC: the signing path needs the SHA256 "
                "preimage, which the node accepts only through a PSBT "
                "input field (PSBT_IN_SHA256). The engine already builds "
                "the claim that way:"
            )
            print()
            print(
                f"     scripts/blake claim-{chain} {swap.swap_id}"
            )
            print(
                "       # requires the secret S; without it this branch "
                "cannot be spent at all"
            )

    print()
    print(
        "PASS: recovery plan generated; nothing was imported, signed, "
        "funded or broadcast"
    )


def command_reconcile(args):
    """Read-only protocol brain: observe, derive, recommend; never act."""
    import json

    # role_dir() is participant-local, so protocol label does not choose
    # a different filesystem tree anymore.
    swap = load_swap(args.swap_id, role="alice")
    report = build_reconcile_report(swap)

    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
        return

    print()
    print("BLAKE SWAP RECONCILIATION")
    print("=========================")
    print("swap id:          ", report["swap_id"])
    print("participant:      ", report["participant_pubkey"])
    print("protocol role:    ", report["protocol_role"])
    print("counterparty:     ", report["counterparty_pubkey"])
    print("checkpoint state: ", report["checkpoint_state"])

    print()
    print("CHAIN REALITY")
    print("-------------")
    b = report["blake"]
    t = report["btc"]
    print("BLAKE:", b["status"] + (f" / {b['branch']}" if b.get("branch") else ""))
    print("BTC:  ", t["status"] + (f" / {t['branch']}" if t.get("branch") else ""))

    print()
    print("SECRET")
    print("------")
    print("known: ", "yes" if report["secret"]["known"] else "no")
    print("source:", report["secret"]["source"])

    timeouts = report.get("timeouts")

    if timeouts:
        print()
        print("TIMEOUT DISTANCE")
        print("----------------")
        print("BTC blocks:  ", timeouts["btc_blocks_remaining"])
        print("BLAKE blocks:", timeouts["blake_blocks_remaining"])
    else:
        print()
        print("TIMEOUT DISTANCE")
        print("----------------")
        print("not evaluated")

    readiness = report.get("readiness", {})

    if readiness.get("checked"):
        print()
        print("EXECUTION READINESS")
        print("-------------------")
        print(
            "ready:",
            "yes" if readiness.get("ready") else "no",
        )
        print("wallet:", readiness.get("wallet"))
        print("loaded:", readiness.get("wallet_loaded"))
        print(
            "trusted balance:",
            readiness.get("trusted_balance"),
            readiness.get("balance_unit", ""),
        )
        print(
            "required balance:",
            readiness.get("required_balance"),
            readiness.get("balance_unit", ""),
        )
        print(
            "fee reserve:",
            readiness.get("fee_reserve"),
            readiness.get("balance_unit", ""),
        )
        print(
            "timeout blocks:",
            readiness.get("timeout_blocks_remaining"),
        )

        for item in readiness.get("reasons", []):
            print(" -", item)

    print()
    print("NEXT SAFE ACTION")
    print("----------------")
    print(report["next_action"])
    print()
    print("reason:")
    print(report["reason"])

    print()
    print("PASS: reconciliation was read-only; no funds or messages were sent")


def _run_print_decision(report):
    print()
    print("BLAKE SWAP RUNNER")
    print("=================")
    print("swap id:         ", report.get("swap_id"))
    print("protocol role:   ", report.get("protocol_role"))
    print("checkpoint:      ", report.get("checkpoint_state"))
    print("decision class:  ", report.get("decision_class"))
    print("next action:     ", report.get("next_action"))
    print()
    print("reason:")
    print(report.get("reason"))



def ignored_match_ttl_seconds():
    """
    Lifetime of a locally ignored market match, in seconds.

    Zero (the default) means a tombstone never expires, which preserves
    the original behaviour. A positive value bounds how long an ignored
    match stays suppressed: an expired tombstone is both ignored and
    removed when the tombstones are next read.

    Override with BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS.
    """
    raw = os.environ.get(
        "BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS",
        "0",
    )

    try:
        value = int(raw)
    except (TypeError, ValueError):
        raise RuntimeError(
            "BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS must be an integer"
        ) from None

    if value < 0:
        raise RuntimeError(
            "BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS must not be negative"
        )

    return value


def _ignored_market_match_ids():
    import json
    import time

    ignore_dir = BASE_SWAP_DIR / "ignored-market-matches"

    if not ignore_dir.exists():
        return set()

    ttl = ignored_match_ttl_seconds()
    now = int(time.time())

    ignored = set()

    for path in ignore_dir.glob("*.json"):
        if len(path.stem) != 64:
            continue

        try:
            with open(path) as f:
                data = json.load(f)
        except Exception:
            continue

        match_id = data.get("match_id")

        if match_id != path.stem:
            continue

        if ttl > 0:
            ignored_at = data.get("ignored_at")

            if not isinstance(ignored_at, int):
                # An undated tombstone cannot be aged; treat it as
                # expired rather than suppressing a match forever.
                ignored_at = 0

            if now - ignored_at >= ttl:
                # Expired: stop suppressing it and reclaim the file.
                try:
                    path.unlink()
                except OSError:
                    pass

                continue

        ignored.add(match_id)

    return ignored


def command_ignore_match(args):
    import asyncio
    import json
    import time

    me = current_nostr_pubkey()
    requested = args.match_id.strip().lower()

    if len(requested) < 8:
        raise ValueError(
            "match ID prefix is too short; use at least 8 characters"
        )

    matches = asyncio.run(
        nostr_fetch_market_matches(
            me,
            limit=200,
        )
    )

    candidates = [
        item
        for item in matches
        if item["event_id"].startswith(requested)
    ]

    if not candidates:
        raise RuntimeError(
            f"No valid market match begins with {requested!r}"
        )

    if len(candidates) > 1:
        print()
        print("AMBIGUOUS MATCH PREFIX")
        print("======================")
        for item in candidates:
            print(
                item["event_id"],
                "swap=" + str(item.get("swap_id")),
            )
        raise RuntimeError(
            "Match prefix is ambiguous; use more characters"
        )

    item = candidates[0]
    match_id = item["event_id"]
    swap_id = item.get("swap_id")

    if (
        isinstance(swap_id, str)
        and len(swap_id) == 12
        and swap_path(swap_id).exists()
    ):
        raise RuntimeError(
            "Refusing to ignore: this match already has local swap state"
        )

    ignore_dir = BASE_SWAP_DIR / "ignored-market-matches"
    ignore_dir.mkdir(parents=True, exist_ok=True)

    path = ignore_dir / f"{match_id}.json"

    if path.exists():
        print()
        print("MATCH ALREADY IGNORED")
        print("=====================")
        print("match:", match_id)
        return

    record = {
        "match_id": match_id,
        "swap_id": swap_id,
        "offer_id": item.get("offer_id"),
        "take_id": item.get("take_id"),
        "maker": item.get("maker"),
        "taker": item.get("taker"),
        "direction": item.get("direction"),
        "blake_amount": item.get("blake_amount"),
        "btc_sats": item.get("btc_sats"),
        "ignored_at": int(time.time()),
        "local_only": True,
    }

    #
    # The tombstone is what stops this match being offered again, so it
    # has to outlive the process that wrote it.
    #
    atomic_write_json(
        path,
        record,
        fsync_directory=True,
    )

    print()
    print("MARKET MATCH IGNORED")
    print("====================")
    print("match:", match_id)
    print("swap: ", swap_id)
    print()
    print("PASS: local tombstone created")
    print("PASS: no Nostr event was deleted or modified")
    print("PASS: no swap state was created")
    print("PASS: no funds moved")


def command_next(args):
    import asyncio
    import contextlib
    import io
    import time
    from types import SimpleNamespace

    me = current_nostr_pubkey()
    actions = []

    freshness_seconds = int(
        os.environ.get(
            "BLAKE_SWAP_NEXT_FRESH_SECONDS",
            "3600",
        )
    )

    if freshness_seconds < 60:
        raise RuntimeError(
            "BLAKE_SWAP_NEXT_FRESH_SECONDS must be at least 60"
        )

    now = int(time.time())
    historical_matches = []

    # --------------------------------------------------------------
    # 0. A unique current market offer from another participant may
    #    be TAKEN automatically. Taking publishes only a signed
    #    coordination event and moves no funds.
    # --------------------------------------------------------------
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            open_offers = asyncio.run(
                nostr_fetch_market_offers(
                    limit=100,
                )
            )
    except Exception:
        open_offers = []

    eligible_offers = [
        offer
        for offer in open_offers
        if offer.get("maker") != me
    ]

    # Do not create another take for an offer we have already taken.
    for offer in eligible_offers:
        if nostr_prior_market_take(
            offer["event_id"],
            taker_pubkey=me,
        ) is not None:
            continue

        actions.append(
            {
                "kind": "take-offer",
                "offer_id": offer["event_id"],
                "label": (
                    f"Take open offer {offer['event_id'][:12]} "
                    f"for {offer['blake_amount']} BLAKE / "
                    f"{int(offer['btc_sats']):,} sats"
                ),
            }
        )

    # --------------------------------------------------------------
    # A. Authenticated Bob accept messages waiting to be imported.
    #    This is coordination only: no funds move.
    # --------------------------------------------------------------
    for path in local_swap_paths():

        swap = load_local_swap(path)

        if swap is None:
            continue

        if swap.state != State.OFFERED.value:
            continue

        if swap.nostr_alice_pub != me:
            continue

        if not swap.nostr_bob_pub:
            continue

        if swap.blake_bob_pub and swap.btc_bob_pub:
            # Already imported; this belongs in READY TO FUND below.
            continue

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                message, event_id, signer = asyncio.run(
                    nostr_fetch_message(
                        swap.swap_id,
                        "accept",
                        expected_pubkey=swap.nostr_bob_pub,
                    )
                )
        except Exception:
            continue

        if signer != swap.nostr_bob_pub:
            continue

        if message.get("swap_id") != swap.swap_id:
            continue

        if not message.get("blake_bob_pub"):
            continue

        if not message.get("btc_bob_pub"):
            continue

        actions.append(
            {
                "kind": "receive-accept",
                "swap_id": swap.swap_id,
                "event_id": event_id,
                "label": (
                    f"Import Bob acceptance for swap {swap.swap_id}"
                ),
            }
        )

    # --------------------------------------------------------------
    # B. Pending validated market matches for the current taker.
    #    Importing a match creates/binds local protocol state only.
    # --------------------------------------------------------------
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            matches = asyncio.run(
                nostr_fetch_market_matches(
                    me,
                    limit=200,
                    dependency_fetch_after=(
                        now - freshness_seconds
                    ),
                )
            )
    except Exception:
        matches = []

    ignored_match_ids = _ignored_market_match_ids()

    for item in matches:
        if item["event_id"] in ignored_match_ids:
            continue

        swap_id = item.get("swap_id")

        if (
            isinstance(swap_id, str)
            and len(swap_id) == 12
            and swap_path(swap_id).exists()
        ):
            continue

        age = now - int(item.get("created_at", 0))

        if age < 0:
            historical_matches.append(
                {
                    "item": item,
                    "age": age,
                    "reason": "future-dated",
                }
            )
            continue

        if age > freshness_seconds:
            historical_matches.append(
                {
                    "item": item,
                    "age": age,
                    "reason": "older-than-auto-window",
                }
            )
            continue

        actions.append(
            {
                "kind": "accept-match",
                "match_id": item["event_id"],
                "swap_id": swap_id,
                "direction": item.get("direction"),
                "maker": item.get("maker"),
                "taker": item.get("taker"),
                "label": (
                    f"Import fresh market match {item['event_id'][:12]} "
                    f"for swap {swap_id}"
                ),
            }
        )

    # --------------------------------------------------------------
    # B2. Reverse-direction maker may import protocol Alice's
    #     ordinary swap offer and publish Bob's acceptance.
    #
    # IMPORTANT: nostr_fetch_market_matches(me) is taker-oriented
    # because market matches are tagged #p with the taker identity.
    # The reverse maker therefore discovers this work from the
    # durable market selection it created when accepting the take.
    # --------------------------------------------------------------
    selection_dir = BASE_SWAP_DIR / "market-selections"

    if selection_dir.exists():
        for selection_path in selection_dir.glob("*.json"):
            if selection_path.name.startswith("offer-"):
                continue

            try:
                with open(selection_path) as f:
                    selection = json.load(f)
            except Exception:
                continue

            if (
                selection.get("direction")
                != MARKET_DIRECTION_BTC_FOR_BLAKE
            ):
                continue

            maker = selection.get("nostr_bob_pub")
            if maker is None:
                maker = selection.get("maker")

            if maker != me:
                continue

            swap_id = selection.get("swap_id")
            match_id = selection.get("market_match_id")
            expected_alice = selection.get("nostr_alice_pub")
            if expected_alice is None:
                expected_alice = selection.get("taker")

            if not (
                isinstance(swap_id, str)
                and len(swap_id) == 12
                and isinstance(match_id, str)
                and len(match_id) == 64
                and isinstance(expected_alice, str)
                and len(expected_alice) == 64
            ):
                continue

            if swap_id_is_locally_retired(swap_id):
                continue

            # Durable selections survive restarts, but they must not
            # resurrect ancient market negotiations into today's
            # automatic work queue. Re-open and verify the exact
            # referenced match, then enforce the same 60-minute
            # automatic-coordination window used elsewhere by next.
            try:
                # Durable selections reference immutable signed match
                # events by exact ID. Prefer the verified local cache;
                # use the relays only when that exact event is absent.
                match_event = nostr_load_cached_event(
                    match_id
                )

                if match_event is None:
                    with contextlib.redirect_stdout(io.StringIO()):
                        match_event = asyncio.run(
                            nostr_fetch_event_by_id(
                                match_id
                            )
                        )

                verify_nostr_event(match_event)

                match_message = json.loads(
                    match_event["content"]
                )

                if (
                    match_message.get("type") != "market-match"
                    or match_message.get("direction")
                    != MARKET_DIRECTION_BTC_FOR_BLAKE
                    or match_message.get("swap_id") != swap_id
                    or match_message.get("maker") != me
                    or match_message.get("taker")
                    != expected_alice
                ):
                    continue

                match_created_at = int(
                    match_message.get(
                        "created_at",
                        match_event.get("created_at", 0),
                    )
                )

                now = int(time.time())

                if (
                    match_created_at <= 0
                    or match_created_at > now + FUTURE_SKEW_SECONDS
                    or now - match_created_at > freshness_seconds
                ):
                    continue

            except Exception:
                continue

            existing_complete = False

            for candidate_role in ("alice", "bob"):
                try:
                    existing = load_swap(
                        swap_id,
                        role=candidate_role,
                    )
                except Exception:
                    continue

                try:
                    derived_role, _, _ = _reconcile_local_role(
                        existing
                    )
                except Exception:
                    continue

                if (
                    derived_role == "bob"
                    and existing.nostr_bob_pub == me
                    and existing.blake_bob_pub
                    and existing.btc_bob_pub
                ):
                    existing_complete = True
                    break

            if existing_complete:
                continue

            # Do not surface this coordination step until protocol
            # Alice's ordinary swap offer is actually available.
            try:
                with contextlib.redirect_stdout(io.StringIO()):
                    asyncio.run(
                        nostr_fetch_message(
                            swap_id,
                            "offer",
                            expected_pubkey=expected_alice,
                        )
                    )
            except Exception:
                continue

            actions.append(
                {
                    "kind": "accept-reverse-offer",
                    "match_id": match_id,
                    "swap_id": swap_id,
                    "label": (
                        f"Accept reverse swap offer for "
                        f"{swap_id} from match "
                        f"{match_id[:12]}"
                    ),
                }
            )

    # --------------------------------------------------------------
    # C. Valid takes on the current participant's still-open offers.
    #    Accepting a take binds negotiation but moves no funds.
    # --------------------------------------------------------------
    # Reuse the market snapshot already fetched in section A.
    # `next` is one coordination pass; fetching the same open-offer
    # view twice only adds relay and event-loop overhead.
    offers = open_offers

    for offer in offers:
        if offer.get("maker") != me:
            continue

        offer_id = offer.get("event_id")
        if not isinstance(offer_id, str):
            continue

        selection_path = (
            BASE_SWAP_DIR
            / "market-selections"
            / f"offer-{offer_id}.json"
        )

        if selection_path.exists():
            continue

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                (
                    original,
                    offer_message,
                    valid_takes,
                ) = asyncio.run(
                    nostr_fetch_market_takes(
                        offer_id,
                        limit=200,
                    )
                )
        except Exception:
            continue

        for take in valid_takes:
            actions.append(
                {
                    "kind": "accept-take",
                    "offer_id": offer_id,
                    "take_id": take["event_id"],
                    "label": (
                        f"Accept take {take['event_id'][:12]} "
                        f"on offer {offer_id[:12]}"
                    ),
                }
            )

    # --------------------------------------------------------------
    # If there is exactly one coordination action, perform it.
    # --------------------------------------------------------------
    if len(actions) == 1:
        action = actions[0]

        print()
        print("NEXT SAFE STEP")
        print("==============")
        print(action["label"])
        print()
        print(
            "PASS: this coordination step does not itself "
            "fund, claim, or refund an HTLC"
        )
        print()

        if action["kind"] == "receive-accept":
            command_receive_accept(
                SimpleNamespace(
                    swap_id=action["swap_id"]
                )
            )

        elif action["kind"] == "accept-match":
            command_accept_only_match(
                SimpleNamespace(
                    match_id=action["match_id"]
                )
            )

            if (
                action.get("direction")
                == MARKET_DIRECTION_BTC_FOR_BLAKE
            ):
                swap_id = action["swap_id"]
                offer_exists = False

                try:
                    with contextlib.redirect_stdout(io.StringIO()):
                        asyncio.run(
                            nostr_fetch_message(
                                swap_id,
                                "offer",
                                expected_pubkey=me,
                            )
                        )
                    offer_exists = True
                except Exception:
                    pass

                if not offer_exists:
                    print()
                    print("Publishing reverse protocol offer...")
                    command_publish_offer(
                        SimpleNamespace(
                            swap_id=swap_id
                        )
                    )

        elif action["kind"] == "accept-reverse-offer":
            command_accept_reverse_swap_offer(
                SimpleNamespace(
                    match_id=action["match_id"]
                )
            )

        elif action["kind"] == "accept-take":
            command_accept_market_take(
                SimpleNamespace(
                    take_id=action["take_id"]
                )
            )

        elif action["kind"] == "take-offer":
            command_take_market_offer(
                SimpleNamespace(
                    offer_id=action["offer_id"]
                )
            )

        else:
            raise RuntimeError(
                f"Unknown next-step action: {action['kind']}"
            )

        print()
        print("PASS: coordination step completed")
        print()
        print(
            "Run `blake next` again to see what is safe now."
        )
        return

    if len(actions) > 1:
        print()
        print("MULTIPLE SAFE COORDINATION STEPS")
        print("================================")
        print(
            "More than one authenticated negotiation needs "
            "attention. Refusing to choose automatically."
        )
        print()

        for index, action in enumerate(actions, start=1):
            print(f"{index}. {action['label']}")

        print()
        print("Resolve the intended trade explicitly:")
        print("  take <offer-prefix>")
        print("  accept-take <offer-prefix>")
        print("  accept-match <match-prefix>")
        print("  engine accept-reverse-swap-offer <match-id>")
        print()
        print(
            "PASS: no funds moved and no ambiguous choice was made"
        )
        return

    # --------------------------------------------------------------
    # D. Prioritize live post-funding work. `next` does not
    #    execute settlement economics; the existing resume policy does.
    # --------------------------------------------------------------
    resumable = []

    for path in local_swap_paths():

        swap = load_local_swap(path)

        if swap is None:
            continue

        if me not in {
            swap.nostr_alice_pub,
            swap.nostr_bob_pub,
        }:
            continue

        # Fast-path terminal pre-BTC refunds for the work queue only.
        #
        # `build_reconcile_report()` remains the full forensic view and
        # still independently evaluates both chains.  But `next` only
        # needs to know whether post-funding work is available.
        #
        # While the exact final BLAKE refund remains confirmed, a
        # BLAKE_REFUNDED swap cannot have resumable settlement work.
        # If that terminal evidence is missing or ambiguous, fall
        # through to full reconciliation so reorg recovery remains
        # conservative.
        if swap.state == State.BLAKE_REFUNDED.value:
            try:
                with contextlib.redirect_stdout(io.StringIO()):
                    terminal_blake = _reconcile_observe_htlc(
                        swap,
                        "blake",
                    )
            except Exception:
                terminal_blake = None

            if (
                isinstance(terminal_blake, dict)
                and terminal_blake.get("status") == "spent"
                and terminal_blake.get("branch") == "refund"
                and terminal_blake.get("spend_txid")
                    == swap.blake_refund_txid
            ):
                continue

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                report = build_reconcile_report(swap)
        except Exception:
            continue

        if report.get("decision_class") in {
            "settled",
            "manual-attention",
        }:
            continue

        # `next` must never classify a normal unfunded OFFERED negotiation
        # as post-funding work.
        #
        # The one safe OFFERED exception is reconciliation of funding that
        # is already confirmed on-chain. `build_reconcile_report()` reports
        # that condition as recovery-needed -> sync-funding.
        if swap.state == State.OFFERED.value:
            if not (
                report.get("decision_class") == "recovery-needed"
                and report.get("next_action") == "sync-funding"
            ):
                continue

        resumable.append(
            {
                "swap": swap,
                "report": report,
            }
        )

    if len(resumable) == 1:
        item = resumable[0]
        swap = item["swap"]
        report = item["report"]

        print()
        print("SETTLEMENT WORK AVAILABLE")
        print("=========================")
        print("swap: ", swap.swap_id)
        print("state:", swap.state)
        print("next: ", report.get("next_action"))
        print("why:  ", report.get("reason"))
        print()
        print(
            "On-chain funding or settlement evidence exists. "
            "Use the existing autonomous recovery/settlement policy:"
        )
        print("  scripts/blake resume", swap.swap_id)
        print()
        print("PASS: next did not execute settlement economics")
        return

    if len(resumable) > 1:
        print()
        print("MULTIPLE ACTIVE SWAPS")
        print("=====================")

        for item in resumable:
            print(
                f"{item['swap'].swap_id}  "
                f"{item['swap'].state}  "
                f"{item['report'].get('next_action')}"
            )

        print()
        print(
            "Use `scripts/blake swaps` or "
            "`scripts/blake resume <swap-prefix>`."
        )
        return

    # --------------------------------------------------------------
    # E. No post-funding work exists. Check whether an Alice swap
    #    is now fully authenticated and ready for explicit funding.
    # --------------------------------------------------------------
    funding_ready = []

    for path in local_swap_paths():

        swap = load_local_swap(path)

        if swap is None:
            continue

        if swap.state != State.OFFERED.value:
            continue

        try:
            protocol_role, _, _ = _reconcile_local_role(swap)
        except Exception:
            continue

        # BLAKE funding is always the protocol-Alice commitment,
        # regardless of which local participant profile created or
        # imported the swap.
        if protocol_role != "alice":
            continue

        if not (
            swap.nostr_bob_pub
            and swap.blake_bob_pub
            and swap.btc_bob_pub
        ):
            continue

        funding_ready.append(swap)

    if len(funding_ready) == 1:
        swap = funding_ready[0]

        print()
        print("READY TO FUND")
        print("=============")
        print("swap: ", swap.swap_id)
        print("trade:", swap.blake_amount, "BLAKE /", f"{swap.btc_sats:,}", "sats")
        print()
        print(
            "All authenticated negotiation and settlement "
            "keys are in place."
        )
        print()
        print(
            "STOP: funding is an explicit economic commitment."
        )
        print("Run:")
        print("  scripts/blake fund")
        print()
        print("PASS: next did not move funds")
        return

    if len(funding_ready) > 1:
        print()
        print("MULTIPLE FUNDING-READY SWAPS")
        print("============================")
        print(
            "More than one trade is ready for an explicit "
            "BLAKE commitment. Refusing to choose."
        )
        print()

        for swap in funding_ready:
            print(
                f"{swap.swap_id}  "
                f"{swap.blake_amount} BLAKE / "
                f"{swap.btc_sats:,} sats"
            )

        print()
        print(
            "Use `fund-blake <swap-id>` for an explicit choice."
        )
        print("PASS: no funds moved")
        return

    print()
    print("NOTHING TO DO")
    print("=============")
    print(
        "No unambiguous current coordination, funding-ready trade, "
        "or resumable settlement was found."
    )

    if historical_matches:
        print()
        print("HISTORICAL MATCHES IGNORED")
        print("==========================")
        print(
            f"{len(historical_matches)} valid unimported match(es) "
            f"were outside the automatic {freshness_seconds // 60}-minute window."
        )
        print(
            "They remain available explicitly with "
            "`scripts/blake accept-match <prefix>`."
        )

    print()
    print(
        "Use `scripts/blake market` to browse offers or "
        "`scripts/blake swaps` for the dashboard."
    )


def dashboard_solvability(items):
    """
    Per-chain HTLC solvability for the swaps that still need something.

    Settled swaps are not asked about: their HTLC is spent, so the answer
    would cost two RPCs per chain and mean nothing. A swap with no HTLC
    yet -- a placeholder offer, say -- reports `solvable: None` rather
    than a warning, because it has not got one to solve.
    """
    status = {}

    for item in items:
        swap = item["swap"]
        per_chain = {}

        for chain in ("blake", "btc"):
            try:
                per_chain[chain] = htlc_solvability(swap, chain)
            except Exception as e:
                per_chain[chain] = {
                    "chain": chain,
                    "address": None,
                    "wallet": None,
                    "solvable": None,
                    "reason": f"{type(e).__name__}: {e}",
                }

        status[swap.swap_id] = per_chain

    return status


def command_dashboard(args):
    import contextlib
    import io
    import json

    me = current_nostr_pubkey()

    active = []
    attention = []
    settled = []
    archived = []

    def outcome_label(swap, report):
        decision = report.get("decision_class")
        action = report.get("next_action")

        if decision == "settled":
            if swap.state == State.COMPLETE.value:
                return "COMPLETED"
            if swap.state in {
                State.BTC_REFUNDED.value,
                State.BLAKE_REFUNDED.value,
            }:
                return "REFUNDED"
            return "SETTLED"

        if decision == "manual-attention":
            return "NEEDS ATTENTION"

        if action == "wait":
            if swap.state == State.BLAKE_FUNDED.value:
                return "WAITING FOR BTC"
            if swap.state == State.BTC_FUNDED.value:
                return "WAITING FOR ALICE"
            if swap.state == State.BTC_CLAIMED.value:
                return "WAITING FOR BOB"
            if swap.state == State.BTC_REFUNDED.value:
                return "WAITING FOR BLAKE REFUND"
            return "WAITING"

        if action in {
            "sync-funding",
            "sync-settlement",
            "complete",
        }:
            return "READY TO RESUME"
        if action == "fund-btc":
            return "READY TO FUND BTC"
        if action == "claim-btc":
            return "READY TO CLAIM BTC"
        if action == "claim-blake":
            return "READY TO CLAIM BLAKE"
        if action == "refund-btc":
            return "READY TO REFUND BTC"
        if action == "refund-blake":
            return "READY TO REFUND BLAKE"
        if action == "fund-blake":
            return "READY TO FUND BLAKE"

        return str(action or decision or swap.state)

    def action_text(report):
        action = report.get("next_action")
        mapping = {
            "sync-funding": "run resume",
            "sync-settlement": "run resume",
            "complete": "run resume",
            "fund-btc": "run resume",
            "claim-btc": "run resume",
            "claim-blake": "run resume",
            "refund-btc": "run resume",
            "refund-blake": "run resume",
            "fund-blake": "run fund",
            "wait": "wait",
            "manual-attention": "inspect",
            "none": "-",
            None: "-",
        }
        return mapping.get(action, str(action))

    for path in local_swap_paths():

        swap = load_local_swap(path)

        if swap is None:
            continue

        if me not in {
            swap.nostr_alice_pub,
            swap.nostr_bob_pub,
        }:
            continue

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                report = build_reconcile_report(swap)
        except Exception:
            attention.append(
                {
                    "swap": swap,
                    "report": {
                        "decision_class": "manual-attention",
                        "next_action": "manual-attention",
                        "reason": "reconciliation failed during dashboard scan",
                    },
                }
            )
            continue

        item = {"swap": swap, "report": report}
        decision_class = report.get("decision_class")

        if decision_class == "settled":
            settled.append(item)
        elif decision_class == "manual-attention":
            attention.append(item)
        else:
            active.append(item)

    archive_dir = BASE_SWAP_DIR / "archive"

    if archive_dir.exists():
        for path in local_swap_paths(archive_dir):

            try:
                with open(path) as f:
                    data = json.load(f)
            except Exception:
                continue

            archived.append(
                {
                    "swap_id": data.get("swap_id", path.stem),
                    "state": data.get("state", "-"),
                    "blake_amount": data.get("blake_amount"),
                    "btc_sats": data.get("btc_sats"),
                }
            )

    def trade_text(swap):
        try:
            return (
                f"{swap.blake_amount:g} BLAKE / "
                f"{int(swap.btc_sats):,} sats"
            )
        except Exception:
            return "-"

    def swap_json_item(item):
        swap = item["swap"]
        report = item["report"]

        blake_report = report.get("blake", {})
        btc_report = report.get("btc", {})

        return {
            "swap_id": swap.swap_id,
            "state": swap.state,
            "protocol_role": report.get("protocol_role"),
            "decision_class": report.get("decision_class"),
            "next_action": report.get("next_action"),
            "reason": report.get("reason"),
            "status": outcome_label(swap, report),
            "action": action_text(report),
            "trade": {
                #
                # `blake_amount` is the display value and has always been
                # a JSON number. `blake_amount_base_units` is the exact
                # integer the state file and the wire carry, added
                # alongside rather than in place of it so no existing
                # consumer's type changes.
                #
                "blake_amount": swap.blake_amount,
                "blake_amount_base_units": (
                    swap.blake_amount_base_units
                ),
                "btc_sats": swap.btc_sats,
            },
            "chains": {
                "blake_status": blake_report.get("status"),
                "btc_status": btc_report.get("status"),
                #
                # Whether a loaded wallet can solve the HTLC: True,
                # False, or None when there is no HTLC yet or no chain to
                # ask. A participant who cannot solve a funded HTLC is the
                # condition docs/KEY_RECOVERY.md exists for.
                #
                "blake_solvable": (
                    solvability.get(swap.swap_id, {})
                    .get("blake", {})
                    .get("solvable")
                ),
                "btc_solvable": (
                    solvability.get(swap.swap_id, {})
                    .get("btc", {})
                    .get("solvable")
                ),
            },
            "transactions": {
                "blake_funding_txid": swap.blake_funding_txid,
                "btc_funding_txid": swap.btc_funding_txid,
                "btc_claim_txid": swap.btc_claim_txid,
                "blake_claim_txid": swap.blake_claim_txid,
                "btc_refund_txid": swap.btc_refund_txid,
                "blake_refund_txid": swap.blake_refund_txid,
            },
        }

    #
    # Only the swaps that still need something are asked about, and only
    # once: the answer is used by both the JSON view and the human one.
    #
    solvability = dashboard_solvability(active + attention)

    if getattr(args, "json", False):
        result = {
            "version": 1,
            "active": [
                swap_json_item(item)
                for item in active
            ],
            "attention": [
                swap_json_item(item)
                for item in attention
            ],
            "settled": [
                swap_json_item(item)
                for item in settled
            ],
            "archived": archived,
            "summary": {
                "active": len(active),
                "attention": len(attention),
                "settled": len(settled),
                "archived": len(archived),
            },
        }

        print(
            json.dumps(
                result,
                indent=2,
                sort_keys=True,
            )
        )
        return

    print()
    print("BLAKE SWAP DASHBOARD")
    print("====================")

    print()
    print("ACTIVE")
    print("------")

    if not active:
        print("No active swaps.")
    else:
        print("Swap          Status                    Action       Trade")
        print("------------  ------------------------  -----------  -------------------------")
        for item in active:
            swap = item["swap"]
            report = item["report"]
            print(
                f"{swap.swap_id:<12}  "
                f"{outcome_label(swap, report):<24}  "
                f"{action_text(report):<11}  "
                f"{trade_text(swap)}"
            )

    print()
    print("NEEDS ATTENTION")
    print("---------------")

    if not attention:
        print("No swaps need manual attention.")
    else:
        for item in attention:
            swap = item["swap"]
            report = item["report"]
            print(
                f"{swap.swap_id}  "
                f"{outcome_label(swap, report)}  "
                f"{report.get('reason')}"
            )

    print()
    print("SETTLED")
    print("-------")

    if not settled:
        print("No settled swaps.")
    else:
        print("Swap          Outcome     Trade")
        print("------------  ----------  -------------------------")
        for item in settled:
            swap = item["swap"]
            report = item["report"]
            print(
                f"{swap.swap_id:<12}  "
                f"{outcome_label(swap, report):<10}  "
                f"{trade_text(swap)}"
            )

    print()
    print("ARCHIVED")
    print("--------")

    if not archived:
        print("No archived swaps.")
    else:
        for item in archived:
            trade = "-"
            if (
                item.get("blake_amount") is not None
                and item.get("btc_sats") is not None
            ):
                try:
                    trade = (
                        f"{float(item['blake_amount']):g} BLAKE / "
                        f"{int(item['btc_sats']):,} sats"
                    )
                except Exception:
                    pass

            print(
                f"{item['swap_id']}  "
                f"ABANDONED  "
                f"{trade}"
            )

    print()
    print("SUMMARY")
    print("-------")
    print("active:    ", len(active))
    print("attention: ", len(attention))
    print("settled:   ", len(settled))
    print("archived:  ", len(archived))

    #
    # Only printed when there is something to say, so the usual output is
    # unchanged. A live swap whose HTLC no loaded wallet can solve is the
    # failure docs/KEY_RECOVERY.md exists for, and it is silent otherwise:
    # the swap looks normal in every other line above.
    #
    unsolvable = [
        (item["swap"].swap_id, chain, status)
        for item in active + attention
        for chain, status in solvability.get(
            item["swap"].swap_id, {}
        ).items()
        if status["solvable"] is False
    ]

    if unsolvable:
        print()
        print("HTLCs NOT SOLVABLE BY A LOADED WALLET")
        print("=====================================")

        for swap_id, chain, status in unsolvable:
            print(f"{swap_id}  {chain:<5}  {status['reason']}")

        print()
        print(
            "Record the descriptor with "
            "`scripts/blake engine watch-htlc <swap-id>`, or follow"
        )
        print("docs/KEY_RECOVERY.md if the signing keys are already gone.")



def command_dismiss(args):
    import contextlib
    import io
    import json

    prefix = args.swap_id.strip().lower()

    if len(prefix) < 4:
        raise ValueError(
            "swap ID prefix is too short; use at least 4 characters"
        )

    matches = []

    for path in local_swap_paths():
        if path.stem.startswith(prefix):
            matches.append(path)

    if not matches:
        raise RuntimeError(
            f"No local swap matches prefix {prefix!r}"
        )

    if len(matches) > 1:
        print()
        print("AMBIGUOUS SWAP PREFIX")
        print("=====================")
        for path in matches:
            print(path.stem)
        raise RuntimeError(
            "Swap prefix is ambiguous; use more characters"
        )

    path = matches[0]

    swap = load_swap(path.stem)

    #
    # Dismissal archives a local record. Refuse to archive a swap this
    # participant is not a party to, so a shared or mis-pointed state
    # root cannot be used to retire somebody else's swap.
    #
    me = current_nostr_pubkey()

    if me not in (
        swap.nostr_alice_pub,
        swap.nostr_bob_pub,
    ):
        raise RuntimeError(
            "SECURITY FAILURE: refusing to dismiss a swap this "
            "participant is not a party to"
        )

    try:
        with contextlib.redirect_stdout(io.StringIO()):
            report = build_reconcile_report(swap)
    except Exception as e:
        raise RuntimeError(
            "Cannot safely dismiss because read-only reconciliation failed"
        ) from e

    blake = report.get("blake", {})
    btc = report.get("btc", {})

    blake_status = blake.get("status")
    btc_status = btc.get("status")

    negotiation_incomplete_unfundable = (
        swap.state == State.OFFERED.value
        and report.get("negotiation_status") == "negotiation-incomplete"
        and report.get("negotiation_reason")
            == "protocol Bob settlement pubkeys were never created or imported"
        and not getattr(swap, "blake_bob_pub", None)
        and not getattr(swap, "btc_bob_pub", None)
    )

    always_unsafe = {
        "unspent",
        "spent",
        "unknown",
    }

    if blake_status in always_unsafe:
        raise RuntimeError(
            f"Refusing to dismiss: BLAKE status is {blake_status!r}"
        )

    if btc_status in always_unsafe:
        raise RuntimeError(
            f"Refusing to dismiss: Bitcoin status is {btc_status!r}"
        )

    allowed_statuses = {"absent", None}

    if negotiation_incomplete_unfundable:
        allowed_statuses = allowed_statuses | {"not-evaluated"}

    if blake_status not in allowed_statuses:
        raise RuntimeError(
            f"Refusing to dismiss: BLAKE status is {blake_status!r}"
        )

    if btc_status not in allowed_statuses:
        raise RuntimeError(
            f"Refusing to dismiss: Bitcoin status is {btc_status!r}"
        )

    settlement_fields = [
        "blake_funding_txid",
        "btc_funding_txid",
        "btc_claim_txid",
        "blake_claim_txid",
        "btc_refund_txid",
        "blake_refund_txid",
    ]

    populated = [
        name
        for name in settlement_fields
        if getattr(swap, name, None)
    ]

    if populated:
        raise RuntimeError(
            "Refusing to dismiss: durable settlement/funding fields exist: "
            + ", ".join(populated)
        )

    archive_dir = BASE_SWAP_DIR / "archive"
    archive_dir.mkdir(
        parents=True,
        exist_ok=True,
    )

    archive_path = archive_dir / path.name

    if archive_path.exists():
        raise RuntimeError(
            f"Archive already contains {archive_path.name}"
        )

    print()
    print("SAFE TO ARCHIVE")
    print("===============")
    print("swap:   ", swap.swap_id)
    print("state:  ", swap.state)
    print("BLAKE:  ", blake_status or "absent")
    print("BTC:    ", btc_status or "absent")
    print("reason: ", report.get("reason"))
    print()
    if negotiation_incomplete_unfundable:
        print(
            "PASS: negotiation never became settlement-capable"
        )
        print(
            "PASS: missing Bob settlement pubkeys make HTLC evaluation inapplicable"
        )
    else:
        print(
            "PASS: no funded HTLCs were found"
        )
    print(
        "PASS: no settlement transaction is recorded"
    )
    print(
        "PASS: archive is local-only; nothing is published or spent"
    )

    shutil.move(
        str(path),
        str(archive_path),
    )

    print()
    print("SWAP ARCHIVED")
    print("=============")
    print("from:", path)
    print("to:  ", archive_path)
    print()
    print(
        "PASS: original JSON preserved intact"
    )



def command_resume_all(args):
    """
    Service-mode settlement supervisor.

    Each local swap is classified independently using the existing
    reconciliation report. Safe automatic paths are driven explicitly
    by swap ID, so unrelated live swaps never compete with each other.

    Initial BLAKE funding is not performed here.
    """
    import json as _json
    from types import SimpleNamespace

    me = current_nostr_pubkey()

    candidates = []
    attention = []

    for path in local_swap_paths():
        #
        # Only canonical local swap records reach this loop: a 12-hex
        # stem that names the swap. Anything else in the state root is
        # not ours to drive.
        #
        stem = path.stem

        try:
            with open(path) as f:
                data = _json.load(f)
        except Exception as e:
            attention.append(
                (path.name, f"unreadable state: {type(e).__name__}: {e}")
            )
            continue

        if not isinstance(data, dict):
            attention.append((stem, "state file is not an object"))
            continue

        swap_id = data.get("swap_id")

        if swap_id != stem:
            attention.append(
                (stem, "state file name does not match its swap_id")
            )
            continue

        #
        # Never drive a swap this participant is not a party to. The
        # record's pinned Nostr identities are the authority; the local
        # protocol role is derived from them elsewhere.
        #
        if me not in (
            data.get("nostr_alice_pub"),
            data.get("nostr_bob_pub"),
        ):
            continue

        try:
            swap = load_swap(swap_id)
            report = build_reconcile_report(swap)
        except Exception as e:
            attention.append(
                (swap_id, f"{type(e).__name__}: {e}")
            )
            continue

        decision = report.get("decision_class")
        action = report.get("next_action")

        if decision == "settled":
            continue

        if decision == "manual-attention":
            attention.append(
                (swap_id, report.get("reason", "manual attention required"))
            )
            continue

        if action == "fund-blake":
            # Initial BLAKE funding is an explicit human commitment.
            # Do not send this swap through the autonomous settlement
            # driver merely to have it refuse the action again.
            continue

        if decision in {"recovery-needed", "actionable"}:
            candidates.append((swap_id, decision, action))
            continue

    print()
    print("MULTI-SWAP SETTLEMENT PASS")
    print("==========================")

    if not candidates:
        print("No automatic settlement/recovery actions are ready.")
    else:
        for swap_id, decision, action in candidates:
            print()
            print(
                f"Driving {swap_id}: "
                f"{decision} / {action}"
            )

            try:
                command_resume(
                    SimpleNamespace(
                        swap_id=swap_id,
                    )
                )
            except Exception as e:
                print()
                print("SWAP PAUSED")
                print("===========")
                print("swap: ", swap_id)
                print(
                    "reason:",
                    f"{type(e).__name__}: {e}",
                )
                attention.append(
                    (
                        swap_id,
                        f"{type(e).__name__}: {e}",
                    )
                )

    if attention:
        print()
        print("ATTENTION")
        print("---------")
        for swap_id, reason in attention:
            print(f"{swap_id}: {reason}")
        print()
        print(
            "PASS: attention on one swap did not block "
            "independent safe swaps"
        )

    print()
    print("PASS: initial BLAKE funding was not performed")


def command_resume(args):
    import contextlib
    import io
    from types import SimpleNamespace

    me = current_nostr_pubkey()
    resumable = []
    attention = []
    explicit = getattr(args, "swap_id", None)

    #
    # Validate the prefix before scanning. Doing it inside the loop made
    # the guard dependent on there being at least one swap file, so with
    # an empty state root a three-character prefix was reported as "no
    # swap matches" instead of "prefix is too short".
    #
    requested_prefix = None

    if explicit is not None:
        requested_prefix = explicit.strip().lower()

        if len(requested_prefix) < 4:
            raise ValueError(
                "swap ID prefix is too short; use at least 4 characters"
            )

    for path in local_swap_paths():

        if (
            requested_prefix is not None
            and not path.stem.startswith(requested_prefix)
        ):
            continue

        swap = load_local_swap(path)

        if swap is None:
            continue

        if me not in {
            swap.nostr_alice_pub,
            swap.nostr_bob_pub,
        }:
            continue

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                report = build_reconcile_report(swap)
        except Exception:
            attention.append(
                {
                    "swap": swap,
                    "decision_class": "manual-attention",
                    "next_action": "manual-attention",
                    "reason": "reconciliation failed during discovery",
                }
            )
            continue

        decision_class = report.get("decision_class")
        if decision_class == "settled":
            continue

        item = {
            "swap": swap,
            "decision_class": decision_class,
            "next_action": report.get("next_action"),
            "reason": report.get("reason"),
        }

        if decision_class == "manual-attention":
            attention.append(item)
        else:
            resumable.append(item)

    if explicit is not None:
        all_matches = resumable + attention

        if not all_matches:
            #
            # A terminal swap is deliberately excluded from the live
            # resume queue, but an explicit prefix should still return
            # a friendly terminal result.
            #
            terminal_matches = []

            for path in local_swap_paths():
                try:
                    with open(path) as f:
                        data = json.load(f)
                except Exception:
                    continue

                swap_id = data.get("swap_id")
                state = data.get("state")

                if (
                    isinstance(swap_id, str)
                    and requested_prefix is not None
                    and swap_id.startswith(requested_prefix)
                    and state in (
                        State.COMPLETE.value,
                        State.BLAKE_REFUNDED.value,
                    )
                ):
                    terminal_matches.append((swap_id, state))

            if len(terminal_matches) == 1:
                swap_id, state = terminal_matches[0]

                if state == State.COMPLETE.value:
                    print()
                    print("SWAP ALREADY SETTLED")
                    print("====================")
                    print("swap: ", swap_id)
                    print("state:", state)
                    print()
                    print("No action required.")
                    return

                if state == State.BLAKE_REFUNDED.value:
                    print()
                    print("SWAP ALREADY REFUNDED")
                    print("=====================")
                    print("swap: ", swap_id)
                    print("state:", state)
                    print()
                    print("No action required.")
                    return

            if len(terminal_matches) > 1:
                raise RuntimeError(
                    f"More than one terminal local swap matches "
                    f"prefix {requested_prefix!r}; use a longer prefix"
                )

            raise RuntimeError(
                f"No live local swap matches prefix {requested_prefix!r}"
            )

        if len(all_matches) > 1:
            print()
            print("AMBIGUOUS SWAP PREFIX")
            print("=====================")
            for item in all_matches:
                print(
                    item["swap"].swap_id,
                    item["decision_class"],
                    item["next_action"],
                )
            raise RuntimeError(
                "Swap prefix is ambiguous; use more characters"
            )

        item = all_matches[0]

        if item["decision_class"] == "manual-attention":
            print()
            print("MANUAL ATTENTION REQUIRED")
            print("=========================")
            print("swap:  ", item["swap"].swap_id)
            print("state: ", item["swap"].state)
            print("reason:", item["reason"])
            print()
            print(
                "Refusing to hand a manual-attention swap "
                "to the autonomous runner."
            )
            return

        resumable = [item]
        attention = []

    if not resumable:
        print()
        print("NO RESUMABLE SWAPS")
        print("==================")
        print(
            "No swap currently has an automatic recovery, "
            "settlement, or waiting path."
        )

        if attention:
            print()
            print("NEEDS MANUAL ATTENTION")
            print("======================")
            for item in attention:
                print(
                    f"{item['swap'].swap_id}  "
                    f"{item['swap'].state}  "
                    f"{item['reason']}"
                )
        return

    if len(resumable) > 1:
        print()
        print("MULTIPLE ACTIVE SWAPS")
        print("=====================")
        print(
            "More than one swap has a live automatic path."
        )
        print(
            "Refusing to guess which one you intend to resume."
        )
        print()
        print("Swap          State             Next")
        print("------------  ----------------  ----------------")

        for item in resumable:
            swap = item["swap"]
            print(
                f"{swap.swap_id:<12}  "
                f"{swap.state:<16}  "
                f"{str(item['next_action']):<16}"
            )

        if attention:
            print()
            print("Other records needing manual attention:")
            for item in attention:
                print(
                    f"  {item['swap'].swap_id}  "
                    f"{item['swap'].state}"
                )

        print()
        print(
            "Run `resume <short-swap-prefix>` for the "
            "active swap you want."
        )
        return

    item = resumable[0]
    swap = item["swap"]

    print()
    print("RESUMING SWAP")
    print("=============")
    print("swap:       ", swap.swap_id)
    print("checkpoint: ", swap.state)
    print("next:       ", item["next_action"])

    if attention:
        print()
        print(
            f"NOTE: {len(attention)} unrelated record(s) "
            "need manual attention but do not block this swap."
        )

    print()
    print(
        "Handing this swap to the existing autonomous driver."
    )
    print()

    command_run(
        SimpleNamespace(
            swap_id=swap.swap_id,
            max_steps=int(getattr(args, "max_steps", 6)),
            fee_rate=getattr(args, "fee_rate", None),
        )
    )



def command_run(args):
    from types import SimpleNamespace

    #
    # The driver may be reached from `run` (which declares --fee-rate) or
    # from `resume`, which passes this namespace on. The defensive read
    # keeps a caller that constructs a namespace by hand -- the tests do
    # -- meaningful: no parameter means "use the environment".
    #
    fee_rate = getattr(args, "fee_rate", None)

    safe_actions = {
        "sync-funding",
        "sync-settlement",
        "complete",
        "claim-blake",
        "claim-btc",
        "fund-btc",
        "refund-blake",
        "refund-btc",
    }

    economic_actions = {
        "fund-blake",
    }

    max_steps = int(getattr(args, "max_steps", 6))

    if max_steps < 1 or max_steps > 20:
        raise ValueError("--max-steps must be between 1 and 20")

    print()
    print("BLAKE SWAP AUTONOMOUS DRIVER")
    print("============================")
    print("swap id:", args.swap_id)
    print()
    print(
        "POLICY: recovery, local completion, protocol-Alice "
        "Bitcoin claim, and protocol-Bob BLAKE claim may execute automatically"
    )
    print(
        "POLICY: BLAKE funding and Nostr publication are disabled"
    )

    #
    # Contain one step's failure so a mid-swap exception reports
    # which step failed and stops cleanly, instead of unwinding the
    # whole driver with a bare traceback after an unknown amount of
    # progress.
    #
    current_step = 0

    try:
        for step in range(1, max_steps + 1):
            current_step = step
            swap = load_swap(args.swap_id)
            report = build_reconcile_report(swap)

            print()
            print(f"RECONCILIATION {step}")
            print("----------------")
            _run_print_decision(report)

            action = report.get("next_action")
            decision_class = report.get("decision_class")
            role = report.get("protocol_role")

            if decision_class == "settled":
                print()
                print("RUN RESULT: settled; no action required")
                return

            if decision_class == "waiting":
                print()
                print("RUN RESULT: waiting for counterparty or chain condition")
                return

            if decision_class == "manual-attention":
                print()
                print("RUN RESULT: manual attention required")
                print("PASS: runner refused to guess or mutate state")
                return

            if action in economic_actions:
                print()
                print("ACTION REQUIRED")
                print("===============")
                print(f"{action} is currently recommended by the reconciler")
                print()
                print("Automatic economic actions are disabled in this runner version.")
                print("PASS: no funds were moved")
                return

            if action not in safe_actions:
                print()
                print(
                    "RUN RESULT: refusing unrecognized or "
                    f"non-allowlisted action: {action!r}"
                )
                print("PASS: no automatic action was executed")
                return

            if role not in ("alice", "bob"):
                print()
                print(
                    "RUN RESULT: protocol role is not securely known; "
                    "refusing automatic action"
                )
                return

            if action == "claim-btc":
                if role != "alice":
                    raise RuntimeError(
                        "SECURITY FAILURE: automatic claim-btc "
                        "requires current participant to be protocol Alice"
                    )

                if report.get("blake", {}).get("status") != "funded":
                    print()
                    print("RUN RESULT: refusing automatic claim-btc")
                    print("reason: BLAKE HTLC is not confirmed and unspent")
                    return

                if report.get("btc", {}).get("status") != "funded":
                    print()
                    print("RUN RESULT: refusing automatic claim-btc")
                    print("reason: Bitcoin HTLC is not confirmed and unspent")
                    return

                secret = report.get("secret", {})
                if not (
                    secret.get("known")
                    and secret.get("source") == "locally-generated"
                    and secret.get("hash_matches") is True
                ):
                    print()
                    print("RUN RESULT: refusing automatic claim-btc")
                    print("reason: protocol Alice secret preconditions are not proven")
                    return

                min_blocks = int(
                    os.environ.get(
                        "BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS",
                        "2",
                    )
                )

                if min_blocks < 1:
                    raise ValueError(
                        "BLAKE_SWAP_RUN_MIN_BTC_CLAIM_BLOCKS must be >= 1"
                    )

                remaining = int(
                    report.get("timeouts", {}).get(
                        "btc_blocks_remaining",
                        -1,
                    )
                )

                if remaining < min_blocks:
                    print()
                    print("RUN RESULT: refusing automatic claim-btc")
                    print(
                        "reason: Bitcoin timeout margin is too small "
                        f"({remaining} blocks remaining; minimum {min_blocks})"
                    )
                    return


            if action == "fund-btc":
                if role != "bob":
                    raise RuntimeError(
                        "SECURITY FAILURE: automatic fund-btc "
                        "requires current participant to be protocol Bob"
                    )

                readiness = report.get("readiness", {})

                if not readiness.get("checked"):
                    print()
                    print("RUN RESULT: refusing automatic fund-btc")
                    print("reason: funding readiness was not evaluated")
                    return

                if readiness.get("ready") is not True:
                    print()
                    print("RUN RESULT: refusing automatic fund-btc")
                    print("reason: local funding readiness did not pass")

                    for item in readiness.get("reasons", []):
                        print(" -", item)

                    return

                if report.get("blake", {}).get("status") != "funded":
                    print()
                    print("RUN RESULT: refusing automatic fund-btc")
                    print("reason: BLAKE HTLC is not confirmed and unspent")
                    return

                if report.get("btc", {}).get("status") != "absent":
                    print()
                    print("RUN RESULT: refusing automatic fund-btc")
                    print("reason: Bitcoin funding is no longer absent")
                    return


            if action == "refund-btc":
                if role != "bob":
                    raise RuntimeError(
                        "SECURITY FAILURE: automatic refund-btc "
                        "requires current participant to be protocol Bob"
                    )

                if report.get("checkpoint_state") != State.BTC_FUNDED.value:
                    print()
                    print("RUN RESULT: refusing automatic refund-btc")
                    print("reason: local checkpoint is not BTC_FUNDED")
                    return

                if report.get("btc", {}).get("status") != "funded":
                    print()
                    print("RUN RESULT: refusing automatic refund-btc")
                    print("reason: Bitcoin HTLC is not confirmed and unspent")
                    return

                if int(report.get("timeouts", {}).get("btc_blocks_remaining", 1)) > 0:
                    print()
                    print("RUN RESULT: refusing automatic refund-btc")
                    print("reason: Bitcoin refund timelock is not mature")
                    return

            if action == "refund-blake":
                if role != "alice":
                    raise RuntimeError(
                        "SECURITY FAILURE: automatic refund-blake "
                        "requires current participant to be protocol Alice"
                    )

                if report.get("checkpoint_state") not in {
                    State.BLAKE_FUNDED.value,
                    State.BTC_REFUNDED.value,
                }:
                    print()
                    print("RUN RESULT: refusing automatic refund-blake")
                    print("reason: local checkpoint is not refund-eligible")
                    return

                if report.get("blake", {}).get("status") != "funded":
                    print()
                    print("RUN RESULT: refusing automatic refund-blake")
                    print("reason: BLAKE HTLC is not confirmed and unspent")
                    return

                if (
                    report.get("checkpoint_state")
                    == State.BLAKE_FUNDED.value
                    and report.get("btc", {}).get("status") != "absent"
                ):
                    print()
                    print("RUN RESULT: refusing automatic refund-blake")
                    print(
                        "reason: pre-BTC refund requires "
                        "Bitcoin funding to be independently absent"
                    )
                    return

                if int(report.get("timeouts", {}).get("blake_blocks_remaining", 1)) > 0:
                    print()
                    print("RUN RESULT: refusing automatic refund-blake")
                    print("reason: BLAKE refund timelock is not mature")
                    return

            print()
            print("AUTOMATIC ACTION")
            print("================")
            print(action)
            print()

            ns = SimpleNamespace(
                swap_id=args.swap_id,
                role=role,
                fee_rate=fee_rate,
            )

            if action == "sync-funding":
                command_sync_funding(ns)
            elif action == "sync-settlement":
                command_sync_settlement(ns)
            elif action == "complete":
                command_complete(ns)

            elif action == "claim-blake":
                if role != "bob":
                    raise RuntimeError(
                        "SECURITY FAILURE: automatic claim-blake "
                        "requires current participant to be protocol Bob"
                    )

                command_claim_blake(ns)

            elif action == "claim-btc":
                command_claim_btc(ns)

            elif action == "fund-btc":
                command_fund_btc(ns)

            elif action == "refund-btc":
                command_refund_btc(ns)

            elif action == "refund-blake":
                command_refund_blake(ns)

            else:
                raise RuntimeError("Internal runner allowlist mismatch")

            print()
            print("PASS: automatic action completed; reconciling again")

    except Exception as e:
        print()
        print("RUN RESULT: step failed")
        print(f"step:  {current_step} of {max_steps}")
        print(f"error: {type(e).__name__}: {e}")
        print("PASS: no further automatic action was attempted")
        raise SystemExit(1) from e
    print()
    print("RUN RESULT")
    print("==========")
    print("maximum reconciliation steps reached")
    print(
        "Step budget exhausted normally; reconcile again before "
        "deciding whether another action is appropriate."
    )


def main():
    parser = argparse.ArgumentParser(
        description="BTC/BLAKE atomic swap prototype"
    )

    sub = parser.add_subparsers(
        dest="command",
        required=True,
    )

    # new
    p = sub.add_parser(
        "new",
        help="Create a new swap"
    )

    p.add_argument(
        "--blake",
        required=True,
        type=float,
    )

    p.add_argument(
        "--sats",
        required=True,
        type=int,
    )

    p.add_argument(
        "--btc-timeout",
        type=int,
        default=None,
        help=(
            "Explicit Bitcoin CLTV height. "
            "If omitted with --blake-timeout, "
            "v1.5 timeout policy derives both."
        ),
    )

    p.add_argument(
        "--blake-timeout",
        type=int,
        default=None,
        help=(
            "Explicit BLAKE CLTV height. "
            "If omitted with --btc-timeout, "
            "v1.5 timeout policy derives both."
        ),
    )

    p.set_defaults(func=command_new)

    # show
    p = sub.add_parser(
        "show",
        help="Show a swap"
    )

    p.add_argument("swap_id")
    p.set_defaults(func=command_show)

    # Test-only state transition probe.
    #
    # This intentionally bypasses chain-evidence checks and therefore
    # must never be exposed as a production command.
    if os.environ.get("BLAKE_SWAP_TEST_MODE") == "1":
        p = sub.add_parser(
            "advance",
            help="TEST ONLY: advance swap state"
        )

        p.add_argument("swap_id")

        p.add_argument(
            "state",
            choices=[x.value for x in State],
        )

        p.add_argument(
            "--role",
            choices=["alice", "bob"],
            required=True,
            help="Protocol role whose local state is advanced",
        )

        p.set_defaults(func=command_advance)

    # list
    p = sub.add_parser(
        "list",
        help="List swaps"
    )

    p.set_defaults(func=command_list)

    p = sub.add_parser(
        "fund",
        help=(
            "Import the only ready Bob acceptance and "
            "fund that protocol-Alice BLAKE HTLC"
        ),
    )

    p.set_defaults(
        func=command_fund_ready_swap
    )

    # fund-blake
    p = sub.add_parser(
        "fund-blake",
        help="Fund the BLAKE HTLC"
    )

    p.add_argument("swap_id")
    p.set_defaults(func=command_fund_blake)

    # verify-blake
    p = sub.add_parser(
        "verify-blake",
        help="Verify the funded BLAKE HTLC"
    )

    p.add_argument("swap_id")
    p.set_defaults(func=command_verify_blake)

    # refresh-timeouts
    p = sub.add_parser(
        "refresh-timeouts",
        help=(
            "Refresh an unfunded Alice swap's "
            "v1.5 timeout plan"
        ),
    )

    p.add_argument("swap_id")

    p.add_argument(
        "--clear-stale-intent",
        action="store_true",
        help=(
            "Clear a funding intent that no confirmed payment "
            "explains. Use only after verifying that no funding "
            "transaction was broadcast."
        ),
    )

    p.set_defaults(
        func=command_refresh_timeouts
    )

    # status
    p = sub.add_parser(
        "status",
        help="Check both swap-chain nodes"
    )

    p.set_defaults(func=command_status)

    # fund-btc
    p = sub.add_parser(
        "fund-btc",
        help="Verify BLAKE and fund the Bitcoin HTLC"
    )

    p.add_argument("swap_id")
    p.set_defaults(func=command_fund_btc)

    # verify-btc
    p = sub.add_parser(
        "verify-btc",
        help="Verify the funded Bitcoin HTLC"
    )

    p.add_argument("swap_id")
    p.set_defaults(func=command_verify_btc)

    # claim-btc
    p = sub.add_parser(
        "claim-btc",
        help="Alice claims Bitcoin and reveals the secret"
    )

    p.add_argument("swap_id")
    add_settlement_fee_rate_argument(p)
    p.set_defaults(func=command_claim_btc)

    # extract-secret
    p = sub.add_parser(
        "extract-secret",
        help="Bob extracts the revealed secret from Bitcoin"
    )

    p.add_argument("swap_id")
    p.set_defaults(func=command_extract_secret)

    # claim-blake
    p = sub.add_parser(
        "claim-blake",
        help="Bob claims BLAKE using secret learned from Bitcoin"
    )

    p.add_argument("swap_id")
    add_settlement_fee_rate_argument(p)
    p.set_defaults(func=command_claim_blake)

    # refund-blake
    p = sub.add_parser(
        "refund-blake",
        help="Alice refunds BLAKE after its timeout"
    )

    p.add_argument("swap_id")
    add_settlement_fee_rate_argument(p)
    p.set_defaults(func=command_refund_blake)

    # refund-btc
    p = sub.add_parser(
        "refund-btc",
        help="Bob refunds Bitcoin after its timeout"
    )

    p.add_argument("swap_id")
    add_settlement_fee_rate_argument(p)
    p.set_defaults(func=command_refund_btc)

    # Discover public BLAKE/BTC offers.
    p = sub.add_parser(
        "market",
        help="List valid BLAKE/BTC offers from Nostr"
    )

    p.add_argument(
        "--limit",
        type=int,
        default=50,
    )

    p.set_defaults(
        func=command_market
    )

    #
    # Canonical machine-readable market state.
    #
    p = sub.add_parser(
        "market-state",
        help=(
            "Show canonical OPEN / MATCHED / "
            "CANCELLED / EXPIRED market state"
        ),
    )

    p.add_argument(
        "--limit",
        type=int,
        default=50,
    )

    p.add_argument(
        "--json",
        action="store_true",
        help="Emit machine-readable JSON",
    )

    p.set_defaults(
        func=command_market_state
    )

    # Public marketplace advertisement.
    # This does NOT create or fund an atomic swap.
    p = sub.add_parser(
        "publish-market-offer",
        help="Advertise a BLAKE/BTC offer on Nostr"
    )

    p.add_argument(
        "--blake",
        required=True,
        type=float,
    )

    p.add_argument(
        "--sats",
        required=True,
        type=int,
    )

    p.add_argument(
        "--minutes",
        type=int,
        default=60,
        help="Offer lifetime in minutes (default: 60)"
    )

    p.add_argument(
        "--direction",
        choices=[
            MARKET_DIRECTION_BLAKE_FOR_BTC,
            MARKET_DIRECTION_BTC_FOR_BLAKE,
        ],
        default=MARKET_DIRECTION_BLAKE_FOR_BTC,
        help=(
            "Asset the maker gives: "
            "blake-for-btc or btc-for-blake "
            "(default: blake-for-btc)"
        ),
    )

    p.set_defaults(
        func=command_publish_market_offer
    )

    p = sub.add_parser(
        "inspect-market-take",
        help="Build and verify a market take without publishing"
    )

    p.add_argument("offer_id")

    p.set_defaults(
        func=command_inspect_market_take
    )

    p = sub.add_parser(
        "market-takes",
        help="Show valid takes for one of your market offers"
    )

    p.add_argument("offer_id")

    p.add_argument(
        "--limit",
        type=int,
        default=100,
    )

    p.add_argument(
        "--json",
        action="store_true",
        help="Emit machine-readable JSON",
    )

    p.set_defaults(
        func=command_market_takes
    )

    p = sub.add_parser(
        "accept-take",
        help=(
            "Accept the only valid take for an offer; "
            "offer ID prefixes are allowed"
        ),
    )

    p.add_argument("offer_id")

    p.set_defaults(
        func=command_accept_only_take
    )

    p = sub.add_parser(
        "accept-market-take",
        help="Select a market take and bind it to a swap"
    )

    p.add_argument("take_id")

    p.set_defaults(
        func=command_accept_market_take
    )

    p = sub.add_parser(
        "publish-market-match",
        help="Publish the maker's signed match for an accepted take"
    )

    p.add_argument("take_id")

    p.set_defaults(
        func=command_publish_market_match
    )

    p = sub.add_parser(
        "market-matches",
        help="Show authenticated market matches for this participant"
    )

    p.add_argument(
        "--limit",
        type=int,
        default=100,
    )

    p.set_defaults(
        func=command_market_matches
    )

    p = sub.add_parser(
        "accept-reverse-market-match",
        help=(
            "Import a btc-for-blake market match "
            "as protocol Alice"
        ),
    )

    p.add_argument("match_id")

    p.set_defaults(
        func=command_accept_reverse_market_match
    )

    p = sub.add_parser(
        "accept-reverse-swap-offer",
        help=(
            "Import matched Alice's protocol offer "
            "as Bob for btc-for-blake"
        ),
    )

    p.add_argument("match_id")

    p.set_defaults(
        func=command_accept_reverse_swap_offer
    )

    p = sub.add_parser(
        "accept-match",
        help=(
            "Discover and import the only pending "
            "valid market match for this participant"
        ),
    )

    p.add_argument(
        "match_id",
        nargs="?",
        default=None,
        help=("Optional short match-ID prefix when more than one pending match exists"),
    )

    p.set_defaults(
        func=command_accept_only_match
    )

    p = sub.add_parser(
        "accept-market-match",
        help="Import the swap bound to a market match"
    )

    p.add_argument("match_id")

    p.set_defaults(
        func=command_accept_market_match
    )

    p = sub.add_parser(
        "take-market-offer",
        help="Take an open BLAKE/BTC market offer"
    )

    p.add_argument("offer_id")

    p.set_defaults(
        func=command_take_market_offer
    )

    p = sub.add_parser(
        "cancel-market-offer",
        help="Cancel one of your public market offers"
    )

    p.add_argument("offer_id")

    p.set_defaults(
        func=command_cancel_market_offer
    )

    p = sub.add_parser(
        "publish-offer",
        help="Publish Alice's swap offer"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_publish_offer
    )

    p = sub.add_parser(
        "accept-offer",
        help="Bob imports a published offer"
    )
    p.add_argument("swap_id")

    p.add_argument(
        "--expected-alice-pub",
        default=None,
        help=(
            "Require the offer to be signed by this exact Nostr "
            "identity. Required over Nostr unless "
            "--allow-unpinned-author is given."
        ),
    )

    p.add_argument(
        "--allow-unpinned-author",
        action="store_true",
        help=(
            "Over Nostr, accept whichever author the relay returns. "
            "Only safe when the offer's author is authenticated by "
            "another means, such as a maker-signed market match."
        ),
    )

    p.set_defaults(
        func=command_accept_offer
    )

    p = sub.add_parser(
        "publish-blake-funded",
        help="Alice publishes BLAKE funding message"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_publish_blake_funded
    )

    p = sub.add_parser(
        "receive-blake-funded",
        help="Bob imports BLAKE funding message"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_receive_blake_funded
    )

    p = sub.add_parser(
        "receive-accept",
        help="Alice imports Bob's acceptance"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_receive_accept
    )

    p = sub.add_parser(
        "publish-btc-funded",
        help="Bob publishes Bitcoin funding message"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_publish_btc_funded
    )

    p = sub.add_parser(
        "receive-btc-funded",
        help="Alice imports Bitcoin funding message"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_receive_btc_funded
    )

    p = sub.add_parser(
        "publish-btc-claimed",
        help="Alice publishes Bitcoin claim transaction"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_publish_btc_claimed
    )

    p = sub.add_parser(
        "receive-btc-claimed",
        help="Bob imports Bitcoin claim transaction"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_receive_btc_claimed
    )
    p = sub.add_parser(
        "publish-blake-claimed",
        help="Bob publishes BLAKE claim transaction"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_publish_blake_claimed
    )

    p = sub.add_parser(
        "receive-blake-claimed",
        help="Alice imports BLAKE claim transaction"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_receive_blake_claimed
    )

    p = sub.add_parser(
        "complete",
        help="Mark one party's swap state COMPLETE"
    )
    p.add_argument("swap_id")
    p.add_argument(
        "--role",
        choices=["alice", "bob"],
        required=True,
    )
    p.set_defaults(
        func=command_complete
    )

    p = sub.add_parser(
        "publish-btc-refunded",
        help="Bob publishes confirmed BTC refund"
    )
    
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_publish_btc_refunded
    )

    p = sub.add_parser(
        "receive-btc-refunded",
        help="Alice verifies confirmed BTC refund"
    )
    p.add_argument("swap_id")
    p.set_defaults(
        func=command_receive_btc_refunded
    )

    p = sub.add_parser(
        "rebroadcast-nostr-event",
        help="Rebroadcast an exact verified cached Nostr event"
    )

    p.add_argument(
        "event_id",
        help="64-character cached Nostr event ID"
    )

    p.set_defaults(
        func=command_rebroadcast_nostr_event
    )


    p = sub.add_parser(
        "sync-settlement",
        help=(
            "Reconcile durable settlement state directly "
            "from confirmed blockchain spends"
        ),
    )

    p.add_argument(
        "swap_id",
    )

    p.add_argument(
        "--role",
        choices=["alice", "bob"],
        required=True,
    )

    p.set_defaults(
        func=command_sync_settlement,
    )


    p = sub.add_parser(
        "sync-funding",
        help=(
            "Discover confirmed HTLC funding directly "
            "from blockchain history"
        ),
    )

    p.add_argument(
        "swap_id",
    )

    p.add_argument(
        "--role",
        choices=[
            "alice",
            "bob",
        ],
        required=True,
    )

    p.set_defaults(
        func=command_sync_funding,
    )

    p = sub.add_parser(
        "watch-htlc",
        help=(
            "Record this swap's HTLC descriptors in the watch-only "
            "wallets, so a funded swap stays observable after a key loss"
        ),
    )

    p.add_argument("swap_id")

    p.add_argument(
        "--chain",
        choices=["blake", "btc"],
        default=None,
        help="Only this chain (default: both)",
    )

    p.add_argument(
        "--json",
        action="store_true",
        help="Emit machine-readable JSON",
    )

    p.set_defaults(func=command_watch_htlc)

    p = sub.add_parser(
        "recovery-plan",
        help=(
            "Show how to recover an HTLC whose keys this wallet no "
            "longer holds: the descriptor, its checksum, and the "
            "import and spend steps"
        ),
    )

    p.add_argument("swap_id")

    p.set_defaults(func=command_recovery_plan)

    p = sub.add_parser(
        "reconcile",
        help=(
            "Read-only reconciliation of local state, both chains, "
            "protocol role, and next safe action"
        ),
    )

    p.add_argument("swap_id")

    p.add_argument(
        "--json",
        action="store_true",
        help="Emit machine-readable JSON",
    )

    p.set_defaults(func=command_reconcile)

    p = sub.add_parser(
        "ignore-match",
        help=(
            "Locally acknowledge a valid unimported market match "
            "so `next` will not surface it automatically"
        ),
    )

    p.add_argument(
        "match_id",
        help="Short or full market match ID",
    )

    p.set_defaults(
        func=command_ignore_match
    )

    p = sub.add_parser(
        "next",
        help=(
            "Perform the only unambiguous non-economic "
            "coordination step, or show the next required action"
        ),
    )

    p.set_defaults(
        func=command_next
    )

    p = sub.add_parser(
        "dashboard",
        help=(
            "Show active, attention, settled, and archived "
            "swaps using read-only reconciliation"
        ),
    )

    p.add_argument(
        "--json",
        action="store_true",
        help="Emit machine-readable dashboard JSON",
    )

    p.set_defaults(
        func=command_dashboard
    )

    p = sub.add_parser(
        "dismiss",
        help=(
            "Archive an abandoned local swap only when "
            "read-only reconciliation proves no funds exist"
        ),
    )

    p.add_argument(
        "swap_id",
        help="Short or full local swap ID",
    )

    p.set_defaults(
        func=command_dismiss
    )

    p = sub.add_parser(
        "resume",
        help=(
            "Resume the only live local swap, or an "
            "explicit short swap-ID prefix"
        ),
    )

    p.add_argument(
        "swap_id",
        nargs="?",
        default=None,
        help=(
            "Optional short swap-ID prefix when more "
            "than one live swap exists"
        ),
    )

    p.add_argument(
        "--max-steps",
        type=int,
        default=6,
        help=(
            "Maximum reconcile/action cycles before stopping "
            "(default: 6)"
        ),
    )

    add_settlement_fee_rate_argument(p)

    p.set_defaults(
        func=command_resume
    )

    p = sub.add_parser(
        "run",
        help=(
            "Conservative autonomous driver: automatically "
            "repairs confirmed state and completes settled swaps"
        ),
    )

    p.add_argument("swap_id")

    p.add_argument(
        "--max-steps",
        type=int,
        default=6,
        help=(
            "Maximum reconcile/action cycles before stopping "
            "(default: 6)"
        ),
    )

    add_settlement_fee_rate_argument(p)

    p.set_defaults(func=command_run)


    p = sub.add_parser(
        "resume-all",
        help=(
            "Drive every independently safe local "
            "post-funding settlement/recovery path"
        ),
    )
    p.set_defaults(
        func=command_resume_all,
    )

    # Parse only AFTER every command has been registered.
    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()

