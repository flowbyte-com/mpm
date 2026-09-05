# claude-code-mpm

Claude Code ↔ MPM integration. Wires the full MPM cognitive substrate into
Claude Code as **22 native MCP tools** (20 unified domain tools plus 2
standalones: `mpm__mpm_memory`, `mpm__mpm_handoff`, `mpm__mpm_scratchpad`,
`mpm__mpm_retrieval_diagnose`, `mpm__mpm_wakes`, …).

## Why this exists

Claude Code's native extension surface is **MCP** (Model Context Protocol).
The canonical MPM machine-facing MCP server is `~/.mpm/bin/mpm-mcp` — the
same binary OpenClaw uses. Rather than write a TypeScript plugin (the
opencode-mpm pattern) or a shell wrapper (a fallback that breaks under
non-interactive subprocess), we reuse the canonical MCP server and let
Claude Code's runtime discover it via the standard `~/.claude/.mcp.json`
side-channel.

This is the same wiring the Claude Code marketplace plugins use (telegram,
serena, firebase — each declares an `.mcp.json` with a `mcpServers` block).

## Coverage

**21 tools**, identical to the opencode-mpm and pi-mpm agents:

| Layer | Count | Tool names |
|---|---|---|
| Unified Domain Tools (Fat RPC) | 19 | `mpm__mpm_memory` (action: save/query/show/shred/reinforce/weaken/snooze/set_weight/patch/promote/review/synthesize/challenge/restore_challenge/commit_milestone), `mpm__mpm_wakes`, `mpm__mpm_theories`, `mpm__mpm_lessons`, `mpm__mpm_decisions`, `mpm__mpm_topics`, `mpm__mpm_references`, `mpm__mpm_evidence`, `mpm__mpm_confidence`, `mpm__mpm_context`, `mpm__mpm_skills`, `mpm__mpm_handoff`, `mpm__mpm_scratchpad`, `mpm__mpm_system`, `mpm__mpm_work`, `mpm__mpm_resolve`, `mpm__mpm_blob_read`, `mpm__mpm_blob_search`, `mpm__mpm_retrieval_diagnose` |
| Standalone tools | 2 | `mpm__log_to_changelog`, `mpm__request_review` |

The 19 Domain Tools share the same `(action, params)` shape. The 2
Standalones have their own narrower schemas. See `~/.mpm/bin/mpm-mcp`'s
`tools/list` JSON-RPC method for the canonical schemas.

## Install

```bash
# 1. Materialize the wiring into ~/.claude/.mcp.json
./install.sh

# 2. Restart Claude Code (mcpServers are loaded at session start)
#    Without restart, the `mpm__*` tools will not appear in the tool list.

# 3. Verify
./install.sh --verify   # or directly: ./verify.py
```

The install script:
- Backs up any existing `~/.claude/.mcp.json` to `~/.claude/backups/claude-code-mpm-<TS>/`
- Materializes `.mcp.json.template` into `~/.claude/.mcp.json` with `${HOME}` substituted
- **Merges** with any existing `mcpServers` — never clobbers other servers
- Sanity-probes the wiring by booting `mpm-mcp` and calling `mpm_system health_check`

It does **not** touch `~/.claude/settings.json`. The `.mcp.json` side-channel
is the canonical Claude Code wire location for MCP server entries.

## Uninstall

```bash
./install.sh --uninstall
```

Removes the `mpm` entry from `~/.claude/.mcp.json` (or removes the file if
it was the only entry). The shell `--uninstall` mode does **not** strip
the managed CLAUDE.md section — to do that, run the Python uninstaller
directly:

```bash
python3 ~/.mpm/agent_installation/claude-code-mpm/scripts/install_claude_instructions.py \
    --scope user --home "$HOME" \
    --target ~/.claude/CLAUDE.md --uninstall
```

Original state is preserved in
`~/.claude/backups/claude-code-mpm-<TS>/`.

## Path resolution

The install script hardcodes the canonical install path
(`${HOME}/.mpm/bin/mpm-mcp`) and the canonical workspace
(`${HOME}/.mpm`). It does **not** read `MPM_BINARY` or `MPM_WORKSPACE`
env vars for resolution — the same deterministic absolute path is what
the opencode-mpm and openclaw-mpm integrations expect. To override,
edit `~/.claude/.mcp.json` after install.

The `~/.mcp.json` template uses `${HOME}` substitution so the same template
works on any machine. The install script resolves `${HOME}` to an absolute
path at install time.

`MPM_WORKSPACE` is set to `${HOME}/.mpm` — the canonical install root. The
canonical install's `src/db/mpm.db` is hardlinked to the actual workspace
DB, so this is a portable, deterministic workspace handle that does not
depend on the operator's `MPM_WORKSPACE` env var layout. The health_check
response carries the resolved `db_path` so operators can verify convergence:

```json
{
  "ok": true,
  "db_path": "$HOME/.mpm/src/db/mpm.db",
  "db_path_raw": "$HOME/.mpm/src/db/mpm.db",
  ...
}
```

## Framework identification (provenance contract)

Claude Code identifies itself to the MPM substrate via the canonical
framework-id env var **`MPM_FRAMEWORK=claude-code`**. This is documented
in MPM's `README.md` §1827 as the wire contract that `mpm-mcp` reads
through `mpmcli.ActiveContextFromEnv` → `ActiveContext.FrameworkName`
→ `tool_invocations.framework_name` and `artifact_provenance.framework_name`.

Without this var, `mpm-mcp` falls through to the default
`framework_name=mcp` and Claude Code-originated artifacts are
misattributed (they look like unattributed MCP-server calls).

**What we set, and why:**

| Var | Value | Static? | Rationale |
|---|---|---|---|
| `MPM_WORKSPACE` | `${HOME}/.mpm` | yes | Canonical install root; portable across machines |
| `MPM_FRAMEWORK` | `claude-code` | yes | Claude Code is the calling agent framework for the entire MCP session |

**What we deliberately do NOT set, and why:**

- `MPM_PROVENANCE_MODEL` — static would be a fabrication. The actual
  model in use is dynamic per session (MiniMax-M2.7 / MiniMax-M3 / opus-5 etc.).
  Leave NULL; mpm-mcp will not lie about model identity.
- `MPM_PROVENANCE_INVOCATION_ID` and
  `MPM_PROVENANCE_PARENT_INVOCATION_ID` — must be generated dynamically
  per invocation by the framework layer. No sensible static value.
- `MPM_PROVENANCE_ACTOR_KIND=agent` — redundant with `mpm-mcp`'s
  audit_hook default (`cmd/mpm-mcp/audit_hook.go` hardcodes
  `actor_kind=agent` for every MCP-server call). Adding it would
  imply a contract this codebase does not implement.

**Provenance is observational, not authoritative.** Artifact correctness
remains independent of provenance availability; missing or incorrect
provenance must never block or poison an artifact transaction.
This is by design — provenance is for attribution and forensics, not for
artifact truth.

**Historical note (2026-08-28 triage):** before this fix, only 2 of
5477 `artifact_provenance` rows attributed to claude-code. The empty-env
template caused Claude Code MCP calls to fall through to `framework_name=mcp`.
After this fix, new Claude Code MCP-originated artifacts record
`framework_name=claude-code` correctly. Historical rows are NOT rewritten
— those represent past attribution, not corrected present state.

## Verification

`./verify.sh` runs the full integration test suite end-to-end against the
live `mpm-mcp` binary. Tests:

| Test | What it proves |
|---|---|
| A — MPM discovery | `mpm_system health_check` returns `ok:true` |
| B — Durable memory write | `mpm_memory save` persists a uniquely-tagged probe |
| C — Memory retrieval | `mpm_memory query` finds the probe by ID |
| D — Retrieval diagnostics | `mpm_retrieval_diagnose` returns structured per-node breakdown |
| E — Cross-session continuity | `mpm_handoff write` writes a handoff that `mpm_handoff list` sees |
| F — Missing MPM handling | A bad `MPM_WORKSPACE` does not fabricate success |
| G — Malformed input handling | An unknown action returns a structured error, not fabricated success |
| H — Non-zero exit semantics | Malformed JSON-RPC produces a non-zero exit |
| I — PATH independence | A clean env (HOME only) resolves and runs the integration |
| J — Shared substrate | The DB the agent uses is the same inode as the canonical DB |

Cleanup is automatic: probe memories and the test handoff are removed via
the supported `mpm_memory shred` and `mpm_handoff(action: "shred")`
interfaces.

## Limitations

- **No live tool-call test in this validation.** Claude Code loads
  `mcpServers` at session start. Verifying that the `mpm__*` tools appear in
  Claude Code's actual tool list requires restarting the Claude Code session
  — out of scope for this script. The verification suite proves the
  binding works by driving `mpm-mcp` directly via the same JSON-RPC
  contract Claude Code uses internally; the binding is therefore
  equivalent to a live tool-call test.
- **PATH independence in the install script.** The install script uses
  `readlink -f`, `python3`, and `date -u`. These are POSIX-standard and
  expected on any Linux host. macOS ships `date` and `readlink` (and
  `python3` is universal); the installer is portable.

## Test history

See `VALIDATION-2026-08-19.md` for the adversarial alpha-integration
validation run on 2026-08-19.

The 2026-08-28 follow-up triage introduced `tests/test_template_provenance_env.py`
which pins the framework-identification env contract against the template
and the materialized `~/.claude/.mcp.json`. Run with:

```bash
PYTHONPATH=. python3 -m unittest tests.test_template_provenance_env -v
```

The suite covers:
- Template: `MPM_FRAMEWORK=claude-code` present; no static model/invocation IDs
- Materialized config: matches template; no `MPM_PROVENANCE_*` overrides
- Idempotent materialization: running the installer twice yields identical env blocks

## See also

- `~/.mpm/agent_installation/opencode-mpm/` — same MCP server, TypeScript plugin mechanism
- `~/.mpm/agent_installation/openclaw-mpm-memory/` — same MCP server, OpenClaw plugin mechanism
- `~/.mpm/agent_installation/pi-mpm/` — same MCP server, Pi subprocess bridge (no MCP support in Pi)
- `~/.mpm/agent_installation/README.md` — index of all integrations
