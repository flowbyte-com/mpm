# Live MPM runtime — operational health audit

> **Scope.** A read-only audit of the **running** MPM installation on this
> host: what is installed, what the services did over the last 72 hours,
> and whether the operational evidence shows errors, repeated failures,
> silent failures, performance problems or resource-consumption problems
> that the test suite would not catch.
>
> **This is an investigation, not a repair sprint.** Nothing was changed:
> no service was restarted, no configuration touched, no MPM tool call was
> made against the live workspace, no wake or critic cycle was triggered,
> no audit record was modified, no deployment or merge was performed.
> Database inspection used an explicitly read-only SQLite connection whose
> read-only behaviour was **verified before use** (see §2.4).
>
> **Author:** Session B. **Date:** 2026-10-28.
> **Runtime audited:** `~/.mpm` at revision `3df7231`.

---

## 1. Verdict in one paragraph

The runtime is **behaving normally**. Across the 25 days of available
journal there is not one WARN or ERROR line, the scheduler has restarted
**zero** times since its current instance began 32.7 hours ago, and its
measured resource use is 40.5 seconds of CPU across that entire period
(0.034 %) with a 16.8 MB resident set. The audit trail contains 105 error
rows in 14,521 invocations (0.72 %), of which **only three are substrate
faults**. Two of those three are the same reproducible defect — a
transaction whose retry loop cannot possibly succeed — and it is still
present in `main`, so it ships in Friday's release candidate. The largest
single finding is not a fault at all: it is a **silent** one, a nightly
scheduled task whose wake rows are marked `fired=1` without ever having
been executed. That one turns out to be correct behaviour on inspection,
and §5.4 explains why with evidence.

**Nothing found here blocks Friday's release candidate.** One Medium
defect should be tracked and fixed after it; the rest are Low.

---

## 2. Verified live runtime and service inventory

### 2.1 The installed revision

| Item | Verified value | How it was verified |
|---|---|---|
| Runtime root | `/home/v/.mpm` | `ls`, and it is a Git checkout |
| Source revision | `3df723156d7649517508212453de72d347759f43` | `git -C ~/.mpm log -1` |
| Revision subject | `fix(install): provision the OpenCode plugin payload during installation (#73)` | same |
| Revision date | 2026-10-24 14:16:28 +0100 | `git log -1 --format=%ci` |
| Branch / dirtiness | `main`, **clean** | `git -C ~/.mpm status --short` (empty) |
| `VERSION` | `0.1.0-dev` | `cat ~/.mpm/VERSION` |
| Build identity | `g3df72315` — matches the revision | binary mtime 2026-10-24 15:14, i.e. built from this tree |
| Binary mtimes | all five: 2026-10-24 15:14 | `ls -la ~/.mpm/bin/` |

**This is confirmed to be *not* `origin/main`.** `origin/main` is
**67 commits ahead** of the installed revision. The release-candidate
report (`docs/dev/release-candidate-2026-10-30.md` §1) records the same
live identity (`MPM 0.1.0-dev+g3df72315`, MainPID 2804, NRestarts 0,
binary mtime 24 October), so the two observations agree.

### 2.2 Services and processes

| Item | State |
|---|---|
| `mpm-scheduler.service` (systemd **user**) | **active (running)**, MainPID **2804**, started **2026-10-27 06:27:22 UTC**, `NRestarts=0` |
| `mpm-telemetry.service` | **not installed** — no unit file in `~/.config/systemd/user/` |
| `mpm-critic` | **not installed** as a service; the binary exists but nothing runs it |
| `mpm-mcp` | running as PID 5663, **owned by the `hermes` agent**, started 2026-10-27 06:27:27 |
| OpenCode | `opencode-cli serve --service` (PID 8420) plus the desktop app; the MPM plugin runs inside it |
| MPM systemd **timers** | **none** — `systemctl --user list-timers` shows no MPM timer |

The scheduler unit's `ExecStart` is
`mpm-scheduler --interval 60s --log-level info --lock ~/.mpm/scheduler.lock`,
with `MPM_WORKSPACE`, `MPM_DB_PATH`, `MPM_CRITIC_BIN` and `MPM_BIN` all
pinned to absolute paths inside the install.

### 2.3 The active database

| Item | Verified value |
|---|---|
| Path | `~/.mpm/src/db/mpm.db` — matches the unit's `MPM_DB_PATH` |
| Size | 16 MB (`mpm.db`) + 4.0 MB (`mpm.db-wal`) |
| `journal_mode` | `wal` |
| `wal_autocheckpoint` | 1000 pages (4 MB) — **the WAL is checkpointing normally** |
| Schema migrations applied | 18 rows, newest `lessons_fts_resync_v1` |
| FTS5 shadow tables present | `lessons_fts`, `memories_fts`, `reference_chunks_fts`, `references_fts`, `scheduled_wakes_fts`, `sessions_fts`, `topics_fts` |

**FTS5 is compiled into all five installed binaries** (verified statically:
`fts5` symbols are present in each binary, and `USING fts5(...)` SQL
templates are embedded in `mpm`). This mattered, because it ruled out the
most obvious explanation for one of the findings (§6.3).

`~/.mpm/src/db/` also holds `mirror.jsonl` (75 entries), `watchdog.jsonl`
(5 entries) and `mirror.jsonl.lock` / `mpm.db.lock`. There is **no**
`~/.mpm/logs/` and **no** `~/.mpm/runtime/` — the sidecar socket directory
does not exist because the telemetry collector is not installed.

### 2.4 The read-only access used for database inspection

Every query in this report was run against a connection opened as
`file:/home/v/.mpm/src/db/mpm.db?mode=ro`. **Its read-only behaviour was
verified before any data was read**, by attempting a write and confirming
it was refused:

```
opened: file:/home/v/.mpm/src/db/mpm.db?mode=ro
  DDL blocked: attempt to write a readonly database
  INSERT blocked: attempt to write a readonly database
```

`PRAGMA integrity_check` on that same connection returns a single `ok`
row — i.e. the database is structurally sound *and* the checking
interpreter could construct every FTS5 virtual table (SQLite reports
`vtable constructor failed: <name>` for an FTS table it cannot open, so
this is a positive result, not a silent skip).

**No MPM binary was executed against the live database.** Running `mpm
call …` against `~/.mpm` would both write a `tool_invocations` audit row
and open the database through the schema-init path; the audit brief
prohibits both, so the live substrate was reached only through SQLite.

---

## 3. The last 72 hours of operational health

### 3.1 The scheduler journal

`journalctl --user -u mpm-scheduler` spans **2026-10-03 13:09 → 2026-10-28
15:01** (35,004 lines).

| Level | Count, full journal | Count, last 72 h |
|---|---|---|
| INFO | 28,794 | **4,371** |
| WARN | **0** | **0** |
| ERROR | **0** | **0** |

The complete message vocabulary of the entire journal is:

| Message | Count | Cadence |
|---|---|---|
| `cascade drain yielded` | 25,505 | every 60 s (one per tick) |
| `singleton lock acquired` | 505 | once per process start |
| `scheduler running` / `scheduler heartbeat configured` / `mpm-scheduler starting` | 505 each | once per process start |
| `scheduler stopping` / `mpm-scheduler stopped cleanly` | 504 each | once per process stop |
| `scheduler heartbeat` | 208 | every 100 ticks (100 min) |
| `cron injected wakes` | 26 | ~once per day |
| `executing wake` / `wake completed` / `tick executed system wakes` / `cascade summary` | 8 each | one per dispatched wake |

**There is not a single error or warning in 25 days of scheduler
operation.** The tick is doing nothing but reporting an empty cascade
queue: `yield_reason=queue_empty intents_materialized=0
intents_failed=0 elapsed_ms=0`.

### 3.2 Service restarts

505 process starts across 25 days, distributed as:

| Day | Starts | Day | Starts |
|---|---|---|---|
| 10-03 | 21 | 10-14 | 2 |
| 10-04 | **145** | 10-15 | 1 |
| 10-05 | **274** | 10-16 – 10-21 | 1–3/day |
| 10-06 – 10-13 | 1–23/day | 10-22 – 10-27 | 1–3/day |

Every transition in the journal is `Stopping … / Stopped … / Started …` —
a **clean, deliberate stop followed by a start**. The journal contains
**zero** occurrences of `Main process exited`, `Failed with result`,
`code=`, `Segmentation fault`, `panic:` or a core dump (grep across the
full 25 days). The current instance additionally reports
`NRestarts=0`.

**Classification: expected behaviour.** The 10-03 → 10-05 concentration
is a development period (440 restarts in three days) that predates the
installed revision by three weeks; the steady state is 1–3 deliberate
restarts per day.

### 3.3 Error and warning frequency — the substrate's own audit trail

`tool_invocations` covers 2026-10-23 10:41 → 2026-10-28 15:04 (the table
itself was introduced on 10-23). All counts below were measured in a
single read at **2026-10-28 15:16:33 UTC**; the table grows continuously,
so a later read will show a slightly larger total.

| Outcome | Rows | Share |
|---|---|---|
| success | 14,416 | 99.28 % |
| error | **105** | **0.72 %** |

Errors by day: 10-23 → 14, 10-24 → 39, 10-25 → 3, 10-26 → 12, 10-27 → 32,
10-28 → 5.

The 105 errors fall into four groups:

| Class | Rows | What they are |
|---|---|---|
| `validation` / `missing_required_field`, `unknown_action` | 55 | **expected.** Registry probes of non-existent actions (`__probe__`, `help`, `list_actions`) and genuinely malformed calls. The registry correctly refuses them. |
| `internal` / `unclassified` | 45 | **caller validation mistakes** — 44 payload-shape errors plus F2's CHECK-constraint rejection. Not system faults, but misclassified as if they were (§7.4). |
| `substrate` | 3 | **the real faults** — §6. |
| `not_found` | 2 | `mpm_work note work not found: b0fb7c85…` (2026-10-23) and `mpm_memory show memory not found: b6ef4f92…` (2026-10-28). Both benign: the second is a theory that was deliberately **shredded 8 seconds after being proposed** (the row's own content begins `PROBE: a theory with no test is not a fact`), so "not found" is the correct answer. |

**Every error message in the trail is accounted for.** There is no
unknown error, no unclassifiable error, and no error with no
corresponding audit row.

### 3.4 The OpenCode log

`~/.local/share/opencode/log/opencode.log` — 52 MB, 2026-10-02 → now.

| Level | Count |
|---|---|
| INFO | 120,849 |
| WARN | 69 |
| ERROR | 19 |

All 19 ERROR lines are OpenCode's own CLI/network failures: `ShowHelp:
Help requested` from a mistyped subcommand, `models.dev` fetch timeouts,
an `UnauthorizedError` from the local server, `ECONNRESET`, and two
`Failed to drain Session` events (a provider usage limit and an invalid
API key). **None is MPM-related.**

Of the 69 WARN lines, **62 are OpenCode's own** `worktree discovery
failed` on `/home/v/src` and `/home/v/Documents/Default Project`. The
remaining 7 are MPM-plugin related and **all historical**:

| Timestamp | Message | Now? |
|---|---|---|
| 2026-10-13 08:35, 2026-10-16 09:46, 2026-10-16 09:47 | `configured plugin path must be a directory` — the config pointed at `…/mpm-opencode/dist/index.js`, a **file** where a directory was expected | fixed |
| 2026-10-23 10:39, 2026-10-23 12:13 | `failed to load plugin` — `ENOENT` on `~/.mpm/agent_installation/mpm-opencode` | fixed |

Both classes were resolved by the installed revision's own commit
(`3df7231`, *"fix(install): provision the OpenCode plugin payload during
installation (#73)"*). The plugin payload is present and correct now
(`dist/index.js`, `dist/index.v2.js`, `resolve-mpm-binary.js`,
`mpm-build-provenance.json`, all dated 2026-10-24 15:14), and the
configuration points at the directory:
`"plugins": ["file:///home/v/.mpm/agent_installation/mpm-opencode"]`.

**No MPM-related OpenCode error or warning has occurred in the last 72
hours.**

---

## 4. Scheduler and wake activity assessment

### 4.1 What the scheduler actually does

Per 60-second tick, in this order: accrue active uptime → poll
`scheduled_tasks` and inject a wake for any due task → dispatch due
wakes that have a registered handler → run the tick handlers (cascade
drain).

In the last 72 hours the scheduler performed **one** substantive action:
`cron injected wakes count=1` on 2026-10-26, 10-27 and 10-28.

### 4.2 The scheduled task

`scheduled_tasks` holds exactly one row:

| Field | Value |
|---|---|
| id | `epistemic-compaction` |
| name | *Nightly epistemic compaction (agent-owned reflex)* |
| cron_expr | `0 3 * * *` |
| status | `active` |
| last_run_at | 2026-10-28 03:00:22 UTC |
| next_run_at | 2026-10-29 03:00:00 UTC |

**One wake per day is the designed behaviour**, and the journal confirms
exactly one injection per day. This is *not* "wakes executing more often
than configured" — it is on-cadence.

### 4.3 The wake rows, and the thing that looks wrong

`scheduled_wakes` holds six rows: one `cascade_summary` wake (2026-10-23)
and five `cron:epistemic-compaction` wakes (10-24 → 10-28). Every row has
`fired=1`.

At first reading this looks like a serious silent failure: **the nightly
compaction has never run**, and the rows claim it did. The full trace:

| Wake | `created_at` = `target_time` | `fired_at` | Δ | `fired_by` | `dispatched_at` |
|---|---|---|---|---|---|
| 10-24 | 06:10:36 | 07:11:35 | 61 min | NULL | NULL |
| 10-25 | 06:27:14 | 08:27:14 | 120 min | NULL | NULL |
| 10-26 | 03:00:14 | 04:27:14 | 87 min | NULL | NULL |
| 10-27 | 03:00:14 | 04:27:14 | 87 min | NULL | NULL |
| 10-28 | 03:00:22 | 04:27:22 | 87 min | NULL | NULL |

A wake that sits *due* for 87 minutes, then reports `fired=1`, with
`fired_by` NULL. And the scheduler journal contains **no** `executing
wake` line for any of them — only eight in total, all `cascade_summary`,
the last on 2026-10-23.

### 4.4 Why it is correct — and where the trap is

Every one of those rows carries the answer in its own metadata:

```json
{"kind":"cron","source":"cron","task_id":"epistemic-compaction",
 "directive_id":"mpm-seed-epistemic-compaction-policy",
 "expired":{"reason":"cron_1h_retention","at":…,"target_time":…}}
```

**`expired.reason = "cron_1h_retention"`** — the rows were *retired by
the retention sweep*, not executed. The mechanism is
`internal/scheduler/wake_expiration.go`: `SweepOverdueCronWakes` retires
cron-kind wakes whose `target_time` is more than `CronRetentionWindow`
(1 hour) in the past. Its docstring states the assumption explicitly:

> *"the WHERE clause excludes notification-kind wakes (covered by the
> other sweep) and **system kinds (covered by their eager-dispatch
> handlers)**."*

That assumption is wrong for cron wakes, and all three delivery actors
exclude them:

| Actor | Its kind filter | Result for `kind='cron'` |
|---|---|---|
| Scheduler eager dispatch (`scheduler.go` `hasHandler` / `executeOne`) | the scheduler registers handlers for `snapshot`, `critic_audit`, `gc`, `broadcast`, `cascade_summary`, `drill` — **there is no `cron` handler** | not dispatched |
| Deadline-driven claim (`dispatch.go:dispatchClaimNextAdHocWake`) | `metadata.kind IS NULL OR '' OR 'notification'` | never claimed → `dispatched_at` stays NULL ✓ matches the data |
| Opportunistic fold (`core.CheckPendingWakes` with `kinds=nil`) | `metadata.kind IS NULL OR 'notification'` | never surfaced → never folded |

`main` agrees with the classification, in
`internal/core/wake_tools.go`: *"cascade_summary and notification are
wake kinds; 'cron' or other unknown values are **scheduled-task kinds**."*
A cron row is a **scheduled-task marker**, not a delivery vehicle — its
1-hour retirement is intended.

**And the work was never needed.** The directive
(`mpm-seed-epistemic-compaction-policy`) triggers the reflex *"When the
wake context reports `epistemic_pressure.exceeded=true`"*, and the
pressure source is `raw_memories`:

| Table | Rows |
|---|---|
| `raw_memories` | **0** |
| `epistemic_cascade_outbox` | 0 |
| `synth_runs` | 0 |
| `capability_events` | 0 |

With no eligible raw memories there is nothing to compact; the reflex's
own documented response would have been `stop_reason: "no_work"`.
`mpm_system compact` was never called — correctly.

**Classification: expected behaviour.** The nightly task fires on
cadence, has no work to do, and its marker row is retired an hour later
by design.

### 4.5 The residual observability defect

One real problem survives the analysis. **`fired=1` is doing two
different jobs.** The core module's own contract says:

> *"`fired=0` → pending … `fired=1` → **terminal**; no longer pending for
> normal delivery"*

and the expiry path sets `fired=1` on a wake that was **never
delivered**. Any operator — or any query — reading `fired=1` as
*"delivered"* will conclude the nightly compaction ran. The row's
`metadata.expired.reason` is the only thing that tells the truth, and
both `fired_by` and `dispatched_at` are NULL, so there is no other
signal. This is a **Low**-severity observability defect: the state
machine conflates "delivered" with "retired".

### 4.6 The rest of the §4 checklist — all clear

| Check | Result |
|---|---|
| Unexpected restarts or crashes | **None.** 505 clean restarts, zero crashes; `NRestarts=0` on the current instance |
| Wake tasks repeatedly failing or retried | **None.** One wake per day, all terminal, none retried |
| Wakes executing more often than configured | **No** — on-cadence (`0 3 * * *`) |
| Jobs stuck, abandoned or duplicated | **None.** `scheduled_wakes` has 6 rows, all terminal; no duplicates |
| Singleton lock anomalies | **None.** `singleton lock acquired` on all 505 starts; the lock lives inside the encrypted tree by design |
| Critic scheduling despite the P2b gates | **None observed.** No `critic_audit` wake exists; `~/.mpm/backups/critic-pre/` is **empty** (the critic has never run) |
| Unexpected activity during inactivity | **None from MPM.** See §8.3 |

---

## 5. Database and persistence health

### 5.1 The watchdog: two real write failures

`~/.mpm/src/db/watchdog.jsonl` holds 5 entries. Three are `warn`
(`memory_shredded`, ordinary operator actions). **Two are `error`**:

```json
{"v":2,"ts":"2026-10-24T19:04:44.920918158Z","level":"error","op":"exec",
 "reason":"sql_error","duration_ms":3104,"retries":5,
 "sql_shape":"INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, is_long_term, weight,…",
 "detail":"exec failed","error":"database is locked"}
{"v":2,"ts":"2026-10-25T08:37:56.05159041Z","level":"error","op":"exec",
 "reason":"sql_error","duration_ms":3110,"retries":5, … ,"error":"database is locked"}
```

### 5.2 The same two failures in the audit trail

Both correspond to `mpm_decisions record` failures in `tool_invocations`
(`outcome_class=substrate`, `outcome_code=wal_busy_timeout`):

| Timestamp | Tool | Result |
|---|---|---|
| 2026-10-24 19:04:26 | `mpm_decisions record` | error |
| 2026-10-24 19:04:26 | `mpm_decisions record` | error |
| 2026-10-24 19:04:26 | `mpm_decisions record` | error |
| 2026-10-24 19:04:32 | `mpm_decisions record` | error |
| 2026-10-24 19:04:32 | `mpm_decisions record` | error |
| 2026-10-24 19:04:32 | `mpm_decisions record` | error |
| 2026-10-24 19:04:41 | `mpm_decisions record` | success ×3, error ×1 |
| 2026-10-24 19:04:49 | `mpm_decisions record` | success ×2 |
| 2026-10-25 08:37:52 | `mpm_decisions record` | success, **error** |

In the 2 minutes around the first burst, 29 `mpm-cli` wake-context
injections and 17 `opencode` writes overlapped. **The failures are
write-write contention between two live writers.**

### 5.3 Root cause — the retry cannot possibly succeed

This is the one substantive defect, and the arithmetic proves the
mechanism.

**The write path.** `RecordDecision` (`internal/core/epistemology_tools.go`)
wraps its write in `dm.WithTx(...)` and calls `dm.SaveMemoryNode(node, …)`,
whose INSERT goes through `node.ExecTracked(...)` — i.e. the **`txNode`**
variant.

**`WithTx` opens a DEFERRED transaction** (`dm.db.Begin()`). In SQLite a
deferred transaction takes a *read* snapshot first and acquires the write
lock only at its first write.

**The failure mode.** If another connection commits between that
transaction's first read and its first write, the write cannot upgrade,
and SQLite returns **`SQLITE_BUSY_SNAPSHOT` immediately**. `busy_timeout`
is *not* invoked for a snapshot conflict — waiting cannot help, because
the snapshot is stale by definition. The only correct remedies are to
roll back and re-take the transaction, or to open write transactions with
`BEGIN IMMEDIATE` so the write lock is taken up front.

**The retry does the one thing that cannot work.**
`txNode.ExecTracked` re-executes the statement **inside the same
transaction**, where the snapshot is unchanged and therefore still stale:

```go
result, err := t.tx.Exec(query, args...)      // same tx, same snapshot
if err != nil && isBusyError(err) && attempts <= retries {
    time.Sleep(backoff)                       // 100→200→400→800→1600 ms
    continue
}
```

**The measured duration confirms the diagnosis.** The recorded
`duration_ms` values are **3104** and **3110**. The backoff schedule for
5 retries sums to 100+200+400+800+1600 = **3100 ms**. So the six
attempts consumed ~4 ms of actual SQL work in total: **every attempt
returned BUSY in under a millisecond.** A contended write lock with
`busy_timeout=5000` would have been waited out for up to 5 seconds per
attempt; it was not. That is the signature of a snapshot conflict, not of
lock contention.

*(The production DSN is `?_foreign_keys=1&_busy_timeout=5000`
(`internal/core/db.go:138`), and go-sqlite3 v1.14.37 parses `_busy_timeout`
in `SQLiteDriver.Open` and applies `PRAGMA busy_timeout` **per connection**
— verified in the module cache at `sqlite3.go:1200` and `:1491`. So every
pooled connection really does have a 5 s wait window, and it was not
exercised.)*

**The codebase already documents the precondition the retry depends on** —
and shows that it was written for a *different* transaction discipline.
`internal/core/db.go:4159`:

> *"2026-08-27: retries=5 … so concurrent writers transparently back off
> and retry on SQLITE_BUSY instead of surfacing the lock error to the
> caller. The cascade materializer runs materializeTheory concurrently
> **under BEGIN IMMEDIATE**; without retry, transient WAL contention …
> escalated to a hard failure that rolled back the enclosing transaction
> and silently dropped the theory row."*

The retry was added for the **BEGIN IMMEDIATE** path, where a busy error
*is* transient. The generic `WithTx` path uses a **deferred** BEGIN and
inherits the same `retries=5` — where it cannot work. `BEGIN IMMEDIATE`
is already used elsewhere in the codebase
(`similarity_candidates_store.go:1109`, `broadcast.go`,
`cascade_materializer.go`), so the fix has a precedent.

**Was knowledge lost?** Not in these two incidents: the callers retried
and the writes succeeded (§5.2). But the *substrate* returned a hard
failure after burning 3.1 s on a doomed operation, and a caller that does
**not** retry loses the write. `mpm_decisions record` failing means the
decision is not recorded. The substrate's own contract (read-back before
returning success) means the failure is reported rather than silent — so
this is a *visible* loss, not a silent one, but it is still a loss.

### 5.4 Is it fixed in `main`?

**No.** `git diff 3df7231 origin/main -- internal/core/db.go` shows
216 added lines, and `txNode.ExecTracked` is **byte-identical** between
the two revisions (verified by extracting and diffing the function).
`WithTx` still begins with `dm.db.Begin()`. Main's changes to this area
are about *attribution* — routing the memories INSERT through a `DBNode`
so watchdog entries carry the right shape and the invocation identity
(`SaveMemoryNodeWithInvocation`, `SaveMemoryNode`) — not about the
retry semantics.

Main's `internal/core/db.go:2645` does acknowledge the phenomenon, but
only for schema initialisation: *"in WAL mode that surfaces as
SQLITE_BUSY_SNAPSHOT, which busy_timeout does not retry."* The
write path was not brought under the same understanding.

### 5.5 The rest of the §5 checklist

| Check | Result |
|---|---|
| SQLite busy / locked errors | **2 events**, both the defect above (§5.3) |
| Corruption errors | **None.** `PRAGMA integrity_check` → `ok` |
| Transaction failures / partial operations | None beyond §5.3. The failed `mpm_decisions record` wrote **no** row — the write-back assertion held |
| Persistent retry loops | **None.** The watchdog shows exactly 2 error records, not a repeating series |
| Schema mismatch warnings | **None.** 18 migrations applied; the schema-init lock (`mpm.db.schema-init.lock`) has existed since 10-27 with no contention recorded |
| Database-selection inconsistencies | **None.** The unit pins `MPM_DB_PATH=~/.mpm/src/db/mpm.db`; every row in the trail was written to that file |
| Unexpected audit or diagnostic writes | **Yes — see §7.3.** 96.7 % of the audit trail is wake-context injection |

---

## 6. OpenCode and MCP operational findings

### 6.1 Plugin health

The OpenCode plugin loads correctly and is the source of the
wake-context injection. Its only registered hook is
`experimental.chat.system.transform`, which fires **once per chat
message**. There is no `tool.*` or `event.*` hook, so the injection rate
tracks *messages*, not tool calls — which matches the measured shape
(§8.2).

**No plugin startup or initialisation failure in 72 hours.** The five
historical failures (§3.4) all predate the installed revision.

### 6.2 Tool execution failures

| Pattern | Count | Assessment |
|---|---|---|
| `unknown action …` | 27 | **Expected.** Registry probes of actions that do not exist. The registry refusing them is the correct behaviour — and `TestE2E_TheRegistryStillHasNoProvenanceTool` pins the analogous case for `mpm_provenance` |
| `__probe__` / `help` / `list_actions` | ~15 | **Expected.** The MCP/CLI capability probe mechanism exercising the action vocabulary |
| payload-shape errors (`commitments must be an array of strings`, `tags must be…`, `field 'limit' must be a number`, `outside the params envelope`) | 44 | **Caller errors.** See §7.4 for the classification defect and §6.4 for the one systematic source |
| `session_id is required` | 1 | **Expected.** `mpm_scratchpad read` (2026-10-28 12:52) — the error names both recovery paths, as specified |
| `work not found` / `memory not found` | 2 | **Expected** (§3.3) |
| `EndSessionV2: insert: CHECK constraint failed: ended_state IN (…)` | 1 | **A real gap — see §7.2** |
| `record decision: database is locked` | 2 | **The defect — §5.3** |
| `integrity check: vtable constructor failed: topics_fts` | 1 | **Transient — §6.3** |

### 6.3 The transient integrity-check failure

`mpm_system health_check` failed **once** in 106 calls:

```
2026-10-26 19:52:51  health_check  error  12ms
  integrity check: vtable constructor failed: topics_fts
```

It is bracketed by successes at 19:52:38 and 19:56:16, and **105 of the 106
health checks run in the audit trail succeeded**. FTS5 *is* compiled into the installed
binaries (§2.3), so "the binary lacks FTS5" is ruled out; and the same
integrity check run against the live database today returns `ok`.

**Classification: insufficient evidence for a cause, but not a
persisting condition.** A transient `vtable constructor failed` during
`PRAGMA integrity_check` is consistent with the check running while
another connection was concurrently modifying an FTS shadow table (the
check constructs each vtable against a schema snapshot it may not share
with a concurrent writer). **What further evidence would settle it:**
the health check's own error path does not record the connection's
schema cookie or the concurrent writer's identity, so the next occurrence
would again be un-attributable. The database is demonstrably sound now.

*Note: the durations of the health checks grew from ~12 ms (10-23) to
~50–70 ms (10-27/28), with one 157 ms outlier. That tracks database
growth, not degradation.*

### 6.4 The one systematic caller-error source: the Pi adapter

23 of the 25 `mpm_handoff write` failures share one message —
`commitments must be an array of strings, got map[string]interface {}` —
and **every one of them has `framework_name = 'pi'`**:

| Date | Count |
|---|---|
| 2026-10-23 10:56 | 5 (then 1 envelope error) |
| 2026-10-23 14:32 | 5 |
| 2026-10-24 11:29 | 9 |
| 2026-10-24 18:17 | 5 (then 1 envelope error) |

The Pi adapter was sending `commitments` as an **object** rather than an
array, and the caller retried the identical malformed payload 5–9 times
per burst. The substrate's rejection is **correct and well-messaged** —
this is a client-side defect, not a substrate one.

**Classification: historical defect, already corrected in `main`.**
Commit `9406936` (*"refactor(agent-installation): retire the legacy host
adapters (#137)"*, 2026-10-27) removed `agent_installation/mpm-pi/`
entirely; the OpenCode-only scope retired the adapter that produced
these. The errors stop at 2026-10-24 18:17 — before the retirement
commit — which is consistent with the Pi host simply not being used
afterwards.

### 6.5 MCP server

The one running `mpm-mcp` (PID 5663) belongs to the `hermes` agent and
has been up since 2026-10-27 06:27:27 with no errors in the OpenCode
journal and no MCP-sourced rows in the audit trail. Nothing to report.

---

## 7. Findings

### 7.1 F1 — Mid-transaction busy retry cannot succeed (Medium)

| | |
|---|---|
| **Severity** | **Medium** |
| **Symptom** | `mpm_decisions record` fails with `database is locked` after burning 3.1 s; the write does not happen unless the caller retries |
| **Evidence** | `watchdog.jsonl` 2026-10-24T19:04:44.920918158Z and 2026-10-25T08:37:56.05159041Z, `level=error`, `op=exec`, `retries=5`, `duration_ms=3104`/`3110`, `error=database is locked`, `sql_shape=INSERT INTO memories (…)`. Corroborated by 7 `mpm_decisions record` error rows in `tool_invocations` on 10-24 19:04:26–19:04:32, overlapping 29 wake-context writes and 17 agent writes in the same 2 minutes |
| **Recurrence** | 2 independent occurrences in 6 days, both under concurrent-writer load |
| **Confirmed or suspected** | **Confirmed.** The 3104/3110 ms durations equal the retry backoff sum (3100 ms) exactly, so all six attempts returned BUSY in <1 ms each — the signature of `SQLITE_BUSY_SNAPSHOT`, which `busy_timeout` does not wait on. `WithTx` opens a deferred transaction; `txNode.ExecTracked` retries the statement inside it, where the snapshot cannot change |
| **Installed-runtime applicability** | **Yes** — present at `3df7231` and observable |
| **Current `main` affected?** | **Yes.** `txNode.ExecTracked` is byte-identical in `main`; `WithTx` still uses a deferred `Begin()`. Main's changes to this area are attribution-only |
| **Recommended action** | Take the write lock up front: open `WithTx`'s transaction with `BEGIN IMMEDIATE` (precedent: `similarity_candidates_store.go:1109`), **or** roll back and re-run the whole `WithTx` callback on a mid-transaction busy instead of retrying the statement in place. Either makes the retry meaningful. A regression test should assert that a concurrent writer does not produce a hard failure |
| **Release impact** | **Does not block the RC**, but ships the defect into it. In both observed incidents the caller retried and the write succeeded, so there was no knowledge loss — but a non-retrying caller (any automated path) loses the write |

### 7.2 F2 — The handoff `state` enum is not validated at the boundary (Low)

| | |
|---|---|
| **Severity** | **Low** |
| **Symptom** | `mpm_handoff write` with an invalid `state` returns a raw database error: `EndSessionV2: insert: CHECK constraint failed: ended_state IN ('clean','crashed','interrupted','force_end')` |
| **Evidence** | `tool_invocations` 2026-10-27 18:21:56 (`framework=opencode`); matching `system_audit_log` row `warn / component=handoff / "EndSessionV2 insert failed: CHECK co…"` at the same second |
| **Recurrence** | 1 occurrence |
| **Confirmed or suspected** | **Confirmed.** `handleHandoffWrite` reads `state` with `getString(p, "state")` and passes it straight to `dm.EndSession(…)`; the only guard is the table's CHECK constraint (`internal/core/schema.go:1100`) |
| **Installed-runtime applicability** | Yes |
| **Current `main` affected?** | **Yes** — `main`'s `handleHandoffWrite` is unchanged in this respect |
| **Recommended action** | Validate the enum at the tool boundary and name the four permitted values. **The precedent is in the same function**: `commitments` and `open_questions` already use the strict `parseStrictStringArrayOrEmpty` helper precisely so that "the four states — omitted, explicit [], valid array, invalid shape — produce distinct outcomes". `state` is the one field in that function without it |
| **Release impact** | None. Cosmetic-quality defect: a confusing error for a caller mistake |

### 7.3 F3 — The audit trail is 96.7 % wake-context noise, with no retention (Low)

| | |
|---|---|
| **Severity** | **Low** (operational), trending up |
| **Symptom** | `tool_invocations` is 14,521 rows, of which **14,037 (96.7 %)** are `read_wake_context`. The table and its seven indexes occupy **9.6 MB** — 691 bytes per row all-in |
| **Evidence** | Per-day injection counts: 10-23 → 4, 10-24 → 244, 10-25 → 1,865, 10-26 → 3,916, 10-27 → 4,925, 10-28 → 2,939 (to 15:04). `dbstat` page counts for `tool_invocations` and its indexes |
| **Recurrence** | Continuous; scales with agent activity |
| **Confirmed or suspected** | **Confirmed.** The OpenCode plugin's single `experimental.chat.system.transform` hook fires once per chat message, and every MPM call writes an audit row |
| **Installed-runtime applicability** | Yes |
| **Current `main` affected?** | **Yes.** `git grep "DELETE FROM tool_invocations"` in `main` finds only test cleanup — **there is no retention path** |
| **Recommended action** | Decide a retention policy for `tool_invocations` (the wake-context rows are the bulk and carry little forensic value), or make wake-context injection non-auditing. At the measured rate the table grows ~1.6 MB/day → ~577 MB/year with no bound |
| **Release impact** | None for Friday. A long-horizon capacity concern |

### 7.4 F4 — Payload-shape errors are classified as system faults (Low)

| | |
|---|---|
| **Severity** | **Low** |
| **Symptom** | 45 of the 105 error rows (43 %) carry `outcome_class='internal'`, `outcome_code='unclassified'` — but they are **caller validation mistakes**, not substrate faults: 44 are payload-shape errors and the 45th is F2's CHECK-constraint rejection |
| **Evidence** | `internal/core/tool_outcome.go` classifies `" is required"` → `validation/missing_required_field`, `"Valid actions include"` → `validation/unknown_action`, `"not found"` → `not_found`, `"database is locked"` → `substrate/wal_busy_timeout`, `"integrity check"` → `substrate/integrity_check_failed`. The **shape family has no case**: `must be an array of strings` (25), `must be a number` (4), `must be a JSON object` (1), `outside the params envelope` (3) all fall through to the default `internal/unclassified` |
| **Recurrence** | The single largest error category in the live trail |
| **Confirmed or suspected** | **Confirmed** by reading the classifier and counting the live rows |
| **Installed-runtime applicability** | Yes |
| **Current `main` affected?** | To be confirmed at the merge — the classifier was not in the diff scope reviewed here, and the live evidence is from the installed revision |
| **Recommended action** | Add the shape family to the classifier → `validation/malformed_payload`. The consequence today is that `internal` — the class that is *supposed* to mean "a system fault" — is 43 % caller noise, so a dashboard or Doctor filtering on `outcome_class='internal'` sees noise and a real defect (F1) as the same thing |
| **Release impact** | None. Observability only |

### 7.5 F5 — `fired=1` conflates "delivered" with "retired" (Low)

Covered in §4.5. A cron-kind wake that was **never delivered** is marked
`fired=1` by the retention sweep, with `fired_by` and `dispatched_at`
both NULL. `fired=1` means "terminal" in the core module's own contract,
but it reads as "delivered". Recommendation: give the expiry path its own
terminal marker (or set `fired_by='cron-retention-sweep'`) so the row
cannot be misread. Release impact: none.

### 7.6 F6 — The installed build lacks the P2b critic arming gate (Low, installed-runtime only)

| | |
|---|---|
| **Severity** | **Low** for the installed runtime; **already closed** in `main` |
| **Symptom** | The installed revision has **no** producer-side gate on wake kinds: `ScheduleWake` accepts any caller-supplied `metadata.kind`, and the scheduler **does** register a `critic_audit` handler. Arming a `critic_audit` wake through the public scheduling surface would therefore dispatch the critic, whose hunts write **trusted** lessons with no evidence citation and no review — the exact trust-boundary violation the P2b gates exist to prevent |
| **Evidence** | `git grep "WakeKindBlocked" 3df7231` → **no hits**; `git grep "WakeKindBlocked" origin/main` → 3 hits. `cmd/mpm-scheduler/main.go:159` registers `s.Register("critic_audit", scheduler.CriticAuditHandler)` in **both** revisions |
| **Recurrence** | **Never occurred.** `scheduled_wakes` contains no `critic_audit` row, and `~/.mpm/backups/critic-pre/` is **empty** — the critic has never run on this host |
| **Confirmed or suspected** | **Confirmed** as a latent capability; **confirmed absent** as an event |
| **Installed-runtime applicability** | Yes — the installed build is one API call away |
| **Current `main` affected?** | **No.** Main adds both gates: the producer-side arming check in `ScheduleWake` and the consumer-side dispatch refusal in `executeOne`, with the refusal recorded in `last_error` so it is observable rather than silent |
| **Recommended action** | No action for the RC. For the installed build: upgrade, or arm nothing. The exposure window closes on the next upgrade |
| **Release impact** | None — `main` closes it |

### 7.7 F7 — A stale in-repo claim about `busy_timeout` (Low)

`internal/core/db.go:1330` states:

> *"The DSN suffix `_busy_timeout=5000` … is supposed to set this at
> connection-open time, but mattn/go-sqlite3's lazy-connection pool can
> open subsequent pooled connections without honoring it, leaving
> busy_timeout at the SQLite default of 0 (no waiting)."*

**This is not true for the pinned driver.** go-sqlite3 v1.14.37 parses
`_busy_timeout` in `SQLiteDriver.Open` (module cache, `sqlite3.go:1200`)
and applies `PRAGMA busy_timeout` per connection (`sqlite3.go:1491`), so
every pooled connection does get the 5 s window. The stale claim is what
makes F1's failure mode hard to see: a maintainer reading it would
conclude the busy timeout is absent and look for a different cause. The
comment should be corrected as part of the F1 fix.

---

## 8. Performance and resource consumption

### 8.1 Measured scheduler consumption

| Metric | Value | Source |
|---|---|---|
| Wall clock | 32.7 h (117,782 s) | `ps etimes` |
| **CPU time** | **40.54 s** → **0.034 % of one core** | `systemctl show -p CPUUsageNSec` |
| Memory current | 16.8 MB | `systemctl show -p MemoryCurrent` |
| Memory peak | 27.9 MB | `systemctl show -p MemoryPeak` |
| RSS | 16.6 MB | `ps rss` |
| Restarts | 0 | `NRestarts` |

The scheduler's own log corroborates this: 25,505 "cascade drain
yielded" lines over 25 days, every one with `intents_materialized=0`,
`intents_failed=0`, `elapsed_ms=0`. **The idle maintenance loop does
essentially nothing and costs essentially nothing.**

### 8.2 Measured cost of the wake-context injection

On the peak day (2026-10-27): **4,925 injections**, p50 duration **73 ms**,
p90 98 ms, p99 128 ms, max 260 ms — **370.7 s of database work in total**.
Lifetime across 13,952 injections: 900.5 s.

Gap distribution on 10-27: **4,034 of 4,925 calls were less than 10 s
apart.** Hourly distribution runs from 244 (06:00 UTC) to 578 (16:00
UTC), with **10 calls in the 00:00 hour** and near-zero between 01:00 and
05:00. The shape is **agent-activity-driven** (bursts while a session is
working, quiet overnight), not a runaway loop.

**This is 5,000 subprocess spawns of the `mpm` CLI per day**, each opening
the SQLite database, reading the wake context, writing an audit row and
closing. The CPU cost is measured and small (~6 minutes/day across
spawns); the *audit-trail* cost is the visible one (F3).

### 8.3 Token consumption — what the logs do and do not support

**The scheduler causes no agent activation and no token consumption.** The
evidence is direct:

- The only wake the scheduler ever dispatches is `cascade_summary` (8 in
  25 days, 60 s after creation), and the scheduler's dispatch is to an
  **in-process handler**, not to an agent.
- The nightly `cron` wake is never dispatched (§4.4), so it never reaches
  an agent either.
- No `gc`, `broadcast`, `snapshot`, `critic_audit` or `drill` wake exists
  in the table, and the critic has never run (`backups/critic-pre/` empty).
- The MPM-side calls are `mpm_context read_wake_context` — a **database
  read** that injects text into a system prompt. It invokes **no LLM**.

**The logs do not contain the information needed to attribute LLM token
usage**, and this report does not guess at it. Token accounting for the
OpenCode host lives outside MPM's evidence surface: `mpm` records
*invocations*, not tokens, and the telemetry sidecar that would record
per-invocation token counts **is not installed** on this host (no
`mpm-telemetry.service`, no `runtime/` socket directory, no
`telemetry.db`). Any statement about token spend attributable to MPM
would be inference, and none is made here.

### 8.4 The rest of the §3 performance checklist

| Check | Result |
|---|---|
| Excessive process launches | **4,925/day** of the `mpm` CLI, agent-driven (§8.2). Bounded by chat-message count, not by MPM |
| Repeated expensive operations | None. The scheduler's per-tick work is `elapsed_ms=0` |
| High CPU / memory | **No.** 0.034 % CPU, 16.8 MB RSS, 27.9 MB peak — all measured |
| Unexpected token-consuming activity | **None attributable to MPM** (§8.3) |
| Inefficient polling | The 60 s tick is configured, and the tick handler is idempotent and empty. `wal_autocheckpoint=1000` keeps the WAL at 4 MB |
| Long-running operations that cannot complete | None. The slowest tracked operation in 6 days is a 260 ms wake-context read |

---

## 9. Existing fixes in `main` that already address observed problems

| Observed problem | Status in `main` | Commit |
|---|---|---|
| The Pi adapter's malformed `commitments` payload (23 errors) | **Fixed** — the legacy host adapters are retired; `agent_installation/mpm-pi/` is gone | `9406936` |
| The OpenCode plugin failing to load (ENOENT on the plugin directory) | **Fixed** — the install provisions the plugin payload | `3df7231` (the installed revision itself) |
| The `configured plugin path must be a directory` misconfiguration | **Fixed** — the config points at the directory, and the install provisions `dist/` | `3df7231` |
| The critic's automatic trust boundary (an armed `critic_audit` wake) | **Fixed** — both the arming gate and the dispatch refusal now exist, and the refusal is recorded in `last_error` | `origin/main` (`internal/core/wake_tools.go`, `internal/scheduler/scheduler.go`) |
| The telemetry hunt writing trusted lessons (§139, delivered separately) | **Fixed** — now a read-only diagnostic that writes nothing | #151 → #150 → #152 (stacked, unmerged) |
| The `mpm-telemetry query` surface creating the store it read | **Fixed** — read-only open, missing store is a clear error | #151 (stacked, unmerged) |
| The `read_wake_context` wake carrying a *shell-out* to a tool that is not in the registry | **Fixed** — the count is computed in-process | #150 (stacked, unmerged) |

**Not fixed in `main`:** F1 (the futile retry), F2 (the `state` enum),
F3 (unbounded audit growth), F5 (`fired` conflation), F7 (the stale
`busy_timeout` comment).

---

## 10. Friday release-candidate risk assessment

**The release candidate's code is `4111c46`** (documentation commit
`a1ba8f0` on top); `release/rc-4111c46` has no commits outside `main`, so
assessing `main` assesses the RC.

### 10.1 Does anything found here block the RC?

**No.** Nothing found is a Critical or High defect, nothing found caused
data loss, and nothing found is a regression introduced by the RC's own
changes.

| Finding | Severity | RC impact | Rationale |
|---|---|---|---|
| F1 — futile mid-transaction retry | Medium | **Does not block; ships** | Present in `main`; 2 events in 6 days; both recovered by caller retry. The RC's code changes are unrelated to the transaction discipline |
| F2 — `state` enum unvalidated | Low | None | Caller-error message quality |
| F3 — unbounded audit growth | Low | None | Capacity, not correctness |
| F4 — shape errors misclassified | Low | None | Observability |
| F5 — `fired` conflation | Low | None | Observability |
| F6 — no critic arming gate | Low | **None — closed in `main`** | Both gates present in the RC |
| F7 — stale `busy_timeout` comment | Low | None | Documentation; misleads the F1 fix if not corrected alongside it |

### 10.2 The live installation's exposure

The installed runtime is **67 commits behind `main`**, which is material
for exactly two reasons:

1. **F6 is still open there.** The installed build can be made to dispatch
   the critic — the automatic trusted-lesson writer — through one public
   API call. It has never happened, but the exposure window closes on
   upgrade.
2. **The Pi adapter and the plugin-provisioning fixes are only in the
   installed build by coincidence of timing** (the plugin fix *is* the
   installed revision; the Pi retirement is not).

### 10.3 Recommendation

**Proceed with Friday's RC.** Track **F1** as a Medium defect to fix
after the release — and fix it with F7 in the same change, because the
stale comment is what makes the failure mode hard to diagnose. **F1 does
not block**, because the two observed failures both recovered on caller
retry and no write was silently lost.

**Before the next live upgrade**, note that the installed build should not
be used to arm `critic_audit` wakes (F6) — which is already moot in
practice, since nothing in the runtime arms them.

---

## 11. Reproduction notes

Everything in this report is reproducible without touching the live
system's state.

**Journal (read-only):**

```bash
journalctl --user -u mpm-scheduler --no-pager -o cat --since "72 hours ago" \
  | grep -oE "level=[A-Z]+" | sort | uniq -c
journalctl --user -u mpm-scheduler --no-pager -o cat \
  | grep -oE 'msg="[^"]+"' | sort | uniq -c | sort -rn
journalctl --user -u mpm-scheduler --no-pager \
  | grep -cE "Main process exited|Failed with result|Segmentation fault|panic:"
```

**Database (explicitly read-only — verify the handle first):**

```python
import sqlite3, os
db = os.path.expanduser('~/.mpm/src/db/mpm.db')
con = sqlite3.connect('file:%s?mode=ro' % db, uri=True)
try:
    con.execute("CREATE TABLE _probe(x)")          # must raise
except Exception as e:
    print("read-only verified:", e)                  # attempt to write a readonly database
for r in con.execute("SELECT started_at, tool_name, action, result_status, "
                     "error_message, outcome_class, outcome_code "
                     "FROM tool_invocations WHERE result_status='error' "
                     "ORDER BY started_at"):
    print(r)
```

**Watchdog (read-only):**

```bash
python3 -c "
import json
for line in open('/home/v/.mpm/src/db/watchdog.jsonl'):
    e = json.loads(line)
    if e.get('level') == 'error':
        print(e['ts'], e['op'], e.get('reason'), e.get('duration_ms'), e.get('retries'), e.get('error'))
"
```

**Resources:**

```bash
systemctl --user show mpm-scheduler \
  -p CPUUsageNSec -p MemoryCurrent -p MemoryPeak -p NRestarts -p ExecMainStartTimestamp
```

---

## 12. Method, and what this audit did not do

**Sources consulted:** the systemd user journal for `mpm-scheduler`
(35,004 lines, 2026-10-03 → 2026-10-28); the OpenCode log (52 MB,
2026-10-02 → now); the live `tool_invocations`, `scheduled_wakes`,
`scheduled_tasks`, `system_audit_log`, `system_config`, `sessions`,
`memories`, `lessons_base`, `raw_memories`, `works`, `work_events`,
`blobs` and `schema_migrations` tables, all read through a verified
read-only connection; `watchdog.jsonl` and `mirror.jsonl`; the installed
unit file; `~/.mpm`'s Git state; the systemd unit and service accounting;
the process table; and the source of both the installed revision
(`3df7231`) and `origin/main` for correlation.

**Deliberately not done:** no service was started, stopped or restarted;
no configuration, credential or scheduler setting was modified; no MPM
binary was executed against the live workspace (so no `tool_invocations`
row was written by this audit); no wake or critic cycle was triggered; no
audit or diagnostic record was modified or deleted; no MPM tool call was
made against `~/.mpm`; the live database was never opened for writing;
OpenCode was not reconfigured; `~/.mpm` was not touched at all.

**Not established by this audit:**

- The cause of the single transient `topics_fts` vtable-constructor
  failure (§6.3). The database is sound now and the condition did not
  recur; the health check does not record enough context to attribute it
  if it does.
- Any attribution of LLM token usage (§8.3). The telemetry sidecar is
  not installed on this host, so no token evidence exists in MPM's
  surfaces. No estimate is offered.
- Whether the concurrent-writer pattern that produced F1 is more or less
  likely on a machine with more agents. The mechanism is established; the
  rate is a property of this host's activity, not a general one.
- Whether the `tool_invocations` growth rate (F3) will hold. The
  projection in §7.3 assumes the observed rate and no retention.
