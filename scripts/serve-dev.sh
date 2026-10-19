#!/usr/bin/env bash
#
# The development loop: build both wasm modules and the server, then serve the app
# against whatever local rig is running.
#
# It reads the rig's own description — `scripts/test-nodes start` writes
# ~/.blake-swap-test/rig-env.json — so the operator never copies four paths by hand. With
# no rig running it serves the app anyway: a static app that reports the chain as
# unreachable is a perfectly good way to work on the page.
#
# Usage:
#   scripts/serve-dev.sh [--addr 127.0.0.1:8787] [--open] [--no-build]
#
# The proxy listens on loopback by default and forwards only the JSON-RPC methods the
# client needs. Do not expose it: it has no authentication, because it is meant to be the
# half of the app that holds the node credentials on a machine the operator controls.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ADDR="127.0.0.1:8787"
OPEN=""
BUILD=1

while [ $# -gt 0 ]; do
  case "$1" in
    --addr) ADDR="$2"; shift 2 ;;
    --open) OPEN="-open"; shift ;;
    --no-build) BUILD=0; shift ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "serve-dev: unknown argument $1" >&2; exit 2 ;;
  esac
done

if [ "$BUILD" = 1 ]; then
  make wasm server
fi

ARGS=(-addr "$ADDR" -dir web)

RIG_FILE="${RIG_ENV:-$HOME/.blake-swap-test/rig-env.json}"
RIG_ARGS_STRING=""

if [ -f "$RIG_FILE" ] && command -v python3 >/dev/null 2>&1; then
  # Read the rig in one place: the datadirs and the two coin daemons, with the cookies
  # located here rather than by the server, so a datadir that has no wallet yet still
  # serves the page.
  RIG_ARGS_STRING="$(python3 - "$RIG_FILE" <<'PY'
import json
import sys
from pathlib import Path

rig = json.loads(Path(sys.argv[1]).read_text())

if not rig.get("running"):
    raise SystemExit(0)

args = []

for name in ("btc", "blake"):
    port = rig.get(f"{name}_rpcport")
    datadir = rig.get(f"{name}_datadir")

    if not port or not datadir:
        raise SystemExit(0)

    cookie = next(
        (
            candidate
            for candidate in (Path(datadir) / "regtest" / ".cookie", Path(datadir) / ".cookie")
            if candidate.is_file()
        ),
        None,
    )

    if cookie is None:
        raise SystemExit(0)

    args += [f"-{name}-rpc", f"http://127.0.0.1:{port}/", f"-{name}-cookie", str(cookie)]

print(" ".join(args))
PY
)" || RIG_ARGS_STRING=""
fi

if [ -n "$RIG_ARGS_STRING" ]; then
  read -r -a RIG_ARGS <<<"$RIG_ARGS_STRING"
  ARGS+=("${RIG_ARGS[@]}")
  echo "serve-dev: proxying the rig described by $RIG_FILE"
elif [ -f "$RIG_FILE" ]; then
  echo "serve-dev: $RIG_FILE is present but not usable; serving without nodes"
else
  echo "serve-dev: no rig found; serving without nodes (start one with scripts/test-nodes start)"
fi

echo "serve-dev: building is done; the app is at http://$ADDR/"
echo "serve-dev: edit, run 'make wasm', reload — no restart needed"

if [ -n "$OPEN" ]; then
  exec ./bin/blake-swap-app "${ARGS[@]}" -open
fi

exec ./bin/blake-swap-app "${ARGS[@]}"
