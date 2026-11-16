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
| `mpm-opencode/` | TypeScript plugin (`src/index.v2.ts`, 17 typed tools + `mpm call` fallback to full registry) + AGENTS.md behavioral-protocol installer (snippet + Python installer) + the **isolated reviewer agent** definition and its installer |
| `lib/` | Host-independent libraries (the telemetry adapter) |
| `scripts/` | Shared host-independent tooling: the renderer, the reconciler, the diagnostic, the managed-block convergence engine, the runtime-package staging library |
| `tests/` | The cross-adapter contract suite (renderer parity, reconciliation, provenance, diagnostics) |

The adapter directory's own README is the host-specific deep dive. See
[INSTALL.md](./INSTALL.md) for installation procedures.

## The isolated reviewer agent

A **review** subagent runs in a child session with the parent's tool
surface. On 2026-11-15 a review subagent wrote a memory into the
operator's **live** MPM database through the `mpm_memory` tool, against an
explicit instruction not to touch it.

A written instruction was not enough, and could not have been: the tool
was still **registered** for the reviewer, so "do not call it" was the
only thing standing between a review note and a write into the operator's
substrate.

The fix is the **permission layer**, and it is per-agent, so normal MPM
operation is untouched. The canonical definition is
[`mpm-opencode/templates/agents/mpm-reviewer.md`](./mpm-opencode/templates/agents/mpm-reviewer.md):

```bash
python3 agent_installation/mpm-opencode/scripts/install_reviewer_agent.py
```

It installs into `~/.config/opencode/agents/mpm-reviewer.md`. OpenCode
watches its configuration directory and reloads agents after a save, so
the definition is live without a restart; `/reload` forces it if the host
has not noticed yet.

**The canonical install does this for you.** `mpm-opencode/install.sh`
and `make install` both install the definition, so a fresh installation
never leaves a review subagent with the parent's tool surface. The
installed copy is MPM-owned state: it is refreshed from the template on
every install, and the guard test below fails when it has drifted.

The definition is **layered**, and each layer says what it closes:

- **denies every MPM tool by name**, so a denied tool is removed from the
  reviewer's tool list entirely — the reviewer cannot call it and cannot
  see it;
- **denies the MPM binaries wherever they are NAMED** — `mpm`,
  `mpm-mcp`, `mpm-scheduler`, `mpm-critic`, `mpm-telemetry`,
  `mpm-update`, `mpm-lint`, and their installed paths — in **seven shapes**:
  at the command position; as its own **word** (`env mpm …`, `xargs mpm …`,
  `$(echo mpm)`); separated from its neighbours by a **tab**
  (`mpm<TAB>--version`); as the **last segment of a path** (`go run ./cmd/mpm`,
  `/home/v/.local/bin/mpm`); as the last segment of a path with a **trailing
  slash** (`go run ./cmd/mpm/`); inside a **Go package pattern**
  (`go run ./cmd/mpm/...`, `go run ./cmd/mpm/./...`); and inside a **quote**
  (`"mpm" …`, `sh -c 'mpm …'`).
  The installers (`install.sh`, `uninstall.sh`, `make install`/
  `uninstall`/`deploy`) and the service-control commands are denied as
  well, so the command line cannot reach the substrate either;
- **denies the `read` tool** a live MPM workspace, and closes the
  `external_directory` boundary of a runtime root;
- **denies `edit`** (a reviewer cannot change what it reviews),
  **`subagent`** (a reviewer cannot launch an agent that lacks the
  isolation), **`question`** and Code Mode (**`execute`**, the bridge
  to every tool the host publishes);
- requires findings to be recorded in a **filesystem scratchpad**, not in
  the substrate, and makes the **disposable sandbox**
  (`mpm-opencode/scripts/reviewer_sandbox.py`) the DEFAULT environment
  for every command.

**What this does not close.** The shell runs with the host user's
authority, and a permission rule is a pattern match on a command string:
the layer is a **barrier, not a jail**. `grep` and `glob` are not
path-restricted (their resources are the pattern and the glob), so the
reviewer's own scope — the working tree it was given — is what keeps
them off a runtime root. The **sandbox** is the layer that does not
depend on the pattern being complete: inside it, `HOME`, `TMPDIR` and
`MPM_WORKSPACE` point at scratch directories, so a route the rules do
not name still cannot write into the operator's runtime root.

Five residuals were **measured**, by probing the installed definition: the
**browser family is not governed by the permission layer** (a deny
on the browser action, the wildcard, the per-tool names and `tool.use`
all leave the browser tools in the reviewer's catalog and invocable);
the **capability loader** is not gated by any permission action
(eighteen candidate action names and the V1-style `tools:` frontmatter
all leave it callable), so the rule was removed and the definition says
a reviewer must not load one; the shell's **redirection targets are not
intercepted** — so the write route into a runtime root is closed by a
path substring, which also refuses a command that merely *quotes* such a
path; the shell's matcher sees **text**, and **nine** routes are not
caught: a name **split by a
substitution** (`$(echo mp)m`), a name held in a **variable**
(`M=mpm; $M --version`), a **script file**, a name **expanded by a
shell glob** (`go run ./cmd/mp?`), a name **expanded by a brace**
(`go run ./cmd/mpm{,.mcp}`), a name **glued to punctuation** rather
than delimited by whitespace (`${x:-mpm} --version`, `for i in mpm; do
$i --version; done`), a name hidden behind a **backslash escape**
(`m\pm --version` is the binary, because the shell removes the
backslash before it parses the command word), a pattern that names a
**DIRECTORY of
packages** (`go build ./cmd/...` — the text holds no name to match, so
the build is not refused, but the binaries it produces are refused when
they are RUN), and the host's refusal to
match a **`cd`** segment's text (`cd cmd/mpm && go run . --version`)
are not caught; and the
permission layer reads the **content of a file write** too, so a
document that merely mentions the name is refused. The first two are
stated in the definition's own prose; the third is accepted
deliberately, because a corrupted live database is worse than a
findings report that cannot quote a path literally; and the fourth and
fifth are why the **sandbox is the default**: inside it, the substrate
resolves its workspace to a scratch directory, so a command that got
through writes to the sandbox.

**The false-denial cost, measured and published.** A fixed sample of
**sixteen** ordinary commands was run under the installed definition; the
sample and every outcome are published in `docs/SPEC.md` §6.4, so a
reader can re-run it rather than believe it. **Six** of the sixteen were
refused. **Two** are correct refusals — the bare name as a word
(`echo mpm`, `echo mpm --version`). **Four** are false denials:
`ls <path ending in the name>`, `git -C <path>` and `make -C <path>`
(the price of the path shape, which is what closes `go run ./cmd/mpm`,
`go run ./cmd/mpm/...` and `go build -o /tmp/x ./cmd/mpm`; the spelling
that works is a path **inside** the directory, `ls <path>/docs` — the
`cd` form, `cd <path> && git log --oneline -1`, also works but is
**not recommended**, because it is the same mechanism as an open route:
the host does not match a `cd` segment's text),
and the **quoted** form `grep -rn "mpm_memory" .` (the quoted shape is
this definition's own rule, and it is load-bearing — with it removed,
the quoted invocations RAN the binary; the workaround is to write the
pattern unquoted). **No command that mentions nothing at all was
refused.**

The deny list is **machine-checked against the registry**:
`internal/core/tools/registry_list.go` is the authoritative list of every
MPM tool, and
[`tests/test_reviewer_isolation_denies_every_mpm_tool.py`](./tests/test_reviewer_isolation_denies_every_mpm_tool.py)
fails if a tool is added to the registry without a matching deny rule.
The registry parse is itself checked, so a name the parse cannot
capture fails the test rather than passing it. The binary deny list is
checked against the **Makefile's own** binary variables, so a new MPM
binary cannot be built without the list being extended. The isolation
therefore cannot quietly erode as the tool surface grows.

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
