# `~/.mpm/agent_installation/` — Agent Integration Surfaces

This directory holds the canonical agent-facing integration code for MPM.
Each supported host gets one subdirectory here. **OpenCode is the only
supported host in this release** — `mpm-opencode/` is the only adapter
directory, and `lib/`, `scripts/` and `tests/` are host-independent.

> **Invariant:** Any new integration code, scripts, manifests, skills,
> extensions, plugins, adapters, or onboarding helpers for an MPM-aware
> agent lives under `~/.mpm/agent_installation/<agent>/`. MPM core
> (`internal/`, `cmd/`, `src/`) is intentionally untouched by these
> adapters.
>
> The MPM machine-facing interface (`mpm call <tool> --payload '{"action":"…","params":{…}}'`
> + the `mpm-mcp` stdio server) is the contract. Adapter directories
> consume that contract — they do not import internal MPM packages.

## I just want the snippets

**→ [`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md)**

The top of that file carries the copy/paste block for the supported
host (OpenCode) — with the exact wording to paste into the host's
persistent instruction file (`~/.config/opencode/AGENTS.md`). The same
content is what the host's installer writes; the installer is a
convenience, not a requirement.

For installation, the rest of this README explains the architecture;
the actual procedural manual is [INSTALL.md](./INSTALL.md).

## What this directory is

A bundle of per-agent installation assets:

| Kind | Examples | Purpose |
|---|---|---|
| **Plugins / extensions** | `mpm-opencode/` | Native host extension code (TypeScript) |
| **Behavioral-instruction installers** | `mpm-opencode/` | Snippet + Python installer that writes the MPM behavioral protocol into the host's persistent instruction file (`AGENTS.md`) |
| **Host-independent libraries** | `lib/` | Code that belongs to no single host (the telemetry adapter) — retiring a host must not take it with it |
| **Canonical protocol doc** | `mpm-agent-protocol.md` | Host-independent behavioral contract — the single source of truth that host adapters reference |

The name `agent_installation/` (rather than `agent_plugins/`) reflects
this mix: only a subset of the directories are plugins in the strict
sense; the rest is install scripts, shared libraries, and protocol
documentation.

## Design principle

> **MPM provides capability. Host instructions make that capability
> part of normal agent behavior.**

A plugin wires MPM's tools into the agent (capability).
A behavioral-instruction installer writes the MPM protocol into the
host's persistent instruction file (`AGENTS.md`), so the agent actually
invokes wake-context, persists during work, writes handoffs at session
end, and falls back to `mpm call` when the preferred integration
breaks.

Without the second layer, capability may exist but adoption is
inconsistent — agents may skip wake, persist nothing, and leave no
handoff, forcing the next session to rediscover everything.

## Canonical behavioral protocol

[`mpm-agent-protocol.md`](./mpm-agent-protocol.md) is the canonical
host-independent contract. It defines the **principles** an MPM-aware
agent must honour:

1. **Wake** at session start (read MPM context before substantive work)
2. **Persist** during work, not only at the end
3. **Handoff** before genuine session closure
4. **MPM is the source of truth** for cross-session continuity
5. **Recovery / fallback** to `mpm call <tool> --payload ...` when the
   preferred integration breaks

All host adapters reference this file. They do not duplicate the
principles; they translate them into host-native instruction surfaces
and MCP-bundle wiring.

## Canonical managed-instruction snippets

[`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md)
is the canonical source for the **exact wording** that adapters render
into each host's managed section. It contains the canonical managed
block (between `<!-- BEGIN MPM MANAGED BLOCK -->` and
`<!-- END MPM MANAGED BLOCK -->` markers). The block uses **bare canonical
tool names** — `mpm_memory`, `mpm_handoff`, and so on. It carries no
placeholder syntax; the renderer applies a host's transport namespace
afterwards, if that host needs one.

The file-based adapter snippets
(`mpm-opencode/templates/AGENTS.md.snippet`) are **generated**, not
hand-edited. The render script
[`scripts/render_managed_blocks.py`](./scripts/render_managed_blocks.py):

1. Reads the canonical source.
2. Extracts the canonical managed block.
3. Applies the adapter's tool-namespace prefix by word-boundary
   substitution on the leading `mpm_` of each tool name. OpenCode's prefix
   is **empty** — it registers the bare canonical names (see the
   capability-name note below), so the render is byte-identical to the
   canonical block.
4. Composes each adapter's full snippet (header + rendered block +
   host-specific notes).
5. Writes the snippet to the adapter's `templates/` directory.

**Drift detection:** the test suite
[`tests/test_render_managed_blocks.py`](./tests/test_render_managed_blocks.py)
verifies byte-for-byte parity between the checked-in snippets and the
rendered output. Run `python3 scripts/render_managed_blocks.py --check`
to verify; re-run without `--check` to regenerate after editing the
canonical source.

## Supported hosts

**OpenCode is the only supported agent host in this release.** The
Claude Code, Pi, Hermes and OpenClaw adapters were retired on
2026-10-27; their directories are gone from the tree.

Retiring an adapter stops MPM from CONFIGURING that host. It does not
touch an integration an operator already has: an existing
`~/.claude/CLAUDE.md`, `~/.pi/agent/AGENTS.md`,
`~/.openclaw/workspace/SOUL.md` or `.hermes.md` is left exactly as it
is, and the diagnostic no longer reports it (a retired host is not
MPM's business any more, and reporting it would tell the operator to
repair an integration MPM no longer supports).

| Host | Release status | Adapter directory | Mechanism | Persistent-instruction surface |
|---|---|---|---|---|
| **OpenCode** | **supported** | [`mpm-opencode/`](./mpm-opencode/) | OpenCode plugin (TypeScript, 17 typed tools + `mpm call` CLI fallback to the full MPM Registry) + AGENTS.md managed block | `<project>/AGENTS.md` or `~/.config/opencode/AGENTS.md` (managed-block convention) |

> **The MPM substrate is host-independent.** The `mpm` CLI, `mpm call
> <tool> --payload '<json>'`, and the `mpm-mcp` stdio server work from any
> host. What an unsupported host lacks is the *managed behavioural block*
> and host-native wake-context delivery.

## Directory map

| Directory | What it contains |
|---|---|
| `mpm-opencode/` | TypeScript plugin (`src/index.v2.ts`, 17 typed tools + `mpm call` fallback to full registry) + AGENTS.md behavioral-protocol installer (snippet + Python installer) |
| `lib/` | Host-independent libraries (the telemetry adapter) |
| `scripts/` | Shared host-independent tooling: the renderer, the reconciler, the diagnostic, the managed-block convergence engine, the runtime-package staging library |
| `tests/` | The cross-adapter contract suite (renderer parity, reconciliation, provenance, diagnostics) |

The adapter directory's own README is the host-specific deep dive. See
[INSTALL.md](./INSTALL.md) for installation procedures.

## Installation

**See [INSTALL.md](./INSTALL.md)** for the full installation manual —
prerequisites, exact commands per host, scope choices, verification,
uninstall, troubleshooting, and provenance configuration.

One-line entry points (full procedure in INSTALL.md):

| Host | Install |
|---|---|
| **OpenCode** (supported) | `cd mpm-opencode && ./install.sh` — builds the payload, registers the plugin in `~/.config/opencode/opencode.jsonc`, and refreshes the AGENTS.md managed block |

To remove the OpenCode MPM integration cleanly (without touching the
MPM substrate):

```bash
cd mpm-opencode && ./install.sh --uninstall
```

It removes MPM's entry from `~/.config/opencode/opencode.jsonc` and
strips the managed block from `~/.config/opencode/AGENTS.md`.
Unrelated plugins, other config sections, comments, and any AGENTS.md
text outside the managed-block markers are preserved.

## Documentation map

| Document | What it covers |
|---|---|
| [`README.md`](./README.md) | This file — orientation, supported hosts, design principle |
| [`INSTALL.md`](./INSTALL.md) | Procedural installation manual — prerequisites, commands per host, verification, uninstall, troubleshooting |
| [`mpm-agent-protocol.md`](./mpm-agent-protocol.md) | Canonical behavioral protocol — principles an MPM-aware agent honours |
| [`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md) | Canonical managed-instruction snippets — exact wording rendered into each host's managed section |
| [`scripts/render_managed_blocks.py`](./scripts/render_managed_blocks.py) | Renders per-adapter snippets from the canonical source |
| [`tests/test_render_managed_blocks.py`](./tests/test_render_managed_blocks.py) | Byte-for-byte drift detection for adapter snippets |
| `<host>/README.md` or `<host>/SKILL.md` | Host-specific deep dive (validation evidence, host quirks, recovery details) |

## Validation record

| Date | Host | Verdict | Evidence |
|---|---|---|---|
| 2026-08-19 | OpenCode | **READY** | Tests A–E live-verified via fresh `opencode run` sessions + plugin-level tests. Plugin required repair (missing `id` export) before it could load. See [`mpm-opencode/VALIDATION-2026-08-19.md`](./mpm-opencode/VALIDATION-2026-08-19.md). |

The validation records of the retired adapters were removed with the
adapters themselves; the historical record of what they were lives in
the repository's history and in `docs/archive/`.

## Skill formation

The MPM Skill Workshop (canonical protocol §3.1 "SKILL FORMATION")
provides a guided workflow for authoring skills. Invoke it via
`mpm call mpm_skills '{"action":"workshop","params":{...}}'`.

## Maintenance

### Refreshing installed managed blocks

After editing `MPM_AGENT_INTEGRATION_SNIPPETS.md` (the canonical source
for the universal managed block), the per-host rendered snippets and
the locally installed managed blocks must be refreshed in lock-step:

```bash
make refresh-installed
```

This target runs:

1. **`render_managed_blocks.py`** — regenerates each adapter's
   `<adapter>/templates/<file>.snippet` from the canonical source.
2. **The host's installer** — refreshes the user's installed managed
   block in `~/.config/opencode/AGENTS.md`. Installers are
   content-aware: if the installed block already matches the snippet,
   the install is a no-op (idempotent).
3. **`render_managed_blocks.py --check`** — verifies byte-for-byte
   parity between the canonical source and the refreshed artifacts.

`make refresh-installed` is the safe re-run path when the canonical
source has drifted from a host's installed file. It is also the
post-edit verification step for any change to
`MPM_AGENT_INTEGRATION_SNIPPETS.md`.

The reconciler refreshes the managed block only on a machine where the
MPM integration is already present. It checks the integration first and
skips the adapter entirely when it is absent, so `make refresh-installed`
never opts a machine into a host it does not use.

### Installed-block provenance

Every installed managed block records where it came from. Immediately
after the inner `<!-- BEGIN MPM MANAGED BLOCK -->` marker, the installer
writes one HTML comment:

```text
<!-- mpm-provenance: version=0.1.0-dev commit=06c029ba11ce18c7f10aa60b0f0497ece2edda7c protocol=2.1.0 applied=2026-10-21T18:04:11Z -->
```

That is one real installation, captured verbatim: the protocol version in
it is the behavioural contract that installation carried, which is why it
is not necessarily the current one.

It carries the MPM **product version** of the build that wrote the
block, the exact **source commit** that build was made from, the
**behavioural protocol version** (`mpm_agent_integration_version`, which
versions the contract rather than the software and moves independently
of the product version), and the UTC moment the block was last applied
or updated.

Two properties matter more than the fields themselves:

* **It is install-time metadata, not canonical text.** A per-build
  commit SHA or a timestamp inside the canonical source would make every
  byte-parity gate (snippet, copy/paste, README, installed-drift) drift
  on every build. The drift gates normalise the comment away on both
  sides of their comparison, so they still compare the behavioural text
  byte for byte.
* **It is idempotent.** The timestamp is preserved when the version, the
  commit, the protocol version and the behavioural text are all
  unchanged, and refreshed whenever any of them changes. Re-running the
  installer with the same version, commit and instructions is a genuine
  no-op: same bytes, same timestamp, no new backup.

An adapter installer run on its own — outside a build — may not be able
to establish a product version or a commit, and then it writes
`unknown` rather than claiming a version it cannot establish. It does
not erase an established value either: when the behavioural text is
unchanged, the value the block already carries is kept.

The mechanism is shared by every host adapter (`scripts/block_provenance.py`),
so the convention is identical across adapters. `install.sh` and
`make refresh-installed` both export `MPM_BUILD_VERSION` and
`MPM_BUILD_COMMIT` from the canonical build script
(`scripts/build-provenance.sh`), so an installed block and the installed
binary cannot disagree about the build.

### One behavioural contract per file

`scripts/managed_block_convergence.py` is the single place that decides
what counts as an MPM-owned region in a host instruction file. Every
persistent-file installer delegates to it, which enforces one
invariant:

> A host instruction file may contain at most one effective MPM
> behavioural contract after reconciliation.

Installers previously recognized only their own marker pair, so a file
holding another host's MPM section — or one whose outer END marker was
never written — matched nothing and fell through to the append path,
leaving two contradictory contracts side by side. The engine
recognizes every MPM marker form that has shipped (all four
`MPM-MANAGED SECTION:<host-id>` forms, the legacy unsuffixed
`MPM-MANAGED SECTION`, both Hermes anchors, and a bare contract with no
wrapper) and converges rather than appends:

| Found in the target file | Outcome |
|---|---|
| no MPM region | insert one |
| this host's section, current | no-op |
| this host's section, stale | replace |
| another host's or a legacy section | migrate to this host's rendering |
| a bare contract with no wrapper | migrate |
| several MPM sections | collapse to one |
| unterminated but deterministically bounded | repair |
| genuinely ambiguous | **refuse**, write nothing, name the reason |

"Deterministically bounded" means exactly one outer BEGIN, no matching
END, and exactly one balanced inner `MPM MANAGED BLOCK` pair after it —
which proves where MPM's territory ends, so no user content is at risk.
Anything else is refused. A refusal is always better than a second
contract. Bytes outside MPM-owned regions are copied through
byte-for-byte, and a backup is written before any change.

### Tool names are capability names

The OpenCode adapter registers the **bare canonical** tool names
(`mpm_memory`, `mpm_context`, `mpm_handoff`, …) — there is no
`mpm__` / `mcp__mpm__` prefix, because those are namespaces an MCP
*client* puts on a server's tools, and the OpenCode plugin is not an
MCP client: it exposes typed plugin tools of its own and reaches the
rest of the registry through `mpm call <tool> --payload '<json>'`.

The universal managed-block wording is therefore the same one the
adapter renders: no host-specific prefix table entry, and no second
contract to keep in sync.

### Diagnostics

`python3 scripts/check_installed_managed_blocks.py` reports marker
shape separately from block currency, so "some MPM text exists
somewhere in the file" is never mistaken for a pass:

| Field | Meaning |
|---|---|
| `marker_state` | `own`, `foreign`, `duplicate`, `bare`, `malformed`, `none` |
| `markers` | every MPM marker present, with the id each one carries |

- `duplicate` — more than one MPM section in one file → **ERROR**
- `foreign` — another host's section, or a bare contract → **WARN**, migrate
- `malformed` — unresolvable markers → **ERROR**; deterministically
  repairable ones are **WARN** with the repair command named, because
  the installer will fix them
- unreachable MCP-transport tool names on a host with no MPM MCP server
  → **ERROR**, since the block would send the agent to a call that
  cannot succeed

`ABSENT` still means only "this host is not part of this machine's MPM
setup".

The structural smoke test
[`tests/test_refresh_installed.py`](./tests/test_refresh_installed.py)
verifies the target is registered in `.PHONY`, that every referenced
installer script and template snippet exists, and that the post-refresh
`--check` is wired up correctly.
