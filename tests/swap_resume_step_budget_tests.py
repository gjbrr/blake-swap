import contextlib
import io
import json
import os
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import swap_v2


def main():
    with tempfile.TemporaryDirectory() as td:
        state_dir = Path(td)
        swap_id = "abcdef123456"

        record = {
            "swap_id": swap_id,
            "state": "BLAKE_FUNDED",
            "nostr_alice_pub": "alice-pub",
            "nostr_bob_pub": "bob-pub",
        }

        (state_dir / f"{swap_id}.json").write_text(json.dumps(record))

        captured = {}

        def fake_load_swap(sid, role="alice"):
            assert sid == swap_id
            return SimpleNamespace(
                swap_id=swap_id,
                state="BLAKE_FUNDED",
                nostr_alice_pub="alice-pub",
                nostr_bob_pub="bob-pub",
            )

        def fake_reconcile(_swap):
            return {
                "decision_class": "actionable",
                "next_action": "sync-funding",
                "reason": "test",
            }

        def fake_run(args):
            captured["swap_id"] = args.swap_id
            captured["max_steps"] = args.max_steps

        old_base = swap_v2.BASE_SWAP_DIR
        try:
            swap_v2.BASE_SWAP_DIR = state_dir

            with (
                patch.object(swap_v2, "current_nostr_pubkey", return_value="alice-pub"),
                patch.object(swap_v2, "load_swap", side_effect=fake_load_swap),
                patch.object(swap_v2, "build_reconcile_report", side_effect=fake_reconcile),
                patch.object(swap_v2, "command_run", side_effect=fake_run),
                contextlib.redirect_stdout(io.StringIO()),
            ):
                swap_v2.command_resume(
                    SimpleNamespace(
                        swap_id=swap_id,
                        max_steps=1,
                    )
                )
        finally:
            swap_v2.BASE_SWAP_DIR = old_base

        assert captured == {
            "swap_id": swap_id,
            "max_steps": 1,
        }, captured

    print("PASS: resume forwards an explicit one-step budget")


if __name__ == "__main__":
    main()
