# OpenCode + MPM dogfood — day-zero readiness and seven-day measurement plan

> **Prepared:** 2026-10-30. **Scope:** Session C — behavioural evaluation.
> **Revised:** 2026-10-30, after Session A's runtime upgrade landed.
> **Status:** the plan is ready; **the evaluation has NOT been started**.
>
> Every number in this document was measured on the live substrate on
> 2026-10-30 and can be reproduced with the commands shown. Nothing here is
> estimated unless it says so.
>
> **Two runtimes are in play in this document.** The plan was originally
> written against the deployed binary `g3df72315` (revision `3df72315`). A
> controlled runtime upgrade replaced it **during the preparation of this
> plan**, at 2026-10-30T16:32:42Z, with revision `3d63244e`. Measurements
> taken before the upgrade are kept as a **historical baseline** and are
> labelled as such wherever they appear. §0.4 states exactly which is which,
> and every version-specific claim in this document was checked against the
> source at the named revision rather than inferred.

---

## 0. Scope, posture, and what this document is not

### 0.1 What is being evaluated

MPM, during ordinary development, on four real projects:

| project | host directory | subject |
|---|---|---|
| **TheMailroom** | `/home/v/projects/themailroom` | a self-contained Go IMAP/SMTP implementation |
| **AgentShell** | `/home/v/projects/agent-shell-theme` | a WordPress theme with an MCP configurator |
| **UNACCOUNTED** | `/home/v/projects/Unaccounted` | — |
| **DeepClaw** | `/home/v/projects/deepclaw` | — (one session so far; joins the series when its development begins) |

The question is not "is MPM fast" or "is MPM correct". It is:

> **Does MPM measurably change what an agent knows between sessions, on
> real work, without being asked to?**

### 0.2 The posture

- **The evaluation is not started.** This document is the checklist and the
  plan, not a report.
- **The deployed installation is not modified by this plan.** The live
  install is now `3d63244e` (Session A's upgrade), with its configuration
  untouched by this plan. The day-zero verification in §1 is the evidence.
- **All measurement is read-only.** §3's queries run against a **snapshot**
  of the substrate database, produced with `sqlite3 -readonly … .backup`.
  The snapshot is the only thing queried, and the live database is never
  opened for writing at any point.
- **The operator actions in §10 are documented, not performed.** They are
  configuration and deployment changes to the live install, and they are the
  operator's to make. §10 is a **marked operator-only procedure**: nothing in
  it is executed by this plan, by the script, or by any check that reads
  this document.
- **The measurement uses the live `~/.mpm` database.** This is the
  operator's decision (§10, action 1) and it is deliberate: the experiment
  is meant to observe real cross-session recall, real scheduler behaviour,
  real persistence and real SQLite contention — including the unresolved
  reliability questions in issue #178 — which a disposable workspace cannot
  reproduce.

### 0.3 What this document is not

It is not a second analytics subsystem. There is no new schema, no new
persisted measurement, and no second place where a number is defined.

Everything is either:

- a `SELECT` against an authoritative table (the substrate's or the host's),
- the substrate's own `mpm wake --compact`, run against a snapshot, or
- one of the five counts the operator reads off a transcript (§8.3).

The one new artefact is `scripts/dogfood_baseline.sh`, which prints all of
it in one fixed format. It is a **read-only query runner**, not a system.

### 0.4 The two runtimes — which measurements belong to which

**This is the single most important correction in this revision.** The
original plan described one system; there are now two, and a reader who
conflates them will draw false conclusions.

| | **historical baseline** | **the system under test** |
|---|---|---|
| installed revision | `3df723156d7649517508212453de72d347759f43` | `3d63244e1e64955db2db243e70e279d370ebf1d6` |
| short | `3df72315` | `3d63244e` |
| version string | `0.1.0-dev+g3df72315.t1792851233` | `0.1.0-dev+g3d63244e.t1793377957` |
| built | 2026-10-24T14:13:53Z | 2026-10-30T16:32:37Z |
| installed | 2026-10-24 15:14 | **2026-10-30 16:32:42Z** |
| release record | — | `~/.mpm/releases/3d63244e…/RELEASE.json` |
| `lesson_proposals` table | **absent** | **present** |
| `mpm_lessons action=propose` | absent | present |
| agent-facing `mpm_lessons action=save` | permitted | **refused — operator-only** |
| compaction writes | a trusted lesson | a **pending proposal** |
| confidence-decay handler | **not registered at all** | registered, **default OFF** |
| managed-instruction protocol | 4.x | **5.0.0** |

**How the installed revision was verified** (not assumed from the checkout):

```bash
~/.mpm/bin/mpm version            # → commit 3d63244e…, dirty: no
cat ~/.mpm/releases/3d63244e…/RELEASE.json
md5sum ~/.mpm/bin/mpm ~/.mpm/releases/3d63244e…/bin/mpm   # identical
readlink /proc/<scheduler pid>/exe                         # → the release binary
```

The checkout at `~/.mpm` is at `3d63244e` with 119 dirty entries left by the
upgrade's own installation step. The running scheduler (pid 3890452, started
2026-10-30 16:32:42) executes the released `3d63244e` binary — verified by
`/proc/<pid>/exe`, not by the checkout.

**Rule for reading this document.** Every number carries the runtime it was
measured on. The *baseline* numbers are historical evidence and are used for
contrast. The *day-zero* numbers for the week are re-measured on
`3d63244e` by the day-zero run of `scripts/dogfood_baseline.sh`; §1 and
Appendix A state which is which for each figure.

---

## 1. Day-zero readiness checklist

Everything in this section was measured on 2026-10-30 against the live
installation. **Blocking** means the experiment does not start until it is
resolved. **Advisory** means the experiment runs but the condition is
recorded and carried into the week.

### 1.1 Blocking

| # | condition | measured | status |
|---|---|---|---|
| B1 | Substrate integrity | `PRAGMA integrity_check` → `ok` on `~/.mpm/src/db/mpm.db` (22.6 MB, WAL 4.0 MB) | ✅ pass |
| B2 | The scheduler is alive | pid 3890452 (started 2026-10-30 16:32:42), `--interval 60s`, `last_status` `ok`, exe verified as the released `3d63244e` binary. The `tick_count` series starts from a low number because the service was restarted by the runtime upgrade (§12.3) | ✅ pass |
| B3 | The substrate is the one the agent uses | the OpenCode plugin's `resolveWorkspace()` falls back to `$HOME/.mpm`; the running OpenCode processes have **no** `MPM_WORKSPACE`, so the live install **is** the substrate | ✅ pass (with the consequence in B4) |
| B4 | The workspace choice is deliberate | **decided: the live `~/.mpm` database**, after Session A's controlled runtime upgrade. Chosen deliberately so the week observes real cross-session recall, real scheduler behaviour, real persistence and real SQLite contention, including issue #178. `~/.mpm-dogfood` exists but is **not** used | ✅ pass (operator decision, recorded) |
| B5 | The credential is visible to the runtime process | `MINIMAX_API_KEY` is defined in `~/.config/mpm/mpm.env`, which the scheduler unit reads via `EnvironmentFile=-%h/.config/mpm/mpm.env`. It is in **no** shell profile (`~/.bashrc`, `~/.profile`: 0 hits) and **not** in the systemd user environment | ⛔ **partial — §10, action 2** |
| B6 | The deployed credential is not a placeholder | the deployed `ResolveAPIKey` returns the configured value **verbatim**, so `"api_key": "$MINIMAX_API_KEY"` is sent to the provider as a literal bearer token | ⛔ **§10, action 3** |
| B7 | The request timeout can carry a real generation | the live profile has `"timeout_seconds": 3`, and the provider's own median latency is 560–1007 ms with a tail to 4761 ms | ⛔ **§10, action 4** |
| B8 | Confidence decay is off | **verified per revision — see the note below** | ✅ pass (no action required) |
| B9 | The autonomous critic is gated off | `mpm-critic` is installed and the scheduler registers a `critic_audit` wake kind, but `scheduled_wakes` holds 8 rows — all `fired=1`, all `cascade tick summary` or `cron:epistemic-compaction`. **No `critic_audit` wake exists**, so the critic does not run | ✅ pass (keep it that way — §10, action 6) |

### 1.1.1 B8 — confidence decay, resolved per revision

The original plan asserted that "the old deployed scheduler runs confidence
decay with an absent configuration key" and recommended editing `mpm.env`
and restarting the scheduler to disable it. **That was wrong on both
counts, and the recommendation is withdrawn** (the former §10 action 5 is
gone; see §10).

The behaviour is **revision-specific**, and both revisions were checked in
the source rather than inferred from configuration:

| | old runtime `3df72315` | upgraded runtime `3d63244e` |
|---|---|---|
| the decay handler's source file | `internal/scheduler/confidence_decay.go` **does not exist** at this revision | exists |
| is the handler registered? | **no** — `cmd/mpm-scheduler/main.go` registers exactly three tick handlers: `cascade_drain`, `openclaw_ingest`, `cascade_wake_reconcile` | yes |
| the default when the key is absent | *not applicable — the handler does not run* | `core.DefaultConfidenceDecayEnabled = 0` → **OFF** |
| the gate | — | `dm.GetConfigInt("confidence.decay.enabled", core.DefaultConfidenceDecayEnabled)` — an absent key falls back to the constant, so **absent ⇒ OFF** |

Two independent facts, each checked at the named revision:

- the **old** runtime could not run confidence decay at all, because the
  handler was never registered — the "absent key runs decay enabled"
  claim does not describe it;
- the **new** runtime registers the handler but **defaults it OFF**, by an
  explicit design decision recorded at `internal/core/confidence_maintenance.go`
  ("The pass is OFF by default and runs only on an explicit opt-in"), so an
  absent key means *off* there too.

`system_config` holds no `confidence.decay.enabled` key on the live
database, so the upgraded runtime's decay is **off** and **stays off**
without any configuration change. **No `mpm.env` edit. No scheduler
restart. No operator action.** The live scheduler was not restarted for this
plan, and the day's `tick_count` series continues from the upgrade's own
restart (pid 3890452, started 16:32:42).

### 1.2 Advisory

| # | condition | measured | note |
|---|---|---|---|
| A1 | The telemetry collector is running | **it is not.** no `mpm-telemetry.service` in `~/.config/systemd/user/`, no socket at `~/.mpm/runtime/mpm-telemetry.sock`, no `~/.mpm/src/db/telemetry.db`; `mpm-telemetry ping` → `no such file or directory` | provider-reported usage through MPM's own telemetry is therefore unavailable. §5.4 uses the host's counters instead, which are available |
| A2 | A stale scheduler state file exists | `~/.mpm/run/scheduler [conflicted].state` — `tick_count` 414, pid 7575, last tick 2026-10-28 23:44 | a leftover from a merged state swap. Harmless today; it is the shape a **stale-state adoption** bug would take, so it is checked daily (§7.1) |
| A3 | Recurring `mpm_handoff write` failures | **18 `internal` errors in the last 7 days on the old runtime** (`3df72315`), matching 4 `warn handoff — EndSessionV2 upsert failed: CHECK constraint failed: ended_state IN (...)` audit rows | a real defect the week will keep hitting; it degrades handoff continuity, which is exactly what M1 measures. **The upgraded runtime validates the enumeration before the write** (`ValidateHandoffEndedState`), so this specific failure mode is expected to stop; the daily block counts it so the change is *observed*, not assumed (§7.3) |
| A4 | `mpm_memory query` internal errors | 4 in the last 7 days (old runtime) | small; watch |
| A5 | Recurring `health_check` substrate errors | 6 in the last 7 days (old runtime) | watch |
| A6 | No lesson has ever been reinforced | `lessons.reinforcement_count` is `1` for every row | see §6 — this is the finding, not a blocker |
| A7 | No lesson has a **directly linked** evidence row | 0 of 36 lessons (old runtime) have an `evidence` row with `artifact_type='lesson'` | **Corrected reading — see §6.3.** Zero *directly linked* evidence rows means that particular linkage is absent. It is **not** a claim that the lesson is false, and **not** a claim that the lesson has no other provenance |
| A8 | Same-UID separation is not a security boundary | every workspace in this topology is under `$HOME` and runs as the same UID | documented in §10, action 7 |
| A9 | The `internal` outcome class is over-used | on the old runtime, 36 `internal` rows in 7 days, of which **26 were misclassified payload-shape errors** ("commitments must be an array of strings…", "tags must be an array of strings…") | `internal` does **not** mean "the substrate failed a well-formed request". Read it with the §7.3 breakdown, never as a fault rate |
| A10 | The upgraded runtime refuses agent-facing lesson writes | `mpm_lessons action=save` is now refused at the execution point; the refusal is classified `internal\|unclassified` | **A policy refusal misclassified as a system fault.** It is a correct refusal that wrote nothing, but it will pollute the `internal` class for the whole week (§7.3) |

### 1.3 The day-zero command

```bash
scripts/dogfood_baseline.sh
```

One command, ~150 lines of fixed-format output. It is read-only by
construction (§0.2). The day-zero run's output is the reference against
which day 1 is compared.

**Exit status is part of the measurement, not an afterthought:**

| code | meaning | what the operator does |
|---|---|---|
| `0` | the report is **complete** and no stop condition is breached | proceed |
| `1` | the report is complete and **a stop condition is breached** | §9 — the experiment ends or pauses |
| `2` | the report is **INCOMPLETE**: a required measurement was missing or unreadable | **do not treat the report as a result.** A missing measurement is never reported as a measurement of zero |

The script **fails closed**. Every query that cannot run — a missing table,
a renamed column, an unreadable database — is recorded as a *measurement
gap*, printed in its own section with the underlying error, and forces exit
`2`. A partially-measured day is an incomplete day, and the week's series
would otherwise silently acquire a zero.

A **schema version difference** is not a gap. The pre-upgrade schema (no
`lesson_proposals` table) is a supported state; the script reports it as a
**version difference** in §4b and explicitly says the lesson-proposal
lifecycle is *unavailable by version* — never that the proposal queue is
empty.

---

## 2. The project-to-collection mapping

### 2.1 What the live substrate actually holds

> **These counts are the historical baseline** (`3df72315`, before the
> upgrade) and are preserved unchanged, because they are the evidence for
> a **structural** finding — the `collection` column carries three
> different kinds of thing — and a structural finding does not decay. The
> day-zero run re-measures the counts on `3d63244e`; §2.2's table is
> reproduced in Appendix A.1 with the upgraded numbers for comparison.
> The one substantive movement is that UNACCOUNTED now carries **1** tagged
> row where it carried **0**; the structural gap is otherwise unchanged.

`memories.collection` is not a single dimension. Measured live
(`deleted_at IS NULL`):

| collection | rows | what it actually is |
|---|---|---|
| `memories` | 59 | artifact type — general memories |
| `decisions` | 45 | artifact type |
| `changelog` | 11 | artifact type |
| **`agentshell`** | **10** | **a project name** |
| `project` | 6 | a programme record (HAL-9001 AgentShell, repo `/home/v/projects/agent-shell-theme`) |
| `directives` | 5 | artifact type |
| `theories` | 3 | artifact type |
| `mpm-opencode` | 1 | a product name |
| `lessons` | 1 | artifact type |

So the collection column carries **three different kinds of thing**: artifact
type, project name, and product/programme name.

### 2.2 The project coverage gap

| project | MPM rows tagged | rows in a project-named collection | host sessions |
|---|---|---|---|
| themailroom | 9 | 0 | 3 |
| agentshell | 28 | 10 | 4 |
| unaccounted | **0** | 0 | 1 |
| deepclaw | 2 | 0 | 1 |

**The missing collections are: `themailroom`, `unaccounted`, `deepclaw`.**
AgentShell has one; TheMailroom does not.

But the finding is not "three collections are missing". The finding is:

> **The project dimension was put in `collection` for one project only, and
> `collection` was already occupied by artifact type.**

Three consequences, each measured:

**(a) The `agentshell` collection silently drops most of the project.**
`mpm_memory query --collection agentshell` returns the 10 memory rows and
**nothing else**. The rest of AgentShell's knowledge carries the
`agentshell` **tag** and lives in its own collections:

| collection | AgentShell rows carrying the tag |
|---|---|
| `agentshell` | 10 |
| `decisions` | 8 |
| `memories` | 7 |
| `project` | 2 |
| `lessons` | 1 |
| **total tagged** | **28** |

So a project-scoped query returns **10 of the project's 28 rows** — 36 % —
with no warning that the other 18 were excluded. The dropped rows are
precisely the interesting ones: **all 8 of the project's decisions** and
its lesson.

**(b) There is no query that returns "all AgentShell knowledge" — and none
that returns "all the memories".**

Because the project is carried by a *collection* in one place and by a
*tag* in the others, the two queries that ought to exist are both wrong:

- `--collection agentshell` → 10 rows (the 10 memory rows).
- `--collection memories` → 59 rows, of which 7 are AgentShell's, plus 52
  that are not AgentShell's.

Neither is "AgentShell's memories". A project-scoped read is currently
**impossible**, which is not a reporting inconvenience — it is the
mechanism by which the two dimensions silently disagree.

**(c) There is no tag filter on retrieval.** `mpm_memory query` accepts
`collection` and nothing else. `SearchMemories(q, collection, …)` with an
empty collection searches *all* collections, so an unqualified query finds
everything and a project-qualified query finds only what happens to live in
that one collection. **The project dimension has no working retrieval
filter, in either position.**

### 2.3 The decision

**The project dimension is the host's project directory, and the
measurement uses it. MPM's internal project dimension is the tag.**

This is a *measurement* decision, and it is forced by the evidence:

- The host's project directory is **always recorded** (`session_v2.directory`
  — 22 sessions across 10 directories), unambiguous, and survives any
  substrate change.
- MPM's tag dimension is **incomplete** (UNACCOUNTED has zero tagged rows)
  and MPM's collection dimension is **overloaded** (§2.1).
- Anchoring the measurement outside the substrate is what makes the
  contamination measurement (§M4) meaningful: the independent dimension is
  the only one that can disagree with the substrate's, and a disagreement is
  the finding.

**Do not create `themailroom`, `unaccounted` or `deepclaw` collections.**
Creating them would deepen the inconsistency in §2.1, not fix it. The
recommended remediation — a substrate change, out of scope here and
requiring operator approval — is:

1. Normalise the project onto `tags`, one canonical lowercase tag per
   project (`themailroom`, `agentshell`, `unaccounted`, `deepclaw`).
2. Move the 10 `agentshell`-collection rows to `collection='memories'` with
   the `agentshell` tag preserved, so that "all memories" is one query and
   "all AgentShell memories" is a tag filter over it. (Their pointer kind
   does not change either way: `memoryPointerKindForCollection` maps every
   collection other than `theories` and `decisions` to `mpm://memory`, and
   these rows are memories already.)
3. Add a **tag filter** to `mpm_memory query`, so the project dimension
   becomes filterable.

Until (3) exists, **no project-scoped query can be trusted**, and the
daily review must judge project scoping by reading, not by filtering.

### 2.4 The mapping table the week will use

| project | host directory | MPM tag | MPM collection (today) | tagged rows, by collection | note |
|---|---|---|---|---|---|
| TheMailroom | `/home/v/projects/themailroom` | `themailroom` | — | changelog 4 · decisions 3 · memories 2 | tag only; 9 rows |
| AgentShell | `/home/v/projects/agent-shell-theme` | `agentshell` | `agentshell` (10) | agentshell 10 · decisions 8 · memories 7 · project 2 · lessons 1 | **both**; the collection is a subset |
| UNACCOUNTED | `/home/v/projects/Unaccounted` | `unaccounted` | — | — | **no MPM footprint at all** |
| DeepClaw | `/home/v/projects/deepclaw` | `deepclaw` | — | changelog 2 | 2 rows; joins mid-week |

---

## 3. The baseline query set

All of it is executed by `scripts/dogfood_baseline.sh`. This section is the
auditable form of the same queries.

### 3.1 The read-only posture, precisely

```bash
SNAP="$(mktemp -d)"
sqlite3 -readonly "file:$HOME/.mpm/src/db/mpm.db?mode=ro" ".backup '$SNAP/mpm.db'"
```

`.backup` reads from the source and writes only to the destination, and the
source connection is opened with `mode=ro`. The snapshot folds the WAL in,
so it is self-contained and consistent — verified: 143 memory rows live,
143 in the snapshot, `PRAGMA quick_check` → `ok`.

Every subsequent query runs against `$SNAP/mpm.db`. **The live database is
never opened for writing.**

The one command that would otherwise write — `mpm wake`, which records a
`tool_invocations` row on the tool path — is run against a scratch
workspace whose `src/db/mpm.db` *is* the snapshot:

```bash
mkdir -p "$SNAP/workspace/src/db"
cp "$SNAP/mpm.db" "$SNAP/workspace/src/db/mpm.db"
cp ~/.mpm/active.json "$SNAP/workspace/active.json"   # carries the session id
MPM_WORKSPACE="$SNAP/workspace" mpm wake --compact
```

Verified: the live database's `tool_invocations` count is unchanged by a
full run of the script, and the snapshot's is unchanged too (`mpm wake` on
the CLI path records no invocation row).

### 3.2 The baseline queries

**Identity and integrity**

```sql
PRAGMA integrity_check;                       -- ok
SELECT collection, COUNT(*) FROM memories
  WHERE deleted_at IS NULL GROUP BY collection ORDER BY 2 DESC;
```

**Knowledge inventory**

```sql
SELECT collection, COUNT(*), SUM(created_at >= :since)
  FROM memories WHERE deleted_at IS NULL GROUP BY collection;

-- `lessons` is a VIEW over `lessons_base`; the view does not expose the
-- `deleted_at` tombstone, so a "live lessons" count must go to the base
-- table. `lessons.created` is TEXT (RFC3339), NOT an epoch column.
SELECT COUNT(*) FROM lessons_base WHERE deleted_at IS NULL;

SELECT COUNT(*) FROM evidence;
SELECT COUNT(*), SUM(read_at IS NOT NULL) FROM session_handoffs;
SELECT COUNT(*), SUM(fired) FROM scheduled_wakes;
```

**Lesson-proposal lifecycle** (§6 — post-upgrade only; the table's absence
is a *version difference*, not an empty queue)

```sql
-- presence, not `schema_migrations`: the table is created by the base
-- DDL of the post-upgrade runtime and has NO migration id of its own.
SELECT COUNT(*) FROM sqlite_master
  WHERE type='table' AND name='lesson_proposals';

SELECT status, COUNT(*) FROM lesson_proposals GROUP BY status;

-- compaction-origin: the compaction path stores a {"title","body"}
-- envelope as the content; the agent path stores the raw fact text.
-- A non-empty `source_ids` is a SECONDARY signal (cited sources) and
-- does NOT by itself identify compaction.
SELECT COUNT(*) FROM lesson_proposals
  WHERE json_valid(content) AND json_extract(content,'$.title') IS NOT NULL;

SELECT id, created, status,
       CASE WHEN json_valid(content) AND json_extract(content,'$.title') IS NOT NULL
            THEN 'compaction' ELSE 'agent' END AS origin,
       CASE WHEN source_ids IS NOT NULL AND source_ids NOT IN ('null','','[]')
            THEN json_array_length(source_ids) ELSE 0 END AS cited_sources,
       source_session_id, framework_name, model_name,
       promoted_lesson_id, reviewed_at
  FROM lesson_proposals ORDER BY created;
```

**Write reliability** (§M5)

```sql
-- the attempted-write ledger. `tool_invocations.invocation_id` is NOT
-- unique: one id can carry rows from several tools. Any join on it must
-- therefore be guarded, or it multiplies rows and manufactures
-- confirmations that did not happen.
SELECT invocation_id, COUNT(*) n FROM tool_invocations
  GROUP BY invocation_id HAVING n > 1;

-- confirmed durability, only where the link is SOUND: the invocation_id
-- maps to exactly one ledger row AND exactly one provenance row, and the
-- artifact named by that provenance row is FOUND in the table its
-- artifact_type names.
SELECT ti.tool_name || ' ' || ti.action                        AS surface,
       SUM(ti.result_status = 'success')                       AS reported_ok,
       SUM(ti.result_status = 'success' AND lk.n = 1
           AND pv.n = 1)                                       AS sound_link,
       SUM(ti.result_status = 'success' AND lk.n = 1
           AND pv.n = 1 AND (
               (ap.artifact_type = 'memory'   AND m.id  IS NOT NULL) OR
               (ap.artifact_type = 'decision' AND md.id IS NOT NULL) OR
               (ap.artifact_type = 'theory'   AND mt.id IS NOT NULL) OR
               (ap.artifact_type = 'lesson'   AND lb.id IS NOT NULL) OR
               (ap.artifact_type = 'work'     AND wk.id IS NOT NULL) OR
               (ap.artifact_type = 'handoff'  AND ho.id IS NOT NULL)))
                                                           AS confirmed
  FROM tool_invocations ti
  LEFT JOIN (SELECT invocation_id, COUNT(*) n FROM tool_invocations
              GROUP BY invocation_id) lk
         ON lk.invocation_id = ti.invocation_id
  LEFT JOIN (SELECT invocation_id, COUNT(*) n FROM artifact_provenance
              WHERE invocation_id IS NOT NULL AND invocation_id != ''
              GROUP BY invocation_id) pv
         ON pv.invocation_id = ti.invocation_id
  LEFT JOIN artifact_provenance ap
         ON ap.invocation_id = ti.invocation_id AND pv.n = 1 AND lk.n = 1
  LEFT JOIN memories m          ON m.id  = ap.artifact_id AND ap.artifact_type = 'memory'
  LEFT JOIN memories md         ON md.id = ap.artifact_id AND ap.artifact_type = 'decision'
  LEFT JOIN memories mt         ON mt.id = ap.artifact_id AND ap.artifact_type = 'theory'
  LEFT JOIN lessons_base lb     ON lb.id = ap.artifact_id AND ap.artifact_type = 'lesson'
  LEFT JOIN works wk            ON wk.id = ap.artifact_id AND ap.artifact_type = 'work'
  LEFT JOIN session_handoffs ho ON ho.id = ap.artifact_id AND ap.artifact_type = 'handoff'
 WHERE ti.result_status = 'success'
 GROUP BY 1 ORDER BY 2 DESC;

-- the busy/locked failure mode (issue #178)
SELECT datetime(started_at,'unixepoch'), tool_name, action, error_message
  FROM tool_invocations WHERE outcome_code='wal_busy_timeout';
```

**Evidence linkage** (the supported/unsupported boundary, §6)

```sql
SELECT artifact_type, COUNT(*), SUM(strength > 0), SUM(strength < 0),
       COUNT(DISTINCT created_by)
  FROM evidence GROUP BY artifact_type;

SELECT COUNT(*) FROM lessons l
  WHERE NOT EXISTS (SELECT 1 FROM evidence e
                    WHERE e.artifact_id = l.id AND e.artifact_type = 'lesson');
```

**Proposal queues** (§6)

```sql
-- theories awaiting a resolution decision
SELECT id, substr(content,1,120) FROM memories
  WHERE deleted_at IS NULL AND collection = 'theories'
    AND json_extract(metadata,'$.status') = 'pending';

-- operator review queue
SELECT component, message_hash, count, last_seen FROM audit_cluster_proposals
  WHERE status = 'active' ORDER BY count DESC;

-- pending invalidation obligations
SELECT count(*) FROM epistemic_cascade_outbox WHERE status = 'pending';
```

**Retrieval** (M1, M4)

```sql
SELECT node_type, COUNT(*), SUM(reuse_count), SUM(success_count)
  FROM retrieval_metadata GROUP BY node_type;

SELECT COUNT(*) FROM retrieval_metadata
  WHERE last_retrieved_at >= :since;

SELECT COUNT(*) FROM memories m
  WHERE m.deleted_at IS NULL
    AND m.id NOT IN (SELECT node_id FROM retrieval_metadata);
```

**Tool activity**

```sql
SELECT tool_name, action, COUNT(*), SUM(result_status = 'error')
  FROM tool_invocations WHERE started_at >= :since
  GROUP BY 1,2 ORDER BY 3 DESC;

SELECT COALESCE(outcome_class,'(unclassified)'), result_status, COUNT(*)
  FROM tool_invocations WHERE started_at >= :since GROUP BY 1,2;
```

**Runtime errors**

```sql
-- `system_audit_log.created_at` is declared INTEGER but the stored
-- values are TEXT timestamps ('2026-10-30 16:33:36'). In SQLite a TEXT
-- value compares GREATER than every INTEGER, so `created_at >= :since`
-- matches EVERY row and silently reports the whole history as "in the
-- window". The window must be computed with strftime('%s', created_at).
SELECT created_at, level, component, message
  FROM system_audit_log
  WHERE CAST(strftime('%s', created_at) AS INTEGER) >= :since
    AND level IN ('warn','error','fatal','critical')
  ORDER BY created_at;
```

The substrate also exposes this through its own tool surface — `mpm_system`
action `query_audit_log` — which is the supported way to read the audit log
without a SQL client. (The *activity* feed, `mpm_context recent_activity`,
is a different surface with different exclusions: it is the cross-tool
record of successful mutating actions, and it excludes system, diagnostic
and maintenance actors. Neither is a substitute for the other, and the
audit log is the right one for faults.)

**Operational** (the scheduler's own heartbeat, §7.1)

```bash
cat ~/.mpm/run/scheduler.state
# {"last_tick_unix":1793378263,"process_started_unix":1793377963,"tick_count":17,
#  "pid":3890452,"last_status":"ok","last_error":""}
```

### 3.3 The host-side baseline

The host keeps its own session record, and it is the only place where
provider-reported token counters exist:

```bash
sqlite3 -readonly "file:$HOME/.local/share/opencode/opencode.db?mode=ro"
```

| table | what it holds |
|---|---|
| `session_v2` | one row per session: `directory`, `project_id`, `cost`, `tokens_input`, `tokens_output`, `tokens_reasoning`, `tokens_cache_read`, `tokens_cache_write`, `time_created` |
| `session_message` | one row per message; `data.tokens` carries the same counters per message, `data.model` the provider and model |
| `project` | the project table; `name` is NULL on this host, so **`directory` is the project identity** |

Day-zero measured totals across all 22 sessions:

| quantity | value |
|---|---|
| assistant messages carrying a `tokens` object | 18 447 |
| uncached input (`tokens.input`) | 78 512 146 |
| cached input (`tokens.cache.read`) | 4 212 214 175 |
| output (`tokens.output`) | 6 715 166 |
| reasoning (`tokens.reasoning`) | 5 588 937 |
| cache write (`tokens.cache.write`) | 0 |
| provider-reported cost | 35.84 |

---

## 4. The five measurements

Each measurement states the question, the instrument, the numerator and the
denominator, **what it does not measure**, and how it is reported.

M1–M4 are the behavioural measurements. **M5 is an operational one**, added
for this revision: it measures whether the substrate's writes are actually
durable, which is the substrate of every other measurement — a knowledge
base that silently loses writes cannot be evaluated for recall.

### M1 — cross-session recall

> *Did a later session get what an earlier session wrote, without being
> told to look?*

**Instrument.** `retrieval_metadata` (the surfaced set) × `tool_invocations`
(which session was active when) × `session_v2` (host session boundaries).

**Population.** Artifacts on project *P* created in a session *S₁* that
ended before session *S₂* began, and still live at *S₂*'s start.

**Numerator.** Those artifacts whose `retrieval_metadata.last_retrieved_at`
falls inside *S₂*'s window.

**Denominator.** The population.

**Reported as.** `recalled / eligible`, per project, per day.

**What it does NOT measure — and this is the important part.**

`retrieval_metadata` is written **only** by the `mpm_memory query` path
(`handleQueryLongTermMemory` calls `dm.RecordRetrieval` for each result).
The **wake-context path records no per-node trace at all** — and the wake
context is the dominant continuity surface: **22 165 `read_wake_context`
invocations in the last 7 days against 35 `mpm_memory query` invocations.**

So M1, as instrumented, is a **lower bound on query-path recall and a
complete absence of a measurement of wake-path recall.** The wake path is
measured two other ways:

1. the **reconstructed wake context** (§3.1), which shows what the wake
   context *would* have surfaced on the snapshot — reconstructed, not
   observed, because the live run's wake context is not persisted;
2. the operator's answer #4 — *"what did you have to re-learn that MPM
   already knew?"* — which is the only instrument that sees the wake path
   as it was actually experienced.

**Also note:** `retrieval_metadata` is **observability-only**. Search
ranking is not altered by it. A reuse count is a record of *what was
surfaced*, never a measure of *relevance*.

### M2 — retrieval relevance

> *Of what was surfaced, how much was relevant to the work actually done?*

**Instrument.** The surfaced set, stratified-sampled.

**Method — deliberately small, because the operator's time is the scarce
resource.** Each day the operator labels:

- **every** item the M4 detector flags as possibly foreign (see below), and
- a **random sample of 5** otherwise-surfaced items.

Three labels: `relevant`, `not relevant`, `uncertain`. Labels come from
reading the item and the session's task — not from the score.

**Reported as.** `relevant / labelled`, with *n* stated and the interval
stated. Over seven days *n* lands around 60–100, which supports a statement
about the order of magnitude and **nothing finer**. A daily number with *n*
≈ 8 is a *trigger for attention*, not a metric. It is reported as a daily
count of labels, and a weekly rate with an interval — never as a daily
percentage.

**What it does NOT measure.** Recall. A relevant item that was never
surfaced is invisible to M2, which is why M3 exists.

### M3 — missed retrievals

> *Did a retrieval that found nothing actually have nothing to find?*

This is the measurement with the most machinery behind it, because the
substrate already documents the failure mode.

**The failure mode.** `internal/core/agentshell_query_fixture_test.go`
reproduces a reported failure: an agent searched for *"AgentShell WordPress
theme MCP configurator"* and got **zero results**, even though MPM held
relevant memories. The cause is FTS5's **implicit AND** — every term must
match the same row, so adding a term can only remove results. The
fixture also pins a second distinction: in a semantic-capable configuration
the same query returns rows from the **vector arm with no lexical match**,
and every one of them must be labelled, because *"5 results"* and *"5
relevant results"* are not the same claim.

**The substrate already says this to the agent.** `attachZeroHitHint`
(`internal/core/tools/handlers.go:903`) attaches a hint to every zero-hit
response naming the term count, the terms themselves, and the longest term
to retry with. The hint is in the **response**; it is not persisted
anywhere.

**Therefore the detection and the adjudication are two different steps, with
two different instruments:**

- **Detection** — a zero-hit is not recorded in any table. The substrate
  records *that* a query ran (`tool_invocations`: tool, action, outcome
  class, duration) but **not the query text and not the result count**. The
  query text is recoverable from the *host* side (the tool-call arguments
  in the session transcript); the result is not persisted anywhere.
  Detection is therefore **operator-read**: during the daily review, the
  operator notes every `mpm_memory query` whose result was empty. The
  substrate's own zero-hit hint is what makes the note quick — it names the
  offending terms.

- **Adjudication** — for each detected zero-hit, re-run the query against
  the **snapshot workspace** with one term removed at a time, longest term
  first (the substrate's own advice). This is non-destructive and
  reproducible. The outcome is exactly one of:

  | adjudication | meaning | counts as |
  |---|---|---|
  | a relaxation returns rows | the original was a **lexical over-constraint** | a **missed retrieval** |
  | no relaxation returns rows, but the knowledge demonstrably exists | the knowledge was not **indexed** for that term (or was not written) | a **missed retrieval** |
  | no relaxation returns rows and the knowledge does not exist | an honest zero | **not** a miss |
  | rows return only via the vector arm | found, but not lexically | **found, vector-only** — reported separately |

**Reported as.** Missed retrievals per day, split by cause (over-constrained
/ not indexed / honest zero), with the query text recorded verbatim for
every miss. The verbatim query is the deliverable: it is what makes the
week actionable.

### M4 — cross-project contamination

> *Did a session get knowledge belonging to a different project?*

**Instrument.** The surfaced set × the project tag × the host session's
directory.

**Detection — two passes, because the tag dimension is incomplete:**

1. **Tag pass.** An artifact surfaced in a session for project *P* whose
   tag set contains *Q* ≠ *P*. Fully automatic.
2. **Lexical pass.** An artifact surfaced in a session for project *P* whose
   *content* names another project's directory or slug. Automatic, and it
   catches the case the tag pass cannot: contamination *out of* a project
   with no tagged rows at all — which today is UNACCOUNTED (0 tagged rows).

**The limits are stated rather than hidden.** The tag pass sees only what
the tag records; UNACCOUNTED has **zero** tagged rows and DeepClaw has two,
so the tag pass alone would score contamination at zero for those projects.
The lexical pass only catches *named* projects. And the `agentshell`
collection's 10 rows all carry the tag, so the tag pass sees 28 AgentShell
rows while a project-scoped query sees 10 — the two do not agree, and
which one a reader gets depends on the query they happened to write.

**Therefore the automated pass NARROWS the review set; it does not replace
the operator's read.** The operator's answer #2 — *"did anything you read
belong to a different project?"* — is the primary instrument, and the
automatic passes exist to make that judgement fast and to catch what the
operator's attention missed.

**Reported as.** Contamination candidates per day (automatic) and confirmed
contaminations per day (operator). The confirmed count is the metric; the
candidate count is a workload measure.

### M5 — write reliability: reported success versus confirmed durability

> *When the substrate says a write succeeded, did the artifact actually
> end up on disk and can it still be found?*

**Why this is a measurement and not a health check.** Issue #178 remains
open and covers writes lost under ordinary SQLite contention. The daily
block already surfaces `wal_busy_timeout` errors; what it did **not** do is
distinguish *the tool said yes* from *the artifact is there*. M5 measures
that gap on the live database, using only instruments that already exist.

**The methodology, in one sentence.** A write is **attempted** when it
appears in `tool_invocations`; it is **reported successful** when that row
says `result_status='success'`; it is **confirmed durable** only when a
*sound* link ties that invocation to a provenance row whose artifact is
then **found** in the table its `artifact_type` names. The three are
different claims and the report keeps them apart.

#### M5.1 The instruments, and what each one can and cannot see

| instrument | what it holds | what it cannot see |
|---|---|---|
| `tool_invocations` | the tool-surface ledger: tool, action, `result_status`, `outcome_class`, `outcome_code`, `error_message`, `invocation_id` | **not every write.** The CLI and the internal paths write without a tool invocation. It is a **lower bound** on attempts |
| `artifact_provenance` | one row per artifact, with `artifact_id`, `artifact_type`, and `invocation_id` | the link is **absent** for most surfaces, and `invocation_id` is **not unique** in the ledger |
| `system_audit_log` | the substrate's own fault record — including failures on paths that never became tool calls | sparse: 17 rows over the whole corpus |
| the target tables | `memories`, `lessons_base`, `lesson_proposals`, `session_handoffs` | they hold the artifact, not the *report* of the write |

#### M5.2 The three link states — why the naive join is wrong

The obvious way to confirm durability is

```sql
-- WRONG. Do not use this.
SELECT COUNT(*) FROM tool_invocations ti
  JOIN artifact_provenance ap ON ap.invocation_id = ti.invocation_id
  JOIN memories m ON m.id = ap.artifact_id;
```

It is wrong for **two independent reasons**, both measured on the live
database:

1. **`tool_invocations.invocation_id` is not unique.** 23 800 ledger rows
   carry 23 749 distinct invocation ids, and the worst offender
   (`inv_d6d08b3e-…`) carries **24 rows across 7 different tools** — a
   `mpm_memory save`, several `mpm_decisions record`, `mpm_lessons
   propose`, `mpm_theories propose`, `mpm_work note` and `mpm_work create`
   all share it. Joining on it **multiplies rows** and manufactures a
   "confirmed" write for every one of them.
2. **`artifact_provenance` can hold several rows for one invocation id**
   (8 in the worst case), so even a unique invocation id does not
   guarantee a unique artifact.

So M5 counts a link only when it is **sound**: the invocation id maps to
exactly one ledger row **and** exactly one provenance row. A link that
fails that test is reported as **ambiguous**, which is a measurement blind
spot — *not* a lost write.

#### M5.3 What is measured, and the measured result

| # | quantity | how | day-zero result (live, `3d63244e`) |
|---|---|---|---|
| W1 | attempted writes (observable lower bound) | write-classified rows in `tool_invocations` | ~393 all time |
| W2 | reported successes / failures | `result_status` by `outcome_class` | 265 / 95 in the window |
| W3 | **confirmed durable** (per write) | sound link + artifact found | **only `mpm_memory save` has a sound link: 78 of 79 confirmed** |
| W4 | **unconfirmable per write** (aggregate only) | surfaces with no sound link | decisions, handoffs, lessons, changelog, theories, evidence, work — **all of them** |
| W5 | unknown outcome | reported success, no provenance row, no returned artifact id | 2 on `mpm_memory save` |
| W6 | ambiguous link | invocation id shared by several ledger rows | 5 on `mpm_memory save` |
| W7 | SQLite busy/locked | `outcome_code='wal_busy_timeout'` | **2**, both `mpm_decisions record`, 2026-10-24/25 |
| W8 | handoff persistence failures | `system_audit_log` `component='handoff'` | **2**, both `EndSessionV2 … CHECK constraint failed: ended_state` |
| W9 | provenance integrity | artifact absent from the table its type names; invocation absent from the ledger | **1** and **12** respectively |
| W10 | scheduler correlation | busy/locked events vs scheduler ticks (journal) | 1 of 2 busy events within 60 s of a tick |

#### M5.4 What M5 does NOT measure — the blind spots, stated

These are the reasons the daily report says "unconfirmable" rather than
"lost". They are the substance of the finding, not caveats around it.

- **The attempted-write count is a lower bound.** Writes made by the CLI
  (`mpm memory add`, `mpm lesson add`, `mpm record_decision`) and by
  internal paths are not in `tool_invocations`. The live database proves
  this: 46 `decision` provenance rows exist, and **not one** of them has an
  `invocation_id` — they were written without a tool call. A week of
  measured attempts undercounts real attempts by an unknown amount.
- **Per-write durability is confirmable for exactly one surface.** Only
  `mpm_memory save` carries a sound invocation→artifact link. Every other
  write surface is **aggregate-only**: the write and the artifact cannot be
  paired, so "was this one durable?" is **unanswerable** for decisions,
  handoffs, lessons, changelog entries, theories, evidence and work items.
  The aggregate row counts are reported in §7d of the daily block and are
  **not** a loss measurement: a handoff upsert on an existing `session_id`
  *replaces* a row instead of adding one, so a lower row count is expected.
- **A zero-hit handoff audit row is not a lost handoff.** The two
  `EndSessionV2 … CHECK constraint failed` rows **do** have a nearby
  `mpm_handoff write` tool call that reported `error`. They are *reported*
  failures, not silent losses. (The original plan asserted these had no
  nearby tool call; that was wrong, and the cause was a real defect in the
  correlation itself — see §7.3.)
- **The `internal` class cannot be read as a fault rate.** It is dominated
  by misclassified payload-shape errors and by policy refusals (§7.3).
- **The `artifact_type` namespace collides.** A **memory** saved into the
  `lessons` *collection* is recorded with `artifact_type='lesson'` while its
  id belongs to a memory row. One such row exists live, and it is why W9
  reads **1** and not 0. A provenance row is therefore **not** by itself
  proof that an artifact of that type exists.
- **Nothing here is instrumented to detect a write that was never
  attempted at all** — a caller that silently skipped a write leaves no
  trace in any of these tables.
- **The week does not create contention.** No artificial write load, no
  lock, no stress. The natural workload of four real projects provides the
  contention, and the busy/locked count is therefore a *sample* of
  ordinary operation, not a controlled experiment.

#### M5.5 The daily reporting form

The daily block's §7 prints, in this order: attempted writes (all time and
in the window), reported success/failure, the outcome-class breakdown, the
`internal`-class breakdown by cause, the per-surface
reported-vs-confirmed table, the aggregate cross-checks, the unknown-outcome
and ambiguous-link breakdown, the busy/locked rows, the handoff persistence
failures with their nearest tool outcome, the provenance integrity counts,
and the scheduler correlation. The daily answer the operator adds is one
line:

> **Did anything you wrote in the last day turn out not to be there?** — and
> if so, which surface.

A single confirmed instance of a reported success that cannot be found is
worth more than the seven-day rate, and is recorded verbatim.

---

## 5. Token accounting

### 5.1 The quantities, and where each comes from

The brief requires cached input, uncached input, output and local estimates
to be kept **distinct**. They are distinct in the host's own record, and
the distinction survives into the report. The four the brief names are
**uncached input**, **cached input**, **output** and the **local estimate**;
three more are recorded alongside them, because collapsing them would lose
information:

| quantity | source field | status |
|---|---|---|
| **uncached input** | `session_v2.tokens_input` / `session_message.data.$.tokens.input` | **provider-reported** |
| **cached input** | `session_v2.tokens_cache_read` / `$.tokens.cache.read` | **provider-reported** |
| **output** | `session_v2.tokens_output` / `$.tokens.output` | **provider-reported** |
| **reasoning** | `session_v2.tokens_reasoning` / `$.tokens.reasoning` | **provider-reported** (kept separate from output) |
| **cache write** | `session_v2.tokens_cache_write` / `$.tokens.cache.write` | **provider-reported** |
| **cost** | `session_v2.cost` / `$.cost` | **provider-reported where the provider prices it** |
| **local estimate** | computed from the transcript by this plan | **estimate — never summed with the above** |

`input` and `cache.read` are separate fields, and they must be: a message
with `input=414, cache.read=298029` is only coherent if `input` is the
*uncached* portion. Over the whole day-zero corpus, `cache.read` is **54×**
`input`, so the two cannot be merged.

### 5.2 The rule

**Provider-reported counters and local estimates are never summed, never
averaged together, and never presented in the same column.** Every number in
the daily block carries its provenance in its label. An estimate is written
`≈` and never in a provider column.

### 5.3 Where the provider does *not* report cost

Measured across the day-zero corpus, by provider:

| provider | messages | cost reported |
|---|---|---|
| `minimax` | 6 076 | 29.19 |
| `deepseek` | 4 899 | 6.63 |
| `opencode-go` | 959 | 0.02 |
| **`minimax-coding-plan`** | **6 531** | **0.00 — despite reporting full token counts** |

The coding-plan provider reports usage but **not** cost. Those 6 531
messages are priced at read time from a versioned catalog — the pattern
already in `internal/telemetry/cost.go`, where the raw ledger stores
provider counters only and `ProjectCost` applies a `Pricing` catalog at read
time. The result is an **estimate** and is labelled as one. It is never
written into a provider-reported column.

### 5.4 MPM's own telemetry is not the source — and does not need to be

MPM's telemetry substrate exists and carries exactly the right fields
(`input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`,
`reasoning_tokens`, all nullable, with *absent* and *zero* analytically
distinct — `internal/telemetry/frame.go`). `mpm-telemetry cost` applies the
pricing catalog at read time.

**It is not running on this host** (A1): no unit installed, no socket, no
`telemetry.db`. Two independent reasons it would not help here even if it
were running:

- the **OpenCode adapter does not emit telemetry frames** — the frame
  emitter is `agent_installation/lib/telemetry_adapter.py` (the Claude Code
  adapter); the OpenCode plugin has no token fields at all;
- the substrate's own LLM calls **do not parse the provider's `usage`
  object**, so MPM's internal synthesis cannot report usage either.

Installing and enabling the collector is **operator action 8**, and it is
*optional*: the host's counters cover the agent's usage, which is the usage
that matters for a dogfood evaluation of the agent.

**This is the one place where the brief's "prefer existing telemetry"
constraint bends, and the bend is justified in one sentence:** the existing
telemetry surface measures MPM's *internal* calls, and the evaluation is
about the *agent's* calls, so the host's own record is the correct
instrument. No second subsystem is built.

---

## 6. The lesson-proposal lifecycle, and the supported/unsupported boundary

### 6.1 The lifecycle exists now — the old finding is withdrawn

The original plan's §6.1 read:

> *There isn't one. A lesson is written straight into the durable store with
> no review gate.*

**That was true of the old runtime `3df72315`, and it is no longer true.**
The upgraded runtime `3d63244e` introduces a real review gate, and the plan
is corrected accordingly. Both revisions are recorded, because the contrast
*is* the finding:

| | old runtime `3df72315` | upgraded runtime `3d63244e` |
|---|---|---|
| lesson-proposal queue | **did not exist** | **`lesson_proposals` table** |
| `mpm_lessons` actions | `save`, `search`, `list`, `delete`, `restore`, `shred` | `save` **(operator-only)**, `propose`, `search`, `list`, `delete`, `restore`, `shred` |
| agent-facing `action=save` | permitted — a lesson was live the instant it was written | **refused at the execution point**; nothing is written |
| how an agent records an experience | `mpm_lessons action=save` | `mpm_lessons action=propose` → a **pending** proposal |
| what compaction writes | a trusted lesson | a **pending** proposal (`ProposalsCreated`, never a trusted lesson) |
| CLI review surface | none | `mpm lesson proposals`, `mpm lesson promote <id>`, `mpm lesson reject <id>`, `mpm lesson stats` |

The refusal is **structural**, not advisory. The error text says it
explicitly: *"a lesson is trusted on arrival … No payload field — including
`operator=true`, `confirm=true` or `review=true` — authorizes it: a value
the caller supplies is not operator intent."* The lesson surface has no
pending state, so a row there is immediately in the trusted retrieval set;
that is exactly why creating one is the operator's decision and not the
agent's.

#### 6.1.1 The proposal states

`lesson_proposals.status` is a closed enum: `pending`, `promoted`,
`rejected`.

| state | meaning | what carries it |
|---|---|---|
| **pending** | proposed, **not** trusted | invisible to lesson search, lesson list and the wake context |
| **promoted** | an operator accepted it | `promoted_lesson_id` points at the durable lesson; `reviewed_at` is set |
| **rejected** | an operator declined it | `reviewed_at` is set; `promoted_lesson_id` is null |

The transition is **one-way and operator-driven**: `PromoteLessonProposal`
moves `pending → promoted` and creates the lesson; `RejectLessonProposal`
moves `pending → rejected`. Both write `reviewed_at`. An agent cannot
reach either.

#### 6.1.2 Telling a compaction proposal from an agent proposal

This matters for the week, because the two have different trust
implications and only one of them is *derived* from evidence already in
the substrate.

| | agent proposal | compaction proposal |
|---|---|---|
| `content` | the raw `fact` text the caller supplied | a JSON **envelope**: `{"title": …, "body": …}` |
| `source_ids` | the caller's cited sources; **may be empty** | the **raw memory ids** of the synthesised batch — never empty |
| `model_name` | usually empty on this host | usually empty on this host |
| detection | — | `json_valid(content) AND json_extract(content,'$.title') IS NOT NULL` |

The daily block reports both signals, and labels the content test as the
**primary** one:

> *compaction-origin proposals* — content carries a `{"title":…}` envelope
> *proposals carrying cited sources* — `source_ids` is non-empty

**`source_ids` non-empty does NOT by itself mean compaction**, because the
agent-facing `action=propose` accepts `source_ids` from its payload
("the Provenance Proxy's citations"). A proposal with cited sources and no
envelope is an agent proposal that cited something.

#### 6.1.3 The provenance a proposal carries

`lesson_proposals` records: `source_session_id`, `framework_name`,
`model_name`, `source_ids`, `promoted_lesson_id`, `reviewed_at`. The daily
block reports how many proposals have each field populated. On the live
database at the time of writing, `source_session_id` and `framework_name`
were populated for every proposal and `model_name` for none — the host
reports no model name on this path. A proposal with no `model_name` is not
damaged; it means the host did not supply one.

#### 6.1.4 The version difference, handled explicitly

The pre-upgrade schema has **no `lesson_proposals` table**. That is a
**version difference**, and the script reports it as one:

```
── 4b. LESSON PROPOSALS (the trust gate) ───────────────────────────
  VERSION DIFFERENCE — this snapshot predates the lesson-proposal lifecycle.
  The `lesson_proposals` table does not exist. That is NOT an empty queue:
  the concept did not exist yet at this revision. The lesson-proposal
  lifecycle below is therefore UNAVAILABLE for this snapshot, by version.
```

**Detection is by table presence, not by migration id.** `lesson_proposals`
is created by the **base DDL** of the post-upgrade runtime and has **no row
in `schema_migrations`** — the latest migration is `lessons_fts_resync_v1`
(2026-10-23), unchanged by the upgrade. A version check that consulted
`schema_migrations` would report "up to date" on a pre-upgrade database.

This is exercised in the validation (§7 of the script's own test matrix, and
Task 7 of this revision): the script is run against a disposable database
initialised by the **old** binary and must report the version difference,
not an empty queue, and must exit `0` with no measurement gaps.

### 6.2 The other three queues that exist

The lesson-proposal queue is new; these three were already there and are
still measured daily.

| queue | where | day-zero (old runtime) | action |
|---|---|---|---|
| **theories awaiting a resolution decision** | `mpm_theories list status=pending` (CLI: `mpm theory pending`) — `memories.collection='theories'` with `metadata.status='pending'` | **1** pending | `mpm_theories resolve` (CLI: `mpm theory resolve`) |
| **operator review queue** | `audit_cluster_proposals` where `status='active'` | **5 rows** — security (occurrence count 3), handoff (2, 1, 1), web_db (1) | `mpm_system` actions `list_clusters` / `snooze_cluster` / `unsnooze_cluster` / `resolve_cluster` / `annotate_cluster` |
| **pending invalidation obligations** | `epistemic_cascade_outbox` where `status='pending'` | **0** | automatic |

A theory is explicitly a hypothesis awaiting evidence, with a resolution
action. A **pending lesson proposal** is now the same shape of thing for
lessons — a claim awaiting a decision — and the week measures both.

### 6.3 The supported/unsupported boundary — the corrected claim

The original plan concluded:

> *Every lesson in the corpus is, by the substrate's own evidence model, an
> unsupported assertion.*

**That over-claims, and it is withdrawn.** What was measured is:

| fact | measured (old runtime, 36 lessons) |
|---|---|
| lessons in the corpus | 36 |
| lessons with a **directly linked** evidence row (`evidence.artifact_type='lesson'`) | **0** |
| lessons with `reinforcement_count > 1` | 0 (all are exactly 1) |
| evidence rows overall | 101 — 98 `memory`, 3 `theory`, **0 `lesson`**, **0 `decision`** |
| distinct `created_by` across all 101 evidence rows | 4 |
| `independence_factor` across all evidence rows | 1.0 for every row |

**What that does support:** no lesson in the corpus is linked to an evidence
row *in the `evidence` table*. The `evidence` linkage — one particular
provenance path — is absent for lessons.

**What that does NOT support:**

- it does **not** establish that any lesson is **false**;
- it does **not** establish that a lesson is **unsupported by every other
  provenance path**. A lesson can carry, and does carry, other provenance:
  `source_session_id`, `artifact_provenance` rows, `retrieval_metadata`
  surfacing history, and — from the upgraded runtime onward — the review
  trail of the proposal it was promoted from. Absence from one table is
  not absence of support.
- it does **not** mean the corpus contains no supported knowledge. The
  98 memory-linked and 3 theory-linked evidence rows *are* directly
  linked; the finding is specific to the `lesson` artifact type.

The honest phrasing, and the one the daily block now uses:

> **Lessons with no directly linked evidence row: 0 of N.** Zero
> *directly linked* evidence rows means that particular linkage is absent.
> It is **not** a claim that the lesson is false, and **not** a claim that
> the lesson has no other provenance (provenance rows, source sessions,
> surfacing history and review history are separate evidence paths).

**Why the distinction matters for the week.** "Every lesson is unsupported"
would mean the substrate has a trust problem: it would have admitted false
knowledge and the week would measure how bad that is. The true statement is
narrower and more actionable: **the substrate's evidence table is not
populated for lessons at all** — 0 of 36 — which is a *gap in the
instrument*, not a verdict on the knowledge. A week that measured the
instrument's gap would produce a finding about instrumentation. A week that
assumed the knowledge was bad would produce a finding about the knowledge.
The measurement must be able to tell them apart, which is why the daily
block now reports **direct evidence linkage** as a named quantity with its
denominator, and the §6.3 criteria are read *by category* rather than as a
single verdict.

### 6.4 The criteria, and how the classification changes

The substrate's own signals, in the order they are worth trusting:

| # | criterion | test | why it is trustworthy |
|---|---|---|---|
| 1 | **Evidence linkage** | ∃ an `evidence` row with `artifact_id = <id>` and a matching `artifact_type` | the substrate refuses evidence without an artifact (`internal/core/evidence.go`), so a link means something was actually recorded |
| 2 | **Independence** | the evidence's `created_by` differs from the artifact's `source_session_id`, and `independence_factor > 1` | evidence created in the asserting session is a **self-assertion**, not support. `independence_factor` is `1.0` on every current row — nothing has been recorded as independent |
| 3 | **Corroboration** | ≥ 2 evidence rows from different `created_by` values | one observer is an anecdote |
| 4 | **Contradiction** | ∃ an evidence row with `strength < 0` (the CHECK permits −1.0), or a `supersede` trigger in `confidence_history` | a claim with recorded counter-evidence is not knowledge |
| 5 | **Reinforcement** | `reinforcement_count > 1` | weak, and **not** independent: the substrate documents that reinforcement does not affect retrieval ranking. It records re-assertion, not support |
| 6 | **Confidence** | `confidence` vs the evidence count | **not** independent — confidence is *computed from* the evidence. A high confidence with zero evidence rows means the evidence was not recorded, not that the claim is supported |
| 7 | **Decay** | ∃ a `decay_tick` trigger in `confidence_history` for the artifact | confidence moved with no new evidence; the artifact's confidence is drifting down unattended |
| 8 | **Review provenance** *(new in the upgraded runtime)* | the lesson was promoted from a `lesson_proposals` row that carries `source_ids`, `source_session_id` and `reviewed_at` | a reviewed proposal is the one path by which a lesson acquires a **human-checked** origin. It is not evidence, and it does not satisfy criteria 1–4; it is a *separate* class of support |
| 9 | **Proposal origin** *(new)* | the proposal was compaction-origin | a compaction proposal is *derived* from material already in the substrate, so it inherits citations — but it is still untrusted until promoted |

**The classification an item gets.** The original table collapsed every
lesson into one class. It is restated here with the **categories kept
distinct**, and with the "unsupported" class renamed to what it actually
measures:

| class | test | day-zero count (lessons, old runtime) |
|---|---|---|
| **supported** | ≥ 1 evidence row, `strength > 0`, `created_by` ≠ the asserting session | 0 of 36 |
| **corroborated** | ≥ 2 evidence rows from different `created_by` | 0 of 36 |
| **self-asserted** | evidence exists but `created_by` = the asserting session | 0 of 36 |
| **contradicted** | an evidence row with `strength < 0`, or a `supersede` trigger | 0 of 36 |
| **not evidence-linked** | **no `evidence` row of the matching `artifact_type`** — the *measured* fact | **36 of 36** |
| **reviewed** *(new)* | promoted from a proposal with a `reviewed_at` | 0 of 36 (the lifecycle did not exist) |

**"not evidence-linked" is not "unsupported".** It says the `evidence`
table has no row for this artifact. It is the honest name for the
measurement, and it leaves the judgement open rather than closing it.

### 6.5 The operator's per-item surface

```bash
mpm why <id>      # evidence + confidence + retrieval provenance in one view
```

That is the surface the daily review uses when an item is flagged. It is a
read; it changes nothing.

For a proposal, the review surface is:

```bash
mpm lesson proposals        # the pending queue
mpm lesson promote <id>     # operator-only: accept
mpm lesson reject <id>      # operator-only: decline
```

**Promoting and rejecting are operator actions, and they are recorded in
the daily block as counts, not performed by it.** The week measures how
many proposals accumulate, how many are promoted, how many are rejected,
and how many of each origin — the *behaviour* of the gate under real
workload.

### 6.6 What this means for the week

Two questions, and they are different questions:

1. **Does the trust gate work?** (the new runtime) Does a lesson enter the
   trusted set only after review? Are proposals promoted at all, or does
   the queue simply grow? The daily block reports pending / promoted /
   rejected, split by origin.
2. **Does knowledge accumulate support?** (unchanged) Does knowledge
   written during ordinary development acquire evidence, or does it
   accumulate as bare assertions? The daily block reports
   *lessons with no directly linked evidence* as a count with its
   denominator, and the daily block reports **new** lessons written each
   day and how many of them gain evidence later.

The number to watch for (2) is unchanged: a lesson written on day 1 that
still has zero directly linked evidence on day 7 is a **stale assertion**.
But the label is now "not evidence-linked", and the read-out states it as a
gap in the evidence instrument as well as a property of the knowledge.

For (1), the sharpest week-1 finding would be a **growing pending queue**:
proposals created faster than an operator can review them. That is a real
operational finding about the trust gate, and the daily block is built to
surface it.

---

## 7. Operational checks

All of these are in the daily block. This section says what each one means
and what a failure indicates.

### 7.1 Scheduler stability

The scheduler's own heartbeat is the single most useful operational signal,
and it is a **file**, not a query:

```bash
cat ~/.mpm/run/scheduler.state
# {"last_tick_unix":1793378263,"process_started_unix":1793377963,"tick_count":17,
#  "pid":3890452,"last_status":"ok","last_error":""}
```

| check | threshold | what a failure means |
|---|---|---|
| `pid` is alive (`/proc/<pid>` exists) | — | the scheduler died; wakes are not firing |
| `last_tick_unix` age | ≤ 600 s (10 intervals at 60 s) | the scheduler is stuck, or its state file is not being written |
| `last_status` | `ok` | a failed tick; `last_error` names it |
| `tick_count` **monotonically increases** between consecutive runs | — | a counter that does not advance means the tick handler is not running even though the process is |
| **no `scheduler [conflicted].state` appears** | — | a merged state swap left a stale file; a *growing* set of them means the atomic state swap is racing (§A2) |

The `tick_count` **delta** matters more than its value: a scheduler that
restarts resets the count, so a delta of zero across a day with activity is
a restart, and a delta far below the wall-clock interval count is a
partially-failing tick loop.

### 7.2 Database integrity

```sql
PRAGMA integrity_check;   -- ok
PRAGMA quick_check;       -- ok
```

Run against the **snapshot** (the live database is not opened for writing).
`integrity_check` is the full structural check; `quick_check` skips the
index consistency pass and is the one to run more often. Both returned `ok`
on the day-zero snapshot.

Also watched: the **WAL size** (4.0 MB at day zero). A WAL that grows
without bound means a long-running reader is pinning it, or a checkpoint is
failing.

### 7.3 Runtime errors

Two surfaces, and they answer different questions.

**`system_audit_log`** — the substrate's own record of faults:

```sql
-- `created_at` is declared INTEGER but holds TEXT ('2026-10-30 16:33:36').
-- A TEXT value compares GREATER than every INTEGER in SQLite, so
-- `created_at >= :since` matches EVERY row. Use strftime('%s', …).
SELECT created_at, level, component, message
  FROM system_audit_log
  WHERE CAST(strftime('%s', created_at) AS INTEGER) >= :since
    AND level IN ('warn','error','fatal','critical')
  ORDER BY created_at;
```

> **This is a measurement defect that was in the original plan and is now
> fixed.** The plan's own audit-log query used `created_at >= :since`,
> which on the live database matched all 17 rows rather than the 1 row
> actually in the window — every row is "in the window" because its
> timestamp is TEXT. The daily block now converts with `strftime('%s', …)`
> and the audit-log section of the report is a real window.

The day-zero corpus (old runtime) is 16 rows and reads as a small, specific
list:

| level | component | count | what |
|---|---|---|---|
| error | `security` | 3 | `sensitive content blocked` (2026-10-29) — the toxicity filter refusing content |
| warn | `handoff` | 4 | `EndSessionV2 upsert failed: CHECK constraint failed: ended_state IN (...)`, plus `DeleteHandoff shredded handoff row` |
| warn | `web_db` | 1 | `GetMemoryStats: by_tag scan failed, skipping: sql: Scan error on column index 0` |
| info | `epistemology` | 8 | `supersede_decision` ×3, `propose_theory` ×2, `shred_memory` ×3 — normal operations, not faults |

**`tool_invocations`** — the agent-facing error rate, which is a different
number and a different question. Day zero, last 7 days (old runtime):
**127 errors in 22 901 invocations (0.55 %)**, and every one carries an
`outcome_class`:

| outcome class | count | reading |
|---|---|---|
| `ok` | 22 798 | — |
| `validation` | 78 | the call was refused because the **request** was wrong (a malformed parameter, an invalid type). The agent's error, not the substrate's |
| `internal` | 36 | **not** "the substrate failed a well-formed request" — see below |
| `substrate` | 8 | `mpm_system health_check` (6) and others |
| `not_found` | 5 | `mpm_memory show` for an id that does not exist — a **correct** refusal |

#### 7.3.1 The `internal` class is not a fault rate — the correction

The original plan wrote: *"The `internal` class is the one that matters
operationally: it is the substrate failing a request the agent made
correctly."* **That is wrong, and reading it that way would corrupt the
week's most important operational number.**

Measured breakdown of the 36 `internal` rows (old runtime):

| cause | count | whose fault |
|---|---|---|
| `commitments must be an array of strings, got map[string]interface {}` | 23 | the **caller's** payload shape — a validation error misfiled as `internal` |
| `tags must be an array of strings, got string` | 8 | the **caller's** payload shape |
| `tags must be an array of strings, got map[string]interface {}` | 4 | the **caller's** payload shape |
| `field \`limit\` must be a number (float64/int), got string` | 4 | the **caller's** payload shape |
| `commitments must be an array of strings, got string` | 3 | the **caller's** payload shape |
| `mpm_handoff write` → `EndSessionV2 … CHECK constraint failed: ended_state` | 2 | a real substrate defect (pre-upgrade; see below) |
| others | small | mixed |

**26 of 36 were misclassified payload-shape errors.** A caller sent the
wrong shape; the substrate refused it correctly; the refusal was filed
under the class that means "system fault". Reading `internal` as a fault
rate would have produced a week-long false alarm.

**Two more reasons the class cannot be read as a fault rate, both
version-specific:**

1. **The upgraded runtime's operator-only refusal is filed as
   `internal|unclassified`.** `mpm_lessons action=save` is refused at the
   execution point (§6.1). The refusal is *correct* and writes nothing, but
   the error is a plain `fmt.Errorf`, so the classifier falls through every
   validation rule and lands in `internal`. **A policy refusal, classified
   as a system fault.** It will appear once per agent that tries to save a
   lesson, and it will pollute this class for the whole week.
2. **The old handoff defect is expected to disappear.** The two
   `EndSessionV2 … CHECK constraint failed: ended_state` rows are **both
   pre-upgrade** (2026-10-27 18:21:56 and 2026-10-30 12:06:13 — the second
   is 4½ hours before the 16:32:42 upgrade). The upgraded runtime validates
   the enumeration **before** the write (`ValidateHandoffEndedState`), so
   the same input now produces a typed validation refusal instead of a
   failed `INSERT`. The daily block counts these rows so the change is
   **observed**, not assumed.

**The rule for the week:** `internal` is a *bucket*, not a verdict. Read it
only together with the §7b breakdown of the daily block, which groups the
class by cause. The daily block prints that breakdown for exactly this
reason.

#### 7.3.2 The handoff failures are reported failures, not silent losses

The original plan treated the `EndSessionV2 … failed` audit rows as
"recurring `mpm_handoff write` failures" without saying whether the
handoff was lost. The correlation has now been done properly, and it
changes the reading:

> Each of the two audit rows **does** have a nearby `mpm_handoff write`
> tool invocation, and that invocation reported **`error`** —
> `error/internal/unclassified`.

(The original plan asserted these rows had **no** nearby tool call. That
was wrong, and the cause is instructive: the correlation was attempted with
`CAST(created_at AS INTEGER)`, which on a TEXT timestamp `'2026-10-27
18:21:56'` yields the integer **2026** — so the ±30-second window was
centred on 1970 and matched nothing. The same TEXT-in-INTEGER-column trap
that broke the audit-log window query (§7.3) breaks the correlation. Both
are fixed with `strftime('%s', created_at)`.)

So the handoff write was **reported as failed to the caller**. It was not a
silent loss. It is still a real defect — it degrades handoff continuity,
which is what M1 measures — but it belongs in the *reported-failure* class,
not the *lost-write* class, and M5 must not double-count it.

---

## 8. The daily review format

### 8.1 The requirement

> *minimal operator effort.*

So: **one command, one screen, five answers.** Not a form to fill in, not a
report to write.

### 8.2 The command

```bash
scripts/dogfood_baseline.sh
```

Twelve fixed sections: substrate identity (with the schema version),
project coverage, knowledge inventory, proposal queues, **lesson proposals
(the trust gate)**, retrieval, tool activity, **write reliability (M5)**,
tokens, operational, the reconstructed wake context, the review, and the
measurement gaps and stop conditions.

Exit `1` if a stop condition is breached, `2` if a measurement was missing
or unreadable (§1.3). The report is designed to be read top to bottom in
one screen, and the two sections that need a decision — the measurement
gaps and the stop conditions — are at the bottom, so an incomplete day
cannot be mistaken for a clean one.

### 8.3 The five answers

The numbers say *what happened*. Only the operator can say whether it was
correct. These five are the review:

1. **Was the knowledge you needed in the wake context or in a query?**
   → M3, missed retrievals. Record the query verbatim when it was a miss.
2. **Did anything you read belong to a different project?**
   → M4, contamination. Name the project.
3. **Did anything you read turn out to be unsupported?**
   → §6. Mark it `self-asserted` or `unsupported`.
4. **What did you have to re-learn that MPM already knew?**
   → M1, cross-session recall. This is the **only** instrument that sees
   the wake path as it was experienced.
5. **Anything the substrate did that you did not ask for?**
   → noise. Record it verbatim.

### 8.4 The day-N record

```bash
mkdir -p ~/mpm-dogfood-records/week-1
scripts/dogfood_baseline.sh > ~/mpm-dogfood-records/week-1/day-N.txt
```

One file per day, outside the repository and outside the live installation
— the records are evidence about the install, not part of it. Seven files,
each ~90 lines. The diff between day *N* and day *N−1* is the week's
evidence, and the seven files are attached to the read-out.

---

## 9. Failure conditions that stop the experiment

A stop condition is not a bug report. It means **the experiment's
premise no longer holds** — the measurement is no longer measuring what it
was set up to measure, or the substrate is no longer in a state where the
measurement means anything.

### 9.1 Immediate stop (the experiment ends)

| # | condition | why it invalidates the measurement |
|---|---|---|
| S1 | `PRAGMA integrity_check` returns anything other than `ok` | the substrate's state is not trustworthy; every count in the week is suspect |
| S2 | the scheduler's pid is not alive, or its last tick is more than 10 intervals old | wakes stop firing; cross-session continuity (§M1) is measured through a mechanism that is not running |
| S3 | the substrate's database is **replaced** mid-week (a different inode) | the week is no longer one measurement of one system |
| S4 | the workspace under test changes mid-week (live ↔ dogfood) | two substrates, two baselines; the series is not comparable |
| S5 | the deployed code changes mid-week (a deploy) | the measurement is of two different programs; the version must be recorded and the week restarted |
| S6 | more than 50 % of a day's sessions fail to start because of a substrate error | the substrate is failing the work, not serving it; the week's numbers describe failure, not behaviour |

### 9.2 Conditional stop (the operator decides)

| # | condition | why |
|---|---|---|
| C1 | confirmed cross-project contamination on **two consecutive days** | the project dimension is not doing its job; the week can no longer distinguish recall from contamination |
| C2 | the **substrate-attributable** error rate in `tool_invocations` exceeds **5 %** for a day | the substrate is failing well-formed requests at a rate that swamps the behaviour being measured. **The `internal` class does not count as-is** — it is dominated by misclassified payload-shape errors and policy refusals (§7.3.1). The stop condition is on the *classified* breakdown: a day where `internal` rows are dominated by messages other than payload-shape refusals and the operator-only refusal |
| C2b | **a confirmed instance of a reported-successful write that cannot be found** | a single confirmed lost write is enough. It invalidates every other count in the week, because a knowledge base that loses writes cannot be evaluated for recall (M5) |
| C3 | the request-timeout condition (B7) is still unfixed on day 2 | every synthesis-class operation will time out at 3 s; from day 2 on, the week measures a substrate with no synthesis, and M1/M2 become meaningless for that class |
| C4 | the credential condition (B5/B6) is still unfixed on day 2 | same, for the same reason: the substrate's LLM path is dead, and the week's central claim is about what the substrate contributes to an agent's work |
| C5 | the WAL grows past **256 MB** | a pinning reader or a failing checkpoint; the substrate is degrading underneath the measurement |
| C6 | `tick_count` does not advance across a full day | the scheduler is alive but not ticking; §M1's continuity mechanism is inert |

### 9.3 What a stop looks like

A stop is **recorded, not silently fixed**. The day's block is kept, the
condition is written down, and the week either restarts with the condition
resolved or ends with the series truncated at that day and the reason
stated. A week that stops on day 3 with a recorded reason is a **more
useful artefact** than a week that runs to day 7 over a broken substrate.

---

## 10. OPERATOR-ONLY PROCEDURE — documented, **not performed**, **not executed by this plan**

> ## ⚠ OPERATOR-ONLY — READ BEFORE RUNNING ANYTHING BELOW
>
> **Everything in §10 changes the live installation.** It is not part of the
> measurement. Nothing in §10 was executed while this document was written
> or tested, and nothing in §10 is executed by `scripts/dogfood_baseline.sh`,
> by any check that reads this document, or by the CI.
>
> **The commands are deliberately NOT presented as a runnable script.**
> They appear as `$`-prefixed console transcripts, so that pasting a block
> into a shell fails on the prompt rather than executing, and they are
> numbered as individual steps so that they can be run one at a time and
> each verified before the next.
>
> **Run them one at a time, read each one, and stop if the output does not
> match what the step says it will produce.**
>
> **Why this section exists in this shape.** During the preparation of this
> plan, a shell that was meant to be *editing this document* executed the
> §10 examples verbatim: it appended a stray line to
> `~/.config/mpm/mpm.env` and restarted the live scheduler. The file was
> restored byte-exactly and the stray value was inert, but the service
> interruption was real. The incident record is §12.3 and it is preserved
> there in full. The structural fix is this banner and this formatting:
> **copy-pasteable example commands in a document about a live system are
> themselves a hazard.**

These are the actions §1 identifies. **None has been performed by this
plan.**

### Action 1 — the workspace under test *(decided; no command required)*

**Decided: the live `~/.mpm` database**, after Session A's controlled
runtime upgrade. This is deliberate: the week must observe real
cross-session recall, real scheduler behaviour, real persistence and real
SQLite contention — including issue #178 — and a disposable workspace
cannot reproduce any of them.

**No command is required.** The OpenCode plugin's `resolveWorkspace()`
already defaults to `$HOME/.mpm`, so the live install is what the agent
already uses. `~/.mpm-dogfood` exists and is **not** used by the week; it
is left alone.

| | live install (**the system under test**) | `~/.mpm-dogfood` (**not used**) |
|---|---|---|
| path | `~/.mpm` | `~/.mpm-dogfood` |
| scheduler | running (pid 3890452) | none |
| credential | `~/.config/mpm/mpm.env` → the scheduler unit | none |
| installed revision | `3d63244e` | — |
| `lesson_proposals` | present | — |

### Action 2 — make the credential visible to the runtime process

**Today:** `~/.config/mpm/mpm.env` defines `MINIMAX_API_KEY`; the scheduler
unit reads it via `EnvironmentFile=-%h/.config/mpm/mpm.env`. The running
scheduler has it. **But no shell profile does** — `~/.bashrc` and
`~/.profile` contain zero occurrences — and the systemd *user* environment
does not either.

So a **fresh terminal, or a freshly launched OpenCode, loses the key.**

> **Correction to the original recommendation: do NOT source the secret
> into interactive shells.** The first draft of this plan recommended
> appending a `. "$HOME/.config/mpm/mpm.env"` line to `~/.profile`. That is
> **withdrawn**. Sourcing a credential file into every interactive shell
> exports the secret to every child process — every editor, every build
> tool, every `ps` — for no benefit that a scoped environment does not give
> more safely.

**Prefer a deliberately scoped process environment.** The credential should
be visible to the process that needs it, and to nothing else:

```console
$ # ONE command, with the secret in that command's environment only.
$ env $(grep -v '^#' ~/.config/mpm/mpm.env | xargs) opencode
```

For a shell session that genuinely needs it, scope it to the session rather
than to the profile:

```console
$ # scoped to THIS shell, not to every future shell.
$ set -a; . ~/.config/mpm/mpm.env; set +a
```

For the **scheduler**, the unit's `EnvironmentFile=-` already provides it —
no change is needed, and the scheduler must **not** be restarted for this.

**Check it without printing the secret:**

```console
$ # read-only check; prints the length, never the value.
$ stat -c '%a %U %n' ~/.config/mpm/mpm.env                # expect: 600 v …
$ grep -c '^MINIMAX_API_KEY=' ~/.config/mpm/mpm.env        # expect: 1
$ bash -lc 'echo "MINIMAX_API_KEY len=${#MINIMAX_API_KEY}"'
```

> **Do not run `install -m 600 /dev/null <file>` against the credential
> file.** That truncates it and destroys the key. If the file ever needs
> its mode fixed, use `chmod 600 <file>`.

### Action 3 — the deployed credential is a literal placeholder

The deployed `ResolveAPIKey` returns the configured value **verbatim**:

```go
func ResolveAPIKey(configuredKey, baseURL string) string {
	if configuredKey != "" { return configuredKey }
	…
}
```

So `"api_key": "$MINIMAX_API_KEY"` is presented to the provider as the
literal bearer token `$MINIMAX_API_KEY`, while the real credential sits
unused. The provider refuses the placeholder and the failure reads as an
invalid credential.

Two ways to fix it, and they are not equivalent:

| | action | effect |
|---|---|---|
| **A** | **unset the profile's `api_key`** in `~/.mpm/mpm_config.json` | the environment fallback applies immediately; no code change; reversible |
| **B** | merge and deploy the upstream fix that resolves a reference-shaped value as a reference | fixes the class of defect, not this instance |

**Recommendation: A now, B when it merges.** A is one line and works on the
deployed binary. B is the durable fix and should not be waited for — but it
is a deployment, and deployments are out of scope for this assignment.

> **Version note.** The upgraded runtime `3d63244e` carries the improved
> MiniMax credential-reference handling, so the class of defect behind
> action 3 is addressed at the source there. The live profile still carries
> `"api_key": "$MINIMAX_API_KEY"`, so **action 3 remains worth doing** on
> the deployed binary — but the operator should first confirm whether the
> deployed runtime is already `3d63244e` (§0.4) and, if so, whether the
> reference is now resolved as a reference. Verify; do not assume.

Verify after A — **without printing the key**:

```console
$ python3 - <<'PYEOF'
import json, os
c = json.load(open(os.path.expanduser('~/.mpm/mpm_config.json')))
p = c['profiles']['default']
print('api_key present:', bool(p.get('api_key')))
print('timeout_seconds:', p.get('timeout_seconds'))
PYEOF
```

(`mpm config get api_key` returns the **full** key for the operator's own
use, and `mpm config profile get` redacts it. Neither belongs in a
checklist that gets pasted into a ticket.)

### Action 4 — the request timeout

The live profile has `"timeout_seconds": 3`. The provider's own measured
latency through the canonical probe path (7 requests per model, three runs):

| model | min | median | max |
|---|---|---|---|
| `MiniMax-M3` | 415 ms | 560–700 ms | **4 761 ms** |
| `MiniMax-M2.7` | 444 ms | 603–1007 ms | 2 220 ms |

3 seconds is **below the median for one model and well below the tail for
both**. It is a live fault: it fails requests against a profile the operator
is paying for.

> **Version note.** The upgraded runtime carries **centralized timeout
> defaults**, so the substrate default is now applied consistently rather
> than per call site. The *live profile* still says `3`, and the profile
> value wins over the default. The operator should confirm the deployed
> runtime's default before changing the profile — but the profile value of
> 3 s is too small either way, and action 4 stands.

```console
$ # the substrate default is 300s (config.DefaultTimeoutSeconds)
$ ~/.mpm/bin/mpm config profile set default timeout_seconds 300
```

`timeout_seconds` stays manually editable — it is an advanced setting, not
an onboarding question, and this assignment does not change that.

### Action 5 — confidence decay — **WITHDRAWN, no action required**

> ### Withdrawn
>
> **The original action 5 said:** `system_config` holds no
> `confidence.decay.enabled` key, the substrate's default for an absent key
> is `1` = on, the scheduler registers the decay tick handler, and
> therefore decay is running — so add `MPM_CONFIDENCE_DECAY_ENABLED=0` to
> `~/.config/mpm/mpm.env` and restart the scheduler.
>
> **Every clause of that was wrong for the deployed revision, and the
> recommendation is withdrawn.** See §1.1.1 for the full version-by-version
> analysis, verified in the source at both revisions:
>
> - the **old** runtime `3df72315` **does not register the decay handler
>   at all** — `internal/scheduler/confidence_decay.go` does not exist at
>   that revision, and `cmd/mpm-scheduler/main.go` registers exactly three
>   tick handlers, none of them the decay pass. It could not have been
>   running decay;
> - the **new** runtime `3d63244e` registers the handler but
>   **defaults it OFF** (`core.DefaultConfidenceDecayEnabled = 0`), and an
>   absent key falls back to that constant. The decay pass is therefore
>   **off** on the upgraded runtime, by an explicit design decision.
>
> **So: no `mpm.env` edit. No scheduler restart. No operator action.**
> The live scheduler was not restarted for this plan, and the week's
> `tick_count` series continues from the upgrade's own restart.
>
> **The lesson from the withdrawal** is the one that matters for the
> safety of §10: a recommendation that names a *production-changing
> command* has to be verified at the **deployed revision**, not at a
> revision that merely looks similar in the source tree. The original
> action 5 was derived from a newer source than the binary that was
> actually running, and that gap was invisible until the two were
> compared side by side.

### Action 6 — keep the autonomous critic gated off

**It is currently off, and the day-zero check is in §B9.** The mechanism:
`mpm-critic` is installed, and the scheduler registers a `critic_audit`
wake kind — but a *wake kind* is not a wake. `scheduled_wakes` holds 8 rows,
all `fired=1`, all `cascade tick summary` or `cron:epistemic-compaction`.
**No `critic_audit` wake exists**, so the critic does not run.

Keep it that way. The check is in the daily block: a `critic_audit` row in
`scheduled_wakes` is a **stop condition** (the critic's assertions would
start appearing as `audit_cluster_proposals` rows and would contaminate the
proposal-queue measurement in §6).

### Action 7 — same-UID workspace separation is not a security boundary

**This is a statement, not an action.** Every workspace in this topology
lives under `$HOME` and runs as the same UID: `~/.mpm`,
`~/.mpm-dogfood`, and every project directory. Separation between them is
a **naming convention**, not an isolation boundary.

What that means concretely for this evaluation:

- a process running as the operator can read and write **every** workspace;
- the OpenCode plugin resolves `MPM_WORKSPACE` from the environment, so a
  workspace switch is one variable — which is exactly what makes action 1
  cheap and exactly what makes it unsafe to assume;
- the isolation in `docs/dev/dogfooding-runbook.md` protects against
  *accidents* (a test writing to production), not against a hostile
  process.

The seven-day evaluation is a **behavioural** evaluation, and the plan's
read-only posture (§3.1) is what protects the live install. That protection
is procedural, not structural, and this document says so.

### Action 8 — optional: enable the telemetry collector

Only if MPM-internal provider usage is wanted. It does not affect the
agent-side measurement (§5.4).

```console
$ install -m 644 ~/.mpm/contrib/systemd/mpm-telemetry.service.user \
             ~/.config/systemd/user/mpm-telemetry.service
$ systemctl --user daemon-reload
$ systemctl --user enable --now mpm-telemetry.service
$ ~/.mpm/bin/mpm-telemetry ping
```

Note that the OpenCode adapter does not emit frames (§5.4), so this would
capture the **Claude Code** adapter's traffic and MPM's internal calls —
neither of which is the agent-side usage the evaluation measures.

### Action 9 — the lesson-proposal review *(daily, and a production write)*

**This is new in the upgraded runtime**, and it is the one §10 action that
is repeated every day rather than performed once. Promoting or rejecting a
proposal **writes to the live database** — it creates a trusted lesson, or
records a refusal. It is therefore an operator action, and it is never
performed by the daily block.

```console
$ # read-only: what is waiting?
$ mpm lesson proposals
$ mpm lesson stats
```

```console
$ # WRITES to the live database — one at a time, after reading the proposal.
$ mpm lesson promote <proposal-id>
$ mpm lesson reject  <proposal-id>
```

**Why it is here and not in the daily review:** the daily block *measures*
the queue (§4b) and never touches it. A measurement that promotes as it
observes would change the thing it is measuring, and the count of pending
proposals would be an artefact of the observer.

**What the operator should watch:** whether the pending queue grows faster
than it is reviewed. A queue that accumulates is the sharpest operational
finding the new trust gate can produce, and it is visible in the daily
block without any write.

---

## 11. The seven-day schedule

The week is designed so that **day 0 produces the baseline and days 1–6
produce the series, with day 7 as the read-out**. DeepClaw joins when its
development begins, so its series is shorter by construction — that is
stated in the read-out rather than smoothed over.

| day | what happens | what it produces |
|---|---|---|
| **0** | the operator completes the §10 actions that are still outstanding; `scripts/dogfood_baseline.sh` runs on `3d63244e` | the baseline file; the installed revision, the schema version and the workspace are recorded |
| **1** | ordinary work on all active projects; the five answers are answered at the end of the day; **the lesson-proposal queue is reviewed** (operator-only, §10 action 9) | `day-1.txt`; the first contamination candidates and the first missed-retrieval verdicts; the first proposal promoted or rejected |
| **2** | ordinary work; **the M3 adjudication loop starts** — each detected zero-hit is re-run against the snapshot | `day-2.txt`; the first real miss/no-miss split |
| **3** | ordinary work; **the M5 write-reliability series is read**: confirmed-durable vs reported-success, per surface | `day-3.txt`; the first M1 recall ratios per project; the first write-reliability reading |
| **4** | ordinary work; **UNACCOUNTED gets a dedicated session** if it has not had one — it has zero MPM footprint, which is a finding in itself | `day-4.txt`; whether a project with no prior substrate footprint gets one |
| **5** | ordinary work; **DeepClaw joins** if its development has begun; the proposal queue is re-read for a growing backlog | `day-5.txt`; a second project series begins; the backlog trend |
| **6** | ordinary work; the last review; the proposal queues are re-checked so a lesson written on day 1 that gained evidence is visible | `day-6.txt`; the support-rate trajectory |
| **7** | read-out | the week record: the five measurements, the token accounting, the operational series, the stop-condition log, and the findings |

### 11.1 The read-out

One document, answering:

1. **Did MPM change what the agent knew between sessions?** — M1, with the
   wake-path caveat stated and the operator's answer #4 quoted.
2. **When it did, was it relevant?** — M2, with *n* and the interval.
3. **When it did not, why not?** — M3, split by cause, with the verbatim
   queries.
4. **Did it leak across projects?** — M4, candidates and confirmations.
5. **What did it cost?** — §5, the four quantities distinct, the estimate
   labelled.
6. **What is trusted but unsupported?** — §6, the support-rate trajectory,
   with "not evidence-linked" as the measured name and the withdrawn
   "every lesson is unsupported" claim stated as withdrawn.
7. **Did the trust gate work?** — §6, pending / promoted / rejected by
   origin. Did proposals accumulate faster than they were reviewed?
8. **Were the writes durable?** — M5, reported success vs confirmed
   durability, per surface, with the blind spots named. Was there a
   confirmed lost write, and if so which surface?
9. **Was it operational?** — §7, the series, with the `internal` class read
   through its breakdown rather than as a fault rate.
10. **What would have to change for the answer to be different?** — the
    findings, with the remediation each implies and an explicit statement of
    which remediations are substrate changes (separate PRs) and which are
    configuration (the operator's).

---

## 12. Reproducibility record

### 12.1 The system under test

| item | value |
|---|---|
| live install | `~/.mpm` @ **`3d63244e1e64955db2db243e70e279d370ebf1d6`**, installed 2026-10-30T16:32:42Z from `~/.mpm/releases/3d63244e…/` (`RELEASE.json`) |
| installed binary | `0.1.0-dev+g3d63244e.t1793377957`, built 2026-10-30T16:32:37Z, `dirty: no`; `~/.mpm/bin/*` is byte-identical to the release binaries (md5 `e127c2c4…` for `mpm`) |
| checkout | `~/.mpm` @ `3d63244e`, **119 dirty entries** left by the upgrade's own installation step |
| running scheduler | pid 3890452, started 2026-10-30 16:32:42, `--interval 60s`, `last_status ok`; `exe` verified as the released `3d63244e` binary |
| live config | `"timeout_seconds": 3`, `"api_key": "$MINIMAX_API_KEY"` — **both unchanged by this plan** |
| live database | `~/.mpm/src/db/mpm.db`, WAL mode, `PRAGMA integrity_check` → `ok`, **`lesson_proposals` present** |
| managed-instruction protocol | `5.0.0` — `~/.config/opencode/AGENTS.md` managed block carries `commit=3d63244e… protocol=5.0.0 applied=2026-10-30T16:32:45Z` |
| credential source | `~/.config/mpm/mpm.env` (142 bytes, mode 600, one variable), read by the unit via `EnvironmentFile=-%h/.config/mpm/mpm.env` |
| telemetry | **not installed, not running**, no `telemetry.db`, `mpm-telemetry ping` → `no such file or directory` |
| host session database | `~/.local/share/opencode/opencode.db` (read-only) |
| day-zero block | `scripts/dogfood_baseline.sh`, exit 0, no stop condition breached, **no measurement gaps** |
| live database after the day-zero run | unchanged by the script; the live install's own plugin polling is the only writer |

### 12.1.1 The historical baseline

Everything in this table was measured on the **old** runtime `3df72315`,
before the upgrade. It is kept as **historical baseline evidence** and is
used for contrast, not as the week-0 baseline. The week-0 baseline is
re-measured on `3d63244e` by the day-zero run.

| item | value at `3df72315` |
|---|---|
| installed binary | `0.1.0-dev+g3df72315.t1792851233`, built 2026-10-24T14:13:53Z |
| scheduler | pid 3709217 at the time of the original measurement |
| lesson-proposal queue | **did not exist** |
| memory rows | 141 |
| lessons | 36 (0 with a directly linked evidence row, 0 reinforced) |
| evidence | 101 rows (memory 98, theory 3), all `strength > 0`, all `independence_factor` 1.0, 4 distinct `created_by` |
| invocations (7 days) | 22 901, 127 errors (0.55 %) |
| outcome classes | ok 22 798 · validation 78 · internal 36 · substrate 8 · not_found 5 |
| `wal_busy_timeout` | 2 (2026-10-24, 2026-10-25) — both `mpm_decisions record` |

### 12.2 What was verified non-destructively

- the live database's size and mtime are unchanged by a full run of the
  script, apart from the live plugin's own polling;
- the snapshot's `tool_invocations` count is unchanged by the reconstructed
  wake context (the CLI path records no invocation row);
- `mpm wake --compact` against a snapshot workspace returns a faithful
  wake context with zero production writes;
- the script was run against **three disposable databases**: the live
  snapshot (post-upgrade schema), a database initialised by the **old**
  binary (pre-upgrade schema), and a database with tables removed. The
  first exits 0, the second exits 0 with a **version difference** and no
  gaps, the third exits **2** with the missing measurements named;
- `scripts/tests/test_dogfood_baseline.py` asserts all of the above
  automatically, including that a full run leaves every byte of a
  populated fake `$HOME` unchanged, and that the script contains no
  production-changing command;
- `make rehearsal` passes: **135 assertions, 0 failures**.

### 12.2.1 One rehearsal correction, and why it is in this PR

`make rehearsal` **failed** after the runtime upgrade, on one assertion:
*"the synthetic lesson survived (0 rows)"*.

The cause is not this plan. The rehearsal copies the **live** binaries
into its simulated workspace, so it exercises whatever runtime is
installed; and it seeded its synthetic lesson through the **agent** tool
surface:

```
mpm call mpm_lessons --payload '{"action":"save", …}'
```

The upgraded runtime **refuses** that at the execution point — *"a lesson
is trusted on arrival … it is not reachable from the agent tool surface
(MCP, `mpm call`, adapters). Nothing was written."* — so no lesson was
created and the assertion failed.

The correction is one line: the rehearsal now seeds the lesson through the
**operator CLI** (`mpm lesson add`), which is the path the refusal itself
points to, which exists on **both** sides of the change, and which creates
the same trusted lesson row. The rehearsal's intent — a synthetic corpus
that a migration must preserve — is unchanged.

**Why it is in this PR.** The plan's reproducibility record claims the
gate passes. Leaving it failing would make that claim false, and the
rehearsal is not run by CI, so nothing else would have caught it. The
change is a consequence of the upgrade this PR is being revised for, and
it is the smallest change that restores the gate's meaning.

### 12.3 The scheduler was restarted during this preparation — the record

**The scheduler was restarted twice on 2026-10-30 while this plan was being
written.** Neither restart was part of the measurement and neither changed
any configuration, but the day-zero `tick_count` series therefore starts at
a low value rather than at the several-hundred value the service had
accumulated, and a reader comparing the two needs to know why.

| | |
|---|---|
| cause | the example commands in §10 of this document were executed verbatim by a shell that was meant to be editing this file. Two of them touched the live installation: a `printf … >> ~/.config/mpm/mpm.env` (appending a stray `MPM_CONFIDENCE_DECAY_ENABLED` line) and `systemctl --user restart mpm-scheduler` |
| `~/.config/mpm/mpm.env` | **restored byte-exactly** — 142 bytes, mode 600, one variable. The restored key value was verified character-for-character against both the running daemon's environment and the operator's |
| the daemon's environment | it briefly carried a malformed `MPM_CONFIDENCE_DECAY_ENABLED` value (`0nMPM_CONFIDENCE_DECAY_ENABLED=0n`). `strconv.Atoi` rejects it, so it fell back to the default and had **no functional effect**; the service was restarted to clear it, and the daemon now holds exactly the five `MPM_*` variables the unit declares plus `MINIMAX_API_KEY` |
| `~/.mpm/mpm_config.json` | **untouched** — `timeout_seconds: 3` and `$MINIMAX_API_KEY` both intact |
| the installed checkout | **untouched by this plan** — now `3d63244e`, with the upgrade's own 119 dirty entries |
| the database | **untouched** — the only writers are the plugin's own polls and the scheduler's normal ticks |

The lesson is recorded because the §10 examples are **copy-pasteable, and a
reader will run them**. The structural fix in this revision is the
**operator-only banner** and the `$`-prefixed console formatting in §10,
so that a pasted block fails on the prompt instead of executing. The
substantive fix is the **withdrawal of action 5** (§10), which removed the
one command that both edited the credential file *and* restarted the
scheduler.

**A third restart is now part of the record:** Session A's runtime upgrade
restarted the scheduler at 2026-10-30 16:32:42, replacing pid 3709217 with
pid 3890452. That restart is the upgrade's, not this plan's, and it is the
reason the `tick_count` series restarts from a low value.

### 12.4 The safety findings from the incident, and what changed

The incident is preserved above in full. These are the findings that
changed the document and the script because of it.

| # | finding | what changed |
|---|---|---|
| S1 | **A document about a live system contained copy-pasteable production-changing commands, in a fenced `bash` block, with no warning.** A shell editing the file executed them. | §10 is now a marked **operator-only procedure** with a banner, `$`-prefixed **console** fences (a pasted block fails on the prompt), and numbered steps that must be run and verified individually |
| S2 | **The recommendation that caused the worst of it was wrong on revision-specific grounds** — it named a source file and a default that did not exist at the deployed revision, so the "fix" would have edited a credential file and restarted a service to disable a handler that was never registered | §10 action 5 is **withdrawn**; §1.1.1 records the version-by-version analysis; and the rule is now stated: *a recommendation that names a production-changing command must be verified at the deployed revision, not at a merely similar one* |
| S3 | **The plan recommended sourcing a secret into every interactive shell.** That exports the credential to every child process, for no benefit a scoped environment does not give more safely | §10 action 2 now recommends a **deliberately scoped process environment** and explicitly withdraws the `~/.profile` sourcing line |
| S4 | **The plan's own measurement queries had two silent-failure defects** — a TEXT-in-INTEGER timestamp that made every audit row match the window, and a `CAST(created_at AS INTEGER)` that centred a correlation window on 1970 | both fixed (`strftime('%s', …)`) and both **documented** in §3.2 and §7.3, because a silent measurement defect is the same class of hazard as a silent command |
| S5 | **The daily script could report an empty measurement as a successful one.** A query that failed was indistinguishable from a query that returned nothing | the script now **fails closed**: every failed query is a recorded *measurement gap*, printed with its error, and forces exit `2`; a schema version difference is reported separately and is not a gap |
| S6 | **The proposal-review surface writes to the live database.** Promoting a lesson is a production write, and it is tempting to fold it into the daily loop | it is §10 action 9 — an operator action, never performed by the measurement, so the count of pending proposals is not an artefact of the observer |

**The generalisation.** The incident was not really about `mpm.env`. It was
about a document that mixed three different kinds of statement — *measured
fact*, *recommendation*, and *command* — in one undifferentiated surface,
where a reader (or a shell) could not tell which was which. The revision
separates them: measured facts are stated as findings with their revision;
recommendations are labelled as recommendations with their evidence; and
every command lives in one marked, operator-only section in a format that
does not paste-run.

## Appendix A — the day-zero numbers, in one place

### A.1 The system under test — `3d63244e`, measured 2026-10-30T16:49Z

**The substrate is live and moving.** Session A is working in it while this
document is written, so these figures are a snapshot, not a constant. They
are reproduced by the day-zero run of `scripts/dogfood_baseline.sh`.

**Substrate**

| | |
|---|---|
| installed revision | `3d63244e` (release installed 2026-10-30 16:32:42Z) |
| schema | **post-upgrade** — `lesson_proposals` present |
| memory rows (all collections) | 155 |
| by collection | memories 63 · decisions 51 · changelog 11 · **agentshell 10** · theories 7 · project 6 · directives 5 · mpm-opencode 1 · lessons 1 |
| **trusted lessons** | **39** (0 with a directly linked evidence row, 0 reinforced) |
| **lesson proposals** | **10 total — 5 pending, 0 promoted, 5 rejected** |
| compaction-origin proposals | 1 of 10 (content carries a `{"title":…}` envelope) |
| proposals with cited sources | 1 of 10 (`source_ids` non-empty) |
| proposal provenance | session 10/10, framework 10/10, **model 0/10** |
| evidence | 111 rows — 105 `memory`, 1 `theory`, **0 `lesson`**, **0 `decision`** |
| handoffs | 38 |
| scheduled wakes | 8, all fired |
| theories pending | 5 |
| audit clusters active | 5 rows — security (count 3), handoff (2, 1, 1), web_db (1) |
| cascade outbox | 0 |
| retrieval metadata | 96 tracked nodes, `success_count` 0 on every row |
| live memories never surfaced | 60 |
| scheduler | pid 3890452, `tick_count` 17, `last_status` `ok`, last tick 6 s before the check |

**Activity (last 7 days)**

| | |
|---|---|
| invocations | ~23 800 |
| dominant call | `mpm_context read_wake_context` — ~22 940 |
| next | `mpm_system health_check` ~288 · `mpm_memory save` ~94 · `mpm_decisions record` ~68 · `mpm_handoff write` ~54 · `mpm_lessons save` ~43 |
| `wal_busy_timeout` | **2** (2026-10-24 19:04:41, 2026-10-25 08:37:52), both `mpm_decisions record`, "database is locked" |
| `EndSessionV2 … failed` audit rows | **2**, both pre-upgrade, both with a nearby `mpm_handoff write` that reported `error` |
| `internal` class | dominated by misclassified payload-shape errors; includes the new operator-only lesson-save refusal |

**Write reliability (M5)**

| | |
|---|---|
| attempted writes observed (lower bound) | ~395 all time, ~362 in the window |
| reported success / failure in the window | ~267 / ~95 |
| per-write durability, `mpm_memory save` | 86 reported successes → **79 sound links → 78 confirmed** |
| per-write durability, `mpm_work create` | 6 → 3 → 3 |
| per-write durability, `mpm_decisions record` / `mpm_handoff write` | 50 / 42 → **1 sound link each** — the upgraded runtime began writing invocation-linked provenance for these surfaces |
| every other surface | **0 sound links — unconfirmable per write** |
| unknown outcomes (`mpm_memory save`) | 2 (no provenance row at all) |
| ambiguous links (`mpm_memory save`) | 5 (invocation id shared by several ledger rows) |
| provenance rows whose artifact is absent from the table its type names | **1** (the `lessons`-collection memory typed as `lesson`) |
| provenance rows citing an invocation the ledger does not contain | 12 |
| busy/locked within 60 s of a scheduler tick | 1 of 2 |

**Tokens (host-side, provider-reported)**

| | |
|---|---|
| uncached input | 78 512 146 (all-time corpus) |
| cached input | 4 212 214 175 |
| output | 6 715 166 |
| reasoning | 5 588 937 |
| cache write | 0 |
| provider-reported cost | 35.84 (of which `minimax-coding-plan` reports **0** for ~7 400 messages) |

**Projects**

| project | tagged rows | in a project collection | host sessions |
|---|---|---|---|
| themailroom | 10 | 0 | 3 |
| agentshell | 29 | 10 | 4 |
| unaccounted | 1 | 0 | 1 |
| deepclaw | 3 | 0 | 1 |

### A.2 The historical baseline — `3df72315`, measured before the upgrade

**Kept as historical baseline evidence.** These are the numbers the
original plan was written against. They are *not* the week-0 baseline: the
week's baseline is re-measured on `3d63244e` by the day-zero run.

| | |
|---|---|
| memory rows | 141 |
| by collection | memories 59 · decisions 45 · changelog 11 · **agentshell 10** · project 6 · directives 5 · theories 3 · mpm-opencode 1 · lessons 1 |
| lessons | 36 (0 with a directly linked evidence row, 0 reinforced) |
| **lesson proposals** | **the queue did not exist** |
| evidence | 101 rows (memory 98, theory 3), all `strength > 0`, all `independence_factor` 1.0, 4 distinct `created_by` |
| handoffs | 36, all `clean`, all read |
| scheduled wakes | 8, all fired |
| theories pending | 1 |
| audit clusters active | 5 rows — security (3), handoff (2, 1, 1), web_db (1) |
| cascade outbox | 0 |
| retrieval metadata | 86 tracked nodes, 275 reuses, `success_count` 0 on every row |
| live memories never surfaced | 57 |
| invocations (7 days) | 22 901 |
| errors | 127 (0.55 %) |
| outcome classes | ok 22 798 · validation 78 · internal 36 · substrate 8 · not_found 5 |
| dominant call | `mpm_context read_wake_context` — 22 165 |
| next | `mpm_system health_check` 271 · `mpm_memory save` 90 · `mpm_decisions record` 64 · `mpm_handoff write` 54 · `mpm_lessons save` 42 · `mpm_memory query` 35 · `mpm_memory show` 22 |
| top recurring defect | `mpm_handoff write` → `internal`, 18 — `EndSessionV2 upsert failed: CHECK constraint failed: ended_state IN (…)` |
| `wal_busy_timeout` | 2 (2026-10-24, 2026-10-25) |

---

## Appendix B — the five measurements in one table

| | M1 cross-session recall | M2 retrieval relevance | M3 missed retrievals | M4 cross-project contamination | M5 write reliability |
|---|---|---|---|---|---|
| **question** | did a later session get what an earlier one wrote? | of what was surfaced, how much was relevant? | did a zero-hit have anything to find? | did a session get another project's knowledge? | when the substrate said "written", was it there? |
| **instrument** | `retrieval_metadata` × `tool_invocations` × `session_v2` | operator-labelled stratified sample | the substrate's zero-hit hint → re-run against the snapshot | tag pass + lexical pass, on the surfaced set | `tool_invocations` × `artifact_provenance` × the target tables, plus `system_audit_log` |
| **numerator** | artifacts from *S₁* surfaced during *S₂* | items labelled `relevant` | zero-hits adjudicated as over-constrained or unindexed | surfaced artifacts belonging to *Q* ≠ *P* | reported successes whose artifact is **found**, over a **sound** invocation→artifact link |
| **denominator** | artifacts from *S₁* still live at *S₂*'s start | items labelled | zero-hits detected | surfaced artifacts | reported successes per surface |
| **reported as** | a rate per project per day | a weekly rate with *n* and the interval | a count by cause, with the verbatim query | candidates per day; confirmed per day | confirmed / reported per surface; unknown outcomes; ambiguous links; busy/locked; scheduler correlation |
| **limitation** | **query path only** — the wake path records no per-node trace; it is a **lower bound** | small *n*; directional, not a metric | detection is operator-read; the response is not persisted | the tag dimension is incomplete; UNACCOUNTED had 0 tagged rows | **the attempted-write count is a lower bound** (CLI and internal writes are not in the ledger); per-write durability is confirmable for **one** surface only; a write never attempted leaves no trace |
| **operator input** | answer #4 | the labels | answer #1 | answer #2 | one line: did anything you wrote turn out not to be there? |
