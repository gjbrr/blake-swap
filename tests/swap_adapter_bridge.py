#!/usr/bin/env python3

"""
One-request JSON adapter bridge for the Python reference implementation.

stdin:
    one JSON object

stdout:
    one JSON object
"""

import json
import os
import subprocess
import sys
from pathlib import Path


def respond(value):
    sys.stdout.write(
        json.dumps(value, sort_keys=True) + "\n"
    )
    sys.stdout.flush()


def main():
    try:
        request = json.loads(sys.stdin.read())
        op = request.get("op")

        if op == "command":
            executable = request["executable"]
            args = request.get("args", [])

            p = subprocess.run(
                [
                    sys.executable,
                    executable,
                    *args,
                ],
                text=True,
                capture_output=True,
                env=os.environ.copy(),
            )

            respond({
                "ok": p.returncode == 0,
                "returncode": p.returncode,
                "stdout": p.stdout,
                "stderr": p.stderr,
            })

            return 0

        if op == "state":
            path = Path(
                request["state_path"]
            )

            if not path.exists():
                respond({
                    "ok": False,
                    "error": (
                        "state file does not exist: "
                        + str(path)
                    ),
                })
                return 0

            data = json.loads(
                path.read_text()
            )

            respond({
                "ok": True,
                "state": data,
            })

            return 0

        respond({
            "ok": False,
            "error": (
                f"unknown adapter operation: {op}"
            ),
        })

        return 0

    except Exception as e:
        respond({
            "ok": False,
            "error": (
                f"{type(e).__name__}: {e}"
            ),
        })

        return 0


if __name__ == "__main__":
    raise SystemExit(main())
