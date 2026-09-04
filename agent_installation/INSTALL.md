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
git clone https://github.com/flowbyte-com/mpm ~/projects/mpm
cd ~/projects/mpm
./scripts/install.sh                # full install: build + binaries + systemd user unit + lingering
mpm ops init directives             # baseline cognitive bootstrap (idempotent)
```

See the root README's §5 (Quick Start) for the full MPM-side
install procedure.

### Verify FTS5 build flag

The `mpm-mcp` binary must be built with the FTS5 SQLite extension. The
Makefile in `~/projects/mpm` does this by default; if you build
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

Most hosts have an installer that materializes the protocol into the
host's persistent-instruction surface (CLAUDE.md / AGENTS.md /
.hermes.md). The host-specific sections below cover this.

### Installer vs copy/paste — same content

The per-host installer (e.g. `install_agents_instructions.py`) writes
the **same** managed block as the copy/paste example at the top of
[`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md).
Both paths converge on:

  - The exact text of the universal canonical managed block, with the
    host's transport-namespace prefix applied (`mpm__` for Claude,
    `mcp__mpm__` for Hermes, bare for OpenCode and Pi).
  - The host-specific wrapper markers (`<!-- BEGIN/END MPM-MANAGED
    SECTION:… -->` for Claude/OpenCode/Pi, `<!-- BEGIN/END MPM-MANAGED
    BLOCK:hermes-mpm -->` for Hermes).

If you cannot or do not want to run the installer, copy the example
for your host from the top of the snippets file and paste it into the
target file inside the appropriate wrapper markers. The drift tests
in `tests/test_render_managed_blocks.py` pin byte-for-byte parity
between the copy/paste examples, the rendered adapter snippets, and
the output of `python3 scripts/render_managed_blocks.py`.

OpenClaw does not use a persistent managed file — runtime injection
injects the same seven invariants automatically.

### Workshop invocation per host

All hosts invoke the workshop via the universal machine interface:

```
mpm call mpm_skills --payload '{"action":"workshop","params":{...}}'
```

Hosts with native MCP integration (Claude Code, etc.) may invoke
directly:

```
mpm__mpm_skills(action="workshop", params={...})
```

The workshop's three outcomes (`published` / `candidate` / `rejected`)
and the `save_payload` hand-off pattern are host-agnostic.

---

## OpenClaw

OpenClaw participates in the MPM cognitive substrate via **two
complementary surfaces**, plus an optional third. All three converge on
the same MPM install and database.

> **Adoption note (OpenClaw is the exception).** Most hosts require
> editing a persistent-instruction file (CLAUDE.md / AGENTS.md /
> .hermes.md) to teach the agent the MPM behavioral contract.
> **OpenClaw does not.** The `openclaw-mpm-memory` plugin adopts the
> wake invariant through the OpenClaw typed-hook chain
> (`session_start` → `agent_turn_prepare` returning `prependContext`).
> The plugin fetches the wake context, caches it per session, and
> injects it into the agent prompt — the agent therefore wakes from MPM
> without any SOUL.md / AGENTS.md / CLAUDE.md edit. The same hook chain
> is documented in [`openclaw-mpm-memory/index.js`](./openclaw-mpm-memory/index.js)
> (`api.on("session_start", ...)` + `api.on("agent_turn_prepare", ...)`).

### What gets installed

| Surface | Where | Mechanism |
|---|---|---|
| **MCP stdio bundle** | OpenClaw runtime config (not in this repo) | `mcp.servers.mpm.command`, `mcp.servers.mpm.env.MPM_WORKSPACE` |
| **Memory slot plugin** | `~/.openclaw/extensions/openclaw-mpm-memory/` | OpenClaw plugin (`kind:"memory"`); routes `memory_search`/`memory_get` to MPM. **Also wires the wake-context adoption hooks (`session_start` + `agent_turn_prepare`) — this is how OpenClaw adopts the wake invariant without a persistent-instruction file edit.** |
| **Auto-mode/persona plugin** *(optional)* | `~/.openclaw/extensions/openclaw-mpm-auto-mode-persona/` | OpenClaw plugin; per-turn mode/persona injection via `mpm route --apply` |

### Installation

#### Step 1 — MCP stdio bundle (full read+write)

Wires `mpm__*` native tools into OpenClaw. Edit OpenClaw's runtime
config (NOT the MPM repo):

```bash
openclaw config set mcp.servers.mpm.command "$HOME/.mpm/bin/mpm-mcp"
openclaw config set mcp.servers.mpm.env.MPM_WORKSPACE "$HOME/.mpm"
openclaw gateway restart
```

> **Note:** `MPM_ACTIVE_MODE` and `MPM_ACTIVE_PERSONA` are intentionally
> not set in the MCP env. MPM resolves both from env at request time
> via `internal/core/mpmcli.ActiveContextFromEnv()` and falls back to
> `default`/`default` when unset. Hardcoding framework-specific values
> here was a pre-2026-08-29 drift that leaked old mode taxonomy into
> every registration. To pin a non-default mode for this OpenClaw
> session, set the env vars on the gateway process itself, not on the
> MCP server entry.

A canonical `.mcp.json` snapshot lives at
[`openclaw-mpm-memory/.mcp.json`](./openclaw-mpm-memory/.mcp.json) for
reproducibility.

**Strengths.** Deterministic absolute binary path (PATH-independent).
Fast (~5 ms median per call vs ~50 ms subprocess). Full surface.
**Weaknesses.** Requires gateway restart to change config; bundle
disposes on mid-session gateway restart — recovery is to fall back to
`mpm call` CLI per the canonical protocol §5.

#### Step 2 — Memory slot plugin (read-only, satisfies doctor check)

```bash
cd openclaw-mpm-memory
openclaw plugins install ./openclaw-mpm-memory --link
./install.sh                                               # idempotent bootstrap
openclaw config set plugins.entries.openclaw-mpm-memory.enabled true
openclaw config set plugins.slots.memory openclaw-mpm-memory
openclaw gateway restart
openclaw doctor --lint --only core/doctor/memory-search --json   # expect ok:true
```

**Strengths.** Satisfies the OpenClaw doctor check; integrates with
OpenClaw's existing `memory_search`/`memory_get` tool surface so any
agent that already speaks that contract transparently benefits.
Fail-open on MPM unavailability (`{disabled:true, error:…}` instead
of crashing the turn). Boot-time health check + DB path invariant
gate. **Weaknesses.** **Read-only** — no `memory_save` tool. Per-call
subprocess overhead (~50 ms cold) vs MCP aggregator (~5 ms).
Compaction flush is neutered at the sink by architectural decision
`40544f5a04a2aac7` (MPM already has higher-fidelity epistemic
sources — scratchpad + explicit memories).

#### Step 3 — Auto-route plugin (optional; turn-key mode/persona injection)

```bash
openclaw plugins install ./openclaw-mpm-auto-mode-persona --link
openclaw plugins inspect openclaw-mpm-auto-mode-persona --runtime --json
```

On every inbound prompt, this plugin shells out to `mpm route --apply`
and appends MPM's returned `<system-reminder>` block to the bootstrap
prompt. Lets MPM own mode/persona switching without touching OpenClaw
core. Configuration (per `openclaw-mpm-auto-mode-persona/README.md`):

```yaml
plugins:
  entries:
    openclaw-mpm-auto-mode-persona:
      config:
        enabled: true        # default true
        mpmBin: mpm          # PATH-resolved
        timeoutMs: 5000      # default 5000 ms
```

### Verification

After all three surfaces are wired:

```bash
# 1. Confirm MCP wiring (should respond to tools/list):
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | "$HOME/.mpm/bin/mpm-mcp" 2>/dev/null | jq '.result.tools | length'
# expect: 22

# 2. Confirm DB path invariance:
mpm call mpm_system --payload '{"action":"health_check","params":{}}' \
  | jq '.db_path'
# expect: $HOME/.mpm/src/db/mpm.db (or your configured MPM_DB_PATH / MPM_WORKSPACE)

# 3. Confirm memory slot plugin wired correctly:
openclaw doctor --lint --only core/doctor/memory-search --json | jq '.ok'
# expect: true

# 4. End-to-end (write then read):
mpm add "openclaw-mpm integration smoke test"
mpm recall --semantic "smoke test"
```

### Uninstall

The three surfaces uninstall independently:

```bash
# MCP wiring — edit OpenClaw runtime config
openclaw config set mcp.servers.mpm.command ''              # or remove the line
openclaw gateway restart

# Memory slot plugin
openclaw plugins uninstall openclaw-mpm-memory
openclaw config set plugins.slots.memory memory-core          # restore default

# Auto-route plugin
openclaw plugins uninstall openclaw-mpm-auto-mode-persona
openclaw gateway restart
```

### OpenClaw-specific recovery

- **Mid-session gateway restart disposes the MCP bundle.** The
  canonical protocol §5 fallback applies: `mpm call <tool> --payload
  '{"action":"<op>","params":{...}}'` from any subprocess. The agent
  should detect the disposal (tool returns "session disposed" / "not
  connected") and pivot to the CLI path without abandoning persistence.
- **DB path drift** (e.g., after a `mpm-scheduler` restart under
  eCryptfs): the boot-time health check refuses to register when
  `MPM_REQUIRED_DB_PATH` does not match. Set the env var in
  `~/.config/mpm/mpm.env` to the canonical DB path, then
  `systemctl --user restart mpm-scheduler`.

---

## Claude Code

### What gets installed

Two artifacts land in `~/.claude/` after `install.sh`:

| File | Managed by | Purpose |
|---|---|---|
| `~/.claude/.mcp.json` | `install.sh` (one-shot) | MCP server config — Claude Code launches `mpm-mcp` as a stdio child process |
| `~/.claude/CLAUDE.md` | `install_claude_instructions.py` | Persistent instructions: MPM behavioral protocol in a managed block |

The install script wires both in one invocation.

### User scope

Default. Writes to `~/.claude/.mcp.json` (applies to all projects) and
`~/.claude/CLAUDE.md` (global persistent instructions).

### Project scope

For per-project CLAUDE.md override: copy `CLAUDE.md.snippet` into a
project-local `.claude/CLAUDE.md` or `<project>/CLAUDE.md` and run:

```bash
python3 ~/.mpm/agent_installation/claude-code-mpm/scripts/install_claude_instructions.py \
    --scope project \
    --target <project>/.claude/CLAUDE.md \
    --snippet ~/.mpm/agent_installation/claude-code-mpm/templates/CLAUDE.md.snippet
```

### Installation

```bash
cd ~/.mpm/agent_installation/claude-code-mpm
./install.sh                                              # one-shot: MCP + CLAUDE.md
```

The install script:
1. Backs up any existing `~/.claude/.mcp.json` to
   `~/.claude/backups/claude-code-mpm-<TS>/`.
2. Materializes `.mcp.json.template` into `~/.claude/.mcp.json` with
   `${HOME}` substituted.
3. **Merges** with any existing `mcpServers` — never clobbers other
   servers.
4. Sanity-probes the wiring by booting `mpm-mcp` and calling
   `mpm_system health_check`.
5. Writes the CLAUDE.md managed section via the Python installer.

It does **not** touch `~/.claude/settings.json`. The `.mcp.json`
side-channel is the canonical Claude Code wire location for MCP server
entries.

### Verification

```bash
./install.sh --verify                                     # runs verify.py
```

What `verify.py` checks:

- `~/.claude/.mcp.json` parses, contains the `mpm` entry, command is
  absolute.
- `mpm-mcp` boots and responds to `tools/list` (expects 22 tools).
- `mpm_system health_check` returns `ok:true` with the canonical
  `db_path`.
- `~/.claude/CLAUDE.md` contains exactly one managed block (count of
  `BEGIN MPM-MANAGED SECTION:claude-code-instructions` markers == 1).

Manual probe (independent of `verify.py`):

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | "$HOME/.mpm/bin/mpm-mcp" 2>/dev/null \
  | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["result"]["tools"]))'
# expect: 22
```

Then **restart Claude Code** (mcpServers and CLAUDE.md are loaded at
session start; without restart, the `mpm__*` tools will not appear in
the tool list).

### Uninstall

```bash
./install.sh --uninstall
```

Removes the `mpm` entry from `~/.claude/.mcp.json` (or removes the
file if it was the only entry) and strips the managed section from
`~/.claude/CLAUDE.md`. Original state is preserved in
`~/.claude/backups/claude-code-mpm-<TS>/`.

### Provenance configuration

`MPM_FRAMEWORK=claude-code` must be in the `env` block of the `mpm`
entry in `~/.claude/.mcp.json`. The `install.sh` template sets this
automatically. If the env var is missing, all `mpm__*` tool
invocations record `framework_name="unknown"` in `artifact_provenance`,
which makes cross-host attribution ambiguous.

Diagnose:

```bash
jq '.mcpServers.mpm.env.MPM_FRAMEWORK' ~/.claude/.mcp.json
# expect: "claude-code"
```

If missing, edit the file directly or re-run `./install.sh` to
regenerate.

### MCP runtime quirks

- The Claude Code runtime may dispose the MCP bundle mid-session after
  a gateway restart. The canonical protocol §5 fallback applies — use
  `mpm call` from a subprocess.
- `~/.claude/settings.json` is NOT touched by `install.sh`. If you
  have a stale MCP server entry there from an older Claude Code
  version, remove it manually; the canonical wire location is the
  side-channel `.mcp.json`.

---

## OpenCode

### What gets installed

| File / dir | Managed by | Purpose |
|---|---|---|
| `~/.config/opencode/plugin/opencode-mpm` | Symlink (manual or installer) | OpenCode plugin entry (TypeScript, 22 tools) |
| `<project>/AGENTS.md` (or `~/.config/opencode/AGENTS.md`) | `install_agents_instructions.py` | Persistent instructions: MPM behavioral protocol in a managed block |
| `~/.mpm/bin/mpm` | External (Makefile + scripts/install.sh) | `mpm` binary on `$PATH` |

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
# 1. install the plugin into opencode's plugin dir
ln -s "$HOME/.mpm/agent_installation/opencode-mpm" \
      "$HOME/.config/opencode/plugin/opencode-mpm"

# 2. make sure ~/.mpm/bin/mpm is on PATH (the plugin's PATH-resolved
#    default is `mpm`; this exports the canonical install root)
export PATH=$HOME/.mpm/bin:$PATH

# 3. install the AGENTS.md behavioral section (user scope):
python3 ~/.mpm/agent_installation/opencode-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --snippet ~/.mpm/agent_installation/opencode-mpm/templates/AGENTS.md.snippet

# 4. (re)start opencode — the plugin will register and emit the boot
#    health check
```

The plugin reads the binary from `$PATH` by default (`mpm`); if you
need a deterministic absolute path, set it via the OpenCode env block
(`MPM_BINARY=$HOME/.mpm/bin/mpm` is the plugin's recognized
override). The plugin also reads `MPM_WORKSPACE` from env (defaults to
`<cwd>`).

### Verification

```bash
# Plugin loaded? List its commands in an OpenCode session:
opencode # then :commands — expect mpm__mpm_memory, mpm__mpm_handoff, mpm__mpm_scratchpad, ...

# AGENTS.md has exactly one managed block:
grep -c 'BEGIN MPM-MANAGED SECTION:opencode-instructions' \
  ~/.config/opencode/AGENTS.md
# expect: 1

# End-to-end via the plugin's exposed tool:
# (in OpenCode): "use mpm__mpm_memory action=save to remember that opencode-mpm integration smoke test passed"
mpm recall --semantic "opencode-mpm integration smoke test"
```

### Build (only needed if you edit the plugin source)

```bash
cd ~/.mpm/agent_installation/opencode-mpm
npm install
npm run build
# emits dist/index.js (the runtime entry) and dist/index.d.ts
```

### Uninstall

```bash
# Remove the plugin symlink:
rm "$HOME/.config/opencode/plugin/opencode-mpm"

# Strip the managed section from AGENTS.md:
python3 ~/.mpm/agent_installation/opencode-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --uninstall

# (or for project scope, repeat with --scope project)
```

---

## Hermes

### Current integration model

Hermes participates in the MPM substrate via **two surfaces**:

1. **MCP stdio server.** Configured in `~/.hermes/config.yaml` via the
   `mcp_servers.mpm` block. Hermes launches `mpm-mcp` as a stdio child
   process on session start.

   ```yaml
   mcp_servers:
     mpm:
       command: $HOME/.mpm/bin/mpm-mcp
       args: []
       env:
         MPM_WORKSPACE: $HOME/.mpm
       timeout: 60
       connect_timeout: 30
       enabled: true
   ```

   Binary path is absolute (no PATH dependency). Workspace is resolved
   via `MPM_WORKSPACE`, which should be `~/.mpm` (the canonical install
   root).

2. **Hermes MPM Skill.** Loaded from `~/.hermes/skills/mpm/SKILL.md`
   when a task involves MPM. Provides operational guidance
   (wake/recall/save discipline, tool call envelope shapes, diagnostic
   methodology).

3. **`.hermes.md` behavioral protocol** (per-project). Walked from cwd
   up to git root by `hermes-agent/agent/prompt_builder.py:_find_hermes_md`
   and concatenated into the system prompt alongside the user-level
   `~/.hermes/SOUL.md` persona.

   The persona stays in `SOUL.md` (loaded raw by `load_soul_md`, no
   managed-block convention). The behavioral protocol goes in
   `.hermes.md` via the installer in this directory.

### Installation

The MCP wiring and the MPM skill are pre-existing (assumed already
configured). What you may need to install is the **.hermes.md
behavioral section** for a specific project:

```bash
cd /path/to/project
python3 ~/.mpm/agent_installation/hermes-mpm/scripts/install_hermes_instructions.py \
    --target-dir /path/to/project \
    --snippet ~/.mpm/agent_installation/hermes-mpm/templates/hermes.md.snippet
```

The installer writes `/path/to/project/.hermes.md` with leading
HTML-comment markers (since Hermes has no built-in managed-block
convention). Idempotent — re-running is a no-op when content is
unchanged.

### Verification

```bash
# 1. MCP wiring works (live test):
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | "$HOME/.mpm/bin/mpm-mcp" 2>/dev/null \
  | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["result"]["tools"]))'
# expect: 22

# 2. .hermes.md was written with exactly one managed block:
grep -c '<!-- BEGIN MPM-MANAGED BLOCK:hermes-mpm -->' /path/to/project/.hermes.md
# expect: 1

# 3. The skill is loadable (Hermes-side):
ls ~/.hermes/skills/mpm/SKILL.md          # exists
head -3 ~/.hermes/skills/mpm/SKILL.md     # name: mpm, description: ...

# 4. DB path convergence:
mpm call mpm_system --payload '{"action":"health_check","params":{}}' \
  | jq '.db_path'
```

### Uninstall

```bash
python3 ~/.mpm/agent_installation/hermes-mpm/scripts/install_hermes_instructions.py \
    --target-dir /path/to/project \
    --uninstall
```

Strips the managed block from `.hermes.md`, unlinks the file if it
would be empty, and writes a backup before mutation.

### Host-specific constraints

- **No managed-block convention.** Unlike Claude Code and OpenCode,
  Hermes does not natively support marker-based managed sections. The
  installer uses HTML-comment markers for re-detection, but the
  rendered output (in the system prompt) shows the markers as part of
  the comment block — invisible to the model. If the user manually
  edits inside the markers, the installer detects the change and
  replaces on next run (with backup).
- **Persona is sacred.** The installer NEVER writes to `~/.hermes/SOUL.md`.
  If you need to change the persona, edit `SOUL.md` directly.
- **Project walk-up.** Hermes walks from cwd upward to git root
  looking for `.hermes.md` or `HERMES.md`. If your project has neither,
  install one. If your project is outside a git repo, only the cwd's
  file is consulted.

---

## Pi

### Current integration model

Pi participates in the MPM substrate via:

1. **Pi extension.** `pi-mpm/index.ts` registers a **17-tool subset**
   of the full 22-tool MPM registry (14 Domain Tools via Fat RPC + 3
   Standalones: `mpm_retrieval_diagnose`, `log_to_changelog`,
   `request_review`). Tools in the full registry not exposed here
   (`mpm_work`, `mpm_resolve`, `mpm_challenge`, `mpm_blob_read`,
   `mpm_blob_search`) remain reachable via `mpm call <tool> --payload
   '<json>'`. Pi auto-loads extensions declared in
   `~/.pi/agent/settings.json`.

2. **AGENTS.md behavioral protocol.** Per pi docs, Pi loads `AGENTS.md`
   (or `CLAUDE.md`) at session start, searching in this order:
   1. `~/.pi/agent/AGENTS.md` (global, all projects)
   2. `AGENTS.md` in any parent directory of cwd (walking up)
   3. `AGENTS.md` in cwd (current project)

   The installer in this directory writes the MPM behavioral protocol
   into `AGENTS.md` using the same managed-marker convention as
   Claude Code / OpenCode. Disable with `--no-context-files` / `-nc`
   for sessions that should bypass the MPM behavioral contract.

### Installation

```bash
# 1. Add the extension path to ~/.pi/agent/settings.json:
#    "extensions": ["~/.mpm/agent_installation/pi-mpm"]

# 2. Install the AGENTS.md behavioral section (global scope):
python3 ~/.mpm/agent_installation/pi-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --snippet ~/.mpm/agent_installation/pi-mpm/templates/AGENTS.md.snippet

# 3. Make sure `mpm` is on PATH (Pi spawns `mpm` as a subprocess):
export PATH=$HOME/.mpm/bin:$PATH
```

### Verification

```bash
# 1. AGENTS.md has exactly one managed block:
grep -c '<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->' \
  ~/.pi/agent/AGENTS.md
# expect: 1

# 2. The extension is discoverable (Pi-side, in a Pi session):
#    :tools — expect mpm_memory, mpm_handoff, mpm_scratchpad, mpm_wakes, ...

# 3. End-to-end via a Pi session:
#    "use mpm_memory action=save to remember that pi-mpm integration smoke test passed"
mpm recall --semantic "pi-mpm integration smoke test"
```

### Uninstall

```bash
# Remove the extension entry from settings.json, then:
python3 ~/.mpm/agent_installation/pi-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --uninstall
```

Strips the managed section from `~/.pi/agent/AGENTS.md`, unlinks the
file if it would be empty, and writes a backup before mutation.

### Host-specific constraints

- **`mpm` on PATH.** Pi spawns `mpm` as a subprocess to dispatch each
  tool call. If `mpm` is not on `$PATH`, every tool call will fail.
  Either add the canonical install root (`$HOME/.mpm/bin`) to your PATH,
  or override the binary path used by Pi via its extension config (the
  default path resolver is `mpm` resolved against `$PATH`).
- **Parameter schema permissiveness.** Each domain tool's parameter
  schema is `{action: string, params: object}` — free-form. The
  per-action fields are documented in each tool's description; the
  mpm backend validates and returns a structured error envelope if a
  required field is missing or an action is unknown.
- **`--no-context-files` bypass.** If a session is launched with
  `--no-context-files` / `-nc`, the AGENTS.md is NOT loaded and the
  behavioral protocol is NOT enforced for that session. Use sparingly
  — it disables wake, persist-during-work, handoff, and recovery
  discipline.

---

## Installing only the behavioral protocol

Some hosts already have an MPM integration (MCP server / extension)
but lack the persistent behavioral instructions. For those, run the
host-specific snippet installer without re-wiring the MCP server:

```bash
# Claude Code (already has MCP wiring, just needs CLAUDE.md):
python3 ~/.mpm/agent_installation/claude-code-mpm/scripts/install_claude_instructions.py \
    --scope user --home "$HOME" \
    --target ~/.claude/CLAUDE.md \
    --snippet ~/.mpm/agent_installation/claude-code-mpm/templates/CLAUDE.md.snippet

# OpenCode:
python3 ~/.mpm/agent_installation/opencode-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --snippet ~/.mpm/agent_installation/opencode-mpm/templates/AGENTS.md.snippet

# Hermes (per project):
python3 ~/.mpm/agent_installation/hermes-mpm/scripts/install_hermes_instructions.py \
    --target-dir /path/to/project \
    --snippet ~/.mpm/agent_installation/hermes-mpm/templates/hermes.md.snippet

# Pi:
python3 ~/.mpm/agent_installation/pi-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --snippet ~/.mpm/agent_installation/pi-mpm/templates/AGENTS.md.snippet
```

For hosts not yet in this directory, copy a snippet from any of the
above, adapt the path/wrapper for the host's persistent-instruction
surface, and ship as a host adapter.

---

## Updating existing installations

The installers are idempotent. Re-running them with the same snippet
is a no-op; re-running them with an updated snippet replaces the
managed block in place (with backup).

```bash
# Standard update flow (any host):
# 1. pull the latest from the MPM repo
cd ~/projects/mpm && git pull
make build && make install

# 2. re-run the host's install command — the installer detects
#    the new snippet content and replaces the managed section.

# 3. restart the host so it re-reads the persistent instruction file
#    (Claude Code) / re-loads the plugin (OpenCode) / etc.
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
| Claude Code's `mpm__*` tools don't appear | MCP bundle not reloaded after install.sh | Restart Claude Code (mcpServers are loaded at session start) |
| OpenClaw doctor check still fails after install | `plugins.slots.memory` not set | `openclaw config set plugins.slots.memory openclaw-mpm-memory` |
| Hermes `.hermes.md` not picked up | Project not under a git root | Verify `_find_hermes_md` would walk into the directory; install `--target /path/to/.hermes.md` explicitly |
| Pi extension not visible in `:tools` | Extension path wrong in settings.json | Check `~/.pi/agent/settings.json` `"extensions"` array; restart Pi |
| Installer refuses to modify target file | Corrupted managed block (one marker without the other) | Restore from `~/.claude/CLAUDE.md.bak.<TS>` (or analogous `.bak.*`) or hand-remove the orphan marker, then re-run installer |
| AGENTS.md / CLAUDE.md missing after uninstall | Uninstall correctly unlinked an empty file | Re-run the installer's install command (not uninstall) to recreate |
| `mpm call` returns "session disposed" / "not connected" | MCP bundle disposed mid-session | Canonical protocol §5 fallback: `mpm call <tool> --payload '{"action":"<op>","params":{...}}'` |
| Health check reports `fts5: checksum mismatch` despite clean DB | Pre-fix health_check bug (Hermes validation 2026-08-21) | Rebuild `mpm-mcp` against the post-fix commit; verify with `mpm ops health_check` (CLI path) which uses a different code path |

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
- **No PATH or env drift.** The `install.sh` for Claude Code uses
  absolute paths (`${HOME}/.mpm/bin/mpm-mcp`) so the integration is
  PATH-independent.
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
- Each host's own README / SKILL.md:
  - [`claude-code-mpm/README.md`](./claude-code-mpm/README.md)
  - [`opencode-mpm/README.md`](./opencode-mpm/README.md)
  - [`openclaw-mpm-memory/README.md`](./openclaw-mpm-memory/README.md)
  - [`openclaw-mpm-auto-mode-persona/README.md`](./openclaw-mpm-auto-mode-persona/README.md)
  - [`hermes-mpm/SKILL.md`](./hermes-mpm/SKILL.md)
  - [`pi-mpm/README.md`](./pi-mpm/README.md)
