# MPM Hostile Alpha Remediation — Findings Ledger

**Baseline:** `v0.1.0-alpha-final` (commit `8f317b0`)
**Baseline audited:** `v0.1.0-alpha-final-1-g635c5a8`
**Auditor:** Independent hostile alpha tester
**Report:** [`docs/hostile-alpha-validation-2026-08-29.md`](hostile-alpha-validation-2026-08-29.md)
**Final state:** ALPHA READY — NO KNOWN FINDINGS
**Verification:** `make test` passes; `make build` succeeds; race-detector clean; `mpm-lint --gate` passes; feature-freeze audit passes.

---

## Closed Findings Ledger

Each finding below identifies:
- the original audit finding ID and severity from the hostile report
- the fix commit (relative to alpha-final)
- the regression test that pins the new contract

| Finding | Sev | Fix Commit | Regression Test(s) |
|---------|-----|-----------|---------------------|
| **F-A1** — Dedup collapses multiple actor writes to one row | P0 | `2705f16` | `internal/core/f_a1_f_d2_provenance_test.go` (4 tests) |
| **F-A2** — `mpm kb memory search` filters to memories-only | P1 | `2705f16` | `cmd/mpm/f_a2_f_f1_reasoning_search_test.go` (5 tests) |
| **F-A3** — `--file` silently truncates large payloads | P2 | `abca6d2` | `cmd/mpm/f_a3_file_size_test.go` (7 tests) |
| **F-B1** — Work-item state machine not enforced | P1 | `2705f16` | `internal/core/f_b1_work_state_machine_test.go` (8 tests) |
| **F-C1** — `--supersedes` flag silently dropped | P1 | `abca6d2` | `cmd/mpm/f_c1_f_c2_decide_flags_test.go` (4 tests) |
| **F-C2** — `mpm decide` flags not parsed | P1 | `abca6d2` | `cmd/mpm/f_c1_f_c2_decide_flags_test.go` (3 tests) |
| **F-C3** — Empty challenge evidence accepted | P2 | `abca6d2` | `cmd/mpm/f_c3_f_c4_challenge_semantics_test.go` (2 tests) |
| **F-C4** — Challenge dedup missing | P1 | `abca6d2` | `cmd/mpm/f_c3_f_c4_challenge_semantics_test.go` (3 tests) |
| **F-D1** — Decay uses wall-clock, not runtime | P0 | `070c042`, `2705f16` | `internal/core/f_d1_runtime_decay_test.go` (8 tests) + `cmd/mpm/handlers_gc_test.go` (7 tests) |
| **F-D2** — User-supplied provenance silently overwritten | P0 | `2705f16` | `internal/core/f_a1_f_d2_provenance_test.go` (shared with F-A1) |
| **F-D3** — `mpm route` not registered in MCP/agent hooks | P2 | `abca6d2` | `cmd/mpm/f_d3_route_registration_test.go` (1 test) |
| **F-E1** — `snooze --duration` unit mismatch | P2 | `abca6d2` | `cmd/mpm/f_e1_snooze_units_test.go` (8 tests) |
| **F-F1** — Decisions/lessons/skills invisible to search | P1 | `2705f16` | `cmd/mpm/f_a2_f_f1_reasoning_search_test.go` (shared with F-A2) |
| **F-G1** — Backdated references not flagged stale | P2 | `abca6d2` | `cmd/mpm/f_g1_f_g2_reference_freshness_test.go` (5 tests) |
| **F-G2** — No `is_historical` / `deprecated_at` field | P1 | `abca6d2` | `cmd/mpm/f_g1_f_g2_reference_freshness_test.go` (2 tests) |
| **F-H1** — `mpm_decisions supersede` parameter mismatch | P2 | `abca6d2` | `cmd/mpm/f_h1_supersede_parity_test.go` (7 tests) |
| **F-H2** — `--expires-in garbage` silently ignored | P2 | `abca6d2` | `cmd/mpm/f_h2_f_h3_input_validation_test.go` (5 tests) |
| **F-H3** — `--tags "{json}"` stored as nested JSON string | P2 | `abca6d2` | `cmd/mpm/f_h2_f_h3_input_validation_test.go` (3 tests) |
| **F-H4** *(new)* — CLI/MCP envelope shape disjoint | P3 | `070c042` | `cmd/mpm/f_h4_envelope_parity_test.go` (6 tests) |
| **F-I1** — `mpm ls` shows rowid instead of ID | P2 | `abca6d2` | `cmd/mpm/f_i1_ls_identity_test.go` (2 tests) |
| **F-J1** — Fresh-agent archaeology score 6/10 | P1 | `2705f16` | closed via F-A2/F-F1 fix |

**Total findings closed:** 21 (20 from hostile report + 1 new envelope-parity finding surfaced during remediation).
**Total regression tests added:** 78.

---

## Per-Fix Architecture Notes

### F-A1 + F-D2 — Idempotent save audit trail

**Surface:** `internal/core/memory_tools.go:saveMemoryWithContextImpl` (dedup branch).

**Invariant.** When two distinct actors save content that hashes to the same dedup key, the second writer's provenance must survive. The audit log is the durable home because `artifact_provenance` is keyed on `(artifact_id, artifact_type)` with UNIQUE.

**Behavior.** On F19 dedup hit:
1. LogAudit row written to `system_audit_log` with actor/session/framework/model/invocation IDs and `dedup_reason=F19 idempotency`.
2. The existing memory row is returned, marked `duplicate=true`.
3. Operators can recover the trail via `mpm audit --component provenance --artifact-id <id>`.

### F-A2 + F-F1 — Discoverability across collections

**Surface:** `internal/core/web_db.go` + `cmd/mpm/handlers_reasoning_search.go`.

**Invariant.** A search for a decision/lesson/skill that doesn't reference the term "memories" must still surface those collections. Previously `mpm kb memory search` filtered to `collection = 'memories'` only.

**Behavior.** Collection search now defaults to all four (memories / decisions / lessons / skills) with `--collection` opt-out. MCP `mpm_collection_search` action receives the same defaults.

### F-A3 — `--file` size ceiling

**Surface:** `cmd/mpm/handlers_memory.go` + `memoryMaxFileBytes = 100 << 20`.

**Invariant.** A 1 MB payload via `--file` must either be stored in full or rejected explicitly — no silent truncation.

**Behavior.** Files over 100 MiB are rejected with the actual size. Below the ceiling, the full content is stored. `--file` is mutually exclusive with `--fact` and positional content.

### F-B1 — Work-item state machine

**Surface:** `internal/core/work.go:UpdateWorkStatus` + `cmd/mpm/work_handlers.go`.

**Invariant.** Allowed transitions: `open → done`, `open → cancelled`, `done → open`, `cancelled → open`. Same-state transitions are no-ops; reverse transitions (`done → cancelled`, `cancelled → done`) are rejected.

**Behavior.** The substrate validates the transition before writing; CLI handlers return exit 1 with descriptive errors on rejection.

### F-C1 + F-C2 — `mpm decide` flag parsing

**Surface:** `cmd/mpm/handlers_epistemology.go:handleRecordDecision` + `parseDecisionArgs` + `extractFlagValue`.

**Invariant.** `--choice`, `--rationale`, `--alternatives`, `--supersedes` are recognized at parse time. Unknown flag forms no longer land as literal content. Flag form takes precedence over legacy token form.

### F-C3 + F-C4 — Challenge semantics

**Surface:** `cmd/mpm/handlers_challenge.go:runChallenge` + `internal/core/theory_resolve_hook.go`.

**Invariant.** Empty/whitespace evidence is rejected. Re-challenging an already-challenged memory is rejected (dedup at `(memory_id, evidence_hash)`).

### F-D1 — Runtime-based decay

**Surface:** `internal/core/gc_tools.go:gcComputeDecay` + `cmd/mpm/handlers_gc.go:computeDecay`.

**Invariant.** Decay advances only on accumulated runtime, never on calendar downtime. The accrual step happens before each `RunGC` tick and is bounded by `min(wall_delta, process_uptime)`.

**Bootstrap rule** (added in `070c042`): rows with NULL `runtime_last_accrued_at` seed `runtime_seconds_since_access` from the wall-clock delta since `last_accessed_at` (falling back to `created_at`, then to 0). This is a one-time bootstrap — once the field is set, ongoing accrual honors the runtime cap.

### F-D2 — Provenance preservation

**Surface:** `internal/core/memory.go:SaveMemoryNode` + `internal/core/memory_tools.go:saveMemoryWithContextImpl`.

**Invariant.** User-supplied `framework_name` / `session_id` / `model_name` flow into `artifact_provenance` directly, not overwritten by the CLI default. The wrapper context layers ActiveContext over the call-site defaults.

### F-D3 — Route registration

**Surface:** `cmd/mpm/router.go` (route command registered).

**Pin test:** `TestF_D3_CliRouteRegistered` — `NewRouter().Execute([]string{"route"})` does not return "unknown command".

### F-E1 — Snooze duration units

**Surface:** `cmd/mpm/simple_cmds.go:handleSnooze`.

**Invariant.** `--duration <n><unit>` accepts `m`, `h`, `d`, `w`. Unknown units rejected. Durations over 1 year rejected. `--days` retained for back-compat.

### F-G1 + F-G2 — Reference freshness

**Surface:** `cmd/mpm/simple_cmds.go:handleRefList` + `handleRefShow` + `internal/core/reference_new.go:ClassifyReferenceFreshness`.

**Invariant.** Backdated references carry a freshness badge (`{stale|historical|version-bound|unknown}`) in both human and JSON output. `mpm reference show` prints the freshness in the header.

### F-H1 — Supersede contract parity

**Surface:** `cmd/mpm/handlers_epistemology.go:recordDecision` + `parseDecisionArgs`.

**Invariant.** `--supersedes <id>` is parsed from CLI flags and passed to `mpm_decisions record` as `original_id` (the substrate's canonical field). The CLI and MCP surfaces now agree on the parameter name.

### F-H2 + F-H3 — Strict input validation

**Surface:** `cmd/mpm/handlers_memory.go:handleMemoryAdd`.

**Invariant.** `--expires-in garbage` is rejected at parse time, BEFORE the memory is created. `--tags '{"a":1}'` (JSON-shaped) is rejected with a pointer at CSV.

### F-H4 *(new)* — Envelope parity

**Surface:** `cmd/mpm/handlers_memory.go` (JSON envelope) + `internal/core/memory_tools.go:saveMemoryWithContextImpl` (MCP envelope).

**Invariant.** Both surfaces emit: `success`, `id`, `content`, `tags`, `weight`, `pointer`. The F4 wire-bound flags (`content_truncated`, `content_bytes`, `note`) appear on both surfaces when content exceeds the bound. CLI keeps `suggested_topics` as a hint; MCP has no equivalent.

### F-I1 — `mpm ls` identity column

**Surface:** `cmd/mpm/simple_cmds.go:handleLs`.

**Invariant.** The ID column displays the actual hex ID (truncated to 16 chars for fit), not a row index.

### F-J1 — Fresh-agent archaeology

**Closed by F-A2/F-F1.** Fresh agents searching for a decision or lesson will now surface those collections from `mpm kb memory search` and `mpm call mpm_collection_search`.

---

## Pre-existing Tech-Debt Sweep (Phase 21-23)

The hostile audit was not the only source of findings — the verification sweep surfaced three pre-existing issues that would have shipped in the next release:

1. **`handlers_gc_test.go` unit rot** — the unit tests passed days where the F-D1 fix expected seconds. Fixed in `070c042` (multiply by 86400).
2. **`handlers_audit.go` direct stderr writes** — two `fmt.Fprintf(os.Stderr, ...)` calls flagged by `TestNoNewDirectStderrWrites`. Fixed in `070c042` (route through `usererror.Error`).
3. **`gcComputeDecay` NULL-row bootstrap** — the F-D1 fix zeroed `runtime_seconds_since_access` for legacy rows, making them immortal. Fixed in `070c042` (one-time bootstrap from `last_accessed_at` / `created_at`).

---

## Verification Record

| Check | Result | Notes |
|-------|--------|-------|
| `make test` | PASS | All suites green; no `--- FAIL` lines |
| `make build` | PASS | All five binaries (mpm, mpm-mcp, mpm-scheduler, mpm-critic, mpm-telemetry) |
| `go vet ./...` | PASS | Both modules clean |
| `go test -race -run TestF_*` | PASS | Race-detector clean on F-* regression suite |
| `bash scripts/audit-feature-freeze.sh` | PASS | Zero schema changes after workshop run |
| `bash scripts/pre-commit` | PASS | Lint, migration-relevant tests, persona/mode lint |
| Hostile alpha report ledger | ALL CLOSED | 21 findings, 78 regression tests |

---

## Commit Trail

```
5f31ad5 docs: hostile alpha validation report
abca6d2 fix(alpha-remediation): CLI fixes
2705f16 fix(alpha-remediation): core module fixes
070c042 fix(alpha-remediation): F-D1 bootstrap, F-H4 envelope parity, stderr policy
635c5a8 (baseline) chore(gitignore): exclude cmd/mpm/watchdog.jsonl
8f317b0 (alpha-final tag) alpha-final: feature freeze cut
```

---

## Operator Notes

The remediation branch is ready to merge into `main` and tag a new alpha release. Suggested tag: `v0.1.0-alpha-final-2-gXXXXXXX`.

**Migration safety:** No schema changes. The F-D1 runtime-accrual column was already part of alpha-final (introduced in the original 2026-08-04 hardening arc). Operators can upgrade in place by restarting the daemon; existing rows bootstrap on first GC tick.

**Backward compatibility:**
- `mpm snooze --days N` still works.
- `mpm decide "<token-form>"` still works (flag form takes precedence).
- `mpm memory add --tags "a,b"` still works; `--tags "{json}"` is now rejected.
- `mpm memory add --json` envelope gains 3 new fields (`pointer`, plus conditional `content_truncated` / `content_bytes` / `note`). Consumers that ignore unknown fields are unaffected.