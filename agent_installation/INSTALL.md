# MPM Agent Integration Installation

> **Audience:** operators wiring MPM into a supported agent host.
> **Not this document:** the *orientation* overview is in
> [`README.md`](./README.md). The *behavioral* contract is in
> [`mpm-agent-protocol.md`](./mpm-agent-protocol.md). Host-specific deep
> dives (validation evidence, host quirks, recovery details) are in each
> host's own README or SKILL.md.

This document covers:

- Prerequisites (one MPM install, one DB)
- Per-host installation: what gets installed, exact commands, verification, uninstall
- Scope choices (user-level vs project-level) where they apply
- Host-specific caveats and recovery paths
- Updating existing installations
- Troubleshooting
- Safety / configuration preservation

---

## Installing MPM from an agent

> **OpenCode is the officially supported agent host for this release.** If you
> are an agent reading this inside OpenCode, the procedure is the OpenCode
> section below plus the discipline in this section. If you are running
> inside another host, **fail closed** — see
> [If no adapter exists for your host](#if-no-adapter-exists-for-your-host).

### The procedure

1. **Discover, do not invent.** Look for an existing adapter before writing
   anything:

   ```bash
   ls agent_installation/
   ```

   `agent_installation/` is reserved for hand-reviewed, version-controlled
   integrations shipped with this repository. **Never scaffold a new adapter
   directory, never fabricate a plugin, and never write files under
   `agent_installation/`** — an auto-generated integration is exactly the
   kind of side-effect that pollutes the source tree and cannot be audited.
   If no adapter exists for your host, see
   [If no adapter exists for your host](#if-no-adapter-exists-for-your-host).

2. **Install MPM itself** (one install, one database). The canonical root is
   `~/.mpm`; see [`docs/INSTALL.md`](../docs/INSTALL.md) for the runtime
   reference and `docs/INSTALL.md` §5 for the path cheat sheet.

3. **Install the host adapter.** For OpenCode:

   ```bash
   cd ~/.mpm/agent_installation/mpm-opencode && ./install.sh
   ```

   The adapter builds the plugin payload, registers it in
   `~/.config/opencode/opencode.jsonc`, and refreshes the managed
   behavioural block in `~/.config/opencode/AGENTS.md`.

4. **Never overwrite user working instructions.** Before touching
   `AGENTS.md`, inspect it. The installer preserves everything outside the
   managed-block markers; if the target has a malformed or partial managed
   section, follow the adapter's fail-closed behaviour rather than
   repairing it destructively.

5. **Verify behavioural adoption.** The installation is incomplete if the
   plugin exists but the agent's persistent instruction file does not
   contain the MPM contract. For a fresh session, confirm at least:

   ```text
   wake context delivered before the first model turn
   durable persistence instruction present
   handoff instruction present
   recovery/fallback instruction present
   ```

   The [OpenCode section](#opencode) below carries the exact commands and
   a verification block that exercises these paths.

6. **Verify idempotency.** Run the installation operation twice. The second
   run must not duplicate managed sections, overwrite user instructions,
   duplicate PATH entries, create duplicate configuration entries, or
   corrupt existing MPM state. The installers are written to preserve user
   content and manage only their own sections; use them rather than
   replacing them with ad-hoc edits.

7. **Verify safe failure behaviour.** If the integration cannot be used,
   report the missing prerequisite clearly and identify what must be fixed.
   **Failure must never destroy existing user files or databases.** Do not
   silently continue with a half-installed integration.

### The architecture-idempotency contract

Re-runs must not reintroduce deprecated architecture patterns, on first
install or on any re-run:

- **The retired session-boundary tool** — the single "session" tool that
  predates the split is retired. The current session-boundary surface is
  `mpm_handoff` (write/read/list/shred) and `mpm_scratchpad`
  (flush/read/discard/promote). A re-run must not produce any tool
  manifest, instruction snippet, or config file that references the
  retired name. This is **enforced, not merely advised**: the repository's
  drift guard (`TestNoRetiredMpmSessionReferences` in
  `cmd/mpm/template_drift_test.go`) fails the build if the retired name
  reappears in an installed surface or in the operational documentation.
- **Legacy Python MCP shims** — there is no Python wrapper layer between
  the host and `mpm-mcp`. A re-run must not recreate any `mpm_*.py` shim
  files or wrapper scripts under `agent_installation/`, the user's config
  directory, or anywhere on the resolved tool path.
- **Split-tool violations** — `mpm_handoff` and `mpm_scratchpad` are
  independent tools. Do not collapse them into a single "session" tool and
  do not write instruction text that presents them as a unified surface.
- **Shell rc mutation** — re-runs must not touch `.bashrc`, `.zshrc`,
  `.profile`, or any shell startup file. The `~/.local/bin` symlinks
  created on first install are the durable PATH surface.

Verify by grepping the installed instruction files for the failure modes
that can actually reach a user's file:

```bash
grep -rE 'python.*mcp.*shim|session\.py|mpm_handoff_scratchpad' \
    agent_installation/ "$HOME/.config/opencode" 2>/dev/null

# expected: no matches (or matches only in passive migration warnings
# under agent_installation/ — these are documentation, not active code)
```

Surface any active (non-documentation) hit as a regression and refuse to
declare the installation complete.

### If no adapter exists for your host

There is no supported adapter for your host in this release. Fail closed:
do not fabricate a native plugin, scaffold an adapter directory, or write
any files under `agent_installation/`.

The MPM substrate itself remains fully available to any host through the
host-independent surfaces — the `mpm` CLI, `mpm call <tool> --payload
'<json>'`, and the `mpm-mcp` stdio server. What an unsupported host lacks
is the *managed behavioural block* and the host-native wake-context
delivery; see [`mpm-agent-protocol.md`](./mpm-agent-protocol.md) for the
contract those provide and the documented manual fallback.

### Source-of-truth hierarchy

When instructions disagree, use this order:

```text
1. Current running MPM/tool registry and implementation
2. The matching adapter implementation under agent_installation/
3. agent_installation/mpm-agent-protocol.md
4. This document and the rest of agent_installation/ documentation
5. README/examples
6. Historical/archive material
```

Do not use an archived audit report to override current implementation, and
do not modify runtime behaviour merely to preserve a stale document.

---

## Prerequisites

MPM itself must be installed and running before any host integration.
The host adapters in this directory are *consumers* of MPM's
machine-facing interface; they do not include MPM itself.

### What "MPM installed" means

A working MPM install consists of:

1. **Five binaries in `$HOME/.mpm/bin/`** (or reachable on `$PATH`):
   `mpm`, `mpm-mcp`, `mpm-scheduler`, `mpm-critic`, `mpm-telemetry`.
2. **A working database** at `$HOME/.mpm/src/db/mpm.db` (or wherever
   `MPM_DB_PATH` / `MPM_WORKSPACE` resolves).
3. **The `mpm-mcp` stdio server** reachable by spawning the binary
   (no separate service required — the host launches it as a stdio
   child process).

### Verify MPM is installed

```bash
# From anywhere:
mpm --version                       # binary present
mpm ops health_check                # DB reachable, no FTS5 corruption
mpm-mcp < /dev/null                 # MCP server speaks JSON-RPC
```

If any of these fail, install MPM first:

```bash
git clone https://github.com/flowbyte-com/mpm ~/src/mpm
cd ~/src/mpm
./install.sh                # full install: build + binaries + systemd user unit + lingering
mpm ops init directives             # baseline cognitive bootstrap (idempotent)
```

`~/.mpm` is the canonical root: the Git checkout and the runtime root are the
same directory, and everything MPM writes there at runtime is gitignored.

**If `~/.mpm` already exists, do not clone over it.** See
[Deciding where the source goes](#deciding-where-the-source-goes) below — it
may already be a checkout, or it may contain a database you cannot recreate.

See [Technical specification §5 (Quick Start)](../docs/SPEC.md#5-quick-start) for the full MPM-side
install procedure.

### Deciding where the source goes

`~/.mpm` is the **runtime root** — database, blobs, backups, and the
provisioned `mode/`, `persona/`, `drills/` definitions. It is not a place a
checkout lives, and it will normally already exist on any machine that has run
MPM before. The question to ask is therefore about the *checkout*, not about
`~/.mpm`.

Work through these cases in order and stop at the first match:

| State of the checkout (`~/src/mpm` by convention) | What to do |
|---|---|
| **Does not exist** | `git clone https://github.com/flowbyte-com/mpm ~/src/mpm`, then install from there. |
| **Exists, and its `.git` is a valid MPM checkout** | **Reuse it.** Run `git -C ~/src/mpm pull` and re-run `./install.sh`. Never clone again. |
| **Exists, but is not a recognizable MPM checkout** | **Stop and report.** Do not delete, overwrite, or clone over it. Show the user what is there and ask. |
| **Exists and is not empty, contents unknown** | **Stop and report.** Treat as the ambiguous case above. |
| **An explicit alternate checkout was supplied** | Honour it. Source may live anywhere outside `~/.mpm`; runtime state and binaries still install under `~/.mpm` unless `MPM_WORKSPACE` says otherwise. |

Rules that matter most on a recovery or migration machine:

- **Never delete or overwrite `~/.mpm` to satisfy clone logic.** A `git clone`
  into a non-empty directory fails rather than merging, and forcing it (or
  `rm -rf` first) can destroy a database that cannot be recreated.
- **Never destroy a database because an install path expected an empty
  directory.** A user may already have a `mpm.db` backup staged at `~/.mpm`.
- `~/.mpm/src/db/` holding a database is evidence this is a live MPM
  workspace, not evidence that it is safe to overwrite.

To inspect before deciding:

```bash
ls -la ~/.mpm 2>/dev/null
git -C ~/.mpm rev-parse --is-inside-work-tree 2>/dev/null && echo "valid checkout"
ls -la ~/.mpm/src/db 2>/dev/null        # a database here means live data
```

### Verify FTS5 build flag

The `mpm-mcp` binary must be built with the FTS5 SQLite extension. The
Makefile in `~/.mpm` does this by default; if you build
manually, pass `-tags fts5 -DSQLITE_ENABLE_FTS5=1`. A binary built
without this flag will silently produce an FTS5-disabled `mpm.db` and
queries will degrade. Diagnose with:

```bash
file $HOME/.mpm/bin/mpm              # should be dynamically linked
mpm ops health_check | jq '.fts5_ok'
```

If `.fts5_ok` is `false`, rebuild from source with the FTS5 tag.

### One DB, many agents

All host adapters converge on the **same MPM database**. The DB path
invariance is enforced by `mpm_required_db_path` (set in
`~/.config/mpm/mpm.env` or as an env var); the boot-time health check
refuses to register if the live `db_path` does not match. Run any host
integration and inspect the `db_path` field of the health check:

```bash
mpm call mpm_system --payload '{"action":"health_check","params":{}}'
# {"ok":true,"db_path":"$HOME/.mpm/src/db/mpm.db",...}
```

---

## Canonical protocol

Every host adapter inherits the same six invariants from
[`mpm-agent-protocol.md`](./mpm-agent-protocol.md):

1. **Wake** at session start
2. **Persist** during work, not only at the end
3. **Skill discovery** before reinventing an established procedure
4. **Handoff** before genuine session closure
5. **MPM is the source of truth** for cross-session continuity
6. **Recovery / fallback** to `mpm call <tool> --payload ...` when the
   preferred integration breaks

OpenCode is the only supported host in this release, and its installer
materializes the protocol into the host's persistent-instruction surface
(`~/.config/opencode/AGENTS.md`). The host-specific section below covers
this.

### Installer vs copy/paste — same content

The per-host installer (e.g. `install_agents_instructions.py`) writes
the **same** managed block as the copy/paste example at the top of
[`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md).
Both paths converge on:

  - The exact text of the universal canonical managed block, with the
    host's transport-namespace prefix applied (bare for OpenCode — the
    OpenCode adapter registers the canonical tool names directly).
  - The host-specific wrapper markers (`<!-- BEGIN/END MPM-MANAGED
    SECTION:opencode-instructions -->` for OpenCode).

If you cannot or do not want to run the installer, copy the example
for your host from the top of the snippets file and paste it into the
target file inside the appropriate wrapper markers. The drift tests
in `tests/test_render_managed_blocks.py` pin byte-for-byte parity
between the copy/paste examples, the rendered adapter snippets, and
the output of `python3 scripts/render_managed_blocks.py`.

### Workshop invocation

The workshop is invoked through the universal machine interface:

```
mpm call mpm_skills --payload '{"action":"workshop","params":{...}}'
```

The workshop's three outcomes (`published` / `candidate` / `rejected`)
and the `save_payload` hand-off pattern are host-agnostic.

---

## OpenCode

### What gets installed

| File / dir | Managed by | Purpose |
|---|---|---|
| `~/.config/opencode/opencode.jsonc` → `plugins[]` entry | `mpm-opencode/install.sh` (via `scripts/jsonc_config.py`) | OpenCode plugin registration: a `file://` URL pointing at the plugin **package directory** (TypeScript, 17 typed tools — 14 Domain Tools + 3 Standalone Tools — + `mpm call` CLI fallback to the full MPM Registry) |
| `~/.mpm/agent_installation/mpm-opencode/dist/` | `mpm-opencode/install.sh` | The compiled plugin payload (`dist/index.v2.js` + `dist/mpm-build-provenance.json`) |
| `~/.config/opencode/AGENTS.md` (or `<project>/AGENTS.md`) | `install_agents_instructions.py` (invoked by `install.sh`) | Persistent instructions: MPM behavioral protocol in a managed block |
| `~/.mpm/bin/mpm` | External (Makefile + install.sh) | `mpm` binary on `$PATH` |

> **No symlink is created.** The OpenCode 2 loader resolves a *directory*
> plugin from the `plugins` array in `opencode.jsonc`; a hand-made symlink
> under `~/.config/opencode/plugin/` is not what it reads. The installer
> writes the JSONC entry with `scripts/jsonc_config.py`, which preserves
> unrelated plugin entries, other config sections, comments and trailing
> commas byte-for-byte.

### User scope

`--scope user` writes to `~/.config/opencode/AGENTS.md`. Applies to all
OpenCode sessions for the user.

### Project scope

`--scope project` writes to `<cwd>/AGENTS.md` at install time. Applies
to OpenCode sessions launched from that directory or its descendants.
Per OpenCode docs, the project-level AGENTS.md takes precedence over
the user-level one when both exist.

### Installation

```bash
# 1. install (builds the payload, registers the plugin, refreshes AGENTS.md)
cd ~/.mpm/agent_installation/mpm-opencode
./install.sh

# 2. (re)start opencode — the plugin will register and emit the boot
#    health check
```

The installer performs four steps in order: build the payload (`npm ci`
against the committed `package-lock.json`, then `npm run build`), stage it
into `~/.mpm/agent_installation/mpm-opencode/dist/`, register it in
`~/.config/opencode/opencode.jsonc`, and refresh the managed behavioural
block in `~/.config/opencode/AGENTS.md`.

It is idempotent and non-destructive: re-running it refreshes the plugin
entry and the managed block without touching other plugins, other config
sections, or any text outside the managed-block markers.

The plugin resolves the MPM binary deterministically rather than trusting
`$PATH`: `MPM_BINARY` if set, else `$HOME/.mpm/bin/mpm`, else
`$HOME/.local/bin/mpm`, else `command -v mpm`. The resolved absolute path is
used for every subprocess invocation, so the integration does not depend on
`$PATH` being sane in the OpenCode runtime environment. `MPM_WORKSPACE`
defaults to `$HOME/.mpm` when unset.

### Verification

```bash
# Plugin loaded? List its typed tools in an OpenCode session:
opencode # then :tools — expect 17 tool registrations named `mpm_*`
# (the OpenCode adapter registers the bare canonical names; there is no
#  `mpm__` prefix — that prefix belongs to hosts that namespace MCP tools)

# AGENTS.md has exactly one managed block:
grep -c 'BEGIN MPM-MANAGED SECTION:opencode-instructions' \
  ~/.config/opencode/AGENTS.md
# expect: 1

# End-to-end via the plugin's exposed tool:
# (in OpenCode): "use mpm_memory action=save to remember that mpm-opencode integration smoke test passed"
mpm recall --semantic "mpm-opencode integration smoke test"
```

### Wake-context delivery — `ctx.session.hook("context")`

The OpenCode plugin delivers MPM wake context automatically at session
start via OpenCode 2's `ctx.session.hook("context")` lifecycle hook. It
calls `mpm_context.read_wake_context` with `format: "system-prompt"`
through a subprocess and pushes the returned context into the
`output.system` array that OpenCode ships as the model-facing system
prompt. The wake context lands in the system prompt of the first turn;
subsequent turns hit the cache by `sessionID`. Manual refresh path
is `mpm call mpm_context --payload '{"action":"read_wake_context",…}'`
(or `mpm_context` typed tool action `read_wake_context`).

> The experimental hook `experimental.chat.system.transform` is the
> **OpenCode V1** name. The V1 plugin source is retained at
> `agent_installation/mpm-opencode/src/index.ts` as a historical reference;
> OpenCode 2 cannot load it. The loadable plugin is `src/index.v2.js`.

### Build (automatic)

The plugin payload is compiled by the **installer**, not by hand. When
`dist/index.v2.js` is missing — or was built from a different source
revision — the adapter's `install.sh` installs its build dependencies
(`npm ci`, against the committed `package-lock.json`) and compiles the
payload itself. `npm ci` is allowed to reach the npm registry: a fresh
machine has no npm cache, and assuming one is what made the old manual
workaround fail.

Requirements: **Node.js 20+ and npm** on `PATH` (the adapter's
`package.json` declares `engines.node: ">=20"`). If either is missing,
or the dependency install fails, the installer stops with a diagnostic
that names the missing prerequisite and registers nothing — a partially
installed plugin is never reported as a successful install.

The compiled payload's source revision is recorded in
`dist/mpm-build-provenance.json` next to it, so the deployed payload can
be tied back to a revision:

```bash
cat ~/.mpm/agent_installation/mpm-opencode/dist/mpm-build-provenance.json
```

To confirm that the deployed payload was built from the deployed release:

```bash
cd ~/.mpm/agent_installation/mpm-opencode && ./install.sh --verify
# verify: deployed payload was built from revision <sha>
# verify: ok
```

To rebuild by hand while editing the plugin source:

```bash
cd ~/.mpm/agent_installation/mpm-opencode
npm ci
npm run build
```

### Uninstall

```bash
# Removes MPM's entry from ~/.config/opencode/opencode.jsonc (every
# historical shape: package dir, dist/index.js file url, the legacy
# hyphenated `opencode`-`mpm` form) AND strips the managed block from
# ~/.config/opencode/AGENTS.md. Unrelated plugins, other config sections,
# comments, and any AGENTS.md text outside the managed-block markers are
# preserved. A backup is written before any mutation.
cd ~/.mpm/agent_installation/mpm-opencode
./install.sh --uninstall

# For a project-scope managed block, repeat the instruction step:
python3 ~/.mpm/agent_installation/mpm-opencode/scripts/install_agents_instructions.py \
    --scope project --target <project>/AGENTS.md \
    --uninstall
```

---

## Installing only the behavioral protocol

A machine that already has the OpenCode plugin registered but lacks the
persistent behavioral instructions can run the snippet installer without
re-wiring the plugin:

```bash
python3 ~/.mpm/agent_installation/mpm-opencode/scripts/install_agents_instructions.py \
    --scope user \
    --snippet ~/.mpm/agent_installation/mpm-opencode/templates/AGENTS.md.snippet
```

For a host not yet in this directory, see
[If no adapter exists for your host](#if-no-adapter-exists-for-your-host):
the substrate is reachable through `mpm call`, and an adapter is a
hand-reviewed, version-controlled artefact — not something to improvise.

---

## Updating existing installations

The installers are idempotent. Re-running them with the same snippet
is a no-op; re-running them with an updated snippet replaces the
managed block in place (with backup).

```bash
# Standard update flow (any host):
# 1. pull the latest from the MPM repo
cd ~/.mpm && git pull
make build && make install

# 2. re-run the host's install command — the installer detects
#    the new snippet content and replaces the managed section.

# 3. restart the host so it re-loads the plugin and re-reads the
#    persistent instruction file
```

If the host adapter's installer doesn't exist yet for your host, the
behavioral protocol can be re-materialized by hand-editing the host's
persistent-instruction file to point at the updated canonical
protocol — but prefer the installer to keep the managed-block
contract honest.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `mpm-mcp` boots but tools/list returns 0 tools | Binary built without FTS5 tag | Rebuild with `-tags fts5 -DSQLITE_ENABLE_FTS5=1` |
| `db_path` in health_check differs across hosts | MPM_DB_PATH / MPM_WORKSPACE / symlink drift | Set `MPM_REQUIRED_DB_PATH` in `~/.config/mpm/mpm.env` to the canonical DB path; restart scheduler |
| OpenCode's `mpm_*` tools don't appear | Plugin not reloaded after install.sh | Restart OpenCode (plugins and `AGENTS.md` are loaded at session start) |
| Installer refuses to modify target file | Corrupted managed block (one marker without the other) | Restore from `~/.config/opencode/AGENTS.md.bak.<TS>` or hand-remove the orphan marker, then re-run installer |
| AGENTS.md missing after uninstall | Uninstall correctly unlinked an empty file | Re-run the installer's install command (not uninstall) to recreate |
| `mpm call` returns "session disposed" / "not connected" | MCP bundle disposed mid-session | Canonical protocol §5 fallback: `mpm call <tool> --payload '{"action":"<op>","params":{...}}'` |
| Health check reports `fts5: checksum mismatch` despite clean DB | Pre-fix health_check bug (validated 2026-08-21) | Rebuild `mpm-mcp` against the post-fix commit; verify with `mpm ops health_check` (CLI path) which uses a different code path |

---

## Safety / configuration preservation

All installers in this directory follow the same contract:

- **Backup before mutation.** Every modification writes
  `<target>.bak.YYYYMMDDTHHMMSSZ` alongside the target before any
  change. The backup is never overwritten — multiple `.bak.*` files
  accumulate over the lifetime of the target.
- **User content preservation.** Content *outside* the managed markers
  is never modified. The installers detect managed sections by marker
  pattern; user-written content above and below the markers is
  preserved across reinstalls and uninstalls.
- **Atomic writes.** New content is written via temp-file + rename to
  avoid partial-write states on filesystem errors.
- **No PATH or env drift.** The OpenCode installer resolves the `mpm`
  binary to an absolute path (`MPM_BINARY`, else `${HOME}/.mpm/bin/mpm`,
  else `${HOME}/.local/bin/mpm`, else `command -v mpm`) so the integration
  is PATH-independent.
- **Idempotence.** Re-running the installer with unchanged snippet
  content is a no-op (no backup created). Re-running with changed
  content replaces the managed section in place.

If the integration breaks (after a host upgrade, MPM upgrade, or
manual edit), the rollback path is: stop the host, restore from
`<target>.bak.<TS>`, restart. Backups are timestamped, so multiple
rollback points are available.

---

## See also

- [`README.md`](./README.md) — orientation, supported hosts, design principle
- [`mpm-agent-protocol.md`](./mpm-agent-protocol.md) — canonical behavioral protocol
- The shipped host's own README:
  - [`mpm-opencode/README.md`](./mpm-opencode/README.md)
