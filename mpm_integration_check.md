# MPM Integration Check

## Purpose

This document defines the canonical procedure for an agent to inspect and report the health of its current MPM integration.

It is an **integration diagnostic**, not an installer and not a repair procedure.

The check must determine whether MPM is genuinely usable by the current agent across the complete path:

```text
agent
  -> host/framework integration
  -> MPM tool or CLI transport
  -> installed MPM runtime
  -> MPM state/database
  -> returned result
```

A configuration file, registered plugin, MCP entry, extension directory, running scheduler, or successful standalone `mpm` command is **not by itself proof of a working agent integration**.

The procedure is host-agnostic and applies to supported integrations including:

```text
Claude Code
OpenClaw
OpenCode
Pi
Hermes
```

Future hosts should use the same capability-based checks rather than copying assumptions from an existing adapter.

---

# 1. Safety contract

## 1.1 Default mode is read-only

Unless the user explicitly requests repair or active mutation testing, this procedure MUST NOT:

- edit files;
- install or uninstall integrations;
- change host configuration;
- restart services;
- rebuild MPM;
- fetch Git changes (the remote-freshness check below performs `git fetch` only when the diagnostic policy permits a read-only network operation; otherwise it must report `UNKNOWN`);
- pull, merge, reset, checkout, or switch branches;
- modify MPM state;
- create memories, handoffs, wakes, skills, decisions, theories, lessons, topics, references, scratchpad entries, or work records;
- change mode or persona;
- modify scheduler state;
- write test artifacts merely to prove that writes are possible.

Diagnostic commands should prefer read-only actions.

## 1.2 Do not "fix while checking"

If a defect is found:

1. record the defect;
2. identify the failing layer;
3. collect enough evidence to make the finding useful;
4. continue with independent checks where safe;
5. report the proposed repair separately.

Do not turn an integration check into an undocumented installer run.

## 1.3 Preserve uncertainty

Use:

```text
PASS
FAIL
WARN
UNKNOWN
NOT APPLICABLE
```

Do not silently convert an unknown result into a pass.

A clean local working tree, or a `HEAD == origin/main` match on the *cached* upstream, is **not** proof that local is current with the network remote. Such a state must be reported as `Remote freshness: UNKNOWN` unless a fetch has actually been performed during this check (and the fetch succeeded).

---

# 2. Identify the current execution environment

Determine the framework from the current process/environment and host-specific runtime evidence.

Do not infer the framework solely from which adapter directories happen to exist.

Useful evidence can include:

```bash
env | sort | grep -E '^(PI_|CLAUDE_|OPENCODE_|HERMES_|OPENCLAW_|MPM_)' || true
ps -o pid,ppid,cmd -p $$ -p "$PPID" 2>/dev/null || true
```

Inspect documented host configuration only when relevant.

Record:

```text
Framework:
Framework version:
Agent/runtime version:
Model/provider if reliably available:
Thinking/reasoning level if reliably available:
Session identifier if reliably available:
Evidence used:
```

### Dynamic model provenance

Do not invent or statically assume the model.

If the host model can vary per session or per turn, report the actual runtime value only when the host exposes it reliably.

Otherwise report:

```text
Model: UNKNOWN / dynamic
```

A stale static `MPM_PROVENANCE_MODEL` value is worse than no model provenance.

---

# 3. Locate the MPM installation

Determine the binary actually used by the current environment.

Run, where available:

```bash
command -v mpm || true
command -v mpm-mcp || true
readlink -f "$(command -v mpm)" 2>/dev/null || true
readlink -f "$(command -v mpm-mcp)" 2>/dev/null || true
```

Inspect the canonical installation locations and verify each:

```bash
test -e "$HOME/.mpm/bin/mpm" && echo "binary present" || echo "binary MISSING"
test -e "$HOME/.local/bin/mpm" && echo "PATH link present" || echo "PATH link MISSING"
test -d "$HOME/.mpm" && echo "workspace dir present" || echo "workspace dir MISSING"
test -d "$HOME/.mpm/src/db" && echo "data dir present" || echo "data dir MISSING"
```

If `~/.mpm` is a symlink, resolve and report the final target:

```bash
readlink -f "$HOME/.mpm"
```

Record:

```text
mpm resolved path:
mpm final target (after readlink -f):
mpm-mcp resolved path:
mpm-mcp final target (after readlink -f):
~/.mpm is symlink: yes/no, target:
~/.mpm/src/db exists: yes/no
~/.local/bin/mpm exists: yes/no
MPM workspace/data root:
```

A shell-resolvable `mpm` does not prove that a service-hosted integration can resolve it.

For subprocess-based adapters, separately inspect the path/binary configured for the actual host runtime.

Do not report a path as "populated" and later as "missing" without an intervening change.

---

# 4. Check installed binary freshness

Determine whether the installed runtime corresponds to the current source checkout where this can be established without modification.

Inspect:

```bash
mpm --version 2>/dev/null || true
~/.mpm/bin/mpm --version 2>/dev/null || true
~/.mpm/bin/mpm-mcp --version 2>/dev/null || true

# Source identity
git -C "$MPM_WORKSPACE" rev-parse HEAD 2>/dev/null || true
git -C "$MPM_WORKSPACE" rev-parse '@{upstream}' 2>/dev/null || true
git -C "$MPM_WORKSPACE" status --porcelain 2>/dev/null || true
```

The CLI binary at `~/.mpm/bin/mpm` is the compiled Go binary itself, not a wrapper. The runtime defaults `MPM_WORKSPACE` to `$HOME/.mpm` internally via `internal/core/config.GetMPMDir()` when the environment variable is unset, so no shell shim is required. If you observe a non-ELF file at any of the canonical binary paths (`.build/bin/mpm`, `~/.mpm/bin/mpm`, or any `~/.mpm/bin/*`), treat it as a stale install artefact and recommend `./install.sh` to restore the canonical layout.

Where source binaries and installed binaries can be compared safely, use hashes or byte comparison:

```bash
sha256sum .build/bin/mpm .build/bin/mpm-mcp .build/bin/mpm-scheduler .build/bin/mpm-critic .build/bin/mpm-telemetry
sha256sum "$HOME/.mpm/bin/mpm" "$HOME/.mpm/bin/mpm-mcp" "$HOME/.mpm/bin/mpm-scheduler" "$HOME/.mpm/bin/mpm-critic" "$HOME/.mpm/bin/mpm-telemetry"
```

`make build` populates only `.build/bin/`; it never writes the install prefix. The two are expected to differ until `make install` (or `./install.sh`) explicitly promotes the developer artifacts.

Relevant binaries include:

```text
mpm
mpm-mcp
mpm-scheduler
mpm-critic
mpm-telemetry
```

Do not assume every binary is expected to be running.

Record freshness separately:

```text
Source HEAD:
Installed build identity (mpm --version):
mpm freshness (hash vs source .build/bin/mpm): PASS/FAIL/UNKNOWN
mpm-mcp freshness: PASS/FAIL/UNKNOWN
mpm-scheduler freshness: PASS/FAIL/UNKNOWN
mpm-critic freshness: PASS/FAIL/UNKNOWN
mpm-telemetry freshness: PASS/FAIL/UNKNOWN
```

Use `UNKNOWN` when build metadata or reproducible comparison is unavailable.

Do not fetch, rebuild, reinstall, or update merely to resolve an `UNKNOWN`.

---

# 5. Check remote source freshness (NEW — required)

A clean local working tree, or a local `HEAD == origin/main` match against the **cached** upstream, is **not** proof that local is current with the network remote.

The previous false-PASS mode here compared `HEAD` to a *locally cached* `origin/main` reference, which can be stale by days. The following rules replace that mode.

## 5.1 Read-only network policy

`git fetch` is permitted during this diagnostic only when:

1. the diagnostic policy explicitly allows a read-only network operation;
2. network access is available in the current environment.

If neither condition is satisfied, do **not** fetch. Report:

```text
Remote freshness: UNKNOWN (fetch not performed in this diagnostic)
Local HEAD: <hash>
Cached upstream: <hash or "(none)">
```

Do **not** report `PASS` in this case.

## 5.2 When fetch is performed

Run:

```bash
git -C "$MPM_WORKSPACE" fetch --no-tags --no-recurse-submodules origin 2>&1 | head -5
git -C "$MPM_WORKSPACE" rev-parse HEAD
git -C "$MPM_WORKSPACE" rev-parse '@{upstream}'
git -C "$MPM_WORKSPACE" rev-list --left-right --count HEAD...'@{upstream}'
```

Classify the result:

```text
0 0  -> source matches fetched upstream
N 0  -> local is ahead
0 N  -> local is behind
N M  -> histories diverged
```

Where `N>0` on both sides indicates diverged histories. Do not classify that as ordinary "ahead/behind".

If the fetched upstream differs by **identity** from a previously cached one (forced update / rewritten history), note that explicitly. A divergent classification requires an additional finding that explains that the rewrite happened.

## 5.3 What to report

Always report at minimum:

```text
Remote freshness: PASS / FAIL / WARN / UNKNOWN
Local HEAD:
Cached upstream (before fetch):
Fetched upstream (after fetch, if performed):
Classification (0/0, N/0, 0/N, N/M):
Fetch performed: yes/no
History rewrite detected: yes/no
```

`PASS` for remote freshness requires both:

- a fetch was performed during this diagnostic, AND
- the local HEAD matches the fetched upstream (`0 0`), AND
- no rewrite/diverge indicators were detected.

Otherwise report `FAIL`, `WARN`, or `UNKNOWN` according to the actual evidence.

---

# 6. Combined source/binaries/remote freshness verdict

The installed-runtime freshness verdict is the conjunction of three independent axes.

## 6.1 Three axes

```text
A. Source tree state vs installed binaries:
     - do installed binaries correspond to local source HEAD?
     - hash compare bin/* vs $HOME/.mpm/bin/* where applicable
B. Local source vs fetched upstream:
     - see Section 5
C. Local source working tree cleanliness:
     - `git status --porcelain` must be empty for "clean"
     - tracked + staged + untracked status is reported, not inferred
```

## 6.2 Verdict table

```text
Source/binaries match AND remote fetch matches AND tree clean  -> Overall freshness: PASS
Source/binaries match BUT remote UNKNOWN                       -> Installed binaries match local source: PASS
                                                                    Remote source freshness: UNKNOWN
                                                                    Overall freshness: UNKNOWN
Source/binaries match BUT remote FAIL                          -> Installed binaries match local source: possibly PASS
                                                                    Source current with upstream: FAIL
                                                                    Overall freshness: FAIL or WARN
Source/binaries FAIL                                           -> Overall freshness: FAIL
Tree dirty                                                     -> Overall freshness: WARN (cannot tie binary to a clean revision)
```

Do not collapse these into one optimistic `PASS`.

When local source matches installed binaries but remote freshness is unknown, the **overall** verdict must be `UNKNOWN`, not `PASS`.

---

# 7. Establish the authoritative current tool surface

Do not hard-code a historical tool count as the primary test.

Derive the current surface from the running implementation or current repository.

The distinction matters between:

```text
Registry-backed MPM operations
MCP discovery/helper tools
host-native typed tools
CLI fallback tools
```

Where available, inspect the current Registry and/or the live discovery mechanism:

```bash
mpm call mpm_system --payload '{"action":"help","params":{}}' 2>&1 | head -40
```

For MCP integrations, distinguish between:

```text
default model-facing MCP surface
  intentionally fixed contract
  mpm_memory, mpm_context, mpm_help
  (do not describe this number as dynamic)

full substrate surface
  derived dynamically from the live Registry
  reachable via mpm_help list discovery + the
  mpm call <tool> CLI fallback
  AND/OR by setting MPM_EXPOSE_ALL_TOOLS=1 on the
  MCP env block to restore the full surface natively
```

A compact default MCP exposure is not a defect if discovery/fallback provides the remaining supported operations by design. Tools filtered out of the default `tools/list` are NOT directly callable over the compact MCP transport; report `NATIVE` only for capabilities that flow through the host's MCP/typed-tool integration, and `FALLBACK` for capabilities reachable only via `mpm call <tool>`.

## 7.1 Tool-count reconciliation

If a tool list is enumerated anywhere in the report:

1. derive the count from the live surface (or the current `cmd/mpm` Registry in source);
2. mechanically reconcile the reported count with the number of names actually listed;
3. never print "21 names / 20 tools" or similar duplications.

For example, when listing the MCP tool surface:

```text
Reported MCP tool count: N
Names listed: M
Reconciled: N == M
```

If `N != M`, that itself is a `WARN` finding.

Record:

```text
Registry tool count (derived):
Discovery/helper tools:
Native host-visible tools:
Fallback-only tools:
Unexpected tools:
Missing expected tools:
Tool count reconciliation: PASS/FAIL/UNKNOWN
```

Do not call a host integration incomplete merely because it intentionally exposes a subset as typed/native tools, provided the documented fallback works.

Conversely, do not report full coverage merely because missing operations theoretically exist somewhere in the repository.

---

# 8. Verify MPM core health

Use a harmless current health operation through the installed runtime.

Prefer the current `mpm_system` health-check contract documented by the live tool surface.

Verify that the result reaches the MPM database/runtime successfully:

```bash
mpm doctor 2>&1 | head -60
mpm status 2>&1 | head -30
```

Check for errors involving:

```text
SQLite
FTS5
missing virtual/shadow tables
database locks
WAL/checkpoint behaviour
schema mismatch
workspace resolution
permissions
```

## 8.1 Subsystem vs maintenance separation

Keep subsystem health separate from maintenance advisories.

Examples of subsystem-level status (affects the integration verdict):

```text
Database integrity: PASS/FAIL
FTS5 health: PASS/FAIL
WAL/checkpoint: PASS/FAIL
Schema match: PASS/FAIL
```

Examples of maintenance advisories (NOT a subsystem defect):

```text
Review backlog: <count> memories due for spaced review
Cron retention: startup_stabilization (transient, expected after daemon start)
Pending cron wakes: 0
```

A spaced-review backlog is **not** an FTS or database warning. Place operational/maintenance advisories in their own finding.

Record:

```text
Core health:
Database reachable:
FTS health:
WAL/checkpoint:
Schema match:
Workspace resolution:
Subsystem errors:

Maintenance advisories (separate from subsystem health):
  - <each advisory, individually>
```

A successful CLI health check proves MPM core health.

It does **not** yet prove host integration health.

---

# 9. Verify host-to-MPM execution

Perform at least one harmless MPM operation through the mechanism the current agent actually uses.

Examples:

```text
Pi/OpenCode native typed tool
Claude Code/Hermes MCP tool
OpenClaw plugin/runtime binding
documented host fallback
```

Do not substitute:

```bash
mpm call ...
```

for a native-tool test when the question is whether the native integration itself works.

CLI fallback may be tested separately.

## 9.1 Native vs fallback semantics (strict)

Use strict meanings:

```text
NATIVE
  capability is exposed and callable through the host's native integration
  (MCP tool, typed extension tool, host plugin binding)

FALLBACK
  capability is available only via documented CLI/MCP fallback
  (e.g. `mpm call <tool> --payload '{...}'`)

MISSING
  no supported path works

NOT APPLICABLE
  capability does not apply to this host
```

Do not call a CLI-only capability `NATIVE`.

Record both paths when applicable:

```text
Native integration path:
Native call result:

CLI fallback path:
CLI fallback result:
```

The desired result is:

```text
agent -> host adapter -> MPM -> successful response
```

---

# 10. Verify wake/context delivery

Check whether the current agent can retrieve the MPM wake/context payload expected by the current architecture.

Verify relevant fields by semantics rather than by an old fixed field count.

Look for evidence such as:

```text
contextual focus
recent memories/context
recent topics or milestones
last handoff when one exists
global/baseline directives
active mode/persona where supported
skills/discovery context where supported
```

An empty collection is not automatically a defect.

Distinguish:

```text
field unavailable
field present but empty
field intentionally omitted
```

Test both paths where applicable:

```bash
mpm wake 2>&1 | head -30
mpm call mpm_context --payload '{"action":"read_wake_context","params":{"projection":"compact"}}' 2>&1 | head -10
```

## 10.1 Separate manual gatherer from automatic host injection

The two paths above verify that the MPM **substrate** can serve wake
context on demand. They do **not** verify that the host's
session-start / turn-prepare hooks actually inject that context into
the agent's prompt at runtime. A wake that is reachable on demand but
never delivered to the model is the documented regression mode that
motivated this section.

Distinguish:

```text
manual wake gatherer (substrate side):         PASS / FAIL
  mpm wake returns recent context, handoff, etc.
  mpm call mpm_context --payload '{"action":"read_wake_context",...}' returns populated WakeContextData

automatic host session-start injection:     PASS / FAIL / UNKNOWN
  OpenClaw mpm-memory-openclaw session_start hook fires AND
  agent_turn_prepare returns { prependContext: <wake> } for the
  same sessionKey that session_start stored.
  Verify via the gateway journal:
    journalctl --user -u openclaw-gateway.service -n 500 --no-pager \
      | grep -E "mpm-memory-openclaw: (session_start|agent_turn_prepare)"
  Successful injection logs show:
    "session_start wake fetched for <sessionKey> (length=N)"
    "agent_turn_prepare for <sessionKey> (cached=true, wakeLength=N)"
  See §17 (host-specific wiring) for the per-host probe commands.
```

A host integration check must NOT call automatic wake delivery
healthy based only on manual CLI retrieval. The two paths measure
different layers:

```text
manual PASS + automatic UNKNOWN  ->  host wiring has not been probed
manual PASS + automatic PASS     ->  full integration confirmed
manual PASS + automatic FAIL     ->  host hook layer broken; CLI escape
                                    hatch available, but model never
                                    receives wake without manual call
manual FAIL + automatic *        ->  substrate side broken; nothing can
                                    deliver wake even if host is wired
```

This separation is the key fix for the false-PASS class: a fresh
OpenClaw session where persona injects but wake does not shows up as
`automatic FAIL` here, not `Wake/context: PASS`.

Record:

```text
Wake/context callable:
Wake/context manual gatherer (substrate): PASS / FAIL
Wake/context automatic host injection:  PASS / FAIL / UNKNOWN
Last handoff visible:
Directives visible:
Mode/persona state visible where applicable:
Unexpected omissions:
```

---

# 11. Verify session and handoff semantics

MPM uses persistent state across sessions, but the architecture must not depend on obsolete session watchers.

Verify that the current integration understands the split lifecycle model:

```text
session ended != work completed != work verified
```

Check current `mpm_handoff` and `mpm_scratchpad` availability according to the live tool surface.

Where a host supplies a session identity, inspect whether the integration forwards or records it correctly.

Do not require a fabricated session ID when the host provides none.

Look for:

```text
framework_session_id / equivalent host identity
optional handoff session_id behaviour
cross-session handoff readability
scratchpad availability
```

The retired `mpm_session` interface must not be exposed as the current lifecycle mechanism.

Record:

```text
Handoff surface:
Scratchpad surface:
Framework session identity:
Session identity provenance:
Obsolete session surface exposed:
```

---

# 12. Verify provenance

Inspect how artifacts produced through this host would be attributed.

At minimum determine whether the host integration supplies truthful framework provenance.

Check:

```text
MPM_PROVENANCE_FRAMEWORK
model provenance if dynamically reliable
session identity if supported
host-specific provenance injection
```

Requirements:

- framework attribution must match the actual host;
- model attribution must not be fabricated;
- static model metadata must not override a genuinely dynamic runtime model;
- missing optional provenance is preferable to false provenance.

Record:

```text
Framework provenance:
Model provenance:
Session provenance:
Provenance assessment:
```

---

# 13. Verify scheduler integration

Determine whether the canonical MPM scheduler is installed and healthy where expected.

For systemd-user installations:

```bash
systemctl --user is-active mpm-scheduler 2>/dev/null || true
systemctl --user status mpm-scheduler --no-pager 2>/dev/null || true
```

Where useful, inspect its configured executable and verify it exists:

```bash
systemctl --user show mpm-scheduler \
  -p ExecStart -p ActiveState -p SubState -p MainPID 2>/dev/null || true

# Resolve and verify ExecStart binary path
readlink -f "$(systemctl --user show mpm-scheduler -p ExecStart --value 2>/dev/null | awk '{print $1}' | tr -d '{}')"
test -e "$(systemctl --user show mpm-scheduler -p ExecStart --value 2>/dev/null | awk '{print $1}' | tr -d '{}')" && echo "scheduler binary EXISTS" || echo "scheduler binary MISSING"
```

Check for the expected scheduler lock where applicable:

```bash
test -e "$HOME/.mpm/scheduler.lock" && echo "lock present" || echo "lock missing"
```

Do not create a second scheduler.

Do not install host-specific watchers as a substitute.

### Important distinction

```text
scheduler process running
```

does not prove:

```text
agent can schedule and later receive a wake correctly
```

Therefore report separately:

```text
Scheduler daemon health:
Scheduler binary freshness:
Scheduler ExecStart path resolves to: <path>
Scheduler ExecStart binary exists: yes/no
Wake scheduling surface available:
Wake-delivery integration:
End-to-end wake status:
```

Without an existing suitable wake to observe, end-to-end delivery may legitimately be `UNKNOWN` in read-only mode.

---

# 14. Verify mode/persona integration

Determine the host's actual current mode/persona contract.

Do not assume every host implements OpenClaw-style automatic per-turn routing.

Distinguish:

```text
automatic routing
passive state visibility
manual routing
unsupported
```

Verify that current mode/persona state can be observed where the host integration claims support.

## 14.1 Separate persona readable vs persona automatically injected vs persona exactly-once

Like wake/context (§10.1), the persona integration has three distinct
layers that must be reported separately. A session where the persona
block appears in the agent's prompt is **not by itself** proof that
automatic host injection is working — the persona body might be baked
into a static file (SOUL.md, CLAUDE.md, AGENTS.md) that the host loads
unconditionally. Conversely, "persona visible on manual probe" does
not prove automatic injection either.

Distinguish:

```text
persona state readable (substrate side):       PASS / FAIL
  mpm_wake reports `active_persona`, or
  active.json at the workspace root carries the persona id.

persona automatically injected (host side):   PASS / FAIL / UNKNOWN
  OpenClaw mpm-auto-mode-persona-openclaw message:received + agent:bootstrap
  hooks both fire for the same sessionKey. Verify via:
    journalctl --user -u openclaw-gateway.service -n 500 --no-pager \
      | grep -E "mpm-auto-mode-persona-openclaw|bootstrap|reminder"
  Successful injection logs show a single append per session turn.

persona injected exactly once (idempotency):   PASS / FAIL / UNKNOWN
  The persona block must not appear N times in the assembled SOUL.md
  content. N>1 indicates either:
    - the host fires agent:bootstrap more than once per turn AND the
      plugin appends on every fire, OR
    - two persona plugins are loaded and both append.
  Verify by reading the actual bootstrapFiles content the model sees
  and counting occurrences of the rendered persona block.
```

The pre-fix regression mode the operator can detect with this
section:

```text
automatic routing:           PASS   (mpm-auto-mode-persona-openclaw is loaded)
persona injected exactly once: FAIL   (3 occurrences in the model prompt)
```

A host that reports `Automatic routing: PASS` MUST also pass the
exactly-once check; otherwise the operator has been told a partial
truth.

Record:

```text
Mode integration:
Persona integration:
Automatic routing:
Persona state readable (substrate): PASS / FAIL
Persona automatically injected (host): PASS / FAIL / UNKNOWN
Persona injected exactly once:        PASS / FAIL / UNKNOWN
State visible in wake/context:
```

Passive integration is not a defect when it is the documented design.

---

# 15. Verify skill discovery

Check that the agent has a usable route to MPM skill discovery.

An empty skills catalogue does not prove an integration failure.

Distinguish:

```text
skills tool unavailable
skills tool works but catalogue is empty
skills exist and are discoverable
```

Record:

```text
Skill discovery callable:
Catalogue state:
Discovery usable:
```

---

# 16. Verify advanced persistent-agent surfaces (strict NATIVE/FALLBACK/MISSING)

Inspect availability of the current persistent-agent capabilities, deriving names from the live Registry rather than from this document.

Relevant capability classes currently include:

```text
memory
context
handoff
scratchpad
work tracking
scheduled wakes
decisions
theories
lessons
topics
references
evidence
confidence
skills
resolution
blob retrieval/search
system health
```

For each class report one of:

```text
NATIVE
  capability is exposed and callable through the host's native integration

FALLBACK
  capability is available only via documented CLI/MCP fallback

MISSING
  no supported path works

NOT APPLICABLE
  capability does not apply to this host
```

These are strict meanings. Do not classify a CLI-only capability as `NATIVE`.

The purpose is to determine whether an agent can maintain useful state across sessions, not merely whether a memory-save endpoint exists.

---

# 17. Check host-specific wiring

Inspect the adapter documentation and actual host configuration for the detected framework.

Use the current adapter directory under:

```text
agent_installation/
```

Do not assume directory names, plugin IDs, package names, or configuration keys are interchangeable.

## 17.1 Path verification

For each path you claim is populated, stale, or missing:

```bash
test -e <path>      # exists (any type)
test -d <path>      # is a directory
test -f <path>      # is a regular file
test -L <path>      # is a symlink
readlink -f <path>  # final target after symlink resolution
```

Do not produce contradictory claims such as:

```text
path is populated
```

and later:

```text
path does not exist
```

without an intervening change.

## 17.2 Pi

Inspect the actual Pi extension registration and its current workspace/binary/provenance behaviour.

Check:

```text
extension registered
native typed tools load
CLI fallback resolves
workspace resolution is correct
framework provenance is pi
runtime model metadata is not statically fabricated
```

## 17.3 OpenClaw

OpenClaw follows the same **persistent managed instruction** model as
the other four hosts. The agent's `SOUL.md` (resolved per-agent from
`openclaw.json` → `agents.entries.<id>.workspace`) is the persistent
behavioural-contract surface. `mpm-memory-openclaw/install.sh` writes
the MPM managed block to that path on install and refreshes it on
reinstall. Persona (808) and any user content outside the managed
markers is preserved verbatim.

Inspect current OpenClaw plugin identities from their manifests/configuration.

For OpenClaw explicitly distinguish:

```text
repository adapter directory  (e.g. agent_installation/mpm-memory-openclaw/)
package.json name            (e.g. "openclaw-mpm-memory")
openclaw.plugin.json id      (e.g. "openclaw-mpm-memory")
plugins.entries key          (in host config)
plugins.slots.memory value   (in host config)
agent SOUL.md path           (per-agent workspace / SOUL.md)
managed-block markers       (BEGIN/END MPM-MANAGED SECTION:openclaw-instructions)
historical names in memories/docs
```

Do not label a name an npm package name, manifest ID, repository directory, or config key without inspecting the authoritative source for that namespace.

Check:

```text
plugin registered
plugin enabled
correct memory slot where applicable
absolute mpm binary binding where required
gateway health
runtime hook wiring
wake/context delivery
mode/persona plugin behaviour where installed
persistent behavioural contract present in SOUL.md
persona / user content above managed markers preserved
```

The persistent behavioural contract in `SOUL.md` is **not** equivalent
to runtime wake injection. Both layers must pass for the agent to be
fully MPM-capable:

```text
Wake gatherer (substrate side):                   PASS / FAIL
Wake runtime injection (auto session_start → agent_turn_prepare):  PASS / FAIL / UNKNOWN
MPM managed block in SOUL.md:                    PASS / FAIL
Persona injection (auto message:received + agent:bootstrap):   PASS / FAIL / UNKNOWN
```

A host that reports `Wake runtime injection: PASS` MUST also pass
`MPM managed block in SOUL.md`; otherwise the operator has been told
a partial truth — runtime wake delivery without the persistent
behavioural contract produces agents that receive current context but
do not know how to use MPM across the session.

The same holds when wake injection reports `FAIL` or `UNKNOWN` for a
host that has no MPM plugin at all. OpenClaw supports a **CLI-fallback
mode** in which the agent reaches MPM through `mpm call <tool>` over
its shell tool instead of through a plugin. That mode delivers no wake
injection, so the managed block is not merely supplementing a
dynamic layer there — it is the *only* thing that tells the agent the
fallback exists. Classify such a host as MPM-enabled on the strength of
reachability plus intent, not on plugin presence:

```text
native plugin mode  mpm-memory-openclaw installed
                    (plugin dir / plugins.entries.* / plugins.slots.memory)
CLI fallback mode   mpm binary installed at ~/.mpm/bin/mpm or ~/.local/bin/mpm
                    AND the OpenClaw workspace instruction surface
                    (AGENTS.md / MEMORY.md / CLAUDE.md, workspace resolved from
                    openclaw.json) already names MPM
```

Both halves are required. MPM is installed on any machine that has any
MPM integration, so reachability alone is far too permissive; and a
workspace that mentions MPM in passing has not been integrated. A
machine satisfying neither is an OpenClaw that has never been
integrated with MPM and must not be opted in.

`make check-installed` applies exactly this rule, and
`make refresh-installed` uses the same one, so the two can never
disagree about whether a host is in scope. To confirm the managed
section directly:

```bash
test -f "$HOME/.openclaw/workspace/SOUL.md" \
  && grep -q 'BEGIN MPM-MANAGED SECTION:openclaw-instructions' \
       "$HOME/.openclaw/workspace/SOUL.md"
```

to confirm the managed section is present in the agent's SOUL.md.

## 17.4 Claude Code

Check:

```text
MCP registration
mpm-mcp resolution
managed MPM behavioural instructions
live MCP call
```

For the managed MPM behavioural instructions: the install target is `~/.claude/CLAUDE.md`. Use the canonical diagnostic:

```bash
make check-installed
```

The diagnostic distinguishes four states:

```text
PASS    installed block byte-matches the current canonical render
WARN    installed block drifted from the current canonical render
ABSENT  install target absent or managed-section markers absent
ERROR   install target present, markers present, but file malformed
```

PASS and WARN are currency concerns; ABSENT is a presence concern. The `make check-installed` diagnostic reports presence and currency separately, so an operator can tell the two apart at a glance. WARN's repair path is `make refresh-installed`. ABSENT's repair path is the per-host `install.sh` (not `refresh-installed` — there is no managed block to refresh).

## 17.5 OpenCode

Check:

```text
plugin registration
native typed tool loading
CLI fallback
workspace resolution
managed behavioural instructions
```

For the managed behavioural instructions: the install target is `~/.config/opencode/AGENTS.md`. Use the same `make check-installed` diagnostic. Repair paths match those listed in §17.4.

## 17.6 Hermes

Check:

```text
MCP registration/configuration
mpm-mcp resolution
behavioural instruction loading
live MCP call
```

---

# 18. Verify behavioural adoption

A technically reachable tool does not mean the agent knows when to use it.

Inspect the actual persistent behavioural instructions loaded by the current host.

Confirm that the current MPM contract covers, where appropriate:

```text
wake/context recovery
durable persistence during work
skill discovery
handoff
scratchpad/lifecycle behaviour
recovery/fallback behaviour
work tracking
scheduled follow-up
```

Reading MPM prime directives alone does not prove that the host-loaded behavioural instruction surface contains the MPM contract. Verify the actual instructions/runtime injection loaded by the host. If that cannot be established, report:

```text
Behavioural adoption: UNKNOWN
```

Check for stale architecture language including:

```text
mpm_session
session watchers
memory watchers
database-path watchers
legacy Python MCP shims
obsolete tool names
obsolete action names
```

Record:

```text
Behavioural instructions loaded:
Current MPM contract present:
Stale architecture references:
Behavioural adoption:
```

---

# 19. Compare documentation to implementation

Inspect the current host adapter documentation against the implementation actually loaded.

Check for drift in:

```text
tool names
action names
plugin IDs
environment variables
binary paths
instruction-file locations
session semantics
provenance behaviour
wake behaviour
mode/persona behaviour
native-vs-fallback tool claims
```

Historical validation documents may intentionally describe older states.

Do not classify an explicitly historical record as current documentation drift merely because the system has since evolved.

Record:

```text
Current documentation consistent:
Historical-only differences:
Actionable documentation drift:
```

---

# 20. Check relevant integration tests

Locate the tests covering the current adapter.

Do not run broad destructive or unrelated suites merely because they exist.

Prefer focused tests covering:

```text
adapter loading
tool registration
workspace resolution
binary resolution
provenance
wake/context
managed instruction parity
fallback behaviour
host-specific regression cases
```

Running tests is allowed only when they are non-mutating with respect to the user's live environment, or when their isolation is clear.

Record:

```text
Relevant tests found:
Tests executed:
Result:
Tests not run and why:
```

---

# 21. Optional active verification

This section is **not part of the default read-only check**.

Only perform active verification when explicitly authorised.

Suitable disposable tests can include:

```text
temporary memory write/read/delete where supported
temporary handoff round trip
temporary scratchpad round trip
temporary scheduled wake round trip
temporary work item round trip
provenance verification on a disposable artifact
```

Every active test must:

1. use clearly disposable data;
2. record what it creates;
3. clean up after itself where the API supports cleanup;
4. avoid overwriting existing user state;
5. avoid creating a second scheduler or watcher;
6. report cleanup failure explicitly.

For scheduler verification, test the canonical MPM wake path rather than inventing sleep loops or external cron jobs.

---

# 22. Failure classification

Classify each problem by layer.

Use:

```text
CORE
INSTALLATION
BINARY-FRESHNESS
REMOTE-SOURCE-FRESHNESS
DATABASE
SCHEDULER
HOST-CONFIG
HOST-ADAPTER
NATIVE-TOOL
MCP
CLI-FALLBACK
WAKE-CONTEXT
HANDOFF
SESSION-IDENTITY
PROVENANCE
MODE-PERSONA
BEHAVIOURAL-INSTRUCTIONS
DOCUMENTATION
TEST-COVERAGE
PATH-VERIFICATION
NAMING
UNKNOWN
```

This matters because:

```text
mpm CLI works
```

and:

```text
Pi/OpenClaw/Claude/OpenCode/Hermes integration works
```

are different claims.

---

# 23. Final report format

Produce a concise report headed:

```text
MPM Integration Check
```

Include:

```text
Date:
Framework:
Framework/runtime version:
Model:
MPM source HEAD:
Cached upstream (origin/main) before fetch:
Fetched upstream (if performed):
Installed MPM build (mpm --version):
Overall result:
```

Then report:

| Area | Status | Evidence / finding |
|---|---|---|
| Framework detection | | |
| MPM binary resolution | | |
| mpm freshness (binary hash vs source) | | |
| Installed binary freshness (mpm-mcp/scheduler/critic/telemetry) | | |
| Remote source freshness | | |
| Combined source/binaries/remote verdict | | |
| MPM core health | | |
| Database / FTS health | | |
| Tool discovery | | |
| Native integration | | |
| CLI fallback | | |
| Wake/context | | |
| Handoff/scratchpad | | |
| Session identity | | |
| Provenance | | |
| Scheduler | | |
| Mode/persona | | |
| Skill discovery | | |
| Persistent-agent surfaces | | |
| Behavioural instructions | | |
| Documentation consistency | | |
| Relevant tests | | |

Then provide:

## Problems found

Only concrete defects or meaningful warnings.

For each:

```text
Severity:
Layer:
Finding:
Evidence:
Impact:
Suggested repair:
```

Do not repair it during the check.

## Unknowns

List anything that could not be established safely.

## Maintenance advisories

Operational items that are not subsystem defects, kept separate from problems:

```text
- <advisory>: <brief>
```

## Integration capability summary

Summarise what the current agent can actually do, for example:

```text
Persistent memory:
Context recovery:
Cross-session handoff:
Scratchpad:
Work continuity:
Scheduled wake/follow-up:
Skills:
Decisions/theories/evidence:
Mode/persona:
Provenance:
```

Use capability evidence, not assumptions.

---

# 24. Overall result rules

Use:

### PASS

The host integration is operational for its documented capabilities, core health is good, native/fallback paths work as designed, source/binaries/remote freshness all check out (including an actual remote fetch in this diagnostic), and no material integration defect was found.

### PASS WITH WARNINGS

The integration works, but one or more non-blocking issues exist, such as documentation drift, optional provenance gaps, stale binaries not on the active path, maintenance advisories (review backlog, startup stabilization), or capabilities intentionally available only through fallback.

### FAIL

A material documented integration path is broken, including cases such as:

```text
host cannot reach MPM
native integration fails to load
required MCP binding is broken
wrong workspace is used
database/FTS failure blocks normal calls
wake/context contract is broken
provenance is materially false
required scheduler path is broken
documented fallback does not work
installed binary freshness fails
remote source freshness fails AND installed binaries match stale local HEAD
```

### UNKNOWN

There is insufficient evidence to determine whether the integration is operational. This includes:

```text
fetch was not performed and remote freshness cannot be established
host runtime injection could not be inspected
tool surface could not be derived from a live registry
```

`UNKNOWN` is a legitimate, non-deceptive verdict. Do not collapse `UNKNOWN` into `PASS` to make the report look better.

---

# 25. Architectural invariants

The check should treat these as current architectural constraints unless the current repository explicitly supersedes them:

```text
No session watchers.
No memory watchers.
No database-path watchers.
No replacement persistence system.
No second scheduler.
No legacy Python MCP shim architecture.
No current mpm_session exposure.
Handoff session identity is optional where the host does not supply it.
Framework provenance must be truthful.
Model provenance must not be fabricated.
The live Registry/implementation is authoritative for the tool surface.
Native typed exposure and full substrate capability are not necessarily identical.
A running daemon is not proof of end-to-end behaviour.
A config file is not proof of runtime behaviour.
A successful CLI call is not proof of native host integration.
A clean local working tree is not proof of remote freshness.
HEAD == cached origin/main is not proof of network-currency.
```

The purpose of this document is to establish what the **running agent can actually do with MPM now**.

Evidence outranks configuration, documentation, and historical validation.
