# Python Reference Implementation — Release Candidate

This document records the release-readiness baseline for the Python
reference implementation of the BTC ↔ BLAKE atomic-swap protocol.

The purpose of this checkpoint is not to declare the software production
ready. It establishes a stable, independently installable Python
implementation that can serve as:

- an executable protocol specification;
- a reference implementation for interoperability work;
- a behavioral oracle for independent implementations such as Moxie;
- a regression target for the portable security-conformance contract.

The software remains experimental and unaudited. Do not use it with
valuable funds.

---

## Release-candidate baseline

The release-hardening work builds on these checkpoints:

- `f4d57b1` — scheduled-v7 mixed-chaos campaign;
- `b2f8611` — installable Python reference implementation;
- `c538d13` — removal of machine-specific node defaults.

The package currently declares:

- package: `blake-swap`;
- version: `0.2.0`;
- supported Python: 3.12–3.13;
- build backend: setuptools;
- installed engine command: `blake-swap-engine`;
- installed autonomous-agent command: `blake-swap-agent`.

A later annotated release tag may identify the final RC commit. This
document does not assume that tag already exists.

---

## What has been proven

### 1. The implementation builds as a normal Python wheel

The repository builds a standard wheel with:

```bash
python -m pip wheel --no-deps --wheel-dir dist .
```

That wheel installs into a clean virtual environment and exposes the two
console entry points, so an independent implementation can run the reference
engine without the checkout:

```bash
python -m venv /tmp/blake-rc
/tmp/blake-rc/bin/pip install dist/blake_swap-0.2.0-py3-none-any.whl
/tmp/blake-rc/bin/blake-swap-engine --help
/tmp/blake-rc/bin/blake-swap-agent --help
```

`tests/installed_wheel_tests.py` proves this on every run in a throwaway
environment: the wheel builds, `pip check` reports a healthy dependency set,
both entry points execute, and the installed engine runs node-free from a
directory that is not the checkout.

The remaining release claims are carried by the portable conformance material
(`docs/MOXIE_SECURITY_CONFORMANCE.md`) and the adapter contract
(`docs/IMPLEMENTATION_ADAPTER_PROTOCOL.md`), which a second implementation is
expected to satisfy rather than this document restating.
