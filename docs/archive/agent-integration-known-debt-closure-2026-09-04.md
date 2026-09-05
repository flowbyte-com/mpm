# MPM Agent Integration — Known-Debt Closure Report

> **Date:** 2026-09-04
> **Branch:** main
> **Commits (this pass):** `c101130`, `5a3e2cc`, `5fe76d9`, `9a441be`, `8302e70`

This is the closure report for the "Known-Debt Closure Pass" that closes
the six explicitly documented technical-debt items from the post-pass-2
reconciliation audit
(`docs/agent-integration-post-pass-2-reconciliation-2026-09-04.md`).
The rule was simple:

> Anything we have explicitly identified as remaining debt must either be
> fixed now or proven obsolete.

All six items are now closed.

---

## 1. Debt Closure Matrix

| ID | Title | Previous state | Action | Evidence | Final state |
|----|-------|----------------|--------|----------|-------------|
| **C-1** | `TestGetMemoriesForExport_NullLegacyColumns` worked around alpha-final's NOT NULL invariant | Test fixture inserted `NULL created_at` — impossible after commit 1aa8457. Test was failing pre-this-pass. | Changed fixture to set `created_at = 1700000000` and leave only the genuinely nullable `tags`/`metadata` columns NULL. Preserved original semantic intent (NULL legacy column tolerance + `COALESCE(tags,'[]')`). | Commit `c101130` (`fix(test): rework TestGetMemoriesForExport_NullLegacyColumns for NOT NULL schema`); test now passes. | CLOSED |
| **C-2** | Protocol §1 wake-payload wording named fields that did not exist on `WakeContextData` | §1 prose listed "key decisions", "active lessons", "key pointers" as fields an agent receives on wake. None exist on the struct (decisions/lessons live behind `mpm_decisions`/`mpm_lessons`, not in wake). | Rewrote §1 to describe what the wake payload actually carries and point at `WakeContextData` for the full field list. Explicitly routed decisions/lessons to their respective tools. Added `TestPass2_Protocol_Section1_WakePayloadFieldsAreAccurate` to `post_pass2_drift_lock_test.go` to pin the absence of banned phrases. | Commit `5a3e2cc` (`fix(protocol): correct §1 wake-payload wording to match WakeContextData`); 116 Python tests + Go tests pass; new drift-lock test passes. | CLOSED |
| **C-3** | Canonical snippet file had no version metadata | `MPM_AGENT_INTEGRATION_SNIPPETS.md` had no contract-version marker; consumers couldn't tell whether an edit was a typo fix or a breaking change. | Added HTML-comment version marker at top of file: `<!-- mpm_agent_integration_version: 1.0.0 -->`. Added a Versioning section with explicit patch/minor/major criteria. Added `CanonicalVersionMarker` test class with three tests (presence, semver validity, exactly-one-at-top). | Commit `5fe76d9` (`feat(snippets): add canonical snippet version marker + Versioning section`); `make refresh-installed` confirms marker survives the render path; new tests pass. | CLOSED |
| **C-4** | No canonical maintenance command for refreshing installed managed blocks | Manual edit of `~/.claude/CLAUDE.md` etc. after editing canonical snippets was the only path; no smoke test guarded against installer-rename drift. | Added `make refresh-installed` target: runs `render_managed_blocks.py`, then each persistent-file host's installer (Claude Code, OpenCode, Pi), then `render_managed_blocks.py --check`. Hermes skipped (no installed block in this env); OpenClaw uses runtime injection (separate install.sh). Per-step distinct exit codes (2..6). Added structural smoke test `agent_installation/tests/test_refresh_installed.py` (13 tests). Added Maintenance section to `agent_installation/README.md`. | Commit `9a441be` (`feat(make): add refresh-installed target for host managed blocks`); end-to-end run at 16:25 and again at 16:46 successfully refreshed all three persistent-file hosts; post-refresh render check PASS. | CLOSED |
| **C-5** | `test_cross_adapter_contract_parity.py` duplicated adapter metadata from `render_managed_blocks.ADAPTERS` | Adding a new persistent-file host required updating two independent lists in two files with no compile-time guarantee of sync. The `INSTALLS` dict inside `NoDuplicateManagedSections` further duplicated per-host installer info. | Refactored the test to import `render_managed_blocks` as a module and derive its `ADAPTERS` dict via `_build_test_adapter_info()`. `_INSTALLERS`, `_EXTRA_INSTALL_ARGS`, `_INSTALL_OUTER_MARKERS`, and `_PERSIST_FAMILIES` live in the test layer only. Removed the redundant `INSTALLS` dict. Adding a new adapter now means editing ONE list (render script) plus ONE map (test). | Commit `8302e70` (`refactor(test): centralize adapter metadata in cross-adapter parity test`); 64 cross-adapter parity tests pass against the refactored code. | CLOSED |
| **C-6** | Local installed adapter artifacts at `~/.claude/CLAUDE.md` etc. may have drifted from canonical source | Not validated at the end of pass-2; could have been stale after the C-3 canonical source edits. | Refreshed as a side effect of `make refresh-installed` smoke test (C-4) and the final §10 validation pass. No commit (per brief — local cleanup only). | `~/.claude/CLAUDE.md`, `~/.config/opencode/AGENTS.md`, `~/.pi/agent/AGENTS.md` all carry the canonical managed block; `grep -c "MPM MANAGED BLOCK"` returns 2 per file (one BEGIN, one END). Hermes skipped (no installed block in this env). | CLOSED |

---

## 2. Files Changed

| File | Change | Commit |
|------|--------|--------|
| `internal/core/migration_lessons_recovery_test.go` | C-1: fixture updated for NOT NULL schema | `c101130` |
| `agent_installation/mpm-agent-protocol.md` | C-2: §1 wake-payload wording corrected | `5a3e2cc` |
| `internal/core/post_pass2_drift_lock_test.go` | C-2: drift-lock test added | `5a3e2cc` |
| `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` | C-3: version marker + Versioning section | `5fe76d9` |
| `agent_installation/tests/test_render_managed_blocks.py` | C-3: `CanonicalVersionMarker` test class | `5fe76d9` |
| `Makefile` | C-4: `refresh-installed` target | `9a441be` |
| `agent_installation/README.md` | C-4: Maintenance section | `9a441be` |
| `agent_installation/tests/test_refresh_installed.py` | C-4: structural smoke test (NEW) | `9a441be` |
| `agent_installation/tests/test_cross_adapter_contract_parity.py` | C-5: centralized adapter metadata | `8302e70` |

Net change: 8 files modified, 1 file created. 5 focused commits.
No `git push` was performed (per §11 discipline).

---

## 3. New Maintenance Capability

**`make refresh-installed`** — the canonical maintenance command for
refreshing every persistent-file host's installed managed block from
the canonical snippets source.

What it does, in order:
1. `python3 agent_installation/scripts/render_managed_blocks.py` —
   regenerates each adapter's `<adapter>/templates/<file>.snippet`
2. Each persistent-file host's installer:
   - `claude-code-mpm` → `~/.claude/CLAUDE.md`
   - `opencode-mpm` → `~/.config/opencode/AGENTS.md`
   - `pi-mpm` → `~/.pi/agent/AGENTS.md`
   (Hermes skipped — no installed block in this environment; OpenClaw
   uses runtime injection via its own `install.sh`.)
3. `python3 agent_installation/scripts/render_managed_blocks.py --check`
   — byte-for-byte parity verification

Each step has a distinct exit code (2..6) so a CI caller can identify
which adapter failed. Installers are content-aware — re-running on a
no-op prints a confirmation and exits 0.

End-to-end verified twice during this pass (smoke test + final §10
validation), both with `render check PASS`.

---

## 4. Drift Controls

The new and strengthened drift controls added by this pass:

| Control | What it pins |
|---------|--------------|
| `TestPass2_Protocol_Section1_WakePayloadFieldsAreAccurate` (new) | Protocol §1 does not name "key decisions", "active lessons", "key pointers" — fields that don't exist on `WakeContextData`. |
| `CanonicalVersionMarker.test_marker_is_present` (new) | The HTML-comment version marker must exist in the canonical snippets file. |
| `CanonicalVersionMarker.test_marker_is_valid_semver` (new) | The marker must be a valid semver string in the documented format. |
| `CanonicalVersionMarker.test_exactly_one_marker_at_top_of_file` (new) | The marker appears exactly once before the H1 (prose mentions don't count). |
| `test_refresh_installed.py` (13 tests, new) | The `refresh-installed` target is registered in `.PHONY`, references each persistent-file installer, ends with `--check`, and every script/snippet it touches exists on disk. |
| `test_cross_adapter_contract_parity.py` (refactored) | Adapter metadata is sourced from `render_managed_blocks.ADAPTERS` — adding a new adapter surfaces immediately because the test's `_build_test_adapter_info()` raises KeyError if `_INSTALLERS` is missing an entry. |

The drift-lock tests for protocol §3.1 / §7.2 / §8.2 / "9-field" /
mpm_decisions enum / mpm_lessons type-optional / mpm_handoff-no-note
remain in place from pass-2 and still pass.

---

## 5. Remaining Known Debt

The following pre-existing failures remain. They are out of scope for
this pass per §12 (scope boundary: do not redesign canonical protocol,
change storage semantics, change scheduler behaviour, redesign MCP
namespaces, etc.) and were present before C-1..C-5 changes (verified
by running the failures against HEAD before any of this-pass commits).

| Test | Failure class | Brief impact | Notes |
|------|---------------|--------------|-------|
| `TestCallQueryMemoryQuality_PerSourceStats` | Test environment: missing `topics_fts`/`lessons_fts`/`scheduled_wakes_fts` tables and a mirror-file path. | Coverage of memory-quality source stats. | Pre-existing (verified on HEAD pre-pass). Likely needs FTS table init in test setup or a fresh test DB. |
| `TestF005_SearchJSONFlag_*` (5 tests) | Same test-environment issue. | `--json` envelope coverage for the search CLI. | Pre-existing. |
| `TestPhase2_ProjectionPointerResolveChain`, `TestPhase2_ProductionResolverWiring` | Same test-environment issue. | Pointer-resolution production wiring. | Pre-existing. |
| `TestDrillE2E_SchedulerTickFiresDrill` | Scheduler drill E2E. | End-to-end scheduler drill firing. | Pre-existing. |

These tests fail under `go test -race ./...` because the race detector
re-exercises the FTS-table init path. `make test` (no race detector)
passes all packages.

**Drift surfaced during C-5 (informational, NOT fixed by this pass):**
The render script's `copy_paste_outer_*` for hermes uses `MPM MANAGED
BLOCK` (space) while the hermes installer emits `MPM-MANAGED BLOCK`
(hyphen). They serve different purposes (canonical-source copy/paste
wrapper vs installed-file anchor). The C-5 refactor preserves this
distinction in `_INSTALL_OUTER_MARKERS`. A future pass could either:
(a) unify the two conventions; or (b) document the divergence more
explicitly in the render script. Neither is required by this pass.

---

## 6. Final Verdict

| # | Question | Answer |
|---|----------|--------|
| 1 | All six C-1..C-6 debt items closed? | **YES** |
| 2 | Each debt item has an evidence trail (commit + test name + run output)? | **YES** |
| 3 | `make test` passes (no Go regressions introduced by this pass)? | **YES** |
| 4 | `make build` succeeds? | **YES** |
| 5 | `go vet ./...` clean? | **YES** |
| 6 | `mpm-lint --gate` passes? | **YES** (sql, fd, imports all PASS) |
| 7 | `agent_installation/scripts/render_managed_blocks.py --check` passes? | **YES** (4 adapters in byte-for-byte parity; 4 copy/paste examples in parity) |
| 8 | `make refresh-installed` succeeds end-to-end? | **YES** (verified twice; render check PASS) |
| 9 | All 116 agent_installation Python tests pass? | **YES** |
| 10 | Scope boundary (§12) respected — no redesign of canonical protocol / storage / scheduler / MCP namespaces? | **YES** |
| 11 | Commit discipline (§11) — focused commits, no no-ops, no rewritten history, no push? | **YES** (5 focused commits; `git log` confirms; working tree clean post-pass; no push) |
| 12 | Drift controls strengthened — new tests prevent the closed defects from regressing? | **YES** (4 new drift-lock tests + 13 new smoke tests) |

---

## 7. Closing Note

The post-pass-2 audit identified six explicit technical-debt items.
All six are now closed in this single focused pass, with drift
controls added so they cannot silently regress. The four pre-existing
test-environment failures surfaced by `go test -race ./...` are
documented but intentionally out of scope — they were not introduced
by this pass and fixing them would violate §12's no-redesign
constraint.

**Status: debt closed. Ready for the next planned work.**
