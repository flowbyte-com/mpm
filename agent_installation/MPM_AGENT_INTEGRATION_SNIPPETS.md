<!-- mpm_agent_integration_version: 4.0.0 -->

# MPM Agent Integration — Canonical Managed Instruction Snippets

> **Status:** canonical source. This file is the **single source of truth**
> for the **exact agent-facing instruction text** that host adapters place
> into their managed MPM section. Behavioural principles live in
> [`mpm-agent-protocol.md`](./mpm-agent-protocol.md); the wording and
> tool-invocation details that agents actually read live here.
>
> **Default assumption:** one universal managed block is shared across every
> supported host. Host-specific textual variants exist only where the
> implementation evidence proves them necessary. Tool-namespace prefixes
> are a transport fact (rendered into the installed block at install time)
> — not a textual variant of the block content. Host-specific file paths,
> hooks, recovery notes, and transport mechanics belong in the host
> adapter (header / footer), not in the universal block.
>
> **Default MCP surface (Sept 2026 launch):** the model initially sees
> three MCP tools (`mpm_memory`, `mpm_context`, `mpm_help`). The full
> 22-tool substrate (21 Registry entries + the `mpm_help` discovery
> closure) is reachable via `mpm_help list` discovery + the
> `mpm call <tool> --payload '…'` CLI fallback, and is fully restored
> on hosts that set `MPM_EXPOSE_ALL_TOOLS=1` in their MCP env block.
> Discovery itself is reachable over that CLI fallback
> (`mpm call mpm_help --payload '{"action":"list"}'`), so a shell-only
> agent can enumerate the surface without a hard-coded tool list.
> `mpm_handoff` is reachable on the compact surface through
> `mpm_context action=write_handoff` / `read_handoff`.
>
> **The managed block below is a behaviour matrix.** Since contract
> `4.0.0` it names the **nine** write destinations — `mpm_memory`,
> `mpm_decisions`, `mpm_lessons`, `mpm_evidence`, `mpm_theories`,
> `mpm_work`, `mpm_skills`, `mpm_references`, `mpm_handoff` — and the
> action each one takes, plus the explicit "nothing" row that keeps
> persistence from becoming a reflex. It still does not restate tool
> documentation: the per-tool action catalogue and required params live
> in the tool-reference stability table further down this file, the full
> behavioural contract lives in `mpm-agent-protocol.md`, and anything
> else is reachable through `mpm_help` discovery. The matrix exists
> because the agent's most expensive mistake on this substrate is
> writing the *wrong kind of artifact*; naming the destination is
> cheaper than letting the agent guess it.
>
> **The OpenCode adapter registers its own 17 typed tools** through
> OpenCode's plugin API, so it is NOT bound by the compact three-tool
> `mpm-mcp` surface described above — every tool the matrix names is a
> native OpenCode tool there. (`mpm_work` is the exception: it is a
> registry tool that reaches OpenCode through the `mpm call <tool>` CLI
> fallback the block names.)

---

# Copy/paste examples

Copy the complete block into the indicated file.

> The blocks below are produced by `scripts/render_managed_blocks.py`
> from the canonical managed block in this file. Drift detection tests
> (`tests/test_render_managed_blocks.py`) assert that each block below
> is byte-for-byte identical to the rendered canonical block, that each
> adapter template snippet matches the rendered version, and that the
> installer-written block matches the rendered version.
>
> OpenCode is the only supported host in this release. The Claude Code,
> Pi, Hermes and OpenClaw copy/paste blocks were removed with their
> adapters on 2026-10-27; the canonical managed block further down is
> host-neutral and unchanged.

## OpenCode

**Target file:** `<project>/AGENTS.md` (project scope) or
`~/.config/opencode/AGENTS.md` (user scope)
**Scope:** both project and user scopes supported; picker lives in
the plugin install hook
**Marker convention:** the block must sit between
`<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->` and
`<!-- END MPM-MANAGED SECTION:opencode-instructions -->`.
**Manual verification (no installer):** the `mpm-opencode` plugin
reads `AGENTS.md` at session start AND injects wake context
automatically via its `experimental.chat.system.transform` hook.
Verify with `opencode run` against a project containing the file;
wake should appear in the system prompt before the first turn.

```markdown
<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->

# MPM — durable knowledge for every agent

**Reusable knowledge belongs in shared MPM records** — any agent can retrieve
them without knowing which session found them. **A handoff carries execution
state, not knowledge**: it references those records. Your session ends; your
knowledge need not. **Do not make your successors rediscover what you already
learned.**

| Situation | Record |
|---|---|
| Reusable fact, constraint, root cause | `mpm_memory` action `save` |
| Choice and rationale | `mpm_decisions` action `record` |
| Correction, recurring pitfall, technique | `mpm_lessons` action `save` |
| Supporting or contradicting observation | `mpm_evidence` action `add` |
| Unverified hypothesis — never a fact or lesson | `mpm_theories` action `propose` |
| Objective spanning sessions | `mpm_work` action `create` |
| Procedure that has proved itself | `mpm_skills` action `save` |
| External document to ingest for later consultation | `mpm_references` action `add` |
| Unfinished work or session transition | `mpm_handoff` action `write` |
| Routine or transient information | nothing |

**Persist as soon as it becomes useful**, not at session end — you need not
predict your session ending.

**Handoff** — a title is not continuation state. `summary` for what was
completed; `commitments` for unfinished work, next actions and
files/branches/PRs; `open_questions` for blockers; shared-record ids
alongside — reference records, don't copy them. Write one for a transfer, a
finished substantial task, or before you stop; not for an acknowledgement, not
after every tool call. **No signal reliably marks a session end**: turn
completion, idle, archive and shutdown are not one.

**When your work changes what is known**, supersede or flag the affected
record and say why; do not rewrite history.

Wake context arrives at session start (recover it through `mpm_context` using
`read_wake_context` if not); follow its pointers — they point into durable
knowledge, not a complete or unquestionable account, and stored state may be
outdated or superseded. Discover unfamiliar operations with `mpm_help` action
`show`; `mpm call <tool>` reaches what the host does not expose.
References are a distinct corpus, not memories: read the underlying source rather than relying solely on a search-result summary.
Check its version and freshness. Full contract:
`~/.mpm/agent_installation/mpm-agent-protocol.md`.
<!-- END MPM MANAGED BLOCK -->

<!-- END MPM-MANAGED SECTION:opencode-instructions -->
```

# The canonical managed block (host-neutral)

The block below is the **universal managed block** — host-neutral,
transport-rendered at install time. The canonical block uses bare
canonical tool names (`mpm_handoff`, `mpm_memory`, ...). The host
adapter's renderer applies each host's tool-namespace prefix at
install time, producing the rendered output that goes into the
agent's persistent instruction file. The result is one canonical
block, four transport-rendered variants, byte-equivalent for
behaviour.

To regenerate the four host variants (after editing this canonical
block or any host's header/footer):

```
python3 scripts/render_managed_blocks.py
```

To verify byte-for-byte parity without writing:

```
python3 scripts/render_managed_blocks.py --check
```

The four copy/paste examples at the top of this file are produced
by the same render, byte-equivalent to what the per-host adapter
template snippet contains and what each per-host installer writes
into the target persistent file.

```text
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->

# MPM — durable knowledge for every agent

**Reusable knowledge belongs in shared MPM records** — any agent can retrieve
them without knowing which session found them. **A handoff carries execution
state, not knowledge**: it references those records. Your session ends; your
knowledge need not. **Do not make your successors rediscover what you already
learned.**

| Situation | Record |
|---|---|
| Reusable fact, constraint, root cause | `mpm_memory` action `save` |
| Choice and rationale | `mpm_decisions` action `record` |
| Correction, recurring pitfall, technique | `mpm_lessons` action `save` |
| Supporting or contradicting observation | `mpm_evidence` action `add` |
| Unverified hypothesis — never a fact or lesson | `mpm_theories` action `propose` |
| Objective spanning sessions | `mpm_work` action `create` |
| Procedure that has proved itself | `mpm_skills` action `save` |
| External document to ingest for later consultation | `mpm_references` action `add` |
| Unfinished work or session transition | `mpm_handoff` action `write` |
| Routine or transient information | nothing |

**Persist as soon as it becomes useful**, not at session end — you need not
predict your session ending.

**Handoff** — a title is not continuation state. `summary` for what was
completed; `commitments` for unfinished work, next actions and
files/branches/PRs; `open_questions` for blockers; shared-record ids
alongside — reference records, don't copy them. Write one for a transfer, a
finished substantial task, or before you stop; not for an acknowledgement, not
after every tool call. **No signal reliably marks a session end**: turn
completion, idle, archive and shutdown are not one.

**When your work changes what is known**, supersede or flag the affected
record and say why; do not rewrite history.

Wake context arrives at session start (recover it through `mpm_context` using
`read_wake_context` if not); follow its pointers — they point into durable
knowledge, not a complete or unquestionable account, and stored state may be
outdated or superseded. Discover unfamiliar operations with `mpm_help` action
`show`; `mpm call <tool>` reaches what the host does not expose.
References are a distinct corpus, not memories: read the underlying source rather than relying solely on a search-result summary.
Check its version and freshness. Full contract:
`~/.mpm/agent_installation/mpm-agent-protocol.md`.
<!-- END MPM MANAGED BLOCK -->

```

# Manual installation per host

The host installs the universal block by render-then-paste (or by running
the per-host installer, which does the same job). The target file, host
wrapper markers, and verification command for the host.

| Host | Target file | Marker convention | Verification command |
|---|---|---|---|
| OpenCode | `~/.config/opencode/AGENTS.md` (user) or `<project>/AGENTS.md` (project) | `<!-- BEGIN/END MPM-MANAGED SECTION:opencode-instructions -->` | open a project in OpenCode; agent's system prompt contains MPM wake content |

For the host, when editing the persistent file by hand:

- Keep the canonical managed block between the host wrapper markers.
- Do not edit the host wrapper markers themselves.
- Do not put MPM instructions in any persona file —
  persona and protocol are kept separate.
- Do not duplicate this block. Each target file receives one and only one
  MPM-managed section.

# Tool-reference stability contract

The block above pins the action enum values that agents see. Changes to
the underlying schema require regeneration of this file. Verified against
the live registry at `internal/core/tools/registry_list.go` on the date
in this file's git log.

| Tool | Action values | Required params on write |
|---|---|---|
| `mpm_handoff` | `write`, `read`, `list`, `shred` | `write` requires `summary`. `state` enum: `clean` `crashed` `interrupted` `force_end`. No `note` parameter. |
| `mpm_scratchpad` | `flush`, `read`, `discard`, `promote` | — |
| `mpm_memory` | `save`, `query`, `show`, `delete`, `restore`, `reinforce`, `weaken`, `snooze`, `set_weight`, `patch`, `promote`, `review`, `synthesize`, `challenge`, `restore_challenge`, `commit_milestone` | `query` defaults `projection: "summary"`; full via `projection: "full"`. `delete` is a reversible soft delete; `restore` clears the tombstone. Permanent shredding is operator-only (`mpm memory shred <id>` from a terminal) and is refused on every agent surface — no payload field, including `confirm=true`, unlocks it. |
| `mpm_decisions` | `record`, `supersede`, `invalidate` | — |
| `mpm_lessons` | `save`, `search`, `list`, `delete`, `restore` | `save` defaults `type` to `"insight"` if absent; valid values: `"insight"`, `"warning"`, `"practice"`; lifecycle: `delete` is reversible soft delete (sets tombstone), `restore` clears the tombstone. Permanent shredding is operator-only (`mpm lesson shred <id>`) and is refused on every agent surface. All lifecycle actions accept canonical `id` (alias: `lesson_id`). |
| `mpm_topics` | `create`, `search`, `link` | — |
| `mpm_references` | `add`, `read`, `search`, `list` | freshness surfaced as `current` `stale` `version-bound` `historical` `unknown` |
| `mpm_context` | `read_wake_context`, `read_directives`, `proactive_recall_hint`, `query_global_rules`, `record_global_rule`, `promote_to_global`, `route` | `read_wake_context` accepts `params.projection: "compact"` |
| `mpm_skills` | `save`, `read`, `list`, `delete`, `promote_to_global`, `workshop` | `workshop` requires `intent`, `mode`, `task_context`, `workflow_description`, `failure_recovery`, `recent_actions`, `evidence` |
| `mpm_wakes` | `schedule`, `check`, `check_pending_event`, `list`, `digest`, `upsert_task`, `list_tasks`, `delete_task` | — |
| `mpm_work` | `create`, `list`, `show`, `update`, `complete`, `cancel`, `history`, `note`, `reopen`, `resolve_contradiction` | `complete` requires `work_id`; host session termination does NOT auto-trigger |
| `mpm_resolve` | (no `action`) | `uri: mpm://...` |
| `mpm_blob_read` | (no `action`) | `id`, `offset`, `max_bytes` |
| `mpm_blob_search` | (no `action`) | `id`, `query` |
| `mpm_memory` action=`challenge` | `memory_id`, `evidence` | Weaken a memory; creates a pending theory contesting it. Companion: `restore_challenge` action undoes. |
| `mpm_retrieval_diagnose` | (no `action`) | `query`, `limit`, `collection`, `scope`, `trace` |
| `mpm_system` | `gc_run`, `compact`, `health_check`, `migrate`, `query_audit_log`, `list_clusters`, `snooze_cluster`, `resolve_cluster`, `annotate_cluster`, `critic_findings` | — |

Companion standalones (transport-namespace may differ per host):

- `mpm_log_to_changelog` (`fact`, `commit_hash`, `tags`, plus optional `confirms_lesson_id`/`confirms_decision_id`/`confirms_theory_id` and `contradicts_lesson_id`/`contradicts_decision_id`/`contradicts_theory_id` for explicit epistemic assertions — see `docs/library/epistemic-confirmation.md`)
- `mpm_request_review` (`components`, `prompt`)

# Maintenance

Edit the canonical managed block above to change what every host
learns. Then run the render script to propagate. Drift detection
tests fail if the rendered output and the checked-in artifacts
diverge.

## Instruction budget policy

**Instruction budgets are optimisation safeguards, not correctness
constraints. Essential capability discovery and operational guidance
must never be omitted merely to satisfy an arbitrary size ceiling.**

The managed block exists to improve *total* agent efficiency, not to
minimise its own token count. A slightly longer instruction that
prevents repeated searches, unnecessary tool calls, incorrect
conclusions, or rediscovery of information MPM already holds is
strongly net-positive: each avoided misfire costs far more context
than the instruction ever will. An agent that concludes "MPM does not
have this documentation" because nothing told it references are a
separate subsystem will spend a long time re-deriving what is already
stored.

**What the always-on surface owes an agent.** The managed block is
delivered into *every* system prompt, so its cost is paid on every
turn, by every agent, forever. That is the constraint that made
contract `4.0.0` (2026-10-28) replace the 11,225-byte prose contract
with a 2,526-byte behaviour matrix: the ten prose responsibilities
were a good contract and a bad *always-on* surface.

**What the always-on surface does not owe.** The elaboration behind
each commitment. That lives in
[`mpm-agent-protocol.md`](./mpm-agent-protocol.md), which the block
names by path. The architecture is deliberately layered:

| Layer | Content | Cost |
|---|---|---|
| the managed block (always on) | the behaviour matrix, the persistence trigger, the uncertainty rule, the handoff contract, the wake/recovery path | paid every turn |
| the canonical protocol (on demand) | the full behavioural contract, maintenance semantics, recovery procedures, freshness rules | paid when the agent reads it |
| the tool-reference stability contract (in this file) | the per-tool action enums and required params | paid when the agent reads it |

The line between the layers is a judgement, and it is made on one
question: **would an agent that never opens the deeper layer get the
behaviour wrong?** If yes, the rule belongs in the managed block. If
no — because the deeper layer is consulted precisely when the agent
needs it — the rule belongs behind the pointer.

The current limits are **3,100 bytes and 460 words** for the
host-neutral canonical block. They are **maintainability warnings and
review triggers**, not budgets to optimise against:

* Do **not** remove useful guidance, compress an instruction until it
  is ambiguous, or sacrifice discoverability to stay under a ceiling.
* If an essential instruction genuinely needs the room, raise the
  ceiling. Say so in review. Do not mutilate the instruction instead.
* If the block is already correct and verbose, tighten the *wording* —
  that is a legitimate improvement. What is not legitimate is
  deleting a capability to buy headroom.

What the ceilings do still guard is **accidental** growth: duplicated
sentences, restated tool documentation, and mechanical reformatting
that balloons a one-line change. Those are the regressions a size
gate catches cheaply, and `test_render_managed_blocks.py` carries that
coverage (byte ceiling, word ceiling, duplicate-sentence detection,
and per-invariant concept anchors).

This policy governs every supported host: all adapters render from this
one canonical block, so it applies uniformly, and to the copy/paste
examples and README rendered from it.

## Updating the managed instructions

1. Edit the canonical managed block above.
2. Update the copy/paste examples at the top of this file to
   match the new canonical block (the render script verifies them
   byte-for-byte; it does not rewrite the surrounding prose).
3. Update the behavioural protocol
   ([`mpm-agent-protocol.md`](./mpm-agent-protocol.md)) if the
   underlying contract changed.
4. Run `python3 scripts/render_managed_blocks.py` to regenerate
   the host template snippets.
5. Run `python3 scripts/render_managed_blocks.py --check` for an
   idempotency assertion (no install required for verification).
6. Run `make test` (or the relevant Go test target) to cover the
   full parity + clean-install + cross-adapter test surface.

## Ownership rule

The render script's output is the **only** source of truth for the
text inside each host's managed section. Do not hand-edit the per-host
`templates/*.snippet` files or any installed persistent-file block —
those changes will be lost on the next render or install. If the
canonical block needs to change, edit it in this file.

Host-specific instructions (file paths, scope choices, hook wiring,
recovery notes) belong in the per-host adapter's header/footer
sections, **outside** the canonical managed block. Add or change
those directly in `scripts/render_managed_blocks.py` (the
`ADAPTERS` table carries per-host `header` and `footer` strings).

# Versioning

This file's top-of-file HTML comment carries the **managed-instruction
contract version**:

```
<!-- mpm_agent_integration_version: MAJOR.MINOR.PATCH -->
```

The version pins the contract that adapters render into each host's
managed section. It is independent of the MPM software release
version (e.g., `v0.1.0-alpha-final`). Parity byte-for-byte remains
the authoritative drift detector; the version marker is a coarse
signal for downstream adapters that want to pin to a known contract
shape.

## When to bump

- **patch** (1.0.0 → 1.0.1) — typo fixes, comment clarifications,
  formatting, prose tightening; no semantic change to the rendered
  contract. Example: fixing a wording glitch inside the canonical
  block.
- **minor** (1.0.0 → 1.1.0) — additive — new optional tool/param
  documented in the tool-reference stability contract table, new
  tool added, new host added, expanded examples; existing
  contracts still resolve under the old version.
- **major** (1.0.0 → 2.0.0) — breaking — managed-block marker
  convention changes, required field added to an existing tool,
  action rename, render-contract change that would force adapter
  re-verification. Host adapters MUST be re-verified against the
  new contract.

Bump this marker when the canonical managed block or the
tool-reference stability contract table changes in a way that
matters to consumers. The marker does **not** replace byte-for-byte
parity; the render script's `--check` mode remains the authoritative
drift detector.

### Why the current marker is `4.0.0`

The contract was `3.0.0` from October 2026, when the managed block was
replaced wholesale by an inheritance-oriented contract: ten prose
responsibilities in place of the previous seven invariants.

It is `4.0.0` from 2026-10-28, when the managed block was replaced
wholesale again — this time by the **compact behaviour-matrix** block.
Two things changed at once, and both are major-bump criteria:

1. **The rendered block is a different document.** The ten prose
   responsibilities are gone from the always-on surface. They are not
   gone from the *contract*: each one is carried by the canonical
   behavioural protocol, and the compact block names the operational
   core — the behaviour matrix, the persistence trigger, the uncertainty
   rule, the handoff contract, and the wake/recovery path. The protocol
   is reachable from the block by path, so the detailed commitments are
   one read away rather than permanently in the system prompt.

2. **The block now enumerates nine write destinations and their
   actions.** Contract `3.0.0` was "deliberately behavioural rather than
   enumerative": it named only the tool it invoked (`mpm_context`, with
   `read_wake_context`) and left the per-tool action catalogue to
   `mpm_help` discovery. `4.0.0` replaces that with an explicit
   behaviour matrix, because the agent's most expensive mistake on this
   substrate is writing the *wrong kind of artifact* — a fact where a
   decision belonged, a theory where a lesson belonged. Naming the
   destination costs about 130 tokens and removes the guess.

This is a **major** bump, not a minor one. A minor bump means
"existing contracts still resolve under the old version"; they do not
here — the rendered block an adapter installs is a different document,
so each adapter must be re-verified against the new contract, which is
exactly the major-bump criterion. The MPM **product** version did not
move with it: this marker versions the behavioural contract, not the
software, and changing the text agents read is not a release of the
binaries.

## Installed-block provenance (added at install time)

An installed managed block records where it came from. Immediately after
the inner `<!-- BEGIN MPM MANAGED BLOCK -->` marker, the installer
writes one HTML comment whose fields are:

| Field | Meaning |
|---|---|
| `version` | the MPM **product** version of the build that wrote the block |
| `commit` | the exact **source commit** that build was made from |
| `protocol` | the behavioural protocol version — the `mpm_agent_integration_version` declared by this file |
| `applied` | the UTC moment the block was last applied or updated |

The comment's exact text is **not reproduced here on purpose**. This
file is the single source of truth for the exact agent-facing
instruction text, and it is guarded against carrying per-install
metadata — a per-build commit SHA or a timestamp baked into it would
make every byte-parity gate drift on every build, and a literal example
of the comment would put that metadata one copy-paste away from every
host. The rendered form is documented in
`agent_installation/README.md`, which is where the managed-block
convention lives. The mechanism itself is
`scripts/block_provenance.py`, and the prefix it emits is
`block_provenance.PROVENANCE_PREFIX`.

The `protocol` field carries the `mpm_agent_integration_version` from
the top of this file. It is **independent** of the product version: it
versions the behavioural contract, not the software, and the two move
on separate schedules. Neither is ever substituted for the other.

The product version and the source commit come from the environment
(`MPM_BUILD_VERSION` / `MPM_BUILD_COMMIT`), which `install.sh` and
`make refresh-installed` both export from the canonical build script
(`scripts/build-provenance.sh`). An adapter installer run on its own —
outside a build — may establish neither, and then it writes `unknown`
rather than claiming a version it cannot establish. It does not erase an
established value either: when the behavioural text is unchanged, the
value the block already carries is kept.

**The comment is not part of the canonical text.** It is per-install
metadata layered on top of it, and that is deliberate: the drift gates
(snippet, copy/paste, README, installed-drift) normalise the provenance
comment away on both sides of their comparison, so they still compare the
behavioural text — which is what they are about — byte for byte.

**The timestamp records the last change, not the last run.** It is
preserved when the version, the commit, the protocol version and the
behavioural text are all unchanged, and refreshed whenever any of them
changes. Re-running the installer with the same version, commit and
instructions is a genuine no-op: same bytes, same timestamp, no new
backup. (The same rule already governed the `<!-- generated: ... -->`
line.)

The mechanism lives in `scripts/block_provenance.py` and is shared by
every host adapter, so the convention is identical across all of them.
