import contextlib
import io
import json
from types import SimpleNamespace
from unittest.mock import patch

import swap_v2


OFFER_ID = "a" * 64
ALICE_PUB = "b" * 64

TAKES = [
    {
        "event_id": "c" * 64,
        "taker": "d" * 64,
        "blake_amount": 10.0,
        "btc_sats": 25000,
        "created_at": 1234567890,
        "direction": "blake-for-btc",
    }
]


async def fake_fetch_market_takes(
    offer_id,
    limit=100,
    allow_matched=False,
    **_kwargs,
):
    assert offer_id == OFFER_ID

    original = {
        "id": OFFER_ID,
        "pubkey": ALICE_PUB,
    }

    offer_message = {
        "blake_amount": 10.0,
        "btc_sats": 25000,
    }

    return original, offer_message, TAKES


class FakePrivateKey:
    def __init__(self, _raw):
        pass

    @property
    def public_key(self):
        return self

    def format(self, compressed=True):
        assert compressed is True
        return bytes.fromhex("02" + ALICE_PUB)


def run_command(*, json_mode, offer_value=OFFER_ID):
    stdout = io.StringIO()

    args = SimpleNamespace(
        offer_id=offer_value,
        limit=100,
        json=json_mode,
    )

    with (
        patch.object(
            swap_v2,
            "nostr_fetch_market_takes",
            side_effect=fake_fetch_market_takes,
        ),
        patch.object(
            swap_v2,
            "resolve_cached_nostr_prefix",
            return_value=OFFER_ID,
        ) as resolve_prefix,
        patch.object(
            swap_v2,
            "resolve_cached_nostr_prefix",
            return_value=OFFER_ID,
        ) as resolve_prefix,
        patch.object(
            swap_v2,
            "nostr_private_key_hex",
            return_value="11" * 32,
        ),
        patch("coincurve.PrivateKey", FakePrivateKey),
        contextlib.redirect_stdout(stdout),
    ):
        swap_v2.command_market_takes(args)

    return stdout.getvalue(), resolve_prefix


def test_json_output():
    stdout, resolve_prefix = run_command(
        json_mode=True,
        offer_value=OFFER_ID[:12],
    )

    parsed = json.loads(stdout)

    resolve_prefix.assert_called_once_with(
        OFFER_ID[:12],
        expected_type="market-offer",
        label="market offer",
        announce=False,
    )

    assert parsed["offer_id"] == OFFER_ID
    assert len(parsed["takes"]) == 1
    assert parsed["takes"][0]["event_id"] == "c" * 64
    assert parsed["takes"][0]["taker"] == "d" * 64
    assert parsed["takes"][0]["blake_amount"] == 10.0
    assert parsed["takes"][0]["btc_sats"] == 25000


def test_human_output_still_present():
    stdout, _ = run_command(json_mode=False)

    assert "TAKES FOR MARKET OFFER" in stdout
    assert "1 valid take(s)" in stdout
    assert "FULL TAKE ID(S)" in stdout
    assert "c" * 64 in stdout


def main():
    test_json_output()
    print(
        "PASS: market-takes resolves offer prefixes and emits validated JSON"
    )

    test_human_output_still_present()
    print("PASS: market-takes human-readable output is preserved")


if __name__ == "__main__":
    main()
