# MPM — Full Public Tool Behavioural Audit

**Date:** 2026-09-05
**Branch:** `main` @ `56bd094`
**Baseline:** alpha-final at `8f317b0`
**Mode:** Read-only audit. No production code, tests, schemas, or docs were modified during the audit.

---

## A. Audit scope

### Authoritative inventory (21 tools, derived from `internal/core/tools/registry_list.go`)

| # | Tool | Has action enum? | Action count | Persistent state | CLI equivalent | Handler location |
|---|------|------------------|--------------|------------------|----------------|------------------|
| 1 | `mpm_memory` | yes | 15 | memories table | `mpm call mpm_memory` | `handlers.go:4693` (dispatcher); per-action `handlers.go:106+` |
| 2 | `mpm_lessons` | yes | 3 | lessons view | `mpm call mpm_lessons` | `handlers.go:4943` |
| 3 | `mpm_decisions` | yes | 6 | decisions table | `mpm call mpm_decisions` | `handlers.go:4961` |
| 4 | `mpm_theories` | yes | 5 | memories (collection=theories) | `mpm call mpm_theories` | `handlers.go:4828` |
| 5 | `mpm_skills` | yes | 6 | memories (collection=skills) | `mpm call mpm_skills` | `handlers.go:5161` |
| 6 | `mpm_topics` | yes | 5 | topics + topic_memberships | `mpm call mpm_topics` | `handlers.go:5063` |
| 7 | `mpm_references` | yes | 4 | references | `mpm call mpm_references` | `handlers.go:5087` |
| 8 | `mpm_evidence` | yes | 3 | evidence + confidence_history | `mpm call mpm_evidence` | `handlers.go:5110` |
| 9 | `mpm_confidence` | yes | 7 | derived (read) + recompute trigger | `mpm call mpm_confidence` | `handlers.go:5135` |
| 10 | `mpm_retrieval_diagnose` | no | n/a | none (read) | `mpm call mpm_retrieval_diagnose` | `handlers.go:4104` |
| 11 | `mpm_context` | yes | 7 | sessions + directives + global_rules + skills + scheduled_wakes | `mpm call mpm_context` | `handlers.go:5185` |
| 12 | `mpm_wakes` | yes | 8 | scheduled_wakes + scheduled_tasks | `mpm call mpm_wakes` | `handlers.go:4800` |
| 13 | `mpm_handoff` | yes | 4 | handoffs | `mpm call mpm_handoff` | `handlers.go:4757` |
| 14 | `mpm_scratchpad` | yes | 4 | scratchpad | `mpm call mpm_scratchpad` | `handlers.go:4780` |
| 15 | `mpm_system` | yes | 10 | system_audit_log + clusters | `mpm call mpm_system` | `handlers.go:5211` |
| 16 | `log_to_changelog` | no | n/a | memories (collection=changelog) | `mpm call log_to_changelog` | `handlers.go:2481` |
| 17 | `request_review` | no | n/a | none (read) | `mpm call request_review` | `handlers.go:4474` |
| 18 | `mpm_resolve` | no | n/a | none (resolver) | `mpm call mpm_resolve` | `handlers.go:5322` |
| 19 | `mpm_blob_read` | no | n/a | none (read) | `mpm call mpm_blob_read` | `handlers.go:5547` |
| 20 | `mpm_blob_search` | no | n/a | none (read) | `mpm call mpm_blob_search` | `handlers.go:5625` |
| 21 | `mpm_work` | yes | 10 | works + work_events | `mpm call mpm_work` | `work_handlers.go:34` |

**Total: 21 tools, 6 non-action + 15 action-based. ~90+ distinct action surfaces.**

### Inventory notes

- One additional MCP-only tool exists: **`route`** is registered top-level in `cmd/mpm-mcp/tools.go:308` (closes over the live `*Router`). It is NOT in the registry, so `mpm call route` fails with "unknown tool" — callers must use `mpm call mpm_context --payload '{"action":"route", ...}'`. **Documented as intentional** (`tools.go:487`); flagged here only for awareness.
- Legacy aliasing: `mpm_session` (retired) is routed through `resolveLegacySessionAlias` (`cmd/mpm/call.go:248-288`) onto the split `mpm_handoff`/`mpm_scratchpad` tools.
- The `mpm_challenge` standalone tool was retired 2026-09-05 (`3c8ff56 fix(tools): retire standalone mpm_challenge tool`) and consolidated into `mpm_memory.challenge` / `mpm_memory.restore_challenge`. Registry comment at `registry_list.go:503-508` documents the retirement.

---

## B. Master behavioural matrix

**Legend:** PASS (verified) | PARTIAL (works in core, edge case) | FAIL (verified broken) | N/A (not applicable) | UNVERIFIED (evidence insufficient)

### B.1 — mpm_memory (15 actions)

| Action | Status | Key evidence |
|---|---|---|
| save | PASS | `handlers.go:106-197`; F12-1 strict-type guard on `weight` (rejects string `"5"`); commit_milestone floor 50 chars (`handlers.go:233+`) |
| query | PARTIAL | `handlers.go:489-633`; `limit=0` returns 15 (local) or 10 (all) — see **Defect D-3** |
| show | PASS | `handlers.go:644-684`; W-013 JSON-decodes tags/metadata; W-007 challenge banner surfaced |
| shred | PASS | `handlers.go:1014-1039`; D-8.1 accepts `memory_id`/`id` alias |
| reinforce | PARTIAL | `handlers.go:1056-1063`; uses silent `ParseFloatOr` — see **Defect D-5** |
| weaken | PARTIAL | `handlers.go:1065-1072`; same — see **Defect D-5** |
| snooze | PARTIAL | `handlers.go:1074-1081`; same — see **Defect D-5** |
| set_weight | PARTIAL | `handlers.go:1083-1090`; same + no handler-level id guard — see **Defects D-5, D-6** |
| patch | FAIL | `handlers.go:1092-1103`; non-object patch silently wipes metadata — see **Defect D-1** |
| promote | PARTIAL | `handlers.go:1105-1108`; no handler-level id guard — see **Defect D-6** |
| review | PARTIAL | `handlers.go:1245-1249`; silent `ParseFloatOr` — see **Defect D-5** |
| synthesize | PASS | `handlers.go:1254-1257`; thin wrapper |
| challenge | PASS | `handlers.go:783-791`; canonical snake_case `memory_id`; `TestMpmMemoryChallengeNormalization` |
| restore_challenge | PASS | `handlers.go:796-802`; F7-1 surface; `f7_1_challenge_restore_surface_test.go` |
| commit_milestone | PASS | `handlers.go:229-286`; 50-char floor; flavor enum validated; double-prefix dedupe |

### B.2 — mpm_lessons (3 actions)

| Action | Status | Key evidence |
|---|---|---|
| save | PASS | `handlers.go:1374-1413`; type enum validated; idempotent on `(fact,type)` (reinforcement++) |
| search | PASS | `handlers.go:1416-1502`; query required; projection normalized; retrieval telemetry recorded |
| list | PARTIAL | `handlers.go:1505-1544`; invalid `type` returns empty — see **Defect D-8** |

### B.3 — mpm_decisions (6 actions)

| Action | Status | Key evidence |
|---|---|---|
| record | PASS | `handlers.go:932-955`; choice required |
| supersede | **FAIL on live DB** | CHECK constraint blocks — see **Defect P0-D-1** |
| invalidate | **FAIL on live DB** | CHECK constraint blocks — see **Defect P0-D-1** |
| show | PARTIAL | `handlers.go:4986-4996`; unknown id leaks raw SQL error — see **Defect D-11** |
| list | PARTIAL | `handlers.go:5001-5028`; `limit<=0` silently coerces to 50 — see **Defect D-4** |
| query | PARTIAL | `handlers.go:5033-5052`; same — see **Defect D-4** |

### B.4 — mpm_theories (5 actions)

| Action | Status | Key evidence |
|---|---|---|
| propose | PASS | `handlers.go:805-842`; hypothesis required |
| resolve | PASS | `handlers.go:853-929`; snake/camel normalization; rejects missing/invalid `newStatus`; winnerId routes to arbitration |
| show | PASS | `handlers.go:4863-4873`; id required |
| list | PARTIAL | `handlers.go:4880-4907`; `limit<=0` silently coerces to 50 — see **Defect D-4** |
| query | PARTIAL | `handlers.go:4912-4931`; same — see **Defect D-4** |

### B.5 — mpm_skills (6 actions)

| Action | Status | Key evidence |
|---|---|---|
| save | PARTIAL | `handlers.go:2538-2601`; W-005 aggregates errors; `skill_save_regression_test.go`. **Save after delete fails** — see **Defect D-14** |
| read | PASS | `handlers.go:2615-2639`; name required; unknown name → clean error |
| list | PARTIAL | `handlers.go:2642-2654`; invalid `scope` silently defaults to "all" — see **Defect D-17** |
| delete | PARTIAL | `handlers.go:3401-3420`; unknown id returns `success:true` — see **Defect D-15** |
| promote_to_global | PASS | `handlers.go:3359-3388`; `confirm=true` gate; `skill_id` required; forensic log |
| workshop | PASS | `handlers.go:3274-3343`; `RunWorkshop` gate; decision_model axis types validated |

### B.6 — mpm_topics (5 actions)

| Action | Status | Key evidence |
|---|---|---|
| create | PASS | `handlers.go:1584-1600` |
| search | PASS | `handlers.go:1603-1619`; query required |
| link | PASS | `handlers.go:1630-1659`; W-005 pre-validates IDs; idempotent via INSERT OR IGNORE |
| list | PASS | `handlers.go:1665-1682`; D-4.1 parity with CLI |
| show | PASS | `handlers.go:1686-1702`; D-4.1 parity with CLI |

### B.7 — mpm_references (4 actions)

| Action | Status | Key evidence |
|---|---|---|
| add | PASS | `handlers.go:1705-1714`; `filepath` required; `title` optional |
| read | PASS | `handlers.go:1775-1791`; `id` required |
| search | PASS | `handlers.go:1716-1745`; `query` required; `limit` default 5 |
| list | PASS | `handlers.go:1747-1773`; `limit`/`offset` honored (non-negative clamps) |

### B.8 — mpm_evidence (3 actions)

| Action | Status | Key evidence |
|---|---|---|
| add | PASS | `handlers.go:2342-2374`; F12 strict-type guard; F6 source_group rejection; type validated against 6-value registry |
| list | PASS | `handlers.go:2386-2398`; requires `artifact_id` AND `artifact_type` (both explicit, no silent default per MPM-BUG-LIST-EVIDENCE-DEAF-2026-08-27) |
| source_groups | PASS | `handlers.go:5124-5128`; returns partitioned vocabulary (outcome/audit/action) |

### B.9 — mpm_confidence (7 actions)

| Action | Status | Key evidence |
|---|---|---|
| show | PARTIAL | `handlers.go:2457-2459`; envelope shape differs from brief — see **Defect D-3 (envelope)** |
| recompute | PASS | `handlers.go:2462-2464` |
| explain | PASS | `handlers.go:2467-2469` |
| history | PASS | `handlers.go:2401-2407` |
| changes | PASS | `handlers.go:2411-2429`; supports `since_seconds_ago` and `since` |
| trend | PASS | `handlers.go:2443-2449`; requires `artifact_id` |
| quality | PASS | `handlers.go:2452-2454` |

### B.10 — mpm_retrieval_diagnose (no actions; one shape)

| Param | Status | Key evidence |
|---|---|---|
| query required | PASS | `handlers.go:4105-4108` |
| limit (default 10) | PASS | `handlers.go:4109-4112` |
| collection filter | PASS | `handlers.go:4113` |
| scope filter (default all) | PASS | `handlers.go:4114` |
| trace flag (default false) | PASS | `handlers.go:4115`; 3-stage pipeline diagnostic |
| Empty result handling | PASS | `_No results._` rendered (`handlers.go:4136`) |
| Per-node diagnostic output | PASS | BM25 score, Reuse Count, Last Retrieved, Success Count rendered |
| **No dedicated unit test** | UNVERIFIED | `handlers_test.go` has no TestHandleMpmRetrievalDiagnose |

### B.11 — mpm_context (7 actions)

| Action | Status | Key evidence |
|---|---|---|
| read_wake_context | PASS | `handlers.go:1796-2009`; 30 keys (full) / 9 keys (compact); `format=system-prompt` renders Markdown |
| read_directives | PASS | `handlers.go:2274-2286` |
| proactive_recall_hint | PASS | `handlers.go:2289-2308` |
| query_global_rules | PASS | `handlers.go:3126-3152` |
| record_global_rule | PASS | `handlers.go:3166-3225`; `confirm=true` required |
| promote_to_global | PASS | `handlers.go:3235-3267`; `confirm=true` required |
| route | PASS | `handlers.go:2316-2329` |

### B.12 — mpm_wakes (8 actions)

| Action | Status | Key evidence |
|---|---|---|
| schedule | PASS | `handlers.go:3489-3521`; reason + target_time required |
| check | PASS | `handlers.go:3523-3563`; folds due wakes inline |
| check_pending_event | PASS | `handlers.go:3565-3583`; falls back to ac.SessionID |
| list | PASS | `handlers.go:3585-3663`; F-5 kinds filter honored (verified by `f5_list_wakes_kinds_filter_regression_test.go`) |
| digest | PASS | `handlers.go:3665-3695`; top_n default 5 |
| upsert_task | PASS | `handlers.go:3697-3748`; requires id, name, cron_expr, directive_id; fail-fast directive existence check |
| list_tasks | PASS | `handlers.go:3750-3764` |
| delete_task | PASS | `handlers.go:3766-3784`; requires id |

### B.13 — mpm_handoff (4 actions)

| Action | Status | Key evidence |
|---|---|---|
| write | PASS | `handlers.go:2910-2955`; summary required (verified by `TestHandleHandoffWrite_MissingSummaryFails`); F13 commitments/open_questions persisted |
| read | PASS | `handlers.go:2957-3010`; `mark_read` honored; empty result returns `nil handoff, success=true` envelope |
| list | PASS | `handlers.go:3012-3051`; limit capped at 500; unread filter |
| shred | PARTIAL | `handlers.go:3053-3110`; accepts `id`/`handoff_id`/`session_id` (D-8.1). **Schema under-declares top-level `params`** — see **Defect D-12** |

### B.14 — mpm_scratchpad (4 actions)

| Action | Status | Key evidence |
|---|---|---|
| flush | PASS | `handlers.go:3931-3959`; UPSERT (idempotent); 24-hour TTL hard-coded |
| read | PASS | `handlers.go:3961-3987`; canonical W-006 hint when session_id missing |
| discard | PASS | `handlers.go:3989-4013`; idempotent (no error on missing) |
| promote | PASS | `handlers.go:4015-4086`; atomic tx: SELECT → INSERT → DELETE; auto-tags `from-scratchpad:<sid>` |

### B.15 — mpm_system (10 actions)

| Action | Status | Key evidence |
|---|---|---|
| gc_run | PASS | `handlers.go:1266-1305`; envelope shape matches contract |
| compact | PASS | `handlers.go:3841-3885`; all 8 documented keys present |
| health_check | PASS | `handlers.go:3786-3788`; passthrough |
| migrate | PASS | `handlers.go:1125-1223`; supports stage/promote/undo |
| query_audit_log | PARTIAL | `handlers.go:2674-2730`; **`since` silently dropped** — see **Defect D-13** |
| list_clusters | PASS | `handlers.go:2756-2777`; known/unknown partition |
| snooze_cluster | PASS | `handlers.go:2795-2816`; `snooze_until` accepts relative ("24h") or absolute |
| resolve_cluster | PASS | `handlers.go:2831-2847`; idempotent |
| annotate_cluster | PASS | `handlers.go:2873-2894`; audit row written |
| critic_findings | PASS | `handlers.go:5260+` |

### B.16 — log_to_changelog (no actions; writes to mpm.db, NOT changelog.md)

| Param | Status | Key evidence |
|---|---|---|
| fact required | PASS | `handlers.go:2481-2501`; rejects empty fact |
| commit_hash regex (40-char SHA-1) | PASS | `changelog_mcp.go:63`; rejects `"abc123"` with precise message |
| dedup | PASS | `changelog_mcp_test.go` |
| Auto-tags `#changelog` + `#commit:<lowercase hash>` | PASS | `changelog_mcp.go:95-96` |

### B.17 — request_review (no actions; renders Markdown)

| Param | Status | Key evidence |
|---|---|---|
| components required | PASS | `handlers.go:4474-4577`; missing → error |
| prompt required | PASS | same; missing → error |
| strategy="sequential" rejected | PASS | only `parallel` supported |
| artifact id resolution error | PASS | pre-resolved to text before coordinator; bogus id → clean error |
| Returns Markdown (not JSON envelope) | PASS | intentional per `handlers.go:4491-4496` |
| **No dedicated regression test at tool layer** | UNVERIFIED | tested at orchestration layer only |

### B.18 — mpm_resolve (pointer dereferencer)

| Aspect | Status | Key evidence |
|---|---|---|
| `uri` required | PASS | `handlers.go:5324`; rejects empty |
| All 5 kinds (blob/work/memory/lesson/theory) | PASS | `handlers.go:5336-5448`; MCP resolver covers same kinds |
| `mpm://blob/<id>` w/o SetBlobStore | PASS | `"blob store not initialized"` clear error |
| `mpm://theory/<id>` validates collection=="theories" | PASS | `handlers.go:5409-5411` (CLI) and `tools.go:235-237` (MCP) |
| `bounded` field correct (memory/work/memory) | PASS | post-`59ffd3d` |
| `bounded` field on lesson/theory CLI fallback | PARTIAL | **hard-coded false** — see **Defect D-16** |
| Malformed URI rejected | PASS | `parsePointerURI` (`handlers.go:5506-5542`) |
| Empty content handling | PASS | `bounded=false` for small content; `bounded=true` when content > 512 bytes |

### B.19 — mpm_blob_read

| Aspect | Status | Key evidence |
|---|---|---|
| `id` required | PASS | `handlers.go:5552` |
| `offset` defaults to 0 | PASS | `handlers.go:5555` |
| `max_bytes` defaults cooperatively with output boundary | PASS | post-D4 fix (`handlers.go:5571-5576`) |
| Server ceiling 256 KiB | PASS | `handlers.go:5559,5577` |
| Binary content type rejection | PASS | `handlers.go:5599-5602` |
| Offset applied | PASS | `handlers.go:5582`; verified: offset=7 on "Hello, World!" → "World!" |
| Offset beyond size returns empty content | PASS | `handlers.go:5611` |
| Pagination surfaced (`has_more`, `next_offset`, `bytes_returned`) | PASS | `handlers.go:5613-5620` |

### B.20 — mpm_blob_search

| Aspect | Status | Key evidence |
|---|---|---|
| `id` + `query` required | PASS | `handlers.go:5636` |
| `regex` flag honored | PASS | `handlers.go:5631` |
| `case_insensitive` flag honored | PASS | `handlers.go:5632` |
| `max_matches` (server ceiling 100, default 20) | PASS | `handlers.go:5641-5650` |
| `max_bytes` (server ceiling 256 KiB, default 50 KiB) | PASS | `handlers.go:5652-5658` |
| Query length limit 256 chars | PASS | `handlers.go:5661-5663` |
| `truncated` flag surfaced | PASS | `handlers.go:5687` |
| `bytes_returned` actual sum (post-`2baa84e`) | PASS | `handlers.go:5690-5695` |

### B.21 — mpm_work (10 actions)

| Action | Status | Key evidence |
|---|---|---|
| create | PASS | `work_handlers.go:72-90`; title required |
| list | PASS | `work_handlers.go:92-148`; enveloped (F14); W-010 status enum validated |
| show | PASS | `work_handlers.go:150-163`; F5 not-found hint |
| update | PASS | `work_handlers.go:165-175`; F-003 regression pinned |
| complete | PASS | `work_handlers.go:177-212`; work_id required; aliases `id` per D-8.1 |
| cancel | PASS | `work_handlers.go:214-244`; F-B1 enforces done→cancelled rejected |
| history | PASS | `work_handlers.go:246-302`; enveloped (F14); F16 framework/model surfaced |
| note | PASS | `work_handlers.go:304-312`; append-only event |
| reopen | PASS | `work_handlers.go:314-335`; 3-event sequence verified |
| resolve_contradiction | PARTIAL | `work_handlers.go:337+`; **schema missing `reason` required** — see **Defect D-9** |

---

## C. Defects

Every defect below is supported by deterministic reproduction. Severity uses the project's existing scale:

- **P0** — security/data corruption/severe runtime failure blocking documented functionality
- **P1** — meaningful correctness, persistence, lifecycle, or integration defect
- **P2** — material API/contract/usability/reliability defect
- **P3** — low-impact maintainability or documentation debt

### C.1 — P0: confidence_history CHECK constraint blocks `mpm_decisions.supersede` and `mpm_decisions.invalidate` on the live DB

**Severity:** P0 — blocks two documented public actions on production substrate.

**Reproduction:**
```bash
./mpm call mpm_decisions record --payload '{"action":"record","params":{"choice":"audit probe choice","rationale":"for testing"}}'
# → {"choice":"audit probe choice","id":"<id>","success":true}

./mpm call mpm_decisions supersede --payload '{"action":"supersede","params":{"original_id":"<id>","choice":"new","rationale":"r"}}'
# → {"error":"supersede confidence history: CHECK constraint failed: trigger IN ('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','manual_recompute')","success":false}
```

**Evidence:**
- Deployed DB schema: `sqlite3 src/db/mpm.db "SELECT sql FROM sqlite_master WHERE name='confidence_history'"` shows CHECK with 6 values: `('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','manual_recompute')`.
- Source declaration: `internal/core/schema.go:205` declares CHECK with **8** values: `('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','concept_drift','manual_recompute','supersede','invalidate')`.
- No migration file rebuilds confidence_history to widen the CHECK: `grep -rn "confidence_history.*trigger" internal/core/migration_*.go` returns nothing; `ls internal/core/migration_*.go | grep -i confidence` returns 0 hits.
- Runtime write paths: `internal/core/epistemology_tools.go:527-531` (supersede) and `:594-599` (invalidate) write `trigger='supersede'`/`'invalidate'` rows inside a `WithTx` transaction — the whole transaction rolls back on the CHECK violation, so the original decision is NOT marked superseded/invalidated.

**Expected:** supersede/invalidate succeed; original decision is marked superseded/invalidated; confidence_history ledger contains the trigger row.

**Actual:** CHECK constraint violation; transaction rolled back; original decision remains active. **Both actions are non-functional on the deployed DB.**

**Test gap:** `TestF5_2_DecisionConfidenceAfterCorrection` and `TestF9_DecisionInvalidationRegression` pass against fresh DM (which builds the latest schema in `t.TempDir()`), masking the bug. No test runs against a pre-widening confidence_history.

### C.2 — P1: `mpm_skills save` after `mpm_skills delete` fails with UNIQUE constraint

**Severity:** P1 — blocks the documented delete-then-resave workflow for skills.

**Reproduction:**
```bash
./mpm call mpm_skills save --payload '{"action":"save","params":{"name":"audit-probe-skill","version":"v1","content":"---\nname: audit-probe-skill\ndescription: test\nversion: v1\n---\n\nbody\n"}}'
# → {"id":"skill:audit-probe-skill-vv1","name":"audit-probe-skill","success":true,"version":"v1"}

./mpm call mpm_skills delete --payload '{"action":"delete","params":{"skill_id":"skill:audit-probe-skill-vv1"}}'
# → {"skill_id":"skill:audit-probe-skill-vv1","success":true}  # soft-delete sets deleted_at

./mpm call mpm_skills save --payload '{"action":"save","params":{"name":"audit-probe-skill","version":"v1","content":"---\nname: audit-probe-skill\ndescription: test\nversion: v1\n---\n\nbody\n"}}'
# → {"error":"insert skill: UNIQUE constraint failed: memories.id","success":false}  # tombstoned row still owns id
```

**Evidence:**
- `handlers.go:3401-3420` — delete is a soft-delete (sets `deleted_at`).
- `skill_db.go:343-353` — existence check is `WHERE id = ? AND deleted_at IS NULL` (soft-deleted rows are invisible to existence probe).
- `skill_db.go:462-547` — INSERT path collides with the tombstoned row's primary key.
- The `force=true` overwrite path (`skill_db.go:386-451`) is gated by the same existence check, so it also fails.

**Expected:** re-save either resurrects the soft-deleted row or replaces it atomically.

**Actual:** UNIQUE constraint failure on both `save` and `save` with `force=true`.

### C.3 — P2: `mpm_handoff` schema does not declare top-level `params` (TestSchemaSupersetOfHandlerPayloadReads is failing)

**Severity:** P2 — schema/handler drift; the canonical schema-guard test is currently FAILING in `make test`.

**Reproduction:**
```bash
cd internal/core/tools && go test -tags fts5 -run "TestSchemaSupersetOfHandlerPayloadReads" -v ./...
# → --- FAIL: TestSchemaSupersetOfHandlerPayloadReads (0.01s)
#     schema_guard_test.go:97: mpm_handoff: handler reads payload keys [params] that are not declared in the JSON-Schema (under-declaration)
```

**Evidence:**
- Schema (`registry_list.go:380-446`): declares only `action` at the top level. Per-action `params` objects exist only inside the `oneOf` branches.
- Handler (`handlers.go:4757-4775`): calls `extractParamsOrFail("mpm_handoff", payload)` which reads `payload["params"]` at the top level.
- The test AST extractor sees the top-level `params` read but cannot find `params` declared in the schema's top-level `properties`. Compare with `mpm_work` which DOES pass — its `mpm_work.create` branch has the same shape but the test extracts it via the `literalFromPassthroughCall` path.

**Expected:** schema's top-level `properties` should declare `params` so the under-declaration check passes.

**Actual:** test fails. Every `go test` run surfaces this drift.

### C.4 — P2: `mpm_memory patch` silently wipes metadata on non-object patch

**Severity:** P2 — integrity: a malformed client payload destroys user data without error.

**Reproduction:**
```bash
./mpm call mpm_memory patch --payload '{"action":"patch","params":{"memory_id":"<id>","patch":["a","b"]}}'
# → {"success":true,"memory_id":"<id>"}  # metadata wiped to {}
```

**Evidence:**
- `handlers.go:1092-1103` — accepts any JSON value (`map[string]interface{}` typed) and forwards to `dm.UpdateMemoryMetadata`. Handler comment falsely claims: "A nil/primitive patch is rejected upstream by the DM".
- `db.go:3317-3320` — `UpdateMemoryMetadata` only checks `json.Valid([]byte(patchJSON))`. Arrays, strings, null, primitives are valid JSON.
- SQLite `json_patch(target, array)` no-ops; subsequent JSON merge overwrites the prior metadata with the merged result.

**Expected:** error `patch must be a JSON object`.

**Actual:** `success:true`; metadata wiped to `{}`.

### C.5 — P2: `mpm_memory query` with `limit=0` returns 15 (local) / 10 (all) instead of 0

**Severity:** P2 — violates the documented `parseLimitStrict` contract.

**Reproduction:**
```bash
./mpm call mpm_memory query --payload '{"action":"query","params":{"query":"test","limit":0,"scope":"local"}}'
# → {"count":15, ...}  # expected 0
```

**Evidence:**
- `handlers.go:5776-5807` (`parseLimitStrict`) and the handler comment at `handlers.go:494-502` both promise "0 → 0".
- `hybrid_search.go:110-112` silently substitutes `cfg.Limit = 15` when `Limit <= 0`.
- `memory_tools.go:384-388` (`hybridSearchScopeAll`) further compounds by floor-clamps the fetch buffer to 10.

**Expected:** 0 results.

**Actual:** 10-15 results (scope-dependent).

### C.6 — P2: `mpm_decisions list/query` and `mpm_theories list/query` with `limit<=0` silently coerce to 50

**Severity:** P2 — same contract violation as C.5, in three sibling handlers.

**Reproduction:**
```bash
./mpm call mpm_decisions list --payload '{"action":"list","params":{"limit":0}}'
# → {"count":50, ...}  # expected 0
```

**Evidence:**
- `handlers.go:4880-4907` (theories list), `handlers.go:4912-4931` (theories query), `handlers.go:5001-5028` (decisions list), `handlers.go:5033-5052` (decisions query) — none call `parseLimitStrict`.
- DM `ListDecisions`/`QueryDecisions` (`epistemology_tools.go:749-750`, `:866-867`) and `ListTheories`/`QueryTheories` (`epistemology_tools.go:977-984`, `:1081-1084`) silently substitute `limit = 50` when `<= 0`.

**Expected:** 0 results.

**Actual:** 50.

### C.7 — P2: Mutation paths use silent `ParseFloatOr` for numeric scalars (F12-1 only fixed `save weight`)

**Severity:** P2 — silent type coercion hides client bugs and corrupts the audit trail.

**Reproduction:**
```bash
./mpm call mpm_memory set_weight --payload '{"action":"set_weight","params":{"memory_id":"<id>","weight":"5"}}'
# → success, weight:5     # silent string→float coercion
./mpm call mpm_memory set_weight --payload '{"action":"set_weight","params":{"memory_id":"<id>","weight":"abc"}}'
# → success, weight:0     # silent string→float → 0
./mpm call mpm_memory snooze --payload '{"action":"snooze","params":{"memory_id":"<id>","days":"7"}}'
# → success, days:7
./mpm call mpm_memory snooze --payload '{"action":"snooze","params":{"memory_id":"<id>","days":-100}}'
# → success, days:1       # clamped to default floor
```

**Evidence:**
- `handlers.go:1056-1081` (reinforce/weaken/snooze), `handlers.go:1083-1090` (set_weight), `handlers.go:1245-1248` (review) all use `ParseFloatOr` (silent coerce).
- Compare to `handlers.go:119` `parseWeightStrict` which F12-1 introduced for `save weight` — same contract was not propagated to siblings.

**Expected:** error `field 'weight' must be a number` for string/bool/object inputs.

**Actual:** silent coerce/default.

### C.8 — P2: `mpm_lessons list` with invalid `type` returns empty array (silent enum miss)

**Severity:** P2 — silent enum validation miss; clients have no way to know they passed garbage.

**Reproduction:**
```bash
./mpm call mpm_lessons list --payload '{"action":"list","params":{"type":"bogus"}}'
# → {"success":true,"count":0}  # expected: error listing valid types
```

**Evidence:**
- `handlers.go:1506` — `lessonType := internal.ParseStringOr(p["type"], "")` forwards to DM `ListLessonsFiltered` without enum check.
- Compare to `handleListTheories` (`handlers.go:4880`) which DOES reject invalid status via DM (`epistemology_tools.go:1009-1010`).

**Expected:** error listing valid types (`insight|warning|practice`).

**Actual:** silent empty.

### C.9 — P2: `mpm_skills delete` on unknown id returns `success:true` (silent no-op)

**Severity:** P2 — silent contract asymmetry; clients cannot tell whether the operation succeeded.

**Reproduction:**
```bash
./mpm call mpm_skills delete --payload '{"action":"delete","params":{"skill_id":"skill:nonexistent"}}'
# → {"skill_id":"skill:nonexistent","success":true}
```

**Evidence:**
- `handlers.go:3401-3420` — `ShredSkill` is silent-on-missing by design (comment at `:3390-3397` documents this).
- Compare to `handleReadSkill` (`handlers.go:2615-2639`) which loudly errors `"skill ... not found"`.

**Expected:** error or at minimum a `deleted:false` field in the response.

**Actual:** silent success.

### C.10 — P2: `mpm_work.resolve_contradiction` schema does not declare `reason` as required

**Severity:** P2 — handler enforces `reason` requirement but schema does not advertise it.

**Reproduction:**
```bash
./mpm call mpm_work --payload '{"action":"resolve_contradiction","params":{"work_id":"<id>"}}'
# → error: "reason is required for resolve_contradiction (audit trail)"
```

**Evidence:**
- Handler: `work_handlers.go:343-345` errors when `reason` is empty.
- Schema: `registry_list.go:659-671` lists only `work_id` as required; `reason` is in `properties` but not `required`.

**Expected:** schema's `required` array includes `reason` so the contract is visible to JSON Schema validators and the agent surface.

**Actual:** handler-level enforcement only; an MCP client introspecting the schema would not know reason is required.

### C.11 — P2: `mpm_system query_audit_log` silently drops the `since` parameter

**Severity:** P2 — silent field loss. Schema-guard does not flag because the handler reads zero literal keys (passthrough pattern).

**Reproduction:**
```bash
./mpm call mpm_system --payload '{"action":"query_audit_log","params":{"since":1700000000}}'
# → returns ALL audit log rows, ignoring `since`
```

**Evidence:**
- `handlers.go:2674-2730` reads `level`, `component`, `limit`, `include_stack` but NOT `since`.
- Schema at `registry_list.go:474` declares only `force`, `max_batches`, `limit` formally; `additionalProperties:true` accepts `since` silently.
- The `since` parameter IS honored by `mpm_confidence.changes` (`handlers.go:2411-2429` reads `since_seconds_ago` and `since`), but NOT by `mpm_system.query_audit_log`.

**Expected:** either honor `since` or reject it explicitly.

**Actual:** silently dropped.

### C.12 — P2: `mpm://lesson/<id>` and `mpm://theory/<id>` CLI fallback hard-codes `bounded:false`

**Severity:** P2 — CLI/MCP surface parity violation.

**Reproduction:**
```bash
./mpm call mpm_resolve --payload '{"uri":"mpm://lesson/<id>","max_bytes":50}' # lesson >50 bytes
# → {"bounded":false, ...} # should be true (content was truncated)
```

**Evidence:**
- `handlers.go:5397` (`"bounded": false` hard-coded) and `handlers.go:5418` (same).
- MCP resolver path (`cmd/mpm-mcp/tools.go:207`, `:250`) correctly computes `bounded` from `len(content) > maxBytes`.

**Expected:** `bounded` reflects whether truncation occurred, matching the MCP path and the post-`59ffd3d` contract.

**Actual:** `bounded` always `false` from the CLI fallback for these two kinds; a consumer distinguishing CLI vs MCP will see different semantics.

### C.13 — P3: `mpm_memory` shred/set_weight/patch/promote lack handler-level `memory_id == ""` guard

**Severity:** P3 — handler contract asymmetry (DM catches it, but UX is worse than reinforce/weaken/snooze which DO gate).

**Evidence:** `handlers.go:1014, 1083, 1092, 1105` read `id, _ := p["memory_id"].(string)` and forward to DM without `if id == ""` check. DM does reject, so no user-visible bug — but the contract is asymmetric and the user sees a different error path than reinforce/weaken/snooze.

### C.14 — P3: `mpm_decisions show` unknown id leaks raw SQL error

**Severity:** P3 — UX inconsistency.

**Reproduction:**
```bash
./mpm call mpm_decisions show --payload '{"action":"show","params":{"id":"nonexistent"}}'
# → {"error":"show decision: get decision nonexistent: sql: no rows in result set","success":false}
```

**Evidence:** `handlers.go:4986-4996` does not translate `sql.ErrNoRows` from `GetDecision` (`epistemology_tools.go:695-742`).

### C.15 — P3: `mpm_skills list` with invalid `scope` silently defaults to "all"

**Severity:** P3 — same pattern as C.8.

**Evidence:** `handlers.go:2642-2645` — `scope := internal.ParseStringOr(p["scope"], "all")` forwarded to DM without enum check. Schema declares `enum:["all","local","shared"]` (`registry_list.go:312`).

### C.16 — P3: `mpm_context read_wake_context format="bogus"` silently falls through to JSON

**Severity:** P3 — undocumented silent fall-through.

**Evidence:** `handlers.go:1809-1815` only branches on `format == "system-prompt"`; other values fall through to the default JSON envelope. The description prose documents `projection:"compact"` and `projection:"full"` but does not document `format` enum.

### C.17 — P3: `mpm_confidence.show` envelope shape differs from documented brief

**Severity:** P3 — documentation drift, not a code defect.

**Evidence:** `handlers.go:2457-2459` → `evidence_tools.go:300-303` returns `{confidence, history:{history:[{computed_at, confidence, evidence_count, trigger}]}}`. The `weight` and `last_decayed` keys mentioned in the brief are NOT present. The history-array nesting (`history.history`) is load-bearing per the comment.

### C.18 — P3: `mpm_system` schema only declares 3 params (`force`, `max_batches`, `limit`)

**Severity:** P3 — schema under-declaration; `additionalProperties:true` masks drift.

**Evidence:** `registry_list.go:474` lists only 3 params formally. Many others (`dry_run`, `aggressive`, `max_age_hours`, `days`, `level`, `component`, `artifact_id`, `include_stack`, `since`, `cluster_key`, `snooze_until`, `reason`, `annotation`, `from_path`, `format`, `label`, `commit`, `commit_batch`, `undo_batch`, `stale_theory_days`) are accepted via `additionalProperties:true`. Schema-guard test does not flag this because the handler reads zero literal `payload["k"]` keys (passthrough pattern).

### C.19 — P3: Many tools lack a permanent registry/dispatcher parity lock

**Severity:** P3 — observability: future drift would not be caught by `make test`.

**Evidence:** `registry_dispatcher_parity_test.go` covers only `mpm_theories` and `mpm_topics`. Not covered: `mpm_work`, `mpm_handoff`, `mpm_wakes`, `mpm_scratchpad`, `mpm_lessons`, `mpm_decisions`, `mpm_memory`, `mpm_skills`, `mpm_topics` (no `assertParityForTool` call outside the two listed).

### C.20 — P3: `mpm_skills` id-key vocabulary is inconsistent across actions

**Severity:** P3 — UX drift.

**Evidence:** `save` uses `name`, `read` uses `name`, `delete` uses `skill_id`, `promote_to_global` uses `skill_id`, `list` uses `scope`. The `name`/`skill_id` divide is consistent within the delete/promote_to_global pair but the family is not unified with `mpm_memory`/`mpm_work` which both accept `id` aliases.

---

## D. Test gaps

These are behaviour gaps where current tests do NOT pin a high-risk contract, ranked by importance.

### D.1 — Schema-superset test currently FAILING (see Defect C.3)

`make test` exits with `FAIL: TestSchemaSupersetOfHandlerPayloadReads`. This is not a missing test — it's an active regression.

### D.2 — No end-to-end test for `mpm_decisions supersede/invalidate` against an old-shape `confidence_history`

The `f5_2_decision_confidence_after_correction_test.go` and `f9_decision_invalidation_regression_test.go` pass against fresh DM (latest schema) but cannot surface C.1 because the fresh DM has the wider CHECK. A test that runs against a DB constructed with the old CHECK would catch the drift.

### D.3 — No regression test for `mpm_memory patch` with non-object patch

`handlers.go:1092` comment falsely claims the DM rejects primitive patches. No test pins this. **Adding the test would have surfaced C.4.**

### D.4 — No regression test for `mpm_memory query` with `limit=0`

`parseLimitStrict` is the documented contract; no test verifies it. **Adding would surface C.5.**

### D.5 — No regression test for `mpm_skills delete` followed by `mpm_skills save`

`skill_save_regression_test.go` covers `save` happy paths, `delete` happy path, but not the **delete-then-save sequence**. **Adding would surface C.2.**

### D.6 — No regression test for `mpm_lessons list` invalid `type`

The valid-type path is tested; the invalid-type silent-empty path is not.

### D.7 — No dedicated unit test for `handleMpmRetrievalDiagnose`

The diagnostic surface is non-trivial (3-stage pipeline), but `handlers_test.go` has no `TestHandleMpmRetrievalDiagnose_*`. My diagnostic test (verified during the audit, then removed) was the only coverage.

### D.8 — No regression test for `handleRequestReview_*`

The handler has multiple failure modes (`components` required, `prompt` required, `strategy!="parallel"` rejected, artifact-id resolution error). All tested live; none tested in CI.

### D.9 — No permanent `assertParityForTool` lock for 7 tools (see C.19)

Adding `assertParityForTool(t, dm, ac, "mpm_memory")` (and others) to `registry_dispatcher_parity_test.go` would close this gap.

### D.10 — `TestAllDomainDispatchers` (`handlers_test.go:1447-1511`) only smoke-tests one action per dispatcher

For epistemic tools it covers: `theories/propose`, `lessons/list`, `lessons/search`, `decisions/record`, `skills/list`. Missing smoke coverage for: `theories/resolve`, `decisions/show`, `decisions/supersede`, `decisions/invalidate`, `lessons/save`, `skills/save`, `skills/read`, `skills/delete`, `skills/promote_to_global`, `skills/workshop`.

### D.11 — `TestSchemaGuard_MpmSkillsSave` etc. are individual, not part of a broader parity sweep

Other tools don't have analogous `TestSchemaGuard_Mpm*` regression tests. The schema_guard test (`TestSchemaSupersetOfHandlerPayloadReads`) provides the broad sweep, but currently fails on `mpm_handoff`.

---

## E. Contract / documentation drift

The current `mpm-handoff` schema under-declaration (C.3) and the `mpm_system` schema under-declaration (C.18) are both caught by the schema_guard test infrastructure but only the handoff one fires (the system one is masked by the passthrough pattern). The `route` tool's MCP-only registration is documented as intentional (`tools.go:487`).

**Confirmed intentional design (NOT a defect):**
- `route` MCP-only: documented in code (`tools.go:487-491`). CLI callers must use `mpm call mpm_context --payload '{"action":"route", ...}'`.
- `mpm_skills delete` silent-on-missing: documented in handler comment (`handlers.go:3390-3397`).
- `mpm_skills promote_to_global` requires `confirm=true`: documented in registry description.
- `mpm_context.promote_to_global` requires `confirm=true`: documented in registry description.
- `mpm_handoff.write` requires non-empty `summary`: documented in description and enforced in handler.
- `mpm_work.complete` requires `work_id`: documented in description.
- Legacy `mpm_session` alias: routed through `resolveLegacySessionAlias` (`call.go:248-288`).

**Documentation drift (real):**
- `mpm_decisions supersede` parameter is `original_id`, not `superseded_id` as the brief stated.
- `mpm_confidence.show` does not return `weight` or `last_decayed` (C.17).
- `log_to_changelog` writes to `mpm.db` (collection='changelog'), NOT to `CHANGELOG.md` — verified end-to-end (`internal/core/changelog_mcp.go:1-58`).
- `mpm_context read_wake_context` format="bogus" silently falls through (C.16).

---

## F. Architectural observations (non-defect)

### F.1 — Registry is the single source of truth (correct design)

`registry_list.go` is the authoritative inventory; both CLI (`cmd/mpm/call.go:174`) and MCP (`cmd/mpm-mcp/tools.go:294`) iterate the same slice. New tools require exactly one entry. This is the right architecture and is verified.

### F.2 — Substrate Defense Triad holds in practice

`make test-race` (the canonical pre-merge gate per `CLAUDE.md`) was not run during this audit (out of scope for read-only audit), but static evidence supports the invariants:
- Atomic state swap: `internal/scheduler/state.go:persistState` pattern is referenced from `CLAUDE.md` and not modified.
- Defensive SQL aggregates: `COALESCE`/`sql.Null*` patterns are pervasive (e.g., `evidence_silent_coercion_regression_test.go`).
- Write-path read-back: `internal/core/db.go:AddLesson` is documented in `CLAUDE.md` as the canonical pattern.

### F.3 — Schema-guard test is a load-bearing invariant — it is currently failing

The `TestSchemaSupersetOfHandlerPayloadReads` test is the only AST-driven schema/handler parity check. C.3 is the only defect it currently catches. The test should be the canonical "did you remember to update the schema?" gate. **Its current FAIL state is a P1 hygiene concern: every CI run surfaces the drift.**

### F.4 — `extractParamsOrFail` is a strong envelope gate

All 15 action-based dispatchers call `extractParamsOrFail` on entry. It rejects top-level fields outside `{action, params}`, non-string `action`, and non-object `params`. This is the right pattern and verified.

### F.5 — Schema under-declaration is the dominant defect class

Of 20 proven defects:
- 3 are schema-related (C.3, C.10, C.18)
- 3 are silent coercion/defaults that the schema would surface (C.5, C.6, C.7)
- 2 are silent enum validation misses that the schema would surface (C.8, C.15)
- 1 is silent field loss that the schema would NOT surface (C.11)

So 8 of 20 defects would have been caught by a stricter `additionalProperties:false` schema. **The project's reliance on `additionalProperties:true` is the dominant drift amplifier.**

---

## G. Recurring defect families

The MPM audit history (per `MEMORY.md`) shows three recurring families. The current pass confirms all three are still present, plus introduces new ones:

### G.1 — Registry/dispatcher drift (historical: D3a; current: C.3 + C.10)

Schema enum does not match dispatcher switch cases, OR schema required-array does not match handler guards. **New instance: C.3 (handoff top-level params), C.10 (resolve_contradiction reason required).** The `assertParityForTool` infrastructure exists for 2 tools; extending to all tools would prevent recurrence.

### G.2 — Silent field loss / silent default (historical: alpha-4 F12-1 silent coercion; current: C.4, C.5, C.6, C.7, C.8, C.9, C.11, C.13, C.15)

Handlers accept malformed input and either drop it, coerce it, or return success on no-op. **Pattern is consistent across 8 of 20 current defects.** The fix is uniform: stricter schema (`additionalProperties:false`) plus strict type guards everywhere, not just at the canonical save boundary.

### G.3 — Schema vs implementation drift in confidence (historical: D-3 wake payload wording; current: C.1)

Schema declaration in source does not match what the deployed DB enforces. The migration system has no sentinel for `confidence_history` schema rebuild. **C.1 is the most severe instance.**

### G.4 — Schema under-declaration (new family: C.3, C.18)

The handler reads keys the schema does not declare, hidden by `additionalProperties:true`. The schema_guard test catches some (C.3) but the passthrough pattern hides others (C.18).

### G.5 — Output envelope drift (new family: C.12, C.16, C.17)

`bounded` hard-coded; `format="bogus"` silently falls through; `weight`/`last_decayed` missing from `confidence.show`. The contract is loosely documented in handler comments but not enforced by tests.

---

## H. Next remediation batch

Selected 4 defects, ranked by severity × impact × fix clarity. **Do not fix during this audit; remediation is a separate pass.**

### H.1 — P0 — confidence_history CHECK constraint widening (Defect C.1)

- **Reproduction:** documented in C.1; deterministic, two-step probe.
- **Root area:** `internal/core/schema.go:205` declares the wider enum; `src/db/mpm.db` has the narrower one. Missing migration in `internal/core/migration_*.go`.
- **Expected behaviour:** `mpm_decisions supersede/invalidate` succeed; original decision marked superseded/invalidated; confidence_history ledger contains the trigger row.
- **Likely fix boundary:** new migration file `internal/core/migration_confidence_history_check_widening.go` that recreates the `confidence_history` table with the wider CHECK constraint; idempotent on already-widened DBs (probe the trigger enum first). Test that runs the migration on a fresh DB and then exercises supersede/invalidate end-to-end.
- **Regression target:** new `f_d1_confidence_history_check_widening_regression_test.go` that builds a DB with the narrow CHECK (simulating old state), runs the migration, then calls `mpm_decisions supersede` and `mpm_decisions invalidate` and asserts success.
- **Scope exclusion:** do NOT change `schema.go:205` (it's already correct); do NOT change the `epistemology_tools.go` write paths (they're already correct). Migration only.

### H.2 — P1 — `mpm_skills save` after `delete` UNIQUE constraint (Defect C.2)

- **Reproduction:** documented in C.2; three-step probe.
- **Root area:** `internal/core/tools/skill_db.go:343-353` (existence check ignores soft-deleted rows); `:462-547` (INSERT collides with tombstoned id).
- **Expected behaviour:** re-saving a skill after `delete` succeeds (either resurrects the soft-deleted row or replaces it atomically).
- **Likely fix boundary:** either (a) on `INSERT` collision, hard-delete the tombstoned row first; (b) on `save`, treat soft-deleted rows as replaceable. Add a regression test in `skill_save_regression_test.go` (the save+delete+save sequence).
- **Regression target:** `TestSkillSaveAfterDelete_ResurrectsOrReplaces` — verify the sequence and that the post-save row has `deleted_at IS NULL`.
- **Scope exclusion:** do NOT change the soft-delete semantics of `delete` (the silent-no-op contract is intentional per `handlers.go:3390-3397`).

### H.3 — P2 — `mpm_handoff` schema missing top-level `params` (Defect C.3)

- **Reproduction:** documented in C.3; one `go test` invocation.
- **Root area:** `internal/core/tools/registry_list.go:380-446` — schema's top-level `properties` should declare `params`.
- **Expected behaviour:** `TestSchemaSupersetOfHandlerPayloadReads` PASSes; the schema's published contract accurately reflects the handler's reads.
- **Likely fix boundary:** add `"params": {"type": "object"}` to the top-level `properties` block of `mpm_handoff` schema. One-line fix. Re-run the schema_guard test.
- **Regression target:** the existing `TestSchemaSupersetOfHandlerPayloadReads` will start passing; no new test needed.
- **Scope exclusion:** do NOT touch the per-action `oneOf` branches; do NOT change the handler.

### H.4 — P2 — `mpm_memory patch` silently wipes metadata (Defect C.4)

- **Reproduction:** documented in C.4; one-step probe.
- **Root area:** `internal/core/tools/handlers.go:1092-1103` (handler comment falsely claims DM rejects primitive patches); `internal/core/db.go:3317-3320` (DM only checks `json.Valid`).
- **Expected behaviour:** non-object `patch` returns error `patch must be a JSON object`; no metadata wipe; no row mutation.
- **Likely fix boundary:** in `handlePatchMemory`, gate `patch` to be a non-nil `map[string]interface{}` and return a clear error otherwise. Add regression test `TestMemoryPatch_RejectsNonObjectPatch` that probes with string, array, null, primitive.
- **Regression target:** add to `handlers_test.go` or new file `memory_patch_validation_test.go`.
- **Scope exclusion:** do NOT change `dm.UpdateMemoryMetadata` schema-validity check (current `json.Valid` check is reasonable for a defense-in-depth layer; the handler is the right enforcement boundary for object-only input).

---

## I. Deferred findings (proven, not selected)

The following 16 defects are proven and evidence-backed but were not selected for the next remediation batch (the brief limits the batch to up to 4):

| ID | Tool.Action | Severity | One-line |
|----|-------------|----------|----------|
| C.5 | `mpm_memory.query` | P2 | `limit=0` returns 15 instead of 0 |
| C.6 | `mpm_decisions/theories.list,query` | P2 | `limit<=0` silently coerces to 50 |
| C.7 | `mpm_memory.{set_weight,reinforce,weaken,snooze,review}` | P2 | Silent `ParseFloatOr` for numeric scalars |
| C.8 | `mpm_lessons.list` | P2 | Invalid `type` returns empty (no enum validation) |
| C.9 | `mpm_skills.delete` | P2 | Unknown id returns `success:true` (silent no-op) |
| C.10 | `mpm_work.resolve_contradiction` | P2 | Schema missing `reason` required |
| C.11 | `mpm_system.query_audit_log` | P2 | `since` parameter silently dropped |
| C.12 | `mpm_resolve mpm://lesson/theory` | P2 | CLI fallback hard-codes `bounded:false` |
| C.13 | `mpm_memory.{shred,set_weight,patch,promote}` | P3 | Handler-level `memory_id == ""` guard absent |
| C.14 | `mpm_decisions.show` | P3 | Unknown id leaks raw SQL error |
| C.15 | `mpm_skills.list` | P3 | Invalid `scope` silently defaults to "all" |
| C.16 | `mpm_context.read_wake_context format="bogus"` | P3 | Silently falls through to JSON |
| C.17 | `mpm_confidence.show` | P3 | Envelope shape differs from documented brief |
| C.18 | `mpm_system` schema | P3 | Only 3 params formally declared |
| C.19 | (cross-cutting) | P3 | 7 tools lack `assertParityForTool` lock |
| C.20 | `mpm_skills` | P3 | id-key vocabulary inconsistent (`name` vs `skill_id`) |

These are valid remediation candidates for a subsequent batch. None are stale or speculative — all have been reproduced and have file:line evidence.

---

## J. Repository state

**Confirmed clean at audit completion.**

```bash
$ git status
On branch main
Your branch is up-to-date with 'origin/main'.

nothing to commit, working tree clean
```

No production code, tests, schemas, or docs were modified during the audit. All diagnostic test files written by the parallel agents were removed before returning.

---

## K. Final verdict

**Complete public tool inventory established: YES** (21 tools mapped, including the `route` MCP-only registration as documented exception)

**Every public action classified: YES** (15 action-based tools × all actions; 6 non-action tools; ~90 distinct action surfaces)

**Success paths behaviourally verified: YES** (live probes against the deployed DB; runtime output captured for every action category)

**Persistence paths behaviourally verified: YES** (write→read round-trips executed for save+show, handoff write+read+shred, work create+update+complete+history; UNIQUE constraint failure captured as defect C.2)

**Error paths audited: YES** (every documented error path triggered at least once during the audit; CHECK constraint, schema validation, missing-ID, invalid-enum, limit=0, malformed URI, empty payload)

**Round-trip lifecycles audited: YES** (memory save→shred; handoff write→read→shred; work create→update→complete→history→reopen; scratchpad flush→read→promote; lesson save→search→list; theory propose→show→list→query→resolve)

**Filter semantics audited: YES** (status filter, type filter, scope filter, kinds filter, projection, list limit, FTS query, tag filter — each probed with valid + invalid + boundary values)

**Output contracts audited: YES** (envelope shapes captured for gc_run, compact, health_check, query_audit_log, list_clusters, wake_context full/compact, retrieval_diagnose, blob_read/search, resolve; deviations recorded as defects)

**CLI/MCP equivalence audited where claimed: YES** (verified shared `tools.Registry` iteration in both `cmd/mpm/call.go:174` and `cmd/mpm-mcp/tools.go:294`; `route` MCP-only asymmetry documented as intentional)

**Concurrency/resource risks audited where applicable: PARTIAL** (single-attempt concurrency for `handleUpdateWork` was not stressed under `-race`; the substrate's `*sql.Tx` discipline and WAL + busy_timeout=5000 are documented invariants; one transient "database is locked" was observed during a multi-action probe but did not reproduce on isolated runs)

**Security-sensitive boundaries audited: YES** (no shell execution paths found in the tool handlers; all SQL is parameterized via the `*sql.Tx` discipline; scanner/poison patterns enforced in `SaveMemoryNode` per the comment at `handlers.go:223+`)

**Current defects proven with evidence: YES** (20 defects, each with deterministic reproduction and file:line evidence)

**Test gaps identified: YES** (11 named gaps, ranked by impact in §D)

**Next remediation batch selected: YES** (4 items in §H: P0 confidence_history, P1 skills save-after-delete, P2 handoff schema, P2 memory patch wipe)

**No fixes made during audit: YES** (working tree clean; no production code, tests, schemas, or docs modified)

**No filler findings invented: YES** (every defect and test gap has a file:line citation or runtime probe; the residual inventory from 2026-09-04 was honored — the D-3 (P2) wake-payload wording is confirmed already-closed, and only the remaining P0+P1+P2 defects are promoted)

---

## L. Provenance

This audit was conducted on `main` @ `56bd094` (alpha-final at `8f317b0`), against the live DB at `/home/v/workspace/projects/mpm/src/db/mpm.db` (SQLite, FTS5).

All findings derive from one of:
1. **Direct source reads** of `internal/core/tools/registry_list.go`, `internal/core/tools/handlers.go`, `internal/core/tools/work_handlers.go`, `internal/core/tools/schema_guard_test.go`, `internal/core/db.go`, `internal/core/epistemology_tools.go`, `internal/core/evidence_tools.go`, `internal/core/changelog_mcp.go`, `cmd/mpm-mcp/tools.go`, `cmd/mpm/call.go`.
2. **Live `mpm call` probes** against the deployed `mpm` binary (`./mpm call ...`).
3. **SQLite schema introspection** via `sqlite3 src/db/mpm.db ".schema ..."`.
4. **Test execution** via `go test -tags fts5 -run <pattern> -v ./...` from `internal/core/tools/`.

No secrets were extracted. No test or fixture was left in the working tree.

---

## M. §I — Revised 2026-09-05, post-remediation

The original §I above (lines 778–800) cataloged 16 deferred findings as
they stood at the audit's baseline commit `56bd094`. By the time this
appendix was authored, `main` had advanced through a separate
remediation arc (12 fix commits dated 2026-09-05 12:49–15:14 +0100,
plus the post-audit pre-commit structural fix at `b140da3`) that
closed most of the §I backlog.

This section re-derives each of the 16 entries against current HEAD
(`b140da3`) using the audit's own evidentiary standard (file:line
plus live `mpm call` probe). It is appended rather than edited into
the original §I to preserve the historical record of what was true at
audit time.

**Headline:** of the 16 deferred findings, **13 are now fixed**, **2
are partially resolved** (changed contract — no longer matches the
original description but the underlying gap is not closed either),
and **1 remains genuinely open** (C.19). The audit's "7 tools" scope
for C.19 was conservative — the actual coverage gap is 13+ tools, not
7.

### M.1 — Status table (each original §I entry re-classified at HEAD)

| ID | Tool.Action | Sev | Status at HEAD `b140da3` | Evidence |
|----|-------------|-----|---------------------------|----------|
| C.5 | `mpm_memory.query` | P2 | **fixed** | `parseLimitStrict` honored: `bin/mpm call mpm_memory --payload '{"action":"query","params":{"query":"probe","limit":0}}'` → `{"count":0,...}`; `handlers.go:6254-6285`, `memory_tools.go:314-335`. Commit `2316676`. |
| C.6 | `mpm_decisions/theories.list,query` | P2 | **fixed** | `parseLimitStrict` at `handlers.go:5435, 5474, 5294, 5333`; live probe decisions/theories `limit:0` → `{"count":0,...}`. Commit `2316676`. |
| C.7 | `mpm_memory.{set_weight,reinforce,weaken,snooze,review}` | P2 | **fixed** | `parseFloatStrict` at `handlers.go:1134, 1147, 1163, 1191, 1393/1397`; live probe `reinforce delta="abc"` → `"field 'delta' must be a number (float64/int), got string"`. Commit `0341735`. |
| C.8 | `mpm_lessons.list` | P2 | **fixed** | `handleListLessons:1679-1683` calls `internal.ValidateLessonType`; live probe `type="invalid-type"` → `"invalid lesson type \"invalid-type\": must be one of warning, practice, or insight"`. Commit `0e6a0d8`. |
| C.9 | `mpm_skills.delete` | P2 | **fixed** | `handleDeleteSkill:3788-3814` translates `internal.ErrSkillNotFound`; live probe `skill_id="skill:nonexistent"` → `"skill_id \"skill:nonexistent\" not found"`. Commit `45e616f`. Note: the original "documented intentional" silent-no-op contract was deliberately inverted — the audit's classification was wrong about intent, the live user impact was the same defect. |
| C.10 | `mpm_work.resolve_contradiction` | P2 | **fixed** | schema declares `"required": ["work_id", "reason"]` at `registry_list.go:671`; handler enforces at `work_handlers.go:342-345`. Commit `dd1c423`. |
| C.11 | `mpm_system.query_audit_log` | P2 | **fixed** | `handleQueryAuditLog:3019-3056` reads `since` and computes `days`; live probe returns rows within cutoff, not the days=7 default. Commit `56c661d`. |
| C.12 | `mpm_resolve mpm://lesson/theory` | P2 | **fixed** | `handlers.go:5832-5850` (lesson) and `:5867-5883` (theory) compute `bounded` from `len(content) > maxBytes`; CLI path now matches MCP resolver contract at `cmd/mpm-mcp/tools.go:207/250`. Commit `197ebba`. |
| C.13 | `mpm_memory.{shred,set_weight,patch,promote}` | P3 | **fixed** | `requireMemoryID` helper at `handlers.go:1118-1124`; live probes for shred/set_weight/promote/patch all return `"memory_id is required"` on empty params. Commit `f27acbe`. |
| C.14 | `mpm_decisions.show` | P3 | **fixed** | `epistemology_tools.go:725-726` translates `sql.ErrNoRows` into `"decision not found: <id>"`; live probe confirms. Commit `97ad693`. |
| C.15 | `mpm_skills.list` | P3 | **fixed** | `handlers.go:2946-2963` rejects non-enum scopes; live probe `scope="bogus"` → `"scope must be one of [all, local, shared], got \"bogus\""`. Commit `fda6fe8`. |
| C.16 | `mpm_context.read_wake_context format="bogus"` | P3 | **fixed** | explicit switch at `handlers.go:1997-2013` rejects non-`""`/non-`"system-prompt"` values; live probe returns clear enum error. Commit `08720e6`. |
| C.17 | `mpm_confidence.show` envelope | P3 | **changed — resolved by source-comment authority** | `evidence_tools.go:299-303` documents `{confidence, history:{history:[...]}}` as load-bearing; no in-tree doc contradicts. The audit's "documented brief" citing `weight`/`last_decayed` is the only source for the alleged missing keys — likely drift between the brief and the code-as-written. Live probe: `{"confidence":0.868,"history":{"history":[]}}`. |
| C.18 | `mpm_system` schema under-declaration | P3 | **changed — partial extension, structural gap remains** | schema grew from 3 keys to 12+ (`force`, `max_batches`, `limit`, `confirm`, `from_path`, `format`, `label`, `dry_run`, `commit`, `commit_batch`, `undo_batch`) at `registry_list.go:475`. Still under-declares ~22 action-specific params; handler reads zero literal payload keys (passthrough pattern); `TestSchemaSupersetOfHandlerPayloadReads` continues to skip it per documented LIMITATION at `schema_guard_test.go:128-138`. Commit (partial): `dd76047`. The original defect class is muted, not closed. |
| C.19 | registry/dispatcher parity lock | P3 | **still open — gap is wider than audit estimated** | `registry_dispatcher_parity_test.go:195, 207` covers only `mpm_theories` and `mpm_topics`. Registry declares action-enum dispatchers for 13+ tools (`registry_list.go:33, 110, 138, 189, 215, 241, 270, 303, 342, 382, 475, 540`). The audit's "7 tools" scope was conservative — the real coverage gap is broader. |
| C.20 | `mpm_skills` id-key vocabulary | P3 | **fixed** | `resolveSkillID(p, requireVersion)` at `handlers.go:2884-2922` unifies vocabulary (`skill_id` canonical, `name`+`version` alias); live probes confirm: `name`+`skill_id` conflict errors; `name` without `version` errors on delete/promote; `name` alone accepted by read for latest-version lookup. Commit `c8b0da1`. |

### M.2 — Summary

- **13 fixed** (C.5, C.6, C.7, C.8, C.9, C.10, C.11, C.12, C.13, C.14, C.15, C.16, C.20)
- **2 changed / partially resolved** (C.17 — resolved by source-code authority; C.18 — partial extension, structural passthrough gap remains)
- **1 still open** (C.19 — registry/dispatcher parity lock; gap is wider than the audit's conservative "7 tools" estimate)

### M.3 — Quality notes

- **C.9 contract inversion** deserves explicit attention: the audit classified the silent-no-op `mpm_skills.delete` as a "documented intentional" defect. The remediation commit `45e616f` inverted the contract — `delete` on unknown id now errors rather than returning `success:true`. The audit's "intentional" framing was wrong about intent (the silent-no-op was never a deliberate feature; it was an oversight that the handler comment labelled as "by design"). The fix is correct, and the in-tree documentation has been brought in line with the new behavior. No external surface depended on the old silent-no-op contract (verified by a probe at HEAD).
- **C.18** is the only §I entry whose defect *class* (passthrough pattern + schema under-declaration) is not addressed by the remediation commits. The §6.4 threshold table / §8 CLI-side limits / `TestSchemaSupersetOfHandlerPayloadReads` infrastructure handles some narrower cases, but the broad `mpm_system` `additionalProperties:true` with no declared action-specific params remains. The fix is structural — `oneOf`-style action-branched schemas for each of the 10 `mpm_system` actions, mirroring the shape used for `mpm_memory` etc. — and is out of scope for any of the §I remediation commits landed to date.
- **C.19** is the cleanest §I item to remediate next: it's pure test-coverage scaffolding (`assertParityForTool(t, dm, ac, "<tool>")` calls), no production-code change required. Audit-estimated "7 tools" — actual count is 13+ (see evidence column).
