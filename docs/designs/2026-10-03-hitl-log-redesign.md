# HITL Log Redesign: mirror.jsonl and watchdog.jsonl

Status: **PROPOSED — NOT IMPLEMENTED**

Date: 2026-10-03

Amendment A (2026-10-03, review feedback, still PROPOSED — NOT
IMPLEMENTED): retention is now a combined size-OR-age rotation plus
age-expiry with count cap (§9 rewritten with arithmetic); `summary`
replaced by deterministic `preview` (§6); SQL normalization is a hard
security contract (§7, §11, §17); destructive/wipe semantics specified
(§12A); single shared rotation path required (§12B); synthesis spelling
settled (§12); `Stats()` finding recorded (§12C); phases expanded to
A–F (§16).

Scope: `src/db/mirror.jsonl`, `src/db/watchdog.jsonl` only.
No change to `mpm.db`, `system_audit_log`, `telemetry.db` (future), writers, or rotation behavior in this task.

## 1. Motivation

`mirror.jsonl` and `watchdog.jsonl` were intended as small,
human-readable HITL files: understandable directly from a terminal with
`tail`, `grep`, `jq`, `less`, `zgrep` when something needs inspection.

They have drifted into high-volume machine exhaust:

- mirror: full `Memory` object serialization including `content` and
  `embedding` on nearly every record.
- watchdog: per-SQL execution trace (`exec`/`query`/`query_row`) for
  fast successful operations.

The post-alpha roadmap (`docs/ROADMAP.md`, Post-alpha telemetry)
explicitly reserves structured behavioural analysis, retrieval metrics,
execution metadata, and framework/model comparisons for `telemetry.db`.
mirror/watchdog must not evolve into proto-telemetry.

Goal: restore both files to a concise human-readable journal (mirror)
and operational black-box log (watchdog), with explicit inclusion rules,
minimal schemas, retention sized to HITL use, and a hard telemetry
boundary.

Non-goals (explicit): implement `telemetry.db`; build analytics; change
telemetry schema; web UI; delete or rewrite historical logs; modify live
`mpm.db`; change writers or rotation behavior yet.

## 2. Original HITL purpose

Conceptual boundaries (reaffirmed):

| Store | Role |
|---|---|
| `mpm.db` | authoritative persistent cognitive/substrate state |
| `mirror.jsonl` | human-readable view of meaningful cognitive/state changes |
| `watchdog.jsonl` | human-readable operational/diagnostic view |
| `telemetry.db` | future structured, machine-oriented analytics layer |

HITL usability test adopted by this design:

> A record belongs in one of these files only if it helps a human
> answer a useful question from `tail`, `grep`, `jq`, `less`, `zgrep`
> without a database query or analytics pipeline. A human should be
> able to inspect either file for a few seconds and understand what MPM
> has recently been doing or what went wrong.

Corollary: a field exists in these files only if a human reading a
terminal benefits. Fields kept "because a machine may analyse it later"
belong in `telemetry.db`, not here.

## 3. Current state: writers

### 3.1 mirror.jsonl writers (5 sites)

Canonical path: `config.GetMirrorPath()` → `<mpmdir>/src/db/mirror.jsonl`
(`internal/core/config/config.go:590`). Mirrors hang off the same root
as `mpm.db` (`internal/core/db.go:1103`).

W1 — normal memory save: `MemoryStore.appendToMirror`
(`internal/core/memory.go:1332-1349`).
Gated by `isMirroredCollection` allow-list
(`memory.go:1358-1373`): `changelog, memories, theories, decisions,
knowledge, directives, mpm-projects, world-cup-2026` = true.
Explicitly not mirrored: `lessons` (own table), `scratchpad_orphans`,
`ephemeral_scratchpad`. Writes full `Memory` struct
(`memory.go:39-59`): `id, content, metadata, tags, created_at, source,
embedding,omitempty, collection, session_id, reference_id,
reinforcement_count, weight, retrieval_priority, importance,
confidence, last_accessed_at, expires_at, suggested_topics, score`.
Callers: `AddMemory` (`:417`), `AddMemoryWithWeight` (`:560`),
`AddMemoryWithWeightForInvocation` (`:642`, the MCP/CLI
`mpm_memory save` path). Errors are warn-only. No inline rotation.

W2 — blocked content: `MemoryStore.appendBlockedAttempt`
(`memory.go:1392-1422`, F-4 hardened 2026-09-04). Writes
`{timestamp, reason, pattern_family, content_sha256, content_length,
action:"blocked", type:"sensitive_attempt"|"poison_attempt"}`. No raw
content anywhere. Callers: `logSensitiveAttempt` (`:987`),
`logPoisonAttempt` (`:1002`), which also `LogAudit` to
`system_audit_log` and stderr.

W3 — contradiction async: `DatabaseManager.ChallengeMemoryAsync`
(`internal/core/db.go:5275-5304`). Detached goroutine (`mirrorWG`),
writes `{event:"contradiction_detected", memory_id, evidence,
timestamp}`. Only mirror writer that checks rotation inline
(`rotateLogIfNeeded` under shared `watchdogMu`). Operational state
lives in `shared.contradiction_log`; mirror is forensic trail
(`internal/core/contradiction_log.go:4,107-111`).

W4 — embedding backfill failure: `logEmbeddingFailure`
(`cmd/mpm/backfill_embeddings.go:429-448`). Writes
`{ts, type:"embedding_failure", memory_id, error}`. No content.

W5 (destructive) — `MemoryStore.ClearMirror` (`memory.go:2347`):
`os.Remove` mirror file. Only caller
`handleMemoryWipe --force` (`cmd/mpm/handlers_memory.go:973-994`).

Note: direct `DM.SaveMemoryNode*` without the `MemoryStore.Add*`
wrapper writes DB only, no mirror line. `SPEC.md:3956` claim about
`capability_source_code` mirroring has no code writer (doc-only).

### 3.2 watchdog.jsonl writers (2 primitives)

Canonical path `<dbDir>/watchdog.jsonl`
(`internal/core/db.go:1182,1273,1880`).

Primitive A — query timing: `watchdogOp`
(`db.go:596-605`): `{timestamp(RFC3339Nano), operation, duration_ms,
query?(truncated 120ch), retries?, error?, slow:bool}`.
`logWatchdog` (`:612-634`) and `logWatchdogRaw` (`:640-656`, custom
schemas) both rotate-then-append under `watchdogMu`, mode `0600`,
failures non-fatal. Emitters: `txNode.ExecTracked` (`:754`,
`operation:exec`), `QueryTracked` (`:786`), `QueryRowTracked`
(`:804`), standalone `ExecTracked` (`:927`), `QueryTracked`
(`:977`), `QueryRowTracked` (`:1015`, spelled `queryrow` vs tx
`query_row`). Busy-retry loop 100ms×2, cap 5s. `slowQueryThreshold`
= 100ms (`:441`).

Primitive B — synthesis events: `logWatchdogOp`
(`internal/core/synthesis_auto.go:620-643`): envelope
`{op, timestamp}+data` via `logWatchdogRaw`, deliberately not
`watchdogOp` shape. Emits `synthesize` (success ledger),
`synthesize_skip` (dedup reasons), `synthesize_failed` (LLM/save
failure, content truncated 120, secrets excluded `:476-482`).
No current writer emits `synthesize_error`, though two readers filter
for it (mismatch, see §4).

### 3.3 Rotation

`rotateLogIfNeeded` (`db.go:93-158`): stat; if size ≥ threshold, gzip
to `path.YYYYMMDD-HHMMSS.gz`, truncate original. Missing → nil.
Not atomic cross-process; contract is one process owns one workspace.
Default `defaultLogRotateBytes` = 5 MiB (`db.go:54-58`), override
`MPM_LOG_ROTATE_BYTES`. Auto-rotation: every `logWatchdog/Raw`
pre-check (`:619,646`), startup for both files (`:1222-1232`),
`ChallengeMemoryAsync` for mirror (`:5294`). `appendToMirror` /
`appendBlockedAttempt` do not rotate. CLI: `mpm ops logs
rotate|status` (`cmd/mpm/handlers_logs.go:52-194`,
`router.go:881-882`); force via threshold 1 byte.

## 4. Current state: readers

### 4.1 mirror.jsonl readers (almost nothing programmatic)

- `MemoryStore.Stats` (`memory.go:1468-1485`): reads file, counts lines
  → `stats[mirror_entries]`, exposes `mirror_file` path. Only
  programmatic read.
- Human/operator: `tail`/`jq` on `src/db/mirror.jsonl` + `*.gz`;
  `mpm ops logs status` sizes.
- Negative: no `LoadMirror`, no replay/ingest/restore, no FTS, no
  `mpm_memory` query path, no cascade materializer, no
  recovery-from-mirror. `contradiction_log.go:4` is explicit: table is
  source of truth, mirror is forensic trail.

### 4.2 watchdog.jsonl readers

- `RecentWatchdogOps(n, opPrefix)` (`db.go:664-712`): reads under lock,
  hard cap 10000, skips corrupt lines, prefix filter, newest-last.
  Exposed via `CoreDB` (`core.go:78,80`).
- `handleSynthesizeStatus(20)` / `handleSynthesizeFailures(50)`
  (`cmd/mpm/synthesize_cmds.go:144-223`): `RecentWatchdogOps(limit,
  "synthesize_")`. Failures filter `name==synthesize_error||err!=""`
  (see writer mismatch above).
- `runDoctorSecurityChecks` (`cmd/mpm/main.go:1433-1479`): counts
  `synthesize_error` in last 50; missing/read-fail → WARN.
- `getRecentWatchdogEvents(dm,3)` (`cmd/mpm/handlers_status.go:528-565`):
  direct tail, keeps only `op!=""`, rendered as Recent events in
  `mpm status` (`:122,335-341,368-409`).
- MCP/tool: none direct. `query_audit_log` reads `system_audit_log`,
  not watchdog. Two tool comments mention watchdog but code calls
  `LogAudit` (audit table).
- Human: `tail`/`jq`, `mpm status`, `mpm synthesize status|failures`,
  `mpm doctor`.

### 4.3 CLI / MCP / recovery / tests / docs index

- CLI: `mpm ops logs rotate [--threshold N] [watchdog|mirror]`,
  `mpm ops logs status`; `mpm memory wipe --force` deletes mirror
  (mislabeled "All memories wiped"); `mpm status`, `mpm synthesize
  status|failures`, `mpm doctor`. No `mirror cat/tail` command.
- MCP/tool paths into mirror: `mpm_memory save` →
  `handleSaveToMemory` → `AddMemoryWithWeightForInvocation` →
  DB + `appendToMirror` (allow-list gated). Other epistemology paths
  via `SaveMemoryNode` reach DB; mirror only via wrapper.
- Recovery: no replay. Loss/truncation loses audit trail only; DB
  unaffected. Missing file is not an error anywhere (read → nil,
  rotate → nil/skipped, write → `O_CREATE`). Corrupt lines skipped.
  `Close()` joins `mirrorWG` with 2s timeout (`db.go:3190-3202`).
  No feature depends on historical rotations (verified by absence of
  rotation readers; only live-file tail + status sizes).
- Tests: `mirror_collection_test.go` (allow-list pin),
  `f4_mirror_credential_leak_regression_test.go` (5 tests: digest-only,
  never raw/prefix), `log_rotate_test.go` (rotate + threshold +
  watchdog end-to-end), `handlers_logs_test.go` (paths, rotate, status),
  `watchdog_read_test.go` (prefix, limit, missing), `fileperms_test.go`
  (0600 tightening), `canonical_layout_test.go`,
  `release_pass_20260914_runaway_safeguard_test.go` (bounded-safeguard
  watchdog). Install/uninstall tests pin co-location and shred targets.
- Docs: `SPEC.md:2041` (blocked → mirror never DB), `:2606`
  (flat-file telemetry moved to cognitive surface), `:3824/:3834`
  (reject + mirror), `:2494` (watchdog telemetry list);
  `audit.go:4-7` (audit table is queryable counterpart);
  `ROADMAP.md` telemetry section (no mirror/watchdog mentions —
  deliberate separation); `CONFIGURATION.md` none.

## 5. Measurements (aggregate-only, no bodies)

Security note: scripts emitted aggregates and structural info only.
Old mirror records may predate credential-leak fixes; no record bodies,
secrets, or memory content were printed. One rotation
(`mirror.jsonl.20260817-110001.gz`) is not a valid gzip
(`BadGzipFile`) and was skipped; its presence is noted for the
tolerant-reader requirement.

Corpus: live `src/db/*.jsonl` + `pre-install-snapshot/` +
`backups/recovered-2026-10-03/jsonl-rotations/` (63 mirror + 99
watchdog rotations, 20260706–20261002, ~88 days).

Sizes: live mirror 316 KiB / 303 records; live watchdog 2.2 MiB /
11,564 records. Rotations compressed: mirror ~78.3 MB, watchdog
~26.3 MB (total ~100 MB with both). Full sweep uncompressed: mirror
~331 MB / 111,799 records (avg 2962 B/line); watchdog ~518 MB /
2,423,679 records (avg 214 B/line).

### 5.1 mirror: event-type distribution (samples)

- Live (303): `memories` 171, `changelog` 41, `theories` 32,
  `contradiction_detected` 59, blocked 0, embedding_failure 0.
- Rotation sample (3 files, 6428 records): `memories` 2532,
  `changelog` 1888, `decisions` 649, `session` 560 (historical
  collection, not in current allow-list), `theories` 349,
  `contradiction_detected` 445, blocked 2 (digest-only post-fix),
  `facts`/`events`/`directives` 1 each (drift).
- Pre-install (28): `memories` 24, `theories` 2, contradiction 2.

### 5.2 mirror: field frequency and drift

- Full-record fields (`id, content, metadata, tags, source,
  collection, weight, retrieval_priority, importance, confidence`)
  present on ~93–100% of memory records depending on generation.
- `embedding` present on 69.6% overall (77,768/111,799); 92.5% in
  rotation sample; 82.1% pre-install; 8.3% live (recent generation
  writes fewer embeddings — still full content on 80.5% live).
- Schema drift across generations: `created` (4216) vs `created_at`
  (1765) in sample; `timestamp`/`event`/`evidence`/`memory_id` on
  contradiction records; legacy `content_snippet` on 2 pre-fix blocked
  records (still in archive, must remain unread as bodies).
- Duplicate IDs: 279/6426 (4.3%) in sample; unique IDs 6147/6426.
  Content-hash sample (last 2 rotations, 1860 with content): 360 hash
  groups, 88 duplicate groups — revisions/re-saves of same content.

### 5.3 mirror: signal vs ballast

- Embedding JSON bytes ~233 MB / 70.4% of uncompressed mirror bytes.
- `content` bytes ~92 MB / 27.7%.
- Together ~98% of mirror bytes are full cognitive content or
  embeddings. Operational envelope (ids, timestamps, ops, collections)
  is ~2%.
- Avg content length: 412 B (rotation sample), 633 B (live),
  1873 B (pre-install). Avg embedding dim: ~292–351 floats.
- Verdict: ballast ≈ 98% by bytes under HITL lens. The 2% (op + id +
  collection + timestamp) plus a bounded deterministic preview is the signal.

### 5.4 watchdog: ops distribution

All-time (2,423,679): `exec` 1,222,197 (50.4%), `query` 625,524,
`queryrow` 317,560, `query_row` 252,719, `synthesize_skip` 5283,
`synthesize_failed` 34, `synthesize` 2. Live (11,564): `exec` 5242,
`query` 3371, `query_row` 1605, `queryrow` 1345, no synth ops.
Pre-install (2062): same SQL mix, no synth ops.

- slow (`slow:true`, >100ms): 1121 all-time (0.046%); live 2 (0.02%).
- errors (`error`/`err` set): 8898 (0.37%); live 12 (0.10%).
- `retries>0`: 920 (0.038%); live 2. `busy/locked` text: 22 all-time.
- Unique normalized (op + literal-stripped query prefix) shapes:
  ~189 (live), ~61 (pre-install), ~210 (rotation sample). Top shapes:
  identity-hash SELECT, `INSERT INTO memories`, work_events INSERT,
  confidence_history INSERT/UPDATE — i.e. a few hundred shapes cover
  millions of lines.
- Verdict: ~99.5% of watchdog lines are fast successful SQL with no
  continuing HITL value. Signal is errors, slow, retries/busy,
  synthesis outcomes, and lifecycle events (the latter currently
  absent from watchdog and living in audit table / scheduler state).

### 5.5 Duplication in SQLite

- `system_audit_log` (live read-only count): 673 rows (20 info,
  653 warn) vs watchdog live 11,564 lines and 2.4M archived. Audit log
  is curated anomalies + deliberate mutations with 30-day TTL
  (`schema.go:927-963`, `audit.go:377`); watchdog is per-statement
  trace. Overlap is intentional but asymmetric: synthesis outcomes are
  dual-written (`synthesis_auto.go:528-597` `LogAudit` + `:620`
  `logWatchdogOp`); blocked attempts dual-write (mirror digest +
  audit row); SQL successes exist only in watchdog.
- `memories` table (live 131 rows) vs mirror live 303 records:
  mirror holds history (revisions + 59 contradiction events), not just
  current rows. Every mirrored memory `id` is trivially re-queryable
  from `mpm.db`; the file adds no authoritative state.
- `shared.contradiction_log` is operator source of truth; mirror
  contradiction lines are fallback/forensic (`contradiction_log.go`).

## 6. Proposed mirror.jsonl

Role: concise human-readable journal of meaningful cognitive/substrate
changes. Answers: "what did MPM recently learn, decide, revise, or
reject?"

Include (only these):

- `memory_created` (allow-listed collections only; keep current
  allow-list, revisit `session`/`facts`/`events` exclusion — historical
  `session` 560 lines had no allow-list standing and no reader)
- `memory_revised` / `memory_superseded` (when revision/supersession
  plumbing emits them; do not synthesize from re-saves)
- `memory_weakened` / `memory_reinforced` only on threshold-crossing
  transitions, not every confidence tick (confidence_history ticks are
  telemetry candidates)
- `decision_created` / `decision_changed`
- `theory_created` / `theory_changed`
- `lesson_created` (currently unmirrored; include as event-only line —
  lessons live in own table and were excluded for bloat, but a
  bloat-free event line restores visibility)
- `contradiction_detected` / `contradiction_resolved`
- `work_lifecycle` only for useful transitions (created/completed/
  abandoned; not every `work_events` row — 439+ live lines collapse to
  a handful)
- `destructive_operation` (wipe/purge/archive with scope + count, never
  content)
- `blocked_attempt` (digest-only, existing F-4 shape)
- `embedding_failure` (existing shape, already content-free)

Exclude: `embedding` arrays always; full `content` always; full
`metadata`/`tags` dumps (keep counts or top-tag only if needed);
`retrieval_priority`/`importance`/`confidence` numeric churn;
`score`, `weight` internals; per-tick reinforcement counts;
high-cardinality machine fields.

Minimum fields per line (v2):

```json
{"v":2,"ts":"...","op":"memory_created","id":"mem-...","collection":"memories","source":"claude_code","preview":"bounded deterministic excerpt ≤280 chars"}
```

- `v`: format version (new). Old rotations have no `v` (treated as v1).
- `ts`: RFC3339 UTC.
- `op`: bounded enum from include-list above.
- `id`: artifact id (`memory_id` unified to `id`).
- `collection`: collection or table name.
- `source`: framework/source label (existing `source`).
- `preview`: deterministic human-readable hint, NOT an LLM summary
  (see Preview contract below). Secret-scanned before write.
- Optional: `digest` (content sha256 short, for correlation),
  `reason` (for blocked/contradiction/resolved transitions).
- Explicitly absent: `embedding`, `content`, full `metadata`.

### Preview contract (deterministic, no LLM)

There is no semantic-summary layer for logging. No new derived
representation, no drift source, no second sensitive copy. The field is
named `preview` (not `summary`) to make that explicit.

Construction, in priority order:

1. If the originating operation already carries a concise human label
   (e.g. decision title, lesson title, work title, contradiction
   reason), use that verbatim after secret-scan, truncated to the bound.
2. Otherwise use a sanitized bounded excerpt of the stored content:
   first N printable characters with newlines/controls collapsed to
   spaces, truncated at a word boundary, suffixed with `…` when cut.
3. Never invoke an LLM, embedding model, or any semantic rewrite to
   produce `preview`. Byte-deterministic given the same input
   (modulo scanner version, which is logged nowhere per-record).

Bounds: 160–280 characters (default cap 240; hard max 280). Short
enough for `tail`, long enough to disambiguate. Pinned by test.

Secret handling: `preview` passes through the same secret/poison
scanner as the save path BEFORE write. On scanner hit the write
follows the blocked path instead: no excerpt at all — only the
structural `blocked_attempt` record (`pattern_family`,
`content_sha256`, `content_length`, `reason`, `type`). A blocked or
sensitive operation MUST NOT leave its rejected text in `preview`,
`reason`, or any other HITL field. Rationale: the log is for a human
with a terminal, not another memory system; a sanitized excerpt is
enough, and anything the scanner rejects must not appear anywhere.

Full-content question settled: HITL value does not justify storing
content again. `preview` + `digest` + `id`/`collection` (re-queryable
from `mpm.db`) replaces it. Terminal readability (≤280-char lines vs
3 KB lines), no duplicate corpus, ~90% size cut (see §10).

## 7. Proposed watchdog.jsonl

Role: concise operational black-box log for humans diagnosing MPM.
Answers: "is MPM healthy, and if not, what went wrong?"

Retain (only these):

- `error` (any `ExecTracked`/`QueryTracked` failure with truncated
  operation + error, no secret/value echo)
- `warn` (explicit warn paths)
- `busy`/`retry` (SQLite busy/lock/retry with attempts + backoff)
- `slow` (duration > threshold, with truncated normalized query)
- `migration` (start/finish/error per migration id)
- `fts_recovery` / `fts_error`
- `scheduler_startup` / `scheduler_shutdown` / `scheduler_drain_anomaly`
- `db_open` / `db_checkpoint` / `db_recovery`
- `service_lifecycle` (start/stop/pair/restart anomalies)
- `integrity_failure` / `health_failure`
- `synthesize` / `synthesize_skip` / `synthesize_failed` (keep existing
  synthesis DLQ role; rename `synthesize_error` readers to
  `synthesize_failed` or keep both as aliases — resolves §3.2 mismatch)
- `unusual_state_transition`

Successful fast SQL operations: omit. They are the 99.5% ballast.
Do not move them to telemetry in this task; tag them as future
telemetry candidates. If sampling/aggregation is later wanted, it
belongs in `telemetry.db` (counts, histograms, per-shape stats), not
in a human tail.

Minimum fields (v2):

```json
{"v":2,"ts":"...","level":"error","op":"exec","duration_ms":12,"sql_shape":"INSERT INTO memories (...)","detail":"bounded human text, no secrets","error":"..."}
```

- `v`: 2. `level`: `info|warn|error` (lifecycle) — errors/warns only
  except lifecycle/migration which may be info. `op`: `exec|query|
  query_row|busy|slow|migration|scheduler|synthesize|...` (`queryrow`
  spelling retired; v1 `queryrow` accepted on read, never written).
  `duration_ms` where relevant. `sql_shape`: normalized SQL shape only
  (see SQL secrecy contract). `detail`: bounded (≤280 chars),
  secret-scanned, never echoes values. No raw `query` field in v2;
  truncated normalized shape lives in `sql_shape` and only on
  slow/error/retry records.
- No change to `slowQueryThreshold` (100ms) in this design; revisit
  after observing retained slow rate.

### SQL secrecy contract (hard invariant)

On every watchdog slow/error/retry line:

- NEVER log SQL argument values (bound parameters, interpolated
  literals,.DO NOT include `?` expansions).
- NEVER log interpolated sensitive literals (tokens, keys, secrets,
  PII pasted into content — these travel as bound values, and bound
  values never reach the log).
- NEVER log database row content (no `content`, no column values, no
  `SELECT` result samples).
- Record ONLY the normalized SQL shape: statement type + tables +
  truncated structural skeleton with all literals replaced by `?`
  (same literal-stripping used for measurement in §5.4), capped at
  120 chars. Plus operational metadata (`op`, `duration_ms`,
  `retries`, `error` string with values redacted at the source).

This is a security invariant on par with F-4: without it the
redesigned watchdog recreates the "secondary sensitive datastore"
problem this redesign removes. Pinned by a dedicated sentinel
regression test (fake `sk-ant-…`, `ghp_…`, Bearer tokens as bound
values and interpolated literals must not appear in output; only the
`?`-normalized shape may). See §17.

## 8. Audit-log division

Intended division:

- DB `system_audit_log` = authoritative, queryable operational history
  (30-day TTL, indexed by level/component/time/invocation/event_code,
  MCP `query_audit_log`, `mpm audit`, wake/cluster joins on
  `invocation_id`). Curated anomalies + deliberate mutations.
- `watchdog.jsonl` = immediate human-readable surface (tail-first,
  last-N events in `mpm status`, doctor checks). Errors/warns/slow/
  lifecycle only, bounded lines, no indexes needed.
- `mirror.jsonl` = immediate human-readable cognitive surface. Event
  lines only; full state stays in `mpm.db` tables.

Rule: do not record the same rich event twice unless there is a clear
reason. Current justified dual-writes: synthesis outcome (DB queryable
+ file tail), blocked digest (forensic file + queryable audit).
Unjustified duplication to remove: every successful SQL statement in
watchdog (exists as normal DB operation; audit log deliberately does
not record it — watchdog should not either).

## 9. Retention and rotation (combined size-OR-age + age-expiry + count cap)

Design around HITL purpose (recent visibility), not indefinite
accumulation. Size-only rotation fails after the volume reduction, so
rotation is SIZE ≥ threshold OR AGE ≥ max-active-age, and expiry is
MAX-AGE with COUNT as a safety bound. All three are enforced behavior,
not advisory text.

### 9.1 Rate arithmetic (from §5 measurements)

Observed (88-day window, 20260706–20261002):

- mirror: ~1270 records/day, ~3.8 MB/day uncompressed.
- watchdog: ~27.5k records/day, ~5.9 MB/day uncompressed.

Projected post-redesign (event-only mirror ~300 B/line at hundreds of
events/day; anomaly-only watchdog ~200 B/line at dozens–hundreds/day):

| stream | proj. writes/day | proj. bytes/day | 5 MiB by size alone | 2 MiB by size alone |
|---|---|---|---|---|
| mirror | ~330–660 (100–200 KB) | ~150 KB/d mid | ~35 days | ~14 days |
| watchdog | ~50–250 (10–50 KB) | ~30 KB/d mid | ~175 days | ~68 days |

At these rates size-only rotation takes weeks (mirror) to months
(watchdog): too slow to keep the active file fresh and bounded. Hence
the age trigger.

### 9.2 Policy

Shared rotation helper enforces, per stream (see §12B):

- Rotate the active file when `size ≥ SIZE_THRESHOLD` OR
  `active age ≥ MAX_ACTIVE_AGE` (age from file mtime at write time;
  checked on every append and at startup).
- Compress rotated file to gzip, `0600`, truncate original.
- Expire rotations by MAX_AGE (delete files older than MAX_AGE).
- Cap COUNT (delete oldest beyond COUNT newest) as a safety bound for
  abnormal bursts; normally age binds first.

| stream | SIZE_THRESHOLD | MAX_ACTIVE_AGE (rotate) | MAX_AGE (expire, enforced) | COUNT cap (safety) |
|---|---|---|---|---|
| mirror | 5 MiB | 30 days | 365 days (enforced delete) | 12 newest |
| watchdog | 2 MiB | 30 days | 90 days (enforced delete) | 5 newest |

Derivation:

- mirror at ~150 KB/d: age binds (≈4.5 MB per 30-day rotation, just
  under 5 MiB; size binds only on burst weeks). 12 monthly rotations ≈
  365 days ≈ 54 MB worst case — matches the 365-day cognitive-audit
  window with a bounded disk cost (vs 78 MB compressed today for 88
  days of snapshots).
- watchdog at ~30 KB/d: age binds (≈0.9 MB per 30-day rotation, well
  under 2 MiB; size binds only on incident storms). 90-day expiry
  keeps ≈3 rotations (≈2.7 MB); COUNT 5 never binds in steady state
  and caps storm bursts at ≈10 MB. Aligns with the 30-day audit-table
  TTL while keeping a 3× file window for `tail` forensics.
- Active-file dwell: mirror ≈30 days / ≤5 MiB; watchdog ≈30 days /
  ≤2 MiB — both stay `tail`/`jq`-friendly regardless of how small the
  daily volume becomes.

Pre-change behavior is unchanged (5 MiB size-only); the age trigger,
expiry deletion, and count cap land with the v2 writers (phase E,
§16). `mpm ops logs rotate` (force) and `status` (sizes + ages) stay
format-agnostic.

### 9.3 Recovery value split (unchanged)

Old mirror rotations: low recovery value (no replay consumer), some
forensic value (pre/post-fix boundary). Old watchdog rotations:
near-zero beyond the last few (fast-SQL trace). Hence asymmetric
MAX_AGE (365d vs 90d).

## 10. Performance

Current write amplification (measured):

- mirror: ~2962 B/write avg (full object + embedding). Embedding JSON
  ~70% + content ~28%. Every `AddMemory` pays JSON marshal of floats
  (~300 floats), file open/append, no rotation check (growth until
  next tracked DB op). Contradiction path pays extra rotation stat.
- watchdog: ~214 B/write avg but ~27.5k writes/day — i.e. per-statement
  file open/append + rotation stat under `watchdogMu`. Lock contention:
  shared `watchdogMu` serializes all watchdog + mirror-async writes;
  at current volume this is a per-statement mutex + stat + append.
  No measured stall in this task (no live benchmarks per constraints),
  but write count dominates cost, not line size.
- Rotation: read-all + gzip + truncate per threshold breach; CLI force
  same path. Corrupt-rotation case shows need for atomic-write
  hardening later (not this task).

Proposed removal:

- mirror: drop embedding (~70%) + full content (~28%) → ~250–350 B/line
  (~90% bytes/write removed). Fewer lines if revision ticks excluded.
- watchdog: drop ~99.5% of lines (fast successes) → write count falls
  from ~27.5k/day to ~100/day. Per-write cost unchanged, total cost
  falls ~99%. `watchdogMu` contention effectively disappears.
- Net archive growth: from ~9.6 MB/day uncompressed to ~0.1–0.25 MB/day
  (~97–99% reduction). No premature optimization; savings fall out of
  HITL scoping.

## 11. Security / privacy model

- Local-only by default; directory `0700`, files `0600` (existing
  `fileperms.go:15,65-68`; all opens use `0600`). Preserved.
- No credential leakage: F-4 invariant (proven by
  `f4_mirror_credential_leak_regression_test.go`, 5 tests) — blocked
  content stored as `{pattern_family, content_sha256, content_length}`,
  never raw/prefix/suffix. HITL design keeps this shape as the only
  blocked representation.
- Minimal duplicate cognitive content: full `content` removed from
  mirror; `preview` per the §6 contract (deterministic, ≤280 chars,
  prefer existing label else sanitized excerpt, secret-scanned
  pre-write, never LLM-generated). No embeddings (non-human-readable,
  large, and a second copy of sensitive geometry). Blocked/sensitive
  input yields NO excerpt — structural record only.
- No secret/value echo in diagnostics: watchdog `sql_shape`/`detail`/
  `error` obey the §7 SQL secrecy contract (normalized shape only,
  never bound values, interpolated literals, or row content).
  Synthesis failure path already excludes secrets
  (`synthesis_auto.go:476-482`) — keep and extend the same guarantee
  to every watchdog writer via the shared path (§12B).
- Blocked HITL record (existing, retained):
  `{timestamp, reason, pattern_family, content_sha256, content_length,
  action:"blocked", type}` — structural info, correlatable by hash
  holder, no rejected content.
- Historical rotations contain pre-fix `content_snippet` (2 records
  observed) and full content pre-dating fixes: leave untouched,
  treat as sensitive (already shred targets in
  `scripts/tests/test_uninstall.py`), never print bodies.

## 12. Backward compatibility

- Leave old rotations readable as historical format (v1, no `v` field).
  No rewriting, no mass migration (already preserved under
  `backups/recovered-2026-10-03/`; live rotations stay alongside).
- Version new records with `"v":2`. Readers tolerate mixed generations:
  `RecentWatchdogOps` skips unparseable lines; prefix filters match
  both schemas (`operation` vs `op`); `Stats` counts lines regardless.
  New CLI/log code must accept missing `v` as v1 and missing
  `operation`/`op` variants (existing inconsistency `queryrow` vs
  `query_row` must be accepted, not "fixed" by rewriting history).
- `mpm ops logs rotate|status` work on sizes/paths, format-agnostic —
  no change needed for mixed generations. `status` additionally shows
  active-file age once MAX_ACTIVE_AGE exists (phase E).
- v2 canonical spellings: `synthesize_failed` (never
  `synthesize_error` in new output; v1 `synthesize_error` accepted on
  read as an alias — see §12); `query_row` (never `queryrow` in new
  output; v1 `queryrow` accepted on read).

## 12A. Destructive semantics (v2, intentional — not inherited)

v2 HITL history is privacy-sensitive even without content: ids,
sources, excerpts/`preview`s, and digests can leak what was known and
when. Destructive behavior is therefore specified, not left as an
accident of `os.Remove` placement.

Audited current behavior (must change where noted):

- `MemoryStore.ClearMirror` (`memory.go:2347-2350`): `os.Remove` of the
  ACTIVE mirror file only. Rotations, watchdog (active + rotations),
  and `mpm.db` untouched.
- `mpm memory wipe --force` (`handlers_memory.go:973-994`): calls only
  `ClearMirror`, then prints "All memories wiped." — misleading: the DB
  is untouched, rotations survive, watchdog survives. Accidental scope.
- Per-id `memory shred` / `ShredMemoryWithCascade`
  (`memory_tools.go:632`, `handlers_memory.go:735`): DB hard delete +
  cascade only. Mirror history (including the shredded content's prior
  lines and rotations) is NOT touched. Watchdog untouched.
- `uninstall.sh --purge`: removes `$DATA_ROOT/src/db` (active files +
  rotations under it), `backups`, config/mode/persona/migrations —
  complete for HITL files. `--shred`: same scope with best-effort
  `shred -f -n1 -z -u` over every regular file under `src/db`,
  `backups`, `logs` first (`uninstall.sh:399-435,597-613`) — complete.

v2 specified behavior:

| operation | active mirror | mirror rotations | active watchdog | watchdog rotations | note |
|---|---|---|---|---|---|
| `memory wipe --force` (corrected scope) | cleared | cleared (deleted) | appended `destructive_operation` line (scope+count, no content) | retained | fixes the false "All memories wiped" — wipe covers DB scope + mirror history; watchdog keeps the tombstone for forensics |
| `memory shred <id>` (per-id) | append `memory_shredded` event (`id`, collection, no excerpt) | NOT rewritten (immutable) | append `memory_shredded` line | NOT rewritten | document explicitly: rotations keep history until expiry/purge; operator uses `ops logs purge` for early removal |
| `ops logs purge --scope mirror\|watchdog` (new, phase E) | truncate | delete per scope | truncate | delete per scope | explicit rotation removal; no DB touch |
| `uninstall --purge` | removed with `src/db` | removed | removed | removed | unchanged, complete |
| `uninstall --shred` | shredded then removed | shredded then removed | shredded then removed | shredded then removed | unchanged, complete |

Rules: tombstone/marker lines carry scope + counts + ids only, never
excerpts or content. Per-id shred never rewrites compressed rotations
(immutability + cost); this limitation is user-visible in `--help` and
in the shred success message. `memory wipe`'s success message is
corrected to state the actual scope (DB + mirror history cleared,
watchdog tombstone written).

## 12B. Shared rotation/append path (centralized)

The audit found rotation fragmented: `ChallengeMemoryAsync` owns an
inline mirror rotation (`db.go:5294` under shared `watchdogMu`) while
`appendToMirror`/`appendBlockedAttempt` never rotate, and watchdog
writers rotate on every write. v2 requires ONE shared append path per
HITL stream so every writer gets identical behavior:

- single function per stream (e.g. `appendMirrorLine(entry)`,
  `appendWatchdogLine(entry)`) owning: `0600` open/append, size-OR-age
  rotation check (§9.2), gzip + truncate, MAX_AGE expiry + COUNT cap
  enforcement (amortized, not necessarily every line), secret-scan gate,
  `sql_shape` normalization (watchdog), and error behavior.
- `ChallengeMemoryAsync`'s bespoke rotation is deleted and routed
  through the shared path (keeps its detached-goroutine + `mirrorWG`
  drain semantics; loses its special-case lock handling — the shared
  path owns the mutex discipline).
- Log failure stays non-fatal to substrate operations (warn + continue,
  existing semantics preserved) unless a caller already treats it as
  fatal — no caller is upgraded to fatal in v2.
- Phase A builds and pins this infrastructure before any v2 writer
  lands (§16); no v2 writer may open the JSONL path directly (pinned
  by linter/test: grep for `mirror.jsonl`/`watchdog.jsonl` opens
  outside the shared path fails).

## 12C. Stats() finding

`MemoryStore.Stats()` (`memory.go:1462-1488`) counts non-empty mirror
lines into `stats["mirror_entries"]` and exposes `mirror_file`. A
repository-wide search found NO CLI/MCP/status/doctor consumer of
`mirror_entries` or `mirror_file` (only the unrelated
`synth.Plan.Stats()` matches) — the count is diagnostic-only today.
Under v2 the same line count silently changes meaning from "memory
snapshots" to "journal events" (plus contradiction/blocked/tombstone
lines that were always mixed in).

Required implementation change (phase D): rename to `mirror_events`
(keep `mirror_entries` as a one-release alias), document the meaning
change in `--help`/status text if the key is ever surfaced, and pin
with a test (v2 fixture with N event lines → `mirror_events == N`).
Do not let dashboards imply "N mirrored memories" when the file holds
events. Watchdog needs no equivalent (no line-count stat exists).

## 13. Telemetry boundary

Do not retain in mirror/watchdog merely because it might become
analytically useful later. That is telemetry's job.

| information | mirror | watchdog | telemetry | mpm.db/audit |
|---|---|---|---|---|
| cognitive change (event line) | YES (event + preview) | no | aggregate/count | YES (state) |
| full content | NO (preview+digest only) | no | no (ref by id) | YES (memories) |
| embedding | NO | no | no (or ref) | YES (vector store) |
| fast successful SQL op | no | NO (omit) | CANDIDATE (counts/histograms) | no |
| SQL error | no | YES | CANDIDATE (rates) | YES if curated (audit warn/error) |
| retry/lock | no | YES | CANDIDATE | YES if anomaly |
| slow operation | no | YES | CANDIDATE (histograms) | no (unless incident) |
| retrieval score | no | no | YES | no |
| framework/model analytics | no | no | YES | provenance cols only |
| cost/token information | no | no | YES | no |
| scheduler failure | no | YES (anomaly line) | YES (rates) | YES (audit) |
| provenance analytics | no | no | YES | YES (provenance tables) |

YES = lives there. CANDIDATE = future telemetry may capture; do not add
to HITL files now.

## 14. User-facing examples (proposed, synthetic data)

```bash
tail -f ~/.mpm/src/db/mirror.jsonl
jq 'select(.op == "contradiction_detected")' ~/.mpm/src/db/mirror.jsonl
tail -f ~/.mpm/src/db/watchdog.jsonl
jq 'select(.level == "error" or (.duration_ms // 0) > 100)' ~/.mpm/src/db/watchdog.jsonl
```

Representative proposed mirror records (synthetic, human-readable):

```json
{"v":2,"ts":"2026-10-03T12:00:01Z","op":"memory_created","id":"mem-a1b2","collection":"memories","source":"claude_code","preview":"Prefers Postgres advisory locks for singleton scheduler","digest":"9f2c…"}
{"v":2,"ts":"2026-10-03T12:05:44Z","op":"decision_created","id":"dec-c3d4","collection":"decisions","source":"opencode","preview":"Adopt event-only mirror lines; drop embeddings and full content","digest":"41ab…"}
{"v":2,"ts":"2026-10-03T12:09:10Z","op":"contradiction_detected","id":"mem-a1b2","collection":"memories","source":"mpm","preview":"Conflicts with mem-e5f6 on lock strategy","reason":"evidence: design note 2026-10-02"}
{"v":2,"ts":"2026-10-03T12:11:02Z","op":"lesson_created","id":"les-77aa","collection":"lessons","source":"claude_code","preview":"FTS rebuild needs triggers re-armed after snapshot restore"}
{"v":2,"ts":"2026-10-03T12:12:30Z","op":"blocked_attempt","id":"n/a","collection":"n/a","source":"claude_code","preview":"","reason":"blocked: github_token","digest":"sha256:…"}
```

(`preview` is the existing operation label where available, else a
sanitized bounded excerpt — never LLM-generated. Blocked lines carry no
excerpt at all.)

Representative proposed watchdog records (synthetic; `sql_shape` is
normalized, values never appear):

```json
{"v":2,"ts":"2026-10-03T12:00:02.123Z","level":"error","op":"exec","duration_ms":3,"sql_shape":"INSERT INTO memories (...)","detail":"INSERT INTO memories failed","error":"constraint failed: ..."}
{"v":2,"ts":"2026-10-03T12:01:00Z","level":"warn","op":"busy","detail":"SQLITE_BUSY retry 2 backoff 200ms","error":"database is locked"}
{"v":2,"ts":"2026-10-03T12:02:10.5Z","level":"warn","op":"query","duration_ms":212,"sql_shape":"SELECT ... FROM memories WHERE ...","detail":"slow query","error":""}
{"v":2,"ts":"2026-10-03T12:03:00Z","level":"info","op":"migration","detail":"migration 0042_identity start"}
{"v":2,"ts":"2026-10-03T12:04:00Z","level":"error","op":"synthesize_failed","detail":"synthesize save failed for staging id …","error":"save failed: ..."}
```

Each line answers one question in seconds: what was learned, what
conflicts, what failed, what was slow, what migrated.

## 15. Migration strategy

No mass migration, no historical rewrite. Steps when implementation is
approved (not this task):

1. Shared infrastructure first (§12B): per-stream append path with
   `0600`, size-OR-age rotation, gzip, expiry+cap, secret gates, SQL
   normalization; `ChallengeMemoryAsync` re-routed through it.
2. Reader aliases: `synthesize_error` ≡ `synthesize_failed` (read both,
   write canonical only), `queryrow` ≡ `query_row` (read both, write
   `query_row`).
3. Mirror event-only cutover (drop embedding/content; `preview` per §6
   contract; lesson event; work-lifecycle throttle; destructive
   tombstones per §12A).
4. Watchdog anomaly-only cutover (omit fast success; `sql_shape` per §7
   contract; keep slow/error/retry/lifecycle/synth).
5. Retention enforcement (phase E): age triggers, expiry deletion,
   count caps per §9.2; `ops logs status` shows ages; observe one
   window and confirm counts.
6. Docs: update SPEC flat-file passages and `audit.go:4-7` counterpart
   note to the v2 contract; correct `memory wipe` messaging.

## 16. Implementation phases (A–F, separable, each independently reviewable)

- A. Shared HITL log/rotation infrastructure: append paths, size-OR-age
  rotation, gzip, expiry+cap, perms, secret gates, SQL normalizer,
  non-fatal error semantics + regression tests. No writer changes.
- B. Mirror v2 writers: event enum, `preview` builder (deterministic,
  secret-scanned, truncation-bound), digest/reason, blocked path,
  tombstones + tests. No watchdog changes.
- C. Watchdog v2 filtering/schema: anomaly-only gating, `sql_shape`
  contract, sentinel-secret regression, lifecycle events + tests.
  No mirror changes.
- D. Readers/status compatibility: `synthesize_error`/`queryrow`
  aliases, mixed v1/v2 reads, `Stats()` → `mirror_events` rename with
  alias, status/doctor rendering + tests.
- E. Retention/privacy cleanup: age triggers, enforced expiry/caps,
  `ops logs purge --scope`, corrected wipe/shred messaging + tests.
- F. Docs + final design archival: SPEC/audit-note updates, telemetry
  handoff notes (candidates only — do not build telemetry).

## 17. Test matrix (future)

- Allow-list pin (extend `mirror_collection_test.go` with lesson event,
  `session` exclusion).
- F-4 regression extended to `preview` builder: determinism (same input
  → same output), truncation bound (160–280, default 240 / max 280),
  no secret passthrough,Blocked-input → structural record only, no
  excerpt anywhere.
- SQL secrecy sentinel regression (§7 contract): seed bound values and
  interpolated literals containing `sk-ant-…`, `ghp_…`, Bearer tokens,
  private-key blocks; force slow + error paths; assert NONE appear in
  watchdog output and only `?`-normalized `sql_shape` does. Covers
  `Exec/Query/QueryRowTracked` (tx + standalone), busy/retry, and
  synthesis-failure detail paths.
- Shared-path pin: no direct JSONL opens outside `appendMirrorLine` /
  `appendWatchdogLine` (grep-gate); perms `0600`/`0700`; rotation
  size-OR-age triggers; expiry + count-cap enforcement; corrupt-line
  skip; missing-file nil; `queryrow` tolerance; `synthesize_error`
  alias; mixed v1/v2 reads.
- Volume: assert fast-success omission (no success `exec`/`query` lines
  by default); slow/error/retry retained with `sql_shape` and no value
  echo.
- Stats: `mirror_events == N` on v2 fixtures; `mirror_entries` alias
  present one release; no "mirrored memories" labeling anywhere.
- Destructive: wipe clears DB scope + active mirror + mirror rotations
  and writes watchdog tombstone (message asserts scope); per-id shred
  appends `memory_shredded` without touching rotations (test asserts
  rotation immutability + help-text disclosure); purge --scope removes
  only scoped files.
- Perms + docs tests: design path lint, no live-DB fixtures, no
  telemetry build.

## 18. Open questions (remaining after Amendment A)

Settled by Amendment A: `summary` → deterministic `preview` (no LLM,
160–280, default cap 240); SQL shape-only as hard contract with
sentinel test; retention as enforced size-OR-age + age-expiry + count
cap (mirror 5 MiB/30d/365d/12, watchdog 2 MiB/30d/90d/5); synthesis
canonical `synthesize_failed` with v1 alias; `query_row` canonical with
v1 alias; `Stats()` → `mirror_events` rename.

1. Should `memory_weakened/reinforced` threshold be confidence-delta,
   crossing named bands, or omitted entirely (telemetry-only)?
2. Should `work_lifecycle` include `in_progress` transitions or only
   terminal ones (completed/abandoned)?
3. Watchdog `slowQueryThreshold` 100ms: keep, or raise to reduce
   retained slow volume after fast-success removal?
4. Should `lesson_created` event line include originating session id?
   (Useful for grep, low cardinality — likely yes.)
5. `ops logs purge --scope` confirmation UX (prompt vs `--force`) and
   exact help wording for the rotation-immutability disclosure.
6. `preview` default cap within 160–280: 240 proposed — confirm against
   real `tail` readability in phase B review.

## 19. Explicit non-goals (restated)

No telemetry.db implementation; no analytics; no telemetry schema
changes; no web UI; no deletion or rewriting of historical logs; no
live-DB modification; no writer or rotation behavior changes in this
design task.
