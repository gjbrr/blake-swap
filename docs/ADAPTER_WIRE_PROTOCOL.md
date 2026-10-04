# Swap Implementation Adapter Wire Protocol

This is the language-neutral boundary between the adversarial security
harness and a participant implementation.

The reference transport uses one JSON request on stdin and one JSON
response on stdout.

## Command request

{
  "op": "command",
  "role": "bob",
  "args": [
    "accept-offer",
    "abcdef123456"
  ]
}

## Command response

{
  "ok": true,
  "returncode": 0,
  "stdout": "...",
  "stderr": ""
}

## State request

{
  "op": "state",
  "role": "bob",
  "swap_id": "abcdef123456"
}

The implementation returns its current durable swap record in JSON.

The Python reference bridge currently receives an additional
`state_path` field as test metadata because Python state is stored as
files. Moxie does not need to use that field or that storage model.

## Requirements

An implementation adapter must:

- execute commands as exactly one protocol role;
- keep Alice and Bob private state isolated;
- return durable state, not speculative in-memory state;
- preserve command success versus refusal;
- never expose Alice's private S merely to satisfy a test;
- permit the harness to control scheduling and regtest block production.

It does not have to reproduce:

- Python function names;
- Python state paths;
- Python exception strings;
- Python flock behavior;
- process architecture;
- wallet-generated transaction IDs.

## Differential combinations

The harness is intended to run:

- Python Alice / Python Bob
- Python Alice / Moxie Bob
- Moxie Alice / Python Bob
- Moxie Alice / Moxie Bob

against identical seeded scenarios.
