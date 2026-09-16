# AUTO_AGENT_INSTALL.md

## Purpose

This file is the **agent-directed MPM installation entry point**.

An agent may be given this file and told:

> Follow `AUTO_AGENT_INSTALL.md` and install/configure MPM for the framework you are currently running.

The goal is to make a fresh MPM installation work for the current agent with the least guesswork and without duplicating the framework-specific installation knowledge already kept under:

```text
agent_installation/
```

**Do not invent a new integration when a matching adapter already exists.**

---

# 1. Core rule: discover, reuse, verify

Start by inspecting the repository's current integration material:

```bash
find agent_installation -maxdepth 3 -type f | sort
```

The current repository contains these host integrations:

```text
agent_installation/claude-code-mpm/
agent_installation/hermes-mpm/
agent_installation/openclaw-mpm-memory/
agent_installation/openclaw-mpm-auto-mode-persona/
agent_installation/opencode-mpm/
agent_installation/pi-mpm/
agent_installation/mpm-agent-protocol.md
```

Use the matching adapter when one exists.

For the current repository:

| Framework | Adapter(s) | Native integration | Persistent behavioral instruction surface |
|---|---|---|---|
| OpenClaw | OpenClaw adapter directories `openclaw-mpm-memory/` + `openclaw-mpm-auto-mode-persona/` (plugin IDs per each `openclaw.plugin.json`; see Section 8) | OpenClaw plugins | `SOUL.md` for the OpenClaw agent behavior; also inspect the current OpenClaw adapter documentation for any host-loaded `AGENTS.md` material |
| Claude Code | `claude-code-mpm/` | MCP via `mpm-mcp` | `CLAUDE.md` |
| OpenCode | `opencode-mpm/` | OpenCode plugin using `mpm call` | `AGENTS.md` |
| Hermes | `hermes-mpm/` | MCP via `mpm-mcp` | `.hermes.md` / `HERMES.md` according to the adapter documentation |
| Pi | `pi-mpm/` | Pi extension using `mpm call` | `AGENTS.md` or `CLAUDE.md` according to Pi's context-file rules |

The adapter directory is the implementation-specific source of truth.

The shared behavioral protocol is:

```text
agent_installation/mpm-agent-protocol.md
```

A human maintainer authoring a future adapter through normal review
reads it as part of that work. An automated install agent does not
construct adapters (see Sections 12–13).

---

# 2. Understand the two installation layers

A complete MPM agent installation has two distinct layers.

## Layer A: capability

Install the MPM core and the framework integration:

```text
MPM binaries
+
MCP / plugin / extension / adapter
```

This gives the agent the ability to call MPM.

## Layer B: behavioral adoption

Install the MPM behavioral contract into the persistent instruction file the host agent actually loads:

```text
SOUL.md
AGENTS.md
CLAUDE.md
.hermes.md / HERMES.md
```

This tells the agent **when and why** to use MPM.

Capability without behavioral adoption is an incomplete installation.

Do not stop after installing an MCP server/plugin/extension.

---

# 3. Establish the current environment

Before modifying anything:

```bash
pwd
git rev-parse --show-toplevel 2>/dev/null || true
git status --short 2>/dev/null || true

printf 'SHELL=%s\n' "${SHELL:-}"
printf 'HOME=%s\n' "${HOME:-}"
uname -srm

command -v go || true
command -v mpm || true
command -v mpm-mcp || true
```

Determine:

- the current project root;
- the current framework/agent;
- the user's home directory;
- whether MPM is already installed;
- whether the current shell is interactive;
- whether this is a project-scoped or user-scoped installation.

**Do not delete existing MPM state.**

---

# 4. Never overwrite user working instructions

Before touching:

```text
SOUL.md
AGENTS.md
CLAUDE.md
.hermes.md
HERMES.md
```

inspect the file.

The installer should preserve existing user content.

Prefer the framework's supplied installer and managed-section mechanism.

Where the adapter uses a managed block such as:

```text
<!-- BEGIN MPM-MANAGED SECTION:... -->
...
<!-- END MPM-MANAGED SECTION:... -->
```

use it exactly as implemented.

If the target has a malformed/partial managed section, follow the adapter's fail-closed behaviour. Do not repair it destructively.

---

# 5. Install MPM itself

Use the repository's canonical MPM installer/build procedure.

Where available, prefer:

```text
scripts/install.sh
```

over reimplementing installation manually.

A normal installation should provide the current MPM binaries, which currently include:

```text
mpm
mpm-mcp
mpm-scheduler
mpm-critic
mpm-telemetry
```

The exact installation location is determined by the current installer/configuration.

Do not assume a hard-coded user path.

After installation verify:

```bash
command -v mpm || true
```

If bare `mpm` resolves in the current shell, use it. If not, locate
the canonical installed binary or wrapper produced by the installer —
typically:

```text
$HOME/.mpm/bin/mpm
```

(or whatever path the current installer reports). Then verify with the
resolved absolute path:

```bash
<resolved-mpm> --help
<resolved-mpm> --version
```

Do not treat missing current-session `PATH` resolution as installation
failure when the canonical installed path works — Section 6 explains
why a freshly-created `~/.local/bin` may not appear in the current
session.

If `mpm` is not resolvable and the canonical installed path cannot be
located, stop and surface the location reported by the installer
before changing `PATH`.

---

# 6. Put MPM on the user's PATH

MPM must be resolvable by normal agent subprocesses where the integration
expects `mpm` to be found by name. There is exactly one supported
mechanism — do not improvise.

## Required mechanism: `~/.local/bin` symlinks

The canonical `scripts/install.sh` (the **only** install path; the legacy
`--system` mode has been removed) creates two symlinks under
`~/.local/bin`:

```text
~/.local/bin/mpm      ->  ~/.mpm/bin/mpm        (CLI wrapper)
~/.local/bin/mpm-mcp  ->  ~/.mpm/bin/mpm-mcp    (MCP stdio server)
```

`~/.local/bin` is the conventional per-user executable location.

Many Linux login environments add it to `PATH` automatically when
present — but only at login time. If the directory is created after
the current login session starts (which is exactly what happens when
`scripts/install.sh` creates it mid-session), the current
shell/session may not gain it automatically. A new login
session will normally pick it up where the user's login profile or
distribution configuration already supports it (for example the
standard `~/.profile` conditional that prepends `$HOME/.local/bin`
when the directory exists).

Concretely: after a fresh install, the installing shell can still
resolve `mpm` only via its absolute path until a new login session
begins. This is expected and is not an install failure.

The symlinks are idempotent — re-running the installer replaces them.

Only these two binaries are symlinked. Internal daemons
(`mpm-scheduler`, `mpm-critic`, `mpm-telemetry`) live only at
`~/.mpm/bin/` and are invoked by systemd, never by the user. Putting
them on PATH would invite accidental direct invocation and drift.

Verify the symlinks themselves (no `PATH` assumption required):

```bash
ls -l "$HOME/.local/bin/mpm" "$HOME/.local/bin/mpm-mcp"
# expect each line to point at $HOME/.mpm/bin/...
```

Then confirm the canonical wrapper works regardless of current-session
`PATH` resolution:

```bash
"$HOME/.local/bin/mpm" --version
```

Optionally check whether `mpm` resolves by name in the current shell:

```bash
command -v mpm || true
```

If it does, great. If not, see the current-shell guidance below — it
is expected on a freshly-created `~/.local/bin` and is not an install
failure.

## What NOT to do

**Strictly forbidden** — never write to the user's shell startup files
under any circumstance:

- `~/.bashrc`
- `~/.bash_profile`
- `~/.zshrc`
- `~/.zprofile`
- `~/.profile`
- `~/.config/fish/config.fish`
- Any shell- or framework-specific startup file

Rationale: the user may run multiple shells, may source these files
in non-interactive contexts where `PATH` mutations cause downstream
breakage, and may have already configured `~/.local/bin` via distro
defaults. Mutating these files from an installer is a hostile act
that creates the exact `PATH`-corruption / shell-state-drift the
symlink mechanism was designed to prevent.

If `~/.local/bin` is missing from the user's current `PATH`
(expected when the directory was created after the current login
session started — see above), the user has two immediate options
without persisting a new shell startup entry:

```text
1. use the canonical installed absolute path:
       $HOME/.local/bin/mpm   (or $HOME/.mpm/bin/mpm if the
       ~/.local/bin symlink is somehow unavailable)

2. or, for the duration of the current shell only:
       export PATH="$HOME/.local/bin:$PATH"
```

A new login session will normally pick `~/.local/bin` up where the
user's login configuration already supports it (for example the
standard `~/.profile` conditional that prepends `$HOME/.local/bin`
when the directory exists).

Do **not** instruct the user — and certainly do not let an install
agent — to append another persistent `export PATH=...` line to
`.bashrc`, `.profile`, `.zshrc`, or any other startup file. The
`~/.local/bin` symlink is the durable mechanism; one startup-file
mutation per host is enough, and that one is the user's to make (or
not).

## Non-interactive agents and services (mandatory `mpmBin`)

An interactive shell `PATH` is not necessarily inherited by services.

If a framework integration supports an explicit absolute `mpmBin`
configuration key (OpenClaw's gateway, for example), it MUST be set
to:

```text
mpmBin = /home/<user>/.local/bin/mpm   # or absolute path to ~/.mpm/bin/mpm
```

NEVER rely on `PATH` resolution from the service's perspective. The
service may have a stripped-down environment, no sourced shell rc
files, and no `~/.local/bin` on its `PATH`. Absolute path binding is
the contract.

This is particularly important for OpenClaw's gateway when it runs
under `systemd --user` and for any other framework whose integration
spawns subprocesses under a non-interactive systemd unit.

---

# 7. Initialise/configure MPM

Follow the current MPM installation documentation and the framework adapter's instructions.

Verify the current configuration/state locations rather than assuming them.

Before initialising:

```bash
ls -la "$HOME/.mpm" 2>/dev/null || true
```

Preserve existing databases and configuration.

Where configuration is required, make that requirement explicit to the user.

Do not create credentials or secrets.

After setup, perform a harmless health check using the current command/interface documented by the repository.

---

# 8. Identify and install the matching framework adapter

Inspect the adapter directory's current README/INSTALL/SKILL material.

### Claude Code

Use:

```text
agent_installation/claude-code-mpm/
```

Install/configure the current `mpm-mcp` integration and install the managed MPM section into the appropriate `CLAUDE.md`.

Use the supplied installer:

```text
claude-code-mpm/install.sh
claude-code-mpm/scripts/install_claude_instructions.py
```

Do not hand-copy the snippet when the installer can do it safely.

### OpenCode

Use:

```text
agent_installation/opencode-mpm/
```

Install the OpenCode plugin and install the managed MPM section into the appropriate `AGENTS.md`.

Use:

```text
opencode-mpm/scripts/install_agents_instructions.py
opencode-mpm/templates/AGENTS.md.snippet
```

Follow the adapter documentation for user/global versus project scope.

### Hermes

Use:

```text
agent_installation/hermes-mpm/
```

Follow `SKILL.md` and the supplied installer for the Hermes behavioral section.

Use the framework's actual instruction-file loading rules, which may use `.hermes.md` / `HERMES.md`.

Do not assume `AGENTS.md` for Hermes just because another agent uses it.

### Pi

Use:

```text
agent_installation/pi-mpm/
```

The extension provides the MPM tools; the managed behavioral section goes into the Pi-compatible context file.

The supplied installer is:

```text
pi-mpm/scripts/install_agents_instructions.py
```

Use the documented scope:

```text
user
project
```

and the supported target/filename options.

Respect Pi's documented context-file precedence.

### OpenClaw

OpenClaw currently has **two complementary MPM plugins**, kept in two
OpenClaw adapter directories:

```text
Adapter directories:
  agent_installation/openclaw-mpm-memory/
  agent_installation/openclaw-mpm-auto-mode-persona/
```

These directory names exist for repository readability. They are not
necessarily the identity OpenClaw itself loads or displays. The
authoritative plugin identity in each case is the `id` field in that
adapter's `openclaw.plugin.json` manifest (mirrored by the `PLUGIN_ID`
constant in its `index.js` and used in its `plugins.entries.*` config
keys) — inspect the manifest before quoting an ID. At the time of
writing those IDs are:

```text
Plugin IDs (per openclaw.plugin.json):
  openclaw-mpm-memory
  openclaw-mpm-auto-mode-persona
```

(Do not confuse these with the npm package names such as
`@openclaw/mpm-memory`, which are a separate namespace.)

They are not interchangeable.

The `openclaw-mpm-memory` plugin provides the MPM-backed memory capability plus wake-context/provenance integration.

The `openclaw-mpm-auto-mode-persona` plugin provides per-turn mode/persona routing.

Inspect both README files and manifests before installing.

**Wake context** is delivered by the `openclaw-mpm-memory` plugin
(plugin ID — adapter directory
`agent_installation/openclaw-mpm-memory/`) via the
`session_start` → `agent_turn_prepare` typed-hook chain (returning
`prependContext`). This is automatic and **does not** require any
persistent MPM instruction block in `SOUL.md` or `AGENTS.md`. The
sessionKey used to correlate the cache write (in `session_start`) with
the cache read (in `agent_turn_prepare`) comes from the hook context
(`ctx.sessionKey`), not from the event payload. The
`openclaw-mpm-memory/tests/runtime_injection.test.js` regression guard
pins this contract.

Automatic wake delivery does not remove the behavioral contract.
Distinguish the two layers defined in Section 2:

```text
wake-context delivery
    = runtime/plugin behavior
    = automatic through the OpenClaw MPM plugin hook chain
    = the installer MUST NOT duplicate wake-injection instructions
      into SOUL.md/AGENTS.md to replicate what the hooks already do

behavioral adoption
    = still required where applicable, covering e.g.:
      durable persistence expectations (persist during work)
      skill discovery/use
      handoff behavior before genuine session closure
      cross-session source-of-truth rules
      recovery/fallback behavior (mpm call escape hatch)
    = governed by the persistent behavioral contract
      (Section 9; agent_installation/mpm-agent-protocol.md),
      which controls when/how the agent uses MPM during work
```

In short: the OpenClaw runtime plugin can provide wake context
automatically while the persistent behavioral contract still governs
what the agent does with MPM once it is working. Do not duplicate
automatic wake-injection instructions unnecessarily, and do not treat
automatic wake delivery as a reason to skip behavioral adoption.

For OpenClaw's gateway, remember that plugin subprocesses may not inherit the user's interactive PATH. Use the adapter's supported `mpmBin` configuration when required.

---

# 9. Install the working-instruction contract

This is mandatory.

Use the framework adapter's snippet/template/installer when present.

The resulting managed section should express the current MPM contract.

The host-independent contract includes:

1. **Wake**  
   Recover relevant MPM context before substantive session work.

2. **Persist**  
   Store durable knowledge during the work rather than only at the end.

3. **Skill discovery**  
   Discover a relevant prior procedure when the task context suggests one may apply, without scanning the entire skill catalogue every turn.

4. **Handoff**  
   Before genuine session closure, record a handoff when substantial work has occurred.

5. **Cross-session source of truth**  
   Durable continuity belongs in MPM, not only transient agent context.

6. **Recovery**  
   If the preferred integration is unavailable, use the documented current `mpm call <tool> --payload ...` fallback.

The exact tool names/actions must come from the current adapter/runtime contract.

Do **not** revive retired `mpm_session` instructions.

The current session-boundary surfaces are split across:

```text
mpm_handoff   (substrate + full MCP surface)
mpm_scratchpad (substrate only)
```

On the **default initial MCP surface** (Claude Code / Hermes / OpenClaw),
`mpm_handoff` write/read is reached via `mpm_context action=write_handoff`
/ `read_handoff`, and `mpm_scratchpad` is reachable only via the
`mpm call mpm_scratchpad` CLI fallback. Both remain directly callable on
hosts that set `MPM_EXPOSE_ALL_TOOLS=1`, and as substrate tools via
`mpm call` on every host.

---

# 10. Do not confuse session lifecycle with work completion

The current protocol treats these as distinct:

```text
session ended
    !=
work completed
    !=
work verified
```

Do not instruct an agent to mark work complete merely because a session ended.

Use the current `mpm_work` contract for explicit work completion.

---

# 11. Verify framework scope

A working-instruction file can be:

```text
global
project-local
parent-directory
```

depending on the host.

Install at the scope the user intends.

Examples:

```text
Claude:
  global ~/.claude/CLAUDE.md
  or project CLAUDE.md where supported

OpenCode:
  ~/.config/opencode/AGENTS.md
  or project AGENTS.md

Pi:
  ~/.pi/agent/AGENTS.md
  or project AGENTS.md
```

Use the host adapter documentation to determine exact precedence.

Do not assume that a project-local file affects every project on the machine.

---

# 12. If no adapter exists for the current framework

If `agent_installation/` has no matching framework directory, the agent
MUST fail closed. **Do NOT fabricate a native plugin, scaffold a new
adapter directory, or write any files under `agent_installation/`** —
that path is reserved for hand-reviewed, version-controlled integrations
shipped with the repository. Auto-generating one of these from an
installer is exactly the kind of side-effect that pollutes the source
tree and is impossible to audit.

## Required fail-closed behavior

1. Install/configure MPM core normally (Section 5) so the `mpm` and
   `mpm-mcp` binaries are installed with the `~/.local/bin`
   symlinks (Section 6). Note the Section 6 login-timing caveat: the
   current shell may need the absolute path until a new login session
   begins.
2. Print a clear diagnostic to the user:
   ```text
   MPM agent install: FAIL-CLOSED
     framework: <detected framework>
     reason:    no matching adapter under agent_installation/
     action:    falling back to canonical `mpm call` CLI surface
                (no native plugin, no behavioral instruction file
                will be installed)
   ```
3. Fall back strictly to the `mpm call` CLI surface for any MPM
   operation. The behavioral contract from
   `agent_installation/mpm-agent-protocol.md` still applies to the
   agent's reasoning (wake on session start, persist during work,
   handoff before closure), but it is communicated through the
   existing agent instruction surface — not by creating new files
   under `agent_installation/`.
4. Do not modify the user's shell startup files, the framework's
   config directory beyond what is required for `mpm call` to resolve,
   or the MPM repository layout.

## What is forbidden

The following are strict violations of this contract and MUST NOT be
performed by an automated installer:

- Creating a new directory under `agent_installation/<framework>/`
- Writing any TypeScript, JavaScript, Python, or shell file under
  `agent_installation/` claiming to be a native integration
- Modifying the framework's runtime config (e.g. `~/.openclaw/`,
  `~/.config/opencode/`) beyond the minimum required to point at the
  existing `~/.local/bin/mpm-mcp` MCP server entry
- Generating "stub" plugin manifests that defer the actual integration
- Patching `mpm-agent-protocol.md` or any other file under
  `agent_installation/` to "make it work" for the missing framework

Native integration for a new framework is a hand-authored,
version-controlled artifact. An installer that needs one must surface
the gap and stop, not invent the artifact.

---

# 13. New framework adapters are maintainer-authored (not installer-created)

An automated install agent MUST NOT create a new framework adapter.
Section 12 is the complete policy for the no-adapter case: fail closed,
install/verify MPM core only, fall back to the canonical `mpm call`
surface, and report that a hand-authored reviewed adapter is required.

A native integration for a new framework is a hand-authored,
version-controlled artifact produced by a repository maintainer through
normal review — never a side effect of running an installation. Such
work keeps new material under:

```text
agent_installation/<framework>/
```

with a structure consistent with the existing adapters, for example:

```text
agent_installation/<framework>/
    README.md
    templates/
        <working-file>.snippet
    scripts/
        <installer>.py
```

and ships only when a genuine native integration has been implemented
and reviewed. This section exists solely to orient a human maintainer
doing that future work; it is not an instruction to any automated
installer, and it does not authorise one to write repository
integration material.

The root `AUTO_AGENT_INSTALL.md` remains the generic dispatcher, not a
dumping ground for framework-specific code.

---

# 14. Verify the installed tools

After installation, verify the current framework can actually access the MPM integration.

At minimum verify:

```text
integration/plugin/extension is present
working-instruction managed section is present
current tool names are used
retired tool names are absent from current instructions
```

For MCP integrations, inspect the actual current tool registry where practical.

The current default initial MCP surface (the model-facing one at session start, with `MPM_EXPOSE_ALL_TOOLS` unset) exposes **3 tools**: `mpm_memory`, `mpm_context`, `mpm_help`. The full internal substrate surface is **22 tools** (21 Registry entries + the `mpm_help` discovery closure registered via cmd/mpm-mcp); reachable on hosts that set `MPM_EXPOSE_ALL_TOOLS=1` in their MCP env block, or via the universal `mpm call <tool> --payload '…'` CLI fallback.

Implementation sources (authoritative; re-derive the numbers from
these rather than trusting this paragraph): the 3-tool default is
`defaultCoreTools` filtered by `coreToolFilter` in `cmd/mpm-mcp/main.go`;
the 21 Registry entries live in `internal/core/tools/registry_list.go`
(pinned by `TestCompactSurface_FullRegistryPreserved` with `want = 21`);
`mpm_help` is registered as a closure in `cmd/mpm-mcp/tools.go`
(`makeHelpHandler` iterates the live Registry, so its `list` output
derives the count at runtime instead of hard-coding it).

Do not hard-code either number into new framework-specific
documentation or adapter logic: derive the surface from the live
Registry (via `mpm_help action=list` or `tools.ByName`) so future
Registry growth does not silently desynchronise downstream adapters.

---

# 15. Verify the agent can actually call MPM

Use a harmless representative operation.

The exact mechanism depends on the framework.

Verify at least one complete path:

```text
agent
  ->
framework integration
  ->
mpm / mpm-mcp
  ->
MPM
  ->
successful result
```

Do not treat the existence of a config file as proof that the runtime works.

For OpenClaw, explicitly consider the gateway's non-interactive environment.

For integrations that spawn `mpm` subprocesses, verify binary resolution under the environment where the integration actually runs.

---

# 16. Verify behavioral adoption

The installation is incomplete if:

```text
tool/plugin exists
but
the agent's persistent instruction file does not contain the MPM contract
```

Check the installed working file.

Confirm that it contains the correct current managed section and refers to the current tool surface.

(For OpenClaw, apply the Section 8 distinction: automatic
wake-context delivery via the plugin hook chain satisfies the wake
layer, while the behavioral contract — persistence, skill discovery,
handoff, source-of-truth, recovery — is still adopted where
applicable. Do not demand a duplicated wake-injection block as proof
of adoption.)

For a fresh agent session, verify at least:

```text
wake context behaviour
durable persistence instruction
handoff instruction
recovery/fallback instruction
```

Where runtime testing is available, exercise the relevant path.

---

# 17. Verify idempotency

Run the relevant installation operation twice.

The second run must not:

- duplicate managed sections;
- overwrite user instructions;
- duplicate PATH entries;
- create duplicate configuration entries;
- corrupt existing MPM state.

The existing framework installers are intended to preserve user content
and manage only their own sections. Use those installers rather than
replacing them with ad-hoc edits.

## Architecture-idempotency contract

Re-runs MUST NOT reintroduce deprecated architecture patterns. The
following are forbidden both on first install and on every re-run:

- **`mpm_session`** — retired. The current session-boundary surface
  is split across `mpm_handoff` (write/read/list/shred) and
  `mpm_scratchpad` (flush/read/discard/promote). Re-running an
  installer must not produce any tool manifest, instruction snippet,
  or config file that references `mpm_session`.
- **Legacy Python MCP shims** — there is no Python wrapper layer
  between the host and `mpm-mcp`. Re-running must not recreate any
  `mpm_*.py` shim files or wrapper scripts under `agent_installation/`,
  the user's config dir, or anywhere on the resolved tool path.
- **Split-tool violations** — `mpm_handoff` and `mpm_scratchpad` are
  independent tools. Do not collapse them into a single "session"
  tool, do not re-introduce a combined `mpm_handoff_scratchpad`
  helper, and do not write behavioral instruction text that refers
  to them as a unified surface.
- **Shell rc mutation** — re-runs must not touch `.bashrc`,
  `.zshrc`, `.profile`, or any shell startup file. The `~/.local/bin`
  symlinks created on first install are the durable PATH surface.

Verify by grepping the installed instruction files:

```bash
grep -rE 'mpm_session|python.*mcp.*shim|session.py' \
    agent_installation/ "$HOME/.claude" "$HOME/.config/opencode" \
    "$HOME/.hermes" "$HOME/.pi" 2>/dev/null

# expected: no matches (or matches only in passive migration warnings
# under agent_installation/ — these are documentation, not active code)
```

The agent must surface any active (non-documentation) hit as a
regression and refuse to declare the installation complete.

---

# 18. Verify safe failure behaviour

If the preferred integration cannot be used:

- the agent should still have a documented MPM fallback where the host adapter supports it;
- installation should report missing prerequisites clearly;
- configuration errors should identify what must be fixed;
- failure must not destroy existing user files or databases.

Do not silently continue with a half-installed agent integration.

---

# 19. Do not install obsolete architecture

A current installation must not add or require:

```text
session watchers
memory watchers
database-path watchers
old Python MCP shims
retired mpm_session tool registrations
obsolete lifecycle hooks
```

The runtime's current session continuity contract uses the split handoff/scratchpad surfaces at the substrate layer. On the model-facing MCP surface, handoff is reached through `mpm_context action=write_handoff` (the substrate `mpm_handoff` tool is no longer in the default initial 3-tool surface; see Section 14 for the authoritative implementation sources).

The old `mpm_session` name may still exist in compatibility/history code. That is not a reason to expose it as a current agent tool.

---

# 20. Configuration and PATH summary

At the end of installation, be able to state:

```text
MPM binary:
MPM binary directory:
PATH updated:
MPM config:
MPM database:
Agent/framework:
Framework adapter:
Integration/plugin/extension:
Working instruction file:
Installation scope:
Managed MPM section:
Verification result:
```

Do not print secrets.

---

# 21. If installation modifies the repository

Normally this document is for installing MPM into the user's environment, not for altering the project source.

An automated install agent must not create repository integration
material for an unsupported framework (see Sections 12–13). If
repository changes are nevertheless expected, that means a human
maintainer is hand-authoring a reviewed adapter outside this
install flow — not that the installer performed one.

Before writing:

```bash
git status --short
```

Preserve unrelated changes.

Review newly created files before presenting the installation as complete.

---

# 22. Final verification checklist

The installation is complete only when all applicable items are true:

```text
[ ] Current agent/framework identified
[ ] Matching adapter found under agent_installation/, or no-adapter fail-closed path followed (core only + mpm call fallback; no adapter created)
[ ] Existing adapter documentation inspected
[ ] MPM core installed/built
[ ] Required binaries available
[ ] mpm resolvable on PATH or configured by supported absolute-path mechanism
[ ] Configuration requirements satisfied and surfaced
[ ] Existing MPM state preserved
[ ] Matching integration/plugin/extension installed
[ ] Persistent working-instruction file identified correctly
[ ] MPM behavioral contract installed there
[ ] Existing user instructions preserved
[ ] Managed section is valid and idempotent
[ ] Current tool names used
[ ] Retired mpm_session is not exposed as a current tool
[ ] No obsolete watcher/Python-shim architecture installed
[ ] Representative MPM operation succeeds
[ ] Non-interactive/service environment considered where relevant
[ ] Second installation run is safe/idempotent
[ ] Final installation state is clearly reported
```

---

# 23. Source-of-truth hierarchy

When instructions disagree, use this order:

```text
1. Current running MPM/tool registry and implementation
2. Matching adapter implementation under agent_installation/
3. agent_installation/mpm-agent-protocol.md
4. Current INSTALL.md / agent_installation documentation
5. README/examples
6. Historical/archive material
```

Do not use an archived audit report to override current implementation.

Do not modify runtime behaviour merely to preserve a stale document.

---

# 24. Final rule

The purpose of this file is to make agent installation **discoverable and executable**, not to replace the framework adapters.

The correct process is:

```text
AUTO_AGENT_INSTALL.md
        |
        v
identify current agent
        |
        v
inspect agent_installation/
        |
        +---- adapter exists ----> use it
        |                             |
        |                             v
        |                      install behavioral contract
        |                             |
        |                             v
        |                      configure integration
        |                             |
        |                             v
        |                      verify real runtime
        |
        +---- no adapter --------> FAIL CLOSED (Section 12)
                                       |
                                       v
                                install/verify MPM core only
                                       |
                                       v
                                expose/use canonical `mpm call`
                                fallback where appropriate
                                       |
                                       v
                                report that a hand-authored
                                reviewed adapter is required
                                (do NOT create repository
                                integration material)
```

The completed installation should leave the user with both:

```text
MPM capability
    +
agent behavioral adoption
```

not merely a plugin sitting on disk that the agent promptly ignores, which would be a remarkably human way to install a memory system.
