# OpenCode + MPM dogfood — day-zero readiness and seven-day measurement plan

> **Prepared:** 2026-10-30. **Scope:** Session C — behavioural evaluation.
> **Status:** the plan is ready; **the evaluation has NOT been started** and
> the deployed installation has **not** been modified.
>
> Every number in this document was measured on the live substrate on
> 2026-10-30 and can be reproduced with the commands shown. Nothing here is
> estimated unless it says so.

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
- **The deployed installation is not modified.** `~/.mpm` remains at
  `3df7231`, clean, with its configuration untouched. The day-zero
  verification in §1 is the evidence.
- **All measurement is read-only.** §3's queries run against a **snapshot**
  of the substrate database, produced with `sqlite3 -readonly … .backup`.
  The snapshot is the only thing queried, and the live database is never
  opened for writing at any point.
- **The operator actions in §10 are documented, not performed.** They are
  configuration and deployment changes to the live install, and they are the
  operator's to make.

### 0.3 What this document is not

It is not a second analytics subsystem. There is no new schema, no new
persisted measurement, and no second place where a number is defined.

Everything is either:

- a `SELECT` against an authoritative table (the substrate's or the host's),
- the substrate's own `mpm wake --compact`, run against a snapshot, or
- one of the five counts the operator reads off a transcript (§8.3).

The one new artefact is `scripts/dogfood_baseline.sh`, which prints all of
it in one fixed format. It is a **read-only query runner**, not a system.

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
| B2 | The scheduler is alive | pid 3709217, `--interval 60s`, `last_status` `ok`, last tick 22 s before the check. The `tick_count` series starts from 1 because the service was restarted during this preparation (§12.2) | ✅ pass |
| B3 | The substrate is the one the agent uses | the OpenCode plugin's `resolveWorkspace()` falls back to `$HOME/.mpm`; the running OpenCode processes have **no** `MPM_WORKSPACE`, so the live install **is** the substrate | ✅ pass (with the consequence in B4) |
| B4 | The workspace choice is deliberate | **not decided.** `~/.mpm-dogfood` exists and was used by a session on 2026-10-29 (443 `opencode` invocations, last at 15:31); the live install had 363 at the time of the check, last at 2026-10-30 12:06 | ⛔ **operator decision required — §10, action 1** |
| B5 | The credential is visible to the runtime process | `MINIMAX_API_KEY` is defined in `~/.config/mpm/mpm.env`, which the scheduler unit reads via `EnvironmentFile=-%h/.config/mpm/mpm.env`. It is in **no** shell profile (`~/.bashrc`, `~/.profile`: 0 hits) and **not** in the systemd user environment | ⛔ **partial — §10, action 2** |
| B6 | The deployed credential is not a placeholder | the deployed `ResolveAPIKey` returns the configured value **verbatim**, so `"api_key": "$MINIMAX_API_KEY"` is sent to the provider as a literal bearer token | ⛔ **§10, action 3** |
| B7 | The request timeout can carry a real generation | the live profile has `"timeout_seconds": 3`, and the provider's own median latency is 560–1007 ms with a tail to 4761 ms | ⛔ **§10, action 4** |
| B8 | Confidence decay is off | `system_config` has **no** `confidence.decay.enabled` key, and the substrate's documented default for an absent key is `1` = **on** (`GetConfigInt(key, 1)`) | ⛔ **discrepancy — §10, action 5** |
| B9 | The autonomous critic is gated off | `mpm-critic` is installed and the scheduler registers a `critic_audit` wake kind, but `scheduled_wakes` holds 8 rows — all `fired=1`, all `cascade tick summary` or `cron:epistemic-compaction`. **No `critic_audit` wake exists**, so the critic does not run | ✅ pass (keep it that way — §10, action 6) |

### 1.2 Advisory

| # | condition | measured | note |
|---|---|---|---|
| A1 | The telemetry collector is running | **it is not.** no `mpm-telemetry.service` in `~/.config/systemd/user/`, no socket at `~/.mpm/runtime/mpm-telemetry.sock`, no `~/.mpm/src/db/telemetry.db`; `mpm-telemetry ping` → `no such file or directory` | provider-reported usage through MPM's own telemetry is therefore unavailable. §5.4 uses the host's counters instead, which are available |
| A2 | A stale scheduler state file exists | `~/.mpm/run/scheduler [conflicted].state` — `tick_count` 414, pid 7575, last tick 2026-10-28 23:44 | a leftover from a merged state swap. Harmless today; it is the shape a **stale-state adoption** bug would take, so it is checked daily (§7.1) |
| A3 | Recurring `mpm_handoff write` failures | 18 `internal` errors in the last 7 days, matching 4 `warn handoff — EndSessionV2 upsert failed: CHECK constraint failed: ended_state IN (...)` audit rows | a real defect the week will keep hitting; it degrades handoff continuity, which is exactly what M1 measures |
| A4 | `mpm_memory query` internal errors | 4 in the last 7 days | small; watch |
| A5 | Recurring `health_check` substrate errors | 6 in the last 7 days | watch |
| A6 | No lesson has ever been reinforced | `lessons.reinforcement_count` is `1` for all 36 rows | see §6 — this is the finding, not a blocker |
| A7 | No lesson has any evidence | 0 of 36 lessons have an `evidence` row | §6 |
| A8 | Same-UID separation is not a security boundary | every workspace in this topology is under `$HOME` and runs as the same UID | documented in §10, action 7 |

### 1.3 The day-zero command

```bash
scripts/dogfood_baseline.sh
```

One command, ~90 lines of fixed-format output, exit 1 if any stop condition
is breached. It is read-only by construction (§0.2). The day-zero run's
output is the reference against which day 1 is compared.

---

## 2. The project-to-collection mapping

### 2.1 What the live substrate actually holds

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

SELECT COUNT(*) FROM lessons;
SELECT COUNT(*) FROM evidence;
SELECT COUNT(*), SUM(read_at IS NOT NULL) FROM session_handoffs;
SELECT COUNT(*), SUM(fired) FROM scheduled_wakes;
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
SELECT datetime(created_at,'unixepoch'), level, component, message
  FROM system_audit_log
  WHERE created_at >= :since AND level IN ('warn','error','fatal','critical')
  ORDER BY created_at DESC;
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
# {"last_tick_unix":…,"process_started_unix":…,"tick_count":1,"pid":3709217,
#  "last_status":"ok","last_error":""}
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

## 4. The four measurements

Each measurement states the question, the instrument, the numerator and the
denominator, **what it does not measure**, and how it is reported.

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

## 6. The proposal queues, and the supported/unsupported boundary

### 6.1 There is no lesson-proposal queue — and that is the finding

The brief asks how to inspect the lesson-proposal queue. The honest answer:

> **There isn't one. A lesson is written straight into the durable store
> with no review gate.**

`mpm_lessons` has six actions — `save`, `search`, `list`, `delete`,
`restore`, `shred` — and **no** `propose`, `review` or `promote`. `save`
calls `dm.AddLesson` and the row is live immediately. There is no pending
state, no reviewer, and no queue.

That makes the brief's real question sharper, not moot: **with no review
gate, every lesson is trusted on write, so the question is whether the
substrate can tell a supported lesson from an unsupported one after the
fact.** It can, partially — and the day-zero numbers are stark:

| fact | measured |
|---|---|
| lessons in the corpus | **36** |
| lessons with **any** evidence row | **0** |
| lessons with `reinforcement_count > 1` | **0** (all are exactly 1) |
| evidence rows overall | 101 — 98 for `memory`, 3 for `theory`, **0** for `lesson`**, 0 for `decision`** |
| distinct `created_by` across all 101 evidence rows | **4** |
| `independence_factor` across all evidence rows | **1.0 for every row** |

**Every lesson in the corpus is, by the substrate's own evidence model, an
unsupported assertion.** Not one has been corroborated, and not one has
been re-asserted.

### 6.2 The three queues that do exist

| queue | where | day-zero | action |
|---|---|---|---|
| **theories awaiting a resolution decision** | `mpm_theories list status=pending` (CLI: `mpm theory pending`) — `memories.collection='theories'` with `metadata.status='pending'` | **1** pending | `mpm_theories resolve` (CLI: `mpm theory resolve`) |
| **operator review queue** | `audit_cluster_proposals` where `status='active'` | **5 rows** — security (occurrence count 3), handoff (2, 1, 1), web_db (1) | `mpm_system` actions `list_clusters` / `snooze_cluster` / `unsnooze_cluster` / `resolve_cluster` / `annotate_cluster` |
| **pending invalidation obligations** | `epistemic_cascade_outbox` where `status='pending'` | **0** | automatic |

The theory queue is the closest analogue to the "lesson-proposal queue" the
brief names, and it is a **better** model: a theory is explicitly a
hypothesis awaiting evidence, with a resolution action. A lesson is not.

### 6.3 Useful knowledge vs unsupported assertion — the criteria

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

**The classification an item gets:**

| class | test | day-zero count (lessons) |
|---|---|---|
| **supported** | ≥ 1 evidence row, `strength > 0`, `created_by` ≠ the asserting session | 0 of 36 |
| **corroborated** | ≥ 2 evidence rows from different `created_by` | 0 of 36 |
| **self-asserted** | evidence exists but `created_by` = the asserting session | 0 of 36 |
| **contradicted** | an evidence row with `strength < 0`, or a `supersede` trigger | 0 of 36 |
| **unsupported** | no evidence row | **36 of 36** |

### 6.4 The operator's per-item surface

```bash
mpm why <id>      # evidence + confidence + retrieval provenance in one view
```

That is the surface the daily review uses when an item is flagged. It is a
read; it changes nothing.

### 6.5 What this means for the week

The corpus as it stands would fail a strict support test almost entirely.
**That is not a blocker — it is the baseline.** The week's question is
whether the *rate* changes: does knowledge written during ordinary
development accumulate support, or does it accumulate as bare assertions?

The daily block reports `lessons without evidence: N of M` every day. The
number to watch is the **new** lessons written each day and how many of them
gain evidence later. A lesson written on day 1 that still has zero evidence
on day 7 is a **stale assertion** — and that is the sharpest finding the
week can produce.

---

## 7. Operational checks

All of these are in the daily block. This section says what each one means
and what a failure indicates.

### 7.1 Scheduler stability

The scheduler's own heartbeat is the single most useful operational signal,
and it is a **file**, not a query:

```bash
cat ~/.mpm/run/scheduler.state
# {"last_tick_unix":1793375308,"process_started_unix":1793375248,
#  "tick_count":1,"pid":3709217,"last_status":"ok","last_error":""}
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
SELECT datetime(created_at,'unixepoch'), level, component, message
  FROM system_audit_log
  WHERE created_at >= :since AND level IN ('warn','error','fatal','critical')
  ORDER BY created_at DESC;
```

The day-zero corpus is 16 rows and reads as a small, specific list:

| level | component | count | what |
|---|---|---|---|
| error | `security` | 3 | `sensitive content blocked` (2026-10-29) — the toxicity filter refusing content |
| warn | `handoff` | 4 | `EndSessionV2 upsert failed: CHECK constraint failed: ended_state IN (...)`, plus `DeleteHandoff shredded handoff row` |
| warn | `web_db` | 1 | `GetMemoryStats: by_tag scan failed, skipping: sql: Scan error on column index 0` |
| info | `epistemology` | 8 | `supersede_decision` ×3, `propose_theory` ×2, `shred_memory` ×3 — normal operations, not faults |

**`tool_invocations`** — the agent-facing error rate, which is a different
number and a different question. Day zero, last 7 days: **127 errors in
22 901 invocations (0.55 %)**, and every one carries an `outcome_class`:

| outcome class | count | reading |
|---|---|---|
| `ok` | 22 798 | — |
| `validation` | 78 | the call was refused because the **request** was wrong (a malformed parameter, an invalid type). The agent's error, not the substrate's |
| `internal` | 36 | the substrate failed on a well-formed request. **The substrate's error** — dominated by `mpm_handoff write` (18) and `mpm_decisions record` (10) |
| `substrate` | 8 | `mpm_system health_check` (6) and others |
| `not_found` | 5 | `mpm_memory show` for an id that does not exist — a **correct** refusal |

The `internal` class is the one that matters operationally: it is the
substrate failing a request the agent made correctly. At day zero it is
**36 per week**, and 18 of them are one recurring handoff defect (§A3).

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

Ten fixed sections: substrate identity, project coverage, knowledge
inventory, proposal queues, retrieval, tool activity, tokens, operational,
the reconstructed wake context, and the stop conditions. Exit 1 if a stop
condition is breached.

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
| C2 | the `internal` error rate in `tool_invocations` exceeds **5 %** for a day | the substrate is failing well-formed requests at a rate that swamps the behaviour being measured |
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

## 10. Operator actions — documented, **not performed**

These are the actions §1 identifies. **None has been performed.** Each is
written so it can be executed without re-deriving anything.

### Action 1 — decide the workspace under test *(blocking)*

Two candidates, both live:

| | live install | shared dogfood workspace |
|---|---|---|
| path | `~/.mpm` | `~/.mpm-dogfood` |
| scheduler | running (pid 3709217) | **none** |
| credential | `~/.config/mpm/mpm.env` → the scheduler unit | none |
| telemetry | collector absent | absent |
| `opencode` invocations | 363, last 2026-10-30 12:06 | 443, last 2026-10-29 15:31 |
| knowledge | memories 59 · decisions 45 · agentshell 10 · … | memories 59 · decisions 38 · changelog 23 · … |

**Recommendation: the live install (`~/.mpm`).** Three reasons: the brief's
configuration conditions (§B5–B8) are live-install conditions; the
scheduler, the credential and the telemetry all live there, and **M1's
continuity mechanism does not exist without a scheduler**; and the
same-UID separation buys less than it costs (action 7).

The plugin's `resolveWorkspace()` already defaults to `$HOME/.mpm`, so
**choosing the live install requires no change at all** — the current
wiring already does it. Choosing the dogfood workspace means exporting
`MPM_WORKSPACE=~/.mpm-dogfood` for every agent session, and accepting that
no scheduler runs there.

### Action 2 — make the credential visible to the runtime process

**Today:** `~/.config/mpm/mpm.env` defines `MINIMAX_API_KEY`; the scheduler
unit reads it via `EnvironmentFile=-%h/.config/mpm/mpm.env`. The running
scheduler has it. **But no shell profile does** — `~/.bashrc` and
`~/.profile` contain zero occurrences — and the systemd *user* environment
does not either.

So a **fresh terminal, a freshly launched OpenCode, or a scheduler restart
outside the unit's env-file path loses the key.**

The file already exists, is mode 600, and defines the variable once.
**Check it; do not rewrite it** — `install -m 600 /dev/null <file>` would
truncate the key away:

```bash
# the check (read-only)
stat -c '%a %U %n' ~/.config/mpm/mpm.env                # expect: 600 v ...
grep -c '^MINIMAX_API_KEY=' ~/.config/mpm/mpm.env        # expect: 1

# the addition, if it is not already sourced by interactive shells
grep -q 'mpm.env' ~/.profile || printf '\n[ -f "$HOME/.config/mpm/mpm.env" ] && . "$HOME/.config/mpm/mpm.env"\n' >> ~/.profile
```

Verify without printing the secret:

```bash
bash -lc 'echo "MINIMAX_API_KEY len=${#MINIMAX_API_KEY}"'
# expect: MINIMAX_API_KEY len=125
```

### Action 3 — the deployed credential is a literal placeholder

The deployed `ResolveAPIKey` (at `3df7231`) returns the configured value
**verbatim**:

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
| **B** | merge and deploy PR #187 (`fix/onboarding-timeout-default`), which resolves a reference-shaped value as a reference | fixes the class of defect, not this instance |

**Recommendation: A now, B when it merges.** A is one line and works on the
deployed binary. B is the durable fix and should not be waited for — but it
is a deployment, and deployments are out of scope for this assignment.

Verify after A — **without printing the key**:

```bash
python3 - <<'PYEOF'
import json, os
c = json.load(open(os.path.expanduser('~/.mpm/mpm_config.json')))
p = c['profiles']['default']
print('api_key present:', bool(p.get('api_key')))
print('timeout_seconds:', p.get('timeout_seconds'))
PYEOF
# expect: api_key present: False
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

```bash
# the substrate default is 300s (config.DefaultTimeoutSeconds)
~/.mpm/bin/mpm config profile set default timeout_seconds 300
```

`timeout_seconds` stays manually editable — it is an advanced setting, not
an onboarding question, and this assignment does not change that.

### Action 5 — confidence decay is **not** currently disabled

The brief states decay remains disabled. The measurement says otherwise:
`system_config` holds **no** `confidence.decay.enabled` key, and
`GetConfigInt(key, 1)` returns the **default of 1 = on** for an absent key
(`internal/core/db.go:4507`). The scheduler registers the decay tick
handler (`internal/scheduler/confidence_decay.go`), so with the key absent
the pass is *enabled* and is gated only by its own 6-hour cadence and the
24-hour minimum age.

`confidence_history` currently holds no `decay_tick` rows — only
`evidence_added` (3) and `supersede` (3) — so nothing has been recomputed
by decay **yet**. That is a reason to fix it now, not to assume it is off.

Two ways, and the environment one is preferred because it needs no database
write:

```bash
# preferred — the scheduler reads MPM_<SECTION>_<SETTING> (db.go:4523)
grep -q '^MPM_CONFIDENCE_DECAY_ENABLED=' ~/.config/mpm/mpm.env || \
  printf 'MPM_CONFIDENCE_DECAY_ENABLED=0\n' >> ~/.config/mpm/mpm.env

# the environment is read at process start, so the wake executor must be
# restarted for the change to take effect
systemctl --user restart mpm-scheduler
```

(The restart is a service action, not a data write. It interrupts wake
dispatch for the duration of the restart, which is why it belongs on day 0
rather than mid-week.)

There is **no CLI for this key**: `mpm config set` covers `model`,
`base_url`, `timeout_seconds` and `synthesis_enabled` only, and
`confidence.decay.enabled` is a `system_config` row, not a profile field.
The environment override is the practical route precisely because it needs
no database write. The alternative is a `system_config` write through the
substrate's own API, which is a **production data write** and is therefore
outside this assignment's boundaries.

The env override is read by `getConfigIntFromEnv` (`db.go:4523`), which
consults `MPM_<SECTION>_<SETTING>` **only when the `system_config` row is
absent** — so the env route works today, and the `system_config` row
remains the fallback if the env var is ever removed.

### Action 6 — keep the autonomous critic gated off

**It is currently off, and the day-zero check is in §B9.** The mechanism:
`mpm-critic` is installed, and the scheduler registers a `critic_audit`
wake kind — but a *wake kind* is not a wake. `scheduled_wakes` holds 8 rows,
all `fired=1`, all `cascade tick summary` or `cron:epistemic-compaction`.
**No `critic_audit` wake exists**, so the critic does not run.

Keep it that way. The check is in the daily block: a `critic_audit` row in
`scheduled_wakes` is a **stop condition** (the critic's assertions would
start appearing as `audit_cluster_proposals` rows and would contaminate the
proposal-queue measurement in §6.2).

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

```bash
install -m 644 ~/.mpm/contrib/systemd/mpm-telemetry.service.user \
             ~/.config/systemd/user/mpm-telemetry.service
systemctl --user daemon-reload
systemctl --user enable --now mpm-telemetry.service
~/.mpm/bin/mpm-telemetry ping
```

Note that the OpenCode adapter does not emit frames (§5.4), so this would
capture the **Claude Code** adapter's traffic and MPM's internal calls —
neither of which is the agent-side usage the evaluation measures.

---

## 11. The seven-day schedule

The week is designed so that **day 0 produces the baseline and days 1–6
produce the series, with day 7 as the read-out**. DeepClaw joins when its
development begins, so its series is shorter by construction — that is
stated in the read-out rather than smoothed over.

| day | what happens | what it produces |
|---|---|---|
| **0** | the blocking conditions in §1.1 are resolved by the operator; `scripts/dogfood_baseline.sh` runs | the baseline file; the confirmed workspace and the confirmed code version are recorded |
| **1** | ordinary work on all active projects; the five answers are answered at the end of the day | `day-1.txt`; the first contamination candidates and the first missed-retrieval verdicts |
| **2** | ordinary work; **the M3 adjudication loop starts** — each detected zero-hit is re-run against the snapshot | `day-2.txt`; the first real miss/no-miss split |
| **3** | ordinary work | `day-3.txt`; the first M1 recall ratios per project |
| **4** | ordinary work; **UNACCOUNTED gets a dedicated session** if it has not had one — it has zero MPM footprint, which is a finding in itself | `day-4.txt`; whether a project with no prior substrate footprint gets one |
| **5** | ordinary work; **DeepClaw joins** if its development has begun | `day-5.txt`; a second project series begins |
| **6** | ordinary work; the last review; the proposal queues are re-checked so a lesson written on day 1 that gained evidence is visible | `day-6.txt`; the support-rate trajectory |
| **7** | read-out | the week record: the four measurements, the token accounting, the operational series, the stop-condition log, and the findings |

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
6. **What is trusted but unsupported?** — §6, the support-rate trajectory.
7. **Was it operational?** — §7, the series.
8. **What would have to change for the answer to be different?** — the
   findings, with the remediation each implies and an explicit statement of
   which remediations are substrate changes (separate PRs) and which are
   configuration (the operator's).

---

## 12. Reproducibility record

| item | value |
|---|---|
| live install | `~/.mpm` @ `3df7231`, `git status --porcelain` → 0 entries |
| live config | `"timeout_seconds": 3`, `"api_key": "$MINIMAX_API_KEY"` — **both unchanged** |
| live database | `~/.mpm/src/db/mpm.db`, WAL mode, 22.6 MB + 4.0 MB WAL, `PRAGMA integrity_check` → `ok` |
| scheduler | pid 3709217, `--interval 60s`, `last_status` `ok`; the `tick_count` series starts at 1 (§12.2) |
| credential source | `~/.config/mpm/mpm.env`, read by the unit via `EnvironmentFile=-%h/.config/mpm/mpm.env` |
| telemetry | **not installed, not running**, no `telemetry.db`, `mpm-telemetry ping` → `no such file or directory` |
| host session database | `~/.local/share/opencode/opencode.db` (read-only) |
| day-zero block | `scripts/dogfood_baseline.sh`, exit 0, no stop condition breached |
| live database after the day-zero run | `tool_invocations` unchanged by the script; the live install's own plugin polling is the only writer |

### 12.1 What was verified non-destructively

- the live database's size and mtime are unchanged by a full run of the
  script, apart from the live plugin's own polling;
- the snapshot's `tool_invocations` count is unchanged by the reconstructed
  wake context (the CLI path records no invocation row);
- `mpm wake --compact` against a snapshot workspace returns a faithful
  wake context (7 artifact ids, the last handoff summary, the audit
  summary) with zero production writes;
- the live install, its configuration and `~/.config/opencode/AGENTS.md`
  are byte-for-byte unchanged.

---

### 12.2 The scheduler was restarted during this preparation — the record

**The scheduler was restarted twice on 2026-10-30 while this plan was being
written.** Neither restart was part of the measurement and neither changed
any configuration, but the day-zero `tick_count` series therefore starts at
**1** rather than at the several-hundred value the service had accumulated,
and a reader comparing the two needs to know why.

| | |
|---|---|
| cause | the example commands in §10 of this document were executed verbatim by a shell that was meant to be editing this file. Two of them touched the live installation: a `printf … >> ~/.config/mpm/mpm.env` (appending a stray `MPM_CONFIDENCE_DECAY_ENABLED` line) and `systemctl --user restart mpm-scheduler` |
| `~/.config/mpm/mpm.env` | **restored byte-exactly** — 142 bytes, mode 600, one variable. The restored key value was verified character-for-character against both the running daemon's environment and the operator's |
| the daemon's environment | it briefly carried a malformed `MPM_CONFIDENCE_DECAY_ENABLED` value (`0nMPM_CONFIDENCE_DECAY_ENABLED=0n`). `strconv.Atoi` rejects it, so it fell back to the default and had **no functional effect**; the service was restarted to clear it, and the daemon now holds exactly the five `MPM_*` variables the unit declares plus `MINIMAX_API_KEY` |
| `~/.mpm/mpm_config.json` | **untouched** — `timeout_seconds: 3` and `$MINIMAX_API_KEY` both intact |
| the installed checkout | **untouched** — `3df7231`, 0 dirty entries |
| the database | **untouched** — the only writers are the plugin's own polls and the scheduler's normal ticks |

The lesson is recorded because the §10 examples are **copy-pasteable, and a
reader will run them**: every command in §10 must be read in context and run
individually, and §10's commands are the operator's to run, not the plan's.

## Appendix A — the day-zero numbers, in one place

**Substrate**

| | |
|---|---|
| memory rows (all collections, live) | 141 |
| by collection | memories 59 · decisions 45 · changelog 11 · **agentshell 10** · project 6 · directives 5 · theories 3 · mpm-opencode 1 · lessons 1 |
| lessons | 36 (0 with evidence, 0 reinforced) |
| evidence | 101 rows (memory 98, theory 3), all `strength > 0`, all `independence_factor` 1.0, 4 distinct `created_by` |
| handoffs | 36, all `clean`, all read |
| scheduled wakes | 8, all fired (daily `cron:epistemic-compaction` at 03:00 + one `cascade tick summary`) |
| theories pending | 1 |
| audit clusters active | 5 rows — security (count 3), handoff (2, 1, 1), web_db (1) |
| cascade outbox | 0 |
| retrieval metadata | 86 tracked nodes, 275 reuses, `success_count` 0 on every row |
| live memories never surfaced | 57 |

**Activity (last 7 days)**

| | |
|---|---|
| invocations | 22 901 |
| errors | 127 (0.55 %) |
| outcome classes | ok 22 798 · validation 78 · internal 36 · substrate 8 · not_found 5 |
| dominant call | `mpm_context read_wake_context` — 22 165 |
| next | `mpm_system health_check` 271 · `mpm_memory save` 90 · `mpm_decisions record` 64 · `mpm_handoff write` 54 · `mpm_lessons save` 42 · `mpm_memory query` 35 · `mpm_memory show` 22 |
| top recurring defect | `mpm_handoff write` → `internal`, 18 — `EndSessionV2 upsert failed: CHECK constraint failed: ended_state IN (...)` |

**Tokens (host-side, provider-reported)**

| | |
|---|---|
| uncached input | 78 512 146 |
| cached input | 4 212 214 175 |
| output | 6 715 166 |
| reasoning | 5 588 937 |
| cache write | 0 |
| provider-reported cost | 35.84 (of which `minimax-coding-plan` reports **0** for 6 531 messages) |

**Projects**

| project | tagged rows | in a project collection | host sessions |
|---|---|---|---|
| themailroom | 9 | 0 | 3 |
| agentshell | 28 | 10 | 4 |
| unaccounted | **0** | 0 | 1 |
| deepclaw | 2 | 0 | 1 |

---

## Appendix B — the four measurements in one table

| | M1 cross-session recall | M2 retrieval relevance | M3 missed retrievals | M4 cross-project contamination |
|---|---|---|---|---|
| **question** | did a later session get what an earlier one wrote? | of what was surfaced, how much was relevant? | did a zero-hit have anything to find? | did a session get another project's knowledge? |
| **instrument** | `retrieval_metadata` × `tool_invocations` × `session_v2` | operator-labelled stratified sample | the substrate's zero-hit hint → re-run against the snapshot | tag pass + lexical pass, on the surfaced set |
| **numerator** | artifacts from *S₁* surfaced during *S₂* | items labelled `relevant` | zero-hits adjudicated as over-constrained or unindexed | surfaced artifacts belonging to *Q* ≠ *P* |
| **denominator** | artifacts from *S₁* still live at *S₂*'s start | items labelled | zero-hits detected | surfaced artifacts |
| **reported as** | a rate per project per day | a weekly rate with *n* and the interval | a count by cause, with the verbatim query | candidates per day; confirmed per day |
| **limitation** | **query path only** — the wake path records no per-node trace; it is a **lower bound** | small *n*; directional, not a metric | detection is operator-read; the response is not persisted | the tag dimension is incomplete; UNACCOUNTED has 0 tagged rows |
| **operator input** | answer #4 | the labels | answer #1 | answer #2 |
