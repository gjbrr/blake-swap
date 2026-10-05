#!/usr/bin/env python3

import json

import swap_protocol_tests as h


def read_intent(base, swap_id, chain):
    path = (
        base
        / "funding-intents"
        / f"{swap_id}.{chain}.json"
    )

    h.require(
        path.exists(),
        f"pending {chain} funding intent does not exist",
    )

    return path, json.loads(path.read_text())


def swap_cmd_with_env(
    *args,
    role,
    extra_env,
    expect_ok=True,
):
    env = {
        "BLAKE_SWAP_TRANSPORT": "file",
        "BLAKE_SWAP_MESSAGE_DIR": str(h.MESSAGE_DIR),
        "BLAKE_SWAP_ROLE": role,
    }

    #
    # Point the engine at the same resolved nodes the harness uses.
    # Only resolved values are included, so an unresolved node path can
    # never be passed on as the literal string "None" and shadow the
    # engine's own defaults.
    #
    env.update(h.node_env())

    if role == "alice":
        env["BLAKE_SWAP_STATE_DIR"] = str(h.ALICE_BASE)
        env["BLAKE_SWAP_CHAIN_WALLET"] = h.ALICE_WALLET
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.ALICE_PARTICIPANT_KEY
        )

    elif role == "bob":
        env["BLAKE_SWAP_STATE_DIR"] = str(h.BOB_BASE)
        env["BLAKE_SWAP_CHAIN_WALLET"] = h.BOB_WALLET
        env["BLAKE_SWAP_PARTICIPANT_KEY_FILE"] = str(
            h.BOB_PARTICIPANT_KEY
        )

    else:
        raise h.TestFailure(
            f"unknown swap role: {role}"
        )

    env.update(extra_env)

    return h.run(
        [h.sys.executable, h.SWAP_V2, *args],
        env=env,
        expect_ok=expect_ok,
    )


def wallet_send_txids(cli, wallet):
    return {
        entry.get("txid")
        for entry in cli(
            "listtransactions",
            "*",
            1000,
            0,
            "true",
            wallet=wallet,
        )
        if entry.get("category") == "send"
        and entry.get("txid")
    }


def test_pending_blake_funding():
    h.banner("PRODUCTION BLAKE FUNDING: PENDING CONFIRMATION")

    swap_id = h.make_fresh_swap(blake=0.001)
    h.handshake(swap_id)

    production_env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
        "BLAKE_SWAP_BLAKE_NETWORK": "regtest",
    }

    before = wallet_send_txids(
        h.blake_cli,
        h.ALICE_WALLET,
    )

    first = swap_cmd_with_env(
        "fund-blake",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    print()
    print("=== FIRST FUND-BLAKE STDOUT ===")
    print(first.stdout)
    print("=== FIRST FUND-BLAKE STDERR ===")
    print(first.stderr)
    print("=== END FIRST FUND-BLAKE OUTPUT ===")
    print()

    h.require(
        "awaiting active-chain confirmation"
        in (first.stdout + first.stderr).lower(),
        "first production BLAKE funding did not report pending confirmation",
    )

    state = h.load_state(
        swap_id,
        "alice",
    )

    h.require(
        state.get("state") == "OFFERED",
        "unconfirmed BLAKE funding advanced durable state",
    )

    intent_path, intent = read_intent(
        h.ALICE_BASE,
        swap_id,
        "blake",
    )

    first_txid = intent.get("txid")
    first_vout = intent.get("vout")

    h.require(
        first_txid,
        "pending BLAKE funding intent did not record txid",
    )
    h.require(
        first_vout is not None,
        "pending BLAKE funding intent did not record vout",
    )

    after_first = wallet_send_txids(
        h.blake_cli,
        h.ALICE_WALLET,
    )

    h.require(
        first_txid in (after_first - before),
        "first BLAKE funding broadcast was not visible in wallet history",
    )

    # Retry while still unconfirmed.
    swap_cmd_with_env(
        "fund-blake",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    state_again = h.load_state(
        swap_id,
        "alice",
    )

    intent_path_again, intent_again = read_intent(
        h.ALICE_BASE,
        swap_id,
        "blake",
    )

    after_second = wallet_send_txids(
        h.blake_cli,
        h.ALICE_WALLET,
    )

    h.require(
        state_again.get("state") == "OFFERED",
        "pending BLAKE funding retry advanced durable state",
    )
    h.require(
        intent_again.get("txid") == first_txid,
        "pending BLAKE funding retry changed txid",
    )
    h.require(
        intent_again.get("vout") == first_vout,
        "pending BLAKE funding retry changed vout",
    )
    h.require(
        after_second == after_first,
        "pending BLAKE funding retry created a second wallet send",
    )
    h.require(
        intent_path_again == intent_path,
        "pending BLAKE funding changed intent path",
    )

    # Normal chain confirmation happens externally.
    h.mine_blake(1)

    swap_cmd_with_env(
        "fund-blake",
        swap_id,
        role="alice",
        extra_env=production_env,
    )

    confirmed = h.load_state(
        swap_id,
        "alice",
    )

    h.require(
        confirmed.get("state") == "BLAKE_FUNDED",
        "confirmed BLAKE funding did not advance state",
    )
    h.require(
        confirmed.get("blake_funding_txid") == first_txid,
        "confirmed BLAKE funding txid changed",
    )
    h.require(
        confirmed.get("blake_funding_vout") == first_vout,
        "confirmed BLAKE funding vout changed",
    )
    h.require(
        confirmed.get("blake_funding_blockhash"),
        "confirmed BLAKE funding has no blockhash",
    )
    h.require(
        not intent_path.exists(),
        "confirmed BLAKE funding did not clear intent",
    )

    h.ok(
        "production BLAKE funding broadcasts once and waits for real confirmation"
    )


def test_pending_btc_funding():
    h.banner("PRODUCTION BTC FUNDING: PENDING CONFIRMATION")

    swap_id = h.make_fresh_swap(blake=0.001)
    h.handshake(swap_id)
    h.fund_blake_and_sync(swap_id)

    production_env = {
        "BLAKE_SWAP_AUTO_MINE": "0",
    }

    before = wallet_send_txids(
        h.btc_cli,
        h.BOB_WALLET,
    )

    first = swap_cmd_with_env(
        "fund-btc",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    h.require(
        "awaiting active-chain confirmation"
        in first.stdout.lower(),
        "first production BTC funding did not report pending confirmation",
    )

    h.require(
        "bitcoin funding confirmation target:"
        in first.stdout.lower(),
        "production BTC funding did not use urgent confirmation target",
    )

    state = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        state.get("state") == "BLAKE_FUNDED",
        "unconfirmed BTC funding advanced durable state",
    )

    intent_path, intent = read_intent(
        h.BOB_BASE,
        swap_id,
        "btc",
    )

    first_txid = intent.get("txid")
    first_vout = intent.get("vout")

    h.require(
        first_txid,
        "pending BTC funding intent did not record txid",
    )
    h.require(
        first_vout is not None,
        "pending BTC funding intent did not record vout",
    )

    after_first = wallet_send_txids(
        h.btc_cli,
        h.BOB_WALLET,
    )

    h.require(
        first_txid in (after_first - before),
        "first BTC funding broadcast was not visible in wallet history",
    )

    # Retry before confirmation: exact same wallet payment.
    swap_cmd_with_env(
        "fund-btc",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    state_again = h.load_state(
        swap_id,
        "bob",
    )

    _path_again, intent_again = read_intent(
        h.BOB_BASE,
        swap_id,
        "btc",
    )

    after_second = wallet_send_txids(
        h.btc_cli,
        h.BOB_WALLET,
    )

    h.require(
        state_again.get("state") == "BLAKE_FUNDED",
        "pending BTC funding retry advanced durable state",
    )
    h.require(
        intent_again.get("txid") == first_txid,
        "pending BTC funding retry changed txid",
    )
    h.require(
        intent_again.get("vout") == first_vout,
        "pending BTC funding retry changed vout",
    )
    h.require(
        after_second == after_first,
        "pending BTC funding retry created a second wallet send",
    )

    h.mine_btc(1)

    swap_cmd_with_env(
        "fund-btc",
        swap_id,
        role="bob",
        extra_env=production_env,
    )

    confirmed = h.load_state(
        swap_id,
        "bob",
    )

    h.require(
        confirmed.get("state") == "BTC_FUNDED",
        "confirmed BTC funding did not advance state",
    )
    h.require(
        confirmed.get("btc_funding_txid") == first_txid,
        "confirmed BTC funding txid changed",
    )
    h.require(
        confirmed.get("btc_funding_vout") == first_vout,
        "confirmed BTC funding vout changed",
    )
    h.require(
        confirmed.get("btc_funding_blockhash"),
        "confirmed BTC funding has no blockhash",
    )
    h.require(
        not intent_path.exists(),
        "confirmed BTC funding did not clear intent",
    )

    h.ok(
        "production BTC funding broadcasts once and waits for real confirmation"
    )


def main():
    h.preflight()
    h.ok("preflight")

    h.ensure_test_participant_keys()

    test_pending_blake_funding()
    test_pending_btc_funding()

    print()
    print("PASS: production pending-funding regression suite")


if __name__ == "__main__":
    main()
