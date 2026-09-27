# Historical migration scripts — do not run

The two scripts in this directory are **spent one-shot source-patching
artifacts** from the development of the reconciliation layer. They rewrite
`src/swap_v2.py` by string surgery. They are kept only so the project history
is legible; they are **not** runtime code, they are **not** imported by
anything, and they must **not** be run.

| Script | Status |
|---|---|
| `install_reconcile.py` | Already applied. It aborts if `command_reconcile` exists, which it does. |
| `fix_reconcile_spent.py` | **Obsolete.** It targets `observe_htlc` and `derive_reconcile_action`, which no longer exist (renamed to `_reconcile_observe_htlc` and `_reconcile_next_action`). It also writes a ~470 KB backup file *before* validating its anchors, so running it aborts **and** litters the tree. |

Both are excluded from test coverage deliberately: they were never executable
in the current tree, so including them would permanently depress the coverage
figure with code that cannot be exercised.

If a source transformation is ever needed again, prefer a reviewed patch over
another script that edits the engine in place.
