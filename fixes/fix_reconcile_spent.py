#!/usr/bin/env python3
from pathlib import Path

p = Path("src/swap_v2.py")
if not p.exists():
    raise SystemExit("ABORT: run from ~/blake-swap")

s = p.read_text()

backup = Path("src/swap_v2.py.before-reconcile-spent-fix")
if backup.exists():
    raise SystemExit(f"ABORT: backup already exists: {backup}")

backup.write_text(s)

start = s.find("def observe_htlc(swap, chain):")
end = s.find("\ndef derive_reconcile_action(", start)

if start == -1 or end == -1:
    raise SystemExit("ABORT: could not locate reconcile observer functions")

new_observe = r'''def observe_htlc(swap, chain):
    """
    Read-only observation of one chain's HTLC.

    Confirmed funding may be discovered from chain history.
    A missing gettxout result is NOT itself an error: for a
    settled swap it normally means the HTLC has been spent,
    so we classify the confirmed spender.
    """
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
        return {
            "status": "invalid",
            "reason": (
                f"incomplete recorded {chain.upper()} "
                "funding outpoint"
            ),
        }

    discovered = False

    if txid is None:
        try:
            match = _quiet_reconcile_call(
                scan_chain_for_expected_funding,
                work,
                chain,
            )
        except Exception as e:
            return {
                "status": "unknown",
                "reason": str(e),
            }

        if match is None:
            return {
                "status": "absent",
            }

        discovered = True
        txid = match["txid"]
        vout = match["vout"]
        blockhash = match["blockhash"]

        setattr(work, tx_field, txid)
        setattr(work, vout_field, vout)
        setattr(work, block_field, blockhash)

    try:
        utxo = cli(
            "gettxout",
            txid,
            vout,
        )
    except Exception as e:
        return {
            "status": "unknown",
            "txid": txid,
            "vout": vout,
            "reason": str(e),
        }

    if utxo is not None:
        try:
            _quiet_reconcile_call(
                verifier,
                work,
                verbose=False,
            )
        except Exception as e:
            return {
                "status": "invalid",
                "txid": txid,
                "vout": vout,
                "reason": str(e),
            }

        return {
            "status": "funded",
            "txid": txid,
            "vout": vout,
            "blockhash": blockhash,
            "confirmations": int(
                utxo.get("confirmations", 0) or 0
            ),
            "discovered": discovered,
        }

    if not blockhash:
        return {
            "status": "unknown",
            "txid": txid,
            "vout": vout,
            "reason": (
                "funding output is unavailable and no "
                "funding blockhash is known"
            ),
        }

    try:
        spent = _quiet_reconcile_call(
            find_confirmed_htlc_spend,
            work,
            chain,
        )
    except Exception as e:
        return {
            "status": "pending-or-ambiguous",
            "txid": txid,
            "vout": vout,
            "reason": str(e),
        }

    if spent is None:
        return {
            "status": "pending-or-ambiguous",
            "txid": txid,
            "vout": vout,
            "reason": (
                "funding output is unavailable but no "
                "confirmed spender was found"
            ),
        }

    return {
        "status": "spent",
        "txid": txid,
        "vout": vout,
        "spend_txid": spent["txid"],
        "spend_blockhash": spent["blockhash"],
        "branch": spent["branch"],
        "discovered": discovered,
    }

'''

s = s[:start] + new_observe + s[end:]

old = '''    if state == State.COMPLETE:
        return (
            "none",
            "swap is already COMPLETE",
        )
'''

new = '''    # COMPLETE is a local checkpoint, not an authority.
    # Accept it only when both chains independently show
    # the expected claim settlement.
    if state == State.COMPLETE:
        if (
            btc_obs.get("status") == "spent"
            and btc_obs.get("branch") == "claim"
            and blake_obs.get("status") == "spent"
            and blake_obs.get("branch") == "claim"
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
'''

if s.count(old) != 1:
    raise SystemExit(
        f"ABORT: expected one COMPLETE shortcut, found {s.count(old)}"
    )

s = s.replace(old, new, 1)

p.write_text(s)

print("PASS: reconcile now classifies spent HTLCs from chain history")
print("PASS: COMPLETE no longer overrides contradictory chain evidence")
print(f"backup: {backup}")
