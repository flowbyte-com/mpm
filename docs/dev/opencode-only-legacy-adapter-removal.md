# Legacy agent adapter removal — dependency matrix and proposed change set

> **Status:** PROPOSAL. **Nothing in this document has been applied.**
> No adapter directory has been removed, no registry edited, no installer
> behaviour changed, no test floor lowered.
>
> **Author:** Session C · **Prepared:** 2026-10-27
> **Base:** `origin/main` @ `d3405d6`
> **Decision owner:** Session A (root installer, Makefile, shared CI/release
> gates, integration decisions)
> **Prerequisite:** PR #125 (`ebbb38b`) — the OpenCode documentation
> corrections and the retirement of `AUTO_AGENT_INSTALL.md`. This document
> assumes #125 is merged, because #125 is what establishes the
> OpenCode-only supported-host wording that the removal PR then propagates
> through the shared documents.
>
> **What this document is for.** "Unsupported for this release" is *not*
> "safe to delete". A legacy adapter can be unsupported and still be
> load-bearing for an upgrade path, a provenance string, a documented
> library, or a guarantee the release would otherwise silently withdraw.
> Every RETIRE row below therefore carries the evidence for its dependents,
> and the four rows that need a human decision are called out explicitly
> rather than decided unilaterally.

## 0. How this was established

Every row below is backed by a reference that was **read**, not inferred:

| Evidence | How |
|---|---|
| External references to each adapter | `rg` over the whole repo for the directory basename and each distinctive identifier, then every hit classified by hand |
| Registry membership | The five registries were read directly (Makefile, `render_managed_blocks.py`, `check_installed_managed_blocks.py`, `cmd/mpm/onboard_hosts.go`, `cmd/mpm/onboard_cmds.go`) |
| Fresh-install behaviour | **Measured**: `reconcile_managed_blocks.py --home <disposable>` against an empty `HOME` (§2) |
| Test populations | **Measured**: `python3 -m unittest discover tests` per adapter and per umbrella file |
| Substring false positives | Enumerated and discarded (§7) |

---

## 1. The five registries

Treating the adapter set as a set is done in **five** places. Editing one
without the others is what the two Go↔Python drift guards exist to catch.

| # | File:line | What it holds | Edit needed on removal |
|---|---|---|---|
| R1 | `Makefile:198` | `AGENT_ADAPTERS := mpm-claude-code mpm-hermes mpm-memory-openclaw mpm-opencode mpm-pi` | drop the four |
| R2 | `Makefile:200` | `AGENT_ADAPTERS_JS := mpm-memory-openclaw mpm-auto-mode-persona-openclaw` | becomes **empty** |
| R3 | `Makefile:209` | `AGENT_ADAPTERS_TS := mpm-opencode` | unchanged |
| R4 | `agent_installation/scripts/render_managed_blocks.py:70–337` | `ADAPTERS` — 5 entries (claude-code 72, opencode 115, pi 148, hermes 181, memory-openclaw 230) | drop 4 entries |
| R5 | `agent_installation/scripts/check_installed_managed_blocks.py:99–136` | `HOST_INSTALL_TARGETS` — 4 entries (pi 102, claude_code 110, opencode 118, openclaw 126) + `SECTION_BUILDERS` (151) | drop 3 entries + `SECTION_BUILDERS` |
| R6 | `cmd/mpm/onboard_hosts.go:47–52` | `onboardHostAdapters` — claude_code, openclaw, opencode, pi | drop 3 |
| R7 | `cmd/mpm/onboard_cmds.go:1121` | `onboardHostLabels` — claude_code, openclaw, opencode, pi | drop 3 |

**Drift guards that fire if R4/R5 and R6/R7 diverge:**
`cmd/mpm/onboard_hosts_test.go:358–405` (`TestOnboardHostAdaptersMatchPython`)
and `cmd/mpm/onboard_cmds_test.go:1143–1178`
(`TestOnboardHostInventoryMatchesPython`).

**Enumerations that need NO edit** (they are scan-based):
`agent_installation/scripts/reconcile_managed_blocks.py:174`
(`AGENT_INSTALLATION_DIR.glob("mpm-*")` → `reconcile.json`),
`scripts/install_runtime_assets.py:416–438`,
`internal/core/tools/adapter_schema_guard_test.go:114`.

> **This is the key structural fact:** removing an adapter *directory* removes
> it from the reconciler, the runtime staging and the schema guard
> automatically. The registries above are the only places that need an edit.

---

## 2. The behavioural problem, measured

`reconcile_managed_blocks.py --home <empty disposable HOME>`:

```
[reconcile]  discovering 6 adapter(s)
[reconcile]  claude_code: ok — wrote fresh <HOME>/.claude/CLAUDE.md
[reconcile]  opencode:    ok — wrote fresh <HOME>/.config/opencode/AGENTS.md
[reconcile]  pi:          ok — install: fresh (<HOME>/.pi/agent/AGENTS.md)
[reconcile]  openclaw:    SKIP (integration not installed)
[reconcile]  hermes:      SKIP (Hermes has no persistent managed block)
[reconcile]  openclaw_auto_mode_persona: SKIP
```

**A fresh install on a machine with no Claude Code, no Pi and no OpenCode
writes three host instruction files.** `mpm-claude-code` and `mpm-pi` have
`opt_in: false` and **no** `reconcile_if` gate — the reconciler treats them as
"always-on". `mpm-memory-openclaw` is gated correctly (it only reconciles when
the MPM OpenClaw integration is already present); `mpm-hermes` and
`mpm-auto-mode-persona-openclaw` are `opt_in: true`.

This is the concrete instance of *"new installations must not unexpectedly
configure unsupported hosts"*. **Removing the two adapter directories fixes
it outright** — no new gate mechanism is needed.

---

## 3. RETIRE — demonstrably unused, exclusively unsupported hosts

All five are host-exclusive. None is referenced by the MPM core at runtime.

| Path | Files | External references | Verdict |
|---|---|---|---|
| `agent_installation/mpm-claude-code/` | 24 | Makefile 198 · render_managed_blocks 72 · check_installed 110 · onboard_hosts.go 48 · template_drift_test.go 53,75,82,88 · `agent_installation/tests/` × 10 · `scripts/tests/` × 2 · `docs/SPEC.md` 1229 · `agent_installation/INSTALL.md`, `README.md`, `mpm-agent-protocol.md` (prose) | **RETIRE** — with two carve-outs (§5) |
| `agent_installation/mpm-pi/` | 18 | Makefile 198 · render_managed_blocks 148 · check_installed 102 · onboard_hosts.go 51 · template_drift_test.go 72,78,84 · `agent_installation/tests/` × 9 · `docs/ONBOARD.md` 333 | **RETIRE** — with one carve-out (§5) |
| `agent_installation/mpm-hermes/` | 9 | Makefile 198 · render_managed_blocks 181 · template_drift_test.go 56,76 · `agent_installation/tests/` × 6 | **RETIRE** |
| `agent_installation/mpm-memory-openclaw/` | 30 | Makefile 198,200 · render_managed_blocks 230 · check_installed 126,134,151 · onboard_hosts.go 49 · `internal/scheduler/ingest.go:241` (a `created_by` string, **not** a path) · `agent_installation/tests/` × 12 | **RETIRE** — with one carve-out (§5) |
| `agent_installation/mpm-auto-mode-persona-openclaw/` | 11 | Makefile 200 · `agent_installation/tests/` × 4 · sibling cross-refs inside `mpm-memory-openclaw` | **RETIRE** |
| `agent_installation/uninstall-openclaw.sh` | 1 | `scripts/tests/test_uninstall_openclaw_failure_semantics.py:40` (executes it) · `agent_installation/tests/test_uninstall_openclaw.mjs:47` (executes it) · `test_naming_invariants.py:116` · `test_agent_installation_machine_specific_paths.py:88` | **RETIRE** with the two executors |

**`mpm-pi`'s TypeScript tests are already dead.** `mpm-pi/tests/` contains
`namespace_contract.test.ts` and `test_resolve_mpm_binary.test.ts`, but
`AGENT_ADAPTERS_TS` contains only `mpm-opencode` (Makefile:209). **They are
never executed by any gate.** Removing `mpm-pi` orphans them permanently —
no coverage is lost by removing them.

---

## 4. RETAIN — needed by OpenCode, the core, or release verification

| Path | Why |
|---|---|
| `agent_installation/mpm-opencode/` (348 files) | **The release's only supported integration.** Plugin, installer, managed-block installer, JSONC writer, resolver, 119 Python + 89 TS tests |
| `agent_installation/scripts/render_managed_blocks.py` | Renders the canonical managed block for every adapter, OpenCode included (`ADAPTERS["mpm-opencode"]`, line 115). Keeps the OpenCode entry; drops the other four |
| `agent_installation/scripts/check_installed_managed_blocks.py` | The read-only drift diagnostic. Keeps `HOST_INSTALL_TARGETS["opencode"]` (line 118) |
| `agent_installation/scripts/reconcile_managed_blocks.py` | The reconciler the root installer calls. Scan-based — no edit |
| `agent_installation/scripts/block_provenance.py`, `managed_block_convergence.py` | Managed-block provenance/convergence helpers |
| `agent_installation/scripts/stage_runtime_package.sh` | Runtime staging |
| `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` | **Canonical source** of the managed block. The renderer reads it; the README block is generated from it |
| `agent_installation/mpm-agent-protocol.md` | Canonical behavioural contract. Host-independent. Installed into the runtime package by `install_runtime_assets.py` |
| `agent_installation/mpm-opencode/VALIDATION-2026-08-19.md` | Dated validation snapshot — historical record, explicitly allowlisted by `test_naming_invariants.py` |
| `agent_installation/tests/` (28 files, 494 tests) | **RETAIN and rework** — see §6 |
| `scripts/tests/` (33 files, 540 tests) | **RETAIN** — see §6 |
| `scripts/install_runtime_assets.py` | Scan-based staging. **No edit** |
| Root `install.sh`, `uninstall.sh` | **Host-agnostic — they name ZERO adapters** (verified by reading both). `install.sh:969–1004` calls the reconciler by path; `uninstall.sh:8–10` and `:110` only *prose* enumerate the five host names |
| `.github/workflows/build-test.yml` | Only reaches adapters via `make release-gate`. **No edit** |

> **Root installer: no functional edit is required.** `install.sh` resolves
> `agent_installation/scripts/reconcile_managed_blocks.py` and runs it with
> `--home "$HOME"`; it never names an adapter. Removing adapter directories
> changes its behaviour automatically — which is exactly why the removal is
> safe and needs no installer change.

---

## 5. REVIEW WITH A — three things that must not be deleted blindly

### 5.1 `agent_installation/mpm-claude-code/src/telemetry_adapter.py`

`docs/SPEC.md:1665` documents the client-side telemetry push library as
living at **`agent_installation/mpm-claude-code/src/telemetry_adapter.py`**.
It is a self-contained async NDJSON emitter; its only other referents are its
own test file and that spec line.

**If the directory is removed, the spec points at a non-existent file.**
Options: (a) relocate the library to a host-independent location (e.g.
`agent_installation/telemetry_adapter.py`) and update the spec; (b) delete it
and rewrite the spec paragraph to describe the socket protocol without naming
a repository path; (c) keep the directory. **Recommendation: (a)** — it is
host-independent code that happens to live in a host directory, and the spec
treats it as a substrate capability.

### 5.2 `internal/scheduler/ingest.go:241` — `"mpm-memory-openclaw-ingest"`

A `created_by` provenance string written into `scheduled_wakes`. It is a
**string literal, not a path**, and nothing imports the OpenClaw plugin to
produce it. It will keep working after the directory is removed, but it will
name a host that no longer exists.

**Recommendation:** leave the literal alone in the removal PR (changing it
alters a `created_by` value that may be queried) and record the rename as a
separate, deliberate change — e.g. `mpm-compaction-ingest`. Flagging it here
so it is a decision, not an accident.

### 5.3 `agent_installation/mpm-opencode/install.sh:126` and `mpm-pi/install.sh:192`

Both carry a **comment** that names the sibling adapter for context. Comments
only; removing the sibling leaves a dangling prose reference. One-line comment
fixes, in the removal PR.

### 5.4 `agent_installation/tests/test_refresh_installed_openclaw_safety.py` (29 tests)

This is a **safety** suite: it proves the refresh path does not damage an
installed OpenClaw integration. For an operator who already runs the OpenClaw
adapter, that protection is still valuable — but the adapter is going away.

**Recommendation:** do **not** delete it silently. Either (a) keep the file and
retarget it to assert that a refresh with the OpenClaw adapter *absent* is a
clean no-op, or (b) delete it and record in the release notes that the
OpenClaw refresh-safety guarantee is withdrawn. **This is A's call** — it is a
guarantee change, not a refactor.

---

## 6. REWORK — the test surface and the floors

### 6.1 Which umbrella files are legacy-only vs. shared

| Umbrella file | Tests | Legacy refs | OpenCode refs | Action |
|---|---|---|---|---|
| `test_refresh_installed_openclaw_safety.py` | 29 | 83 | 0 | §5.4 — A's decision |
| `test_cross_adapter_contract_parity.py` | 67 | 35 | 10 | Retire the file; its *intent* (every adapter honours the same managed-block contract) survives as a **single-adapter** assertion in `test_managed_block_provenance.py` |
| `test_runtime_package_provisioning.py` | 52 | 59 | 18 | Rework — drop `OPENCLAW_PLUGINS` (44–68) and `NO_RUNTIME_PACKAGE` (851) |
| `test_managed_block_convergence.py` | 33 | 31 | 2 | Rework — the `ADAPTERS` path (28) points at `mpm-memory-openclaw` |
| `test_reference_discovery_contract.py` | 25 | 24 | 9 | Rework — snippet/target maps |
| `test_managed_block_provenance.py` | 21 | 21 | 2 | Rework — the `ADAPTERS` tuple (81) |
| `test_check_installed_managed_blocks.py` | 12 | 43 | 16 | Rework — the fixture `HOST_INSTALL_TARGETS` |
| `test_installed_block_drift.py` | 5 | 16 | 6 | Rework — `SECTION_BUILDERS` (157) + `HOST_INSTALL_TARGETS` (224–265) |
| `test_refresh_installed.py` | 13 | 15 | 6 | Rework — expected installer paths (93) |
| `test_install_failure_propagation.py` | 8 | 56 | 13 | Retire — 4 of its 5 installer paths are legacy |
| `test_clean_install_roundtrip.py` | 6 | 19 | 7 | Rework — keep the OpenCode round-trip |
| `test_install_update_path.py` | 5 | 19 | 7 | Rework — the host→adapter map (207) |
| `test_adapter_provenance_contract.py` | 7 | 34 | 10 | Rework — `CANONICAL_FRAMEWORKS` (63) + `ADAPTERS` (66–157) |
| `test_naming_invariants.py` | 9 | 40 | 19 | Rework — `CANONICAL_ADAPTERS` (48) becomes `{"mpm-opencode"}`; `MIGRATION_AWARE_FILES` loses the retired paths |
| `test_live_behavioural_verification.py` | 5 | 7 | 2 | Rework — the snippet paths (188–191) |
| `test_check_installed_diagnostic.py` | 5 | 16 | 0 | Rework — fixtures at 145/184 |
| `test_stage2e4_behavioral_contract.py` | 12 | 4 | 3 | Keep — largely host-independent |
| `test_render_managed_blocks.py` | 67 | 17 | 3 | Keep — the renderer is shared; drop the retired entries' expectations |
| `test_mpm_integration_check_hardening.py` | 37 | 7 | 0 | Keep — mostly host-independent |
| `test_readme_managed_block_parity.py` | 13 | 0 | 5 | Keep — unchanged |
| `test_build_install_ownership.py` | 20 | 0 | 0 | Keep — unchanged |
| `test_install_make_dependency_graph.py` | 13 | 0 | 0 | Keep — unchanged |
| `test_render_invocation_context.py` | 9 | 1 | 0 | Keep |
| `test_reconcile_provisioning.py` | 8 | 0 | 1 | Keep |
| `test_reconcile_error_classification.py` | 3 | 0 | 0 | Keep |
| `test_operator_only_action_advertisement.py` | 3 | 0 | 0 | Keep |
| `test_canonical_root_co_location.py` | 6 | 0 | 0 | Keep |
| `test_agent_installation_machine_specific_paths.py` | 1 | 1 | 0 | Rework — the `WHITELIST` entry (88) |

### 6.2 The floors — measured values, not guesses

| Floor | Line | Now | After removal | Action |
|---|---|---|---|---|
| `AGENT_ADAPTER_TEST_MIN_PY_TESTS` | `Makefile:192` | 150 | **119** (mpm-opencode alone: 100 + 19 new) | must drop to ≤ 119 |
| `AGENT_ADAPTER_TEST_MIN_JS_TESTS` | `Makefile:193` | 100 | **0** — `AGENT_ADAPTERS_JS` becomes empty | must drop to 0 **or** the JS branch must be removed |
| `AGENT_ADAPTER_TEST_MIN_TS_TESTS` | `Makefile:194` | 15 | 89 | unchanged |
| `AGENT_INSTALL_TEST_MIN_TESTS` | `Makefile:191` | 300 | **~380–400** (494 minus the legacy-only files and the legacy cases inside the shared ones) | re-baseline against the measured post-removal count |
| `SCRIPTS_TEST_MIN_TESTS` | `Makefile:165` | 50 | ~540 | unchanged |
| `RELEASE_TEST_MIN_TESTS` | `Makefile:174` | 20 | 26 | unchanged |
| `BENCH_TEST_MIN_TESTS` | `Makefile:183` | 50 | 75 | unchanged |

**The floors must be re-baselined from the measured post-removal count, and
that count must be published in the Makefile comment next to the floor** —
the current comments (Makefile 189–190) say "318" and "178 Python + 137
passing JS", which are stale today (494 / 293 / 164).

**Recommendation on how to re-baseline without weakening the gate:** keep the
floors as a *ratchet* — set them to the measured post-removal count minus a
small margin, and never lower them again. The OpenCode-specific coverage is
protected by the *existence* of the OpenCode suites, not by the aggregate
count. Do not lower a floor to make a failing suite disappear.

**`scripts/tests/test_makefile_agent_adapter_wiring.py` hard-codes counts**
that break on removal:
- `:157–161` `assertGreaterEqual(len(names), 3)` on `AGENT_ADAPTERS` → **must
  become 1**
- `:175–179` `assertGreaterEqual(len(names), 1)` on `AGENT_ADAPTERS_JS` → **must
  become 0**, and the JS branch of `test-agent-adapters` must handle an empty
  list without reporting "0 passing tests" as a failure
- `:199–205` `assertIn("mpm-opencode", names)` on `AGENT_ADAPTERS_TS` → unchanged

`scripts/tests/test_makefile_typescript_test_wiring.py:332–339` builds a
**synthetic** Makefile with five fake adapter dirs — it is a fixture, not a
reference to the real set, and needs no change.

### 6.3 `scripts/tests/` — two files are legacy-only

- `scripts/tests/test_uninstall_openclaw_failure_semantics.py` (10 tests) —
  executes `agent_installation/uninstall-openclaw.sh`. **Retire with the
  script.**
- `scripts/tests/test_onboard_runtime_assets.py:131` — a `builders` map with
  `{"mpm-memory-openclaw": "install_openclaw_instructions"}`. Rework.

Everything else in `scripts/tests/` is host-independent and **retained**.

---

## 7. False positives — do not chase these

These matched a search token but are **not** adapter references:

| Hit | What it actually is |
|---|---|
| `release_acceptance/*_test.go` `framework=claude-code`, `framework=pi`, `openclaw` | **Provenance `framework_name` string values** written to DB rows. The release-acceptance suite is cross-agent *continuity* coverage over the substrate and must be **kept** |
| `cmd/mpm-mcp/audit_hook.go:62` `openclaw/pi/opencode/claude-code/hermes` | A comment enumerating **framework ids** |
| `cmd/mpm/onboard_hosts_test.go` `pi`, `opencode`, `claude_code`, `openclaw` | **Host label** arguments to `mpm onboard --hosts` |
| `cmd/mpm/onboard_cmds.go:1121` | The host-label registry (R7) |
| `scripts/tests/test_uninstall.py:290,294,308` | **Fake host binaries on PATH** — host names, not adapter dirs |
| `scripts/tests/test_install_root_host_agnostic.py` | Same — host-name stubs |
| `scripts/deploy.sh:10,75,136` `openclaw gateway restart` | Invokes the **OpenClaw host CLI**, not the adapter |
| `Makefile:866`, `install.sh:927` `~/.pi/agent/AGENTS.md` | A **host install target path**, not the adapter directory |
| `internal/scheduler/ingest.go:241` | A `created_by` **string literal** (§5.2) |
| `test_naming_invariants.py` `claude-code-mpm`, `hermes-mmp`, … | The **forbidden** list — the pre-2026-09-17 names this repo migrated away from |
| `agent_installation/tests/fixtures/fake-openclaw.sh` | A **test fixture** for the uninstaller |
| `mpm-memory-openclaw/install.sh:302,312`, `uninstall-openclaw.sh:660,665` | Legacy→canonical **migration** logic inside the adapters being removed |

No occurrence of the bare token `mpm-pi` inside `pipeline`/`piping` exists —
`mpm-pi` is unambiguous.

---

## 8. The proposed removal PR — smallest safe change set

**Title:** `feat(release): retire the legacy agent adapters (OpenCode-only)`

### Step 1 — delete the adapter directories
`git rm -r` the five directories plus `agent_installation/uninstall-openclaw.sh`.

### Step 2 — the five registries
R1–R7 in §1. Keep the OpenCode entry in every one.

### Step 3 — the Go drift guards
R6/R7 must move in lockstep with R5, or
`TestOnboardHostAdaptersMatchPython` and
`TestOnboardHostInventoryMatchesPython` fail — which is the intended
behaviour, not an obstacle.

### Step 4 — the floors and the hard-coded counts
§6.2. **Measure the post-removal counts first, then set the floors.** Publish
the measured numbers in the Makefile comment.

### Step 5 — the shared docs' supported-host claims
`agent_installation/README.md` § Supported hosts (the table is already
OpenCode-only after Session C's PR #125 — the removal PR drops the four
retired rows and the "not supported in this release" column),
`agent_installation/INSTALL.md` (drop the four retired host sections, the
"Installing only the behavioral protocol" list, the troubleshooting rows and
the "See also" list), `docs/INSTALL.md` § 3 (drop § 3b's retired subsections),
root `README.md` (already OpenCode-only after PR #125),
`uninstall.sh:8–10,110` (the prose enumerating five hosts),
`docs/ONBOARD.md:333`, `docs/SPEC.md:1229,1665`,
`mpm_integration_check.md:605,610,816,819,836,990,1000,1054`.

### Step 6 — the carve-outs
§5.1 (telemetry adapter relocation), §5.2 (`created_by` string decision),
§5.3 (two comments), §5.4 (the refresh-safety guarantee).

### Step 7 — verify in a disposable environment
Never against the live `~/.mpm`.

```bash
# 1. Clean install on an empty HOME: only OpenCode must be configured.
HOME=$(mktemp -d) python3 agent_installation/scripts/reconcile_managed_blocks.py --home "$HOME"
#    expect: opencode written; NO .claude/, NO .pi/,
#            NO .openclaw/, NO .hermes/ under $HOME

# 2. Upgrade on a HOME that already has the legacy adapters installed:
#    the legacy files must be UNTOUCHED (not deleted) — the removal must
#    not destructively remove a user's existing integrations.

# 3. make test-agent-adapters / make test-agent-installation
# 4. make release-gate
```

**Requirement 2 is the important one.** Removing an adapter directory stops
the *installer* from writing those hosts' files. It must **not** cause any
code to delete a user's existing `~/.claude/CLAUDE.md`,
`~/.pi/agent/AGENTS.md` or OpenClaw plugin registrations. Nothing in the
current removal path does — the reconciler only writes and reconciles what it
finds — but the requirement should be asserted by a test, not assumed.

---

## 9. Release-test recommendations

| Test | Recommendation |
|---|---|
| `make test-agent-adapters` (TS) | **Keep the floor at 15.** 89 TS tests, all OpenCode. The floor is now an *OpenCode* floor, not an aggregate one |
| `make test-agent-adapters` (PY) | Re-baseline to the measured mpm-opencode count. **Do not lower it to "whatever passes"** — set it to the measured count minus a small margin and ratchet |
| `make test-agent-adapters` (JS) | The JS branch has **no remaining adapter**. Either drop the branch and the floor, or keep the branch and set the floor to 0 with an explicit "no JS adapters in this release" message. **Never** let it report "0 passing tests" as a silent pass — the current recipe already fails closed on a missing count, and that fail-closed behaviour must be preserved |
| `make test-agent-installation` (umbrella) | Re-baseline from the measured count. The *content* of the gate matters more than the number: the OpenCode install/reconcile/drift/provenance coverage must survive |
| `cd agent_installation && python3 -m unittest discover tests` (CI `gate` job, workflow:194) | **Unchanged** — this is the umbrella suite and it stays |
| `make release-gate` (CI `release-gate` job, workflow:378) | **Unchanged** — it is the only job that reaches `test-agent-adapters` |
| `release_acceptance/` | **Keep every test.** They exercise the substrate's cross-agent *continuity* using provenance strings, not adapter code. Removing them would weaken the release gate for no benefit |
| **NEW** — `test_fresh_install_configures_only_opencode` | Assert that a reconcile against an empty `HOME` writes **only** `~/.config/opencode/AGENTS.md` and creates no `.claude/`, `.pi/`, `.openclaw/` or `.hermes/` path. This is the regression guard for §2 |
| **NEW** — `test_removal_does_not_delete_existing_host_files` | Seed a `HOME` with legacy host files and a legacy plugin registration, run the reconciler, assert every seeded file is **byte-identical** afterwards. This is the regression guard for requirement 5 in §2 |

---

## 10. Conflicts with Session A's ownership

| Area | This PR touches it? | Proposal |
|---|---|---|
| Root `install.sh` | **No functional change.** Only a comment reference to the retired `AUTO_AGENT_INSTALL.md` (fixed in Session C's PR #125) | none |
| Root `uninstall.sh` | Prose only (`:8–10`, `:110` enumerate the five host names) | 2-line prose fix in the removal PR |
| `Makefile` | Floors + three adapter lists | §6.2 — A's call on the numbers |
| Shared CI / release gates | **No change** | none |
| `.github/workflows/build-test.yml` | **No change** | none |
| Go `onboard` registry | R6/R7 | §1 — must move with R5 |
| `agent_installation/scripts/*` | R4/R5 | §1 — must move with R6/R7 |
| `docs/SPEC.md` | §5.1 (telemetry path), §5.2 | A's call on the telemetry adapter's fate |
| Session B's critic work | **Untouched** | none |
| P5 ingestion | **Untouched** | none |

---

## 11. Open questions for A

1. **Telemetry adapter** (§5.1): relocate to a host-independent path, or
   delete and rewrite the spec paragraph?
2. **`created_by` string** (§5.2): leave `mpm-memory-openclaw-ingest` in
   place, or rename it to a host-neutral value as a separate deliberate
   change?
3. **OpenClaw refresh-safety guarantee** (§5.4): withdraw it, or retarget the
   29 tests to the "adapter absent ⇒ clean no-op" property?
4. **JS gate branch**: drop it, or keep it with a floor of 0?
5. **Post-removal umbrella floor**: the exact number must be *measured* after
   the removal, not predicted. The removal PR should set it in the same
   commit that removes the directories, from the observed count.
