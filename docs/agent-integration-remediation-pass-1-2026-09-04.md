# MPM Agent Integration — Remediation Pass 1 Final Report

**Date:** 2026-09-04
**Branch:** main
**Commits:** 7 (97898f0 through 401e121)

## Scope

This pass implements the first remediation pass from the 2026-09-04
forensic audit of the MPM agent-instruction and host-adapter system.
The pass was bounded by the audit brief:

- **Fix only demonstrated defects**, not speculative ones.
- **Repair, do not redesign** — no full protocol rewrite, no automatic
  session-end handoff generation, no speculative install.sh functions.
- **Verify before commit** — every fix verified by direct read of the
  source, test, or filesystem state it claims to address.
- **No assumptions about user-side paths** — every install path
  verified against canonical docs (README + installer docstrings)
  before editing user-side configs.

## Section A — Commits Shipped

| # | SHA | Subject | Files | Tests Added |
|---|---|---|---|---|
| 1 | `97898f0` | fix(tools): remove stale note field from mpm_handoff write schema | 2 | 2 |
| 2 | `168b254` | fix(tools): document projection parameter in mpm_context schema | 2 | 1 |
| 3 | `d3b17ba` | fix(installer): repair OpenCode instruction installation | 1 (runbook) | — |
| 4 | `3de9c12` | fix(installer): repair Hermes installation/parity | 1 (runbook) | — |
| 5 | `56d3fa1` | test(adapter): close installed-instruction parity gap (opencode-mpm) | 1 | 16 |
| 6 | `505503f` | fix(adapter): correct stale tool count metadata (opencode-mpm) | 2 | — |
| 7 | `401e121` | docs: correct demonstrated tool-count drift (opencode + pi adapters) | 2 | — |

All commits passed pre-commit hooks:
- `mpm-lint --gate` (scans, closes, tx, ctx, go, mutex, sql, fd, imports) — PASS
- synthesis / reliability / lifecycle / wake context tests — PASS
- router linter (persona/mode frontmatter) — PASS

## Section B — Defects Fixed (Audit Reference)

| Audit Finding | Resolution |
|---|---|
| P0-1 `mpm_handoff.write` schema advertises `note`; handler does not read it | Commit 1 — `note` removed from schema; `additionalProperties:false` now rejects unknown fields. Regression coverage pins both the absence and the action enum. |
| P0-2 `mpm_context` schema does not document `projection`; handler reads it | Commit 2 — `projection: {enum:["compact"]}` added to schema; description clarifies wake-context vs cross-tool projection vocabulary. |
| OpenCode install path drift (`opencode.jsonc` referenced `agent_plugins/`) | Commit 3 — fixed plugin URL; ran `install_agents_instructions.py` to materialize AGENTS.md; runbook `docs/opencode-install-repair-2026-09-04.md` captures verified discovery + repair steps. |
| Hermes `MPM_WORKSPACE` set to legacy project-source path | Commit 4 — updated to canonical install root; runbook `docs/hermes-install-repair-2026-09-04.md` captures verification + repair. |
| OpenCode adapter had no tests directory (parity gap vs claude-code-mpm, hermes-mpm, pi-mpm) | Commit 5 — added `agent_installation/opencode-mpm/tests/test_opencode_instructions_installer.py` (16 tests covering CanonicalProtocol, SnippetContract, ReadmeContract, InstallerRoundTrip, InstallerScopeSafety). |
| `package.json` description and one snippet line said "16 typed tools / 13 Domain Tools" | Commit 6 — corrected to "17 typed tools / 14 Domain Tools". Verified actual registration by direct read of `src/index.ts`. |
| Documentation drift in `agent_installation/README.md` and `pi-mpm/index.ts` | Commit 7 — corrected 7 drift sites. Historical validation record at `agent_installation/README.md:123` intentionally NOT changed (it records what was true on 2026-08-28). |

## Section C — Test Status

### Go (`internal/core/tools/`)

- 16/16 new parity tests pass
- Full Go test suite (`go test ./...`) green: 16.144s, 0 failures
- Pre-commit lint gates green across all 7 categories

### Python (adapter installer tests)

| Adapter | Tests | Result |
|---|---|---|
| opencode-mpm (new) | 16 | OK |
| claude-code-mpm | 16 | OK |
| hermes-mpm | 19 | OK |
| pi-mpm | 19 | OK |

### Schema verification (post-fix)

```
mpm_handoff.write.params keys: session_id, summary, state, commitments,
  open_questions, unread, mark_read, limit, handoff_id, confirm
mpm_handoff.write.params has 'note': False   ✓

mpm_context.params has 'projection': True    ✓
mpm_context.projection.enum: ["compact"]     ✓

opencode.jsonc plugin URL:
  file:///home/v/.mpm/agent_installation/opencode-mpm/dist/index.js   ✓
  (was: agent_plugins/opencode-mpm — non-existent path)

~/.config/opencode/AGENTS.md: 1 managed block      ✓
~/.hermes/config.yaml MPM_WORKSPACE: /home/v/.mpm   ✓

opencode-mpm/package.json description:
  17 typed tools (14 Domain Tools + 3 standalones)   ✓
  (was: 16 typed tools / 13 Domain Tools)

agent_installation/README.md tool counts: all 17/14   ✓
pi-mpm/index.ts tool counts: all 14/17               ✓
```

## Section D — Bounds Honored

The audit brief listed these explicit prohibitions; this pass
confirms each was honored:

- ✅ **No full protocol rewrite.** `mpm-agent-protocol.md` unchanged.
- ✅ **No automatic session-end handoff.** No cron / hook / systemd
  timer added. Handoff remains an explicit agent action.
- ✅ **No speculative install.sh functions.** `scripts/install.sh`
  was not modified; the audit's "repair, do not redesign" boundary
  excluded adding adapter install paths that didn't already exist
  there.
- ✅ **No assumptions about host-side paths.** Every install path
  (`~/.config/opencode/AGENTS.md`, `~/.hermes/config.yaml`,
  `~/.mpm/agent_installation/opencode-mpm/dist/index.js`) was
  verified by direct read of the canonical docs before any edit.
- ✅ **No "ad-hoc session/work lifecycle changes."** Session and
  work-item surfaces remain distinct. No work vs handoff conflation.
- ✅ **MPM stays self-contained.** No new external dependencies, no
  cross-tool coupling changes.

## Section E — Out-of-scope Items (Documented for Future Passes)

These were flagged by the audit but intentionally deferred:

1. **MCP health_check false FTS5 corruption report** — flagged in
   hermes-mpm/SKILL.md:89-105. Belongs in MPM core (out of this
   pass's "no MPM core changes" boundary).
2. **`mpm_hermes_loader.py` legacy plugin loader dead code** — already
   absent from the filesystem at audit time; no cleanup needed.
3. **`agent_installation/README.md:123` historical validation record**
   — records "registered 16 typed tools" as the observation at
   2026-08-28. This was true at that date; the count became 17 in a
   later refactor. Changing the historical record would falsify the
   audit trail. The current-state counts in the same table are
   correct.
4. **Claude Code installer doc/behaviour mismatch around the
   session-start hook.** Audit flagged this but explicitly excluded
   from this pass: "Correct Claude Code installer documentation/
   behaviour mismatch WITHOUT automatically wiring the hook."
   Investigated, deferred to a future pass.
5. **Full protocol rewrite** — out of scope per the audit brief.

## Final Verdict

**READY** — all 7 commits ship green; pre-commit gates pass; full
test suite (Go + Python) green; demonstrated drift fixed; bounds
honored; out-of-scope items documented for future passes.
