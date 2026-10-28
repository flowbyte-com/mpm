# `agent_installation/measure/` — verification harnesses

This directory holds the harnesses that **verify** MPM's agent-installation
surface. They are not part of the installation machinery — nothing in
`install.sh`, `reconcile_managed_blocks.py` or any host adapter invokes
them, and they are not shipped to an operator's machine.

They live in their own directory for one reason: **two of them mutate
the canonical source in place.** A harness that edits
`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` needs a home that
is clearly separate from the scripts it mutates, so that "this is a
verification tool, not part of the installation path" is obvious from
the directory listing alone.

| Harness | What it does | Writes to durable state? |
|---|---|---|
| [`mutation_probe.py`](mutation_probe.py) | Proves the managed-block tests are **non-vacuous**: it mutates the behaviour under test, runs the named test, and asserts the test goes **RED**. Every mutation is reverted and the tree is verified byte-identical afterwards. | No — it reverts everything it changes. |
| [`installer_regression.py`](installer_regression.py) | Runs the real OpenCode installer through four paths — fresh install, upgrade, reinstall, existing-block replacement — plus the retired-host safety check, against a **disposable `HOME`** and redirected XDG roots. | No — `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME` and `XDG_CACHE_HOME` all point into a temporary directory. |
| [`block_behaviour_comparison.py`](block_behaviour_comparison.py) | Runs the **real installed `opencode` host** against a disposable project, workspace and database, and measures what the agent **persists** under two different managed instruction surfaces. | No — every root is redirected; the live installation, the live database and the live OpenCode configuration are never written. |

The fixtures the behavioural comparison needs — the verbatim 3.0.0
managed block, and the 3.0.0 protocol and canonical source that the
"old" arm is served — are in [`fixtures/`](fixtures/), with their
provenance documented in [`fixtures/README.md`](fixtures/README.md).

## Running them

```bash
# Non-vacuity probe. Exit code 0 means every mutation was detected.
python3 agent_installation/measure/mutation_probe.py

# Installer regression. Writes a JSON envelope with --json.
python3 agent_installation/measure/installer_regression.py --json /tmp/installer-regression.json

# Behavioural comparison. Writes a JSON envelope with --json.
python3 agent_installation/measure/block_behaviour_comparison.py --json /tmp/behaviour-comparison.json
```

The first two need the `mpm` binary built (`make build`) for the
installer path and the discovery suite respectively; the third needs
the `opencode` binary on `PATH` or at `~/.opencode/bin/opencode`.

**None of them is a CI gate.** They are evidence harnesses: they were
run to produce the numbers in
[`docs/dev/session-c-compact-managed-block.md`](../../docs/dev/session-c-compact-managed-block.md),
and the harnesses themselves are the evidence that those numbers were
not produced by a test that cannot fail.
