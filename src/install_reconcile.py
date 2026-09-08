#!/usr/bin/env python3
from pathlib import Path

p = Path('src/swap_v2.py')
if not p.exists():
    raise SystemExit('ABORT: run this from the blake-swap project root')

original = p.read_text()
if 'def command_reconcile(args):' in original:
    raise SystemExit('ABORT: reconcile command already appears to be installed')

function_marker = '\ndef main():\n'
if original.count(function_marker) != 1:
    raise SystemExit(
        f"ABORT: expected one def main() marker, found {original.count(function_marker)}"
    )

block = r'''

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


def _reconcile_quiet(fn, *args, **kwargs):
    import contextlib
    import io
    with contextlib.redirect_stdout(io.StringIO()):
        return fn(*args, **kwargs)


def _reconcile_observe_htlc(swap, chain):
    """Observe funding/spend truth without mutating durable swap state."""
    import copy

    work = copy.copy(swap)

    if chain == "blake":
        cli = blake_cli
        tx_field = "blake_funding_txid"
        vout_field = "blake_funding_vout"
        block_field = "blake_funding_blockhash"
        verifier = verify_blake_contract
    elif chain == "btc":
        cli = btc_cli
        tx_field = "btc_funding_txid"
        vout_field = "btc_funding_vout"
        block_field = "btc_funding_blockhash"
        verifier = verify_btc_contract
    else:
        raise ValueError(f"Unknown chain: {chain}")

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

    if utxo is not None:
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
            "confirmations": int(utxo.get("confirmations", 0) or 0),
            "discovered": discovered,
        }

    if not blockhash:
        return {
            "status": "unknown",
            "txid": txid,
            "vout": vout,
            "reason": "outpoint unavailable and funding blockhash is unknown",
        }

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
        return "none", "swap is already COMPLETE"

    unsafe = {"unknown", "invalid", "pending-or-ambiguous"}
    if blake.get("status") in unsafe:
        return "manual-attention", f"BLAKE observation is {blake['status']}"
    if btc.get("status") in unsafe:
        return "manual-attention", f"Bitcoin observation is {btc['status']}"

    # Chain truth outranks a stale checkpoint.
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


def build_reconcile_report(swap):
    """Build a machine-readable, read-only protocol report."""
    role, me, counterparty = _reconcile_local_role(swap)

    blake_height = int(blake_cli("getblockcount"))
    btc_height = int(btc_cli("getblockcount"))

    blake = _reconcile_observe_htlc(swap, "blake")
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

    return {
        "swap_id": swap.swap_id,
        "participant_pubkey": me,
        "protocol_role": role,
        "counterparty_pubkey": counterparty,
        "checkpoint_state": swap.state,
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
        "read_only": True,
    }


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

    print()
    print("TIMEOUT DISTANCE")
    print("----------------")
    print("BTC blocks:  ", report["timeouts"]["btc_blocks_remaining"])
    print("BLAKE blocks:", report["timeouts"]["blake_blocks_remaining"])

    print()
    print("NEXT SAFE ACTION")
    print("----------------")
    print(report["next_action"])
    print()
    print("reason:")
    print(report["reason"])

    print()
    print("PASS: reconciliation was read-only; no funds or messages were sent")
'''

patched = original.replace(function_marker, block + function_marker, 1)

parser_marker = '''    # Parse only AFTER every command has been registered.\n    args = parser.parse_args()\n'''
if patched.count(parser_marker) != 1:
    raise SystemExit(
        f"ABORT: expected one argparse finalization marker, found {patched.count(parser_marker)}"
    )

parser_block = '''    p = sub.add_parser(\n        "reconcile",\n        help=(\n            "Read-only reconciliation of local state, both chains, "\n            "protocol role, and next safe action"\n        ),\n    )\n\n    p.add_argument("swap_id")\n\n    p.add_argument(\n        "--json",\n        action="store_true",\n        help="Emit machine-readable JSON",\n    )\n\n    p.set_defaults(func=command_reconcile)\n\n    # Parse only AFTER every command has been registered.\n    args = parser.parse_args()\n'''

patched = patched.replace(parser_marker, parser_block, 1)

backup = Path('src/swap_v2.py.before-reconcile')
if backup.exists():
    raise SystemExit(f'ABORT: backup already exists: {backup}')

backup.write_text(original)
p.write_text(patched)

print('PASS: installed read-only reconcile command')
print('backup:', backup)
