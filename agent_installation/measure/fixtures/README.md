# Behavioural-comparison fixtures

The behavioural comparison (`../block_behaviour_comparison.py`) measures
what an OpenCode agent **persists** under two different managed
instruction surfaces. Each fixture here is one arm's world.

| Fixture | What it is | Used by |
|---|---|---|
| `block-3.0.0.md` | The **verbatim** managed block from managed-instruction contract **3.0.0** — the block `origin/main` installs and the one live in `~/.config/opencode/AGENTS.md` today. | the "old" arm's `AGENTS.md` |
| `protocol-3.0.0.md` | The canonical behavioural protocol **as it stood at contract 3.0.0**. | the "old" arm's `~/.mpm/agent_installation/` |
| `snippets-3.0.0.md` | The canonical managed-instruction source **as it stood at contract 3.0.0** (its managed block is byte-identical to `block-3.0.0.md`). | the "old" arm's `~/.mpm/agent_installation/` |

The "new" arm uses the **live tree**: the current
`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` and
`agent_installation/mpm-agent-protocol.md`.

## Why each arm gets a whole reference stack

**The arm is not just the block.** Both blocks point at
`~/.mpm/agent_installation/mpm-agent-protocol.md`, and an agent that
follows that pointer *reads it* — which is exactly what the contract
tells it to do.

That was measured, not assumed. A first run of the "old" arm had no
`~/.mpm` at all, so the agent went looking for the protocol, found the
real repository's copy, and read the **4.0.0** commitments it was
supposed to be compared against. The cell was contaminated, and the
harness now reports contamination rather than counting it.

So the harness builds each arm's `~/.mpm/agent_installation/` from
these fixtures:

- **old arm** → the 3.0.0 protocol and the 3.0.0 canonical source.
  That is the world that shipped.
- **new arm** → the 4.0.0 protocol and the 4.0.0 canonical source.
  That is the world this branch proposes.

Comparing the two **worlds** is the question the release decision asks.
Comparing two *blocks* over one fixed reference stack would flatter the
old arm: the 4.0.0 protocol carries the C1–C12 commitments that the
old block used to carry in prose, so serving it to the old arm would
hand that arm the very content under test.

## Provenance

`block-3.0.0.md` is the LAST
`<!-- BEGIN MPM MANAGED BLOCK -->` … `<!-- END MPM MANAGED BLOCK -->`
occurrence in:

```
git show origin/main:agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md
```

`protocol-3.0.0.md` and `snippets-3.0.0.md` are the whole files at the
commit that adopted contract 3.0.0:

```
git show f19fb24:agent_installation/mpm-agent-protocol.md
git show f19fb24:agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md
```

The block fixture is byte-for-byte, including its own
`<!-- BEGIN … -->` / `<!-- END … -->` markers and the provenance
comments inside it. **These files are not edited**: they are the
comparison baseline, and any change to one would invalidate every
comparison run against it.

**Size of the block fixture.** 11,225 bytes / 1,564 words / 2,009
tokens (`cl100k_base`).

**Why the LAST occurrence.** The canonical source carries the block
twice: once as the OpenCode copy/paste example, and once as the
host-neutral canonical block that every adapter renders from. The
renderer reads the LAST one. The fixture is the same text, taken the
same way, so the two arms of the comparison are directly comparable.

**How to regenerate** (only if the upstream file changes — which would
itself be a finding worth reporting):

```bash
git show origin/main:agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md \
  | python3 -c '
import re, sys
blocks = re.findall(
    r"<!-- BEGIN MPM MANAGED BLOCK -->\n.*?<!-- END MPM MANAGED BLOCK -->\n",
    sys.stdin.read(), re.DOTALL,
)
sys.stdout.write(blocks[-1])
' > agent_installation/measure/fixtures/block-3.0.0.md

git show f19fb24:agent_installation/mpm-agent-protocol.md \
  > agent_installation/measure/fixtures/protocol-3.0.0.md
git show f19fb24:agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md \
  > agent_installation/measure/fixtures/snippets-3.0.0.md
```
